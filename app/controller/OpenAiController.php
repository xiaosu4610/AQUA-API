<?php
/**
 * 对外的 OpenAI 兼容接口
 *
 * 路由：
 *   GET  /v1/models              模型清单
 *   POST /v1/chat/completions    对话补全（流式 / 非流式）
 *
 * ═══ 这一层只做三件事 ═══
 *
 *   1. **鉴权与准入**：令牌是否有效、账号是否可用、这个模型收不收费且余额够不够、
 *      模型是否在白名单内；
 *   2. **选路**：按模型挑渠道、从密钥池取 Key、构造上游请求；
 *   3. **交给引擎**：把「搬字节」这件事交给 RelayEngine，本控制器随即返回。
 *
 * 真正的转发是非阻塞的（见 RelayEngine 的说明），所以这里**不会**等上游返回。
 * 一旦进了引擎，客户端连接的读写就全部由引擎掌管，控制器不再插手。
 *
 * ═══ 为什么请求体要重新编码 ═══
 *
 * 我们可能需要在转发前改动请求体（模型名映射、剔除上游不认的字段、
 * 合并站长配置的附加参数）。既然要改，就只能先解码再编码。
 * 代价是失去「字节级原样透传」，但保留了「语义级原样」——
 * 客户端发来的所有未知字段都会原封不动地带过去，
 * 因此上游新加的参数不需要我们跟着改代码。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Channel;
use app\common\ChannelKey;
use app\common\NullResponse;
use app\common\Pricing;
use app\common\RelayEngine;
use app\common\RelayJob;
use app\common\Settings;
use app\common\User;
use app\common\UserToken;
use support\Request;
use support\Response;

class OpenAiController
{
    // ═══════════════════════════════════════════════════════════
    // GET /v1/models
    // ═══════════════════════════════════════════════════════════

    /**
     * 模型清单。
     *
     * 返回的是「这个令牌实际能用到的模型」：所有启用渠道的模型清单取并集，
     * 再按令牌的模型白名单过滤。这样客户端拿到的列表是**可以直接用的**，
     * 而不是一份「理论上存在但你没权限」的名单。
     */
    public function models(Request $request): Response
    {
        $auth = $this->authenticate($request);

        if (!$auth['ok']) {
            return $auth['response'];
        }

        $models = [];

        foreach (Channel::all() as $channel) {
            if ((int) $channel['status'] !== Channel::STATUS_ENABLED) {
                continue;
            }

            foreach (Channel::modelsOf($channel) as $model) {
                $model = trim((string) $model);

                if ($model === '' || isset($models[$model])) {
                    continue;
                }

                if (!UserToken::allowsModel($auth['token'], $model)) {
                    continue;
                }

                $models[$model] = true;
            }
        }

        $siteName = Settings::siteName();
        $list = [];

        // 顺带带上「有没有配价格」这个信息：客户端用不到，
        // 但站长用 curl 看一眼就知道哪些模型还没定价
        foreach (array_keys($models) as $model) {
            $pricing = Pricing::findByModel((string) $model);

            $list[] = [
                'id' => $model,
                'object' => 'model',
                'created' => time(),
                'owned_by' => $siteName,
                'priced' => $pricing !== null,
            ];
        }

        return json([
            'object' => 'list',
            'data' => $list,
        ]);
    }

    // ═══════════════════════════════════════════════════════════
    // POST /v1/chat/completions
    // ═══════════════════════════════════════════════════════════

    /**
     * 对话补全。
     */
    public function chatCompletions(Request $request): Response
    {
        // ── 1. 鉴权 ──
        $auth = $this->authenticate($request);

        if (!$auth['ok']) {
            return $auth['response'];
        }

        /** @var array<string, mixed> $user */
        $user = $auth['user'];
        /** @var array<string, mixed> $token */
        $token = $auth['token'];

        // ── 2. 解析请求体 ──
        $raw = $request->rawBody();
        $body = json_decode($raw, true);

        if (!is_array($body)) {
            return $this->error(400, '请求体不是合法的 JSON');
        }

        $model = trim((string) ($body['model'] ?? ''));

        if ($model === '') {
            return $this->error(400, '缺少 model 参数');
        }

        if (!UserToken::allowsModel($token, $model)) {
            return $this->error(
                403,
                "令牌「{$token['name']}」无权使用模型 {$model}",
                'invalid_request_error',
                'model_not_allowed'
            );
        }

        // stream 只认真正的布尔值：客户端传字符串 "false" 时不能当成真
        $stream = ($body['stream'] ?? false) === true;

        // ── 3. 定价 ──
        $pricing = Pricing::findByModel($model);

        if ($pricing === null && !Settings::bool('billing.unpriced_is_free', true)) {
            return $this->error(
                400,
                "模型 {$model} 尚未定价，本站当前不允许调用未定价的模型",
                'invalid_request_error',
                'model_not_priced'
            );
        }

        // ── 3.1 余额门槛（只拦收费模型）──
        //
        // 为什么放在这里而不是鉴权里：这里才知道「这次调用收不收费」。
        // 免费模型（billing_mode=free 或未定价按免费处理）即使余额为 0 也放行 ——
        // 用 0 元的调用去卡余额没有意义，而生产上正是这样把所有人卡住的：
        // 上游全是免费模型，用户余额全是 0，于是「人人 401 / 余额不足」。
        if (Settings::bool('billing.require_balance', true)
            && Pricing::isChargeable($pricing, Settings::bool('billing.unpriced_is_free', true))
            && (float) $user['balance'] <= 0) {
            return $this->error(
                402,
                '余额不足：当前余额 ' . User::money((float) $user['balance'])
                . "。模型 {$model} 是收费的，充值后即可调用；本站的免费模型不受余额限制",
                'insufficient_quota',
                'insufficient_balance'
            );
        }

        // ── 4. 选路：先看有没有可用渠道，避免白白建一个任务 ──
        $candidates = Channel::candidates($model);

        if ($candidates === []) {
            return $this->error(
                503,
                "当前没有可用的上游渠道可以服务模型 {$model}（渠道被禁用、处于熔断、或模型不在其清单内）",
                'server_error',
                'no_available_channel'
            );
        }

        // ── 5. 组装任务 ──
        $job = new RelayJob();
        $job->client = $request->connection;
        $job->stream = $stream;
        $job->model = $model;
        $job->token = $token;
        $job->user = $user;
        $job->pricing = $pricing;
        $job->multiplier = Settings::float('billing.default_multiplier', 1.0);
        $job->estimateRatio = Settings::float('billing.estimate_ratio', 1.0);
        $job->candidates = $candidates;
        $job->maxRetries = max(0, Settings::int('gateway.max_retries', 2));
        $job->ttftTimeout = max(1, Settings::int('gateway.ttft_timeout', 30));
        $job->idleTimeout = max(1, Settings::int('gateway.idle_timeout', 60));
        $job->heartbeatInterval = max(1, Settings::int('gateway.heartbeat_interval', 15));

        $retryStatuses = array_filter(array_map(
            'intval',
            preg_split('/[\s,]+/', (string) Settings::get('gateway.retry_status', '401,403,429,500,502,503,504')) ?: []
        ));
        $job->retryStatuses = $retryStatuses === [] ? [401, 403, 429, 500, 502, 503, 504] : array_values($retryStatuses);

        // 输入 token 的估算值：请求里没有真实用量，先按请求文本估一个，
        // 万一上游不回 usage，至少还有个数（会标记为估算）
        $job->promptEstimate = Pricing::estimateTokens(
            $this->messagesText($body),
            $job->estimateRatio
        );

        $controller = $this;

        $job->prepare = static function (?array $failedChannel, string $reason) use ($job, $body, $model, $controller): ?array {
            return $controller->planNextAttempt($job, $failedChannel, $reason, $body, $model);
        };

        // 成功后回写健康状态：把渠道的失败连击清零、把密钥的失败计数清零。
        // 这件事交给回调而不是写进引擎，是因为引擎只该懂「搬字节」，
        // 健康度属于业务概念
        $job->onSuccess = static function (RelayJob $job): void {
            Channel::markChannelSuccess($job->channel);

            if ($job->poolKey !== null && isset($job->poolKey['key_id'])) {
                ChannelKey::markSuccess((int) $job->poolKey['key_id']);
            }
        };

        // ── 6. 交给引擎；本方法随即返回 ──
        RelayEngine::submit($job);

        // 返回「什么都不发」的占位：真正的响应头由引擎在确认上游 2xx 之后才发，
        // 这样上游报错时客户端能收到真实状态码（详见 NullResponse 的说明）
        return new NullResponse();
    }

    // ═══════════════════════════════════════════════════════════
    // 选路（由引擎按需回调）
    // ═══════════════════════════════════════════════════════════

    /**
     * 计划下一次尝试：回写上一次的健康状态，然后挑下一条渠道并取密钥。
     *
     * 引擎在每次「首次请求」与「换渠道重试」时都会调到这里，
     * 所以这里是**唯一**需要处理渠道/密钥健康状态的地方。
     *
     * @param array<string, mixed>|null $failedChannel
     * @param array<string, mixed> $body
     * @return array{channel: array, key: ?array, spec: array}|null
     */
    public function planNextAttempt(
        RelayJob $job,
        ?array $failedChannel,
        string $reason,
        array $body,
        string $model
    ): ?array {
        if ($failedChannel !== null) {
            $this->recordFailure($job, $failedChannel, $reason);
        }

        $defaultRpm = Settings::int('key_pool.default_rpm', 40);
        $totalTimeout = max(1, Settings::int('gateway.total_timeout', 600));

        while ($job->candidates !== []) {
            $channel = array_shift($job->candidates);

            $acquired = Channel::acquireKey($channel, $defaultRpm);

            if ($acquired === null) {
                // 这条渠道的密钥池空了或全部停用。这**不算渠道故障** ——
                // 渠道本身是好的，只是这把（这些）Key 现在不能用，
                // 所以不惩罚它，直接换下一条
                continue;
            }

            // 模型名映射：把对外的统一名字翻成上游要求的名字
            $upstreamModel = Channel::upstreamModel($channel, $model);
            $body['model'] = $upstreamModel;

            $spec = Channel::buildSpec($channel, $acquired['key'], '/chat/completions', $body, $totalTimeout);

            $job->upstreamModel = $upstreamModel;
            $job->poolKey = $acquired['from_pool'] ? ['key_id' => $acquired['key_id']] : null;

            return ['channel' => $channel, 'key' => $acquired, 'spec' => $spec];
        }

        return null;
    }

    /**
     * 回写失败：渠道级与密钥级分别处理。
     *
     * 两者的判据刻意不同（这是很容易写错的地方）：
     *   · **密钥级**：只有 401/403 才说明「这把 Key 失效了」，永久停用；
     *     429/5xx 只是上游现在忙，标记为瞬时失败（进冷却，到期自动恢复）。
     *   · **渠道级**：只有「这条线路根本不通」才计入熔断（见下）。
     */
    private function recordFailure(RelayJob $job, array $failedChannel, string $reason): void
    {
        $status = $job->httpStatus;
        $message = $job->error !== '' ? $job->error : $reason;

        if ($this->isRouteLevelFailure($job, $status)) {
            Channel::markChannelFailure($failedChannel, $status, $message);
        }

        // 密钥级
        if ($job->poolKey !== null && isset($job->poolKey['key_id'])) {
            $permanent = in_array($status, [401, 403], true);

            ChannelKey::markFailure((int) $job->poolKey['key_id'], $message, $permanent);
        }
    }

    /**
     * 这次失败是否说明「渠道（线路）本身有问题」。
     *
     * 这个判断很关键，因为它决定要不要累计到渠道熔断。算错方向的两个后果：
     *
     *   · **算得太宽**（把 404、400 也算进去）：连续请求几个上游不存在的模型，
     *     就能把一条完全健康的渠道熔断掉。这个坑是上线后实测踩到的 ——
     *     NIM 的 /models 清单里有大量「列出来但账号无权调用」的模型，
     *     逐個试过去正好凑满熔断阈值，随后正常请求也开始报「无可用渠道」。
     *   · **算得太窄**（连超时都不算）：上游整体挂了也不会被熔断，
     *     每个请求都要先撞一次墙才轮换，白白浪费用户的等待时间。
     *
     * 结论：只有「换一条线路就不一样」的失败才算 ——
     * 连接层错误、5xx、429（这条线路现在被限流）、408（上游超时）。
     * 而 4xx 里的其余情况（404 模型不存在、400 参数不对、422 等）
     * 是**这次请求**的问题，不是线路的问题。
     */
    private function isRouteLevelFailure(RelayJob $job, int $status): bool
    {
        if ($job->error !== '') {
            return true;
        }

        return $status >= 500 || in_array($status, [408, 429], true);
    }

    /**
     * 把 messages 里的文本拼起来（用于估算输入 token）。
     *
     * content 可能是字符串，也可能是多模态数组（[{"type":"text","text":"..."}]），
     * 两种都要能处理。
     *
     * @param array<string, mixed> $body
     */
    private function messagesText(array $body): string
    {
        $messages = $body['messages'] ?? null;

        if (!is_array($messages)) {
            return '';
        }

        $text = '';

        foreach ($messages as $message) {
            if (!is_array($message)) {
                continue;
            }

            $content = $message['content'] ?? '';

            if (is_string($content)) {
                $text .= $content . "\n";
                continue;
            }

            if (is_array($content)) {
                foreach ($content as $part) {
                    if (is_array($part) && isset($part['text']) && is_string($part['text'])) {
                        $text .= $part['text'] . "\n";
                    }
                }
            }
        }

        return $text;
    }

    // ═══════════════════════════════════════════════════════════
    // 鉴权与错误响应
    // ═══════════════════════════════════════════════════════════

    /**
     * 校验调用令牌。
     *
     * ⚠️ 失败的**状态码不能一律用 401**：鉴权失败有很多种，
     * 用户要采取的动作完全不同 ——
     *   · 401 → 去检查/重新复制令牌
     *   · 402 → 去充值
     *   · 403 → 去换令牌 / 找站长解封
     * 全回 401（并把 code 写成 invalid_api_key）会让「余额不足」看起来
     * 像「密钥错了」，用户就会一直卡在验证密钥上。状态码由
     * UserToken::authorize() 一并给出，这里只负责透传。
     *
     * @return array{ok:bool, response:Response|null, token:array<string,mixed>|null, user:array<string,mixed>|null}
     */
    private function authenticate(Request $request): array
    {
        $plain = $this->bearerToken($request);

        if ($plain === '') {
            return [
                'ok' => false,
                'response' => $this->error(
                    401,
                    '缺少 API Key。请在请求头里带上 Authorization: Bearer <令牌>',
                    'invalid_request_error',
                    'missing_api_key'
                ),
                'token' => null,
                'user' => null,
            ];
        }

        $result = UserToken::authorize($plain);

        if (!$result['ok']) {
            return [
                'ok' => false,
                'response' => $this->error(
                    (int) ($result['status'] ?? 401),
                    $result['reason'],
                    (string) ($result['type'] ?? 'invalid_request_error'),
                    (string) ($result['code'] ?? 'invalid_api_key')
                ),
                'token' => null,
                'user' => null,
            ];
        }

        return ['ok' => true, 'response' => null, 'token' => $result['token'], 'user' => $result['user']];
    }

    /**
     * 从请求头取令牌。
     *
     * 同时接受 `Authorization: Bearer xxx` 与 `x-api-key: xxx`：
     * 前者是 OpenAI 的规范，后者是 Anthropic / 部分客户端的习惯，
     * 两种都认能省掉用户一堆「为什么我的 Key 用不了」的困惑。
     */
    private function bearerToken(Request $request): string
    {
        $header = trim((string) $request->header('authorization', ''));

        if ($header !== '') {
            if (preg_match('/^Bearer\s+(.+)$/i', $header, $matches) === 1) {
                return trim($matches[1]);
            }

            // 有些客户端不加 Bearer 前缀，直接把令牌丢进 Authorization
            if (str_starts_with($header, 'sk-')) {
                return $header;
            }
        }

        return trim((string) $request->header('x-api-key', ''));
    }

    /**
     * OpenAI 风格的错误响应。
     *
     * 格式必须与官方一致：客户端 SDK 是按这个结构解析错误信息的，
     * 换成自己的格式会让用户看到一堆「未知错误」。
     */
    private function error(
        int $status,
        string $message,
        string $type = 'invalid_request_error',
        string $code = ''
    ): Response {
        $body = (string) json_encode([
            'error' => [
                'message' => $message,
                'type' => $type,
                'param' => null,
                'code' => $code === '' ? null : $code,
            ],
        ], JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);

        // 不能用 json() 助手：它固定返回 200，而错误响应的状态码
        // 恰恰是客户端判断「该重试还是该报错」的依据
        return response($body, $status, ['Content-Type' => 'application/json; charset=utf-8']);
    }
}
