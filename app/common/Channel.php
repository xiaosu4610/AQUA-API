<?php
/**
 * 渠道（上游连接配置）
 *
 * 一个「渠道」= 一套可以调用上游的配置：类型 + 地址 + Key + 可用模型。
 * 网关的路由就是「从多个渠道里挑一个最合适的」。
 *
 * ═══ 本文件是 app/common/Crypto.php 的第一个真实消费者 ═══
 *
 * 上游 API Key 与登录密码的处理方式**完全不同**：
 *   · 登录密码用哈希（不可逆）—— 因为只需要「验证」，永不需还原；
 *   · 上游 Key 用加密（可逆）—— 因为转发时必须把它还原出来放进请求头。
 * 所以这里是 encrypt/decrypt，而不是 hash/verify。
 *
 * ═══ 关于「测活」的实现取舍 ═══
 *
 * 测活用的是**阻塞式 curl**。这与项目主线的「非阻塞流式转发」要求相反，
 * 但在这里是可接受的，理由是：
 *   · 它是站长手动点一次的操作，不是高频请求；
 *   · 每次都只取业务无关的 /models 清单，耗时短、可设超时。
 * 真要用于「定时批量探测」时，必须改写成非阻塞实现并放到独立进程，
 * 否则会拖住工作进程 —— 这点已写进项目文档的待办。
 */

declare(strict_types=1);

namespace app\common;

use RuntimeException;
use support\Log;

final class Channel
{
    /** 渠道状态：启用 / 停用 */
    public const STATUS_ENABLED = 1;
    public const STATUS_DISABLED = 0;

    /**
     * 协议适配器表 —— 决定「怎么跟上游说话」。
     *
     * 这里刻意只放**协议级**的差异，而不是把每个供应商都列一遍：
     * 供应商（谁、地址是什么）是数据，放在 config/providers.php；
     * 适配器（怎么通信）是代码，放在这里。一个适配器通常服务几十个供应商。
     *
     * `implemented` 是**诚实的可用性标记**：
     *   未实现的适配器不会出现在「可选」列表里（界面会置灰并标注），
     *   因为让用户选一个「选了却用不了」的选项，比不提供它更糟糕。
     *
     * 新增一个供应商时：多数情况**不需要动这个文件** ——
     * 只要它是 OpenAI 兼容的，在 config/providers.php 里加一段配置即可。
     */
    public const ADAPTERS = [
        // ── 已实现 ──────────────────────────────────────────
        'openai_sse' => [
            'label' => 'OpenAI 兼容（/v1/chat/completions + SSE）',
            'implemented' => true,
            'hint' => '标准协议，绝大多数国内外厂商都兼容它',
            // defaults：这个协议下「绝大多数厂商」的样子。
            // 选供应商后会自动带出，用户仍可在高级配置里逐项改写。
            'defaults' => ['auth_type' => 'bearer'],
        ],
        'nim' => [
            'label' => 'NVIDIA NIM',
            'implemented' => true,
            'hint' => '协议上兼容 OpenAI，但限流、thinking 字段名等行为有特殊性，故单独成类',
            'defaults' => ['auth_type' => 'bearer'],
        ],

        // ── 待实现：协议与 OpenAI 差异较大，需要独立的解码/编码实现 ──
        // 这些虽然暂时选不了，但**默认配置照旧声明** ——
        // 一是便于将来实现时直接用，二是让「差异长什么样」在代码里可见。
        'azure' => [
            'label' => 'Azure OpenAI',
            'implemented' => false,
            'hint' => '鉴权用 api-key 头，地址需带 api-version 查询参数',
            'defaults' => [
                'auth_type' => 'header',
                'auth_name' => 'api-key',
                'auth_prefix' => '',
                'extra_query' => ['api-version' => '2024-10-21'],
            ],
        ],
        'anthropic' => [
            'label' => 'Anthropic Messages',
            'implemented' => false,
            'hint' => '事件类型与字段名与 OpenAI 完全不同',
            'defaults' => [
                'auth_type' => 'header',
                'auth_name' => 'x-api-key',
                'auth_prefix' => '',
                'extra_headers' => ['anthropic-version' => '2023-06-01'],
            ],
        ],
        'gemini' => [
            'label' => 'Google Gemini',
            'implemented' => false,
            'hint' => 'generateContent 协议，流式结构差异大',
            'defaults' => ['auth_type' => 'query', 'auth_name' => 'key', 'auth_prefix' => ''],
        ],
        'vertex' => [
            'label' => 'Google Vertex AI',
            'implemented' => false,
            'hint' => 'GCP 服务账号鉴权',
            'defaults' => ['auth_type' => 'bearer'],
        ],
        'aws' => [
            'label' => 'AWS Bedrock',
            'implemented' => false,
            'hint' => 'SigV4 签名鉴权',
            'defaults' => ['auth_type' => 'none'],
        ],
        'baidu' => [
            'label' => '百度文心（access_token 流程）',
            'implemented' => false,
            'hint' => '需先用 API Key 换取 access_token',
            'defaults' => ['auth_type' => 'query', 'auth_name' => 'access_token', 'auth_prefix' => ''],
        ],
        'xunfei' => [
            'label' => '讯飞星火（WebSocket）',
            'implemented' => false,
            'hint' => '非 HTTP 协议',
            'defaults' => ['auth_type' => 'header', 'auth_name' => 'Authorization'],
        ],
        'coze' => [
            'label' => '扣子 Coze（机器人 / 工作流）',
            'implemented' => false,
            'hint' => '以会话/工作流为中心，不是对话补全',
            'defaults' => ['auth_type' => 'bearer'],
        ],
        'replicate' => [
            'label' => 'Replicate（异步任务）',
            'implemented' => false,
            'hint' => '提交任务 + 轮询结果',
            'defaults' => ['auth_type' => 'header', 'auth_name' => 'Authorization', 'auth_prefix' => 'Token '],
        ],
        'chatgpt_sub' => [
            'label' => 'ChatGPT 订阅账号（会话凭证）',
            'implemented' => false,
            'hint' => '用订阅账号凭证而非 API Key，凭证会过期需换新',
            'defaults' => ['auth_type' => 'bearer'],
        ],
        'ollama' => [
            'label' => 'Ollama 原生协议',
            'implemented' => false,
            'hint' => '非 OpenAI 格式；如需即刻可用请选它的 OpenAI 兼容层',
            'defaults' => ['auth_type' => 'none'],
        ],
    ];

    /**
     * 渠道「高级配置」的字段清单 —— 单一事实来源。
     *
     * 这一张表同时驱动三件事，因此加一个可配置项只需要改这里一处：
     *   1. 渠道表单长什么样（控件类型、标签、说明）
     *   2. 保存时怎么解析与校验
     *   3. 构造上游请求时怎么取值
     *
     * 设计原则：**能数据化的差异就不要写死在代码里**。
     * 各家上游在「鉴权方式、额外参数、超时」上的差异是无穷的，
     * 为每一种都加一列或加一段 if，正是同类项目代码膨胀到几十万行的原因。
     *
     * type 的含义：
     *   text     单行文本
     *   int      整数（0 表示「用全局默认」）
     *   select   下拉（选项见 options）
     *   headers  多行 `Name: Value` → 存成对象
     *   pairs    多行 `k=v` → 存成对象
     *   json     一个 JSON 对象 → 存成对象
     *   csv      逗号分隔 → 存成数组
     */
    public const ADV_FIELDS = [
        'auth_type' => [
            'label' => '鉴权方式',
            'type' => 'select',
            'default' => 'bearer',
            'options' => [
                'bearer' => 'Authorization: Bearer <Key>（最通用）',
                'header' => '自定义请求头，如 api-key: <Key>',
                'query' => 'URL 查询参数，如 ?key=<Key>',
                'none' => '不带鉴权（本地模型 / 免鉴权网关）',
            ],
            'hint' => '选供应商后会自动带出该家常用的方式',
        ],
        'auth_name' => [
            'label' => '鉴权字段名',
            'type' => 'text',
            'default' => '',
            'hint' => '留空用默认：Bearer 模式固定 Authorization；自定义头/查询参数模式默认 api-key',
        ],
        'auth_prefix' => [
            'label' => '鉴权值前缀',
            'type' => 'text',
            'default' => 'Bearer ',
            'hint' => '拼在 Key 前面的字符串。留空表示「沿用默认」（Bearer 模式为 "Bearer "，其余为空）',
        ],
        'extra_headers' => [
            'label' => '附加请求头',
            'type' => 'headers',
            'default' => [],
            'hint' => '每行一个，格式 名称: 值。用于上游要求的版本头、组织标识等',
        ],
        'extra_query' => [
            'label' => '附加查询参数',
            'type' => 'pairs',
            'default' => [],
            'hint' => '每行一个，格式 键=值。Azure OpenAI 的 api-version 就填这里',
        ],
        'extra_body' => [
            'label' => '附加请求体参数',
            'type' => 'json',
            'default' => [],
            'hint' => '一个 JSON 对象，会合并进请求体。例如 {"top_p":1,"stream_options":{"include_usage":true}}',
        ],
        'strip_body' => [
            'label' => '剔除请求体字段',
            'type' => 'csv',
            'default' => [],
            'hint' => '逗号分隔。有些上游收到无法识别的参数会直接报错，这里把它们去掉',
        ],
        'keep_body' => [
            'label' => '保留请求体字段',
            'type' => 'csv',
            'default' => [],
            'hint' => '逗号分隔。本站默认会把上游不认识的客户端私有字段剥掉'
                . '（例如某些客户端会带 dsh_plugin_packages，NIM 收到就整条请求 400）。'
                . '如果这家上游其实支持某个非标准字段，把它的名字填在这里',
        ],
        'model_map' => [
            'label' => '模型名映射（对外名=上游名）',
            'type' => 'pairs',
            'default' => [],
            'hint' => '每行一条，格式：对外名=上游真实名。'
                . '例：本站展示、用户调用的模型叫 llama-3.1-8b，而上游要求写 meta/llama-3.1-8b-instruct，'
                . '就填 llama-3.1-8b=meta/llama-3.1-8b-instruct。'
                . '用户始终用「对外名」请求本站，本站按这里的规则换成上游名再转发 —— 用户完全无感。'
                . '留空表示两边名字一致',
        ],
        'usage_fallback' => [
            'label' => '上游不返回用量时',
            'type' => 'select',
            'default' => 'estimate',
            'options' => [
                'estimate' => '按文本估算（推荐）',
                'zero' => '记 0 并标记「用量缺失」',
            ],
            'hint' => '三方中转、各种反代在流式响应里常常不回 usage 字段。'
                . '估算值会带上标记，绝不伪装成真实用量；'
                . '记 0 则完全不计费，只记录请求次数',
        ],
        'connect_timeout' => [
            'label' => '连接超时（秒）',
            'type' => 'int',
            'default' => 0,
            'hint' => '0 表示用全局默认',
        ],
        'total_timeout' => [
            'label' => '总超时（秒）',
            'type' => 'int',
            'default' => 0,
            'hint' => '0 表示用全局默认；流式请求的总时长上限',
        ],
        'proxy' => [
            'label' => '上游代理',
            'type' => 'text',
            'default' => '',
            'hint' => '如 http://127.0.0.1:7890 或 socks5h://127.0.0.1:1080。留空表示直连',
        ],
    ];

    /**
     * 判断某个适配器是否已实现（界面据此决定是否允许选择）。
     */
    public static function adapterImplemented(string $adapter): bool
    {
        return (bool) (self::ADAPTERS[$adapter]['implemented'] ?? false);
    }

    /**
     * 读取供应商预设清单（config/providers.php）。
     *
     * @return array<string, array<string, string>>
     */
    public static function providers(): array
    {
        return (array) config('providers', []);
    }

    /** 渠道列表（按优先级降序、权重降序，即路由的取用顺序） */
    public static function all(): array
    {
        return Db::select(
            'SELECT * FROM channels ORDER BY priority DESC, weight DESC, id ASC'
        );
    }

    /**
     * 读取单个渠道。
     *
     * @return array<string, mixed>|null
     */
    public static function find(int $id): ?array
    {
        return Db::selectOne('SELECT * FROM channels WHERE id = ?', [$id]);
    }

    /**
     * 新增渠道。
     *
     * @param array<string, mixed> $data
     * @return int 新渠道 id
     */
    public static function create(array $data): int
    {
        $now = time();

        Db::execute(
            'INSERT INTO channels
                (name, type, base_url, api_key_enc, models, config, priority, weight, status, rpm_limit, created_at, updated_at)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
            [
                (string) $data['name'],
                (string) $data['type'],
                (string) $data['base_url'],
                self::encryptKey((string) ($data['api_key'] ?? '')),
                self::encodeModels($data['models'] ?? []),
                self::encodeConfig($data['config'] ?? []),
                (int) ($data['priority'] ?? 0),
                (int) ($data['weight'] ?? 1),
                (int) ($data['status'] ?? self::STATUS_ENABLED),
                (int) ($data['rpm_limit'] ?? 0),
                $now,
                $now,
            ]
        );

        return (int) Db::pdo()->lastInsertId();
    }

    /**
     * 更新渠道。
     *
     * @param array<string, mixed> $data
     * @param bool $replaceKey 是否替换 Key。
     *        编辑页面不会回显原 Key（不把密钥送到浏览器），
     *        因此「Key 输入框留空」必须理解为「保持原 Key 不变」，
     *        否则用户改个名字就会把 Key 清空。
     */
    public static function update(int $id, array $data, bool $replaceKey): void
    {
        $fields = [
            'name = ?',
            'type = ?',
            'base_url = ?',
            'models = ?',
            'config = ?',
            'priority = ?',
            'weight = ?',
            'status = ?',
            'rpm_limit = ?',
            'updated_at = ?',
        ];

        $bindings = [
            (string) $data['name'],
            (string) $data['type'],
            (string) $data['base_url'],
            self::encodeModels($data['models'] ?? []),
            self::encodeConfig($data['config'] ?? []),
            (int) ($data['priority'] ?? 0),
            (int) ($data['weight'] ?? 1),
            (int) ($data['status'] ?? self::STATUS_ENABLED),
            (int) ($data['rpm_limit'] ?? 0),
            time(),
        ];

        if ($replaceKey) {
            $fields[] = 'api_key_enc = ?';
            $bindings[] = self::encryptKey((string) ($data['api_key'] ?? ''));
        }

        $bindings[] = $id;

        Db::execute(
            'UPDATE channels SET ' . implode(', ', $fields) . ' WHERE id = ?',
            $bindings
        );
    }

    /**
     * 删除渠道。
     */
    public static function delete(int $id): void
    {
        Db::execute('DELETE FROM channels WHERE id = ?', [$id]);
    }

    /**
     * 取回渠道的**明文** Key（仅用于发起上游请求）。
     *
     * 这是整个项目里唯一允许出现明文 Key 的地方。
     * 调用方必须遵守：拿到后只用于构造上游请求头，
     * **不得写日志、不得返回给前端、不得写进任何持久化字段**。
     */
    public static function plainKey(array $channel): string
    {
        $encrypted = (string) ($channel['api_key_enc'] ?? '');
        if ($encrypted === '') {
            return '';
        }

        try {
            return Crypto::decrypt($encrypted);
        } catch (RuntimeException $e) {
            // 解密失败通常意味着 APP_KEY 被换过。
            // 这里不把异常直接抛给上层，而是记日志并返回空串，
            // 让调用方按「没有 Key」处理 —— 否则整个渠道列表页都会崩掉。
            Log::error('渠道 #' . ($channel['id'] ?? '?') . ' 的 API Key 解密失败：' . $e->getMessage());

            return '';
        }
    }

    /**
     * 生成用于展示的 Key 掩码，例如 `nvapi-…9f3a`。
     *
     * 页面与日志里只允许出现这个掩码，绝不能出现完整 Key。
     * 掩码规则统一由 Crypto::mask() 提供，保证与密钥池的展示口径一致。
     */
    public static function maskedKey(array $channel): string
    {
        return Crypto::mask(self::plainKey($channel));
    }

    /**
     * 取一把可用于发起请求的 Key。
     *
     * 取用顺序：**优先密钥池，其次渠道自带的那把单 Key**。
     *
     * 这样设计的理由是兼顾两类使用者：
     *   · 开源版用户：一个渠道填一把 Key 就够用，不必理解「密钥池」这个概念；
     *   · 官方部署：一个渠道下挂几百把 Key，靠池子轮换把免费额度聚合起来。
     *
     * 注意：**只有从池子取到的 Key 才受每分钟限流约束** ——
     * 单 Key 场景下没有可轮换的余量，限流没有意义（超了就超了，没有备选）。
     * 从池子取用时，返回数组里会带 `class` 信息，调用方据此决定失败后怎么处置。
     *
     * @param int $defaultLimit 池子里单把 Key 的默认每分钟上限
     * @return array{key:string, key_id:int|null, from_pool:bool}|null
     *         null 表示确实是「没有任何 Key 可用」
     */
    public static function acquireKey(array $channel, int $defaultLimit = 40): ?array
    {
        $channelId = (int) ($channel['id'] ?? 0);

        if ($channelId > 0) {
            $poolKey = ChannelKey::acquire($channelId, $defaultLimit);
            if ($poolKey !== null) {
                return [
                    'key' => (string) $poolKey['plain_key'],
                    'key_id' => (int) $poolKey['id'],
                    'from_pool' => true,
                ];
            }

            // 池子非空但一把都取不到 => 全都用满了或被停用了。
            // 这种情况要返回 null（让上层报「当前无可用密钥」），
            // 而**不能**回落到渠道自带的单 Key —— 否则会绕过限流，
            // 把本该被限制的流量打到那把 Key 上。
            if (ChannelKey::statsForChannel($channelId)['total'] > 0) {
                return null;
            }
        }

        // 没有密钥池：退回单 Key 模式
        $single = self::plainKey($channel);

        return $single === '' ? null : ['key' => $single, 'key_id' => null, 'from_pool' => false];
    }

    // ═══════════════════════════════════════════════════════════
    // 路由选路（按模型挑渠道）
    // ═══════════════════════════════════════════════════════════

    /** 连续失败多少次后熔断该渠道 */
    public const FAIL_STREAK_TO_OPEN = 5;

    /** 熔断持续秒数（到点自动放出来再试） */
    public const BREAKER_SECONDS = 60;

    /**
     * 按模型挑出候选渠道，返回**已排好序**的列表。
     *
     * 排序规则（两段式，缺一不可）：
     *   1. **优先级高的优先**（priority 越大越优先）——
     *      站长用它表达「主线路 / 备用线路」这种明确意图，必须严格尊重；
     *   2. **同优先级内按权重加权随机** —— 这才是 weight 的语义。
     *      如果权重只用来排序（大的永远排前面），那它实际上和优先级没区别，
     *      流量根本不会按权重分摊。
     *
     * 被排除的渠道：
     *   · 未启用；· 已在 excludeIds 里（换渠道重试时排除刚失败的那条）；
     *   · 熔断未恢复；· 模型清单里没有这个模型。
     *
     * @param array<int, int> $excludeIds
     * @return array<int, array<string, mixed>>
     */
    public static function candidates(string $model, array $excludeIds = []): array
    {
        $buckets = [];

        foreach (self::all() as $channel) {
            if ((int) $channel['status'] !== self::STATUS_ENABLED) {
                continue;
            }

            if (in_array((int) $channel['id'], $excludeIds, true)) {
                continue;
            }

            if (self::breakerOpen($channel)) {
                continue;
            }

            if (!self::supportsModel($channel, $model)) {
                continue;
            }

            $buckets[(int) $channel['priority']][] = $channel;
        }

        // 优先级从高到低
        krsort($buckets, SORT_NUMERIC);

        $ordered = [];

        foreach ($buckets as $rows) {
            foreach (self::weightedShuffle($rows) as $row) {
                $ordered[] = $row;
            }
        }

        return $ordered;
    }

    /**
     * 该渠道是否支持这个模型。
     *
     * 模型清单为空 → 视为「不限」（与同类项目的惯例一致）。
     * 这样新建渠道后即使还没拉模型清单也能直接使用，不至于卡住。
     */
    public static function supportsModel(array $channel, string $model): bool
    {
        $models = self::modelsOf($channel);

        if ($models === []) {
            return true;
        }

        foreach ($models as $item) {
            $item = trim((string) $item);

            if ($item === '') {
                continue;
            }

            // 支持末尾通配：`gpt-4*` 匹配 gpt-4o / gpt-4-turbo
            if (str_ends_with($item, '*')) {
                if (str_starts_with($model, rtrim($item, '*'))) {
                    return true;
                }
                continue;
            }

            if ($item === $model) {
                return true;
            }
        }

        return false;
    }

    /**
     * 按 weight 做**无放回的加权抽样**，得到一个随机顺序。
     *
     * 加权随机的意义：权重 10 与权重 1 的两条渠道，
     * 前者排在第一位的概率是后者的 10 倍。长期看流量就按 10:1 分摊。
     */
    private static function weightedShuffle(array $rows): array
    {
        $picked = [];

        while ($rows !== []) {
            $total = 0;
            foreach ($rows as $row) {
                $total += max(1, (int) $row['weight']);
            }

            $roll = random_int(1, max(1, $total));
            $accumulated = 0;
            $chosen = null;

            foreach ($rows as $key => $row) {
                $accumulated += max(1, (int) $row['weight']);
                if ($roll <= $accumulated) {
                    $chosen = $key;
                    break;
                }
            }

            $chosen ??= array_key_first($rows);
            $picked[] = $rows[$chosen];
            unset($rows[$chosen]);
        }

        return $picked;
    }

    /**
     * 该渠道是否处于熔断中。
     */
    public static function breakerOpen(array $channel): bool
    {
        return (int) ($channel['breaker_until'] ?? 0) > time();
    }

    /**
     * 记录一次渠道级失败（连不上、超时、上游 5xx 等）。
     *
     * 连续失败达到阈值就短路一段时间。与密钥池的冷却同理：
     * 到期自动恢复，不需要人工干预 —— 否则上游抖一下，
     * 站长就得手工把每条渠道点回来。
     */
    public static function markChannelFailure(array $channel, int $status, string $message): void
    {
        $id = (int) ($channel['id'] ?? 0);
        if ($id <= 0) {
            return;
        }

        $streak = (int) ($channel['fail_streak'] ?? 0) + 1;
        $now = time();
        $until = $streak >= self::FAIL_STREAK_TO_OPEN ? $now + self::BREAKER_SECONDS : null;

        Db::execute(
            'UPDATE channels
             SET fail_streak = ?, breaker_until = ?, last_error = ?, updated_at = ?
             WHERE id = ?',
            [$streak, $until, mb_substr($message, 0, 450), $now, $id]
        );

        if ($until !== null) {
            Log::warning(sprintf(
                '渠道「%s」连续失败 %d 次，已熔断 %d 秒：%s',
                (string) ($channel['name'] ?? $id),
                $streak,
                self::BREAKER_SECONDS,
                $message
            ));
        }
    }

    /**
     * 记录一次渠道级成功：清空失败连击并解除熔断。
     */
    public static function markChannelSuccess(array $channel): void
    {
        $id = (int) ($channel['id'] ?? 0);
        if ($id <= 0) {
            return;
        }

        // 本来就没有失败记录时不必写库 —— 成功是常态，
        // 每次成功都写一次会让数据库承受大量无意义的 UPDATE
        if ((int) ($channel['fail_streak'] ?? 0) === 0 && empty($channel['breaker_until'])) {
            return;
        }

        Db::execute(
            'UPDATE channels SET fail_streak = 0, breaker_until = NULL, updated_at = ? WHERE id = ?',
            [time(), $id]
        );
    }

    /**
     * 渠道测活：验证「地址可达」+「Key 有效」两件事。
     *
     * ⚠️ 为什么必须做两步（这是实测踩出来的坑）：
     *   很多上游（包括 NVIDIA NIM）的 `/models` 端点**不需要鉴权** ——
     *   用一个完全无效的 Key 去请求，照样返回 200 和完整模型清单。
     *   如果只测 `/models`，就会得到「连接成功」这个**假阳性**，
     *   让站长以为 Key 是好的，直到真正发起推理才发现 401。
     *
     *   所以这里分两步：
     *     ① GET /models            —— 证明网络可达，并顺带拿到模型清单
     *     ② POST /chat/completions —— 发一个 max_tokens=1 的最小请求，
     *                                 这才是真正校验 Key 的步骤
     *   第二步会消耗极少量额度（通常几个 token），这是为「结论可信」付的代价。
     *
     * @return array{ok: bool, message: string, models: array<int, string>, http_code: int}
     */
    public static function test(int $id): array
    {
        $channel = self::find($id);
        if ($channel === null) {
            return ['ok' => false, 'message' => '渠道不存在', 'models' => [], 'http_code' => 0];
        }

        // 优先用**密钥池**里的一把。
        // 官方部署下渠道可能挂几百把 Key，测活从池子取才能同时验证两件事：
        // 「池子取得到 Key」以及「取出来的这把 Key 确实有效」。
        $acquired = self::acquireKey($channel, Settings::int('key_pool.default_rpm', 40));

        if ($acquired === null) {
            $poolTotal = ChannelKey::statsForChannel((int) $channel['id'])['total'];

            $result = [
                'ok' => false,
                'message' => $poolTotal > 0
                    ? "密钥池中有 {$poolTotal} 把 Key，但当前全部不可用（已达每分钟上限或已停用）"
                    : '该渠道没有可用的 API Key',
                'models' => [],
                'http_code' => 0,
            ];
            self::recordTest($id, $result);

            return $result;
        }

        $key = $acquired['key'];

        // ── 第一步：拉取模型清单，确认网络可达 ──
        $listResponse = self::send(self::buildSpec($channel, $key, '/models'));

        if ($listResponse['error'] !== '') {
            $result = [
                'ok' => false,
                'message' => '无法连接上游：' . $listResponse['error'],
                'models' => [],
                'http_code' => $listResponse['code'],
            ];
            self::recordTest($id, $result);

            return $result;
        }

        $models = self::extractModelIds($listResponse['body']);

        // 「只上架能真正调用的模型」开着时，把**已经确定**不能用的模型先剔掉再上架。
        //
        // 依据是上一次检测的结论（no_access / unroutable）。首次拉取时还没检测过，
        // 此时不剔任何东西 —— 判定必须有据可依，不能凭猜。
        $dropped = 0;
        if ($models !== [] && Settings::bool('probe.free_only', true)) {
            $unusable = ModelProbeTask::unusableModels($id);
            if ($unusable !== []) {
                $filtered = array_values(array_filter(
                    $models,
                    static fn (string $m): bool => !in_array($m, $unusable, true)
                ));
                $dropped = count($models) - count($filtered);
                $models = $filtered;
            }
        }

        // 若渠道还没配置模型清单，把拉到的清单回填，省去手工录入。
        // 注意判断方式：空数组存进库是字符串 `'[]'` 而不是空串，
        // 所以这里必须用 modelsOf() 判断「逻辑上是否为空」，
        // 否则回填逻辑永远不会触发（这是实际踩到过的 bug）。
        if ($models !== [] && self::modelsOf($channel) === []) {
            Db::execute(
                'UPDATE channels SET models = ?, updated_at = ? WHERE id = ?',
                [self::encodeModels($models), time(), $id]
            );
        }

        // 没拿到模型清单就没法做第二步的推理校验，只能给「网络通但未验证 Key」的结论
        if ($models === []) {
            $result = [
                'ok' => false,
                'message' => "网络可达（HTTP {$listResponse['code']}），但未取到模型清单，无法进一步校验 Key。"
                    . '请手动填写至少一个模型名后重试。',
                'models' => [],
                'http_code' => $listResponse['code'],
            ];
            self::recordTest($id, $result);

            return $result;
        }

        // ── 第二步：用最小推理请求真正校验 Key ──
        $probeModel = self::pickProbeModel($channel, $models);
        $chatResponse = self::send(self::buildSpec($channel, $key, '/chat/completions', [
            'model' => $probeModel,
            'messages' => [['role' => 'user', 'content' => 'hi']],
            'max_tokens' => 1,
            'stream' => false,
        ]));

        $result = self::interpretProbe($chatResponse, $probeModel, $models);
        if ($dropped > 0) {
            // 如实说明剔掉了几个：否则站长会觉得「上游明明有 80 个模型，怎么只上架了 60 个」
            $result['message'] .= "（已按「只上架能真正调用的模型」跳过 {$dropped} 个已知无权限/不支持的模型）";
        }
        self::recordTest($id, $result);

        // 把探测结果反馈到密钥池，让池子的健康状态跟着更新。
        // ⚠️ 只有「Key 无效（401/403）」才算永久失败。
        // 429、5xx、网络抖动都不能停用 Key —— 否则上游一次抖动就会把
        // 几百把本来好好的 Key 全部停掉，这正是项目文档里
        // 「瞬时故障禁止进 retired」那条原则的落实。
        if ($acquired['key_id'] !== null) {
            if ($result['ok']) {
                ChannelKey::markSuccess($acquired['key_id']);
            } else {
                ChannelKey::markFailure(
                    $acquired['key_id'],
                    (string) $result['message'],
                    in_array($result['http_code'], [401, 403], true)
                );
            }
        }

        return $result;
    }

    /**
     * 根据推理探测的结果给出结论。
     *
     * 单独抽出来是为了让「状态码 → 人话」的映射集中可见 ——
     * 这类映射最容易写漏分支，散在流程里以后没人看得全。
     *
     * @param array{code:int, body:string, error:string} $probe
     * @param array<int, string> $models
     * @return array{ok: bool, message: string, models: array<int,string>, http_code: int}
     */
    private static function interpretProbe(array $probe, string $probeModel, array $models): array
    {
        $count = count($models);

        // Key 无效：这是测活最需要准确报出来的一种情况
        if (in_array($probe['code'], [401, 403], true)) {
            return [
                'ok' => false,
                'message' => "API Key 无效或无权限（推理请求返回 HTTP {$probe['code']}）。"
                    . '注意：上游的模型清单接口通常是公开的，所以「能列出模型」并不代表 Key 可用。',
                'models' => $models,
                'http_code' => $probe['code'],
            ];
        }

        // 429：Key 有效，只是当前被限流 —— 这其实是「可用」的正面信号
        if ($probe['code'] === 429) {
            return [
                'ok' => true,
                'message' => "Key 有效，但当前被上游限流（HTTP 429）。可用模型 {$count} 个。",
                'models' => $models,
                'http_code' => $probe['code'],
            ];
        }

        // 400/404：Key 通过了鉴权，但这次探测请求本身不被接受
        // （最常见的原因是所选模型对该 Key 不可用）
        if (in_array($probe['code'], [400, 404], true)) {
            return [
                'ok' => true,
                'message' => "Key 通过鉴权，但探测模型「{$probeModel}」不可用（HTTP {$probe['code']}）。"
                    . "可尝试在渠道里指定其它模型。已发现 {$count} 个模型。",
                'models' => $models,
                'http_code' => $probe['code'],
            ];
        }

        if ($probe['error'] !== '') {
            return [
                'ok' => false,
                'message' => '推理请求失败：' . $probe['error'],
                'models' => $models,
                'http_code' => $probe['code'],
            ];
        }

        if ($probe['code'] === 200) {
            return [
                'ok' => true,
                'message' => "Key 有效，推理请求成功。已发现 {$count} 个可用模型。",
                'models' => $models,
                'http_code' => 200,
            ];
        }

        return [
            'ok' => false,
            'message' => "推理请求返回异常状态码 HTTP {$probe['code']}",
            'models' => $models,
            'http_code' => $probe['code'],
        ];
    }

    /**
     * 选一个用于探测的模型。
     *
     * 优先用渠道自己配置的模型（那是站长明确想用的），
     * 否则用上游清单里的第一个 —— 目的是「只要能通过鉴权即可」，
     * 不追求一定成功推理，因此不必挑最便宜的模型。
     *
     * @param array<int, string> $upstreamModels
     */
    private static function pickProbeModel(array $channel, array $upstreamModels): string
    {
        $configured = self::modelsOf($channel);
        $candidates = $configured !== [] ? $configured : $upstreamModels;

        // 避开已知无权限的模型：拿一个明知道会 403 的模型去验证 Key，
        // 会把一把好 Key 判成「无效」—— 那是最容易让人白折腾半天的误报
        if (Settings::bool('probe.free_only', true)) {
            $unusable = ModelProbeTask::unusableModels((int) ($channel['id'] ?? 0));
            if ($unusable !== []) {
                $usable = array_values(array_filter(
                    $candidates,
                    static fn (mixed $m): bool => !in_array((string) $m, $unusable, true)
                ));
                if ($usable !== []) {
                    $candidates = $usable;
                }
            }
        }

        return (string) $candidates[0];
    }

    /**
     * 把测试结果写入渠道记录（成功/失败/时间/错误摘要）。
     */
    private static function recordTest(int $id, array $result): void
    {
        Db::execute(
            'UPDATE channels SET last_test_at = ?, last_test_ok = ?, last_error = ?, updated_at = ? WHERE id = ?',
            [
                time(),
                $result['ok'] ? 1 : 0,
                // 错误信息截断存储，避免超长响应把字段撑爆
                mb_substr((string) $result['message'], 0, 450),
                time(),
                $id,
            ]
        );
    }

    /**
     * 从上游的模型清单响应里提取模型 id。
     *
     * 兼容 OpenAI 风格 `{"data":[{"id":"..."}]}`；
     * 也兼容直接返回数组 `["模型名", ...]` 的少数实现。
     *
     * @return array<int, string>
     */
    private static function extractModelIds(string $body): array
    {
        $decoded = json_decode($body, true);
        if (!is_array($decoded)) {
            return [];
        }

        $items = $decoded['data'] ?? $decoded;

        $ids = [];
        foreach ((array) $items as $item) {
            if (is_array($item) && isset($item['id'])) {
                $ids[] = (string) $item['id'];
            } elseif (is_string($item)) {
                $ids[] = $item;
            }
        }

        $ids = array_values(array_unique(array_filter($ids)));
        sort($ids);

        return $ids;
    }

    /**
     * 加密 Key。空串按空处理（允许先建渠道、后补 Key）。
     */
    private static function encryptKey(string $plain): string
    {
        return $plain === '' ? '' : Crypto::encrypt($plain);
    }

    /**
     * 模型清单：数组 → JSON 字符串（存库用）。
     *
     * @param array<int, string>|string $models
     */
    private static function encodeModels(array|string $models): string
    {
        if (is_string($models)) {
            // 表单里是一行一个模型的文本，统一按换行/逗号切分
            $models = preg_split('/[\s,]+/', $models) ?: [];
        }

        $models = array_values(array_unique(array_filter(array_map('trim', $models))));

        return (string) json_encode($models, JSON_UNESCAPED_UNICODE);
    }

    /**
     * 模型清单：JSON 字符串 → 数组（读取用）。
     *
     * @return array<int, string>
     */
    public static function modelsOf(array $channel): array
    {
        $raw = (string) ($channel['models'] ?? '');
        if ($raw === '') {
            return [];
        }

        $decoded = json_decode($raw, true);

        return is_array($decoded) ? array_values($decoded) : [];
    }

    // ═══════════════════════════════════════════════════════════
    // 渠道级高级配置
    // ═══════════════════════════════════════════════════════════

    /**
     * 取出渠道的**最终生效**高级配置。
     *
     * 三层合并，后者覆盖前者：
     *   ① Channel::ADV_FIELDS 里声明的默认值（保底，任何适配器都有）
     *   ② 该渠道所用适配器的 defaults（例如 Azure 的 api-key 头 + api-version）
     *   ③ 渠道自身 config 列里的值（站长在高级配置里填的，优先级最高）
     *
     * 为什么要做「三层」而不是直接用渠道里的值：
     *   绝大多数渠道只需要「地址 + Key」，高级配置全为空。
     *   靠适配器默认值兜底，这些渠道不必写任何配置也能正确鉴权；
     *   而真遇到怪癖上游时，站长又能逐项覆盖。
     *
     * @return array<string, mixed>
     */
    public static function advConfig(array $channel): array
    {
        $result = [];
        foreach (self::ADV_FIELDS as $name => $spec) {
            $result[$name] = $spec['default'];
        }

        $adapterDefaults = (array) (self::ADAPTERS[(string) ($channel['type'] ?? '')]['defaults'] ?? []);
        foreach ($adapterDefaults as $name => $value) {
            if (array_key_exists($name, $result)) {
                $result[$name] = $value;
            }
        }

        $stored = self::decodeConfig($channel['config'] ?? null);
        foreach ($stored as $name => $value) {
            if (array_key_exists($name, $result)) {
                $result[$name] = $value;
            }
        }

        return $result;
    }

    /**
     * 存库前的配置编码：空配置存空串（而不是 '{}'）。
     *
     * 与 models 字段的处理不同，这里不做「空值也算有值」的兼容 ——
     * 因为 config 是新增列，历史数据本来就是空串，
     * 统一成空串可以让「是否有自定义配置」的判断保持一行。
     *
     * @param array<string, mixed>|string|null $config
     */
    private static function encodeConfig(array|string|null $config): string
    {
        if ($config === null || $config === '') {
            return '';
        }

        if (is_string($config)) {
            $decoded = json_decode($config, true);
            $config = is_array($decoded) ? $decoded : [];
        }

        $config = array_filter($config, static fn ($v) => $v !== '' && $v !== [] && $v !== 0 && $v !== '0');

        return $config === []
            ? ''
            : (string) json_encode($config, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);
    }

    /**
     * 读取渠道的原始配置。兼容「列还不存在」的历史库结构。
     *
     * @return array<string, mixed>
     */
    public static function decodeConfig(mixed $raw): array
    {
        if (!is_string($raw) || $raw === '') {
            return [];
        }

        $decoded = json_decode($raw, true);

        return is_array($decoded) ? $decoded : [];
    }

    /**
     * 把表单提交的高级配置文本解析成结构化数组。
     *
     * 解析放在「保存时」而不是「每次请求时」：
     * 请求转发是热路径，每毫秒都值得省；
     * 而保存是站长手动点一次的操作，慢一点无所谓。
     * 代价是解析规则变更后旧数据不会自动重解析 ——
     * 但解析规则本身足够稳定，这个取舍是划算的。
     *
     * 非法输入不抛异常，而是**丢弃该项并收集错误信息**：
     * 让站长一次看到全部问题，而不是改一个报一个。
     *
     * @param array<string, mixed> $post 表单原始提交（$_POST 风格）
     * @return array{config: array<string, mixed>, errors: array<int, string>}
     */
    public static function parseAdvForm(array $post): array
    {
        $config = [];
        $errors = [];

        foreach (self::ADV_FIELDS as $name => $spec) {
            $raw = $post[$name] ?? null;

            // 下拉：只接受清单内的取值，防止表单被改出脏值
            if ($spec['type'] === 'select') {
                $value = trim((string) $raw);
                if ($value !== '' && !isset($spec['options'][$value])) {
                    $errors[] = "「{$spec['label']}」的取值不在允许范围内";
                    continue;
                }
                $config[$name] = $value === '' ? (string) $spec['default'] : $value;
                continue;
            }

            if ($spec['type'] === 'int') {
                $value = (int) $raw;
                $config[$name] = $value >= 0 ? $value : 0;
                continue;
            }

            if (in_array($spec['type'], ['text'], true)) {
                $config[$name] = trim((string) $raw);
                continue;
            }

            // 以下都是多行/结构化输入，统一按文本处理
            $text = trim((string) $raw);

            if ($text === '') {
                $config[$name] = [];
                continue;
            }

            switch ($spec['type']) {
                case 'headers':
                    $parsed = self::parseHeaderLines($text);
                    if ($parsed === null) {
                        $errors[] = "「{$spec['label']}」格式不正确，每行应为 名称: 值";
                        continue 2;
                    }
                    $config[$name] = $parsed;
                    break;

                case 'pairs':
                    $parsed = self::parsePairLines($text);
                    if ($parsed === null) {
                        $errors[] = "「{$spec['label']}」格式不正确，每行应为 键=值";
                        continue 2;
                    }
                    $config[$name] = $parsed;
                    break;

                case 'csv':
                    $config[$name] = array_values(array_filter(array_map(
                        'trim',
                        preg_split('/[\s,]+/', $text) ?: []
                    )));
                    break;

                default: // json
                    $decoded = json_decode($text, true);
                    if (!is_array($decoded)) {
                        $errors[] = "「{$spec['label']}」必须是合法的 JSON 对象，当前内容无法解析";
                        continue 2;
                    }
                    $config[$name] = $decoded;
            }
        }

        return ['config' => $config, 'errors' => $errors];
    }

    /**
     * 解析 `名称: 值` 多行文本。
     * 含「没有冒号的行」时返回 null（表示输入有误），由调用方给出提示。
     *
     * @return array<string, string>|null
     */
    private static function parseHeaderLines(string $text): ?array
    {
        $result = [];

        foreach (preg_split('/\r\n|\r|\n/', $text) ?: [] as $line) {
            $line = trim($line);
            if ($line === '') {
                continue;
            }

            $pos = strpos($line, ':');
            if ($pos === false) {
                return null;
            }

            $name = trim(substr($line, 0, $pos));
            if ($name === '') {
                return null;
            }

            $result[$name] = trim(substr($line, $pos + 1));
        }

        return $result;
    }

    /**
     * 解析 `键=值` 多行文本。
     *
     * @return array<string, string>|null
     */
    private static function parsePairLines(string $text): ?array
    {
        $result = [];

        foreach (preg_split('/\r\n|\r|\n/', $text) ?: [] as $line) {
            $line = trim($line);
            if ($line === '') {
                continue;
            }

            $pos = strpos($line, '=');
            if ($pos === false) {
                return null;
            }

            $key = trim(substr($line, 0, $pos));
            if ($key === '') {
                return null;
            }

            $result[$key] = trim(substr($line, $pos + 1));
        }

        return $result;
    }

    /**
     * 把结构化配置还原成表单可编辑的文本（用于编辑页回显）。
     *
     * @param array<string, mixed> $config
     * @return array<string, string> 字段名 => 文本值
     */
    public static function advFormText(array $config): array
    {
        $text = [];

        foreach (self::ADV_FIELDS as $name => $spec) {
            $value = $config[$name] ?? $spec['default'];

            $text[$name] = match ($spec['type']) {
                'headers' => implode("\n", array_map(
                    static fn ($k, $v) => $k . ': ' . $v,
                    array_keys((array) $value),
                    array_values((array) $value)
                )),
                'pairs' => implode("\n", array_map(
                    static fn ($k, $v) => $k . '=' . $v,
                    array_keys((array) $value),
                    array_values((array) $value)
                )),
                'csv' => implode(', ', (array) $value),
                'json' => $value === [] ? '' : (string) json_encode($value, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES),
                'int' => (string) (int) $value,
                default => (string) $value,
            };
        }

        return $text;
    }

    /**
     * 模型名映射：把「对外统一模型名」翻译成「上游要求的真实模型名」。
     *
     * 例：对外都叫 `gpt-4o`，而某中转站要求写 `openai/gpt-4o`，
     * 在 model_map 里配一行 `gpt-4o=openai/gpt-4o` 即可，调用方无感。
     * 没配映射时原样返回，因此这个能力对未使用它的渠道零成本。
     */
    public static function upstreamModel(array $channel, string $model): string
    {
        $map = (array) (self::advConfig($channel)['model_map'] ?? []);

        return isset($map[$model]) ? (string) $map[$model] : $model;
    }

    /**
     * OpenAI 兼容请求体的**标准字段**（白名单）。
     *
     * 为什么要有这份清单：网关对请求体的处理一直是「原样搬过去」，
     * 而各家客户端都会往里塞自己的东西。实测踩到的例子：
     * 某客户端带 `dsh_plugin_packages`，NIM 收到直接 400
     * （`Validation: Unsupported parameter(s)`）—— 整条请求失败，
     * 而用户完全不知道原因，只会觉得「这个站调不通」。
     *
     * 这份清单只列**公认**的字段：OpenAI 官方接口 + 各家常见的兼容字段。
     * 各家自己的私有字段留给「渠道高级配置 → 保留请求体字段」声明 ——
     * 因为「这家上游支持什么」是渠道级知识，不该写死在一张全局表里。
     *
     * ⚠️ 漏掉一个字段的后果是「它被静默剥掉」，所以剥离时会记一条日志（去重后），
     *    站长能在日志里看到究竟是哪些字段被剥了。
     */
    private const STANDARD_BODY_FIELDS = [
        // 必需
        'model' => true, 'messages' => true,
        // 兼容：老式补全接口 / embeddings / 多模态输入
        'prompt' => true, 'input' => true, 'suffix' => true,
        // 采样与长度
        'temperature' => true, 'top_p' => true, 'top_k' => true, 'n' => true,
        'max_tokens' => true, 'max_completion_tokens' => true, 'max_output_tokens' => true,
        'stop' => true, 'seed' => true, 'best_of' => true, 'echo' => true,
        'presence_penalty' => true, 'frequency_penalty' => true, 'repetition_penalty' => true,
        'logit_bias' => true, 'logprobs' => true, 'top_logprobs' => true,
        // 流式
        'stream' => true, 'stream_options' => true,
        // 工具调用
        'tools' => true, 'tool_choice' => true, 'parallel_tool_calls' => true,
        'functions' => true, 'function_call' => true,
        // 输出形态
        'response_format' => true, 'modalities' => true, 'audio' => true, 'prediction' => true,
        // 其它官方字段
        'user' => true, 'metadata' => true, 'store' => true, 'service_tier' => true,
        'reasoning_effort' => true, 'verbosity' => true, 'prompt_cache_key' => true,
        // 各家常见的兼容字段（推理开关、向量维度等）
        'thinking' => true, 'enable_thinking' => true, 'reasoning' => true,
        'dimensions' => true, 'encoding_format' => true,
    ];

    /** 已经记过日志的「被剥离字段」，避免每次请求刷一条 */
    private static array $reportedStrippedFields = [];

    /**
     * 把**上游不认识**的客户端私有字段剥掉。
     *
     * 保留的是：标准字段（白名单）+ 全局「额外保留」+ 本渠道「保留请求体字段」。
     * 写完新字段名会记一条（去重）日志，方便发现「原来有客户端在用这个字段」。
     *
     * @param array<string, mixed> $body
     * @return array<string, mixed>
     */
    public static function sanitizeBody(array $channel, array $body): array
    {
        if (!Settings::bool('gateway.strip_unknown_params', true)) {
            return $body;
        }

        $keep = [];
        foreach (self::keepBodyFields($channel) as $field) {
            $keep[$field] = true;
        }

        $stripped = [];
        foreach (array_keys($body) as $name) {
            $field = (string) $name;
            if (isset(self::STANDARD_BODY_FIELDS[$field]) || isset($keep[$field])) {
                continue;
            }

            unset($body[$field]);
            $stripped[] = $field;
        }

        if ($stripped !== []) {
            self::reportStrippedFields((string) ($channel['name'] ?? ''), $stripped);
        }

        return $body;
    }

    /**
     * 本渠道允许保留的非标准字段：全局配置 + 渠道配置一起算。
     *
     * 两级都要有，因为它们的用途不同：
     *   · 全局那级是「我们所有上游都认这个字段」（比如站内统一用 reasoning_effort）
     *   · 渠道那级是「只有这家认」（比如某家私有的 chat_template_kwargs）
     *
     * @return array<int, string>
     */
    public static function keepBodyFields(array $channel): array
    {
        $adv = self::advConfig($channel);
        $fields = [];

        // ⚠️ 两种形态都要处理，这不是洁癖：
        //   · 渠道级 `keep_body` 在保存时已被解析成**数组**（见 parseAdvForm）
        //   · 全局配置是一串**逗号分隔的文本**
        // 曾经只按文本处理，于是对数组做了一次 (string) 转换 ——
        // 那会抛 Array to string conversion，而引擎的 prepare() 会把异常
        // 兜成「当前没有可用的上游渠道」，结果是**整站所有转发都 503**，
        // 而错误信息完全指不到真正的原因（这个坑已经踩过一次）。
        foreach ([
            Settings::get('gateway.keep_body_params', ''),
            $adv['keep_body'] ?? [],
        ] as $source) {
            $list = is_array($source)
                ? $source
                : (preg_split('/[\s,]+/', (string) $source) ?: []);

            foreach ($list as $field) {
                $field = trim((string) $field);
                if ($field !== '') {
                    $fields[$field] = true;
                }
            }
        }

        return array_keys($fields);
    }

    /**
     * 记一条「剥掉了哪些字段」的日志（同一字段只记一次）。
     *
     * 为什么要去重：这个动作发生在**每一个转发请求**上，
     * 不去重的话一个客户端就能把日志刷满，真正的问题反而被淹没。
     */
    private static function reportStrippedFields(string $channelName, array $fields): void
    {
        $fresh = [];
        foreach ($fields as $field) {
            if (isset(self::$reportedStrippedFields[$field])) {
                continue;
            }

            self::$reportedStrippedFields[$field] = true;
            $fresh[] = $field;
        }

        if ($fresh === []) {
            return;
        }

        Log::info('已剥掉上游不认识的请求字段（渠道「' . $channelName . '」）：' . implode('、', $fresh)
            . '。若这家上游其实支持其中某个字段，请把它写进渠道高级配置的「保留请求体字段」；'
            . '若所有上游都支持，写进「配置管理 → 网关 → 额外保留的请求字段名」');
    }

    /**
     * 构造一次上游请求的完整规格（URL / 请求头 / 请求体 / curl 选项）。
     *
     * 抽成独立方法的理由：**流式转发与测活必须用同一套构造逻辑**。
     * 若各写一份，迟早会出现「测活能通、线上转发报 401」这类
     * 极难定位的不一致 —— 鉴权头的拼法尤其容易写歪。
     *
     * 注意这里**不发起请求**，只产出规格，因此可以被未来的
     * 非阻塞流式引擎直接复用（它需要的是规格，不是 curl 句柄）。
     *
     * @param array<string, mixed> $channel 渠道记录
     * @param string $key    明文 Key（可为空，例如本地免鉴权模型）
     * @param string $path   相对路径，如 /chat/completions
     * @param array<string, mixed>|null $body 请求体；null 表示无请求体（GET）
     * @param int|null $fallbackTotal 渠道未配总超时时的兜底秒数。
     *        测活传 null（用较短的探测超时）；正式转发传 gateway.total_timeout
     *        —— 探测要「快速失败」，转发要「给足时间」，两者的合理值差很多
     * @return array{url:string, headers:array<int,string>, body:?string, connect_timeout:int, total_timeout:int, proxy:string}
     */
    public static function buildSpec(
        array $channel,
        string $key,
        string $path,
        ?array $body = null,
        ?int $fallbackTotal = null
    ): array {
        $adv = self::advConfig($channel);

        $baseUrl = rtrim((string) $channel['base_url'], '/');
        $url = $baseUrl . $path;

        // ── 附加查询参数 ──
        // 用 http_build_query 而不是手工拼 &，以便正确处理需要转义的值
        $extraQuery = (array) ($adv['extra_query'] ?? []);
        if ($extraQuery !== []) {
            $url .= (str_contains($url, '?') ? '&' : '?') . http_build_query($extraQuery);
        }

        // ── 鉴权 ──
        // Accept 与 Expect 是刻意加的：
        //   · Accept 声明只收 JSON，避免上游按浏览器偏好返回 HTML 错误页
        //   · Expect 置空是为了压掉 curl 对大于 1KB 的请求体自动加上的
        //     `Expect: 100-continue`。部分上游不认这个头，会白等 1 秒才收请求体
        $headers = ['Accept: application/json', 'Expect:'];
        $authType = (string) ($adv['auth_type'] ?? 'bearer');
        $authName = trim((string) ($adv['auth_name'] ?? ''));
        $authPrefix = (string) ($adv['auth_prefix'] ?? '');

        if ($key !== '') {
            switch ($authType) {
                case 'bearer':
                    $headers[] = 'Authorization: ' . $authPrefix . $key;
                    break;

                case 'header':
                    $headers[] = ($authName !== '' ? $authName : 'api-key') . ': ' . $authPrefix . $key;
                    break;

                case 'query':
                    $url .= (str_contains($url, '?') ? '&' : '?')
                        . rawurlencode($authName !== '' ? $authName : 'api-key')
                        . '=' . rawurlencode($key);
                    break;

                case 'none':
                default:
                    // 明确不带鉴权：本地自建模型、内网网关常见
                    break;
            }
        }

        // ── 附加请求头（放在鉴权之后，允许覆盖同名头）──
        foreach ((array) ($adv['extra_headers'] ?? []) as $name => $value) {
            $headers[] = $name . ': ' . $value;
        }

        // ── 请求体 ──
        $encodedBody = null;

        if ($body !== null) {
            // 0) 剔除上游不接受的字段（站长手工声明的）
            foreach ((array) ($adv['strip_body'] ?? []) as $strip) {
                unset($body[$strip]);
            }

            // 0.1) 剥掉**上游不认识的客户端私有字段**（自动，默认开）。
            //      必须在合并 extra_body **之前**做：
            //      extra_body 是站长自己写进这家上游的字段，属于「已知支持」，不能被剥掉
            $body = self::sanitizeBody($channel, $body);

            // 1) 合并附加参数。顺序很关键：先铺附加参数，再写业务参数 ——
            //    这样业务参数（model/messages/stream）永远压过附加配置，
            //    站长不可能用一个手填的 {"model":"x"} 把真实模型名顶掉。
            $body = array_merge((array) ($adv['extra_body'] ?? []), $body);

            // 2) 模型名映射
            if (isset($body['model']) && is_string($body['model'])) {
                $body['model'] = self::upstreamModel($channel, $body['model']);
            }

            $headers[] = 'Content-Type: application/json';
            $encodedBody = (string) json_encode($body, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);
        }

        // 超时：渠道级没配（0）就用全局默认，全局默认来自函数签名之外的调用方
        $connect = (int) ($adv['connect_timeout'] ?? 0);
        $total = (int) ($adv['total_timeout'] ?? 0);

        return [
            'url' => $url,
            'headers' => $headers,
            'body' => $encodedBody,
            'connect_timeout' => $connect > 0 ? $connect : Settings::int('gateway.connect_timeout', 8),
            'total_timeout' => $total > 0
                ? $total
                : ($fallbackTotal ?? Settings::int('gateway.probe_timeout', 20)),
            'proxy' => trim((string) ($adv['proxy'] ?? '')),
        ];
    }

    /**
     * 按规格发起一次阻塞式 HTTP 请求。
     *
     * ⚠️ 与项目主线的「非阻塞流式转发」相反 —— 这里只服务于测活
     * （站长手动点一次，可接受阻塞）。定时批量探测必须改成非阻塞实现。
     *
     * @param array{url:string, headers:array<int,string>, body:?string, connect_timeout:int, total_timeout:int, proxy:string} $spec
     * @return array{code:int, body:string, error:string}
     */
    private static function send(array $spec): array
    {
        $ch = curl_init($spec['url']);

        $options = [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_CUSTOMREQUEST => $spec['body'] === null ? 'GET' : 'POST',
            CURLOPT_HTTPHEADER => $spec['headers'],
            CURLOPT_CONNECTTIMEOUT => $spec['connect_timeout'],
            CURLOPT_TIMEOUT => $spec['total_timeout'],
            CURLOPT_SSL_VERIFYPEER => true,
        ];

        if ($spec['body'] !== null) {
            $options[CURLOPT_POSTFIELDS] = $spec['body'];
        }

        if ($spec['proxy'] !== '') {
            $options[CURLOPT_PROXY] = $spec['proxy'];
        }

        curl_setopt_array($ch, $options);

        $body = curl_exec($ch);
        $code = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
        $error = curl_error($ch);
        curl_close($ch);

        return [
            'code' => $code,
            'body' => $body === false ? '' : (string) $body,
            'error' => $error,
        ];
    }
}
