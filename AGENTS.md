# AGENTS.md —— AI Agent 与协作者代码导读

> 本文件面向 **AI 编程助手** 与 **人类开发者**，目的是让你在最短时间内理解本项目
> 的结构、约定与扩展方式。**修改代码前请先读完本文件。**

---

## 一、项目一句话

AQUA-API 是自托管的 LLM API 网关：统一上游（API Key / 订阅账号 / 自托管模型）与下游协议，
负责路由、计费与运营。

## 二、代码地图（按数据流向）

```
请求进入
  │
  ├─ cmd/aqua/main.go            程序入口：加载配置 → 初始化依赖 → 启动服务
  │
  ├─ internal/config/            配置加载（默认值 → 文件 → 环境变量）
  │
  ├─ internal/server/            HTTP 层
  │     ├── server.go             引擎与中间件装配
  │     ├── router.go             路由注册
  │     └── health.go             健康检查
  │
  ├─ internal/relay/             核心域：协议适配与转发
  │     ├── relay.go              转发编排（选渠道 → 转换 → 调用 → 回写）
  │     └── openai.go             OpenAI 兼容协议实现（M1 为透传）
  │
  ├─ internal/model/             领域模型 + 仓储接口（不含 SQL）
  │     └── channel.go            Channel 实体与 ChannelRepository 接口
  │
  └─ internal/store/             持久化实现
        ├── store.go             连接管理 + 迁移执行
        ├── schema.sql           建表语句（嵌入二进制）
        └── channel_repo.go      ChannelRepository 的 SQL 实现
```

## 三、注释约定（必须遵守）

**每个 Go 源文件头部必须有如下注释块**，三段缺一不可：

```go
// Package xxx 一句话说明本包职责。
//
// 意图（Why）：
//   为什么需要这个包、解决什么问题。
//
// 流转（Flow）：
//   调用方 → 本包 → 下游，写清数据与依赖的走向。
//
// 扩展（Extend）：
//   新增功能时应该改哪些文件、注意哪些联动点。
package xxx
```

其他要求：
- 导出符号（类型/函数/常量）必须有注释，首行以符号名开头。
- 关键或非直觉逻辑写「为什么」，不要复述代码字面意思。
- `TODO(模块): 说明 —— 原因/计划` 为统一格式。
- 注释语言：中文。

## 四、强制工程纪律

1. **小步提交**：每个可独立描述的小步骤完成后**立即** `git commit`。
   - 提交前必须 `go build ./...` 通过；逻辑变更必须 `go test ./...` 通过。
   - 一次提交只做一件事；禁止 `git add -A`（避免误入敏感文件）。
   - 提交信息格式：`<类型>(<范围>): <说明>`，如 `feat(config): 新增配置加载与校验`。
2. **禁止提交敏感信息**：`.private/`、密钥、证书、`.db`、真实生产配置。
3. **原创性红线**：不得复制任何参考项目的源代码、注释、常量表或命名风格；
   仅可借鉴功能需求与算法思想。实现时不要回看参考项目源码（`opensource/` 目录）。
4. **依赖约束**：本机 **无 gcc、CGO 不可用**，只能引入纯 Go 依赖
   （SQLite 使用 `modernc.org/sqlite`，不要用 `mattn/go-sqlite3`）。

## 五、分层与依赖约束

```
cmd  →  config / server / store / model / relay
server → relay / model
relay  → model（仓储接口）
store  → model（实现其接口）
```

- `internal/` 各包之间**禁止循环依赖**。
- `model` 层不感知 HTTP；`server` 层不直接写 SQL。
- 新增仓储：在 `model` 定义接口 → 在 `store` 实现 → 在 `main.go` 注入。

## 六、扩展指南

| 想做什么 | 改哪里 |
|---|---|
| 新增配置项 | `internal/config/config.go`：结构体字段 + 默认值 + 环境变量映射 + 校验（四处同步） |
| 新增数据表 | `internal/store/schema.sql` 加建表语句 + 在 `model/` 定义实体 + 在 `store/` 写仓储 |
| 新增 HTTP 接口 | `internal/server/router.go` 注册 + 处理器实现 |
| 新增上游渠道 | `internal/relay/` 下新增协议实现，并在转发编排处注册（M2 起引入渠道类型注册表） |
| 新增协议转换 | `internal/relay/` 下实现转换器（M2 起引入转换器注册表） |

## 七、验证方式

```bash
go build ./...     # 必须通过
go test ./...      # 必须通过
go vet ./...       # 建议
gofmt -l .         # 应无输出（表示已格式化）
```

## 八、常见误区

- ❌ 直接 `git add .` —— 会带入忽略规则之外的临时文件。
- ❌ 在 `server` 层写 SQL —— 破坏分层，应走 `model` 接口。
- ❌ 用 `mattn/go-sqlite3` —— 需要 CGO，本机编译失败。
- ❌ 照抄 `opensource/` 里的实现 —— 违反原创性红线（法律风险）。
