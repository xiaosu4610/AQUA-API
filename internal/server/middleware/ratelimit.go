// 本文件实现一个轻量的进程内频率限制器，主要用于登录与注册接口。
//
// 意图（Why）：
//
//	口令校验（bcrypt）是刻意昂贵的操作，若不限制频率，攻击者可以用少量并发
//	请求打满 CPU——我们的生产实例只有 2 核且与转发服务共享 CPU，
//	一旦被占满，整个网关的转发能力都会受影响。
//	同时频率限制也能显著抬高口令爆破的成本。
//
// 设计取舍：
//   - 采用【进程内】滑动窗口，不引入 Redis：登录是低频操作，单实例足够；
//     且避免为一个小功能引入外部依赖（Redis 留待多实例部署时再引入）。
//   - 按"客户端 IP"计数。生产环境位于 nginx 之后，需由 nginx 设置
//     X-Real-IP / X-Forwarded-For，本中间件优先读取它们。
//
// 流转（Flow）：
//
//	router 注册登录接口 → RateLimiter.Middleware() → Allow(ip) 判定 → 放行或 429
//
// 扩展（Extend）：
//
//	多实例部署时：把 Allow 的实现替换为基于 Redis 的计数即可，
//	  中间件本身无需改动。
package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// RateLimiter 是按 key（通常是客户端 IP）计数的滑动窗口限流器，并发安全。
type RateLimiter struct {
	limit  int           // 窗口内允许的最大次数
	window time.Duration // 时间窗口长度

	mu          sync.Mutex
	hits        map[string][]time.Time // key → 窗口内的命中时间点
	lastCleanup time.Time              // 上次清理时间，用于惰性清理
}

// cleanupInterval 是惰性清理的间隔。
//
// 为什么需要清理：被限流的 key 只增不减会让内存无限增长
// （例如扫描器用大量不同 IP 探测）。定期清理掉窗口内无记录的 key 即可。
const cleanupInterval = 10 * time.Minute

// NewRateLimiter 创建限流器。
//
// 参数 limit 为窗口内允许的次数；window 为窗口长度。
// 若 limit <= 0 或 window <= 0，则退化为"不限流"（Allow 恒为 true），
// 便于在测试或明确不需要限流的场景下关闭。
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:       limit,
		window:      window,
		hits:        make(map[string][]time.Time),
		lastCleanup: time.Now(),
	}
}

// Allow 判断 key 在当前窗口内是否还有配额；返回 false 表示应拒绝。
func (l *RateLimiter) Allow(key string) bool {
	if l == nil || l.limit <= 0 || l.window <= 0 {
		return true
	}
	if key == "" {
		// 无法识别来源时放行：宁可少限流，也不要把正常用户误伤为同一来源。
		return true
	}

	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupIfNeeded(now, cutoff)

	recent := l.hits[key]
	// 丢弃窗口外的记录（切片按时间递增，从头开始丢即可）
	idx := 0
	for idx < len(recent) && recent[idx].Before(cutoff) {
		idx++
	}
	recent = recent[idx:]

	if len(recent) >= l.limit {
		// 注意：此处仍需把裁剪后的切片写回，否则过期记录会一直残留
		l.hits[key] = recent
		return false
	}

	l.hits[key] = append(recent, now)
	return true
}

// cleanupIfNeeded 惰性清理空桶，避免内存随访问来源数量无限增长。
//
// 说明：调用方必须已持有 l.mu。
func (l *RateLimiter) cleanupIfNeeded(now, cutoff time.Time) {
	if now.Sub(l.lastCleanup) < cleanupInterval {
		return
	}
	for key, times := range l.hits {
		if len(times) == 0 || times[len(times)-1].Before(cutoff) {
			delete(l.hits, key)
		}
	}
	l.lastCleanup = now
}

// Middleware 返回限流中间件。
//
// 参数 keyFunc 决定按什么维度计数；通常传入 ClientIP。
func (l *RateLimiter) Middleware(keyFunc func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !l.Allow(keyFunc(c)) {
			abortWithError(c, http.StatusTooManyRequests,
				"请求过于频繁，请稍后再试", oai.TypeRateLimit, "too_many_requests")
			return
		}
		c.Next()
	}
}

// ClientIP 提取客户端真实 IP。
//
// 取值顺序（重要，关系到限流是否有效）：
//  1. X-Real-IP：由我们的 nginx 显式设置，最可信；
//  2. X-Forwarded-For 的第一个地址：多级代理场景，取最左侧（即最初的客户端）；
//  3. gin 的 ClientIP()：直连场景或未配置代理头时的兜底。
//
// 安全提示：这些头可被客户端伪造。生产环境必须由 nginx 用 proxy_set_header
// 覆盖写入（而不是追加），否则攻击者可通过伪造头绕过限流。
func ClientIP(c *gin.Context) string {
	if ip := strings.TrimSpace(c.GetHeader("X-Real-IP")); ip != "" {
		return ip
	}
	if forwarded := c.GetHeader("X-Forwarded-For"); forwarded != "" {
		if first, _, found := strings.Cut(forwarded, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}
	return NormalizeIP(c.ClientIP())
}

// NormalizeIP 规整 IP 字符串。
//
// 必要性：IPv4 与 IPv6 的表示形式可能不同（如 ::ffff:1.2.3.4 与 1.2.3.4），
// 若不做归一，同一客户端会被当作两个来源，限流形同虚设。
func NormalizeIP(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return raw
	}
	// 统一转为 IPv4 表示（若本就是 IPv4 或 IPv4-mapped IPv6）
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
