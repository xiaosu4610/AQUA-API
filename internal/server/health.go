// 本文件实现运维探活与基础信息服务相关处理器。
//
// 意图（Why）：
//
//	健康检查是自动化运维的基础：负载均衡据此摘除故障节点、监控系统据此告警、
//	部署脚本据此判断新版本是否就绪。因此它不仅要回答"进程活着吗"，
//	还要回答"依赖是否可用"——数据库不可用时即便进程在跑也不应接收流量。
//
// 流转（Flow）：
//
//	GET /healthz → handleHealthz → 探测数据库 → 汇总状态 → 返回 JSON
//	GET /        → handleRoot     → 返回服务标识与版本
//
// 扩展（Extend）：
//
//	新增依赖探测（如 Redis）：在 healthResponse 增加字段，
//	并在 handleHealthz 中追加探测逻辑；任一关键依赖失败都应计入 degraded。
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/version"
)

// 健康检查相关常量。
const (
	healthCheckTimeout = 2 * time.Second // 单项依赖探测超时，避免健康检查本身被拖慢
	statusOK           = "ok"            // 全部依赖正常
	statusDegraded     = "degraded"      // 存在不可用依赖
	dependencyOK       = "ok"
	dependencyFailed   = "unavailable" // 刻意不返回具体错误原因，避免向外部泄露内部细节
)

// healthResponse 是健康检查的响应体。
//
// 字段设计说明：既给出机器可判定的 status，也给出可用于人工排查的版本与迁移信息。
// 注意：不包含任何连接串、路径等敏感信息——健康检查端点通常无需鉴权即可访问。
type healthResponse struct {
	Status           string `json:"status"`            // ok / degraded
	Version          string `json:"version"`           // 构建版本
	UptimeSeconds    int64  `json:"uptime_seconds"`    // 已运行秒数
	Database         string `json:"database"`          // ok / unavailable
	MigrationVersion int    `json:"migration_version"` // 当前数据库结构版本
}

// handleHealthz 处理 GET /healthz。
//
// 返回约定：
//   - 200：所有关键依赖可用，可接收流量；
//   - 503：存在不可用依赖，调用方（负载均衡/编排系统）应停止向本实例转发流量。
func (s *Server) handleHealthz(c *gin.Context) {
	// 给探测设置独立超时：健康检查必须"快速失败"，
	// 否则数据库卡住会导致探活请求堆积，反而加剧故障。
	ctx, cancel := context.WithTimeout(c.Request.Context(), healthCheckTimeout)
	defer cancel()

	resp := healthResponse{
		Status:        statusOK,
		Version:       version.Get().Version,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Database:      dependencyOK,
	}

	// 探测数据库连通性
	if err := s.deps.Store.DB().PingContext(ctx); err != nil {
		resp.Status = statusDegraded
		resp.Database = dependencyFailed
		// 不把底层错误写入响应体（可能包含文件路径等内部信息），仅通过状态码与枚举表达
		c.JSON(http.StatusServiceUnavailable, resp)
		return
	}

	// 上报数据库结构版本，便于确认新版本是否已完成迁移
	if v, err := s.deps.Store.LatestMigrationVersion(ctx); err == nil {
		resp.MigrationVersion = v
	}

	c.JSON(http.StatusOK, resp)
}

// serviceInfoResponse 是根路径返回的服务标识信息。
type serviceInfoResponse struct {
	Name      string `json:"name"`       // 服务名
	Version   string `json:"version"`    // 构建版本
	GitCommit string `json:"git_commit"` // 提交哈希，便于确认线上运行的确切代码版本
	BuildTime string `json:"build_time"` // 构建时间
}

// handleRoot 处理 GET /，返回服务标识信息。
//
// 用途：运维或使用者访问根路径时能确认「服务是什么、什么版本」，
// 而不是收到一个含义不明的 404。
func (s *Server) handleRoot(c *gin.Context) {
	info := version.Get()
	c.JSON(http.StatusOK, serviceInfoResponse{
		Name:      "AQUA-API",
		Version:   info.Version,
		GitCommit: info.GitCommit,
		BuildTime: info.BuildTime,
	})
}
