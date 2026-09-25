<?php
/**
 * 后台「模型检测」页面预览（开发用，不参与生产）。
 *
 * 用途：把「检测中」和「已完成」两种页面状态一次性摆出来，
 * 好在浏览器里看排版 —— 真去线上跑一遍 82 个模型的检测要等好几分钟，
 * 而这一页的排版问题（进度条跳动、长模型名撑破布局、勾选框错位）
 * 恰恰只能靠眼睛发现。
 *
 * 用法：php scripts/probe-preview.php [端口，默认 8787]
 *
 * 它会用一个**临时数据库**（不碰开发库、不碰生产库）起一个应用服务，
 * 打印登录地址与密码，按 Ctrl+C 结束。
 */

declare(strict_types=1);

require __DIR__ . '/_probe-test-harness.php';

use app\common\ModelProbe;
use app\common\ModelProbeTask;
use app\common\Schema;

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

$app = probe_app_start($port);
$base = 'http://127.0.0.1:' . $app['port'];

echo "════════════════════════════════════════════════════════\n";
echo "后台「模型检测」页面预览\n";
echo "════════════════════════════════════════════════════════\n";
echo "登录页     {$base}/admin/login\n";
echo "密码       preview-pass-1234\n";
echo "检测中     {$base}/admin/channels/probe?id={$runningTaskId}\n";
echo "已完成     {$base}/admin/channels/probe?id={$taskId}\n";
echo "渠道列表   {$base}/admin/channels\n";
echo "临时库     {$dbFile}\n";
echo "按 Ctrl+C 结束（临时库与进程会自动清理）\n";
echo "────────────────────────────────────────────────────────\n";

while (true) {
    sleep(1);
}
