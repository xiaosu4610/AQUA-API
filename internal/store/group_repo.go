// 本文件是 model.ModelGroupRepository 的 SQL 实现（模型分组）。
//
// 意图（Why）：
//
//	分组的读取有两个明显不同的场景，本文件按场景做了取舍：
//	  1) 计费链路（每次请求都要查倍率）——由 relay.Billing 做内存缓存，
//	     本层只提供廉价的单条查询；
//	  2) 后台列表与模型广场——需要全量分组，条数极少（通常个位数），
//	     直接整表查询即可，无需分页优化。
//
// 流转（Flow）：
//
//	NewModelGroupRepository(db)
//	  ├─ 后台维护：server → Create / Update / Delete / List
//	  ├─ 计费倍率：Billing → GetByName（带缓存）→ ApplyRatio
//	  └─ 模型广场：server → List(enabledOnly) 组装分组视图
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的 groupColumns / scanModelGroup /
//	Create 列清单 / Update 语句四处。
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

// groupColumns 集中定义查询列，顺序必须与 scanModelGroup 的扫描顺序严格一致。
const groupColumns = `id, name, display_name, ratio, description, enabled, created_at, updated_at`

// defaultGroupPageSize / maxGroupPageSize 是分组列表的分页参数。
//
// 上限取 200：分组是运营概念，实际部署极少超过十几个；
// 上限只为防止异常请求拉出超大响应。
const (
	defaultGroupPageSize = 50
	maxGroupPageSize     = 200
)

// modelGroupRepository 是 model.ModelGroupRepository 的 SQL 实现，并发安全。
type modelGroupRepository struct {
	db *sql.DB
}

// NewModelGroupRepository 创建分组仓储。
func NewModelGroupRepository(db *sql.DB) model.ModelGroupRepository {
	return &modelGroupRepository{db: db}
}

// Create 新增分组。
func (r *modelGroupRepository) Create(ctx context.Context, group *model.ModelGroup) error {
	if err := group.Validate(); err != nil {
		return fmt.Errorf("store: 分组非法: %w", err)
	}

	now := time.Now()
	group.Name = strings.ToLower(strings.TrimSpace(group.Name))
	group.CreatedAt = now
	group.UpdatedAt = now

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO model_groups (name, display_name, ratio, description, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		group.Name, group.DisplayName, group.Ratio, group.Description,
		boolToInt(group.Enabled), group.CreatedAt.Unix(), group.UpdatedAt.Unix(),
	)
	if err != nil {
		// 唯一索引冲突即"同名分组已存在"。用错误文本判断而非预查，
		// 因为预查存在并发窗口（两个请求同时通过检查后都插入）。
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return model.ErrModelGroupDuplicated
		}
		return fmt.Errorf("store: 新增分组失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增分组的 ID 失败: %w", err)
	}
	group.ID = uint64(id)
	return nil
}

// GetByName 按标识查询分组。
func (r *modelGroupRepository) GetByName(ctx context.Context, name string) (*model.ModelGroup, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+groupColumns+" FROM model_groups WHERE name = ?",
		strings.ToLower(strings.TrimSpace(name)))

	group, err := scanModelGroup(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrModelGroupNotFound
		}
		return nil, err
	}
	return group, nil
}

// List 查询分组列表（按名称升序）。
func (r *modelGroupRepository) List(ctx context.Context, query model.ModelGroupQuery) ([]*model.ModelGroup, error) {
	where, args := buildGroupWhere(query)

	limit := normalizeLimit(query.Limit, defaultGroupPageSize, maxGroupPageSize)
	offset := normalizeOffset(query.Offset)

	sqlText := "SELECT " + groupColumns + " FROM model_groups" + where +
		" ORDER BY name ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询分组列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	groups := make([]*model.ModelGroup, 0, 16)
	for rows.Next() {
		group, err := scanModelGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历分组列表失败: %w", err)
	}
	return groups, nil
}

// Count 统计分组数量。
func (r *modelGroupRepository) Count(ctx context.Context, query model.ModelGroupQuery) (int64, error) {
	where, args := buildGroupWhere(query)

	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM model_groups"+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计分组数失败: %w", err)
	}
	return total, nil
}

// Update 按 ID 更新分组。
//
// 刻意不更新 name：它是被渠道与价格表引用的标识，
// 允许改名会让历史配置静默失联（渠道突然不属于任何分组）。
func (r *modelGroupRepository) Update(ctx context.Context, group *model.ModelGroup) error {
	if group.ID == 0 {
		return errors.New("store: 更新分组时 ID 不能为 0")
	}
	if err := group.Validate(); err != nil {
		return fmt.Errorf("store: 分组非法: %w", err)
	}

	group.UpdatedAt = time.Now()
	res, err := r.db.ExecContext(ctx, `
		UPDATE model_groups SET
			display_name = ?, ratio = ?, description = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		group.DisplayName, group.Ratio, group.Description,
		boolToInt(group.Enabled), group.UpdatedAt.Unix(), group.ID,
	)
	if err != nil {
		return fmt.Errorf("store: 更新分组 %d 失败: %w", group.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrModelGroupNotFound
	}
	return nil
}

// Delete 按 ID 删除分组。
func (r *modelGroupRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM model_groups WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除分组 %d 失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取删除影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrModelGroupNotFound
	}
	return nil
}

// buildGroupWhere 依据查询条件拼装 WHERE 子句与参数。
func buildGroupWhere(query model.ModelGroupQuery) (string, []any) {
	if !query.EnabledOnly {
		return "", nil
	}
	return " WHERE enabled = 1", nil
}

// scanModelGroup 把一行数据映射为分组对象。
func scanModelGroup(sc rowScanner) (*model.ModelGroup, error) {
	var (
		id          uint64
		name        string
		displayName string
		ratio       int64
		description string
		enabled     int
		createdAt   int64
		updatedAt   int64
	)

	if err := sc.Scan(&id, &name, &displayName, &ratio, &description, &enabled,
		&createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取分组字段失败: %w", err)
	}

	return &model.ModelGroup{
		ID:          id,
		Name:        name,
		DisplayName: displayName,
		Ratio:       ratio,
		Description: description,
		Enabled:     enabled != 0,
		CreatedAt:   time.Unix(createdAt, 0),
		UpdatedAt:   time.Unix(updatedAt, 0),
	}, nil
}
