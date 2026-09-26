// 本文件定义「站点公告」领域模型与仓储接口。
//
// 意图（Why）：
//
//	站点需要一个"管理员发布、全体用户可见"的通知渠道：维护窗口、活动上线、
//	计费调整、临时故障说明等。此前只能靠外部群聊口头传达，用户进入站点后
//	看不到任何官方说明，容易把已知问题当成故障去投诉。
//
//	把公告提升为独立领域对象，是为了让两条关键规则有明确归属：
//	  1) 展示时间窗口——publish_at / expire_at 决定"何时可见"，
//	     0 表示不限制（立即发布 / 永不过期）。定时发布与自动下线本质是
//	     时间窗口问题，由查询按 now 过滤，无需后台任务去改状态；
//	  2) 展示语义——level 决定前台的语气与配色，取值受白名单约束，
//	     避免出现前端无法识别的未知颜色。
//
// 流转（Flow）：
//
//	后台：AnnouncementsView → Repository.Create / Update / Delete / List / GetByID
//	前台：AnnouncementBanner → Repository.ListActive(now) → 按窗口过滤并排序
//
// 扩展（Extend）：
//
//	新增字段（如"仅对某分组可见""关联跳转链接"）时：
//	在本结构体加字段 + 建新迁移加列 + 同步 store/announcement_repo.go 的
//	列清单与扫描逻辑三处；若新增 level 取值，同步下方白名单常量与前端配色映射。
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAnnouncementNotFound 表示公告不存在。
//
// 后台编辑/删除一个已被并发删除的公告时返回它，供上层映射为 404，
// 而不是含糊的 500（管理员的实际处境是"这条已被别人删了"，需要被明确告知）。
var ErrAnnouncementNotFound = errors.New("model: 公告不存在")

// AnnouncementLevel 表示公告的展示语气。
//
// 用字符串而非整数枚举：它是"展示语义"而非业务状态，字符串自解释、
// 便于排查；新增一种语气也不需要改动数据库取值映射。
type AnnouncementLevel string

const (
	// AnnouncementLevelInfo 普通信息（默认）：中性提示。
	AnnouncementLevelInfo AnnouncementLevel = "info"
	// AnnouncementLevelSuccess 成功/喜报：如活动上线、功能开放。
	AnnouncementLevelSuccess AnnouncementLevel = "success"
	// AnnouncementLevelWarning 警告：如计费调整、维护预告，需引起注意。
	AnnouncementLevelWarning AnnouncementLevel = "warning"
	// AnnouncementLevelDanger 危险/故障：如服务异常、数据风险，最醒目的配色。
	AnnouncementLevelDanger AnnouncementLevel = "danger"
)

// announcementLevels 是 level 的白名单。
//
// 集中定义而非在 Validate 里散落判断：前端配色映射与后端校验共用同一份
// 语义来源，新增取值时只需改这一处（并同步前端）。
var announcementLevels = map[AnnouncementLevel]struct{}{
	AnnouncementLevelInfo:    {},
	AnnouncementLevelSuccess: {},
	AnnouncementLevelWarning: {},
	AnnouncementLevelDanger:  {},
}

// IsValid 判断 level 是否在白名单内。
func (l AnnouncementLevel) IsValid() bool {
	_, ok := announcementLevels[l]
	return ok
}

// String 返回 level 的中文描述（用于日志与后台展示）。
func (l AnnouncementLevel) String() string {
	switch l {
	case AnnouncementLevelInfo:
		return "普通"
	case AnnouncementLevelSuccess:
		return "喜报"
	case AnnouncementLevelWarning:
		return "警告"
	case AnnouncementLevelDanger:
		return "故障"
	default:
		return fmt.Sprintf("未知(%s)", string(l))
	}
}

// NormalizeAnnouncementLevel 归一化 level：去空白并转小写；空串回退为默认值 info。
//
// 为什么要归一化而不是直接报错：后台表单的"语气"下拉在未选择时会传空串，
// 这属于"使用默认值"而非"填错"，直接报错会让用户困惑。
func NormalizeAnnouncementLevel(raw string) AnnouncementLevel {
	level := AnnouncementLevel(strings.ToLower(strings.TrimSpace(raw)))
	if level == "" {
		return AnnouncementLevelInfo
	}
	return level
}

// 公告字段长度上限。
//
// 上限的意义是防御"误贴一整篇文章"这类输入：标题会出现在横幅上，
// 太长会撑破布局；正文在前台是纯文本展示，20000 字符足以覆盖正常通知。
const (
	// announcementTitleMaxLen 是标题最大字符数。
	announcementTitleMaxLen = 200
	// announcementContentMaxLen 是正文最大字符数。
	announcementContentMaxLen = 20000
)

// Announcement 表示一条站点公告。
type Announcement struct {
	ID      uint64            // 主键
	Title   string            // 标题
	Content string            // 正文（纯文本）
	Level   AnnouncementLevel // 展示语气（info/success/warning/danger）
	Pinned  bool              // 是否置顶（前台排在普通公告之前）
	Enabled bool              // 是否发布（停用即草稿，对前台不可见）
	// PublishAt 为开始展示时间；零值表示立即发布（落库为 0）。
	PublishAt time.Time
	// ExpireAt 为停止展示时间；零值表示永不过期（落库为 0）。
	ExpireAt  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate 校验公告的必要字段与长度约束。
//
// 校验在 model 层集中完成（而非散落在 handler 与 store）：
// 一次校验、两处复用（HTTP 层用于返回 400，仓储层用于兜底防止绕过接口的写入）。
func (a *Announcement) Validate() error {
	if a == nil {
		return errors.New("公告为空")
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		return errors.New("公告标题不能为空")
	}
	if len([]rune(title)) > announcementTitleMaxLen {
		return fmt.Errorf("公告标题最多 %d 个字符，当前 %d", announcementTitleMaxLen, len([]rune(title)))
	}
	if len([]rune(a.Content)) > announcementContentMaxLen {
		return fmt.Errorf("公告正文最多 %d 个字符，当前 %d", announcementContentMaxLen, len([]rune(a.Content)))
	}
	if !a.Level.IsValid() {
		return fmt.Errorf("公告语气非法: %q（可选 info/success/warning/danger）", string(a.Level))
	}
	// 时间窗口自洽性：既定时又设置了"过期早于发布"，这条公告永远不可能可见，
	// 属于明显的填写错误，直接拦下比让它静默地"发了等于没发"更好。
	if !a.PublishAt.IsZero() && !a.ExpireAt.IsZero() && !a.ExpireAt.After(a.PublishAt) {
		return errors.New("公告过期时间必须晚于发布时间")
	}
	return nil
}

// AnnouncementQuery 是公告列表（管理端）的查询条件。
type AnnouncementQuery struct {
	// Enabled 非空时按发布状态过滤（后台"只看已发布/只看草稿"）。
	Enabled *bool
	// Level 非空时按语气过滤。
	Level AnnouncementLevel
	// Keyword 按标题或正文模糊匹配。
	Keyword string
	Limit   int
	Offset  int
}

// AnnouncementRepository 定义公告的持久化操作。
type AnnouncementRepository interface {
	// Create 写入一条公告并回填 ID 与时间戳。
	Create(ctx context.Context, item *Announcement) error

	// Update 按 ID 覆盖更新公告；不存在时返回 ErrAnnouncementNotFound。
	Update(ctx context.Context, item *Announcement) error

	// Delete 按 ID 删除；不存在时返回 ErrAnnouncementNotFound。
	Delete(ctx context.Context, id uint64) error

	// GetByID 按 ID 查询；不存在时返回 ErrAnnouncementNotFound。
	GetByID(ctx context.Context, id uint64) (*Announcement, error)

	// List 是管理端查询：包含未启用与已过期的全部公告，返回当页数据与总数。
	List(ctx context.Context, query AnnouncementQuery) ([]*Announcement, int, error)

	// ListActive 是公开端查询：只返回 enabled=1 且 now 落在
	// [publish_at, expire_at] 内的公告（0 视为不限制），
	// 按 pinned 降序、publish_at 降序排列，最多返回 limit 条（上限 20）。
	//
	// 时间通过参数传入而非在实现里调用 time.Now()：定时发布/过期是核心逻辑，
	// 必须能在测试中用固定时间点精确验证窗口边界，而不是依赖真实时钟。
	ListActive(ctx context.Context, now time.Time, limit int) ([]*Announcement, error)
}

// announcementActiveMaxLimit 是公开端一次最多返回的公告条数。
//
// 取 20：前台横幅是"一眼扫过"的展示位，过多会淹没页面；
// 上限也防止外部用超大 limit 拉走全部数据。
const AnnouncementActiveMaxLimit = 20

// AnnouncementActiveLimit 把调用方传入的 limit 归一化到 [1, 上限] 区间。
//
// 导出而非内联进仓储：handler 也需要用它来构造响应，两边共用同一上限，
// 避免"仓储截断到 20、handler 却以为自己要了 100"这类不一致。
func AnnouncementActiveLimit(limit int) int {
	if limit <= 0 || limit > AnnouncementActiveMaxLimit {
		return AnnouncementActiveMaxLimit
	}
	return limit
}
