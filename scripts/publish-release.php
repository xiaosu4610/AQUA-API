<?php
/**
 * 发行版发布（把打包好的 zip 挂到 Gitee / GitHub 的 Release 上）
 *
 * ═══ 为什么要有这个脚本 ═══
 *
 * 发行包是「后台自动更新」要拉的东西，光把 zip 留在本地磁盘上等于没发布 ——
 * 站长那边永远拉不到。以前这一步是手工去网页上点，容易漏、也容易传错文件。
 *
 * ═══ 凭据从哪来（重要） ═══
 *
 * 从 **git 的凭据管理器**里现取（`git credential fill`），
 * **不落任何文件、不写进仓库、也不打印出来**。这是项目底线：
 * 凭据一旦进仓库就等于向全世界公开。取不到凭据时脚本会明确告诉你
 * 「需要先给 git 配好这个平台的凭据」，而不是让你把 token 写到某个配置文件里。
 *
 * 用法（在项目根目录）：
 *   php scripts/publish-release.php --dry-run        # 只显示将要做什么
 *   php scripts/publish-release.php                  # 发布 Gitee + GitHub（有凭据的平台才发）
 *   php scripts/publish-release.php --only=gitee     # 只发 Gitee
 *
 * 前置条件：
 *   1. 版本号已经改好（composer.json 的 version）
 *   2. 已经打包：php scripts/build-release.php
 *   3. 标签已经推到远端（Release 要挂在标签上）
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

$args = aqua_args();
$dryRun = isset($args['dry-run']);
$only = (string) ($args['only'] ?? '');

$root = aqua_root();
$version = trim((string) json_decode((string) @file_get_contents($root . '/composer.json'), true)['version'] ?? '');
$zip = $root . '/build/aqua-api-php-' . $version . '.zip';
$shaFile = $zip . '.sha256';

if ($version === '') {
    fwrite(STDERR, "读不到 composer.json 里的版本号\n");
    exit(1);
}
if (!is_file($zip) || !is_file($shaFile)) {
    fwrite(STDERR, "找不到发行包：{$zip}\n请先运行 php scripts/build-release.php\n");
    exit(1);
}

// .sha256 文件的内容形如「<hash>  <文件名>」，这里只取哈希部分
$sha = (string) (preg_split('/\s+/', trim((string) file_get_contents($shaFile)) ?: '')[0] ?? '');
$tag = 'v' . $version;

/**
 * 从 git 凭据管理器取某个平台的凭据（取不到返回 null）。
 *
 * 为什么用 git credential fill 而不是读某个配置文件：
 * 那个 helper 是系统级的凭据存储（Windows 凭据管理器 / macOS 钥匙串），
 * 凭据只在这条管道里出现一次，不落盘。取不到就说明维护者还没配，
 * 此时**不要**退化成「让用户把 token 贴到文件里」——那正是要避免的事。
 *
 * @return array{user:string, secret:string}|null
 */
function release_credential(string $host, ?string $user = null): ?array
{
    $input = "protocol=https\nhost={$host}\n" . ($user !== null ? "username={$user}\n" : '') . "\n";

    $descriptors = [0 => ['pipe', 'r'], 1 => ['pipe', 'w'], 2 => ['pipe', 'w']];
    $process = proc_open('git credential fill', $descriptors, $pipes, aqua_root());
    if (!is_resource($process)) {
        return null;
    }

    fwrite($pipes[0], $input);
    fclose($pipes[0]);
    $output = (string) stream_get_contents($pipes[1]);
    fclose($pipes[1]);
    fclose($pipes[2]);
    proc_close($process);

    $user = null;
    $secret = null;
    foreach (explode("\n", $output) as $line) {
        if (str_starts_with($line, 'username=')) {
            $user = substr($line, 9);
        }
        if (str_starts_with($line, 'password=')) {
            $secret = substr($line, 9);
        }
    }

    return ($user !== null && $secret !== null && $secret !== '') ? ['user' => $user, 'secret' => $secret] : null;
}

/**
 * 发一个 HTTP 请求（返回状态码与响应体）。
 *
 * @param array<int, string> $headers
 * @param array<string, mixed>|null $json
 * @return array{code:int, body:string}
 */
function release_http(string $method, string $url, array $headers = [], ?array $json = null, ?string $uploadFile = null): array
{
    $ch = curl_init($url);
    $options = [
        CURLOPT_CUSTOMREQUEST => $method,
        CURLOPT_HTTPHEADER => $headers,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT => 180,
    ];

    if ($json !== null) {
        $options[CURLOPT_POSTFIELDS] = (string) json_encode($json, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);
    }
    if ($uploadFile !== null) {
        $options[CURLOPT_POSTFIELDS] = ['file' => new CURLFile($uploadFile)];
    }

    curl_setopt_array($ch, $options);
    $body = (string) curl_exec($ch);
    $code = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
    curl_close($ch);

    return ['code' => $code, 'body' => $body];
}

$notes = <<<TEXT
发行版 {$version}

本包为**整包**（含 vendor/），可直接覆盖到现有安装的根目录完成升级。
升级前请先备份数据库与 .env（.env 不在包内，不会被覆盖）。

校验：SHA-256 {$sha}

本次更新要点见仓库提交记录与 tag {$tag}。
TEXT;

echo "════════════════════════════════════════════════════════\n";
echo "发布发行版 {$version}\n";
echo "════════════════════════════════════════════════════════\n";
echo "包文件    {$zip}\n";
echo '大小      ' . round(filesize($zip) / 1048576, 2) . " MB\n";
echo "SHA-256   {$sha}\n";
echo "标签      {$tag}\n";
echo '模式      ' . ($dryRun ? "演练（不会真的发布）" : '真实发布') . "\n\n";

$targets = [];

// ── Gitee ──
if ($only === '' || $only === 'gitee') {
    $cred = release_credential('gitee.com');
    if ($cred === null) {
        echo "[Gitee] 跳过：没有取到凭据（先在 git 里登录/存一次 Gitee 凭据）\n";
    } else {
        $targets[] = ['name' => 'Gitee', 'host' => 'gitee.com', 'user' => $cred['user'], 'token' => $cred['secret']];
    }
}

// ── GitHub ──
if ($only === '' || $only === 'github') {
    $cred = release_credential('github.com');
    if ($cred === null) {
        echo "[GitHub] 跳过：没有取到凭据\n";
    } else {
        $targets[] = ['name' => 'GitHub', 'host' => 'github.com', 'user' => $cred['user'], 'token' => $cred['secret']];
    }
}

if ($targets === []) {
    echo "没有任何可发布的平台，结束。\n";
    exit(1);
}

foreach ($targets as $target) {
    $isGitee = $target['name'] === 'Gitee';
    $repo = $isGitee ? 'xiaosu4610/aqua-api-php' : 'xiaosu4610/AQUA-API-PHP';

    echo "── {$target['name']}（{$repo}）──\n";

    if ($isGitee) {
        $api = 'https://gitee.com/api/v5/repos/' . $repo;
        $auth = ['access_token' => $target['token']];
    } else {
        $api = 'https://api.github.com/repos/' . $repo;
        $auth = [];
        // ⚠️ User-Agent 是必须的：GitHub 的 API 会对没有 UA 的请求直接 403
        // （提示「forbidden by administrative rules」），而这句话很容易被
        // 误读成「token 没权限」，白查半天
        $authHeader = [
            'Authorization: Bearer ' . $target['token'],
            'User-Agent: aqua-api-php-release',
            'Accept: application/vnd.github+json',
            'X-GitHub-Api-Version: 2022-11-28',
        ];
    }

    if ($dryRun) {
        echo "  将创建 Release：{$tag}，名称「{$version}」，并上传 " . basename($zip) . "\n";
        continue;
    }

    // ① 建 Release
    if ($isGitee) {
        $payload = [
            'access_token' => $target['token'],
            'tag_name' => $tag,
            'name' => $version,
            'body' => $notes,
            'target_commitish' => 'main',
        ];
        $response = release_http('POST', $api . '/releases', ['Content-Type: application/json'], $payload);
    } else {
        $payload = [
            'tag_name' => $tag,
            'name' => $version,
            'body' => $notes,
            'target_commitish' => 'main',
        ];
        $response = release_http('POST', $api . '/releases', $authHeader + ['Content-Type: application/json'], $payload);
    }

    $created = json_decode($response['body'], true);
    $releaseId = (int) ($created['id'] ?? 0);

    if ($response['code'] >= 300 || $releaseId <= 0) {
        // 已经建过（重复发布）时，找出现有的 release 直接补附件
        echo "  创建返回 HTTP {$response['code']}，" . mb_substr($response['body'], 0, 200) . "\n";
        if ($isGitee) {
            $list = release_http('GET', $api . '/releases?access_token=' . rawurlencode($target['token']) . '&per_page=100');
        } else {
            $list = release_http('GET', $api . '/releases?per_page=100', $authHeader);
        }
        foreach ((array) json_decode($list['body'], true) as $release) {
            if ((string) ($release['tag_name'] ?? '') === $tag) {
                $releaseId = (int) ($release['id'] ?? 0);
                echo "  找到已有 Release #{$releaseId}，改为补传附件\n";
                break;
            }
        }
        if ($releaseId <= 0) {
            echo "  **发布失败**：既建不出来也找不到已有的 Release\n";
            continue;
        }
    } else {
        echo "  已创建 Release #{$releaseId}\n";
    }

    // ② 上传附件
    if ($isGitee) {
        $upload = release_http(
            'POST',
            $api . '/releases/' . $releaseId . '/attach_files?access_token=' . rawurlencode($target['token']),
            [],
            null,
            $zip
        );
    } else {
        $upload = release_http(
            'POST',
            'https://uploads.github.com/repos/' . $repo . '/releases/' . $releaseId
                . '/assets?name=' . rawurlencode(basename($zip)),
            $authHeader + ['Content-Type: application/octet-stream'],
            null,
            $zip
        );
    }

    echo $upload['code'] < 300
        ? "  附件已上传：" . basename($zip) . "\n"
        : "  附件上传返回 HTTP {$upload['code']}：" . mb_substr($upload['body'], 0, 200) . "\n";

    // ③ 同时把 .sha256 也传上去（便于下载后校验）
    if ($isGitee) {
        $uploadSha = release_http('POST', $api . '/releases/' . $releaseId . '/attach_files?access_token=' . rawurlencode($target['token']), [], null, $shaFile);
    } else {
        $uploadSha = release_http(
            'POST',
            'https://uploads.github.com/repos/' . $repo . '/releases/' . $releaseId . '/assets?name=' . rawurlencode(basename($shaFile)),
            $authHeader + ['Content-Type: application/octet-stream'],
            null,
            $shaFile
        );
    }
    echo $uploadSha['code'] < 300 ? "  校验文件已上传\n" : "  校验文件上传返回 HTTP {$uploadSha['code']}\n";
}

echo "\n完成。发布页：\n";
echo "  Gitee  https://gitee.com/xiaosu4610/aqua-api-php/releases\n";
echo "  GitHub https://github.com/xiaosu4610/AQUA-API-PHP/releases\n";
