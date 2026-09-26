// 本文件实现「站点公告」的公开读取接口与后台管理接口。
//
// 意图（Why）：
//
//	公告是站点与用户之间的单向通知渠道：维护窗口、活动上线、计费调整、故障说明。
//	本文件把这条链路暴露出来，并守住两个底线：
//	  1) 公开端只返回"当前应当可见"的公告——时间窗口与启用状态由仓储层
//	     的 ListActive 判定，接口不额外放宽条件，避免把草稿/过期内容泄露给用户；
//	  2) 管理端必须能看到全部公告（含停用与已过期），否则管理员找不到自己的
//	     草稿或历史公告，会误以为数据丢失。
//
// 流转（Flow）：
//
//	公开：AnnouncementBanner → GET /api/announcements → ListActive(now)
//	后台：AnnouncementsView → GET/POST/PUT/DELETE /api/admin/announcements[...] → Repository
//
// 扩展（Extend）：
//
//	新增字段（如"关联跳转链接"）时：model 加字段 + 迁移加列 + 仓储列清单 +
//	本文件 DTO 与请求体四处同步。
//	新增读接口（如"按 ID 取单条"）时，在 router.go 的对应分组注册。
package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
)

// publicAnnouncementDTO 是公开端的公告表示。
//
// 只下发"用户需要看到"的字段：标题、正文、语气、是否置顶与时间。
// 刻意不包含 enabled —— 公开端返回的必然都是生效公告，透出该字段只会误导前端
// 再判一次；也不包含内部时间戳 update_at，减少无意义的契约面。
type publicAnnouncementDTO struct {
	ID        uint64 `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Level     string `json:"level"`
	Pinned    bool   `json:"pinned"`
	PublishAt int64  `json:"publish_at"`
	ExpireAt  int64  `json:"expire_at"`
	CreatedAt int64  `json:"created_at"`
}

// announcementDTO 是后台的公告表示（比公开端多出启用状态与更新时间）。
type announcementDTO struct {
	ID        uint64 `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Level     string `json:"level"`
	LevelText string `json:"level_text"`
	Pinned    bool   `json:"pinned"`
	Enabled   bool   `json:"enabled"`
	PublishAt int64  `json:"publish_at"`
	ExpireAt  int64  `json:"expire_at"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// toPublicAnnouncementDTO 把领域模型转为公开端 DTO。
func toPublicAnnouncementDTO(item *model.Announcement) publicAnnouncementDTO {
	if item == nil {
		return publicAnnouncementDTO{}
	}
	return publicAnnouncementDTO{
		ID:        item.ID,
		Title:     item.Title,
		Content:   item.Content,
		Level:     string(item.Level),
		Pinned:    item.Pinned,
		PublishAt: unixOrZero(item.PublishAt),
		ExpireAt:  unixOrZero(item.ExpireAt),
		CreatedAt: unixOrZero(item.CreatedAt),
	}
}

// toAnnouncementDTO 把领域模型转为后台 DTO。
func toAnnouncementDTO(item *model.Announcement) announcementDTO {
	if item == nil {
		return announcementDTO{}
	}
	return announcementDTO{
		ID:        item.ID,
		Title:     item.Title,
		Content:   item.Content,
		Level:     string(item.Level),
		LevelText: item.Level.String(),
		Pinned:    item.Pinned,
		Enabled:   item.Enabled,
		PublishAt: unixOrZero(item.PublishAt),
		ExpireAt:  unixOrZero(item.ExpireAt),
		CreatedAt: unixOrZero(item.CreatedAt),
		UpdatedAt: unixOrZero(item.UpdatedAt),
	}
}

// ---------------------------------------------------------------------------
// 公开端：读取生效公告
// ---------------------------------------------------------------------------

// handlePublicListAnnouncements 处理 GET /api/announcements（无需登录）。
//
// 返回 { items: [...] }：前台横幅会遍历该数组并按 level 上色。
// 统一包一层 items 而不是直接返回数组，是为了给将来"服务端裁剪/分页"留出扩展位。
func (s *Server) handlePublicListAnnouncements(c *gin.Context) {
	if s.deps.Announcements == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"公告模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	items, err := s.deps.Announcements.ListActive(
		c.Request.Context(), time.Now(), model.AnnouncementActiveMaxLimit)
	if err != nil {
		s.respondInternalError(c, "查询公告失败")
		return
	}

	dtos := make([]publicAnnouncementDTO, 0, len(items))
	for _, item := range items {
		dtos = append(dtos, toPublicAnnouncementDTO(item))
	}
	c.JSON(http.StatusOK, gin.H{"items": dtos})
}

// ---------------------------------------------------------------------------
// 管理端：公告维护
// ---------------------------------------------------------------------------

// handleAdminListAnnouncements 处理 GET /api/admin/announcements（分页 + 筛选）。
//
// 与公开端的关键差异：这里【不】过滤时间窗口，停用与已过期的公告同样返回，
// 否则管理员会找不到草稿与历史公告。
func (s *Server) handleAdminListAnnouncements(c *gin.Context) {
	if s.deps.Announcements == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"公告模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	query := model.AnnouncementQuery{
		Keyword: strings.TrimSpace(c.Query("keyword")),
	}
	if raw := strings.TrimSpace(c.Query("enabled")); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			query.Enabled = &parsed
		}
	}
	if level := strings.TrimSpace(c.Query("level")); level != "" {
		query.Level = model.NormalizeAnnouncementLevel(level)
	}

	page, size, offset := parsePagination(c)
	query.Limit = size
	query.Offset = offset

	items, total, err := s.deps.Announcements.List(c.Request.Context(), query)
	if err != nil {
		s.respondInternalError(c, "查询公告列表失败")
		return
	}

	dtos := make([]announcementDTO, 0, len(items))
	for _, item := range items {
		dtos = append(dtos, toAnnouncementDTO(item))
	}
	c.JSON(http.StatusOK, newPagedResponse(dtos, total, page, size))
}

// announcementRequest 是新建/更新公告的请求体。
//
// 全部字段用指针：更新接口据此区分"未提供"与"提供了零值"——
// 只改标题时不应把置顶/启用意外改回默认值。
type announcementRequest struct {
	Title     *string `json:"title"`
	Content   *string `json:"content"`
	Level     *string `json:"level"`
	Pinned    *bool   `json:"pinned"`
	Enabled   *bool   `json:"enabled"`
	PublishAt *int64  `json:"publish_at"`
	ExpireAt  *int64  `json:"expire_at"`
}

// handleAdminCreateAnnouncement 处理 POST /api/admin/announcements。
func (s *Server) handleAdminCreateAnnouncement(c *gin.Context) {
	if s.deps.Announcements == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"公告模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	var req announcementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	if req.Title == nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "公告标题不能为空",
			oai.TypeInvalidRequest, "missing_title")
		return
	}

	item := &model.Announcement{
		Title: *req.Title,
		// 新建时默认发布、默认不置顶：绝大多数公告创建即生效，
		// 想先存草稿的管理员显式传 enabled=false 即可。
		Enabled: true,
		Level:   model.AnnouncementLevelInfo,
	}
	if req.Content != nil {
		item.Content = *req.Content
	}
	if req.Level != nil {
		item.Level = model.NormalizeAnnouncementLevel(*req.Level)
	}
	if req.Pinned != nil {
		item.Pinned = *req.Pinned
	}
	if req.Enabled != nil {
		item.Enabled = *req.Enabled
	}
	if req.PublishAt != nil {
		item.PublishAt = announcementTimeFromUnix(*req.PublishAt)
	}
	if req.ExpireAt != nil {
		item.ExpireAt = announcementTimeFromUnix(*req.ExpireAt)
	}

	if err := item.Validate(); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_announcement")
		return
	}

	if err := s.deps.Announcements.Create(c.Request.Context(), item); err != nil {
		// 校验已在上面做过，到这里基本只可能是数据库故障；
		// 为稳妥起见仍按"非法内容"处理校验类错误，其余按内部错误。
		if isAnnouncementValidationError(err) {
			oai.WriteError(c.Writer, http.StatusBadRequest, "公告内容不合法",
				oai.TypeInvalidRequest, "invalid_announcement")
			return
		}
		s.respondInternalError(c, "保存公告失败")
		return
	}

	c.JSON(http.StatusOK, toAnnouncementDTO(item))
}

// handleAdminUpdateAnnouncement 处理 PUT /api/admin/announcements/{id}。
//
// 采用"读回 → 覆盖提供的字段 → 整体更新"：这样既能支持只改一个字段，
// 也保证时间窗口的自洽性校验（过期需晚于发布）在合并后的完整数据上生效，
// 而不是分别校验两个孤立的值（它们单独看都可能合法）。
func (s *Server) handleAdminUpdateAnnouncement(c *gin.Context) {
	if s.deps.Announcements == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"公告模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}

	var req announcementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "请求体格式错误",
			oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}
	if req.Title == nil && req.Content == nil && req.Level == nil && req.Pinned == nil &&
		req.Enabled == nil && req.PublishAt == nil && req.ExpireAt == nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, "未提供任何可更新字段",
			oai.TypeInvalidRequest, "empty_update")
		return
	}

	ctx := c.Request.Context()
	item, err := s.deps.Announcements.GetByID(ctx, id)
	if err != nil {
		writeAnnouncementError(c, err)
		return
	}

	if req.Title != nil {
		item.Title = *req.Title
	}
	if req.Content != nil {
		item.Content = *req.Content
	}
	if req.Level != nil {
		item.Level = model.NormalizeAnnouncementLevel(*req.Level)
	}
	if req.Pinned != nil {
		item.Pinned = *req.Pinned
	}
	if req.Enabled != nil {
		item.Enabled = *req.Enabled
	}
	if req.PublishAt != nil {
		item.PublishAt = announcementTimeFromUnix(*req.PublishAt)
	}
	if req.ExpireAt != nil {
		item.ExpireAt = announcementTimeFromUnix(*req.ExpireAt)
	}

	if err := item.Validate(); err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_announcement")
		return
	}

	if err := s.deps.Announcements.Update(ctx, item); err != nil {
		writeAnnouncementError(c, err)
		return
	}

	// Update 只写库不刷新时间戳，这里重新读回以便响应里带上最新的 updated_at
	if refreshed, err := s.deps.Announcements.GetByID(ctx, id); err == nil {
		item = refreshed
	}
	c.JSON(http.StatusOK, toAnnouncementDTO(item))
}

// handleAdminDeleteAnnouncement 处理 DELETE /api/admin/announcements/{id}。
func (s *Server) handleAdminDeleteAnnouncement(c *gin.Context) {
	if s.deps.Announcements == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"公告模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	if err := s.deps.Announcements.Delete(c.Request.Context(), id); err != nil {
		writeAnnouncementError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// writeAnnouncementError 处理后台操作公告时的错误响应：不存在 → 404，其余 → 500。
func writeAnnouncementError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrAnnouncementNotFound) {
		oai.WriteError(c.Writer, http.StatusNotFound, "公告不存在",
			oai.TypeInvalidRequest, "announcement_not_found")
		return
	}
	oai.WriteError(c.Writer, http.StatusInternalServerError,
		"网关内部错误", oai.TypeServer, oai.CodeInternal)
}

// announcementTimeFromUnix 把接口层的 Unix 秒转为时间；<=0 返回零值时间（表示不限制）。
//
// 与 unixOrZero 互为逆运算，二者共同维持"接口层用 0 表示未设置"的约定。
func announcementTimeFromUnix(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// isAnnouncementValidationError 判断仓储层返回的错误是否为内容校验失败。
//
// 仓储在写入前会再跑一次 Validate（防止绕过接口的写入），校验失败时用
// fmt.Errorf 包装了 model 层的中文错误；由于没有独立哨兵错误，这里以
// 前缀 "store: 公告非法" 识别。仅为把这类错误从 500 纠正为 400，
// 不做更细的语义区分。
func isAnnouncementValidationError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "公告非法")
}
