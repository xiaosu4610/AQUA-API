// 公告仓储的单元测试。
//
// 意图（Why）：
//
//	公告的核心风险不在 CRUD，而在【可见性判定】与【排序】：
//	  · 时间窗口边界判断错（提前露出 / 该下线没下线）会让用户看到不该看的内容；
//	  · 排序错会让置顶公告被淹没，等于发布失败；
//	  · 管理端列表若漏掉停用/过期公告，"草稿找不到"会让管理员以为数据丢了。
//	因此本文件把时间窗口边界、排序与 limit 上限作为重点用例锁定。
//
// 流转（Flow）：
//
//	go test ./internal/store/ → 真实 SQLite（临时文件）上执行迁移并验证仓储行为
//
// 扩展（Extend）：
//
//	新增筛选维度或展示规则时，在下方补充对应用例；时间相关断言一律用固定时间点，
//	不要依赖 time.Now()（否则用例会在边界附近的运行时刻随机失败）。
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestAnnouncementFixture 构造基于临时数据库的公告仓储。
func newTestAnnouncementFixture(t *testing.T) model.AnnouncementRepository {
	t.Helper()
	st := newTestStore(t)
	return NewAnnouncementRepository(st.DB())
}

// newTestAnnouncement 构造一条已发布的普通公告（默认启用、不置顶、无时间限制）。
func newTestAnnouncement(title string) *model.Announcement {
	return &model.Announcement{
		Title:   title,
		Content: title + " 的正文",
		Level:   model.AnnouncementLevelInfo,
		Enabled: true,
	}
}

func TestAnnouncementRepository_增删改查主链路(t *testing.T) {
	repo := newTestAnnouncementFixture(t)
	ctx := context.Background()

	item := newTestAnnouncement("系统升级通知")
	item.Level = model.AnnouncementLevelWarning
	item.Pinned = true
	if err := repo.Create(ctx, item); err != nil {
		t.Fatalf("写入公告失败: %v", err)
	}
	if item.ID == 0 {
		t.Fatal("写入后应回填 ID")
	}
	if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		t.Fatal("写入后应回填创建/更新时间")
	}

	got, err := repo.GetByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("按 ID 查询失败: %v", err)
	}
	if got.Title != "系统升级通知" || got.Level != model.AnnouncementLevelWarning || !got.Pinned {
		t.Fatalf("读回的数据不一致: %+v", got)
	}

	// 全量更新
	got.Title = "系统升级通知（已更新）"
	got.Enabled = false
	got.Content = "新的正文"
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("更新公告失败: %v", err)
	}
	again, err := repo.GetByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("更新后查询失败: %v", err)
	}
	if again.Title != "系统升级通知（已更新）" || again.Enabled || again.Content != "新的正文" {
		t.Fatalf("更新未生效: %+v", again)
	}

	if err := repo.Delete(ctx, item.ID); err != nil {
		t.Fatalf("删除公告失败: %v", err)
	}
	if _, err := repo.GetByID(ctx, item.ID); !errors.Is(err, model.ErrAnnouncementNotFound) {
		t.Fatalf("删除后查询应返回 ErrAnnouncementNotFound，实际 %v", err)
	}
	// 重复删除 / 更新不存在的记录都应返回哨兵错误
	if err := repo.Delete(ctx, item.ID); !errors.Is(err, model.ErrAnnouncementNotFound) {
		t.Fatalf("重复删除应返回 ErrAnnouncementNotFound，实际 %v", err)
	}
	gone := newTestAnnouncement("不存在")
	gone.ID = item.ID
	if err := repo.Update(ctx, gone); !errors.Is(err, model.ErrAnnouncementNotFound) {
		t.Fatalf("更新不存在记录应返回 ErrAnnouncementNotFound，实际 %v", err)
	}
}

func TestAnnouncementRepository_校验失败被拒绝(t *testing.T) {
	repo := newTestAnnouncementFixture(t)
	ctx := context.Background()

	cases := []struct {
		name string
		item *model.Announcement
	}{
		{name: "标题为空", item: &model.Announcement{Title: "   ", Content: "ok", Level: model.AnnouncementLevelInfo}},
		{
			name: "标题超长",
			item: &model.Announcement{
				Title: strings.Repeat("标", 201), Content: "ok", Level: model.AnnouncementLevelInfo,
			},
		},
		{
			name: "正文超长",
			item: &model.Announcement{
				Title: "标题", Content: strings.Repeat("正", 20001), Level: model.AnnouncementLevelInfo,
			},
		},
		{
			name: "语气非法",
			item: &model.Announcement{Title: "标题", Content: "ok", Level: model.AnnouncementLevel("purple")},
		},
		{
			name: "过期早于发布",
			item: &model.Announcement{
				Title: "标题", Content: "ok", Level: model.AnnouncementLevelInfo,
				PublishAt: time.Unix(2000, 0), ExpireAt: time.Unix(1000, 0),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := repo.Create(ctx, tc.item); err == nil {
				t.Fatalf("非法公告应被拒绝，实际写入成功: %+v", tc.item)
			}
		})
	}
}

func TestAnnouncementRepository_管理端列表含停用与过期(t *testing.T) {
	repo := newTestAnnouncementFixture(t)
	ctx := context.Background()

	past := time.Now().Add(-48 * time.Hour)
	disabled := newTestAnnouncement("已停用的草稿")
	disabled.Enabled = false
	expired := newTestAnnouncement("已过期的公告")
	expired.ExpireAt = past
	live := newTestAnnouncement("生效中的公告")
	for _, item := range []*model.Announcement{disabled, expired, live} {
		if err := repo.Create(ctx, item); err != nil {
			t.Fatalf("写入公告失败: %v", err)
		}
	}

	items, total, err := repo.List(ctx, model.AnnouncementQuery{Limit: 50})
	if err != nil {
		t.Fatalf("查询列表失败: %v", err)
	}
	if total != 3 || len(items) != 3 {
		t.Fatalf("管理端应看到全部 3 条，实际 total=%d len=%d", total, len(items))
	}

	enabled := true
	items, total, err = repo.List(ctx, model.AnnouncementQuery{Enabled: &enabled, Limit: 50})
	if err != nil {
		t.Fatalf("按启用筛选失败: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("启用的公告应为 2 条，实际 total=%d len=%d", total, len(items))
	}
}

func TestAnnouncementRepository_ListActive时间窗口边界(t *testing.T) {
	repo := newTestAnnouncementFixture(t)
	ctx := context.Background()

	now := time.Unix(1_700_000_000, 0)

	visibleImmediate := newTestAnnouncement("立即发布且永不过期")
	visibleScheduled := newTestAnnouncement("已开始的定时公告")
	visibleScheduled.PublishAt = now.Add(-time.Hour)
	future := newTestAnnouncement("尚未到发布时间")
	future.PublishAt = now.Add(time.Hour)
	expired := newTestAnnouncement("已过期")
	expired.ExpireAt = now.Add(-time.Second)
	atExpiry := newTestAnnouncement("恰好到点")
	atExpiry.ExpireAt = now // expire_at > now 为假 → 应不可见
	atPublish := newTestAnnouncement("恰好到发布时间")
	atPublish.PublishAt = now // publish_at <= now 为真 → 应可见
	disabled := newTestAnnouncement("停用")
	disabled.Enabled = false

	all := []*model.Announcement{visibleImmediate, visibleScheduled, future, expired, atExpiry, atPublish, disabled}
	for _, item := range all {
		if err := repo.Create(ctx, item); err != nil {
			t.Fatalf("写入公告失败: %v", err)
		}
	}

	items, err := repo.ListActive(ctx, now, 50)
	if err != nil {
		t.Fatalf("查询生效公告失败: %v", err)
	}

	got := make(map[string]bool, len(items))
	for _, item := range items {
		got[item.Title] = true
	}
	wantVisible := []string{"立即发布且永不过期", "已开始的定时公告", "恰好到发布时间"}
	for _, title := range wantVisible {
		if !got[title] {
			t.Errorf("公告 %q 应可见，实际未返回", title)
		}
	}
	wantHidden := []string{"尚未到发布时间", "已过期", "恰好到点", "停用"}
	for _, title := range wantHidden {
		if got[title] {
			t.Errorf("公告 %q 不应可见，实际被返回", title)
		}
	}
}

func TestAnnouncementRepository_ListActive排序置顶优先且新发布在前(t *testing.T) {
	repo := newTestAnnouncementFixture(t)
	ctx := context.Background()

	now := time.Unix(1_700_000_000, 0)

	pinned := newTestAnnouncement("置顶的旧公告")
	pinned.Pinned = true
	pinned.PublishAt = now.Add(-10 * time.Hour)

	newest := newTestAnnouncement("最新的普通公告")
	newest.PublishAt = now.Add(-time.Hour)

	middle := newTestAnnouncement("中间的普通公告")
	middle.PublishAt = now.Add(-5 * time.Hour)

	noSchedule := newTestAnnouncement("无计划时间的公告") // PublishAt 为 0

	for _, item := range []*model.Announcement{newest, pinned, noSchedule, middle} {
		if err := repo.Create(ctx, item); err != nil {
			t.Fatalf("写入公告失败: %v", err)
		}
	}

	items, err := repo.ListActive(ctx, now, 50)
	if err != nil {
		t.Fatalf("查询生效公告失败: %v", err)
	}
	gotTitles := make([]string, 0, len(items))
	for _, item := range items {
		gotTitles = append(gotTitles, item.Title)
	}

	want := []string{"置顶的旧公告", "最新的普通公告", "中间的普通公告", "无计划时间的公告"}
	if len(gotTitles) != len(want) {
		t.Fatalf("返回条数 = %d，期望 %d（%v）", len(gotTitles), len(want), gotTitles)
	}
	for i := range want {
		if gotTitles[i] != want[i] {
			t.Fatalf("排序不正确：第 %d 位 = %q，期望 %q（完整顺序 %v）", i+1, gotTitles[i], want[i], gotTitles)
		}
	}
}

func TestAnnouncementRepository_ListActive上限为20条(t *testing.T) {
	repo := newTestAnnouncementFixture(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)

	for i := 0; i < 25; i++ {
		item := newTestAnnouncement("公告")
		if err := repo.Create(ctx, item); err != nil {
			t.Fatalf("写入公告失败: %v", err)
		}
	}

	items, err := repo.ListActive(ctx, now, 100) // 传入超过上限的值应被收敛为 20
	if err != nil {
		t.Fatalf("查询生效公告失败: %v", err)
	}
	if len(items) != 20 {
		t.Fatalf("公开端最多返回 20 条，实际 %d", len(items))
	}
}
