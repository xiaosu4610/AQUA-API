<?php
/**
 * SQL 标识符守卫：不许把 MySQL 保留字当表名或列名。
 *
 * ═══ 为什么需要这个测试 ═══
 *
 * 生产上刚炸过一次：线路分组那张表起名 `groups`，而 `groups` 是
 * **MySQL 8 的保留字**。开发库是 SQLite（它不介意），所以本地全绿；
 * 一到生产的 MySQL，CREATE TABLE / SELECT / INSERT 全部语法错误 ——
 * 整段迁移在第一步就中断，表与列一个都没建成。
 * 更坏的是它**不立刻报错**：服务照常启动（初始化异常只记日志），
 * 直到新代码去写一个不存在的列，才以「新建令牌失败」的形式暴露给用户。
 *
 * 这类错误的代价与「少写一个字符」完全不成比例，所以在这里做一道静态拦截：
 * 把代码里的 **SQL 字符串**抓出来（只看引号/heredoc 里的内容，
 * 不扫注释与普通文案，否则满屏误报），检查其中的
 *   · FROM / JOIN / INTO / UPDATE / CREATE TABLE 后面的表名
 *   · INSERT INTO x (...) 里的列名
 *   · UPDATE x SET a = ?, b = ? 里的列名
 *   · addColumnIfMissing('表', '列', '类型') 这两项
 * 有没有撞上 MySQL 保留字。
 *
 * 说明：保留字表是**按「有可能被当标识符用」筛选过的**，不是 MySQL 的完整清单 ——
 * 目标是把真风险盖住，又不引入一堆看着像 SQL 的自然语言误报。
 *
 * 用法：php dev/test-sql-identifiers.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

/** MySQL 8.0 保留字里「有可能被拿来当表名 / 列名」的那一批 */
const RESERVED_WORDS = [
    'ACCESSIBLE', 'ADD', 'ALTER', 'ANALYZE', 'BEFORE', 'BETWEEN',
    'BINARY', 'BLOB', 'BOTH', 'CALL', 'CASCADE', 'CASE', 'CHANGE', 'CHAR', 'CHARACTER',
    'CHECK', 'COLLATE', 'COLUMN', 'CONDITION', 'CONSTRAINT', 'CONTINUE', 'CONVERT',
    'CROSS', 'CUBE', 'CURRENT_DATE', 'CURRENT_TIME', 'CURRENT_TIMESTAMP', 'CURRENT_USER',
    'CURSOR', 'DATABASE', 'DATABASES', 'DEC', 'DECIMAL', 'DECLARE', 'DEFAULT', 'DELETE',
    'DESC', 'DESCRIBE', 'DISTINCT', 'DIV', 'DOUBLE', 'DUAL', 'EACH', 'ELSE', 'ELSEIF',
    'ENCLOSED', 'ESCAPED', 'EXCEPT', 'EXISTS', 'EXIT', 'EXPLAIN', 'FALSE', 'FETCH',
    'FIRST_VALUE', 'FLOAT', 'FOR', 'FORCE', 'FOREIGN', 'FULLTEXT', 'FUNCTION',
    'GENERATED', 'GRANT', 'GROUP', 'GROUPING', 'GROUPS', 'HAVING', 'IGNORE',
    'INDEX', 'INFILE', 'INNER', 'INOUT', 'INSENSITIVE', 'INT', 'INTEGER', 'INTERVAL',
    'ITERATE', 'JSON_TABLE', 'KEY', 'KEYS', 'KILL', 'LAG', 'LAST_VALUE',
    'LATERAL', 'LEAD', 'LEADING', 'LEAVE', 'LEFT', 'LIKE', 'LIMIT', 'LINES', 'LOAD', 'LOCALTIME',
    'LOCALTIMESTAMP', 'LOCK', 'LONG', 'LOOP', 'MATCH', 'MAXVALUE', 'MOD', 'MODIFIES', 'NATURAL',
    'NULL', 'NUMERIC', 'OPTIMIZE', 'OPTION', 'OPTIONALLY', 'ORDER',
    'OUT', 'OUTER', 'OUTFILE', 'OVER', 'PARTITION', 'PRECISION', 'PRIMARY', 'PROCEDURE', 'PURGE',
    'RANGE', 'RANK', 'READ', 'READS', 'REAL', 'RECURSIVE', 'REFERENCES', 'REGEXP', 'RELEASE',
    'RENAME', 'REPEAT', 'REPLACE', 'REQUIRE', 'RESIGNAL', 'RESTRICT', 'RETURN', 'REVOKE',
    'RIGHT', 'RLIKE', 'ROW', 'ROWS', 'ROW_NUMBER', 'SCHEMA', 'SCHEMAS', 'SELECT', 'SENSITIVE',
    'SEPARATOR', 'SHOW', 'SIGNAL', 'SPATIAL', 'SPECIFIC', 'SQL', 'SQLEXCEPTION',
    'SQLSTATE', 'SQLWARNING', 'SSL', 'STARTING', 'STORED', 'STRAIGHT_JOIN', 'SYSTEM',
    'TERMINATED', 'TRAILING', 'TRIGGER', 'TRUE', 'UNDO', 'UNION', 'UNIQUE',
    'UNLOCK', 'UNSIGNED', 'USAGE', 'USE', 'USING', 'VALUES', 'VARBINARY', 'VARCHAR',
    'VARYING', 'VIRTUAL', 'WHEN', 'WHILE', 'WINDOW', 'WRITE', 'XOR',
];

$files = [];
foreach ([dirname(__DIR__) . '/app', dirname(__DIR__) . '/scripts'] as $dir) {
    $iterator = new RecursiveIteratorIterator(new RecursiveDirectoryIterator($dir, FilesystemIterator::SKIP_DOTS));
    foreach ($iterator as $file) {
        if ($file->isFile() && $file->getExtension() === 'php') {
            $files[] = $file->getPathname();
        }
    }
}

check('扫到了待检查的 PHP 文件', count($files) > 50, '只找到 ' . count($files) . ' 个文件');

$violations = [];
$checked = 0;

/**
 * 给一个标识符与它在文件里的偏移量，命中保留字就记一条。
 *
 * @param array<int, string> $reserved
 */
$inspect = static function (
    string $ident,
    string $path,
    string $code,
    int $offset,
    array $reserved
) use (&$violations, &$checked): void {
    $checked++;

    if (!in_array(strtoupper($ident), $reserved, true)) {
        return;
    }

    $line = substr_count(substr($code, 0, $offset), "\n") + 1;
    $violations[] = basename($path) . ':' . $line . ' —— ' . $ident . ' 是 MySQL 保留字，不能当标识符（请改名）';
};

/**
 * 一段 SQL 文本 → 里面的表名与列名。
 *
 * @return array<int, array{0:string, 1:int}> 标识符 + 它在该文本里的偏移
 */
$identifiersIn = static function (string $sql): array {
    $found = [];

    // 表名：FROM / JOIN / INTO / UPDATE / CREATE TABLE [IF NOT EXISTS]
    preg_match_all(
        '/\b(?:FROM|JOIN|INTO|UPDATE|CREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?)\s+`?([A-Za-z_][A-Za-z0-9_]*)`?/',
        $sql,
        $m,
        PREG_OFFSET_CAPTURE
    );
    foreach ($m[1] as [$ident, $offset]) {
        $found[] = [$ident, $offset];
    }

    // INSERT INTO x (...) 里的列名
    preg_match_all('/\bINSERT\s+INTO\s+`?[A-Za-z_][A-Za-z0-9_]*`?\s*\(([^)]*)\)/', $sql, $m, PREG_OFFSET_CAPTURE);
    foreach ($m[1] as [$list, $offset]) {
        foreach (explode(',', $list) as $column) {
            $column = trim(trim($column), '`');
            if (preg_match('/^[A-Za-z_][A-Za-z0-9_]*$/', $column) === 1) {
                $found[] = [$column, $offset];
            }
        }
    }

    // UPDATE x SET a = ?, b = ?
    preg_match_all('/\bUPDATE\s+`?[A-Za-z_][A-Za-z0-9_]*`?\s+SET\s+([A-Za-z_][^\r\n]*)/', $sql, $m, PREG_OFFSET_CAPTURE);
    foreach ($m[1] as [$set, $offset]) {
        preg_match_all('/`?([A-Za-z_][A-Za-z0-9_]*)`?\s*=/', $set, $columns, PREG_OFFSET_CAPTURE);
        foreach ($columns[1] as [$column, $columnOffset]) {
            $found[] = [$column, $offset + $columnOffset];
        }
    }

    return $found;
};

foreach ($files as $path) {
    $code = (string) file_get_contents($path);

    // 只看「写在引号或 heredoc 里的东西」：
    // 代码里的 SQL 一定在这些地方，而注释与文案里的 FROM/UPDATE 只是英文单词
    $chunks = [];
    preg_match_all('/\'((?:[^\'\\\\]|\\\\.)*)\'/', $code, $m, PREG_OFFSET_CAPTURE);
    foreach ($m[1] as $i => [$text, $offset]) {
        $chunks[] = [$text, $offset];
    }
    preg_match_all('/<<<\'?SQL\'?\R(.*?)\R\s*SQL;/s', $code, $m, PREG_OFFSET_CAPTURE);
    foreach ($m[1] as [$text, $offset]) {
        $chunks[] = [$text, $offset];
    }

    foreach ($chunks as [$sql, $base]) {
        foreach ($identifiersIn($sql) as [$ident, $offset]) {
            $inspect($ident, $path, $code, $base + $offset, RESERVED_WORDS);
        }

        // 迁移里的 addColumnIfMissing('表', '列', '类型')
        preg_match_all(
            "/addColumnIfMissing\(\s*'([A-Za-z_][A-Za-z0-9_]*)'\s*,\s*'([A-Za-z_][A-Za-z0-9_]*)'/",
            $sql,
            $m,
            PREG_OFFSET_CAPTURE
        );
        foreach ($m[1] as $i => [$table, $offset]) {
            $inspect($table, $path, $code, $base + $offset, RESERVED_WORDS);
            $inspect($m[2][$i][0], $path, $code, $base + $m[2][$i][1], RESERVED_WORDS);
        }
    }
}

check('检查了足够多的标识符（没白跑）', $checked > 100, '只检查了 ' . $checked . ' 个');

foreach ($violations as $violation) {
    echo "  [命中] {$violation}\n";
}

check('没有把 MySQL 保留字当表名 / 列名', $violations === [], count($violations) . ' 处命中');

// 反例自检：把假想的坏代码喂给同一套规则，必须能抓出来。
// 不做这一步的话，上面那条「通过」有可能只是因为规则根本没生效
$badSql = 'SELECT * FROM groups WHERE id = ?';
$badHits = 0;
foreach ($identifiersIn($badSql) as [$ident]) {
    if (in_array(strtoupper($ident), RESERVED_WORDS, true)) {
        $badHits++;
    }
}
$badInsert = 'INSERT INTO tokens (id, groups, name) VALUES (?, ?, ?)';
foreach ($identifiersIn($badInsert) as [$ident]) {
    if (in_array(strtoupper($ident), RESERVED_WORDS, true)) {
        $badHits++;
    }
}
$badUpdate = 'UPDATE channel_keys SET rank = ?, groups = ? WHERE id = ?';
foreach ($identifiersIn($badUpdate) as [$ident]) {
    if (in_array(strtoupper($ident), RESERVED_WORDS, true)) {
        $badHits++;
    }
}
check('规则本身能抓出 groups / rank（反例自检）', $badHits >= 4, '只抓到 ' . $badHits . ' 处');

exit(probe_test_report());
