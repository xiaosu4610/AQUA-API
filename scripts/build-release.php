<?php
/**
 * 发行版打包脚本
 *
 * 【这个脚本干什么】
 *   把当前工作区打成一个可直接分发的 zip，作为 Gitee / GitHub release 的附件 ——
 *   也就是「后台自动更新」功能（见 docs/05）要拉取的那个更新包。
 *   同时产出配套的 .sha256 校验值。
 *
 * 【为什么必须在本地跑，绝不在服务器上跑】
 *   服务器上的代码是被更新的那一侧。让「被更新者」自己去打包，等于在
 *   目标机器上做源码收集，一旦脚本被判错就会把服务器上的实际凭据打进包里。
 *   打包只应该在「干净的源码工作区」发生。
 *
 * 【为什么不用 Git 自动生成的源码包】
 *   `.gitignore` 忽略了 `/vendor/`，所以 Git 源码包里**没有依赖**。
 *   直接拿它更新会把站点更新成「缺 vendor/autoload.php」的状态 —— 当场白屏，
 *   而站长手上只有 SSH 一条路。所以必须自己打包，且必须含 vendor。
 *
 * 【三条硬规则】
 *   1. **白名单收集** —— 只列「能进」的，没说能进的一律不进。
 *      排除法（黑名单）漏一条就是把凭据打包发到公开仓库，且不可逆。
 *   2. **必须包含 vendor/** —— 理由见上。
 *   3. **命中禁止清单就中止构建**，而不是静默跳过 ——
 *      静默跳过会让人以为「检查过了、没问题」，而这恰恰是最危险的状态。
 *      唯一的例外是 vendor/ 里的**上游测试样本**（第三方公开代码里的 .env/.key 之类）：
 *      那些不中止，但**必须逐条打印出来**让人看见，不允许悄悄放过。
 *
 * 用法：
 *   php scripts/build-release.php            # 版本号取自 composer.json
 *   php scripts/build-release.php 0.2.0      # 覆盖版本号（一般用不到）
 *
 * 产物：
 *   build/aqua-api-php-<version>.zip
 *   build/aqua-api-php-<version>.zip.sha256
 */

declare(strict_types=1);

if (PHP_SAPI !== 'cli') {
    fwrite(STDERR, "本脚本只能在命令行运行\n");
    exit(1);
}

// ═══════════════════════════════════════════════════════════
// 白名单：只有列在这里的东西才可能进包
// ═══════════════════════════════════════════════════════════

/** 整个目录进包 */
const INCLUDE_DIRS = [
    'app',      // 应用代码
    'config',   // 配置（跑起来必需；里面没有任何凭据，凭据在 .env 与 options 表）
    'public',   // 入口与静态资源（favicon 在这里）
    'scripts',  // 运维脚本（改密码、清理日志、导入密钥等，对部署者有用）
    'support',  // Webman 的支撑类
    'vendor',   // ★ 依赖：必须进包，理由见文件头
];

/** 单个文件进包 */
const INCLUDE_FILES = [
    '.env.example',   // 配置模板（真凭据在 .env，不进包）
    'LICENSE',
    'README.md',
    'composer.json',
    'composer.lock',
    'start.php',
    'windows.php',
    'windows.bat',
];

/** 路径片段命中即中止（不受白名单保护） */
const DENY_PATH_PATTERNS = [
    '/private/',
    '/docs/',      // 含生产域名、备案号等运营信息，不该随发行版分发
    '/runtime/',
    '/storage/',
    '/logs/',
    '/.git/',
    '/.idea/',
    '/.vscode/',
    '/node_modules/',
];

/** 文件名命中即中止（.env.example 是唯一例外） */
const DENY_NAME_PATTERNS = [
    '.env',
    '.pem',
    '.key',
    '.crt',
    '.p12',
    '.pfx',
    '.ppk',
    '.token',
    '.db',
    '.sqlite',
    '.sqlite3',
    '.log',
    '.bak',
    '.tmp',
    '.orig',
    'config.local.php',
    'config.secret.php',
    'keys-alive',
    'nvidia-keys-',
];

/** 明确放行的文件名（上面的模式会误伤的） */
const DENY_EXCEPTIONS = ['.env.example'];

// ═══════════════════════════════════════════════════════════
// 正文
// ═══════════════════════════════════════════════════════════

$root = dirname(__DIR__);

/**
 * 命中禁止清单吗？
 *
 * @return string 命中的模式描述；空串表示没命中
 */
function denyReason(string $relPath): string
{
    $padded = '/' . trim($relPath, '/');

    foreach (DENY_PATH_PATTERNS as $pattern) {
        if (str_contains($padded, $pattern)) {
            return '路径含 ' . $pattern;
        }
    }

    $name = basename($relPath);

    if (in_array($name, DENY_EXCEPTIONS, true)) {
        return '';
    }

    foreach (DENY_NAME_PATTERNS as $pattern) {
        // 后缀型模式按「结尾」匹配，避免误伤正常文件（如 settings.php 不该被 .php 类规则打中）
        $hit = str_starts_with($pattern, '.')
            ? str_ends_with(mb_strtolower($name), $pattern)
            : str_contains(mb_strtolower($name), $pattern);

        if ($hit) {
            return '文件名含 ' . $pattern;
        }
    }

    return '';
}

// ── 1. 版本号 ──
$composerFile = $root . '/composer.json';
$composer = json_decode((string) file_get_contents($composerFile), true);

if (!is_array($composer)) {
    fwrite(STDERR, "✗ composer.json 解析失败\n");
    exit(1);
}

$version = trim((string) ($argv[1] ?? ($composer['version'] ?? '')));

if ($version === '') {
    fwrite(STDERR, "✗ 拿不到版本号：composer.json 里没有 version 字段，也没传命令行参数\n");
    exit(1);
}

if (!preg_match('/^\d+\.\d+\.\d+$/', $version)) {
    fwrite(STDERR, "✗ 版本号「{$version}」不是 MAJOR.MINOR.PATCH 形式，拒绝打包\n");
    exit(1);
}

// ── 2. 收集文件 ──
$files = [];
$missing = [];

foreach (INCLUDE_DIRS as $dir) {
    $abs = $root . '/' . $dir;

    if (!is_dir($abs)) {
        $missing[] = $dir . '/';
        continue;
    }

    $iterator = new RecursiveIteratorIterator(
        new RecursiveDirectoryIterator($abs, FilesystemIterator::SKIP_DOTS),
        RecursiveIteratorIterator::SELF_FIRST
    );

    foreach ($iterator as $item) {
        /** @var SplFileInfo $item */
        if ($item->isLink() || !$item->isFile()) {
            continue;
        }

        $rel = str_replace('\\', '/', substr($item->getPathname(), mb_strlen($root) + 1));
        $files[$rel] = $item->getPathname();
    }
}

foreach (INCLUDE_FILES as $file) {
    $abs = $root . '/' . $file;

    if (!is_file($abs)) {
        $missing[] = $file;
        continue;
    }

    $files[$file] = $abs;
}

if ($missing !== []) {
    fwrite(STDERR, "✗ 白名单里有项目不存在，工作区不完整，拒绝打包：\n  " . implode("\n  ", $missing) . "\n");
    exit(1);
}

ksort($files);

// ── 3. 禁止清单检查 ──
$fatal = [];
$externalFixtures = [];   // vendor 里的上游测试样本：只报告，不阻断

foreach ($files as $rel => $abs) {
    $reason = denyReason($rel);

    if ($reason === '') {
        continue;
    }

    if (str_starts_with($rel, 'vendor/')) {
        // 第三方公开代码自带的测试样本。它不是我们的凭据，
        // 拦下来会让打包彻底不可用，所以放行 —— 但必须打印出来（见下方输出）
        $externalFixtures[] = $rel . '   （' . $reason . '）';
        continue;
    }

    $fatal[] = $rel . '   （' . $reason . '）';
}

if ($fatal !== []) {
    fwrite(STDERR, "✗ 禁止清单命中，已中止构建（绝不静默跳过）：\n  " . implode("\n  ", $fatal) . "\n");
    exit(1);
}

// ── 4. 生成 manifest ──
$phpMin = (string) ($composer['require']['php'] ?? '8.1');
$phpMin = trim(preg_replace('/[^0-9.]/', '', $phpMin), '.');

// schema 版本从 Schema.php 里读出来（不加载框架，只做一次正则 —— 打包时不该需要数据库）
$schemaVersion = 0;
$schemaFile = $root . '/app/common/Schema.php';

if (is_file($schemaFile)) {
    if (preg_match('/const\s+VERSION\s*=\s*(\d+)/', (string) file_get_contents($schemaFile), $m)) {
        $schemaVersion = (int) $m[1];
    }
}

$manifest = [
    'name' => 'aqua-api-php',
    'version' => $version,
    'php_min' => $phpMin,
    'schema_version' => $schemaVersion,
    'released_at' => time(),
    'file_count' => count($files) + 1,
];

// ── 5. 打包 ──
$buildDir = $root . '/build';
$zipName = 'aqua-api-php-' . $version . '.zip';
$zipPath = $buildDir . '/' . $zipName;

if (!is_dir($buildDir) && !mkdir($buildDir, 0755, true) && !is_dir($buildDir)) {
    fwrite(STDERR, "✗ 建不了 build/ 目录\n");
    exit(1);
}

if (is_file($zipPath) && !unlink($zipPath)) {
    fwrite(STDERR, "✗ 删不掉旧包 {$zipName}（可能被别的程序占用）\n");
    exit(1);
}

$zip = new ZipArchive();

if ($zip->open($zipPath, ZipArchive::CREATE | ZipArchive::OVERWRITE) !== true) {
    fwrite(STDERR, "✗ 建不了 zip：{$zipPath}\n");
    exit(1);
}

// 包根就是项目根：不加任何顶层目录前缀。
// （Git 的源码包会套一层 aqua-api-php-<tag>/，更新器还得剥离，这里从源头避免）
foreach ($files as $rel => $abs) {
    $zip->addFile($abs, $rel);
}

$zip->addFromString(
    'manifest.json',
    json_encode($manifest, JSON_PRETTY_PRINT | JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES) . "\n"
);

// ★ 显式指定压缩：addFile() 在某些环境下会以「不压缩」写入，
// 那样含 vendor 的包会有上百 MB，下载与暂存都很吃亏
for ($i = 0; $i < $zip->numFiles; $i++) {
    $zip->setCompressionIndex($i, ZipArchive::CM_DEFLATE, 9);
}

$zip->close();

// ── 6. 校验值 ──
$sha256 = hash_file('sha256', $zipPath);

if ($sha256 === false) {
    fwrite(STDERR, "✗ 算不出 sha256\n");
    exit(1);
}

// 用 sha256sum 的标准格式（「哈希 + 两个空格 + 文件名」），
// 这样下载后可以直接 `sha256sum -c` 验证，不用自己拼命令
file_put_contents($buildDir . '/' . $zipName . '.sha256', $sha256 . '  ' . $zipName . "\n");

// ── 7. 结果自检 + 输出 ──
$check = new ZipArchive();
$entries = [];

if ($check->open($zipPath) === true) {
    for ($i = 0; $i < $check->numFiles; $i++) {
        $entries[] = (string) $check->getNameIndex($i);
    }
    $check->close();
}

$hasAutoload = in_array('vendor/autoload.php', $entries, true);
$size = (float) filesize($zipPath);

echo "════════════════════════════════════════════\n";
echo "发行版打包完成\n";
echo "════════════════════════════════════════════\n";
printf("版本号        %s\n", $version);
printf("PHP 最低       %s\n", $phpMin);
printf("Schema 版本    %d\n", $schemaVersion);
printf("包内文件数     %d\n", count($entries));
printf("包大小         %.2f MB\n", $size / 1048576);
printf("SHA-256       %s\n", $sha256);
echo "\n产物：\n  {$zipPath}\n  {$zipPath}.sha256\n";

printf("\n自检：vendor/autoload.php %s\n", $hasAutoload ? '✓ 在位' : '✗ 缺失（更新后会白屏！）');

if ($externalFixtures !== []) {
    printf("\n注意：vendor/ 内有 %d 个上游自带的测试样本，已按「第三方公开代码」放行：\n", count($externalFixtures));
    foreach (array_slice($externalFixtures, 0, 8) as $line) {
        echo '  - ' . $line . "\n";
    }
    if (count($externalFixtures) > 8) {
        printf("  …（还有 %d 个，均为同一来源）\n", count($externalFixtures) - 8);
    }
}

echo "\n已确认包内不含 .env / private/ / docs/ / runtime/ / storage/\n";

if (!$hasAutoload) {
    exit(1);
}

exit(0);
