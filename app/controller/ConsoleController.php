<?php
/**
 * 下游用户 · 控制台
 *
 * 路由（全部需要登录）：
 *   GET  /console                概览：余额、令牌、用量
 *   POST /console/token/create   新建令牌
 *   POST /console/token/toggle   启用 / 停用令牌
 *   POST /console/token/delete   删除令牌
 *   POST /console/token/reset    重置令牌已用额度
 *   POST /console/profile        改显示名
 *   POST /console/password       改密码
 *
 * ═══ 越权防护 ═══
 *
 * 所有针对「某个令牌」的操作都必须先确认它属于当前登录用户 ——
 * 否则改一下表单里的 id 就能操作别人的令牌。
 * 这类漏洞在自研后台里极其常见，所以这里把它做成一个统一步骤
 * （见 requireOwnedToken），而不是每个 action 各写一遍判断。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Csrf;
use app\common\Settings;
use app\common\UsageLog;
use app\common\User;
use app\common\UserToken;
use app\common\Url;
use support\Log;
use support\Request;
use support\Response;

class ConsoleController
{
    private const MIN_PASSWORD_LENGTH = 8;

    private const FLASH_NOTICE = 'user_notice';
    private const FLASH_TYPE = 'user_notice_type';

    /**
     * GET /console —— 控制台首页
     */
    public function index(Request $request): Response
    {
        $user = $this->currentUser();
        if ($user === null) {
            return redirect('/login');
        }

        $tokens = [];
        foreach (UserToken::allForUser((int) $user['id']) as $row) {
            $usable = UserToken::usable($row);

            $tokens[] = [
                'id' => (int) $row['id'],
                'name' => (string) $row['name'],
                'mask' => (string) $row['key_mask'],
                'enabled' => (int) $row['status'] === UserToken::STATUS_ENABLED,
                'usable' => $usable['ok'],
                'reason' => $usable['reason'],
                'quotaLabel' => UserToken::quotaLabel($row),
                'models' => trim((string) ($row['models'] ?? '')),
                'expiresAt' => $this->formatTime($row['expires_at'] ?? null),
                'lastUsedAt' => $this->formatTime($row['last_used_at'] ?? null),
            ];
        }

        $todayStart = strtotime('today') ?: time();

        return $this->view('console', [
            'user' => $user,
            'tokens' => $tokens,
            'today' => UsageLog::summaryForUser((int) $user['id'], $todayStart),
            'month' => UsageLog::summaryForUser((int) $user['id'], $todayStart - 29 * 86400),
            'recent' => $this->recentRows((int) $user['id']),
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'rechargeEnabled' => \app\common\Epay::enabled(),
            // 给页面上的「Base URL 复制」用：用户接入时唯一需要改的就是它
            'apiBaseUrl' => Url::apiBase(request()),
            'apiExample' => $this->apiExample(),
        ]);
    }

    /** 一段可直接抄走的调用示例（把真实地址塞进去，省得用户自己拼） */
    private function apiExample(): string
    {
        return "curl " . Url::apiBase(request()) . "/chat/completions \\\n"
            . "  -H \"Authorization: Bearer 你的令牌\" \\\n"
            . "  -H \"Content-Type: application/json\" \\\n"
            . "  -d '{\"model\":\"模型名\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}'";
    }

    /**
     * POST /console/token/create
     */
    public function createToken(Request $request): Response
    {
        $user = $this->currentUser();
        if ($user === null) {
            return redirect('/login');
        }

        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $name = trim((string) $request->post('name', ''));
        $quota = (float) $request->post('quota_limit', 0);
        $models = trim((string) $request->post('models', ''));
        $expiresDays = (int) $request->post('expires_days', 0);

        if ($quota < 0) {
            return $this->back('额度不能为负数', 'err');
        }

        // 有效期用「天数」而不是让用户填时间戳：
        // 让人算 Unix 时间戳是不合理的
        $expiresAt = $expiresDays > 0 ? time() + $expiresDays * 86400 : null;

        $created = UserToken::create((int) $user['id'], $name, $quota, $models, $expiresAt);

        // 明文令牌只在**创建这一刻**返回一次，之后永远拿不到
        return $this->view('token_created', [
            'plain' => $created['plain'],
            'name' => $name === '' ? '默认令牌' : $name,
        ]);
    }

    /**
     * POST /console/token/toggle
     */
    public function toggleToken(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $token = $this->requireOwnedToken($request);
        if ($token === null) {
            return $this->back('令牌不存在或不属于你', 'err');
        }

        $enable = (int) $request->post('enable_id', 0) > 0;
        UserToken::setStatus(
            (int) $token['id'],
            $enable ? UserToken::STATUS_ENABLED : UserToken::STATUS_DISABLED
        );

        return $this->back($enable ? '令牌已启用' : '令牌已停用', 'ok');
    }

    /**
     * POST /console/token/delete
     */
    public function deleteToken(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $token = $this->requireOwnedToken($request);
        if ($token === null) {
            return $this->back('令牌不存在或不属于你', 'err');
        }

        UserToken::delete((int) $token['id']);

        return $this->back('令牌已删除（用它的调用会立即失效）', 'ok');
    }

    /**
     * POST /console/token/reset —— 把「已用额度」清零
     *
     * 为什么用户自己就能重置额度：额度是**限制消费用的**，
     * 不是记账依据（记账在 usage_logs 里，用户改不了）。
     * 让用户自己重置，省掉了「额度用完后找站长清零」这件琐事。
     */
    public function resetTokenQuota(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $token = $this->requireOwnedToken($request);
        if ($token === null) {
            return $this->back('令牌不存在或不属于你', 'err');
        }

        UserToken::resetQuota((int) $token['id']);

        return $this->back('已把该令牌的「已用额度」清零', 'ok');
    }

    /**
     * POST /console/profile —— 改显示名
     */
    public function updateProfile(Request $request): Response
    {
        $user = $this->currentUser();
        if ($user === null) {
            return redirect('/login');
        }

        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        User::updateProfile((int) $user['id'], (string) $request->post('display_name', ''));

        return $this->back('资料已更新', 'ok');
    }

    /**
     * POST /console/password —— 改密码
     *
     * 与后台管理员改密码同理：必须先验证当前密码，
     * 否则会话被劫持后攻击者可以直接改密码把账号锁死。
     */
    public function changePassword(Request $request): Response
    {
        $user = $this->currentUser();
        if ($user === null) {
            return redirect('/login');
        }

        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $current = (string) $request->post('current_password', '');
        $new = (string) $request->post('new_password', '');
        $confirm = (string) $request->post('confirm_password', '');

        if (!password_verify($current, (string) $user['password_hash'])) {
            return $this->back('当前密码不正确', 'err');
        }

        if (mb_strlen($new) < self::MIN_PASSWORD_LENGTH) {
            return $this->back('新密码至少 ' . self::MIN_PASSWORD_LENGTH . ' 位', 'err');
        }

        if ($new !== $confirm) {
            return $this->back('两次输入的新密码不一致', 'err');
        }

        if ($new === $current) {
            return $this->back('新密码不能与当前密码相同', 'err');
        }

        User::setPassword((int) $user['id'], $new);

        // 改完密码清掉登录态强制重登 —— 与后台同一套逻辑：
        // 旧会话仍然有效等于「改了密码但旧凭证还能用」
        session()->delete(AuthController::SESSION_KEY);

        return redirect('/login');
    }

    // ═══════════════════════════════════════════════════════════
    // 内部
    // ═══════════════════════════════════════════════════════════

    /** @return array<string, mixed>|null */
    private function currentUser(): ?array
    {
        $id = AuthController::currentUserId();

        return $id > 0 ? User::find($id) : null;
    }

    /**
     * 取「当前用户拥有」的令牌；不属于当前用户时返回 null。
     *
     * 抽成统一方法的原因见文件头说明：越权防护必须是**一个必经步骤**，
     * 不能在每个 action 里各写一遍 —— 漏一个就是一个漏洞。
     *
     * @return array<string, mixed>|null
     */
    private function requireOwnedToken(Request $request): ?array
    {
        $user = $this->currentUser();
        if ($user === null) {
            return null;
        }

        $tokenId = (int) $request->post('enable_id', 0);
        if ($tokenId <= 0) {
            $tokenId = (int) $request->post('disable_id', 0);
        }
        if ($tokenId <= 0) {
            $tokenId = (int) $request->post('delete_id', 0);
        }
        if ($tokenId <= 0) {
            $tokenId = (int) $request->post('reset_id', 0);
        }
        if ($tokenId <= 0) {
            $tokenId = (int) $request->post('id', 0);
        }

        if ($tokenId <= 0 || !UserToken::belongsTo($tokenId, (int) $user['id'])) {
            return null;
        }

        return UserToken::find($tokenId);
    }

    /**
     * 最近的用量记录（整理成模板友好的形状）。
     *
     * @return array<int, array<string, mixed>>
     */
    private function recentRows(int $userId): array
    {
        $rows = [];

        foreach (UsageLog::recentForUser($userId, 20) as $row) {
            $rows[] = [
                'time' => date('m-d H:i', (int) $row['created_at']),
                'model' => (string) $row['model'],
                'tokens' => (int) $row['total_tokens'],
                'estimated' => (int) $row['usage_estimated'] === 1,
                'cost' => (float) $row['downstream_cost'],
                'status' => (string) $row['status'],
                'error' => (string) ($row['error_message'] ?? ''),
            ];
        }

        return $rows;
    }

    private function formatTime(mixed $timestamp): string
    {
        return is_numeric($timestamp) && (int) $timestamp > 0
            ? date('Y-m-d H:i', (int) $timestamp)
            : '—';
    }

    /**
     * 写入提示并回到控制台（POST → 重定向 → GET）。
     */
    private function back(string $message, string $type): Response
    {
        session()->set(self::FLASH_NOTICE, $message);
        session()->set(self::FLASH_TYPE, $type);

        return redirect('/console');
    }

    /**
     * 写入提示并回到指定页面。
     *
     * 与 back() 分开是因为注销确认页需要「出错后留在原页」——
     * 把人甩回控制台会让他找不到刚才那个确认入口。
     */
    private function backTo(string $path, string $message, string $type): Response
    {
        session()->set(self::FLASH_NOTICE, $message);
        session()->set(self::FLASH_TYPE, $type);

        return redirect($path);
    }

    /**
     * GET /console/delete —— 注销账号的确认页（第一步）
     *
     * 为什么单独一页而不是弹个 confirm：注销会连令牌一起删掉，
     * 用户的程序会立刻开始报 401。这种后果必须**离开当前页面、看清代价**再点，
     * 而不是在列表页上被一个弹窗顺手点掉。
     */
    public function deleteAccountPage(Request $request): Response
    {
        $user = $this->currentUser();
        if ($user === null) {
            return redirect('/login');
        }

        return $this->view('account_delete', [
            'user' => $user,
            'tokenCount' => count(UserToken::allForUser((int) $user['id'])),
        ]);
    }

    /**
     * POST /console/delete —— 真正注销（第二步）
     *
     * 二次确认的实现方式：这一页要求**重输密码 + 亲手输入自己的邮箱**，
     * 两个都对才执行。比勾一个「我已确认」的复选框可靠得多 ——
     * 后者在手机上很容易被误触。
     */
    public function deleteAccount(Request $request): Response
    {
        $user = $this->currentUser();
        if ($user === null) {
            return redirect('/login');
        }

        if (!Csrf::check($request->post('_csrf'))) {
            return $this->backTo('/console/delete', '页面已过期，请重新提交', 'err');
        }

        $password = (string) $request->post('password', '');
        $emailTyped = trim((string) $request->post('confirm_email', ''));

        // 直接比对密码哈希，**不**走 User::attempt()：后者会累计失败次数，
        // 在确认页输错一次密码不该让用户被锁号（那是给登录入口用的风控）
        if (!password_verify($password, (string) ($user['password_hash'] ?? ''))) {
            return $this->backTo('/console/delete', '密码不正确，注销已取消', 'err');
        }

        if (mb_strtolower($emailTyped) !== mb_strtolower((string) $user['email'])) {
            return $this->backTo('/console/delete', '邮箱没对上，注销已取消（请输入你自己的邮箱）', 'err');
        }

        $email = (string) $user['email'];
        User::deleteAccount((int) $user['id']);

        // 会话整段清掉：账号已经不存在了，留着登录态只会到处 401
        session()->flush();

        Log::warning("用户自助注销账号：{$email}（id={$user['id']}），其令牌已删除、订单与用量记录保留但已匿名");

        return $this->view('notice', [
            'title' => '账号已注销',
            'message' => "你的账号（{$email}）与全部令牌已删除。"
                . "\n" . '充值订单与用量记录因对账需要仍然保留，但已不再与该账号关联。'
                . "\n" . '如果这是误操作，请联系站长；重新注册会是一个全新的账号。',
            'ok' => true,
        ]);
    }

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
