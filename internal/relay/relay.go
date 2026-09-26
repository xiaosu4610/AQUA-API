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
)

// Options 是 Relay 的可选参数。
type Options struct {
	// Group 指定默认路由分组。为空时使用 defaultGroup。
	Group string
	// ResponseHeaderTimeout 是等待上游响应头的超时（即"首字节时间"）。
	// 注意它与"整个请求超时"不同：流式响应可能持续数分钟，
	// 我们只限制"上游多久没开始响应"，而不是"响应多久必须结束"。
	ResponseHeaderTimeout time.Duration
}

// Relay 是转发引擎，持有渠道仓储与上游 HTTP 客户端。
//
// 并发安全：所有字段在构造后不再修改，可被多 goroutine 共享。
type Relay struct {
	channels model.ChannelRepository
	client   *http.Client
	group    string
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

	return &Relay{
		channels: channels,
		group:    group,
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

// SelectChannel 为指定模型选择可用渠道。
//
// M1 策略（最简可用版）：
//
//	取本分组下所有【启用】渠道，按仓储返回顺序（优先级降序 → 权重降序）
//	找到第一个声明支持该模型的渠道。
//
// 过渡约定：若渠道的模型列表为空，视为"支持全部模型"。
// 这样做的原因是 M1 阶段用户往往只配一个渠道做连通性验证，
// 强制填模型列表会带来不必要的摩擦。M2 引入严格模式与模型映射后会收紧该行为。
//
// TODO(relay): M2 引入负载均衡（权重随机 + 负载感知）与"排除已失败渠道"。
func (r *Relay) SelectChannel(ctx context.Context, modelName string) (*model.Channel, error) {
	enabled := model.ChannelStatusEnabled

	candidates, err := r.channels.List(ctx, model.ChannelQuery{
		Group:  r.group,
		Status: &enabled,
	})
	if err != nil {
		return nil, fmt.Errorf("relay: 查询候选渠道失败: %w", err)
	}

	for _, ch := range candidates {
		// 空模型列表 = 通配（见上方"过渡约定"）
		if len(ch.Models) == 0 || ch.HasModel(modelName) {
			return ch, nil
		}
	}

	return nil, fmt.Errorf("%w（分组=%s, 模型=%s）", ErrNoAvailableChannel, r.group, modelName)
}
