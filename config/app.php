<?php
/**
 * 应用级配置
 *
 * 约定（全项目通用）：
 *   凡是「会随部署环境变化」的配置，一律从环境变量读取（见 .env.example）；
 *   只有与部署环境无关的常量，才硬编码在本文件里。
 *   这样做的好处是：同一份代码可以在开发机、测试机、生产机上直接复用。
 */

use support\Request;

// 先取出调试开关，后面几项都依赖它。
// getenv() 能读到值，是因为 Webman 在 support/bootstrap.php 里已用 Dotenv
// 加载过 .env（前提是 vendor 里存在 vlucas/phpdotenv）。
$debug = filter_var(getenv('APP_DEBUG') ?: 'false', FILTER_VALIDATE_BOOL);

return [
    // 调试开关。为 true 时框架会把详细异常信息返回给客户端。
    // ⚠️ 生产环境必须为 false。
    'debug' => $debug,

    // 错误级别：
    //   调试时全开，便于发现问题；
    //   生产时屏蔽废弃/通知级别，避免日志被噪音淹没（真正的问题反而看不见）。
    'error_reporting' => $debug
        ? E_ALL
        : E_ALL & ~E_DEPRECATED & ~E_NOTICE,

    // 时区。影响日志时间戳与所有日期计算。
    'default_timezone' => getenv('APP_TIMEZONE') ?: 'Asia/Shanghai',

    'request_class' => Request::class,
    'public_path' => base_path() . DIRECTORY_SEPARATOR . 'public',
    'runtime_path' => base_path(false) . DIRECTORY_SEPARATOR . 'runtime',
    'controller_suffix' => 'Controller',

    // 控制器实例是否跨请求复用。
    //
    // 这里保持 false（每请求新建实例），原因是本项目的运行模型是
    // 「常驻内存 + 长连接流式转发」：一旦复用实例，写在控制器属性上的
    // 任何状态都会残留到下一个请求，造成串数据（比如 A 用户的额度算到 B 用户头上）。
    //
    // 同理，本项目的业务对象一律不允许跨请求共享可变状态，
    // 需要共享的数据必须走 Redis 或文件锁。
    'controller_reuse' => false,
];
