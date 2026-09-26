// 本文件定义「访问令牌（Token）」领域模型。
//
// 意图（Why）：
//
//	令牌是网关对外的"下游凭证"：使用者拿它调用网关，网关据此判断
//	  「你是谁、能不能用、能用哪些模型、还有多少额度」。
//	把判定规则放在领域层，可以保证无论从哪条路径（后台 API、批量导入、命令行）
//	创建令牌，都遵守同一套约束。
//
// 流转（Flow）：
//
//	internal/store 实现 TokenRepository
//	  ├─ 写入：Validate → 计算 key_hash（查找用）+ key_enc（展示用）→ 落库
//	  └─ 读取：按 key_hash 或 id 取出 → 解密 key → 返回领域对象
//	internal/server 的鉴权中间件调用 GetByKey 完成校验
//
// 扩展（Extend）：
//
//	新增令牌属性（如 IP 白名单、RPM 限制）时：
//	  1) 在此结构体加字段并补 Validate 规则；
//	  2) 新建迁移脚本（如 0003_token_xxx.sql）加列——切勿修改已发布的脚本；
//	  3) 同步更新 internal/store/token_repo.go 的列清单与扫描逻辑。
package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrTokenNotFound 表示未找到指定令牌。
var ErrTokenNotFound = errors.New("model: 令牌不存在")

// 令牌 KEY 的格式约定。
const (
	// TokenKeyPrefix 是令牌 KEY 的固定前缀，便于使用者一眼识别这是网关令牌。
	TokenKeyPrefix = "sk-"
	// tokenKeyRandomBytes 是 KEY 的随机部分长度（字节）。
	// 24 字节 = 192 位熵，十六进制编码后为 48 个字符，暴力枚举不可行。
	tokenKeyRandomBytes = 24
)

// TokenStatus 表示令牌状态。
//
// 取值与数据库 tokens.status 一一对应，禁止改动已落库的数值。
type TokenStatus int

const (
	// TokenStatusEnabled 启用：可正常调用。
	TokenStatusEnabled TokenStatus = 1
	// TokenStatusDisabled 手动禁用：由管理员关闭，不因时间或额度自动恢复。
	TokenStatusDisabled TokenStatus = 2
	// TokenStatusExpired 已过期：超过 expires_at 后由系统判定。
	TokenStatusExpired TokenStatus = 3
	// TokenStatusExhausted 额度耗尽：非不限额度且剩余额度不足时由系统判定。
	TokenStatusExhausted TokenStatus = 4
)

// String 返回状态的中文名，便于日志与后台展示。
func (s TokenStatus) String() string {
	switch s {
	case TokenStatusEnabled:
		return "启用"
	case TokenStatusDisabled:
		return "手动禁用"
	case TokenStatusExpired:
		return "已过期"
	case TokenStatusExhausted:
		return "额度耗尽"
	default:
		return fmt.Sprintf("未知(%d)", int(s))
	}
}

// IsValid 判断状态取值是否合法。
func (s TokenStatus) IsValid() bool {
	switch s {
	case TokenStatusEnabled, TokenStatusDisabled, TokenStatusExpired, TokenStatusExhausted:
		return true
	default:
		return false
	}
}

// Token 表示一个访问令牌。
//
// 安全约定（与 Channel 一致）：
//
//	Key 字段在内存中是【明文】，落库时由仓储加密（key_enc）并另存摘要（key_hash）。
//	对外输出必须使用 MaskedKey()，绝不直接序列化本结构体。
type Token struct {
	ID      uint64 // 主键
	OwnerID uint64 // 归属用户 ID（0 = 系统令牌，M2 尚无用户体系）
	Name    string // 名称，便于区分用途（如 "CI 构建用"）
	Key     string // 令牌明文【仅内存】，形如 sk-<48位十六进制>

	Status TokenStatus // 状态

	// ExpiresAt 为过期时间；零值表示永不过期（落库为 0）。
	ExpiresAt time.Time

	RemainQuota    int64 // 剩余额度（内部单位）
	UnlimitedQuota bool  // 是否不限额度（为 true 时忽略 RemainQuota）
	UsedQuota      int64 // 已用额度（内部单位）

	// Models 是允许使用的模型白名单；为空表示不限制。
	Models []string

	CreatedAt time.Time
	UpdatedAt time.Time

	// LastUsedAt 是最近一次使用时间；零值表示从未使用。
	//
	// 用途：帮助使用者辨认"哪些 key 还在用、哪些可以清理"，也便于发现异常调用。
	LastUsedAt time.Time
}

// GenerateTokenKey 生成一个新的令牌 KEY，形如 sk-<48 位十六进制>。
//
// 使用 crypto/rand：必须用密码学安全随机源，否则令牌可被预测（严重安全问题）。
func GenerateTokenKey() (string, error) {
	buf := make([]byte, tokenKeyRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("model: 生成令牌随机数失败: %w", err)
	}
	return TokenKeyPrefix + hex.EncodeToString(buf), nil
}

// Validate 校令令牌字段合法性，供创建与更新时调用。
func (t *Token) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("令牌名称不能为空")
	}

	// KEY 校验：既接受由 GenerateTokenKey 生成的标准格式，也拒绝明显异常的输入
	if !strings.HasPrefix(t.Key, TokenKeyPrefix) {
		return fmt.Errorf("令牌 KEY 必须以 %q 开头", TokenKeyPrefix)
	}
	if len(t.Key) <= len(TokenKeyPrefix) {
		return errors.New("令牌 KEY 缺少随机部分")
	}

	if !t.Status.IsValid() {
		return fmt.Errorf("令牌状态非法: %d", int(t.Status))
	}

	// 额度校验：不限额度时忽略剩余额度；否则剩余额度不能为负
	if !t.UnlimitedQuota && t.RemainQuota < 0 {
		return fmt.Errorf("令牌剩余额度不能为负数: %d", t.RemainQuota)
	}
	if t.UsedQuota < 0 {
		return fmt.Errorf("令牌已用额度不能为负数: %d", t.UsedQuota)
	}

	return nil
}

// IsExpired 判断令牌在给定时刻是否已过期（永不过期返回 false）。
//
// 传入 now 而非在内部取当前时间，是为了便于测试与保证同一请求内的判定一致。
func (t *Token) IsExpired(now time.Time) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return now.After(t.ExpiresAt)
}

// HasQuota 判断令牌是否还有可用额度。
//
// 说明：不限额度、或剩余额度大于 0 时视为有额度。
// 精确扣费在 M5 计费模块实现，此处只做"能否放行"的粗判。
func (t *Token) HasQuota() bool {
	if t.UnlimitedQuota {
		return true
	}
	return t.RemainQuota > 0
}

// AllowsModel 判断令牌是否被允许访问指定模型。
//
// 匹配规则：白名单为空表示不限制；否则精确匹配（大小写敏感）。
func (t *Token) AllowsModel(name string) bool {
	if len(t.Models) == 0 {
		return true
	}
	for _, m := range t.Models {
		if m == name {
			return true
		}
	}
	return false
}

// EffectiveStatus 返回"结合当前时间与额度"后的实际状态。
//
// 设计意图：数据库里的 status 只记录管理员意图（启用/手动禁用），
// 而"是否过期""额度是否耗尽"是随时间变化的事实，应在判定时实时计算。
// 这样才能做到"到期即失效"，而无需依赖定时任务去批量改状态。
//
// 判定优先级：手动禁用 > 过期 > 额度耗尽 > 启用。
// 说明：手动禁用优先，是因为管理员的显式操作应当压过自动判定。
func (t *Token) EffectiveStatus(now time.Time) TokenStatus {
	if t.Status == TokenStatusDisabled {
		return TokenStatusDisabled
	}
	if t.IsExpired(now) {
		return TokenStatusExpired
	}
	if !t.HasQuota() {
		return TokenStatusExhausted
	}
	return TokenStatusEnabled
}

// MaskedKey 返回脱敏后的令牌 KEY，供日志与后台列表展示使用。
//
// 脱敏规则：保留前缀与末 4 位，例如 "sk-abcdef12...cdef" → "sk-abc****cdef"。
// 目的：让使用者能辨认是哪把令牌，同时不足以被直接盗用。
func (t *Token) MaskedKey() string {
	const keepSuffix = 4

	key := t.Key
	if key == "" {
		return ""
	}
	if len(key) <= len(TokenKeyPrefix)+keepSuffix {
		return strings.Repeat("*", len(key))
	}
	return key[:len(TokenKeyPrefix)+3] + "****" + key[len(key)-keepSuffix:]
}

// TokenQuery 描述令牌列表的查询条件。
type TokenQuery struct {
	OwnerID *uint64      // 按归属人过滤；nil 表示不过滤
	Status  *TokenStatus // 按状态过滤；nil 表示不过滤
	Limit   int          // 返回条数上限；<=0 使用默认值
	Offset  int          // 偏移量，用于分页
}

// TokenRepository 定义令牌的持久化操作。
//
// 实现约定：
//   - 写入时必须保存 key_hash（摘要，供查找）与 key_enc（密文，供展示）；
//   - GetByKey 必须走摘要索引查找，禁止全表解密比对；
//   - 未找到时返回 ErrTokenNotFound。
type TokenRepository interface {
	// Create 新增令牌，成功后回填 ID、CreatedAt、UpdatedAt。
	Create(ctx context.Context, t *Token) error

	// GetByID 按主键查询令牌，不存在时返回 ErrTokenNotFound。
	GetByID(ctx context.Context, id uint64) (*Token, error)

	// GetByKey 按令牌明文查询（内部通过摘要索引匹配），不存在时返回 ErrTokenNotFound。
	// 这是鉴权热路径，实现必须高效。
	GetByKey(ctx context.Context, key string) (*Token, error)

	// List 按条件查询令牌列表，按 ID 升序返回。
	List(ctx context.Context, q TokenQuery) ([]*Token, error)

	// Count 返回符合条件的令牌总数，用于分页。
	Count(ctx context.Context, q TokenQuery) (int, error)

	// StatusCounts 按状态分组统计令牌数量，用于仪表盘概览。
	StatusCounts(ctx context.Context) (map[TokenStatus]int, error)

	// RecordUsage 记录令牌最近一次使用时间。
	//
	// 说明：只更新一个字段，避免把整行写回造成并发覆盖。
	RecordUsage(ctx context.Context, id uint64, at time.Time) error

	// ConsumeQuota 调整令牌额度：amount 为正表示扣减，为负表示退还。
	//
	// 关键实现要求：必须在【单条 SQL】内完成自增与自减。
	// 若写成"先读出来、在内存里加减、再写回"，并发请求会互相覆盖，
	// 表现为"用于统计的已用额度明显偏小"——这是计费类系统最典型的漏计缺陷。
	//
	// 不限额度（unlimited_quota）的令牌只累加已用额度、不动剩余额度，
	// 但用量仍然照常记录，便于站长核算上游成本。
	// 剩余额度扣到 0 为止（不允许为负），避免出现"倒欠额度"的怪异状态。
	//
	// 退还（amount < 0）用于异步任务失败时回滚提交阶段已扣的额度。
	ConsumeQuota(ctx context.Context, id uint64, amount int64, at time.Time) error

	// Update 按 ID 更新令牌（不修改创建时间），不存在时返回 ErrTokenNotFound。
	Update(ctx context.Context, t *Token) error

	// Delete 按 ID 物理删除令牌，不存在时返回 ErrTokenNotFound。
	Delete(ctx context.Context, id uint64) error
}
