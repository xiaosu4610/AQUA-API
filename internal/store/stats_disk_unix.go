// 本文件实现 Unix 系（生产环境 Linux，含 darwin）的磁盘容量探测。
//
// 意图（Why）：
//
//	磁盘水位是运维监控的必选项——数据库写满分区时服务会立刻不可用，
//	提前看到剩余空间能争取处置时间。Windows 上没有 syscall.Statfs，
//	因此用 build tag 把平台差异分到两个文件，保证本机（Windows 开发）
//	与生产（Linux）都能编译通过。
//
// 流转（Flow）：
//
//	store.DiskUsage → diskSpace(path) → syscall.Statfs → 返回总/可用字节
//
// 扩展（Extend）：
//
//	需要更多指标（inode 使用率等）时，扩展返回值或新增函数，
//	并同步更新 stats_disk_windows.go 的占位实现，保持两边签名一致。
//
//go:build !windows

package store

import "syscall"

// diskSpace 返回 path 所在文件系统的总容量与可用空间（字节）。
//
// 为什么用 Bavail 而非 Bfree 作为「可用」：Bavail 是普通用户可用的块数，
// 已扣除 root 预留块，比 Bfree 更贴近「服务还能写多少」的真实语义。
func diskSpace(path string) (total, free uint64, ok bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, false
	}
	blockSize := uint64(stat.Bsize)
	return stat.Blocks * blockSize, stat.Bavail * blockSize, true
}
