<?php
/**
 * 后台 · 渠道与密钥池管理
 *
 * 路由：
 *   GET  /admin/channels         渠道列表
 *   GET  /admin/channels/new     新增表单
 *   GET  /admin/channels/edit    编辑表单（?id=N）
 *   POST /admin/channels/save    保存（新增或更新）
 *   POST /admin/channels/delete  删除
 *   POST /admin/channels/test    测活
 *
 * 安全上的两条硬规矩（本模块直接持有上游凭据，必须守住）：
 *
 *   1. **Key 绝不回显到页面**。编辑页的 Key 输入框永远是空的，
 *      留空表示「保持原 Key 不变」。如果把 Key 回显给浏览器，
 *      它就进入了浏览器缓存、可能被 XSS 读取、也可能被截图泄露。
 *
 *   2. **所有变更走 POST + CSRF**，删除与测活也不例外。
 *      用 GET 做删除是经典错误：一个 <img src="/delete?id=1"> 就能删掉数据。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Channel;
use app\common\ChannelKey;
use app\common\Crypto;
use app\common\Csrf;
use app\common\Settings;
use support\Request;
use support\Response;

class ChannelController
{
    private const FLASH_NOTICE = 'channel_notice';
    private const FLASH_TYPE = 'channel_notice_type';

    /**
     * GET /admin/channels —— 渠道列表
     */
    public function index(Request $request): Response
    {
        // 一次性取回所有渠道的密钥池统计，避免在循环里逐个查询（N+1）
        $poolStats = ChannelKey::statsForChannels();

        $channels = [];
        foreach (Channel::all() as $row) {
            $id = (int) $row['id'];
            $pool = $poolStats[$id] ?? ['total' => 0, 'enabled' => 0, 'disabled' => 0, 'exhausted' => 0];

            $channels[] = [
                'id' => $id,
                'name' => (string) $row['name'],
                'typeLabel' => self::typeLabel((string) $row['type']),
                'baseUrl' => (string) $row['base_url'],
                'keyMasked' => Channel::maskedKey($row),
                'modelCount' => count(Channel::modelsOf($row)),
                'models' => Channel::modelsOf($row),
                'priority' => (int) $row['priority'],
                'weight' => (int) $row['weight'],
                'enabled' => (int) $row['status'] === Channel::STATUS_ENABLED,
                'rpmLimit' => (int) $row['rpm_limit'],
                'lastTestAt' => self::formatTime($row['last_test_at'] ?? null),
                'lastTestOk' => $row['last_test_ok'] === null ? null : (int) $row['last_test_ok'] === 1,
                'lastError' => (string) ($row['last_error'] ?? ''),
                // 密钥池信息：官方部署下一个渠道会挂几百把 Key，
                // 列表页必须能一眼看出「还有多少把能用」
                'poolTotal' => $pool['total'],
                'poolEnabled' => $pool['enabled'],
                'poolDisabled' => $pool['disabled'],
            ];
        }

        return view('admin/channels', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'channels' => $channels,
            'notice' => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
            'keyConfigured' => Crypto::isConfigured(),
        ], '');
    }

    /**
     * GET /admin/channels/new —— 新增表单
     */
    public function createForm(Request $request): Response
    {
        return $this->form(null);
    }

    /**
     * GET /admin/channels/edit?id=N —— 编辑表单
     */
    public function editForm(Request $request): Response
    {
        $id = (int) $request->get('id', 0);
        $channel = Channel::find($id);

        if ($channel === null) {
            return $this->back('渠道不存在', 'err');
        }

        return $this->form($channel);
    }

    /**
     * POST /admin/channels/save —— 保存
     */
    public function save(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('id', 0);
        $isUpdate = $id > 0;

        $data = [
            'name' => trim((string) $request->post('name', '')),
            'type' => (string) $request->post('type', ''),
            'base_url' => rtrim(trim((string) $request->post('base_url', '')), '/'),
            'api_key' => trim((string) $request->post('api_key', '')),
            'models' => (string) $request->post('models', ''),
            'priority' => (int) $request->post('priority', 0),
            'weight' => (int) $request->post('weight', 1),
            'status' => (int) $request->post('status', Channel::STATUS_ENABLED),
            'rpm_limit' => (int) $request->post('rpm_limit', 0),
        ];

        // 校验集中在控制器：数据层的职责是存取，不该承担业务规则
        $error = $this->validate($data);
        if ($error !== '') {
            return $this->back($error, 'err');
        }

        // 要写 Key 就必须先有 APP_KEY。
        // 否则 Crypto 会抛异常、页面只看到一个 500 —— 使用者根本猜不到
        // 是「没配加密密钥」这个原因。这里提前拦下并给出可执行的指引。
        if ($data['api_key'] !== '' && !Crypto::isConfigured()) {
            return $this->back(
                '未配置 APP_KEY，无法加密保存 API Key。'
                . '请在服务器的 .env 中加入 APP_KEY（生成方式：php -r "echo bin2hex(random_bytes(32));"），'
                . '然后执行 systemctl restart aqua-api。',
                'err'
            );
        }

        if ($isUpdate) {
            if (Channel::find($id) === null) {
                return $this->back('要更新的渠道不存在', 'err');
            }

            // Key 输入框留空 => 保持原 Key 不变（见类注释第 1 条）
            Channel::update($id, $data, $data['api_key'] !== '');
            $message = "渠道「{$data['name']}」已更新";
        } else {
            $id = Channel::create($data);
            $message = "渠道「{$data['name']}」已创建";
        }

        if ($data['api_key'] === '' && !$isUpdate) {
            $message .= '（未填 Key，可稍后补上）';
        }

        return $this->back($message, 'ok');
    }

    /**
     * POST /admin/channels/delete —— 删除
     */
    public function delete(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('id', 0);
        $channel = Channel::find($id);

        if ($channel === null) {
            return $this->back('渠道不存在', 'err');
        }

        Channel::delete($id);

        return $this->back("渠道「{$channel['name']}」已删除", 'ok');
    }

    /**
     * POST /admin/channels/test —— 测活
     */
    public function test(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('id', 0);
        $result = Channel::test($id);

        return $this->back($result['message'], $result['ok'] ? 'ok' : 'err');
    }

    /**
     * GET /admin/channels/keys?id=N —— 密钥池管理
     *
     * 一个渠道下可能挂几百把 Key，这一页就是运维它的地方：
     * 看还剩多少能用、把失效的停掉、批量粘贴导入、重置限流窗口。
     */
    public function keys(Request $request): Response
    {
        $id = (int) $request->get('id', 0);
        $channel = Channel::find($id);

        if ($channel === null) {
            return $this->back('渠道不存在', 'err');
        }

        // 只显示掩码。完整 Key 绝不进入 HTML —— 这一页渲染的是几百条记录，
        // 一旦把明文送进浏览器，泄露面会被放大几百倍。
        $rows = [];
        foreach (ChannelKey::allForChannel($id) as $row) {
            $rows[] = [
                'id' => (int) $row['id'],
                'masked' => ChannelKey::masked($row),
                'enabled' => (int) $row['status'] === ChannelKey::STATUS_ENABLED,
                'rpmLimit' => (int) $row['rpm_limit'],
                'used' => (int) $row['used_requests'],
                'inWindow' => (int) $row['window_start'] === (int) (floor(time() / 60) * 60),
                'lastUsedAt' => self::formatTime($row['last_used_at'] ?? null),
                'failCount' => (int) $row['fail_count'],
                'lastError' => (string) ($row['last_error'] ?? ''),
            ];
        }

        return view('admin/channel_keys', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'channelId' => $id,
            'channelName' => (string) $channel['name'],
            // 每把密钥的默认限额：优先用渠道自己的设置，没有则用全局默认
            'defaultRpm' => (int) $channel['rpm_limit'] > 0
                ? (int) $channel['rpm_limit']
                : Settings::int('nim.rpm_limit', 40),
            'stats' => ChannelKey::statsForChannel($id),
            'keys' => $rows,
            'notice' => (string) session()->pull(self::FLASH_NOTICE, ''),
            'noticeType' => (string) session()->pull(self::FLASH_TYPE, 'info'),
            'keyConfigured' => Crypto::isConfigured(),
        ], '');
    }

    /**
     * POST /admin/channels/keys/import —— 批量导入密钥（粘贴文本）
     */
    public function keysImport(Request $request): Response
    {
        $id = (int) $request->post('id', 0);
        $channel = Channel::find($id);

        if ($channel === null) {
            return $this->back('渠道不存在', 'err');
        }

        if (!Csrf::check($request->post('_csrf'))) {
            return $this->backToKeys($id, '页面已过期，请重新提交', 'err');
        }

        if (!Crypto::isConfigured()) {
            return $this->backToKeys($id, '未配置 APP_KEY，无法加密保存密钥', 'err');
        }

        // 与 CLI 工具使用同一套切分规则：换行、空格、逗号都算分隔符
        $raw = (string) $request->post('keys', '');
        $parts = array_filter(array_map('trim', preg_split('/[\s,]+/', $raw) ?: []));

        if ($parts === []) {
            return $this->backToKeys($id, '没有解析到任何密钥，请检查粘贴内容', 'err');
        }

        $rpm = (int) $request->post('rpm', 0);
        $stats = ChannelKey::import($id, array_values($parts), max(0, $rpm));

        return $this->backToKeys(
            $id,
            "导入完成：新增 {$stats['added']} 把，已存在跳过 {$stats['skipped']} 把，格式无效 {$stats['invalid']} 条",
            'ok'
        );
    }

    /**
     * POST /admin/channels/keys/toggle —— 启用/停用某把密钥
     *
     * 参数用两个不同的按钮名（enable_id / disable_id）而不是
     * 「key_id + enable 标志」，是为了让几百行密钥表格只需要**一个** form：
     * 按钮的 name/value 会随提交一起带上，而同一个 form 里
     * 无法为不同的按钮设置不同的隐藏字段值。
     */
    public function keysToggle(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $enableId = (int) $request->post('enable_id', 0);
        $disableId = (int) $request->post('disable_id', 0);

        $keyId = $enableId > 0 ? $enableId : $disableId;
        $enable = $enableId > 0;

        $row = ChannelKey::find($keyId);
        if ($row === null) {
            return $this->back('密钥不存在', 'err');
        }

        $channelId = (int) $row['channel_id'];
        ChannelKey::setStatus($keyId, $enable ? ChannelKey::STATUS_ENABLED : ChannelKey::STATUS_DISABLED);

        return $this->backToKeys($channelId, $enable ? '已启用该密钥' : '已停用该密钥', 'ok');
    }

    /**
     * POST /admin/channels/keys/delete —— 删除某把密钥
     */
    public function keysDelete(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $keyId = (int) $request->post('delete_id', 0);
        $row = ChannelKey::find($keyId);

        if ($row === null) {
            return $this->back('密钥不存在', 'err');
        }

        $channelId = (int) $row['channel_id'];
        ChannelKey::delete($keyId);

        return $this->backToKeys($channelId, '已删除该密钥', 'ok');
    }

    /**
     * POST /admin/channels/keys/reset —— 重置该渠道的限流窗口计数
     *
     * 用途：上游抖动导致大量请求失败、把配额「浪费」掉之后，
     * 站长希望立刻把计数清零重新开始，而不必等下一个自然分钟。
     */
    public function keysReset(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->back('页面已过期，请重新提交', 'err');
        }

        $id = (int) $request->post('id', 0);
        if (Channel::find($id) === null) {
            return $this->back('渠道不存在', 'err');
        }

        ChannelKey::resetWindows($id);

        return $this->backToKeys($id, '已重置该渠道全部密钥的限流计数', 'ok');
    }

    /**
     * 渲染新增/编辑表单。
     *
     * @param array<string, mixed>|null $channel 为 null 表示新增
     */
    private function form(?array $channel): Response
    {
        $isEdit = $channel !== null;

        // 供应商预设按「适配器是否已实现」分成两组。
        // 分成两组而不是混在一起，是因为**不能让用户选到用不了的选项** ——
        // 界面会把「预留」这组置灰并注明原因，而不是假装它可用。
        $available = [];
        $reserved = [];
        foreach (Channel::providers() as $key => $provider) {
            $adapter = (string) ($provider['adapter'] ?? '');

            if (Channel::adapterImplemented($adapter)) {
                $available[$key] = $provider;
            } else {
                $reserved[$key] = $provider;
            }
        }

        return view('admin/channel_form', [
            'csrf' => Csrf::token(),
            'siteName' => Settings::siteName(),
            'siteMode' => Settings::siteModeLabel(),
            'isEdit' => $isEdit,
            'id' => $isEdit ? (int) $channel['id'] : 0,
            'name' => $isEdit ? (string) $channel['name'] : '',
            'type' => $isEdit ? (string) $channel['type'] : 'nim',
            'baseUrl' => $isEdit
                ? (string) $channel['base_url']
                : (string) (Channel::providers()['nim']['base_url'] ?? ''),
            'modelsText' => $isEdit ? implode("\n", Channel::modelsOf($channel)) : '',
            'priority' => $isEdit ? (int) $channel['priority'] : 0,
            'weight' => $isEdit ? (int) $channel['weight'] : 1,
            'rpmLimit' => $isEdit ? (int) $channel['rpm_limit'] : 0,
            'enabled' => $isEdit ? (int) $channel['status'] === Channel::STATUS_ENABLED : true,
            // 只把掩码送去页面，绝不下发真实 Key
            'keyMasked' => $isEdit ? Channel::maskedKey($channel) : '',
            'adapters' => Channel::ADAPTERS,
            'providersAvailable' => $available,
            'providersReserved' => $reserved,
            // 解密失败时给出提示，避免站长面对一个「Key 明明存了却报没权限」的谜题
            'keyBroken' => $isEdit && (string) ($channel['api_key_enc'] ?? '') !== '' && Channel::plainKey($channel) === '',
            'keyConfigured' => Crypto::isConfigured(),
        ], '');
    }

    /**
     * 校验表单数据，返回错误信息（空串表示通过）。
     *
     * @param array<string, mixed> $data
     */
    private function validate(array $data): string
    {
        if ($data['name'] === '') {
            return '渠道名称不能为空';
        }

        if (!isset(Channel::ADAPTERS[$data['type']])) {
            return '协议适配器不存在';
        }

        // 只允许选用**已实现**的适配器。
        // 未实现的选了也走不通，与其等到发起请求时才失败，
        // 不如在保存这一刻就拦住，并告诉用户可以改用哪些兼容协议。
        if (!Channel::adapterImplemented($data['type'])) {
            return '该协议适配器尚未实现，暂不能接入。'
                . '请改选「OpenAI 兼容」类协议（多数厂商都支持），或选择已实现的上游。';
        }

        if ($data['base_url'] === '') {
            return '上游地址不能为空';
        }

        // 只允许 http/https：避免把 file:// 之类的协议填进来
        $scheme = strtolower((string) parse_url($data['base_url'], PHP_URL_SCHEME));
        if (!in_array($scheme, ['http', 'https'], true)) {
            return '上游地址必须以 http:// 或 https:// 开头';
        }

        if ($data['priority'] < 0 || $data['priority'] > 9999) {
            return '优先级需在 0 ~ 9999 之间';
        }

        if ($data['weight'] < 0 || $data['weight'] > 1000) {
            return '权重需在 0 ~ 1000 之间';
        }

        if ($data['rpm_limit'] < 0) {
            return '每分钟请求上限不能为负数';
        }

        if (!in_array($data['status'], [Channel::STATUS_ENABLED, Channel::STATUS_DISABLED], true)) {
            return '状态值不合法';
        }

        return '';
    }

    /**
     * 写提示并重定向。
     *
     * @param string $location 重定向目标。默认回渠道列表；
     *        密钥池相关的操作传 '/admin/channels/keys?id=N'。
     */
    private function back(string $message, string $type, string $location = '/admin/channels'): Response
    {
        session()->set(self::FLASH_NOTICE, $message);
        session()->set(self::FLASH_TYPE, $type);

        return response('', 302, ['Location' => $location]);
    }

    /**
     * 回到某个渠道的密钥池页面。
     */
    private function backToKeys(int $channelId, string $message, string $type): Response
    {
        return $this->back($message, $type, '/admin/channels/keys?id=' . $channelId);
    }

    private static function typeLabel(string $type): string
    {
        return Channel::ADAPTERS[$type]['label'] ?? $type;
    }

    private static function formatTime(mixed $timestamp): string
    {
        if (!is_numeric($timestamp) || (int) $timestamp <= 0) {
            return '—';
        }

        return date('m-d H:i', (int) $timestamp);
    }
}
