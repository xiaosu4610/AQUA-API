<?php
/**
 * 把 SQLite 里的数据迁移到 MySQL
 *
 * 背景：本项目默认用 SQLite（零配置即可跑起来），但 SQLite 在多进程写入下
 * 终究是单写者模型，站点规模上来以后必须换 MySQL。这个脚本负责把老数据搬过去，
 * **一条不丢** —— 尤其是渠道密钥池里那几百把 Key（它们是加密存的，
 * 只要 APP_KEY 不变就能照常解密，因此搬迁时不做任何解密/再加密）。
 *
 * ═══ 用法 ═══════════════════════════════════════════════════
 *
 *   1. 先在 .env 里填好 MySQL 连接（DB_DSN / DB_USER / DB_PASSWORD），
 *      但**先不要重启服务**（此时服务仍连着 SQLite，站点不中断）；
 *   2. 停服务：systemctl stop aqua-api
 *   3. 跑本脚本：sudo -u www-data php scripts/migrate-sqlite-to-mysql.php
 *   4. 起服务：systemctl start aqua-api
 *
 * 也可以先 `--dry-run` 看一眼会搬多少条，不做任何写入。
 *
 * ═══ 为什么要停服务 ═════════════════════════════════════════
 *
 * 迁移期间如果服务还在跑，它会继续往 **旧库** 写（新增渠道、更新限流计数），
 * 那些变更不会出现在迁移结果里 —— 表现为「刚加的渠道不见了」。
 * 停机几秒换取确定性，是划算的。
 *
 * ═══ 幂等性 ═══════════════════════════════════════════════
 *
 * 脚本按主键逐条判重后再插入，因此**可以重复执行**：
 * 中断后重跑不会产生重复数据，已迁过的表也会被跳过（只补缺失的行）。
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

$args = aqua_args();
$dryRun = isset($args['dry-run']);
$sourceFile = $args['source'] ?? (aqua_root() . '/runtime/aqua.sqlite');

// ── 前置检查 ────────────────────────────────────────────────

if (trim(aqua_env('DB_DSN')) === '') {
    fwrite(STDERR, "目标库还没配置成 MySQL（.env 里的 DB_DSN 为空）。\n");
    fwrite(STDERR, "请先在 .env 里填 DB_DSN / DB_USER / DB_PASSWORD，再运行本脚本。\n");
    exit(1);
}

if (!is_readable($sourceFile)) {
    fwrite(STDERR, "找不到 SQLite 源文件：$sourceFile\n");
    fwrite(STDERR, "如果它不在默认位置，用 --source=<路径> 指定。\n");
    exit(1);
}

echo "源库（SQLite）：$sourceFile\n";
echo "目标库（MySQL）：" . preg_replace('/password=[^;]*/i', 'password=***', aqua_env('DB_DSN')) . "\n";
echo $dryRun ? "模式：试运行（不写入任何数据）\n\n" : "模式：正式迁移\n\n";

$source = new PDO('sqlite:' . $sourceFile, null, null, [
    PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
    PDO::ATTR_DEFAULT_FETCH_MODE => PDO::FETCH_ASSOC,
]);

$target = aqua_pdo();

/**
 * 要搬运的表，以及各自的业务主键。
 *
 * 键是表名，值是「用于判重的主键列」。用业务主键（而不是自增 id）判重，
 * 是因为 options 表的主键是字符串 opt_key，而其余表是自增 id ——
 * 两种情况都要能正确处理。
 *
 * ⚠️ 顺序有讲究：先搬被引用的表（channels），再搬引用它的表（channel_keys）。
 * 虽然本项目没有建外键约束，但保持这个顺序能让迁移日志读起来更符合直觉。
 */
$tables = [
    'options' => 'opt_key',
    'admins' => 'id',
    'channels' => 'id',
    'channel_keys' => 'id',
    'pricing' => 'id',
    'usage_logs' => 'id',
];

/** 表是否存在（源库可能是老版本，缺少后加的表） */
$sourceTables = $source->query("SELECT name FROM sqlite_master WHERE type = 'table'")
    ->fetchAll(PDO::FETCH_COLUMN);

$totalCopied = 0;

foreach ($tables as $table => $pk) {
    if (!in_array($table, $sourceTables, true)) {
        printf("%-14s 源库没有这张表，跳过\n", $table);
        continue;
    }

    // 目标表由 Schema::ensure() 建好；这里只做防御性检查
    try {
        $target->query("SELECT 1 FROM {$table} LIMIT 1");
    } catch (PDOException) {
        printf("%-14s 目标库没有这张表（请先启动过一次服务完成建表），跳过\n", $table);
        continue;
    }

    $rows = $source->query("SELECT * FROM {$table}")->fetchAll();

    // 先把目标库已有的主键取出来，避免逐行查库（几百把 Key 时差别很明显）
    $existing = $target->query("SELECT {$pk} FROM {$table}")->fetchAll(PDO::FETCH_COLUMN);
    $existingSet = array_flip(array_map('strval', $existing));

    $copied = 0;
    $skipped = 0;

    foreach ($rows as $row) {
        // schema_version 属于程序内部元数据，目标库在启动时已自行写入，
        // 不能被源库的旧值覆盖（否则会误触发一次多余的迁移）
        if ($table === 'options' && (string) $row['opt_key'] === 'schema_version') {
            $skipped++;
            continue;
        }

        if (isset($existingSet[(string) $row[$pk]])) {
            $skipped++;
            continue;
        }

        if ($dryRun) {
            $copied++;
            continue;
        }

        $columns = array_keys($row);
        $placeholders = implode(', ', array_fill(0, count($columns), '?'));
        $sql = sprintf(
            'INSERT INTO %s (%s) VALUES (%s)',
            $table,
            implode(', ', array_map(static fn ($c) => "`{$c}`", $columns)),
            $placeholders
        );

        $target->prepare($sql)->execute(array_values($row));
        $copied++;
    }

    printf("%-14s 源 %4d 条 → 新增 %4d 条，已存在/跳过 %4d 条\n", $table, count($rows), $copied, $skipped);
    $totalCopied += $copied;
}

echo "\n";

if ($dryRun) {
    echo "试运行结束：共会新增 {$totalCopied} 条记录（未写入任何数据）。\n";
    exit(0);
}

if ($totalCopied === 0) {
    echo "没有需要迁移的新数据（目标库已是最新）。\n";
} else {
    echo "迁移完成：共新增 {$totalCopied} 条记录。\n";
}

echo "\n接下来：\n";
echo "  1. 启动服务：systemctl start aqua-api\n";
echo "  2. 登录后台，确认「渠道管理」里渠道数量、密钥池把数与迁移前一致\n";
echo "  3. 确认无误后再删除（或重命名）旧的 runtime/aqua.sqlite，作为回退兜底\n";
