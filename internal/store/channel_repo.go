// 本文件是 model.ChannelRepository 接口的 SQL 实现。
//
// 意图（Why）：
//
//	把渠道数据真正落到 SQLite（后续可扩展 PostgreSQL）。此层的核心职责有三：
//	  1) 屏蔽 SQL 细节，让业务代码只面对领域模型；
//	  2) 承担【密钥加解密】——领域模型里是明文，落库必须是密文；
//	  3) 把数据库错误翻译成领域错误（如 ErrChannelNotFound）。
//
// 流转（Flow）：
//
//	main.go 装配 → NewChannelRepository(db, cipher)
//	  └─ server / relay 调用接口方法
//	       ├─ Create/Update：Validate 校验 → cipher.Encrypt 加密 → 写库
//	       └─ GetByID/List ：读库 → cipher.Decrypt 解密 → 返回领域对象
//
// 扩展（Extend）：
//
//	新增查询条件：在 model.ChannelQuery 加字段 → 在 List 中拼 WHERE 与参数（务必用占位符，
//	             禁止字符串拼接用户输入，避免 SQL 注入）。
//	新增字段：同步改 schema.sql（追加迁移）+ 本文件的 channelColumns / scanChannel / insert / update。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/crypto"
	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 列表查询的条数约束。
//
// 为什么要设上限：防止调用方传 0 或超大值导致一次性拉全表，
// 在渠道数量增长后会成为内存与慢查询风险。
const (
	defaultChannelListLimit = 100  // Limit 未指定时的默认条数
	maxChannelListLimit     = 1000 // 单次查询允许的最大条数
)

// channelColumns 集中定义查询列，避免各处手写列名导致顺序错乱。
//
// 注意：列顺序必须与 scanChannel 的 Scan 参数顺序严格一致。
const channelColumns = `id, name, type, type_key, extra_config, base_url, api_key_enc, models, group_name, priority, weight, status, created_at, updated_at, last_test_at, last_test_ok, key_strategy`

// channelRepository 是 model.ChannelRepository 的 SQL 实现。
//
// 并发安全：内部只持有 *sql.DB（自带连接池）与 *crypto.Cipher（无状态），可被多 goroutine 共享。
type channelRepository struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

// NewChannelRepository 创建渠道仓储。
//
// 参数 cipher 用于密钥加解密，不可为 nil —— 没有加密能力就不应该允许写入密钥。
func NewChannelRepository(db *sql.DB, cipher *crypto.Cipher) model.ChannelRepository {
	return &channelRepository{db: db, cipher: cipher}
}

// Create 新增渠道并回填数据库生成的 ID 与时间戳。
func (r *channelRepository) Create(ctx context.Context, ch *model.Channel) error {
	// 领域校验前置：保证任何入口写入的数据都符合规则
	if err := ch.Validate(); err != nil {
		return fmt.Errorf("store: 渠道数据非法: %w", err)
	}

	// 密钥加密：领域模型持有明文，落库一律转为密文
	encryptedKey, err := r.cipher.Encrypt(ch.APIKey)
	if err != nil {
		return fmt.Errorf("store: 加密渠道密钥失败: %w", err)
	}

	now := time.Now()
	ch.CreatedAt = now
	ch.UpdatedAt = now

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO channels
			(name, type, type_key, extra_config, base_url, api_key_enc, models, group_name, priority, weight, status, created_at, updated_at, key_strategy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ch.Name, ch.Type, ch.TypeKey, encodeExtraConfig(ch.ExtraConfig), ch.BaseURL, encryptedKey, encodeModels(ch.Models),
		ch.Group, ch.Priority, ch.Weight, int(ch.Status),
		ch.CreatedAt.Unix(), ch.UpdatedAt.Unix(),
		string(model.NormalizeKeyStrategy(string(ch.KeyStrategy))),
	)
	if err != nil {
		return fmt.Errorf("store: 新增渠道失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增渠道的自增 ID 失败: %w", err)
	}
	ch.ID = uint64(id)
	return nil
}

// GetByID 按主键查询渠道。不存在时返回 model.ErrChannelNotFound。
func (r *channelRepository) GetByID(ctx context.Context, id uint64) (*model.Channel, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+channelColumns+" FROM channels WHERE id = ?", id)

	ch, err := r.scanChannel(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrChannelNotFound
		}
		return nil, err
	}
	return ch, nil
}

// List 按条件查询渠道列表。
//
// 返回顺序固定为「优先级降序 → 权重降序 → ID 升序」，即路由选取候选时的推荐顺序，
// 上层可直接按序遍历做渠道选择，无需二次排序。
func (r *channelRepository) List(ctx context.Context, q model.ChannelQuery) ([]*model.Channel, error) {
	// 动态拼接 WHERE：所有值都通过占位符传入，杜绝 SQL 注入
	where, args := buildChannelWhere(q)

	var sb strings.Builder
	sb.WriteString("SELECT " + channelColumns + " FROM channels")
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}
	sb.WriteString(" ORDER BY priority DESC, weight DESC, id ASC")

	// 分页保护：归一化到合法区间
	limit := q.Limit
	if limit <= 0 {
		limit = defaultChannelListLimit
	}
	if limit > maxChannelListLimit {
		limit = maxChannelListLimit
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	sb.WriteString(" LIMIT ? OFFSET ?")
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询渠道列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 预分配容量，减少切片扩容
	channels := make([]*model.Channel, 0, limit)
	for rows.Next() {
		ch, err := r.scanChannel(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	// 注意：必须检查迭代过程中的错误，否则可能静默返回不完整结果
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历渠道结果集失败: %w", err)
	}
	return channels, nil
}

// Update 按 ID 更新渠道。不存在时返回 model.ErrChannelNotFound。
func (r *channelRepository) Update(ctx context.Context, ch *model.Channel) error {
	if ch.ID == 0 {
		return errors.New("store: 更新渠道时 ID 不能为 0")
	}
	if err := ch.Validate(); err != nil {
		return fmt.Errorf("store: 渠道数据非法: %w", err)
	}

	encryptedKey, err := r.cipher.Encrypt(ch.APIKey)
	if err != nil {
		return fmt.Errorf("store: 加密渠道密钥失败: %w", err)
	}

	ch.UpdatedAt = time.Now()

	// 刻意不更新 created_at：创建时间应保持不可变，便于审计
	res, err := r.db.ExecContext(ctx, `
		UPDATE channels SET
			name = ?, type = ?, type_key = ?, extra_config = ?, base_url = ?, api_key_enc = ?, models = ?,
			group_name = ?, priority = ?, weight = ?, status = ?, updated_at = ?, key_strategy = ?
		WHERE id = ?`,
		ch.Name, ch.Type, ch.TypeKey, encodeExtraConfig(ch.ExtraConfig), ch.BaseURL, encryptedKey, encodeModels(ch.Models),
		ch.Group, ch.Priority, ch.Weight, int(ch.Status),
		ch.UpdatedAt.Unix(), string(model.NormalizeKeyStrategy(string(ch.KeyStrategy))), ch.ID,
	)
	if err != nil {
		return fmt.Errorf("store: 更新渠道 %d 失败: %w", ch.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		// 影响 0 行说明 ID 不存在（而不是"内容没变化"——SQLite 对相同值更新仍计入行数）
		return model.ErrChannelNotFound
	}
	return nil
}

// Delete 按 ID 物理删除渠道。不存在时返回 model.ErrChannelNotFound。
func (r *channelRepository) Delete(ctx context.Context, id uint64) error {
	res, err := r.db.ExecContext(ctx, "DELETE FROM channels WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: 删除渠道 %d 失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取删除影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrChannelNotFound
	}
	return nil
}

// buildChannelWhere 构造渠道查询的 WHERE 子句与参数。
//
// 抽成独立函数是为了让"列表查询"与"计数查询"共用同一份筛选逻辑——
// 若各写一份，很容易出现"列表按分组过滤、计数忘了过滤"导致分页总页数错误。
func buildChannelWhere(q model.ChannelQuery) (string, []any) {
	var (
		conditions []string
		args       []any
	)
	if q.Group != "" {
		conditions = append(conditions, "group_name = ?")
		args = append(args, q.Group)
	}
	if q.Status != nil {
		conditions = append(conditions, "status = ?")
		args = append(args, int(*q.Status))
	}
	return strings.Join(conditions, " AND "), args
}

// Count 返回符合条件的渠道总数。
func (r *channelRepository) Count(ctx context.Context, q model.ChannelQuery) (int, error) {
	where, args := buildChannelWhere(q)

	sb := strings.Builder{}
	sb.WriteString("SELECT COUNT(1) FROM channels")
	if where != "" {
		sb.WriteString(" WHERE " + where)
	}

	var total int
	if err := r.db.QueryRowContext(ctx, sb.String(), args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计渠道数失败: %w", err)
	}
	return total, nil
}

// StatusCounts 按状态分组统计渠道数量。
func (r *channelRepository) StatusCounts(ctx context.Context) (map[model.ChannelStatus]int, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT status, COUNT(1) FROM channels GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("store: 统计渠道状态失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[model.ChannelStatus]int, 3)
	for rows.Next() {
		var (
			status int
			count  int
		)
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("store: 读取渠道状态统计失败: %w", err)
		}
		counts[model.ChannelStatus(status)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历渠道状态统计失败: %w", err)
	}
	return counts, nil
}

// RecordTestResult 记录一次测活结果。
//
// 实现要点：只更新两个字段，不像 Update 那样把整行配置写回——
// 测活是后台高频行为，若整行写回，会用陈旧副本覆盖管理员刚修改的配置。
func (r *channelRepository) RecordTestResult(ctx context.Context, id uint64, at time.Time, ok bool) error {
	res, err := r.db.ExecContext(ctx,
		"UPDATE channels SET last_test_at = ?, last_test_ok = ? WHERE id = ?",
		at.Unix(), boolToInt(ok), id)
	if err != nil {
		return fmt.Errorf("store: 记录渠道 %d 测活结果失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return model.ErrChannelNotFound
	}
	return nil
}

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的共同能力，使扫描逻辑只需写一份。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanChannel 把一行数据映射为领域对象，并完成密钥解密。
//
// 错误处理说明：
//   - sql.ErrNoRows 原样返回，由调用方翻译成 model.ErrChannelNotFound；
//   - 解密失败会包装为明确错误——通常意味着 AQUA_APP_KEY 被更换或数据被篡改。
func (r *channelRepository) scanChannel(sc rowScanner) (*model.Channel, error) {
	var (
		id          uint64
		name        string
		channelTy   int
		typeKey     string
		extraJSON   string
		baseURL     string
		encoded     string
		modelsCSV   string
		group       string
		priority    int
		weight      int
		status      int
		createdAt   int64
		updatedAt   int64
		lastTestAt  int64
		lastTestOK  int
		keyStrategy string
	)

	if err := sc.Scan(&id, &name, &channelTy, &typeKey, &extraJSON, &baseURL, &encoded, &modelsCSV,
		&group, &priority, &weight, &status, &createdAt, &updatedAt,
		&lastTestAt, &lastTestOK, &keyStrategy); err != nil {
		// sql.ErrNoRows 属于正常控制流，不额外包装，便于调用方用 errors.Is 判断
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取渠道字段失败: %w", err)
	}

	apiKey, err := r.cipher.Decrypt(encoded)
	if err != nil {
		return nil, fmt.Errorf("store: 解密渠道 %d 的密钥失败（AQUA_APP_KEY 是否变更？）: %w", id, err)
	}

	return &model.Channel{
		ID:          id,
		Name:        name,
		Type:        channelTy,
		TypeKey:     typeKey,
		ExtraConfig: decodeExtraConfig(extraJSON),
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Models:      decodeModels(modelsCSV),
		Group:       group,
		Priority:    priority,
		Weight:      weight,
		Status:      model.ChannelStatus(status),
		CreatedAt:   time.Unix(createdAt, 0),
		UpdatedAt:   time.Unix(updatedAt, 0),
		LastTestAt:  unixToExpiresAt(lastTestAt), // 复用"0 表示零值时间"的转换
		LastTestOK:  lastTestOK != 0,

		KeyStrategy: model.NormalizeKeyStrategy(keyStrategy),
	}, nil
}

// encodeModels 把模型列表编码为逗号分隔字符串（落库格式）。
//
// 处理细则：去除首尾空白、丢弃空项、去重（保持首次出现顺序）。
// 去重的意义：避免因重复配置导致路由时对同一模型重复候选。
func encodeModels(models []string) string {
	if len(models) == 0 {
		return ""
	}

	seen := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, m := range models {
		name := strings.TrimSpace(m)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return strings.Join(result, ",")
}

// decodeModels 把逗号分隔字符串还原为模型列表（读取格式）。
func decodeModels(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if name := strings.TrimSpace(p); name != "" {
			result = append(result, name)
		}
	}
	return result
}

// encodeExtraConfig 把类型专属参数序列化为 JSON 对象字符串（落库格式）。
//
// 空值一律落成 "{}"：让"没有扩展配置"在库里只有一种表示（空 JSON 对象），
// 避免 NULL 与空串并存导致读取端要处理两种"没有值"的情形。
// 序列化失败（理论上不会：键值均为字符串）时同样回退为 "{}"，绝不让一次
// 保存因扩展配置而整体失败——扩展配置只是锦上添花，不应阻断主流程。
func encodeExtraConfig(values map[string]string) string {
	if len(values) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// decodeExtraConfig 把 JSON 对象字符串还原为键值映射（读取格式）。
//
// 返回 nil 表示"无扩展配置"（空串 / "{}" / 非法 JSON）：
// 协议适配器对 nil 映射会自动回退到类型默认值（见 relay 的 extraValue）。
// 非法 JSON 不报错而是视为空：历史脏数据不应让整条渠道无法读取。
func decodeExtraConfig(raw string) map[string]string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
