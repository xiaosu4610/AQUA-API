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
use app\common\Timeouts;
use support\Log;
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
        'probe' => ['label' => '模型上架', 'hint' => '拉取上游模型清单与测活判定的口径：哪些模型才算「可上架」'],
        'gateway' => ['label' => '网关', 'hint' => '转发行为：超时分层、重试、SSE 心跳'],
        'key_pool' => ['label' => '密钥池', 'hint' => '多 Key 轮换的限流与失效治理策略'],
        'register' => ['label' => '注册', 'hint' => '下游用户的准入控制：是否开放注册、是否验证邮箱、邀请码、赠送额度'],
        'mail' => ['label' => '邮件', 'hint' => 'SMTP 发信配置，用于邮箱验证与找回密码'],
        'payment' => ['label' => '支付', 'hint' => '易支付 V1 / V2，用于用户在线充值'],
        'billing' => ['label' => '计费', 'hint' => '上游成本与下游售价的计算口径；逐个模型的价格在「模型定价」页配置'],
        'security' => ['label' => '安全', 'hint' => '后台登录防爆破与登录态有效期'],
        'users' => ['label' => '用户编号', 'hint' => '注销后把编号空位补齐，让编号始终连续（1、2、3…）'],
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
     * 配置项的中文名与说明（第一个元素是名字，第二个是「填错会怎样」的大白话）。
     *
     * 为什么必须补这一层：页面原来只显示 `billing.unpriced_is_free` 这种键名 ——
     * 那是给写代码的人看的。站长看到它只能猜，猜错就是把业务规则改坏，
     * 而且改坏了往往要过几天（用户来投诉）才发现。
     *
     * 写法要求（很重要，不是文风问题）：
     *   · 名字用**用户会说的话**，不用术语：「没有人订阅的超时」而不是「idle_timeout」
     *   · 说明必须回答「这是什么现象 / 填错会怎样」，而不只是重复名字
     *   · 保留英文键名显示在角落（模板里小字灰色），方便对照文档与 .env
     */
    private const KEY_LABELS = [
        // ── 站点 ──
        'site.name' => ['站点名称', '显示在浏览器标题、后台页头与首页上。改完立即生效，不影响已发出的邮件'],
        'site.domain' => ['站点域名', '可留空。填了会用于邮件里的链接与支付回调地址（例如 https://aqua.is3.cc）'],
        'site.mode' => ['是否向用户收费', '选「收费」会显示定价与充值入口；选「免费」会隐藏它们。页面上不会出现任何标签'],
        'site.description' => ['站点一句话介绍', '显示在首页副标题。留空则用内置文案'],
        'site.announcement' => ['站点公告', '填了会在首页与后台顶部显示一条横幅，适合通知维护、限流等临时情况'],
        'site.icp' => ['ICP 备案号', '境内服务器必须展示，否则有被通报的风险。例如 陕ICP备2025068157号-2'],
        'site.footer' => ['页脚署名', '显示在页脚。留空则显示开源项目名'],
        'site.show_models' => ['首页展示可用模型', '关闭后访客看不到任何模型清单，只看到登录入口。新站建议保持开启'],
        'site.demo_model' => ['首页示例用的模型名', '首页示例代码会用它。填一个你亲自验证过能调通的模型，否则访客照抄会报错'],

        // ── 模型上架 ──
        'probe.free_only' => [
            '只上架能真正调用的模型',
            '开着时：上游说「没有权限」的模型按「不是免费模型」处理 —— 不写进渠道清单、检测后自动下架。'
            . '超时或无法判定的模型仍然保留（免费模型只是慢，不是不能用）。关掉则拉到什么就上架什么',
        ],

        // ── 网关 ──
        'gateway.connect_timeout' => ['连接上游的超时（秒）', '只管「连上」这一步（TCP/TLS 握手）。连不上就换 Key/渠道，通常 5-10 秒足够'],
        'gateway.probe_timeout' => ['测活/拉清单的超时（秒）', '后台「测活」和「检测模型」单个请求的最长等待。调大能少误判慢模型，代价是测一遍更久'],
        'gateway.ttft_timeout' => ['首字节超时（秒）', '上游多久还不吐第一个字就算「没响应」，会触发换 Key 重试。免费上游冷启动慢，默认 300 秒；调小会让慢模型被误判成不可用'],
        'gateway.idle_timeout' => ['卡住超时（秒）', '两个数据块之间最长允许停多久。长回答不会因为总时长被掐，只会因为「卡住不动」被掐'],
        'gateway.total_timeout' => ['单请求总时长上限（秒）', '兜底值，防止一条连接被永久占住。默认 300 秒；填 0 表示不设上限（不推荐）'],
        'gateway.max_retries' => ['失败最多重试几次', '每次重试都会换一条渠道或换一把 Key，不是把同样的请求再发一遍。设 0 表示不重试'],
        'gateway.retry_status' => ['哪些错误码该换渠道重试', '逗号分隔。默认含 401/403（那把 Key 坏了，换一把常常立刻就好）与 429/5xx（上游抖了）'],
        'gateway.heartbeat_interval' => ['SSE 心跳间隔（秒）', '每隔这么久发一个心跳，防止 Nginx 等中间设备把长时间没数据的连接掐断'],

        // ── 密钥池 ──
        'key_pool.default_rpm' => ['单把 Key 每分钟上限', '渠道和 Key 都没单独设置时用这个值。NVIDIA 免费层官方口径约 40 次/分钟'],
        'key_pool.fail_threshold' => ['连续失败几次就停用该 Key', '只统计瞬时失败（429/5xx/网络抖动）。凭据失效（401/403）是立即停用，不看这个值'],
        'key_pool.auto_disable' => ['自动停用坏 Key', '关掉后只计数不停用。排查问题时可以关，但正常运营建议开着'],
        'key_pool.cooldown_seconds' => ['停用后多久自动恢复（秒）', '到期自动重新启用再给一次机会。设 0 表示不自动恢复，需要人工启用'],

        // ── 注册 ──
        'register.open' => ['开放注册', '关闭后注册页显示「暂未开放」，只能由你在后台手工开号'],
        'register.need_verify' => ['注册需要邮箱验证码', '开启后必须收到并填对邮件里的 6 位验证码才能注册成功。前提是下面的邮件服务已配好'],
        'register.invite_code' => ['邀请码', '非空时注册必须填对。内测或想控制入口时用它'],
        'register.verify_email' => ['注册前检查邮箱是否真实存在', '能挡掉乱填的地址。发信通道的退信率一高会被邮件商降级，所以这项值得开'],
        'register.verify_smtp' => ['连到对方邮件服务器确认收件人存在', '最准的一项，但需要服务器能访问外部 25 端口（云厂商常封）。封了就关掉，前三项检查仍然有效'],
        'register.block_disposable' => ['拒绝一次性/临时邮箱', '这类地址收得到信但用户拿不到，会拉高投诉率'],
        'register.extra_blocked_domains' => ['额外屏蔽的邮箱域名', '逗号或空格分隔。内置了一份常见一次性邮箱，这里放你自己遇到的'],
        'register.gift_balance' => ['注册赠送余额', '这是真金白银（能消耗你的上游额度）。开放注册时建议配合「新用户令牌额度」一起收紧'],
        'register.default_token_quota' => ['新用户令牌额度上限', '注册时自动建的那把令牌能消耗多少额度。0 表示不限（慎用）'],

        // ── 邮件 ──
        'mail.enabled' => ['启用发信', '关掉后注册验证码与找回密码都发不出邮件（会记日志），但流程不报错，容易让人以为「邮件在发」'],
        'mail.host' => ['SMTP 服务器', '例如 smtp.qiye.aliyun.com / smtp.qq.com / smtp.163.com'],
        'mail.port' => ['SMTP 端口', 'SSL 通常 465；STARTTLS 通常 587'],
        'mail.secure' => ['加密方式', '必须与端口匹配：465 配 SSL，587 配 STARTTLS。配错的表现是「一直连不上」'],
        'mail.username' => ['SMTP 登录账号', '通常就是发件邮箱地址，例如 acu@ltzy.top'],
        'mail.password' => ['SMTP 口令/授权码', '多数邮箱这里要填的是「授权码」而不是登录密码。此项加密存储，只显示「已配置」'],
        'mail.from' => ['发件人地址', '必须与上面的 SMTP 账号一致，否则会被判为伪造发件人而进垃圾箱'],
        'mail.from_name' => ['发件人显示名', '收件人看到的发件人名字。留空则用站点名称'],

        // ── 支付 ──
        'payment.enabled' => ['启用在线充值', '关闭后用户只能看余额，不能自助充值'],
        'payment.gateway' => ['支付接口类型', '不同接口要填的参数不一样，选好后下面会自动只显示它需要的项'],
        'payment.api_url' => ['支付网关地址', '只填到域名，不要带路径。例如 https://pay.example.com'],
        'payment.pid' => ['商户 PID', '支付平台分配给你的商户号'],
        'payment.key' => ['商户密钥', '支付平台给的密钥，用于签名校验。此项加密存储，只显示「已配置」'],
        'payment.submit_path' => ['接口路径（可选）', '留空用该类型的默认路径（V1 是 /submit.php，V2 是 /mapi.php）。对不上时在这里覆盖'],
        'payment.pay_types' => ['开放的支付方式', '逗号分隔：alipay（支付宝）/ wxpay（微信）/ qqpay（QQ）/ bank（网银）'],
        'payment.min_amount' => ['单笔最低充值金额', '低于这个数的充值请求会被拒'],
        'payment.credit_rate' => ['到账倍率', '充 1 元到账多少余额。做活动时调大即可，例如填 1.2 表示送 20%'],

        // ── 计费 ──
        'billing.currency' => ['货币单位', '只用于界面展示，不做汇率换算。要换币种请按统一口径重新填价格'],
        'billing.default_multiplier' => ['默认加价倍率', '模型只填了上游成本、没填售价时：售价 = 成本 × 这个倍率。必须写小数（1.0）'],
        'billing.estimate_ratio' => ['估算用量的校准系数', '上游不返回用量时按文本估算的补偿系数。估少了调大（1.2），估多了调小'],
        'billing.unpriced_is_free' => ['没有定价的模型免费放行', '开着：没配价格的模型可以调用、成本记 0（适合前期）。关掉：一律拒绝（适合已正式收费的站）'],
        'billing.require_balance' => ['余额为 0 时拒绝调用', '开着：没钱就不能用。关掉等于允许欠费使用，只适合免费站'],
        'billing.log_retention_days' => ['用量日志保留天数', '超期由维护脚本清理。日志表是唯一会无限增长的表，不建议设得过大'],

        // ── 安全 ──
        'security.login_max_attempts' => ['后台密码连续错几次锁定', '后台只有一个入口且不需要用户名，没有锁定等于把门敞开'],
        'security.login_lock_minutes' => ['后台锁定时长（分钟）', '锁满这么久后自动解锁'],
        'security.user_login_max_attempts' => ['用户密码连续错几次锁定', '用户是一个群体，阈值比后台宽松些，避免把记错密码的正常人挡在门外'],
        'security.user_login_lock_minutes' => ['用户锁定时长（分钟）', '锁满这么久后自动解锁'],
        'security.admin_session_minutes' => ['后台登录态有效期（分钟）', '超过后需要重新输密码。改完立即生效，不用重启'],

        // ── 用户编号 ──
        'users.auto_compact_ids' => ['自动补齐用户编号', '用户注销后编号会空出一位（1、3、5…）。开着就自动补齐成连续的 1..N。注意：补齐时所有人需重新登录一次'],
        'users.compact_interval_days' => ['每隔几天检查一次', '每次检查若发现空位才动手，没有空位就不打扰任何人'],

        // ── 会话 ──
        'session.secure' => ['会话 Cookie 仅走 HTTPS', '生产必须是「是」。本地用 http://127.0.0.1 调试时设为「否」，否则登录后会立刻掉线'],
    ];

    /**
     * 每种支付接口类型需要填哪些项，以及它「怎么走」。
     *
     * 为什么要有这张表：不同支付接口要的参数根本不一样 ——
     * 易支付只要「地址 + 商户号 + 密钥」，而将来的支付宝当面付需要
     * 「APPID + 应用私钥 + 支付宝公钥」，Stripe 又要「公钥 + 私钥 + Webhook 密钥」。
     * 把全部参数一次性摊在页面上，站长根本不知道哪些跟自己有关，
     * 于是要么乱填、要么去问人。所以：**只显示当前选中的那个类型需要的项**。
     *
     * fields 里没列到的键（enabled / gateway / min_amount 这类）对所有类型都通用，始终显示。
     */
    private const PAY_GATEWAY_FIELDS = [
        'epay_v1' => [
            'label' => '易支付 V1（页面跳转型）',
            'fields' => ['api_url', 'pid', 'key', 'submit_path'],
            'note' => '需要：支付站地址、商户 PID、商户密钥。默认走 /submit.php，'
                . '用户点充值后会跳到支付站的收银台页面。兼容性最好，新站建议先用它。',
        ],
        'epay_v2' => [
            'label' => '易支付 V2（API 型）',
            'fields' => ['api_url', 'pid', 'key', 'submit_path'],
            'note' => '需要：支付站地址、商户 PID、商户密钥。默认走 /mapi.php，'
                . '由本站请求接口拿到支付链接再跳转，适合想做自定义收银台的情况。'
                . '注意 V2 的签名规则与 V1 不同（是否保留空值），本站已按各自约定处理，'
                . '切换类型后请重新确认一遍密钥是否填对。',
        ],
    ];

    /**
     * 配置项的中文名 / 说明（供本页与仪表盘共用）。
     *
     * 放在这里而不是散在各页面：中文名是「这个键是说给人听的说法」，
     * 必须只有一处定义 —— 否则配置页说「首字节超时」、仪表盘说「等待首包」，
     * 站长会以为是两个不同的东西。
     */
    public static function labelOf(string $key): string
    {
        return self::KEY_LABELS[$key][0] ?? $key;
    }

    public static function hintOf(string $key): string
    {
        return self::KEY_LABELS[$key][1] ?? '';
    }

    /**
     * 枚举值的显示文案（`site.mode` 的 `commercial` → 「收费模式（…）」）。
     *
     * 仪表盘原来直接显示英文取值，站长看到 `commercial` 并不知道
     * 它到底改变了什么；这里统一换成配置页里那个说法。
     */
    public static function optionLabel(string $key, string $value): string
    {
        return self::ENUM_OPTIONS[$key][$value] ?? $value;
    }

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
            // 支付接口类型 → 它需要哪些项、怎么走。页面据此只显示当前类型要填的项
            'payNotes'   => self::PAY_GATEWAY_FIELDS,
            // 配置之间的依赖检查：单项都合法、组合起来却是坏的，
            // 这类问题最难自己发现（比如「要求邮箱验证」+「没配邮件服务」
            // 会让所有新用户注册后卡在门外）
            'warnings'   => $this->configWarnings(),
            // 超时专项卡片：这几个数字是站长最常调的（免费上游慢就要放宽），
            // 单独放一张卡片、配好范围和推荐值，比让他在几十项配置里翻要省事得多
            'timeouts'   => $this->timeoutCard(),
            // pull 会读取并删除，保证提示只显示一次
            'notice'     => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
        ], '');
    }

    /**
     * 超时卡片的渲染数据（键、中文名、说明、当前值、允许范围、推荐值）。
     *
     * @return array<int, array{key:string, label:string, hint:string, value:int, min:int, max:int, recommended:int}>
     */
    private function timeoutCard(): array
    {
        $rows = [];
        $current = Timeouts::all();
        $fields = array_flip(Timeouts::FORM_NAMES);   // 配置键 => 表单短名

        foreach (Timeouts::DEFAULTS as $key => $default) {
            [$min, $max] = Timeouts::RANGE[$key] ?? [1, 3600];
            $rows[] = [
                'key' => $key,
                'field' => (string) ($fields[$key] ?? $key),
                'label' => self::labelOf($key),
                'hint' => self::hintOf($key),
                'value' => $current[$key] ?? $default,
                'min' => $min,
                'max' => $max,
                'recommended' => Timeouts::RECOMMENDED[$key] ?? $default,
            ];
        }

        return $rows;
    }

    /**
     * POST /admin/settings/timeouts —— 只保存超时这几项
     *
     * ═══ 为什么要单独一个接口，而不是复用 /admin/settings ═══
     *
     * 主配置表单是「整页一起提交」的，而它把「布尔项没出现在表单里」解释成
     * 「用户把它关掉了」（未勾选的复选框不会出现在请求里）——
     * 所以拿一个只含超时字段的表单去提交主接口，会**顺手关掉一堆开关**。
     * 这个坑很隐蔽：站长只想改个超时，结果邮件、注册、支付开关全被关掉。
     *
     * 因此这里是独立接口，只认这几个键、只写这几个键，别的一律不碰。
     *
     * ⚠️ 字段名用短名（`t[ttft]`），不用配置键本身（`t[gateway.ttft_timeout]`）：
     * 配置键带点号，而 PHP 会改写变量名里的点号，这类字段名很容易在某次
     * 调整里悄悄失效，症状是「点保存没反应」。见 Timeouts::FORM_NAMES。
     */
    public function saveTimeouts(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $submitted = (array) $request->post('t', []);
        $errors = [];
        $values = [];

        foreach (Timeouts::FORM_NAMES as $field => $key) {
            if (!array_key_exists($field, $submitted)) {
                // 没提交这一项：跳过（不做「缺失即清零」这种危险推断）
                continue;
            }

            $raw = trim((string) $submitted[$field]);
            if ($raw === '' || !preg_match('/^-?\d+$/', $raw)) {
                $errors[] = '「' . self::labelOf($key) . '」要填整数秒';
                continue;
            }

            $value = (int) $raw;
            [$min, $max] = Timeouts::RANGE[$key] ?? [1, 3600];

            if ($value < $min || $value > $max) {
                $errors[] = '「' . self::labelOf($key) . "」要在 {$min}-{$max} 秒之间";
                continue;
            }

            $values[$key] = $value;
        }

        // ⚠️ 顺序很重要：必须先报错、再判空。
        // 反过来的话，任何一项校验失败都会走进「没有要保存的超时项」，
        // 站长看到的是「我明明填了值，它说没有」—— 报错信息被自己吞掉了
        if ($errors !== []) {
            return $this->back(implode('；', $errors), 'err');
        }

        if ($values === []) {
            return $this->back('没有要保存的超时项', 'info');
        }

        // ── 组合校验：这几个值之间有包含关系，单个合法、组合起来荒谬的情况必须挡住 ──
        // total = 0 表示「不设总时长上限」，此时「谁比总时长大」无从谈起，跳过比较
        $total = $values['gateway.total_timeout'] ?? Timeouts::total();

        if ($total > 0) {
            foreach (['gateway.connect_timeout', 'gateway.ttft_timeout', 'gateway.idle_timeout'] as $key) {
                if (isset($values[$key]) && $values[$key] > $total) {
                    $errors[] = '「' . self::labelOf($key) . '」不能大于「' . self::labelOf('gateway.total_timeout')
                        . "」（{$values[$key]} > {$total}）：总时长一到就结束了，比它还长的等待永远不会生效";
                }
            }
        }

        if ($errors !== []) {
            return $this->back(implode('；', $errors), 'err');
        }

        $saved = 0;
        foreach ($values as $key => $value) {
            if (Settings::get($key) === $value) {
                continue;   // 没变就不写库，避免把 .env 的值「物化」进数据库
            }
            Settings::put($key, $value);
            $saved++;
        }

        if ($saved === 0) {
            return $this->back('超时没有任何改动', 'info');
        }

        Log::info('管理员调整了请求超时：' . json_encode($values, JSON_UNESCAPED_UNICODE));

        return $this->back("已保存 {$saved} 项超时设置，将在此后数秒内于所有进程生效", 'ok');
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

            $item = [
                'key'         => $key,
                // 中文名 + 大白话说明（缺失时退回键名，至少不会显示成空白）
                'label'       => self::labelOf($key),
                'hint'        => self::hintOf($key),
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
                // 这一项属于哪些支付接口类型（空数组 = 对所有类型通用，始终显示）。
                // 模板据此在行上打 data-gateways，切换接口类型时由一小段 JS 收起不相关的行
                'gateways'    => self::gatewaysUsing($key),
            ];

            $groups[$prefix]['items'][] = $item;
        }

        return array_values($groups);
    }

    /**
     * 某个配置键被哪些支付接口类型需要（空数组 = 通用项）。
     *
     * @return array<int, string>
     */
    private static function gatewaysUsing(string $key): array
    {
        if (!str_starts_with($key, 'payment.')) {
            return [];
        }

        $field = substr($key, strlen('payment.'));
        $used = [];
        foreach (self::PAY_GATEWAY_FIELDS as $gateway => $meta) {
            if (in_array($field, $meta['fields'], true)) {
                $used[] = (string) $gateway;
            }
        }

        return $used;
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
