// 本文件是 model.AlertChannelRepository 的 SQL 实现。
//
// 意图（Why）：
//
//	告警通道配置是"少量、由站长手工维护"的数据，因此实现刻意保持朴素：
//	全表读、按 ID 写。过度优化（如为 events 建索引、引入缓存层）在这个量级上
//	只会增加失效路径——缓存与库不一致时，站长会遇到"我明明改了却还在往那儿发"。
//
// 流转（Flow）：
//
//	后台保存 → model.Validate() → Create/Update → 落库
//	发送时 → ListEnabled() → notify.Dispatcher 过滤与投递
//
// 扩展（Extend）：
//
//	若通道数增长到需要分页：给 List 加 (offset, limit) 参数，
//	不要在调用方做全量过滤——那会让"列表只取前 N 条"退化成全表扫描。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LTZY-ACU/ltzy-api/internal/model"
)

// alertChannelColumns 是 alert_channels 的列清单（读写共用，避免两处手写导致错位）。
const alertChannelColumns = `id, name, kind, target, events, enabled, created_at, updated_at`

// alertChannelRepository 是 model.AlertChannelRepository 的 SQL 实现。
//
// 并发安全：只持有 *sql.DB（自带连接池），无内部状态。
type alertChannelRepository struct {
	db *sql.DB
}

// NewAlertChannelRepository 创建告警通道仓储。
func NewAlertChannelRepository(db *sql.DB) model.AlertChannelRepository {
	return &alertChannelRepository{db: db}
}

// Create 写入一条通道配置并回填 ID 与时间戳。
func (r *alertChannelRepository) Create(ctx context.Context, c *model.AlertChannel) error {
	if c == nil {
		return errors.New("store: 告警通道为空")
	}
	now := time.Now()
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO alert_channels (name, kind, target, events, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(c.Name), model.NormalizeAlertChannelKind(c.Kind),
		strings.TrimSpace(c.Target), strings.TrimSpace(c.Events), boolToInt(c.Enabled), now.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("store: 写入告警通道失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取告警通道 ID 失败: %w", err)
	}
	c.ID = uint64(id)
	c.CreatedAt, c.UpdatedAt = now, now
	return nil
}

// Update 按 ID 覆盖一条通道配置。
//
// 用部分列更新而非整行替换：updated_at 由服务端维护，
// 若交给调用方传入，一个"从旧表单提交"的旧时间戳会把更新记录搅乱。
func (r *alertChannelRepository) Update(ctx context.Context, c *model.AlertChannel) error {
	if c == nil || c.ID == 0 {
		return errors.New("store: 告警通道 ID 为空")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE alert_channels
		    SET name = ?, kind = ?, target = ?, events = ?, enabled = ?, updated_at = ?
		  WHERE id = ?`,
		strings.TrimSpace(c.Name), model.NormalizeAlertChannelKind(c.Kind),
		strings.TrimSpace(c.Target), strings.TrimSpace(c.Events), boolToInt(c.Enabled),
		time.Now().Unix(), c.ID)
	if err != nil {
		return fmt.Errorf("store: 更新告警通道失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		// 区分"不存在"与"存在但没变化"：前者要报 404 让用户知道配置可能已被别人删掉，
		// 后者应视为成功（某些数据库在值未变时也会返回 0 行）。
		if _, getErr := r.Get(ctx, c.ID); getErr != nil {
			return getErr
		}
	}
	return nil
}

// Delete 按 ID 删除一条通道配置。
func (r *alertChannelRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM alert_channels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除告警通道失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取删除影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrAlertChannelNotFound
	}
	return nil
}

// Get 按 ID 读取单条通道配置。
func (r *alertChannelRepository) Get(ctx context.Context, id uint64) (*model.AlertChannel, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+alertChannelColumns+` FROM alert_channels WHERE id = ?`, id)
	c, err := scanAlertChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.ErrAlertChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取告警通道失败: %w", err)
	}
	return c, nil
}

// ListEnabled 返回全部已启用的通道。
func (r *alertChannelRepository) ListEnabled(ctx context.Context) ([]*model.AlertChannel, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+alertChannelColumns+` FROM alert_channels WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: 读取告警通道失败: %w", err)
	}
	return scanAlertChannels(rows)
}

// List 返回全部通道（后台列表用），同时返回总数。
func (r *alertChannelRepository) List(ctx context.Context) ([]*model.AlertChannel, int, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+alertChannelColumns+` FROM alert_channels ORDER BY id DESC`)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 读取告警通道失败: %w", err)
	}
	items, err := scanAlertChannels(rows)
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_channels`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计告警通道数量失败: %w", err)
	}
	return items, total, nil
}

// scanAlertChannel 扫描一行告警通道记录。
func scanAlertChannel(row rowScanner) (*model.AlertChannel, error) {
	var (
		c                    model.AlertChannel
		kind, events         string
		enabled              int
		createdAt, updatedAt int64
	)
	if err := row.Scan(&c.ID, &c.Name, &kind, &c.Target, &events, &enabled,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	c.Kind = model.NormalizeAlertChannelKind(kind)
	c.Events = events
	c.Enabled = enabled != 0
	c.CreatedAt = time.Unix(createdAt, 0)
	c.UpdatedAt = time.Unix(updatedAt, 0)
	return &c, nil
}

// scanAlertChannels 扫描多行并负责关闭 rows。
//
// 关闭必须放在本函数：调用方拿到的是 []*model.AlertChannel，
// 没有任何机会自己 Close——一旦漏关，连接池会在几轮查询后耗尽。
func scanAlertChannels(rows *sql.Rows) ([]*model.AlertChannel, error) {
	defer func() { _ = rows.Close() }()
	items := make([]*model.AlertChannel, 0, 4)
	for rows.Next() {
		c, err := scanAlertChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 扫描告警通道失败: %w", err)
		}
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历告警通道失败: %w", err)
	}
	return items, nil
}
