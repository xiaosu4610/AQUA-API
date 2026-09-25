<?php
/**
 * 上线两条付费专线：硅基流动 / TierFlow。
 *
 * ═══ 这个脚本解决什么 ═══
 *
 * 站长给了两家的密钥（硅基流动 2 把 × 16 元、TierFlow 2 把 × 74 元），
 * 要求「临时免费放出来跑个量」，同时「余额用完自动停用密钥并下架」。
 * 这需要在库里落三样东西：渠道（线路 + 密钥池）、定价（官方原价）、分组归属。
 * 手点后台要建 2 条渠道、4 把密钥、5 条定价，且任何一处填错都会让成本对不上 ——
 * 所以做成脚本：**可重复执行、每次执行都把配置拉回同一状态**，
 * 出错了再跑一次即可，不需要「猜上次哪一步没做」。
 *
 * ═══ 收费模型为什么用 aqua/ 前缀 ═══
 *
 * 站长要求这几个收费模型用专用模型 ID（`aqua/模型名`），这是**临时口径**：
 * 一方面把它们与免费线路上可能同名的模型区分开，另一方面将来要做更完整的
 * 模型治理时好一次性换掉。实现方式是「对外名声 + model_map 翻译」——
 * 渠道的模型清单里写 `aqua/GLM-5.3`，转发时按 model_map 换成上游真实名 `GLM-5.3`，
 * 用户侧始终只用 `aqua/...`，上游侧看到的仍是它认识的名字。
 *
 * ═══ 用法 ═══
 *
 *     php scripts/launch-paid-lines.php --dry-run     # 只打印准备做什么，不写库
 *     php scripts/launch-paid-lines.php               # 真正执行（含「放开使用」）
 *     php scripts/launch-paid-lines.php --keep-hidden # 建线路但不放开（只给管理员看）
 *
 * ⚠️ 执行完必须重启服务：webman 是常驻内存的，进程里缓存着分组与渠道。
 *    脚本会自己打印这句提醒。
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

$args = aqua_args();
$dryRun = isset($args['dry-run']);
$keepHidden = isset($args['keep-hidden']);

$pdo = aqua_pdo();
$appKey = aqua_require_app_key();

// 分组表是 Schema v15 建的。表不在就说明服务还没用新版本启动过 ——
// 与其让脚本抛一个 SQL 报错，不如直说该做什么
try {
    $pdo->query('SELECT 1 FROM line_groups LIMIT 1');
} catch (PDOException) {
    fwrite(STDERR, "数据库里还没有分组表（数据库结构未升级到 v15）。\n");
    fwrite(STDERR, "请先用新版本启动一次服务，让它完成建表：\n");
    fwrite(STDERR, "    sudo systemctl restart aqua-api\n");
    exit(1);
}

// ═══════════════════════════════════════════════════════════
// 线路定义（这里是唯一的真相源）
// ═══════════════════════════════════════════════════════════
//
// 价格口径：站长原话「按照官方原版价格计算」，即上游成本与对外售价都用官方价。
// 线路本身设成「对用户临时免费」（groups.price_mode = free），
// 所以这些数字现在**不向用户收费**，只用于算「这条线在替我烧多少钱」。

$lines = [
    [
        'group' => 'siliconflow',
        'groupLabel' => '高速稳定专线 · 硅基流动',
        'name' => '高速稳定专线 · 硅基流动',
        'baseUrl' => 'https://api.siliconflow.cn/v1',
        'note' => '硅基流动（官方直连）',
        'keys' => [
            ['sk-qpyvctolijqiidwvpnrvnulkwjcokcsuvnddukoomdimofml', 16.00],
            ['sk-tpynivuuwqgbqttkvuvgtytdxyktygzsgueaaietktdqalan', 16.00],
        ],
        'models' => [
            [
                'name' => 'aqua/DeepSeek-V4-Flash',
                'upstream' => 'deepseek-ai/DeepSeek-V4-Flash',
                'input' => 3.0,
                'output' => 9.0,
                'cacheHit' => 0.3,
                // 站长给的价目表：02:00–08:00 谷时半价，其余时段是原价两倍
                'windows' => [
                    ['from' => '02:00', 'to' => '08:00', 'input' => 1.5, 'output' => 4.5, 'cache_hit' => 0.15],
                    ['from' => '00:00', 'to' => '24:00', 'input' => 3.0, 'output' => 9.0, 'cache_hit' => 0.3],
                ],
                'note' => '硅基流动官方价（含 02:00–08:00 谷时价）',
            ],
        ],
    ],
    [
        'group' => 'tierflow',
        'groupLabel' => '高速稳定专线 · TierFlow',
        'name' => '高速稳定专线 · TierFlow',
        'baseUrl' => 'https://tierflow.cn/v1',
        'note' => 'TierFlow（北京清枢智汇，按官方原价扣额度）',
        'keys' => [
            ['sk-jq5W7HhCAkPgNlvSaRmP1TcZE6MSJvmxutyOag28psCyF2tV', 74.00],
            ['sk-4OjRhN98CzU0MvvBvQPe8aqwu0Eg6IhSGPiTvPArQiqNEVEI', 74.00],
        ],
        'models' => [
            ['name' => 'aqua/GLM-5.3', 'upstream' => 'GLM-5.3', 'input' => 8.0, 'output' => 28.0, 'cacheHit' => 0, 'note' => 'TierFlow 官方价'],
            ['name' => 'aqua/GLM-5.3-Flash', 'upstream' => 'GLM-5.3-Flash', 'input' => 0.8, 'output' => 2.8, 'cacheHit' => 0, 'note' => 'TierFlow 官方价'],
            // ⚠️ FlashX 在 TierFlow 的公开价目里没有，这里**暂按 Flash 同价**记成本。
            //    备注已写明「待确认」，拿到报价后在 后台 → 模型定价 里改一个数即可
            ['name' => 'aqua/GLM-5.3-FlashX', 'upstream' => 'GLM-5.3-FlashX', 'input' => 0.8, 'output' => 2.8, 'cacheHit' => 0, 'note' => 'TierFlow 官方价目里没有这个型号，暂按 GLM-5.3-Flash 同价，待站长确认'],
            ['name' => 'aqua/Qwen3.8-Flash', 'upstream' => 'Qwen3.8-Flash', 'input' => 0.8, 'output' => 2.7, 'cacheHit' => 0, 'note' => 'TierFlow 官方价'],
        ],
    ],
];

$now = time();
$log = static function (string $line) use ($dryRun): void {
    echo ($dryRun ? '[预演] ' : '') . $line . "\n";
};

// ═══════════════════════════════════════════════════════════
// 一、分组
// ═══════════════════════════════════════════════════════════

echo "═══ 一、线路分组 ═══\n";

foreach ($lines as $line) {
    $row = $pdo->query("SELECT * FROM line_groups WHERE code = " . $pdo->quote($line['group']))->fetch(PDO::FETCH_ASSOC);

    if ($row === false) {
        if (!$dryRun) {
            $stmt = $pdo->prepare(
                'INSERT INTO line_groups (code, label, description, cost_mode, price_mode, visible, default_visible, sort, status, created_at, updated_at)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)'
            );
            $stmt->execute([
                $line['group'],
                $line['groupLabel'],
                $line['note'] . ' · 临时免费线路',
                'paid',
                'free',
                $keepHidden ? 0 : 1,
                $keepHidden ? 0 : 1,
                $line['group'] === 'siliconflow' ? 30 : 40,
                $now,
                $now,
            ]);
        }
        $log("新建分组「{$line['groupLabel']}」（临时免费，上游花钱）");
        continue;
    }

    // 已存在：只把它拉回该有的状态，不动它的 code
    if (!$dryRun) {
        $stmt = $pdo->prepare(
            'UPDATE line_groups SET cost_mode = ?, price_mode = ?, visible = ?, default_visible = ?, status = 1, updated_at = ? WHERE id = ?'
        );
        $stmt->execute([
            'paid',
            'free',
            $keepHidden ? 0 : 1,
            $keepHidden ? 0 : 1,
            $now,
            (int) $row['id'],
        ]);
    }
    $log("分组「{$line['groupLabel']}」已存在 → 更新为「上游花钱 / 对用户免费 / " . ($keepHidden ? '暂不放开' : '已放开') . "」");
}

// ═══════════════════════════════════════════════════════════
// 二、渠道 + 密钥
// ═══════════════════════════════════════════════════════════

echo "\n═══ 二、渠道与密钥 ═══\n";

foreach ($lines as $line) {
    $groupRow = $pdo->query("SELECT * FROM line_groups WHERE code = " . $pdo->quote($line['group']))->fetch(PDO::FETCH_ASSOC);
    $groupId = (int) ($groupRow['id'] ?? 0);

    $models = [];
    $map = [];
    foreach ($line['models'] as $m) {
        $models[] = $m['name'];
        if ($m['upstream'] !== $m['name']) {
            $map[$m['name']] = $m['upstream'];
        }
    }

    $config = $map === [] ? '' : (string) json_encode(['model_map' => $map], JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);

    $stmt = $pdo->prepare('SELECT * FROM channels WHERE name = ?');
    $stmt->execute([$line['name']]);
    $channel = $stmt->fetch(PDO::FETCH_ASSOC);

    if ($channel === false) {
        if (!$dryRun) {
            $stmt = $pdo->prepare(
                'INSERT INTO channels (name, type, base_url, models, config, group_id, priority, weight, status, created_at, updated_at)
                 VALUES (?, ?, ?, ?, ?, ?, 50, 1, 1, ?, ?)'
            );
            $stmt->execute([
                $line['name'],
                'openai',
                $line['baseUrl'],
                (string) json_encode($models, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES),
                $config,
                $groupId,
                $now,
                $now,
            ]);
            $channel = ['id' => (int) $pdo->lastInsertId()];
        } else {
            $channel = ['id' => 0];
        }
        $log("新建渠道「{$line['name']}」→ {$line['baseUrl']}（" . count($models) . ' 个模型，优先级 50）');
    } else {
        if (!$dryRun) {
            $stmt = $pdo->prepare(
                'UPDATE channels SET base_url = ?, models = ?, config = ?, group_id = ?, status = 1, updated_at = ? WHERE id = ?'
            );
            $stmt->execute([
                $line['baseUrl'],
                (string) json_encode($models, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES),
                $config,
                $groupId,
                $now,
                (int) $channel['id'],
            ]);
        }
        $log("渠道「{$line['name']}」已存在 → 更新地址、模型清单与归组");
    }

    $channelId = (int) $channel['id'];

    foreach ($line['keys'] as [$plain, $budget]) {
        $hash = hash('sha256', 'aqua-channel-key:' . $plain);

        $stmt = $pdo->prepare('SELECT id, budget_total FROM channel_keys WHERE channel_id = ? AND key_hash = ?');
        $stmt->execute([$channelId, $hash]);
        $existing = $stmt->fetch(PDO::FETCH_ASSOC);

        if ($existing !== false) {
            // 已存在：只把额度与备注对齐，**不动状态** ——
            // 这把密钥可能正是因为「额度用完」被自动停用的，
            // 脚本不该在站长没补额度的情况下把它悄悄放开
            if (!$dryRun) {
                $stmt = $pdo->prepare('UPDATE channel_keys SET budget_total = ?, budget_note = ?, updated_at = ? WHERE id = ?');
                $stmt->execute([number_format($budget, 10, '.', ''), $line['note'], $now, (int) $existing['id']]);
            }
            $log('  密钥 ' . substr($plain, 0, 10) . '… 已存在（额度 ' . $budget . ' 元，状态保持不变）');
            continue;
        }

        if (!$dryRun && $channelId > 0) {
            $stmt = $pdo->prepare(
                'INSERT INTO channel_keys (channel_id, key_hash, api_key_enc, status, budget_total, budget_used, budget_note, created_at, updated_at)
                 VALUES (?, ?, ?, 1, ?, 0, ?, ?, ?)'
            );
            $stmt->execute([
                $channelId,
                $hash,
                aqua_encrypt($plain, $appKey),
                number_format($budget, 10, '.', ''),
                $line['note'],
                $now,
                $now,
            ]);
        }
        $log('  新增密钥 ' . substr($plain, 0, 10) . '…（额度 ' . $budget . " 元，用完会自动停用并下架整条线）");
    }
}

// ═══════════════════════════════════════════════════════════
// 三、定价（官方原价）
// ═══════════════════════════════════════════════════════════

echo "\n═══ 三、定价（上游成本与对外售价都用官方价）═══\n";

foreach ($lines as $line) {
    $groupRow = $pdo->query("SELECT * FROM line_groups WHERE code = " . $pdo->quote($line['group']))->fetch(PDO::FETCH_ASSOC);
    $groupId = (int) ($groupRow['id'] ?? 0);

    foreach ($line['models'] as $m) {
        $windows = $m['windows'] ?? [];
        $windowsJson = $windows === [] ? null : (string) json_encode($windows, JSON_UNESCAPED_UNICODE);
        $note = $m['note'];

        $stmt = $pdo->prepare('SELECT id FROM pricing WHERE model = ?');
        $stmt->execute([$m['name']]);
        $pricing = $stmt->fetch(PDO::FETCH_ASSOC);

        $fields = [
            $m['input'], $m['output'], 0, $m['cacheHit'],
            $m['input'], $m['output'], 0, $m['cacheHit'],
            $windowsJson, $windowsJson,
            1000000, $note,
        ];

        if ($pricing === false) {
            if (!$dryRun) {
                $stmt = $pdo->prepare(
                    'INSERT INTO pricing
                        (model, billing_mode, upstream_kind, group_id,
                         upstream_input_price, upstream_output_price, upstream_call_price, upstream_cache_hit_price,
                         downstream_input_price, downstream_output_price, downstream_call_price, downstream_cache_hit_price,
                         upstream_price_windows, downstream_price_windows,
                         price_unit, note, status, created_at, updated_at)
                     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)'
                );
                $stmt->execute(array_merge(
                    [$m['name'], 'token', 'official', $groupId],
                    $fields,
                    [$now, $now]
                ));
            }
            $priceText = number_format($m['input'], 4, '.', '') . ' / ' . number_format($m['output'], 4, '.', '');
            $log("新增定价 {$m['name']}：输入 {$priceText} 元每百万 token" . ($windows === [] ? '' : '（含分时段价）'));
            continue;
        }

        if (!$dryRun) {
            $stmt = $pdo->prepare(
                'UPDATE pricing SET billing_mode = ?, upstream_kind = ?, group_id = ?,
                    upstream_input_price = ?, upstream_output_price = ?, upstream_call_price = ?, upstream_cache_hit_price = ?,
                    downstream_input_price = ?, downstream_output_price = ?, downstream_call_price = ?, downstream_cache_hit_price = ?,
                    upstream_price_windows = ?, downstream_price_windows = ?,
                    price_unit = ?, note = ?, status = 1, updated_at = ?
                 WHERE id = ?'
            );
            $stmt->execute(array_merge(
                ['token', 'official', $groupId],
                $fields,
                [$now, (int) $pricing['id']]
            ));
        }
        $log("定价 {$m['name']} 已存在 → 按价目表拉齐");
    }
}

// ═══════════════════════════════════════════════════════════
// 四、总结
// ═══════════════════════════════════════════════════════════

echo "\n═══ 四、结果 ═══\n";

if ($dryRun) {
    echo "这是预演，什么都没有写入。去掉 --dry-run 即真正执行。\n";
    exit(0);
}

foreach ($lines as $line) {
    $groupRow = $pdo->query("SELECT * FROM line_groups WHERE code = " . $pdo->quote($line['group']))->fetch(PDO::FETCH_ASSOC);
    $groupId = (int) ($groupRow['id'] ?? 0);

    $stmt = $pdo->prepare('SELECT COUNT(*) AS c FROM channel_keys k JOIN channels c2 ON c2.id = k.channel_id WHERE c2.group_id = ? AND k.status = 1');
    $stmt->execute([$groupId]);
    $enabledKeys = (int) $stmt->fetch(PDO::FETCH_ASSOC)['c'];

    $stmt = $pdo->prepare('SELECT COALESCE(SUM(budget_total), 0) AS t, COALESCE(SUM(budget_used), 0) AS u FROM channel_keys k JOIN channels c2 ON c2.id = k.channel_id WHERE c2.group_id = ?');
    $stmt->execute([$groupId]);
    $budget = $stmt->fetch(PDO::FETCH_ASSOC);

    $total = (float) $budget['t'];
    $used = (float) $budget['u'];

    printf(
        "%s：%d 把密钥可用，额度 %.2f 元（已用 %.4f，剩余 %.2f）\n",
        $line['groupLabel'],
        $enabledKeys,
        $total,
        $used,
        max(0.0, $total - $used)
    );

    foreach ($line['models'] as $m) {
        echo '    对外模型名：' . $m['name'] . '（上游实际收到 ' . $m['upstream'] . "）\n";
    }
}

echo "\n⚠️ 记得重启服务让配置生效：\n";
echo "    sudo systemctl restart aqua-api\n";
echo "\n验证方式（用户侧只认 aqua/ 开头的名字）：\n";
echo "    curl https://aqua.is3.cc/v1/chat/completions -H 'Authorization: Bearer <你的令牌>' \\\n";
echo "      -H 'Content-Type: application/json' \\\n";
echo "      -d '{\"model\":\"aqua/GLM-5.3\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}'\n";
echo "\n调用后在 后台 → 线路分组 里看「已用额度」是否增长 —— 涨了就说明成本记账通了。\n";
