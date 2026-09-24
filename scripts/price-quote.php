<?php
/**
 * 成本试算（命令行）
 *
 * 用途：在不发起任何真实请求的前提下，验证「这个模型这一次调用会花多少钱、
 * 收多少钱」，也可以用来核对定价表填得对不对。
 *
 * ═══ 为什么要有这个工具 ═══
 *
 * 计费逻辑一旦出错，症状是「账目对不上」而不是「页面报错」——
 * 这种问题在真跑起来之后极难定位。有一个能离线复算的工具，
 * 就能在上线前把公式与数据核对清楚。
 *
 * ═══ 用法 ═══
 *
 *   # 按模型名查定价并试算（输入 1200 token，输出 800 token）
 *   php scripts/price-quote.php --model=gpt-4o-mini --in=1200 --out=800
 *
 *   # 指定倍率覆盖（不影响数据库里的配置）
 *   php scripts/price-quote.php --model=gpt-4o-mini --in=1200 --out=800 --multiplier=1.5
 *
 *   # 列出所有已定价的模型及其参考毛利
 *   php scripts/price-quote.php --list
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

// 计费逻辑的唯一权威实现，必须复用而不是在脚本里另写一份公式 ——
// 一旦两处公式不一致，试算结果就失去了「验证」的意义
require aqua_root() . '/app/common/Pricing.php';

use app\common\Pricing;

$args = aqua_args();

/** 金额显示：去掉无意义的小数尾巴 */
$fmt = static function (float $v): string {
    return $v === 0.0 ? '0' : rtrim(rtrim(number_format($v, 10, '.', ''), '0'), '.');
};

/**
 * 脚本不加载框架，因此 Db 用不了 —— 这里直接用 PDO 读表。
 * 表结构由 Schema 建好，这里的查询只读取，不做任何写入。
 */
$pdo = aqua_pdo();

// ── --list：把所有定价和参考毛利列一遍 ─────────────────────
if (isset($args['list'])) {
    $rows = $pdo->query('SELECT * FROM pricing ORDER BY model ASC')->fetchAll(PDO::FETCH_ASSOC);

    if ($rows === []) {
        echo "定价表是空的。可以登录后台 →「模型定价」→「从渠道补齐」先把模型名导入。\n";
        exit(0);
    }

    printf("%-38s %-13s %-12s %14s %14s %8s\n", '模型', '计费模式', '上游种类', '上游成本', '下游售价', '参考倍率');
    echo str_repeat('-', 104) . "\n";

    foreach ($rows as $row) {
        // 参考量：一个计价单位的输入 + 一个计价单位的输出
        $unit = max(1, (int) $row['price_unit']);
        $quote = Pricing::quote($row, $unit, $unit, 1.0);

        $ratio = $quote['upstream_cost'] > 0
            ? number_format($quote['downstream_cost'] / $quote['upstream_cost'], 2) . 'x'
            : '—';

        printf(
            "%-38s %-13s %-12s %14s %14s %8s\n",
            mb_substr((string) $row['model'], 0, 36),
            (string) $row['billing_mode'],
            (string) $row['upstream_kind'],
            $fmt($quote['upstream_cost']),
            $fmt($quote['downstream_cost']),
            $ratio
        );
    }

    echo "\n参考量说明：以「1 个计价单位的输入 + 1 个计价单位的输出」试算，\n";
    echo "因此数字看起来比单次调用大得多（通常是「每百万 token」的报价）。\n";
    exit(0);
}

// ── 单模型试算 ─────────────────────────────────────────────
$model = trim($args['model'] ?? '');
if ($model === '') {
    fwrite(STDERR, "请用 --model=<模型名> 指定要试算的模型，或用 --list 列出全部定价。\n");
    exit(1);
}

$prompt = max(0, (int) ($args['in'] ?? 0));
$completion = max(0, (int) ($args['out'] ?? 0));
$multiplier = isset($args['multiplier']) ? (float) $args['multiplier'] : null;

$stmt = $pdo->prepare('SELECT * FROM pricing WHERE model = ?');
$stmt->execute([$model]);
$row = $stmt->fetch(PDO::FETCH_ASSOC);

// 倍率默认取配置里的 billing.default_multiplier，读不到就退回 1.0
if ($multiplier === null) {
    $raw = $pdo->prepare('SELECT opt_value FROM options WHERE opt_key = ?');
    $raw->execute(['billing.default_multiplier']);
    $value = $raw->fetchColumn();
    $decoded = $value === false ? null : json_decode((string) $value, true);
    $multiplier = is_numeric($decoded) ? (float) $decoded : 1.0;
}

$quote = Pricing::quote($row === false ? null : $row, $prompt, $completion, $multiplier);

echo "模型：$model\n";
echo "用量：输入 {$prompt} token，输出 {$completion} token\n";
echo "倍率：{$multiplier}（下游售价未单独配置时使用）\n";
echo str_repeat('-', 52) . "\n";

if (!$quote['priced']) {
    echo "定价：**未配置**（该模型在定价表里查不到，或已被停用）\n";
    echo "上游成本：0\n";
    echo "下游售价：0\n";
    echo "\n提示：可在后台 →「模型定价」新增该模型，或点「从渠道补齐」批量建行。\n";
    exit(0);
}

echo '计费模式：' . $quote['mode_label'] . "\n";
echo '计价单位：每 ' . number_format($quote['unit']) . " token\n";
echo '上游成本：' . $fmt($quote['upstream_cost']) . "\n";
echo '下游售价：' . $fmt($quote['downstream_cost']) . "\n";
echo '毛利：    ' . $fmt($quote['profit']) . "\n";

if ($quote['upstream_cost'] > 0) {
    echo '毛利倍率：' . number_format($quote['downstream_cost'] / $quote['upstream_cost'], 4) . "\n";
} else {
    echo "毛利倍率：无法计算（该模式下上游边际成本为 0）\n";
}
