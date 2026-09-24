<?php
/**
 * 清理过期用量日志
 *
 * 用量日志是整个项目里**唯一会无限增长**的表：每次请求一条记录，
 * 一天一万次调用就是一年 365 万行。不清理的话，查询会越来越慢，
 * 备份体积也会失控。
 *
 * 保留天数由 `billing.log_retention_days` 决定（默认 90 天）。
 *
 * ═══ 用法 ═══
 *
 *   # 看会删掉多少条（不实际删除）
 *   sudo -u www-data php scripts/purge-usage-logs.php --dry-run
 *
 *   # 真正执行
 *   sudo -u www-data php scripts/purge-usage-logs.php
 *
 *   # 覆盖保留天数
 *   sudo -u www-data php scripts/purge-usage-logs.php --days=30
 *
 * ═══ 建议挂 cron ═══
 *
 * 本项目没有内置定时任务（常驻内存进程里跑调度器会带来多进程重复执行的问题），
 * 因此这件事交给系统 cron：
 *
 *   0 4 * * * cd /var/www/aqua && sudo -u www-data php scripts/purge-usage-logs.php >/dev/null 2>&1
 *
 * 注意：**必须在项目目录下执行**，且要用服务同款用户（www-data）——
 * 否则 clean 出来文件属主不对，SQLite 场景下服务会失去写权限。
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

$args = aqua_args();
$dryRun = isset($args['dry-run']);

// 保留天数：命令行优先，其次读数据库里的配置，最后用 90 天兜底
$days = isset($args['days']) ? max(1, (int) $args['days']) : null;

$pdo = aqua_pdo();

if ($days === null) {
    $stmt = $pdo->prepare('SELECT opt_value FROM options WHERE opt_key = ?');
    $stmt->execute(['billing.log_retention_days']);
    $raw = $stmt->fetchColumn();
    $decoded = $raw === false ? null : json_decode((string) $raw, true);
    $days = is_numeric($decoded) ? max(1, (int) $decoded) : 90;
}

$cutoff = time() - $days * 86400;

echo '保留天数：' . $days . ' 天（删除 ' . date('Y-m-d H:i', $cutoff) . ' 之前的记录）' . PHP_EOL;

// 先数一遍：直接 DELETE 而不给任何提示，站长无法判断这个动作是否正常
$countStmt = $pdo->prepare('SELECT COUNT(*) FROM usage_logs WHERE created_at < ?');
$countStmt->execute([$cutoff]);
$count = (int) $countStmt->fetchColumn();

if ($count === 0) {
    echo '没有需要清理的记录。' . PHP_EOL;
    exit(0);
}

if ($dryRun) {
    echo '试运行：将删除 ' . number_format($count) . ' 条记录（未实际删除）。' . PHP_EOL;
    exit(0);
}

$deleteStmt = $pdo->prepare('DELETE FROM usage_logs WHERE created_at < ?');
$deleteStmt->execute([$cutoff]);

echo '已删除 ' . number_format($deleteStmt->rowCount()) . ' 条记录。' . PHP_EOL;

// 顺带报一下剩余量，便于观察增长趋势
$remaining = (int) $pdo->query('SELECT COUNT(*) FROM usage_logs')->fetchColumn();
echo '当前剩余 ' . number_format($remaining) . ' 条。' . PHP_EOL;
