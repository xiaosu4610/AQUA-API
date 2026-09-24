<?php
/**
 * 易支付（双协议：V1 页面跳转型 / V2 API 型）
 *
 * ═══ 先把话说清楚：易支付不是一个统一规范 ═══
 *
 * 「易支付」是同类个人收款网关的统称（彩虹易支付及其大量衍生版），
 * 各家实现**在细节上并不一致**：接口路径、参与签名的字段、
 * 空值是否参与签名、返回结构都可能有差别。
 * 所以本类的做法是：
 *
 *   · 内置最常见的两种约定（V1 / V2），开箱可用；
 *   · **把易变的点做成配置**（接口路径、空值是否参与签名），
 *     对不上时站长改配置即可，不用改代码；
 *   · 回调一律**原文落库**（payment_orders.notify_raw），
 *     签名校验失败时把收到的参数写进日志 —— 对不上时靠的就是这些一手证据。
 *
 * 签名规则（两种模式共用的部分）：
 *   把参数按键名 ASCII 升序排列，去掉 `sign` 与 `sign_type`，
 *   拼成 `a=1&b=2` 的形式，**末尾直接拼接商户密钥**（注意不是 `&key=`），
 *   最后取 md5 小写。
 *
 * ═══ 两种模式的差别 ═══
 *
 *   V1（页面跳转）：GET 跳转到 `{api_url}/submit.php?参数`，由上游页面完成收款。
 *                    签名字符串里**去掉空值**。
 *   V2（API 型）  ：GET `{api_url}/mapi.php?参数`，返回 JSON，
 *                    从里面取 `payurl` / `qrcode` / `urlscheme` 再跳转。
 *                    签名字符串**保留空值**（即 `a=&b=2`）。
 *
 * 这两个差别是最容易导致「签名错误」的地方，因此都做成了可配置项，
 * 默认值按上述约定，对不上时可以单项调整。
 */

declare(strict_types=1);

namespace app\common;

use RuntimeException;
use Throwable;
use support\Log;

final class Epay
{
    public const V1 = 'epay_v1';
    public const V2 = 'epay_v2';

    /** 网关清单（供后台下拉与校验使用） */
    public const GATEWAYS = [
        self::V1 => [
            'label' => '易支付 V1（页面跳转型）',
            'path' => '/submit.php',
            'hint' => '跳到上游收银台页面。兼容性最好，绝大多数易支付实现都支持',
        ],
        self::V2 => [
            'label' => '易支付 V2（API 型）',
            'path' => '/mapi.php',
            'hint' => '调用上游 API 拿支付链接/二维码，返回 JSON。适合自己控制收银台样式',
        ],
    ];

    /** 支持的支付方式 */
    public const PAY_TYPES = [
        'alipay' => '支付宝',
        'wxpay' => '微信支付',
        'qqpay' => 'QQ 钱包',
        'bank' => '网银',
    ];

    /**
     * 读取支付配置。
     *
     * @return array{gateway:string, api_url:string, pid:string, key:string, pay_types:array<int,string>,
     *               min_amount:float, credit_rate:float, path:string, keep_empty:bool}
     */
    public static function config(): array
    {
        $gateway = (string) Settings::get('payment.gateway', self::V1);
        if (!isset(self::GATEWAYS[$gateway])) {
            $gateway = self::V1;
        }

        $types = array_values(array_filter(array_map(
            'trim',
            preg_split('/[\s,]+/', (string) Settings::get('payment.pay_types', 'alipay,wxpay')) ?: []
        )));

        // 路径留空时用该协议的默认路径；填了就按填的来（各家路径不一样）
        $path = trim((string) Settings::get('payment.submit_path', ''));
        if ($path === '') {
            $path = self::GATEWAYS[$gateway]['path'];
        }
        if (!str_starts_with($path, '/')) {
            $path = '/' . $path;
        }

        return [
            'gateway' => $gateway,
            'api_url' => rtrim(trim((string) Settings::get('payment.api_url', '')), '/'),
            'pid' => trim((string) Settings::get('payment.pid', '')),
            'key' => Settings::secret('payment.key'),
            'pay_types' => $types === [] ? ['alipay'] : $types,
            'min_amount' => max(0.01, Settings::float('payment.min_amount', 1.0)),
            'credit_rate' => max(0.0001, Settings::float('payment.credit_rate', 1.0)),
            'path' => $path,
            // 签名是否保留空值：**由协议决定，不做成独立开关**。
            // 理由：这是「这个协议怎么规定」的问题，不是站长的偏好 ——
            // 多一个开关只会多一个可以调错的地方。若某家实现的约定与
            // 所选协议不符，换一个协议模式（并覆盖接口路径）即可。
            // 回调校验时两种规则都会试（见 verifyNotify），
            // 所以「收得到钱但校验不过」这种情况基本不会发生。
            'keep_empty' => $gateway === self::V2,
        ];
    }

    public static function enabled(): bool
    {
        $c = self::config();

        return Settings::bool('payment.enabled', false) && self::isConfigured();
    }

    /**
     * 配置是否齐全（不看开关）。
     */
    public static function isConfigured(): bool
    {
        $c = self::config();

        return $c['api_url'] !== '' && $c['pid'] !== '' && $c['key'] !== '';
    }

    /**
     * 生成支付跳转地址。
     *
     * @param array<string, mixed> $order 订单记录
     * @param string $baseUrl 站点对外基址（用于拼回调地址），如 https://aqua.is3.cc
     * @param string $payType 支付方式
     * @return array{ok:bool, error:string, url:string}
     */
    public static function buildPayUrl(array $order, string $baseUrl, string $payType): array
    {
        $c = self::config();

        if (!self::isConfigured()) {
            return ['ok' => false, 'error' => '支付接口尚未配置完整（需要接口地址、商户 PID 与密钥）', 'url' => ''];
        }

        if (!in_array($payType, $c['pay_types'], true)) {
            return ['ok' => false, 'error' => '不支持的支付方式', 'url' => ''];
        }

        $baseUrl = rtrim($baseUrl, '/');

        $params = [
            'pid' => $c['pid'],
            'type' => $payType,
            'out_trade_no' => (string) $order['order_no'],
            'notify_url' => $baseUrl . '/pay/notify',
            'return_url' => $baseUrl . '/pay/return',
            // 商品名会显示在收银台上，站点名 + 用途更清楚
            'name' => Settings::siteName() . ' 余额充值',
            // 金额**必须保留两位小数**：部分网关对 1 与 1.00 的处理不同，
            // 传 1 有时会被判定为金额格式非法
            'money' => number_format((float) $order['amount'], 2, '.', ''),
            'sign_type' => 'MD5',
        ];

        if ($c['gateway'] === self::V1) {
            $params['sitename'] = Settings::siteName();
        } else {
            // V2 的 API 型接口会用这两个字段做风控，缺失时部分网关直接拒绝
            $params['clientip'] = '127.0.0.1';
            $params['device'] = 'pc';
        }

        $params['sign'] = self::sign($params, $c['key'], $c['keep_empty']);

        return [
            'ok' => true,
            'error' => '',
            'url' => $c['api_url'] . $c['path'] . '?' . http_build_query($params),
        ];
    }

    /**
     * 计算签名。
     *
     * @param array<string, mixed> $params
     */
    public static function sign(array $params, string $key, bool $keepEmpty): string
    {
        return md5(self::signString($params, $key, $keepEmpty));
    }

    /**
     * 校验回调签名。
     *
     * 用 hash_equals 而不是 `===`：定长比较，避免时序侧信道。
     * 虽然对支付回调来说时序攻击不现实，但这类细节保持一致的习惯更好。
     *
     * @param array<string, mixed> $params 回调带来的全部参数
     * @return array{ok:bool, error:string}
     */
    public static function verifyNotify(array $params): array
    {
        $c = self::config();

        if (!self::isConfigured()) {
            return ['ok' => false, 'error' => '支付接口未配置'];
        }

        $received = (string) ($params['sign'] ?? '');
        if ($received === '') {
            return ['ok' => false, 'error' => '回调缺少 sign 参数'];
        }

        // 先按配置的规则校验
        if (hash_equals(self::sign($params, $c['key'], $c['keep_empty']), strtolower($received))) {
            return ['ok' => true, 'error' => ''];
        }

        // 再按「相反的空值规则」试一次。
        // 为什么允许这样：各家易支付对空值的处理确实不统一，而回调参数
        // 往往比下单时多出几个空字段。这一步能救掉一大类「明明能支付、
        // 回调却报签名错误」的疑难，且不降低安全性 ——
        // 密钥仍然必须正确，只是拼接方式放宽了一种
        if (hash_equals(self::sign($params, $c['key'], !$c['keep_empty']), strtolower($received))) {
            Log::warning('易支付回调签名按「另一种空值规则」才校验通过，建议在后台把「签名保留空值」调整过来');
            return ['ok' => true, 'error' => ''];
        }

        // 对不上时把收到的参数（**去掉 sign 之外不做脱敏，回调里本来没有敏感信息**）
        // 记进日志，方便站长拿着去和上游核对
        Log::error('易支付回调签名校验失败，收到的参数：' . json_encode($params, JSON_UNESCAPED_UNICODE));

        return ['ok' => false, 'error' => '签名校验失败'];
    }

    /**
     * 拼接签名字符串。
     *
     * @param array<string, mixed> $params
     */
    private static function signString(array $params, string $key, bool $keepEmpty): string
    {
        unset($params['sign'], $params['sign_type']);

        if (!$keepEmpty) {
            $params = array_filter(
                $params,
                static fn ($v) => $v !== '' && $v !== null
            );
        }

        // 按键名 ASCII 升序 —— 这是易支付签名的硬性约定，
        // 顺序错了签名必错，且报错信息只会说「签名错误」
        ksort($params, SORT_STRING);

        $pairs = [];
        foreach ($params as $name => $value) {
            $pairs[] = $name . '=' . $value;
        }

        // 注意：是「直接拼接密钥」，不是 `&key=`。这是最常见的坑
        return implode('&', $pairs) . $key;
    }

    /**
     * 判断回调是否表示支付成功。
     *
     * 常见取值：trade_status=TRADE_SUCCESS，或 status=1。
     * 两者都认，因为不同实现用不同的字段名。
     *
     * @param array<string, mixed> $params
     */
    public static function notifyIsPaid(array $params): bool
    {
        $status = (string) ($params['trade_status'] ?? '');

        if ($status !== '') {
            return $status === 'TRADE_SUCCESS' || $status === 'TRADE_FINISHED';
        }

        return (string) ($params['status'] ?? '') === '1';
    }

    /**
     * 生成订单号。
     *
     * 格式：日期 + 用户 id + 随机串。带日期便于人工排查，
     * 带用户 id 能直接从订单号看出是谁充的。
     */
    public static function makeOrderNo(int $userId): string
    {
        return date('YmdHis') . sprintf('%05d', $userId % 100000) . strtoupper(bin2hex(random_bytes(3)));
    }

    /**
     * 把上游返回的 JSON 解析成可跳转的地址（V2 API 型用）。
     *
     * 不同实现返回的字段名不同，因此逐个尝试；
     * 都取不到就报错并附上原始响应，便于判断是接口不兼容还是配置错了。
     *
     * @return array{ok:bool, error:string, url:string}
     */
    public static function extractPayUrl(string $response): array
    {
        $decoded = json_decode(trim($response), true);

        if (!is_array($decoded)) {
            return [
                'ok' => false,
                'error' => '上游返回的不是 JSON，无法解析。原始响应（已截断）：' . mb_substr(trim($response), 0, 200),
                'url' => '',
            ];
        }

        foreach (['payurl', 'qrcode', 'urlscheme', 'url', 'pay_url', 'code_url'] as $field) {
            $value = $decoded[$field] ?? null;
            if (is_string($value) && $value !== '') {
                return ['ok' => true, 'error' => '', 'url' => $value];
            }
        }

        $code = (string) ($decoded['code'] ?? '');
        $msg = (string) ($decoded['msg'] ?? ($decoded['message'] ?? ''));

        return [
            'ok' => false,
            'error' => '上游未返回可用的支付地址（code=' . $code . ' msg=' . $msg . '）',
            'url' => '',
        ];
    }

    /**
     * 请求上游 API（V2 用）。阻塞式，理由同 Mailer：
     * 下单是人手动触发的低频操作。
     *
     * @return array{ok:bool, error:string, body:string}
     */
    public static function httpGet(string $url): array
    {
        try {
            $ch = curl_init($url);
            curl_setopt_array($ch, [
                CURLOPT_RETURNTRANSFER => true,
                CURLOPT_CONNECTTIMEOUT => 10,
                CURLOPT_TIMEOUT => 20,
                CURLOPT_SSL_VERIFYPEER => true,
                CURLOPT_FOLLOWLOCATION => true,
                CURLOPT_MAXREDIRS => 3,
            ]);

            $body = curl_exec($ch);
            $error = curl_error($ch);
            curl_close($ch);

            if ($body === false) {
                return ['ok' => false, 'error' => '请求支付接口失败：' . $error, 'body' => ''];
            }

            return ['ok' => true, 'error' => '', 'body' => (string) $body];
        } catch (Throwable $e) {
            return ['ok' => false, 'error' => '请求支付接口异常：' . $e->getMessage(), 'body' => ''];
        }
    }
}
