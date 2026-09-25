<?php
/**
 * SSE / JSON 用量扫描器
 *
 * 转发是**原样透传**的（字节进字节出，不重新编码），因为网关不该去改动
 * 上游的响应体 —— 一旦改动就可能破坏客户端依赖的字段。
 * 但我们又必须知道「这次用了多少 token」才能计费，所以旁路扫一遍。
 *
 * ═══ 为什么只统计「长度」而不是把文本留下 ═══
 *
 * 长回答可能有几百 KB。把它们全留在内存里，几百条并发流就是几百 MB ——
 * 而我们要的只是「输出有多少 token」。所以这里只累加字符数，
 * 用完即弃，内存占用是常数级。
 *
 * ═══ 关于 usage 的两条现实 ═══
 *
 * 1. 上游**可能不返回** usage（三方中转、各种反代很常见）。
 *    这时按字符数估算（Pricing::estimateTokens），并标记为估算值。
 * 2. usage 通常在**最后一个** data 事件里（OpenAI 需要 stream_options
 *    显式开启，NIM 之类的实现则默认带）。所以扫描必须读完整条流，
 *    不能在拿到第一个 usage 后就停下。
 */

declare(strict_types=1);

namespace app\common;

final class SseScanner
{
    /** 未处理完的残留字节（可能是半个事件） */
    private string $buffer = '';

    /** 非流式：整包 JSON 都攒在这里 */
    private string $body = '';

    /** 上游返回的用量（优先于估算） */
    public int $promptTokens = 0;
    public int $completionTokens = 0;
    public bool $usageFound = false;

    /** 输出内容的字符数（估算 token 用）。拆成中日韩与其它两类，
     *  因为两者的 token 密度差 4 倍 —— 只记一个总字符数会让中文回答被严重低估 */
    public int $contentCjk = 0;
    public int $contentOther = 0;

    /** 上游在流里回了错误（少见，但有些实现会这么做） */
    public string $streamError = '';

    public function __construct(private readonly bool $stream) {}

    /**
     * 喂入一段原始字节。
     */
    public function feed(string $bytes): void
    {
        if ($bytes === '') {
            return;
        }

        if (!$this->stream) {
            $this->body .= $bytes;

            return;
        }

        $this->buffer .= $bytes;

        // SSE 事件之间用空行分隔。按 "\n\n" 切分即可覆盖 \n\n 与 \r\n\r\n
        // （\r\n\r\n 里也含有 \n\n，切出来的片段多一个 \r 不影响解析）
        while (($pos = strpos($this->buffer, "\n\n")) !== false) {
            $event = substr($this->buffer, 0, $pos);
            $this->buffer = substr($this->buffer, $pos + 2);
            $this->handleEvent($event);
        }
    }

    /**
     * 处理一个完整的 SSE 事件（可能含多行 data）。
     */
    private function handleEvent(string $event): void
    {
        $payloads = [];

        foreach (explode("\n", $event) as $line) {
            $line = rtrim($line, "\r");

            // 注释行（: 开头）与空行忽略 —— 心跳就是注释行
            if ($line === '' || $line[0] === ':') {
                continue;
            }

            // 只关心 data 字段；event/id/retry 对计费没有意义
            if (str_starts_with($line, 'data:')) {
                // 规范允许 "data:值" 与 "data: 值" 两种写法
                $payloads[] = ltrim(substr($line, 5), ' ');
            }
        }

        if ($payloads === []) {
            return;
        }

        // 多行 data 按规范用 \n 连接成一个负载
        $payload = implode("\n", $payloads);

        if ($payload === '[DONE]') {
            return;
        }

        $decoded = json_decode($payload, true);
        if (!is_array($decoded)) {
            return;
        }

        // 上游把错误塞在流里（HTTP 200 但内容报错）—— 记下来，交给引擎判断
        if (isset($decoded['error'])) {
            $this->streamError = is_string($decoded['error'])
                ? $decoded['error']
                : (string) json_encode($decoded['error'], JSON_UNESCAPED_UNICODE);
        }

        $this->absorbUsage($decoded);
        $this->absorbContent($decoded);
    }

    /**
     * 收尾：非流式在这一步解析整包 JSON。
     */
    public function finish(): void
    {
        if ($this->stream || $this->body === '') {
            return;
        }

        $decoded = json_decode($this->body, true);
        if (!is_array($decoded)) {
            return;
        }

        if (isset($decoded['error'])) {
            $this->streamError = is_string($decoded['error'])
                ? $decoded['error']
                : (string) json_encode($decoded['error'], JSON_UNESCAPED_UNICODE);
        }

        $this->absorbUsage($decoded);
        $this->absorbContent($decoded);
    }

    /**
     * 提取 usage。
     *
     * 兼容三种字段名：OpenAI 的 prompt_tokens/completion_tokens、
     * Anthropic 风格的 input_tokens/output_tokens、以及只给总数的 total_tokens。
     * 各家实现差异很大，能多认一种就少一次估算。
     *
     * @param array<string, mixed> $decoded
     */
    private function absorbUsage(array $decoded): void
    {
        $usage = $decoded['usage'] ?? null;

        if (!is_array($usage)) {
            return;
        }

        $prompt = $usage['prompt_tokens'] ?? ($usage['input_tokens'] ?? null);
        $completion = $usage['completion_tokens'] ?? ($usage['output_tokens'] ?? null);

        // 只给 total 的情况：按 0/总数 记，总比什么都没有强
        if ($prompt === null && $completion === null && isset($usage['total_tokens'])) {
            $prompt = 0;
            $completion = (int) $usage['total_tokens'];
        }

        if (!is_numeric($prompt) && !is_numeric($completion)) {
            return;
        }

        // 取**最后一个**出现的 usage：流式实现常常在中间事件里回传增量统计，
        // 最后那个才是本轮的最终值
        if (is_numeric($prompt)) {
            $this->promptTokens = (int) $prompt;
        }
        if (is_numeric($completion)) {
            $this->completionTokens = (int) $completion;
        }

        $this->usageFound = true;
    }

    /**
     * 累加输出内容的字符数（仅用于「上游没给 usage」时估算）。
     *
     * @param array<string, mixed> $decoded
     */
    private function absorbContent(array $decoded): void
    {
        $choices = $decoded['choices'] ?? null;

        if (!is_array($choices)) {
            return;
        }

        foreach ($choices as $choice) {
            if (!is_array($choice)) {
                continue;
            }

            // 流式是 delta.content，非流式是 message.content
            $text = $choice['delta']['content'] ?? ($choice['message']['content'] ?? ($choice['text'] ?? null));

            if (is_string($text) && $text !== '') {
                $this->addContent($text);
            }
        }
    }

    /**
     * 累加一段输出内容的字符构成。
     */
    private function addContent(string $text): void
    {
        $cjk = (int) preg_match_all('/[\x{4e00}-\x{9fff}\x{3040}-\x{30ff}\x{ac00}-\x{d7af}]/u', $text);
        $length = mb_strlen($text, 'UTF-8');

        $this->contentCjk += $cjk;
        $this->contentOther += max(0, $length - $cjk);
    }
}
