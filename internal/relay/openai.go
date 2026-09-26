// 本文件实现 OpenAI 兼容协议的请求转发（M2 起支持多渠道路由与故障转移）。
//
// 意图（Why）：
//
//	绝大多数客户端工具（SDK、Cursor、LobeChat 等）都按 OpenAI 协议发起请求，
//	因此把 OpenAI 兼容格式作为网关的「母语」最省事：上游若也是 OpenAI 兼容实现，
//	我们只需改写目标地址与鉴权，其余原样转发，几乎零转换成本与信息损失。
//
// 流转（Flow）：
//
//	ServeChatCompletions(w, req)
//	  ├─ 步骤1 读取请求体（复用 oai.ReadBody：限长 + 还原 body）
//	  ├─ 步骤2 探测 model 字段（oai.PeekModel）
//	  ├─ 步骤3 选渠道并转发，失败按策略换渠道重试（见 forwardWithFallback）
//	  ├─ 步骤4 构造上游请求：改写 URL / Authorization，保留 Accept 与 User-Agent
//	  ├─ 步骤5 回写状态码与响应头（过滤逐跳头）
//	  └─ 步骤6 流式拷贝响应体并逐段 Flush（SSE 关键）
//
// 扩展（Extend）：
//
//	新增端点（/v1/embeddings 等）：复用 oai 包中的协议工具与
//	  forwardWithFallback，仅需处理各自的请求体差异。
//	新增协议转换：在步骤 3 之前插入"请求体转换"，在步骤 6 处插入"响应体转换"。
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// copyBufferBytes 是上游响应的拷贝缓冲区大小。
//
// 取 32KB：既能减少系统调用次数，又能让首字尽早到达客户端
// （缓冲区过大会让分片被攒住，破坏"逐字输出"的体验）。
const copyBufferBytes = 32 * 1024

// ServeChatCompletions 处理 POST /v1/chat/completions（透传 + 多渠道路由）。
//
// 参数使用标准库类型而非框架类型，目的是让 relay 包不依赖具体 Web 框架，
// 便于测试（httptest 直接调用）与将来替换框架。
func (r *Relay) ServeChatCompletions(w http.ResponseWriter, req *http.Request) {
	// ── 步骤 1：读取请求体 ──────────────────────────────────────
	// 复用 oai.ReadBody：它同时完成限长检查与 body 还原，
	// 保证鉴权中间件读过 body 后这里仍能完整读取。
	body, err := oai.ReadBody(req)
	if err != nil {
		if errors.Is(err, oai.ErrRequestTooLarge) {
			oai.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("请求体超过上限（%d 字节）", oai.MaxRequestBodyBytes),
				oai.TypeInvalidRequest, oai.CodeRequestTooLarge)
			return
		}
		oai.WriteError(w, http.StatusBadRequest, "读取请求体失败",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	// ── 步骤 2：探测 model 字段（路由与校验的输入）──────────────
	modelName, err := oai.PeekModel(body)
	if err != nil {
		if errors.Is(err, oai.ErrMissingModel) {
			oai.WriteError(w, http.StatusBadRequest, "缺少 model 字段",
				oai.TypeInvalidRequest, oai.CodeMissingModel)
			return
		}
		oai.WriteError(w, http.StatusBadRequest, "请求体不是合法的 JSON",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	// ── 步骤 3~6：转发（含失败换渠道重试）───────────────────────
	// adapter 为 nil：入站已是 OpenAI 协议，响应直接透传，无需转换。
	r.forwardWithFallback(w, req, modelName, body, nil, oai.ChatCompletionsPath)
}

// ServeEmbeddings 处理 POST /v1/embeddings（OpenAI 兼容的向量嵌入透传）。
//
// 为什么需要它：NVIDIA 免费模型里有不少 embedding / rerank / clip 类模型，
// 它们只提供 /v1/embeddings。网关若只转发对话接口，这些模型就会
// "上架了但调不通"，只能从清单里剔掉——白白浪费可用的免费算力。
//
// 实现上完全复用对话那套链路（选渠道 → 密钥池 → 重试 → 计费 → 日志），
// 唯一差别是上游路径不同。计费同样按 token：embedding 请求的 usage
// 只有 prompt_tokens，completion 为 0，公式天然成立。
func (r *Relay) ServeEmbeddings(w http.ResponseWriter, req *http.Request) {
	body, err := oai.ReadBody(req)
	if err != nil {
		if errors.Is(err, oai.ErrRequestTooLarge) {
			oai.WriteError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("请求体超过上限（%d 字节）", oai.MaxRequestBodyBytes),
				oai.TypeInvalidRequest, oai.CodeRequestTooLarge)
			return
		}
		oai.WriteError(w, http.StatusBadRequest, "读取请求体失败",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	modelName, err := oai.PeekModel(body)
	if err != nil {
		if errors.Is(err, oai.ErrMissingModel) {
			oai.WriteError(w, http.StatusBadRequest, "缺少 model 字段",
				oai.TypeInvalidRequest, oai.CodeMissingModel)
			return
		}
		oai.WriteError(w, http.StatusBadRequest, "请求体不是合法的 JSON",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	r.forwardWithFallback(w, req, modelName, body, nil, oai.EmbeddingsPath)
}

// forwardTarget 描述「一次转发尝试」的完整目标：哪个渠道 + 用哪把密钥。
//
// 为什么要额外携带密钥信息：一个渠道可能挂着几百把密钥（密钥池），
// "渠道选对了"不代表"这把密钥能用"。把密钥与重试能力一起传给转发函数，
// 才能让它在密钥级失败时做出正确决策（换同渠道的另一把，而不是换渠道）。
type forwardTarget struct {
	channel *model.Channel
	// apiKey 是本次要提交给上游的密钥（可能来自密钥池，也可能是渠道自带的单密钥）。
	apiKey string
	// keyID 是密钥池内的记录 ID；0 表示单密钥模式（渠道没有密钥池）。
	keyID uint64
	// hasSpareKey 表示同渠道池内还有本次未用过的备用密钥，可用于密钥级重试。
	hasSpareKey bool
	// hasSpareChannel 表示除本渠道外还有其他候选渠道，可用于渠道级重试。
	hasSpareChannel bool
}

// forwardWithFallback 按路由策略选择渠道与密钥并转发，失败时按失败类型重试。
//
// 重试语义（M3 起细分两类失败）：
//   - 密钥级失败（401/403/402/429）：说明"这把密钥不能用"，
//     优先在同渠道的密钥池里换一把（这是密钥池的核心价值：
//     单渠道也能靠池子消化掉失效密钥，不会因一把钥匙坏掉就整体不可用）；
//   - 渠道级失败（连接失败、500/502/503/504/529）：说明"这个上游有问题"，
//     换下一个渠道；
//   - 其他 4xx（400/404 等）：请求本身有问题，换谁都无法成功，直接透传上游响应。
//
// 重试的硬约束：只有在【尚未向客户端写出任何内容】时才允许重试，
// 因此判定必须发生在 WriteHeader 之前——状态码一旦发出就无法撤回。
//
// 参数 adapter 为 nil 时按 OpenAI 协议原样透传；非 nil 时由适配器
// 把上游响应转换为下游协议（见 adapter.go）。
//
// 参数 upstreamPath 是上游要请求的端点路径（如 /v1/chat/completions、
// /v1/embeddings）。参数化的原因：不同下游能力对应上游不同端点，
// 但"选渠道 / 密钥池 / 重试 / 计费 / 日志"这一整套逻辑完全相同，
// 不该为了多一个端点而复制一遍转发实现。
func (r *Relay) forwardWithFallback(w http.ResponseWriter, req *http.Request, modelName string, body []byte, adapter Adapter, upstreamPath string) {
	// 按开关决定是否给流式请求注入 stream_options.include_usage（默认关闭，
	// 取舍见 usage.go 的 injectStreamUsageOption）。只在这里改写一次，
	// 保证同一次请求的各次重试使用完全相同的请求体。
	body = withStreamUsageOption(body, injectStreamUsageOption)

	// 一次性取出候选集：同一次请求内的多次重试都基于它挑选，避免每次重试都查库
	candidates, err := r.listCandidates(req.Context(), modelName)
	if err != nil {
		// 仓储查询失败：不向客户端暴露细节
		// TODO(relay): 接入结构化日志后在此记录 err
		writeAdaptedError(w, adapter, http.StatusInternalServerError, "网关内部错误",
			oai.TypeServer, oai.CodeInternal)
		return
	}
	if len(candidates) == 0 {
		// 一个可用渠道都没有：属于"配置/容量"问题。
		// 用 503（而非 500）表达"暂时无可用后端"，客户端稍后重试可能成功。
		//
		// 注意：这种情况也记一条日志——"配置漏了模型"是常见事故，
		// 若不留痕，站长只能看到用户报错却查不到原因。
		r.recordUsage(req.Context(), usageEntry{
			UserID:     identityFromRequest(req.Context()).UserID,
			TokenID:    identityFromRequest(req.Context()).TokenID,
			Model:      modelName,
			IsStream:   oai.PeekStream(body),
			StatusCode: http.StatusServiceUnavailable,
			ErrorText:  "无可用渠道",
		})
		writeAdaptedError(w, adapter, http.StatusServiceUnavailable,
			"当前没有可用的上游渠道能处理该模型",
			oai.TypeServer, oai.CodeNoAvailableChannel)
		return
	}

	excludedChannels := make(map[uint64]struct{}) // 本次请求已放弃的渠道
	usedKeys := make(map[uint64]struct{})         // 本次请求已用过的密钥（避免重复撞同一把）

	// 两套独立的尝试预算（重要，别合并成一个）：
	//
	//   · channelAttempts：渠道级失败（连不上、5xx）的预算 = maxAttempts。
	//     渠道级失败每次都要重新建连，代价高，必须严格限制，否则故障时延迟被放大。
	//   · 密钥级失败不走渠道预算，而是由 keyAttempts 单独计量，上限 maxKeyLevelAttempts。
	//
	// 为什么密钥级需要更大的预算：一个渠道的密钥池可能来自几百个不同账号
	// （例如 NVIDIA NIM 的免费额度池），而每个账号的模型授权是不同的——
	// 同一个模型在 A 账号"没有权限"、在 B 账号完全可用。
	// 此时"换一把密钥"的成本极低（同一上游、复用连接、上游是立即拒绝的），
	// 所以值得多试几把；若沿用 3 次的渠道预算，绝大多数请求会白跑一趟。
	channelAttempts := 0
	// lastFailure 记录"最后一次上游失败"，用于所有重试耗尽后透传真实原因
	var lastFailure upstreamFailure
retryLoop:
	for keyAttempts := 1; keyAttempts <= maxKeyLevelAttempts; keyAttempts++ {
		ch := pickCandidate(candidates, excludedChannels)
		if ch == nil {
			// 候选渠道已全部放弃，退出循环统一报错
			break
		}

		apiKey, keyID, ok, hasSpareKey := r.resolveChatKey(req.Context(), ch, usedKeys)
		if !ok {
			// 该渠道当前没有可用密钥（池内全部被禁用或自动摘除）：
			// 直接放弃这个渠道，避免白白消耗一次尝试预算。
			excludedChannels[ch.ID] = struct{}{}
			continue
		}
		if keyID != 0 {
			usedKeys[keyID] = struct{}{}
		}

		target := forwardTarget{
			channel:         ch,
			apiKey:          apiKey,
			keyID:           keyID,
			hasSpareKey:     hasSpareKey,
			hasSpareChannel: r.hasOtherChannel(candidates, excludedChannels, ch.ID),
		}

		switch r.forwardChat(w, req, target, modelName, body, adapter, upstreamPath, &lastFailure) {
		case forwardResponded:
			return
		case forwardRetryKey:
			// 密钥级失败：保留该渠道，下一轮会从它的池里换一把密钥
			continue
		case forwardRetryChannel:
			excludedChannels[ch.ID] = struct{}{}
			channelAttempts++
			if channelAttempts >= r.maxAttempts {
				// 渠道预算耗尽：上游整体故障时不该无限试下去
				break retryLoop
			}
			continue
		}
	}

	// 所有尝试都用尽：优先把上游最后一次的真实响应透传出去。
	//
	// 为什么不统一回 502：多账号密钥池下最常见的失败是
	// "该模型在池内所有账号上都没有授权"，上游已经明确说了原因
	// （如 NVIDIA 的 "Not found for account"）。
	// 把它换成含糊的"所有候选渠道均请求失败"，会让用户与管理员
	// 都无从判断该换渠道、换模型还是换账号。
	if lastFailure.status != 0 {
		message := extractUpstreamErrorMessage(lastFailure.body)
		if message == "" {
			message = fmt.Sprintf("上游返回 HTTP %d", lastFailure.status)
		}

		r.recordUsage(req.Context(), usageEntry{
			UserID:     identityFromRequest(req.Context()).UserID,
			TokenID:    identityFromRequest(req.Context()).TokenID,
			Model:      modelName,
			IsStream:   oai.PeekStream(body),
			StatusCode: lastFailure.status,
			ErrorText:  truncateReason(message),
		})

		if adapter == nil {
			// 直通路径：状态码与响应体原样透传，客户端可自助排查
			copyResponseHeaders(w.Header(), lastFailure.header)
			w.WriteHeader(lastFailure.status)
			_, _ = w.Write(lastFailure.body)
			return
		}
		// 转换路径：按下游协议生成错误（各协议的 Content-Type 不同）
		writeAdaptedError(w, adapter, lastFailure.status, message,
			oai.TypeInvalidRequest, oai.CodeUpstreamRequestFailed)
		return
	}

	// 没有任何可透传的上游响应（例如候选渠道为空、全部连不上）：
	// 记一条日志（channel_id 为 0，因为没有一个渠道成功完成会话）
	r.recordUsage(req.Context(), usageEntry{
		UserID:     identityFromRequest(req.Context()).UserID,
		TokenID:    identityFromRequest(req.Context()).TokenID,
		Model:      modelName,
		IsStream:   oai.PeekStream(body),
		StatusCode: http.StatusBadGateway,
		ErrorText:  "所有候选渠道均请求失败",
	})
	writeAdaptedError(w, adapter, http.StatusBadGateway, "所有候选渠道均请求失败",
		oai.TypeServer, oai.CodeUpstreamRequestFailed)
}

// resolveChatKey 为一次转发解析出要使用的上游凭据。
//
// 返回：凭据值（API Key 或 OAuth access_token）、池内 ID（单密钥模式为 0）、
// 是否解析成功、同渠道是否还有备用凭据。
//
// 四种情形：
//  1. 渠道配置了凭据池 → 从"启用且本次未用过"的凭据中随机挑一条；
//  2. 挑中的是 OAuth 凭据且即将过期 → 先刷新再使用；
//  3. 渠道没有凭据池（历史数据） → 使用渠道自带的单密钥，保持向后兼容；
//  4. 池内凭据本次已全部试过或全部被摘除 → 返回 ok=false，让上层换渠道。
//
// 容错：凭据池查询失败时不阻断转发，而是退回单密钥——
// 统计能力不应该成为转发链路上的单点故障。
func (r *Relay) resolveChatKey(ctx context.Context, ch *model.Channel, used map[uint64]struct{}) (string, uint64, bool, bool) {
	if r.keys != nil {
		if pool, err := r.keys.ListUsable(ctx, ch.ID); err == nil && len(pool) > 0 {
			// 循环而非单次挑选：OAuth 凭据可能因刷新失败而不可用，
			// 此时应换池内下一条，而不是让整个请求失败。
			for attempt := 0; attempt < maxCredentialAttempts; attempt++ {
				available := make([]*model.ChannelKey, 0, len(pool))
				for _, k := range pool {
					if _, dup := used[k.ID]; dup {
						continue
					}
					available = append(available, k)
				}
				if len(available) == 0 {
					return "", 0, false, false
				}

				picked := model.PickKey(available)
				// 记录使用时间：失败不影响本次转发（这是展示性数据，不是控制流）
				_ = r.keys.MarkUsed(ctx, picked.ID, time.Now())

				value := picked.CredentialValue()
				if r.oauth != nil && picked.NeedsRefresh(time.Now()) {
					fresh, err := r.oauth.EnsureFresh(ctx, picked)
					if err != nil {
						// 刷新失败：计入连续失败（达阈值会被自动摘除），
						// 标记为本次已用过并换下一条凭据
						_ = r.keys.MarkFailure(ctx, picked.ID, truncateReason("刷新令牌失败: "+err.Error()))
						used[picked.ID] = struct{}{}
						continue
					}
					value = fresh
				}

				if strings.TrimSpace(value) == "" {
					// 凭据内容为空（配置错误）：跳过并计一次失败，
					// 否则它会一直占着池子却永远发不出请求
					_ = r.keys.MarkFailure(ctx, picked.ID, "凭据内容为空")
					used[picked.ID] = struct{}{}
					continue
				}
				return value, picked.ID, true, len(available) > 1
			}
			return "", 0, false, false
		}
	}

	if ch.APIKey == "" {
		return "", 0, false, false
	}
	return ch.APIKey, 0, true, false
}

// maxCredentialAttempts 是单次请求内最多尝试的凭据条数。
//
// 取 3：既能在"个别 OAuth 账号刷新失败"时快速换到可用凭据，
// 又不会因为池里存在大量坏凭据而让单个请求长时间打转
// （每个坏凭据都要等一次刷新超时，代价不低）。
const maxCredentialAttempts = 3

// maxKeyLevelAttempts 是单次请求内最多尝试的凭据轮数（凭据级重试预算）。
//
// 取值 8 的权衡：
//   - 一个渠道挂几百把密钥时，个别密钥被限流（429）或临时失效是常态，
//     多试几把能显著提升成功率，而这些失败都是毫秒级的、不消耗算力；
//   - 但不能无限试：上游整体故障时会把延迟放大到不可接受。
//
// 注意它【不适用于"该账号没有这个模型"】这类失败：实测本部署的
// 500 把免费密钥授权完全一致（3 把取样密钥对 8 个模型结论逐一相同），
// 所以"某模型在 A 账号没有"就等于"在整池都没有"，换密钥毫无意义，
// 只会白白多花几秒。这类失败由 keyFailureEntitlement 单独处理：直接透传上游原因。
const maxKeyLevelAttempts = 8

// keyFailureKind 表示一次上游失败与"凭据"的关系，决定是否可以换密钥重试。
type keyFailureKind int

const (
	// keyFailureNone 与凭据无关（如请求体有误、上游 5xx）：
	// 换密钥与换渠道都没用，应直接透传上游响应。
	keyFailureNone keyFailureKind = iota
	// keyFailureCredential 凭据本身不可用（失效 / 受限 / 被限流）：
	// 池内换一把很可能成功，值得重试。
	keyFailureCredential
	// keyFailureEntitlement 该凭据所在账号没有这个模型 / 无权访问：
	// 池内账号同质时换密钥无意义，应直接透传上游原因，
	// 让使用者一眼看出"模型不在可用范围内"（而不是等几秒后拿到含糊的 502）。
	keyFailureEntitlement
)

// upstreamFailure 记录"最后一次上游失败"的原始信息。
//
// 为什么需要它：当所有重试都用尽时，若只回一句"所有候选渠道均请求失败"（502），
// 用户与管理员都无从判断到底是模型不存在、账号没权限、还是上游故障。
// 把上游最后一次响应（状态码 + 响应头 + 响应体片段）留存下来原样透传，
// 才能让人一眼看懂"该模型在当前账号池里确实不可用"。
type upstreamFailure struct {
	status int
	header http.Header
	body   []byte
}

// keyLevelPeekBytes 是判定"凭据被拒"时窥探响应体的字节数。
//
// 取 4KiB：各类上游的错误说明都很短，足够覆盖；同时避免为判断读入大响应。
const keyLevelPeekBytes = 4 << 10

// keyLevelRejectionMarkers 是"上游明确指出这把凭据/这个账号不可用"的文本特征。
//
// 为什么需要文本判据：有些上游用 400/404 表达"该账号没有这个模型"
// （NVIDIA NIM 返回 404 + application/problem+json，内容形如
// "Function '...': Not found for account '...'"）。
// 只看状态码会把这类"换把密钥就能成功"的情形误判为"请求本身有错"。
var keyLevelRejectionMarkers = []string{
	"not found for account",
	"no permission",
	"permission denied",
	"does not have access",
	"you do not have access",
	"not authorized",
	"unauthorized",
	"invalid api key",
	"api key is invalid",
	"incorrect api key",
	"account is not authorized",
	"quota",
	"rate limit",
	"exceeded",
}

// bodyWithPrefix 把"已读走的前缀"与"剩余部分"重新组合为 ReadCloser。
//
// 用途：判断错误类型时需要读一小段响应体，读完之后必须把响应体还原，
// 否则后续"把上游错误透传给客户端"的路径会读到残缺内容。
type bodyWithPrefix struct {
	io.Reader
	closer io.Closer
}

// Close 关闭底层响应体。
func (b bodyWithPrefix) Close() error { return b.closer.Close() }

// peekBody 读取响应体开头的一段并还原它，返回读到的内容。
func peekBody(resp *http.Response, limit int) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}

	buf := make([]byte, limit)
	n, err := io.ReadFull(resp.Body, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	peek := buf[:n]

	// 还原：先用已读到的前缀，再接上尚未读完的部分
	resp.Body = bodyWithPrefix{
		Reader: io.MultiReader(bytes.NewReader(peek), resp.Body),
		closer: resp.Body,
	}
	return peek, nil
}

// extractUpstreamErrorMessage 从上游错误体里取一句可读说明。
//
// 兼容两种常见形态：
//   - OpenAI 风格：{"error":{"message":"..."}}
//   - RFC7807 问题详情：{"title":"Not Found","detail":"Function ...: Not found for account ..."}
//     （NVIDIA NIM 用的就是这种）
//
// 这样"上游到底说了什么"才能如实呈现在错误信息里。
func extractUpstreamErrorMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if message := extractErrorMessage(raw); message != "" && message != "上游请求失败" {
		return message
	}

	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &problem); err == nil {
		if strings.TrimSpace(problem.Detail) != "" {
			return problem.Detail
		}
		if strings.TrimSpace(problem.Title) != "" {
			return problem.Title
		}
	}
	return ""
}

// classifyKeyFailure 判断这次上游失败与"凭据"是什么关系。
//
// 判据分三类（顺序不能反）：
//  1. 401 / 403 / 402 / 429：凭据本身不可用（失效 / 受限 / 被限流）→ 值得换密钥；
//  2. 400 / 404 且响应体文本表明"该账号无权访问/没有这个模型"
//     → 属于授权范围问题，同质账号池里换密钥无意义；
//  3. 其余（请求体有误、5xx 等）→ 与凭据无关。
//
// 返回 snippet 供"所有重试都用尽"时透传上游真实原因。
func (r *Relay) classifyKeyFailure(resp *http.Response) (keyFailureKind, string, []byte) {
	if resp == nil {
		return keyFailureNone, "", nil
	}
	if isKeyLevelFailure(resp.StatusCode) {
		return keyFailureCredential, fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode), nil
	}
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
		return keyFailureNone, "", nil
	}

	peek, err := peekBody(resp, keyLevelPeekBytes)
	if err != nil || len(peek) == 0 {
		// 读不到内容就无法判定：按"请求本身的问题"处理，
		// 宁可少重试，也不要因为一次读取失败而做无谓的密钥轮换。
		return keyFailureNone, "", nil
	}

	lowered := strings.ToLower(string(peek))
	for _, marker := range keyLevelRejectionMarkers {
		if strings.Contains(lowered, marker) {
			return keyFailureEntitlement, fmt.Sprintf("上游返回 HTTP %d，并指出该账号无权访问（命中 %q）",
				resp.StatusCode, marker), peek
		}
	}
	return keyFailureNone, "", nil
}

// saveFailure 记录一次上游失败响应，供后续"所有重试都用尽"时透传真实原因。
//
// 复制响应头而不是直接引用：响应体关闭后引用仍可用，但复制能避免调用方误改。
func saveFailure(lastFailure *upstreamFailure, resp *http.Response, snippet []byte) {
	if lastFailure == nil || resp == nil || len(snippet) == 0 {
		return
	}
	lastFailure.status = resp.StatusCode
	lastFailure.header = resp.Header.Clone()
	lastFailure.body = snippet
}

// truncateReason 截断失败原因，避免超长错误信息撑大数据库字段。
func truncateReason(reason string) string {
	const maxLength = 200
	if len(reason) <= maxLength {
		return reason
	}
	return reason[:maxLength]
}

// hasOtherChannel 判断"放弃当前渠道后"是否还有其他候选渠道。
//
// 实现方式：临时把当前渠道加入排除集后再挑一次（仅探测，不真正使用结果）。
// 之所以需要它：客户端可收到的错误信息质量取决于此——
// 若已无退路，应当把上游的真实错误（如 401 密钥无效）透传给客户端，
// 而不是丢弃它返回含糊的 502。
func (r *Relay) hasOtherChannel(candidates []*model.Channel, excluded map[uint64]struct{}, currentID uint64) bool {
	probe := make(map[uint64]struct{}, len(excluded)+1)
	for id := range excluded {
		probe[id] = struct{}{}
	}
	probe[currentID] = struct{}{}
	return pickCandidate(candidates, probe) != nil
}

// markKeyFailure 记录一次密钥失败（连续失败达阈值时由仓储自动摘除该密钥）。
//
// 刻意忽略错误：这是统计与自愈用途，失败不应影响对客户端的响应。
func (r *Relay) markKeyFailure(ctx context.Context, keyID uint64, reason string) {
	if r.keys == nil || keyID == 0 {
		return
	}
	_ = r.keys.MarkFailure(ctx, keyID, reason)
}

// markKeySuccess 记录一次密钥成功（清零连续失败计数）。
func (r *Relay) markKeySuccess(ctx context.Context, keyID uint64) {
	if r.keys == nil || keyID == 0 {
		return
	}
	_ = r.keys.MarkSuccess(ctx, keyID)
}

// forwardOutcome 描述一次转发的结局，用于决定后续重试方向。
type forwardOutcome int

const (
	// forwardResponded 表示已向客户端回写响应（含上游返回错误码的情况）。
	//
	// 重要：一旦进入该状态就【不能】再重试——HTTP 状态码已经发出，无法撤回。
	forwardResponded forwardOutcome = iota
	// forwardRetryKey 表示密钥级失败且同渠道还有备用密钥：应换密钥重试。
	forwardRetryKey
	// forwardRetryChannel 表示渠道级失败且还有其他候选渠道：应换渠道重试。
	forwardRetryChannel
)

// forwardChat 把请求转发到指定目标（渠道 + 密钥），并把上游响应回写给客户端。
//
// 返回值表示结局，供上层决定重试方向（见 forwardOutcome）。
//
// 参数 lastFailure 非 nil 时，会把"凭据级失败"的上游响应原样记进去，
// 供上层在重试全部用尽后透传真实原因（而不是含糊的 502）。
//
// 参数 upstreamPath 为上游端点路径，由调用方按下游能力指定。
func (r *Relay) forwardChat(w http.ResponseWriter, req *http.Request, target forwardTarget, modelName string, body []byte, adapter Adapter, upstreamPath string, lastFailure *upstreamFailure) forwardOutcome {
	// 记录起始时间用于计算耗时（写入调用日志）
	start := time.Now()
	ch := target.channel

	// 拼接上游地址：去掉 base_url 末尾多余的斜杠，避免出现 "//v1/..." 这类路径
	upstreamURL := strings.TrimRight(ch.BaseURL, "/") + upstreamPath

	// 用请求 context：客户端断开时自动取消上游请求，避免无谓的上游消耗
	upReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		// 请求构造失败属于网关侧问题，未向上游发出请求，可换渠道重试
		return forwardRetryChannel
	}

	// 请求头策略：只设置必要的头，【绝不复用客户端的 Authorization】——
	// 客户端带的是本网关的令牌，上游需要的是渠道密钥，二者混用会导致
	// 上游鉴权失败，并把网关令牌泄露给第三方上游。
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+target.apiKey)

	// 保留 Accept：客户端可能要求 text/event-stream（流式），这是协议协商的一部分
	if accept := req.Header.Get("Accept"); accept != "" {
		upReq.Header.Set("Accept", accept)
	}
	// 保留 User-Agent：部分上游按 UA 做风控或功能分级，透传可减少非预期差异
	if ua := req.Header.Get("User-Agent"); ua != "" {
		upReq.Header.Set("User-Agent", ua)
	}

	resp, err := r.client.Do(upReq)
	if err != nil {
		// 连接层面失败（超时、连接被拒、TLS 失败）：未写出任何响应，可安全重试。
		//
		// 刻意【不】记为密钥失败：连接失败与密钥无关，
		// 若计入失败次数，几次网络抖动就会把池里的好密钥误杀干净。
		//
		// 也不在此记日志：多次重试会产生多条记录，把请求数统计放大；
		// 最终失败会在 forwardWithFallback 的统一出口处记录一条。
		return forwardRetryChannel
	}
	defer func() { _ = resp.Body.Close() }()

	// ── 失败分流（关键：决定"换密钥"、"换渠道"还是直接透传）────
	switch kind, reason, snippet := r.classifyKeyFailure(resp); kind {
	case keyFailureCredential:
		// 凭据不可用（失效/受限/被限流）：记一次失败，达阈值会被自动摘除
		if target.keyID != 0 {
			r.markKeyFailure(req.Context(), target.keyID, truncateReason(reason))
		}
		// 留存响应，供后续所有重试都失败时透传真实原因
		saveFailure(lastFailure, resp, snippet)

		if target.hasSpareKey {
			// 同渠道还有别的密钥：丢弃本次响应，换一把密钥重试
			drainAndClose(resp)
			return forwardRetryKey
		}
		if target.hasSpareChannel {
			drainAndClose(resp)
			return forwardRetryChannel
		}
		// 已无退路：把上游错误原样透传，客户端据此知道"密钥是失效的"

	case keyFailureEntitlement:
		// 该账号没有这个模型 / 无权访问：不是密钥坏了，而是"这个模型不在可用范围内"。
		//
		// 为什么不换密钥重试：实测本部署的密钥池来自同质的免费账号，
		// 授权集合完全一致，换多少把结果都一样，只会白白多花几秒。
		// 直接透传上游原因，使用者能立刻判断"该模型不可用"。
		saveFailure(lastFailure, resp, snippet)

		if target.hasSpareChannel {
			// 但另一个渠道可能是别的上游，值得一试
			drainAndClose(resp)
			return forwardRetryChannel
		}
		// 无退路：透传（保持上游原文，含它的 status 与 detail）

	default:
		if isRetryableStatus(resp.StatusCode) && target.hasSpareChannel {
			// 上游故障/过载，且还有别的渠道可试
			drainAndClose(resp)
			return forwardRetryChannel
		}
	}

	// ── 步骤 5~6：回写响应 ──────────────────────────────────────
	// 同时把内容喂给抓取器，用于事后解析 usage（token 数）。
	sniffer := newUsageSniffer()

	if adapter == nil {
		// 直通路径：入站与上游同为 OpenAI 协议，状态码与响应体原样透传
		// （上游的错误状态码如 401/403 一并透传，便于客户端自助排查）。
		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		flushCopy(w, resp.Body, sniffer)
	} else {
		// 转换路径：由适配器把上游的 OpenAI 响应改写为下游协议格式。
		// 注意此时响应头由 writeAdapted 决定（各协议的 Content-Type 不同）。
		r.writeAdapted(w, req, resp, adapter, sniffer, body)
	}

	// 成功响应：清零该密钥的连续失败计数（"连续失败"语义要求成功即重置）
	if resp.StatusCode < http.StatusMultipleChoices {
		r.markKeySuccess(req.Context(), target.keyID)
	}

	// 记录用量。此时响应已完整回传，写库不会影响客户端感知的延迟。
	// usage 由 sniffer 【增量】解析：无论响应多长、usage 出现在最后一个 SSE 事件里，
	// 只要上游返回过就能取到（旧实现受 256KB 上限影响，长回答会被整段丢弃而记 0）。
	usage, hasUsage := sniffer.Usage()
	identity := identityFromRequest(req.Context())
	entry := usageEntry{
		UserID:     identity.UserID,
		TokenID:    identity.TokenID,
		ChannelID:  ch.ID,
		Model:      modelName,
		Usage:      usage,
		LatencyMS:  int(time.Since(start).Milliseconds()),
		IsStream:   oai.PeekStream(body),
		StatusCode: resp.StatusCode,
	}
	if !hasUsage {
		// 上游确实没返回 usage：token 只能记 0，但必须留下标注，
		// 让计费缺口在日志里可见（该列仅作提示，成功/失败仍以状态码为准）。
		entry.ErrorText = usageMissingNote
	}
	r.recordUsage(req.Context(), entry)

	return forwardResponded
}

// drainAndClose 在读空响应体后交由调用方关闭。
//
// 为什么要读完再关：直接关闭会让底层 TCP 连接无法复用，
// 高并发故障场景下会退化为"每次重试都重新建连"，进一步放大故障。
// 只读有限长度，避免恶意上游用超大响应体拖慢网关。
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
}

// maxDrainBytes 是重试前丢弃上游响应体的最大读取量。
//
// 取值 64KiB：典型错误体只有几百字节，64KiB 足够读完以维持连接复用，
// 同时避免异常上游用超大响应体拖慢或撑爆网关。
const maxDrainBytes = 64 << 10

// isKeyLevelFailure 判断上游状态码是否属于「这把密钥自身的问题」。
//
// 判定原则："换同一渠道的另一把密钥很可能成功"才归为密钥级失败。
// 这类失败的处置优先级是「换密钥」高于「换渠道」——因为换密钥更便宜
// （同一上游、同一连接池、无需重新握手），而且能保留原本更优的渠道。
//
//   - 401：密钥无效或被吊销；
//   - 403：密钥无权访问该模型（免费额度密钥常见）；
//   - 402：余额/额度不足（部分上游的约定）；
//   - 429：该密钥被限流或额度耗尽（同一上游换一把密钥通常立即恢复）。
//
// 注意 429 同时出现在 isRetryableStatus 中：它既是密钥级也是可重试的，
// 但本函数优先把它当密钥处理（见 forwardChat 的分支顺序）。
func isKeyLevelFailure(status int) bool {
	switch status {
	case http.StatusUnauthorized, // 401
		http.StatusForbidden,       // 403
		http.StatusPaymentRequired, // 402
		http.StatusTooManyRequests: // 429
		return true
	default:
		return false
	}
}

// isRetryableStatus 判断上游状态码是否值得换渠道重试。
//
// 判定原则："换一个渠道很可能成功"才重试：
//   - 500/502/503/504：上游服务端故障；
//   - 529：上游过载（Anthropic 等厂商的约定状态码）；
//   - 429：限流/额度不足。在单密钥渠道下换渠道通常立即改善；
//     在密钥池渠道下会先尝试换密钥（见 isKeyLevelFailure）。
//
// 明确不重试的常见状态码及原因：
//   - 400/422：请求参数有误，换渠道同样失败；
//   - 401/403：在单密钥渠道下换渠道无意义（错误来自这把密钥本身）；
//   - 404：模型不存在，属于配置问题；
//   - 408/499：客户端侧问题。
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		529:                            // 上游过载（非标准码，厂商约定）
		return true
	default:
		return false
	}
}

// copyResponseHeaders 把上游响应头复制到客户端，并过滤掉不应转发的头。
//
// 过滤原因：
//   - 逐跳头（hop-by-hop）仅在单段连接内有效，代理不应转发；
//   - Content-Length / Transfer-Encoding 描述的是"上游连接"的消息边界，
//     我们回写时由 Go 的 http 包重新决定帧格式，直接复制可能造成长度不一致。
func copyResponseHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// hopByHopHeaders 是 HTTP/1.1 规范定义的逐跳头（不应被代理转发）。
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	// 消息边界由本机重新生成，不透传上游的值
	"Content-Length": {},
}

// isHopByHopHeader 判断响应头是否应被过滤（大小写不敏感）。
func isHopByHopHeader(key string) bool {
	_, found := hopByHopHeaders[http.CanonicalHeaderKey(key)]
	return found
}

// flushCopy 流式拷贝响应体，并在每个分片后立即 Flush。
//
// 参数 tee 可为 nil；非 nil 时会把内容同时写入它（用于抓取响应以解析 usage）。
// tee 的写入始终不返回错误（见 usageSniffer.Write 的实现），因此不会干扰转发。
//
// 为什么必须 Flush：大模型流式回答依赖 SSE，若数据被缓冲在网关或 HTTP 层，
// 客户端的体验会从"逐字出现"退化为"等全文生成完再一次性蹦出来"，
// 与不经网关直连相比是明显的体验倒退。
func flushCopy(w http.ResponseWriter, src io.Reader, tee io.Writer) {
	// gin 的 ResponseWriter 实现了 http.Flusher；用类型断言兼容不支持刷新的实现
	flusher, canFlush := w.(http.Flusher)

	buf := make([]byte, copyBufferBytes)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if tee != nil {
				_, _ = tee.Write(buf[:n])
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				// 客户端已断开（例如用户取消），无需继续读取上游，直接结束
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			// io.EOF 表示正常结束；其他错误（上游中断）此时已无法补救，
			// 因为响应头早已发出，只能结束本次传输。
			return
		}
	}
}
