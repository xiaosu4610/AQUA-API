<?php
/**
 * 上游供应商预设（可选清单）
 *
 * 作用：让「添加渠道」变成选一个供应商、粘一个 Key 就完事，
 * 不需要用户去翻各家文档找 base_url、也不用记住协议差异。
 *
 * ═══ 三个字段的含义 ═══
 *
 *   adapter —— **协议类型**，决定用哪套解码/编码实现。目前只有两种：
 *       · openai_sse   兼容 OpenAI /v1 协议（绝大多数国内外厂商都是这类）
 *       · nim           NVIDIA NIM（协议上也是 OpenAI 兼容，但有限流、
 *                       thinking 字段名不一致等特殊行为，需要专门处理）
 *       将来会补 anthropic / gemini 等非 OpenAI 协议的适配器 ——
 *       这里已为它们预留位置（见文件末尾的注释）。
 *
 *   base_url —— 官方接口地址。**这是最容易过期的字段**，
 *       各家改版时会调整，所以每一项都附了 docs 链接，
 *       用户遇到 404 时可以去官方文档核对。
 *
 *   hint —— 一句话提示，说明该供应商的特点（尤其是计费/限流相关）。
 *
 * ═══ 为什么不在这里写死「可用模型清单」 ═══
 *
 * 模型清单变化极快（上线、下线、改名），写死在代码里必然过期，
 * 反而会误导用户去填一个已经下线的模型名。
 * 因此本文件只管「怎么连上」，模型清单一律靠「测活」向上游实时拉取。
 */

declare(strict_types=1);

return [

    // ── 本项目的主力上游 ─────────────────────────────────
    'nim' => [
        'label' => 'NVIDIA NIM',
        'adapter' => 'nim',
        'base_url' => 'https://integrate.api.nvidia.com/v1',
        'docs' => 'https://build.nvidia.com',
        'hint' => 'Key 形如 nvapi-xxxx。免费层约 40 RPM/模型，适合用多 Key 池轮换。',
    ],

    // ── OpenAI 官方与兼容生态 ─────────────────────────────
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
        'hint' => '推理速度极快，有免费额度。',
    ],
    'mistral' => [
        'label' => 'Mistral',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.mistral.ai/v1',
        'docs' => 'https://docs.mistral.ai',
        'hint' => '欧洲厂商，有免费层。',
    ],

    // ── 国产模型（当前重点）────────────────────────────────
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
        'hint' => 'GLM 系列。注意 v4 路径，不是 /v1。',
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
    'siliconflow' => [
        'label' => '硅基流动 SiliconFlow',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.siliconflow.cn/v1',
        'docs' => 'https://docs.siliconflow.cn',
        'hint' => '聚合多家开源模型，国内直连，有免费额度。',
    ],
    'volces' => [
        'label' => '火山方舟（字节豆包）',
        'adapter' => 'openai_sse',
        'base_url' => 'https://ark.cn-beijing.volces.com/api/v3',
        'docs' => 'https://www.volcengine.com/docs/82379',
        'hint' => '注意是 /api/v3；模型名通常需要填「接入点 ID」而非模型名。',
    ],
    'hunyuan' => [
        'label' => '腾讯混元',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.hunyuan.cloud.tencent.com/v1',
        'docs' => 'https://cloud.tencent.com/document/product/1729',
        'hint' => '混元系列。',
    ],
    'baichuan' => [
        'label' => '百川智能',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.baichuan-ai.com/v1',
        'docs' => 'https://platform.baichuan-ai.com/docs',
        'hint' => 'Baichuan 系列。',
    ],
    'minimax' => [
        'label' => 'MiniMax',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.minimax.chat/v1',
        'docs' => 'https://platform.minimaxi.com/document',
        'hint' => 'abab 系列，部分接口需 GroupId 参数。',
    ],
    'stepfun' => [
        'label' => '阶跃星辰 StepFun',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.stepfun.com/v1',
        'docs' => 'https://platform.stepfun.com/docs',
        'hint' => 'Step 系列。',
    ],
    'lingyiwanwu' => [
        'label' => '零一万物 Yi',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.lingyiwanwu.com/v1',
        'docs' => 'https://platform.lingyiwanwu.com/docs',
        'hint' => 'Yi 系列。',
    ],
    'spark' => [
        'label' => '讯飞星火',
        'adapter' => 'openai_sse',
        'base_url' => 'https://spark-api-open.xf-yun.com/v1',
        'docs' => 'https://www.xfyun.cn/doc/spark/',
        'hint' => '使用「HTTP 服务」的 OpenAI 兼容地址，不是 WebSocket 那套。',
    ],
    'sensenova' => [
        'label' => '商汤日日新',
        'adapter' => 'openai_sse',
        'base_url' => 'https://api.sensenova.cn/compatible-mode/v1',
        'docs' => 'https://platform.sensenova.cn/doc',
        'hint' => '需走 compatible-mode 兼容路径。',
    ],

    // ── 自建 / 本地推理 ───────────────────────────────────
    'ollama' => [
        'label' => 'Ollama（本地）',
        'adapter' => 'openai_sse',
        'base_url' => 'http://127.0.0.1:11434/v1',
        'docs' => 'https://github.com/ollama/ollama/blob/main/docs/openai.md',
        'hint' => '本地部署，无需 Key（可留空）。注意 base_url 用 http。',
    ],
    'vllm' => [
        'label' => 'vLLM / LM Studio（本地）',
        'adapter' => 'openai_sse',
        'base_url' => 'http://127.0.0.1:8000/v1',
        'docs' => 'https://docs.vllm.ai',
        'hint' => '自建推理服务，多数兼容 OpenAI 协议。',
    ],
    'custom' => [
        'label' => '自定义（其它 OpenAI 兼容服务）',
        'adapter' => 'openai_sse',
        'base_url' => '',
        'docs' => '',
        'hint' => '任何兼容 OpenAI /v1 协议的服务都可以填在这里。',
    ],

    /*
    ── 预留：非 OpenAI 协议的适配器 ────────────────────────
    下面这些供应商的官方协议与 OpenAI 不同，需要各自独立的
    解码器/编码器才能正确处理流式与字段差异。按项目规划，
    这类适配器统一放在「声明式适配器」体系里实现，不在本文件硬编码，
    以免出现「选了但用不了」的假入口。

        · Anthropic Claude   —— Messages API（事件类型与 OpenAI 完全不同）
        · Google Gemini      —— generateContent（SSE 结构与字段名差异大）
        · 百度文心一言        —— 需 access_token 换取流程，鉴权与 OpenAI 不同
        · 讯飞星火 WebSocket  —— 非 HTTP 协议（已优先支持其 HTTP 兼容版）

    实现顺序见项目开发文档的里程碑。
    */
];
