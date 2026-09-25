<?php
/**
 * 一次转发任务的全部状态
 *
 * 为什么是一个「装满公开属性的对象」而不是一堆数组键：
 * 转发过程中会频繁读写十几个状态字段（是否已提交、积压了多少、
 * 上游状态码、首次响应时间……），数组键写错只会静默产生 null，
 * 而对象属性写错会直接报错。在一条对正确性要求很高的路径上，
 * 让错误尽早暴露比写起来省事更重要。
 */

declare(strict_types=1);

namespace app\common;

use Closure;
use CurlHandle;
use Workerman\Connection\TcpConnection;

final class RelayJob
{
    /** 任务序号（同时用作 curl 句柄的自定义标识） */
    public int $id = 0;

    public ?CurlHandle $easy = null;

    /** 下游客户端连接 */
    public TcpConnection $client;

    /** 本次尝试使用的渠道与密钥 */
    public array $channel = [];
    public ?array $poolKey = null;
    public array $spec = [];

    /**
     * 构造下一次尝试的回调（由控制器提供）。
     *
     * 签名：function(?array $channel): ?array{channel: array, key: ?array, spec: array}
     * 返回 null 表示没有更多可用渠道/密钥。
     *
     * 为什么要回调而不是把逻辑写进引擎：引擎只该懂「怎么把字节搬来搬去」，
     * 「选哪个渠道、怎么拼请求」属于业务。分开之后，
     * 换渠道重试这件事对引擎来说只是「再要一份 spec」。
     */
    public ?Closure $prepare = null;

    /** 还没试过的候选渠道（按优先级 + 权重排好序） */
    public array $candidates = [];

    /**
     * 成功后回调（由控制器提供，用于回写渠道与密钥的健康状态）。
     *
     * 与 prepare 同样属于「业务概念」：引擎不该知道什么是渠道健康度，
     * 只负责在成功时通知一声。
     */
    public ?Closure $onSuccess = null;

    /**
     * 最终失败回调（由控制器提供，用于回写**模型级**健康度）。
     *
     * 与 onSuccess 对称，但有两点必须分清，否则会把健康度记错：
     *
     *   · 它只在「这次请求彻底失败」时触发一次 —— **不是**每次换渠道都触发。
     *     一次调用可能换 3 条渠道、4 把 Key 才失败；若每次都记，
     *    一个模型失败一次会被记 4 次，阈值 3 立刻凑满 ——
     *     那不是「连续失败 3 次」，而是「失败 1 次」。
     *   · 它记的是**模型**维度，而 prepare 回调里的 recordFailure 记的是
     *     **渠道/密钥**维度。粒度不同，两者都要有：
     *     渠道坏了影响「所有模型」，模型坏了只影响「这一个模型」。
     */
    public ?Closure $onFailure = null;

    /** 请求参数 */
    public bool $stream = false;
    public string $model = '';
    public string $upstreamModel = '';
    public ?array $token = null;
    public ?array $user = null;
    public ?array $pricing = null;
    public float $multiplier = 1.0;
    public float $estimateRatio = 1.0;

    /** 输入 token 的估算值（请求体里没有真实用量，只能按请求文本估） */
    public int $promptEstimate = 0;

    /** 时间与超时（秒；用 microtime 保证精度） */
    public float $startedAt = 0.0;
    public float $attemptStartedAt = 0.0;
    public float $firstByteAt = 0.0;
    public float $lastActivityAt = 0.0;
    public float $lastHeartbeatAt = 0.0;
    public int $ttftTimeout = 300;
    public int $idleTimeout = 60;
    public int $heartbeatInterval = 15;

    /** 上游响应状态 */
    public int $httpStatus = 0;

    /** 待发给客户端的字节 */
    public string $pending = '';

    /** 用量扫描器 */
    public SseScanner $scanner;

    /** 是否已把响应头发给客户端（发出后状态码与头就不可改了） */
    public bool $committed = false;

    /** 已重试次数 */
    public int $retries = 0;
    public int $maxRetries = 2;

    /** 可重试的上游状态码 */
    public array $retryStatuses = [429, 500, 502, 503, 504];

    /** 结束状态 */
    public bool $done = false;
    public bool $aborted = false;
    public string $error = '';

    /** 最终统计（收尾时填充，供记账用） */
    public int $promptTokens = 0;
    public int $completionTokens = 0;
    public bool $usageEstimated = false;

    /** 输入里命中缓存的部分（token 数），按独立单价计价 */
    public int $cachedTokens = 0;

    /** 输入里未命中缓存的部分（上游没给时留 0，计价侧用 输入−命中 兜底） */
    public int $cacheMissTokens = 0;

    /**
     * 这个模型的所属分组对用户免费（售价按 0 计，成本照记）。
     *
     * 由控制器在选好模型后按分组口径填 —— 放在这里而不是引擎里去查分组，
     * 是为了让引擎保持「只搬字节、不算业务」；同时计费口径在一处决定，好核对。
     */
    public bool $freeToUser = false;

    /** 最后一次尝试的耗时（毫秒），写入用量日志 */
    public int $latencyMs = 0;

    public function __construct()
    {
        $this->scanner = new SseScanner(false);
        $this->lastActivityAt = $this->startedAt = microtime(true);
        $this->lastHeartbeatAt = $this->startedAt;
    }

    /** 重置为「准备发起一次新尝试」的状态（换渠道重试时调用） */
    public function resetForAttempt(array $channel, ?array $poolKey, array $spec): void
    {
        $this->channel = $channel;
        $this->poolKey = $poolKey;
        $this->spec = $spec;
        $this->httpStatus = 0;
        $this->pending = '';
        $this->scanner = new SseScanner($this->stream);
        $this->firstByteAt = 0.0;
        $this->lastActivityAt = microtime(true);
        $this->lastHeartbeatAt = $this->lastActivityAt;
    }

    /** 是否已经确定要把这次尝试的结果丢弃（用于换渠道重试） */
    public function canRetry(): bool
    {
        // 只要还没向客户端输出过任何字节，就还能换渠道重来
        return !$this->committed && $this->retries < $this->maxRetries;
    }

    public function elapsedMs(): int
    {
        return (int) round((microtime(true) - $this->startedAt) * 1000);
    }
}
