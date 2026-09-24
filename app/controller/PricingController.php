<?php
/**
 * 后台 · 模型定价（成本与售价）
 *
 * 路由：
 *   GET  /admin/pricing        定价列表
 *   GET  /admin/pricing/new    新增
 *   GET  /admin/pricing/edit   编辑
 *   POST /admin/pricing/save   保存
 *   POST /admin/pricing/delete 删除
 *   POST /admin/pricing/sync   从渠道模型清单批量补齐
 *
 * 为什么这一页与「渠道管理」分开：
 *   渠道回答的是「从哪家上游调」，定价回答的是「这个模型值多少钱」。
 *   同一个模型可能接了好几家上游，而价格只有一套 ——
 *   把价格挂在渠道上会导致同一个模型出现多套互相矛盾的价格。
 *   逐渠道的成本差异，交给渠道高级配置里的「模型名映射」与「上游种类」表达。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Csrf;
use app\common\Pricing;
use app\common\Settings;
use app\common\UsageLog;
use support\Request;
use support\Response;

class PricingController
{
    /** 每页条数。模型可能有几百个，必须有分页 */
    private const PER_PAGE = 50;

    /** Session 中存放提示信息的键 */
    private const FLASH_NOTICE = 'pricing_notice';
    private const FLASH_TYPE = 'pricing_notice_type';

    /**
     * GET /admin/pricing —— 定价列表
     */
    public function index(Request $request): Response
    {
        $keyword = trim((string) $request->get('q', ''));
        $page = max(1, (int) $request->get('page', 1));
        $total = Pricing::count($keyword);
        $pages = max(1, (int) ceil($total / self::PER_PAGE));
        $page = min($page, $pages);

        $rows = [];
        foreach (Pricing::page($keyword, self::PER_PAGE, ($page - 1) * self::PER_PAGE) as $row) {
            $rows[] = $this->toRow($row);
        }

        return view('admin/pricing', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'rows' => $rows,
            'keyword' => $keyword,
            'page' => $page,
            'pages' => $pages,
            'total' => $total,
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'modes' => Pricing::MODES,
            'kinds' => Pricing::UPSTREAM_KINDS,
            // 今日用量汇总，让「成本」这一页同时能看到账面结果
            'today' => UsageLog::summary(strtotime('today') ?: time()),
            'notice' => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
        ], '');
    }

    /**
     * GET /admin/pricing/new —— 新增表单
     */
    public function createForm(Request $request): Response
    {
        return $this->form(null);
    }

    /**
     * GET /admin/pricing/edit?id=N —— 编辑表单
     */
    public function editForm(Request $request): Response
    {
        $row = Pricing::find((int) $request->get('id', 0));

        if ($row === null) {
            return $this->back('要编辑的定价不存在', 'err');
        }

        return $this->form($row);
    }

    /**
     * POST /admin/pricing/save —— 保存（新增或更新）
     */
    public function save(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('id', 0);

        $data = [
            'model' => trim((string) $request->post('model', '')),
            'billing_mode' => (string) $request->post('billing_mode', Pricing::MODE_TOKEN),
            'upstream_kind' => (string) $request->post('upstream_kind', 'official'),
            'upstream_input_price' => (float) $request->post('upstream_input_price', 0),
            'upstream_output_price' => (float) $request->post('upstream_output_price', 0),
            'upstream_call_price' => (float) $request->post('upstream_call_price', 0),
            'downstream_input_price' => (float) $request->post('downstream_input_price', 0),
            'downstream_output_price' => (float) $request->post('downstream_output_price', 0),
            'downstream_call_price' => (float) $request->post('downstream_call_price', 0),
            'price_unit' => (int) $request->post('price_unit', Pricing::DEFAULT_UNIT),
            'note' => trim((string) $request->post('note', '')),
            'status' => (int) $request->post('status', Pricing::STATUS_ENABLED),
        ];

        $error = $this->validate($data, $id);
        if ($error !== '') {
            return $this->back($error, 'err');
        }

        if ($id > 0) {
            Pricing::update($id, $data);
            return $this->back("定价「{$data['model']}」已更新", 'ok');
        }

        Pricing::create($data);

        return $this->back("定价「{$data['model']}」已新增", 'ok');
    }

    /**
     * POST /admin/pricing/delete —— 删除
     */
    public function delete(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $row = Pricing::find((int) $request->post('id', 0));
        if ($row === null) {
            return $this->back('定价不存在', 'err');
        }

        Pricing::delete((int) $row['id']);

        return $this->back("定价「{$row['model']}」已删除（该模型此后按未定价处理）", 'ok');
    }

    /**
     * POST /admin/pricing/sync —— 从各渠道的模型清单补齐定价行
     *
     * 一个渠道测活后能拉回几十上百个模型名，让站长手抄是不现实的。
     * 先批量建行（价格留 0），再挑常用的填价格。
     */
    public function sync(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $added = Pricing::syncFromChannels();

        if ($added === 0) {
            return $this->back('没有需要补齐的模型（渠道里的模型都已有定价行）', 'info');
        }

        return $this->back("已从渠道模型清单补齐 {$added} 个模型的定价行，价格均为 0，请按需填写", 'ok');
    }

    // ═══════════════════════════════════════════════════════════
    // 内部
    // ═══════════════════════════════════════════════════════════

    /**
     * 渲染新增/编辑表单。
     *
     * @param array<string, mixed>|null $row
     */
    private function form(?array $row): Response
    {
        $isEdit = $row !== null;

        return view('admin/pricing_form', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'isEdit' => $isEdit,
            'id' => $isEdit ? (int) $row['id'] : 0,
            'model' => $isEdit ? (string) $row['model'] : '',
            'billingMode' => $isEdit ? (string) $row['billing_mode'] : Pricing::MODE_TOKEN,
            'upstreamKind' => $isEdit ? (string) $row['upstream_kind'] : 'official',
            'upstreamInput' => $isEdit ? self::price($row['upstream_input_price']) : '0',
            'upstreamOutput' => $isEdit ? self::price($row['upstream_output_price']) : '0',
            'upstreamCall' => $isEdit ? self::price($row['upstream_call_price']) : '0',
            'downstreamInput' => $isEdit ? self::price($row['downstream_input_price']) : '0',
            'downstreamOutput' => $isEdit ? self::price($row['downstream_output_price']) : '0',
            'downstreamCall' => $isEdit ? self::price($row['downstream_call_price']) : '0',
            'priceUnit' => $isEdit ? (int) $row['price_unit'] : Pricing::DEFAULT_UNIT,
            'note' => $isEdit ? (string) ($row['note'] ?? '') : '',
            'enabled' => $isEdit ? (int) $row['status'] === Pricing::STATUS_ENABLED : true,
            'modes' => Pricing::MODES,
            'kinds' => Pricing::UPSTREAM_KINDS,
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'defaultMultiplier' => Settings::float('billing.default_multiplier', 1.0),
        ], '');
    }

    /**
     * 把一行定价整理成列表页需要的形状。
     *
     * 「参考倍率」用 100 万输入 + 100 万输出的假想用量算出来：
     * 单个数字就能让人看出「这个模型在赚还是在赔、赚多少」，
     * 而逐个去看 6 个价格字段是看不出来的。
     *
     * @param array<string, mixed> $row
     * @return array<string, mixed>
     */
    private function toRow(array $row): array
    {
        $unit = (int) $row['price_unit'];
        $quote = Pricing::quote($row, $unit, $unit, Settings::float('billing.default_multiplier', 1.0));

        $ratio = null;
        if ($quote['upstream_cost'] > 0) {
            $ratio = $quote['downstream_cost'] / $quote['upstream_cost'];
        }

        return [
            'id' => (int) $row['id'],
            'model' => (string) $row['model'],
            'mode' => (string) $row['billing_mode'],
            'modeLabel' => Pricing::MODES[$row['billing_mode']]['label'] ?? (string) $row['billing_mode'],
            'kind' => (string) $row['upstream_kind'],
            'kindLabel' => Pricing::UPSTREAM_KINDS[$row['upstream_kind']]['label'] ?? (string) $row['upstream_kind'],
            'unit' => $unit,
            'upstreamInput' => self::price($row['upstream_input_price']),
            'upstreamOutput' => self::price($row['upstream_output_price']),
            'upstreamCall' => self::price($row['upstream_call_price']),
            'downstreamInput' => self::price($row['downstream_input_price']),
            'downstreamOutput' => self::price($row['downstream_output_price']),
            'downstreamCall' => self::price($row['downstream_call_price']),
            // 以「每 unit 输入 + 每 unit 输出」为参考量的成本与售价
            'refUpstream' => $quote['upstream_cost'],
            'refDownstream' => $quote['downstream_cost'],
            'refProfit' => $quote['profit'],
            'ratio' => $ratio,
            'enabled' => (int) $row['status'] === Pricing::STATUS_ENABLED,
            'note' => (string) ($row['note'] ?? ''),
        ];
    }

    /**
     * 校验定价数据，返回错误信息（空串表示通过）。
     *
     * @param array<string, mixed> $data
     */
    private function validate(array $data, int $id): string
    {
        if ($data['model'] === '') {
            return '模型名不能为空';
        }

        if (!isset(Pricing::MODES[$data['billing_mode']])) {
            return '计费模式不存在';
        }

        if (!isset(Pricing::UPSTREAM_KINDS[$data['upstream_kind']])) {
            return '上游种类不存在';
        }

        if ($data['price_unit'] < 1) {
            return '计价单位必须大于 0（常见写法：每 100 万 token 填 1000000）';
        }

        foreach (['upstream_input_price', 'upstream_output_price', 'upstream_call_price',
                  'downstream_input_price', 'downstream_output_price', 'downstream_call_price'] as $field) {
            if ($data[$field] < 0) {
                return '价格不能为负数';
            }
        }

        // 重名检查：model 上有唯一约束，不先拦下就会抛一个数据库层的异常给站长看
        $exists = Pricing::findByModelAnyStatus($data['model']);
        if ($exists !== null && (int) $exists['id'] !== $id) {
            return "模型「{$data['model']}」已经有定价了。同一个模型只能有一条定价 —— "
                . '如果是同一模型接了多家上游、成本不同，请按主要线路估价，'
                . '或在渠道高级配置里用「模型名映射」把名字区分开';
        }

        return '';
    }

    /**
     * 价格数值 → 可编辑的字符串。
     *
     * 去掉无意义的小数尾巴（0.0000000000 → 0），
     * 否则表单里一长串 0 既难读也容易改错。
     */
    private static function price(mixed $value): string
    {
        $float = (float) $value;

        if ($float === 0.0) {
            return '0';
        }

        return rtrim(rtrim(number_format($float, 10, '.', ''), '0'), '.');
    }

    /**
     * 写入提示并重定向回定价列表（POST → 重定向 → GET）。
     */
    private function back(string $message, string $type): Response
    {
        session()->set(self::FLASH_NOTICE, $message);
        session()->set(self::FLASH_TYPE, $type);

        return response('', 302, ['Location' => '/admin/pricing']);
    }
}
