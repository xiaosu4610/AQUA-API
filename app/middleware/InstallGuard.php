<?php
/**
 * 安装守卫
 *
 * 作用：**还没安装时，把访客导向安装向导**。
 *
 * 为什么需要它：新部署的系统在未安装状态下，后台会显示
 * 「后台尚未初始化，请设置 ADMIN_PASSWORD」这类只有运维才看得懂的提示，
 * 而普通用户会以为站点坏了。安装向导才是这个阶段唯一该看到的东西。
 *
 * 为什么也放行 /healthz：装没装好，探活接口都应该能回答 ——
 * 否则监控会把「未安装」误报成「服务挂了」。
 */

declare(strict_types=1);

namespace app\middleware;

use app\common\InstallState;
use Webman\Http\Request;
use Webman\Http\Response;
use Webman\MiddlewareInterface;

class InstallGuard implements MiddlewareInterface
{
    /**
     * 未安装时也放行的路径前缀。
     * 安装程序自身当然要放行，否则会自己把自己重定向成死循环。
     */
    private const ALLOW_PREFIXES = ['/install', '/healthz', '/favicon'];

    public function process(Request $request, callable $handler): Response
    {
        $path = $request->path();

        foreach (self::ALLOW_PREFIXES as $prefix) {
            if (str_starts_with($path, $prefix)) {
                return $handler($request);
            }
        }

        if (InstallState::isInstalled()) {
            return $handler($request);
        }

        return redirect('/install');
    }
}
