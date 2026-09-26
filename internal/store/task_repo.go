// 本文件是 model.TaskRepository 的 SQL 实现（异步任务）。
//
// 意图（Why）：
//
//	异步任务的读写有两个特殊要求，决定了本文件的写法：
//	  1) 「只允许前进」：任务的进度只能由非终态推进到终态，不能被在途的
//	     轮询响应改回去。因此 UpdateProgress / Finish 的 SQL 都带
//	     status NOT IN (终态) 条件，把这条不变量下沉到数据库，
//	     而不是只靠 Go 侧的对象状态判断（多实例部署时对象状态并不可靠）；
//	  2) 「扫描要便宜」：轮询器会周期性扫描未完成任务，必须走
//	     idx_tasks_status_created 索引，且限制返回条数，避免一次拉全表。
//
// 流转（Flow）：
//
//	NewTaskRepository(db)
//	  ├─ 提交：relay.TaskService.Submit → Create
//	  ├─ 查询：GET /v1/tasks/{ref} → GetByRef
//	  ├─ 轮询：TaskService.PollOnce → ListPending → UpdateProgress / Finish
//	  └─ 管理：后台任务列表 → List / Count
//
// 扩展（Extend）：
//
//	新增字段：先建迁移加列，再同步本文件的 taskColumns / scanTask /
//	Create 的列清单 / Finish 的参数四处（漏改会出现"写入成功但读出来是零值"）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// taskColumns 集中定义查询列，顺序必须与 scanTask 的扫描顺序严格一致。
const taskColumns = `id, task_ref, user_id, token_id, channel_id, kind, provider, model,
	prompt, params, status, progress, upstream_id, result_url, result_data, error, quota,
	created_at, updated_at, finished_at`

// 任务列表的分页参数。
//
// 上限取 200：管理后台的翻页浏览用不到更大页，
// 而任务行含 prompt 与 result_data（可能很长），页过大容易拉出几十 MB 响应。
const (
	defaultTaskPageSize = 20
	maxTaskPageSize     = 200
)

// defaultPendingScanLimit 是轮询器单次扫描的任务数上限。
//
// 取 50：一次轮询要发 50 次上游请求，控制在该量级可避免
// 大量积压任务同时打满上游与本地连接池。
const defaultPendingScanLimit = 50

// taskRepository 是 model.TaskRepository 的 SQL 实现，并发安全。
type taskRepository struct {
	db *sql.DB
}

// NewTaskRepository 创建异步任务仓储。
func NewTaskRepository(db *sql.DB) model.TaskRepository {
	return &taskRepository{db: db}
}

// Create 创建任务。
//
// 说明：task_ref 由调用方（relay）生成并传入，因为任务号需要在
// "向上游提交之前"就确定下来——上游返回后我们才能把它与 task_ref 关联，
// 若等落库后再生成，中途崩溃就会产生"上游有任务但本地无记录"的悬挂状态。
func (r *taskRepository) Create(ctx context.Context, task *model.Task) error {
	if err := task.Validate(); err != nil {
		return fmt.Errorf("store: 任务非法: %w", err)
	}

	now := time.Now()
	task.CreatedAt = now
	task.UpdatedAt = now
	if task.Status == 0 {
		task.Status = model.TaskStatusQueued
	}
	if strings.TrimSpace(task.Params) == "" {
		task.Params = "{}"
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO tasks
			(task_ref, user_id, token_id, channel_id, kind, provider, model, prompt, params,
			 status, progress, upstream_id, result_url, result_data, error, quota,
			 created_at, updated_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.TaskRef, task.UserID, task.TokenID, task.ChannelID, string(task.Kind), task.Provider,
		task.Model, task.Prompt, task.Params, int(task.Status), task.Progress,
		task.UpstreamID, task.ResultURL, task.ResultData, task.Error, task.Quota,
		task.CreatedAt.Unix(), task.UpdatedAt.Unix(), unixOrZero(task.FinishedAt),
	)
	if err != nil {
		// 唯一索引冲突即"任务号重复"。任务号有 96 位随机，重复意味着
		// 随机源或生成逻辑出了严重问题，应作为内部错误暴露。
		if strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
			return fmt.Errorf("store: 任务号重复（%s），请检查随机源", task.TaskRef)
		}
		return fmt.Errorf("store: 创建任务失败: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: 读取新增任务的 ID 失败: %w", err)
	}
	task.ID = uint64(id)
	return nil
}

// GetByRef 按对外任务号查询。
func (r *taskRepository) GetByRef(ctx context.Context, taskRef string) (*model.Task, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE task_ref = ?", strings.TrimSpace(taskRef))

	task, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.ErrTaskNotFound
		}
		return nil, err
	}
	return task, nil
}

// List 查询任务列表（按创建时间倒序）。
func (r *taskRepository) List(ctx context.Context, query model.TaskQuery) ([]*model.Task, error) {
	where, args := buildTaskWhere(query)

	limit := normalizeLimit(query.Limit, defaultTaskPageSize, maxTaskPageSize)
	offset := normalizeOffset(query.Offset)

	sqlText := "SELECT " + taskColumns + " FROM tasks" + where +
		" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询任务列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tasks := make([]*model.Task, 0, 32)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历任务列表失败: %w", err)
	}
	return tasks, nil
}

// Count 统计符合条件的任务数。
func (r *taskRepository) Count(ctx context.Context, query model.TaskQuery) (int64, error) {
	where, args := buildTaskWhere(query)

	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(1) FROM tasks"+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计任务数失败: %w", err)
	}
	return total, nil
}

// UpdateProgress 更新状态与进度（仅对非终态生效）。
//
// 关键：WHERE 中带 status NOT IN (3,4,5)。这行条件就是"任务只允许前进"
// 这条不变量的落地形式——若省略它，一个慢返回的轮询响应会把已完成的
// 任务改回"进行中"，用户将看到结果消失、进度倒退。
//
// upstream_id 为空时保留原值：不同上游的轮询路径可能只在首次返回上游任务号。
func (r *taskRepository) UpdateProgress(ctx context.Context, id uint64, status model.TaskStatus, progress int, upstreamID string) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}

	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
			status = ?,
			progress = ?,
			upstream_id = CASE WHEN ? = '' THEN upstream_id ELSE ? END,
			updated_at = ?
		WHERE id = ? AND status NOT IN (?, ?, ?)`,
		int(status), progress, upstreamID, upstreamID, time.Now().Unix(), id,
		int(model.TaskStatusSucceeded), int(model.TaskStatusFailed), int(model.TaskStatusCanceled),
	)
	if err != nil {
		return fmt.Errorf("store: 更新任务 %d 进度失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		// 两种可能：任务不存在，或已处于终态（被别处的轮询抢先完成）。
		// 两者都不算错误，调用方无需处理——这正是我们要的语义：
		// 重复轮询是常态，不应该产生错误噪音。
		return nil
	}
	return nil
}

// Finish 标记任务为终态。
//
// 同样带 status NOT IN (终态) 条件：只有第一个到达终态的写入生效，
// 从而保证"失败退还额度"这类副作用最多执行一次（幂等）。
func (r *taskRepository) Finish(ctx context.Context, id uint64, status model.TaskStatus, resultURL, resultData, errorText string, quota int64) error {
	now := time.Now()
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
			status = ?, progress = ?, result_url = ?, result_data = ?, error = ?,
			quota = ?, updated_at = ?, finished_at = ?
		WHERE id = ? AND status NOT IN (?, ?, ?)`,
		int(status), terminalProgress(status), resultURL, resultData, truncateForColumn(errorText, 500),
		quota, now.Unix(), now.Unix(), id,
		int(model.TaskStatusSucceeded), int(model.TaskStatusFailed), int(model.TaskStatusCanceled),
	)
	if err != nil {
		return fmt.Errorf("store: 结束任务 %d 失败: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取更新影响行数失败: %w", err)
	}
	if affected == 0 {
		// 已被其它路径结束：属于正常的并发情形，不视为错误。
		// 调用方据此可以安全地跳过"退还额度"等副作用（因为对方已执行过）。
		return model.ErrTaskAlreadyFinished
	}
	return nil
}

// ListPending 列出需要继续轮询的任务。
//
// 排序与过滤都贴合 idx_tasks_status_created(status, created_at)：
// 按创建时间升序，让"先提交的任务"优先被轮询。
func (r *taskRepository) ListPending(ctx context.Context, limit int) ([]*model.Task, error) {
	if limit <= 0 {
		limit = defaultPendingScanLimit
	}

	rows, err := r.db.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE status IN (?, ?) ORDER BY created_at ASC LIMIT ?",
		int(model.TaskStatusQueued), int(model.TaskStatusRunning), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询待轮询任务失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tasks := make([]*model.Task, 0, limit)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历待轮询任务失败: %w", err)
	}
	return tasks, nil
}

// buildTaskWhere 依据查询条件拼装 WHERE 子句与参数。
//
// 单独抽出来是为了让 List 与 Count 共用同一套过滤条件——
// 两处各写一遍是"总数与列表对不上"这类 bug 的经典来源。
func buildTaskWhere(query model.TaskQuery) (string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)

	if query.UserID > 0 {
		conditions = append(conditions, "user_id = ?")
		args = append(args, query.UserID)
	}
	if query.Kind.IsValid() {
		conditions = append(conditions, "kind = ?")
		args = append(args, string(query.Kind))
	}
	if query.Status != nil {
		conditions = append(conditions, "status = ?")
		args = append(args, int(*query.Status))
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// terminalProgress 返回终态对应的进度值。
//
// 成功固定为 100，失败/取消保留传入值（通常由调用方置为当前进度）。
func terminalProgress(status model.TaskStatus) int {
	if status == model.TaskStatusSucceeded {
		return 100
	}
	return 0
}

// truncateForColumn 按【字节】截断文本，避免超长错误信息撑大数据库字段。
//
// 只按字节截断即可：这里的目标是"限制体积"，不追求截断点语义完整。
func truncateForColumn(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max]
}

// scanTask 把一行数据映射为任务对象。
func scanTask(sc rowScanner) (*model.Task, error) {
	var (
		id         uint64
		taskRef    string
		userID     uint64
		tokenID    uint64
		channelID  uint64
		kind       string
		provider   string
		modelName  string
		prompt     string
		params     string
		status     int
		progress   int
		upstreamID string
		resultURL  string
		resultData string
		errorText  string
		quota      int64
		createdAt  int64
		updatedAt  int64
		finishedAt int64
	)

	if err := sc.Scan(&id, &taskRef, &userID, &tokenID, &channelID, &kind, &provider, &modelName,
		&prompt, &params, &status, &progress, &upstreamID, &resultURL, &resultData, &errorText,
		&quota, &createdAt, &updatedAt, &finishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: 读取任务字段失败: %w", err)
	}

	task := &model.Task{
		ID:         id,
		TaskRef:    taskRef,
		UserID:     userID,
		TokenID:    tokenID,
		ChannelID:  channelID,
		Kind:       model.TaskKind(kind),
		Provider:   provider,
		Model:      modelName,
		Prompt:     prompt,
		Params:     params,
		Status:     model.TaskStatus(status),
		Progress:   progress,
		UpstreamID: upstreamID,
		ResultURL:  resultURL,
		ResultData: resultData,
		Error:      errorText,
		Quota:      quota,
		CreatedAt:  time.Unix(createdAt, 0),
		UpdatedAt:  time.Unix(updatedAt, 0),
	}
	if finishedAt > 0 {
		task.FinishedAt = time.Unix(finishedAt, 0)
	}
	return task, nil
}
