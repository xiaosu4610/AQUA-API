<?php
/**
 * 上游供应商预设（可选清单）
 *
 * 作用：让「添加渠道」变成「选一个供应商 → 粘一个 Key」就完事，
 * 用户不需要去翻各家文档找 base_url，也不用记住鉴权方式的差异。
 *
 * ═══════════════════════════════════════════════════════════
 * 两层的分工（这一点是本文件的核心）
 * ═══════════════════════════════════════════════════════════
 *
 *   adapter（协议适配器）—— 决定「怎么说话」。数量少，**必须有对应代码实现**。
 *        定义在 app/common/Channel.php 的 Channel::ADAPTERS 里。
 *
 *   provider（供应商预设）—— 决定「对谁说、地址是什么、要填什么」。
 *        数量多，**纯数据、无需代码**。就是本文件。
 *
 * 为什么必须分开：如果每个供应商都要写一份适配器代码，那么供应商越多
 * 代码越膨胀（这是同类项目动辄几十万行的重要原因）。而实际上
 * **绝大多数供应商用的都是同一套 OpenAI 兼容协议，差异只在地址与鉴权头**。
 * 把它们做成数据，加一个供应商只是加一段配置。
 *
 * ═══════════════════════════════════════════════════════════
 * 字段说明
 * ═══════════════════════════════════════════════════════════
 *
 *   label    —— 显示名
 *   adapter  —— 协议适配器标识（必须在 Channel::ADAPTERS 中存在）
 *   base_url —— 官方接口地址。**最容易过期的字段**，遇到 404 请核对官方文档
 *   hint     —— 一句话提示，重点说明「这家有什么不一样」的地方
 *   docs     —— 官方文档地址（仅在确认过的情况下填写）
 *
 * 为什么不在这里写死「可用模型清单」：
 *   模型上下线极快，写死在代码里必然过期，反而会误导用户去填一个
 *   已经下线的模型名。模型清单一律靠「测活」向上游实时拉取。
 *
 * ═══════════════════════════════════════════════════════════
 * 数据来源
 * ═══════════════════════════════════════════════════════════
 *
 * 本清单中的官方 base_url 取自 New API 项目公开源码的渠道类型定义
 * （constant/channel.go 的 ChannelBaseURLs），并做了分类与中文化。
 * 那是同类项目里覆盖最全的一份清单，比凭记忆罗列可靠。
 * 另补充了若干原清单没有、但常用的供应商（如 Anyscale、Together 等）。
 *
 * ⚠️ 但要注意：本项目为**独立实现**，不复制对方代码。
 *    这里只是「借用公开事实（各家官方地址）」这一层信息。
 */

declare(strict_types=1);

return [

    // ═══════════════════════════════════════════════════════
    // 一、本项目主力上游
    // ═══════════════════════════════════════════════════════
    'nim' => [
        'label' => 'NVIDIA NIM',
        'adapter' => 'nim',
        'base_url' => 'https://integrate.api.nvidia.com/v1',
        'docs' => 'https://build.nvidia.com',
        'hint' => 'Key 形如 nvapi-xxxx。免费层约 40 RPM/模型，适合多 Key 池轮换。',
    ],

    // ═══════════════════════════════════════════════════════
    // 二、OpenAI 官方及兼容生态
    // ═══════════════════════════════════════════════════════
    'openai' => [
        'label' => 'OpenAI 官方',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.openai.com/v1',
        'docs' => 'https://platform.openai.com/docs/api-reference',
        'hint' => '官方接口。国内访问需自备网络条件。',
    ],
    'openrouter' => [
        'label' => 'OpenRouter',
        'adapter' => 'openai_sse',
        'base_url' => 'https://openrouter.ai/api/v1',
        'docs' => 'https://openrouter.ai/docs',
        'hint' => '聚合平台，一个 Key 可用多家模型，按量计费。',
    ],
    'groq' => [
        'label' => 'Groq',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.groq.com/openai/v1',
        'docs' => 'https://console.groq.com/docs',
        'hint' => '推理速度极快，有免费额度，适合做快速线路。',
    ],
    'mistral' => [
        'label' => 'Mistral',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.mistral.ai/v1',
        'docs' => 'https://docs.mistral.ai',
        'hint' => '欧洲厂商，有免费层。',
    ],
    'cohere' => [
        'label' => 'Cohere',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.cohere.ai/compatibility/v1',
        'hint' => '注意用 compatibility 这条兼容路径，不是原生 /v1。',
    ],
    'xai' => [
        'label' => 'xAI（Grok）',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.x.ai/v1',
        'docs' => 'https://docs.x.ai',
        'hint' => 'Grok 系列，OpenAI 兼容。',
    ],
    'perplexity' => [
        'label' => 'Perplexity',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.perplexity.ai',
        'hint' => '主打联网检索，返回里常带引用信息。',
    ],
    'together' => [
        'label' => 'Together AI',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.together.xyz/v1',
        'docs' => 'https://docs.together.ai',
        'hint' => '托管大量开源模型（Llama、Qwen 等）。',
    ],
    'fireworks' => [
        'label' => 'Fireworks AI',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.fireworks.ai/inference/v1',
        'docs' => 'https://docs.fireworks.ai',
        'hint' => '托管开源模型，注意地址里带 inference。',
    ],
    'anyscale' => [
        'label' => 'Anyscale Endpoints',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.endpoints.anyscale.com/v1',
        'docs' => 'https://docs.anyscale.com',
        'hint' => '托管开源模型；注意该平台业务已并入其它服务，地址可能变更。',
    ],
    'deepinfra' => [
        'label' => 'DeepInfra',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.deepinfra.com/v1/openai',
        'docs' => 'https://deepinfra.com/docs',
        'hint' => '注意兼容路径是 /v1/openai。',
    ],
    'submodel' => [
        'label' => 'Submodel',
        'adapter' => 'openai_sse',
        'base_url' => 'https://llm.submodel.ai/v1',
        'hint' => '开源模型托管平台。',
    ],
    'jina' => [
        'label' => 'Jina AI',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.jina.ai/v1',
        'docs' => 'https://jina.ai/reader',
        'hint' => '主打 Embedding / Rerank，也有对话模型。',
    ],
    'cloudflare' => [
        'label' => 'Cloudflare Workers AI',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1',
        'docs' => 'https://developers.cloudflare.com/workers-ai/',
        'hint' => '地址里的 {account_id} 要换成你自己的账号 ID。',
    ],

    // ═══════════════════════════════════════════════════════
    // 三、Google 系（协议与 OpenAI 不同，适配器待实现）
    // ═══════════════════════════════════════════════════════
    'gemini' => [
        'label' => 'Google Gemini',
        'adapter' => 'gemini',
        'base_url' => 'https://generativelanguage.googleapis.com/v1beta',
        'docs' => 'https://ai.google.dev/gemini-api/docs',
        'hint' => '用 generateContent 协议，与 OpenAI 差异较大，需要独立适配器。',
    ],
    'vertex' => [
        'label' => 'Google Vertex AI',
        'adapter' => 'vertex',
        'base_url' => '',
        'docs' => 'https://cloud.google.com/vertex-ai/docs',
        'hint' => '需 GCP 服务账号鉴权，地址随项目与区域变化。',
    ],

    // ═══════════════════════════════════════════════════════
    // 四、其它需要独立协议的厂商（适配器待实现）
    // ═══════════════════════════════════════════════════════
    'anthropic' => [
        'label' => 'Anthropic Claude',
        'adapter' => 'anthropic',
        'base_url' => 'https://api.anthropic.com/v1',
        'docs' => 'https://docs.anthropic.com',
        'hint' => 'Messages API，事件类型与 OpenAI 完全不同，需要独立适配器。',
    ],
    'azure' => [
        'label' => 'Azure OpenAI',
        'adapter' => 'azure',
        'base_url' => '',
        'docs' => 'https://learn.microsoft.com/azure/ai-services/openai/',
        'hint' => '鉴权用 api-key 头，且地址必须带 api-version 查询参数；地址含你自己的资源名。',
    ],
    'aws' => [
        'label' => 'AWS Bedrock',
        'adapter' => 'aws',
        'base_url' => '',
        'docs' => 'https://docs.aws.amazon.com/bedrock/',
        'hint' => '用 AWS SigV4 签名鉴权，需要 AccessKey/SecretKey，不是单一 API Key。',
    ],
    'baidu' => [
        'label' => '百度文心（旧版）',
        'adapter' => 'baidu',
        'base_url' => 'https://aip.baidubce.com',
        'hint' => '需先用 API Key 换 access_token，鉴权流程与 OpenAI 不同；新版建议用「百度千帆」。',
    ],
    'xunfei_ws' => [
        'label' => '讯飞星火（WebSocket 版）',
        'adapter' => 'xunfei',
        'base_url' => '',
        'hint' => '原生是 WebSocket 协议，不是 HTTP。建议优先用「讯飞星火」的 HTTP 兼容版。',
    ],
    'coze' => [
        'label' => '扣子 Coze',
        'adapter' => 'coze',
        'base_url' => 'https://api.coze.cn',
        'hint' => '以「机器人 / 工作流」为中心，并非标准对话补全接口。',
    ],
    'replicate' => [
        'label' => 'Replicate',
        'adapter' => 'replicate',
        'base_url' => 'https://api.replicate.com',
        'hint' => '以「提交任务 + 轮询结果」为主，属于异步任务模型。',
    ],
    'chatgpt_sub' => [
        'label' => 'ChatGPT 订阅账号（Codex 形态）',
        'adapter' => 'chatgpt_sub',
        'base_url' => 'https://chatgpt.com',
        'hint' => '用订阅账号的会话凭证而非 API Key；凭证会过期需定期更换，适合放进密钥池统一轮换。',
    ],
    'ollama' => [
        'label' => 'Ollama（本地原生）',
        'adapter' => 'ollama',
        'base_url' => 'http://127.0.0.1:11434',
        'docs' => 'https://github.com/ollama/ollama/blob/main/docs/api.md',
        'hint' => '这是原生 Ollama 协议；若走 OpenAI 兼容层请选下面那个。',
    ],

    // ═══════════════════════════════════════════════════════
    // 五、国产模型（当前重点，均为 OpenAI 兼容）
    // ═══════════════════════════════════════════════════════
    'deepseek' => [
        'label' => 'DeepSeek 深度求索',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.deepseek.com/v1',
        'docs' => 'https://platform.deepseek.com/api-docs',
        'hint' => 'OpenAI 兼容。推理模型会返回 reasoning_content 字段。',
    ],
    'zhipu' => [
        'label' => '智谱 AI（GLM）',
        'adapter' => 'openai_sse',
        'base_url' => 'https://open.bigmodel.cn/api/paas/v4',
        'docs' => 'https://open.bigmodel.cn/dev/api',
        'hint' => '注意是 v4 路径，不是 /v1。',
    ],
    'zhipu_coding' => [
        'label' => '智谱 GLM 编程套餐',
        'adapter' => 'openai_sse',
        'base_url' => 'https://open.bigmodel.cn/api/coding/paas/v4',
        'hint' => '订阅制编程套餐的专用地址；另有 Claude 协议入口（待适配）。',
    ],
    'dashscope' => [
        'label' => '阿里云百炼（通义千问）',
        'adapter' => 'openai_sse',
        'base_url' => 'https://dashscope.aliyuncs.com/compatible-mode/v1',
        'docs' => 'https://help.aliyun.com/zh/model-studio/',
        'hint' => '必须用 compatible-mode 这条 OpenAI 兼容路径。',
    ],
    'moonshot' => [
        'label' => '月之暗面（Kimi）',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.moonshot.cn/v1',
        'docs' => 'https://platform.moonshot.cn/docs',
        'hint' => 'Kimi 系列，上下文较长。',
    ],
    'kimi_coding' => [
        'label' => 'Kimi 编程套餐',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.kimi.com/coding/v1',
        'hint' => '订阅制编程套餐的专用地址。',
    ],
    'volces' => [
        'label' => '火山方舟（字节豆包）',
        'adapter' => 'openai_sse',
        'base_url' => 'https://ark.cn-beijing.volces.com/api/v3',
        'docs' => 'https://www.volcengine.com/docs/82379',
        'hint' => '注意是 /api/v3；模型名通常要填「接入点 ID」而不是模型名。',
    ],
    'doubao_coding' => [
        'label' => '豆包编程套餐',
        'adapter' => 'openai_sse',
        'base_url' => 'https://ark.cn-beijing.volces.com/api/coding/v3',
        'hint' => '订阅制编程套餐的专用地址。',
    ],
    'hunyuan' => [
        'label' => '腾讯混元',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.hunyuan.cloud.tencent.com/v1',
        'docs' => 'https://cloud.tencent.com/document/product/1729',
        'hint' => '混元系列。',
    ],
    'qianfan' => [
        'label' => '百度千帆（V2）',
        'adapter' => 'openai_sse',
        'base_url' => 'https://qianfan.baidubce.com/v2',
        'docs' => 'https://cloud.baidu.com/doc/WENXINWORKSHOP/',
        'hint' => '千帆 V2 提供了 OpenAI 兼容接口，比旧版文心方便得多。',
    ],
    'xunfei' => [
        'label' => '讯飞星火（HTTP 版）',
        'adapter' => 'openai_sse',
        'base_url' => 'https://spark-api-open.xf-yun.com/v1',
        'docs' => 'https://www.xfyun.cn/doc/spark/',
        'hint' => '用「HTTP 服务」的 OpenAI 兼容地址，不是 WebSocket 那套。',
    ],
    'baichuan' => [
        'label' => '百川智能',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.baichuan-ai.com/v1',
        'hint' => 'Baichuan 系列。',
    ],
    'minimax' => [
        'label' => 'MiniMax',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.minimax.chat/v1',
        'hint' => 'abab 系列，部分接口需要额外传 GroupId 参数。',
    ],
    'stepfun' => [
        'label' => '阶跃星辰 StepFun',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.stepfun.com/v1',
        'hint' => 'Step 系列。',
    ],
    'lingyiwanwu' => [
        'label' => '零一万物 Yi',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.lingyiwanwu.com/v1',
        'hint' => 'Yi 系列。',
    ],
    'sensenova' => [
        'label' => '商汤日日新',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.sensenova.cn/compatible-mode/v1',
        'hint' => '需走 compatible-mode 兼容路径。',
    ],
    'siliconflow' => [
        'label' => '硅基流动 SiliconFlow',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.siliconflow.cn/v1',
        'docs' => 'https://docs.siliconflow.cn',
        'hint' => '聚合多家开源模型，国内直连，有免费额度。',
    ],
    'ai360' => [
        'label' => '360 智脑',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.360.cn/v1',
        'hint' => '360 系列模型。',
    ],
    'moka' => [
        'label' => 'Moka AI',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.moka.ai/v1',
        'hint' => 'Moka 系列模型。',
    ],

    // ═══════════════════════════════════════════════════════
    // 六、本地 / 自建推理（多数零成本，适合放主力或兜底）
    // ═══════════════════════════════════════════════════════
    'ollama_openai' => [
        'label' => 'Ollama（OpenAI 兼容层）',
        'adapter' => 'openai_sse',
        'base_url' => 'http://127.0.0.1:11434/v1',
        'hint' => '本地部署，无需 Key（可留空）。注意 base_url 用 http。',
    ],
    'vllm' => [
        'label' => 'vLLM',
        'adapter' => 'openai_sse',
        'base_url' => 'http://127.0.0.1:8000/v1',
        'docs' => 'https://docs.vllm.ai',
        'hint' => '高吞吐自建推理，默认 8000 端口。',
    ],
    'sglang' => [
        'label' => 'SGLang',
        'adapter' => 'openai_sse',
        'base_url' => 'http://127.0.0.1:30000/v1',
        'docs' => 'https://docs.sglang.ai',
        'hint' => '注意默认端口是 30000。',
    ],
    'xinference' => [
        'label' => 'Xinference',
        'adapter' => 'openai_sse',
        'base_url' => 'http://127.0.0.1:9997/v1',
        'hint' => '默认 9997 端口。',
    ],
    'lmstudio' => [
        'label' => 'LM Studio',
        'adapter' => 'openai_sse',
        'base_url' => 'http://127.0.0.1:1234/v1',
        'hint' => '桌面端本地推理，默认 1234 端口。',
    ],

    // ═══════════════════════════════════════════════════════
    // 七、中转 / 聚合平台（行为各异，需自行核对）
    // ═══════════════════════════════════════════════════════
    'ohmygpt' => [
        'label' => 'OhMyGPT',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.ohmygpt.com/v1',
        'hint' => '第三方中转平台，具体可用模型以其站点为准。',
    ],
    'aiproxy' => [
        'label' => 'AIProxy',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.aiproxy.io/v1',
        'hint' => '第三方中转平台。',
    ],
    'api2gpt' => [
        'label' => 'API2GPT',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.api2gpt.com/v1',
        'hint' => '第三方中转平台。',
    ],
    'aigc2d' => [
        'label' => 'AIGC2D',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.aigc2d.com/v1',
        'hint' => '第三方中转平台。',
    ],
    'openaimax' => [
        'label' => 'OpenAIMax',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.openaimax.com/v1',
        'hint' => '第三方中转平台。',
    ],
    'fastgpt' => [
        'label' => 'FastGPT',
        'adapter' => 'openai_sse',
        'base_url' => 'https://fastgpt.run/api/openapi/v1',
        'hint' => '以「应用编排」为中心，接口是其开放 API 而非纯模型接口。',
    ],
    'dify' => [
        'label' => 'Dify',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.dify.ai/v1',
        'hint' => '以「应用 / 工作流」为中心，接口是其应用 API。',
    ],
    'newapi_self' => [
        'label' => '自建中转（New API / One API 等）',
        'adapter' => 'openai_sse',
        'base_url' => '',
        'hint' => '把另一个中转站当作上游接入，填它给你的地址即可。',
    ],

    // ═══════════════════════════════════════════════════════
    // 八、自定义
    // ═══════════════════════════════════════════════════════
    'custom' => [
        'label' => '自定义（其它 OpenAI 兼容服务）',
        'adapter' => 'openai_sse',
        'base_url' => '',
        'hint' => '任何兼容 OpenAI /v1 协议的服务都可以填在这里。',
    ],

    /*
    ── 暂不纳入的类别，以及原因 ────────────────────────────
    下面这些在同类项目里存在，但**不属于本项目的当前范围**，
    因此刻意不放进预设清单，避免出现「选了却没有对应实现」的假入口：

        · 图像 / 视频生成：Midjourney、Suno、Kling（可灵）、即梦、
          Vidu、Sora、Doubao Video。它们不是对话补全接口，
          请求与计费模型都不同，需要单独设计任务模型（见项目文档的功能边界）。

        · 已被官方下线 / 长期不维护：PaLM（已被 Gemini 取代）。
    */
];
