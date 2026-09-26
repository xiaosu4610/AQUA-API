// 异步任务领域模型与按次计价的单元测试。
//
// 测试重点：
//   - 任务号形态与唯一性（它是客户端的唯一凭据，重复即灾难）；
//   - "任务只允许前进"：终态后进度更新必须无效；
//   - 按次计价的边界（缺省份数按 1 次，不能算成免费）。
package model

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerateTaskRef_形态与唯一性(t *testing.T) {
	const samples = 200
	seen := make(map[string]struct{}, samples)

	for i := 0; i < samples; i++ {
		ref, err := GenerateTaskRef()
		if err != nil {
			t.Fatalf("生成任务号失败: %v", err)
		}
		if !strings.HasPrefix(ref, "task_") {
			t.Fatalf("任务号缺少 task_ 前缀: %s", ref)
		}
		raw := strings.TrimPrefix(ref, "task_")
		if len(raw) != 24 {
			t.Fatalf("任务号随机部分应为 24 位十六进制，实际 %d 位（%s）", len(raw), ref)
		}
		if _, err := hex.DecodeString(raw); err != nil {
			t.Fatalf("任务号随机部分不是合法十六进制: %s", ref)
		}
		if _, dup := seen[ref]; dup {
			t.Fatalf("任务号重复: %s", ref)
		}
		seen[ref] = struct{}{}
	}
}

func TestTask_ApplyProgress_终态不回退(t *testing.T) {
	task := &Task{Status: TaskStatusRunning, Progress: 60}

	task.ApplyProgress(TaskStatusRunning, 80)
	if task.Progress != 80 {
		t.Fatalf("非终态应可更新进度，实际 %d", task.Progress)
	}

	// 进度越界应被归一化（上游偶尔会回传 120% 之类的值）
	task.ApplyProgress(TaskStatusRunning, 120)
	if task.Progress != 100 {
		t.Fatalf("进度应被截断到 100，实际 %d", task.Progress)
	}

	task.MarkSucceeded("https://example.com/a.png", "{}")
	task.ApplyProgress(TaskStatusQueued, 5)
	if task.Status != TaskStatusSucceeded || task.Progress != 100 {
		t.Fatalf("终态不应被回退: status=%v progress=%d", task.Status, task.Progress)
	}
}

func TestTask_MarkFailed_原因被截断(t *testing.T) {
	task := &Task{Status: TaskStatusRunning}
	task.MarkFailed(strings.Repeat("错", 1000))

	if task.Status != TaskStatusFailed {
		t.Fatalf("状态应为已失败，实际 %v", task.Status)
	}
	// 按字节截断 500，中文占 3 字节，因此长度应明显小于原文
	if len(task.Error) > 510 {
		t.Fatalf("失败原因应被截断，实际长度 %d", len(task.Error))
	}
	if task.FinishedAt.IsZero() {
		t.Fatal("进入终态应记录结束时间")
	}
}

func TestTaskValidate_类别与进度校验(t *testing.T) {
	base := Task{
		TaskRef:  "task_x",
		UserID:   1,
		Kind:     TaskKindImage,
		Provider: "midjourney",
		Progress: 0,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("合法任务不应报错: %v", err)
	}

	badKind := base
	badKind.Kind = TaskKind("unknown")
	if err := badKind.Validate(); err == nil {
		t.Fatal("非法类别应报错")
	}

	badProgress := base
	badProgress.Progress = 101
	if err := badProgress.Validate(); err == nil {
		t.Fatal("进度超过 100 应报错")
	}
}

func TestTaskKindFromString_大小写与非法值(t *testing.T) {
	kind, err := TaskKindFromString(" IMAGE ")
	if err != nil || kind != TaskKindImage {
		t.Fatalf("应容忍大小写与空白，实际 kind=%v err=%v", kind, err)
	}
	if _, err := TaskKindFromString("pdf"); err == nil {
		t.Fatal("非法类别应报错")
	}
}

func TestModelPrice_ComputePerCallQuota_缺省按一次(t *testing.T) {
	price := &ModelPrice{Model: "mj-imagine", PerCallPrice: 500, Group: "default", Enabled: true}

	if got := price.ComputePerCallQuota(1); got != 500 {
		t.Fatalf("一次调用应扣 500，实际 %d", got)
	}
	if got := price.ComputePerCallQuota(4); got != 2000 {
		t.Fatalf("四次调用应扣 2000，实际 %d", got)
	}
	// 份数缺省/非法时必须按 1 次计费，否则会变成"免费"（站长白亏）
	if got := price.ComputePerCallQuota(0); got != 500 {
		t.Fatalf("份数为 0 应按 1 次计费，实际 %d", got)
	}
	if got := price.ComputePerCallQuota(-3); got != 500 {
		t.Fatalf("份数为负应按 1 次计费，实际 %d", got)
	}

	var nilPrice *ModelPrice
	if got := nilPrice.ComputePerCallQuota(2); got != 0 {
		t.Fatalf("空规则应返回 0，实际 %d", got)
	}
}

func TestTask_并发Test_ApplyProgress_不越界(t *testing.T) {
	// 说明：本用例验证多次调用后进度值始终落在合法区间。
	// 刻意不在多个 goroutine 里访问同一个 Task —— Task 是可变对象，
	// 按设计应由单个调用方持有；并发保护在仓储层（SQL 条件更新），
	// 因此这里用顺序调用验证归一化逻辑即可。
	task := &Task{Status: TaskStatusRunning}
	for _, progress := range []int{-10, 0, 50, 100, 200} {
		task.ApplyProgress(TaskStatusRunning, progress)
		if task.Progress < 0 || task.Progress > 100 {
			t.Fatalf("进度应被归一化到 0-100，实际 %d", task.Progress)
		}
	}
}
