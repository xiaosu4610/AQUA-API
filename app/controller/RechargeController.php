<?php
/**
 * 下游用户 · 充值（易支付 V1 / V2）
 *
 * 路由：
 *   GET  /recharge            充值页（金额、支付方式、订单记录）
 *   POST /recharge/create     创建订单并跳转支付
 *   GET  /pay/notify          异步回调（**公开，网关调用**）
 *   GET  /pay/return          同步返回页
 *
 * ═══ 为什么 notify 与 return 是两件事 ═══
 *
 *   · notify（异步）：网关服务器直接请求我们，**这个才是到账依据**。
 *     它不依赖用户浏览器，用户付完钱就关掉页面也照样到账。
 *   · return（同步）：用户付款后被浏览器带回来的页面，
 *     **只能用来展示结果，绝不能用来加钱** ——
 *     用户手工访问一次这个地址就能给自己加钱，那是个印钞漏洞。
 *
 * ═══ 为什么 notify 不校验登录态与 CSRF ═══
 *
 * 因为请求来自支付网关的服务器，既没有我们的会话、也不可能有 CSRF 令牌。
 * 它的安全性**完全建立在签名校验上**（Epay::verifyNotify）——
 * 密钥只有我们和网关知道，伪造不了。这也是为什么签名必须用
 * 定长比较、且校验失败必须留痕。
 *
 * ═══ 幂等 ═══
 *
 * 网关没收到 "success" 就会不断重试回调，重复到达是**常态而非异常**。
 * 到账动作由 PaymentOrder::markPaid 用带条件的 UPDATE 保证只加一次钱。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Csrf;
use app\common\Epay;
use app\common\PaymentOrder;
use app\common\Settings;
use app\common\Url;
use support\Log;
use support\Request;
use support\Response;
use Throwable;

class RechargeController
{
    private const FLASH_NOTICE = 'user_notice';
    private const FLASH_TYPE = 'user_notice_type';

    /**
     * GET /recharge —— 充值页
     */
    public function index(Request $request): Response
    {
        $user = $this->currentUser();
        if ($user === null) {
            return redirect('/login');
        }

        $config = Epay::config();

        $orders = [];
        foreach (PaymentOrder::forUser((int) $user['id'], 50) as $row) {
            $orders[] = [
                'orderNo' => (string) $row['order_no'],
                'amount' => (float) $row['amount'],
                'credit' => (float) $row['credit'],
                'status' => (string) $row['status'],
                'payType' => (string) $row['pay_type'],
                'createdAt' => date('Y-m-d H:i', (int) $row['created_at']),
                'paidAt' => is_numeric($row['paid_at'] ?? null) ? date('m-d H:i', (int) $row['paid_at']) : '—',
            ];
        }

        return $this->view('recharge', [
            'user' => $user,
            'orders' => $orders,
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'enabled' => Epay::enabled(),
            'configured' => Epay::isConfigured(),
            'payTypes' => $config['pay_types'],
            'payTypeLabels' => Epay::PAY_TYPES,
            'minAmount' => $config['min_amount'],
            'creditRate' => $config['credit_rate'],
            'notice' => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
        ]);
    }

    /**
     * POST /recharge/create —— 创建订单并跳到支付页
     */
    public function create(Request $request): Response
    {
        $user = $this->currentUser();
        if ($user === null) {
            return redirect('/login');
        }

        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        if (!Epay::enabled()) {
            return $this->back('在线充值当前未开放', 'err');
        }

        $config = Epay::config();

        // 金额做两位小数归一：网关对 1 与 1.00 的处理可能不同，
        // 而且浮点误差会算出 9.999999999 这种值
        $amount = round((float) $request->post('amount', 0), 2);
        $payType = (string) $request->post('pay_type', '');

        if ($amount < $config['min_amount']) {
            return $this->back('单笔充值不能低于 ' . number_format($config['min_amount'], 2), 'err');
        }

        if ($amount > 100000) {
            // 上限是防御性的：金额填错一位（比如 1000 打成 100000）
            // 会导致用户付出一笔远超预期的钱
            return $this->back('单笔充值金额过大，请联系站长处理', 'err');
        }

        if (!in_array($payType, $config['pay_types'], true)) {
            return $this->back('请选择支付方式', 'err');
        }

        // 先落库、再跳转：顺序反了会出现「钱付了但找不到订单」
        $credit = round($amount * $config['credit_rate'], 4);
        $orderId = PaymentOrder::create(
            (int) $user['id'],
            $amount,
            $credit,
            $config['gateway'],
            $payType
        );

        $order = PaymentOrder::find($orderId);
        if ($order === null) {
            return $this->back('订单创建失败，请重试', 'err');
        }

        $built = Epay::buildPayUrl($order, $this->baseUrl($request), $payType);

        if (!$built['ok']) {
            return $this->back('发起支付失败：' . $built['error'], 'err');
        }

        // V2 是 API 型：先请求一次拿到真正的支付地址
        if ($config['gateway'] === Epay::V2) {
            $response = Epay::httpGet($built['url']);

            if (!$response['ok']) {
                return $this->back('发起支付失败：' . $response['error'], 'err');
            }

            $extracted = Epay::extractPayUrl($response['body']);

            if (!$extracted['ok']) {
                Log::error('易支付 V2 返回无法解析：' . $response['body']);

                return $this->back('发起支付失败：' . $extracted['error'], 'err');
            }

            return redirect($extracted['url']);
        }

        // V1 是页面跳转型：直接跳过去就行
        return redirect($built['url']);
    }

    /**
     * GET|POST /pay/notify —— 异步回调（到账的唯一依据）
     */
    public function notify(Request $request): Response
    {
        $params = $this->callbackParams($request);
        $raw = json_encode($params, JSON_UNESCAPED_UNICODE) ?: '';

        // 原始回调一律先落日志：出问题时这是唯一的一手证据
        Log::info('收到支付回调：' . $raw);

        $verified = Epay::verifyNotify($params);
        if (!$verified['ok']) {
            return response('fail', 400);
        }

        $orderNo = (string) ($params['out_trade_no'] ?? '');
        if ($orderNo === '') {
            return response('fail', 400);
        }

        if (!Epay::notifyIsPaid($params)) {
            // 验签通过但不是「支付成功」状态（比如用户中途取消）—— 如实回应，
            // 不要标记失败：用户可能还会重新支付，订单应保持 pending
            Log::info('支付回调不是成功状态，忽略：' . $raw);

            return response('success');
        }

        $result = PaymentOrder::markPaid($orderNo, (string) ($params['trade_no'] ?? ''), $raw);

        if (!$result['ok']) {
            Log::error('支付回调处理失败：' . $result['message'] . '，原文：' . $raw);

            return response('fail', 400);
        }

        if ($result['already']) {
            Log::info('重复回调，已幂等处理：' . $orderNo);
        }

        // 必须原样回 "success"，网关才不会再重试
        return response('success');
    }

    /**
     * GET /pay/return —— 同步返回页
     *
     * 只展示结果，**绝不在这里加钱**（见文件头说明）。
     */
    public function returnPage(Request $request): Response
    {
        $orderNo = (string) $request->get('out_trade_no', '');
        $order = $orderNo === '' ? null : PaymentOrder::findByOrderNo($orderNo);

        return $this->view('pay_result', [
            'orderNo' => $orderNo,
            'paid' => $order !== null && (string) $order['status'] === PaymentOrder::STATUS_PAID,
            'pending' => $order !== null && (string) $order['status'] === PaymentOrder::STATUS_PENDING,
            'found' => $order !== null,
            'amount' => $order === null ? 0.0 : (float) $order['amount'],
            'credit' => $order === null ? 0.0 : (float) $order['credit'],
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
        ]);
    }

    // ═══════════════════════════════════════════════════════════
    // 内部
    // ═══════════════════════════════════════════════════════════

    /**
     * 合并回调参数。
     *
     * GET 与 POST 都要收：不同易支付实现用不同的方式回传，
     * 有的 GET、有的 POST、有的两者都带。合并时以「后者覆盖前者」，
     * 但绝不接受任何来自 body 的额外字段名 —— 只取我们认识的那些，
     * 避免把无关数据（甚至恶意构造的字段）带进签名计算。
     *
     * @return array<string, string>
     */
    private function callbackParams(Request $request): array
    {
        $allowed = [
            'pid', 'trade_no', 'out_trade_no', 'type', 'name', 'money', 'trade_status',
            'status', 'sign', 'sign_type', 'param', 'buyer', 'addtime', 'endtime',
        ];

        $result = [];

        foreach ([$request->get(), $request->post()] as $source) {
            foreach ((array) $source as $key => $value) {
                if (in_array($key, $allowed, true) && is_scalar($value)) {
                    $result[$key] = (string) $value;
                }
            }
        }

        return $result;
    }

    /** @return array<string, mixed>|null */
    private function currentUser(): ?array
    {
        $id = AuthController::currentUserId();

        return $id > 0 ? \app\common\User::find($id) : null;
    }

    /**
     * 站点对外基址（用于拼回调地址）。
     *
     * 回调地址必须是**网关服务器能访问到的外部地址** —— 用 127.0.0.1
     * 会让订单永远停在 pending，而订单页看起来一切正常，极难排查。
     * 因此走 Url::base()（优先用后台配的 site.domain）。
     */
    private function baseUrl(Request $request): string
    {
        return Url::base($request);
    }

    private function back(string $message, string $type): Response
    {
        session()->set(self::FLASH_NOTICE, $message);
        session()->set(self::FLASH_TYPE, $type);

        return redirect('/recharge');
    }

    /**
     * @param array<string, mixed> $vars
     */
    private function view(string $template, array $vars = []): Response
    {
        return view('user/' . $template, array_merge([
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
        ], $vars), '');
    }
}
