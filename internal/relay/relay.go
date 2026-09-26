// Package relay 是网关的核心域：把客户端的模型请求转发到选定的上游渠道。
//
// 意图（Why）：
//
//	网关存在的根本价值在于"统一入口 + 智能转发"。本包负责其中最关键的一环：
//	  1) 根据请求中的模型名，从渠道池中选出可用渠道；
//	  2) 改写目标地址与鉴权信息，向上游发起请求；
//	  3) 把上游响应（含 SSE 流式分片）原样回写给客户端。
//	M1 阶段只做"单渠道透传"——不转换协议、不做计费、不做重试；
//	这些能力会在 M2+ 按里程碑逐步加入，但接口形态保持一致。
//
// 流转（Flow）：
//
//	server/router.go
//	  └─ POST /v1/chat/completions → Relay.ServeChatCompletions
//	       ├─ 读取并解析请求体（仅取 model 字段用于路由，其余原样透传）
//	       ├─ Relay.SelectChannel(reqCtx, model)  选渠道
//	       ├─ forwardChat(...)                    构造上游请求并发送
//	       └─ flushCopy(...)                      流式回写响应体
//
// 扩展（Extend）：
//
//	新增协议（如 Anthropic Messages）：新建同级的 anthropic.go，
//	  复用本文件的 SelectChannel，并在 server/router.go 注册对应路径。
//	新增路由策略（负载均衡、粘性会话）：只改 SelectChannel 的实现，
//	  调用方（openai.go 等）无需变动——这是把选渠道抽成方法的意义。
//	新增重试/熔断：在 forwardChat 外层包一层循环，配合"排除已失败渠道"。
package relay

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// ErrNoAvailableChannel 表示当前没有任何可用渠道能处理请求的模型。
//
// 这是运维最常见的故障信号（渠道被禁用/模型未声明），因此单独定义，
// 便于上层层返回明确的 503 与可操作的提示。
var ErrNoAvailableChannel = errors.New("relay: 没有可用的上游渠道")

// 上游 HTTP 客户端的默认参数。
const (
	defaultGroup               = "default"        // 默认分组
	defaultResponseHeaderWait  = 60 * time.Second // 等待上游返回响应头的上限（首字节）
	defaultTLSHandshakeTimeout = 10 * time.Second // TLS 握手超时
	defaultIdleConnTimeout     = 90 * time.Second // 空闲连接回收时间
	defaultMaxIdleConnsPerHost = 20               // 每个上游主机的空闲连接上限

	// defaultMaxAttempts 是单次请求最多尝试的渠道数。
	//
	// 取 3 的权衡：过小则故障转移能力弱（一个渠道抖动就直接报错），
	// 过大则故障时延迟被放大（每次重试都要重新建连），且会加剧上游压力。
	// 3 次足以覆盖"个别渠道故障"，同时把最坏延迟控制在可接受范围。
	defaultMaxAttempts = 3
)

// Options 是 Relay 的可选参数。
type Options struct {
	// Group 指定默认路由分组。为空时使用 defaultGroup。
	Group string
	// ResponseHeaderTimeout 是等待上游响应头的超时（即"首字节时间"）。
	// 注意它与"整个请求超时"不同：流式响应可能持续数分钟，
	// 我们只限制"上游多久没开始响应"，而不是"响应多久必须结束"。
	ResponseHeaderTimeout time.Duration
	// MaxAttempts 是单次请求最多尝试的次数（含首次）。
	//
	// 说明：这里计的是"总尝试预算"，密钥级重试与渠道级重试共用同一预算——
	// 这样最坏延迟可预期（不会因为"渠道内换密钥 × 渠道间切换"而相乘放大）。
	// 池内失效密钥由自动摘除机制在数次请求后逐步清理，无需靠单次请求穷举。
	// <=0 时使用 defaultMaxAttempts。
	MaxAttempts int
	// UsageLogs 为调用日志仓储；为 nil 时不记录用量（便于单元测试）。
	UsageLogs model.UsageLogRepository
	// Tokens 为访问令牌仓储，用于更新令牌的最近使用时间；可为 nil。
	Tokens model.TokenRepository
	// Keys 为渠道密钥池仓储；为 nil 时退化为"每渠道单密钥"模式（便于单元测试）。
	Keys model.ChannelKeyRepository
	// Billing 为计费组件；为 nil 时只记录用量而不扣减额度。
	Billing *Billing
}

// Relay 是转发引擎，持有渠道仓储与上游 HTTP 客户端。
//
// 并发安全：所有字段在构造后不再修改，可被多 goroutine 共享。
type Relay struct {
	channels    model.ChannelRepository
	client      *http.Client
	group       string
	maxAttempts int

	// usageLogs / tokens 用于转发后记录用量与令牌使用时间，两者均可为 nil。
	usageLogs model.UsageLogRepository
	tokens    model.TokenRepository
	// keys 为渠道密钥池仓储（可选）。为 nil 时行为退化为单密钥。
	keys model.ChannelKeyRepository
	// billing 为计费组件（可选）。为 nil 时不扣费，仅记录用量。
	billing *Billing
}

// New 创建转发引擎。
//
// 关于 HTTP 客户端的超时设计（重要）：
//
//	刻意【不设置】http.Client.Timeout。该字段覆盖"从发起请求到读完响应体"的全过程，
//	而大模型流式响应可能持续数分钟，设置后会导致长回答被强制中断。
//	正确的做法是分层控制超时：
//	  - ResponseHeaderTimeout：限制上游多久必须开始响应（防挂死）；
//	  - 客户端断连：由 request context 自动取消（http.NewRequestWithContext）；
//	  - 总时长上限：后续里程碑在业务层按模型配置。
func New(channels model.ChannelRepository, opts Options) *Relay {
	group := opts.Group
	if group == "" {
		group = defaultGroup
	}
	headerTimeout := opts.ResponseHeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = defaultResponseHeaderWait
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	return &Relay{
		channels:    channels,
		group:       group,
		maxAttempts: maxAttempts,
		usageLogs:   opts.UsageLogs,
		tokens:      opts.Tokens,
		keys:        opts.Keys,
		billing:     opts.Billing,
		client: &http.Client{
			Transport: &http.Transport{
				// 走系统代理环境变量：便于在受限网络中经代理访问上游
				Proxy: http.ProxyFromEnvironment,

				// 连接复用：网关是高频转发场景，复用连接可显著降低延迟
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   defaultMaxIdleConnsPerHost,
				IdleConnTimeout:       defaultIdleConnTimeout,
				TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
				ExpectContinueTimeout: 1 * time.Second,

				// 上游"必须开始响应"的时限，避免连接挂死占用资源
				ResponseHeaderTimeout: headerTimeout,

				// 上游普遍支持 HTTP/2，开启可提升多路复用效率
				ForceAttemptHTTP2: true,
			},
		},
	}
}

// listCandidates 查询本分组的启用渠道，并过滤出支持该模型的候选。
//
// 返回结果保持仓储给出的顺序：优先级降序 → 权重降序 → ID 升序。
// 路由逻辑依赖"优先级降序"这一前提来分层，故不可在此重排。
//
// 为什么"一次查询、多次挑选"：故障转移会在同一请求内多次选渠道。
// 若每次重试都查一次库，故障场景下会把数据库压力放大数倍；
// 而渠道配置在单次请求的毫秒级窗口内几乎不会变化，缓存于内存是安全且划算的。
func (r *Relay) listCandidates(ctx context.Context, modelName string) ([]*model.Channel, error) {
	enabled := model.ChannelStatusEnabled

	channels, err := r.channels.List(ctx, model.ChannelQuery{
		Group:  r.group,
		Status: &enabled,
	})
	if err != nil {
		return nil, fmt.Errorf("relay: 查询候选渠道失败: %w", err)
	}

	eligible := make([]*model.Channel, 0, len(channels))
	for _, ch := range channels {
		// 过渡约定：模型列表为空 = 支持全部模型（便于只配一个渠道做连通性验证）。
		// 该行为将在引入严格模式后收紧。
		if len(ch.Models) == 0 || ch.HasModel(modelName) {
			eligible = append(eligible, ch)
		}
	}
	return eligible, nil
}

// SelectChannel 为指定模型选择可用渠道（不排除任何渠道）。
//
// 适用场景：管理后台的连通性测试、渠道体检等"只想知道有没有可用渠道"的调用。
// 转发路径使用 listCandidates + pickCandidate，以便跳过本次请求已失败的渠道。
func (r *Relay) SelectChannel(ctx context.Context, modelName string) (*model.Channel, error) {
	candidates, err := r.listCandidates(ctx, modelName)
	if err != nil {
		return nil, err
	}

	ch := pickCandidate(candidates, nil)
	if ch == nil {
		return nil, fmt.Errorf("%w（分组=%s, 模型=%s）", ErrNoAvailableChannel, r.group, modelName)
	}
	return ch, nil
}

// pickCandidate 从候选集中挑出一个渠道，跳过 excluded 中已失败的渠道。
//
// 策略：
//  1. 只考虑【最高优先级】的一层。优先级是运维表达"先用谁"的强意图
//     （如先用便宜的、再用贵的），不允许被权重跨越；
//  2. 层内按权重随机，避免流量全部压在同层第一个渠道上；
//  3. 该层被排除殆尽时，自然回落到下一优先级层（candidates 已按优先级降序）。
//
// 候选已耗尽时返回 nil（调用方据此判断"无更多可尝试渠道"）。
func pickCandidate(candidates []*model.Channel, excluded map[uint64]struct{}) *model.Channel {
	var topPriority int
	hasTop := false

	tier := make([]*model.Channel, 0, len(candidates))
	for _, ch := range candidates {
		if _, skip := excluded[ch.ID]; skip {
			continue
		}
		if !hasTop {
			topPriority = ch.Priority
			hasTop = true
		}
		// 列表已按优先级降序，遇到更低优先级即说明本层已收集完毕
		if ch.Priority != topPriority {
			break
		}
		tier = append(tier, ch)
	}

	if len(tier) == 0 {
		return nil
	}
	return weightedPick(tier)
}

// weightedPick 在同优先级的一组渠道中按权重随机挑选一个。
//
// 算法：累加权重后取一个随机数 r ∈ [0, total)，顺序累减权重，
// 首次使累计值小于 0 的渠道即中选。权重越大命中概率越高。
//
// 边界处理：若权重之和 <= 0（理论上被 Validate 拦住，兜底防止死循环），
// 退化为等概率随机。
func weightedPick(channels []*model.Channel) *model.Channel {
	if len(channels) == 1 {
		return channels[0]
	}

	total := 0
	for _, ch := range channels {
		total += ch.Weight
	}
	if total <= 0 {
		return channels[rand.IntN(len(channels))]
	}

	remaining := rand.IntN(total)
	for _, ch := range channels {
		remaining -= ch.Weight
		if remaining < 0 {
			return ch
		}
	}
	// 理论上不可达（前面必然返回），兜底返回最后一个
	return channels[len(channels)-1]
}
