<?php
/**
 * 进程定义
 *
 * 这里是 Webman 的「进程模型」配置，两个最关键的点：
 *
 * 1. listen —— 监听地址。
 *    默认**只监听 127.0.0.1**，即应用进程不直接对外，所有外部流量必须经过
 *    Nginx 反向代理（由 Nginx 负责 TLS、域名、限流）。
 *    如果把这里改成 0.0.0.0，就等于绕过 Nginx 把应用裸奔在公网上，
 *    HTTPS 也会失效 —— 这是很常见但很危险的误配。
 *
 * 2. count —— 工作进程数。
 *    Workerman 是「事件循环」模型：**单个进程可以同时服务成百上千条连接**，
 *    所以 count 并不是「最大并发连接数」，而是「并行计算能力」。
 *
 *    但要注意一个前提：只有当代码里的 IO 是**非阻塞**的，单进程才能同时
 *    处理多条连接。如果请求处理器内部用了阻塞式调用（比如直接 curl_exec
 *    去请求上游），那么这个进程在处理期间就无法响应其它请求 ——
 *    此时**真实并发上限就退化成了 count**。
 *
 *    本项目是「长连接流式转发」，绝大部分时间都花在等上游返回上，
 *    因此流式引擎必须基于非阻塞 IO 实现（详见项目开发文档的流式引擎章节）。
 *    在流式引擎落地之前，count 需要给得宽裕一些，以掩盖阻塞调用带来的瓶颈。
 */

use app\process\Http;
use app\process\UserIdMaintainer;
use support\Log;
use support\Request;

global $argv;

return [
    'webman' => [
        'handler' => Http::class,

        // 监听地址：默认仅本机，由 Nginx 反代对外
        'listen' => getenv('HTTP_LISTEN') ?: 'http://127.0.0.1:8787',

        // 工作进程数。默认按 CPU 核数 × 2，可用环境变量覆盖。
        // 取值思路：本项目是 IO 密集型（等待上游），进程数适当多于核数，
        // 可以在部分代码尚未异步化时提供并发余量；
        // 但每多一个进程就多一份内存占用，小内存机器不宜给太大。
        'count' => (int) (getenv('HTTP_WORKER_COUNT') ?: max(2, cpu_count() * 2)),

        // 运行用户/用户组留空，因为本服务由 systemd 以 www-data 身份启动，
        // 不需要 Workerman 自己再做一次降权。
        'user' => '',
        'group' => '',

        'reusePort' => false,
        'eventLoop' => '',
        'context' => [],
        'constructor' => [
            'requestClass' => Request::class,
            'logger' => Log::channel('default'),
            'appPath' => app_path(),
            'publicPath' => public_path(),
        ],
    ],

    // 用户编号维护：每 3 天把注销留下的编号空位补上（详见 app/process/UserIdMaintainer.php）
    // count 固定为 1 —— 重排是搬主键，多进程同时做会互相撞车
    'user_id_maintainer' => [
        'handler' => UserIdMaintainer::class,
        'count' => 1,
        'reloadable' => false,
        'constructor' => [],
    ],

    // 文件变更检测与自动热重载
    // 仅在 Linux 且未以 -d（守护模式）启动时生效，生产环境因此不会加载它。
    'monitor' => [
        'handler' => app\process\Monitor::class,
        'reloadable' => false,
        'constructor' => [
            'monitorDir' => array_merge([
                app_path(),
                config_path(),
                base_path() . '/process',
                base_path() . '/support',
                base_path() . '/resource',
                base_path() . '/.env',
            ], glob(base_path() . '/plugin/*/app'), glob(base_path() . '/plugin/*/config'), glob(base_path() . '/plugin/*/api')),
            'monitorExtensions' => [
                'php', 'html', 'htm', 'env',
            ],
            'options' => [
                'enable_file_monitor' => !in_array('-d', $argv) && DIRECTORY_SEPARATOR === '/',
                'enable_memory_monitor' => DIRECTORY_SEPARATOR === '/',
            ],
        ],
    ],
];
