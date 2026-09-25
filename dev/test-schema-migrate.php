<?php
/**
 * 迁移内核的测试：版本推进、幂等、以及「迁移失败不能被吞掉」。
 *
 * ═══ 为什么值得单测 ═══
 *
 * 生产上刚出过两次事故，都在这一段代码里：
 *   ① 表名撞 MySQL 保留字 → 整段迁移第一步就死，而服务照常启动，
 *      直到新代码写不存在的列才爆出来
 *   ② 八个工作进程同时启动抢着加同一列 → 抢输的进程收到 Duplicate column，
 *      异常一路冒到 InitDb 就中断了它后面所有的建表动作
 * 这两个都不是「功能写错了」，而是**失败方式设计得不好**：
 * 一个不吭声，一个把可恢复的竞争当成致命错误。
 * 所以这里把「幂等」「失败要抛」「已存在要当成功」三件事钉住。
 *
 * 用法：php dev/test-schema-migrate.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Db;
use app\common\Schema;
use app\common\Settings;

$dbFile = probe_test_temp('schema-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();

echo "一、版本推进\n";

check('版本等于 Schema::VERSION', (int) Settings::get('schema_version', 0) === Schema::VERSION, (string) Settings::get('schema_version', '无'));
check('版本号以 JSON 字符串存', str_starts_with((string) Settings::raw('schema_version'), '"'), (string) Settings::raw('schema_version'));

echo "\n二、建出来的东西\n";

$tables = [];
foreach (Db::select("SELECT name FROM sqlite_master WHERE type = 'table'") as $row) {
    $tables[] = (string) $row['name'];
}
check('建了 line_groups 表', in_array('line_groups', $tables, true), implode(',', $tables));
check('没有建 groups 这张撞保留字的表', !in_array('groups', $tables, true));

$columns = static function (string $table): array {
    $columns = [];
    foreach (Db::select("PRAGMA table_info({$table})") as $row) {
        $columns[] = (string) $row['name'];
    }

    return $columns;
};

$expected = [
    'channels' => ['group_id'],
    'pricing' => ['group_id', 'upstream_cache_hit_price', 'downstream_cache_hit_price', 'upstream_price_windows', 'downstream_price_windows'],
    'tokens' => ['allow_groups'],
    'channel_keys' => ['budget_total', 'budget_used', 'budget_note', 'budget_disabled', 'budget_reported', 'budget_reported_at'],
    'usage_logs' => ['cached_tokens', 'channel_key_id'],
];
foreach ($expected as $table => $need) {
    $have = $columns($table);
    foreach ($need as $column) {
        check("{$table}.{$column} 已建出来", in_array($column, $have, true), implode(',', $have));
    }
}

check('种子分组有 4 个', (int) (Db::selectOne('SELECT COUNT(*) AS c FROM line_groups')['c'] ?? 0) === 4);

echo "\n三、幂等：重复执行不报错、不重复建\n";

$before = (int) (Db::selectOne('SELECT COUNT(*) AS c FROM line_groups')['c'] ?? 0);
$threw = false;
try {
    Schema::ensure();
    Schema::ensure();
} catch (Throwable $e) {
    $threw = true;
    echo '  （异常：' . $e->getMessage() . "）\n";
}
check('重复执行 Schema::ensure() 不抛异常', !$threw);
check('分组没有被重复插入', (int) (Db::selectOne('SELECT COUNT(*) AS c FROM line_groups')['c'] ?? 0) === $before);

echo "\n四、列已存在时静默通过；SQL 真写错时必须抛出来\n";

$method = new ReflectionMethod(Schema::class, 'addColumnIfMissing');
$method->setAccessible(true);

$ok = true;
try {
    // 已存在的列：多进程抢输的那一方走的就是这条路，必须当成功
    $method->invoke(null, 'channels', 'group_id', 'INTEGER NOT NULL DEFAULT 0');
} catch (Throwable $e) {
    $ok = false;
    echo '  （异常：' . $e->getMessage() . "）\n";
}
check('给已存在的列再调一次 → 静默通过', $ok);

$thrown = '';
try {
    // 这条 ALTER 语法就是错的（DEFAULT 后面少了个值）：这不是竞争，必须抛 ——
    // 否则迁移会「假装成功」地跳过去，问题留到几个月后才以别的形式爆出来。
    // 刻意不用「类型名不存在」当反例：SQLite 是动态类型，任何类型名它都收
    $method->invoke(null, 'channels', 'a_broken_column', 'INTEGER NOT NULL DEFAULT');
} catch (Throwable $e) {
    $thrown = $e->getMessage();
}
check('列定义写错 → 抛出异常（没被吞掉）', $thrown !== '', '居然没抛：迁移会把错误吃下去');
check('写错的列没有被建出来', !in_array('a_broken_column', $columns('channels'), true));

exit(probe_test_report());
