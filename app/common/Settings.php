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

use support\Log;
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

    /**
     * 内部保留键 —— 存放在 options 表里、但**不是用户可配置项**。
     *
     * 目前只有 schema_version（表结构版本号，由程序自己维护）。
     * 这些键必须从 keys() 里排除掉，否则会出现在后台的配置管理页上，
     * 站长改一下就可能让迁移逻辑误判，属于「不该给人碰的东西」。
     */
    private const INTERNAL_KEYS = ['schema_version'];

    /**
     * **需要加密存储的配置键**。
     *
     * 这些是「必须能还原出原文」的凭据（SMTP 授权码、支付商户密钥），
     * 因此与渠道 API Key 同类：用 AES-256-GCM 加密后入库，
     * 后台**只显示「已配置」，绝不回显**。
     *
     * 为什么不像普通配置那样直接存明文：
     * options 表会被导出、备份、在排查问题时被 SELECT 出来 ——
     * 明文的口令一旦落到这些渠道里，就等于泄露。
     * 而这类凭据恰恰是「拿到就能花你的钱」的那种。
     *
     * 注意：清单之外的键一律按普通配置处理，所以新增凭据类配置时
     * **必须**记得加到这里。
     */
    private const SECRET_KEYS = ['mail.password', 'payment.key'];

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
            return self::coerce($key, $all[$key]);
        }

        // ② 环境变量（.env）
        $env = getenv(self::envKey($key));
        if ($env !== false && $env !== '') {
            return self::coerce($key, $env);
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
     * 把取到的值按其「应有类型」归一化。
     *
     * 为什么必须做这一步（这是踩过的真实 bug）：
     *   环境变量**永远是字符串** —— .env 里写 `SESSION_SECURE=false`，
     *   取出来是字符串 `'false'`，而不是布尔 `false`。
     *   于是会连环出三个问题：
     *     1. 后台的复选框会渲染成「已勾选」（PHP 里非空字符串 'false' 是真值）；
     *     2. 保存时的「值是否变化」判断永远不成立（'false' !== true），
     *        导致每次点保存都把配置**物化**进数据库，
     *        之后改 .env 就不再生效 —— 而且现象极其难排查；
     *     3. 调用方拿到的是字符串，做数值比较或布尔判断会得到意外结果。
     *
     *   类型由 `config/settings.php` 的默认值决定：那里写 true 就按布尔，
     *   写 40 就按整数。因此只要在声明默认值时写对类型，
     *   整个链路上的类型就都正确，不需要到处手工转换。
     */
    private static function coerce(string $key, mixed $value): mixed
    {
        if (!self::hasCodeDefault($key)) {
            return $value;
        }

        $default = self::codeDefault($key);

        return match (true) {
            // 布尔：能识别 'true'/'false'/'1'/'0'/'on'/'off'
            is_bool($default) => filter_var($value, FILTER_VALIDATE_BOOL, FILTER_NULL_ON_FAILURE)
                ?? (bool) $value,
            // 整数：非数字时回落到默认值，避免写入脏数据后整个配置读不出来
            is_int($default) => is_numeric($value) ? (int) $value : $default,
            // 浮点：倍率这类天然带小数的配置。**不能按整数处理** ——
            // 否则 1.5 这种倍率会被截成 1，价格算错且极难发现
            is_float($default) => is_numeric($value) ? (float) $value : $default,
            // 字符串
            default => is_scalar($value) ? (string) $value : $value,
        };
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
     * 读取浮点配置（倍率、系数这类）。
     */
    public static function float(string $key, float $default = 0.0): float
    {
        $value = self::get($key, $default);

        return is_numeric($value) ? (float) $value : $default;
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
     * 站点名称（带默认值，供视图层直接使用）。
     */
    public static function siteName(): string
    {
        return (string) self::get('site.name', 'aqua-api-php');
    }

    /**
     * 该键是否为加密存储的凭据。
     */
    public static function isSecret(string $key): bool
    {
        return in_array($key, self::SECRET_KEYS, true);
    }

    /**
     * 该凭据是否已配置（有非空值）。
     * 后台据此显示「已配置 / 未配置」，而不是回显内容。
     */
    public static function hasSecret(string $key): bool
    {
        return self::isSecret($key) && trim((string) self::get($key, '')) !== '';
    }

    /**
     * 读取凭据的**明文**。
     *
     * ⚠️ 仅供内部使用（发邮件、算支付签名）。
     * 任何情况下都不许把它送回浏览器或写进日志。
     *
     * 兼容一种情况：值来自 .env 时是明文（环境变量层不做加密），
     * 而来自数据库时是 `v1:` 开头的密文。按前缀区分，两种都能用。
     */
    public static function secret(string $key): string
    {
        if (!self::isSecret($key)) {
            return '';
        }

        $raw = trim((string) self::get($key, ''));

        if ($raw === '') {
            return '';
        }

        if (!str_starts_with($raw, 'v1:')) {
            // 来自 .env 的明文
            return $raw;
        }

        try {
            return Crypto::decrypt($raw);
        } catch (Throwable $e) {
            // 解密失败通常是 APP_KEY 被换过。返回空串并按「未配置」处理，
            // 而不是抛异常 —— 否则配置页一打开就白屏，站长根本看不到问题在哪
            Log::error("配置项 {$key} 解密失败：" . $e->getMessage());

            return '';
        }
    }

    /**
     * 写入凭据（加密后入库）。
     *
     * 传空串表示**不修改**：这是刻意的 —— 后台不回显凭据，
     * 所以「输入框是空的」绝大多数情况表示「没动它」，
     * 若按「清空」处理，用户改一次别的配置就会把凭据抹掉。
     *
     * @throws \RuntimeException 未配置 APP_KEY 时无法加密
     */
    public static function putSecret(string $key, string $plain): void
    {
        if (!self::isSecret($key) || $plain === '') {
            return;
        }

        self::put($key, Crypto::encrypt($plain));
    }

    /**
     * 清空某个凭据（删除数据库里的值）。
     */
    public static function forgetSecret(string $key): void
    {
        if (self::isSecret($key)) {
            self::reset($key);
        }
    }

    /**
     * 站点模式的中文名：商业站 / 公益站。
     *
     * 放在这里而不是各个控制器里，是因为它被首页、仪表盘、配置页多处使用 ——
     * 一旦将来增加站点模式，只需要改这一个地方。
     */
    public static function siteModeLabel(): string
    {
        return (string) self::get('site.mode', 'commercial') === 'public_welfare'
            ? '公益站'
            : '商业站';
    }

    /**
     * 列出所有「已知」的配置键。
     *
     * 来源有两处，合并后返回：
     *   1. `config/settings.php` 里声明的键（递归展平成点号形式）
     *   2. 数据库里已存在、但代码里没有声明的键
     *      —— 保留第 2 类是为了让「手工写进库的配置」也能在后台看到，
     *         否则会出现「数据在库里、后台却不显示」的困惑
     *
     * 后台的配置管理页用它来决定「要展示哪些配置项」。
     *
     * @return array<int, string>
     */
    public static function keys(): array
    {
        $keys = [];

        // ① 代码声明的键
        foreach (self::flatten((array) config('settings', [])) as $key => $_) {
            $keys[$key] = true;
        }

        // ② 数据库里额外的键（排除程序自用的内部键）
        foreach (array_keys(self::all()) as $key) {
            if (in_array($key, self::INTERNAL_KEYS, true)) {
                continue;
            }
            $keys[$key] = true;
        }

        $result = array_keys($keys);
        sort($result);

        return $result;
    }

    /**
     * 取得某个键的「代码默认值」。
     * 找不到时返回 null —— 调用方据此判断该键是不是代码里声明的。
     */
    public static function codeDefault(string $key): mixed
    {
        $value = config('settings.' . $key, self::MISSING);

        return $value === self::MISSING ? null : $value;
    }

    /**
     * 该键是否在 config/settings.php 中有声明。
     * 后台据此判断「能否恢复默认」以及「用哪种表单控件」。
     */
    public static function hasCodeDefault(string $key): bool
    {
        return config('settings.' . $key, self::MISSING) !== self::MISSING;
    }

    /**
     * 恢复默认：删除数据库中的覆盖值，让取值链回落到环境变量或代码默认值。
     * 数据库里本来就没有该键时，本方法是空操作（幂等）。
     */
    public static function reset(string $key): void
    {
        Db::execute('DELETE FROM options WHERE opt_key = ?', [$key]);
        unset(self::$cache[$key]);
    }

    /**
     * 把多维配置数组展平成「点号键」形式。
     *   例：['nim' => ['rpm_limit' => 40]] → ['nim.rpm_limit' => 40]
     *
     * @param array<string, mixed> $array
     * @return array<string, mixed>
     */
    private static function flatten(array $array, string $prefix = ''): array
    {
        $result = [];

        foreach ($array as $key => $value) {
            $fullKey = $prefix === '' ? (string) $key : $prefix . '.' . $key;

            if (is_array($value)) {
                $result += self::flatten($value, $fullKey);
                continue;
            }

            $result[$fullKey] = $value;
        }

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
