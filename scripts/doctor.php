<?php
/**
 * 全链路自检（只读）
 *
 * 回答站长最常问的那个问题：**「为什么用户调用接口说 401？」**
 *
 * ═══ 为什么需要它 ═══
 *
 * 一次调用能不能成功，取决于一串条件：令牌存在 → 令牌启用 → 没过期 →
 * 额度没用完 → 所属用户存在 → 用户启用 → 余额够（取决于开关）→
 * 有可用渠道 → 渠道有可用 Key → 该模型有定价。
 * 任何一环不满足，用户看到的都只是一句「401 / API Key 错误」。
 * 让站长挨个去后台页面翻这八项，是件很折磨人的事 —— 这个脚本一次全查完，
 * 并直接指出**卡在哪一环、怎么改**。
 *
 * ═══ 只读 ═══
 *
 * 本脚本不改任何数据。它出现在生产上的合理性就在于此：
 * 排查问题时最怕「诊断动作本身把现场改了」。
 *
 * 用法：
 *   sudo -u www-data php scripts/doctor.php
 *   sudo -u www-data php scripts/doctor.php --tokens        # 逐令牌体检（令牌多时才需要）
 *   sudo -u www-data php scripts/doctor.php --verbose       # 连各渠道明细一起打
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

$args = aqua_args();
$showTokens = isset($args['tokens']);
$verbose = isset($args['verbose']);

$pdo = aqua_pdo();
$dialect = (string) $pdo->getAttribute(PDO::ATTR_DRIVER_NAME);

/**
 * 读 options 表里的配置（后台改过的值都在这里）。
 * 找不到就用代码里的默认值 —— 默认值与 config/settings.php 必须一致，
 * 改了那边记得一起改（这是本脚本唯一的重复点）。
 *
 * @return array<string, mixed>
 */
function doctor_settings(PDO $pdo): array
{
    $map = [];
    try {
        foreach ($pdo->query('SELECT opt_key, opt_value FROM options') as $row) {
            $map[(string) $row['opt_key']] = json_decode((string) $row['opt_value'], true);
        }
    } catch (Throwable $e) {
        // options 表不存在（没装好）时不能崩，交给后面的检查去报
    }

    return $map;
}

/**
 * 取配置值（数据库优先，其次代码默认值）。
 */
function doctor_cfg(array $settings, string $key, mixed $default = null): mixed
{
    return array_key_exists($key, $settings) ? $settings[$key] : $default;
}

function doctor_count(PDO $pdo, string $sql, array $params = []): int
{
    try {
        $stmt = $pdo->prepare($sql);
        $stmt->execute($params);

        return (int) $stmt->fetchColumn();
    } catch (Throwable $e) {
        return -1;
    }
}

/** @return array<int, array<string, mixed>> */
function doctor_all(PDO $pdo, string $sql, array $params = []): array
{
    try {
        $stmt = $pdo->prepare($sql);
        $stmt->execute($params);

        return $stmt->fetchAll(PDO::FETCH_ASSOC) ?: [];
    } catch (Throwable $e) {
        return [];
    }
}

$settings = doctor_settings($pdo);
$problems = [];   // 会**直接导致调用失败**的问题
$cautions = [];   // 不致命但值得知道的事

echo "════════════════════════════════════════════════════════\n";
echo "全链路自检\n";
echo "════════════════════════════════════════════════════════\n";

// ═══════════════════════════════════════════════════════════
// 一、环境
// ═══════════════════════════════════════════════════════════
echo "\n【一】环境\n";
echo '  PHP 版本          ' . PHP_VERSION . "\n";
echo '  数据库            ' . $dialect . "\n";
echo '  schema 版本       ' . (string) doctor_cfg($settings, 'schema_version', '(未记录)') . "\n";
echo '  APP_KEY           ' . (aqua_env('APP_KEY') !== '' ? '已配置' : '**未配置**') . "\n";
if (aqua_env('APP_KEY') === '') {
    $problems[] = 'APP_KEY 未配置：渠道的 API Key 无法解密，转发时发不出请求（用户侧表现为 500/502）';
}

$siteDomain = trim((string) doctor_cfg($settings, 'site.domain', ''));
echo '  站点域名          ' . ($siteDomain === '' ? '(未配置，用请求 Host)' : $siteDomain) . "\n";

// ═══════════════════════════════════════════════════════════
// 二、上游（渠道 / 密钥 / 模型 / 定价）
// ═══════════════════════════════════════════════════════════
echo "\n【二】上游\n";

$channelTotal = doctor_count($pdo, 'SELECT COUNT(*) FROM channels');
$channelOn = doctor_count($pdo, 'SELECT COUNT(*) FROM channels WHERE status = 1');
$keyTotal = doctor_count($pdo, 'SELECT COUNT(*) FROM channel_keys');
$keyOn = doctor_count($pdo, 'SELECT COUNT(*) FROM channel_keys WHERE status = 1');

echo '  渠道              ' . $channelOn . ' 启用 / 共 ' . $channelTotal . "\n";
echo '  上游密钥          ' . $keyOn . ' 启用 / 共 ' . $keyTotal . "\n";

if ($channelOn <= 0) {
    $problems[] = '没有任何**启用**的渠道：请求无处可转，用户会收到「无可用渠道」类错误';
} elseif ($keyOn <= 0) {
    $problems[] = '没有可用的上游密钥（密钥池为空或全部停用）：转发会立刻失败';
}

// 模型总数（各渠道模型清单去重）
$modelSet = [];
foreach (doctor_all($pdo, 'SELECT name, models, status, breaker_until, last_error FROM channels ORDER BY id') as $channel) {
    if ((int) $channel['status'] !== 1) {
        continue;
    }
    $models = json_decode((string) $channel['models'], true) ?: [];
    foreach ((array) $models as $model) {
        $modelSet[(string) $model] = true;
    }
    if (!empty($channel['breaker_until']) && (int) $channel['breaker_until'] > time()) {
        $cautions[] = sprintf(
            '渠道「%s」处于熔断中，解禁时间 %s（原因：%s）',
            (string) $channel['name'],
            date('Y-m-d H:i', (int) $channel['breaker_until']),
            mb_substr((string) ($channel['last_error'] ?? ''), 0, 60)
        );
    }
}
echo '  可用模型（去重）  ' . count($modelSet) . "\n";
if ($modelSet === []) {
    $problems[] = '启用渠道里没有任何模型清单：用户调什么都会 404「模型不存在」';
}

$pricingTotal = doctor_count($pdo, 'SELECT COUNT(*) FROM pricing');
$pricingFree = doctor_count($pdo, "SELECT COUNT(*) FROM pricing WHERE billing_mode = 'free'");
$pricingPaid = max(0, $pricingTotal - $pricingFree);
echo '  定价条目          ' . $pricingTotal . '（其中免费 ' . $pricingFree . "）\n";
echo '  未定价模型        ' . max(0, count($modelSet) - $pricingTotal) . " 个（按 billing.unpriced_is_free 决定是否扣费）\n";

// 「还有没有要花钱的模型」——余额门槛只对它们有意义。
// 全站免费时，余额为 0 完全不影响调用，诊断里就不该再拿余额说事，
// 否则会把「一切正常」说成「大家都只能调免费模型」，等于自己制造恐慌
$unpricedIsFree = (bool) doctor_cfg($settings, 'billing.unpriced_is_free', true);
$chargeable = $pricingPaid + ($unpricedIsFree ? 0 : max(0, count($modelSet) - $pricingTotal));
echo '  其中收费模型      ' . $chargeable . " 个（只有这些受余额限制）\n";

// ═══════════════════════════════════════════════════════════
// 三、下游（用户 / 令牌）
// ═══════════════════════════════════════════════════════════
echo "\n【三】下游\n";

$userTotal = doctor_count($pdo, 'SELECT COUNT(*) FROM users');
$userOn = doctor_count($pdo, 'SELECT COUNT(*) FROM users WHERE status = 1');
$userZero = doctor_count($pdo, 'SELECT COUNT(*) FROM users WHERE status = 1 AND balance <= 0');
$tokenTotal = doctor_count($pdo, 'SELECT COUNT(*) FROM tokens');
$tokenOn = doctor_count($pdo, 'SELECT COUNT(*) FROM tokens WHERE status = 1');

echo '  用户              ' . $userOn . ' 启用 / 共 ' . $userTotal . "\n";
echo '  余额为 0 的用户   ' . $userZero . "\n";
echo '  令牌              ' . $tokenOn . ' 启用 / 共 ' . $tokenTotal . "\n";

$requireBalance = (bool) doctor_cfg($settings, 'billing.require_balance', true);
echo '  余额为 0 时拒绝   ' . ($requireBalance ? '是（billing.require_balance=true）' : '否（允许欠费使用）') . "\n";

if ($tokenTotal === 0) {
    $problems[] = '库里一把令牌都没有：用户必须先到控制台创建令牌，才能调用';
}

// ═══════════════════════════════════════════════════════════
// 四、关键开关
// ═══════════════════════════════════════════════════════════
echo "\n【四】关键开关\n";

$switches = [
    'site.mode' => ['站点是否收费', 'commercial'],
    'register.open' => ['开放注册', false],
    'register.need_verify' => ['注册需邮箱验证码', true],
    'register.gift_balance' => ['注册赠送余额', 0],
    'billing.require_balance' => ['余额为 0 时拒绝调用', true],
    'billing.unpriced_is_free' => ['未定价模型按免费处理', true],
    'mail.enabled' => ['邮件服务已启用', false],
    'gateway.ttft_timeout' => ['首字节超时（秒）', 30],
    'gateway.total_timeout' => ['单请求总时长上限（秒）', 600],
    'probe.free_only' => ['只上架能真正调用的模型', true],
    'users.auto_compact_ids' => ['自动补齐用户编号', true],
];
foreach ($switches as $key => [$label, $default]) {
    $value = doctor_cfg($settings, $key, $default);
    $text = is_bool($value) ? ($value ? '开' : '关') : (string) $value;
    printf("  %-30s %s\n", $label . '（' . $key . '）', $text);
}

// 「收费模式 + 余额为 0 时拒绝 + 注册不送余额」= 谁都调不了，
// 这是最容易配错、也最难自查的一组组合，单独点出来
$mode = (string) doctor_cfg($settings, 'site.mode', 'commercial');
$gift = (float) doctor_cfg($settings, 'register.gift_balance', 0);
if ($mode === 'commercial' && $requireBalance && $gift <= 0 && $chargeable > 0) {
    $cautions[] = '当前是「收费模式 + 余额为 0 时拒绝调用 + 注册不赠送余额 + 站内有收费模型」：'
        . '新用户注册后余额为 0，调那些收费模型会拿到 402「余额不足」'
        . '（状态码已与 401 分开，不会再让人以为是密钥问题）。'
        . '要么给注册赠送额度（register.gift_balance）、要么手工给用户充值，'
        . '要么把收费模型设为免费，要么把「余额为 0 时拒绝调用」关掉';
}

if ((bool) doctor_cfg($settings, 'register.need_verify', true) && !(bool) doctor_cfg($settings, 'mail.enabled', false)) {
    $cautions[] = '要求邮箱验证但邮件服务未启用：新用户注册时会被直接放行（这是兜底，不是故障），'
        . '但「找回密码」不可用';
}

// ═══════════════════════════════════════════════════════════
// 五、逐令牌体检 —— 直接回答「用户的 Key 到底能不能用」
// ═══════════════════════════════════════════════════════════
echo "\n【五】令牌体检（只列会被拒绝的）\n";

$tokenRows = doctor_all(
    $pdo,
    'SELECT t.id, t.name, t.key_mask, t.status, t.expires_at, t.quota_limit, t.quota_used, t.user_id,
            u.email, u.status AS user_status, u.balance
     FROM tokens t LEFT JOIN users u ON u.id = t.user_id
     ORDER BY t.id'
);

$blocked = [];      // 一定会被拒（令牌/账号本身的问题）
$conditional = [];  // 只在调「收费模型」时被拒（余额问题）
$healthy = 0;
foreach ($tokenRows as $row) {
    $fatal = [];
    $limited = [];

    if ((int) $row['status'] !== 1) {
        $fatal[] = '令牌已停用';
    }
    if ((string) ($row['expires_at'] ?? '') !== '' && (int) $row['expires_at'] > 0 && (int) $row['expires_at'] <= time()) {
        $fatal[] = '令牌已过期（' . date('Y-m-d H:i', (int) $row['expires_at']) . '）';
    }
    if ((float) $row['quota_limit'] > 0 && (float) $row['quota_used'] >= (float) $row['quota_limit']) {
        $fatal[] = '令牌额度已用完（' . $row['quota_used'] . '/' . $row['quota_limit'] . '）';
    }
    if ($row['email'] === null) {
        $fatal[] = '所属用户不存在（user_id=' . (int) $row['user_id'] . '）';
    } else {
        if ((int) $row['user_status'] !== 1) {
            $fatal[] = '所属账号已被停用';
        }
        // ⚠️ 余额为 0 **不是**「令牌一定不能用」：免费/未定价模型照样能调。
        // 而且只有**站内还存在收费模型**时，余额才可能挡住调用 ——
        // 全站免费时余额完全不影响，诊断里提它只会造成恐慌
        if ($requireBalance && $chargeable > 0 && (float) $row['balance'] <= 0) {
            $limited[] = '账号余额为 ' . (float) $row['balance'] . '，调收费模型会判 402「余额不足」';
        }
    }

    if ($fatal === [] && $limited === []) {
        $healthy++;
        if ($showTokens) {
            printf("  [可用] #%d %s %s（%s）\n", (int) $row['id'], (string) $row['key_mask'], (string) $row['email'], (string) $row['name']);
        }
        continue;
    }

    $item = ['id' => (int) $row['id'], 'mask' => (string) $row['key_mask'], 'email' => (string) ($row['email'] ?? '?')];

    if ($fatal !== []) {
        $blocked[] = $item + ['reasons' => $fatal];
        continue;
    }

    $conditional[] = $item + ['reasons' => $limited];
}

echo '  完全可用 ' . $healthy . ' 把；令牌本身有问题（一定被拒）' . count($blocked)
    . ' 把；只能调免费模型 ' . count($conditional) . " 把\n";

foreach (array_slice($blocked, 0, 20) as $item) {
    printf("  [一定被拒] #%d %s %s → %s\n", $item['id'], $item['mask'], $item['email'], implode('；', $item['reasons']));
}
foreach (array_slice($conditional, 0, 20) as $item) {
    printf("  [仅收费模型被拒] #%d %s %s → %s\n", $item['id'], $item['mask'], $item['email'], implode('；', $item['reasons']));
}
if (count($blocked) + count($conditional) > 40) {
    echo '  …还有更多（用 --tokens 看全部）' . "\n";
}

if ($blocked !== [] && count($blocked) === $tokenTotal) {
    $problems[] = '**所有令牌都一定被拒** —— 逐条原因见上方「[一定被拒]」列表';
}

// 余额为 0 且开关开着、且站内确实还有收费模型：这是最常见的「明明 Key 没问题却调不通」
if ($requireBalance && $chargeable > 0 && $userZero > 0 && $userOn > 0) {
    $cautions[] = sprintf(
        '有 %d 个启用用户的余额为 0，且 billing.require_balance=true，站内还有 %d 个收费模型：'
        . '这些账号调那些收费模型会拿到 402「余额不足」（令牌本身没问题）。'
        . '三条路：把模型设为免费（定价页有一键「全部设为免费」）、'
        . '给用户充值或设注册赠送额度（register.gift_balance）、'
        . '或把「余额为 0 时拒绝调用」关掉（适合免费站）',
        $userZero,
        $chargeable
    );
}

// ═══════════════════════════════════════════════════════════
// 六、结论
// ═══════════════════════════════════════════════════════════
echo "\n【六】结论\n";

if ($problems === [] && $cautions === []) {
    echo "  一切正常：渠道、密钥、模型、定价、用户、令牌都在可用状态，可以正常调用。\n";
    echo "  用户侧接入地址与示例见：/docs\n";
} else {
    if ($problems === []) {
        echo "  没有致命问题。但下面这些会造成「接口用不了」的观感，建议处理：\n";
    }
    foreach ($problems as $index => $text) {
        echo '  [阻塞 ' . ($index + 1) . '] ' . $text . "\n";
    }
    foreach ($cautions as $index => $text) {
        echo '  [注意 ' . ($index + 1) . '] ' . $text . "\n";
    }
}

echo "\n";
exit($problems === [] ? 0 : 1);
