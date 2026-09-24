<?php
/**
 * 管理后台控制器（超级管理员）
 *
 * 后台路径约定：
 *   GET  /admin/login   登录页
 *   POST /admin/login   提交登录
 *   POST /admin/logout  退出登录
 *   GET  /admin         仪表盘
 *
 * 几点设计说明：
 *
 * 1. **登录只要密码、不要用户名**。本项目是「超级管理员后台」，
 *    只有一个管理员，省掉用户名字段可以少一处可被枚举的入口。
 *    相应的代价是必须靠「失败锁定」来防撞库（见 app/common/Admin.php）。
 *
 * 2. **所有改配置的动作都用 POST + CSRF 令牌**。
 *    后台能改上游地址、密钥池、计费规则，被 CSRF 利用的后果很重。
 *
 * 3. **登录失败不区分「密码错」和「用户不存在」**（本项目没有用户名，
 *    这一点天然满足），避免给攻击者提供额外信息。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Admin;
use app\common\Csrf;
use app\common\Db;
use app\common\Settings;
use app\middleware\AdminAuth;
use support\Request;
use support\Response;

class AdminController
{
    /**
     * 仪表盘上展示的配置项清单。
     *
     * 只挑「站长最常需要确认」的项，不是把 options 表全量倒出来 ——
     * 配置项会越来越多，全量展示反而看不到重点。
     * 后续做配置编辑页时，按分组展示完整清单。
     */
    private const DASHBOARD_CONFIG_KEYS = [
        'site.name',
        'site.mode',
        'gateway.ttft_timeout',
        'gateway.idle_timeout',
        'gateway.total_timeout',
        'gateway.max_retries',
        'key_pool.default_rpm',
        'session.secure',
    ];

    /** 管理员密码最短长度。与其他校验、脚本重置保持一致 */
    private const MIN_PASSWORD_LENGTH = 8;

    /** 配置来源的中文说明，供仪表盘展示 */
    private const SOURCE_LABELS = [
        'database' => '数据库（后台可改）',
        'env' => '环境变量 .env',
        'default' => '代码默认值',
        'none' => '未配置',
    ];

    /**
     * GET /admin/login —— 登录页
     */
    public function loginPage(Request $request): Response
    {
        // 已登录就不必再看登录页
        if (session()->has(AdminAuth::SESSION_KEY)) {
            return response('', 302, ['Location' => '/admin']);
        }

        return view('admin/login', [
            'csrf'  => Csrf::token(),
            'error' => '',
        ], '');
    }

    /**
     * POST /admin/login —— 提交登录
     */
    public function login(Request $request): Response
    {
        // ① CSRF 校验：令牌对不上说明不是从我们的页面提交的
        if (!Csrf::check($request->post('_csrf'))) {
            return view('admin/login', [
                'csrf'  => Csrf::token(),
                'error' => '页面已过期，请重新提交',
            ], '');
        }

        // ② 校验密码。真实 IP 取自 Nginx 透传的 X-Real-IP，
        //    用于失败日志与锁定记录 —— 直接取 REMOTE_ADDR 会全是 127.0.0.1
        $ip = $request->getRealIp() ?: 'unknown';
        $result = Admin::attempt((string) $request->post('password', ''), $ip);

        if (!$result['ok']) {
            // 失败时重新渲染登录页并带上原因，同时刷新 CSRF 令牌
            return view('admin/login', [
                'csrf'  => Csrf::token(),
                'error' => $result['message'],
            ], '');
        }

        // ③ 登录成功，只往 Session 写一个标记。
        //    额外存一个时间戳，中间件据此判定「登录态是否已超时」
        //    （超时分钟数可在配置页调整）。
        session()->set(AdminAuth::SESSION_KEY, 1);
        session()->set(AdminAuth::LOGIN_AT_KEY, time());

        return response('', 302, ['Location' => '/admin']);
    }

    /**
     * POST /admin/logout —— 退出登录
     */
    public function logout(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return response('', 302, ['Location' => '/admin']);
        }

        // 清空整个会话，而不是只删登录标记 ——
        // 登录后产生的其它临时数据（如表单令牌）也应一并失效
        session()->flush();

        return response('', 302, ['Location' => '/admin/login']);
    }

    /**
     * GET /admin —— 仪表盘
     */
    public function index(Request $request): Response
    {
        $admin = Admin::find();

        return view('admin/dashboard', [
            'csrf'       => Csrf::token(),
            'siteName'   => Settings::siteName(),
            'siteMode'   => Settings::siteModeLabel(),
            'phpVersion' => PHP_VERSION,
            'dbDriver'   => Db::isSqlite() ? 'SQLite' : 'MySQL',
            'debugOn'    => (bool) config('app.debug'),
            // 数据库里存的是 Unix 时间戳，展示前才格式化成本地时间
            'loginAt'    => $this->formatTime($admin['last_login_at'] ?? null),
            'loginIp'    => (string) ($admin['last_login_ip'] ?? '—'),
            'configs'    => $this->dashboardConfigs(),
        ], '');
    }

    /**
     * GET /admin/password —— 修改密码页
     */
    public function passwordPage(Request $request): Response
    {
        return $this->passwordView();
    }

    /**
     * POST /admin/password —— 提交修改密码
     *
     * 必须校验**当前密码**：否则会话一旦被劫持（或管理员忘了锁屏），
     * 攻击者可以直接改掉密码，把真正的管理员锁在门外。
     */
    public function changePassword(Request $request): Response
    {
        if (!Csrf::check($request->post('_csrf'))) {
            return $this->passwordView('页面已过期，请重新提交', 'err');
        }

        $current = (string) $request->post('current_password', '');
        $new = (string) $request->post('new_password', '');
        $confirm = (string) $request->post('confirm_password', '');

        $admin = Admin::find();
        if ($admin === null) {
            return $this->passwordView('管理员尚未初始化', 'err');
        }

        // 注意顺序：先验证当前密码，再校验新密码格式 ——
        // 这样攻击者无法通过「新密码太短」之类的提示来试探当前密码是否正确
        if (!password_verify($current, (string) $admin['password_hash'])) {
            // 复用统一的失败计数与锁定逻辑，避免修改密码接口成为绕过登录锁定的后门
            Admin::attempt($current, $request->getRealIp() ?: 'unknown');

            return $this->passwordView('当前密码不正确', 'err');
        }

        if (strlen($new) < self::MIN_PASSWORD_LENGTH) {
            return $this->passwordView('新密码太短，至少 ' . self::MIN_PASSWORD_LENGTH . ' 位', 'err');
        }

        if ($new !== $confirm) {
            return $this->passwordView('两次输入的新密码不一致', 'err');
        }

        if ($new === $current) {
            return $this->passwordView('新密码不能与当前密码相同', 'err');
        }

        Admin::setPassword($new);

        // 密码已变更，清空会话强制重新登录 ——
        // 如果浏览器里还留着旧会话，等于「改了密码但旧凭证仍然有效」
        session()->flush();

        return response('', 302, ['Location' => '/admin/login']);
    }

    /**
     * 渲染修改密码页。
     */
    private function passwordView(string $notice = '', string $type = 'info'): Response
    {
        return view('admin/password', [
            'csrf'       => Csrf::token(),
            'siteName'   => Settings::siteName(),
            'siteMode'   => Settings::siteModeLabel(),
            'notice'     => $notice,
            'noticeType' => $type,
        ], '');
    }

    /**
     * 把 Unix 时间戳格式化成本地可读时间。
     * 空值或无效值统一返回「—」，避免页面上出现 1970-01-01 这种噪音。
     */
    private function formatTime(mixed $timestamp): string
    {
        if (!is_numeric($timestamp) || (int) $timestamp <= 0) {
            return '—';
        }

        return date('Y-m-d H:i:s', (int) $timestamp);
    }

    /**
     * 组装仪表盘要展示的配置清单（含生效值与来源）。
     *
     * source 与 sourceLabel 都要给：前者是机器可读的类别（'database' / 'env' /
     * 'default'），模板用它决定徽标配色；后者是给人看的中文。
     * 只给中文的话，模板就得拿中文字符串去匹配配色，改一个字就配色失效。
     *
     * @return array<int, array{key: string, value: string, source: string, sourceLabel: string}>
     */
    private function dashboardConfigs(): array
    {
        $rows = [];

        foreach (self::DASHBOARD_CONFIG_KEYS as $key) {
            $source = Settings::source($key);

            $rows[] = [
                'key'    => $key,
                'value'  => $this->stringify(Settings::get($key)),
                'source' => $source,
                // 只有来自数据库的值才允许在后台直接改；
                // 环境变量与默认值需要改文件，这里标出来避免站长白找
                'sourceLabel' => self::SOURCE_LABELS[$source] ?? $source,
            ];
        }

        return $rows;
    }

    /**
     * 把配置值转成便于阅读的字符串。
     * 布尔值转「是/否」而不是 1/0，数组转 JSON。
     */
    private function stringify(mixed $value): string
    {
        return match (true) {
            $value === null => '（空）',
            is_bool($value) => $value ? '是' : '否',
            is_array($value) => (string) json_encode($value, JSON_UNESCAPED_UNICODE),
            default => (string) $value,
        };
    }
}
