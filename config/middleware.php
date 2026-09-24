<?php
/**
 * 全局中间件
 *
 * ⚠️ 格式说明（Webman 的约定，容易搞错）：
 *   键是「应用名」，`'@'` 特指**全局**（对所有应用生效）。
 *   直接把类名放在顶层数组里会抛出 `Bad middleware config` ——
 *   因为它会被当成「应用名 => 中间件列表」，而列表却不是数组。
 *
 * 为什么用全局而不是挂在某组路由上：
 *   「未安装就导向安装向导」必须覆盖**所有**路径。
 *   挂在路由组上一定会漏掉某些入口，而漏掉的那个就是
 *   未安装状态下报出难懂错误的页面。
 */

declare(strict_types=1);

use app\middleware\InstallGuard;

return [
    '@' => [
        InstallGuard::class,
    ],
];
