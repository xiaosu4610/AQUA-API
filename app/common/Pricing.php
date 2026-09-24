<?php
/**
 * 模型定价与成本计算
 *
 * ═══ 这个类要回答的问题 ═══
 *
 *   1. 这一次请求，我们在上游花了多少钱？（成本）
 *   2. 这一次请求，我们向用户收多少钱？（售价）
 *   3. 两者的差额是多少？（毛利）
 *
 * ═══ 为什么不能只有「一个价格」 ═══
 *
 * 站长原话：「不是所有上游和所有下游都是免费的，也不是所有的计费都是跟官方一样的」。
 * 这句话拆开就是三种现实：
 *
 *   · **上游成本 ≠ 官方价**。三方中转站的 OpenAI 可能是官方价的三折；
 *     各种反代、KIRO 这类形态的价格与官方毫无关系；
 *     自建推理（vLLM/Ollama）的边际成本是 0；订阅套餐是固定月费摊不出单价。
 *   · **下游售价 ≠ 上游成本**。站长可以按上游成本乘一个倍率定价，
 *     也可以完全独立定一套价格（例如统一按次收费）。
 *   · **计费口径 ≠ 都是按 token**。有些上游按次、有些是订阅、
 *     有些根本不返回 usage 字段。
 *
 * 因此这里把「上游成本」与「下游售价」彻底分开，并把计费口径做成
 * billing_mode 枚举，而不是试图用一个公式覆盖所有情况。
 *
 * ═══ 关于「估算」这件事的诚实态度 ═══
 *
 * 部分上游（尤其是各种反代与三方中转）在流式响应里**不返回 usage**，
 * 这时只能估算。估算永远不准，所以：
 *   · 估算结果一律打上 usage_estimated 标记，绝不伪装成真实用量；
 *   · 估算只用于「记账与限额」，不用于对账级结算；
 *   · 宁可如实记录 0 并告警，也不要用官方价硬凑一个数字 ——
 *     那会让毛利统计凭空多出一笔并不存在的成本。
 */

declare(strict_types=1);

namespace app\common;

final class Pricing
{
    /** 计费模式 */
    public const MODE_TOKEN = 'token';
    public const MODE_CALL = 'call';
    public const MODE_SUBSCRIPTION = 'subscription';
    public const MODE_FREE = 'free';

    /** 定价启用状态 */
    public const STATUS_ENABLED = 1;
    public const STATUS_DISABLED = 0;

    /** 默认计价单位：每 100 万 token 计一次价（业界最常见的报价口径） */
    public const DEFAULT_UNIT = 1000000;

    /**
     * 计费模式清单（同时供后台下拉使用）。
     */
    public const MODES = [
        self::MODE_TOKEN => [
            'label' => '按 Token',
            'hint' => '输入与输出分别计价，适用于绝大多数对话模型',
        ],
        self::MODE_CALL => [
            'label' => '按次',
            'hint' => '每次请求固定费用，与 token 无关。图像、语音、异步任务类多用这种',
        ],
        self::MODE_SUBSCRIPTION => [
            'label' => '订阅制',
            'hint' => '编程套餐 / 包月这类：边际成本为 0，只记用量不产生成本。'
                . '若仍要向用户收费，请单独填下游售价',
        ],
        self::MODE_FREE => [
            'label' => '免费',
            'hint' => '免费额度或自建模型，且对下游也免费',
        ],
    ];

    /**
     * 上游种类 —— 只用来说明「这笔成本是什么性质」，不参与计算。
     *
     * 之所以要有它，是因为站长在复盘时需要一眼分辨
     * 「这个模型的成本是官方原价、还是中转折扣价、还是根本就是 0」。
     * 光看数字看不出这一点。
     */
    public const UPSTREAM_KINDS = [
        'official' => ['label' => '官方直连', 'hint' => '价格与官方一致'],
        'relay' => ['label' => '三方中转 / 反代', 'hint' => '价格由对方决定，可能与官方相差很大'],
        'self' => ['label' => '自建推理', 'hint' => '自己的显卡，边际成本为 0'],
        'subscription' => ['label' => '订阅套餐', 'hint' => '包月固定支出，单次请求边际成本为 0'],
        'free' => ['label' => '免费额度', 'hint' => '免费层或赠送额度'],
    ];

    // ═══════════════════════════════════════════════════════════
    // 数据访问
    // ═══════════════════════════════════════════════════════════

    /**
     * 全部定价（按模型名排序，便于人工查找）。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function all(): array
    {
        return Db::select('SELECT * FROM pricing ORDER BY model ASC');
    }

    /**
     * 分页查询（供后台列表页使用）。
     *
     * @return array<int, array<string, mixed>>
     */
    public static function page(string $keyword, int $limit, int $offset): array
    {
        if ($keyword === '') {
            return Db::select(
                'SELECT * FROM pricing ORDER BY model ASC LIMIT ' . $limit . ' OFFSET ' . $offset
            );
        }

        return Db::select(
            'SELECT * FROM pricing WHERE model LIKE ? ORDER BY model ASC LIMIT ' . $limit . ' OFFSET ' . $offset,
            ['%' . $keyword . '%']
        );
    }

    /** 分页用的总数 */
    public static function count(string $keyword = ''): int
    {
        $row = $keyword === ''
            ? Db::selectOne('SELECT COUNT(*) AS c FROM pricing')
            : Db::selectOne('SELECT COUNT(*) AS c FROM pricing WHERE model LIKE ?', ['%' . $keyword . '%']);

        return (int) ($row['c'] ?? 0);
    }

    /**
     * 按模型名查定价（只返回启用中的）。
     *
     * 刻意**不做**「模糊匹配」「去掉厂商前缀再试一次」这类聪明事：
     * 模型名千奇百怪，猜错了会把 A 的价格算到 B 头上，
     * 而这种错误在账面上极难发现。名字对不上就该由站长显式配置，
     * 或者用渠道高级配置里的「模型名映射」把名字对齐。
     *
     * @return array<string, mixed>|null
     */
    public static function findByModel(string $model): ?array
    {
        return Db::selectOne(
            'SELECT * FROM pricing WHERE model = ? AND status = ?',
            [$model, self::STATUS_ENABLED]
        );
    }

    /**
     * 按模型名查定价，**不看启用状态**。
     *
     * 与 findByModel 的区别：那个服务于「转发时取值」（停用的定价不该生效），
     * 这个服务于「后台校验重名」（停用的行也占着唯一约束，必须能被查到）。
     *
     * @return array<string, mixed>|null
     */
    public static function findByModelAnyStatus(string $model): ?array
    {
        return Db::selectOne('SELECT * FROM pricing WHERE model = ?', [$model]);
    }

    /** @return array<string, mixed>|null */
    public static function find(int $id): ?array
    {
        return Db::selectOne('SELECT * FROM pricing WHERE id = ?', [$id]);
    }

    /**
     * 新增定价。
     *
     * @param array<string, mixed> $data
     * @return int 新记录 id
     */
    public static function create(array $data): int
    {
        $now = time();

        Db::execute(
            'INSERT INTO pricing
                (model, billing_mode, upstream_kind,
                 upstream_input_price, upstream_output_price, upstream_call_price,
                 downstream_input_price, downstream_output_price, downstream_call_price,
                 price_unit, note, status, created_at, updated_at)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
            [
                (string) $data['model'],
                (string) $data['billing_mode'],
                (string) $data['upstream_kind'],
                self::dec($data['upstream_input_price'] ?? 0),
                self::dec($data['upstream_output_price'] ?? 0),
                self::dec($data['upstream_call_price'] ?? 0),
                self::dec($data['downstream_input_price'] ?? 0),
                self::dec($data['downstream_output_price'] ?? 0),
                self::dec($data['downstream_call_price'] ?? 0),
                max(1, (int) ($data['price_unit'] ?? self::DEFAULT_UNIT)),
                (string) ($data['note'] ?? ''),
                (int) ($data['status'] ?? self::STATUS_ENABLED),
                $now,
                $now,
            ]
        );

        return (int) Db::pdo()->lastInsertId();
    }

    /**
     * 更新定价。
     *
     * @param array<string, mixed> $data
     */
    public static function update(int $id, array $data): void
    {
        Db::execute(
            'UPDATE pricing SET
                model = ?, billing_mode = ?, upstream_kind = ?,
                upstream_input_price = ?, upstream_output_price = ?, upstream_call_price = ?,
                downstream_input_price = ?, downstream_output_price = ?, downstream_call_price = ?,
                price_unit = ?, note = ?, status = ?, updated_at = ?
             WHERE id = ?',
            [
                (string) $data['model'],
                (string) $data['billing_mode'],
                (string) $data['upstream_kind'],
                self::dec($data['upstream_input_price'] ?? 0),
                self::dec($data['upstream_output_price'] ?? 0),
                self::dec($data['upstream_call_price'] ?? 0),
                self::dec($data['downstream_input_price'] ?? 0),
                self::dec($data['downstream_output_price'] ?? 0),
                self::dec($data['downstream_call_price'] ?? 0),
                max(1, (int) ($data['price_unit'] ?? self::DEFAULT_UNIT)),
                (string) ($data['note'] ?? ''),
                (int) ($data['status'] ?? self::STATUS_ENABLED),
                time(),
                $id,
            ]
        );
    }

    public static function delete(int $id): void
    {
        Db::execute('DELETE FROM pricing WHERE id = ?', [$id]);
    }

    /**
     * 从所有渠道的模型清单里补齐缺失的定价行（价格全部留 0）。
     *
     * 为什么需要它：一个渠道测活后拉回几十上百个模型名，
     * 让站长一个个手抄到定价页是不现实的。先批量建行、价格留空，
     * 再挑常用的几个填价格 —— 这是实际使用中最顺手的顺序。
     *
     * 已存在的模型不会被动到（价格不会被覆盖）。
     *
     * @return int 新增行数
     */
    public static function syncFromChannels(): int
    {
        $models = [];

        foreach (Channel::all() as $channel) {
            foreach (Channel::modelsOf($channel) as $model) {
                $models[$model] = true;
            }
        }

        $added = 0;
        $now = time();

        foreach (array_keys($models) as $model) {
            $exists = Db::selectOne('SELECT id FROM pricing WHERE model = ?', [$model]);
            if ($exists !== null) {
                continue;
            }

            Db::execute(
                'INSERT INTO pricing
                    (model, billing_mode, upstream_kind,
                     upstream_input_price, upstream_output_price, upstream_call_price,
                     downstream_input_price, downstream_output_price, downstream_call_price,
                     price_unit, note, status, created_at, updated_at)
                 VALUES (?, ?, ?, 0, 0, 0, 0, 0, 0, ?, ?, ?, ?, ?)',
                [
                    $model,
                    self::MODE_TOKEN,
                    'official',
                    self::DEFAULT_UNIT,
                    '由渠道模型清单批量创建，价格待填',
                    self::STATUS_ENABLED,
                    $now,
                    $now,
                ]
            );
            $added++;
        }

        return $added;
    }

    // ═══════════════════════════════════════════════════════════
    // 成本计算
    // ═══════════════════════════════════════════════════════════

    /**
     * 算出一次请求的上游成本与下游售价。
     *
     * 计算规则（这是整个计费的核心，逐条说明为什么）：
     *
     *   上游成本：
     *     · 按 Token → token 数 / 计价单位 × 单价
     *     · 按次     → 每次固定价
     *     · 订阅/免费 → **0**。订阅是固定月费，摊不进单次请求；
     *                   硬摊会让「本次请求成本」这个数字失去意义
     *
     *   下游售价：
     *     · 免费模式 → 恒为 0（对下游也免费）
     *     · 填了下游单价 → 用下游单价算（此时与上游成本无关，
     *       支持「统一按次收费」这类与成本脱钩的定价）
     *     · 下游单价全为 0 → 上游成本 × 倍率（最常用的定价方式）
     *
     * @param array<string, mixed>|null $pricing 定价行；null 表示该模型未定价
     * @param int   $promptTokens     输入 token 数
     * @param int   $completionTokens 输出 token 数
     * @param float $multiplier       倍率（下游单价未配置时使用）
     * @return array{mode:string, unit:int, upstream_cost:float, downstream_cost:float,
     *               profit:float, priced:bool, mode_label:string, upstream_kind:string}
     */
    public static function quote(
        ?array $pricing,
        int $promptTokens,
        int $completionTokens,
        float $multiplier = 1.0
    ): array {
        if ($pricing === null) {
            // 未定价：金额为 0，并如实告知调用方「这个模型没有价格」，
            // 由调用方决定放行还是拒绝（见 billing.unpriced_is_free）
            return [
                'mode' => self::MODE_FREE,
                'unit' => self::DEFAULT_UNIT,
                'upstream_cost' => 0.0,
                'downstream_cost' => 0.0,
                'profit' => 0.0,
                'priced' => false,
                'mode_label' => '未定价',
                'upstream_kind' => '',
            ];
        }

        $mode = (string) ($pricing['billing_mode'] ?? self::MODE_TOKEN);
        $unit = max(1, (int) ($pricing['price_unit'] ?? self::DEFAULT_UNIT));

        // ── 上游成本 ──
        $upstream = match ($mode) {
            self::MODE_TOKEN => $promptTokens / $unit * (float) $pricing['upstream_input_price']
                + $completionTokens / $unit * (float) $pricing['upstream_output_price'],
            self::MODE_CALL => (float) $pricing['upstream_call_price'],
            // 订阅制与免费：边际成本为 0。固定月费属于「运营支出」，
            // 不在单次请求上摊销 —— 否则「本次成本」会随着请求量变化而跳动，无法解读
            default => 0.0,
        };

        // ── 下游售价 ──
        // 哪些字段参与计算，取决于计费模式：
        //   token / subscription —— 用「输入/输出单价」（订阅制的上游成本为 0，
        //                            但向用户收费仍按 token 计，这是编程套餐的常见做法）
        //   call                 —— 用「每次单价」
        //   free                 —— 不收费
        $usesTokenPrice = in_array($mode, [self::MODE_TOKEN, self::MODE_SUBSCRIPTION], true);

        if ($mode === self::MODE_FREE) {
            $downstream = 0.0;
        } else {
            $downstream = $usesTokenPrice
                ? $promptTokens / $unit * (float) $pricing['downstream_input_price']
                    + $completionTokens / $unit * (float) $pricing['downstream_output_price']
                : (float) $pricing['downstream_call_price'];

            // 本模式下该用的下游单价全是 0，视为「没配」，回落到「上游成本 × 倍率」。
            // 只看本模式对应的字段，避免被其它模式残留的数字干扰
            $hasOwnPrice = $usesTokenPrice
                ? ((float) $pricing['downstream_input_price'] > 0
                    || (float) $pricing['downstream_output_price'] > 0)
                : (float) $pricing['downstream_call_price'] > 0;

            if (!$hasOwnPrice) {
                $downstream = $upstream * max(0.0, $multiplier);
            }
        }

        return [
            'mode' => $mode,
            'unit' => $unit,
            'upstream_cost' => round($upstream, 10),
            'downstream_cost' => round($downstream, 10),
            'profit' => round($downstream - $upstream, 10),
            'priced' => true,
            'mode_label' => self::MODES[$mode]['label'] ?? $mode,
            'upstream_kind' => (string) ($pricing['upstream_kind'] ?? ''),
        ];
    }

    /**
     * 估算一段文本的 token 数（上游不返回 usage 时使用）。
     *
     * 口径说明（这是粗估，不是分词）：
     *   · 中日韩字符：约 1 字 = 1 token
     *   · 其余字符（英文、数字、代码符号）：约 4 字符 = 1 token
     *
     * 为什么不用「统一按字符数除以 4」：中文占比高的请求会被严重低估，
     * 而低估的后果是「少记成本」，属于账面上看不见的漏损。
     * 这个启发式规则在实践中误差可控，且实现足够简单、无需引入词表。
     *
     * @param float $ratio 校准系数（billing.estimate_ratio），
     *                     用于按自己上游的实际表现微调
     */
    public static function estimateTokens(string $text, float $ratio = 1.0): int
    {
        if ($text === '') {
            return 0;
        }

        $length = mb_strlen($text, 'UTF-8');
        $cjk = (int) preg_match_all('/[\x{4e00}-\x{9fff}\x{3040}-\x{30ff}\x{ac00}-\x{d7af}]/u', $text);
        $other = max(0, $length - $cjk);

        $tokens = ($cjk + $other / 4) * max(0.1, $ratio);

        return max(1, (int) round($tokens));
    }

    /**
     * 把来源不明的数值转成可安全落库的小数字符串。
     *
     * 为什么统一留 10 位小数：DECIMAL(20,10) 的精度上限；
     * 用 number_format 而不是 round，是为了避免 PHP 把
     * 1.0E-7 这类小数值转成科学计数法（数据库不认这种写法）。
     */
    private static function dec(mixed $value): string
    {
        return number_format((float) $value, 10, '.', '');
    }
}
