# agentwork

> [English](README.md) | [Design](DESIGN.md) | [设计 (中文)](DESIGN.zh.md)

多协议 AI Agent 任务管理与调度平台。统一管理 CLI Agent（Claude Code、Codex、
OpenCode、自定义 Agent）—— 创建 Goal、分配给 Agent 或 Squad，由守护进程自动调度
和执行 Run。

**单进程、单机、单用户、无需认证。** 本地 SQLite 持久化，进程内事件总线。开箱即
用：守护进程、CLI 工具、Next.js Web 界面。

- **多协议** — ACP、JSONL、JSON-RPC
- **多传输** — stdio、WebSocket、TCP
- **Goal/Run 两层架构** — Goal 持有状态权威，Run 是执行记录
- **Squad 协作** — 将 Agent 编组为 Squad，由 Leader 统一分配
- **Cron 定时调度** — 按计划周期性创建 Goal
- **实时 Web UI** — 通过 WebSocket 推送实时事件流

> **一句话脑图：** Goal 描述"要做什么、谁负责、进展如何"；Run 记录"某次执行由
> 谁做的、结果怎样"。同一个 Goal 可以被执行多次、在不同 Agent 间交接，完整历史
> 全部保留。Run 没有状态决定权 —— 状态由 Goal 层统一仲裁。

## 快速开始

### 环境要求

- Go 1.26+
- Node.js 20+
- npm 10+

### 后端

```bash
# 构建
go build -o agentwork-daemon ./cmd/agentwork-daemon
go build -o agentwork-cli ./cmd/agentwork-cli

# 启动守护进程（默认 :7373）
./agentwork-daemon
```

### 前端

```bash
cd web
npm install
npm run dev
```

打开 **http://localhost:3000**，通过 Web 界面创建你的第一个 Runtime、Agent 和
Goal。

![Goal 列表](docs/images/goal-list.png)

## 核心概念

| 概念 | 说明 |
|---|---|
| **Runtime** | 启动规格 —— 如何连接一个讲某种协议的程序。`transport`（stdio/ws/tcp）+ `executable`+`args` 或 `endpoint` + `env`。 |
| **Provider** | 协议类型 —— Agent 讲哪种线协议。`acp` / `jsonl` / `jsonrpc`。Runtime 的一个字段。 |
| **Agent** | Runtime + 人设（system_prompt / model / workdir / max_concurrent）。并发单元：每个 Agent 有一个 Worker + 信号量。 |
| **Squad** | 路由组，自己不干活。有一个 Leader（某个 Agent）。把 Goal 分配给 Squad / @mention Squad 时路由到 Leader，由 Leader 分配子 Goal。 |
| **Goal** | 工作项（产品平面）。可分配给 agent / squad / human。有状态机和可选的 `parent_id` 用于子 Goal 协调。**状态权威的唯一持有者。** |
| **Run** | 一次执行（执行平面）：某个 Agent 对某个 Goal 的一次执行。到终态后向 Goal 层报告 —— 绝不直接写 Goal 状态。 |

## 架构

```
┌──────────────────────────────────────────────────────────┐
│  agentwork-daemon (单进程)                                 │
│                                                           │
│  HTTP API + WS hub  ──→  service layer  ──→  store(SQLite) │
│        │                       │                           │
│        │                       │ bus.Publish (事务提交后)    │
│        ▼                       ▼                           │
│      WS 广播              daemon 调度器                     │
│  (前端 / CLI)             claim run → runTask              │
│                                  │  by runtime.provider     │
│                                  ▼                          │
│                           runtime.Open(spec)               │
│                                  │  返回 transport R/W       │
│                                  ▼                          │
│                           backend.Execute(Session)         │
│                                  │  acp|jsonl|jsonrpc        │
│                                  ▼                          │
│                           agent CLI 子进程                  │
│                                  │ 调用 agentwork-cli       │
│                                  ▼  (+ env: SERVER_URL…)    │
└──────────────────────────────────┼─────────────────────-─-┘
                                   ▼
                          agentwork-cli (Agent 侧工具)
```

## CLI 工具

`agentwork-cli` 是 Agent 侧工具。守护进程会将其注入到每个 Agent 子进程中，Agent
通过调用它来产生结构化的副作用。

| 命令 | 说明 |
|---|---|
| `goal list` | 列出所有 Goal（JSON） |
| `goal create --title T [--description D] [--assignee A] [--parent P] [--status S]` | 创建子 Goal |
| `goal assign <目标-agent-id> [--note N]` | 将当前 Goal 交接给另一个 Agent |
| `goal comment --text T [--role R]` | 发表评论；可包含 `[@Name](mention://agent/<id>)` 来在该 Agent 上创建 Run |
| `goal wait` | 将当前 Goal 标记为等待子 Goal 完成 |
| `agent list` | 列出所有 Agent（JSON） |
| `squad list` | 列出所有 Squad（JSON） |

守护进程为每个 Agent 子进程设置以下环境变量：
`AGENTWORK_SERVER_URL`、`AGENTWORK_GOAL_ID`、`AGENTWORK_RUN_ID`、
`AGENTWORK_AGENT_ID`。

## 技术栈

| 层 | 技术 |
|---|---|
| **后端** | Go 1.26, SQLite (modernc), gorilla/websocket, robfig/cron |
| **前端** | Next.js 16, React 19, Tailwind CSS 4, TanStack React Query |
| **协议** | ACP, JSONL, JSON-RPC |
| **传输** | stdio, WebSocket, TCP |
