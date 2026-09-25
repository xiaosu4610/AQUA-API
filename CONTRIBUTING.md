# 参与开发

## 仓库分工

| 仓库 | 定位 |
| --- | --- |
| **Gitee** `gitee.com/xiaosu4610/aqua-api-php` | **唯一真相源**。所有开发、Issue、Pull Request 都在这里 |
| GitHub `github.com/xiaosu4610/AQUA-API-PHP` | **只读镜像**，由 Gitee 的推送镜像自动同步。请不要在这里提 PR —— 它不会回流到 Gitee，最后会变成两份分裂的代码 |

## 分支模型

| 分支 | 用途 | 谁能写 |
| --- | --- | --- |
| `main` | 稳定代码，与发行版（tag）对应 | **只接受 Pull Request**，不接受任何直推、不接受强推 |
| `dev` | 日常集成分支 | 同上，走 PR |
| `feat/*`、`fix/*`、`docs/*` | 你自己的功能分支，随便推 | 任何人 |

发布流程：`feat/*` → PR → `dev` → 验证通过后 PR 到 `main` → 打 tag。

## 提交 Pull Request 的步骤

1. Fork 本项目（或在获得协作者权限后直接从 `dev` 开分支）
2. `git switch -c feat/你的功能 dev`
3. 提交前自测：`D:\php84\php.exe -l <改过的 php 文件>`；
   与渠道/计费/安全相关的改动请附上可复现的验证步骤
4. 推到你自己的分支（**不要**推 `main` / `dev`）
5. 在 **Gitee** 上发起 Pull Request 到 `dev`，写清楚「改了什么、为什么、怎么验证」

## 硬性规则

- **不要直推 `main` / `dev`**，也不要对任何已推送分支做 `--force`。
  本地护栏（可选启用）：`git config core.hooksPath .githooks`
- **不要把任何凭据写进代码、配置、文档或提交信息** ——
  上游 API Key 存数据库（AES-256-GCM 加密），服务器口令、SMTP 口令、Git 令牌一律只存在于
  服务器 `.env`（权限 600）或系统凭据管理器里。凭据一旦进入仓库就等于向全世界公开。
- 一个 PR 只做一件事。大改动请先开 Issue 说明设计与影响面。

## 代码风格

- 注释写「为什么」，不写「是什么」：例如「为什么这里必须立即中止」比「遍历模型」有用得多
- 用户可见的文案一律中文，且不要用 Markdown 语法（页面不渲染 `**加粗**`）
- 时间统一存 Unix 时间戳（INTEGER），展示时才格式化
