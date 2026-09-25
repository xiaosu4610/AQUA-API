<?php
/**
 * 下游用户 · 注册 / 登录 / 邮箱验证 / 找回密码
 *
 * 路由：
 *   GET  /login      POST /login      登录
 *   GET  /register   POST /register   注册
 *   GET  /verify                      邮箱验证（点邮件里的链接）
 *   GET  /forgot     POST /forgot     申请重置密码
 *   GET  /reset      POST /reset      设置新密码
 *   POST /logout                      退出
 *
 * ═══ 会话与后台完全隔离 ═══
 *
 * 用户写 session 的键是 `user_id`，后台管理员写的是 `admin_logged_in`。
 * 两者互不影响：管理员登录不会让他在前台变成某个用户，
 * 用户登录也不会获得任何后台权限。共用一套 session 存储但用不同的键，
 * 是「同一浏览器同时开前后台」这种真实场景下必须处理的。
 *
 * ═══ 邮件发不出去时的兜底 ═══
 *
 * 「要求邮箱验证」但「邮件服务没配」= 所有人都注册不进来 ——
 * 这是一个会把站点直接搞死的组合。因此这里做了兜底：
 * 需要验证但邮件发不出去时，**直接放行并如实告知**，
 * 同时把原因记进日志。宁可少一道验证，也不要让人卡死。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Csrf;
use app\common\EmailVerifier;
use app\common\Mailer;
use app\common\Settings;
use app\common\Url;
use app\common\User;
use app\common\UserToken;
use support\Log;
use support\Request;
use support\Response;
use Throwable;

class AuthController
{
    /** 用户登录态在 Session 中的键 */
    public const SESSION_KEY = 'user_id';

    /** 密码最短长度。与管理员保持一致 */
    private const MIN_PASSWORD_LENGTH = 8;

    private const FLASH_NOTICE = 'user_notice';
    private const FLASH_TYPE = 'user_notice_type';

    // ═══════════════════════════════════════════════════════════
    // 登录 / 退出
    // ═══════════════════════════════════════════════════════════

    /** GET /login */
    public function loginPage(Request $request): Response
    {
        if (self::currentUserId() > 0) {
            return redirect('/console');
        }

        return $this->view('login', [
            'email' => '',
            'error' => '',
            'registerOpen' => Settings::bool('register.open', false),
        ]);
    }

    /** POST /login */
    public function login(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->view('login', [
                'email' => '',
                'error' => '页面已过期，请重新提交',
                'registerOpen' => Settings::bool('register.open', false),
            ]);
        }

        $email = (string) $request->post('email', '');
        $password = (string) $request->post('password', '');

        $result = User::attempt($email, $password, $request->getRealIp());

        if (!$result['ok']) {
            return $this->view('login', [
                'email' => $email,
                'error' => $result['message'],
                'registerOpen' => Settings::bool('register.open', false),
            ]);
        }

        session()->set(self::SESSION_KEY, (int) $result['user']['id']);
        session()->set('user_login_at', time());

        return redirect('/console');
    }

    /** POST /logout */
    public function logout(Request $request): Response
    {
        if (Csrf::check($request->post('_csrf'))) {
            // 只删用户相关的键，不动整个会话 ——
            // 否则同一个浏览器里正开着的后台会被一起登出
            session()->delete(self::SESSION_KEY);
            session()->delete('user_login_at');
        }

        return redirect('/login');
    }

    /**
     * 当前登录用户的 id（未登录返回 0）。
     * 放在这里而不是单独一个类，是因为「谁算登录」只有这一处定义最不易错。
     */
    public static function currentUserId(): int
    {
        $id = (int) session()->get(self::SESSION_KEY, 0);

        return $id > 0 ? $id : 0;
    }

    // ═══════════════════════════════════════════════════════════
    // 注册
    // ═══════════════════════════════════════════════════════════

    /** GET /register */
    public function registerPage(Request $request): Response
    {
        if (self::currentUserId() > 0) {
            return redirect('/console');
        }

        if (!Settings::bool('register.open', false)) {
            return $this->view('notice', [
                'title' => '暂未开放注册',
                'message' => '本站当前不开放自助注册。如需账号请联系站长开通。',
                'ok' => false,
            ]);
        }

        return $this->view('register', [
            'email' => '',
            'inviteCode' => '',
            'error' => '',
            'needVerify' => Settings::bool('register.need_verify', true),
            'needInvite' => trim((string) Settings::get('register.invite_code', '')) !== '',
            'mailReady' => Mailer::enabled(),
        ]);
    }

    /** POST /register */
    public function register(Request $request): Response
    {
        if (!Settings::bool('register.open', false)) {
            return $this->view('notice', [
                'title' => '暂未开放注册',
                'message' => '本站当前不开放自助注册。',
                'ok' => false,
            ]);
        }

        if (!Csrf::check($request->post('_csrf'))) {
            return $this->registerFormError($request, '页面已过期，请重新提交');
        }

        $email = trim((string) $request->post('email', ''));
        $password = (string) $request->post('password', '');
        $password2 = (string) $request->post('password2', '');
        $invite = trim((string) $request->post('invite_code', ''));

        // ── 邀请码 ──
        $expectedInvite = trim((string) Settings::get('register.invite_code', ''));
        if ($expectedInvite !== '' && !hash_equals($expectedInvite, $invite)) {
            return $this->registerFormError($request, '邀请码不正确', $email, $invite);
        }

        if (!User::isValidEmail($email)) {
            return $this->registerFormError($request, '邮箱格式不正确', $email, $invite);
        }

        if (mb_strlen($password) < self::MIN_PASSWORD_LENGTH) {
            return $this->registerFormError(
                $request,
                '密码至少 ' . self::MIN_PASSWORD_LENGTH . ' 位',
                $email,
                $invite
            );
        }

        if ($password !== $password2) {
            return $this->registerFormError($request, '两次输入的密码不一致', $email, $invite);
        }

        // ── 邮箱真实性验证 ──
        //
        // 放在这里（前面那些便宜的检查都过了之后）而不是最前面：
        // 密码不一致、邀请码错这类错误应当立刻返回，
        // 没必要为了它们去连一次外部邮件服务器（最长十几秒）。
        //
        // 这一步的意义是保护**发信信誉**：邮件服务商会统计无效地址率，
        // 指标一高整条通道会被降级甚至封禁，那时连真用户都收不到信。
        if (Settings::bool('register.verify_email', true)) {
            $check = EmailVerifier::verify($email);

            if ($check['result'] === EmailVerifier::INVALID) {
                return $this->registerFormError($request, $check['message'], $email, $invite);
            }

            // UNKNOWN 放行，但记一条日志 —— 站长据此能看出探测的实际有效率。
            // 判不了就拒绝是错的：那会挡掉真用户，比放进几个无效地址严重得多
            if ($check['result'] === EmailVerifier::UNKNOWN) {
                Log::info("注册邮箱未能确认存在性（{$check['stage']}）：{$email} —— {$check['message']}");
            }
        }

        // ── 是否需要邮箱验证 ──
        // 需要验证、但邮件服务没配好 → 直接视为已验证（见文件头说明）
        $mailReady = Mailer::enabled();
        $needVerify = Settings::bool('register.need_verify', true) && $mailReady;

        if (Settings::bool('register.need_verify', true) && !$mailReady) {
            Log::warning('注册要求邮箱验证，但邮件服务未配置或未启用 —— 本次注册已直接完成验证，请尽快配置 SMTP');
        }

        $created = User::register(
            $email,
            $password,
            $request->getRealIp() ?: 'unknown',
            $needVerify,
            Settings::float('register.gift_balance', 0.0)
        );

        if (!$created['ok']) {
            return $this->registerFormError($request, $created['message'], $email, $invite);
        }

        // 自动建一把令牌，省掉「注册完还要自己建令牌」这一步。
        // 额度取配置值（0 = 不限），管理员可用它控制新用户的可消耗上限
        $token = UserToken::create(
            $created['id'],
            '默认令牌',
            Settings::float('register.default_token_quota', 0.0)
        );

        if ($needVerify) {
            $this->sendVerifyMail($email, $created['verifyToken']);

            return $this->view('notice', [
                'title' => '注册成功，请验证邮箱',
                'message' => '我们已向 ' . $email . ' 发送了一封验证邮件，'
                    . '点击邮件里的链接即可激活账号。'
                    . "\n" . '若长时间没收到，请检查垃圾邮件文件夹。',
                'ok' => true,
                'extra' => '提示：验证完成后即可用邮箱与密码登录。',
            ]);
        }

        // 不需要验证：直接登录进控制台，并在页面上把令牌明文给他看一次
        session()->set(self::SESSION_KEY, $created['id']);
        session()->set('user_login_at', time());

        return $this->view('notice', [
            'title' => '注册成功',
            'message' => $mailReady
                ? '账号已激活，你现在就可以使用了。'
                : '账号已激活。注意：本站开启了「邮箱验证」但邮件服务尚未配置，'
                    . '本次已直接完成验证（此情况已记入日志，请站长尽快配置 SMTP）。',
            'ok' => true,
            'tokenPlain' => $token['plain'],
            'extra' => '下面这把令牌只会显示这一次，请立即复制保存 —— 之后列表里只显示掩码。',
        ]);
    }

    /** GET /verify —— 邮箱验证 */
    public function verify(Request $request): Response
    {
        $token = trim((string) $request->get('token', ''));
        $user = User::findByVerifyToken($token);

        if ($user === null) {
            return $this->view('notice', [
                'title' => '验证链接无效',
                'message' => '这个验证链接无效或已经使用过了。'
                    . "\n" . '如果你已经验证成功，直接去登录即可；' . "\n"
                    . '否则请重新注册，或联系站长处理。',
                'ok' => false,
            ]);
        }

        User::markVerified((int) $user['id']);

        return $this->view('notice', [
            'title' => '邮箱验证成功',
            'message' => '你的账号已激活，现在可以用邮箱与密码登录了。',
            'ok' => true,
        ]);
    }

    // ═══════════════════════════════════════════════════════════
    // 找回密码
    // ═══════════════════════════════════════════════════════════

    /** GET /forgot */
    public function forgotPage(Request $request): Response
    {
        return $this->view('forgot', [
            'email' => '',
            'error' => '',
        ]);
    }

    /** POST /forgot */
    public function forgot(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->view('forgot', ['email' => '', 'error' => '页面已过期，请重新提交']);
        }

        $email = trim((string) $request->post('email', ''));
        $user = User::findByEmail($email);

        // 无论邮箱是否存在都返回同样的提示。
        // 这里与注册流程的取舍不同：注册时含糊其辞会让正常用户困惑，
        // 而找回密码页如果如实回答「该邮箱未注册」，
        // 就成了一个免费的账号枚举接口 —— 收益太小，风险更大
        if ($user !== null && Mailer::enabled()) {
            $token = User::issueResetToken((int) $user['id']);
            $this->sendResetMail($email, $token);
        }

        return $this->view('notice', [
            'title' => '重置链接已发出',
            'message' => '如果 ' . $email . ' 是本站已注册的邮箱，'
                . '你会收到一封包含重置链接的邮件（有效期 1 小时）。',
            'ok' => true,
        ]);
    }

    /** GET /reset */
    public function resetPage(Request $request): Response
    {
        $token = trim((string) $request->get('token', ''));
        $user = User::findByResetToken($token);

        if ($user === null || !User::resetTokenValid($user)) {
            return $this->view('notice', [
                'title' => '重置链接无效或已过期',
                'message' => '这个重置链接无效或已超过有效期（1 小时）。'
                    . "\n" . '请回到「找回密码」重新申请一次。',
                'ok' => false,
            ]);
        }

        return $this->view('reset', [
            'token' => $token,
            'error' => '',
        ]);
    }

    /** POST /reset */
    public function reset(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->view('notice', [
                'title' => '提交失败',
                'message' => '页面已过期，请重新打开重置链接。',
                'ok' => false,
            ]);
        }

        $token = trim((string) $request->post('token', ''));
        $password = (string) $request->post('password', '');
        $password2 = (string) $request->post('password2', '');

        $user = User::findByResetToken($token);

        if ($user === null || !User::resetTokenValid($user)) {
            return $this->view('notice', [
                'title' => '重置链接无效或已过期',
                'message' => '请回到「找回密码」重新申请一次。',
                'ok' => false,
            ]);
        }

        if (mb_strlen($password) < self::MIN_PASSWORD_LENGTH) {
            return $this->view('reset', [
                'token' => $token,
                'error' => '密码至少 ' . self::MIN_PASSWORD_LENGTH . ' 位',
            ]);
        }

        if ($password !== $password2) {
            return $this->view('reset', [
                'token' => $token,
                'error' => '两次输入的密码不一致',
            ]);
        }

        User::setPassword((int) $user['id'], $password);

        return $this->view('notice', [
            'title' => '密码已重置',
            'message' => '请用新密码登录。',
            'ok' => true,
        ]);
    }

    // ═══════════════════════════════════════════════════════════
    // 内部
    // ═══════════════════════════════════════════════════════════

    /**
     * 注册表单出错：带着已填内容回到表单，省得用户重打一遍。
     */
    private function registerFormError(Request $request, string $error, string $email = '', string $invite = ''): Response
    {
        return $this->view('register', [
            'email' => $email,
            'inviteCode' => $invite,
            'error' => $error,
            'needVerify' => Settings::bool('register.need_verify', true),
            'needInvite' => trim((string) Settings::get('register.invite_code', '')) !== '',
            'mailReady' => Mailer::enabled(),
        ]);
    }

    private function sendVerifyMail(string $email, string $token): void
    {
        $this->sendMail($email, '验证你的邮箱', '点击下面的按钮完成邮箱验证', $token, 'verify', '验证邮箱');
    }

    private function sendResetMail(string $email, string $token): void
    {
        $this->sendMail($email, '重置密码', '点击下面的按钮设置新密码（链接 1 小时内有效）', $token, 'reset', '设置新密码');
    }

    /**
     * 发一封带按钮的邮件。
     *
     * 链接地址用 Epay::baseUrl 同一套推导逻辑（配置的域名优先，否则用请求 Host）——
     * 因为「回调地址」和「邮件里的链接」面对的是同一个问题：
     * 必须是对外可访问的地址，用 127.0.0.1 会让人点不开。
     */
    private function sendMail(
        string $email,
        string $subject,
        string $line,
        string $token,
        string $action,
        string $buttonText
    ): void {
        $base = $this->baseUrl();

        $html = Mailer::template(
            $subject,
            [
                '你好，',
                $line,
                '如果不是你本人操作，忽略这封邮件即可。',
            ],
            $buttonText,
            $base . '/' . $action . '?token=' . urlencode($token)
        );

        $result = Mailer::send($email, $subject, $html);

        if (!$result['ok']) {
            // 发信失败不能中断注册/找回流程，但必须留痕 ——
            // 站长是靠日志发现「用户收不到邮件」的
            Log::error("向 {$email} 发送「{$subject}」失败：" . $result['error']);
        }
    }

    /**
     * 站点对外基址。
     *
     * 用 Url::base() 而不是自己判断协议：回调地址与邮件链接面对的是同一个
     * 「反代下 scheme 会判错」的坑，两处各写一遍必然会不一致。
     */
    private function baseUrl(): string
    {
        try {
            return Url::base(request());
        } catch (Throwable) {
            return 'http://localhost';
        }
    }

    /**
     * 渲染用户侧页面。
     *
     * @param array<string, mixed> $vars
     */
    private function view(string $template, array $vars = []): Response
    {
        return view('user/' . $template, array_merge([
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'notice' => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
        ], $vars), '');
    }
}
