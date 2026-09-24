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
        'nim.base_url',
        'nim.rpm_limit',
        'nim.ttft_timeout',
        'nim.idle_timeout',
        'nim.total_timeout',
        'session.secure',
    ];

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
        //    额外存一个时间戳，方便将来做「闲置超时自动登出」。
        session()->set(AdminAuth::SESSION_KEY, 1);
        session()->set('admin_login_at', time());

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
            'siteName'   => (string) Settings::get('site.name', 'aqua-api-php'),
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
     * @return array<int, array{key: string, value: string, source: string}>
     */
    private function dashboardConfigs(): array
    {
        $rows = [];

        foreach (self::DASHBOARD_CONFIG_KEYS as $key) {
            $source = Settings::source($key);

            $rows[] = [
                'key'    => $key,
                'value'  => $this->stringify(Settings::get($key)),
                'source' => self::SOURCE_LABELS[$source] ?? $source,
                // 只有来自数据库的值才允许在后台直接改；
                // 环境变量与默认值需要改文件，这里标出来避免站长白找
                'editable' => $source === 'database',
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
