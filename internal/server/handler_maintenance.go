// 本文件实现运维监控与备份导出/校验接口（全部需管理员权限，挂在 /api/admin 下）。
//
// 意图（Why）：
//
//	网关上线后，站长最需要三件事：一是「现在健不健康」（概览），
//	二是「出事了怎么留一份数据」（备份导出），三是「这份备份能不能用」（只读校验）。
//	本文件把它们收在一处，且刻意【不实现在线恢复】——见 handleMaintenanceInspectBackup
//	的注释，那是会导致数据损坏的危险操作，只提供人工操作步骤。
//
// 流转（Flow）：
//
//	GET  /api/admin/maintenance/overview        → handleMaintenanceOverview
//	GET  /api/admin/maintenance/backup          → handleMaintenanceBackup（SQLite 一致性快照下载）
//	POST /api/admin/maintenance/backup/inspect  → handleMaintenanceInspectBackup（只读校验）
//	  └─ 统计口径全部下沉到 internal/store（见 stats_repo.go），本层只做协议转换
//
// 扩展（Extend）：
//
//	新增运维指标：在 overview 的响应结构体加字段，并从 store 的统计方法取数；
//	新增备份能力（如定时导出）：复用 handleMaintenanceBackup 的快照生成逻辑，
//	  但务必保持「总是生成完整快照、不直接拷贝在用文件」这一底线。
package server

import (
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/store"
	"gitee.com/xiaosu4610/aqua-api/internal/version"
)

// 运维接口相关常量。
const (
	// maintenanceBackupMaxBytes 是上传备份文件的体积上限（512 MiB）。
	// 取值说明：自托管网关的 SQLite 库通常远小于此；设上限是为了避免一次上传耗尽内存/磁盘。
	maintenanceBackupMaxBytes = 512 << 20 // 512 MiB
	// maintenanceBackupFormField 是 multipart 表单中备份文件的字段名。
	maintenanceBackupFormField = "backup"
	// maintenanceBackupNameLayout 是下载文件名中的时间格式（形如 20260927-0630）。
	maintenanceBackupNameLayout = "20060102-1504"
)

// ---------------------------------------------------------------------------
// 响应结构
// ---------------------------------------------------------------------------

// maintenanceOverviewResponse 是运维概览的响应体。
type maintenanceOverviewResponse struct {
	Version       string                 `json:"version"`        // 构建版本
	BuildTime     string                 `json:"build_time"`     // 构建时间
	GitCommit     string                 `json:"git_commit"`     // 提交哈希
	StartedAt     int64                  `json:"started_at"`     // 启动时刻（Unix 秒）
	UptimeSeconds int64                  `json:"uptime_seconds"` // 已运行秒数
	Database      maintenanceDatabaseDTO `json:"database"`       // 数据库信息
	Disk          maintenanceDiskDTO     `json:"disk"`           // 磁盘水位
	Tables        []maintenanceTableDTO  `json:"tables"`         // 各表行数
	Usage         maintenanceUsageDTO    `json:"usage"`          // 调用健康度
}

// maintenanceDatabaseDTO 描述数据库驱动与体积。
type maintenanceDatabaseDTO struct {
	Driver        string `json:"driver"`         // 驱动名（sqlite 等）
	SizeBytes     int64  `json:"size_bytes"`     // 数据 объем（字节）；不可用时为 0
	SizeAvailable bool   `json:"size_available"` // 是否成功取到体积（非 SQLite 或取不到时为 false）
}

// maintenanceDiskDTO 描述数据目录所在分区的磁盘水位。
type maintenanceDiskDTO struct {
	Available  bool    `json:"available"`   // 是否成功获取（Windows 开发环境为 false）
	TotalBytes uint64  `json:"total_bytes"` // 分区总容量
	FreeBytes  uint64  `json:"free_bytes"`  // 分区可用空间
	UsedBytes  uint64  `json:"used_bytes"`  // 已使用空间
	UsedRatio  float64 `json:"used_ratio"`  // 使用比例（0~1，前端按百分比展示）
}

// maintenanceTableDTO 是单张表的行数。
type maintenanceTableDTO struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// maintenanceUsageWindowDTO 是单个时间窗口内的调用健康度。
type maintenanceUsageWindowDTO struct {
	Requests     int64   `json:"requests"`       // 调用总次数
	Failures     int64   `json:"failures"`       // 失败次数
	FailureRate  float64 `json:"failure_rate"`   // 失败率（0~1）
	AvgLatencyMS float64 `json:"avg_latency_ms"` // 平均耗时（毫秒）
}

// maintenanceUsageDTO 汇总近 24 小时与近 7 天的调用健康度。
type maintenanceUsageDTO struct {
	Last24h maintenanceUsageWindowDTO `json:"last_24h"`
	Last7d  maintenanceUsageWindowDTO `json:"last_7d"`
}

// maintenanceCompareRowDTO 是「备份行数 vs 当前行数」的对比行。
//
// InBackup 为 false 表示备份中不存在该表（备份行数按 0 展示但语义是「缺失」）。
type maintenanceCompareRowDTO struct {
	Name        string `json:"name"`
	InBackup    bool   `json:"in_backup"`
	BackupRows  int64  `json:"backup_rows"`
	CurrentRows int64  `json:"current_rows"`
}

// maintenanceInspectResponse 是备份只读校验的响应体。
type maintenanceInspectResponse struct {
	Valid                bool                       `json:"valid"`                  // 校验通过恒为 true（不通过会以 400 返回）
	SchemaVersion        int                        `json:"schema_version"`         // 备份的 schema 版本
	SchemaTablePresent   bool                       `json:"schema_table_present"`   // 备份是否含 schema_migrations 表
	CurrentSchemaVersion int                        `json:"current_schema_version"` // 当前运行库的 schema 版本，便于对比
	Tables               []maintenanceCompareRowDTO `json:"tables"`                 // 各表行数对比
	RestoreSteps         []string                   `json:"restore_steps"`          // 人工恢复步骤（刻意不提供在线恢复）
	Note                 string                     `json:"note"`                   // 为什么不提供在线恢复
}

// ---------------------------------------------------------------------------
// 概览
// ---------------------------------------------------------------------------

// handleMaintenanceOverview 返回运维概览。
//
// 取数策略：数据库体积与磁盘水位是「尽力而为」的可选项，取不到就标记为不可用，
// 不让整页失败；而各表行数与调用健康度是核心信息，取数失败直接返回 500。
func (s *Server) handleMaintenanceOverview(c *gin.Context) {
	ctx := c.Request.Context()
	now := time.Now()
	info := version.Get()

	resp := maintenanceOverviewResponse{
		Version:       info.Version,
		BuildTime:     info.BuildTime,
		GitCommit:     info.GitCommit,
		StartedAt:     s.startedAt.Unix(),
		UptimeSeconds: int64(now.Sub(s.startedAt).Seconds()),
		Database:      maintenanceDatabaseDTO{Driver: s.deps.Store.Driver()},
	}

	// 数据库体积：非 SQLite 或取不到时 SizeAvailable=false，字段留空。
	if size, ok, err := s.deps.Store.DatabaseSizeBytes(ctx); err == nil && ok {
		resp.Database.SizeBytes = size
		resp.Database.SizeAvailable = true
	}

	// 磁盘水位：Windows 开发环境不可用（见 stats_disk_*.go），可用性通过字段表达。
	if disk, err := s.deps.Store.DiskUsage(ctx); err == nil && disk.Available {
		used := disk.TotalBytes - disk.FreeBytes
		resp.Disk = maintenanceDiskDTO{
			Available:  true,
			TotalBytes: disk.TotalBytes,
			FreeBytes:  disk.FreeBytes,
			UsedBytes:  used,
		}
		if disk.TotalBytes > 0 {
			resp.Disk.UsedRatio = float64(used) / float64(disk.TotalBytes)
		}
	}

	// 各表行数（核心信息，失败即报错）。
	stats, err := s.deps.Store.TableRowCounts(ctx)
	if err != nil {
		s.respondInternalError(c, "统计各表行数失败")
		return
	}
	resp.Tables = make([]maintenanceTableDTO, 0, len(stats))
	for _, item := range stats {
		resp.Tables = append(resp.Tables, maintenanceTableDTO{Name: item.Name, Rows: item.Rows})
	}

	// 调用健康度（核心信息，失败即报错）。
	health, err := s.deps.Store.UsageHealth(ctx, now)
	if err != nil {
		s.respondInternalError(c, "统计调用健康度失败")
		return
	}
	resp.Usage = maintenanceUsageDTO{
		Last24h: toMaintenanceUsageWindowDTO(health.Last24h),
		Last7d:  toMaintenanceUsageWindowDTO(health.Last7d),
	}

	c.JSON(http.StatusOK, resp)
}

// toMaintenanceUsageWindowDTO 把 store 的窗口统计转为对外 DTO，并计算失败率。
//
// 失败率在「零调用」时约定为 0（而非 NaN），保证前端 formatPercent 得到 0.0% 而不是空值。
func toMaintenanceUsageWindowDTO(window store.UsageWindow) maintenanceUsageWindowDTO {
	dto := maintenanceUsageWindowDTO{
		Requests:     window.Requests,
		Failures:     window.Failures,
		AvgLatencyMS: window.AvgLatencyMS,
	}
	if window.Requests > 0 {
		dto.FailureRate = float64(window.Failures) / float64(window.Requests)
	}
	return dto
}

// ---------------------------------------------------------------------------
// 备份导出
// ---------------------------------------------------------------------------

// handleMaintenanceBackup 生成并下载一份数据库一致性快照。
//
// 关键决策（务必保留）：
//   - 只支持 SQLite：其它数据库应由 DBA 使用各自的备份工具（mysqldump / pg_dump），
//     网关假装支持只会产出不可用的备份；
//   - 用 VACUUM INTO 生成快照，而不是直接拷贝正在写入的 .db 文件：
//     数据库运行在 WAL 模式下，数据可能分散在 -wal/-shm 中，直接拷贝磁盘文件
//     得到的副本可能缺少已提交事务、甚至因页撕裂而损坏，恢复时才暴露问题就晚了。
//     VACUUM INTO 由 SQLite 自身在一致性读快照上重建出一个结构完整的新库文件。
func (s *Server) handleMaintenanceBackup(c *gin.Context) {
	if s.deps.Store.Driver() != store.DriverSQLite {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"当前仅支持对 SQLite 数据库导出备份；其它数据库请使用其官方备份工具（如 mysqldump / pg_dump）。",
			oai.TypeInvalidRequest, "backup_unsupported_driver")
		return
	}

	ctx := c.Request.Context()

	// 1) 在系统临时目录预留唯一文件名（并发导出不会互相覆盖），随后立即删除：
	//    VACUUM INTO 要求目标文件不存在，否则会直接报错。
	tmp, err := os.CreateTemp("", "aqua-backup-*.db")
	if err != nil {
		s.respondInternalError(c, "创建备份临时文件失败")
		return
	}
	tmpPath := tmp.Name()
	if closeErr := tmp.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		s.respondInternalError(c, "创建备份临时文件失败")
		return
	}
	// 无论成功与否都要清理临时文件（下载是流式的，写入响应后再清理）
	defer func() { _ = os.Remove(tmpPath) }()
	if err := os.Remove(tmpPath); err != nil {
		s.respondInternalError(c, "准备备份临时文件失败")
		return
	}

	// 2) 生成一致性快照（原因见本函数顶部注释）。
	if _, err := s.deps.Store.DB().ExecContext(ctx, "VACUUM INTO ?", tmpPath); err != nil {
		s.respondInternalError(c, "生成数据库快照失败")
		return
	}

	// 3) 以 octet-stream 流式下载，文件名带时间戳便于区分多份备份。
	filename := "aqua-backup-" + time.Now().Format(maintenanceBackupNameLayout) + ".db"
	c.Header("Content-Type", "application/octet-stream")
	c.FileAttachment(tmpPath, filename)
}

// ---------------------------------------------------------------------------
// 备份校验（只读）
// ---------------------------------------------------------------------------

// handleMaintenanceInspectBackup 对上传的备份文件做只读校验，并与当前库对比。
//
// 刻意不实现「在线恢复」：
//   - SQLite 的数据库是一个正在被连接池使用的文件；在线替换它会导致已有连接
//     仍指向被删除的旧 inode、WAL/SHM 与新文件错配，轻则数据不一致、重则整库损坏；
//   - 恢复本身是低频且高风险的操作，交给管理员在停机窗口按步骤手工完成更安全、
//     也可留痕。因此本接口只回答「这份备份是否可读、内容大致长什么样」，
//     并把恢复步骤以文案形式返回给前端。
func (s *Server) handleMaintenanceInspectBackup(c *gin.Context) {
	// 限制整体请求体大小：超限时 multipart 解析会直接失败，避免磁盘/内存被打满。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maintenanceBackupMaxBytes)

	fileHeader, err := c.FormFile(maintenanceBackupFormField)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"未收到备份文件：请以 multipart/form-data 上传字段名为 backup 的 .db 文件（单文件不超过 512 MiB）。",
			oai.TypeInvalidRequest, "backup_file_missing")
		return
	}
	if fileHeader.Size > maintenanceBackupMaxBytes {
		oai.WriteError(c.Writer, http.StatusRequestEntityTooLarge,
			"备份文件超过 512 MiB 上限，请改用文件系统级备份或拆分处理。",
			oai.TypeInvalidRequest, "backup_too_large")
		return
	}

	// 落到临时文件后用只读方式打开校验；绝不在当前运行的数据库上执行任何写入。
	tmp, err := os.CreateTemp("", "aqua-inspect-*.db")
	if err != nil {
		s.respondInternalError(c, "创建校验临时文件失败")
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	src, err := fileHeader.Open()
	if err != nil {
		_ = tmp.Close()
		s.respondInternalError(c, "读取上传文件失败")
		return
	}
	if _, err := io.Copy(tmp, src); err != nil {
		_ = src.Close()
		_ = tmp.Close()
		s.respondInternalError(c, "保存上传文件失败")
		return
	}
	// 显式关闭（而非仅 defer）：下一步要打开该文件读取，需确保数据已刷盘。
	if err := src.Close(); err != nil {
		_ = tmp.Close()
		s.respondInternalError(c, "读取上传文件失败")
		return
	}
	if err := tmp.Close(); err != nil {
		s.respondInternalError(c, "保存上传文件失败")
		return
	}

	ctx := c.Request.Context()
	inspection, err := store.InspectSQLiteBackup(ctx, tmpPath)
	if err != nil {
		// 不把底层错误（可能含临时路径）透给客户端，只给可操作的中文提示。
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"上传的文件无法作为 SQLite 数据库打开，请确认它是本系统导出的 .db 备份且未损坏。",
			oai.TypeInvalidRequest, "backup_invalid")
		return
	}

	currentStats, err := s.deps.Store.TableRowCounts(ctx)
	if err != nil {
		s.respondInternalError(c, "统计当前数据库表行数失败")
		return
	}
	currentVersion, err := s.deps.Store.LatestMigrationVersion(ctx)
	if err != nil {
		s.respondInternalError(c, "读取当前数据库版本失败")
		return
	}

	c.JSON(http.StatusOK, maintenanceInspectResponse{
		Valid:                true,
		SchemaVersion:        inspection.SchemaVersion,
		SchemaTablePresent:   inspection.HasSchemaTable,
		CurrentSchemaVersion: currentVersion,
		Tables:               buildMaintenanceCompareRows(inspection.Tables, currentStats),
		RestoreSteps:         maintenanceRestoreSteps(),
		Note: "出于数据安全考虑，本接口只做只读校验，不提供在线恢复：" +
			"正在被服务使用的 SQLite 文件若被在线替换，可能导致连接指向已删除的旧文件、WAL 与新文件错配，进而损坏数据。请按下述步骤在停机窗口手工恢复。",
	})
}

// buildMaintenanceCompareRows 按核心表清单（固定顺序）组装备份与当前库的行数对比。
//
// 用 store.MaintenanceTables() 作为顺序基准，保证同一个备份在多次校验下顺序稳定，
// 也保证「备份缺表」这一事实被显式呈现（InBackup=false）。
func buildMaintenanceCompareRows(backup, current []store.TableStat) []maintenanceCompareRowDTO {
	backupRows := make(map[string]int64, len(backup))
	for _, item := range backup {
		backupRows[item.Name] = item.Rows
	}
	currentRows := make(map[string]int64, len(current))
	for _, item := range current {
		currentRows[item.Name] = item.Rows
	}

	names := store.MaintenanceTables()
	rows := make([]maintenanceCompareRowDTO, 0, len(names))
	for _, name := range names {
		backupRowsValue, inBackup := backupRows[name]
		rows = append(rows, maintenanceCompareRowDTO{
			Name:        name,
			InBackup:    inBackup,
			BackupRows:  backupRowsValue,
			CurrentRows: currentRows[name],
		})
	}
	return rows
}

// maintenanceRestoreSteps 返回人工恢复备份的中文操作步骤。
//
// 之所以返回文案而非执行动作：恢复必须伴随停机与人工确认，
// 由前端展示步骤、管理员照做，避免一键误操作。
func maintenanceRestoreSteps() []string {
	return []string{
		"1. 停止 AQUA-API 服务（如 systemctl stop aqua-api，或结束正在运行的进程），确保没有任何连接在写数据库。",
		"2. 找到当前数据库文件（默认 ./data/aqua.db），将其改名留存，例如 aqua.db.before-restore。",
		"3. 把已校验的备份文件复制到同一目录，并重命名为当前数据库文件名（aqua.db）。",
		"4. 若同目录存在 aqua.db-wal / aqua.db-shm，一并删除（它们属于旧库，会与新文件不匹配）。",
		"5. 重新启动 AQUA-API 服务，进入后台确认数据已恢复；确认无误后再删除改名留存的旧文件。",
	}
}
