// Package version 集中管理构建版本信息。
//
// 意图（Why）：
//
//	版本信息会被 HTTP 健康检查、启动日志与命令行 --version 共同使用。
//	集中在一处可避免多处硬编码导致的不一致（例如日志显示 v1.0 而健康检查显示 dev）。
//
// 流转（Flow）：
//
//	构建时通过 ldflags 注入 → version.Get() → server 健康检查 / main 启动日志
//
// 扩展（Extend）：
//
//	新增构建元信息（如构建者、Go 版本）时，在下方 var 与 Info 结构体中同步添加，
//	并更新 Makefile / 构建脚本中的 ldflags。
//
// 注入示例：
//
//	go build -ldflags "-X gitee.com/xiaosu4610/aqua-api/internal/version.Version=v1.0.0 \
//	                   -X gitee.com/xiaosu4610/aqua-api/internal/version.GitCommit=abc1234" \
//	         ./cmd/aqua
package version

// 以下变量刻意使用 var 而非 const：只有变量才能被链接器通过 -ldflags 覆盖。
var (
	// Version 为语义化版本号。默认值 "dev" 表示未经正式构建流程的本地编译产物。
	Version = "dev"
	// BuildTime 为构建时间（RFC3339 格式），由构建脚本注入。
	BuildTime = "unknown"
	// GitCommit 为构建对应的提交哈希，便于线上问题定位到具体代码版本。
	GitCommit = "unknown"
)

// Info 描述一份完整的版本信息，便于序列化为 JSON 输出。
type Info struct {
	Version   string `json:"version"`    // 语义化版本号
	BuildTime string `json:"build_time"` // 构建时间
	GitCommit string `json:"git_commit"` // 提交哈希
}

// Get 返回当前二进制的版本信息。
func Get() Info {
	return Info{
		Version:   Version,
		BuildTime: BuildTime,
		GitCommit: GitCommit,
	}
}
