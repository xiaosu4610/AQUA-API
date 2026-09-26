// 本文件实现内置的异步任务上游适配器。
//
// 意图（Why）：
//
//	把"不同上游的提交/查询协议差异"全部收敛在这一层。编排逻辑
//	（选渠道、计费、落库、退还、轮询，见 async.go）只写一遍，
//	因此每接入一个新上游，代价只是在本文件加一个结构体。
//
// 已内置两类（覆盖开源网关最常见的两种异步上游形态）：
//
//	1) midjourney：典型的"提交 → 轮询"两步异步接口，协议为业界通行的
//	   /mj/submit/{action} 与 /mj/task/{id}/fetch。它同时代表了一整类
//	   第三方任务代理服务（绘画、视频、音乐代理多沿用此形态）。
//	2) openai_image：OpenAI 兼容的图像生成接口（/v1/images/generations）。
//	   它是同步接口，但用户仍然希望"拿一个任务号再取结果"（因为图像生成
//	   动辄十几秒，同步等待容易触发客户端超时），因此这里把它包装成
//	   提交即完成的任务——客户端只需查一次即可拿到结果。
//
// 设计原则：适配器只负责"协议翻译"，不碰计费、不碰数据库、不碰路由。
//
// 流转（Flow）：
//
//	TaskService.Submit → provider.Submit → 上游
//	TaskService.PollOnce → provider.Poll → 上游
//
// 扩展（Extend）：
//
//	新增上游：实现 TaskProvider 三个方法，并在 newBuiltinProviders 中注册。
//	注意两点：
//	  - Poll 返回的 Error 必须脱敏（不要带上游地址与密钥）；
//	  - 状态映射要保守：无法识别的状态一律当作"进行中"，
//	    绝不因为新增了一个上游状态就把任务误判为失败。
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 上游响应体的读取上限。
//
// 取 4 MiB：任务接口的响应通常只有几 KB；个别上游会把"提示词 + 结果列表"
// 一起返回，留出余量即可，同时防止异常上游用超大响应撑爆内存。
const maxTaskResponseBytes = 4 << 20

// 适配器名常量。
//
// 集中定义：这些名字会写入 tasks.provider，一旦改动就会与历史数据脱节，
// 因此必须是唯一事实来源，不能在多处手写字符串。
const (
	// ProviderMidjourney 是 Midjourney 代理形态的异步适配器。
	ProviderMidjourney = "midjourney"
	// ProviderOpenAIImage 是 OpenAI 兼容图像接口的适配器。
	ProviderOpenAIImage = "openai_image"
)

// newBuiltinProviders 返回全部内置适配器。
//
// 切片顺序即"自动选择"的优先级：同一类别有多个适配器时取靠前者。
// 把 Midjourney 排在前面，是因为它是真正的异步接口，更适合任务语义；
// openai_image 属于兼容性兜底。
func newBuiltinProviders(client *http.Client) []TaskProvider {
	if client == nil {
		client = http.DefaultClient
	}
	return []TaskProvider{
		&midjourneyProvider{client: client},
		&openAIImageProvider{client: client},
	}
}

// ── 适配器 1：Midjourney 代理（提交 → 轮询）────────────────────────

// midjourneyProvider 对接 /mj/submit/{action} + /mj/task/{id}/fetch 形态的上游。
type midjourneyProvider struct {
	client *http.Client
}

// Name 返回适配器名。
func (p *midjourneyProvider) Name() string { return ProviderMidjourney }

// Supports 声明支持的任务类别。
//
// 同时声明 image 与 video：这类代理服务的动作集合里既有绘图动作
// （imagine / blend / upscale）也有视频动作，具体由 action 参数决定，
// 因此不该在类别层面把它限制成只能处理图像。
func (p *midjourneyProvider) Supports(kind model.TaskKind) bool {
	return kind == model.TaskKindImage || kind == model.TaskKindVideo
}

// submitPathFor 返回提交动作对应的路径。
func submitPathFor(action string) string {
	action = strings.TrimSpace(action)
	if action == "" {
		action = "imagine"
	}
	// 只取最后一段并过滤非法字符，避免把调用方传入的 action 直接拼进 URL
	// 造成路径穿越（如 action="../v1/chat/completions"）。
	action = strings.TrimPrefix(action, "/")
	if idx := strings.LastIndexAny(action, "/\\"); idx >= 0 {
		action = action[idx+1:]
	}
	if action == "" {
		action = "imagine"
	}
	return "/mj/submit/" + action
}

// Submit 提交任务。
func (p *midjourneyProvider) Submit(ctx context.Context, req *TaskRequest, ch *model.Channel, apiKey string) (*TaskSubmitResult, error) {
	action, _ := req.Params["action"].(string)

	payload := make(map[string]any, len(req.Params)+1)
	for key, value := range req.Params {
		// action 已经体现在路径里，不再放进请求体
		if key == "action" {
			continue
		}
		payload[key] = value
	}
	payload["prompt"] = req.Prompt

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("构造请求体失败: %w", err)
	}

	url := strings.TrimRight(ch.BaseURL, "/") + submitPathFor(action)
	upReq, err := newTaskRequest(ctx, http.MethodPost, url, body, apiKey)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTaskResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}

	// 响应形态：{"code":1,"description":"","result":"<taskId>"}
	var parsed struct {
		Code        int             `json:"code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON")
	}
	if parsed.Code != 1 {
		return nil, fmt.Errorf("上游拒绝任务（code=%d）：%s", parsed.Code, truncateBytes(parsed.Description, 200))
	}

	upstreamID := decodeTaskID(parsed.Result)
	if upstreamID == "" {
		return nil, fmt.Errorf("上游未返回任务号")
	}

	return &TaskSubmitResult{
		UpstreamID: upstreamID,
		Status:     model.TaskStatusQueued,
	}, nil
}

// Poll 查询任务状态。
func (p *midjourneyProvider) Poll(ctx context.Context, task *model.Task, ch *model.Channel, apiKey string) (*TaskPollResult, error) {
	url := strings.TrimRight(ch.BaseURL, "/") + "/mj/task/" + task.UpstreamID + "/fetch"
	upReq, err := newTaskRequest(ctx, http.MethodGet, url, nil, apiKey)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		return nil, fmt.Errorf("查询上游任务失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTaskResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Status     string   `json:"status"`
		Progress   string   `json:"progress"`
		ImageURL   string   `json:"imageUrl"`
		ImageURLs  []string `json:"imageUrls"`
		FailReason string   `json:"failReason"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON")
	}

	status := mapMidjourneyStatus(parsed.Status)

	// 结果地址：优先单张，其次取列表首项
	resultURL := strings.TrimSpace(parsed.ImageURL)
	if resultURL == "" && len(parsed.ImageURLs) > 0 {
		resultURL = strings.TrimSpace(parsed.ImageURLs[0])
	}

	result := &TaskPollResult{
		Status:     status,
		Progress:   parsePercent(parsed.Progress),
		ResultURL:  resultURL,
		ResultData: string(raw),
	}
	if status == model.TaskStatusFailed {
		reason := strings.TrimSpace(parsed.FailReason)
		if reason == "" {
			reason = "上游任务失败"
		}
		result.Error = truncateBytes(reason, 500)
	}
	if status == model.TaskStatusSucceeded && result.Progress == 0 {
		result.Progress = 100
	}
	return result, nil
}

// mapMidjourneyStatus 把上游状态字符串映射为本地状态。
//
// 未知状态一律映射为"进行中"：这样新增的上游状态不会导致任务被误判失败
// （误判失败会立刻退还额度又给了结果，属于"白送"，是最不该出现的错误方向）。
func mapMidjourneyStatus(raw string) model.TaskStatus {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "SUCCESS":
		return model.TaskStatusSucceeded
	case "FAILURE", "FAILED":
		return model.TaskStatusFailed
	case "CANCEL", "CANCELED", "CANCELLED":
		return model.TaskStatusCanceled
	case "IN_PROGRESS", "SUBMITTING":
		return model.TaskStatusRunning
	case "SUBMITTED", "NOT_START", "QUEUED", "WAITING", "MODAL", "ACTION_REQUIRED":
		return model.TaskStatusQueued
	default:
		return model.TaskStatusRunning
	}
}

// parsePercent 解析形如 "50%" 的进度字符串。
func parsePercent(raw string) int {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// decodeTaskID 从上游返回的 result 字段解析任务号。
//
// 为什么容错两种形态：不同代理实现的 result 有时代码契约是字符串
// （"1690000000000000000"），有时是对象（{"id":"..."}）。
// 直接按一种解析会在换上游时突然失效，排查成本很高。
func decodeTaskID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}

	var asObject struct {
		ID      string `json:"id"`
		TaskID  string `json:"taskId"`
		TaskRef string `json:"task_ref"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		switch {
		case strings.TrimSpace(asObject.ID) != "":
			return strings.TrimSpace(asObject.ID)
		case strings.TrimSpace(asObject.TaskID) != "":
			return strings.TrimSpace(asObject.TaskID)
		case strings.TrimSpace(asObject.TaskRef) != "":
			return strings.TrimSpace(asObject.TaskRef)
		}
	}
	return ""
}

// ── 适配器 2：OpenAI 兼容图像生成（同步接口包装为任务）──────────────

// imagesGenerationsPath 是 OpenAI 兼容的图像生成端点。
const imagesGenerationsPath = "/v1/images/generations"

// openAIImageProvider 对接 OpenAI 兼容的 /v1/images/generations。
type openAIImageProvider struct {
	client *http.Client
}

// Name 返回适配器名。
func (p *openAIImageProvider) Name() string { return ProviderOpenAIImage }

// Supports 声明支持的任务类别（仅图像）。
func (p *openAIImageProvider) Supports(kind model.TaskKind) bool {
	return kind == model.TaskKindImage
}

// Submit 调用同步图像接口，并直接返回"已完成"的任务。
//
// 为什么要包装成任务：图像生成通常 10~60 秒，很多客户端/网关的
// 超时阈值低于此值。包装成任务后客户端可以"提交 → 稍后取结果"，
// 体验与异步上游完全一致，客户端无需为不同上游写两套逻辑。
func (p *openAIImageProvider) Submit(ctx context.Context, req *TaskRequest, ch *model.Channel, apiKey string) (*TaskSubmitResult, error) {
	payload := make(map[string]any, len(req.Params)+2)
	for key, value := range req.Params {
		// action 是 Midjourney 形态的专有参数，交给 OpenAI 兼容接口会被拒
		if key == "action" {
			continue
		}
		payload[key] = value
	}
	payload["model"] = req.Model
	payload["prompt"] = req.Prompt

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("构造请求体失败: %w", err)
	}

	url := strings.TrimRight(ch.BaseURL, "/") + imagesGenerationsPath
	upReq, err := newTaskRequest(ctx, http.MethodPost, url, body, apiKey)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTaskResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("上游返回 HTTP %d：%s", resp.StatusCode, truncateBytes(extractErrorMessage(raw), 200))
	}

	// 响应形态：{"created":...,"data":[{"url":"..."} 或 {"b64_json":"..."}]}
	var parsed struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON")
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("上游未返回任何图像")
	}

	return &TaskSubmitResult{
		UpstreamID: "",
		Status:     model.TaskStatusSucceeded,
		Progress:   100,
		ResultURL:  strings.TrimSpace(parsed.Data[0].URL),
		ResultData: string(raw),
	}, nil
}

// Poll 对同步型适配器无实际意义（任务提交时已是终态）。
//
// 仍然实现它：接口要求如此，且返回"已完成 + 本地已存结果"是语义正确的
// 兜底——万一将来有人把它接进轮询链路，也不会产生错误行为。
func (p *openAIImageProvider) Poll(_ context.Context, task *model.Task, _ *model.Channel, _ string) (*TaskPollResult, error) {
	return &TaskPollResult{
		Status:     model.TaskStatusSucceeded,
		Progress:   100,
		ResultURL:  task.ResultURL,
		ResultData: task.ResultData,
	}, nil
}
