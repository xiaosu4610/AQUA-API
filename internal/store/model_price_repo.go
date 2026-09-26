// 本文件是 model.ModelPriceRepository 的 SQL 实现。
//
// 意图（Why）：
//
//	把计价规则落到数据库。定价数据量很小（几十条），但读取极其频繁
//	（每次转发都要算钱），因此：
//	  1) 本层只做纯粹的存取，不做缓存——缓存放在 relay 侧的 Billing 组件，
//	     因为"何时失效"取决于业务（后台改价后应立即生效）；
//	  2) 查询固定按"具体 → 笼统"排序返回，让上层可直接按序取用。
//
// 流转（Flow）：
//
//	NewModelPriceRepository(db)
//	  ├─ 后台维护：Create / Update / Delete / List
//	  └─ 转发计费：List(group, enabledOnly=true) → model.MatchModelPrice
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的 modelPriceColumns / scanModelPrice / insert / update 四处。
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

// modelPriceColumns 集中定义查询列，顺序必须与 scanModelPrice 的扫描顺序严格一致。
const modelPriceColumns = `id, model, prompt_price, completion_price, group_name, enabled, remark, created_at, updated_at`

// modelPriceRepository 是 model.ModelPriceRepository 的 SQL 实现，并发安全。
type modelPriceRepository struct {
	db *sql.DB
}

// NewModelPriceRepository 创建计价规则仓储。
func NewModelPriceRepository(db *sql.DB) model.ModelPriceRepository {
	return &modelPriceRepository{db: db}
}

// Create 新增计价规则。
func (r *modelPriceRepository) Create(ctx context.Context, price *model.ModelPrice) error {
	if err := price.Validate(); err != nil {
		return fmt.Errorf("store: 计价规则非法: %w", err)
	}

	now := time.Now()
	price.Model = strings.TrimSpace(price.Model)
	price.Group = strings.TrimSpace(price.Group)
	price.CreatedAt = now
	price.UpdatedAt = now

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO model_prices
			(model, prompt_price, completion_price, group_name, enabled, remark, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		price.Model, price.PromptPrice, price.CompletionPrice, price.Group,
		boolToInt(price.Enabled), price.Remark, price.CreatedAt.Unix(), price.UpdatedAt.Unix(),
	)
	if err != nil {
		// 唯一索引冲突即"同分组同模型已存在"。用错误文本判断而非预查，
		// 因为预查存在并发窗口（两个请求同时通过检查后都插入）。
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return model.ErrModelPriceDuplicated
		}
		return fmt.Errorf("store: 新增计价规则失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增计价规则的 ID 失败: %w", err)
	}
	price.ID = uint64(id)
	return nil
}

// GetByID 按主键查询计价规则。
func (r *modelPriceRepository) GetByID(ctx context.Context, id uint64) (*model.ModelPrice, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+modelPriceColumns+" FROM model_prices WHERE id = ?", id)

	price, err := scanModelPrice(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrModelPriceNotFound
		}
		return nil, err
	}
	return price, nil
}

// List 查询计价规则。
//
// 返回顺序为"具体 → 笼统"（由 SQL 计算出的排序键保证）：
// 精确匹配的规则排在前，通配规则排在后，便于人工核对"哪条会生效"。
func (r *modelPriceRepository) List(ctx context.Context, group string, enabledOnly bool) ([]*model.ModelPrice, error) {
	var (
		conditions []string
		args       []any
	)
	if group = strings.TrimSpace(group); group != "" {
		conditions = append(conditions, "group_name = ?")
		args = append(args, group)
	}
	if enabledOnly {
		conditions = append(conditions, "enabled = 1")
	}

	query := "SELECT " + modelPriceColumns + " FROM model_prices"
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	// 排序键：通配（含 *）排后，且按模式长度降序，从而实现"精确 → 长前缀 → 短前缀 → *"
	query += " ORDER BY (model LIKE '%*%') ASC, LENGTH(model) DESC, id ASC"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询计价规则失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	prices := make([]*model.ModelPrice, 0, 32)
	for rows.Next() {
		price, err := scanModelPrice(rows)
		if err != nil {
			return nil, err
		}
		prices = append(prices, price)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历计价规则失败: %w", err)
	}
	return prices, nil
}

// Update 按 ID 更新计价规则。
func (r *modelPriceRepository) Update(ctx context.Context, price *model.ModelPrice) error {
	if price.ID == 0 {
		return errors.New("store: 更新计价规则时 ID 不能为 0")
	}
	if err := price.Validate(); err != nil {
		return fmt.Errorf("store: 计价规则非法: %w", err)
	}

	price.Model = strings.TrimSpace(price.Model)
	price.Group = strings.TrimSpace(price.Group)
	price.UpdatedAt = time.Now()

	// 刻意不更新 created_at：创建时间应保持不可变，便于审计
	res, err := r.db.ExecContext(ctx, `
		UPDATE model_prices SET
			model = ?, prompt_price = ?, completion_price = ?, group_name = ?,
			enabled = ?, remark = ?, updated_at = ?
		WHERE id = ?`,
		price.Model, price.PromptPrice, price.CompletionPrice, price.Group,
		boolToInt(price.Enabled), price.Remark, price.UpdatedAt.Unix(), price.ID,
	)
	if err != nil {
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return model.ErrModelPriceDuplicated
		}
		return fmt.Errorf("store: 更新计价规则 %d 失败: %w", price.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrModelPriceNotFound
	}
	return nil
}

// Delete 按 ID 删除计价规则。
func (r *modelPriceRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM model_prices WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除计价规则 %d 失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取删除影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrModelPriceNotFound
	}
	return nil
}

// scanModelPrice 把一行数据映射为计价规则对象。
func scanModelPrice(sc rowScanner) (*model.ModelPrice, error) {
	var (
		id              uint64
		modelName       string
		promptPrice     int64
		completionPrice int64
		group           string
		enabled         int
		remark          string
		createdAt       int64
		updatedAt       int64
	)

	if err := sc.Scan(&id, &modelName, &promptPrice, &completionPrice, &group,
		&enabled, &remark, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取计价规则字段失败: %w", err)
	}

	return &model.ModelPrice{
		ID:              id,
		Model:           modelName,
		PromptPrice:     promptPrice,
		CompletionPrice: completionPrice,
		Group:           group,
		Enabled:         enabled != 0,
		Remark:          remark,
		CreatedAt:       time.Unix(createdAt, 0),
		UpdatedAt:       time.Unix(updatedAt, 0),
	}, nil
}
