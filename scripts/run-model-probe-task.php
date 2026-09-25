<?php
/**
 * 独立执行渠道模型探测任务，仅接收任务 ID。
 */

declare(strict_types=1);

use app\common\Channel;
use app\common\ChannelKey;
use app\common\ModelProbe;
use app\common\ModelProbeTask;
use app\common\Schema;
use app\common\Settings;

if (PHP_SAPI !== 'cli') {
    exit(1);
}

require dirname(__DIR__) . '/vendor/autoload.php';
require dirname(__DIR__) . '/support/bootstrap.php';

$taskId = isset($argv[1]) && ctype_digit((string) $argv[1]) ? (int) $argv[1] : 0;
if ($taskId <= 0 || count($argv) !== 2) {
    fwrite(STDERR, "用法：php scripts/run-model-probe-task.php <任务ID>\n");
    exit(1);
}

$claimed = false;

try {
    Schema::ensure();
    $task = ModelProbeTask::find($taskId);
    if ($task === null) {
        throw new RuntimeException('任务不存在');
    }
    // 已完成/已中止/已失败的任务：什么都不做就退出。
    // 不能在这里抛异常 —— 抛了会走下面的 catch，把一个好端端的完成任务标记成失败。
    if ((string) $task['status'] !== 'pending') {
        exit(0);
    }

    $channel = Channel::find((int) $task['channel_id']);
    if ($channel === null) {
        throw new RuntimeException('渠道不存在');
    }
    $models = Channel::modelsOf($channel);
    if ($models === []) {
        throw new RuntimeException('渠道模型清单为空');
    }

    $keys = [];
    foreach (ChannelKey::allForChannel((int) $channel['id']) as $row) {
        if ((int) $row['status'] !== ChannelKey::STATUS_ENABLED) {
            continue;
        }
        $plain = ChannelKey::plainKey($row);
        if ($plain !== '') {
            $keys[] = $plain;
        }
    }
    if ($keys === []) {
        $single = Channel::plainKey($channel);
        if ($single !== '') {
            $keys[] = $single;
        }
    }
    if ($keys === []) {
        throw new RuntimeException('渠道没有可用凭据');
    }

    // 只有把 pending 改成 running 成功的进程才继续跑；没领到说明已有进程在跑这份清单。
    $claimed = ModelProbeTask::start($taskId);
    if (!$claimed) {
        exit(0);
    }

    // 后台进程能加载框架，所以这里取「最终生效配置」（声明默认值 + 适配器默认值 + 渠道配置）。
    // 与转发引擎用的是同一份配置 —— 否则某些靠适配器默认值鉴权的渠道
    // （例如 query 型、自定义头型）会被探成「鉴权失败」，把好模型误判成不可用。
    $adv = Channel::advConfig($channel);
    $channel['probe_config'] = $adv;

    // 超时口径与 scripts/prune-models.php 的默认值保持一致（20 秒）：
    // 渠道级 total_timeout 是「0 表示用全局默认」，直接把它当秒数会让探测只用 5 秒，
    // 而不够长的超时会把冷启动慢的模型误判成「无法判定」。
    $configured = (int) ($adv['total_timeout'] ?? 0);
    $timeout = $configured > 0 ? max(5, $configured) : max(5, Settings::int('gateway.probe_timeout', 20));

    // 固定用第 1 把凭据，与 scripts/prune-models.php 口径一致：轮换会让结论随凭据变化，无法复现。
    $probeKey = $keys[0];

    foreach ($models as $model) {
        ModelProbeTask::setCurrent($taskId, $model);
        $result = ModelProbe::probe($channel, $probeKey, $model, $timeout);
        if ($result['result'] === ModelProbe::AUTH_ERROR) {
            ModelProbeTask::abort($taskId, $result['http']);
            exit(2);
        }
        ModelProbeTask::addResult($taskId, $result);
        usleep(100_000);
    }

    ModelProbeTask::finish($taskId);
    exit(0);
} catch (Throwable $e) {
    // 只有「我们确实领到过这个任务」或者「它还在等待执行」时才标注失败。
    // 已经被别的进程领走（running）的任务不归这个进程管，不能去改它的状态。
    if ($taskId > 0 && ($claimed || (string) (ModelProbeTask::find($taskId)['status'] ?? '') === 'pending')) {
        try {
            ModelProbeTask::fail($taskId, $e->getMessage());
        } catch (Throwable) {
        }
    }
    fwrite(STDERR, mb_substr($e->getMessage(), 0, 500) . "\n");
    exit(1);
}
