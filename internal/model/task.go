// 本文件定义「异步任务」领域模型与仓储接口。
//
// 意图（Why）：
//
//	绘图、视频生成这类上游是异步的：提交拿到任务号，再轮询取结果。
//	这类请求无法用"一问一答"的转发模型承载（视频可能耗时数分钟，
//	同步等待必然超时，且客户端无法看到进度）。
//
//	因此把任务的生命周期显式建模：
//	  提交（扣额度） → 排队/进行中 → 完成（可取结果） 或 失败/取消。
//
// 为什么额度在"提交时"就扣：
//
//	异步任务的上游一旦受理就会消耗资源（部分平台甚至不可退）。
//	若等任务完成才扣费，用户可以无限提交然后取消来白嫖算力；
//	因此采用"提交即扣、失败退还"的策略——这也是主流平台的做法。
//
// 流转（Flow）：
//
//	POST /v1/tasks（提交）
//	  └─ relay: 选渠道 → 适配器 Submit → 落库（已扣额度）
//	GET /v1/tasks/{ref}（查询）
//	  └─ relay: 若未终结 → 适配器 Poll → 更新库 → 返回
//
// 扩展（Extend）：
//
//	新增任务类别（如语音合成）：加一个 TaskKind 常量 + 实现 Provider 接口，
//	本文件与 store 层无需改动。
package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TaskKind 表示任务类别。
type TaskKind string

const (
	// TaskKindImage 图像生成 / 编辑。
	TaskKindImage TaskKind = "image"
	// TaskKindVideo 视频生成。
	TaskKindVideo TaskKind = "video"
	// TaskKindMusic 音乐 / 音频生成。
	TaskKindMusic TaskKind = "music"
)

// IsValid 判断类别是否合法。
func (k TaskKind) IsValid() bool {
	switch k {
	case TaskKindImage, TaskKindVideo, TaskKindMusic:
		return true
	default:
		return false
	}
}

// TaskStatus 表示任务状态。
type TaskStatus int

const (
	// TaskStatusQueued 排队中：已受理但上游尚未开始处理。
	TaskStatusQueued TaskStatus = 1
	// TaskStatusRunning 进行中：上游正在处理。
	TaskStatusRunning TaskStatus = 2
	// TaskStatusSucceeded 已完成：结果可取。
	TaskStatusSucceeded TaskStatus = 3
	// TaskStatusFailed 已失败。
	TaskStatusFailed TaskStatus = 4
	// TaskStatusCanceled 已取消。
	TaskStatusCanceled TaskStatus = 5
)

// String 返回状态中文名，便于日志与界面展示。
func (s TaskStatus) String() string {
	switch s {
	case TaskStatusQueued:
		return "排队中"
	case TaskStatusRunning:
		return "进行中"
	case TaskStatusSucceeded:
		return "已完成"
	case TaskStatusFailed:
		return "已失败"
	case TaskStatusCanceled:
		return "已取消"
	default:
		return fmt.Sprintf("未知(%d)", int(s))
	}
}

// IsTerminal 判断是否为终态（终态之后不再轮询）。
func (s TaskStatus) IsTerminal() bool {
	return s == TaskStatusSucceeded || s == TaskStatusFailed || s == TaskStatusCanceled
}

// ErrTaskNotFound 表示任务不存在。
var ErrTaskNotFound = errors.New("model: 任务不存在")

// ErrTaskAlreadyFinished 表示任务已处于终态，本次结束写入未生效。
//
// 用途：轮询器与查询接口都可能尝试结束同一个任务（用户查询时顺带推进一次，
// 后台轮询器同时也在推进）。该错误让调用方知道"别人已经结束了它"，
// 从而避免重复执行"失败退还额度"这类副作用。
var ErrTaskAlreadyFinished = errors.New("model: 任务已结束")

// Task 表示一个异步任务。
type Task struct {
	ID     uint64 // 主键（内部使用）
	TaskRef string // 对外任务号
	UserID  uint64 // 归属用户
	TokenID uint64 // 使用的访问令牌

	ChannelID uint64   // 实际执行的渠道；0 表示尚未分配
	Kind      TaskKind // 任务类别
	Provider  string   // 上游适配器名
	Model     string   // 模型或动作名
	Prompt    string   // 提示词
	Params    string   // 原始请求参数（JSON）

	Status   TaskStatus // 当前状态
	Progress int        // 进度 0-100
	// UpstreamID 是上游任务号，轮询时必须回传给它。
	UpstreamID string
	// ResultURL 是首个结果地址（列表页预览用）。
	ResultURL string
	// ResultData 是完整结果（JSON 字符串）。
	ResultData string
	// Error 是失败原因（已脱敏，不包含上游地址与密钥）。
	Error string
	// Quota 是实际扣减的额度。
	Quota int64

	CreatedAt  time.Time
	UpdatedAt  time.Time
	// FinishedAt 为进入终态的时间；零值表示尚未结束。
	FinishedAt time.Time
}

// Validate 校验任务的基本合法性。
func (t *Task) Validate() error {
	if strings.TrimSpace(t.TaskRef) == "" {
		return errors.New("任务号不能为空")
	}
	if t.UserID == 0 {
		return errors.New("任务必须归属某个用户")
	}
	if !t.Kind.IsValid() {
		return fmt.Errorf("任务类别非法: %q（支持 image / video / music）", t.Kind)
	}
	if strings.TrimSpace(t.Provider) == "" {
		return errors.New("必须指定上游适配器（provider）")
	}
	if t.Progress < 0 || t.Progress > 100 {
		return fmt.Errorf("进度必须在 0-100 之间，当前 %d", t.Progress)
	}
	return nil
}

// ApplyProgress 更新进度（只在非终态时生效）。
//
// 为什么要拦终态：轮询存在竞态——本地标记"已完成"之后，一个在途的
// 轮询响应可能带着旧进度回来。若不加判断，会把已完成的任务改回"进行中"。
func (t *Task) ApplyProgress(status TaskStatus, progress int) {
	if t.Status.IsTerminal() {
		return
	}
	t.Status = status
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	t.Progress = progress
}

// MarkSucceeded 标记任务成功。
func (t *Task) MarkSucceeded(resultURL, resultData string) {
	t.Status = TaskStatusSucceeded
	t.Progress = 100
	t.ResultURL = resultURL
	t.ResultData = resultData
	t.Error = ""
	t.FinishedAt = time.Now()
	t.UpdatedAt = t.FinishedAt
}

// MarkFailed 标记任务失败。
func (t *Task) MarkFailed(reason string) {
	t.Status = TaskStatusFailed
	t.Error = truncateText(reason, 500)
	t.FinishedAt = time.Now()
	t.UpdatedAt = t.FinishedAt
}

// TaskQuery 是任务列表的查询条件。
type TaskQuery struct {
	// UserID > 0 时只查该用户的任务；0 表示不限（管理员视角）。
	UserID uint64
	Kind   TaskKind
	Status *TaskStatus
	Limit  int
	Offset int
}

// TaskRepository 定义异步任务的持久化操作。
type TaskRepository interface {
	// Create 创建任务。
	Create(ctx context.Context, task *Task) error

	// GetByRef 按对外任务号查询。
	GetByRef(ctx context.Context, taskRef string) (*Task, error)

	// List 查询任务列表（按创建时间倒序）。
	List(ctx context.Context, query TaskQuery) ([]*Task, error)

	// Count 统计符合条件的任务数。
	Count(ctx context.Context, query TaskQuery) (int64, error)

	// UpdateProgress 更新状态与进度（仅对非终态生效）。
	UpdateProgress(ctx context.Context, id uint64, status TaskStatus, progress int, upstreamID string) error

	// Finish 标记任务为终态。
	Finish(ctx context.Context, id uint64, status TaskStatus, resultURL, resultData, errorText string, quota int64) error

	// ListPending 列出需要继续轮询的任务（非终态，按创建时间升序）。
	//
	// 按创建时间升序的原因：先提交的任务通常先完成，
	// 尽早轮询它们能让用户更快看到结果。
	ListPending(ctx context.Context, limit int) ([]*Task, error)
}

// GenerateTaskRef 生成对外任务号。
//
// 形态：task_ + 24 位十六进制（96 位随机）。
// 为什么不用自增 ID：连续编号会暴露业务量，且便于被枚举探测他人任务。
func GenerateTaskRef() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("model: 生成任务号失败: %w", err)
	}
	return "task_" + hex.EncodeToString(raw), nil
}

// truncateText 截断过长文本（避免把上游整页 HTML 塞进数据库）。
func truncateText(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// TaskKindFromString 解析任务类别，非法时返回错误。
func TaskKindFromString(raw string) (TaskKind, error) {
	kind := TaskKind(strings.ToLower(strings.TrimSpace(raw)))
	if !kind.IsValid() {
		return "", fmt.Errorf("不支持的任务类别 %q（支持 image / video / music）", raw)
	}
	return kind, nil
}
