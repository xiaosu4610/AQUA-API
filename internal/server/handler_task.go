// 本文件实现异步任务的对外接口与后台管理接口。
//
// 意图（Why）：
//
//	异步任务有两种使用者，接口也分成两组：
//	  1) 程序（下游客户端）：走 /v1/tasks，用访问令牌鉴权，只能看自己的任务；
//	  2) 人（站长 / 用户）：走 /api 后台与门户，用会话鉴权，可查看与取消。
//	两者共用同一套 DTO 与查询逻辑，避免"接口看到的字段不一致"。
//
// 安全约定：
//
//	任务号（task_ref）是随机 96 位，但归属校验仍必须做——
//	若仅凭任务号就能读结果，一旦任务号通过日志/截图泄露，
//	任何人都能看到他人生成的内容。因此：
//	  - /v1 路径强制 task.UserID == 令牌归属用户，否则一律 404
//	    （用 404 而非 403，避免"任务存在"这一信息被用于枚举）。
//
// 流转（Flow）：
//
//	POST /v1/tasks            → TaskService.Submit → 返回任务 DTO
//	GET  /v1/tasks/{ref}      → TaskService.PollOnce（顺带推进一次）→ DTO
//	GET  /v1/tasks            → 列表（仅自己的）
//	GET  /api/user/tasks      → 门户列表（仅自己的）
//	GET  /api/admin/tasks     → 后台列表（全部，可按用户/类别/状态过滤）
//	POST /api/admin/tasks/{ref}/cancel → 取消并退还额度
//	GET  /api/admin/task-providers     → 已注册的上游适配器（供界面提示）
//
// 扩展（Extend）：
//
//	新增任务参数：无需改本文件（未识别的字段会原样进入 params 交给适配器）。
//	新增任务类别：在 model.TaskKind 加常量即可被本文件自动接受。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
	"gitee.com/xiaosu4610/aqua-api/internal/oai"
	"gitee.com/xiaosu4610/aqua-api/internal/relay"
	"gitee.com/xiaosu4610/aqua-api/internal/server/middleware"
)

// taskRequestReservedKeys 是任务请求体中"由网关自己解释"的字段。
//
// 其余字段会原样放进 params 交给上游适配器——这样新增上游参数
// （size、quality、seed…）完全不需要改网关代码。
var taskRequestReservedKeys = map[string]struct{}{
	"kind":     {},
	"model":    {},
	"prompt":   {},
	"provider": {},
}

// taskDTO 是异步任务的对外表示。
//
// 刻意包含 params 与 result_data：任务类接口的价值之一就是"可复现"，
// 使用者需要看到当初提交了什么、上游返回了什么。
type taskDTO struct {
	TaskRef    string `json:"task_ref"`
	Kind       string `json:"kind"`
	KindText   string `json:"kind_text"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	Params     string `json:"params"`
	Status     int    `json:"status"`
	StatusText string `json:"status_text"`
	Progress   int    `json:"progress"`
	ResultURL  string `json:"result_url"`
	// ResultData 是上游返回的完整结果（JSON 字符串）。
	ResultData string `json:"result_data"`
	Error      string `json:"error"`
	Quota      int64  `json:"quota"`
	UserID     uint64 `json:"user_id"`
	ChannelID  uint64 `json:"channel_id"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	FinishedAt int64  `json:"finished_at"`
}

// toTaskDTO 把领域模型转为对外 DTO。
func toTaskDTO(task *model.Task) taskDTO {
	if task == nil {
		return taskDTO{}
	}
	return taskDTO{
		TaskRef:    task.TaskRef,
		Kind:       string(task.Kind),
		KindText:   taskKindText(task.Kind),
		Provider:   task.Provider,
		Model:      task.Model,
		Prompt:     task.Prompt,
		Params:     task.Params,
		Status:     int(task.Status),
		StatusText: task.Status.String(),
		Progress:   task.Progress,
		ResultURL:  task.ResultURL,
		ResultData: task.ResultData,
		Error:      task.Error,
		Quota:      task.Quota,
		UserID:     task.UserID,
		ChannelID:  task.ChannelID,
		CreatedAt:  unixOrZero(task.CreatedAt),
		UpdatedAt:  unixOrZero(task.UpdatedAt),
		FinishedAt: unixOrZero(task.FinishedAt),
	}
}

// taskKindText 返回任务类别的中文名。
func taskKindText(kind model.TaskKind) string {
	switch kind {
	case model.TaskKindImage:
		return "图像生成"
	case model.TaskKindVideo:
		return "视频生成"
	case model.TaskKindMusic:
		return "音乐生成"
	default:
		return string(kind)
	}
}

// handleSubmitTask 处理 POST /v1/tasks（提交异步任务）。
func (s *Server) handleSubmitTask(c *gin.Context) {
	if s.deps.TaskService == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"异步任务模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	token, ok := middleware.TokenFromContext(c)
	if !ok {
		oai.WriteError(c.Writer, http.StatusUnauthorized,
			"访问令牌无效", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
		return
	}

	body, err := oai.ReadBody(c.Request)
	if err != nil {
		if errors.Is(err, oai.ErrRequestTooLarge) {
			oai.WriteError(c.Writer, http.StatusRequestEntityTooLarge,
				"请求体超过上限", oai.TypeInvalidRequest, oai.CodeRequestTooLarge)
			return
		}
		oai.WriteError(c.Writer, http.StatusBadRequest,
			"读取请求体失败", oai.TypeInvalidRequest, oai.CodeInvalidJSON)
		return
	}

	req, err := parseTaskRequest(body)
	if err != nil {
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "invalid_task")
		return
	}

	task, err := s.deps.TaskService.Submit(c.Request.Context(), token.OwnerID, token.ID, req)
	if err != nil {
		writeTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, toTaskDTO(task))
}

// parseTaskRequest 解析任务提交请求体。
//
// 解析策略：先整体反序列化为 map，取出网关自己解释的字段，
// 剩下的原样保留在 Params 中交给适配器。
func parseTaskRequest(body []byte) (*relay.TaskRequest, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("请求体不是合法的 JSON 对象")
	}

	kindRaw, _ := raw["kind"].(string)
	kind, err := model.TaskKindFromString(kindRaw)
	if err != nil {
		return nil, err
	}

	modelName, _ := raw["model"].(string)
	if strings.TrimSpace(modelName) == "" {
		return nil, errors.New("缺少 model（或动作名，如 imagine / blend）")
	}

	prompt, _ := raw["prompt"].(string)
	provider, _ := raw["provider"].(string)

	params := make(map[string]any, len(raw))
	for key, value := range raw {
		if _, reserved := taskRequestReservedKeys[key]; reserved {
			continue
		}
		params[key] = value
	}

	return &relay.TaskRequest{
		Kind:     kind,
		Model:    strings.TrimSpace(modelName),
		Prompt:   prompt,
		Params:   params,
		Count:    taskCountFromParams(params),
		Provider: strings.TrimSpace(provider),
	}, nil
}

// taskCountFromParams 从参数中读取"份数"。
//
// 兼容 n 与 count 两种写法：图像接口用 n，视频接口多用 count，
// 使用者不该为了走网关而记住我们选了哪个名字。
func taskCountFromParams(params map[string]any) int64 {
	for _, key := range []string{"n", "count"} {
		value, exists := params[key]
		if !exists {
			continue
		}
		switch typed := value.(type) {
		case float64:
			if typed >= 1 {
				return int64(typed)
			}
		case string:
			if parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil && parsed >= 1 {
				return parsed
			}
		}
	}
	return 1
}

// handleGetTask 处理 GET /v1/tasks/{ref}。
//
// 每次查询都会顺带推进一次任务（PollOnce）：这样用户"看到"结果的时间
// 与后台轮询周期无关，体验更接近"实时"。
func (s *Server) handleGetTask(c *gin.Context) {
	if s.deps.TaskService == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"异步任务模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	token, ok := middleware.TokenFromContext(c)
	if !ok {
		oai.WriteError(c.Writer, http.StatusUnauthorized,
			"访问令牌无效", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
		return
	}

	task, err := s.deps.Tasks.GetByRef(c.Request.Context(), c.Param("ref"))
	if err != nil {
		writeTaskLookupError(c, err)
		return
	}
	// 归属校验：不是自己的任务一律当作不存在
	if task.UserID != token.OwnerID {
		writeTaskNotFound(c)
		return
	}

	task, _ = s.deps.TaskService.PollOnce(c.Request.Context(), task)
	c.JSON(http.StatusOK, toTaskDTO(task))
}

// handleListMyTasks 处理 GET /v1/tasks（列出当前令牌所属用户的任务）。
func (s *Server) handleListMyTasks(c *gin.Context) {
	if s.deps.Tasks == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"异步任务模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}
	token, ok := middleware.TokenFromContext(c)
	if !ok {
		oai.WriteError(c.Writer, http.StatusUnauthorized,
			"访问令牌无效", oai.TypeAuthentication, oai.CodeInvalidAPIKey)
		return
	}
	query, page, size := buildTaskQuery(c, token.OwnerID)
	s.respondTaskList(c, query, page, size)
}

// handleMyListTasks 处理 GET /api/user/tasks（用户门户）。
func (s *Server) handleMyListTasks(c *gin.Context) {
	if s.deps.Tasks == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"异步任务模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}
	user, ok := middleware.CurrentUser(c)
	if !ok {
		oai.WriteError(c.Writer, http.StatusUnauthorized,
			"未登录", oai.TypeAuthentication, oai.CodeMissingAPIKey)
		return
	}
	query, page, size := buildTaskQuery(c, user.ID)
	s.respondTaskList(c, query, page, size)
}

// handleAdminListTasks 处理 GET /api/admin/tasks（后台，可看全部）。
func (s *Server) handleAdminListTasks(c *gin.Context) {
	if s.deps.Tasks == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"异步任务模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}
	// user_id 为 0 表示不限用户（管理员视角）
	userID := uint64(0)
	if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			userID = parsed
		}
	}
	query, page, size := buildTaskQuery(c, userID)
	s.respondTaskList(c, query, page, size)
}

// handleAdminCancelTask 处理 POST /api/admin/tasks/{ref}/cancel。
func (s *Server) handleAdminCancelTask(c *gin.Context) {
	if s.deps.TaskService == nil || s.deps.Tasks == nil {
		oai.WriteError(c.Writer, http.StatusServiceUnavailable,
			"异步任务模块未启用", oai.TypeServer, oai.CodeInternal)
		return
	}

	ctx := c.Request.Context()
	task, err := s.deps.Tasks.GetByRef(ctx, c.Param("ref"))
	if err != nil {
		writeTaskLookupError(c, err)
		return
	}

	if err := s.deps.TaskService.Cancel(ctx, task); err != nil {
		if errors.Is(err, model.ErrTaskAlreadyFinished) {
			oai.WriteError(c.Writer, http.StatusConflict,
				"任务已结束，无法取消", oai.TypeInvalidRequest, "task_already_finished")
			return
		}
		s.respondInternalError(c, "取消任务失败")
		return
	}
	c.JSON(http.StatusOK, toTaskDTO(task))
}

// handleTaskProviders 处理 GET /api/admin/task-providers。
//
// 用途：后台"新建任务"表单需要知道"当前支持哪些类别、分别由哪个适配器承接"，
// 否则使用者只能靠猜 adapte 名。
func (s *Server) handleTaskProviders(c *gin.Context) {
	if s.deps.TaskService == nil {
		c.JSON(http.StatusOK, gin.H{"items": []any{}})
		return
	}

	items := make([]gin.H, 0, 4)
	for _, provider := range s.deps.TaskService.Providers() {
		kinds := make([]gin.H, 0, 3)
		for _, kind := range []model.TaskKind{model.TaskKindImage, model.TaskKindVideo, model.TaskKindMusic} {
			if provider.Supports(kind) {
				kinds = append(kinds, gin.H{"kind": string(kind), "text": taskKindText(kind)})
			}
		}
		items = append(items, gin.H{
			"name":  provider.Name(),
			"kinds": kinds,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// respondTaskList 输出任务列表（列表 + 总数）。
func (s *Server) respondTaskList(c *gin.Context, query model.TaskQuery, page, size int) {
	ctx := c.Request.Context()

	total, err := s.deps.Tasks.Count(ctx, query)
	if err != nil {
		s.respondInternalError(c, "统计任务总数失败")
		return
	}

	tasks, err := s.deps.Tasks.List(ctx, query)
	if err != nil {
		s.respondInternalError(c, "查询任务列表失败")
		return
	}

	items := make([]taskDTO, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, toTaskDTO(task))
	}
	c.JSON(http.StatusOK, newPagedResponse(items, int(total), page, size))
}

// buildTaskQuery 从查询参数构造任务查询条件。
//
// 分页沿用全局约定（page/size），与令牌、日志等列表页保持一致，
// 避免同一套后台里出现两种分页参数。
func buildTaskQuery(c *gin.Context, userID uint64) (model.TaskQuery, int, int) {
	page, size, offset := parsePagination(c)
	query := model.TaskQuery{
		UserID: userID,
		Limit:  size,
		Offset: offset,
	}

	if kind, err := model.TaskKindFromString(c.Query("kind")); err == nil {
		query.Kind = kind
	}
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			status := model.TaskStatus(parsed)
			query.Status = &status
		}
	}
	return query, page, size
}

// writeTaskError 把任务服务返回的错误翻译为 HTTP 响应。
func writeTaskError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, relay.ErrTaskProviderNotFound),
		errors.Is(err, relay.ErrTaskProviderUnknown):
		oai.WriteError(c.Writer, http.StatusBadRequest, err.Error(),
			oai.TypeInvalidRequest, "task_provider_unavailable")
	case errors.Is(err, relay.ErrTaskUnavailable),
		errors.Is(err, relay.ErrNoAvailableChannel):
		// 与对话链路保持一致：无可用渠道用 503（暂时性），而非 500
		oai.WriteError(c.Writer, http.StatusServiceUnavailable, err.Error(),
			oai.TypeServer, oai.CodeNoAvailableChannel)
	default:
		oai.WriteError(c.Writer, http.StatusBadGateway, err.Error(),
			oai.TypeServer, oai.CodeUpstreamRequestFailed)
	}
}

// writeTaskLookupError 处理"按任务号查询失败"的统一响应。
func writeTaskLookupError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrTaskNotFound) {
		writeTaskNotFound(c)
		return
	}
	oai.WriteError(c.Writer, http.StatusInternalServerError,
		"网关内部错误", oai.TypeServer, oai.CodeInternal)
}

// writeTaskNotFound 统一返回 404，不区分"不存在"与"不属于你"。
func writeTaskNotFound(c *gin.Context) {
	oai.WriteError(c.Writer, http.StatusNotFound,
		"任务不存在", oai.TypeInvalidRequest, "task_not_found")
}
