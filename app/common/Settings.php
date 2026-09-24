<?php
/**
 * 配置中心
 *
 * 这是「把配置尽量数据库化」的落点：管理后台改一个开关，写进 options 表，
 * 各个工作进程在缓存过期后自动读到新值 —— 不需要改代码，也不需要重启服务。
 *
 * 取值优先级（从高到低）：
 *   1. 进程内缓存      —— 避免每个请求都查库
 *   2. options 表      —— 后台可改的运行期配置
 *   3. 环境变量        —— .env 里的引导配置，如 DB_DSN / APP_KEY
 *   4. config/settings.php —— 代码级默认值
 *   5. 调用方传入的默认值
 *
 * ⚠️ 注意数据库优先级**高于**环境变量：这是刻意的，
 *    因为「后台改了配置要能生效」是核心诉求。
 *    代价是 .env 无法覆盖已在数据库里存在的键 —— 想用 .env 的值，
 *    需要先在后台把它恢复默认（即删除数据库里那一行）。
 *
 * 环境变量的键名映射规则：配置键的点号转下划线并转大写。
 *   例：nim.base_url  →  NIM_BASE_URL
 *       site.mode     →  SITE_MODE
 *
 * ⚠️ 关于缓存与多进程：本项目有多个工作进程，后台在 A 进程写入配置后，
 *    B 进程的内存缓存不会立刻失效。因此这里用「短 TTL」折中：
 *    配置修改最多延迟 CACHE_TTL 秒全量生效。
 *    这个数值是【一致性 vs 性能】的取舍点，后续如需秒级一致，
 *    可换成 Redis 广播失效或版本号比对。
 */

declare(strict_types=1);

namespace app\common;

use Throwable;

final class Settings
{
    /**
     * 进程内缓存存活秒数。
     * 调大 = 查库更少但配置生效更慢；调小 = 反之。
     */
    private const CACHE_TTL = 5;

    /**
     * 哨兵值：用于判断 config/settings.php 里是否真的存在某个键。
     * 直接判断 null 不可靠，因为「配置成了 null」和「没这个配置」是两回事。
     */
    private const MISSING = "\0__missing__\0";

    /** @var array<string, mixed> 键 => 已解码的值 */
    private static array $cache = [];

    /** 上次加载时间戳，用于判断缓存是否过期 */
    private static int $loadedAt = 0;

    /**
     * 读取配置。
     *
     * @param string $key     配置键，如 site.mode
     * @param mixed  $default 找不到时的默认值
     */
    public static function get(string $key, mixed $default = null): mixed
    {
        $all = self::all();

        // ① 数据库中的值（管理后台写入，优先级最高）
        if (array_key_exists($key, $all)) {
            return $all[$key];
        }

        // ② 环境变量（.env）
        $env = getenv(self::envKey($key));
        if ($env !== false && $env !== '') {
            return $env;
        }

        // ③ 代码级默认值（config/settings.php）
        // 用一个哨兵值区分「没配置」和「配置成了 null」——这两者含义不同
        $codeDefault = config('settings.' . $key, self::MISSING);
        if ($codeDefault !== self::MISSING) {
            return $codeDefault;
        }

        // ④ 调用方传入的默认值
        return $default;
    }

    /**
     * 读取整数配置（带默认值与强制转换，避免到处写 (int) 转换）。
     */
    public static function int(string $key, int $default = 0): int
    {
        $value = self::get($key, $default);

        return is_numeric($value) ? (int) $value : $default;
    }

    /**
     * 读取布尔配置。
     * 兼容 true/false、"1"/"0"、"true"/"false" 等常见写法。
     */
    public static function bool(string $key, bool $default = false): bool
    {
        $value = self::get($key, $default);
        if (is_bool($value)) {
            return $value;
        }

        return filter_var($value, FILTER_VALIDATE_BOOL, FILTER_NULL_ON_FAILURE) ?? $default;
    }

    /**
     * 写入配置（持久化到 options 表，并同步刷新本进程缓存）。
     *
     * 值会以 JSON 编码存储，以保留数据类型（布尔、整数、数组）。
     */
    public static function put(string $key, mixed $value): void
    {
        $encoded = json_encode($value, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);

        // 先删后插，避免依赖各家数据库的 upsert 语法差异
        // （SQLite 是 INSERT OR REPLACE，MySQL 是 ON DUPLICATE KEY UPDATE，
        //   用 delete + insert 两者通吃，且逻辑一眼看懂）
        Db::execute('DELETE FROM options WHERE opt_key = ?', [$key]);
        Db::execute(
            'INSERT INTO options (opt_key, opt_value, updated_at) VALUES (?, ?, ?)',
            [$key, $encoded, time()]
        );

        // 同步更新本进程缓存，让写入方立刻读到自己刚写的值
        self::$cache[$key] = $value;
    }

    /**
     * 读取 options 表的原始值（不做 JSON 解码、不查缓存、找不到表时返回 null）。
     *
     * 专供内部使用：Schema 需要用它读 schema_version ——
     * 而此时 options 表可能**还不存在**，所以这里必须容忍查询失败。
     */
    public static function raw(string $key): ?string
    {
        try {
            $row = Db::selectOne(
                'SELECT opt_value FROM options WHERE opt_key = ?',
                [$key]
            );
        } catch (Throwable) {
            // 表还不存在（首次启动）—— 视为「没有这个配置」，交由调用方决定怎么做
            return null;
        }

        return $row['opt_value'] ?? null;
    }

    /**
     * 获取全部配置（数据库部分），返回已解码的键值对。
     *
     * @return array<string, mixed>
     */
    public static function all(): array
    {
        if (self::cacheFresh()) {
            return self::$cache;
        }

        $result = [];

        try {
            foreach (Db::select('SELECT opt_key, opt_value FROM options') as $row) {
                $result[$row['opt_key']] = self::decode($row['opt_value']);
            }
        } catch (Throwable) {
            // 表尚未建好时（例如建表过程中）先返回空数组，
            // 让 Schema 能正常完成建表，而不是在这里炸掉
            $result = [];
        }

        self::$cache = $result;
        self::$loadedAt = time();

        return $result;
    }

    /**
     * 查询某个配置项的值「来自哪一层」。
     *
     * 后台会用它展示「默认值 / 环境变量 / 数据库」的标记，
     * 让站长一眼看出哪些项被改过、改在哪一层 —— 排查配置不生效时非常有用。
     *
     * @return string database | env | default | none
     */
    public static function source(string $key): string
    {
        if (array_key_exists($key, self::all())) {
            return 'database';
        }

        $env = getenv(self::envKey($key));
        if ($env !== false && $env !== '') {
            return 'env';
        }

        if (config('settings.' . $key, self::MISSING) !== self::MISSING) {
            return 'default';
        }

        return 'none';
    }

    /**
     * 清空进程内缓存，强制下次访问重新读库。
     */
    public static function forget(): void
    {
        self::$cache = [];
        self::$loadedAt = 0;
    }

    private static function cacheFresh(): bool
    {
        return self::$loadedAt > 0 && (time() - self::$loadedAt) < self::CACHE_TTL;
    }

    /**
     * 把数据库里存的 JSON 字符串还原成 PHP 值。
     * 兼容历史遗留的「非 JSON 裸字符串」，避免一处脏数据导致整表读不出来。
     */
    private static function decode(?string $raw): mixed
    {
        if ($raw === null) {
            return null;
        }

        $decoded = json_decode($raw, true);
        if (json_last_error() !== JSON_ERROR_NONE) {
            return $raw;
        }

        return $decoded;
    }

    /**
     * 配置键 → 环境变量名。
     *   例：nim.rpm_limit → NIM_RPM_LIMIT
     */
    private static function envKey(string $key): string
    {
        return strtoupper(str_replace(['.', '-'], '_', $key));
    }
}
