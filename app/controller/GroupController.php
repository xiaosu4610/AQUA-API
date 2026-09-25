<?php
/**
 * 后台 · 线路分组（分组管理 + 额度看板）
 *
 * 路由：
 *   GET  /admin/groups             分组列表 + 额度看板
 *   POST /admin/groups/save        新建 / 修改分组
 *   POST /admin/groups/delete      删除空分组
 *   POST /admin/groups/budget      给某把密钥追加额度（含自动恢复启用）
 *   POST /admin/groups/reconcile   读上游累计用量，与本地记账并排对照
 *
 * ═══ 为什么「额度看板」放在这个页面，而不是单独一页 ═══
 *
 * 站长的原话是「哪条线在烧我的钱、还剩多少」。这两个问题必须一起回答：
 * 分组告诉他「这条线是什么」，额度告诉他「这条线还能撑多久」。
 * 分成两页会迫使他来回对照「分组 id 对应哪条线」，而这一页直接把
 * 分组名、渠道名、密钥掩码、剩余额度、消耗速率、预计可用天数并排放好。
 *
 * ═══ 为什么额度是「本地记账」而不是「读上游余额」═══
 *
 * 实测两家上游都没有「按 API Key 读余额」的接口（详见 ChannelKey::addCost 的说明），
 * 所以余额 = 人工录入的额度 − 本站按真实 usage 累加的成本。
 * 这不仅是唯一可行的方案，也顺带给出了一个上游给不了的数字：**消耗速率**。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Channel;
use app\common\ChannelKey;
use app\common\Csrf;
use app\common\Group;
use app\common\Settings;
use support\Log;
use support\Request;
use support\Response;

class GroupController
{
    private const FLASH_NOTICE = 'group_notice';
    private const FLASH_TYPE = 'group_notice_type';

    /**
     * GET /admin/groups
     */
    public function index(Request $request): Response
    {
        $editId = (int) $request->get('edit', 0);
        $editing = $editId > 0 ? Group::find($editId) : null;

        return view('admin/groups', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'groups' => Group::overview(),
            'budgetRows' => ChannelKey::budgetRows(),
            'editing' => $editing,
            'budgetWarnRatio' => ChannelKey::BUDGET_WARN_RATIO,
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'notice' => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
        ], '');
    }

    /**
     * POST /admin/groups/save —— 新建或修改
     */
    public function save(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('id', 0);

        $data = [
            'code' => trim((string) $request->post('code', '')),
            'label' => trim((string) $request->post('label', '')),
            'description' => trim((string) $request->post('description', '')),
            'cost_mode' => (string) $request->post('cost_mode', Group::COST_FREE),
            'price_mode' => (string) $request->post('price_mode', Group::PRICE_PRICED),
            'visible' => $request->post('visible', '0') === '1',
            'default_visible' => $request->post('default_visible', '0') === '1',
            'sort' => (int) $request->post('sort', 0),
            'status' => $request->post('status', '1') === '1',
        ];

        $result = $id > 0 ? Group::update($id, $data) : Group::create($data);

        if (!$result['ok']) {
            return $this->back($result['message'], 'err');
        }

        // 可见性变了要清缓存，否则前台要等 5 秒才生效 ——
        // 站长点完「保存」后会立刻去前台看一眼，那时看到的必须是新状态
        Group::forget();

        return $this->back($result['message'], 'ok');
    }

    /**
     * POST /admin/groups/delete
     */
    public function delete(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $result = Group::delete((int) $request->post('id', 0));
        Group::forget();

        return $this->back($result['message'], $result['ok'] ? 'ok' : 'err');
    }

    /**
     * POST /admin/groups/budget —— 追加额度
     */
    public function addBudget(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('key_id', 0);
        $amount = (float) $request->post('amount', 0);
        $note = trim((string) $request->post('note', ''));

        $result = ChannelKey::addBudget($id, $amount, $note);

        if ($result['ok']) {
            Log::warning(sprintf('管理员给密钥池 #%d 追加了 %s 元额度', $id, $amount));
        }

        return $this->back($result['message'], $result['ok'] ? 'ok' : 'err');
    }

    /**
     * POST /admin/groups/reconcile —— 读上游累计用量（对账）
     *
     * 只读，不改本地账目：它存在的意义是「让我们知道自己算得对不对」。
     * 上游没有这个接口的（硅基流动）会明确回一句「上游没有这个接口」，
     * 而不是编一个数字。
     */
    public function reconcile(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('key_id', 0);
        $row = ChannelKey::find($id);

        if ($row === null) {
            return $this->back('密钥不存在', 'err');
        }

        $channel = Channel::find((int) $row['channel_id']);

        if ($channel === null) {
            return $this->back('这把密钥所属的渠道已被删除，无法读取上游数据', 'err');
        }

        $plain = ChannelKey::plainKey($row);
        $reported = Channel::readUpstreamUsage($channel, $plain);

        if ($reported === null) {
            return $this->back(
                '上游没有提供这个接口（TierFlow 有 /v1/dashboard/billing/usage，硅基流动没有），'
                . '所以这一把只能靠本地记账',
                'info'
            );
        }

        ChannelKey::setReported($id, $reported);

        $used = (float) $row['budget_used'];

        return $this->back(sprintf(
            '上游读数 %s，本地记账 %s（差额 %s）。%s',
            self::num($reported),
            self::num($used),
            self::num($reported - $used),
            abs($reported - $used) <= max(0.01, $used * 0.01)
                ? '两边基本一致，计价口径可信'
                : '两边差距较大：请核对单价与时段设置（上游单位可能是美元，若如此，差额需按汇率换算后再看）'
        ), 'ok');
    }

    // ═══════════════════════════════════════════════════════════
    // 内部
    // ═══════════════════════════════════════════════════════════

    private static function num(float $value): string
    {
        if ($value == 0.0) {
            return '0';
        }

        return rtrim(rtrim(number_format($value, 6, '.', ''), '0'), '.');
    }

    private function back(string $message, string $type): Response
    {
        session()->set(self::FLASH_NOTICE, $message);
        session()->set(self::FLASH_TYPE, $type);

        return response('', 302, ['Location' => '/admin/groups']);
    }
}
