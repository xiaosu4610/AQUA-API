#!/usr/bin/env php
<?php
/**
 * 后台管理员密码重置工具（命令行）
 *
 * 用法（在项目根目录执行）：
 *     php scripts/reset-admin-password.php
 *
 * 为什么需要这个脚本：
 *   后台是「超级管理员后台」，登录只凭密码、没有用户名，
 *   一旦忘记密码，除了改数据库没有别的入口 —— 这对任何部署都是隐患。
 *   本项目刻意不提供任何默认密码（开源项目里硬编码的默认密码等于公开密码），
 *   所以必须给出一个明确的、不依赖网页的重置途径。
 *
 * 设计说明：
 *   1. 密码**从标准输入读取、不回显**，不接受命令行参数 ——
 *      命令行参数会进入 shell 历史，也可能被同机的其它进程通过 ps 看到。
 *   2. 本脚本**不加载 Webman 框架**，只用 PDO 直连数据库。
 *      原因：忘记密码时往往意味着环境可能有问题，
 *      这时候依赖越少越好，只要求 PHP 能跑、数据库能连上。
 *   3. 数据库位置与 app/common/Db.php 保持一致（SQLite 在 runtime/ 下），
 *      避免出现「网页用这个库、脚本改那个库」的错位。
 */

declare(strict_types=1);

// ── 定位项目根目录 ──────────────────────────────────────
$root = dirname(__DIR__);

// ── 读取 .env（手动解析，不依赖 vlucas/phpdotenv）─────────
// 与 support/bootstrap.php 的规则保持一致：**第一个出现的键生效**，
// 后面的重复定义会被忽略。这点很关键，历史上踩过坑：
// 若 .env 里先出现空的 DB_DSN=，后面再写真实值是不生效的。
function loadEnv(string $file): array
{
    $vars = [];
    if (!is_readable($file)) {
        return $vars;
    }

    foreach (file($file, FILE_IGNORE_NEW_LINES | FILE_SKIP_EMPTY_LINES) as $line) {
        $line = trim($line);
        if ($line === '' || $line[0] === '#') {
            continue;
        }
        $pos = strpos($line, '=');
        if ($pos === false) {
            continue;
        }
        $key = trim(substr($line, 0, $pos));
        $value = trim(substr($line, $pos + 1));
        // 去掉可能的引号
        $value = trim($value, "\"'");
        // 首个定义生效，不覆盖
        if (!array_key_exists($key, $vars)) {
            $vars[$key] = $value;
        }
    }

    return $vars;
}

$env = loadEnv($root . DIRECTORY_SEPARATOR . '.env');

// ── 连接数据库 ──────────────────────────────────────────
$dsn = trim((string) ($env['DB_DSN'] ?? ''));

try {
    if ($dsn === '') {
        $dbFile = $root . DIRECTORY_SEPARATOR . 'runtime' . DIRECTORY_SEPARATOR . 'aqua.sqlite';
        if (!is_file($dbFile)) {
            fwrite(STDERR, "找不到数据库文件：$dbFile\n");
            fwrite(STDERR, "请先启动一次服务，让程序完成建表。\n");
            exit(1);
        }
        $pdo = new PDO('sqlite:' . $dbFile, null, null, [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
    } else {
        $pdo = new PDO(
            $dsn,
            (string) ($env['DB_USER'] ?? ''),
            (string) ($env['DB_PASSWORD'] ?? ''),
            [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]
        );
    }
} catch (PDOException $e) {
    fwrite(STDERR, '数据库连接失败：' . $e->getMessage() . "\n");
    exit(1);
}

// ── 交互式读取新密码（不回显）────────────────────────────
/**
 * 关闭终端回显后读取一行，读完立即恢复。
 * Windows 没有 stty，退化为普通读取（并有可见输入）。
 */
function readHidden(string $prompt): string
{
    fwrite(STDOUT, $prompt);

    $isWindows = DIRECTORY_SEPARATOR === '\\';
    if (!$isWindows) {
        // 关闭回显；失败也不致命（例如管道输入的场景）
        @shell_exec('stty -echo 2>/dev/null');
    }

    $value = rtrim((string) fgets(STDIN), "\r\n");

    if (!$isWindows) {
        @shell_exec('stty echo 2>/dev/null');
    }

    fwrite(STDOUT, "\n");

    return $value;
}

fwrite(STDOUT, "=== aqua-api-php 管理员密码重置 ===\n\n");

$password = readHidden('请输入新的管理员密码：');
if (strlen($password) < 8) {
    fwrite(STDERR, "密码太短：至少 8 位。\n");
    exit(1);
}

$confirm = readHidden('请再次输入以确认：');
if ($password !== $confirm) {
    fwrite(STDERR, "两次输入不一致，已取消。\n");
    exit(1);
}

// ── 写入 ────────────────────────────────────────────────
$hash = password_hash($password, PASSWORD_DEFAULT);
$now = time();

$exists = $pdo->query('SELECT id FROM admins WHERE id = 1')->fetch();
if ($exists) {
    $stmt = $pdo->prepare(
        'UPDATE admins SET password_hash = ?, failed_attempts = 0, locked_until = NULL, updated_at = ? WHERE id = 1'
    );
    $stmt->execute([$hash, $now]);
    echo "已更新管理员密码，并同时清除了失败计数与锁定状态。\n";
} else {
    $stmt = $pdo->prepare(
        'INSERT INTO admins (id, password_hash, failed_attempts, created_at, updated_at) VALUES (1, ?, 0, ?, ?)'
    );
    $stmt->execute([$hash, $now, $now]);
    echo "已创建管理员账号。\n";
}

echo "如果服务以其它用户身份运行（例如 www-data），请确认数据库文件属主未被改变。\n";
