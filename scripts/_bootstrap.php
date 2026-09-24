<?php
/**
 * CLI 脚本共用引导
 *
 * 供 scripts/ 下的命令行工具使用。刻意**不加载 Webman 框架**，理由是：
 *   这些脚本的使用场景往往是「环境本身有问题」（忘记密码、密钥要批量导入），
 *   此时依赖越少越可靠 —— 只要 PHP 能跑、数据库能连上就够。
 *
 * 注意：本文件不是 Web 入口，放在 scripts/ 下不会被公网访问到。
 */

declare(strict_types=1);

/**
 * 项目根目录。
 */
function aqua_root(): string
{
    static $root = null;
    if ($root === null) {
        $root = dirname(__DIR__);
    }

    return $root;
}

/**
 * 手动解析 .env。
 *
 * ⚠️ 解析规则必须与 Webman 的 support/bootstrap.php 保持一致，
 * 尤其是「**第一个出现的键生效**、后面的重复定义被忽略」这一条。
 * 历史上踩过坑：.env 里先出现空的 `ADMIN_PASSWORD=`、后面才是真实值，
 * 结果真实值被忽略，导致管理员死活建不出来。
 * 这也是为什么所有 CLI 脚本都共用本函数，而不是各自实现一份。
 */
function aqua_load_env(): array
{
    static $vars = null;
    if ($vars !== null) {
        return $vars;
    }

    $vars = [];
    $file = aqua_root() . DIRECTORY_SEPARATOR . '.env';

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
        $value = trim(substr($line, $pos + 1), " \t\"'");

        if (!array_key_exists($key, $vars)) {
            $vars[$key] = $value;
        }
    }

    return $vars;
}

/**
 * 读取一个环境变量。
 */
function aqua_env(string $key, string $default = ''): string
{
    $vars = aqua_load_env();
    $value = $vars[$key] ?? '';

    return $value === '' ? $default : $value;
}

/**
 * 建立数据库连接（与 app/common/Db.php 的定位规则保持一致）。
 *
 * 这里刻意重复了一小段 DSN 逻辑而不是调用 app/common/Db.php：
 * 一旦调用它就会牵扯进框架的自动加载与常量定义，
 * 而这些脚本存在的意义正是「不依赖框架也能用」。
 * 保持两者一致的手段是——数据库路径只有 runtime/aqua.sqlite 这一处，
 * 改动时两边一起改（已在项目记忆里记下这条约束）。
 */
function aqua_pdo(): PDO
{
    static $pdo = null;
    if ($pdo instanceof PDO) {
        return $pdo;
    }

    $dsn = trim(aqua_env('DB_DSN'));

    try {
        if ($dsn === '') {
            $file = aqua_root() . DIRECTORY_SEPARATOR . 'runtime' . DIRECTORY_SEPARATOR . 'aqua.sqlite';
            if (!is_file($file)) {
                fwrite(STDERR, "找不到数据库文件：$file\n");
                fwrite(STDERR, "请先启动一次服务，让程序完成建表。\n");
                exit(1);
            }

            $pdo = new PDO('sqlite:' . $file, null, null, [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
        } else {
            $pdo = new PDO(
                $dsn,
                aqua_env('DB_USER'),
                aqua_env('DB_PASSWORD'),
                [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]
            );
        }
    } catch (PDOException $e) {
        fwrite(STDERR, '数据库连接失败：' . $e->getMessage() . "\n");
        exit(1);
    }

    return $pdo;
}

/**
 * 校验 APP_KEY 已配置，并返回它。
 *
 * 密钥导入必须加密，没有 APP_KEY 就无法进行 —— 这里提前给出明确提示，
 * 而不是等到写入时才抛一个底层异常。
 */
function aqua_require_app_key(): string
{
    $key = trim(aqua_env('APP_KEY'));

    if ($key === '') {
        fwrite(STDERR, "未配置 APP_KEY，无法加密保存密钥。\n");
        fwrite(STDERR, "请在 .env 中设置 APP_KEY，生成方式：\n");
        fwrite(STDERR, "    php -r \"echo bin2hex(random_bytes(32));\"\n");
        exit(1);
    }

    return $key;
}

/**
 * 与 app/common/Crypto.php 完全一致的加密实现（AES-256-GCM）。
 *
 * 格式：v1:base64( iv(12) | tag(16) | ciphertext )
 * 两边必须严格一致，否则「脚本写进去的、程序解不出来」。
 */
function aqua_encrypt(string $plain, string $appKey): string
{
    $derived = hash('sha256', $appKey, true);
    $iv = random_bytes(12);
    $tag = '';

    $ciphertext = openssl_encrypt($plain, 'aes-256-gcm', $derived, OPENSSL_RAW_DATA, $iv, $tag);
    if ($ciphertext === false) {
        fwrite(STDERR, '加密失败：' . openssl_error_string() . "\n");
        exit(1);
    }

    return 'v1:' . base64_encode($iv . $tag . $ciphertext);
}

/**
 * 读取密钥文件，返回去空行后的明文列表。
 *
 * 兼容的输入格式：
 *   · 一行一把 Key（最常见）
 *   · 行内以逗号/空白分隔的多个 Key
 *   · 以 # 开头的注释行会被跳过
 *
 * @return array<int, string>
 */
function aqua_read_keys(string $file): array
{
    if (!is_readable($file)) {
        fwrite(STDERR, "无法读取密钥文件：$file\n");
        exit(1);
    }

    $keys = [];
    foreach (file($file, FILE_IGNORE_NEW_LINES | FILE_SKIP_EMPTY_LINES) as $line) {
        $line = trim($line);
        if ($line === '' || $line[0] === '#') {
            continue;
        }

        foreach (preg_split('/[\s,]+/', $line) ?: [] as $piece) {
            $piece = trim($piece);
            if ($piece !== '') {
                $keys[] = $piece;
            }
        }
    }

    return $keys;
}

/**
 * 解析 `--key=value` 形式的命令行参数。
 *
 * @return array<string, string>
 */
function aqua_args(): array
{
    $args = [];
    foreach (array_slice($GLOBALS['argv'], 1) as $arg) {
        if (!str_starts_with($arg, '--')) {
            continue;
        }
        $pos = strpos($arg, '=');
        if ($pos === false) {
            $args[substr($arg, 2)] = '1';
            continue;
        }
        $args[substr($arg, 2, $pos - 2)] = substr($arg, $pos + 1);
    }

    return $args;
}
