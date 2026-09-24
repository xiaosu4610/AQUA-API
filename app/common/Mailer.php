<?php
/**
 * 邮件发送（SMTP，自实现，零依赖）
 *
 * ═══ 为什么自己写而不用 PHPMailer / SwiftMailer ═══
 *
 * 本项目只需要「发一封简单的 HTML 邮件」这一件事：验证邮箱、重置密码、通知。
 * 引入一个邮件库会带来三样东西：额外的依赖、额外的升级与安全面、
 * 以及一个「为了兼容它的 API 而绕来绕去」的封装层。
 * 而 SMTP 的核心交互只有十来条命令，自己写反而更可控。
 *
 * ═══ ⚠️ 这是阻塞式 IO，会占住工作进程 ═══
 *
 * fsockopen + 同步读写会阻塞整个事件循环。之所以可以接受：
 * 注册验证、找回密码都是**低频、由人手动触发**的操作，
 * 一次几百毫秒到一两秒，影响面极小。
 * 若将来要做「批量发送通知」，必须改成异步（投递到队列 + 独立进程），
 * 否则会把转发请求一起拖慢 —— 这条写在这里，避免以后忘记。
 */

declare(strict_types=1);

namespace app\common;

use RuntimeException;
use Throwable;
use support\Log;

final class Mailer
{
    /** 与 SMTP 服务器交互的超时（秒）。设小一点，宁可失败也不要吊死进程 */
    private const TIMEOUT = 12;

    /**
     * 读取邮件配置。
     *
     * 口令走 Settings::secret()（加密存储），其余走普通配置。
     *
     * @return array{host:string, port:int, secure:string, username:string, password:string, from:string, from_name:string}
     */
    public static function config(): array
    {
        return [
            'host' => trim((string) Settings::get('mail.host', '')),
            'port' => Settings::int('mail.port', 465),
            'secure' => (string) Settings::get('mail.secure', 'ssl'),
            'username' => trim((string) Settings::get('mail.username', '')),
            'password' => Settings::secret('mail.password'),
            'from' => trim((string) Settings::get('mail.from', '')),
            // 发件人显示名默认用站点名，收件人看到的是「aqua-api-php <no-reply@x>」
            'from_name' => trim((string) Settings::get('mail.from_name', '')) ?: Settings::siteName(),
        ];
    }

    /**
     * 邮件功能是否已配置齐全（不影响「开关」判断，开关由调用方看 mail.enabled）。
     */
    public static function isConfigured(): bool
    {
        $c = self::config();

        return $c['host'] !== '' && $c['from'] !== '';
    }

    /**
     * 是否允许发信 = 开关打开 且 配置齐全。
     */
    public static function enabled(): bool
    {
        return Settings::bool('mail.enabled', false) && self::isConfigured();
    }

    /**
     * 发送一封 HTML 邮件。
     *
     * @return array{ok:bool, error:string}
     */
    public static function send(string $to, string $subject, string $html): array
    {
        if (!self::isValidAddress($to)) {
            return ['ok' => false, 'error' => '收件人邮箱格式不正确'];
        }

        $c = self::config();

        if ($c['host'] === '' || $c['from'] === '') {
            return ['ok' => false, 'error' => '邮件服务尚未配置（缺少 SMTP 服务器或发件人地址）'];
        }

        try {
            self::deliver($c, $to, $subject, $html);

            return ['ok' => true, 'error' => ''];
        } catch (Throwable $e) {
            // 邮件发不出去不能变成致命错误：注册流程要照常完成，
            // 用户之后可以点「重发验证邮件」。这里记日志并如实返回原因
            Log::error('邮件发送失败：' . $e->getMessage());

            return ['ok' => false, 'error' => $e->getMessage()];
        }
    }

    /**
     * 渲染一封带品牌样式的通知邮件。
     *
     * 邮件里的内联样式（而不是 class）是刻意的：
     * 各家邮箱客户端对外部/内嵌 CSS 的支持差异极大，
     * 内联样式是唯一通用的做法。
     *
     * @param array<int, string> $lines 正文段落（纯文本，会自动转义）
     */
    public static function template(
        string $title,
        array $lines,
        string $buttonText = '',
        string $buttonUrl = ''
    ): string {
        $siteName = htmlspecialchars(Settings::siteName(), ENT_QUOTES, 'UTF-8');

        $body = '';
        foreach ($lines as $line) {
            $body .= '<p style="margin:0 0 14px;font-size:14px;line-height:1.8;color:#3c4a58">'
                . nl2br(htmlspecialchars($line, ENT_QUOTES, 'UTF-8')) . '</p>';
        }

        $button = '';
        if ($buttonText !== '' && $buttonUrl !== '') {
            $button = '<p style="margin:22px 0 6px">'
                . '<a href="' . htmlspecialchars($buttonUrl, ENT_QUOTES, 'UTF-8') . '" '
                . 'style="display:inline-block;padding:11px 22px;background:#0d9dbf;color:#ffffff;'
                . 'border-radius:8px;text-decoration:none;font-size:14px;font-weight:600">'
                . htmlspecialchars($buttonText, ENT_QUOTES, 'UTF-8') . '</a></p>'
                . '<p style="margin:0 0 14px;font-size:12px;color:#8b9aa8">'
                . '按钮点不动时，把下面这个地址复制到浏览器：<br>'
                . '<span style="word-break:break-all">' . htmlspecialchars($buttonUrl, ENT_QUOTES, 'UTF-8') . '</span></p>';
        }

        return '<!DOCTYPE html><html><body style="margin:0;padding:24px;background:#f5f7fa">'
            . '<div style="max-width:520px;margin:0 auto;background:#ffffff;border:1px solid #e3e8ef;'
            . 'border-radius:14px;padding:28px 26px;font-family:system-ui,-apple-system,\'Segoe UI\','
            . '\'Microsoft YaHei\',sans-serif">'
            . '<div style="font-size:12px;color:#8b9aa8;margin-bottom:6px">' . $siteName . '</div>'
            . '<h1 style="margin:0 0 18px;font-size:18px;color:#1b2733">'
            . htmlspecialchars($title, ENT_QUOTES, 'UTF-8') . '</h1>'
            . $body . $button
            . '<hr style="border:0;border-top:1px solid #e3e8ef;margin:22px 0 12px">'
            . '<div style="font-size:12px;color:#8b9aa8">'
            . '这封邮件由系统自动发出，请勿直接回复。</div>'
            . '</div></body></html>';
    }

    // ═══════════════════════════════════════════════════════════
    // SMTP 交互
    // ═══════════════════════════════════════════════════════════

    /**
     * 与 SMTP 服务器完成一次完整的投递。
     *
     * @param array<string, mixed> $c
     * @throws RuntimeException 任何一步不符合预期都抛出，由 send() 统一兜住
     */
    private static function deliver(array $c, string $to, string $subject, string $html): void
    {
        $host = (string) $c['host'];
        $port = (int) $c['port'];
        $secure = strtolower((string) $c['secure']);

        // SSL（通常 465 端口）在连接建立时就加密；
        // STARTTLS（通常 587）是先明文握手再升级
        $transport = $secure === 'ssl' ? 'ssl://' : '';
        $socket = @fsockopen($transport . $host, $port, $errno, $errstr, self::TIMEOUT);

        if ($socket === false) {
            throw new RuntimeException("无法连接 SMTP 服务器 {$host}:{$port}（{$errstr}）");
        }

        stream_set_timeout($socket, self::TIMEOUT);

        try {
            self::expect($socket, 220, '服务器未正常问候');

            // EHLO 用域名而不是 IP：部分服务器会因此拒绝
            self::command($socket, 'EHLO ' . self::hostname(), 250, 'EHLO 被拒绝');

            if ($secure === 'tls' || $secure === 'starttls') {
                self::command($socket, 'STARTTLS', 220, 'STARTTLS 被拒绝');
                if (!stream_socket_enable_crypto($socket, true, STREAM_CRYPTO_METHOD_TLS_CLIENT)) {
                    throw new RuntimeException('STARTTLS 加密协商失败');
                }
                // 加密通道建立后必须重新 EHLO，这是 RFC 要求
                self::command($socket, 'EHLO ' . self::hostname(), 250, '加密后 EHLO 被拒绝');
            }

            // 需要认证就认证（有些内网中继不需要）
            if ((string) $c['username'] !== '') {
                self::command($socket, 'AUTH LOGIN', 334, '服务器不支持 AUTH LOGIN');
                self::command($socket, base64_encode((string) $c['username']), 334, '用户名被拒绝');
                self::command($socket, base64_encode((string) $c['password']), 235, '邮箱账号或授权码不正确');
            }

            $from = (string) $c['from'];
            self::command($socket, 'MAIL FROM:<' . $from . '>', 250, '发件人被拒绝');
            self::command($socket, 'RCPT TO:<' . $to . '>', [250, 251], '收件人被拒绝');
            self::command($socket, 'DATA', 354, 'DATA 被拒绝');

            fwrite($socket, self::buildMessage($c, $to, $subject, $html) . "\r\n.\r\n");
            self::expect($socket, 250, '邮件内容被拒绝');

            // QUIT 失败无所谓，但礼节上还是要发
            @fwrite($socket, "QUIT\r\n");
        } finally {
            @fclose($socket);
        }
    }

    /**
     * 组装 MIME 邮件。
     *
     * 两个容易踩的点：
     *   1. **主题必须做 MIME 编码**。中文主题直接写进头部会出现乱码，
     *      正确写法是 `=?UTF-8?B?<base64>?=`。
     *   2. **正文用 base64 编码**并每 76 字符折行。正文里的长行
     *      （HTML 常常一行几百字符）会超出 SMTP 的 1000 字符上限被截断。
     *
     * @param array<string, mixed> $c
     */
    private static function buildMessage(array $c, string $to, string $subject, string $html): string
    {
        $fromName = (string) $c['from_name'];
        $encodedName = '=?UTF-8?B?' . base64_encode($fromName) . '?=';
        $encodedSubject = '=?UTF-8?B?' . base64_encode($subject) . '?=';

        $headers = [
            'Date: ' . date('r'),
            'From: ' . $encodedName . ' <' . $c['from'] . '>',
            'To: <' . $to . '>',
            'Subject: ' . $encodedSubject,
            // Message-ID 里带随机串，避免被部分服务器按重复邮件丢弃
            'Message-ID: <' . bin2hex(random_bytes(12)) . '@' . self::hostname() . '>',
            'MIME-Version: 1.0',
            'Content-Type: text/html; charset=UTF-8',
            'Content-Transfer-Encoding: base64',
        ];

        $body = chunk_split(base64_encode($html), 76, "\r\n");

        return implode("\r\n", $headers) . "\r\n\r\n" . $body;
    }

    /**
     * 发一条命令并校验响应码。
     *
     * @param int|array<int, int> $expect 期望的响应码（可多个）
     */
    private static function command($socket, string $command, int|array $expect, string $failHint): void
    {
        fwrite($socket, $command . "\r\n");
        self::expect($socket, $expect, $failHint);
    }

    /**
     * 读取（可能多行的）SMTP 响应并校验响应码。
     *
     * SMTP 的多行响应格式：除最后一行外，每行的第 4 个字符是 `-`。
     * 只读一行会把后续内容留到下一次读取，导致后面全部错位 ——
     * 这是手写 SMTP 最常见的坑。
     *
     * @param int|array<int, int> $expect
     */
    private static function expect($socket, int|array $expect, string $failHint): void
    {
        $codes = (array) $expect;
        $response = '';

        while (true) {
            $line = fgets($socket, 1024);

            if ($line === false) {
                throw new RuntimeException($failHint . '：服务器没有响应（连接可能被中断或超时）');
            }

            $response .= $line;

            // 第 4 个字符不是 '-' 就表示这是最后一行
            if (strlen($line) < 4 || $line[3] !== '-') {
                break;
            }
        }

        $code = (int) substr($response, 0, 3);

        if (!in_array($code, $codes, true)) {
            // 响应里可能带有服务器信息，截断后一并给出，方便定位
            throw new RuntimeException($failHint . '：服务器返回 ' . trim($response));
        }
    }

    /**
     * 本机主机名（用于 EHLO）。
     * 拿不到就用一个合规的占位名，不能用 IP。
     */
    private static function hostname(): string
    {
        $name = gethostname();

        return is_string($name) && $name !== '' ? $name : 'localhost';
    }

    private static function isValidAddress(string $email): bool
    {
        return filter_var($email, FILTER_VALIDATE_EMAIL) !== false;
    }
}
