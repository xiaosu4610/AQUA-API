<?php
/**
 * 转发引擎（非阻塞）
 *
 * ═══ 为什么不能用阻塞 curl ═══
 *
 * 本项目跑在 Workerman 的事件循环里，一个进程要同时伺候成百上千个连接。
 * 一旦用阻塞 curl 去等上游，这个进程在整条流结束前什么都干不了 ——
 * 真实并发上限就退化成「进程数」，与 PHP-FPM 毫无区别，
 * 而「流式 + 高并发」正是这个项目存在的理由。
 *
 * ═══ 用的是 curl_multi，不是 AsyncTcpConnection ═══
 *
 * Linux 下最正统的做法是把上游 socket 注册进事件循环（epoll 驱动），
 * 但 PHP 的 curl 扩展**没有暴露** CURLMOPT_SOCKETFUNCTION /
 * curl_multi_socket_action（本机 PHP 8.4 实测确认），所以那条路走不通。
 *
 * 退而求其次用 curl_multi + 短间隔定时器驱动：
 *   · curl_multi_exec 是非阻塞的，事件循环永远不会被它卡住；
 *   · 代价是「轮询」——有活跃转发时每隔 2ms 主动查一次（约 500 次/秒），
 *     没有任何转发时**定时器会停掉，CPU 占用为 0**；
 *   · 换来的是 libcurl 全权处理 TLS、chunked、gzip、重定向，
 *     不必手写 HTTP 客户端（手写这些才是真正容易出错的地方）。
 *
 * 这个取舍值得记一笔：2ms 的轮询延迟对 LLM 流式输出完全无感
 * （用户看到的是几十毫秒一个字），而手写 HTTP 客户端会带来
 * 一堆协议层 bug。若将来 CPU 成为瓶颈，升级路径是
 * 「独立抓取进程池 + 进程间流式回传」，届时引擎的对外接口不用改。
 *
 * ═══ 状态码为什么必须「等上游确认后才发」 ═══
 *
 * 上游返回 401/429/5xx 时，客户端应该收到**真实状态码**才能正确重试。
 * 而 HTTP 状态码一旦发出就改不了。所以这里刻意**先不写响应头**，
 * 等上游回了 2xx 才把 `200 + text/event-stream` 发出去；
 * 上游报错则作为正常 HTTP 错误响应返回（详见 NullResponse 的说明）。
 */

declare(strict_types=1);

namespace app\common;

use CurlHandle;
use CurlMultiHandle;
use support\Log;
use Throwable;
use Workerman\Connection\TcpConnection;
use Workerman\Protocols\Http\Chunk;
use Workerman\Protocols\Http\Response;
use Workerman\Timer;

final class RelayEngine
{
    /** 有数据流动时的轮询间隔（秒）。够小以保证流式体验，够大以不烧 CPU */
    private const TICK_ACTIVE = 0.002;

    /** 空闲时（等上游首字节）的轮询间隔 */
    private const TICK_IDLE = 0.02;

    /** 流式场景下允许在内存里积压的上限；超过说明客户端消费不过来 */
    private const MAX_STREAM_PENDING = 2097152;

    /** 非流式场景下允许缓存的整包上限（长回答可能很大） */
    private const MAX_BUFFER_PENDING = 16777216;

    /**
     * 客户端发送队列达到 maxSendBufferSize 的这个比例时，暂停向它灌数据。
     *
     * ⚠️ 这个值**必须明显小于 1**，否则背压就是摆设：
     * Workerman 的 TcpConnection::send() 在「发送队列已满
     * （sendBuffer ≥ maxSendBufferSize）」时会**直接返回 false 并丢掉这段数据**
     * —— 不是排队、不是报错，是静默丢弃。
     * 所以水位线一旦贴着 1.0，就会出现「我们以为还能发，其实数据已经被丢了」，
     * 客户端拿到一个被截断的流却看不出任何异常。
     *
     * 取 0.25：留出四倍余量，让「停止灌数据 → 队列自然排空」有足够空间，
     * 数据转而堆在我们自己的 pending 缓冲里（那里有明确的上限与报错）。
     */
    private const CLIENT_BACKPRESSURE = 0.25;

    private static ?CurlMultiHandle $multi = null;

    /** @var array<int, RelayJob> 任务序号 => 任务 */
    private static array $jobs = [];

    /** 同时活跃过的最大转发数（用于验证非阻塞确实生效） */
    private static int $peak = 0;

    private static ?int $timerId = null;

    private static float $timerInterval = 0.0;

    // ═══════════════════════════════════════════════════════════
    // 对外接口
    // ═══════════════════════════════════════════════════════════

    /**
     * 提交一个转发任务。
     *
     * 注意：调用后**立刻返回**，真正的转发在事件循环里推进。
     * 调用方（控制器）随后返回一个「什么都不发」的响应占位即可。
     */
    public static function submit(RelayJob $job): void
    {
        $job->id = self::nextJobId();
        self::$jobs[$job->id] = $job;

        // 记录并发峰值：这是「非阻塞是否真的生效」的唯一直接证据
        self::$peak = max(self::$peak, count(self::$jobs));

        $first = self::prepare($job, null, '首次请求');

        if ($first === null) {
            // 连一个可用渠道都没有，属于服务端配置问题。
            //
            // ⚠️ 优先用 $job->error：prepare() 在「构造请求规格时抛异常」的情况下
            //    会把原因写进去。原来这里一律回「无可用渠道」，
            //    等于把一个明确的程序错误说成「渠道配置问题」——
            //    实测踩过：一个配置解析 bug 让**所有**转发都回这句话，
            //    排查方向完全跑偏（去查渠道和密钥池，而真正的问题在别处）
            self::finishError(
                $job,
                503,
                $job->error !== ''
                    ? $job->error
                    : '当前没有可用的上游渠道（渠道被禁用、不支持该模型，或密钥池全部用尽）'
            );

            return;
        }

        $job->resetForAttempt($first['channel'], $first['key'], $first['spec']);
        self::attach($job);
    }

    /**
     * 事件循环的一拍。
     *
     * 用 public 是为了能被 Timer 直接回调（Timer 只接受可调用值）。
     */
    public static function tick(): void
    {
        if (self::$multi === null || self::$jobs === []) {
            self::stopTimer();

            return;
        }

        // 1) 推进所有传输。curl_multi_exec 不阻塞，返回 CURLM_CALL_MULTI_PERFORM
        //    表示还有活要立刻干，循环到它不返回该值为止
        do {
            $status = curl_multi_exec(self::$multi, $running);
        } while ($status === CURLM_CALL_MULTI_PERFORM);

        // 2) 取回已结束的传输
        while (($info = curl_multi_info_read(self::$multi)) !== false) {
            $easy = $info['handle'] ?? null;

            if (!$easy instanceof CurlHandle) {
                continue;
            }

            $job = self::jobOfHandle($easy);
            self::detachHandle($easy);

            if ($job === null) {
                continue;
            }

            if (($info['result'] ?? 1) === CURLE_OK) {
                self::onTransferDone($job);
            } else {
                self::onTransferFailed($job, (int) $info['result']);
            }
        }

        // 3) 维护每个还活着的任务
        $now = microtime(true);

        foreach (self::$jobs as $job) {
            if ($job->done || !isset(self::$jobs[$job->id])) {
                continue;
            }

            if (!self::clientAlive($job)) {
                self::abortForClientGone($job);

                continue;
            }

            self::maintain($job, $now);
        }

        // 4) 让轮询间隔随活跃度自适应：数据在流动时保持高频（保证流式体验），
        //    全体都在等上游首字节时降到低频（避免为「什么也没发生」空转 CPU）。
        //    没有任务时 stopTimer 已经把定时器彻底摘掉，CPU 占用为 0
        self::adaptInterval($now);
    }

    /**
     * 进程退出前清理（主要给测试与优雅重启用）。
     */
    public static function shutdown(): void
    {
        foreach (self::$jobs as $job) {
            if ($job->easy instanceof CurlHandle && self::$multi !== null) {
                @curl_multi_remove_handle(self::$multi, $job->easy);
            }
            $job->easy = null;
            $job->done = true;
        }

        self::$jobs = [];
        self::$multi = null;
        self::stopTimer();
    }

    /** 当前活跃的转发数（供健康检查/调试观察） */
    public static function activeCount(): int
    {
        return count(self::$jobs);
    }

    /**
     * 进程启动以来同时活跃的最大转发数。
     *
     * 用它判断「非阻塞到底有没有生效」最直接：如果这个数字超过了
     * 「一个进程只处理一条流」的上限（例如 8 个进程却出现 20），
     * 就说明引擎确实在单个进程里同时推进了多条流 ——
     * 换成阻塞式 curl 不可能出现这种数字。
     *
     * ⚠️ 常驻内存模型下这是**单进程**的数字，反代看到的总量需各进程相加。
     */
    public static function peakCount(): int
    {
        return self::$peak;
    }

    // ═══════════════════════════════════════════════════════════
    // 任务推进
    // ═══════════════════════════════════════════════════════════

    /**
     * 单个任务的状态机。
     */
    private static function maintain(RelayJob $job, float $now): void
    {
        // ── 已提交：只负责搬数据、保活、超时 ──
        if ($job->committed) {
            self::flush($job);

            if ($job->stream && $now - $job->lastHeartbeatAt >= $job->heartbeatInterval) {
                self::sendChunk($job, ": keep-alive\n\n");
                $job->lastHeartbeatAt = $now;
            }

            if ($job->firstByteAt > 0 && $now - $job->lastActivityAt > $job->idleTimeout) {
                self::finishError($job, 504, '上游持续 ' . $job->idleTimeout . ' 秒没有返回数据，已中断');
            }

            return;
        }

        // ── 未提交：等待上游状态码 ──
        //
        // ⚠️ 只有**流式**请求才在这里提前提交响应头。
        // 非流式必须等到整包收完再提交 —— 否则中途一旦出错，
        // 状态码已经发出去是 200 了，客户端会以为拿到了完整结果，
        // 实际上拿到的是半个 JSON。等收完再发，出错时还能如实返回 5xx。
        if ($job->httpStatus >= 200 && $job->httpStatus < 300) {
            if ($job->stream) {
                self::commit($job);
                self::flush($job);
            }

            return;
        }

        if ($job->httpStatus > 0) {
            // 上游明确回了错误状态
            $message = self::upstreamErrorMessage($job);

            if (in_array($job->httpStatus, $job->retryStatuses, true)
                && $job->canRetry()
                && self::retry($job, '上游返回 HTTP ' . $job->httpStatus)
            ) {
                return;
            }

            self::finishError($job, $job->httpStatus, $message);

            return;
        }

        // ── 还没收到响应头：检查「首字节超时」 ──
        if ($now - $job->attemptStartedAt > $job->ttftTimeout) {
            if ($job->canRetry() && self::retry($job, '上游 ' . $job->ttftTimeout . ' 秒未响应')) {
                return;
            }

            self::finishError($job, 504, '上游 ' . $job->ttftTimeout . ' 秒内没有响应（首字节超时）');
        }
    }

    /**
     * 传输成功结束。
     */
    private static function onTransferDone(RelayJob $job): void
    {
        if ($job->done) {
            return;
        }

        $job->scanner->finish();

        // 极快的响应可能在一个 tick 内就走完了，状态码还没判过
        if (!$job->committed) {
            if ($job->httpStatus >= 200 && $job->httpStatus < 300) {
                self::commit($job);
            } else {
                $message = self::upstreamErrorMessage($job);

                if (in_array($job->httpStatus, $job->retryStatuses, true)
                    && $job->canRetry()
                    && self::retry($job, '上游返回 HTTP ' . $job->httpStatus)
                ) {
                    return;
                }

                self::finishError($job, $job->httpStatus ?: 502, $message);

                return;
            }
        }

        self::flushAll($job);
        self::finishSuccess($job);
    }

    /**
     * 传输失败（连接层错误）。
     */
    private static function onTransferFailed(RelayJob $job, int $curlCode): void
    {
        if ($job->done) {
            return;
        }

        $reason = self::curlErrorText($curlCode, $job);

        // 因积压超限而主动中止的，换渠道没有意义（问题在客户端或响应体过大），
        // 直接如实报错，别浪费一次重试
        if ($job->aborted) {
            self::finishError($job, 502, $job->error !== '' ? $job->error : $reason);

            return;
        }

        // 连接层错误（超时、连不上、TLS 失败）换渠道通常能好，值得重试
        if ($job->canRetry() && self::retry($job, $reason)) {
            return;
        }

        self::finishError($job, 502, '无法连接上游或上游中断：' . $reason);
    }

    /**
     * 换一个渠道/密钥重试。
     *
     * @return bool 是否成功换到了新的尝试
     */
    private static function retry(RelayJob $job, string $reason): bool
    {
        $job->retries++;
        self::detach($job);

        $next = self::prepare($job, $job->channel, $reason);

        if ($next === null) {
            return false;
        }

        $job->resetForAttempt($next['channel'], $next['key'], $next['spec']);
        self::attach($job);

        return true;
    }

    /**
     * 向控制器要下一次尝试（选渠道、取密钥、拼请求）。
     *
     * @return array{channel: array, key: ?array, spec: array}|null
     */
    private static function prepare(RelayJob $job, ?array $failedChannel, string $reason): ?array
    {
        if ($job->prepare === null) {
            return null;
        }

        try {
            $result = ($job->prepare)($failedChannel, $reason);
        } catch (Throwable $e) {
            $job->error = '构造上游请求失败：' . $e->getMessage();

            return null;
        }

        return is_array($result) ? $result : null;
    }

    // ═══════════════════════════════════════════════════════════
    // curl 句柄
    // ═══════════════════════════════════════════════════════════

    /**
     * 创建 curl 句柄并挂到 multi 上。
     */
    private static function attach(RelayJob $job): void
    {
        $spec = $job->spec;

        $job->attemptStartedAt = microtime(true);
        $job->lastActivityAt = $job->attemptStartedAt;
        $job->lastHeartbeatAt = $job->attemptStartedAt;

        $easy = curl_init();

        $options = [
            CURLOPT_URL => $spec['url'],
            // 不用 RETURNTRANSFER：我们要的是「边到边处理」，而不是最后拿一整块
            CURLOPT_RETURNTRANSFER => false,
            CURLOPT_HEADER => false,
            CURLOPT_CONNECTTIMEOUT => max(1, (int) $spec['connect_timeout']),
            CURLOPT_TIMEOUT => max(1, (int) $spec['total_timeout']),
            // 空闲超时：连续 N 秒速率低于 1 字节/秒就断开。
            // 用速率而不是总时长，长回答才不会因为「总时长超了」被中途掐断
            CURLOPT_LOW_SPEED_LIMIT => 1,
            CURLOPT_LOW_SPEED_TIME => max(1, $job->idleTimeout),
            CURLOPT_SSL_VERIFYPEER => true,
            CURLOPT_FOLLOWLOCATION => false,
            // 让 libcurl 自动解压：上游开 gzip 时我们不关心压缩，只关心内容
            CURLOPT_ENCODING => '',
            CURLOPT_HTTPHEADER => $spec['headers'],
            CURLOPT_PRIVATE => (string) $job->id,
            CURLOPT_HEADERFUNCTION => static function ($ch, string $line) use ($job): int {
                self::onHeaderLine($job, $line);

                return strlen($line);
            },
            CURLOPT_WRITEFUNCTION => static function ($ch, string $data) use ($job): int {
                return self::onBodyData($job, $data);
            },
        ];

        if (($spec['body'] ?? null) !== null) {
            $options[CURLOPT_POST] = true;
            $options[CURLOPT_POSTFIELDS] = $spec['body'];
        }

        if (($spec['proxy'] ?? '') !== '') {
            $options[CURLOPT_PROXY] = $spec['proxy'];
        }

        curl_setopt_array($easy, $options);

        $job->easy = $easy;

        if (self::$multi === null) {
            self::$multi = curl_multi_init();
        }

        curl_multi_add_handle(self::$multi, $easy);
        self::ensureTimer();
    }

    /**
     * 处理一行响应头。
     *
     * 只需要状态码：其余头一律不转发给客户端（尤其是 Content-Length 与
     * Content-Encoding —— 一个会与 chunked 冲突，另一个已被 libcurl 解压）。
     */
    private static function onHeaderLine(RelayJob $job, string $line): void
    {
        if (str_starts_with($line, 'HTTP/')) {
            $parts = explode(' ', $line, 3);
            $job->httpStatus = (int) ($parts[1] ?? 0);
        }
    }

    /**
     * 收到一段响应体。
     *
     * 这里只做三件事：统计用量、累积待发数据、判断积压是否超限。
     * **不在这里写客户端** —— 因为此时还不知道上游状态码，
     * 万一上游报错就得换渠道重来，先写出去就撤不回来了。
     */
    private static function onBodyData(RelayJob $job, string $data): int
    {
        if ($job->done || $job->aborted) {
            // 返回不足长度会让 curl 以 CURLE_WRITE_ERROR 中止传输 —— 正是我们想要的
            return 0;
        }

        $job->scanner->feed($data);
        $job->lastActivityAt = microtime(true);

        if ($job->firstByteAt === 0.0) {
            $job->firstByteAt = $job->lastActivityAt;
        }

        $job->pending .= $data;

        $limit = $job->stream ? self::MAX_STREAM_PENDING : self::MAX_BUFFER_PENDING;

        if (strlen($job->pending) > $limit) {
            $job->error = '积压超过上限（' . round($limit / 1048576, 1) . 'MB），'
                . ($job->stream ? '客户端消费过慢或已停止读取' : '单次响应体过大');
            $job->aborted = true;

            return 0;
        }

        return strlen($data);
    }

    private static function detach(RelayJob $job): void
    {
        if ($job->easy instanceof CurlHandle) {
            self::detachHandle($job->easy);
        }
    }

    private static function detachHandle(CurlHandle $easy): void
    {
        if (self::$multi !== null) {
            @curl_multi_remove_handle(self::$multi, $easy);
        }
        // PHP 8 起 curl_close 已是空操作，句柄交给 GC 回收
    }

    private static function jobOfHandle(CurlHandle $easy): ?RelayJob
    {
        $id = (int) curl_getinfo($easy, CURLINFO_PRIVATE);

        return self::$jobs[$id] ?? null;
    }

    // ═══════════════════════════════════════════════════════════
    // 与客户端交互
    // ═══════════════════════════════════════════════════════════

    /**
     * 把响应头发给客户端 —— 这一步之后状态码不可再改。
     */
    private static function commit(RelayJob $job): void
    {
        $job->committed = true;

        $headers = $job->stream
            // ⚠️ 必须带 charset 参数：Workerman 的 Response 只对**精确等于**
            //    `text/event-stream` 的 Content-Type 走特殊分支，那个分支
            //    不会补上头部结束的空行，会导致响应头永远发不完。
            //    带参数后走正常的 chunked 分支，framing 才是对的（已实测确认）
            ? [
                'Content-Type' => 'text/event-stream; charset=utf-8',
                'Cache-Control' => 'no-cache, no-transform',
                // 告诉 Nginx 不要缓冲 —— 否则流式会被攒成一坨再吐出来
                'X-Accel-Buffering' => 'no',
            ]
            : [
                'Content-Type' => 'application/json; charset=utf-8',
                'Cache-Control' => 'no-store',
            ];

        // Transfer-Encoding: chunked + 空 body ⇒ 只发头，之后由我们逐个发分块。
        // 用 chunked 而不是「靠关闭连接表示结束」，是为了让 keep-alive 仍然有效
        $headers['Transfer-Encoding'] = 'chunked';

        self::rawSend($job, new Response(200, $headers, ''));
    }

    /**
     * 流式转发：把积压的数据发给客户端。
     *
     * ⚠️ 这是流式转发的关键：必须在「客户端消费得动」时才灌数据，
     * 否则慢客户端会把服务端内存吃光（连接发送队列 + 我们的 pending 缓冲）。
     * 判断依据是连接自身的发送队列长度。
     *
     * 非流式不走这里 —— 整包必须攒齐后一次发出（见 flushAll）。
     */
    private static function flush(RelayJob $job): void
    {
        if (!$job->stream || $job->pending === '') {
            return;
        }

        $client = $job->client;
        $queued = $client->getSendBufferQueueSize();
        $watermark = (int) ($client->maxSendBufferSize * self::CLIENT_BACKPRESSURE);

        // 客户端还堵着，先把数据留在 pending 里，下一个 tick 再试。
        // 不做这件事的话，慢客户端会让我们无上限地往发送队列里堆数据
        if ($queued > $watermark) {
            return;
        }

        $data = $job->pending;
        $job->pending = '';

        // 按水位线判断后仍被丢弃，说明水位线给得还是太松。
        // 这种情况必须留痕 —— 否则表现为「客户端偶发收到截断的流」，
        // 却没有任何线索指向原因
        if (!self::rawSend($job, new Chunk($data))) {
            Log::error(sprintf(
                '转发数据被连接队列丢弃：job=%d 渠道=%s 长度=%d。请调小 CLIENT_BACKPRESSURE',
                $job->id,
                (string) ($job->channel['name'] ?? ''),
                strlen($data)
            ));
        }
    }

    /**
     * 收尾前的最后一次发送：无论流式还是非流式，把剩余数据全部写出去。
     */
    private static function flushAll(RelayJob $job): void
    {
        if ($job->pending === '') {
            return;
        }

        $data = $job->pending;
        $job->pending = '';

        // 收尾这一次尤其不能丢：丢了客户端就拿到一个被截断的响应，
        // 而且它看起来「正常结束了」
        if (!self::rawSend($job, new Chunk($data))) {
            $job->error = '响应结尾数据未能写出（客户端发送队列已满或连接已断开）';
            Log::error(sprintf(
                '转发收尾数据被丢弃：job=%d 渠道=%s 长度=%d',
                $job->id,
                (string) ($job->channel['name'] ?? ''),
                strlen($data)
            ));
        }
    }

    /**
     * 成功收尾：计算用量、记账、结束响应。
     */
    private static function finishSuccess(RelayJob $job): void
    {
        $job->done = true;
        $job->latencyMs = $job->elapsedMs();

        // 非流式请求是在这里才提交响应头（见 maintain 里的说明）
        if (!$job->committed) {
            self::commit($job);
        }

        // 上游在流里/整包里回了 error：字节已经透传（客户端有权看到上游原话），
        // 但不能向用户收费 —— 用户没拿到服务
        $upstreamError = $job->scanner->streamError;

        self::account($job, $upstreamError === '' ? null : $upstreamError);

        self::flushAll($job);
        self::endResponse($job);
        self::cleanup($job);
    }

    /**
     * 失败收尾。
     *
     * 未提交时返回**真实的 HTTP 错误响应**（客户端据此重试或提示），
     * 已提交时只能中断流并留下一条注释行。
     */
    private static function finishError(RelayJob $job, int $status, string $message): void
    {
        if ($job->done) {
            return;
        }

        $job->done = true;
        $job->error = $message;
        $job->latencyMs = $job->elapsedMs();

        self::account($job, $message);

        if ($job->aborted) {
            // 客户端已经走了，什么都不用发
            self::cleanup($job);

            return;
        }

        if (!$job->committed) {
            $status = $status >= 400 ? $status : 502;
            $body = (string) json_encode([
                'error' => [
                    'message' => $message,
                    'type' => 'upstream_error',
                    'code' => $status,
                ],
            ], JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);

            self::rawSend($job, new Response($status, [
                'Content-Type' => 'application/json; charset=utf-8',
                'Cache-Control' => 'no-store',
            ], $body));

            self::closeClient($job);

            return;
        }

        // 已经发过 200 了，改不了状态码。按 OpenAI 的做法在流里补一个 error 事件 ——
        // 官方的流式实现在中途出错时就是这么做的，因此客户端 SDK 认得这种写法；
        // 只发注释行（: 开头）的话客户端会彻底看不见出错这件事
        if ($job->stream) {
            self::sendChunk($job, 'data: ' . json_encode([
                'error' => [
                    'message' => $message,
                    'type' => 'upstream_error',
                    'code' => $status,
                ],
            ], JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES) . "\n\n");
        }

        self::endResponse($job);
        self::cleanup($job);
    }

    /**
     * 客户端断线：上游请求没有意义了，直接放弃。
     */
    private static function abortForClientGone(RelayJob $job): void
    {
        $job->aborted = true;
        $job->done = true;
        $job->error = '客户端提前断开连接';
        $job->latencyMs = $job->elapsedMs();

        // 这种情况不算上游失败，不记账也不惩罚渠道/密钥：
        // 用户点了「停止生成」是完全正常的行为，不该影响上游健康度
        self::cleanup($job);
    }

    /**
     * 发送结束分块（chunked 的终止块）并关闭连接。
     *
     * 为什么要关闭：分块终止只是「响应体结束」，连接本可以继续复用；
     * 但我们已经绕过了框架自行写响应，交由框架继续处理后续请求容易出错。
     * SSE 客户端本来就期待连接结束，直接关掉最省心也最不容易出问题。
     */
    private static function endResponse(RelayJob $job): void
    {
        self::closeClient($job, new Chunk(''));
    }

    private static function closeClient(RelayJob $job, mixed $final = null): void
    {
        try {
            if ($final === null) {
                $job->client->close();

                return;
            }

            $job->client->close($final);
        } catch (Throwable) {
            // 连接可能已经没了，忽略
        }
    }

    /** 只发一个数据分块（不结束响应） */
    private static function sendChunk(RelayJob $job, string $data): void
    {
        if ($data === '') {
            return;
        }

        self::rawSend($job, new Chunk($data));
    }

    /**
     * 直接向连接写（绕过框架的响应流程）。
     *
     * @return bool 是否真的写进了发送队列。false 表示连接已关闭或队列已满
     *              —— 后者会**静默丢数据**，调用方必须检查这个返回值
     */
    private static function rawSend(RelayJob $job, mixed $payload): bool
    {
        try {
            return $job->client->send($payload) !== false;
        } catch (Throwable $e) {
            $job->error = '写入客户端失败：' . $e->getMessage();

            return false;
        }
    }

    private static function clientAlive(RelayJob $job): bool
    {
        try {
            $status = $job->client->getStatus();

            return $status === TcpConnection::STATUS_ESTABLISHED || $status === TcpConnection::STATUS_CLOSING;
        } catch (Throwable) {
            return false;
        }
    }

    // ═══════════════════════════════════════════════════════════
    // 记账
    // ═══════════════════════════════════════════════════════════

    /**
     * 结算这一次请求：算 token、算钱、扣余额、写日志。
     *
     * 只在最后调用一次（无论成功还是失败）。失败也要写日志 ——
     * 只记成功请求会让人误以为「这个渠道很稳」，而它可能一半请求都在失败。
     */
    private static function account(RelayJob $job, ?string $error): void
    {
        $scanner = $job->scanner;

        // 优先用上游给的真实用量；没有就按字符估算并标记
        if ($scanner->usageFound) {
            $job->promptTokens = $scanner->promptTokens;
            $job->completionTokens = $scanner->completionTokens;
            $job->cachedTokens = $scanner->cachedTokens;
            $job->cacheMissTokens = $scanner->cacheMissTokens;
            $job->usageEstimated = false;
        } else {
            $job->promptTokens = $job->promptEstimate;
            $job->completionTokens = Pricing::estimateFromCounts(
                $scanner->contentCjk,
                $scanner->contentOther,
                $job->estimateRatio
            );
            $job->usageEstimated = true;
        }

        // 计价口径三个来源：
        //   · $job->cachedTokens —— 缓存命中按独立单价算（命中价常是输入价的 1/10）
        //   · (int) $job->startedAt —— 按**本次请求发生时刻**落时段，用「谷时半价」那类价目
        //   · $job->freeToUser —— 所属分组对用户免费（临时免费的专线）：售价记 0、成本照记
        $quote = Pricing::quote(
            $job->pricing,
            $job->promptTokens,
            $job->completionTokens,
            $job->multiplier,
            $job->cachedTokens,
            (int) $job->startedAt,
            $job->freeToUser
        );

        // 失败或上游报错：记成本、不收费（用户没拿到服务）
        $downstream = $error === null ? $quote['downstream_cost'] : 0.0;

        try {
            UsageLog::record([
                'model' => $job->model,
                'channel_id' => (int) ($job->channel['id'] ?? 0),
                'channel_key_id' => (int) ($job->poolKey['key_id'] ?? 0),
                'channel_name' => (string) ($job->channel['name'] ?? ''),
                'user_id' => $job->user === null ? null : (int) $job->user['id'],
                'token_id' => $job->token === null ? null : (int) $job->token['id'],
                'prompt_tokens' => $job->promptTokens,
                'completion_tokens' => $job->completionTokens,
                'cached_tokens' => $job->cachedTokens,
                'usage_estimated' => $job->usageEstimated,
                'upstream_cost' => $quote['upstream_cost'],
                'downstream_cost' => $downstream,
                'billing_mode' => $quote['mode'],
                'is_stream' => $job->stream,
                'latency_ms' => $job->latencyMs,
                'status' => $error === null ? UsageLog::STATUS_OK : UsageLog::STATUS_ERROR,
                'error' => $error,
            ]);

            // 把这次的上游成本记到「这次用的那把密钥」的额度账上。
            // 必须按密钥而不是按渠道：额度是按密钥给的（一把 16 元、另一把 74 元），
            // 混在一起就算不出「哪一把快要没了」。
            // 失败的重试也算成本（上游确实处理了），所以不看 $error
            if ($job->poolKey !== null && isset($job->poolKey['key_id']) && $quote['upstream_cost'] > 0) {
                ChannelKey::addCost((int) $job->poolKey['key_id'], (float) $quote['upstream_cost']);
            }

            // 扣费与扣额度。两者都失败也不影响已完成的响应，
            // 但必须写日志 —— 少收的钱要能对得出来
            if ($downstream > 0) {
                if ($job->user !== null && !User::tryDebit((int) $job->user['id'], $downstream)) {
                    Log::warning(sprintf(
                        '扣费失败（余额不足）：user_id=%d, 金额=%s, 模型=%s',
                        $job->user['id'],
                        $downstream,
                        $job->model
                    ));
                }

                if ($job->token !== null && !UserToken::charge((int) $job->token['id'], $downstream)) {
                    Log::warning(sprintf(
                        '令牌额度扣减失败：token_id=%d, 金额=%s',
                        $job->token['id'],
                        $downstream
                    ));
                }
            } elseif ($job->token !== null) {
                // 金额为 0（模型未定价 / 免费 / 公益站）时不会走到上面的扣费分支，
                // 但「最后使用时间」仍要更新 —— 否则控制台里所有令牌都显示
                // 「从未使用」，站长根本看不出谁在用、哪些令牌是死的
                UserToken::touch((int) $job->token['id']);
            }
        } catch (Throwable $e) {
            // 记账问题绝不能影响已经完成的转发
            Log::error('转发记账失败：' . $e->getMessage());
        }

        // 成功时通知业务层回写健康状态（把失败连击清零）。
        // 放在记账之后：健康度是「次要信息」，不能让它的异常影响账目
        if ($error === null && $job->onSuccess !== null) {
            try {
                ($job->onSuccess)($job);
            } catch (Throwable $e) {
                Log::error('回写渠道健康状态失败：' . $e->getMessage());
            }
        }

        // 失败时通知业务层回写**模型**健康度（连续失败到阈值就暂时摘掉这个模型）。
        // 与上面的 onSuccess 严格对称：只在「最终失败」时触发一次，
        // 重试过程中的中间失败不进这里 —— 那是渠道/密钥维度的账
        if ($error !== null && $job->onFailure !== null) {
            try {
                ($job->onFailure)($job, (string) $error);
            } catch (Throwable $e) {
                Log::error('回写模型健康状态失败：' . $e->getMessage());
            }
        }
    }

    // ═══════════════════════════════════════════════════════════
    // 清理与定时器
    // ═══════════════════════════════════════════════════════════

    private static function cleanup(RelayJob $job): void
    {
        self::detach($job);

        // 断开闭包对 job 的引用，避免 curl 句柄 ↔ 闭包 ↔ job 形成环后迟迟不回收
        $job->easy = null;
        $job->prepare = null;
        $job->onSuccess = null;
        $job->onFailure = null;

        unset(self::$jobs[$job->id]);

        if (self::$jobs === []) {
            self::stopTimer();
        }
    }

    private static function ensureTimer(?float $interval = null): void
    {
        $interval ??= self::TICK_ACTIVE;

        if (self::$timerId !== null && abs(self::$timerInterval - $interval) < 0.0001) {
            return;
        }

        self::stopTimer();
        self::$timerInterval = $interval;
        self::$timerId = Timer::add($interval, [self::class, 'tick']);
    }

    /**
     * 按「最近是否有数据流动」切换轮询频率。
     */
    private static function adaptInterval(float $now): void
    {
        $busy = false;

        foreach (self::$jobs as $job) {
            if ($now - $job->lastActivityAt < 0.3) {
                $busy = true;
                break;
            }
        }

        self::ensureTimer($busy ? self::TICK_ACTIVE : self::TICK_IDLE);
    }

    private static function stopTimer(): void
    {
        if (self::$timerId !== null) {
            Timer::del(self::$timerId);
            self::$timerId = null;
        }

        self::$timerInterval = 0.0;
    }

    /** 任务序号：进程内自增即可，不需要跨进程唯一 */
    private static function nextJobId(): int
    {
        static $seq = 0;

        return ++$seq;
    }

    /**
     * 把 curl 的错误码翻成能看懂的原因。
     */
    private static function curlErrorText(int $code, RelayJob $job): string
    {
        return match ($code) {
            CURLE_OPERATION_TIMEDOUT => '上游超时',
            CURLE_COULDNT_CONNECT => '无法连接到上游（域名解析失败、端口不通或被拒绝）',
            CURLE_COULDNT_RESOLVE_HOST => '上游域名解析失败',
            CURLE_SSL_CONNECT_ERROR, CURLE_SSL_CERTPROBLEM => '上游 TLS 握手失败',
            CURLE_WRITE_ERROR => $job->error !== '' ? $job->error : '数据传输被中断',
            CURLE_GOT_NOTHING => '上游没有返回任何数据',
            CURLE_PARTIAL_FILE => '上游响应不完整',
            default => curl_strerror($code) ?: ('curl 错误码 ' . $code),
        };
    }

    /**
     * 从上游的错误响应体里提炼一句人话。
     */
    private static function upstreamErrorMessage(RelayJob $job): string
    {
        $body = trim($job->pending);

        if ($body !== '') {
            $decoded = json_decode($body, true);

            if (is_array($decoded)) {
                $message = $decoded['error']['message']
                    ?? ($decoded['error'] ?? ($decoded['message'] ?? ($decoded['detail'] ?? null)));

                if (is_string($message) && $message !== '') {
                    return '上游返回 HTTP ' . $job->httpStatus . '：' . mb_substr($message, 0, 300);
                }
            }
        }

        return '上游返回 HTTP ' . $job->httpStatus;
    }
}
