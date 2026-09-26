// Package aqua 是模块根包，唯一职责是承载前端构建产物的嵌入声明。
//
// 意图（Why）：
//
//	Go 的 //go:embed 指令【不能引用父目录】（模式中不允许出现 ".."），
//	因此嵌入声明必须写在包含目标目录的包内。前端产物位于仓库根的 web/dist，
//	故把该声明放在根包（与 go.mod 同级）是唯一可行且最自然的位置。
//
// 流转（Flow）：
//
//	构建：cd web && npm run build  →  产出 web/dist/*
//	编译：本文件把 web/dist 嵌入二进制
//	  └─ cmd/aqua/main.go 读取 aqua.WebDist → 传给 server 注册静态路由
//
// 扩展（Extend）：
//
//	若将来前端产物改到其他目录（例如 internal/webui/dist），
//	应把本文件迁移到该目录并删除根包，避免根目录堆砌 Go 文件。
package aqua

import "embed"

// WebDist 是前端构建产物（web/dist）的嵌入文件系统。
//
// 注意：
//   - 使用 all: 前缀是为了把以 "." 或 "_" 开头的文件也纳入嵌入
//     （默认规则会跳过它们，将来若产物中出现这类文件会静默丢失）；
//   - web/dist 下必须至少存在一个可嵌入文件，否则编译失败。
//     仓库中保留了 web/dist/PLACEHOLDER.txt 作为占位，保证未构建前端时也能编译通过。
//
//go:embed all:web/dist
var WebDist embed.FS
