<?php
/**
 * 请求超时的唯一事实源
 *
 * ═══ 为什么值得单独一个类 ═══
 *
 * 超时这几个数字散在四处：配置默认值（config/settings.php）、转发控制器设置
 * RelayJob、文档页正文、后台校验规则。任何一处写歪都会造成
 * 「配置里写着 300、实际 30 秒就断」这类最难查的不一致 ——
 * 用户看到的现象是「模型明明能用却报超时」，而站长在后台怎么改都没用。
 *
 * 所以这里定下三件事，别处只准引用：
 *   · 每个键的**默认值**（与 config/settings.php 保持一致）
 *   · 每个键的**允许范围**（后台表单与接口校验共用，避免一个 0 秒的值把转发打死）
 *   · **推荐值**（后台卡片上的一键恢复目标）
 *
 * ═══ 为什么默认是 300 秒 ═══
 *
 * 免费上游（NVIDIA 免费层这类）冷启动动辄几十秒。首字节卡在 30 秒，
 * 会把一批**本来能用**的模型直接判成「上游无响应」——
 * 用户遇到的就是「明明模型广场上写着可用，调用却报 504」。
 * 而 300 秒已经足够长：再等下去，用户早就放弃了，留着连接没有意义。
 */

declare(strict_types=1);

namespace app\common;

final class Timeouts
{
    /**
     * 各键的默认值（秒）。
     *
     * ⚠️ 必须与 config/settings.php 的 gateway 组一致：改那边记得改这边。
     * 之所以两处都有：config 是「站长能在后台看到并恢复默认」的声明，
     * 这里是「代码里直接引用」的入口。
     */
    public const DEFAULTS = [
        'gateway.connect_timeout' => 8,
        'gateway.probe_timeout' => 20,
        'gateway.ttft_timeout' => 300,
        'gateway.idle_timeout' => 60,
        'gateway.total_timeout' => 300,
    ];

    /**
     * 各键的允许范围（秒）。
     *
     * 上界都是 3600：再长的等待对用户已无意义（而且会占着连接不放）。
     * 下界不为 0：0 或负数会把「这层超时」退化成「立刻失败」，
     * 是那种配完之后「什么都调不通」的取值。
     *
     * **例外**：`gateway.total_timeout` 允许 0 —— 它的语义是
     * 「不设总时长上限」（curl 的 TIMEOUT=0 即无限等待）。
     * 这是老配置里就有的口径，不能因为要校验就把这条路堵死，
     * 否则原来配了 0 的站点会突然变成「每个请求 1 秒就超时」。
     */
    public const RANGE = [
        'gateway.connect_timeout' => [1, 300],
        'gateway.probe_timeout' => [1, 600],
        'gateway.ttft_timeout' => [1, 3600],
        'gateway.idle_timeout' => [1, 3600],
        'gateway.total_timeout' => [0, 3600],
    ];

    /** 后台卡片上「恢复推荐值」的目标 */
    public const RECOMMENDED = [
        'gateway.connect_timeout' => 8,
        'gateway.probe_timeout' => 20,
        'gateway.ttft_timeout' => 300,
        'gateway.idle_timeout' => 60,
        'gateway.total_timeout' => 300,
    ];

    /**
     * 表单字段名 → 配置键。
     *
     * 为什么表单不直接用配置键当字段名（`name="gateway.ttft_timeout"`）：
     * PHP 会把**顶层**变量名里的点号改写成下划线，字段名一不留神就变成
     * 另一个名字，控制器按原点号去找就「读不到值」，症状是「点保存没反应」。
     * （层叠数组里的键不受影响 —— 这点是实测确认过的，别把它当借口写错。）
     * 用短名映射一层，既避开这个坑，HTML 里也更短更好读。
     */
    public const FORM_NAMES = [
        'connect' => 'gateway.connect_timeout',
        'probe' => 'gateway.probe_timeout',
        'ttft' => 'gateway.ttft_timeout',
        'idle' => 'gateway.idle_timeout',
        'total' => 'gateway.total_timeout',
    ];

    /** 连上上游（TCP/TLS 握手）的超时 */
    public static function connect(): int
    {
        return self::read('gateway.connect_timeout');
    }

    /** 测活 / 拉模型清单这类探测请求的超时 */
    public static function probe(): int
    {
        return self::read('gateway.probe_timeout');
    }

    /** 首字节超时：多久还不吐第一个字就算「上游没响应」 */
    public static function ttft(): int
    {
        return self::read('gateway.ttft_timeout');
    }

    /** 空闲超时：两个数据块之间的最大间隔 */
    public static function idle(): int
    {
        return self::read('gateway.idle_timeout');
    }

    /** 单请求总时长上限（兜底） */
    public static function total(): int
    {
        return self::read('gateway.total_timeout');
    }

    /**
     * 全部超时项（键 => 当前生效值），供后台卡片与自检工具展示。
     *
     * @return array<string, int>
     */
    public static function all(): array
    {
        $values = [];
        foreach (self::DEFAULTS as $key => $_) {
            $values[$key] = self::read($key);
        }

        return $values;
    }

    private static function read(string $key): int
    {
        $value = Settings::int($key, self::DEFAULTS[$key] ?? 60);

        // 兜底到范围内：数据库里若被写进 0 或负数（例如手工改库），
        // 宁可用默认值，也不要让转发因为一个荒谬的数字而全挂
        [$min, $max] = self::RANGE[$key] ?? [1, 3600];

        return max($min, min($max, $value));
    }
}
