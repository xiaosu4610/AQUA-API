<?php
/**
 * 后台 · 用户与订单
 *
 * 路由：
 *   GET  /admin/users           用户列表
 *   POST /admin/users/status    启用 / 停用
 *   POST /admin/users/balance   调整余额（正数加、负数减）
 *   GET  /admin/orders          订单列表
 *   POST /admin/orders/fail     把订单标记为失败
 *
 * ═══ 为什么调整余额要单独记一笔日志 ═══
 *
 * 站长手工加减余额是**绕过支付渠道直接改钱**，属于最需要留痕的操作。
 * 这里不做「操作日志表」（那是另一个功能），但每次调整都写框架日志，
 * 至少能在出问题时对得上账。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Csrf;
use app\common\PaymentOrder;
use app\common\Settings;
use app\common\User;
use support\Log;
use support\Request;
use support\Response;

class AdminUserController
{
    private const PER_PAGE = 50;

    private const FLASH_NOTICE = 'admin_user_notice';
    private const FLASH_TYPE = 'admin_user_notice_type';

    /**
     * GET /admin/users
     */
    public function index(Request $request): Response
    {
        $page = max(1, (int) $request->get('page', 1));
        $total = User::count();
        $pages = max(1, (int) ceil($total / self::PER_PAGE));
        $page = min($page, $pages);

        $rows = [];
        foreach (User::page(self::PER_PAGE, ($page - 1) * self::PER_PAGE) as $row) {
            $rows[] = [
                'id' => (int) $row['id'],
                'email' => (string) $row['email'],
                'displayName' => (string) $row['display_name'],
                'balance' => (float) $row['balance'],
                'totalSpent' => (float) $row['total_spent'],
                'enabled' => (int) $row['status'] === User::STATUS_ENABLED,
                'verified' => User::isVerified($row),
                'createdAt' => date('Y-m-d', (int) $row['created_at']),
                'lastLoginAt' => is_numeric($row['last_login_at'] ?? null)
                    ? date('Y-m-d H:i', (int) $row['last_login_at'])
                    : '从未登录',
            ];
        }

        $orders = PaymentOrder::summary();

        return view('admin/users', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'rows' => $rows,
            'page' => $page,
            'pages' => $pages,
            'total' => $total,
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'orderSummary' => $orders,
            'registerOpen' => Settings::bool('register.open', false),
            'idStatus' => $this->idStatus(),
            'notice' => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
        ], '');
    }

    /**
     * 编号维护的现状（给「编号维护」卡片用）。
     *
     * @return array{gap:bool, total:int, maxId:int, auto:bool, intervalDays:int, lastAt:string, epoch:int, quiet:bool}
     */
    private function idStatus(): array
    {
        $row = User::selectCountAndMax();
        $last = Settings::int('users.last_compacted_at', 0);

        return [
            'gap' => $row['total'] > 0 && $row['max'] !== $row['total'],
            'total' => $row['total'],
            'maxId' => $row['max'],
            'auto' => Settings::bool('users.auto_compact_ids', true),
            'intervalDays' => max(1, Settings::int('users.compact_interval_days', 3)),
            'lastAt' => $last > 0 ? date('Y-m-d H:i', $last) : '从未补位',
            'epoch' => User::idEpoch(),
            'quiet' => User::canCompactNow(),
        ];
    }

    /**
     * POST /admin/users/compact —— 立即把用户编号重排成连续编号
     *
     * 与定时任务（app/process/UserIdMaintainer）是同一个实现，
     * 只是由站长手动触发。有在途流量时**不执行**：
     * 重排会让编号换人，正在飞的请求可能把扣费记到别人账上。
     */
    public function compact(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        if (!User::needsCompact()) {
            return $this->back('当前编号本来就是连续的，无需补位', 'ok');
        }

        if (!User::canCompactNow()) {
            return $this->back('近 5 分钟内有调用流量，为避免扣费记错人，本次未执行；请稍后再试', 'err');
        }

        $result = User::compactIds();

        Log::warning(sprintf(
            '管理员手动重排用户编号：共 %d 个账号，移动 %d 个',
            $result['total'],
            $result['moved']
        ));

        return $this->back(
            sprintf('编号已重排：现有 %d 个账号全部连续，移动了 %d 个；所有用户需要重新登录（旧登录态已失效）',
                $result['total'],
                $result['moved']),
            'ok'
        );
    }

    /**
     * POST /admin/users/status
     */
    public function status(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('id', 0);
        if (User::find($id) === null) {
            return $this->back('用户不存在', 'err');
        }

        $enable = (int) $request->post('enable_id', 0) > 0;
        User::setStatus($id, $enable ? User::STATUS_ENABLED : User::STATUS_DISABLED);

        return $this->back($enable ? '用户已启用' : '用户已停用（其令牌同时失效）', 'ok');
    }

    /**
     * POST /admin/users/balance —— 手工调整余额
     *
     * 正数充值、负数扣减。扣减走 User::tryDebit（余额不足会拒绝），
     * 避免站长一不小心把余额改成负数。
     */
    public function balance(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('id', 0);
        $user = User::find($id);

        if ($user === null) {
            return $this->back('用户不存在', 'err');
        }

        $delta = round((float) $request->post('delta', 0), 8);

        if ($delta == 0.0) {
            return $this->back('调整金额不能为 0', 'err');
        }

        if ($delta > 0) {
            User::credit($id, $delta);
        } elseif (!User::tryDebit($id, abs($delta))) {
            return $this->back('余额不足，无法扣减这么多', 'err');
        }

        // 手工改钱必须留痕，便于事后对账
        Log::warning(sprintf(
            '管理员手工调整用户余额：user_id=%d, 变动=%s',
            $id,
            $delta > 0 ? '+' . $delta : (string) $delta
        ));

        return $this->back(
            '已调整余额 ' . ($delta > 0 ? '+' : '') . number_format($delta, 8) . '，'
            . '当前余额 ' . User::money((float) User::find($id)['balance']),
            'ok'
        );
    }

    /**
     * GET /admin/orders
     */
    public function orders(Request $request): Response
    {
        $status = (string) $request->get('status', '');
        $page = max(1, (int) $request->get('page', 1));
        $total = PaymentOrder::count($status);
        $pages = max(1, (int) ceil($total / self::PER_PAGE));
        $page = min($page, $pages);

        $rows = [];
        foreach (PaymentOrder::page(self::PER_PAGE, ($page - 1) * self::PER_PAGE, $status) as $row) {
            $user = User::find((int) $row['user_id']);

            $rows[] = [
                'id' => (int) $row['id'],
                'orderNo' => (string) $row['order_no'],
                'userId' => (int) $row['user_id'],
                'userEmail' => $user === null ? '（用户已不存在）' : (string) $user['email'],
                'amount' => (float) $row['amount'],
                'credit' => (float) $row['credit'],
                'gateway' => (string) $row['gateway'],
                'payType' => (string) $row['pay_type'],
                'tradeNo' => (string) ($row['trade_no'] ?? ''),
                'status' => (string) $row['status'],
                'createdAt' => date('Y-m-d H:i', (int) $row['created_at']),
                'paidAt' => is_numeric($row['paid_at'] ?? null) ? date('m-d H:i', (int) $row['paid_at']) : '—',
            ];
        }

        return view('admin/orders', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'rows' => $rows,
            'status' => $status,
            'page' => $page,
            'pages' => $pages,
            'total' => $total,
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'summary' => PaymentOrder::summary(),
            'paymentEnabled' => \app\common\Epay::enabled(),
            'notice' => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
        ], '');
    }

    /**
     * POST /admin/orders/fail —— 标记订单失败
     *
     * 典型用途：用户反馈「付了钱但没到账」，站长核对网关后台后确认
     * 这笔根本没支付成功，或者需要作废它避免重复处理。
     * 注意这里**只改状态、不加钱** —— 要补钱请用「用户余额调整」，
     * 两个动作分开才能让每笔钱都有明确来源。
     */
    public function fail(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err', '/admin/orders');
        }

        $id = (int) $request->post('id', 0);
        $order = PaymentOrder::find($id);

        if ($order === null) {
            return $this->back('订单不存在', 'err', '/admin/orders');
        }

        if ((string) $order['status'] === PaymentOrder::STATUS_PAID) {
            return $this->back('该订单已支付到账，不能标记失败', 'err', '/admin/orders');
        }

        PaymentOrder::markFailed($id, '由管理员手工标记失败');
        Log::warning("管理员把订单标记为失败：order_no={$order['order_no']}");

        return $this->back('订单已标记为失败', 'ok', '/admin/orders');
    }

    private function back(string $message, string $type, string $location = '/admin/users'): Response
    {
        session()->set(self::FLASH_NOTICE, $message);
        session()->set(self::FLASH_TYPE, $type);

        return response('', 302, ['Location' => $location]);
    }
}
