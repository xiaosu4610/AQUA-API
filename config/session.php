<?php
/**
 * 会话（Session）配置
 *
 * 后台登录依赖会话，因此这里的安全项必须设置正确：
 *
 * · http_only  = true  —— 禁止 JavaScript 读取会话 Cookie，降低 XSS 窃取会话的风险
 * · same_site  = Lax   —— 跨站请求不携带 Cookie，是防御 CSRF 的第一道门槛
 * · secure     = 生产必须 true —— 会话 Cookie 只允许走 HTTPS 传输，
 *                        否则在 HTTP 上会明文传输会话凭证
 *
 * ⚠️ 关于 secure：
 *   设为 true 后，浏览器**不会**在 http:// 下保存这个 Cookie。
 *   所以本地用 http://127.0.0.1:8787 开发时，必须在 .env 里设置
 *   SESSION_SECURE=false，否则「登录成功但立刻又跳回登录页」——
 *   这个现象很容易被误判成代码 bug，实际只是 Cookie 没被保存。
 *
 * 存储方式：默认用文件（runtime/sessions）。
 * 多机部署时必须换成 redis，否则各机器的会话不互通。
 */

use Webman\Session\FileSessionHandler;
use Webman\Session\RedisSessionHandler;
use Webman\Session\RedisClusterSessionHandler;

// 生产默认要求 HTTPS；本地开发在 .env 里显式关掉
//
// ⚠️ 但有一个例外：**安装阶段必须允许非 HTTPS Cookie**。
//
// 因为安装向导常常是通过 http 打开的（本地 127.0.0.1、或还没配证书的服务器），
// 而安装表单需要 CSRF 令牌 —— 令牌存在会话里，会话又依赖 Cookie。
// 若此时强制 secure=true，浏览器根本不会保存这个 Cookie，
// 提交表单就会一直报「页面已过期」，且原因完全看不出来。
//
// 判断方式刻意保持「零依赖」：只看 .env 在不在，不去查数据库、不加载应用类。
// 理由：config 文件的加载时机很早，此时任何复杂判断都可能引入新的启动期问题。
//    · 没有 .env —— 还没开始装（或正装在装），放行 http Cookie
//    · 有 .env   —— 已部署，按 SESSION_SECURE 的规则来（默认 true）
// 这样既不影响已有部署（它们本来就有 .env），又让新安装能顺利走完。
$installed = is_file(base_path() . DIRECTORY_SEPARATOR . '.env');

$secure = $installed
    ? (filter_var(
        getenv('SESSION_SECURE') ?: 'true',
        FILTER_VALIDATE_BOOL,
        FILTER_NULL_ON_FAILURE
    ) ?? true)
    : false;

return [
    // file = 文件（默认，零依赖）| redis | redis_cluster
    'type' => 'file',

    'handler' => FileSessionHandler::class,

    'config' => [
        'file' => [
            'save_path' => runtime_path() . '/sessions',
        ],
        'redis' => [
            'host' => '127.0.0.1',
            'port' => 6379,
            'auth' => '',
            'timeout' => 2,
            'database' => '',
            'prefix' => 'redis_session_',
        ],
        'redis_cluster' => [
            'host' => ['127.0.0.1:7000', '127.0.0.1:7001', '127.0.0.1:7001'],
            'timeout' => 2,
            'auth' => '',
            'prefix' => 'redis_session_',
        ],
    ],

    // 会话 Cookie 名。带上项目前缀便于在同域多服务时区分
    'session_name' => 'AQUASID',

    'auto_update_timestamp' => false,

    // 服务端会话有效期：7 天
    'lifetime' => 7 * 24 * 60 * 60,

    // 浏览器端 Cookie 有效期：365 天（服务端过期后 Cookie 也无意义，但保留以便延长会话）
    'cookie_lifetime' => 365 * 24 * 60 * 60,

    'cookie_path' => '/',

    // 留空表示仅当前域名。后台只在单一域名下使用，不需要跨子域共享
    'domain' => '',

    'http_only' => true,

    'secure' => $secure,

    'same_site' => 'lax',

    // 会话垃圾回收概率 [分子, 分母]：1/1000 的请求会触发一次过期会话清理
    'gc_probability' => [1, 1000],
];
