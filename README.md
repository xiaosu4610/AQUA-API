# AQUA-API

> 新一代 AI 资产网关 —— 把官方 API Key、云厂商、订阅账号、自托管模型统一接入一个池子，
> 提供统一入口、智能调度、精确计费与运营能力。

![品牌图标](favicon.ico)

## 这是什么

AQUA-API 是一个自托管的 **LLM API 网关（AI Gateway）**：上游是各种大模型服务，下游是你的应用
（Claude Code / Codex CLI / Cursor / 自研 App / 脚本）。它负责把请求按协议统一、按策略路由、
按用量计费，并让站长或团队可以运营化管理。

一句话：**一个入口，接管你所有的 AI 上游与用量。**

## 核心特性（规划与进展）

| 能力 | 状态 |
|---|---|
| 配置系统（默认值 → 配置文件 → 环境变量 三级覆盖） | ✅ M1 |
| SQLite 存储与自动迁移（纯 Go 驱动，零 CGO） | ✅ M1 |
| 渠道密钥 AES-256-GCM 加密存储（禁止明文落库） | ✅ M1 |
| 渠道领域模型与仓储 | ✅ M1 |
| HTTP 服务与健康检查 | ✅ M1 |
| OpenAI 兼容协议透传（含 SSE 流式逐片转发） | ✅ M1 |
| 多渠道路由与负载均衡 | ⏳ M2 |
| 协议互转（OpenAI ↔ Anthropic ↔ Gemini） | ⏳ M2 |
| 令牌鉴权与额度管理 | ⏳ M2 |
| 订阅账号池化（OAuth） | ⏳ M3 |
| 异步任务（Midjourney / 视频） | ⏳ M4 |
| 计费与支付 | ⏳ M5 |

> 详细路线图见项目内部文档（`docs/`，不随仓库发布）。

## 快速开始

```bash
# 1. 构建
go build -o bin/aqua ./cmd/aqua

# 2. 生成加密主密钥（渠道密钥以密文落库，必须提供主密钥）
./bin/aqua -gen-key

# 3. 注入主密钥（切勿写入配置文件或提交进仓库）
#    Linux / macOS :  export AQUA_APP_KEY=<上一步生成的密钥>
#    Windows       :  $env:AQUA_APP_KEY="<上一步生成的密钥>"

# 4. 运行（默认监听 127.0.0.1:8787，数据落在 ./data）
./bin/aqua

# 5. 健康检查
curl http://127.0.0.1:8787/healthz
```

指定配置文件：`./bin/aqua -config ./aqua.json`

## 配置

优先级：**默认值 < 配置文件 < 环境变量**。

### 环境变量（全部以 `AQUA_` 为前缀）

| 变量 | 必填 | 说明 |
|---|---|---|
| `AQUA_APP_KEY` | ✅ | 加密主密钥，用于加密渠道密钥。**只能通过环境变量提供**，配置文件中的同名字段会被忽略。生成命令：`aqua -gen-key` |
| `AQUA_SERVER_LISTEN` | | 监听地址，默认 `127.0.0.1:8787` |
| `AQUA_SERVER_MODE` | | `debug` / `release` / `test` |
| `AQUA_DATABASE_DRIVER` | | 目前仅支持 `sqlite` |
| `AQUA_DATABASE_DSN` | | SQLite 文件路径，默认 `./data/aqua.db`（父目录会自动创建） |
| `AQUA_LOG_LEVEL` | | `debug` / `info` / `warn` / `error` |
| `AQUA_LOG_FORMAT` | | `text` / `json` |

### 配置文件（不含任何密钥）

```json
{
  "server": { "listen": "127.0.0.1:8787", "mode": "release" },
  "database": { "driver": "sqlite", "dsn": "./data/aqua.db" },
  "log": { "level": "info" }
}
```

> 安全设计：`security.appKey` 的 json tag 为 `-`，即使被写进配置文件也不会被读取，
> 从而避免密钥随仓库泄露。主密钥须妥善备份——一旦变更，已加密的渠道密钥将无法解密。

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 服务信息（名称 / 版本 / 构建信息） |
| GET | `/healthz` | 健康检查，含数据库连通性探测；依赖不可用时返回 503 |
| POST | `/v1/chat/completions` | OpenAI 兼容对话接口（当前为透传，鉴权在 M2 加入） |

错误响应遵循 OpenAI 格式，便于现有 SDK 直接解析：

```json
{"error":{"message":"当前没有可用的上游渠道能处理该模型","type":"server_error","code":"no_available_channel"}}
```

## 开发

```bash
go build ./...      # 编译
go test ./...       # 测试
gofmt -w .          # 格式化
```

### 工程约定（重要）

本项目遵循两条强制规范，详见 [`AGENTS.md`](AGENTS.md)：

1. **小步提交**：每个可独立描述的小步骤完成后立即提交一次，禁止攒到最后一次性提交。
2. **注释面向 AI 友好**：每个源文件头部必须包含「意图 / 流转 / 扩展」三段说明。

## 目录结构

```
cmd/aqua/          程序入口（仅装配，无业务逻辑）
internal/config/   配置加载与校验
internal/model/    领域模型与仓储接口
internal/store/    持久化实现（SQL）
internal/server/   HTTP 服务（路由/中间件/处理器）
internal/relay/    协议适配与请求转发
docs/              内部文档（不发布）
opensource/        参考项目（不发布）
```

## 许可证

源代码采用 [Apache License 2.0](LICENSE)。
品牌资产（`favicon.ico`、"AQUA-API" 名称与标识）不在源代码许可授权范围内，
详见 [NOTICE](NOTICE)。
