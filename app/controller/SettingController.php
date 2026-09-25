<?php
/**
 * 后台 · 配置管理
 *
 * 路由：
 *   GET  /admin/settings         配置列表 + 编辑表单
 *   POST /admin/settings         保存全部修改
 *   POST /admin/settings/reset   恢复某一项为默认值
 *
 * 两条关键设计：
 *
 * 1. **只允许写入「已声明」的配置键**。
 *    保存时会拿配置键清单做白名单校验，未声明的键直接忽略。
 *    否则表单一旦被篡改，攻击者就能往 options 表里塞任意键值 ——
 *    虽然暂时读不到，但会污染数据、也可能被将来的功能意外读取。
 *
 * 2. **类型由「代码默认值」决定，而不是靠猜**。
 *    config/settings.php 里 `'secure' => true` 是布尔，那这个键就按布尔处理；
 *    是整数就按整数。这样表单控件类型、写入前的类型转换都不需要额外维护一张映射表，
 *    也不会出现「后台存了字符串 "0"，判断时被当成真」这类隐蔽 bug。
 *
 * 保存与恢复默认都采用 POST → 重定向 → GET，
 * 避免用户刷新页面导致重复提交（浏览器的「重新提交表单」弹窗）。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Crypto;
use app\common\Csrf;
use app\common\Mailer;
use app\common\Settings;
use support\Request;
use support\Response;

class SettingController
{
    /** 配置来源的中文说明 */
    private const SOURCE_LABELS = [
        'database' => '数据库（后台可改）',
        'env' => '环境变量 .env',
        'default' => '代码默认值',
        'none' => '未配置',
    ];

    /**
     * 分组的中文名与一句话说明。
     *
     * 分组由配置键的「点号前缀」决定（site.xxx 属于 site 组），
     * 因此新增配置项只要放进 config/settings.php 的合适分组，
     * 这里就会自动归类，不需要改代码。
     *
     * 未在此登记的前缀会以「其他」呈现 —— 这是刻意留的口子：
     * 手工写进数据库的自定义配置项也能被看到，而不是消失在页面上。
     */
    private const GROUP_LABELS = [
        'site' => ['label' => '站点', 'hint' => '站点名称、模式、公告、备案号等对外展示信息'],
        'gateway' => ['label' => '网关', 'hint' => '转发行为：超时分层、重试、SSE 心跳'],
        'key_pool' => ['label' => '密钥池', 'hint' => '多 Key 轮换的限流与失效治理策略'],
        'register' => ['label' => '注册', 'hint' => '下游用户的准入控制：是否开放注册、是否验证邮箱、邀请码、赠送额度'],
        'mail' => ['label' => '邮件', 'hint' => 'SMTP 发信配置，用于邮箱验证与找回密码'],
        'payment' => ['label' => '支付', 'hint' => '易支付 V1 / V2，用于用户在线充值'],
        'billing' => ['label' => '计费', 'hint' => '上游成本与下游售价的计算口径；逐个模型的价格在「模型定价」页配置'],
        'security' => ['label' => '安全', 'hint' => '后台登录防爆破与登录态有效期'],
        'session' => ['label' => '会话', 'hint' => '会话 Cookie 的安全属性'],
    ];

    /**
     * 枚举型配置的可选值。
     *
     * 为什么不让人直接手敲：这几个键的取值都是固定的英文标识，
     * 打错一个字母（比如 `starttls` 写成 `starttls ` 或 `STARTTLS`）
     * 症状是「邮件发不出去」「支付跳转失败」，而错误信息完全指不到拼写。
     * 做成下拉，从源头上消灭这类问题。
     */
    private const ENUM_OPTIONS = [
        'site.mode' => [
            // 刻意不用「商业站 / 公益站」这种划分说法：它只是「要不要向用户收费」的开关，
            // 说清楚它会改变什么，比贴一个标签有用
            'commercial' => '收费模式（显示定价、充值入口）',
            'public_welfare' => '免费模式（隐藏定价、充值入口）',
        ],
        'mail.secure' => [
            'ssl' => 'SSL（连接即加密，通常 465）',
            'tls' => 'STARTTLS（先明文再升级，通常 587）',
            'none' => '不加密（仅内网自建邮件服务）',
        ],
        'payment.gateway' => [
            'epay_v1' => '易支付 V1 · 页面跳转型',
            'epay_v2' => '易支付 V2 · API 型',
        ],
    ];

    /** Session 中存放提示信息的键 */
    private const FLASH_NOTICE = 'settings_notice';
    private const FLASH_TYPE = 'settings_notice_type';

    /**
     * GET /admin/settings —— 配置列表
     */
    public function index(Request $request): Response
    {
        return view('admin/settings', [
            'csrf'       => Csrf::token(),
            'siteName'   => Settings::siteName(),
            'siteMode'   => Settings::siteModeLabel(),
            'groups'     => $this->buildGroups(),
            // 配置之间的依赖检查：单项都合法、组合起来却是坏的，
            // 这类问题最难自己发现（比如「要求邮箱验证」+「没配邮件服务」
            // 会让所有新用户注册后卡在门外）
            'warnings'   => $this->configWarnings(),
            // pull 会读取并删除，保证提示只显示一次
            'notice'     => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
        ], '');
    }

    /**
     * 检查配置项之间的组合是否自相矛盾。
     *
     * 为什么要有这一块：单个配置项的取值都是合法的，
     * 但组合起来会产生一个谁也想不到的后果 —— 而站长在页面上
     * 逐个看过去是看不出来的（每一项都"正常"）。
     *
     * @return array<int, array{level:string, title:string, detail:string}>
     */
    private function configWarnings(): array
    {
        $warnings = [];

        $needVerify = Settings::bool('register.need_verify', true);
        $mailEnabled = Settings::bool('mail.enabled', false);
        $mailReady = Mailer::isConfigured();

        // ① 要求邮箱验证，却发不出邮件 → 用户注册完永远是「未验证」状态，
        //    既登录不了也没法自己解决
        if ($needVerify && (!$mailEnabled || !$mailReady)) {
            $warnings[] = [
                'level' => 'err',
                'title' => '注册流程可能是坏的：要求邮箱验证，但邮件发不出去',
                'detail' => $mailEnabled
                    ? '「邮件」组里还没有填齐 SMTP 服务器与发件人地址。'
                        . '此时新用户注册后会卡在「邮箱未验证」进不来。'
                    : '「邮件」组里的「是否启用发信」是关闭的。'
                        . '此时新用户注册后会卡在「邮箱未验证」进不来。'
                        . '（系统会临时放行，但这是不该依赖的兜底）',
            ];
        }

        // ② 做收件人探测却没有发件地址 → 探测会退化成只查 MX，
        //    挡不住「域名对但邮箱不存在」这类无效地址
        if (Settings::bool('register.verify_email', true)
            && Settings::bool('register.verify_smtp', true)
            && trim((string) Settings::get('mail.from', '')) === ''
        ) {
            $warnings[] = [
                'level' => 'warn',
                'title' => '邮箱真实性验证的强度被削弱了',
                'detail' => '收件人探测需要一个发件地址（SMTP 对话里的 MAIL FROM）。'
                    . '现在「邮件」组里的发件人地址是空的，探测会退化成只检查域名 MX —— '
                    . '能挡掉拼错的域名，但挡不住「域名存在而邮箱不存在」的地址。',
            ];
        }

        // ③ 开了收件人探测但本机 25 端口不通（云厂商常封）—— 这条只能提示，
        //    不能真去连一次（每打开一次配置页都探测会很慢也很失礼）
        if (Settings::bool('register.verify_email', true) && Settings::bool('register.verify_smtp', true)) {
            $warnings[] = [
                'level' => 'info',
                'title' => '收件人探测需要服务器能访问外部 25 端口',
                'detail' => '阿里云、腾讯云等厂商默认可能封禁 25 端口出站。'
                    . '若发现验证日志里大量出现「无法连接对方邮件服务器」，'
                    . '说明 25 端口不通 —— 此时请把「是否做收件人探测」关掉，'
                    . '前三级仍然有效（此时注册不会再被拖慢）。',
            ];
        }

        return $warnings;
    }

    /**
     * POST /admin/settings —— 保存全部修改
     */
    public function save(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $submitted = (array) $request->post('v', []);

        // 白名单：只接受已知的配置键
        $allowed = Settings::keys();

        // ── 先整体校验，有一处不合法就整体不写 ──
        // 为什么不「能存的先存」：配置项之间常有依赖（比如开了邮箱验证却没配
        // 邮件服务，用户就注册不进来）。存一半会留下一个自相矛盾的状态，
        // 比整个不存更难排查。
        $errors = [];

        foreach ($allowed as $key) {
            if (!array_key_exists($key, $submitted)) {
                continue;
            }

            $value = (string) $submitted[$key];

            // 枚举项：只接受清单内的取值
            if (isset(self::ENUM_OPTIONS[$key]) && $value !== '' && !isset(self::ENUM_OPTIONS[$key][$value])) {
                $errors[] = "「{$key}」的取值不在允许范围内";
            }

            // 凭据项：要写入就必须有 APP_KEY 才能加密
            if (Settings::isSecret($key) && $value !== '' && !Crypto::isConfigured()) {
                $errors[] = "未配置 APP_KEY，无法加密保存「{$key}」"
                    . '（生成方式：php -r "echo bin2hex(random_bytes(32));"，写入 .env 后重启服务）';
            }
        }

        if ($errors !== []) {
            return $this->back(implode('；', $errors), 'err');
        }

        // ── 再逐项写入 ──
        $saved = 0;

        foreach ($allowed as $key) {
            // 凭据：输入框留空表示「不修改」。
            // 后台从不回显凭据，所以「空的输入框」绝大多数情况是没动它；
            // 若按清空处理，用户改一次别的配置就会把凭据抹掉。
            // 想清空请点该行的「恢复默认」。
            if (Settings::isSecret($key)) {
                $plain = (string) ($submitted[$key] ?? '');
                if ($plain === '') {
                    continue;
                }

                Settings::putSecret($key, $plain);
                $saved++;
                continue;
            }

            $isBool = $this->typeOf($key) === 'bool';

            // 复选框未勾选时浏览器**不会提交该字段**，因此对布尔项来说
            // 「表单里有这一项、但没提交」等价于「用户把它关掉了」。
            // 不处理这一步，布尔配置一旦打开就永远关不掉。
            if ($isBool && !array_key_exists($key, $submitted)) {
                $new = false;
            } elseif (array_key_exists($key, $submitted)) {
                $new = $this->cast($key, $submitted[$key]);
            } else {
                // 非布尔项却没提交：说明表单被改过或字段缺失，跳过不动
                continue;
            }

            // ⚠️ 关键：只在「值确实变了」时才写库。
            //
            // 如果无脑把表单里所有字段都写进数据库，会有一个很隐蔽的副作用：
            // 本来从 .env 读取的配置被"物化"成数据库值之后，
            // 以后**再改 .env 就不会生效了**（因为数据库优先级高于环境变量）。
            // 站长会陷入「我明明改了 .env 怎么没用」的困惑，而且很难想到原因。
            // 因此这里做变更检测，未改动的项保持原样、不落库。
            $current = Settings::get($key);
            if ($new === $current) {
                continue;
            }

            Settings::put($key, $new);
            $saved++;
        }

        if ($saved === 0) {
            return $this->back('没有检测到任何改动', 'info');
        }

        return $this->back("已保存 {$saved} 项配置，将在此后数秒内于所有进程生效", 'ok');
    }

    /**
     * POST /admin/settings/reset —— 恢复某一项为默认值
     */
    public function reset(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $key = (string) $request->post('reset_key', '');

        if ($key === '' || !in_array($key, Settings::keys(), true)) {
            return $this->back('要恢复的配置项不存在', 'err');
        }

        Settings::reset($key);

        return $this->back("配置项「{$key}」已恢复默认（数据库覆盖值已删除）", 'ok');
    }

    /**
     * 按分组构造配置列表，供模板渲染。
     *
     * 每一项都带上：当前生效值、来源、类型、能否恢复默认。
     * 模板据此决定用复选框还是输入框、以及要不要显示「恢复默认」按钮。
     *
     * 分组的意义在于**可发现性**：配置项会越来越多，
     * 一长条平铺的表格会让人找不到东西，也让「这个项目到底能配什么」变得不可知。
     *
     * @return array<int, array{key:string, label:string, hint:string, items:array<int, array{key:string, display:string, raw:mixed, type:string, source:string, sourceLabel:string, canReset:bool}>}>
     */
    private function buildGroups(): array
    {
        $groups = [];

        foreach (Settings::keys() as $key) {
            $raw = Settings::get($key);
            $source = Settings::source($key);

            // 分组名 = 键的第一个点号前缀；没有点号则归入「其他」
            $prefix = str_contains($key, '.') ? substr($key, 0, strpos($key, '.')) : '_other';

            if (!isset($groups[$prefix])) {
                $meta = self::GROUP_LABELS[$prefix] ?? ['label' => '其他', 'hint' => '未归类的配置项'];
                $groups[$prefix] = [
                    'key' => $prefix,
                    'label' => $meta['label'],
                    'hint' => $meta['hint'],
                    'items' => [],
                ];
            }

            $groups[$prefix]['items'][] = [
                'key'         => $key,
                'raw'         => Settings::isSecret($key) ? '' : $raw,
                // 凭据从不回显：只告诉站长「配没配」
                'display'     => $this->displayValue($key, $raw),
                'type'        => $this->typeOf($key),
                'source'      => $source,
                'sourceLabel' => Settings::isSecret($key) && $source === 'database'
                    ? '数据库（已加密存储）'
                    : (self::SOURCE_LABELS[$source] ?? $source),
                // 枚举项的可选值，供模板渲染下拉
                'options'     => self::ENUM_OPTIONS[$key] ?? [],
                'configured'  => Settings::isSecret($key) ? Settings::hasSecret($key) : false,
                // 只有数据库里存在覆盖值时才谈得上「恢复默认」
                'canReset'    => $source === 'database',
            ];
        }

        return array_values($groups);
    }

    /**
     * 表单里显示的「值」。
     *
     * 凭据一律不回显：已配置显示空串 + 提示（模板据 configured 判断），
     * 未配置也是空串。这样即便页面被人看到、被截图、被浏览器缓存，
     * 也不会泄露任何口令。
     */
    private function displayValue(string $key, mixed $raw): string
    {
        if (Settings::isSecret($key)) {
            return '';
        }

        return $this->stringify($raw);
    }

    /**
     * 判定某个配置键的类型，依据是 config/settings.php 里的默认值。
     * 没有代码默认值的键（人工写进库的）按字符串处理。
     */
    private function typeOf(string $key): string
    {
        // 凭据与枚举优先判定：它们的控件类型不能由默认值的 PHP 类型推导
        if (Settings::isSecret($key)) {
            return 'secret';
        }

        if (isset(self::ENUM_OPTIONS[$key])) {
            return 'enum';
        }

        if (!Settings::hasCodeDefault($key)) {
            return 'string';
        }

        return match (true) {
            is_bool(Settings::codeDefault($key)) => 'bool',
            is_int(Settings::codeDefault($key)) => 'int',
            is_float(Settings::codeDefault($key)) => 'float',
            default => 'string',
        };
    }

    /**
     * 按类型把表单提交的字符串转成正确的 PHP 类型。
     * 表单传来的永远是字符串，不转换就会写出 "0" / "40" 这类值，
     * 后续布尔判断与数值比较都会出错。
     */
    private function cast(string $key, mixed $value): mixed
    {
        return match ($this->typeOf($key)) {
            'bool' => filter_var($value, FILTER_VALIDATE_BOOL),
            'int' => (int) $value,
            'float' => (float) $value,
            default => is_string($value) ? trim($value) : $value,
        };
    }

    /**
     * 把配置值转成便于阅读与编辑的字符串。
     */
    private function stringify(mixed $value): string
    {
        return match (true) {
            $value === null => '',
            is_bool($value) => $value ? '1' : '0',
            is_array($value) => (string) json_encode($value, JSON_UNESCAPED_UNICODE),
            default => (string) $value,
        };
    }

    /**
     * 写入提示并重定向回配置页（POST → 重定向 → GET）。
     */
    private function back(string $message, string $type): Response
    {
        session()->set(self::FLASH_NOTICE, $message);
        session()->set(self::FLASH_TYPE, $type);

        return response('', 302, ['Location' => '/admin/settings']);
    }
}
