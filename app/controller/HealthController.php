<?php
/**
 * 健康检查控制器
 *
 * 用途：供运维、负载均衡、监控系统探活。此端点**不做鉴权**，
 *       因此必须保证它既轻量又不泄露内部信息。
 *
 * 为什么单独抽一个控制器而不是放在业务控制器里：
 *   探活请求频率高且来自外部（可能被恶意轮询），
 *   保持它「不查数据库、不查 Redis、不写日志」是最安全的做法。
 *   依赖项的可用性由后续的「就绪检查」单独承担。
 *
 * 本文件同时作为全项目的编码风格范例：
 *   1. 文件头说明「这个文件是干什么的」以及关键设计取舍；
 *   2. 声明 strict_types，避免隐式类型转换带来的隐蔽 bug；
 *   3. 所有方法显式声明参数类型与返回类型；
 *   4. 关键决策处写清「为什么这样写」，而不只是「这样写」。
 */

declare(strict_types=1);

namespace app\controller;

use support\Request;
use support\Response;
use Workerman\Worker;

class HealthController
{
    /**
     * GET /healthz —— 存活探针
     *
     * 返回体在调试环境与生产环境**故意不同**：
     *   生产环境只回 {"status":"ok"}，
     *   因为暴露精确的 PHP / 框架版本号，等于替攻击者完成了 CVE 匹配的第一步。
     *   调试环境才补充版本信息，方便本地确认跑的是哪个版本。
     */
    public function index(Request $request): Response
    {
        $payload = [
            'status' => 'ok',
        ];

        // 仅在调试模式下补充版本信息
        if (config('app.debug')) {
            $payload['debug_info'] = [
                'app'       => 'aqua-api-php',
                'php'       => PHP_VERSION,
                'workerman' => Worker::VERSION,
                'time'      => date('c'),
            ];
        }

        return json($payload);
    }
}
