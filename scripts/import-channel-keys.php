#!/usr/bin/env php
<?php
/**
 * 渠道密钥批量导入工具
 *
 * 用途：把一批上游 API Key 导入某个渠道的密钥池（加密存储、自动去重）。
 * 典型场景：几百把 NIM 免费额度 Key 一次性导入，靠池子轮换把额度聚合成高并发。
 *
 * ═══ 用法 ═══
 *
 *   导入：
 *     php scripts/import-channel-keys.php --channel=<渠道id> --file=<密钥文件> [--rpm=40] [--dry-run]
 *
 *   查看池子状态：
 *     php scripts/import-channel-keys.php --channel=<渠道id> --list
 *
 *   清空池子（危险，需交互确认）：
 *     php scripts/import-channel-keys.php --channel=<渠道id> --clear
 *
 * ═══ 安全须知 ═══
 *
 *   · 密钥文件**不要放进版本库**。本项目约定：这类文件一律放 private/ 目录，
 *     该目录已在 .gitignore 中整体忽略。
 *   · 导入是**幂等**的：同一批文件重复执行不会产生重复记录
 *     （依据 Key 的 SHA-256 去重），所以中断后可以放心重跑。
 *   · 本脚本的输出**绝不包含任何完整 Key**，只显示数量与掩码。
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

$args = aqua_args();
$pdo = aqua_pdo();
$appKey = aqua_require_app_key();

// ── 定位渠道 ────────────────────────────────────────────
$channelId = (int) ($args['channel'] ?? 0);

if ($channelId <= 0) {
    fwrite(STDERR, "请用 --channel=<渠道id> 指定要导入到哪个渠道。\n\n");
    fwrite(STDERR, "可用的渠道：\n");
    foreach ($pdo->query('SELECT id, name, type FROM channels ORDER BY id')->fetchAll(PDO::FETCH_ASSOC) as $c) {
        fprintf(STDERR, "  #%d  %s  [%s]\n", $c['id'], $c['name'], $c['type']);
    }
    exit(1);
}

$channel = $pdo->prepare('SELECT * FROM channels WHERE id = ?');
$channel->execute([$channelId]);
$channel = $channel->fetch(PDO::FETCH_ASSOC);

if (!$channel) {
    fwrite(STDERR, "渠道 #{$channelId} 不存在。\n");
    exit(1);
}

$channelName = $channel['name'];

// ── 查看状态 ────────────────────────────────────────────
if (isset($args['list'])) {
    printPoolStatus($pdo, $channelId, $channelName);
    exit(0);
}

// ── 清空池子 ────────────────────────────────────────────
if (isset($args['clear'])) {
    printPoolStatus($pdo, $channelId, $channelName);

    fwrite(STDOUT, "\n确定要清空该渠道的全部密钥吗？此操作不可撤销，请输入 yes 确认：");
    $answer = trim((string) fgets(STDIN));

    if ($answer !== 'yes') {
        fwrite(STDOUT, "已取消。\n");
        exit(0);
    }

    $deleted = $pdo->prepare('DELETE FROM channel_keys WHERE channel_id = ?');
    $deleted->execute([$channelId]);

    fwrite(STDOUT, '已删除 ' . $deleted->rowCount() . " 条密钥记录。\n");
    exit(0);
}

// ── 导入 ────────────────────────────────────────────────
$file = (string) ($args['file'] ?? '');
if ($file === '') {
    fwrite(STDERR, "请用 --file=<密钥文件路径> 指定要导入的文件。\n");
    exit(1);
}

$rpmLimit = (int) ($args['rpm'] ?? 40);
$dryRun = isset($args['dry-run']);

$keys = aqua_read_keys($file);
$unique = array_values(array_unique($keys));

fwrite(STDOUT, "=== 导入准备 ===\n");
fprintf(STDOUT, "  目标渠道 : #%d %s\n", $channelId, $channelName);
fprintf(STDOUT, "  密钥文件 : %s\n", $file);
fprintf(STDOUT, "  读取条数 : %d\n", count($keys));
fprintf(STDOUT, "  去重后   : %d\n", count($unique));
fprintf(STDOUT, "  单把限额 : %d 次/分钟\n", $rpmLimit);

if ($dryRun) {
    fwrite(STDOUT, "\n[试运行] 未写入数据库。去掉 --dry-run 即可真正导入。\n");
    exit(0);
}

// 先看导入前的状态，便于对比
$before = countKeys($pdo, $channelId);

$insert = $pdo->prepare(
    'INSERT INTO channel_keys
        (channel_id, key_hash, api_key_enc, status, rpm_limit, window_start, used_requests, created_at, updated_at)
     VALUES (?, ?, ?, 1, ?, 0, 0, ?, ?)'
);

$added = 0;
$skipped = 0;
$now = time();

foreach ($unique as $key) {
    if (strlen($key) < 8) {
        continue;
    }

    // 去重哈希与 app/common/ChannelKey.php 保持一致（含固定的领域前缀）
    $hash = hash('sha256', 'aqua-channel-key:' . $key);

    $exists = $pdo->prepare('SELECT id FROM channel_keys WHERE channel_id = ? AND key_hash = ?');
    $exists->execute([$channelId, $hash]);
    if ($exists->fetchColumn()) {
        $skipped++;
        continue;
    }

    try {
        $insert->execute([$channelId, $hash, aqua_encrypt($key, $appKey), $rpmLimit, $now, $now]);
        $added++;
    } catch (PDOException) {
        // 并发导入时唯一约束会拦下重复项，按跳过处理
        $skipped++;
    }
}

$after = countKeys($pdo, $channelId);

fwrite(STDOUT, "\n=== 导入结果 ===\n");
fprintf(STDOUT, "  新增      : %d\n", $added);
fprintf(STDOUT, "  已存在跳过: %d\n", $skipped);
fprintf(STDOUT, "  池子总数  : %d（导入前 %d）\n", $after, $before);

printPoolStatus($pdo, $channelId, $channelName);

/**
 * 统计某渠道的密钥数量。
 */
function countKeys(PDO $pdo, int $channelId): int
{
    $stmt = $pdo->prepare('SELECT COUNT(*) FROM channel_keys WHERE channel_id = ?');
    $stmt->execute([$channelId]);

    return (int) $stmt->fetchColumn();
}

/**
 * 打印密钥池状态。只显示数量与「最早/最晚使用时间」这类运维信息，
 * **不显示任何密钥内容**（连掩码也不打，避免日志被截图外泄）。
 */
function printPoolStatus(PDO $pdo, int $channelId, string $channelName): void
{
    $stmt = $pdo->prepare(
        'SELECT
            COUNT(*) AS total,
            SUM(CASE WHEN status = 1 THEN 1 ELSE 0 END) AS enabled,
            SUM(CASE WHEN status = 0 THEN 1 ELSE 0 END) AS disabled,
            MAX(last_used_at) AS last_used
         FROM channel_keys WHERE channel_id = ?'
    );
    $stmt->execute([$channelId]);
    $s = $stmt->fetch(PDO::FETCH_ASSOC);

    $total = (int) ($s['total'] ?? 0);
    $enabled = (int) ($s['enabled'] ?? 0);
    $disabled = (int) ($s['disabled'] ?? 0);
    $rpmStmt = $pdo->prepare('SELECT COALESCE(SUM(CASE WHEN rpm_limit > 0 THEN rpm_limit ELSE 40 END), 0) FROM channel_keys WHERE channel_id = ? AND status = 1');
    $rpmStmt->execute([$channelId]);
    $aggregateRpm = (int) $rpmStmt->fetchColumn();

    fwrite(STDOUT, "\n=== 密钥池状态（{$channelName}）===\n");
    fprintf(STDOUT, "  总数      : %d\n", $total);
    fprintf(STDOUT, "  可用      : %d\n", $enabled);
    fprintf(STDOUT, "  已停用    : %d\n", $disabled);
    fprintf(STDOUT, "  聚合限额  : 约 %d 次/分钟（可用密钥的限额之和）\n", $aggregateRpm);

    if (!empty($s['last_used'])) {
        fprintf(STDOUT, "  最近使用  : %s\n", date('Y-m-d H:i:s', (int) $s['last_used']));
    } else {
        fwrite(STDOUT, "  最近使用  : 尚未使用\n");
    }
}
