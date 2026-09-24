<?php
/**
 * 站点首页
 *
 * 这是一个**公开**页面（不需要登录），定位是「站点门户 + 运行状态」：
 *   · 让访客/站长一眼确认服务是活的；
 *   · 给出进入管理后台的入口；
 *   · 将来这里会承载公益站的「用量公示」（可用模型、今日调用量等），
 *     这是公开透明的落点，所以从一开始就做成公开页面而不是重定向到后台。
 *
 * 安全取舍：**不展示 PHP 版本、框架版本等内部信息**。
 * 首页是任何人可访问的，暴露精确版本号等于替攻击者完成 CVE 匹配的第一步。
 * 需要看版本信息请登录后台（后台仪表盘有）。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Epay;
use app\common\Settings;
use support\Request;
use support\Response;

class HomeController
{
    /**
     * GET / —— 站点首页
     */
    public function index(Request $request): Response
    {
        $mode = (string) Settings::get('site.mode', 'commercial');

        return view('home', [
            'siteName' => (string) Settings::get('site.name', 'aqua-api-php'),
            // 站点模式用中文展示，便于访客理解这个站点是商业站还是公益站
            'modeLabel' => $mode === 'public_welfare' ? '公益站' : '商业站',
            'isWelfare' => $mode === 'public_welfare',
            // 以下四项都可在后台配置。简介留空时由模板回落到内置文案，
            // 而不是在这里拼一段默认值 —— 默认文案属于展示层
            'description' => trim((string) Settings::get('site.description', '')),
            'announcement' => trim((string) Settings::get('site.announcement', '')),
            'icp' => trim((string) Settings::get('site.icp', '')),
            'footer' => trim((string) Settings::get('site.footer', '')),
            // 用户侧入口：已登录直接给「控制台」，否则给「登录 / 注册」。
            // 注册开关由站长控制，没开就不显示注册入口
            'loggedIn' => \app\controller\AuthController::currentUserId() > 0,
            'registerOpen' => Settings::bool('register.open', false),
            'rechargeEnabled' => Epay::enabled(),
        ], '');
    }
}
