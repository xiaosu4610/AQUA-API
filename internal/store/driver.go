// 本文件是数据库驱动注册表：既供 store.Open 分派连接实现，
// 也供安装向导渲染「选驱动 → 展开对应填写框」的触发式表单。
//
// 意图（Why）：
//
//	安装向导不能让使用者面对一堆输入框——小白站长不知道哪个字段属于哪个数据库，
//	平铺只会让人填错。因此驱动的「显示名 + 需要的字段」由后端统一描述：
//	前端只负责「选中哪个驱动，就渲染它的 Fields」，不需要在前端硬编码字段，
//	将来新增驱动也不必改前端。
//
//	同一份元数据还承担「可用性」声明：连接实现尚未落地的驱动标记为
//	Available=false，安装向导据此不展示该选项，避免用户选了却装不上。
//
// 流转（Flow）：
//
//	install 接口（GET /api/install/state）→ Drivers() → 前端渲染驱动卡片与字段
//	store.Open(driver, dsn) → 按 Key 分派到具体连接实现
//
// 扩展（Extend）：
//
//	新增数据库：在下列驱动常量与 Drivers() 里补一条（含 Fields），
//	实现 open 函数并把 Available 置为 true，同时新增 migrations/<driver>/ 目录。
package store

import (
	"fmt"
	"strings"
)

// 驱动名常量。取值同时用于配置文件（database.driver）与环境变量，改动会导致旧配置失效。
const (
	DriverSQLite   = "sqlite"
	DriverMySQL    = "mysql"
	DriverPostgres = "postgres"
)

// DriverField 描述安装向导里「一个输入框」。
//
// 为什么由后端描述字段：字段名、必填与否、是否敏感、默认值都属于「连接该数据库需要什么」，
// 这是后端才知道的知识；放前端会出现「后端加了字段、前端忘了加输入框」的静默漏配。
type DriverField struct {
	// Key 字段标识，前端按它回传（如 path / host / password）。
	Key string
	// Label 中文标签，直接展示给使用者。
	Label string
	// Placeholder 输入框占位提示，通常是一个具体示例。
	Placeholder string
	// Help 一行说明，讲清「这个值去哪儿找 / 留空会怎样」。
	Help string
	// Default 默认值（为空表示没有默认值）。
	Default string
	// Required 是否必填；非必填字段留空时由连接实现填默认值。
	Required bool
	// Secret 是否敏感字段（密码类），前端按密码框渲染且不回显。
	Secret bool
}

// DriverInfo 描述一种数据库驱动。
type DriverInfo struct {
	// Key 驱动标识，写入配置文件并用于 Open 分派。
	Key string
	// Label 展示名（如「SQLite（免配置）」）。
	Label string
	// Description 一句话说明适用场景，帮助使用者选对。
	Description string
	// Available 为 false 表示连接实现尚未落地：安装向导不展示该选项。
	Available bool
	// Fields 选定该驱动后才需要填写的字段（触发式渲染）。
	Fields []DriverField
}

// Drivers 返回全部已在册的驱动。
//
// 注意顺序：把可用且推荐的放前面，安装向导通常默认选中第一个。
func Drivers() []DriverInfo {
	return []DriverInfo{
		{
			Key:         DriverSQLite,
			Label:       "SQLite（免配置）",
			Description: "单文件数据库，随程序一起跑，无需单独安装数据库服务。个人站点与中小流量推荐。",
			Available:   true,
			Fields: []DriverField{
				{
					Key:         "path",
					Label:       "数据文件路径",
					Placeholder: "./data/aqua.db",
					Help:        "目录不存在会自动创建。建议放在数据盘上并纳入备份。",
					Default:     "./data/aqua.db",
					Required:    true,
				},
			},
		},
		{
			Key:         DriverMySQL,
			Label:       "MySQL / MariaDB",
			Description: "需要自行准备数据库服务。适合多实例部署与已有数据库运维体系的场景。",
			// 连接实现与迁移脚本尚未落地，先不提供给使用者选择。
			Available: false,
			Fields: []DriverField{
				{Key: "host", Label: "主机", Placeholder: "127.0.0.1", Default: "127.0.0.1", Required: true},
				{Key: "port", Label: "端口", Placeholder: "3306", Default: "3306", Required: true},
				{Key: "database", Label: "数据库名", Placeholder: "aqua", Required: true},
				{Key: "username", Label: "用户名", Placeholder: "aqua", Required: true},
				{Key: "password", Label: "密码", Required: false, Secret: true},
			},
		},
		{
			Key:         DriverPostgres,
			Label:       "PostgreSQL",
			Description: "需要自行准备数据库服务。适合已在使用 PostgreSQL 的团队。",
			Available:   false,
			Fields: []DriverField{
				{Key: "host", Label: "主机", Placeholder: "127.0.0.1", Default: "127.0.0.1", Required: true},
				{Key: "port", Label: "端口", Placeholder: "5432", Default: "5432", Required: true},
				{Key: "database", Label: "数据库名", Placeholder: "aqua", Required: true},
				{Key: "username", Label: "用户名", Placeholder: "aqua", Required: true},
				{Key: "password", Label: "密码", Required: false, Secret: true},
			},
		},
	}
}

// AvailableDrivers 只返回当前可选的驱动（安装向导用它渲染选项）。
func AvailableDrivers() []DriverInfo {
	all := Drivers()
	result := make([]DriverInfo, 0, len(all))
	for _, info := range all {
		if info.Available {
			result = append(result, info)
		}
	}
	return result
}

// FindDriver 按 Key 查找驱动元数据。
func FindDriver(key string) (DriverInfo, bool) {
	for _, info := range Drivers() {
		if info.Key == key {
			return info, true
		}
	}
	return DriverInfo{}, false
}

// DriverKeys 返回全部在册驱动名，用于错误信息里提示可选值。
func DriverKeys() []string {
	all := Drivers()
	keys := make([]string, 0, len(all))
	for _, info := range all {
		keys = append(keys, info.Key)
	}
	return keys
}

// AvailableDriverNames 返回可选驱动的展示名串，用于「都不可用时」的提示。
func AvailableDriverNames() string {
	list := AvailableDrivers()
	names := make([]string, 0, len(list))
	for _, info := range list {
		names = append(names, fmt.Sprintf("%s(%s)", info.Label, info.Key))
	}
	return strings.Join(names, "、")
}
