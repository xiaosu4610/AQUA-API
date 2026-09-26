// 本文件实现异步任务的编排：提交、轮询、后台推进。
//
// 意图（Why）：
//
//	绘图、视频这类上游是"提交拿任务号 → 轮询取结果"的异步模型。
//	客户端不可能一直挂着连接等（视频要几分钟），因此网关必须自己
//	把任务生命周期管起来：
//	  1) 提交时选渠道、扣额度、落库，立刻返回任务号；
//	  2) 客户端凭任务号查询，网关按需向上游拉一次最新状态；
//	  3) 后台轮询器兜底推进任务——这很重要，因为大量客户端
//	     提交后就不再查询，若只依赖"客户端查询时推进"，
//	     这些任务会永远停在"进行中"，额度和结果都成了悬案。
//
// 为什么把 Provider 抽成接口：
//
//	不同上游的提交/查询协议差异很大（Midjourney 代理、OpenAI 兼容图像接口、
//	各家的视频接口）。把差异收敛在 TaskProvider 里，编排逻辑（选渠道、
//	计费、落库、退还、轮询）只写一遍，新增上游只需加一个适配器。
//
// 流转（Flow）：
//
//	POST /v1/tasks → TaskService.Submit
//	  ├─ 选渠道 + 解析凭据（复用 relay 的候选集与密钥池）
//	  ├─ Billing.ChargeOnce（提交即扣费，失败退还）
//	  ├─ Provider.Submit（拿上游任务号）
//	  └─ TaskRepository.Create（落库，返回任务号）
//	GET /v1/tasks/{ref} → TaskService.PollOnce
//	  └─ Provider.Poll → Finish（终态，失败退还）或 UpdateProgress
//	后台 goroutine → TaskService.RunPoller → 周期性 ListPending + PollOnce
//
// 扩展（Extend）：
//
//	新增上游：在 async_providers.go 实现 TaskProvider 并注册到
//	newBuiltinProviders，无需改动本文件。
//	新增任务类别：在 model 中加 TaskKind 常量，适配器的 Supports 里声明即可。
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// 任务链路的错误定义。
var (
	// ErrTaskProviderNotFound 表示没有支持该任务类别的上游适配器。
	ErrTaskProviderNotFound = errors.New("relay: 没有支持该任务类别的上游适配器（请在渠道配置中指定 provider）")
	// ErrTaskProviderUnknown 表示请求指定了不存在的适配器名。
	ErrTaskProviderUnknown = errors.New("relay: 指定的上游适配器不存在")
	// ErrTaskUnavailable 表示没有可用渠道/凭据来执行该任务。
	ErrTaskUnavailable = errors.New("relay: 当前没有可用的上游渠道能执行该任务")
)

// 任务链路的时间参数。
const (
	// taskOpTimeout 是单次上游调用（提交/查询）的超时。
	//
	// 与对话转发不同：任务类接口是"短请求短响应"（提交只是登记，查询只是读状态），
	// 因此可以放心设置超时；这也避免上游故障时后台轮询器被拖住。
	taskOpTimeout = 60 * time.Second

	// defaultPollInterval 是后台轮询器的默认扫描间隔。
	//
	// 取 15 秒的权衡：太密会让待轮询任务多的部署频繁打上游；
	// 太疏会让用户"任务完成了但界面上还显示进行中"的等待变长。
	defaultPollInterval = 15 * time.Second

	// taskPollerBatch 是单次轮询扫描的任务数上限。
	taskPollerBatch = 50

	// taskPromptMaxBytes 是提示词的入库长度上限。
	//
	// 截断而非拒绝：提示词过长是使用者的自由，不该因此失败，
	// 但库里只保留前若干字节供列表展示与排查即可。
	taskPromptMaxBytes = 2000
)

// TaskRequest 是一次任务提交的入参（已从 HTTP 请求体解析出来）。
type TaskRequest struct {
	Kind   model.TaskKind // 任务类别
	Model  string         // 模型/动作名
	Prompt string         // 提示词
	Params map[string]any // 其余原始参数（原样交给上游适配器）
	Count  int64          // 份数（影响按次计费）
	// Provider 指定上游适配器名；为空时按类别自动选择。
	//
	// 之所以允许显式指定：一个部署里可能同时接了 Midjourney 代理与
	// OpenAI 兼容图像接口，两者都声称支持 image，使用者需要能选。
	Provider string
}

// TaskSubmitResult 是适配器提交成功后的返回。
type TaskSubmitResult struct {
	// UpstreamID 是上游任务号，后续轮询必须回传。
	UpstreamID string
	// Status 为提交后的即时状态：多数上游返回"排队中"；
	// 同步型接口（如 OpenAI 兼容图像）可直接返回"已完成"。
	Status   model.TaskStatus
	Progress int
	// ResultURL / ResultData 仅在 Status 已为终态时才有值。
	ResultURL  string
	ResultData string
}

// TaskPollResult 是适配器查询一次后的返回。
type TaskPollResult struct {
	Status     model.TaskStatus
	Progress   int
	ResultURL  string
	ResultData string
	// Error 为失败原因（适配器负责脱敏，不得包含上游地址与密钥）。
	Error string
}

// TaskProvider 是异步任务的上游适配器。
//
// 实现约定：
//   - Submit 返回的任务号会被持久化，Poll 时通过 task.UpstreamID 拿回；
//   - 两个方法都可能返回网络错误，编排层会把它当成"暂时查不到状态"处理
//     （不立即判失败，避免一次上游抖动就把用户任务判死）。
type TaskProvider interface {
	// Name 返回适配器名（入库到 tasks.provider）。
	Name() string
	// Supports 声明该适配器支持哪些任务类别。
	Supports(kind model.TaskKind) bool
	// Submit 向上游提交任务。
	Submit(ctx context.Context, req *TaskRequest, ch *model.Channel, apiKey string) (*TaskSubmitResult, error)
	// Poll 查询任务的最新状态。
	Poll(ctx context.Context, task *model.Task, ch *model.Channel, apiKey string) (*TaskPollResult, error)
}

// TaskService 编排异步任务的生命周期。
//
// 并发安全：providers 与 relay 在构造后不再修改。
type TaskService struct {
	tasks     model.TaskRepository
	relay     *Relay
	providers []TaskProvider
}

// NewTaskService 创建任务服务并注册内置适配器。
//
// 参数 relay 用于复用"选渠道 + 解析凭据 + 计费"这套既有能力——
// 任务链路不该另起一套路由与密钥池逻辑，否则两边行为必然漂移。
func NewTaskService(tasks model.TaskRepository, r *Relay) *TaskService {
	return &TaskService{
		tasks:     tasks,
		relay:     r,
		providers: newBuiltinProviders(r.client),
	}
}

// Providers 返回已注册的适配器信息，供后台界面展示"支持哪些上游"。
func (s *TaskService) Providers() []TaskProvider {
	return s.providers
}

// findProvider 按请求选择适配器。
//
// 选择顺序：显式指定 > 第一个支持该类别的内置适配器。
// 顺序是稳定的（切片顺序固定），因此同一请求每次解析结果一致——
// 这对"任务落在哪个上游"的可预期性很重要。
func (s *TaskService) findProvider(req *TaskRequest) (TaskProvider, error) {
	name := strings.TrimSpace(req.Provider)
	if name != "" {
		for _, p := range s.providers {
			if p.Name() == name {
				if !p.Supports(req.Kind) {
					return nil, fmt.Errorf("%w：适配器 %s 不支持 %s 类任务",
						ErrTaskProviderNotFound, name, req.Kind)
				}
				return p, nil
			}
		}
		return nil, fmt.Errorf("%w：%s", ErrTaskProviderUnknown, name)
	}

	for _, p := range s.providers {
		if p.Supports(req.Kind) {
			return p, nil
		}
	}
	return nil, ErrTaskProviderNotFound
}

// Submit 提交一个异步任务：选渠道 → 扣费 → 提交上游 → 落库。
//
// 失败即退还：只要任务没能成功落库（客户端拿不到任务号），
// 提交阶段扣掉的额度就必须退回去，否则用户会为一笔"不存在的任务"付费。
//
// 已知取舍：若上游其实受理了任务、但我们在读取响应时超时，
// 这里会按失败处理并退还额度——用户白得一次生成。
// 之所以接受：这类情况极罕见，而"宁可少收，不可错收"是计费系统的原则。
func (s *TaskService) Submit(ctx context.Context, userID, tokenID uint64, req *TaskRequest) (*model.Task, error) {
	if s.tasks == nil {
		return nil, errors.New("relay: 异步任务模块未启用")
	}
	if !req.Kind.IsValid() {
		return nil, fmt.Errorf("不支持的任务类别 %q", req.Kind)
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		return nil, errors.New("缺少 model（或动作名）")
	}

	provider, err := s.findProvider(req)
	if err != nil {
		return nil, err
	}

	ch, apiKey, keyID, err := s.pickTarget(ctx, req.Model)
	if err != nil {
		return nil, err
	}

	// ── 提交即扣费 ────────────────────────────────────────────
	// 为什么在提交上游之前扣：见 model.Task 的文件头说明（防白嫖算力）。
	quota := int64(0)
	if s.relay != nil && s.relay.billing != nil {
		quota = s.relay.billing.ChargeOnce(ctx, userID, tokenID, req.Model, req.Count)
	}

	// ── 提交上游 ──────────────────────────────────────────────
	paramsJSON, err := json.Marshal(req.Params)
	if err != nil {
		s.refund(ctx, userID, tokenID, quota)
		return nil, fmt.Errorf("任务参数不是合法 JSON: %w", err)
	}

	submitCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
	defer cancel()

	result, err := provider.Submit(submitCtx, req, ch, apiKey)
	if err != nil {
		s.refund(ctx, userID, tokenID, quota)
		return nil, fmt.Errorf("提交上游任务失败: %w", err)
	}

	taskRef, err := model.GenerateTaskRef()
	if err != nil {
		s.refund(ctx, userID, tokenID, quota)
		return nil, err
	}

	task := &model.Task{
		TaskRef:    taskRef,
		UserID:     userID,
		TokenID:    tokenID,
		ChannelID:  ch.ID,
		Kind:       req.Kind,
		Provider:   provider.Name(),
		Model:      req.Model,
		Prompt:     truncateBytes(req.Prompt, taskPromptMaxBytes),
		Params:     string(paramsJSON),
		Status:     model.TaskStatusQueued,
		UpstreamID: result.UpstreamID,
		Quota:      quota,
	}
	if result.Status == 0 {
		task.Status = model.TaskStatusQueued
	} else {
		task.Status = result.Status
	}
	task.Progress = result.Progress
	if task.Status == model.TaskStatusSucceeded {
		task.MarkSucceeded(result.ResultURL, result.ResultData)
	}

	if err := s.tasks.Create(ctx, task); err != nil {
		// 落库失败：上游任务已经创建，但客户端拿不到任务号，只能退还并报错
		s.refund(ctx, userID, tokenID, quota)
		return nil, fmt.Errorf("保存任务失败: %w", err)
	}

	// 记录凭据使用情况：提交成功即视为"这把凭据可用"
	s.markKeySuccess(ctx, keyID)
	return task, nil
}

// PollOnce 拉取一次任务的最新状态并落库。
//
// 返回值语义：
//   - 任务已处于终态：原样返回，不产生任何上游请求；
//   - 拉取失败：返回当前（可能仍是进行中的）任务快照，不判失败。
//     原因：一次网络抖动不代表任务失败，后台轮询器下一轮会再试。
func (s *TaskService) PollOnce(ctx context.Context, task *model.Task) (*model.Task, error) {
	if s.tasks == nil || task == nil {
		return task, nil
	}
	if task.Status.IsTerminal() {
		return task, nil
	}

	provider := s.providerByName(task.Provider)
	if provider == nil {
		// 适配器被下线（如上游服务停用）：按失败收尾，退还额度，
		// 否则任务会永远卡在"进行中"。
		s.finishTask(ctx, task, model.TaskStatusFailed, "", "", "上游适配器不可用", 0)
		return task, nil
	}

	ch, apiKey, keyID, err := s.channelForTask(ctx, task)
	if err != nil {
		// 渠道被删除或密钥池已空：本轮无法推进，保留任务等下一轮
		// （渠道可能只是被临时禁用后又被启用，不该据此判任务失败）
		return task, nil
	}

	pollCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
	defer cancel()

	result, err := provider.Poll(pollCtx, task, ch, apiKey)
	if err != nil {
		// 上游暂时查不到状态：不改变本地状态，等下一轮
		return task, nil
	}

	if result.Status.IsTerminal() {
		s.markKeySuccess(ctx, keyID)
		s.finishTask(ctx, task, result.Status, result.ResultURL, result.ResultData, result.Error, task.Quota)
		return task, nil
	}

	// 非终态：只更新状态与进度（仓储层保证不会覆盖已达终态的任务）
	if err := s.tasks.UpdateProgress(ctx, task.ID, result.Status, result.Progress, task.UpstreamID); err != nil {
		slog.Warn("更新任务进度失败", "error", err, "task_ref", task.TaskRef)
		return task, nil
	}
	task.ApplyProgress(result.Status, result.Progress)
	return task, nil
}

// finishTask 把任务置为终态，并在失败/取消时退还额度。
//
// 幂等性：Finish 带"仅非终态可写"条件，返回 ErrTaskAlreadyFinished 时
// 说明别处已经结束过它（退还也已执行过），因此这里直接返回，不重复退还。
func (s *TaskService) finishTask(ctx context.Context, task *model.Task,
	status model.TaskStatus, resultURL, resultData, errorText string, quota int64) {

	err := s.tasks.Finish(ctx, task.ID, status, resultURL, resultData, errorText, quota)
	if errors.Is(err, model.ErrTaskAlreadyFinished) {
		return
	}
	if err != nil {
		slog.Warn("结束任务失败", "error", err, "task_ref", task.TaskRef)
		return
	}

	// 仅当本次写入真正生效（err == nil）才退还，保证"最多退一次"
	if status != model.TaskStatusSucceeded && task.Quota > 0 {
		s.refund(ctx, task.UserID, task.TokenID, task.Quota)
	}

	// 同步内存快照，便于调用方直接返回给客户端
	switch status {
	case model.TaskStatusSucceeded:
		task.MarkSucceeded(resultURL, resultData)
	default:
		task.MarkFailed(errorText)
		if status == model.TaskStatusCanceled {
			task.Status = model.TaskStatusCanceled
		}
	}
}

// Cancel 取消任务（仅非终态可取消），并退还额度。
func (s *TaskService) Cancel(ctx context.Context, task *model.Task) error {
	if s.tasks == nil || task == nil {
		return model.ErrTaskNotFound
	}
	if task.Status.IsTerminal() {
		return model.ErrTaskAlreadyFinished
	}
	s.finishTask(ctx, task, model.TaskStatusCanceled, "", "", "用户取消", task.Quota)
	return nil
}

// RunPoller 周期性推进未完成任务，直到 ctx 结束。
//
// 为什么必须有它：绝大多数客户端提交后就不再查询。
// 若只依赖"客户端查询时顺带推进"，这些任务会永远停在"进行中"，
// 用户永远拿不到结果，额度也永远处于"已扣但未定"的状态。
//
// 启动方式：由 main 以 goroutine 方式调用（见 cmd/aqua）。
func (s *TaskService) RunPoller(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollPending(ctx)
		}
	}
}

// pollPending 扫描一轮待推进的任务。
func (s *TaskService) pollPending(ctx context.Context) {
	if s.tasks == nil {
		return
	}
	// 每轮用独立的短超时：避免某轮上游卡住导致后续轮次全部顺延
	roundCtx, cancel := context.WithTimeout(ctx, taskOpTimeout)
	defer cancel()

	pending, err := s.tasks.ListPending(roundCtx, taskPollerBatch)
	if err != nil {
		slog.Warn("扫描待轮询任务失败", "error", err)
		return
	}
	for _, task := range pending {
		if ctx.Err() != nil {
			return
		}
		_, _ = s.PollOnce(ctx, task)
	}
}

// pickTarget 为任务选出渠道与凭据。
//
// 复用转发链路的候选集与密钥池：任务与对话请求共享同一套路由语义
// （优先级分层 → 层内权重随机 → 池内随机取凭据）。
func (s *TaskService) pickTarget(ctx context.Context, modelName string) (*model.Channel, string, uint64, error) {
	if s.relay == nil {
		return nil, "", 0, ErrTaskUnavailable
	}

	candidates, err := s.relay.listCandidates(ctx, modelName)
	if err != nil {
		return nil, "", 0, err
	}
	if len(candidates) == 0 {
		return nil, "", 0, fmt.Errorf("%w（模型=%s）", ErrTaskUnavailable, modelName)
	}

	// 逐一尝试候选渠道，直到找到一个有可用凭据的
	excluded := make(map[uint64]struct{})
	for range candidates {
		ch := pickCandidate(candidates, excluded)
		if ch == nil {
			break
		}
		apiKey, keyID, ok, _ := s.relay.resolveChatKey(ctx, ch, nil)
		if !ok {
			excluded[ch.ID] = struct{}{}
			continue
		}
		return ch, apiKey, keyID, nil
	}
	return nil, "", 0, ErrTaskUnavailable
}

// channelForTask 取回任务所属渠道并解析出一把可用凭据。
func (s *TaskService) channelForTask(ctx context.Context, task *model.Task) (*model.Channel, string, uint64, error) {
	if s.relay == nil || task.ChannelID == 0 {
		return nil, "", 0, ErrTaskUnavailable
	}
	ch, err := s.relay.channels.GetByID(ctx, task.ChannelID)
	if err != nil {
		return nil, "", 0, err
	}
	apiKey, keyID, ok, _ := s.relay.resolveChatKey(ctx, ch, nil)
	if !ok {
		return nil, "", 0, ErrTaskUnavailable
	}
	return ch, apiKey, keyID, nil
}

// providerByName 按名字找适配器。
func (s *TaskService) providerByName(name string) TaskProvider {
	for _, p := range s.providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// refund 退还额度（内部统一走 Billing，容忍计费组件缺失）。
func (s *TaskService) refund(ctx context.Context, userID, tokenID uint64, quota int64) {
	if s.relay == nil || s.relay.billing == nil || quota <= 0 {
		return
	}
	s.relay.billing.Refund(ctx, userID, tokenID, quota)
}

// markKeySuccess 清零凭据的连续失败计数（失败静默忽略：属统计用途）。
func (s *TaskService) markKeySuccess(ctx context.Context, keyID uint64) {
	if s.relay == nil || keyID == 0 {
		return
	}
	s.relay.markKeySuccess(ctx, keyID)
}

// truncateBytes 按字节截断字符串，超长时追加省略号。
func truncateBytes(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// newRequest 构造带 JSON 请求体的上游请求（任务链路专用）。
//
// 与对话转发一样，凭据通过 Authorization 头传递（Bearer）；
// Midjourney 类的代理服务通常把密钥放在 Authorization 里校验。
func newTaskRequest(ctx context.Context, method, url string, body []byte, apiKey string) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("relay: 构造上游任务请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}
