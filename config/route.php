<?php
/**
 * 路由定义
 *
 * 本项目的路由策略：**全部显式声明**。
 *
 * 最后一行关闭了 Webman 的「控制器自动路由」。这是刻意的安全选择：
 * 自动路由会把 app/controller 下的每个公开方法都映射成一个可访问路径，
 * 一旦某个内部方法忘了加权限校验，就会直接暴露在公网。
 * 显式声明路由可以彻底避免这类事故，代价只是多敲一行代码。
 */

use app\controller\AdminController;
use app\controller\ChannelController;
use app\controller\HealthController;
use app\controller\HomeController;
use app\controller\SettingController;
use app\middleware\AdminAuth;
use Webman\Route;

// ── 站点首页（公开）──────────────────────────────────────
// 定位是「站点门户 + 运行状态」。将来公益站的用量公示也挂在这里，
// 所以从一开始就做成公开页面，而不是把根路径重定向到后台。
Route::get('/', [HomeController::class, 'index']);

// ── 探活 ────────────────────────────────────────────────
// 不做鉴权，供运维 / 负载均衡 / 监控调用
Route::get('/healthz', [HealthController::class, 'index']);

// ── 管理后台 · 登录相关（公开）────────────────────────────
Route::get('/admin/login', [AdminController::class, 'loginPage']);
Route::post('/admin/login', [AdminController::class, 'login']);

// ── 管理后台 · 需要登录的部分 ────────────────────────────
// 统一挂在 AdminAuth 中间件下。这样做的好处是：
// 以后往这个组里新增后台页面时，**不需要记得加鉴权**，默认就是受保护的 ——
// 「默认安全」比「记得加」可靠得多。
Route::group('/admin', function () {
    // GET /admin —— 仪表盘
    Route::get('', [AdminController::class, 'index']);

    // 修改密码
    Route::get('/password', [AdminController::class, 'passwordPage']);
    Route::post('/password', [AdminController::class, 'changePassword']);

    // 配置管理
    Route::get('/settings', [SettingController::class, 'index']);
    Route::post('/settings', [SettingController::class, 'save']);
    Route::post('/settings/reset', [SettingController::class, 'reset']);

    // 渠道与密钥池管理
    Route::get('/channels', [ChannelController::class, 'index']);
    Route::get('/channels/new', [ChannelController::class, 'createForm']);
    Route::get('/channels/edit', [ChannelController::class, 'editForm']);
    Route::post('/channels/save', [ChannelController::class, 'save']);
    Route::post('/channels/delete', [ChannelController::class, 'delete']);
    Route::post('/channels/test', [ChannelController::class, 'test']);

    // 渠道密钥池（一个渠道下可挂多把 Key，轮换使用）
    Route::get('/channels/keys', [ChannelController::class, 'keys']);
    Route::post('/channels/keys/import', [ChannelController::class, 'keysImport']);
    Route::post('/channels/keys/toggle', [ChannelController::class, 'keysToggle']);
    Route::post('/channels/keys/delete', [ChannelController::class, 'keysDelete']);
    Route::post('/channels/keys/reset', [ChannelController::class, 'keysReset']);

    // POST /admin/logout —— 退出登录
    Route::post('/logout', [AdminController::class, 'logout']);
})->middleware([AdminAuth::class]);

// ── 对外的 OpenAI 兼容接口（/v1/*）──────────────────────
// 待实现（M1 流式引擎）

// 关闭控制器自动路由。必须放在所有路由注册之后。
Route::disableDefaultRoute();
