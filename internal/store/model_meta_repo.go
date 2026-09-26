// 本文件是 model.ModelRepository 与 model.ChannelModelMappingRepository 的
// SQL 实现（模型实体 + 渠道级模型映射）。
//
// 意图（Why）：
//
//	模型清单与映射都是"配置类"数据：条数很少、读取不频繁、写入集中在后台。
//	因此本文件只做纯粹的存取，不引入缓存——缓存（尤其是转发热路径要用的映射）
//	交由上层在"后台改完立即失效"，因为"何时失效"取决于业务而非存储。
//
//	读写约定：
//	  1) 能力标签以 JSON 数组字符串落库（模型不单独建能力表，见迁移 0016 说明）；
//	  2) 映射按渠道【整组替换】，保证"界面所见 = 落库结果"；
//	  3) 模型名与映射名两侧都做 Trim，落库的值始终干净。
//
// 流转（Flow）：
//
//	NewModelMetaRepository(db) / NewChannelModelMappingRepository(db)
//	  ├─ 后台维护：Create / Update / Delete / List / ReplaceForChannel
//	  └─ 转发解析：ListByChannel → model.ResolveMapping / ResolvePublicModel
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的列常量 / scan 函数 / insert 列清单 /
//	update 语句四处；批量导入走 UpsertBatch，勿逐条 Create（重名会失败）。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// modelMetaColumns 集中定义查询列，顺序必须与 scanModelMeta 的扫描顺序严格一致。
const modelMetaColumns = `id, name, display_name, vendor, description, context_length, enabled, capabilities, created_at, updated_at`

// channelModelMappingColumns 集中定义查询列，顺序必须与 scanChannelModelMapping 一致。
const channelModelMappingColumns = `id, channel_id, upstream_model, public_model, priority, enabled, remark, created_at, updated_at`

// 模型列表的分页参数：与分组一致，条量小但设上限以防异常请求拉出超大响应。
const (
	defaultModelPageSize = 50
	maxModelPageSize     = 500
)

// modelMetaRepository 是 model.ModelRepository 的 SQL 实现，并发安全。
type modelMetaRepository struct {
	db *sql.DB
}

// NewModelMetaRepository 创建模型实体仓储。
func NewModelMetaRepository(db *sql.DB) model.ModelRepository {
	return &modelMetaRepository{db: db}
}

// Create 新增模型。
func (r *modelMetaRepository) Create(ctx context.Context, m *model.Model) error {
	if err := m.Validate(); err != nil {
		return fmt.Errorf("store: 模型非法: %w", err)
	}

	now := time.Now()
	m.Name = strings.TrimSpace(m.Name)
	m.Vendor = strings.TrimSpace(m.Vendor)
	m.CreatedAt = now
	m.UpdatedAt = now

	caps, err := encodeCapabilities(m.Capabilities)
	if err != nil {
		return err
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO models
			(name, display_name, vendor, description, context_length, enabled, capabilities, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Name, m.DisplayName, m.Vendor, m.Description, m.ContextLength,
		boolToInt(m.Enabled), caps, m.CreatedAt.Unix(), m.UpdatedAt.Unix(),
	)
	if err != nil {
		// 唯一索引冲突即"同名模型已存在"。用错误文本判断而非预查，
		// 因为预查存在并发窗口（两个请求同时通过检查后都插入）。
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return model.ErrModelMetaDuplicated
		}
		return fmt.Errorf("store: 新增模型失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增模型的 ID 失败: %w", err)
	}
	m.ID = uint64(id)
	return nil
}

// GetByID 按主键查询模型。
func (r *modelMetaRepository) GetByID(ctx context.Context, id uint64) (*model.Model, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+modelMetaColumns+" FROM models WHERE id = ?", id)
	return scanModelMetaOrNotFound(row)
}

// GetByName 按对外模型名查询模型。
func (r *modelMetaRepository) GetByName(ctx context.Context, name string) (*model.Model, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+modelMetaColumns+" FROM models WHERE name = ?",
		strings.TrimSpace(name))
	return scanModelMetaOrNotFound(row)
}

// List 查询模型列表（按模型名升序，保证界面顺序稳定）。
func (r *modelMetaRepository) List(ctx context.Context, query model.ModelQuery) ([]*model.Model, error) {
	where, args := buildModelWhere(query)

	limit := normalizeLimit(query.Limit, defaultModelPageSize, maxModelPageSize)
	offset := normalizeOffset(query.Offset)

	sqlText := "SELECT " + modelMetaColumns + " FROM models" + where +
		" ORDER BY name ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询模型列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	models := make([]*model.Model, 0, 32)
	for rows.Next() {
		m, err := scanModelMeta(rows)
		if err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历模型列表失败: %w", err)
	}
	return models, nil
}

// Count 统计模型数量。
func (r *modelMetaRepository) Count(ctx context.Context, query model.ModelQuery) (int64, error) {
	where, args := buildModelWhere(query)

	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM models"+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计模型数失败: %w", err)
	}
	return total, nil
}

// Update 按 ID 更新模型。
//
// 刻意不更新 name：它是被令牌白名单、渠道清单与映射引用的标识，
// 允许改名会让历史配置静默失联。
func (r *modelMetaRepository) Update(ctx context.Context, m *model.Model) error {
	if m.ID == 0 {
		return errors.New("store: 更新模型时 ID 不能为 0")
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("store: 模型非法: %w", err)
	}

	caps, err := encodeCapabilities(m.Capabilities)
	if err != nil {
		return err
	}

	m.UpdatedAt = time.Now()
	res, err := r.db.ExecContext(ctx, `
		UPDATE models SET
			display_name = ?, vendor = ?, description = ?, context_length = ?,
			enabled = ?, capabilities = ?, updated_at = ?
		WHERE id = ?`,
		m.DisplayName, strings.TrimSpace(m.Vendor), m.Description, m.ContextLength,
		boolToInt(m.Enabled), caps, m.UpdatedAt.Unix(), m.ID,
	)
	if err != nil {
		return fmt.Errorf("store: 更新模型 %d 失败: %w", m.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrModelMetaNotFound
	}
	return nil
}

// Delete 按 ID 删除模型。
func (r *modelMetaRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM models WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除模型 %d 失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取删除影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrModelMetaNotFound
	}
	return nil
}

// UpsertBatch 批量写入模型（存在即更新，不存在即新增），单事务完成。
//
// 用 INSERT ... ON CONFLICT(name) DO UPDATE 而不是"先查后写"：
// 一条语句即完成，且在单事务内不存在检查与写入之间的并发窗口。
// 冲突目标 name 上有唯一索引（见迁移 0016），这是该语句成立的前提。
func (r *modelMetaRepository) UpsertBatch(ctx context.Context, models []*model.Model) error {
	if len(models) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启批量写入事务失败: %w", err)
	}
	// 失败时回滚；成功 Commit 后此调用为无操作
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO models
			(name, display_name, vendor, description, context_length, enabled, capabilities, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			display_name   = excluded.display_name,
			vendor         = excluded.vendor,
			description    = excluded.description,
			context_length = excluded.context_length,
			enabled        = excluded.enabled,
			capabilities   = excluded.capabilities,
			updated_at     = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("store: 准备批量写入语句失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now()
	for _, m := range models {
		if m == nil {
			continue
		}
		if err := m.Validate(); err != nil {
			return fmt.Errorf("store: 模型非法: %w", err)
		}
		m.Name = strings.TrimSpace(m.Name)
		m.Vendor = strings.TrimSpace(m.Vendor)
		caps, err := encodeCapabilities(m.Capabilities)
		if err != nil {
			return err
		}
		createdAt := m.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		m.CreatedAt = createdAt
		m.UpdatedAt = now

		if _, err := stmt.ExecContext(ctx,
			m.Name, m.DisplayName, m.Vendor, m.Description, m.ContextLength,
			boolToInt(m.Enabled), caps, createdAt.Unix(), m.UpdatedAt.Unix(),
		); err != nil {
			return fmt.Errorf("store: 批量写入模型 %q 失败: %w", m.Name, err)
		}
		// 回填 ID，使调用方拿到的实体与 Create 一致（可继续用于 Update 等）
		if err := tx.QueryRowContext(ctx, "SELECT id FROM models WHERE name = ?", m.Name).Scan(&m.ID); err != nil {
			return fmt.Errorf("store: 读取模型 %q 的 ID 失败: %w", m.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交批量写入失败: %w", err)
	}
	return nil
}

// buildModelWhere 依据查询条件拼装 WHERE 子句与参数。
func buildModelWhere(query model.ModelQuery) (string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 4)

	if keyword := strings.TrimSpace(query.Keyword); keyword != "" {
		// 关键词同时匹配模型名与展示名：管理员往往记得展示名而非标识
		conditions = append(conditions, "(name LIKE ? OR display_name LIKE ?)")
		like := "%" + keyword + "%"
		args = append(args, like, like)
	}
	if vendor := strings.TrimSpace(query.Vendor); vendor != "" {
		conditions = append(conditions, "vendor = ?")
		args = append(args, vendor)
	}
	if query.Enabled != nil {
		conditions = append(conditions, "enabled = ?")
		args = append(args, boolToInt(*query.Enabled))
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// scanModelMetaOrNotFound 扫描单行，未命中时转换为领域错误。
func scanModelMetaOrNotFound(sc rowScanner) (*model.Model, error) {
	m, err := scanModelMeta(sc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrModelMetaNotFound
		}
		return nil, err
	}
	return m, nil
}

// scanModelMeta 把一行数据映射为模型对象。
func scanModelMeta(sc rowScanner) (*model.Model, error) {
	var (
		id            uint64
		name          string
		displayName   string
		vendor        string
		description   string
		contextLength int64
		enabled       int
		capabilities  string
		createdAt     int64
		updatedAt     int64
	)

	if err := sc.Scan(&id, &name, &displayName, &vendor, &description, &contextLength,
		&enabled, &capabilities, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取模型字段失败: %w", err)
	}

	return &model.Model{
		ID:            id,
		Name:          name,
		DisplayName:   displayName,
		Vendor:        vendor,
		Description:   description,
		ContextLength: contextLength,
		Enabled:       enabled != 0,
		Capabilities:  decodeCapabilities(capabilities),
		CreatedAt:     time.Unix(createdAt, 0),
		UpdatedAt:     time.Unix(updatedAt, 0),
	}, nil
}

// encodeCapabilities 把能力列表序列化为 JSON 数组字符串。
//
// 空列表落库为 "[]" 而不是空串：空串不是合法 JSON，读取侧会困惑于
// "到底是空数组还是脏数据"，统一为 "[]" 可让解析路径无分支。
func encodeCapabilities(capabilities []string) (string, error) {
	normalized := make([]string, 0, len(capabilities))
	for _, c := range capabilities {
		if v := strings.TrimSpace(c); v != "" {
			normalized = append(normalized, v)
		}
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("store: 序列化能力标签失败: %w", err)
	}
	return string(raw), nil
}

// decodeCapabilities 解析能力标签；脏数据一律降级为空列表（不影响模型可用性）。
func decodeCapabilities(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var capabilities []string
	if err := json.Unmarshal([]byte(raw), &capabilities); err != nil {
		return nil
	}
	return capabilities
}

// ---------------------------------------------------------------------------
// 渠道级模型映射
// ---------------------------------------------------------------------------

// channelModelMappingRepository 是 model.ChannelModelMappingRepository 的 SQL 实现。
type channelModelMappingRepository struct {
	db *sql.DB
}

// NewChannelModelMappingRepository 创建渠道模型映射仓储。
func NewChannelModelMappingRepository(db *sql.DB) model.ChannelModelMappingRepository {
	return &channelModelMappingRepository{db: db}
}

// ListByChannel 查询某渠道的全部映射（含停用）。
func (r *channelModelMappingRepository) ListByChannel(ctx context.Context, channelID uint64) ([]*model.ChannelModelMapping, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+channelModelMappingColumns+" FROM channel_model_mappings"+
			" WHERE channel_id = ? ORDER BY priority DESC, id ASC", channelID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询渠道 %d 的模型映射失败: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()

	mappings := make([]*model.ChannelModelMapping, 0, 16)
	for rows.Next() {
		m, err := scanChannelModelMapping(rows)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历模型映射失败: %w", err)
	}
	return mappings, nil
}

// ReplaceForChannel 用给定映射整组替换该渠道的映射，单事务完成。
//
// 先在校验阶段拦住"同渠道内上游模型名重复"：唯一索引虽也会报错，
// 但在应用层给出可读错误（ErrChannelModelMappingDuplicated）比让驱动抛
// UNIQUE 约束错误更利于后台界面提示。
func (r *channelModelMappingRepository) ReplaceForChannel(
	ctx context.Context, channelID uint64, mappings []*model.ChannelModelMapping,
) error {
	if channelID == 0 {
		return errors.New("store: 替换模型映射时渠道 ID 不能为 0")
	}

	now := time.Now()
	prepared := make([]*model.ChannelModelMapping, 0, len(mappings))
	seen := make(map[string]struct{}, len(mappings))
	for _, m := range mappings {
		if m == nil {
			continue
		}
		m.ChannelID = channelID
		if err := m.Validate(); err != nil {
			return fmt.Errorf("store: 渠道模型映射非法: %w", err)
		}
		m.UpstreamModel = strings.TrimSpace(m.UpstreamModel)
		m.PublicModel = strings.TrimSpace(m.PublicModel)
		if _, duplicated := seen[m.UpstreamModel]; duplicated {
			return model.ErrChannelModelMappingDuplicated
		}
		seen[m.UpstreamModel] = struct{}{}
		if m.CreatedAt.IsZero() {
			m.CreatedAt = now
		}
		m.UpdatedAt = now
		prepared = append(prepared, m)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启映射替换事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM channel_model_mappings WHERE channel_id = ?", channelID); err != nil {
		return fmt.Errorf("store: 清空渠道 %d 的模型映射失败: %w", channelID, err)
	}

	for _, m := range prepared {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO channel_model_mappings
				(channel_id, upstream_model, public_model, priority, enabled, remark, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ChannelID, m.UpstreamModel, m.PublicModel, m.Priority,
			boolToInt(m.Enabled), m.Remark, m.CreatedAt.Unix(), m.UpdatedAt.Unix(),
		)
		if err != nil {
			if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
				return model.ErrChannelModelMappingDuplicated
			}
			return fmt.Errorf("store: 写入渠道 %d 的模型映射失败: %w", channelID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: 读取映射 ID 失败: %w", err)
		}
		m.ID = uint64(id)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交映射替换失败: %w", err)
	}
	return nil
}

// scanChannelModelMapping 把一行数据映射为映射对象。
func scanChannelModelMapping(sc rowScanner) (*model.ChannelModelMapping, error) {
	var (
		id            uint64
		channelID     uint64
		upstreamModel string
		publicModel   string
		priority      int
		enabled       int
		remark        string
		createdAt     int64
		updatedAt     int64
	)

	if err := sc.Scan(&id, &channelID, &upstreamModel, &publicModel, &priority,
		&enabled, &remark, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取模型映射字段失败: %w", err)
	}

	return &model.ChannelModelMapping{
		ID:            id,
		ChannelID:     channelID,
		UpstreamModel: upstreamModel,
		PublicModel:   publicModel,
		Priority:      priority,
		Enabled:       enabled != 0,
		Remark:        remark,
		CreatedAt:     time.Unix(createdAt, 0),
		UpdatedAt:     time.Unix(updatedAt, 0),
	}, nil
}
