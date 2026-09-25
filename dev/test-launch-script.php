<?php
/**
 * 上线脚本（scripts/launch-paid-lines.php）的验证。
 *
 * 为什么值得单测一个「一次性脚本」：它是站长的「一声令下」——
 * 在生产上手工跑，出错时已经是生产环境了。而且它要写渠道、密钥、定价三张表，
 * 参数个数、字段顺序错一个都会炸。测试里直接把它当子进程跑两遍，
 * 一遍验「写出了什么」，一遍验「再跑一次不会重复写」。
 *
 * 用法：php dev/test-launch-script.php
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Channel;
use app\common\Db;
use app\common\Group;
use app\common\Schema;

$dbFile = probe_test_temp('launch-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();

$script = dirname(__DIR__) . DIRECTORY_SEPARATOR . 'scripts' . DIRECTORY_SEPARATOR . 'launch-paid-lines.php';

/** 跑一次脚本（子进程），返回它的输出 */
$run = static function (string $extra = '') use ($script): array {
    $cmd = escapeshellarg(PHP_BINARY) . ' ' . escapeshellarg($script) . ($extra === '' ? '' : ' ' . $extra);
    $out = [];
    $code = 0;
    exec($cmd . ' 2>&1', $out, $code);

    return ['code' => $code, 'output' => implode("\n", $out)];
};

$count = static function (string $sql, array $params = []): int {
    $row = Db::selectOne($sql, $params);

    return (int) ($row['c'] ?? 0);
};

echo "一、预演不写库\n";

$dry = $run('--dry-run');
check('预演退出码为 0', $dry['code'] === 0, 'exit=' . $dry['code'] . ' ' . mb_substr($dry['output'], -400));
check('预演说清了这是预演', str_contains($dry['output'], '什么都没有写入'), mb_substr($dry['output'], -200));
check('预演没有建渠道', $count('SELECT COUNT(*) AS c FROM channels') === 0);
check('预演没有写定价', $count('SELECT COUNT(*) AS c FROM pricing') === 0);

echo "\n二、真执行\n";

$first = $run();
check('执行退出码为 0', $first['code'] === 0, 'exit=' . $first['code'] . ' ' . mb_substr($first['output'], -500));
check('输出了两条线路的额度汇总', str_contains($first['output'], '硅基流动') && str_contains($first['output'], 'TierFlow'));
check('提醒了要重启服务', str_contains($first['output'], 'systemctl restart aqua-api'));

check('建了两条渠道', $count('SELECT COUNT(*) AS c FROM channels') === 2, (string) $count('SELECT COUNT(*) AS c FROM channels'));
check('建了四把密钥', $count('SELECT COUNT(*) AS c FROM channel_keys') === 4, (string) $count('SELECT COUNT(*) AS c FROM channel_keys'));
check('建了五条定价', $count('SELECT COUNT(*) AS c FROM pricing') === 5, (string) $count('SELECT COUNT(*) AS c FROM pricing'));

check(
    '硅基流动两把密钥各 16 元',
    $count("SELECT COUNT(*) AS c FROM channel_keys k JOIN channels c2 ON c2.id = k.channel_id
            JOIN line_groups g ON g.id = c2.group_id WHERE g.code = 'siliconflow' AND k.budget_total = 16") === 2
);
check(
    'TierFlow 两把密钥各 74 元',
    $count("SELECT COUNT(*) AS c FROM channel_keys k JOIN channels c2 ON c2.id = k.channel_id
            JOIN line_groups g ON g.id = c2.group_id WHERE g.code = 'tierflow' AND k.budget_total = 74") === 2
);
check('密钥都是启用状态', $count('SELECT COUNT(*) AS c FROM channel_keys WHERE status = 1') === 4);

echo "\n三、临时模型 ID 的映射\n";

$glmChannel = Db::selectOne("SELECT c.* FROM channels c JOIN line_groups g ON g.id = c.group_id WHERE g.code = 'tierflow'");
check('专线渠道归到了 tierflow 分组', $glmChannel !== null);
check('渠道清单里是对外名 aqua/GLM-5.3', in_array('aqua/GLM-5.3', Channel::modelsOf($glmChannel), true));
check('转发时翻译成上游真实名', Channel::upstreamModel($glmChannel, 'aqua/GLM-5.3') === 'GLM-5.3');
check('aqua/Qwen3.8-Flash 也能翻译', Channel::upstreamModel($glmChannel, 'aqua/Qwen3.8-Flash') === 'Qwen3.8-Flash');

$sfChannel = Db::selectOne("SELECT c.* FROM channels c JOIN line_groups g ON g.id = c.group_id WHERE g.code = 'siliconflow'");
check('硅基流动渠道地址正确', (string) $sfChannel['base_url'] === 'https://api.siliconflow.cn/v1', (string) $sfChannel['base_url']);
check('硅基流动只上了 1 个模型', count(Channel::modelsOf($sfChannel)) === 1);
check('带斜杠的模型名也能映射', Channel::upstreamModel($sfChannel, 'aqua/DeepSeek-V4-Flash') === 'deepseek-ai/DeepSeek-V4-Flash');

echo "\n四、定价与时段价\n";

$price = Db::selectOne("SELECT * FROM pricing WHERE model = 'aqua/DeepSeek-V4-Flash'");
check('硅基流动定价行在', $price !== null);
check('上游输入价 3.0', (float) $price['upstream_input_price'] === 3.0, (string) $price['upstream_input_price']);
check('上游输出价 9.0', (float) $price['upstream_output_price'] === 9.0, (string) $price['upstream_output_price']);
check('缓存命中价 0.3', (float) $price['upstream_cache_hit_price'] === 0.3, (string) $price['upstream_cache_hit_price']);
check('计价单位是每百万', (int) $price['price_unit'] === 1000000);

$windows = app\common\Pricing::windowsFrom($price['upstream_price_windows']);
check('时段价写进去了（两段）', count($windows) === 2, '实际 ' . count($windows) . ' 段');
check('第一段是 02:00-08:00 谷时', ($windows[0]['from'] ?? '') === '02:00' && ($windows[0]['to'] ?? '') === '08:00', (string) ($windows[0]['from'] ?? ''));

$glmPrice = Db::selectOne("SELECT * FROM pricing WHERE model = 'aqua/GLM-5.3'");
check('GLM-5.3 官方价 8 / 28', (float) $glmPrice['upstream_input_price'] === 8.0 && (float) $glmPrice['upstream_output_price'] === 28.0);
$flashX = Db::selectOne("SELECT * FROM pricing WHERE model = 'aqua/GLM-5.3-FlashX'");
check('FlashX 的备注写明了「待确认」', str_contains((string) $flashX['note'], '待站长确认'), (string) $flashX['note']);

echo "\n五、放开可见\n";

$silicon = Group::findByCode('siliconflow');
$tierflow = Group::findByCode('tierflow');
check('两条专线都已可见', (int) $silicon['visible'] === 1 && (int) $tierflow['visible'] === 1);
check('两条专线都进了默认开放（新令牌自动能用）', (int) $silicon['default_visible'] === 1 && (int) $tierflow['default_visible'] === 1);
check('两条专线都是「上游花钱 / 对用户免费」', (string) $silicon['cost_mode'] === 'paid' && (string) $silicon['price_mode'] === 'free');

echo "\n六、重复执行不会重复写（幂等）\n";

$second = $run();
check('第二次执行退出码为 0', $second['code'] === 0, 'exit=' . $second['code'] . ' ' . mb_substr($second['output'], -500));
check('渠道还是两条', $count('SELECT COUNT(*) AS c FROM channels') === 2, (string) $count('SELECT COUNT(*) AS c FROM channels'));
check('密钥还是四把', $count('SELECT COUNT(*) AS c FROM channel_keys') === 4, (string) $count('SELECT COUNT(*) AS c FROM channel_keys'));
check('定价还是五条', $count('SELECT COUNT(*) AS c FROM pricing') === 5, (string) $count('SELECT COUNT(*) AS c FROM pricing'));
check('输出里说的是「已存在」而不是「新建」', str_contains($second['output'], '已存在'));

echo "\n七、--keep-hidden：只建线路，不放开使用\n";

$hidden = $run('--keep-hidden');
check('退出码为 0', $hidden['code'] === 0, mb_substr($hidden['output'], -400));
Group::forget(); // 脚本是另一个进程改的库，本进程的分组缓存还活着（5 秒 TTL）
$silicon = Group::findByCode('siliconflow');
check('专线被收回可见', (int) $silicon['visible'] === 0);
check('专线被移出默认开放', (int) $silicon['default_visible'] === 0);
check('渠道与密钥都还在（只是不对外开放）', $count('SELECT COUNT(*) AS c FROM channels') === 2);

echo "\n八、密钥被自动停用后，脚本不会把它悄悄放开\n";

Db::execute("UPDATE channel_keys SET status = 0, budget_disabled = 1 WHERE budget_total = 16");
$run();
check(
    '因额度耗尽被停用的密钥仍然是停用状态',
    $count('SELECT COUNT(*) AS c FROM channel_keys WHERE budget_total = 16 AND status = 0') === 2
);

exit(probe_test_report());
