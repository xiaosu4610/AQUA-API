<?php
/**
 * 数据库连接
 *
 * 设计要点：
 *
 * 1. **默认 SQLite，零配置可用**（项目目标之一：用户填一个 Key 就能跑起来）。
 *    只有在 .env 里填了 DB_DSN 时才切换到 MySQL。
 *
 * 2. **连接在进程内复用**。
 *    本项目运行在「常驻内存」模型下，同一个工作进程会处理成千上万个请求。
 *    因此 PDO 实例用静态变量缓存，避免每个请求都重新建连 ——
 *    这与传统 PHP-FPM 下「请求结束即释放」的习惯不同，是本项目的基本前提。
 *
 * 3. **SQLite 在多进程下必须开 WAL 并设 busy_timeout**。
 *    我们有多个工作进程会同时读写同一个 SQLite 文件；
 *    若不开 WAL，写操作会互相阻塞并直接抛 "database is locked"。
 *    busy_timeout 则是让「撞锁」的进程等一会儿重试，而不是立刻失败。
 */

declare(strict_types=1);

namespace app\common;

use PDO;
use PDOException;
use RuntimeException;

final class Db
{
    /** 进程内缓存的连接。null 表示尚未建立连接。 */
    private static ?PDO $pdo = null;

    /** 当前是否使用 SQLite。建立连接后才有意义。 */
    private static bool $sqlite = true;

    /**
     * 获取 PDO 连接（进程内单例）。
     */
    public static function pdo(): PDO
    {
        if (self::$pdo instanceof PDO) {
            return self::$pdo;
        }

        $dsn = trim((string) (getenv('DB_DSN') ?: ''));

        try {
            if ($dsn === '') {
                // ── SQLite：零配置默认路径 ──
                self::$sqlite = true;
                $file = runtime_path() . DIRECTORY_SEPARATOR . 'aqua.sqlite';

                // 确保目录存在（runtime 目录通常已由框架创建，这里防御性处理）
                $dir = dirname($file);
                if (!is_dir($dir) && !mkdir($dir, 0755, true) && !is_dir($dir)) {
                    throw new RuntimeException("无法创建数据库目录：$dir");
                }

                $pdo = new PDO('sqlite:' . $file, null, null, [
                    PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
                ]);

                // WAL：允许多进程「并发读 + 单写」，是 SQLite 支撑多进程的前提
                $pdo->exec('PRAGMA journal_mode = WAL');
                // 撞锁时最多等 5 秒再报错，而不是立刻失败
                $pdo->exec('PRAGMA busy_timeout = 5000');
                // 打开外键约束（SQLite 默认是关闭的）
                $pdo->exec('PRAGMA foreign_keys = ON');
                // 折中：兼顾安全与速度。FULL 会影响写入性能，NORMAL 在 WAL 下足够可靠
                $pdo->exec('PRAGMA synchronous = NORMAL');
            } else {
                // ── MySQL（或其它 PDO 驱动）──
                self::$sqlite = false;
                $pdo = new PDO(
                    $dsn,
                    (string) (getenv('DB_USER') ?: ''),
                    (string) (getenv('DB_PASSWORD') ?: ''),
                    [
                        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
                        // 关闭模拟预处理，使用真正的服务端预处理语句（防注入的第一道防线）
                        PDO::ATTR_EMULATE_PREPARES => false,
                        PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
                    ]
                );
            }
        } catch (PDOException $e) {
            // 数据库连不上属于启动期致命错误，必须显式抛出而不是静默降级
            throw new RuntimeException('数据库连接失败：' . $e->getMessage(), 0, $e);
        }

        self::$pdo = $pdo;

        return $pdo;
    }

    /**
     * 当前是否为 SQLite。
     * 供 Schema 层生成方言相关的 DDL 使用。
     */
    public static function isSqlite(): bool
    {
        // 先触发一次连接，确保判定结果准确
        self::pdo();

        return self::$sqlite;
    }

    /**
     * 执行查询并返回全部行。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function select(string $sql, array $bindings = []): array
    {
        $stmt = self::pdo()->prepare($sql);
        $stmt->execute($bindings);

        return $stmt->fetchAll();
    }

    /**
     * 执行查询并返回首行（无结果返回 null）。
     *
     * @return array<string, mixed>|null
     */
    public static function selectOne(string $sql, array $bindings = []): ?array
    {
        $stmt = self::pdo()->prepare($sql);
        $stmt->execute($bindings);
        $row = $stmt->fetch();

        return $row === false ? null : $row;
    }

    /**
     * 执行写入语句，返回受影响行数。
     */
    public static function execute(string $sql, array $bindings = []): int
    {
        $stmt = self::pdo()->prepare($sql);
        $stmt->execute($bindings);

        return $stmt->rowCount();
    }
}
