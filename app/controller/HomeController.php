<?php
/**
 * 站点首页
 *
 * 这一层只负责**取真实数据**，不做任何文案编造：
 *   · 「可用模型」是从已启用渠道的模型清单实时汇总的，不是写死的宣传；
 *   · 「服务已就绪」取决于是否真的存在已启用的渠道 —— 没配上游就如实显示
 *     「待配置」，而不是挂一个漂亮的状态骗访客。
 *
 * 只暴露模型名这一层信息。渠道名、上游地址、密钥池规模都属于运维细节，
 * 放上首页等于白送情报给扫描者。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Channel;
use app\common\Epay;
use app\common\Settings;
use app\common\Url;
use support\Request;
use support\Response;

class HomeController
{
    /** 首页最多展示多少个模型名。再多就变成一堵墙，反而没人看 */
    private const MAX_MODELS_ON_HOME = 24;

    /**
     * GET / —— 站点首页
     */
    public function index(Request $request): Response
    {
        $mode = (string) Settings::get('site.mode', 'commercial');
        $showModels = Settings::bool('site.show_models', true);

        // 汇总模型清单：只统计**已启用**的渠道 —— 被停用的渠道所支持的模型
        // 对外并不存在，列出来只会误导访客
        $all = [];
        $hasUsableChannel = false;

        foreach (Channel::all() as $channel) {
            if ((int) $channel['status'] !== Channel::STATUS_ENABLED) {
                continue;
            }

            $hasUsableChannel = true;

            foreach (Channel::modelsOf($channel) as $model) {
                $model = trim((string) $model);
                if ($model !== '') {
                    $all[$model] = true;
                }
            }
        }

        $models = array_keys($all);
        sort($models, SORT_NATURAL | SORT_FLAG_CASE);

        // 示例代码用哪个模型：优先用站长在后台指定的「验证过能跑通」的那个，
        // 没指定才退回按字母序的第一个（那不一定真能用）
        $demoModel = trim((string) Settings::get('site.demo_model', ''));
        if ($demoModel === '') {
            $demoModel = (string) ($models[0] ?? '');
        }

        return view('home', [
            'siteName' => (string) Settings::get('site.name', 'aqua-api-php'),
            // 不再对外展示「商业站 / 公益站」这类划分标签（见 Settings::siteModeLabel 的说明），
            // 但站点模式本身仍决定页面显示哪些入口（isWelfare 用于按模式调整导航）
            'modeLabel' => '',
            'isWelfare' => $mode === 'public_welfare',
            // 首页示例代码用的「站点基址」（不带 /v1，模板里自己拼端点路径），
            // 以及完整的接入地址（带 /v1，供首屏一键复制）
            'apiBase' => rtrim(\app\common\Url::base($request), '/'),
            'apiBaseUrl' => \app\common\Url::apiBase($request),
            'description' => trim((string) Settings::get('site.description', '')),
            'announcement' => trim((string) Settings::get('site.announcement', '')),
            'icp' => trim((string) Settings::get('site.icp', '')),
            'footer' => trim((string) Settings::get('site.footer', '')),
            'loggedIn' => AuthController::currentUserId() > 0,
            'registerOpen' => Settings::bool('register.open', false),
            'rechargeEnabled' => Epay::enabled(),
            'showModels' => $showModels,
            'models' => $showModels ? array_slice($models, 0, self::MAX_MODELS_ON_HOME) : [],
            'modelTotal' => count($models),
            'demoModel' => $demoModel !== '' ? $demoModel : 'your-model',
            // 有已启用渠道就认为转发通道可用。这里不做真实探测 ——
            // 首页每次访问都去连一次上游是不可接受的
            'relayReady' => $hasUsableChannel,
            // 示例代码里的接口基址：优先用配置的域名，回落到当前访问的 Host
            'apiBase' => Url::base($request),
        ], '');
    }
}
