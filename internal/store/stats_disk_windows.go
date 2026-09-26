// 本文件是 Windows 平台下磁盘容量探测的占位实现。
//
// 意图（Why）：
//
//	标准库 syscall 在 Windows 上没有 Statfs，若把探测逻辑写在同一文件里，
//	Windows 开发机将无法编译。生产环境是 Linux，含 syscall.Statfs 的实现见
//	stats_disk_unix.go（build tag !windows）。本文件只在 Windows 编译，
//	明确返回「不可用」，让上层省略磁盘字段，而不是给出错误的 0。
//
// 流转（Flow）：
//
//	store.DiskUsage → diskSpace(path) → 恒返回 ok=false → 概览省略磁盘字段
//
// 扩展（Extend）：
//
//	若将来需要在 Windows 上提供真实水位，可用 golang.org/x/sys/windows 的
//	GetDiskFreeSpaceEx 实现本函数；注意项目约束「零新增第三方依赖」，
//	引入前需评估取舍。
//
//go:build windows

package store

// diskSpace 在 Windows 上不提供真实取值（返回 ok=false）。
//
// 参数 path 仅用于保持与 Unix 实现一致的签名；本实现不读取它。
func diskSpace(path string) (total, free uint64, ok bool) {
	_ = path
	return 0, 0, false
}
