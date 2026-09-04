# agentwork

> [中文](README.zh.md) | [Design](DESIGN.md) | [协作模型](Collaboration.v2.md)

An **AI task pipeline OS** — a single-user control plane that runs CLI agents
unattended through the full loop: goal → execution → machine verification →
human gate → delivery. You define acceptance policies in natural language,
agents do the work, the platform judges, and you only appear at checkpoints.

**Single process, single machine, single user, no auth.** Local SQLite for
state, in-process event bus, Go daemon + Next.js web UI.

## Highlights

- **Full unattended loop** — goals, runs, machine verification, structural
  guards, human gates, automatic delivery (merge + re-verify + push)
- **Acceptance policy per domain** — you state "what done means" in natural
  language; a processor agent compiles it into executable checks (verify
  commands, guards, gates) which you confirm once
- **Sub-goals with changes** — the owner splits work into sub-goals (own
  assignee, own worktree, machine or agent verification); verified sub-goals
  produce **Changes** with revisions the owner integrates — conflicts wake
  the assignee to rework automatically
- **Four collaboration behaviors** — Comment / Consult / Handoff / Sub-goal,
  exposed as the `agentwork` CLI with owner-only permissions enforced server-side
- **Multi-protocol & transport** — ACP / JSONL / JSON-RPC over stdio / ws / tcp;
  agents can carry their own extra MCP servers
- **Triggers** — Web, cron schedules, GitHub/GitCode issues (webhook + poll),
  Feishu IM inbound (@-bot task creation)
- **Feishu notifications** — approval cards (approve/reject right in IM),
  completion/failure pushes, a daily digest
- **Real-time web UI** — live event stream, execution timeline, sub-goal /
  change / verification panels
- **Agent chat (ACP bridge)** — the web is a real ACP client for each agent:
  `GET /agents/{id}/acp` relays ACP frames unparsed to the machine-side CLI;
  sessions are the CLI's own (list/load/new), permission requests surface as
  an approval modal, and turns can be interrupted or retried on a fresh
  session when poisoned history fails at the input layer

> **One-line mental model:** a **goal** is a work item and the sole holder of
> state authority; a **run** is one agent's turn on it — runs report up, never
> decide. The platform judges completion; you hold the gates.

## Quick start

### Prerequisites

- Go 1.26+
- Node.js 20+
- npm 10+

### Backend

```bash
./build.sh   # builds build/agentwork (CLI) + build/agentwork-daemon
             # with one shared version stamp (AGENTWORK_COMPILE_VERSION)

# Start the daemon (default :7373)
./build/agentwork-daemon

# And on the machine that runs the agents:
./build/agentwork connect
```

### Frontend

```bash
cd web
npm install
npm run build && npm start
```

### First goal

1. Create a **domain** (项目): repo URL + acceptance policy in natural language
   (or leave it unfrozen to force a human checkpoint)
2. Create a **runtime** (e.g. `opencode acp --pure`, stdio + acp) and an
   **agent**
3. Run `agentwork connect` on your machine — the daemon dispatches runs to
   it via `run.poll` (pull model)
4. Create a **goal**, assign the agent, and watch the loop run: execution →
   verification → review → your approval → automatic delivery
5. Or chat with any agent directly from the Agents page — a per-agent ACP
   session with the persona/skills staged fresh at every open

## Core concepts

| Concept | What it is |
|---|---|
| **Domain** (Project) | An asset/evolution domain: shared repo + acceptance policy (NL intent compiled into checks) + default gates. |
| **Goal** | A work item (product plane). Status: backlog → active → review → done / failed / cancelled. The sole holder of state authority. |
| **Run** | One agent's turn on a goal. Status: queued → running → completed / failed / cancelled, plus a **role** (owner / subgoal / consult / review / verify). No authority — it reports to the goal layer. |
| **Runtime** | A launch spec — transport (stdio/ws/tcp) + provider (acp/jsonl/jsonrpc) + executable/args or endpoint + env. |
| **Agent** | A runtime + persona (system prompt / model / env / extra MCP servers / max concurrency). |
| **Chat** | A direct ACP conversation with an agent over `GET /agents/{id}/acp` — the daemon relays frames unparsed to the machine-side CLI; sessions live in the CLI's own store, permissions are answered in the panel. |
| **Squad** | A routing group that does no work itself: goals route to its leader, who delegates via sub-goals; members with role=reviewer are auto-pulled into review checkpoints. |
| **Sub-goal** | A work item split off a goal (not a child goal — no recursion). Own assignee + optional agent verifier; machine retries (≤3) and verifier rejections are counted separately. |
| **Change** | A sub-goal's logical deliverable: ready → integrating → integrated, or conflict → assignee rework → revision N+1. The owner integrates it via the `integrate_change` tool. |
| **Verifier** | Machine (domain verify commands) by default, or a named agent issuing structured verdicts (`verify_sub_goal`). |
| **Gates** | Human checkpoint rules from the acceptance policy (merge, diff_contains, ...); weak verification forces a checkpoint. |
| **Mention** | The Consult primitive: a structured URI in a comment pulls an agent into a read-only guest run; the requester resumes automatically after the answer. |
| **Schedule** | A cron template that clones a goal per firing (idempotent, timezone-aware). |
| **Trigger** | Web, cron, GitHub/GitCode issues (webhook + poll, deduped by source ref), Feishu IM inbound. |

## Collaboration (four behaviors)

| Behavior | CLI command | Who may |
|---|---|---|
| **Comment** (say) | `agentwork goal comment --text T` | anyone |
| **Consult** (ask) | a mention in `goal comment`: `[@Name](mention://agent/<id>)` | the goal's owner |
| **Handoff** (transfer) | `agentwork goal assign <to-agent-id> [--note N]` | the goal's owner |
| **Sub-goal** (split) | `agentwork subgoal create --title T --assignee A` / `subgoal cancel <id>` | the goal's owner |

Agents coordinate exclusively through the `agentwork` CLI (a shim of the
daemon binary, on the agent's PATH at spawn). Every command carries the
per-run token (`AGENTWORK_TOKEN`), which the daemon's `/rpc` endpoint
resolves to the run's `(goal, agent, role)` — the token is the ONLY
identity, self-reported ids are ignored, and the handoff owner check is
enforced server-side. Plus `subgoal verify`, `change integrate`, and the
`*_list` commands. Full model in [Collaboration.v2.md](Collaboration.v2.md).

## Architecture

```
                 ┌────────────── HTTP API + WS hub ──────────────┐
                 │   goals/runs/sub-goals/changes/…               │
                 │   + /agents/{id}/acp (ACP chat relay)          │
                 ▼                                               ▼
           service layer (state authority)                web UI (Next.js)
                 │  bus.Publish (after commit)              │ ACP client (chat)
                 ▼                                           │
            SQLite (truth) ── daemon scheduler ──────────────┤
                                 │ run.poll / chat.*        │
                                 ▼                           ▼
                          agentwork connect   ◄──  /connect (JSON-RPC)
                                 │  runTask, chat bridge (spawn + relay)
                                 ▼
                          runtime.Open(spec)
                                 │  acp | jsonl | jsonrpc
                                 ▼
                          agent CLI subprocess ◄── ACP frames relayed unparsed
                           (stdio/ws/tcp)
                           └─ fs/terminal RPC + agentwork CLI (/rpc)
                              (worktree, collaboration commands)
                 ┌───────────────┬─────────────────────────────┘
                 ▼               ▼
          machine verify /   review gate → deliver
          guards (domain)    (human approve → merge+re-verify+push)
```

- **Event ≠ truth** — the bus is a wakeup hint; every transition is a
  conditional DB transaction, idempotent under replay
- **Machine executor** — `agentwork connect` holds a persistent JSON-RPC
  link and pulls work (`run.poll`); it executes runs, hosts the per-agent chat
  bridge (spawns the CLI, relays ACP frames unparsed, tears down gracefully
  with stdin EOF before the kill)
- **Per-run worktrees** — `~/.agentwork/runs/<runID>` ephemeral git worktrees
  against per-domain bare repos; crash recovery replays terminal runs and
  re-derives attention at startup
- **Coordinator** — derived OwnerAttention (integration / recovery /
  user_action) wakes the owner exactly when there is work for it

## CLI tool

`agentwork` is the **machine executor** and a human debugging tool (agents
use the `agentwork` CLI shim for collaboration):

```bash
agentwork connect               # the machine side: pulls dispatches, runs them, hosts chats
agentwork goal list --limit 5   # debugging commands (default http://localhost:7373,
agentwork stats                 #   or set AGENTWORK_SERVER_URL)
```

## Tech stack

| Layer | Stack |
|---|---|
| **Backend** | Go 1.26, SQLite (modernc), gorilla/websocket, robfig/cron, MCP go-sdk |
| **Frontend** | Next.js 16, React 19, Tailwind CSS 4, TanStack React Query |
| **Protocols** | ACP, JSONL, JSON-RPC |
| **Transports** | stdio, WebSocket, TCP |
| **Notifications** | Feishu (approval cards, digest, IM inbound) |
| **Issue triggers** | GitHub, GitCode |

## Telemetry & Privacy

Official binaries may report anonymous task lifecycle events (created /
assigned / finished / deleted) to an external analytics platform. This
helps us understand usage and improve the product.

**What is reported:** event type, goal ID, goal status, daemon version, and
a timestamp. The `anonymousId` / `distinctId` / `instance_id` fields carry a
hostname-derived identifier (hostname used directly if it is already a
32-char hex value, otherwise the MD5 of the hostname) so events from one
deployment can be correlated. **No source code, prompts, model output, file
contents, or acceptance policies are reported.**

**Default behavior — disabled.** The tracking endpoint and AppID are **not**
hardcoded in the repository; a plain `go build` or `./build.sh` leaves them
empty, producing a disabled binary that makes no HTTP calls. Only CI
pipeline builds that inject `EVENT_POST_URL` / `EVENT_APP_ID` (stored as
pipeline variables, not in the repo) produce a binary that emits events.

**Enable for a custom build:**

```bash
EVENT_POST_URL="https://your-track-endpoint" \
EVENT_APP_ID="your-app-id" \
./build.sh
```

**Disable explicitly:**

```bash
EVENT_POST_URL="" ./build.sh
```
