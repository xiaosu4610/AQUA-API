// 本文件定义「用户」与「登录会话」领域模型。
//
// 意图（Why）：
//
//	网关需要区分两类凭据，它们服务完全不同的场景，切勿混用：
//	  1) 会话令牌（本文件的 Session）：人通过浏览器登录网站后持有，用于访问管理与门户接口；
//	  2) 访问令牌（model.Token）：程序调用模型接口时持有（sk- 开头），用于转发鉴权。
//	用户是额度与归属的主体：令牌与调用日志都归属于某个用户，运营与计费围绕用户展开。
//
// 流转（Flow）：
//
//	注册：HashPassword(明文) → User{PasswordHash} → UserRepository.Create
//	登录：GetByUsername → VerifyPassword → SessionRepository.Create(会话令牌摘要)
//	鉴权：SessionRepository.GetByTokenHash → 反查 User → 注入请求上下文
//
// 扩展（Extend）：
//
//	新增用户属性（如邮箱验证、OAuth 绑定）时：
//	  1) 在此结构体加字段；
//	  2) 新建迁移脚本加列（切勿修改已发布的脚本）；
//	  3) 同步更新 internal/store/user_repo.go 的列清单与扫描逻辑。
package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// maxUsernameRunes 是用户名允许的最大字符数。
//
// 说明：用户名下限已取消（1 个字符即可注册），此处仅保留上限——
// 超长用户名会撑爆后台列表与登录框，也会放大唯一索引体积。
// 刻意使用「字符数」而非字节数：中文用户名按字节算是 3 倍，
// 用字节数限制会让"看起来一样长"的中英文名得到不同待遇。
const maxUsernameRunes = 64

// 领域错误。
var (
	// ErrUserNotFound 表示按条件未找到用户。
	ErrUserNotFound = errors.New("model: 用户不存在")
	// ErrUsernameTaken 表示用户名已被占用。
	ErrUsernameTaken = errors.New("model: 用户名已被占用")
	// ErrSessionNotFound 表示会话不存在或已失效。
	ErrSessionNotFound = errors.New("model: 会话不存在")
)

// UserRole 表示用户角色。
type UserRole int

const (
	// UserRoleUser 普通用户：仅能管理自己的令牌、查看自己的用量。
	UserRoleUser UserRole = 1
	// UserRoleAdmin 管理员：可管理渠道、令牌、用户与系统设置。
	UserRoleAdmin UserRole = 10
)

// IsValid 判断角色取值是否合法。
func (r UserRole) IsValid() bool {
	return r == UserRoleUser || r == UserRoleAdmin
}

// String 返回角色的中文名。
func (r UserRole) String() string {
	switch r {
	case UserRoleUser:
		return "普通用户"
	case UserRoleAdmin:
		return "管理员"
	default:
		return fmt.Sprintf("未知(%d)", int(r))
	}
}

// UserStatus 表示用户状态。
type UserStatus int

const (
	// UserStatusEnabled 启用。
	UserStatusEnabled UserStatus = 1
	// UserStatusDisabled 禁用：禁止登录，其令牌也不应再被放行。
	UserStatusDisabled UserStatus = 2
)

// IsValid 判断状态取值是否合法。
func (s UserStatus) IsValid() bool {
	return s == UserStatusEnabled || s == UserStatusDisabled
}

// String 返回状态的中文名。
func (s UserStatus) String() string {
	switch s {
	case UserStatusEnabled:
		return "启用"
	case UserStatusDisabled:
		return "禁用"
	default:
		return fmt.Sprintf("未知(%d)", int(s))
	}
}

// QuotaUnlimited 表示「不限额度」，用于 User.Quota。
//
// 用 -1 而非 0 表示不限：0 表示"没有额度"，与"不想限制"是完全相反的语义，
// 若混用会导致「新建用户默认无法调用」或「本该受限的用户可无限使用」。
const QuotaUnlimited int64 = -1

// User 表示一个用户。
//
// 安全约定：PasswordHash 只存 bcrypt 哈希，绝不存明文；
// 该字段不得出现在任何对外响应中（对外结构体见 internal/server 的 DTO）。
type User struct {
	ID           uint64     // 主键
	Username     string     // 登录名
	PasswordHash string     // 口令哈希（bcrypt）
	Email        string     // 邮箱（可选）
	Role         UserRole   // 角色
	Status       UserStatus // 状态
	Quota        int64      // 总额度；QuotaUnlimited(-1) 表示不限
	UsedQuota    int64      // 已用额度
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// IsAdmin 判断是否为管理员。
func (u *User) IsAdmin() bool {
	return u.Role == UserRoleAdmin
}

// IsActive 判断用户是否可正常使用（未被禁用）。
func (u *User) IsActive() bool {
	return u.Status == UserStatusEnabled
}

// HasQuota 判断用户是否还有可用额度。
//
// 说明：不限额度、或「总额度 - 已用额度 > 0」时视为有额度。
func (u *User) HasQuota() bool {
	if u.Quota == QuotaUnlimited {
		return true
	}
	return u.Quota-u.UsedQuota > 0
}

// RemainingQuota 返回剩余额度；不限额度时返回 QuotaUnlimited。
func (u *User) RemainingQuota() int64 {
	if u.Quota == QuotaUnlimited {
		return QuotaUnlimited
	}
	return u.Quota - u.UsedQuota
}

// Validate 校验用户字段合法性。
//
// 说明：不校验 PasswordHash 的格式——它由 crypto.HashPassword 生成，
// 这里只保证非空，避免把"哈希算法细节"耦合进领域校验。
func (u *User) Validate() error {
	if strings.TrimSpace(u.Username) == "" {
		return errors.New("用户名不能为空")
	}
	// 用户名【不再限制最小长度】：1 个字符也允许。
	//
	// 为什么取消下限：用户名只是标识符，真正的鉴权依据是口令；
	// 强制"至少 3 个字符"既挡不住攻击者（他们用随机长串），
	// 又会让想用简短昵称（如"水""A"）的正常用户被拒。
	// 唯一性由数据库唯一索引保证，与长度无关。
	//
	// 仍保留上限的原因与取值：超长用户名会撑爆列表界面、放大唯一索引，
	// 也便于构造超长请求；按【字符数】限制在 64 以内，
	// 用字符数而非字节数是为了让中文用户名不被按 3 倍字节误判。
	if count := utf8.RuneCountInString(u.Username); count > maxUsernameRunes {
		return fmt.Errorf("用户名最多 %d 个字符，当前 %d", maxUsernameRunes, count)
	}
	if u.PasswordHash == "" {
		return errors.New("口令哈希不能为空（请勿直接构造用户对象，应通过服务层创建）")
	}
	if !u.Role.IsValid() {
		return fmt.Errorf("用户角色非法: %d", int(u.Role))
	}
	if !u.Status.IsValid() {
		return fmt.Errorf("用户状态非法: %d", int(u.Status))
	}
	if u.Quota < QuotaUnlimited {
		return fmt.Errorf("用户总额度非法: %d（允许 -1 表示不限）", u.Quota)
	}
	if u.UsedQuota < 0 {
		return fmt.Errorf("用户已用额度不能为负数: %d", u.UsedQuota)
	}
	return nil
}

// EffectiveStatus 返回"结合额度"后的实际状态，供鉴权与调用前判定使用。
//
// 为什么要单独计算：数据库中的 status 只记录管理员意图（启用/禁用），
// "额度是否耗尽"是随用量变化的事实，应在判定时实时计算。
func (u *User) EffectiveStatus() UserStatus {
	if u.Status == UserStatusDisabled {
		return UserStatusDisabled
	}
	return UserStatusEnabled
}

// UserQuery 描述用户列表的查询条件。
type UserQuery struct {
	Keyword string      // 按用户名/邮箱模糊匹配；空表示不过滤
	Role    *UserRole   // 按角色过滤
	Status  *UserStatus // 按状态过滤
	Limit   int         // 条数上限；<=0 使用默认值
	Offset  int         // 偏移量
}

// UserRepository 定义用户的持久化操作。
type UserRepository interface {
	// Create 新增用户，成功后回填 ID 与时间戳。
	// 用户名重复时返回 ErrUsernameTaken。
	Create(ctx context.Context, u *User) error

	// GetByID 按主键查询，不存在时返回 ErrUserNotFound。
	GetByID(ctx context.Context, id uint64) (*User, error)

	// GetByUsername 按登录名查询，不存在时返回 ErrUserNotFound。
	GetByUsername(ctx context.Context, username string) (*User, error)

	// List 按条件查询用户列表，按 ID 升序返回。
	List(ctx context.Context, q UserQuery) ([]*User, error)

	// Count 返回符合条件的用户总数，用于分页。
	Count(ctx context.Context, q UserQuery) (int, error)

	// Update 按 ID 更新用户（不修改创建时间），不存在时返回 ErrUserNotFound。
	Update(ctx context.Context, u *User) error

	// Delete 按 ID 删除用户，不存在时返回 ErrUserNotFound。
	Delete(ctx context.Context, id uint64) error

	// AddUsedQuota 累加已用额度（可为负，用于回滚），并同步更新令牌/用户额度。
	//
	// 说明：采用「增量更新」而非"读取-修改-写回"，避免并发请求互相覆盖。
	AddUsedQuota(ctx context.Context, id uint64, delta int64) error

	// AddQuota 累加用户【总额度】（充值入账时使用），并返回累加后的总额度。
	//
	// 为什么需要它而不是复用 Update：充值入账与"用户编辑资料"是两条路径，
	// 后者会把整行写回，存在"用陈旧副本覆盖刚入账额度"的风险。
	// 增量更新还天然支持"同一秒到账多笔订单"。
	//
	// 特殊约定：若该用户为不限额度（quota = -1），则【不做任何修改】并返回
	// QuotaUnlimited —— 给"不限"加数字没有意义，反而会把它变成有限额度。
	AddQuota(ctx context.Context, id uint64, delta int64) (int64, error)

	// CountAdmins 返回管理员数量。
	//
	// 用途：删除/降级最后一个管理员会让系统无人可管理，需据此拒绝该操作。
	CountAdmins(ctx context.Context) (int, error)
}

// 会话令牌的格式约定。
const (
	// SessionTokenPrefix 是会话令牌的固定前缀，便于在日志中一眼识别。
	SessionTokenPrefix = "sess_"
	// sessionTokenRandomBytes 是随机部分长度（字节）。32 字节 = 256 位熵。
	sessionTokenRandomBytes = 32
)

// GenerateSessionToken 生成一个会话令牌明文，形如 sess_<64 位十六进制>。
//
// 必须使用密码学安全随机源，否则会话可被预测（等于账号被接管）。
func GenerateSessionToken() (string, error) {
	buf := make([]byte, sessionTokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("model: 生成会话令牌失败: %w", err)
	}
	return SessionTokenPrefix + hex.EncodeToString(buf), nil
}

// Session 表示一次网站登录会话。
//
// 安全约定：TokenHash 存的是会话令牌的 SHA-256 摘要，不存明文——
// 数据库泄露时攻击者无法用它直接登录（与访问令牌表同策略）。
type Session struct {
	ID        uint64    // 主键
	UserID    uint64    // 归属用户
	TokenHash string    // 令牌明文的 SHA-256 摘要
	ExpiresAt time.Time // 过期时间
	CreatedAt time.Time // 创建时间
}

// IsExpired 判断会话在给定时刻是否已过期。
func (s *Session) IsExpired(now time.Time) bool {
	return now.After(s.ExpiresAt)
}

// SessionQuery 描述会话查询条件。
type SessionQuery struct {
	UserID *uint64 // 按用户过滤
	Limit  int
	Offset int
}

// SessionRepository 定义会话的持久化操作。
type SessionRepository interface {
	// Create 新增会话。
	Create(ctx context.Context, s *Session) error

	// GetByTokenHash 按摘要查询会话，不存在时返回 ErrSessionNotFound。
	GetByTokenHash(ctx context.Context, tokenHash string) (*Session, error)

	// DeleteByTokenHash 按摘要删除会话（退出登录）。
	//
	// 不存在时不返回错误：重复退出登录属于正常操作，不应报错。
	DeleteByTokenHash(ctx context.Context, tokenHash string) error

	// DeleteByUserID 删除某用户的全部会话。
	//
	// 用途：用户被禁用或改密后，应立即踢掉其所有登录态。
	DeleteByUserID(ctx context.Context, userID uint64) error

	// DeleteExpired 清理在 before 之前过期的会话，返回删除条数。
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}
