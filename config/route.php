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
use app\controller\AdminUserController;
use app\controller\AuthController;
use app\controller\ChannelController;
use app\controller\ConsoleController;
use app\controller\HealthController;
use app\controller\HomeController;
use app\controller\InstallController;
use app\controller\OpenAiController;
use app\controller\PricingController;
use app\controller\RechargeController;
use app\controller\SettingController;
use app\middleware\AdminAuth;
use Webman\Route;

// ── 安装向导（公开）──────────────────────────────────────
// 装完之后这两个入口只会显示「已安装」的说明页，不再提供可执行的表单。
// /install.php 是路径别名：常规 PHP 项目都用这个地址，照顾使用习惯
Route::get('/install', [InstallController::class, 'index']);
Route::post('/install', [InstallController::class, 'run']);
Route::get('/install.php', [InstallController::class, 'index']);
Route::post('/install.php', [InstallController::class, 'run']);

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

// ── 下游用户 · 注册与登录（公开）──────────────────────────
// 与后台是两套完全独立的入口：只有密码的后台 + 邮箱密码的用户
Route::get('/login', [AuthController::class, 'loginPage']);
Route::post('/login', [AuthController::class, 'login']);
Route::get('/register', [AuthController::class, 'registerPage']);
Route::post('/register', [AuthController::class, 'register']);
Route::get('/verify', [AuthController::class, 'verify']);
Route::get('/forgot', [AuthController::class, 'forgotPage']);
Route::post('/forgot', [AuthController::class, 'forgot']);
Route::get('/reset', [AuthController::class, 'resetPage']);
Route::post('/reset', [AuthController::class, 'reset']);
Route::post('/logout', [AuthController::class, 'logout']);

// ── 下游用户 · 控制台（控制器内校验登录态）────────────────
Route::get('/console', [ConsoleController::class, 'index']);
Route::post('/console/token/create', [ConsoleController::class, 'createToken']);
Route::post('/console/token/toggle', [ConsoleController::class, 'toggleToken']);
Route::post('/console/token/delete', [ConsoleController::class, 'deleteToken']);
Route::post('/console/token/reset', [ConsoleController::class, 'resetTokenQuota']);
Route::post('/console/profile', [ConsoleController::class, 'updateProfile']);
Route::post('/console/password', [ConsoleController::class, 'changePassword']);

// ── 充值 ────────────────────────────────────────────────
Route::get('/recharge', [RechargeController::class, 'index']);
Route::post('/recharge/create', [RechargeController::class, 'create']);

// ── 支付回调（**公开**，由支付网关服务器调用）──────────────
// 它的安全性完全建立在签名校验上，而不是登录态 —— 网关没有我们的会话。
// notify 是到账的唯一依据；return 只用于展示结果
Route::any('/pay/notify', [RechargeController::class, 'notify']);
Route::get('/pay/return', [RechargeController::class, 'returnPage']);

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

    // 模型定价（上游成本 + 下游售价）
    Route::get('/pricing', [PricingController::class, 'index']);
    Route::get('/pricing/new', [PricingController::class, 'createForm']);
    Route::get('/pricing/edit', [PricingController::class, 'editForm']);
    Route::post('/pricing/save', [PricingController::class, 'save']);
    Route::post('/pricing/delete', [PricingController::class, 'delete']);
    Route::post('/pricing/sync', [PricingController::class, 'sync']);

    // 下游用户与订单
    Route::get('/users', [AdminUserController::class, 'index']);
    Route::post('/users/status', [AdminUserController::class, 'status']);
    Route::post('/users/balance', [AdminUserController::class, 'balance']);
    Route::get('/orders', [AdminUserController::class, 'orders']);
    Route::post('/orders/fail', [AdminUserController::class, 'fail']);

    // POST /admin/logout —— 退出登录
    Route::post('/logout', [AdminController::class, 'logout']);
})->middleware([AdminAuth::class]);

// ── 对外的 OpenAI 兼容接口（/v1/*）──────────────────────
// 鉴权走请求头里的令牌（Bearer），**不挂后台中间件** ——
// 调用方是程序而不是浏览器，没有会话与 CSRF 令牌。
// 未安装时会被 InstallGuard 拦到安装向导，这是正确的
Route::get('/v1/models', [OpenAiController::class, 'models']);
Route::post('/v1/chat/completions', [OpenAiController::class, 'chatCompletions']);

// 关闭控制器自动路由。必须放在所有路由注册之后。
Route::disableDefaultRoute();
