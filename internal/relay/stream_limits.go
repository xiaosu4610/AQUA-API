// 本文件集中管理「流式读取路径的内存上限」，并支持运行期调整。
//
// 意图（Why）：
//
//	网关在解析上游 SSE 流时，若干处需要"单行/尾部最多保留多少字节"的上限。
//	这些值此前是散落在各文件里的硬编码常量，站长遇到超长事件（如带 base64 图片
//	或超长 reasoning 的响应）被截断时，只能改代码重新编译——站长明确要求
//	"上限要支持超级管理员在后台可调"。
//
//	为什么用包级持有者而不是给每个函数加参数：
//	  这些上限是【进程级内存上限】（对全部请求一致），不是每请求配置；
//	  而使用点位于若干自由函数（wrapXxxUpstreamStream / aggregateCodexStream）
//	  与 usage 嗅探器内部，没有 *Relay 实例可依赖。逐个加参数会牵动 relay 的
//	  调用链而收益为零，包级原子值更贴合语义且改动面最小。
//
//	默认值与合法区间的【唯一权威定义在 model.LimitSettings】（model 是 relay 的
//	下层，不能反向依赖 relay，故由 model 定义、relay 引用）；本文件只做
//	"取值 + 兜底夹取"，不再自带一份数字，杜绝两处漂移。
//
// 流转（Flow）：
//
//	启动与后台保存 → server 把 model.LimitSettings 映射为 relay.StreamLimits
//	  → relay.SetStreamLimits(...) → atomic 存值
//	读取点 → StreamLimitsNow()
//	  ├─ wrapAnthropicUpstreamStream / wrapGeminiUpstreamStream
//	  ├─ wrapCodexUpstreamStream / aggregateCodexStream（scanner.Buffer）
//	  └─ usageSniffer.scan（尾部兜底）
//
// 扩展（Extend）：
//
//	新增一类流式上限：在 model.LimitSettings 加字段与默认值/区间，
//	在本结构体加字段并在 normalized 补夹取规则，最后把 server 的映射补一行。
//	切勿在读取点直接读硬编码常量——那会让后台调整对该路径失效。
package relay

import (
	"sync/atomic"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// StreamLimits 是流式读取路径的内存上限（进程级生效）。
type StreamLimits struct {
	// AnthropicLineBytes 是解析 Anthropic SSE 单行的字节上限。
	AnthropicLineBytes int
	// GeminiLineBytes 是解析 Gemini SSE 单行的字节上限。
	GeminiLineBytes int
	// CodexLineBytes 是解析 OpenAI Responses（Codex）SSE 事件行的字节上限。
	CodexLineBytes int
	// UsageTailBytes 是等待一个 usage 对象接收完整时允许保留的最大尾部字节数。
	UsageTailBytes int
}

// DefaultStreamLimits 返回与「改为可调之前」完全一致的默认值。
//
// 直接取 model 的默认定义：那些常量同时是本次改造前的行为基线，
// 也是设置读取失败时的兜底值，必须与旧行为逐一相等。
func DefaultStreamLimits() StreamLimits {
	defaults := model.DefaultLimitSettings()
	return StreamLimits{
		AnthropicLineBytes: int(defaults.SSEAnthropicLineBytes),
		GeminiLineBytes:    int(defaults.SSEGeminiLineBytes),
		CodexLineBytes:     int(defaults.SSECodexLineBytes),
		UsageTailBytes:     int(defaults.SSEUsageTailBytes),
	}
}

// normalized 把非法或缺失的取值归一到合法区间。
//
// 规则：<=0 视为"未设置"，回退默认值；超出边界则夹取到边界。
// 读取路径上做夹取（而不是只在写入时校验）：历史设置值、手工改库、
// 配置迁移都可能带来越界值，而 0 会让 bufio.Scanner 立即报错。
//
// 区间的权威定义同样在 model（与后台表单展示的区间一致），
// 这里只做二次夹取，属于纵深防御。
func (l StreamLimits) normalized() StreamLimits {
	def := DefaultStreamLimits()
	lineMin, lineMax := int(model.MinLimitSSELineBytes), int(model.MaxLimitSSELineBytes)
	tailMin, tailMax := int(model.MinLimitSSEUsageTailBytes), int(model.MaxLimitSSEUsageTailBytes)
	return StreamLimits{
		AnthropicLineBytes: clampInt(l.AnthropicLineBytes, def.AnthropicLineBytes, lineMin, lineMax),
		GeminiLineBytes:    clampInt(l.GeminiLineBytes, def.GeminiLineBytes, lineMin, lineMax),
		CodexLineBytes:     clampInt(l.CodexLineBytes, def.CodexLineBytes, lineMin, lineMax),
		UsageTailBytes:     clampInt(l.UsageTailBytes, def.UsageTailBytes, tailMin, tailMax),
	}
}

// clampInt 把 value 归一到 [minValue, maxValue]：<=0 用 fallback，其余越界夹取。
func clampInt(value, fallback, minValue, maxValue int) int {
	if value <= 0 {
		value = fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

// currentStreamLimits 保存当前生效的流式上限。
//
// 初值为 nil，表示"尚未由 server 注入"；此时 StreamLimitsNow 返回默认值，
// 因此单元测试与任何未装配设置的场景都与改造前行为一致。
var currentStreamLimits atomic.Pointer[StreamLimits]

// StreamLimitsNow 返回当前生效的流式上限（并发安全，热路径可直接调用）。
func StreamLimitsNow() StreamLimits {
	if p := currentStreamLimits.Load(); p != nil {
		return *p
	}
	return DefaultStreamLimits()
}

// SetStreamLimits 覆盖流式上限（供 server 在启动与后台保存后调用）。
//
// 入参会先经 normalized 夹取，因此调用方无需自己保证区间合法性；
// 传零值等价于"恢复默认值"。
func SetStreamLimits(limits StreamLimits) {
	normalized := limits.normalized()
	currentStreamLimits.Store(&normalized)
}

// ResetStreamLimits 恢复默认上限（供测试清理，避免用例之间互相污染）。
func ResetStreamLimits() {
	currentStreamLimits.Store(nil)
}
