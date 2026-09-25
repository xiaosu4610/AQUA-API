<?php
/**
 * 邮箱真实性验证
 *
 * 目的：在**给用户发验证邮件之前**先判断这个地址是不是真实可用的邮箱。
 *
 * ═══ 为什么必须做这件事 ═══
 *
 * 不是「为了严谨」，而是为了发信信誉：
 * 邮件服务商（阿里云邮件推送、腾讯、SendGrid 都一样）会统计你的
 * **无效地址率**与**退信率**。这两个指标一旦升高，后果是整条发信通道
 * 被降级甚至封禁 —— 那时连真实用户都收不到验证邮件，
 * 而且恢复非常麻烦（要申诉、要养信誉）。
 *
 * 所以结论是：**宁可不发，也不要向可疑地址发**。
 *
 * ═══ 四级验证，逐级淘汰 ═══
 *
 *   ① 语法        —— 最便宜，先挡掉明显写错的
 *   ② 一次性邮箱  —— 临时邮箱域名黑名单（这类地址收得到信，但用户拿不到，
 *                    属于"有效但无用"，且会拉高投诉率）
 *   ③ 域名 MX     —— 该域名到底收不收信（拼错的域名、随便编的域名在此出局）
 *   ④ SMTP 探测   —— 连到对方的收信服务器，用 RCPT TO 直接问
 *                    「这个邮箱存在吗」。这是**唯一能确认单个地址存在**
 *                    的手段，也是本类最有价值的一步
 *
 * ═══ ★ 最重要的一条设计原则：只有「确定不存在」才拒绝 ★══
 *
 * SMTP 探测有大量无法判定的情况：对方灰名单（4xx）、限流、
 * catch-all（任何地址都收）、乃至**因为我们自己的 IP 信誉差而拒绝所有人**。
 * 这些情况下如果把用户挡在门外，就是「真用户注册不了」——
 * 比放进几个无效地址严重得多（一个是事故，一个是噪音）。
 *
 * 因此本类把结论分成三种：
 *   · INVALID —— 有确凿证据表明地址不存在，**拒绝**
 *   · OK      —— 对方明确接受，**放行**
 *   · UNKNOWN —— 判不了，**放行**（并记日志，便于站长观察探测成功率）
 *
 * ═══ ⚠️ 阻塞式 IO，且会连到第三方服务器 ═══
 *
 * 探测最长可能花十几秒（要连 1~2 台 MX）。注册是低频的人工操作，
 * 这个代价可以接受；但**绝不能**把它放到批量任务或转发路径上。
 * 另外：频繁对同一家 MX 做探测会被对方视为异常行为，
 * 所以这里限制「每次注册最多探测 2 台 MX、每台只问一次」。
 */

declare(strict_types=1);

namespace app\common;

use support\Log;
use Throwable;

final class EmailVerifier
{
    /** 判定结果 */
    public const OK = 'ok';
    public const INVALID = 'invalid';
    public const UNKNOWN = 'unknown';

    /** 连接与读取超时（秒）。探测是「顺带确认」，不该让用户干等太久 */
    private const CONNECT_TIMEOUT = 5;
    private const READ_TIMEOUT = 5;

    /** 每次最多尝试几台 MX。多试会增加被对方注意的概率，收益却很小 */
    private const MAX_MX_TRY = 2;

    /**
     * 内置的一次性 / 临时邮箱域名。
     *
     * 不求穷尽（这类域名每天都在新增），只覆盖最常见的一批 ——
     * 剩下的靠站长在配置里追加（`register.extra_blocked_domains`）。
     * 追求「完整列表」是不现实的，而且会让代码里塞进几千行数据。
     */
    private const DISPOSABLE_DOMAINS = [
        // 英文常见
        '10minutemail.com', '10minutemail.net', '20minutemail.com', 'guerrillamail.com',
        'guerrillamail.net', 'sharklasers.com', 'mailinator.com', 'mailinator.net',
        'tempmail.com', 'tempmail.net', 'temp-mail.org', 'tempr.email', 'throwaway.email',
        'throwawaymail.com', 'trashmail.com', 'trashmail.net', 'yopmail.com', 'yopmail.net',
        'maildrop.cc', 'mailnesia.com', 'mailcatch.com', 'fakeinbox.com', 'dispostable.com',
        'getnada.com', 'nada.email', 'mytemp.email', 'moakt.com', 'emailondeck.com',
        'spamgourmet.com', 'mintemail.com', 'tempinbox.com', 'discard.email', 'mailto.plus',
        '1secmail.com', '1secmail.net', '1secmail.org', 'eyabut.com', 'burnermail.io',
        'inboxkitten.com', 'mohmal.com', 'tempmailo.com', 'temporary-mail.net', 'luxusmail.org',
        'vomoto.com', 'armyspy.com', 'cuvox.de', 'dayrep.com', 'einrot.com', 'fleckens.hu',
        'gustr.com', 'jourrapide.com', 'rhyta.com', 'superrito.com', 'teleworm.us',
        // 中文圈常见
        'linshiyouxiang.net', '24mail.chacuo.net', 'snapmail.cc', 'mailtemp.info',
        'linshiyou.com', 'tmpmail.net', 'zwoho.com', 'akapost.com',
    ];

    /**
     * RCPT TO 的响应里若出现这些词，说明拒绝的原因是**对方在拒绝我们**
     * （IP 信誉、黑名单、策略），而不是「这个地址不存在」。
     * 这类响应必须当作「判不了」而不是「不存在」—— 见文件头说明。
     */
    private const OUR_FAULT_HINTS = [
        'blocked', 'blacklist', 'block list', 'spam', 'spamhaus', 'policy',
        'denied', 'deny', 'reputation', 'spf', 'dkim', 'dmarc', 'rbl',
        'not allowed to send', 'access denied', 'rejected due to',
    ];

    /**
     * 验证一个邮箱地址。
     *
     * @return array{result:string, stage:string, message:string, mx:array<int,string>}
     *         result 取 OK / INVALID / UNKNOWN；stage 说明卡在哪一级
     */
    public static function verify(string $email): array
    {
        $email = strtolower(trim($email));

        // ── ① 语法 ──
        if (!self::checkSyntax($email)) {
            return self::fail(self::INVALID, '语法', '邮箱格式不正确');
        }

        [$local, $domain] = explode('@', $email, 2);

        // ── ② 一次性邮箱 ──
        if (Settings::bool('register.block_disposable', true) && self::isDisposable($domain)) {
            return self::fail(
                self::INVALID,
                '一次性邮箱',
                '本站不接受临时/一次性邮箱，请使用常用邮箱注册'
            );
        }

        // ── ③ 域名 MX ──
        $mxHosts = self::mxHosts($domain);

        if ($mxHosts === false) {
            // DNS 查询本身不可用（少见的运行环境问题），不能再往下判，放行
            return self::fail(self::UNKNOWN, '域名', '无法查询该邮箱域名的收信配置（DNS 不可用），已放行');
        }

        if ($mxHosts === []) {
            return self::fail(
                self::INVALID,
                '域名',
                '该邮箱域名没有配置收信服务器（MX 记录），无法接收任何邮件 —— 请检查邮箱是否写错'
            );
        }

        // ── ④ SMTP 收件人探测 ──
        if (!Settings::bool('register.verify_smtp', true)) {
            return self::pass('域名', '域名收信配置正常（未开启收件人探测）', $mxHosts);
        }

        return self::probeRecipient($email, $local, $mxHosts);
    }

    // ═══════════════════════════════════════════════════════════
    // 各级检查
    // ═══════════════════════════════════════════════════════════

    /**
     * 语法检查。
     *
     * 在 filter_var 之外额外要求：
     *   · 长度不超过 191（数据库字段上限，早拒绝比后报错清楚）
     *   · 域名必须含点且顶级域至少 2 位
     *   · 不含连续的点（`a..b@x.com` 是无效的）
     *
     * filter_var 自己已经相当严格（比大多数手写正则可靠），
     * 这里只补它不覆盖的几条。
     */
    private static function checkSyntax(string $email): bool
    {
        if (mb_strlen($email) > 191) {
            return false;
        }

        if (filter_var($email, FILTER_VALIDATE_EMAIL) === false) {
            return false;
        }

        if (!str_contains($email, '@')) {
            return false;
        }

        [, $domain] = explode('@', $email, 2);

        if (!str_contains($domain, '.') || str_contains($domain, '..')) {
            return false;
        }

        $tld = substr((string) strrchr($domain, '.'), 1);

        return strlen($tld) >= 2;
    }

    /**
     * 是否为一次性邮箱域名（含子域名匹配）。
     *
     * 用「后缀匹配」而不是精确相等：`mail.tempmail.com` 这类子域也是同一家。
     * 匹配时要求以点分界，避免 `nottempmail.com` 被误判。
     */
    private static function isDisposable(string $domain): bool
    {
        $blocked = self::DISPOSABLE_DOMAINS;

        // 站长追加的域名
        $extra = trim((string) Settings::get('register.extra_blocked_domains', ''));
        if ($extra !== '') {
            foreach (preg_split('/[\s,;]+/', $extra) ?: [] as $item) {
                $item = strtolower(trim($item));
                if ($item !== '') {
                    $blocked[] = $item;
                }
            }
        }

        foreach ($blocked as $item) {
            if ($domain === $item || str_ends_with($domain, '.' . $item)) {
                return true;
            }
        }

        return false;
    }

    /**
     * 查询域名的 MX 记录（按优先级从低到高返回域名列表）。
     *
     * @return array<int, string>|false  false 表示 DNS 查询不可用（与「没有 MX」不同）
     */
    private static function mxHosts(string $domain): array|false
    {
        if (!function_exists('dns_get_record')) {
            return false;
        }

        $records = @dns_get_record($domain, DNS_MX);

        if ($records === false) {
            // dns_get_record 失败有两种可能：DNS 不可用，或域名确实不存在。
            // 用一次 A 记录查询区分：A 记录能查到说明域名存在、只是没有 MX
            $a = @dns_get_record($domain, DNS_A);

            if ($a === false) {
                return false;
            }

            // 域名存在但没有 MX：按 RFC，此时回退到 A 记录（少量站点确实这么收信）
            if ($a !== []) {
                $hosts = [];
                foreach ($a as $row) {
                    if (!empty($row['ip'])) {
                        $hosts[] = $domain;
                        break;
                    }
                }

                return $hosts;
            }

            return [];
        }

        if ($records === []) {
            return [];
        }

        usort($records, static fn ($a, $b) => ($a['pri'] ?? 0) <=> ($b['pri'] ?? 0));

        $hosts = [];
        foreach ($records as $record) {
            $target = trim((string) ($record['target'] ?? ''));
            if ($target !== '') {
                $hosts[] = $target;
            }
        }

        return $hosts;
    }

    /**
     * SMTP 收件人探测：连到对方收信服务器，用 RCPT TO 问「这个邮箱存在吗」。
     *
     * @param array<int, string> $mxHosts
     * @return array{result:string, stage:string, message:string, mx:array<int,string>}
     */
    private static function probeRecipient(string $email, string $local, array $mxHosts): array
    {
        if (!self::mailFromAddress()) {
            // 没有可用的发件地址就无法完成 SMTP 对话（MAIL FROM 是必须的）
            return self::pass('收件人', '尚未配置发件地址，跳过收件人探测', $mxHosts);
        }

        $tried = 0;
        $lastReason = '对方邮件服务器无响应';

        foreach ($mxHosts as $host) {
            if ($tried >= self::MAX_MX_TRY) {
                break;
            }
            $tried++;

            $probe = self::smtpProbe($host, $email);

            if ($probe['code'] === 0) {
                $lastReason = $probe['text'] !== '' ? $probe['text'] : '无法连接对方邮件服务器';
                continue;
            }

            $verdict = self::classifyRcpt($probe['code'], $probe['text']);

            if ($verdict === self::OK) {
                return self::pass('收件人', '该邮箱真实可用', $mxHosts);
            }

            if ($verdict === self::INVALID) {
                return self::fail(
                    self::INVALID,
                    '收件人',
                    '该邮箱不存在（对方邮件服务器明确回复：' . $probe['text'] . '）'
                );
            }

            // UNKNOWN：记下来，换个 MX 再试一次，都判不了就放行
            $lastReason = '对方未明确答复（' . $probe['code'] . ' ' . $probe['text'] . '）';
        }

        return self::pass('收件人', '无法确认该邮箱是否存在（' . $lastReason . '），已放行', $mxHosts);
    }

    /**
     * 与一台 MX 完成一次最小 SMTP 对话，返回 RCPT TO 的响应码与文本。
     *
     * 由于我们只问「收不收这个地址」，**不发送任何邮件内容**，
     * 因此不会打扰收件人，也不会消耗发信额度。
     *
     * @return array{code:int, text:string} code 为 0 表示连接或对话失败
     */
    private static function smtpProbe(string $host, string $email): array
    {
        $from = self::mailFromAddress();

        $socket = @fsockopen($host, 25, $errno, $errstr, self::CONNECT_TIMEOUT);
        if ($socket === false) {
            return ['code' => 0, 'text' => "连接 {$host}:25 失败（{$errstr}）"];
        }

        stream_set_timeout($socket, self::READ_TIMEOUT);

        try {
            $greeting = self::readResponse($socket);
            if ($greeting === null || !str_starts_with($greeting, '2')) {
                return ['code' => 0, 'text' => '对方未正常问候'];
            }

            // EHLO 用本机主机名：部分服务器会因 HELO 缺失或非法而直接拒绝
            self::write($socket, 'EHLO ' . self::localHostname());
            $ehlo = self::readResponse($socket);
            if ($ehlo === null || !str_starts_with($ehlo, '2')) {
                // 退回老式 HELO
                self::write($socket, 'HELO ' . self::localHostname());
                $helo = self::readResponse($socket);
                if ($helo === null || !str_starts_with($helo, '2')) {
                    return ['code' => 0, 'text' => '对方拒绝了 EHLO/HELO'];
                }
            }

            self::write($socket, 'MAIL FROM:<' . $from . '>');
            $mail = self::readResponse($socket);
            if ($mail === null || !str_starts_with($mail, '2')) {
                // 连发件人都被拒 —— 这是「我们的问题」，绝不能算作收件人不存在
                return ['code' => 0, 'text' => '对方拒绝了我们的发件人地址（可能本机 IP 信誉不足）'];
            }

            self::write($socket, 'RCPT TO:<' . $email . '>');
            $rcpt = self::readResponse($socket);

            @fwrite($socket, "QUIT\r\n");

            if ($rcpt === null) {
                return ['code' => 0, 'text' => '对方对 RCPT TO 没有响应（超时）'];
            }

            return [
                'code' => (int) substr($rcpt, 0, 3),
                'text' => trim(substr($rcpt, 4)),
            ];
        } catch (Throwable $e) {
            return ['code' => 0, 'text' => '探测过程异常：' . $e->getMessage()];
        } finally {
            if (is_resource($socket)) {
                @fclose($socket);
            }
        }
    }

    /**
     * 把 RCPT TO 的响应码翻译成判定结果。
     *
     * 这是整个验证里最需要谨慎的一处，逐条说明：
     *
     *   250 / 251   对方接收 → OK
     *   550         邮箱不存在（最常见的「没有这个用户」）→ INVALID，
     *               但**先看文本**：若是「因为你在黑名单/被策略拒绝」，
     *               那是我们的问题，改判 UNKNOWN
     *   553         邮箱名不被允许 → INVALID（通常是地址本身有问题）
     *   551 / 552   用户不在本地 / 邮箱已满 → UNKNOWN（地址可能有效）
     *   5xx 其它    策略性拒绝，含义模糊 → UNKNOWN
     *   4xx         临时拒绝（灰名单、限流）→ UNKNOWN，**绝不能判为不存在**
     *   0           连接层失败 → UNKNOWN
     */
    private static function classifyRcpt(int $code, string $text): string
    {
        $lower = strtolower($text);

        foreach (self::OUR_FAULT_HINTS as $hint) {
            if (str_contains($lower, $hint)) {
                return self::UNKNOWN;
            }
        }

        return match (true) {
            $code === 250, $code === 251 => self::OK,
            $code === 550, $code === 553 => self::INVALID,
            default => self::UNKNOWN,
        };
    }

    // ═══════════════════════════════════════════════════════════
    // 小工具
    // ═══════════════════════════════════════════════════════════

    /**
     * 探测时使用的发件地址。
     *
     * 优先用站长配置的发信地址（同一个域名，对方最容易接受）；
     * 没配就用站点域名拼一个 postmaster —— 虽然可能被拒，
     * 但被拒时我们会判为 UNKNOWN 而不是误杀用户。
     */
    private static function mailFromAddress(): string
    {
        $from = trim((string) Settings::get('mail.from', ''));

        if ($from !== '') {
            return $from;
        }

        $domain = trim((string) Settings::get('site.domain', ''));
        $domain = preg_replace('#^https?://#i', '', $domain) ?? $domain;
        $domain = rtrim(explode('/', $domain)[0] ?? '', '/');

        return $domain === '' ? '' : 'postmaster@' . $domain;
    }

    private static function localHostname(): string
    {
        $name = gethostname();

        return is_string($name) && $name !== '' ? $name : 'localhost';
    }

    private static function write($socket, string $line): void
    {
        @fwrite($socket, $line . "\r\n");
    }

    /**
     * 读取一条（可能多行的）SMTP 响应。
     *
     * 多行响应的判断方式：除最后一行外，第 4 个字符是 `-`。
     * 只读一行会把剩余内容留到下次读取，导致后续对话全部错位 ——
     * 手写 SMTP 最常见的坑（Mailer 里也有同样的处理）。
     */
    private static function readResponse($socket): ?string
    {
        $response = '';

        while (true) {
            $line = fgets($socket, 1024);

            if ($line === false) {
                return $response === '' ? null : trim($response);
            }

            $response .= $line;

            if (strlen($line) < 4 || $line[3] !== '-') {
                break;
            }
        }

        return trim($response);
    }

    /** @param array<int, string> $mx */
    private static function pass(string $stage, string $message, array $mx): array
    {
        return ['result' => self::OK, 'stage' => $stage, 'message' => $message, 'mx' => $mx];
    }

    private static function fail(string $result, string $stage, string $message): array
    {
        // 判不了的情况记一条日志：站长据此能看出「探测到底有没有在起作用」
        if ($result === self::UNKNOWN) {
            Log::info("邮箱验证未能判定（{$stage}）：{$message}");
        }

        return ['result' => $result, 'stage' => $stage, 'message' => $message, 'mx' => []];
    }
}
