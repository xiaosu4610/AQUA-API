// 本文件定义「额度预扣/结算/退还」的领域模型与仓储接口。
//
// 意图（Why）：
//
//	把"额度从什么时候开始被占用"这件事从"响应之后"提前到"请求之前"。
//	升级前额度只在鉴权时判一次、扣费在响应之后，并发请求会同时通过检查再各自后扣，
//	且 user.used_quota 纯自增不封顶，导致"已用 > 总额度"（用户倒欠）。
//	引入「预留台账」后：
//	  预留（Reserve）→ 请求处理 → 结算（Settle）/ 退还（Release）。
//
//	把实体与仓储接口放在领域层（而不是 SQL 里），原因：
//	  1) "一次调用最多被扣多少"是计费语义，必须唯一；
//	  2) 需要被多处复用：鉴权中间件做预留、转发结束后结算、启动时回收陈旧预留。
//
// 流转（Flow）：
//
//	middleware.TokenAuth ── Reserve ──▶ quota_reservations（在途）
//	relay.recordUsage    ── Settle ──▶ 按实际用量多退少补
//	relay.recordUsage    ── Release ─▶ 请求失败时全额退还
//	启动/后台清理        ── CleanupExpired ─▶ 回收超时的在途预留
//
// 扩展（Extend）：
//
//	新增计价维度（缓存价、按张计费）时：只需改 relay.Billing 的估算与结算口径，
//	本文件与仓储层无需变动——它们只关心"预扣了多少、退还多少"。
package model

import (
	"context"
	"errors"
	"time"
)

// ErrQuotaInsufficient 表示可用额度不足以完成本次预留。
//
// 鉴权层据此返回 429；它不是"仓储故障"，而是正常的业务拒绝，必须可被 errors.Is 识别。
var ErrQuotaInsufficient = errors.New("model: 额度不足")

// ErrReservationNotFound 表示未找到指定 request_id 的预留记录。
var ErrReservationNotFound = errors.New("model: 预留记录不存在")

// QuotaUnknown 是"未取得用量"时传给 Settle 的哨兵值。
//
// 语义：结算时按【预留量】收取（既不退还也不补扣）。
// 为什么需要它：上游不返回 usage 时无法算出实际用量，若按 0 结算就等于白送；
// 按预留量收取是保守且可解释的处理方式（日志里会标注"未取得 usage"）。
const QuotaUnknown int64 = -1

// ReservationStatus 表示一条预留记录的生命周期状态。
//
// 取值与数据库 quota_reservations.status 一一对应，禁止改动已落库的数值。
type ReservationStatus int

const (
	// ReservationInFlight 在途：已预扣额度，等待结算或释放。
	ReservationInFlight ReservationStatus = 1
	// ReservationSettled 已结算：已按实际用量多退少补，终态。
	ReservationSettled ReservationStatus = 2
	// ReservationReleased 已释放：已全额退还预扣额度，终态。
	ReservationReleased ReservationStatus = 3
)

// IsTerminal 判断状态是否为终态（结算或释放）。
func (s ReservationStatus) IsTerminal() bool {
	return s == ReservationSettled || s == ReservationReleased
}

// String 返回状态的中文名，便于日志与排查。
func (s ReservationStatus) String() string {
	switch s {
	case ReservationInFlight:
		return "在途"
	case ReservationSettled:
		return "已结算"
	case ReservationReleased:
		return "已释放"
	default:
		return "未知"
	}
}

// QuotaReservation 表示一条额度预留记录。
type QuotaReservation struct {
	ID        uint64            // 主键
	RequestID string            // 幂等键：一次调用的唯一标识
	UserID    uint64            // 归属用户（0 表示无用户）
	TokenID   uint64            // 使用的访问令牌（0 表示未使用令牌）
	Reserved  int64             // 预扣额度
	Settled   int64             // 结算后实际额度（未结算为 0）
	Status    ReservationStatus // 状态
	CreatedAt time.Time         // 预留时间
	ExpiresAt time.Time         // 在途超时兜底时间；零值表示不回收
}

// ReserveRequest 描述一次预留请求。
type ReserveRequest struct {
	RequestID string        // 幂等键，不可为空
	UserID    uint64        // 归属用户；<=0 时只扣令牌不扣用户
	TokenID   uint64        // 使用令牌；<=0 时只扣用户不扣令牌
	Amount    int64         // 预扣额度（>=0）
	TTL       time.Duration // 在途有效期；<=0 时由实现使用默认值
}

// QuotaRepository 定义额度预留台账的持久化操作。
//
// 实现约定（关键，防资损）：
//   - Reserve / Settle / Release 三者都必须幂等：相同 request_id 重复调用不重复加减额度；
//   - 额度扣减必须用【条件更新 + 受影响行数】判定（禁止"先读后写"），
//     这是并发下"不超支"的唯一可靠保证；
//   - Settle / Release 只能作用于"在途"记录，对已终态的调用应安全返回（不报错、不重复处理）。
type QuotaRepository interface {
	// Reserve 预扣额度：插入在途记录并原子扣减用户与令牌额度。
	//
	// request_id 已存在时直接返回既有记录（幂等，不重复扣减）。
	// 可用额度不足时返回 ErrQuotaInsufficient，且不产生任何扣减。
	Reserve(ctx context.Context, req ReserveRequest) (*QuotaReservation, error)

	// Settle 结算：把在途预留改为已结算，并按 reserved−actualQuota 多退少补。
	//
	// actualQuota 为 QuotaUnknown 时按预留量收取；
	// 补扣不足时按可用量扣减（返回记录的 Settled 反映真实入账额度，便于识别缺口）。
	// 重复调用或对非在途记录调用无副作用，返回既有记录。
	Settle(ctx context.Context, requestID string, actualQuota int64) (*QuotaReservation, error)

	// Release 全额退还预留（请求失败时调用）。幂等：只对"在途"生效。
	Release(ctx context.Context, requestID string) error

	// GetByRequestID 按幂等键查询预留记录，不存在时返回 ErrReservationNotFound。
	GetByRequestID(ctx context.Context, requestID string) (*QuotaReservation, error)

	// PendingAmount 返回某用户在途预留的合计额度（用于计算可用额度）。
	PendingAmount(ctx context.Context, userID uint64) (int64, error)

	// CleanupExpired 回收"在途且超过 expires_at"的陈旧预留并退还额度，返回处理条数。
	//
	// 为什么必须要有：进程崩溃（或被杀）会留下永久"在途"的残留，
	// 不回收则这部分额度永远退不回来，用户会以为额度凭空消失。
	CleanupExpired(ctx context.Context, now time.Time) (int, error)
}
