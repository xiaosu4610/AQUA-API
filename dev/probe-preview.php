<?php
/**
 * 后台与前台页面预览（开发用，不参与生产）。
 *
 * 用途：把「检测中」和「已完成」两种状态、以及一堆真实的用量数据
 * 一次性摆出来，好在浏览器里看排版 —— 真去线上跑一遍 82 个模型的检测
 * 要等好几分钟，而排版问题（进度条跳动、长模型名撑破布局、勾选框错位、
 * 报表列太挤）恰恰只能靠眼睛发现。
 *
 * 用法：php dev/probe-preview.php [端口，默认 8787]
 *
 * 它会用一个**临时数据库**（不碰开发库、不碰生产库）起一个应用服务，
 * 打印登录地址与密码，按 Ctrl+C 结束。
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\Db;
use app\common\ModelProbe;
use app\common\ModelProbeTask;
use app\common\Schema;
use app\common\User;
use app\common\UserToken;

$port = (int) ($argv[1] ?? 8787);
$dbFile = probe_test_temp('preview-db', '.sqlite');
probe_test_bootstrap($dbFile);
Schema::ensure();
probe_seed_admin('preview-pass-1234');

$completedModels = [
    'meta/llama-3.1-8b-instruct',
    'meta/llama-3.1-70b-instruct',
    'meta/llama-3.3-70b-instruct',
    'nvidia/llama-3.1-nemotron-70b-instruct',
    'mistralai/mistral-7b-instruct-v0.3',
    'mistralai/mixtral-8x22b-instruct-v0.1',
    'google/gemma-2-9b-it',
    'microsoft/phi-3-medium-128k-instruct',
    'nvidia/nv-embedqa-e5-v5',
    'nvidia/llama-nemotron-embed-1b-v2',
    'nvidia/nv-rerankqa-mistral-4b-v3',
    'stabilityai/stable-diffusion-xl',
];
$channelId = probe_seed_channel(
    'NVIDIA NIM（预览数据）',
    'https://integrate.api.nvidia.com/v1',
    $completedModels,
    ['nvapi-preview-key-0000']
);

$created = ModelProbeTask::create($channelId, $completedModels);
$taskId = (int) $created['task']['id'];
ModelProbeTask::start($taskId);
$plan = [
    'meta/llama-3.1-8b-instruct' => [ModelProbe::OK, 200, '{"choices":[{"message":{"content":"hi"}}]}'],
    'meta/llama-3.1-70b-instruct' => [ModelProbe::OK, 200, '{"choices":[{"message":{"content":"hi"}}]}'],
    'meta/llama-3.3-70b-instruct' => [ModelProbe::OK, 200, '{"choices":[{"message":{"content":"hi"}}]}'],
    'nvidia/llama-3.1-nemotron-70b-instruct' => [ModelProbe::OK, 200, '{"choices":[{"message":{"content":"hi"}}]}'],
    'mistralai/mistral-7b-instruct-v0.3' => [ModelProbe::OK, 200, '{"choices":[{"message":{"content":"hi"}}]}'],
    'mistralai/mixtral-8x22b-instruct-v0.1' => [ModelProbe::OK, 200, '{"choices":[{"message":{"content":"hi"}}]}'],
    'google/gemma-2-9b-it' => [ModelProbe::NO_ACCESS, 404, '{"detail":"Function \'gemma-2-9b-it\' not found for account"}'],
    'microsoft/phi-3-medium-128k-instruct' => [ModelProbe::NO_ACCESS, 404, '{"detail":"Function \'phi-3\' not found for account"}'],
    'nvidia/nv-embedqa-e5-v5' => [ModelProbe::UNROUTABLE, 404, '404 page not found'],
    'nvidia/llama-nemotron-embed-1b-v2' => [ModelProbe::UNROUTABLE, 404, '404 page not found'],
    'nvidia/nv-rerankqa-mistral-4b-v3' => [ModelProbe::INCONCLUSIVE, 0, 'Operation timed out after 20001 milliseconds'],
    'stabilityai/stable-diffusion-xl' => [ModelProbe::INCONCLUSIVE, 400, '{"error":"bad request: model is not a chat model"}'],
];
foreach ($completedModels as $model) {
    [$classification, $http, $summary] = $plan[$model];
    ModelProbeTask::setCurrent($taskId, $model);
    ModelProbeTask::addResult($taskId, [
        'model' => $model,
        'upstream_model' => $model,
        'result' => $classification,
        'http' => $http,
        'summary' => $summary,
        // 预览用的假耗时：按模型名稳定散列，这样每次跑出来的排版都一样，
        // 便于比较「快 / 一般 / 慢」三档标签在页面上的观感
        'latency_ms' => 380 + (crc32($model) % 9200),
    ]);
}
ModelProbeTask::finish($taskId);

// 另起一个「检测中」的任务，用来看进度页
$runningModels = array_merge($completedModels, ['nvidia/nemotron-4-340b-instruct', 'ai21labs/jamba-1.5-large-instruct']);
$runningChannel = probe_seed_channel(
    '硅基流动（预览数据）',
    'https://api.siliconflow.cn/v1',
    $runningModels,
    ['sk-preview-key-1111']
);
$runningTaskId = (int) ModelProbeTask::create($runningChannel, $runningModels)['task']['id'];
ModelProbeTask::start($runningTaskId);
foreach (array_slice($runningModels, 0, 5) as $model) {
    ModelProbeTask::addResult($runningTaskId, [
        'model' => $model,
        'upstream_model' => $model,
        'result' => ModelProbe::OK,
        'http' => 200,
        'summary' => '{"choices":[]}',
    ]);
}
ModelProbeTask::setCurrent($runningTaskId, 'mistralai/mixtral-8x22b-instruct-v0.1');

// ── 下游数据：用户、令牌、用量日志 ──────────────────────────
// 报表页与用户控制台没有数据就是一片「暂无记录」，看不出排版好坏
$demoUsers = [
    ['preview-a@example.com', 128.5],
    ['preview-b@example.com', 42.0],
    ['preview-c@example.com', 0.0],
];
$userIds = [];
foreach ($demoUsers as [$email, $balance]) {
    $created = User::register($email, 'preview-user-pass', '127.0.0.1', false);
    $userIds[$email] = (int) $created['id'];
    UserToken::create((int) $created['id'], '预览令牌');
    if ($balance > 0) {
        User::credit((int) $created['id'], $balance);
    }
}

$modelsForLogs = array_slice($completedModels, 0, 6);
$todayStart = strtotime('today') ?: time();
foreach ($userIds as $email => $userId) {
    for ($i = 0; $i < 26; $i++) {
        $model = $modelsForLogs[$i % count($modelsForLogs)];
        $failed = $i % 9 === 4;
        $downstream = 0.002 * (1 + $i % 5);
        Db::execute(
            'INSERT INTO usage_logs (created_at, user_id, token_id, channel_id, channel_name, model, billing_mode,
                                     is_stream, prompt_tokens, completion_tokens, total_tokens, usage_estimated,
                                     upstream_cost, downstream_cost, latency_ms, status, error_message)
             VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)',
            [
                $todayStart - ($i % 5) * 86400 + 7200 + $i * 60,
                $userId,
                $channelId,
                'NVIDIA NIM（预览数据）',
                $model,
                'token',
                $i % 3 === 0 ? 1 : 0,
                120 + $i * 7,
                240 + $i * 11,
                360 + $i * 18,
                $i % 7 === 3 ? 1 : 0,
                number_format($downstream * 0.45, 10, '.', ''),
                number_format($downstream, 10, '.', ''),
                420 + (crc32($model . $i) % 9000),
                $failed ? 'error' : 'ok',
                $failed ? 'rate limited by upstream (429)' : null,
            ]
        );
    }
}

$app = probe_app_start($port);
$base = 'http://127.0.0.1:' . $app['port'];

echo "════════════════════════════════════════════════════════\n";
echo "页面预览（前台 + 后台）\n";
echo "════════════════════════════════════════════════════════\n";
echo "站点首页   {$base}/\n";
echo "模型广场   {$base}/models\n";
echo "接口文档   {$base}/docs\n";
echo "后台登录   {$base}/admin/login\n";
echo "后台用量报表 {$base}/admin/usage\n";
echo "检测中     {$base}/admin/channels/probe?id={$runningTaskId}\n";
echo "已完成     {$base}/admin/channels/probe?id={$taskId}\n";
echo "渠道列表   {$base}/admin/channels\n";
echo "后台密码   preview-pass-1234\n";
echo "用户控制台 {$base}/login （preview-a@example.com / preview-user-pass）\n";
echo "临时库     {$dbFile}\n";
echo "按 Ctrl+C 结束（临时库与进程会自动清理）\n";
echo "────────────────────────────────────────────────────────\n";

while (true) {
    sleep(1);
}
