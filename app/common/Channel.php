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
        ],
        'nim' => [
            'label' => 'NVIDIA NIM',
            'implemented' => true,
            'hint' => '协议上兼容 OpenAI，但限流、thinking 字段名等行为有特殊性，故单独成类',
        ],

        // ── 待实现：协议与 OpenAI 差异较大，需要独立的解码/编码实现 ──
        'azure' => [
            'label' => 'Azure OpenAI',
            'implemented' => false,
            'hint' => '鉴权用 api-key 头，地址需带 api-version 查询参数',
        ],
        'anthropic' => [
            'label' => 'Anthropic Messages',
            'implemented' => false,
            'hint' => '事件类型与字段名与 OpenAI 完全不同',
        ],
        'gemini' => [
            'label' => 'Google Gemini',
            'implemented' => false,
            'hint' => 'generateContent 协议，流式结构差异大',
        ],
        'vertex' => [
            'label' => 'Google Vertex AI',
            'implemented' => false,
            'hint' => 'GCP 服务账号鉴权',
        ],
        'aws' => [
            'label' => 'AWS Bedrock',
            'implemented' => false,
            'hint' => 'SigV4 签名鉴权',
        ],
        'baidu' => [
            'label' => '百度文心（access_token 流程）',
            'implemented' => false,
            'hint' => '需先用 API Key 换取 access_token',
        ],
        'xunfei' => [
            'label' => '讯飞星火（WebSocket）',
            'implemented' => false,
            'hint' => '非 HTTP 协议',
        ],
        'coze' => [
            'label' => '扣子 Coze（机器人 / 工作流）',
            'implemented' => false,
            'hint' => '以会话/工作流为中心，不是对话补全',
        ],
        'replicate' => [
            'label' => 'Replicate（异步任务）',
            'implemented' => false,
            'hint' => '提交任务 + 轮询结果',
        ],
        'chatgpt_sub' => [
            'label' => 'ChatGPT 订阅账号（会话凭证）',
            'implemented' => false,
            'hint' => '用订阅账号凭证而非 API Key，凭证会过期需换新',
        ],
        'ollama' => [
            'label' => 'Ollama 原生协议',
            'implemented' => false,
            'hint' => '非 OpenAI 格式；如需即刻可用请选它的 OpenAI 兼容层',
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
                (name, type, base_url, api_key_enc, models, priority, weight, status, rpm_limit, created_at, updated_at)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
            [
                (string) $data['name'],
                (string) $data['type'],
                (string) $data['base_url'],
                self::encryptKey((string) ($data['api_key'] ?? '')),
                self::encodeModels($data['models'] ?? []),
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
        $acquired = self::acquireKey($channel, Settings::int('nim.rpm_limit', 40));

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

        $baseUrl = rtrim((string) $channel['base_url'], '/');

        // ── 第一步：拉取模型清单，确认网络可达 ──
        $listResponse = self::request('GET', $baseUrl . '/models', $key);

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
        $chatResponse = self::request(
            'POST',
            $baseUrl . '/chat/completions',
            $key,
            (string) json_encode([
                'model' => $probeModel,
                'messages' => [['role' => 'user', 'content' => 'hi']],
                'max_tokens' => 1,
                'stream' => false,
            ], JSON_UNESCAPED_UNICODE)
        );

        $result = self::interpretProbe($chatResponse, $probeModel, $models);
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
        if ($configured !== []) {
            return $configured[0];
        }

        return $upstreamModels[0];
    }

    /**
     * 发起一次 HTTP 请求。
     *
     * ⚠️ 这里是**阻塞式 curl**。与本项目主线要求的非阻塞转发相反，
     * 但测活是站长手动点一次的操作，可以接受。
     * 若将来要做「定时批量探测」，必须改为非阻塞实现并放到独立进程，
     * 否则会把工作进程卡住（详见项目文档的待办）。
     *
     * @return array{code:int, body:string, error:string}
     */
    private static function request(string $method, string $url, string $bearer, ?string $jsonBody = null): array
    {
        $headers = [
            'Authorization: Bearer ' . $bearer,
            'Accept: application/json',
        ];

        $ch = curl_init($url);

        $options = [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_CUSTOMREQUEST => $method,
            // 测活是给人看的，宁可快速失败也不要让人干等
            CURLOPT_CONNECTTIMEOUT => 8,
            CURLOPT_TIMEOUT => 20,
            CURLOPT_SSL_VERIFYPEER => true,
        ];

        if ($jsonBody !== null) {
            $headers[] = 'Content-Type: application/json';
            $options[CURLOPT_POSTFIELDS] = $jsonBody;
        }

        $options[CURLOPT_HTTPHEADER] = $headers;

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
}
