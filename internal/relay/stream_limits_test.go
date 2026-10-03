// 流式上限持有者的单元测试。
//
// 意图（Why）：
//
//	stream_limits.go 把"散落的硬编码上限"改成运行期可调，但**默认行为必须与改造前
//	逐一相等**，否则零配置部署会悄悄改变网关对上游 SSE 的容忍度。本测试把
//	"默认值 = 旧常量"、"越界被夹取"、"设置后即时生效"三条锁定下来，
//	作为"可调"与"不回归"之间的护栏。
//
// 流转（Flow）：
//
//	测试直接调用 relay 包内符号（同包测试）：DefaultStreamLimits / normalized /
//	SetStreamLimits / StreamLimitsNow / ResetStreamLimits，不经过 HTTP 层。
//
// 扩展（Extend）：
//
//	新增一类流式上限时，在 TestDefaultStreamLimits_与旧常量一致 与
//	TestStreamLimits_normalized夹取越界 各补一条断言即可。
package relay

import (
	"testing"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// TestDefaultStreamLimits_与旧常量一致 锁定默认值与改造前写死的常量逐一相等。
//
// 同时断言与 model.DefaultLimitSettings() 一致：model 是唯一权威定义源，
// relay 侧只是引用，一旦有人只在某一处改了数字，这里会立刻失败。
func TestDefaultStreamLimits_与旧常量一致(t *testing.T) {
	got := DefaultStreamLimits()

	if got.AnthropicLineBytes != 1<<20 {
		t.Errorf("默认 Anthropic 单行上限 = %d，期望 %d", got.AnthropicLineBytes, 1<<20)
	}
	if got.GeminiLineBytes != 1<<20 {
		t.Errorf("默认 Gemini 单行上限 = %d，期望 %d", got.GeminiLineBytes, 1<<20)
	}
	if got.CodexLineBytes != 8<<20 {
		t.Errorf("默认 Codex 单行上限 = %d，期望 %d", got.CodexLineBytes, 8<<20)
	}
	if got.UsageTailBytes != 1<<20 {
		t.Errorf("默认 usage 尾部上限 = %d，期望 %d", got.UsageTailBytes, 1<<20)
	}

	defaults := model.DefaultLimitSettings()
	want := StreamLimits{
		AnthropicLineBytes: int(defaults.SSEAnthropicLineBytes),
		GeminiLineBytes:    int(defaults.SSEGeminiLineBytes),
		CodexLineBytes:     int(defaults.SSECodexLineBytes),
		UsageTailBytes:     int(defaults.SSEUsageTailBytes),
	}
	if got != want {
		t.Fatalf("默认值与 model 权威定义不一致：got=%+v want=%+v", got, want)
	}
}

// TestStreamLimits_normalized夹取越界 锁定夹取规则：非正值回退默认，越界夹到边界。
func TestStreamLimits_normalized夹取越界(t *testing.T) {
	def := DefaultStreamLimits()
	cases := []struct {
		name string
		in   StreamLimits
		want StreamLimits
	}{
		{
			name: "零值全部回退默认",
			in:   StreamLimits{},
			want: def,
		},
		{
			name: "负值全部回退默认",
			in: StreamLimits{
				AnthropicLineBytes: -1,
				GeminiLineBytes:    -100,
				CodexLineBytes:     -1,
				UsageTailBytes:     -1,
			},
			want: def,
		},
		{
			name: "低于下界夹到下界",
			in: StreamLimits{
				AnthropicLineBytes: 1,
				GeminiLineBytes:    1,
				CodexLineBytes:     1,
				UsageTailBytes:     1,
			},
			want: StreamLimits{
				AnthropicLineBytes: int(model.MinLimitSSELineBytes),
				GeminiLineBytes:    int(model.MinLimitSSELineBytes),
				CodexLineBytes:     int(model.MinLimitSSELineBytes),
				UsageTailBytes:     int(model.MinLimitSSEUsageTailBytes),
			},
		},
		{
			name: "高于上界夹到上界",
			in: StreamLimits{
				AnthropicLineBytes: 1 << 30,
				GeminiLineBytes:    1 << 30,
				CodexLineBytes:     1 << 30,
				UsageTailBytes:     1 << 30,
			},
			want: StreamLimits{
				AnthropicLineBytes: int(model.MaxLimitSSELineBytes),
				GeminiLineBytes:    int(model.MaxLimitSSELineBytes),
				CodexLineBytes:     int(model.MaxLimitSSELineBytes),
				UsageTailBytes:     int(model.MaxLimitSSEUsageTailBytes),
			},
		},
		{
			name: "合法值原样保留",
			in: StreamLimits{
				AnthropicLineBytes: 2 << 20,
				GeminiLineBytes:    3 << 20,
				CodexLineBytes:     16 << 20,
				UsageTailBytes:     2 << 20,
			},
			want: StreamLimits{
				AnthropicLineBytes: 2 << 20,
				GeminiLineBytes:    3 << 20,
				CodexLineBytes:     16 << 20,
				UsageTailBytes:     2 << 20,
			},
		},
	}

	for _, tc := range cases {
		if got := tc.in.normalized(); got != tc.want {
			t.Errorf("%s：normalized = %+v，期望 %+v", tc.name, got, tc.want)
		}
	}
}

// TestStreamLimitsNow_未注入时回退默认 锁定"尚未由 server 注入"的冷启动语义：
// StreamLimitsNow 必须返回默认值，保证单测与未装配场景与改造前行为一致。
func TestStreamLimitsNow_未注入时回退默认(t *testing.T) {
	ResetStreamLimits()
	t.Cleanup(ResetStreamLimits)

	if got := StreamLimitsNow(); got != DefaultStreamLimits() {
		t.Fatalf("未注入时应返回默认上限，实际 %+v", got)
	}
}

// TestSetStreamLimits_设置后即时生效 锁定后台保存后的即时生效路径：
// SetStreamLimits 存值后，StreamLimitsNow 立刻反映新值（无需重启）。
func TestSetStreamLimits_设置后即时生效(t *testing.T) {
	ResetStreamLimits()
	t.Cleanup(ResetStreamLimits)

	want := StreamLimits{
		AnthropicLineBytes: 2 << 20,
		GeminiLineBytes:    4 << 20,
		CodexLineBytes:     16 << 20,
		UsageTailBytes:     3 << 20,
	}
	SetStreamLimits(want)

	if got := StreamLimitsNow(); got != want {
		t.Fatalf("设置后应立即生效，实际 %+v，期望 %+v", got, want)
	}
}

// TestSetStreamLimits_越界入参被夹取 锁定写入侧的纵深防御：即便调用方传入越界值，
// 落库到进程级状态的也必须是夹取后的合法值（0 会让 bufio.Scanner 直接报错）。
func TestSetStreamLimits_越界入参被夹取(t *testing.T) {
	ResetStreamLimits()
	t.Cleanup(ResetStreamLimits)

	SetStreamLimits(StreamLimits{
		AnthropicLineBytes: 0,
		GeminiLineBytes:    -1,
		CodexLineBytes:     1 << 30,
		UsageTailBytes:     1,
	})

	got := StreamLimitsNow()
	if got.AnthropicLineBytes != int(model.DefaultLimitSSEAnthropicLineBytes) {
		t.Errorf("零值应回退默认，实际 %d", got.AnthropicLineBytes)
	}
	if got.GeminiLineBytes != int(model.DefaultLimitSSEGeminiLineBytes) {
		t.Errorf("负值应回退默认，实际 %d", got.GeminiLineBytes)
	}
	if got.CodexLineBytes != int(model.MaxLimitSSELineBytes) {
		t.Errorf("越上界应夹到 %d，实际 %d", model.MaxLimitSSELineBytes, got.CodexLineBytes)
	}
	if got.UsageTailBytes != int(model.MinLimitSSEUsageTailBytes) {
		t.Errorf("越下界应夹到 %d，实际 %d", model.MinLimitSSEUsageTailBytes, got.UsageTailBytes)
	}
}

// TestResetStreamLimits_恢复默认 锁定测试清理语义：Reset 后回到未注入状态。
func TestResetStreamLimits_恢复默认(t *testing.T) {
	t.Cleanup(ResetStreamLimits)

	SetStreamLimits(StreamLimits{AnthropicLineBytes: 2 << 20})
	ResetStreamLimits()

	if got := StreamLimitsNow(); got != DefaultStreamLimits() {
		t.Fatalf("Reset 后应回默认，实际 %+v", got)
	}
}
