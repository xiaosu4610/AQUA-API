// Package channeltype —— 本文件是「渠道目录（catalog）」：逐一登记我们计划支持的上游。
//
// 意图（Why）：
//
//	registry.go 定义的是「渠道类型由哪些字段描述」这份契约；本文件则给出契约的
//	一份**全量实例**——把现实中要接入的上游一家家列出来。之所以要把它们做成数据
//	而不是代码，是因为上游的差异几乎都集中在少数几个字段上（默认地址、鉴权方式、
//	必填额外参数、能力位），加一家上游通常只是"多写一条"，不必动转发逻辑。
//
//	本文件刻意按「大类」分段（文本 / 订阅 / 聚合 / 图像 / 视频 / 音频 / 嵌入 /
//	自建），与后台的分组展示一一对应，方便按行核对"哪些上游已登记、哪些还没"。
//
// 流转（Flow）：
//
//	Types() → registry.go 的 Find / AvailableTypes / TypesByCategory / Validate / Keys
//	  └─ 后台 GET /api/admin/channel-types：按 Category 分组返回，前端触发式渲染表单
//	  └─ 保存渠道：Validate 用 Available 与必填额外参数做前置校验
//	  └─ 转发：relay 依据 Protocol / AuthMode 选适配器与鉴权头
//
// 扩展（Extend）：
//
//	新增上游：在对应分段追加一条 Type，并按下述红线自检。
//	改动前必读的两条铁律：
//	  1) Available 必须诚实——只有**适配器已实现**的类型才置 true。当前只实现了
//	     OpenAI 兼容适配器，因此只有 Protocol=ProtocolOpenAI 且鉴权落在
//	     {AuthBearer, AuthQueryKey, AuthNone} 的类型可以置 true；其余一律 false，
//	     后台会显示"即将支持"。标错会让站长"配好了却调不通"，属于安全底线。
//	  2) 字面量保持紧凑——Go 结构体字面量允许省略零值字段，只写非零项，
//	     避免每条都重复一堆 false/空串而淹没真正的差异。
//	不变量由 catalog_test.go 钉住；改完请执行 go test ./internal/channeltype/...
package channeltype

// Types 返回全量上游渠道目录。
//
// 为什么做成函数而不是包级变量：
//  1. 调用方拿到的是新切片，无法从外部改坏"注册表"（例如前端展示时顺手排序、
//     或某处误 append 导致全局目录被污染）；
//  2. 不引入包级可变状态，天然并发安全。
//
// 代价是每次调用都重新构造切片——但目录只在启动与后台拉取类型时读取，
// 频率极低，这点分配可以忽略。
func Types() []Type {
	return []Type{
		// ═══════════════════════════════════════════════════════════════
		// 文本大模型（OpenAI 兼容为主，少数原生协议）
		// ═══════════════════════════════════════════════════════════════
		{
			Key: "openai", Label: "OpenAI 官方", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.openai.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "官方 OpenAI，是绝大多数下游工具的默认协议，也可作为一切兼容实现的对照基准。",
		},
		{
			Key: "azure_openai", Label: "Azure OpenAI", Category: CategoryText,
			Protocol: ProtocolAzure, AuthMode: AuthAPIKeyHeader, AuthHeader: "api-key",
			BaseURLEditable: true,
			ExtraFields: []ExtraField{
				{Key: "deployment", Label: "部署名", Placeholder: "gpt-4o", Required: true,
					Help: "Azure 门户里「部署」的名称；请求路径由它决定，并不是模型名本身。"},
				{Key: "api_version", Label: "API 版本", Default: "2024-10-21", Required: true,
					Help: "Azure 用查询参数选择接口版本，写错会直接返回 400。"},
			},
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "资源名进域名、部署名进路径、鉴权走 api-key 头，三点都与标准 OpenAI 不同，需专用适配器。",
		},
		{
			Key: "deepseek", Label: "DeepSeek 深度求索", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.deepseek.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "以推理与代码见长的国产模型，完全兼容 OpenAI Chat Completions。",
		},
		{
			Key: "moonshot", Label: "Moonshot Kimi", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.moonshot.cn/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "月之暗面 Kimi，长上下文见长，接口为 OpenAI 兼容。",
		},
		{
			Key: "zhipu", Label: "智谱 GLM", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL: "https://open.bigmodel.cn/api/paas/v4",
			// 版本段是 /api/paas/v4 而非标准的 /v1，地址允许站长改写以便对接新版本。
			BaseURLEditable:   true,
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "智谱开放平台，OpenAI 兼容入口挂在 /api/paas/v4 下，版本段与标准 /v1 不同。",
		},
		{
			Key: "dashscope", Label: "阿里云百炼（通义千问）", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://dashscope.aliyuncs.com/compatible-mode/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "通过 compatible-mode 暴露 OpenAI 兼容接口；其原生 DashScope 协议与 OpenAI 并不一致。",
		},
		{
			Key: "siliconflow", Label: "硅基流动 SiliconFlow", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.siliconflow.cn/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "聚合开源模型的国产平台，接口 OpenAI 兼容，另提供嵌入与重排端点。",
		},
		{
			Key: "openrouter", Label: "OpenRouter", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://openrouter.ai/api/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "聚合多家厂商模型并自动路由，OpenAI 兼容，模型名通常带厂商前缀。",
		},
		{
			Key: "groq", Label: "Groq", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.groq.com/openai/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "以 LPU 低延迟推理为卖点，OpenAI 兼容，另托管 Whisper 系列语音识别模型。",
		},
		{
			Key: "together", Label: "Together AI", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.together.xyz/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "托管大量开源模型，接口 OpenAI 兼容。",
		},
		{
			Key: "mistral", Label: "Mistral AI", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.mistral.ai/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "法国 Mistral，接口 OpenAI 兼容，另有原生 SDK。",
		},
		{
			Key: "cohere", Label: "Cohere", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL: "https://api.cohere.ai/compatibility/v1",
			// 兼容入口的版本段是 /compatibility/v1，非标准，允许站长按文档调整。
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapTools | CapReasoning,
			Available:       true,
			Notes:           "Cohere 的 Compatibility API 提供 OpenAI 兼容对话接口；其原生 /v2 协议不同，未按兼容入口提供模型清单。",
		},
		{
			Key: "xai", Label: "xAI Grok", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.x.ai/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "xAI 的 Grok 系列模型，接口 OpenAI 兼容。",
		},
		{
			Key: "perplexity", Label: "Perplexity", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.perplexity.ai",
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapReasoning,
			Available:       false,
			Notes:           "以联网检索问答为主；对话端点为 /chat/completions（无 /v1 前缀），路径与本站约定不同，需适配后再开放。",
		},
		{
			Key: "minimax", Label: "MiniMax 稀宇科技", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.minimax.cn/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "同时提供 OpenAI 与 Anthropic 两套兼容接口，此处走 OpenAI 兼容路径。",
		},
		{
			Key: "stepfun", Label: "阶跃星辰 StepFun", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.stepfun.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "阶跃星辰，接口 OpenAI 兼容，除文本外还有视觉与语音能力。",
		},
		{
			Key: "lingyiwanwu", Label: "零一万物 Yi", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.lingyiwanwu.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "零一万物 Yi 系列模型，接口 OpenAI 兼容。",
		},
		{
			Key: "baichuan", Label: "百川智能 Baichuan", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.baichuan-ai.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "百川大模型，接口 OpenAI 兼容。",
		},
		{
			Key: "ai360", Label: "360 智脑", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.360.cn/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "360 智脑，接口 OpenAI 兼容。",
		},
		{
			Key: "hunyuan", Label: "腾讯混元 Hunyuan", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.hunyuan.cloud.tencent.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "腾讯混元大模型，提供 OpenAI 兼容接口。",
		},
		{
			Key: "doubao", Label: "火山方舟（豆包）", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL: "https://ark.cn-beijing.volces.com/api/v3",
			// 版本段是 /api/v3，非标准 /v1，允许站长按方舟文档调整。
			BaseURLEditable:   true,
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "字节火山方舟，OpenAI 兼容，接口挂在 /api/v3 下（版本段与标准 /v1 不同）。",
		},
		{
			Key: "baidu_qianfan", Label: "百度千帆（文心）", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://qianfan.baidubce.com/v2",
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapTools | CapReasoning,
			Available:       false,
			Notes:           "千帆 v2 提供 OpenAI 兼容入口，但模型命名与部分字段语义存在差异，需专门适配后再开放。",
		},
		{
			Key: "xunfei", Label: "讯飞星火 Spark", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://spark-api-open.xf-yun.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "讯飞星火的 HTTP 版 OpenAI 兼容入口，与其 WebSocket 原生协议不同。",
		},
		{
			Key: "novita", Label: "Novita AI", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL: "https://api.novita.ai/openai",
			// 兼容入口挂在 /openai 路径下，地址可编辑以便对接其新版本。
			BaseURLEditable:   true,
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "海外模型托管平台，OpenAI 兼容入口挂在 /openai 路径下。",
		},
		{
			Key: "nvidia", Label: "NVIDIA NIM", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://integrate.api.nvidia.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "NVIDIA 模型推理服务，OpenAI 兼容，提供相当数量的免费额度。",
		},
		{
			Key: "aihubmix", Label: "AIHubMix", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://aihubmix.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "聚合型中转站点，OpenAI 兼容，聚合多家模型。",
		},
		{
			Key: "giteeai", Label: "Gitee AI", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://ai.gitee.com/v1",
			Caps:              CapChat | CapStream | CapTools | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "Gitee 的模型推理服务，OpenAI 兼容，部分模型限时免费。",
		},
		{
			Key: "cloudflare_workers_ai", Label: "Cloudflare Workers AI", Category: CategoryText,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			BaseURLEditable: true,
			ExtraFields: []ExtraField{
				{Key: "account_id", Label: "账户 ID", Required: true,
					Help: "Cloudflare 账户 ID；兼容地址形如 /client/v4/accounts/<账户ID>/ai。"},
			},
			Caps:      CapChat | CapStream | CapTools,
			Available: false,
			Notes:     "OpenAI 兼容入口是账户级地址（含 account_id），需按账户拼装地址后再开放。",
		},
		{
			Key: "custom_openai", Label: "自定义 OpenAI 兼容", Category: CategoryText,
			// 地址必须由使用者填写：这类上游可能是任意第三方或内网服务。
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			BaseURLEditable:   true,
			Caps:              CapChat | CapStream | CapTools | CapVision | CapReasoning,
			SupportsModelList: true, Available: true,
			Notes: "任何遵循 OpenAI 协议的第三方或内网服务：必须自行填写上游地址。",
		},
		{
			Key: "anthropic", Label: "Anthropic Claude", Category: CategoryText,
			Protocol: ProtocolAnthropic, AuthMode: AuthXAPIKey,
			DefaultBaseURL: "https://api.anthropic.com/v1",
			DefaultHeaders: map[string]string{"anthropic-version": "2023-06-01"},
			Caps:           CapChat | CapStream | CapTools | CapVision | CapReasoning,
			Available:      true,
			Notes:          "原生 Messages 协议：鉴权走 x-api-key，且必须携带版本头，需专用适配器。",
		},
		{
			Key: "gemini", Label: "Google Gemini", Category: CategoryText,
			Protocol: ProtocolGemini, AuthMode: AuthQueryKey,
			DefaultBaseURL: "https://generativelanguage.googleapis.com",
			Caps:           CapChat | CapStream | CapTools | CapVision | CapReasoning,
			Available:      true,
			Notes:          "原生 generateContent 协议：模型名与动作写在路径里、密钥走查询参数，已内置专用适配器。",
		},
		{
			Key: "palm", Label: "Google PaLM 旧版", Category: CategoryText,
			Protocol: ProtocolPaLM, AuthMode: AuthQueryKey,
			BaseURLEditable: true,
			Caps:            CapChat,
			Available:       false,
			Notes:           "Google 早期的 PaLM 文本协议，官方已不再推荐；保留登记仅为兼容历史渠道。",
		},
		{
			Key: "vertex_ai", Label: "Google Vertex AI", Category: CategoryText,
			Protocol: ProtocolVertex, AuthMode: AuthServiceAccount,
			BaseURLEditable: true,
			ExtraFields: []ExtraField{
				{Key: "project_id", Label: "项目 ID", Required: true, Help: "GCP 项目 ID。"},
				{Key: "region", Label: "区域", Placeholder: "us-central1", Required: true,
					Help: "如 us-central1，地址与配额都随区域变化。"},
				{Key: "service_account_json", Label: "服务账号 JSON", Required: true, Secret: true,
					Help: "服务账号密钥文件内容，属敏感信息，仅加密存储。"},
			},
			Caps:      CapChat | CapStream | CapTools | CapVision | CapReasoning,
			Available: false,
			Notes:     "企业级 Vertex AI：地址随项目与区域变化，鉴权用服务账号 JSON，需专用适配器。",
		},
		{
			Key: "bedrock", Label: "AWS Bedrock", Category: CategoryText,
			Protocol: ProtocolBedrock, AuthMode: AuthSigV4,
			BaseURLEditable: true,
			ExtraFields: []ExtraField{
				{Key: "region", Label: "区域", Placeholder: "us-east-1", Required: true,
					Help: "Bedrock 服务所在区域，签名与地址都依赖它。"},
				{Key: "ak", Label: "Access Key ID", Required: true, Secret: true,
					Help: "IAM 访问密钥 ID。"},
				{Key: "sk", Label: "Secret Access Key", Required: true, Secret: true,
					Help: "IAM 访问密钥，属敏感信息，仅加密存储。"},
			},
			Caps:      CapChat | CapStream | CapTools | CapVision | CapReasoning,
			Available: false,
			Notes:     "AWS 托管模型：请求需 SigV4 签名并绑定区域，报文格式与 OpenAI 差异较大，需专用适配器。",
		},

		// ═══════════════════════════════════════════════════════════════
		// 自建与本地部署（协议多兼容 OpenAI，但地址按部署而异）
		// ═══════════════════════════════════════════════════════════════
		{
			Key: "ollama", Label: "Ollama（本地）", Category: CategorySelfHosted,
			Protocol: ProtocolOllama, AuthMode: AuthNone,
			DefaultBaseURL:  "http://localhost:11434/v1",
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapTools | CapReasoning,
			Available:       false,
			Notes:           "本地一键跑模型；其原生 /api 协议与 OpenAI 兼容层并存，原生路径尚未适配。",
		},
		{
			Key: "vllm", Label: "vLLM", Category: CategorySelfHosted,
			Protocol: ProtocolOpenAI, AuthMode: AuthNone,
			DefaultBaseURL:    "http://localhost:8000/v1",
			BaseURLEditable:   true,
			Caps:              CapChat | CapStream | CapTools,
			SupportsModelList: true, Available: true,
			Notes: "高吞吐自建推理框架，默认暴露 OpenAI 兼容接口，本机部署通常无需鉴权。",
		},
		{
			Key: "sglang", Label: "SGLang", Category: CategorySelfHosted,
			Protocol: ProtocolOpenAI, AuthMode: AuthNone,
			DefaultBaseURL:    "http://localhost:30000/v1",
			BaseURLEditable:   true,
			Caps:              CapChat | CapStream | CapTools,
			SupportsModelList: true, Available: true,
			Notes: "面向高并发推理的自建框架，默认 OpenAI 兼容接口，本机部署通常无需鉴权。",
		},
		{
			Key: "xinference", Label: "Xinference", Category: CategorySelfHosted,
			Protocol: ProtocolOpenAI, AuthMode: AuthNone,
			DefaultBaseURL:    "http://localhost:9997/v1",
			BaseURLEditable:   true,
			Caps:              CapChat | CapStream | CapTools,
			SupportsModelList: true, Available: true,
			Notes: "自建模型服务平台，除 OpenAI 兼容接口外还有自有管理接口。",
		},
		{
			Key: "lmstudio", Label: "LM Studio", Category: CategorySelfHosted,
			Protocol: ProtocolOpenAI, AuthMode: AuthNone,
			DefaultBaseURL:    "http://localhost:1234/v1",
			BaseURLEditable:   true,
			Caps:              CapChat | CapStream | CapTools,
			SupportsModelList: true, Available: true,
			Notes: "桌面端本地模型工具，内置 OpenAI 兼容服务端，默认无需鉴权。",
		},

		// ═══════════════════════════════════════════════════════════════
		// 订阅账号（凭会话配额调度，全部未实现）
		// ═══════════════════════════════════════════════════════════════
		{
			Key: "claude_subscription", Label: "Claude 订阅账号", Category: CategorySubscription,
			Protocol: ProtocolAnthropic, AuthMode: AuthOAuth,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapTools,
			Available:       false,
			Notes:           "以 Claude 订阅（Pro/Max）的会话凭据驱动，按窗口配额调度，需 OAuth 自动续期与账号池。",
		},
		{
			Key: "openai_codex_subscription", Label: "Codex 订阅账号", Category: CategorySubscription,
			Protocol: ProtocolOpenAI, AuthMode: AuthOAuth,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapTools,
			Available:       false,
			Notes:           "以 ChatGPT/Codex 订阅凭据驱动，需 OAuth 自动续期与按窗口配额调度。",
		},
		{
			Key: "gemini_code_assist", Label: "Gemini Code Assist", Category: CategorySubscription,
			Protocol: ProtocolGemini, AuthMode: AuthOAuth,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapTools,
			Available:       false,
			Notes:           "Google 的订阅型编码助手，凭据与配额规则特殊，需专门适配。",
		},
		{
			Key: "antigravity", Label: "Antigravity", Category: CategorySubscription,
			Protocol: ProtocolGemini, AuthMode: AuthOAuth,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapTools,
			Available:       false,
			Notes:           "Google 新推出的智能体式开发环境，背后由 Gemini 配额驱动，需订阅凭据对接。",
		},
		{
			Key: "grok_subscription", Label: "Grok 订阅账号", Category: CategorySubscription,
			Protocol: ProtocolOpenAI, AuthMode: AuthCookie,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream,
			Available:       false,
			Notes:           "以 X/Grok 网页会话驱动，凭据为 Cookie，稳定性依赖于前端接口演进，需专门适配。",
		},

		// ═══════════════════════════════════════════════════════════════
		// 聚合与编排（协议各异，全部未实现）
		// ═══════════════════════════════════════════════════════════════
		{
			Key: "dify", Label: "Dify", Category: CategoryAggregator,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream,
			Available:       false,
			Notes:           "开源 LLM 应用编排平台；每个应用的兼容端点各不相同，需按应用填写地址后适配。",
		},
		{
			Key: "coze", Label: "扣子 Coze", Category: CategoryAggregator,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream,
			Available:       false,
			Notes:           "对话机器人编排平台，原生接口并非 OpenAI 协议，需专门适配。",
		},
		{
			Key: "fastgpt", Label: "FastGPT", Category: CategoryAggregator,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream,
			Available:       false,
			Notes:           "自建知识库问答平台；兼容端点随部署而变化，需自行填写地址后适配。",
		},
		{
			Key: "self_hub", Label: "自枢纽互转", Category: CategoryAggregator,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapChat | CapStream | CapTools,
			Available:       false,
			Notes:           "把本站或自建实例互为上游；协议虽兼容，但需先做环路检测与配额边界，故暂不开放。",
		},

		// ═══════════════════════════════════════════════════════════════
		// 图像生成（全部未实现）
		// ═══════════════════════════════════════════════════════════════
		{
			Key: "midjourney", Label: "Midjourney", Category: CategoryImage,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapImage | CapAsyncTask,
			Available:       false,
			Notes:           "官方无公开直连 API，实际依赖第三方中转服务，地址随服务商而异，需填写并适配。",
		},
		{
			Key: "jimeng", Label: "即梦 Jimeng", Category: CategoryImage,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapImage | CapAsyncTask,
			Available:       false,
			Notes:           "字节即梦图像生成，走火山方舟体系且为异步任务，需专用适配。",
		},
		{
			Key: "cogview", Label: "智谱 CogView", Category: CategoryImage,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://open.bigmodel.cn/api/paas/v4",
			BaseURLEditable: true,
			Caps:            CapImage,
			Available:       false,
			Notes:           "智谱 CogView 图像生成，接口形态与对话接口不同，需专用适配。",
		},
		{
			Key: "wanx", Label: "通义万相 Wanx", Category: CategoryImage,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://dashscope.aliyuncs.com",
			BaseURLEditable: true,
			Caps:            CapImage | CapAsyncTask,
			Available:       false,
			Notes:           "阿里通义万相图像生成，走 DashScope 原生异步任务接口。",
		},
		{
			Key: "replicate", Label: "Replicate", Category: CategoryImage,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.replicate.com/v1",
			BaseURLEditable: true,
			Caps:            CapImage | CapAsyncTask,
			Available:       false,
			Notes:           "按模型版本运行开源模型，采用「提交-轮询-取产物」的异步预测接口，需专用适配。",
		},
		{
			Key: "stability", Label: "Stability AI", Category: CategoryImage,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.stability.ai",
			BaseURLEditable: true,
			Caps:            CapImage,
			Available:       false,
			Notes:           "Stable Diffusion 官方服务，接口为自有格式，需专用适配。",
		},
		{
			Key: "seedream", Label: "Seedream（豆包图像）", Category: CategoryImage,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://ark.cn-beijing.volces.com/api/v3",
			BaseURLEditable: true,
			Caps:            CapImage | CapAsyncTask,
			Available:       false,
			Notes:           "字节 Seedream 图像模型，走火山方舟异步任务接口，需专用适配。",
		},
		{
			Key: "gemini_image", Label: "Gemini 图像生成", Category: CategoryImage,
			Protocol: ProtocolGemini, AuthMode: AuthQueryKey,
			DefaultBaseURL:  "https://generativelanguage.googleapis.com",
			BaseURLEditable: true,
			Caps:            CapImage,
			Available:       false,
			Notes:           "Gemini 的图像生成能力，走原生 generateContent 协议，需专用适配。",
		},
		{
			Key: "xai_image", Label: "xAI 图像生成", Category: CategoryImage,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.x.ai/v1",
			BaseURLEditable: true,
			Caps:            CapImage,
			Available:       false,
			Notes:           "xAI 图像生成，接口路径与 OpenAI 的图像端点不同，需适配后再开放。",
		},

		// ═══════════════════════════════════════════════════════════════
		// 视频生成（全部未实现）
		// ═══════════════════════════════════════════════════════════════
		{
			Key: "sora", Label: "OpenAI Sora", Category: CategoryVideo,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.openai.com/v1",
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "OpenAI 视频生成，采用异步任务接口，需专用适配。",
		},
		{
			Key: "kling", Label: "可灵 Kling", Category: CategoryVideo,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "快手可灵视频生成，异步任务接口，需填写自有地址并适配。",
		},
		{
			Key: "vidu", Label: "Vidu", Category: CategoryVideo,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "生数科技 Vidu 视频生成，异步任务接口，需专用适配。",
		},
		{
			Key: "doubao_video", Label: "豆包视频生成", Category: CategoryVideo,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://ark.cn-beijing.volces.com/api/v3",
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "字节豆包视频生成，走火山方舟异步任务接口，需专用适配。",
		},
		{
			Key: "hailuo", Label: "海螺 Hailuo（MiniMax）", Category: CategoryVideo,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "MiniMax 海螺视频生成，异步任务接口，需专用适配。",
		},
		{
			Key: "veo", Label: "Google Veo", Category: CategoryVideo,
			Protocol: ProtocolGemini, AuthMode: AuthQueryKey,
			DefaultBaseURL:  "https://generativelanguage.googleapis.com",
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "Google Veo 视频生成，走 Gemini 原生异步接口，需专用适配。",
		},
		{
			Key: "vertex_video", Label: "Vertex AI 视频生成", Category: CategoryVideo,
			Protocol: ProtocolVertex, AuthMode: AuthServiceAccount,
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "Vertex AI 上的视频生成，需服务账号与项目/区域，需专用适配。",
		},
		{
			Key: "runway", Label: "Runway", Category: CategoryVideo,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.dev.runwayml.com/v1",
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "Runway 视频生成，异步任务接口，需专用适配。",
		},
		{
			Key: "seedance", Label: "Seedance（豆包视频）", Category: CategoryVideo,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://ark.cn-beijing.volces.com/api/v3",
			BaseURLEditable: true,
			Caps:            CapVideo | CapAsyncTask,
			Available:       false,
			Notes:           "字节 Seedance 视频生成，走火山方舟异步任务接口，需专用适配。",
		},

		// ═══════════════════════════════════════════════════════════════
		// 音频（仅 OpenAI 兼容路径已实现）
		// ═══════════════════════════════════════════════════════════════
		{
			Key: "openai_audio", Label: "OpenAI 音频", Category: CategoryAudio,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL: "https://api.openai.com/v1",
			Caps:           CapAudio | CapAsyncTask,
			Available:      true,
			Notes:          "OpenAI 兼容的语音合成与识别接口（/v1/audio/*），可直接复用 OpenAI 适配器。",
		},
		{
			Key: "azure_audio", Label: "Azure 音频", Category: CategoryAudio,
			Protocol: ProtocolAzure, AuthMode: AuthAPIKeyHeader, AuthHeader: "api-key",
			BaseURLEditable: true,
			ExtraFields: []ExtraField{
				{Key: "deployment", Label: "部署名", Required: true, Help: "Azure 语音部署名称。"},
				{Key: "api_version", Label: "API 版本", Default: "2024-10-21", Required: true,
					Help: "Azure 接口版本，写错会直接 400。"},
			},
			Caps:      CapAudio | CapAsyncTask,
			Available: false,
			Notes:     "Azure 语音服务，地址与鉴权方式同 Azure OpenAI，需专用适配。",
		},
		{
			Key: "groq_whisper", Label: "Groq 语音识别", Category: CategoryAudio,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL: "https://api.groq.com/openai/v1",
			Caps:           CapAudio,
			Available:      false,
			Notes:          "Groq 托管的 Whisper 系列识别模型，接口为 OpenAI 兼容，但本网关尚未开放语音转发。",
		},
		{
			Key: "minimax_tts", Label: "MiniMax 语音合成", Category: CategoryAudio,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapAudio,
			Available:       false,
			Notes:           "MiniMax 语音合成，接口为自有格式，需专用适配。",
		},
		{
			Key: "volc_tts", Label: "火山语音合成", Category: CategoryAudio,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapAudio,
			Available:       false,
			Notes:           "火山引擎语音合成，自有签名与接口格式，需专用适配。",
		},
		{
			Key: "suno", Label: "Suno 音乐生成", Category: CategoryAudio,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			BaseURLEditable: true,
			Caps:            CapMusic | CapAsyncTask,
			Available:       false,
			Notes:           "Suno 音乐生成，异步任务接口，多以第三方中转形式接入，需填写地址并适配。",
		},
		{
			Key: "elevenlabs", Label: "ElevenLabs", Category: CategoryAudio,
			Protocol: ProtocolCustom, AuthMode: AuthAPIKeyHeader, AuthHeader: "xi-api-key",
			DefaultBaseURL:  "https://api.elevenlabs.io/v1",
			BaseURLEditable: true,
			Caps:            CapAudio,
			Available:       false,
			Notes:           "ElevenLabs 语音合成，鉴权走 xi-api-key 头，接口为自有格式，需专用适配。",
		},

		// ═══════════════════════════════════════════════════════════════
		// 嵌入与重排（仅 OpenAI 兼容嵌入已实现）
		// ═══════════════════════════════════════════════════════════════
		{
			Key: "openai_embedding", Label: "OpenAI 嵌入", Category: CategoryEmbedding,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:    "https://api.openai.com/v1",
			Caps:              CapEmbedding,
			SupportsModelList: true, Available: true,
			Notes: "OpenAI 兼容的向量嵌入接口（/v1/embeddings），可直接复用 OpenAI 适配器。",
		},
		{
			Key: "jina", Label: "Jina AI", Category: CategoryEmbedding,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.jina.ai/v1",
			BaseURLEditable: true,
			Caps:            CapEmbedding | CapRerank,
			Available:       false,
			Notes:           "Jina 的嵌入接口 OpenAI 兼容，但重排端点（/v1/rerank）尚未适配，故暂不开放。",
		},
		{
			Key: "cohere_rerank", Label: "Cohere 重排", Category: CategoryEmbedding,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.cohere.ai",
			BaseURLEditable: true,
			Caps:            CapRerank,
			Available:       false,
			Notes:           "Cohere 重排接口为自有请求格式，需专用适配。",
		},
		{
			Key: "dashscope_rerank", Label: "通义重排", Category: CategoryEmbedding,
			Protocol: ProtocolCustom, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://dashscope.aliyuncs.com",
			BaseURLEditable: true,
			Caps:            CapRerank,
			Available:       false,
			Notes:           "阿里通义重排模型，走 DashScope 原生接口，需专用适配。",
		},
		{
			Key: "siliconflow_rerank", Label: "硅基流动重排", Category: CategoryEmbedding,
			Protocol: ProtocolOpenAI, AuthMode: AuthBearer,
			DefaultBaseURL:  "https://api.siliconflow.cn/v1",
			BaseURLEditable: true,
			Caps:            CapRerank,
			Available:       false,
			Notes:           "硅基流动的重排端点（/v1/rerank）尚未适配，暂不开放。",
		},
		{
			Key: "gemini_embedding", Label: "Gemini 嵌入", Category: CategoryEmbedding,
			Protocol: ProtocolGemini, AuthMode: AuthQueryKey,
			DefaultBaseURL:  "https://generativelanguage.googleapis.com",
			BaseURLEditable: true,
			Caps:            CapEmbedding,
			Available:       false,
			Notes:           "Gemini 原生嵌入接口（embedContent），需专用适配。",
		},
	}
}
