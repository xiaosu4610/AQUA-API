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
 *   2. **不加载 Webman 框架**（连 .env 解析都用 scripts/_bootstrap.php 里的
 *      独立实现）。原因：需要重置密码时，往往意味着环境本身可能有问题，
 *      这时候依赖越少越好 —— 只要 PHP 能跑、数据库能连上就够。
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

fwrite(STDOUT, "=== aqua-api-php 管理员密码重置 ===\n\n");

/**
 * 关闭终端回显后读取一行，读完立即恢复。
 * Windows 没有 stty，退化为普通读取（输入可见）。
 */
function readHidden(string $prompt): string
{
    fwrite(STDOUT, $prompt);

    $isWindows = DIRECTORY_SEPARATOR === '\\';
    if (!$isWindows) {
        @shell_exec('stty -echo 2>/dev/null');
    }

    $value = rtrim((string) fgets(STDIN), "\r\n");

    if (!$isWindows) {
        @shell_exec('stty echo 2>/dev/null');
    }

    fwrite(STDOUT, "\n");

    return $value;
}

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

$pdo = aqua_pdo();
$hash = password_hash($password, PASSWORD_DEFAULT);
$now = time();

$exists = $pdo->query('SELECT id FROM admins WHERE id = 1')->fetchColumn();

if ($exists) {
    $stmt = $pdo->prepare(
        'UPDATE admins SET password_hash = ?, failed_attempts = 0, locked_until = NULL, updated_at = ? WHERE id = 1'
    );
    $stmt->execute([$hash, $now]);
    fwrite(STDOUT, "已更新管理员密码，并同时清除了失败计数与锁定状态。\n");
} else {
    $stmt = $pdo->prepare(
        'INSERT INTO admins (id, password_hash, failed_attempts, created_at, updated_at) VALUES (1, ?, 0, ?, ?)'
    );
    $stmt->execute([$hash, $now, $now]);
    fwrite(STDOUT, "已创建管理员账号。\n");
}

fwrite(STDOUT, "\n提示：如果服务以其它用户身份运行（例如 www-data），\n");
fwrite(STDOUT, "      请确认数据库文件的属主未被改变。\n");
