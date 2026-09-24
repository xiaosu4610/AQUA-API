<?php
/**
 * 安装向导
 *
 * 路由：
 *   GET  /install        安装页（含环境检查与配置表单）
 *   POST /install        执行安装
 *   别名 /install.php    照顾「常规 PHP 项目」的使用习惯
 *
 * ═══ 为什么是 Webman 路由，而不是根目录放一个 install.php ═══
 *
 * 本项目的部署形态是 Nginx 反向代理到常驻内存的 Webman 进程，
 * **没有任何 PHP-FPM/CGI** —— 因此放在 public/ 下的 .php 文件
 * 根本不会被执行，只会被当成静态文件下载（那反而会泄露安装代码）。
 * 所以安装程序必须是应用内的路由。
 * 同时保留 /install.php 这个路径别名，让人按习惯也能打开。
 *
 * ═══ 装完就锁 ═══
 *
 * 安装成功后写入 runtime/install.lock，之后访问安装页只会看到一个
 * 「已安装」的说明页 —— 不再提供任何可执行的表单。
 * 之所以不采取「物理删除自己」，是因为代码是随版本分发的，
 * 删掉下次升级又回来了，反而留下一个无人知晓的风险点。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Admin;
use app\common\Csrf;
use app\common\Db;
use app\common\EnvFile;
use app\common\InstallState;
use app\common\Schema;
use app\common\Settings;
use app\process\Monitor;
use PDO;
use PDOException;
use support\Request;
use support\Response;
use Throwable;
use Workerman\Timer;

class InstallController
{
    /** 管理员密码最短长度，与其它校验、脚本重置保持一致 */
    private const MIN_PASSWORD_LENGTH = 8;

    /**
     * GET /install —— 安装页
     */
    public function index(Request $request): Response
    {
        if (InstallState::isInstalled()) {
            return $this->render('locked', [
                'lockInfo' => InstallState::info(),
            ]);
        }

        return $this->render('form', [
            'checks' => $this->checks(),
            'old' => $this->defaults($request),
            'error' => '',
            // 检测到已有 SQLite 数据时给出提醒（不会丢，但需要手工迁移）
            'legacySqlite' => $this->detectLegacySqlite(),
        ]);
    }

    /**
     * POST /install —— 执行安装
     */
    public function run(Request $request): Response
    {
        if (InstallState::isInstalled()) {
            return $this->render('locked', ['lockInfo' => InstallState::info()]);
        }

        if (!Csrf::check($request->post('_csrf'))) {
            return $this->fail($request, '页面已过期，请重新提交');
        }

        $input = [
            'db_type' => (string) $request->post('db_type', 'sqlite'),
            'db_host' => trim((string) $request->post('db_host', '')),
            'db_port' => trim((string) $request->post('db_port', '')),
            'db_name' => trim((string) $request->post('db_name', '')),
            'db_user' => trim((string) $request->post('db_user', '')),
            'db_password' => (string) $request->post('db_password', ''),
            'site_name' => trim((string) $request->post('site_name', '')),
            'site_domain' => trim((string) $request->post('site_domain', '')),
            'site_mode' => (string) $request->post('site_mode', 'commercial'),
            'admin_password' => (string) $request->post('admin_password', ''),
            'admin_password2' => (string) $request->post('admin_password2', ''),
            'https' => (string) $request->post('https', '') !== '',
        ];

        $error = $this->validate($input);
        if ($error !== '') {
            return $this->fail($request, $error, $input);
        }

        // 环境不达标就别往下走 —— 装到一半失败比一开始就拦住更糟
        foreach ($this->checks() as $check) {
            if ($check['required'] && !$check['ok']) {
                return $this->fail($request, '环境检查未通过：' . $check['label'] . '（' . $check['need'] . '）', $input);
            }
        }

        // ── ① 数据库连通性（MySQL 时才需要真连一次）──
        $dsn = '';
        $dbLabel = 'SQLite（文件）';

        if ($input['db_type'] === 'mysql') {
            $probe = $this->testMysql($input);
            if (!$probe['ok']) {
                return $this->fail($request, '数据库连接失败：' . $probe['error'], $input);
            }

            $dsn = $probe['dsn'];
            $dbLabel = 'MySQL · ' . $input['db_host'] . ':' . ($input['db_port'] ?: '3306') . '/' . $input['db_name'];
        }

        // ── ② 写 .env ──
        // APP_KEY 在这里生成：它用于加密上游 Key，一旦有数据就不能再换
        $appKey = bin2hex(random_bytes(32));

        $values = [
            'APP_DEBUG' => 'false',
            'APP_TIMEZONE' => 'Asia/Shanghai',
            'APP_KEY' => $appKey,
            // 会话 Cookie 是否只允许 HTTPS。按安装时的访问方式自动判断，
            // 判断错的后果很明确：走 http 却设成 true，浏览器不会保存 Cookie，
            // 表现为「登录成功却立刻跳回登录页」
            'SESSION_SECURE' => $input['https'] ? 'true' : 'false',
            'DB_DSN' => $dsn,
            'DB_USER' => $input['db_type'] === 'mysql' ? $input['db_user'] : '',
            'DB_PASSWORD' => $input['db_type'] === 'mysql' ? $input['db_password'] : '',
            'SITE_MODE' => $input['site_mode'],
        ];

        $backup = null;

        // ⚠️ 安装期间必须**暂停文件监视器**。
        //
        // 踩过的坑：安装要写 .env，而 Webman 的 Monitor 进程会盯着 .env 的
        // 修改时间，一发现变化就通知主进程重载工作进程 —— 重载会掐断
        // 正在处理的这次请求，浏览器侧表现为
        // 「Unable to read data from the transport connection」，
        // 而 .env 已经写了一半、表也没建完，留下一地鸡毛。
        // 框架自带 Monitor::pause()/resume() 正是为此准备的。
        $monitorPaused = $this->pauseMonitor();

        try {
            $backup = EnvFile::backup();
            EnvFile::write($values);
        } catch (Throwable $e) {
            $this->resumeMonitorLater($monitorPaused);

            return $this->fail($request, $e->getMessage(), $input);
        }

        // 让**当前进程**立刻认这批新值。
        // 不做这一步，Db 会继续按旧的 getenv 结果连库
        foreach ($values as $key => $value) {
            putenv($key . '=' . $value);
        }

        // ── ③ 建表 + 建管理员 + 写站点配置 ──
        try {
            Db::reset();
            Schema::ensure();

            // 配置有进程内缓存，切库后必须清掉，否则读到的还是旧库的值
            Settings::forget();

            Settings::put('site.name', $input['site_name']);
            Settings::put('site.domain', $input['site_domain']);
            Settings::put('site.mode', $input['site_mode']);

            // 管理员直接写库，**不把密码留在 .env 里** ——
            // .env 是长期存在的文件，少一处明文就少一处泄露面
            Admin::setPassword($input['admin_password']);
        } catch (Throwable $e) {
            $this->resumeMonitorLater($monitorPaused);

            return $this->fail(
                $request,
                '初始化数据库失败：' . $e->getMessage()
                . ($backup !== null ? '（原 .env 已备份为 ' . basename($backup) . '，可自行恢复）' : ''),
                $input
            );
        }

        // ── ④ 上锁 ──
        InstallState::markInstalled([
            'database' => $dbLabel,
            'site_name' => $input['site_name'],
            'site_domain' => $input['site_domain'],
            'site_mode' => $input['site_mode'],
        ]);

        // 安装已全部完成，可以把监视器放回去了（延迟恢复，见方法注释）
        $this->resumeMonitorLater($monitorPaused);

        return $this->render('done', [
            'dbLabel' => $dbLabel,
            'siteName' => $input['site_name'],
            'siteDomain' => $input['site_domain'],
            'https' => $input['https'],
            'backup' => $backup,
            // 用相对地址展示，避免猜错协议/域名给站长一个打不开的链接
            'adminUrl' => '/admin/login',
        ]);
    }

    // ═══════════════════════════════════════════════════════════
    // 内部
    // ═══════════════════════════════════════════════════════════

    /**
     * 暂停文件监视器。
     *
     * 返回 false 表示当前环境没有监视器（例如命令行运行），无需恢复。
     */
    private function pauseMonitor(): bool
    {
        try {
            Monitor::pause();

            return true;
        } catch (Throwable) {
            // 拿不到监视器也不影响安装本身，最坏情况是触发一次重载
            return false;
        }
    }

    /**
     * 延迟恢复文件监视器。
     *
     * 为什么不是立刻恢复：一旦恢复，监视器下一次 tick（每秒一次）就会发现
     * .env 的修改时间变了并触发重载 —— 而这次请求的响应可能还没发出去，
     * 结果又是「装完了但页面打不开」。给它 3 秒，让响应先落地。
     *
     * 万一这个进程中途消失、定时器没跑到，也不会留下永久暂停：
     * Monitor 的构造函数里会调用 resume()，即下次监视器启动时自动恢复。
     */
    private function resumeMonitorLater(bool $paused): void
    {
        if (!$paused) {
            return;
        }

        try {
            Timer::add(3, static function (): void {
                Monitor::resume();
            }, [], false);
        } catch (Throwable) {
            // 定时器不可用就直接恢复，宁可冒一次重载的风险
            Monitor::resume();
        }
    }

    /**
     * 渲染安装相关页面。
     *
     * @param array<string, mixed> $vars
     */
    private function render(string $stage, array $vars = []): Response
    {
        return view('install/index', array_merge([
            'csrf' => Csrf::token(),
            'stage' => $stage,
            'appVersion' => InstallState::appVersion(),
        ], $vars), '');
    }

    /**
     * 安装失败：回到表单页并把已填内容带回去。
     *
     * 不回填的话，站长每改一个字段就要把所有数据库参数重打一遍 ——
     * 这种体验会让人直接放弃安装。
     *
     * @param array<string, mixed> $old
     */
    private function fail(Request $request, string $error, array $old = []): Response
    {
        return $this->render('form', [
            'checks' => $this->checks(),
            'old' => $old === [] ? $this->defaults($request) : array_merge($this->defaults($request), $old),
            'error' => $error,
            'legacySqlite' => $this->detectLegacySqlite(),
        ]);
    }

    /**
     * 表单默认值。
     *
     * @return array<string, mixed>
     */
    private function defaults(Request $request): array
    {
        return [
            'db_type' => 'sqlite',
            'db_host' => '127.0.0.1',
            'db_port' => '3306',
            'db_name' => 'aqua',
            'db_user' => '',
            'db_password' => '',
            'site_name' => 'aqua-api-php',
            'site_domain' => '',
            'site_mode' => 'commercial',
            'admin_password' => '',
            'admin_password2' => '',
            // 自动判断：反代场景看 X-Forwarded-Proto，直连看请求 scheme
            'https' => $this->isHttps($request),
        ];
    }

    /**
     * 判断当前访问是否走 HTTPS。
     *
     * 必须同时看 X-Forwarded-Proto：生产环境是 Nginx 反代，
     * Webman 收到的是 http，但对外是 https —— 只看 scheme 会把
     * SESSION_SECURE 误判成 false。
     */
    private function isHttps(Request $request): bool
    {
        $proto = strtolower((string) $request->header('x-forwarded-proto', ''));

        if ($proto !== '') {
            return str_contains($proto, 'https');
        }

        return $request->header('https', '') === 'on'
            || (string) $request->header('x-forwarded-ssl', '') === 'on';
    }

    /**
     * 环境检查项。
     *
     * required = false 的项只提示、不阻断（例如 Windows 下没有 pcntl）。
     *
     * @return array<int, array{label:string, ok:bool, value:string, need:string, required:bool}>
     */
    private function checks(): array
    {
        $checks = [];

        $checks[] = [
            'label' => 'PHP 版本',
            'ok' => PHP_VERSION_ID >= 80100,
            'value' => PHP_VERSION,
            'need' => '>= 8.1',
            'required' => true,
        ];

        foreach (['pdo', 'curl', 'mbstring', 'openssl', 'json'] as $ext) {
            $loaded = extension_loaded($ext);
            $checks[] = [
                'label' => "扩展 {$ext}",
                'ok' => $loaded,
                'value' => $loaded ? '已启用' : '未启用',
                'need' => '必须启用',
                'required' => true,
            ];
        }

        // 两种数据库驱动至少有一个能用即可
        $sqlite = extension_loaded('pdo_sqlite');
        $mysql = extension_loaded('pdo_mysql');
        $checks[] = [
            'label' => '数据库驱动',
            'ok' => $sqlite || $mysql,
            'value' => ($sqlite ? 'pdo_sqlite ' : '') . ($mysql ? 'pdo_mysql' : '') ?: '都没有',
            'need' => 'pdo_sqlite 或 pdo_mysql 至少一个',
            'required' => true,
        ];

        $rootWritable = is_writable(base_path());
        $checks[] = [
            'label' => '项目根目录可写',
            'ok' => $rootWritable,
            'value' => $rootWritable ? '可写' : '不可写',
            'need' => '需要写入 .env',
            'required' => true,
        ];

        $runtime = runtime_path();
        if (!is_dir($runtime)) {
            @mkdir($runtime, 0755, true);
        }
        $runtimeWritable = is_dir($runtime) && is_writable($runtime);
        $checks[] = [
            'label' => 'runtime/ 可写',
            'ok' => $runtimeWritable,
            'value' => $runtimeWritable ? '可写' : '不可写',
            'need' => '会话、日志、锁文件都放这里',
            'required' => true,
        ];

        // Windows 没有 pcntl/posix，这两个只在 Linux 多进程下需要
        $isWindows = DIRECTORY_SEPARATOR === '\\';
        $checks[] = [
            'label' => '扩展 pcntl / posix',
            'ok' => $isWindows || (extension_loaded('pcntl') && extension_loaded('posix')),
            'value' => $isWindows ? 'Windows 下不需要' : (extension_loaded('pcntl') ? '已启用' : '未启用'),
            'need' => 'Linux 多进程运行需要',
            'required' => false,
        ];

        return $checks;
    }

    /**
     * 校验表单输入，返回错误信息（空串表示通过）。
     *
     * @param array<string, mixed> $input
     */
    private function validate(array $input): string
    {
        if (!in_array($input['db_type'], ['sqlite', 'mysql'], true)) {
            return '数据库类型不合法';
        }

        if ($input['db_type'] === 'mysql') {
            if ($input['db_host'] === '') {
                return 'MySQL 主机地址不能为空';
            }

            if ($input['db_name'] === '') {
                return '数据库名不能为空';
            }

            // 库名会拼进 CREATE DATABASE 语句，必须严格限制字符集
            if (preg_match('/^[A-Za-z0-9_]+$/', $input['db_name']) !== 1) {
                return '数据库名只能包含字母、数字与下划线';
            }

            if ($input['db_user'] === '') {
                return 'MySQL 用户名不能为空';
            }

            if ($input['db_port'] !== '' && !ctype_digit($input['db_port'])) {
                return '端口必须是数字';
            }
        }

        if ($input['site_name'] === '') {
            return '站点名称不能为空';
        }

        if (!in_array($input['site_mode'], ['commercial', 'public_welfare'], true)) {
            return '站点模式不合法';
        }

        if (strlen($input['admin_password']) < self::MIN_PASSWORD_LENGTH) {
            return '管理员密码太短，至少 ' . self::MIN_PASSWORD_LENGTH . ' 位';
        }

        if ($input['admin_password'] !== $input['admin_password2']) {
            return '两次输入的管理员密码不一致';
        }

        return '';
    }

    /**
     * 测试 MySQL 连接；库不存在时尝试自动创建。
     *
     * 自动创建是刻意的：让站长为了装一个程序先去手敲 CREATE DATABASE
     * 是一件不必要的事，而这一步失败的报错（Unknown database）
     * 对不熟悉数据库的人来说完全不指向解决办法。
     *
     * @param array<string, mixed> $input
     * @return array{ok:bool, error:string, dsn:string}
     */
    private function testMysql(array $input): array
    {
        $host = $input['db_host'];
        $port = $input['db_port'] ?: '3306';
        $name = $input['db_name'];

        $options = [
            PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
            PDO::ATTR_EMULATE_PREPARES => false,
        ];

        $dsn = "mysql:host={$host};port={$port};dbname={$name};charset=utf8mb4";

        try {
            new PDO($dsn, $input['db_user'], $input['db_password'], $options);

            return ['ok' => true, 'error' => '', 'dsn' => $dsn];
        } catch (PDOException $e) {
            $isUnknownDb = str_contains($e->getMessage(), 'Unknown database')
                || (int) $e->getCode() === 1049;

            if (!$isUnknownDb) {
                return ['ok' => false, 'error' => $this->friendlyMysqlError($e), 'dsn' => ''];
            }
        }

        // 库不存在：连到服务器层（不指定 dbname）建库后重试
        try {
            $server = new PDO("mysql:host={$host};port={$port};charset=utf8mb4", $input['db_user'], $input['db_password'], $options);
            // 库名已在校验阶段限制为 [A-Za-z0-9_]，反引号包裹是第二道保险
            $server->exec("CREATE DATABASE IF NOT EXISTS `{$name}` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci");
            $server = null;

            new PDO($dsn, $input['db_user'], $input['db_password'], $options);

            return ['ok' => true, 'error' => '', 'dsn' => $dsn];
        } catch (PDOException $e) {
            return [
                'ok' => false,
                'error' => '数据库「' . $name . '」不存在，且自动创建失败（'
                    . $this->friendlyMysqlError($e) . '）。请手工创建该库，或换一个有建库权限的账号',
                'dsn' => '',
            ];
        }
    }

    /**
     * 把 PDO 的英文报错翻成能指向解决办法的中文。
     *
     * 原始报错（如 SQLSTATE[HY000] [2002] Connection refused）对非专业用户
     * 几乎没有意义，而它背后通常就是那么几种情况。
     */
    private function friendlyMysqlError(PDOException $e): string
    {
        $message = $e->getMessage();

        return match (true) {
            str_contains($message, 'Connection refused') => '无法连接（连接被拒绝）。请确认 MySQL 已启动，且允许从本机连接',
            str_contains($message, 'Access denied') => '账号或密码不正确，或该账号没有从本机访问的权限',
            str_contains($message, 'getaddrinfo') || str_contains($message, 'Name or service not known') => '主机地址无法解析，请检查是否填错',
            str_contains($message, "Can't connect to MySQL server on") => '连不上 MySQL 服务器，请检查主机与端口',
            default => $message,
        };
    }

    /**
     * 检测是否已存在带数据的 SQLite 库。
     *
     * 只用于提示：换库**不会**自动迁移旧数据，
     * 站长需要知道这一点，否则会以为数据丢了。
     *
     * @return array{exists:bool, size:int}
     */
    private function detectLegacySqlite(): array
    {
        $file = runtime_path() . DIRECTORY_SEPARATOR . 'aqua.sqlite';

        if (!is_file($file)) {
            return ['exists' => false, 'size' => 0];
        }

        return ['exists' => true, 'size' => (int) filesize($file)];
    }
}
