// 本文件定义「运行上限」领域模型。
//
// 意图（Why）：
//
//	网关里有一批"保护性上限"（请求体大小、批量导入条数、统计窗口、试用时长…），
//	它们此前直接写死在代码里——数值本身是合理的，但站长一旦有更大规模的正当需求
//	（例如批量导入上万词条、把排行榜窗口拉长到两年），就只能改代码重新发版。
//	本站的诉求是"上限要支持超级管理员在后台可调，可塑性、可修改性要强"，
//	因此把这批上限抽成可在后台修改的设置项；同时保留每项的合法区间，
//	防止把保护性上限改成"等于没有"（那等于把闸门重新打开）。
//
//	与 SiteSettings 的关系：这里只承载"数值型的安全/运行上限"，键统一前缀 limit_，
//	读写与校验自成一档，便于后台用一张独立表单集中呈现。
//
// 流转（Flow）：
//
//	后台读取：SettingRepository.GetAll → 与 DefaultLimitSettings 合并 → 返回给前端
//	后台保存：前端提交（部分字段）→ Validate 校验 → SetMany → 主动失效 server 侧缓存
//	运行期读取：server.limitSettingsCached(ctx) → 命中缓存直接返回，
//	           未命中则 LoadLimitSettings 一次并缓存（避免每请求查库）
//
// 扩展（Extend）：
//
//	新增一项可调上限时：
//	  1) 在 LimitSettings 加字段（注释写清单位与含义）；
//	  2) 在 DefaultLimitSettings 补默认值（必须等于改动前写死的常量）；
//	  3) 在 ToMap/LoadLimitSettings/Validate 三处同步补读写与区间校验；
//	  4) 在 server 层的读取点用 limitSettingsCached() 取该字段替换硬编码常量。
package model

import (
	"context"
	"fmt"
	"strconv"
)

// 运行上限的设置键（统一前缀 limit_，集中定义避免各处拼字符串拼错）。
const (
	// SettingKeyLimitBodyMaxBytes 是普通 JSON 接口的请求体上限（字节）。
	SettingKeyLimitBodyMaxBytes = "limit_body_max_bytes"
	// SettingKeyLimitSensitiveImportMaxWords 是敏感词单次批量导入的条数上限。
	SettingKeyLimitSensitiveImportMaxWords = "limit_sensitive_import_max_words"
	// SettingKeyLimitLeaderboardMaxDays 是排行榜允许的最大统计窗口（天）。
	SettingKeyLimitLeaderboardMaxDays = "limit_leaderboard_max_days"
	// SettingKeyLimitModelStatsMaxMinutes 是模型实时指标允许的最大窗口（分钟）。
	SettingKeyLimitModelStatsMaxMinutes = "limit_model_stats_max_minutes"
	// SettingKeyLimitTrialGrantMaxHours 是限时试用额的时长上限（小时）。
	SettingKeyLimitTrialGrantMaxHours = "limit_trial_grant_max_hours"
	// SettingKeyLimitAnnouncementActiveMax 是公开端一次返回的公告条数上限。
	SettingKeyLimitAnnouncementActiveMax = "limit_announcement_active_max"
	// SettingKeyLimitSSEAnthropicLineBytes 是解析 Anthropic SSE 单行的字节上限。
	SettingKeyLimitSSEAnthropicLineBytes = "limit_sse_anthropic_line_bytes"
	// SettingKeyLimitSSEGeminiLineBytes 是解析 Gemini SSE 单行的字节上限。
	SettingKeyLimitSSEGeminiLineBytes = "limit_sse_gemini_line_bytes"
	// SettingKeyLimitSSECodexLineBytes 是解析 Codex（OpenAI Responses）SSE 事件行的字节上限。
	SettingKeyLimitSSECodexLineBytes = "limit_sse_codex_line_bytes"
	// SettingKeyLimitSSEUsageTailBytes 是流式解析中"等待 usage 对象完整"的尾部字节上限。
	SettingKeyLimitSSEUsageTailBytes = "limit_sse_usage_tail_bytes"
)

// 各项运行上限的合法区间（下界 / 上界）。
//
// 为什么每项都要上界：这些值是"保护性上限"。若允许改成天文数字（或干脆不限），
// 保护本身就不复存在——超大请求体打爆内存、一次导入卡死写锁、超长统计窗口
// 拖垮数据库。上界给出"可塑性"的天花板，仍足以覆盖真实运营需求。
const (
	// 普通 JSON 请求体上限：64 KiB ~ 64 MiB。
	MinLimitBodyMaxBytes int64 = 64 << 10
	MaxLimitBodyMaxBytes int64 = 64 << 20

	// 敏感词单次导入上限：1 ~ 20000 条。
	MinLimitSensitiveImportMaxWords = 1
	MaxLimitSensitiveImportMaxWords = 20000

	// 排行榜统计窗口上限：1 ~ 3650 天（约 10 年）。
	MinLimitLeaderboardMaxDays = 1
	MaxLimitLeaderboardMaxDays = 3650

	// 模型实时指标窗口上限：1 ~ 43200 分钟（30 天）。
	MinLimitModelStatsMaxMinutes = 1
	MaxLimitModelStatsMaxMinutes = 43200

	// 限时试用时长上限：1 ~ 2160 小时（90 天）。
	MinLimitTrialGrantMaxHours = 1
	MaxLimitTrialGrantMaxHours = 2160

	// 公开端公告条数上限：1 ~ AnnouncementActiveAbsoluteMaxLimit（100）。
	// 与仓储层的硬上限同源，保证"后台设成多大，仓储就真的允许多大"。
	MinLimitAnnouncementActiveMax = 1
	MaxLimitAnnouncementActiveMax = AnnouncementActiveAbsoluteMaxLimit

	// SSE 单行上限：64 KiB ~ 256 MiB。
	//
	// 下界 64 KiB：再小会让正常的大段事件（长 reasoning、带图片的 delta）频繁截断；
	// 上界 256 MiB：这些值直接决定"单个流式请求最多占多少内存"，
	// 放开成任意值等于关掉内存保护（并发长响应足以打爆进程）。
	MinLimitSSELineBytes int64 = 64 << 10
	MaxLimitSSELineBytes int64 = 256 << 20

	// SSE usage 尾部兜底：64 KiB ~ 64 MiB。
	//
	// usage 对象本身只有几百字节，下界 64 KiB 已是极宽松的余量；
	// 上界 64 MiB 防"尾部随响应无限增长"这条内存兜底被误关掉。
	MinLimitSSEUsageTailBytes int64 = 64 << 10
	MaxLimitSSEUsageTailBytes int64 = 64 << 20
)

// SSE 流式上限的默认值（与改为可调之前写死在 relay 的常量逐一相等）。
//
// 单独定义而不复用 relay 的常量：model 是被 relay 依赖的下层，
// 不能反向引用 relay（会形成循环依赖）。因此这里作为唯一权威定义，
// relay 侧通过 model.DefaultLimitSettings() 取值，并由 relay 的
// stream_limits_test 断言二者一致，杜绝两处漂移。
const (
	// DefaultLimitSSEAnthropicLineBytes 对应 relay.maxAnthropicStreamLineBytes。
	DefaultLimitSSEAnthropicLineBytes int64 = 1 << 20
	// DefaultLimitSSEGeminiLineBytes 对应 relay.maxGeminiStreamLineBytes。
	DefaultLimitSSEGeminiLineBytes int64 = 1 << 20
	// DefaultLimitSSECodexLineBytes 对应 relay.maxCodexStreamLineBytes。
	DefaultLimitSSECodexLineBytes int64 = 8 << 20
	// DefaultLimitSSEUsageTailBytes 对应 relay.usageTailMaxBytes。
	DefaultLimitSSEUsageTailBytes int64 = 1 << 20
)

// LimitSettings 是运行上限设置的强类型视图。
//
// 为什么单独成结构体（而不是塞进 SiteSettings）：这些字段是"数值型安全上限"，
// 读写、校验与后台渲染都与运营类设置不同，独立成档便于扩展与审阅。
type LimitSettings struct {
	// BodyMaxBytes 是普通 JSON 接口的请求体上限（字节）。
	//
	// 默认 4 MiB：覆盖敏感词整表导入、公告批量等最大输入，同时把
	// "每请求内存占用 × 并发数"封顶在可控范围内。
	BodyMaxBytes int64
	// SensitiveImportMaxWords 是敏感词单次批量导入的条数上限（条）。
	//
	// 默认 2000：防止误粘贴一个超大字典长时间占用写锁。
	SensitiveImportMaxWords int
	// LeaderboardMaxDays 是排行榜允许的最大统计窗口（天）。
	//
	// 默认 365：窗口越长，用量聚合扫描的行数越多。
	LeaderboardMaxDays int
	// ModelStatsMaxMinutes 是模型实时指标允许的最大窗口（分钟）。
	//
	// 默认 1440（24 小时）：用于模型详情页的实时速率展示。
	ModelStatsMaxMinutes int
	// TrialGrantMaxHours 是限时试用额的时长上限（小时）。
	//
	// 默认 720（30 天）：试用额是"限时"额度，时长过大等于事实上的永久额度。
	TrialGrantMaxHours int
	// AnnouncementActiveMax 是公开端一次返回的公告条数上限（条）。
	//
	// 默认 20：前台横幅是"一眼扫过"的展示位，过多会淹没页面。
	AnnouncementActiveMax int
	// SSEAnthropicLineBytes 是解析 Anthropic SSE 单行的字节上限（字节）。
	//
	// 默认 1 MiB。超长事件（长 reasoning、带 base64 图片的 delta）在此被截断时，
	// 站长可调大；但调大意味着单个流式请求的内存占用上限随之提高。
	SSEAnthropicLineBytes int64
	// SSEGeminiLineBytes 是解析 Gemini SSE 单行的字节上限（字节）。默认 1 MiB。
	SSEGeminiLineBytes int64
	// SSECodexLineBytes 是解析 Codex（OpenAI Responses）SSE 事件行的字节上限（字节）。
	//
	// 默认 8 MiB：Responses 的事件里会整段携带已生成内容，因此基线本就高于其它协议。
	SSECodexLineBytes int64
	// SSEUsageTailBytes 是流式解析中"等待 usage 对象接收完整"时允许保留的尾部字节上限（字节）。
	//
	// 默认 1 MiB。它是内存兜底：一个 usage 对象不可能达到该量级，
	// 一旦超过就说明上游发的不是合法 JSON（或对象永不闭合），解析器会跳过该标记。
	SSEUsageTailBytes int64
}

// DefaultLimitSettings 返回全部运行上限的默认值。
//
// 这些默认值与改动前写死在代码里的常量【逐一相等】，保证"零配置时行为不变"：
//   - server.defaultBodyLimitBytes（4 MiB）
//   - server.maxSensitiveImportWords（2000）
//   - server.leaderboardMaxDays（365）
//   - server.modelStatsMaxMinutes（1440）
//   - server.trialGrantMaxHours（720）
//   - model.AnnouncementActiveMaxLimit（20）
func DefaultLimitSettings() LimitSettings {
	return LimitSettings{
		BodyMaxBytes:            4 << 20, // 4 MiB
		SensitiveImportMaxWords: 2000,
		LeaderboardMaxDays:      365,
		ModelStatsMaxMinutes:    24 * 60, // 1440
		TrialGrantMaxHours:      24 * 30, // 720
		AnnouncementActiveMax:   20,

		// SSE 流式上限：与 relay 侧原常量逐一相等（见下方各 Default 常量的说明）。
		SSEAnthropicLineBytes: DefaultLimitSSEAnthropicLineBytes,
		SSEGeminiLineBytes:    DefaultLimitSSEGeminiLineBytes,
		SSECodexLineBytes:     DefaultLimitSSECodexLineBytes,
		SSEUsageTailBytes:     DefaultLimitSSEUsageTailBytes,
	}
}

// ToMap 把强类型视图转为 KV，供持久化使用。
func (s LimitSettings) ToMap() map[string]string {
	return map[string]string{
		SettingKeyLimitBodyMaxBytes:            strconv.FormatInt(s.BodyMaxBytes, 10),
		SettingKeyLimitSensitiveImportMaxWords: strconv.Itoa(s.SensitiveImportMaxWords),
		SettingKeyLimitLeaderboardMaxDays:      strconv.Itoa(s.LeaderboardMaxDays),
		SettingKeyLimitModelStatsMaxMinutes:    strconv.Itoa(s.ModelStatsMaxMinutes),
		SettingKeyLimitTrialGrantMaxHours:      strconv.Itoa(s.TrialGrantMaxHours),
		SettingKeyLimitAnnouncementActiveMax:   strconv.Itoa(s.AnnouncementActiveMax),
		SettingKeyLimitSSEAnthropicLineBytes:   strconv.FormatInt(s.SSEAnthropicLineBytes, 10),
		SettingKeyLimitSSEGeminiLineBytes:      strconv.FormatInt(s.SSEGeminiLineBytes, 10),
		SettingKeyLimitSSECodexLineBytes:       strconv.FormatInt(s.SSECodexLineBytes, 10),
		SettingKeyLimitSSEUsageTailBytes:       strconv.FormatInt(s.SSEUsageTailBytes, 10),
	}
}

// LoadLimitSettings 从 KV 仓储读取运行上限，并与默认值合并。
//
// 合并语义（关键安全属性）：仅在"存在且解析成功且落在合法区间"时覆盖，
// 任一不满足都保留默认值。因此：
//   - 设置表读失败 → 返回默认值 + 错误（调用方据此降级，但站点仍可用）；
//   - 某个键被手工写坏（非数字 / 越界）→ 该项回退默认，其余项照常生效。
//
// 这样即使设置表本身出问题，也不会让服务因一个坏值而不可用或失去保护。
func LoadLimitSettings(ctx context.Context, repo SettingRepository) (LimitSettings, error) {
	settings := DefaultLimitSettings()
	if repo == nil {
		return settings, nil
	}

	values, err := repo.GetAll(ctx)
	if err != nil {
		return settings, fmt.Errorf("model: 读取运行上限设置失败: %w", err)
	}

	if v, ok := values[SettingKeyLimitBodyMaxBytes]; ok {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil &&
			parsed >= MinLimitBodyMaxBytes && parsed <= MaxLimitBodyMaxBytes {
			settings.BodyMaxBytes = parsed
		}
	}
	if v, ok := parseLimitInt(values, SettingKeyLimitSensitiveImportMaxWords,
		MinLimitSensitiveImportMaxWords, MaxLimitSensitiveImportMaxWords); ok {
		settings.SensitiveImportMaxWords = v
	}
	if v, ok := parseLimitInt(values, SettingKeyLimitLeaderboardMaxDays,
		MinLimitLeaderboardMaxDays, MaxLimitLeaderboardMaxDays); ok {
		settings.LeaderboardMaxDays = v
	}
	if v, ok := parseLimitInt(values, SettingKeyLimitModelStatsMaxMinutes,
		MinLimitModelStatsMaxMinutes, MaxLimitModelStatsMaxMinutes); ok {
		settings.ModelStatsMaxMinutes = v
	}
	if v, ok := parseLimitInt(values, SettingKeyLimitTrialGrantMaxHours,
		MinLimitTrialGrantMaxHours, MaxLimitTrialGrantMaxHours); ok {
		settings.TrialGrantMaxHours = v
	}
	if v, ok := parseLimitInt(values, SettingKeyLimitAnnouncementActiveMax,
		MinLimitAnnouncementActiveMax, MaxLimitAnnouncementActiveMax); ok {
		settings.AnnouncementActiveMax = v
	}
	if v, ok := parseLimitInt64(values, SettingKeyLimitSSEAnthropicLineBytes,
		MinLimitSSELineBytes, MaxLimitSSELineBytes); ok {
		settings.SSEAnthropicLineBytes = v
	}
	if v, ok := parseLimitInt64(values, SettingKeyLimitSSEGeminiLineBytes,
		MinLimitSSELineBytes, MaxLimitSSELineBytes); ok {
		settings.SSEGeminiLineBytes = v
	}
	if v, ok := parseLimitInt64(values, SettingKeyLimitSSECodexLineBytes,
		MinLimitSSELineBytes, MaxLimitSSELineBytes); ok {
		settings.SSECodexLineBytes = v
	}
	if v, ok := parseLimitInt64(values, SettingKeyLimitSSEUsageTailBytes,
		MinLimitSSEUsageTailBytes, MaxLimitSSEUsageTailBytes); ok {
		settings.SSEUsageTailBytes = v
	}

	return settings, nil
}

// parseLimitInt 解析一个设定为整数的运行上限：键缺失、非数字或越界时返回 (0, false)。
//
// 抽成小工具是为了让 LoadLimitSettings 逐项读写的"同一个口径"只有一处实现，
// 避免某一项漏了区间判断（那正是"设置表被写坏后失去保护"的常见来源）。
func parseLimitInt(values map[string]string, key string, min, max int) (int, bool) {
	raw, ok := values[key]
	if !ok {
		return 0, false
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < min || parsed > max {
		return 0, false
	}
	return parsed, true
}

// parseLimitInt64 与 parseLimitInt 同口径，供 int64 型的上限使用。
//
// SSE 相关的上限取值可达数百 MiB，放在 int 上虽也够用，但与
// LimitSettings 的 int64 字段同类型可省去来回转换，也让"越界即丢弃"的
// 判断与其它项完全一致。
func parseLimitInt64(values map[string]string, key string, min, max int64) (int64, bool) {
	raw, ok := values[key]
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < min || parsed > max {
		return 0, false
	}
	return parsed, true
}

// Validate 逐项校验运行上限是否落在合法区间。
//
// 供后台保存时把关：越界返回带合法区间的中文错误（说明"能填什么范围"），
// 由 handler 转成 400 反馈给管理员。运行期则由 LoadLimitSettings 的
// "越界即回退默认"兜底，两道防线保证拿到的值永远可用。
func (s LimitSettings) Validate() error {
	if s.BodyMaxBytes < MinLimitBodyMaxBytes || s.BodyMaxBytes > MaxLimitBodyMaxBytes {
		return fmt.Errorf("普通请求体上限必须在 %d ~ %d 字节之间，当前 %d",
			MinLimitBodyMaxBytes, MaxLimitBodyMaxBytes, s.BodyMaxBytes)
	}
	if s.SensitiveImportMaxWords < MinLimitSensitiveImportMaxWords || s.SensitiveImportMaxWords > MaxLimitSensitiveImportMaxWords {
		return fmt.Errorf("敏感词单次导入上限必须在 %d ~ %d 条之间，当前 %d",
			MinLimitSensitiveImportMaxWords, MaxLimitSensitiveImportMaxWords, s.SensitiveImportMaxWords)
	}
	if s.LeaderboardMaxDays < MinLimitLeaderboardMaxDays || s.LeaderboardMaxDays > MaxLimitLeaderboardMaxDays {
		return fmt.Errorf("排行榜统计窗口上限必须在 %d ~ %d 天之间，当前 %d",
			MinLimitLeaderboardMaxDays, MaxLimitLeaderboardMaxDays, s.LeaderboardMaxDays)
	}
	if s.ModelStatsMaxMinutes < MinLimitModelStatsMaxMinutes || s.ModelStatsMaxMinutes > MaxLimitModelStatsMaxMinutes {
		return fmt.Errorf("模型统计窗口上限必须在 %d ~ %d 分钟之间，当前 %d",
			MinLimitModelStatsMaxMinutes, MaxLimitModelStatsMaxMinutes, s.ModelStatsMaxMinutes)
	}
	if s.TrialGrantMaxHours < MinLimitTrialGrantMaxHours || s.TrialGrantMaxHours > MaxLimitTrialGrantMaxHours {
		return fmt.Errorf("试用时长上限必须在 %d ~ %d 小时之间，当前 %d",
			MinLimitTrialGrantMaxHours, MaxLimitTrialGrantMaxHours, s.TrialGrantMaxHours)
	}
	if s.AnnouncementActiveMax < MinLimitAnnouncementActiveMax || s.AnnouncementActiveMax > MaxLimitAnnouncementActiveMax {
		return fmt.Errorf("公开公告条数上限必须在 %d ~ %d 条之间，当前 %d",
			MinLimitAnnouncementActiveMax, MaxLimitAnnouncementActiveMax, s.AnnouncementActiveMax)
	}
	if s.SSEAnthropicLineBytes < MinLimitSSELineBytes || s.SSEAnthropicLineBytes > MaxLimitSSELineBytes {
		return fmt.Errorf("Anthropic SSE 单行上限必须在 %d ~ %d 字节之间，当前 %d",
			MinLimitSSELineBytes, MaxLimitSSELineBytes, s.SSEAnthropicLineBytes)
	}
	if s.SSEGeminiLineBytes < MinLimitSSELineBytes || s.SSEGeminiLineBytes > MaxLimitSSELineBytes {
		return fmt.Errorf("Gemini SSE 单行上限必须在 %d ~ %d 字节之间，当前 %d",
			MinLimitSSELineBytes, MaxLimitSSELineBytes, s.SSEGeminiLineBytes)
	}
	if s.SSECodexLineBytes < MinLimitSSELineBytes || s.SSECodexLineBytes > MaxLimitSSELineBytes {
		return fmt.Errorf("Codex SSE 单行上限必须在 %d ~ %d 字节之间，当前 %d",
			MinLimitSSELineBytes, MaxLimitSSELineBytes, s.SSECodexLineBytes)
	}
	if s.SSEUsageTailBytes < MinLimitSSEUsageTailBytes || s.SSEUsageTailBytes > MaxLimitSSEUsageTailBytes {
		return fmt.Errorf("SSE usage 尾部上限必须在 %d ~ %d 字节之间，当前 %d",
			MinLimitSSEUsageTailBytes, MaxLimitSSEUsageTailBytes, s.SSEUsageTailBytes)
	}
	return nil
}
