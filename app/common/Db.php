<?php
/**
 * 数据库连接
 *
 * 设计要点：
 *
 * 1. **默认 SQLite，零配置可用**（项目目标之一：用户填一个 Key 就能跑起来）。
 *    DB_DSN 为空时用 `runtime/aqua.sqlite`；填了就用填的 ——
 *    MySQL 的 DSN 走 MySQL，`sqlite:路径` 的 DSN 仍走 SQLite（只是换了文件位置）。
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

        // 判定方言不能只看「DB_DSN 有没有填」——
        // DSN 本身完全可以是 SQLite（例如把库放到别的目录，或者测试指向临时库）。
        // 早期版本按「填了就是 MySQL」处理，于是 `DB_DSN=sqlite:...` 会走进 MySQL 分支：
        // 连得上库，却被当成 MySQL 方言，建表时生成 AUTO_INCREMENT，
        // 在 SQLite 上直接报语法错误。所以这里按 DSN 前缀来判断。
        $sqliteFile = null;
        if ($dsn === '') {
            $sqliteFile = runtime_path() . DIRECTORY_SEPARATOR . 'aqua.sqlite';
        } elseif (stripos($dsn, 'sqlite:') === 0) {
            $sqliteFile = substr($dsn, strlen('sqlite:'));
        }

        try {
            if ($sqliteFile !== null) {
                self::$sqlite = true;

                // 确保目录存在（runtime 目录通常已由框架创建，这里防御性处理）
                $dir = dirname($sqliteFile);
                if (!is_dir($dir) && !mkdir($dir, 0755, true) && !is_dir($dir)) {
                    throw new RuntimeException("无法创建数据库目录：$dir");
                }

                $pdo = new PDO('sqlite:' . $sqliteFile, null, null, [
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
     * 丢弃当前连接，让下次访问重新按 .env 建立连接。
     *
     * ⚠️ 这个方法只服务于**安装程序**，正常业务代码不该调用。
     *
     * 为什么必须有它：本项目是常驻内存模型，PDO 实例在进程内一直复用。
     * 而安装程序会在运行过程中改掉 .env（把默认的 SQLite 换成用户填的 MySQL），
     * 此时进程里还留着连向旧库的连接 —— 不丢弃它，后续建表、建管理员
     * 都会悄悄写到旧库去，表现为「安装成功了但登录时提示后台未初始化」。
     *
     * 注意：本进程丢弃连接不代表**其它工作进程**也会切换 ——
     * 它们各自持有旧连接，因此安装完成后必须重启服务才能全量生效。
     * 安装成功页上会明确提示这一点。
     */
    public static function reset(): void
    {
        self::$pdo = null;
        self::$sqlite = true;
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
