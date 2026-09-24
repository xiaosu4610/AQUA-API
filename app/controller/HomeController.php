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
        ], '');
    }
}
