# agentwork

> [English](README.md) | [设计文档](DESIGN.md) | [协作模型](Collaboration.v2.md)

**AI 干活的流水线操作系统**——单用户控制面，让 CLI agent 无人值守地跑完整闭环：
任务 → 执行 → 机器验证 → 人工卡点 → 自动交付。你用自然语言定义验收策略，agent
干活，平台判定，人只在卡点出现。

**单进程、单机、单用户、无需认证。** 本地 SQLite 持久化，进程内事件总线，Go
daemon + Next.js Web 界面。

## 特性

- **全自动闭环** — goal / run / 机器验证 / 结构化约束 / 人工卡点 / 自动交付
  （合并 + 复验 + 推送）
- **验收策略属于域（项目）** — 用自然语言说"怎样算完成"，处理器 agent 编译成
  可执行检查（验证命令、约束、卡点），你确认一次后冻结
- **子任务 + 变更交付** — owner 拆活成子任务（独立负责人、独立 worktree、
  机器或 agent 验证）；验证通过的子任务产出 **Change**（带修订版本），owner
  集成——冲突会自动唤醒 assignee 返修
- **四种协作行为** — Comment（说）/ Consult（问）/ Handoff（接力）/ Sub-goal
  （拆活），以 `agentwork` CLI 暴露，owner 权限在服务端强制
- **多协议多传输** — ACP / JSONL / JSON-RPC × stdio / ws / tcp；agent 可挂
  自己的额外 MCP 服务器
- **多触发渠道** — Web、cron 定时、GitHub/GitCode issue（webhook + 轮询）、
  飞书入站（@机器人 建任务）
- **飞书通知** — 审批卡（IM 内直接批/驳）、完成/失败推送、每日摘要
- **实时 Web UI** — 事件流、执行时间线、子任务 / 变更 / 验证记录面板
- **Agent 聊天（ACP 桥）** — Web 就是每个 agent 的真实 ACP 客户端：
  `GET /agents/{id}/acp` 把 ACP 帧原样中继到机器侧 CLI；会话是 CLI 自己的
  （list/load/new），权限请求弹出审批框，回合可中断；毒会话历史在输入层
  失败时自动换新会话重试

> **一句话脑图：** **goal** 是工作条目，状态权威的唯一持有者；**run** 是某个
> agent 在它上面的一次执行——run 只汇报、不决策。完成与否由平台判定，卡点由人
> 把关。

## 快速开始

### 环境要求

- Go 1.26+
- Node.js 20+
- npm 10+

### 后端

```bash
./build.sh   # 产出 build/agentwork（CLI）+ build/agentwork-daemon，
             # 带同一版本戳（AGENTWORK_COMPILE_VERSION）

# 启动 daemon（默认 :7373）
./build/agentwork-daemon

# 在跑 agent 的机器上：
./build/agentwork connect
```

### 前端

```bash
cd web
npm install
npm run build && npm start
```

### 第一个任务

1. 创建**项目**（域）：仓库地址 + 自然语言验收策略（不确认冻结则强制人工卡点）
2. 创建 **runtime**（如 `opencode acp --pure`，stdio + acp）和 **agent**
3. 在机器上运行 `agentwork connect`——daemon 通过 `run.poll` 把任务派给
   它（拉取模型）
4. 创建 **goal**、指派给 agent，看闭环跑起来：执行 → 验证 → 卡点 → 你审批 →
   自动交付
5. 或者直接在 Agent 页面与任意 agent 聊天——每次打开都会重新下发人设和技能

## 核心概念

| 概念 | 含义 |
|---|---|
| **项目（Domain）** | 资产/演进域：共享仓 + 验收策略（自然语言意图编译成检查清单）+ 默认卡点 |
| **Goal** | 工作条目（产品面）。状态：backlog → active → review → done / failed / cancelled。状态权威的唯一持有者 |
| **Run** | 某个 agent 在 goal 上的一次执行。状态五态 + **角色**（owner / subgoal / consult / review / verify）。无权威——终态上报 goal 层仲裁 |
| **Runtime** | 启动规格——transport（stdio/ws/tcp）+ provider（acp/jsonl/jsonrpc）+ executable/args 或 endpoint + env |
| **Agent** | runtime + 人设（system prompt / model / env / 额外 MCP 服务器 / 并发数） |
| **聊天（Chat）** | 通过 `GET /agents/{id}/acp` 与 agent 的直接 ACP 会话——daemon 把帧原样中继到机器侧 CLI；会话存于 CLI 自己的存储，权限在面板里审批 |
| **Squad** | 路由组，不干活：goal 路由到 leader，由其拆子任务；role=reviewer 的成员在卡点时被平台自动拉入审查 |
| **子任务（Sub-goal）** | 从 goal 拆出的工作项（不是 child goal，不可递归）。独立 assignee + 可选 agent verifier；机器重试（≤3）与验证驳回分开计数 |
| **Change** | 子任务的逻辑交付物：ready → integrating → integrated，或 conflict → 返修 → 修订 N+1。owner 用 `integrate_change` 集成 |
| **Verifier** | 默认机器（域验证命令），或指定 agent 发结构化裁决（`verify_sub_goal`） |
| **卡点（Gate）** | 验收策略中的人工检查点规则（merge、diff_contains…）；弱验证强制人工段 |
| **Mention** | Consult 原语：评论里的结构化 URI 把 agent 拉进只读 guest run，回答后平台自动恢复发起者 |
| **定时任务（Schedule）** | cron 模板，每次触发克隆一个 goal（幂等、时区感知） |
| **触发渠道** | Web、cron、GitHub/GitCode issue（webhook + 轮询，source_ref 去重）、飞书入站 |

## 协作（四种行为）

| 行为 | CLI 命令 | 权限 |
|---|---|---|
| **Comment**（说） | `agentwork goal comment --text T` | 任何人 |
| **Consult**（问） | `goal comment` 里 mention：`[@Name](mention://agent/<id>)` | goal 的 owner |
| **Handoff**（接力） | `agentwork goal assign <to-agent-id> [--note N]` | goal 的 owner |
| **Sub-goal**（拆活） | `agentwork subgoal create --title T --assignee A` / `subgoal cancel <id>` | goal 的 owner |

agent 只通过 `agentwork` CLI（守护进程二进制的 shim，spawn 时放进 agent 的 PATH）
协作。每条命令携带 per-run token（`AGENTWORK_TOKEN`），守护进程 `/rpc` 端点将其
解析为 run 的 `(goal, agent, role)`——token 是唯一身份，自报 id 被忽略，handoff
的 owner 校验在服务端强制。另有 `subgoal verify`、`change integrate` 及
`*_list` 命令。完整模型见 [Collaboration.v2.md](Collaboration.v2.md)。

## 架构

```
                 ┌────────────── HTTP API + WS hub ──────────────┐
                 │   goals/runs/sub-goals/changes/…               │
                 │   + /agents/{id}/acp（ACP 聊天中继）            │
                 ▼                                               ▼
           service 层（状态权威）                       Web UI（Next.js）
                 │  bus.Publish（commit 后）                │ ACP 客户端（聊天）
                 ▼                                           │
            SQLite（真相） ── daemon 调度器 ─────────────────┤
                                 │ run.poll / chat.*        │
                                 ▼                           ▼
                          agentwork connect   ◄──  /connect（JSON-RPC）
                                 │  runTask、聊天桥（spawn + 中继）
                                 ▼
                          runtime.Open(spec)
                                 │  acp | jsonl | jsonrpc
                                 ▼
                          agent CLI 子进程 ◄── ACP 帧原样中继
                           （stdio/ws/tcp）
                           └─ fs/terminal RPC + agentwork CLI（/rpc）
                              （worktree、协作命令）
                 ┌───────────────┬─────────────────────────────┘
                 ▼               ▼
         机器验证 / 约束     卡点 → 交付
         （域验收策略）      （人批 → 合并+复验+推送）
```

- **Event ≠ 真相** — 事件总线只是唤醒提示；一切状态转移都是条件化 DB 事务，
  重放幂等
- **机器执行器** — `agentwork connect` 持有一条常驻 JSON-RPC 链接并拉取
  工作（`run.poll`）；它执行 run、托管每个 agent 的聊天桥（spawn CLI、原样
  中继 ACP 帧、关闭时先 stdin EOF 优雅收尾再杀进程）
- **run 级 worktree** — `~/.agentwork/runs/<runID>` 临时 git worktree，基于每域
  bare 仓；崩溃恢复在启动时重放未仲裁的终态 run 并重推导 attention
- **Coordinator** — 派生的 OwnerAttention（integration / recovery /
  user_action）在有活干时才唤醒 owner

## CLI 工具

`agentwork` 既是**机器执行器**，也是人调试用的工具（agent 走 `agentwork` CLI shim 协作）：

```bash
agentwork connect               # 机器侧：拉取派发、执行 run、托管聊天桥
agentwork goal list --limit 5   # 调试命令（默认连 http://localhost:7373，
agentwork stats                 #   或设 AGENTWORK_SERVER_URL）
```

## 技术栈

| 层 | 技术 |
|---|---|
| **后端** | Go 1.26、SQLite（modernc）、gorilla/websocket、robfig/cron、MCP go-sdk |
| **前端** | Next.js 16、React 19、Tailwind CSS 4、TanStack React Query |
| **协议** | ACP、JSONL、JSON-RPC |
| **传输** | stdio、WebSocket、TCP |
| **通知** | 飞书（审批卡、日报、IM 入站） |
| **Issue 触发** | GitHub、GitCode |

## 遥测与隐私

官方构建的二进制可能会将匿名的任务生命周期事件（创建 / 指派 / 完成 / 删除）
上报到一个外部分析平台，以帮助我们了解使用情况、改进产品。

**上报内容：** 事件类型、goal ID、goal 状态、daemon 版本号、时间戳。
`anonymousId` / `distinctId` / `instance_id` 字段携带的是基于主机名派生的
标识（若主机名本身是 32 位十六进制则直接使用，否则取其 MD5），从而把同一部署
的事件关联起来。**不上报源码、提示词、模型输出、文件内容或验收策略。**

**默认行为——禁用。** 上报端点和 AppID **不**硬编码在仓库里；一次普通的
`go build` 或 `./build.sh` 会将它们留空，产出不发起任何 HTTP 请求的禁用二进制。
只有通过 CI 流水线变量注入 `EVENT_POST_URL` / `EVENT_APP_ID`（以流水线变量形式
存储，不进仓库）的构建才会产出会上报事件的二进制。

**为自定义构建启用：**

```bash
EVENT_POST_URL="https://your-track-endpoint" \
EVENT_APP_ID="your-app-id" \
./build.sh
```

**显式关闭：**

```bash
EVENT_POST_URL="" ./build.sh
```
