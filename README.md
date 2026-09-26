<div align="center">

<img src="favicon.ico" width="88" alt="AQUA-API">

# AQUA-API

### 把你的所有 AI 上游，收进一个入口

**自托管 LLM API 网关 · AI 资产（用量）管理系统**

官方 API Key · 云厂商 · 中转站 · 订阅账号 · 本地自建模型
统一协议 · 智能调度 · 精确计费 · 开箱即用的运营后台

## 🌐 官方网站 `https://aqua.ltzy.top`

> 域名如有变更，**以本仓库为准**（先改这里，再同步其它任何地方）。

[![License](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8.svg?logo=go&logoColor=white)](https://go.dev)
[![CGO](https://img.shields.io/badge/CGO-%E9%9B%B6%E4%BE%9D%E8%B5%96-success.svg)](#为什么选它)
[![Deploy](https://img.shields.io/badge/%E9%83%A8%E7%BD%B2-%E5%8D%95%E4%BA%8C%E8%BF%9B%E5%88%B6%20%2F%20Docker-informational.svg)](#-快速开始)
[![Platform](https://img.shields.io/badge/%E5%B9%B3%E5%8F%B0-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey.svg)](#-快速开始)

[**简体中文**](README.md) ｜ [English](README.en.md) ｜ [🏠 在线演示](https://aqua.ltzy.top)

</div>

---

## 🔗 官方地址

| | 地址 |
|---|---|
| **官方网站（在线演示）** | **`https://aqua.ltzy.top`** |
| **主仓库（国内）** | `https://gitee.com/xiaosu4610/AQUA-API` |
| **镜像仓库（海外）** | GitHub，由 Gitee 自动同步 |

**本仓库是官方地址的唯一权威来源。** 域名若变更，会**先在本仓库更新**，再同步到其它任何地方。
所以：**收藏本仓库，比收藏一个域名更可靠。**

### 🛡 防伪提醒

- 本项目**不提供、也未授权**任何"代充""代运营""官方合租"服务。
  服务端代码完全开源（AGPL-3.0），任何人都可以拿去自建 —— **能跑起来 ≠ 是官方**。
- 只认上表中的两个地址。其它域名即使界面一模一样，也与本项目无关。
- 官方不会私聊向你索要账号密码、支付口令或验证码。

### 链接打不开？

这类域名被社交软件（QQ / 微信等）**误报拦截**是常见现象 —— 我们自己就遇到过链接被批量举报。
遇到打不开时：

1. 换浏览器、或切换网络（手机流量 ↔ 家庭宽带）再试；
2. **分享本仓库地址而不是裸域名** —— 代码托管平台的链接被误拦的概率低得多，
   而且对方能从仓库里自己确认最新官网地址；
3. 若确认是误拦，按对应平台的提示提交申诉即可。

---

## 📖 目录

- [🔗 官方地址](#-官方地址)
- [⚠️ 免责声明](#-免责声明)
- [这是什么](#这是什么)
- [为什么选它](#为什么选它)
- [核心特性](#核心特性)
- [支持的协议与上游](#支持的协议与上游)
- [🚀 快速开始](#-快速开始)
- [配置](#配置)
- [接入示例](#接入示例)
- [常见问题](#常见问题)
- [路线图](#路线图)
- [开发](#开发)
- [许可证](#许可证)

---

## ⚠️ 免责声明

本项目是**技术中立的 API 网关软件**，仅提供协议转换、请求路由与用量记账能力。

1. **使用者需自行承担全部合规责任**：包括但不限于遵守你所接入的每一个上游服务商的
   服务条款（ToS）、账号使用政策、地区法律法规与出口管制要求。
2. **请勿用于规避上游的付费机制、额度限制或地域限制**。是否允许池化、转售、共享账号，
   取决于上游服务商的具体条款 —— 那是使用者的判断，不是本软件的功能承诺。
3. 本项目**不提供任何上游账号、密钥或额度**，也不对其可用性、稳定性与合法性作任何担保。
4. 软件按「原样」提供，作者不对因使用本软件产生的任何直接或间接损失负责。

**继续使用即表示你已阅读并同意上述条款。**

---

## 这是什么

AQUA-API 是一个**自托管的 LLM API 网关**，同时是一套**AI 资产（用量）管理系统**。

你手上的上游通常是一团互不兼容的东西：OpenAI 官方 Key、Azure、Claude、Gemini、
各家云厂商、各种中转站、订阅来的账号（Claude / Codex / Gemini），以及本地跑的
Ollama / vLLM。而你的下游是各种应用：Claude Code、Codex CLI、Cursor、自研 App、脚本、插件。

AQUA-API 站在中间，把这一团整理成**一个入口、一套协议、一本清楚的账**：

```
                      ┌──────────────────────────────┐
   Claude Code ─┐     │                              │     ┌─ OpenAI 官方 Key
   Codex CLI  ──┤     │          AQUA-API            │     ├─ Azure OpenAI
   Cursor     ──┼────▶│                              │────▶├─ Anthropic / Gemini
   自研 App   ──┤     │  统一协议 · 智能调度 · 计费   │     ├─ 云厂商 / 中转站
   脚本/插件  ─┘     │  凭据池 · 分组 · 运营后台      │     ├─ 订阅账号（OAuth）
                      └──────────────────────────────┘     └─ 本地 Ollama / vLLM
                        OpenAI / Anthropic / Gemini             按类型自动适配
```

---

## 为什么选它

市面上不缺中转站，缺的是**能安心托付账目**的那一个。下面每一条都是"踩过坑之后"的设计：

| 关注点 | 常见做法 | AQUA-API |
|---|---|---|
| **上游密钥** | 明文存库，后台能看回明文 | **AES-256-GCM 加密落库**，密钥只从环境变量注入；后台被攻破也导不出明文 |
| **加密主密钥** | 一并写进配置文件 | 配置文件里的同名字段**直接被忽略**，只能走环境变量 —— 不可能随仓库泄露 |
| **上游故障** | 连续失败即永久禁用，池子越用越小 | **冷却 + 半开**：限流只是"临时避让"、到期自动恢复；只有上游明确说"凭据已吊销"才摘除 |
| **限流与并发** | 全局一个阈值，一限全限 | **按凭据**独立计（权重 / 优先级 / 每分钟上限 / 在途数），超限自动换下一把，不误伤凭据 |
| **额度** | 请求前后各查一次，并发下能透支 | **预扣 → 结算 → 退还**，可用额度 = 额度 − 已用 − 在途，并发也扣不出负数 |
| **流式计费** | 只读响应前若干字节，长回答**计 0 费** | 增量解析 SSE，`usage` 出现在流的最后一帧也能拿到 |
| **协议** | 只做 OpenAI 兼容 | 下游 **OpenAI / Anthropic / Gemini** 三套协议齐备，上游按渠道类型选适配器 |
| **人群区分** | 密钥即权限，无法区分免费与付费 | **分组**决定可用渠道与价格，**密钥可选分组**：同一份上游，免费人群与付费人群各走各的账 |
| **部署** | 要装数据库、Redis、编译环境 | **单二进制 + SQLite**，前端已内嵌；零 CGO，不需要 gcc |

---

## 核心特性

### 🌐 网关与转发

- **下游三协议**：OpenAI 兼容（`/v1/chat/completions`、`/v1/models`、`/v1/embeddings`）、Anthropic、Gemini
- **上游适配器**：OpenAI 兼容、**Azure OpenAI**（部署名 + api-version）、**Anthropic**、**Gemini**
- **渠道类型目录 79 种**：文本 / 图像 / 视频 / 音频 / 嵌入 / 聚合 / 自建 / 订阅；后台选中类型后**自动展开该类型专属字段**（选 Azure 才出现"部署名 + api-version"）
- **流式双向转换**：上游 Anthropic / Gemini 的 SSE 事件 ↔ OpenAI `chat.completion.chunk`，含工具调用
- **统一中间表示**：内部全部收敛到 OpenAI 协议（N×1），新增上游只写"进"、新增下游只写"出"
- **300 秒上游超时**：大模型长回答不会被掐断
- **错误原样透传**：上游真实错误（含 RFC7807 `detail`、OpenAI `error.message`）直接回传，不吞错

### 🔑 凭据池与智能调度

- **五种策略**：顺序 / 轮询 / 加权随机 / 最久未用 / **最少在途**（默认），渠道级可切换
- **凭据级参数**：权重、优先级、每分钟上限、在途数、冷却截止 —— 后台逐把可调
- **失败分级**：429 / 5xx → 短冷却（指数退避）；401 / 403 / 402 → 长冷却；**明确无效才摘除**；连接层失败不计入
- **会话粘性**：同一会话固定同一把凭据（提升上游缓存命中率），目标失效自动降级并清除绑定
- **多种录入**：单条 / 批量粘贴 / 多合一

### 💰 计费与账务

- **口径**：`额度 = (输入 Token × 输入价 + 输出 Token × 输出价) / 1,000,000`，另支持**按次计价**
- **价格规则**：按模型名或**通配模式**匹配，可挂到分组；内存缓存，改价即时生效
- **额度安全**：预扣 + 结算 + 退还；**不计费模型跳过预扣**（免费模型不会被额度墙挡住）
- **额度语义**：`-1` = 不限；判定用「剩余 ≤ 0」而不是「== 0」，堵住超额透支
- **支付通道**：人工确认 / 易支付 / **Stripe** / **支付宝官方**（RSA2 签名）/ **微信支付官方**（APIv3 + 平台证书验签 + AES-GCM 解密）
- **兑换码**：批量生成；并发兑换是单事务原子扣减（10 个并发抢同一码，只会成功一次）
- **订单账务**：回调验签、幂等入账、人工补单 / 关单 / 退款

### 🛠 运营与后台

- **模型广场**：分面筛选（分组 / 厂商 / 可用状态）+ **分面计数联动** + 搜索 + 排序 + 卡片/列表双视图 + 详情弹窗（价格表 + 可直接运行的 cURL）
- **渠道管理**：增删改、连通性测活、密钥池抽屉、**一键从上游拉取模型列表**
- **分组**：分组实体 + 计费倍率 + **引用统计**（删除前告诉你影响多少渠道与价格）
- **令牌**：额度 / 过期 / 模型白名单 / **所属分组**；明文只在创建时展示一次
- **异步任务**：生成类任务提交、轮询、取消；按次计价；失败自动退还
- **其余后台**：用户、兑换码、订单、调用日志、订阅账号（OAuth）
- **移动端**：底部导航、表格自动降级为卡片、安全区适配、弹窗底部弹出

---

## 支持的协议与上游

**下游（你的应用怎么连我们）**：`OpenAI 兼容` · `Anthropic` · `Gemini`

**上游（我们怎么连别人）**：

| 状态 | 类型 |
|---|---|
| ✅ **已实现** | OpenAI 兼容（覆盖 DeepSeek / Kimi / 智谱 / 通义 / 硅基流动 / OpenRouter / Groq / Together / Mistral / xAI / Ollama / vLLM / 各类中转站）、**Azure OpenAI**、**Anthropic**、**Gemini** |
| 🧭 **已登记待接入** | AWS Bedrock（SigV4）、Google Vertex（服务账号 JWT），以及图像 / 视频 / 音频 / 嵌入类上游 |

> **诚实说明**：目录里已登记但适配器尚未完成的类型，后台会标为「即将支持」并**禁止选中** ——
> 我们不会让你配到一半才发现调不通。

---

## 🚀 快速开始

### 方式一：Docker Compose（推荐，一条命令）

```bash
git clone https://gitee.com/xiaosu4610/AQUA-API.git && cd AQUA-API
cp .env.example .env

docker build -t aqua-api:local .          # 首次构建（前端 + 后端 + 运行镜像）
docker run --rm aqua-api:local -gen-key   # 打印一个主密钥，把它填进 .env 的 AQUA_APP_KEY

docker compose up -d
```

浏览器打开 `http://127.0.0.1:8787`。数据落在宿主机 `./data`，迁移服务器时打包该目录即可。

### 方式二：Docker run（不用 compose）

```bash
docker build -t aqua-api:local .

docker run -d --name aqua-api \
  -p 8787:8787 \
  -e AQUA_APP_KEY="<你的主密钥>" \
  -e AQUA_SERVER_LISTEN=0.0.0.0:8787 \
  -v "$PWD/data:/data" \
  --restart unless-stopped \
  aqua-api:local
```

### 方式三：单二进制（Linux 服务器 / systemd）

```bash
go build -o aqua ./cmd/aqua           # 纯 Go，零 CGO，不需要 gcc

./aqua -gen-key                        # 生成加密主密钥（只生成，不落盘）

sudo useradd -r -s /usr/sbin/nologin aqua
sudo mkdir -p /opt/aqua /etc/aqua /var/lib/aqua
sudo cp aqua /opt/aqua/aqua && sudo chown aqua:aqua /opt/aqua/aqua

sudo cp .env /etc/aqua/aqua.env        # 填入真实密钥
sudo chmod 600 /etc/aqua/aqua.env && sudo chown root:root /etc/aqua/aqua.env

sudo cp aqua-api.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now aqua-api
sudo systemctl status aqua-api
```

**升级**：替换 `/opt/aqua/aqua` 后 `sudo systemctl restart aqua-api`（数据库迁移在启动时自动执行）。

### 方式四：源码直接跑（开发用）

```bash
# 前端（可选：仓库里的 web/dist 为占位，正式界面需构建后才会内嵌）
cd web && npm ci && npm run build && cd ..

go build -o bin/aqua ./cmd/aqua
export AQUA_APP_KEY="<你的主密钥>"      # Windows: $env:AQUA_APP_KEY="..."
./bin/aqua -config ./aqua.json          # 不加 -config 则使用默认值与环境变量
curl http://127.0.0.1:8787/healthz
```

> **前端是内嵌的**：`go:embed` 会把 `web/dist` 打进二进制，所以**部署只需要一个文件**。
> 直接 `go build` 而不构建前端时，界面会是占位页 —— 但 API 完全可用。

### 反向代理

对外提供服务时建议前置 Nginx / Caddy 并启用 HTTPS。两个容易踩的点：

```nginx
location / {
    proxy_pass http://127.0.0.1:8787;
    proxy_http_version 1.1;

    # 1) 流式响应必须关闭缓冲，否则前端要等整段生成完才逐字显示
    proxy_buffering off;
    proxy_cache off;

    # 2) 超时必须大于网关的上游超时（默认 300 秒），否则长回答会被反代先掐断
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;

    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

> 若你的域名挂在 Cloudflare 橙云代理后面，注意 CF 回源有 **100 秒硬上限**，
> 超过就会 524 —— 想完整吃到 300 秒超时，请加一条**灰云（DNS only）**记录直连源站。

---

## 配置

优先级：**默认值 < 配置文件 < 环境变量**。

### 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `AQUA_APP_KEY` | ✅ | 加密主密钥，**只能走环境变量**（配置文件里的同名字段会被忽略）。`aqua -gen-key` 生成 |
| `AQUA_SERVER_LISTEN` | | 监听地址，默认 `127.0.0.1:8787`；容器内须为 `0.0.0.0:8787` |
| `AQUA_SERVER_MODE` | | `debug` / `release` / `test` |
| `AQUA_DATABASE_DRIVER` | | 目前为 `sqlite` |
| `AQUA_DATABASE_DSN` | | SQLite 文件路径，默认 `./data/aqua.db`（父目录自动创建） |
| `AQUA_RELAY_GROUP` | | 网关**默认分组**（不带分组的令牌走哪个分组），默认 `default` |
| `AQUA_SMTP_*` | | 邮件发信（注册验证码 / 通知） |
| `AQUA_EPAY_KEY`、`AQUA_STRIPE_SECRET_KEY`、`AQUA_STRIPE_WEBHOOK_SECRET`、`AQUA_ALIPAY_PRIVATE_KEY`、`AQUA_ALIPAY_PUBLIC_KEY`、`AQUA_WECHATPAY_APIV3_KEY`、`AQUA_WECHATPAY_PRIVATE_KEY`、`AQUA_WECHATPAY_PLATFORM_PUBLIC_KEY` | | 各支付通道密钥 |
| `AQUA_LOG_LEVEL` / `AQUA_LOG_FORMAT` | | `debug`/`info`/`warn`/`error`，`text`/`json` |

完整样例见 [`.env.example`](.env.example)。

### 配置文件

```json
{
  "server":   { "listen": "127.0.0.1:8787", "mode": "release" },
  "database": { "driver": "sqlite", "dsn": "./data/aqua.db" },
  "log":      { "level": "info", "format": "text" }
}
```

### 两条安全铁律

1. **密钥类配置一律不入库**。支付、SMTP、加密主密钥都只能从环境变量注入；
   只有运营参数（网关地址、商户号、汇率、限额、开关）进数据库、后台可改。
   这样即使数据库被完整拖走，也拿不到任何一把能直接用的凭据。
2. **主密钥务必单独备份**。它一变，库里所有上游密钥就再也解不开了（只能重新录入）。

---

## 接入示例

任何 OpenAI 兼容客户端，把 Base URL 指过来、Key 换成 AQUA-API 的令牌即可。

### curl

```bash
curl https://你的域名/v1/chat/completions \
  -H "Authorization: Bearer sk-你的令牌" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "你的模型名",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'
```

### OpenAI SDK（Python）

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://你的域名/v1",
    api_key="sk-你的令牌",
)
resp = client.chat.completions.create(
    model="你的模型名",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

### Claude Code / Anthropic 客户端

AQUA-API 原生支持 Anthropic 协议，因此可以直接接管 Claude Code 的流量：

```bash
export ANTHROPIC_BASE_URL=https://你的域名
export ANTHROPIC_AUTH_TOKEN=sk-你的令牌
claude
```

### 其他客户端

Cursor、Codex CLI、Cherry Studio、NextChat、LobeChat、沉浸式翻译等，选择
「OpenAI 兼容 / 自定义 OpenAI 接口」，填入上面的 Base URL 与令牌即可。

---

## 常见问题

**Q：`/healthz` 返回 503 怎么办？**
数据库不可用时就会 503。看日志里的数据库错误；SQLite 场景优先检查数据目录权限。

**Q：为什么后台看不到渠道密钥明文？**
这是刻意设计。密钥以 AES-256-GCM 密文落库，界面只显示掩码 —— 后台被攻破也导不出可用凭据。
需要更换时直接覆盖写入新密钥即可。

**Q：把渠道换到新分组后，所有令牌都报「无可用渠道」？**
这是最容易踩的坑。网关有一个**默认分组**（`AQUA_RELAY_GROUP`），决定"不带分组的令牌"去哪找渠道。
渠道迁到新分组后，必须同步改这个默认分组并重启，否则老令牌会立刻失联。
（本次开发中我们自己也踩过，所以专门写进 FAQ 和迁移文档。）

**Q：免费模型为什么还是被额度挡住？**
未命中任何价格规则的模型会**跳过预扣**，正常不应被挡。若被挡，检查分组下是否配了
**通配价格规则**（例如 `*`）—— 那会让模型变成"有价"，从而走额度判定。

**Q：上游返回 429 / 超时频繁？**
429 属于**凭据级**失败：会换密钥重试并把该密钥置入短冷却（指数退避，到期自动恢复）。
若频发，通常是密钥太少或上游限速低，去渠道里加密钥或调低单密钥的每分钟上限。

**Q：数据怎么备份？**
SQLite 场景下：停服务（或用 `VACUUM INTO` 热备）→ 拷 `aqua.db` → **同时备份 `AQUA_APP_KEY`**。
少了主密钥，备份里的上游密钥就是一堆无法解密的字节。

**Q：支持 MySQL / PostgreSQL 吗？**
当前默认且仅支持 SQLite，已能覆盖自托管与中小规模场景。存储层已经留出方言接缝
（迁移目录按方言分目录、驱动注册表带字段元数据），后续接入不需要重写业务层。

**Q：怎么新增一个上游渠道类型？**
在 `internal/channeltype/catalog.go` 登记类型元数据（默认地址、鉴权方式、额外必填参数、请求路径模板、能力位）。
如果它属于已有的协议族（如 OpenAI 兼容），登记完即可用；协议不同则需在 `internal/relay/` 加一个适配器。

**Q：为什么日志里看不到我配置的上游密钥？**
同样是刻意设计：日志只输出"是否已注入凭据"与上游主机与路径，**连 URL 的查询串都不输出**
（防止把走查询参数的密钥打出来）。

---

## 路线图

- [x] 协议互转（OpenAI ↔ Anthropic ↔ Gemini），流式双向转换含工具调用
- [x] 凭据池五种调度策略、冷却半开、会话粘性、在途计数
- [x] 预扣 / 结算 / 退还的额度体系；流式用量增量解析
- [x] 分组与倍率、模型广场、兑换码、五种支付通道、异步任务
- [x] 单二进制 + Docker 部署，前端内嵌
- [ ] AWS Bedrock / Google Vertex 签名鉴权
- [ ] 图像 / 视频 / 音频类上游适配器
- [ ] 模型 ID 映射的**转发层接入**（后台已可配置）
- [ ] 独立管理后台入口 + 浏览器安装向导
- [ ] 订阅账号的配额窗口（按 5 小时 / 日 / 周自动恢复）
- [ ] 管理操作审计、公告与 FAQ、代理池

---

## 开发

```bash
go build ./...       # 编译
go test ./...        # 测试
gofmt -w .           # 格式化

cd web && npm ci && npm run type-check && npm run build   # 前端
```

### 工程约定（强制）

1. **小步提交** —— 每个可独立描述的小步骤完成后立刻提交，禁止攒到最后一次性提交；
   每次提交都应是可编译、可回滚的状态。
2. **注释面向 AI 友好** —— 每个源文件头部必须写清「**意图 / 流转 / 扩展**」三段，
   让任何人（或 AI）30 秒内看懂这个文件干什么、数据怎么流转、往哪儿扩展。
3. **密钥不入库、不落盘、不进日志** —— 见上文「两条安全铁律」。

详见 [`AGENTS.md`](AGENTS.md)。

### 目录结构

```
cmd/aqua/              程序入口（仅装配，不含业务逻辑）
internal/config/       配置加载与校验
internal/model/        领域模型与仓储接口（不含 SQL）
internal/store/        持久化实现（SQL + 版本化迁移，按方言分目录）
internal/server/       HTTP 层（路由 / 中间件 / 处理器）
internal/relay/        协议适配与转发（核心域）
internal/payment/      支付通道适配
internal/channeltype/  上下游类型注册表
web/                   前端（构建产物内嵌进二进制）
Dockerfile             多阶段构建：前端 → 后端 → 极简运行镜像
aqua-api.service       systemd 单元（裸机部署）
```

---

## 许可证

源代码采用 [**GNU AGPL-3.0**](LICENSE)。

> **如果把它作为网络服务对外提供**，AGPL 第 13 条要求你向使用者提供对应源代码。
> 自用或内部部署不受此约束，但请保留版权声明。

品牌资产（`favicon.ico`、"AQUA-API" 名称与标识）不在源代码许可授权范围内，详见 [NOTICE](NOTICE)。

---

<div align="center">

**如果这个项目帮你省下了对账的时间，欢迎点个 Star ⭐**

[🏠 在线演示](https://aqua.ltzy.top) ｜ [🐛 提交 Issue](https://gitee.com/xiaosu4610/AQUA-API/issues) ｜ [📖 English](README.en.md)

</div>
