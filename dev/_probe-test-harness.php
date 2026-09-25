<?php
/**
 * 探测相关测试共用的「测试台」：临时库、模拟上游、应用服务、登录会话、断言。
 *
 * 为什么单独抽一个文件：控制器测试与端到端测试都要做同样一套重活 ——
 * 起一个真的 Webman 服务、真的登录、真的打 HTTP。这些代码写两遍的话，
 * 迟早只有一份会跟着框架改动更新，另一份悄悄失效（而失效的测试
 * 通常不会报错，只是「测了个寂寞」）。
 *
 * 本文件不是可执行脚本，只能被 test-*.php 引入。
 */

declare(strict_types=1);

if (basename((string) ($_SERVER['SCRIPT_FILENAME'] ?? '')) === basename(__FILE__)) {
    fwrite(STDERR, "这是测试台，不是可执行脚本。请运行 scripts/test-model-probe-*.php\n");
    exit(1);
}

$GLOBALS['probeTestTempFiles'] = $GLOBALS['probeTestTempFiles'] ?? [];
$GLOBALS['probeTestProcesses'] = $GLOBALS['probeTestProcesses'] ?? [];
$GLOBALS['probeTestChecks'] = $GLOBALS['probeTestChecks'] ?? 0;
$GLOBALS['probeTestFailures'] = $GLOBALS['probeTestFailures'] ?? [];

// ═══════════════════════════════════════════════════════════
// 断言
// ═══════════════════════════════════════════════════════════

function check(string $name, bool $ok, string $detail = ''): void
{
    $GLOBALS['probeTestChecks']++;
    if ($ok) {
        echo "  [通过] {$name}\n";

        return;
    }
    $GLOBALS['probeTestFailures'][] = $name . ($detail !== '' ? " —— {$detail}" : '');
    echo "  [失败] {$name}" . ($detail !== '' ? " —— {$detail}" : '') . "\n";
}

function throws(string $name, callable $fn, string $expectSubstring = ''): void
{
    try {
        $fn();
    } catch (Throwable $e) {
        check($name, $expectSubstring === '' || str_contains($e->getMessage(), $expectSubstring), '异常消息是：' . $e->getMessage());

        return;
    }
    check($name, false, '没有抛出异常');
}

/** 打印汇总并返回进程退出码 */
function probe_test_report(): int
{
    $checks = (int) $GLOBALS['probeTestChecks'];
    $failures = (array) $GLOBALS['probeTestFailures'];
    echo "\n";
    if ($failures === []) {
        echo "全部 {$checks} 项检查通过。\n";

        return 0;
    }
    echo "共 {$checks} 项检查，" . count($failures) . " 项失败：\n";
    foreach ($failures as $failure) {
        echo "  - {$failure}\n";
    }

    return 1;
}

// ═══════════════════════════════════════════════════════════
// 临时文件与子进程回收
// ═══════════════════════════════════════════════════════════

function probe_test_temp(string $tag, string $suffix = '.tmp'): string
{
    // 加一个自增序号：同一个脚本里可能多次起服务（例如「开关打开」与「开关关闭」两次），
    // 若文件名只按 pid 生成，第二次会撞上还被前一个进程占着的同名日志文件，
    // 报 “Failed to open stream: Permission denied”
    $GLOBALS['probeTestTempSeq'] = (int) ($GLOBALS['probeTestTempSeq'] ?? 0) + 1;
    $path = sys_get_temp_dir() . DIRECTORY_SEPARATOR . 'aqua-probe-' . $tag . '-'
        . getmypid() . '-' . $GLOBALS['probeTestTempSeq'] . $suffix;
    foreach (['', '-wal', '-shm'] as $tail) {
        if (is_file($path . $tail)) {
            @unlink($path . $tail);
        }
    }
    $GLOBALS['probeTestTempFiles'][] = $path;

    return $path;
}

function probe_test_track_process($process): void
{
    $GLOBALS['probeTestProcesses'][] = $process;
}

function probe_test_cleanup(): void
{
    foreach ((array) $GLOBALS['probeTestProcesses'] as $process) {
        if (!is_resource($process)) {
            continue;
        }
        $status = proc_get_status($process);
        $pid = (int) ($status['pid'] ?? 0);
        if ($pid > 0) {
            // /T 连子进程一起收掉：windows.php 会再 spawn 出真正监听端口的 worker，
            // 只杀父进程会留下一个占着端口的孤儿进程。
            @shell_exec('taskkill /F /T /PID ' . $pid . ' 2>NUL');
        }
        @proc_terminate($process);
        @proc_close($process);
    }
    $GLOBALS['probeTestProcesses'] = [];

    // 必须还原 .env：这是开发者的真实配置文件，哪怕测试中途崩了也不能留在被改过的状态
    $backup = $GLOBALS['probeTestEnvBackup'] ?? null;
    if (is_array($backup) && is_file((string) $backup[0])) {
        @file_put_contents((string) $backup[0], (string) $backup[1]);
        $GLOBALS['probeTestEnvBackup'] = null;
    }

    foreach ((array) $GLOBALS['probeTestTempFiles'] as $file) {
        foreach (['', '-wal', '-shm'] as $tail) {
            if (is_file($file . $tail)) {
                @unlink($file . $tail);
            }
        }
    }
}

register_shutdown_function('probe_test_cleanup');

// ═══════════════════════════════════════════════════════════
// 环境
// ═══════════════════════════════════════════════════════════

/**
 * 指向一个临时 SQLite 库并加载框架。
 *
 * ⚠️ 这里**不能**靠 putenv('DB_DSN=...') 来隔离数据库：
 *    support/bootstrap.php 用 Dotenv 的 **mutable** 模式加载 .env，
 *    而 mutable 的含义就是「覆盖已存在的环境变量」——
 *    于是 .env 里那行 `DB_DSN=`（空值）会把我们设的值冲掉，
 *    最终所有进程都连到 runtime/aqua.sqlite，也就是**开发者的库**。
 *    （实测过：临时库文件根本不会被创建，数据全落进开发库。）
 *
 * 所以改成**临时改写 .env 里的 DB_DSN**，收尾时按原样还原：
 *     · 测试进程自己读 .env，连到临时库
 *     · 子进程（应用服务、任务执行器）也读同一个 .env，同样连到临时库
 *   这是唯一能同时覆盖「父进程 + 子进程」的写法。
 */
function probe_test_bootstrap(string $dbFile): void
{
    $GLOBALS['probeTestTempFiles'][] = $dbFile;

    $envPath = dirname(__DIR__) . DIRECTORY_SEPARATOR . '.env';
    if (!is_file($envPath)) {
        fwrite(STDERR, "找不到 .env，无法把测试指向临时库\n");
        exit(1);
    }
    $original = (string) file_get_contents($envPath);
    $GLOBALS['probeTestEnvBackup'] = [$envPath, $original];

    $dsn = 'sqlite:' . $dbFile;
    $updated = preg_replace('/^DB_DSN=.*$/m', 'DB_DSN=' . $dsn, $original, 1, $count);
    if (!is_string($updated)) {
        fwrite(STDERR, "改写 .env 失败\n");
        exit(1);
    }
    if ($count === 0) {
        $updated = rtrim($original, "\r\n") . "\nDB_DSN=" . $dsn . "\n";
    }
    file_put_contents($envPath, $updated);
    putenv('DB_DSN=' . $dsn);

    if (!class_exists('app\\common\\Db')) {
        require dirname(__DIR__) . '/vendor/autoload.php';
        require dirname(__DIR__) . '/support/bootstrap.php';
    }

    // 加载完之后再确认一次：如果这里对不上，说明隔离没生效，
    // 与其让测试悄悄污染开发库，不如立刻停下
    $active = trim((string) (getenv('DB_DSN') ?: ''));
    if ($active !== $dsn) {
        fwrite(STDERR, "数据库隔离未生效（DB_DSN 实际是「{$active}」），已中止以免污染开发库\n");
        exit(1);
    }
}

// ═══════════════════════════════════════════════════════════
// 模拟上游
// ═══════════════════════════════════════════════════════════

/**
 * 起一个本地模拟上游，按「模型名前缀」决定回什么。
 *
 * 之所以用模型名当剧本：探测内核只关心「上游回了什么」，
 * 把各种响应形态挂在模型名上，就能在一条渠道里覆盖全部分支。
 *
 * @return array{port:int, logFile:string}
 */
function probe_mock_start(int $delayMs = 0): array
{
    $logFile = probe_test_temp('mock-log', '.txt');
    $routerFile = probe_test_temp('mock-router', '.php');
    $serverLog = probe_test_temp('mock-server', '.log');

    $source = <<<'PHP'
<?php
$logFile = __LOG_FILE__;
$delayMs = __DELAY_MS__;
$path = (string) (parse_url($_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH) ?: '/');
$decoded = json_decode((string) file_get_contents('php://input'), true);
$model = is_array($decoded) ? (string) ($decoded['model'] ?? '') : '';
$auth = (string) ($_SERVER['HTTP_AUTHORIZATION'] ?? ($_SERVER['HTTP_API_KEY'] ?? ''));
if ($logFile !== '') {
    file_put_contents($logFile, $path . ' | ' . $model . ' | ' . $auth . "\n", FILE_APPEND);
}
if ($delayMs > 0) {
    usleep($delayMs * 1000);
}
header('Content-Type: application/json');

// 整条渠道鉴权失败：用来验证「立即中止、不把好模型误判成不可用」
if (str_starts_with($path, '/authfail/')) {
    http_response_code(401);
    echo '{"error":"invalid api key"}';
    return;
}
if (str_starts_with($path, '/authfail403/')) {
    http_response_code(403);
    echo '{"status":403,"title":"Forbidden","detail":"Authorization failed"}';
    return;
}
// 拉模型清单：测活时 Channel::test() 会先 GET /models
if (str_ends_with($path, '/models')) {
    echo '{"data":[{"id":"ok-list-1"},{"id":"denied-list-1"},{"id":"ok-list-2"}]}';
    return;
}
if (str_starts_with($model, 'slow-')) {
    sleep(30);
    echo '{"choices":[]}';
    return;
}
if (str_starts_with($model, 'ok-')) {
    // 客户端要流式就回真正的 SSE：转发引擎对 SSE 与非流式走的是不同分支，
    // 拿一个 JSON 体冒充 SSE 会把「流式链路」测成假的
    if (!empty($decoded['stream'])) {
        header('Content-Type: text/event-stream');
        echo 'data: {"id":"mock","choices":[{"delta":{"content":"hi"}}]}' . "\n\n";
        echo 'data: {"id":"mock","choices":[{"delta":{},"finish_reason":"stop"}],'
            . '"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}' . "\n\n";
        echo "data: [DONE]\n\n";
        return;
    }
    echo '{"id":"mock","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}';
    return;
}
if (str_starts_with($model, 'denied-')) {
    http_response_code(404);
    echo '{"detail":"Function \'x\' not found for account"}';
    return;
}
if (str_starts_with($model, 'echoauth-')) {
    http_response_code(404);
    echo '{"detail":"no access; you sent ' . $auth . '"}';
    return;
}
if (str_starts_with($model, 'missing-')) {
    http_response_code(404);
    echo '404 page not found';
    return;
}
if (str_starts_with($model, 'badreq-')) {
    http_response_code(400);
    echo '{"error":"bad request"}';
    return;
}
if (str_starts_with($model, 'unprocessable-')) {
    http_response_code(422);
    echo '{"error":"unprocessable entity"}';
    return;
}
if (str_starts_with($model, 'ratelimit-')) {
    http_response_code(429);
    echo '{"error":"rate limited"}';
    return;
}
if (str_starts_with($model, 'boom-')) {
    http_response_code(500);
    echo 'boom';
    return;
}
http_response_code(400);
echo '{"error":"unknown model"}';
PHP;

    file_put_contents($routerFile, str_replace(
        ['__LOG_FILE__', '__DELAY_MS__'],
        [var_export($logFile, true), (string) max(0, $delayMs)],
        $source
    ));

    $port = probe_test_free_port();
    $process = proc_open(
        [PHP_BINARY, '-S', "127.0.0.1:{$port}", $routerFile],
        [1 => ['file', $serverLog, 'a'], 2 => ['file', $serverLog, 'a']],
        $pipes,
        dirname(__DIR__)
    );
    if (!is_resource($process)) {
        fwrite(STDERR, "无法启动模拟上游\n");
        exit(1);
    }
    probe_test_track_process($process);

    $ready = probe_test_wait(static function () use ($port): bool {
        $probe = @fsockopen('127.0.0.1', $port, $code, $message, 0.3);
        if ($probe === false) {
            return false;
        }
        fclose($probe);

        return true;
    }, 10000);

    if (!$ready) {
        fwrite(STDERR, '模拟上游起不来，日志：' . (string) @file_get_contents($serverLog) . "\n");
        exit(1);
    }

    return ['port' => $port, 'logFile' => $logFile];
}

/** @return array<int, string> 模拟上游收到的请求记录（一行一次） */
function probe_mock_requests(string $logFile, string $pathPrefix = ''): array
{
    if (!is_file($logFile)) {
        return [];
    }
    $lines = file($logFile, FILE_IGNORE_NEW_LINES | FILE_SKIP_EMPTY_LINES) ?: [];
    if ($pathPrefix === '') {
        return $lines;
    }

    return array_values(array_filter($lines, static fn (string $line): bool => str_starts_with($line, $pathPrefix)));
}

function probe_test_free_port(): int
{
    $socket = stream_socket_server('tcp://127.0.0.1:0', $errno, $errstr);
    if ($socket === false) {
        fwrite(STDERR, "无法分配本地端口：$errstr\n");
        exit(1);
    }
    $name = (string) stream_socket_get_name($socket, false);
    fclose($socket);

    return (int) substr($name, (int) strrpos($name, ':') + 1);
}

function probe_test_wait(callable $probe, int $timeoutMs = 60000, int $intervalMs = 150): bool
{
    $deadline = microtime(true) + $timeoutMs / 1000;
    while (microtime(true) < $deadline) {
        if ($probe()) {
            return true;
        }
        usleep($intervalMs * 1000);
    }

    return false;
}

// ═══════════════════════════════════════════════════════════
// 应用服务（真的 Webman，不是模拟）
// ═══════════════════════════════════════════════════════════

/**
 * 起一个真的 Webman 服务（连临时库）。
 *
 * @param int|null $port 指定端口（预览用）；留空则随机挑一个空闲端口
 * @return array{port:int}
 */
function probe_app_start(?int $port = null): array
{
    $port ??= probe_test_free_port();
    $logFile = probe_test_temp('app-server', '.log');

    // 用独立端口，避免和开发中的 8787 撞车；工作进程数降到 1，好回收
    putenv('HTTP_LISTEN=http://127.0.0.1:' . $port);
    putenv('HTTP_WORKER_COUNT=1');

    $process = proc_open(
        [PHP_BINARY, dirname(__DIR__) . '/windows.php'],
        [1 => ['file', $logFile, 'a'], 2 => ['file', $logFile, 'a']],
        $pipes,
        dirname(__DIR__)
    );
    if (!is_resource($process)) {
        fwrite(STDERR, "无法启动应用服务\n");
        exit(1);
    }
    probe_test_track_process($process);

    $ready = probe_test_wait(static function () use ($port): bool {
        $response = probe_http($port, 'GET', '/healthz');

        return $response['status'] === 200;
    }, 40000, 250);

    if (!$ready) {
        fwrite(STDERR, '应用服务起不来，日志：' . mb_substr((string) @file_get_contents($logFile), -2000) . "\n");
        exit(1);
    }

    return ['port' => $port];
}

// ═══════════════════════════════════════════════════════════
// HTTP 会话
// ═══════════════════════════════════════════════════════════

/**
 * @param array<string, mixed> $post
 * @return array{status:int, body:string, location:string, headers:string}
 */
function probe_http(int $port, string $method, string $path, array $post = [], string $cookieJar = ''): array
{
    $ch = curl_init('http://127.0.0.1:' . $port . $path);
    $options = [
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_HEADER => true,
        CURLOPT_CUSTOMREQUEST => strtoupper($method),
        CURLOPT_TIMEOUT => 60,
    ];
    if ($post !== []) {
        // 用表单编码而不是 multipart：这才是浏览器提交表单的真实形态，
        // 数组字段会编码成 models[0]=…，与页面上的 name="models[]" 对得上
        $options[CURLOPT_POSTFIELDS] = http_build_query($post);
        $options[CURLOPT_HTTPHEADER] = ['Content-Type: application/x-www-form-urlencoded'];
    }
    if ($cookieJar !== '') {
        $options[CURLOPT_COOKIEJAR] = $cookieJar;
        $options[CURLOPT_COOKIEFILE] = $cookieJar;
    }
    curl_setopt_array($ch, $options);
    $raw = (string) curl_exec($ch);
    $error = curl_error($ch);
    curl_close($ch);

    if ($error !== '') {
        return ['status' => 0, 'body' => $error, 'location' => '', 'headers' => ''];
    }

    $split = strpos($raw, "\r\n\r\n");
    $head = $split === false ? $raw : substr($raw, 0, $split);
    $body = $split === false ? '' : substr($raw, $split + 4);
    $status = 0;
    $location = '';
    foreach (explode("\r\n", $head) as $line) {
        if (preg_match('#^HTTP/\S+\s+(\d{3})#', $line, $m)) {
            $status = (int) $m[1];
        }
        if (stripos($line, 'location:') === 0) {
            $location = trim(substr($line, 9));
        }
    }

    return ['status' => $status, 'body' => $body, 'location' => $location, 'headers' => $head];
}

/** 从页面里取出 CSRF 令牌 */
function probe_csrf(int $port, string $path, string $cookieJar): string
{
    $page = probe_http($port, 'GET', $path, [], $cookieJar);
    if (preg_match('/name="_csrf"\s+value="([^"]+)"/', $page['body'], $m)) {
        return $m[1];
    }

    return '';
}

/** 登录后台，成功后 Cookie 落在 $cookieJar 里 */
function probe_login(int $port, string $cookieJar, string $password): bool
{
    $token = probe_csrf($port, '/admin/login', $cookieJar);
    if ($token === '') {
        return false;
    }
    $response = probe_http($port, 'POST', '/admin/login', ['_csrf' => $token, 'password' => $password], $cookieJar);

    return $response['status'] === 302 && $response['location'] === '/admin';
}

// ═══════════════════════════════════════════════════════════
// 数据播种
// ═══════════════════════════════════════════════════════════

function probe_seed_admin(string $password): void
{
    app\common\Admin::setPassword($password);
}

/**
 * @param array<int, string> $models
 * @param array<int, string> $keys
 */
function probe_seed_channel(string $name, string $baseUrl, array $models, array $keys): int
{
    app\common\Db::execute(
        'INSERT INTO channels (name, type, base_url, models, status, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)',
        [$name, 'openai', $baseUrl, json_encode($models), time(), time()]
    );
    $channelId = (int) app\common\Db::pdo()->lastInsertId();

    foreach ($keys as $key) {
        app\common\Db::execute(
            'INSERT INTO channel_keys (channel_id, key_hash, api_key_enc, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)',
            [
                $channelId,
                hash('sha256', $key),
                app\common\Crypto::encrypt($key),
                app\common\ChannelKey::STATUS_ENABLED,
                time(),
                time(),
            ]
        );
    }

    return $channelId;
}
