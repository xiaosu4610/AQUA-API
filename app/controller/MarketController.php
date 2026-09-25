<?php
/**
 * 模型广场（公开页）
 *
 * 它要回答访客在注册之前最关心的一件事：**这里有什么模型、多少钱。**
 * 这是中转站类产品转化率最高的一个页面 ——
 * 让人先看清楚再决定要不要注册，比先要求注册再看清单有效得多。
 *
 * ═══ 三条诚实原则 ═══
 *
 * 1）**只列真的能调用的模型。**
 *    停用渠道支持的模型、以及「未定价 + 站长不允许未定价放行」的模型，
 *    列出来只会让用户白试一次。宁可比别人少列，也不要列不能用的。
 *
 * 2）**只显示售价，绝不显示上游成本与毛利。**
 *    成本是站长的经营信息。售价可以乘倍率算出，但不能把成本直接摆出来 ——
 *    否则等于把进价告诉顾客。
 *
 * 3）**价格一律复用 Pricing::quote()。**
 *    不在这里另写一套价格逻辑，否则「页面显示的价格」和「实际扣费的价格」
 *    迟早会出现不一致，而那种不一致是最难排查的一类问题。
 *
 * @var 见 index()
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Channel;
use app\common\Pricing;
use app\common\Settings;
use support\exception\PageNotFoundException;
use support\Request;
use support\Response;

class MarketController
{
    /** 每页多少个模型 */
    private const PER_PAGE = 30;

    /** 参与筛选与排序的模型数上限，防止极端情况把内存吃光 */
    private const MAX_SCAN = 3000;

    /**
     * GET /models —— 模型广场
     */
    public function index(Request $request): Response
    {
        // 站长可以整体关掉公开模型清单。关掉时这条路就不该存在 ——
        // 返回一个空页面会让人以为「这个站没有模型」，那是错误信息
        if (!Settings::bool('site.show_models', true)) {
            throw new PageNotFoundException();
        }

        $multiplier = Settings::float('billing.default_multiplier', 1.0);
        $unpricedIsFree = Settings::bool('billing.unpriced_is_free', true);
        $currency = (string) Settings::get('billing.currency', 'CNY');
        $mode = (string) Settings::get('site.mode', 'commercial');

        // ── 汇总可用模型 ──
        // 只统计**已启用**渠道。被停用的渠道所支持的模型对外并不存在
        $rows = [];
        foreach ($this->availableModels() as $model) {
            $pricing = Pricing::findByModel($model);

            // 未定价 且 站长不允许未定价放行 —— 调用会被拒，所以不列
            if ($pricing === null && !$unpricedIsFree) {
                continue;
            }

            $rows[] = $this->describe($model, $pricing, $multiplier);
        }

        usort($rows, static fn (array $a, array $b): int => strnatcasecmp($a['model'], $b['model']));

        // ── 筛选与排序（在 PHP 里做）──
        // 模型总数是「几十到几千」这个量级，一次全取回来再筛，
        // 比拼接动态 SQL 简单得多，也不会因为条件组合写出慢查询
        $filters = $this->readFilters($request);
        $filtered = $this->applyFilters($rows, $filters);
        $filtered = $this->applySort($filtered, $filters['sort']);

        // ── 分页 ──
        $total = count($filtered);
        $pages = max(1, (int) ceil($total / self::PER_PAGE));
        $page = min(max(1, $filters['page']), $pages);
        $offset = ($page - 1) * self::PER_PAGE;
        $pageRows = array_slice($filtered, $offset, self::PER_PAGE);

        return view('market', [
            'siteName' => (string) Settings::get('site.name', 'aqua-api-php'),
            'modeLabel' => $mode === 'public_welfare' ? '公益站' : '商业站',
            'isWelfare' => $mode === 'public_welfare',
            'description' => trim((string) Settings::get('site.description', '')),
            'loggedIn' => AuthController::currentUserId() > 0,
            'showModels' => true,
            'currency' => $currency,

            'rows' => $pageRows,
            'page' => $page,
            'pages' => $pages,
            'perPage' => self::PER_PAGE,
            'total' => $total,
            'allTotal' => count($rows),

            'filters' => $filters,
            'vendors' => $this->vendors($rows),

            // 汇总卡：都要来自真实数据，没有就如实为 0
            'freeCount' => count(array_filter($rows, static fn (array $r): bool => $r['isFree'])),
            'callModes' => $this->modeCounts($rows),
        ], '');
    }

    // ═══════════════════════════════════════════════════════════
    // 取数
    // ═══════════════════════════════════════════════════════════

    /**
     * 已启用渠道所支持的模型名清单。
     *
     * @return array<int, string>
     */
    private function availableModels(): array
    {
        $seen = [];

        foreach (Channel::all() as $channel) {
            if ((int) $channel['status'] !== Channel::STATUS_ENABLED) {
                continue;
            }

            foreach (Channel::modelsOf($channel) as $model) {
                $model = trim((string) $model);
                if ($model !== '') {
                    $seen[$model] = true;
                }
            }

            if (count($seen) >= self::MAX_SCAN) {
                break;
            }
        }

        return array_keys($seen);
    }

    /**
     * 把一个模型整理成「可以用来展示的一行」。
     *
     * 价格全部走 Pricing::quote()：
     *   传 (unit, 0) 算出的是「每 unit 输入的售价」，
     *   传 (0, unit) 算出的是「每 unit 输出的售价」。
     * 这样四种计费口径（token / call / subscription / free）都能自动兼容，
     * 页面不需要知道里面的规则。
     *
     * @param array<string, mixed>|null $pricing
     * @return array<string, mixed>
     */
    private function describe(string $model, ?array $pricing, float $multiplier): array
    {
        // 厂商取模型名的前缀（`openai/gpt-oss-20b` → openai）。
        // 没有前缀的归入「其他」—— 不猜，猜错了会把模型归到别的厂商名下
        $vendor = '';
        $slash = strpos($model, '/');
        if ($slash !== false && $slash > 0) {
            $vendor = substr($model, 0, $slash);
        }

        $row = [
            'model' => $model,
            'vendor' => $vendor,
            'vendorLabel' => $vendor !== '' ? $vendor : '其他',
            'mode' => '',
            'modeLabel' => '',
            'inPrice' => null,
            'outPrice' => null,
            'callPrice' => null,
            'unit' => Pricing::DEFAULT_UNIT,
            'sortPrice' => 0.0,
            'isFree' => false,
            'note' => '',
        ];

        // 没有定价行：当前不收费（因为 unpriced_is_free 为真，否则上面已被过滤掉）
        if ($pricing === null) {
            $row['mode'] = 'unpriced';
            $row['modeLabel'] = '未定价';
            $row['isFree'] = true;
            $row['note'] = '当前不收费';

            return $row;
        }

        $mode = (string) ($pricing['billing_mode'] ?? Pricing::MODE_TOKEN);
        $unit = max(1, (int) ($pricing['price_unit'] ?? Pricing::DEFAULT_UNIT));

        $row['mode'] = $mode;
        $row['modeLabel'] = Pricing::MODES[$mode]['label'] ?? $mode;
        $row['unit'] = $unit;

        if ($mode === Pricing::MODE_FREE) {
            $row['isFree'] = true;
            $row['note'] = '免费';

            return $row;
        }

        if ($mode === Pricing::MODE_CALL) {
            $call = Pricing::quote($pricing, $unit, 0, $multiplier);
            $row['callPrice'] = (float) $call['downstream_cost'];
            $row['sortPrice'] = $row['callPrice'];

            // 配了「按次」但每次的价格是 0 —— 等同于没定价。
            // 不标出来会出现「按次 · 每次 0」这种自相矛盾的显示
            if ($row['callPrice'] <= 0) {
                $row['isFree'] = true;
            }

            return $row;
        }

        // 按 Token 与订阅制：都按「输入 / 输出」两个单价展示
        $in = Pricing::quote($pricing, $unit, 0, $multiplier);
        $out = Pricing::quote($pricing, 0, $unit, $multiplier);
        $row['inPrice'] = (float) $in['downstream_cost'];
        $row['outPrice'] = (float) $out['downstream_cost'];
        $row['sortPrice'] = $row['inPrice'] + $row['outPrice'];

        if ($row['inPrice'] <= 0 && $row['outPrice'] <= 0) {
            $row['isFree'] = true;
        }

        return $row;
    }

    // ═══════════════════════════════════════════════════════════
    // 筛选与排序
    // ═══════════════════════════════════════════════════════════

    /**
     * @return array{q:string, vendor:string, mode:string, sort:string, page:int}
     */
    private function readFilters(Request $request): array
    {
        $allowSort = ['name', 'price_asc', 'price_desc'];

        $sort = (string) $request->get('sort', 'name');

        return [
            'q' => mb_substr(trim((string) $request->get('q', '')), 0, 80),
            'vendor' => mb_substr(trim((string) $request->get('vendor', '')), 0, 60),
            'mode' => trim((string) $request->get('mode', '')),
            'sort' => in_array($sort, $allowSort, true) ? $sort : 'name',
            'page' => max(1, (int) $request->get('page', 1)),
        ];
    }

    /**
     * @param array<int, array<string, mixed>> $rows
     * @param array<string, mixed>             $filters
     * @return array<int, array<string, mixed>>
     */
    private function applyFilters(array $rows, array $filters): array
    {
        if ($filters['q'] !== '') {
            $needle = mb_strtolower($filters['q']);
            $rows = array_filter(
                $rows,
                static fn (array $r): bool => mb_strpos(mb_strtolower($r['model']), $needle) !== false
            );
        }

        if ($filters['vendor'] !== '') {
            $rows = array_filter($rows, static fn (array $r): bool => $r['vendorLabel'] === $filters['vendor']);
        }

        if ($filters['mode'] !== '') {
            $rows = array_filter($rows, static fn (array $r): bool => $r['mode'] === $filters['mode']);
        }

        return array_values($rows);
    }

    /**
     * @param array<int, array<string, mixed>> $rows
     * @return array<int, array<string, mixed>>
     */
    private function applySort(array $rows, string $sort): array
    {
        if ($sort === 'price_asc') {
            usort($rows, static function (array $a, array $b): int {
                return [$a['sortPrice'], $a['model']] <=> [$b['sortPrice'], $b['model']];
            });
        } elseif ($sort === 'price_desc') {
            usort($rows, static function (array $a, array $b): int {
                return [$b['sortPrice'], $a['model']] <=> [$a['sortPrice'], $b['model']];
            });
        }

        return $rows;
    }

    /**
     * 可选的厂商清单（只列真实存在的），并统计各计费方式的数量。
     *
     * @param array<int, array<string, mixed>> $rows
     * @return array<int, string>
     */
    private function vendors(array $rows): array
    {
        $vendors = [];
        foreach ($rows as $row) {
            $vendors[$row['vendorLabel']] = true;
        }

        $list = array_keys($vendors);
        // 「其他」永远排在最后 —— 它是一个兜底桶，不是真的厂商
        usort($list, static function (string $a, string $b): int {
            if ($a === '其他') {
                return 1;
            }
            if ($b === '其他') {
                return -1;
            }

            return strnatcasecmp($a, $b);
        });

        return $list;
    }

    /**
     * @param array<int, array<string, mixed>> $rows
     * @return array<string, int>
     */
    private function modeCounts(array $rows): array
    {
        $counts = [];
        foreach ($rows as $row) {
            $counts[(string) $row['mode']] = ($counts[(string) $row['mode']] ?? 0) + 1;
        }

        return $counts;
    }
}
