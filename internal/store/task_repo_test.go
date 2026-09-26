// 异步任务仓储的单元测试。
//
// 测试重点（都是"错了会静默产生坏数据"的场景）：
//   - 任务只允许前进：轮询竞态不能把已完成的任务改回进行中；
//   - 结束幂等：Finish 在同一任务上第二次调用必须返回 ErrTaskAlreadyFinished，
//     这是"失败退还额度最多执行一次"的依据；
//   - 待轮询扫描只返回非终态，且按创建时间升序。
package store

import (
	"context"
	"errors"
	"testing"

	"gitee.com/xiaosu4610/aqua-api/internal/model"
)

// newTestTaskRepo 构造基于临时数据库的任务仓储。
func newTestTaskRepo(t *testing.T) model.TaskRepository {
	t.Helper()
	st := newTestStore(t)
	return NewTaskRepository(st.DB())
}

// newTask 构造一个可用于落库的任务。
func newTask(t *testing.T, ref string) *model.Task {
	t.Helper()
	return &model.Task{
		TaskRef:  ref,
		UserID:   1,
		TokenID:  2,
		Kind:     model.TaskKindImage,
		Provider: "midjourney",
		Model:    "mj-imagine",
		Prompt:   "一只在雨里的猫",
		Params:   `{"n":1}`,
		Status:   model.TaskStatusQueued,
		Quota:    120,
	}
}

func TestTaskRepository_CreateAndGetByRef(t *testing.T) {
	repo := newTestTaskRepo(t)
	ctx := context.Background()

	task := newTask(t, "task_aaaaaaaaaaaaaaaaaaaaaaaa")
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if task.ID == 0 {
		t.Fatal("创建后应回填 ID")
	}
	if task.CreatedAt.IsZero() {
		t.Fatal("创建后应回填创建时间")
	}

	got, err := repo.GetByRef(ctx, task.TaskRef)
	if err != nil {
		t.Fatalf("按任务号查询失败: %v", err)
	}
	if got.Prompt != "一只在雨里的猫" || got.Quota != 120 || got.Kind != model.TaskKindImage {
		t.Fatalf("读回的数据不一致: %+v", got)
	}
	if got.Status != model.TaskStatusQueued {
		t.Fatalf("初始状态应为排队中，实际 %v", got.Status)
	}
}

func TestTaskRepository_查询不存在_应返回ErrTaskNotFound(t *testing.T) {
	repo := newTestTaskRepo(t)

	_, err := repo.GetByRef(context.Background(), "task_000000000000000000000000")
	if !errors.Is(err, model.ErrTaskNotFound) {
		t.Fatalf("应返回 ErrTaskNotFound，实际 %v", err)
	}
}

func TestTaskRepository_UpdateProgress_终态后不可回退(t *testing.T) {
	repo := newTestTaskRepo(t)
	ctx := context.Background()

	task := newTask(t, "task_bbbbbbbbbbbbbbbbbbbbbbbb")
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	// 先推进到进行中
	if err := repo.UpdateProgress(ctx, task.ID, model.TaskStatusRunning, 40, "up-1"); err != nil {
		t.Fatalf("更新进度失败: %v", err)
	}
	got, _ := repo.GetByRef(ctx, task.TaskRef)
	if got.Status != model.TaskStatusRunning || got.Progress != 40 || got.UpstreamID != "up-1" {
		t.Fatalf("进度更新未生效: %+v", got)
	}

	// 置为终态
	if err := repo.Finish(ctx, task.ID, model.TaskStatusSucceeded, "https://example.com/a.png", `{"ok":true}`, "", 120); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}

	// 一个"在途的旧轮询响应"带着低进度回来，必须被拒绝
	if err := repo.UpdateProgress(ctx, task.ID, model.TaskStatusRunning, 10, ""); err != nil {
		t.Fatalf("对终态任务的进度更新不应报错: %v", err)
	}

	got, _ = repo.GetByRef(ctx, task.TaskRef)
	if got.Status != model.TaskStatusSucceeded || got.Progress != 100 {
		t.Fatalf("终态被回退（任务只允许前进）: %+v", got)
	}
	if got.ResultURL != "https://example.com/a.png" {
		t.Fatalf("结果地址应被保留: %+v", got)
	}
}

func TestTaskRepository_Finish_重复结束应返回已结束(t *testing.T) {
	repo := newTestTaskRepo(t)
	ctx := context.Background()

	task := newTask(t, "task_cccccccccccccccccccccccc")
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	if err := repo.Finish(ctx, task.ID, model.TaskStatusFailed, "", "", "上游超时", 0); err != nil {
		t.Fatalf("首次结束应成功: %v", err)
	}
	// 第二次结束必须被拒：调用方据此保证"退还额度"只执行一次
	err := repo.Finish(ctx, task.ID, model.TaskStatusFailed, "", "", "上游超时", 0)
	if !errors.Is(err, model.ErrTaskAlreadyFinished) {
		t.Fatalf("重复结束应返回 ErrTaskAlreadyFinished，实际 %v", err)
	}
}

func TestTaskRepository_ListPending_只返回非终态且按创建时间升序(t *testing.T) {
	repo := newTestTaskRepo(t)
	ctx := context.Background()

	first := newTask(t, "task_dddddddddddddddddddddddd")
	second := newTask(t, "task_eeeeeeeeeeeeeeeeeeeeeeee")
	done := newTask(t, "task_ffffffffffffffffffffffff")

	for _, task := range []*model.Task{first, second, done} {
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("创建任务失败: %v", err)
		}
	}
	if err := repo.Finish(ctx, done.ID, model.TaskStatusSucceeded, "u", "d", "", 0); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}

	pending, err := repo.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("查询待轮询任务失败: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("应只返回 2 个非终态任务，实际 %d", len(pending))
	}
	if pending[0].TaskRef != first.TaskRef || pending[1].TaskRef != second.TaskRef {
		t.Fatalf("应按创建时间升序返回，实际 %s、%s", pending[0].TaskRef, pending[1].TaskRef)
	}
}

func TestTaskRepository_List_按用户与状态过滤(t *testing.T) {
	repo := newTestTaskRepo(t)
	ctx := context.Background()

	mine := newTask(t, "task_111111111111111111111111")
	other := newTask(t, "task_222222222222222222222222")
	other.UserID = 99

	for _, task := range []*model.Task{mine, other} {
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("创建任务失败: %v", err)
		}
	}

	tasks, err := repo.List(ctx, model.TaskQuery{UserID: 1})
	if err != nil {
		t.Fatalf("查询任务列表失败: %v", err)
	}
	if len(tasks) != 1 || tasks[0].TaskRef != mine.TaskRef {
		t.Fatalf("按用户过滤结果不正确: %+v", tasks)
	}

	total, err := repo.Count(ctx, model.TaskQuery{UserID: 1})
	if err != nil {
		t.Fatalf("统计任务数失败: %v", err)
	}
	if total != 1 {
		t.Fatalf("按用户统计应为 1，实际 %d", total)
	}

	// 状态过滤：把 mine 结束掉，再查"已完成"应只剩它
	if err := repo.Finish(ctx, mine.ID, model.TaskStatusSucceeded, "", "", "", 120); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}
	succeeded := model.TaskStatusSucceeded
	tasks, err = repo.List(ctx, model.TaskQuery{Status: &succeeded})
	if err != nil {
		t.Fatalf("按状态查询失败: %v", err)
	}
	if len(tasks) != 1 || tasks[0].TaskRef != mine.TaskRef {
		t.Fatalf("按状态过滤结果不正确: %+v", tasks)
	}
}
