// Package reqctx 在标准库的 request context 中传递「本次请求的身份信息」。
//
// 意图（Why）：
//
//	转发引擎（internal/relay）需要在请求结束后写调用日志，而调用者身份
//	（哪个用户、哪把令牌）是在 HTTP 层的鉴权中间件里确定的。
//	若为此让 relay 依赖 gin 上下文，会让核心域耦合到 Web 框架；
//	因此改用标准库 context 传递，保持 relay 只需依赖 net/http。
//
// 流转（Flow）：
//
//	middleware.TokenAuth 鉴权成功
//	  └─ reqctx.WithIdentity(c.Request.Context(), ...) 写回 c.Request
//	       └─ relay 转发结束时 reqctx.IdentityFrom(req.Context()) 取出并落日志
//
// 扩展（Extend）：
//
//	需要传递更多请求级信息（如客户端 IP、请求 ID）时，在本结构体加字段——
//	注意只放"纯数据"，不要把仓储或客户端等依赖塞进来（那会造成隐式耦合）。
package reqctx

import "context"

// Identity 描述一次模型调用请求的调用者身份。
//
// 字段全部使用零值安全的类型：未认证请求（如未启用鉴权的场景）取到零值，
// 日志中表现为 user_id=0、token_id=0，语义清晰。
type Identity struct {
	UserID  uint64 // 调用者用户 ID（0 表示未认证或系统调用）
	TokenID uint64 // 使用的访问令牌 ID（0 表示未使用令牌）

	// RequestID 是本次调用的幂等键，由鉴权中间件在【做了额度预留】时生成。
	//
	// 为什么放在这里：额度预留在 HTTP 层（鉴权中间件）发生，而结算发生在转发引擎
	// （internal/relay）里，两者只能通过 request context 传递这个键。
	// 为空表示本次调用【未做预留】（未定价模型 / 信任额度旁路 / 未启用），
	// 转发结束后据此跳过结算，避免无谓地查库。
	RequestID string // 幂等键（空 = 未预留）
}

// ctxKey 是本包专属的 context 键类型。
//
// 使用自定义类型而非字符串：避免与其他包的键冲突
// （字符串键一旦重名会互相覆盖，且不会有任何编译期提示）。
type ctxKey struct{}

// identityKey 是身份信息在 context 中的键。
var identityKey = ctxKey{}

// WithIdentity 返回携带调用者身份的 context。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// IdentityFrom 取出调用者身份；第二个返回值为 false 表示未写入过身份。
func IdentityFrom(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(identityKey).(Identity)
	return id, ok
}
