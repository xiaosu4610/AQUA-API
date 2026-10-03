// 运行上限设置的单元测试。
//
// 测试重点：
//   - 默认值必须与改动前写死在代码里的常量逐一相等（零配置行为不变）；
//   - KV 中的合法值能被正确覆盖；
//   - 越界的值一律拒绝保存（Validate 报错），运行期读取时回退默认；
//   - 设置表读失败时回退默认且不 panic（服务不因设置表问题而不可用）。
package model

import (
	"context"
	"errors"
	"testing"
)

// errorSettingRepo 是"读库必失败"的设置仓储，用于验证读库失败回退默认。
type errorSettingRepo struct{}

func (errorSettingRepo) Get(context.Context, string) (string, error) { return "", errReadSettings }
func (errorSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return nil, errReadSettings
}
func (errorSettingRepo) Set(context.Context, string, string) error { return errReadSettings }
func (errorSettingRepo) SetMany(context.Context, map[string]string) error {
	return errReadSettings
}

var errReadSettings = errors.New("模拟设置表读取失败")

func TestDefaultLimitSettings_与旧硬编码常量一致(t *testing.T) {
	got := DefaultLimitSettings()

	if got.BodyMaxBytes != 4<<20 {
		t.Errorf("默认请求体上限 = %d，期望 %d", got.BodyMaxBytes, 4<<20)
	}
	if got.SensitiveImportMaxWords != 2000 {
		t.Errorf("默认敏感词导入上限 = %d，期望 2000", got.SensitiveImportMaxWords)
	}
	if got.LeaderboardMaxDays != 365 {
		t.Errorf("默认排行榜天数上限 = %d，期望 365", got.LeaderboardMaxDays)
	}
	if got.ModelStatsMaxMinutes != 24*60 {
		t.Errorf("默认模型统计分钟上限 = %d，期望 %d", got.ModelStatsMaxMinutes, 24*60)
	}
	if got.TrialGrantMaxHours != 24*30 {
		t.Errorf("默认试用时长上限 = %d，期望 %d", got.TrialGrantMaxHours, 24*30)
	}
	if got.AnnouncementActiveMax != AnnouncementActiveMaxLimit {
		t.Errorf("默认公告条数上限 = %d，期望 %d", got.AnnouncementActiveMax, AnnouncementActiveMaxLimit)
	}
	if got.SSEAnthropicLineBytes != 1<<20 {
		t.Errorf("默认 Anthropic SSE 单行上限 = %d，期望 %d", got.SSEAnthropicLineBytes, 1<<20)
	}
	if got.SSEGeminiLineBytes != 1<<20 {
		t.Errorf("默认 Gemini SSE 单行上限 = %d，期望 %d", got.SSEGeminiLineBytes, 1<<20)
	}
	if got.SSECodexLineBytes != 8<<20 {
		t.Errorf("默认 Codex SSE 单行上限 = %d，期望 %d", got.SSECodexLineBytes, 8<<20)
	}
	if got.SSEUsageTailBytes != 1<<20 {
		t.Errorf("默认 SSE usage 尾部上限 = %d，期望 %d", got.SSEUsageTailBytes, 1<<20)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("默认值必须通过校验，实际: %v", err)
	}
}

func TestLoadLimitSettings_KV覆盖合法值(t *testing.T) {
	repo := &fakeSettingRepo{values: map[string]string{
		SettingKeyLimitBodyMaxBytes:            "8388608",
		SettingKeyLimitSensitiveImportMaxWords: "5000",
		SettingKeyLimitLeaderboardMaxDays:      "730",
		SettingKeyLimitModelStatsMaxMinutes:    "2880",
		SettingKeyLimitTrialGrantMaxHours:      "1440",
		SettingKeyLimitAnnouncementActiveMax:   "50",
		SettingKeyLimitSSEAnthropicLineBytes:   "2097152",  // 2 MiB
		SettingKeyLimitSSEGeminiLineBytes:      "4194304",  // 4 MiB
		SettingKeyLimitSSECodexLineBytes:       "16777216", // 16 MiB
		SettingKeyLimitSSEUsageTailBytes:       "2097152",  // 2 MiB
	}}

	loaded, err := LoadLimitSettings(context.Background(), repo)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}

	if loaded.BodyMaxBytes != 8388608 {
		t.Errorf("请求体上限未覆盖，实际 %d", loaded.BodyMaxBytes)
	}
	if loaded.SensitiveImportMaxWords != 5000 {
		t.Errorf("敏感词导入上限未覆盖，实际 %d", loaded.SensitiveImportMaxWords)
	}
	if loaded.LeaderboardMaxDays != 730 {
		t.Errorf("排行榜天数上限未覆盖，实际 %d", loaded.LeaderboardMaxDays)
	}
	if loaded.ModelStatsMaxMinutes != 2880 {
		t.Errorf("模型统计分钟上限未覆盖，实际 %d", loaded.ModelStatsMaxMinutes)
	}
	if loaded.TrialGrantMaxHours != 1440 {
		t.Errorf("试用时长上限未覆盖，实际 %d", loaded.TrialGrantMaxHours)
	}
	if loaded.AnnouncementActiveMax != 50 {
		t.Errorf("公告条数上限未覆盖，实际 %d", loaded.AnnouncementActiveMax)
	}
	if loaded.SSEAnthropicLineBytes != 2097152 {
		t.Errorf("Anthropic SSE 单行上限未覆盖，实际 %d", loaded.SSEAnthropicLineBytes)
	}
	if loaded.SSEGeminiLineBytes != 4194304 {
		t.Errorf("Gemini SSE 单行上限未覆盖，实际 %d", loaded.SSEGeminiLineBytes)
	}
	if loaded.SSECodexLineBytes != 16777216 {
		t.Errorf("Codex SSE 单行上限未覆盖，实际 %d", loaded.SSECodexLineBytes)
	}
	if loaded.SSEUsageTailBytes != 2097152 {
		t.Errorf("SSE usage 尾部上限未覆盖，实际 %d", loaded.SSEUsageTailBytes)
	}
}

func TestLoadLimitSettings_脏值越界回退默认(t *testing.T) {
	repo := &fakeSettingRepo{values: map[string]string{
		SettingKeyLimitBodyMaxBytes:            "不是数字",
		SettingKeyLimitSensitiveImportMaxWords: "-5",       // 低于下界
		SettingKeyLimitLeaderboardMaxDays:      "99999999", // 高于上界
		SettingKeyLimitModelStatsMaxMinutes:    "0",        // 低于下界
		SettingKeyLimitTrialGrantMaxHours:      "100000",   // 高于上界
		SettingKeyLimitAnnouncementActiveMax:   "100000",   // 高于上界
		SettingKeyLimitSSEAnthropicLineBytes:   "abc",      // 非数字
		SettingKeyLimitSSEGeminiLineBytes:      "1",        // 低于下界
		SettingKeyLimitSSECodexLineBytes:       "999999999", // 高于上界
		SettingKeyLimitSSEUsageTailBytes:       "-1",       // 低于下界
	}}

	loaded, err := LoadLimitSettings(context.Background(), repo)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}

	defaults := DefaultLimitSettings()
	if loaded != defaults {
		t.Fatalf("越界/脏值应全部回退默认，实际 %+v，期望 %+v", loaded, defaults)
	}
}

func TestLoadLimitSettings_读库失败回退默认(t *testing.T) {
	loaded, err := LoadLimitSettings(context.Background(), errorSettingRepo{})
	if err == nil {
		t.Fatal("读库失败应返回错误，便于调用方留痕")
	}
	if loaded != DefaultLimitSettings() {
		t.Fatalf("读库失败应回退默认，实际 %+v", loaded)
	}
}

func TestLoadLimitSettings_仓储为nil时返回默认(t *testing.T) {
	loaded, err := LoadLimitSettings(context.Background(), nil)
	if err != nil {
		t.Fatalf("仓储为 nil 不应报错: %v", err)
	}
	if loaded != DefaultLimitSettings() {
		t.Fatalf("仓储为 nil 应返回默认，实际 %+v", loaded)
	}
}

func TestLimitSettings_Validate_越界拒绝(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*LimitSettings)
	}{
		{"请求体上限低于下界", func(s *LimitSettings) { s.BodyMaxBytes = MinLimitBodyMaxBytes - 1 }},
		{"请求体上限高于上界", func(s *LimitSettings) { s.BodyMaxBytes = MaxLimitBodyMaxBytes + 1 }},
		{"敏感词导入上限越界", func(s *LimitSettings) { s.SensitiveImportMaxWords = MaxLimitSensitiveImportMaxWords + 1 }},
		{"排行榜天数越界", func(s *LimitSettings) { s.LeaderboardMaxDays = MaxLimitLeaderboardMaxDays + 1 }},
		{"模型统计分钟越界", func(s *LimitSettings) { s.ModelStatsMaxMinutes = 0 }},
		{"试用时长越界", func(s *LimitSettings) { s.TrialGrantMaxHours = MaxLimitTrialGrantMaxHours + 1 }},
		{"公告条数越界", func(s *LimitSettings) { s.AnnouncementActiveMax = MaxLimitAnnouncementActiveMax + 1 }},
		{"Anthropic SSE 单行低于下界", func(s *LimitSettings) { s.SSEAnthropicLineBytes = MinLimitSSELineBytes - 1 }},
		{"Gemini SSE 单行高于上界", func(s *LimitSettings) { s.SSEGeminiLineBytes = MaxLimitSSELineBytes + 1 }},
		{"Codex SSE 单行越界", func(s *LimitSettings) { s.SSECodexLineBytes = 0 }},
		{"SSE usage 尾部高于上界", func(s *LimitSettings) { s.SSEUsageTailBytes = MaxLimitSSEUsageTailBytes + 1 }},
	}
	for _, tc := range cases {
		settings := DefaultLimitSettings()
		tc.mutate(&settings)
		if err := settings.Validate(); err == nil {
			t.Errorf("%s：应被拒绝，实际通过", tc.name)
		}
	}
}

func TestLimitSettings_ToMap与Load往返一致(t *testing.T) {
	original := LimitSettings{
		BodyMaxBytes:            8 << 20,
		SensitiveImportMaxWords: 3000,
		LeaderboardMaxDays:      180,
		ModelStatsMaxMinutes:    720,
		TrialGrantMaxHours:      168,
		AnnouncementActiveMax:   40,

		SSEAnthropicLineBytes: 2 << 20,
		SSEGeminiLineBytes:    3 << 20,
		SSECodexLineBytes:     9 << 20,
		SSEUsageTailBytes:     2 << 20,
	}

	repo := &fakeSettingRepo{values: original.ToMap()}
	loaded, err := LoadLimitSettings(context.Background(), repo)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if loaded != original {
		t.Fatalf("ToMap/Load 未往返一致，实际 %+v，期望 %+v", loaded, original)
	}
}

// TestAnnouncementActiveLimit_默认回退与硬上限钳制 锁定"公告条数可调但有硬上限"的语义：
// 非正值回退默认；默认到硬上限之间原样生效；超过硬上限被钳制。
func TestAnnouncementActiveLimit_默认回退与硬上限钳制(t *testing.T) {
	if got := AnnouncementActiveLimit(0); got != AnnouncementActiveMaxLimit {
		t.Fatalf("limit<=0 应回退默认 %d，实际 %d", AnnouncementActiveMaxLimit, got)
	}
	if got := AnnouncementActiveLimit(50); got != 50 {
		t.Fatalf("50 应原样返回（默认 20 与硬上限之间），实际 %d", got)
	}
	if got := AnnouncementActiveLimit(AnnouncementActiveAbsoluteMaxLimit + 1); got != AnnouncementActiveAbsoluteMaxLimit {
		t.Fatalf("超硬上限应钳制到 %d，实际 %d", AnnouncementActiveAbsoluteMaxLimit, got)
	}
}
