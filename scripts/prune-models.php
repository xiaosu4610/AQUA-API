<?php
/**
 * 渠道模型清理：探测某个渠道**到底哪些模型真的能调用**，把不可用的从清单里摘掉
 *
 * 【为什么需要它】
 *   很多上游的 `GET /models` 返回的是**一份通用目录**，不是「你的账号能用的清单」。
 *   NVIDIA NIM 就是典型：列出来 82 个模型，其中大量模型对账号不可调用，
 *   调用时返回 `404 Function 'xxx' Not found for account`。
 *
 *   留着它们的代价是实打实的：
 *     · 模型广场把这些列给访客看 → 用户照着挑一个 → 调用失败 → 以为站有问题
 *     · 转发引擎按清单选路 → 每次都先去撞一个注定 404 的模型，白白多一次往返
 *     · 首页的模型标签云变成一堵墙，真实可用的那些反而被淹没
 *
 *   而上游**没有**「查询我的账号有哪些权限」的接口，所以唯一诚实的办法是**逐个探一次**。
 *
 * 【怎么判定「不可用」—— 这一条是全文最重要的】
 *   只有**能确定**的情况才摘掉，其余一律保留：
 *
 *     HTTP 200                → 可用
 *     HTTP 404                → 不可用（这个地址与鉴权都对，只有模型名对不上）
 *     HTTP 401 / 403          → **立即中止整个探测**（这是 Key 或鉴权的问题，
 *                              继续跑会把所有模型都误判成不可用）
 *     HTTP 429 / 5xx / 超时    → 重试一次；仍失败则**保留**（属于「这次没问出来」）
 *     HTTP 400 / 422          → **保留**（很可能是「这个模型不是对话模型」，
 *                              例如 embedding/rerank 类，拿对话接口探它当然不通过）
 *
 *   还有一道兜底：**如果所有模型都返回 404，拒绝执行** ——
 *   那几乎一定是地址或鉴权配错了，而不是「账号真的一个模型都用不了」。
 *
 * 【为什么默认不写入】
 *   默认是只读的探测与报告（dry run）。要真正下架必须显式加 `--apply`，
 *   而且写入前会把原始清单备份到 runtime/ 下。
 *
 * 【已知限制（不要假装它能做更多）】
 *   本脚本不加载框架，因此**只认渠道 `config` 列里显式写出的鉴权配置**
 *   （auth_type / auth_name / auth_prefix / extra_headers / extra_query / model_map）。
 *   如果某个渠道依赖**适配器内置默认值**来鉴权（例如 Azure 的 `api-key` 头
 *   来自适配器而非渠道配置），请先在后台的「高级配置」里把它显式填出来再跑，
 *   否则探测会因鉴权失败而中止（这正是上面那道中止逻辑要拦住的情况）。
 *
 * 用法：
 *   php scripts/prune-models.php --channel=1                    # 只探测并报告
 *   php scripts/prune-models.php --channel=1 --apply            # 探测后下架不可用的
 *   php scripts/prune-models.php --channel=1 --only=gpt --limit=5   # 只探一部分（试跑）
 *   php scripts/prune-models.php --channel=1 --only=llama --timeout=60  # 大模型冷启动慢时放宽超时
 *
 * 参数：
 *   --channel=<id>   必填，渠道 ID
 *   --apply          真正写入（不传则只报告）
 *   --delay=<毫秒>   每次探测之间停多久，默认 100，避免把上游打急
 *   --limit=<n>      最多探测多少个（试跑用）
 *   --only=<子串>    只探测模型名含该子串的
 *   --timeout=<秒>   单个模型的探测超时，默认 20。大模型冷启动可能要几十秒，
 *                    超时的那批会被判为「无法判定」并**保留**（不据此下架）
 */

declare(strict_types=1);

require __DIR__ . '/_bootstrap.php';

if (PHP_SAPI !== 'cli') {
    fwrite(STDERR, "本脚本只能在命令行运行\n");
    exit(1);
}

/**
 * 判定口径只有一处权威实现。
 *
 * 后台的「检测可用模型」走的是同一个 ModelProbe —— 如果这里再写一份，
 * 两边迟早漂移，出现「后台说可用、CLI 说不可用」这种最难解释的分歧。
 * 本脚本不加载框架，所以直接 require 这个不依赖框架的类
 * （与 price-quote.php 引入 Pricing 是同一个做法）。
 */
require aqua_root() . '/app/common/ModelProbe.php';

use app\common\ModelProbe;

/** 单个模型探测的默认超时（秒）。大模型冷启动可能要几十秒，可用 --timeout 调大 */
const PROBE_TIMEOUT_DEFAULT = 20;

// ═══════════════════════════════════════════════════════════
// 参数
// ═══════════════════════════════════════════════════════════

$args = aqua_args();
$channelId = (int) ($args['channel'] ?? 0);
$apply = isset($args['apply']);
$delayMs = max(0, (int) ($args['delay'] ?? 100));
$limit = max(0, (int) ($args['limit'] ?? 0));
$only = trim((string) ($args['only'] ?? ''));
$modelsArg = trim((string) ($args['models'] ?? ''));
$timeout = max(5, (int) ($args['timeout'] ?? PROBE_TIMEOUT_DEFAULT));

if ($channelId <= 0) {
    fwrite(STDERR, "用法：php scripts/prune-models.php --channel=<渠道ID> [--apply] [--delay=毫秒] [--limit=n] [--only=子串]\n");
    fwrite(STDERR, "不带 --apply 时只探测并报告，不改动任何数据。\n");
    exit(1);
}

$pdo = aqua_pdo();

// ═══════════════════════════════════════════════════════════
// 读渠道与凭据
// ═══════════════════════════════════════════════════════════

$stmt = $pdo->prepare('SELECT * FROM channels WHERE id = ?');
$stmt->execute([$channelId]);
$channel = $stmt->fetch(PDO::FETCH_ASSOC);

if (!$channel) {
    fwrite(STDERR, "找不到渠道 id={$channelId}\n");
    exit(1);
}

$models = json_decode((string) ($channel['models'] ?? ''), true);
$models = is_array($models) ? array_values(array_filter(array_map('strval', $models), static fn ($m) => trim($m) !== '')) : [];

if ($models === []) {
    fwrite(STDERR, "渠道「{$channel['name']}」的模型清单是空的，没有可探测的对象。\n");
    exit(0);
}

// 只取启用中的 Key —— 停用的本来就是坏的，拿它探测会得出错误结论
$stmt = $pdo->prepare('SELECT id, api_key_enc FROM channel_keys WHERE channel_id = ? AND status = 1 ORDER BY id');
$stmt->execute([$channelId]);
$keyRows = $stmt->fetchAll(PDO::FETCH_ASSOC);

if ($keyRows === []) {
    fwrite(STDERR, "渠道「{$channel['name']}」没有启用中的 Key，无法探测。\n");
    exit(1);
}

$appKey = aqua_require_app_key();

$keys = [];
foreach ($keyRows as $row) {
    try {
        $keys[] = aqua_decrypt((string) $row['api_key_enc'], $appKey);
    } catch (Throwable $e) {
        // 单把 Key 解不开就跳过它，不因为一把坏 Key 让整个探测失败
        fwrite(STDERR, "  · 跳过一把无法解密的 Key（id={$row['id']}）：{$e->getMessage()}\n");
    }
}

if ($keys === []) {
    fwrite(STDERR, "渠道「{$channel['name']}」沒有任何可解密的 Key。\n");
    exit(1);
}

// ═══════════════════════════════════════════════════════════
// 渠道配置（只用于报告展示；请求构造与判定都交给共享内核）
// ═══════════════════════════════════════════════════════════

$probeConfig = [];
$rawConfig = (string) ($channel['config'] ?? '');
if ($rawConfig !== '') {
    $decoded = json_decode($rawConfig, true);
    $probeConfig = is_array($decoded) ? $decoded : [];
}
$authType = (string) ($probeConfig['auth_type'] ?? 'bearer');

// 只为在报告里显示「实际会请求哪个地址」。
// 传空 Key：query 型鉴权会把 Key 拼进地址，打印到终端就是泄露
$url = (string) ModelProbe::buildSpec($channel, '', '', $timeout)['url'];

/** 探测用哪把 Key：固定用第一把启用中的。
 *
 *  刻意**不轮换** —— 轮换会让「这个模型能不能用」的结论随 Key 变化而变，
 *  同一份清单两次跑出不同结果，没法解释也没法复核。
 *  固定一把 ⇒ 结论可复现，口径是「以这把 Key 所属账号为准」，
 *  这个限制在报告页头会写出来。 */
$probeKey = $keys[0];

// ═══════════════════════════════════════════════════════════
// 开始探测
// ═══════════════════════════════════════════════════════════

$targets = $models;
if ($only !== '') {
    $targets = array_values(array_filter($models, static fn (string $m): bool => stripos($m, $only) !== false));
}
if ($modelsArg !== '') {
    // 精确点名探测：用于「上一轮判为无法判定的那几个，放宽超时再问一次」
    $explicit = array_flip(array_filter(array_map('trim', explode(',', $modelsArg))));
    $targets = array_values(array_filter($targets, static fn (string $m): bool => isset($explicit[$m])));
}
if ($limit > 0) {
    $targets = array_slice($targets, 0, $limit);
}

if ($targets === []) {
    fwrite(STDERR, "筛选之后没有要探测的模型。\n");
    exit(0);
}

echo "════════════════════════════════════════════════════════\n";
echo "渠道模型探测\n";
echo "════════════════════════════════════════════════════════\n";
printf("渠道      #%d %s（%s）\n", $channelId, $channel['name'], $channel['type']);
printf("地址      %s\n", $url);
printf("鉴权      %s%s\n", $authType, $authType === 'query' ? '（Key 拼在查询串里）' : '');
printf("可用 Key  %d 把（本次固定用第 1 把探测，结论以它所属账号为准）\n", count($keys));
printf("模型      清单共 %d 个，本次探测 %d 个\n", count($models), count($targets));
printf("超时      单个模型 %d 秒\n", $timeout);
printf("模式      %s\n", $apply ? '★ 写入模式（会真的下架）' : '只读探测（不改动数据，加 --apply 才写入）');
echo "────────────────────────────────────────────────────────\n";

/** @var array<string, array{result:string, http:int, note:string}> $results */
$results = [];
$aborted = false;

foreach ($targets as $i => $model) {
    // 判定、重试、model_map、鉴权拼装全部在共享内核里，这里只负责展示
    $probe = ModelProbe::probe($channel, $probeKey, $model, $timeout);
    $verdict = (string) $probe['result'];
    $http = (int) $probe['http'];

    if ($verdict === ModelProbe::AUTH_ERROR) {
        // ★ 立即中止：Key 或鉴权有问题时继续跑，会把所有模型都误判成不可用
        $results[$model] = ['result' => $verdict, 'http' => $http, 'note' => '鉴权失败，已中止'];
        echo "\n✗ 探测中止：上游返回 {$http}（鉴权失败）\n";
        echo "  这把 Key 或渠道的鉴权配置有问题。若拿它继续探测，\n";
        echo "  所有模型都会被误判成「不可用」，因此这里直接停下。\n";
        echo '  上游原始响应：' . trim((string) $probe['summary']) . "\n";
        $aborted = true;
        break;
    }

    $results[$model] = [
        'result' => $verdict,
        'http' => $http,
        'note' => $verdict === ModelProbe::OK ? '' : mb_substr((string) $probe['summary'], 0, 110),
    ];

    printf(
        "  [%3d/%3d] %-52s %s\n",
        $i + 1,
        count($targets),
        mb_strimwidth($model, 0, 52, '…'),
        match (true) {
            $verdict === ModelProbe::OK => '可用',
            $verdict === ModelProbe::NO_ACCESS => '无权限',
            $verdict === ModelProbe::UNROUTABLE => '不可用（疑非对话模型）',
            // 两种「没问出来」分开显示，但都归入保留 ——
            // 区别在站长眼里有意义：限流/超时等一会儿可能就好，400 多半是模型本身不是对话模型
            $http === 0 || $http === 429 || $http >= 500 => '上游异常（保留）',
            default => '无法判定（保留）',
        }
    );

    if ($delayMs > 0) {
        usleep($delayMs * 1000);
    }
}

if ($aborted) {
    echo "\n未改动任何数据。\n";
    exit(2);
}

// ═══════════════════════════════════════════════════════════
// 汇总
// ═══════════════════════════════════════════════════════════

$okList = [];
$deniedList = [];      // 账号无此模型权限（404 权限型）
$unroutableList = [];  // 其它 404（多为非对话模型）
$keepList = [];        // 没问出来的，一律保留

foreach ($results as $model => $info) {
    match ($info['result']) {
        ModelProbe::OK => $okList[] = $model,
        ModelProbe::NO_ACCESS => $deniedList[] = $model,
        ModelProbe::UNROUTABLE => $unroutableList[] = $model,
        default => $keepList[] = $model,
    };
}

// 两种 404 都不可调用，一起下架 —— 但报告里分开列，因为原因的差别对站长有意义：
// 「没权限」要去上游申请，「不是对话模型」则要去给网关加对应能力
$deadList = array_merge($deniedList, $unroutableList);

echo "────────────────────────────────────────────────────────\n";
printf("可用                        %d\n", count($okList));
printf("无权限（账号没有该模型）      %d\n", count($deniedList));
printf("不可用（疑非对话模型）        %d\n", count($unroutableList));
printf("保留（没问出来）             %d\n", count($keepList));
echo "────────────────────────────────────────────────────────\n";

if ($deniedList !== []) {
    echo "\n下架 · 账号无此模型权限：\n";
    foreach ($deniedList as $model) {
        echo "  · {$model}\n";
    }
}

if ($unroutableList !== []) {
    echo "\n下架 · 本线路不可用（多半是非对话模型，例如 embedding / rerank / CLIP）\n";
    echo "  它们不是「你没有权限」，而是**本来就不是对话模型**，拿对话接口探它当然不通过。\n";
    echo "  如果将来网关要支持 embedding 等接口，可以从备份文件里把它们捞回来。\n";
    foreach ($unroutableList as $model) {
        // 附上响应摘要：这一类 404 的形态彼此不同（纯文本 / HTML / 别的 JSON），
        // 把原文列出来，将来真加 embedding 支持时能一眼看出该往哪查
        $note = (string) ($results[$model]['note'] ?? '');
        echo "  · {$model}\n";
        if ($note !== '') {
            echo '      ' . mb_substr($note, 0, 90) . "\n";
        }
    }
}

if ($keepList !== []) {
    echo "\n保留（没问出来，因此不下架）：\n";
    foreach ($keepList as $model) {
        $http = (int) $results[$model]['http'];
        $why = ($http === 0 || $http === 429 || $http >= 500)
            ? '上游异常，重试一次仍未通过'   // 限流/网络/上游故障：过一会儿可能就好了
            : '无法判定';                    // 400/422 等：多半本就不是对话模型
        echo "  · {$model}   （HTTP {$http}，{$why}）\n";
    }
    echo "  说明：冷启动较慢的模型可能只是本次超时。可用\n";
    echo "        --models=<模型名> --timeout=60 单独复探一次再决定。\n";
}

if ($deadList === []) {
    echo "\n没有可下架的模型。\n";
    exit(0);
}

// ═══════════════════════════════════════════════════════════
// 兜底：整份清单都 404 时拒绝执行
// ═══════════════════════════════════════════════════════════

// 只在**整份清单**探测时才启用这道兜底。
// 试跑（--limit / --only / --models）本就会遇到「挑到的这几个恰好都不可用」，
// 那种情况报「地址配错了」是误报
$isFullRun = ($limit === 0 && $only === '' && $modelsArg === '');

if ($isFullRun && count($deadList) === count($targets)) {
    fwrite(STDERR, "\n✗ 整份清单全部返回 404，拒绝执行。\n");
    fwrite(STDERR, "  这几乎一定是**地址或鉴权配错了**，而不是「账号一个模型都用不了」。\n");
    fwrite(STDERR, "  请先确认 base_url 与鉴权方式，再重跑。\n");
    exit(3);
}

if (!$apply) {
    echo "\n（只读模式，未改动数据。确认上面这份名单没问题后，加 --apply 执行下架。）\n";
    exit(0);
}

// ═══════════════════════════════════════════════════════════
// 写入
// ═══════════════════════════════════════════════════════════

$kept = array_values(array_filter(
    $models,
    static fn (string $m): bool => !in_array($m, $deadList, true)
));

// 备份原始清单：下架是数据改动，必须留一条回退的路
$backupDir = aqua_root() . '/runtime';
if (!is_dir($backupDir)) {
    @mkdir($backupDir, 0755, true);
}
$backupFile = $backupDir . '/prune-models-backup-' . $channelId . '-' . date('Ymd-His') . '.json';

$backup = [
    'channel_id' => $channelId,
    'channel_name' => (string) $channel['name'],
    'pruned_at' => time(),
    'original_models' => $models,
    'removed' => $deadList,
    'kept' => $kept,
    'detected_by' => 'scripts/prune-models.php',
];

if (file_put_contents($backupFile, json_encode($backup, JSON_PRETTY_PRINT | JSON_UNESCAPED_UNICODE)) === false) {
    fwrite(STDERR, "✗ 写不了备份文件 {$backupFile}，为安全起见不执行下架\n");
    exit(4);
}

$stmt = $pdo->prepare('UPDATE channels SET models = ?, updated_at = ? WHERE id = ?');
$stmt->execute([json_encode($kept, JSON_UNESCAPED_UNICODE), time(), $channelId]);

echo "\n✓ 已下架 " . count($deadList) . " 个模型\n";
printf("  清单：%d → %d 个\n", count($models), count($kept));
printf("  备份：%s\n", $backupFile);
echo "  回退：把备份文件里的 original_models 写回 channels.models 即可\n";
