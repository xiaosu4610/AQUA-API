<?php
/**
 * 接口文档（公开页）
 *
 * 为什么要有这一页：用户拿到令牌后的第一个问题永远是
 * 「我该填哪个地址、怎么调」。没有这一页，答案就散在聊天记录与别人写的教程里，
 * 而其中任何一处过期都会变成「你们接口不能用」的投诉。
 *
 * 两条原则：
 *   1. **只写真实存在的端点**。本站目前只有 /v1/models 与 /v1/chat/completions，
 *      文档里就绝不出现 /v1/embeddings 之类的「应该有」的接口 ——
 *      照着文档调不通，比没有文档更伤。
 *   2. **地址、模型名都从运行时取**，不写死。换了域名或模型清单变化时，文档跟着变。
 */

declare(strict_types=1);

namespace app\controller;

use app\common\Channel;
use app\common\Settings;
use app\common\Timeouts;
use app\common\Url;
use support\Request;
use support\Response;

class DocsController
{
    /** GET /docs */
    public function index(Request $request): Response
    {
        return view('docs', [
            'siteName' => (string) Settings::get('site.name', 'aqua-api-php'),
            'baseUrl' => Url::apiBase(request()),
            'models' => $this->availableModels(),
            'currency' => (string) Settings::get('billing.currency', 'CNY'),
            'loggedIn' => AuthController::currentUserId() > 0,
            // 慢模型要提醒用户把客户端超时调大 —— 这是实际支持里最高频的一类问题
            'ttftTimeout' => Timeouts::ttft(),
            'totalTimeout' => Timeouts::total(),
        ], '');
    }

    /**
     * 文档里展示的模型清单（只取已启用渠道支持的，最多列 60 个）。
     *
     * 与模型广场同一口径：文档里出现的模型名必须真的能调用，
     * 否则用户复制过去就是一次失败体验。
     *
     * @return array<int, string>
     */
    private function availableModels(): array
    {
        $seen = [];
        foreach (Channel::all() as $channel) {
            if ((int) $channel['status'] !== Channel::STATUS_ENABLED) {
                continue;
            }

            foreach (Channel::modelsOf($channel) as $model) {
                $model = trim((string) $model);
                if ($model !== '') {
                    $seen[$model] = true;
                }
            }

            if (count($seen) >= 60) {
                break;
            }
        }

        $list = array_keys($seen);
        sort($list, SORT_NATURAL | SORT_FLAG_CASE);

        return $list;
    }
}
