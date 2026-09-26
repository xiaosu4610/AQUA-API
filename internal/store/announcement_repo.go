// 本文件是 model.AnnouncementRepository 的 SQL 实现（站点公告）。
//
// 意图（Why）：
//
//	公告的核心是一段"有可见时间窗口的展示数据"，实现要守住两点：
//	  1) 时间窗口判定放在 SQL 里，而不是把全部数据读出来再在 Go 层筛——
//	     前台查询是高频只读路径，让数据库用索引过滤并排序才是正确姿势；
//	  2) 条件一律用占位符拼接，杜绝 SQL 注入（公告正文来自后台表单，
//	     是最不该被注入的一类字符串）。
//
//	公开端 ListActive 把 now 作为参数传入，而非在实现里调用 time.Now()：
//	定时发布/过期是核心逻辑，必须能用固定时间点在测试中精确验证窗口边界。
//
// 流转（Flow）：
//
//	NewAnnouncementRepository(db)
//	  ├─ 后台：server → Create / Update / Delete / GetByID / List
//	  └─ 前台：server → ListActive(now, limit)
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的 announcementColumns / scanAnnouncement /
//	Create / Update 四处列清单。
//	新增筛选条件：在 model.AnnouncementQuery 加字段，并在 buildAnnouncementWhere 实现。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// announcementColumns 集中定义查询列，顺序必须与 scanAnnouncement 的扫描顺序严格一致。
const announcementColumns = `id, title, content, level, pinned, enabled, publish_at, expire_at, created_at, updated_at`

// 公告列表（管理端）的分页参数：默认 20，上限 100（与后台其他列表保持一致）。
const (
	defaultAnnouncementPageSize = 20
	maxAnnouncementPageSize     = 100
)

// announcementRepository 是 model.AnnouncementRepository 的 SQL 实现，并发安全。
type announcementRepository struct {
	db *sql.DB
}

// NewAnnouncementRepository 创建公告仓储。
func NewAnnouncementRepository(db *sql.DB) model.AnnouncementRepository {
	return &announcementRepository{db: db}
}

// Create 写入一条公告并回填 ID 与时间戳。
func (r *announcementRepository) Create(ctx context.Context, item *model.Announcement) error {
	if item == nil {
		return errors.New("store: 公告为空")
	}
	normalizeAnnouncement(item)
	if err := item.Validate(); err != nil {
		return fmt.Errorf("store: 公告非法: %w", err)
	}

	now := time.Now()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	item.UpdatedAt = item.CreatedAt

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO announcements
			(title, content, level, pinned, enabled, publish_at, expire_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.Title, item.Content, string(item.Level), boolToInt(item.Pinned), boolToInt(item.Enabled),
		unixOrZeroLocal(item.PublishAt), unixOrZeroLocal(item.ExpireAt),
		item.CreatedAt.Unix(), item.UpdatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: 写入公告失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增公告 ID 失败: %w", err)
	}
	item.ID = uint64(id)
	return nil
}

// Update 按 ID 覆盖更新公告；不存在时返回 model.ErrAnnouncementNotFound。
//
// 采用"全量覆盖"而非"逐字段 PATCH"：公告表单本就是一次提交完整内容，
// 全量写简单可靠，也避免部分更新时不清楚"哪些字段该保留"。
func (r *announcementRepository) Update(ctx context.Context, item *model.Announcement) error {
	if item == nil {
		return errors.New("store: 公告为空")
	}
	normalizeAnnouncement(item)
	if err := item.Validate(); err != nil {
		return fmt.Errorf("store: 公告非法: %w", err)
	}

	res, err := r.db.ExecContext(ctx, `
		UPDATE announcements
		SET title = ?, content = ?, level = ?, pinned = ?, enabled = ?,
		    publish_at = ?, expire_at = ?, updated_at = ?
		WHERE id = ?`,
		item.Title, item.Content, string(item.Level), boolToInt(item.Pinned), boolToInt(item.Enabled),
		unixOrZeroLocal(item.PublishAt), unixOrZeroLocal(item.ExpireAt),
		time.Now().Unix(), item.ID,
	)
	if err != nil {
		return fmt.Errorf("store: 更新公告 %d 失败: %w", item.ID, err)
	}
	return announcementAffectedOrNotFound(res, item.ID)
}

// Delete 按 ID 删除；不存在时返回 model.ErrAnnouncementNotFound。
func (r *announcementRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM announcements WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除公告 %d 失败: %w", id, err)
	}
	return announcementAffectedOrNotFound(res, id)
}

// GetByID 按 ID 查询；不存在时返回 model.ErrAnnouncementNotFound。
func (r *announcementRepository) GetByID(ctx context.Context, id uint64) (*model.Announcement, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+announcementColumns+" FROM announcements WHERE id = ?", id)

	item, err := scanAnnouncement(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrAnnouncementNotFound
		}
		return nil, err
	}
	return item, nil
}

// List 是管理端查询：包含未启用与已过期的全部公告，返回当页数据与总数。
func (r *announcementRepository) List(ctx context.Context, query model.AnnouncementQuery) ([]*model.Announcement, int, error) {
	where, args := buildAnnouncementWhere(query)

	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM announcements"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计公告数量失败: %w", err)
	}

	limit := normalizeLimit(query.Limit, defaultAnnouncementPageSize, maxAnnouncementPageSize)
	offset := normalizeOffset(query.Offset)

	// 后台默认排序与公开端一致（置顶优先、时间倒序），让管理员看到的前后顺序
	// 与用户实际看到的接近；再用 id 倒序兜底，保证分页稳定（同值不错行）。
	sqlText := "SELECT " + announcementColumns + " FROM announcements" + where +
		" ORDER BY pinned DESC, publish_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询公告列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]*model.Announcement, 0, limit)
	for rows.Next() {
		item, err := scanAnnouncement(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: 遍历公告列表失败: %w", err)
	}
	return items, total, nil
}

// ListActive 是公开端查询：只返回 enabled=1 且 now 落在 [publish_at, expire_at] 内的公告。
//
// 时间窗口的 SQL 语义（与迁移注释一致）：
//   - publish_at = 0 视为"立即发布"（不再有下界）；
//   - expire_at  = 0 视为"永不过期"（不再有上界）；
//   - 到期判定用 expire_at > now（开区间）：恰好到期的瞬间即不可见，符合"到点下线"的直觉。
//
// 排序 pinned DESC, publish_at DESC：置顶公告永远排在前面；同组内新的居上。
// 注意 publish_at = 0 的行在此排序下会靠后——它代表"没有计划时间"的历史公告，
// 与前台"最新通知优先"的诉求并不冲突（真正重要的会置顶）。
func (r *announcementRepository) ListActive(ctx context.Context, now time.Time, limit int) ([]*model.Announcement, error) {
	nowUnix := now.Unix()
	limit = model.AnnouncementActiveLimit(limit)

	rows, err := r.db.QueryContext(ctx, `
		SELECT `+announcementColumns+`
		FROM announcements
		WHERE enabled = 1
		  AND (publish_at = 0 OR publish_at <= ?)
		  AND (expire_at = 0 OR expire_at > ?)
		ORDER BY pinned DESC, publish_at DESC, id DESC
		LIMIT ?`,
		nowUnix, nowUnix, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 查询生效公告失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]*model.Announcement, 0, limit)
	for rows.Next() {
		item, err := scanAnnouncement(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历生效公告失败: %w", err)
	}
	return items, nil
}

// buildAnnouncementWhere 依据查询条件拼装 WHERE 子句与参数（全部参数化，杜绝 SQL 注入）。
func buildAnnouncementWhere(query model.AnnouncementQuery) (string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)

	if query.Enabled != nil {
		conditions = append(conditions, "enabled = ?")
		args = append(args, boolToInt(*query.Enabled))
	}
	if query.Level != "" {
		conditions = append(conditions, "level = ?")
		args = append(args, string(query.Level))
	}
	if keyword := strings.TrimSpace(query.Keyword); keyword != "" {
		// 模糊匹配标题或正文；转义 % 与 _ 避免用户输入被当作通配符
		pattern := "%" + escapeLike(keyword) + "%"
		conditions = append(conditions, "(title LIKE ? ESCAPE '\\' OR content LIKE ? ESCAPE '\\')")
		args = append(args, pattern, pattern)
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// scanAnnouncement 把一行数据映射为公告对象。
func scanAnnouncement(sc rowScanner) (*model.Announcement, error) {
	var (
		id        uint64
		title     string
		content   string
		level     string
		pinned    int
		enabled   int
		publishAt int64
		expireAt  int64
		createdAt int64
		updatedAt int64
	)

	if err := sc.Scan(&id, &title, &content, &level, &pinned, &enabled, &publishAt, &expireAt,
		&createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取公告字段失败: %w", err)
	}

	return &model.Announcement{
		ID:      id,
		Title:   title,
		Content: content,
		Level:   model.AnnouncementLevel(level),
		Pinned:  pinned != 0,
		Enabled: enabled != 0,
		// 0 表示不限制：转成 Go 零值时间，便于领域层用 IsZero 判断
		PublishAt: unixToTimeOrZero(publishAt),
		ExpireAt:  unixToTimeOrZero(expireAt),
		CreatedAt: time.Unix(createdAt, 0),
		UpdatedAt: time.Unix(updatedAt, 0),
	}, nil
}

// normalizeAnnouncement 归一化公告的写入字段：去空白、level 回退默认值。
//
// 放在仓储入口统一处理（Create/Update 都会经过），保证"域名层看到的模型"
// 与"落库的内容"一致，而不是库里有大小写混杂的 level 需要各处防御。
func normalizeAnnouncement(item *model.Announcement) {
	item.Title = strings.TrimSpace(item.Title)
	item.Level = model.NormalizeAnnouncementLevel(string(item.Level))
}

// announcementAffectedOrNotFound 依据受影响行数判断操作是否命中记录。
func announcementAffectedOrNotFound(res sql.Result, id uint64) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrAnnouncementNotFound
	}
	return nil
}
