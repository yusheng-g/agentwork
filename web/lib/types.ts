// API types — mirror the Go structs in internal/service.

// ProbeCLI is one agent CLI a remote machine reported (CLI 分支 Phase 1).
export interface ProbeCLI {
  name: string;
  version: string;
  acp_spawn: string[];
  skills_dir?: string;
  profile_files?: string[];
}

// Skill is a platform-managed skill package (CLI 分支 Phase 4).
export interface Skill {
  id: string;
  name: string;
  description: string;
  created_at: string;
}

// Machine is a remote host registered via `agentwork connect`.
export interface Machine {
  id: string;
  name: string;
  hostname: string;
  version: string;
  probed_clis: string; // JSON []ProbeCLI
  last_seen_at: string;
  status: string; // connected | offline
  created_at: string;
}

export interface Runtime {
  id: string;
  name: string;
  machine_id: string; // the registered machine that executes this runtime's runs
  args: string[]; // acp_spawn — how the machine starts the CLI
  env: Record<string, string>;
  status: string; // active | absent — absent = the machine's latest probe no longer sees the CLI
  created_at: string;
}

// McpServer mirrors acp.McpServer: an extra MCP server the agent's runs
// advertise at session/new alongside the platform's workspace server.
export interface McpServer {
  type?: string; // ""=stdio | http | sse
  name: string;
  command?: string; // stdio
  args?: string[]; // stdio
  env?: { name: string; value: string }[]; // stdio: process env
  url?: string; // http / sse
  headers?: { name: string; value: string }[]; // http / sse: request headers
}

export interface Agent {
  id: string;
  name: string;
  type: string; // standard | steward
  description?: string; // human-facing one-liner (shown in the web list); distinct from system_prompt
  runtime_id: string;
  system_prompt: string;
  model: string;
  env: Record<string, string>;
  mcp_servers: McpServer[]; // the agent's own tools (browser/db/…), always after the workspace server
  skills: string[]; // platform-managed skill ids (CLI 分支 Phase 4) — pushed to the agent's machine
  max_concurrent: number;
  created_at: string;
}

export type GoalStatus =
  | "backlog"
  | "active"
  | "review"
  | "done"
  | "failed"
  | "cancelled";

export interface Goal {
  id: string;
  title: string;
  description: string;
  domain_id: string;
  assignee_type: string; // agent | squad | human
  assignee_id: string;
  status: GoalStatus;
  handoff_note: string;
  review_request: string;
  human_iterations: number;
  created_by_type: string;
  created_by_id: string;
  created_at: string;
  source_ref: string; // M4-B: "github:owner/repo#123" (external issue source)
  current_agent_id: string; // latest running/queued run's agent ('' = none)
  attention: string; // v2 OwnerAttention：'' | integration | wrapup | recovery | user_action
  review_phase: string; // 审查窗口派生阶段：'' | awaiting_review | reviewing | awaiting_approval（决策 6-19 延伸）
}

export interface GateRule {
  name: string; // merge | diff_contains | diff_excludes (M2)
  when: string;
  pattern: string; // diff_* gates: glob over changed paths
}

export interface GateStat {
  rule: string;
  total: number;
  approved: number;
  rejected: number;
}

export interface Guard {
  type: string; // diff_contains | diff_excludes | coverage_delta
  pattern: string;
  min_delta: number;
}

export interface Checks {
  setup: string[]; // environment preparation (dependency installs) before verify
  excludes: string[]; // commit-time exclusion globs (domain-declared, from the repo's .gitignore)
  verify: string[];
  guards: Guard[];
  gates: GateRule[];
}

export interface Domain {
  id: string;
  type: string; // repo (M0)
  name: string;
  git_url: string;
  default_branch: string;
  git_identity: string;
  git_credentials: string;
  policy_text: string;
  checks: Checks;
  verification_strength: string; // strong|medium|weak
  max_run_duration: number;
  verify_timeout: number;
  processor_agent_id: string;
  checks_compiled_at: string;
  metrics_baseline: string;
  issue_repo: string; // M4-B: "owner/repo" tracked for issues ('' = none)
  issue_assignee: string; // M4-B: agent|squad handling this repo's issues
  issue_assignee_type: string; // M4-B: agent | squad
  issue_provider: string; // M4-B: github | gitcode
  pull_assignee: string; // M4-B PR pull: agent|squad handling this repo's PRs ('' = don't pull PRs)
  pull_assignee_type: string; // M4-B PR pull: agent | squad
  created_at: string;
  scratch_dir?: string; // scratch 域的项目目录（repo 域无）——人找产物的路径
}

export type RunStatus = "queued" | "running" | "completed" | "failed" | "cancelled";

// DomainGitTestResult is the outcome of POST /domains/test (决策 6-24):
// the config-time git probe — repo URL + branch + token read permission,
// verified BEFORE the first run instead of failing it.
export interface DomainGitTestResult {
  ok: boolean;
  branch_exists: boolean;
  resolved_branch?: string; // the branch checked (configured, or the remote's HEAD default)
  refs?: string[];
  error?: string;
  latency_ms: number;
}

export interface Run {
  id: string;
  goal_id: string;
  agent_id: string;
  run_kind: string; // worker|processor
  domain_id: string;
  prompt: string;
  session_id: string;
  workdir: string;
  status: RunStatus;
  cancel_reason?: string; // structured: idle_watchdog|handoff|stopped|timeout|runaway|goal_terminal|goal_cancelled
  role: string; // owner | subgoal | consult | review | verify（决策 5-4/6-9，enqueue 时派生）
  attempt: number;
  result_summary: string;
  evidence: string; // JSON: diff stats + verify output + agent summary
  trigger_comment_id: string;
  is_leader_run: boolean;
  squad_id: string;
  queued_at: string;
  started_at: string;
  finished_at: string;
  created_at: string;
}

export interface Comment {
  id: string;
  goal_id: string;
  author_type: string; // human | agent | system
  author_id: string;
  parent_id: string;
  content: string;
  created_at: string;
  run_id?: string; // the run whose product this comment is ('' = trigger/context)
  ask_human?: boolean; // 决策 7-3: agent's --ask question to the human (goal creator)
}

export interface Squad {
  id: string;
  name: string;
  description: string;
  leader_id: string;
  instructions: string;
  created_at: string;
}

export interface SubGoal {
  id: string;
  goal_id: string;
  title: string;
  description: string;
  assignee_id: string;
  verifier_id: string; // '' = 机器验证
  status: string; // running | verifying | verified | rejected | cancelled | failed
  execution_attempt: number;
  quality_iteration: number;
  created_at: string;
}

// Change is a sub-goal's logical deliverable (v2 决策 6-3): the owner
// integrates it into the goal branch. HeadRef = the LATEST revision's head.
export interface Change {
  id: string;
  goal_id: string;
  sub_goal_id: string;
  status: string; // ready | integrating | integrated | conflict
  head_ref: string;
  created_at: string;
}

// ChangeRevision binds a change to the integration base it was built
// against — conflict rework appends seq N+1 on the SAME change.
export interface ChangeRevision {
  id: string;
  change_id: string;
  seq: number;
  base_ref: string;
  head_ref: string;
  created_at: string;
}

// ChangeDetail is a change with its revision history (GET /goals/{id}/changes).
export interface ChangeDetail extends Change {
  revisions: ChangeRevision[];
}

// VerificationResult is one verification round of a sub-goal (v2 决策 6-5):
// machine checks or an agent verifier's structured verdict.
export interface VerificationResult {
  id: string;
  goal_id: string;
  sub_goal_id: string;
  verifier_run_id: string; // '' = machine verification
  status: string; // passed | rejected
  summary: string;
  evidence: string;
  created_at: string;
}

export interface SquadMember {
  id: string;
  squad_id: string;
  member_type: string; // agent | human
  member_id: string;
  role: string;
  created_at: string;
}

export interface Schedule {
  id: string;
  name: string;
  title_template: string;
  description: string;
  assignee_type: string; // agent | squad
  assignee_id: string;
  domain_id: string; // 触发时克隆 goal 所属的域（验收策略 + worktree）
  cron_expression: string;
  timezone: string;
  enabled: boolean;
  next_run_at: string;
  last_run_at: string;
  created_at: string;
}

// ScheduleRun is one firing of a schedule — the fired goal's identity and
// current status (the schedule detail's firing history).
export interface ScheduleRun {
  id: string;
  schedule_id: string;
  goal_id: string;
  goal_title: string;
  goal_status: string;
  planned_at: string;
  status: string; // dispatched|failed
  created_at: string;
}

// LogLine is one daemon log line (GET /logs and the live log:line stream).
export interface LogLine {
  ts: string;
  level: string; // debug | info | warn | error
  text: string;
}

// TeamImport is a TEMPORARY tracking row for a team-definition-repo import
// processor run. The steward agent decides which runtime each imported agent
// binds to (from the team definition or random assignment). git_url/
// credentials/branch persist from the HTTP request to daemon dispatch.
// ImportTeam cleans up old completed/failed rows.
export interface TeamImport {
  id: string;
  run_id: string;
  git_url: string;
  git_credentials: string;
  default_branch: string;
  status: string; // pending|completed|failed
  result: string; // JSON summary
  created_at: string;
}

export interface TeamImportResponse {
  team_import: TeamImport;
  run: Run;
}

// ── Templates (GitCode YAML template library) ──
// Mirror internal/service/template.go + template_apply.go. The list endpoint
// returns TemplateSummary; the get-one endpoint returns TemplateDetail with
// the raw YAML + the inner spec node re-encoded as JSON.

// TemplateMeta is the common front matter of every template.
export interface TemplateMeta {
  api_version: string; // "agentwork/v1"
  kind: string; // "squad-template" | "project-template"
  id: string;
  name: string;
  description: string;
  version: string;
  tags?: string[];
  icon?: string;
}

// TemplateSummary is the list-item shape (metadata only, no spec).
export interface TemplateSummary extends TemplateMeta {
  path: string;
}

// TemplateDetail is the get-one shape: metadata + raw YAML + decoded spec.
// `spec` is the INNER spec node only (repo/domain/team/…), not the whole
// document — the f9a6bb1 fix made Get return spec, not {api_version,…,spec}.
export interface TemplateDetail extends TemplateMeta {
  path: string;
  spec_yaml: string;
  spec: Record<string, unknown>;
}

// ── Template spec shapes (mirror the Go spec structs, for preview/derive) ──
// These describe what the backend parsed; the dialog derives its form state
// from spec.repo.create / spec.domain.type / spec.team / spec.goals.

export interface TplRepoCreate {
  create: boolean;
  visibility: string; // "private" | "public"
  auto_init?: boolean;
  gitignore_template?: string;
  org?: string;
}

export interface TplDomain {
  type: string; // "repo" | "scratch" (default repo)
  default_branch: string;
  git_identity?: string;
  issue_assignee?: string;
  issue_assignee_type?: string; // "agent" | "squad"
  policy_text?: string;
}

export interface TplAgent {
  name: string;
  description?: string;
  system_prompt?: string;
  skills?: string[];
  runtime?: string;
  max_concurrent?: number;
  model?: string;
  env?: Record<string, string>;
}

export interface TplMember {
  name: string;
  role?: string;
}

export interface TplSquad {
  name: string;
  description?: string;
  leader: string;
  instructions?: string;
  members?: TplMember[];
}

// squad-template spec.
export interface SquadTemplateSpec {
  strategy?: string; // "create" | "upsert" (default upsert)
  agents: TplAgent[];
  squad: TplSquad;
}

// project-template spec.
export interface ProjectTemplateSpec {
  repo?: TplRepoCreate; // absent = user supplies git_url
  domain: TplDomain;
  team?: SquadTemplateSpec; // optional agents+squad riding the project
  goals?: TplGoal[];
  schedules?: TplSchedule[];
}

export interface TplGoal {
  title: string;
  description?: string;
  assignee?: string; // agent or squad name
  assignee_type?: string; // "agent" | "squad" (default agent)
  start?: boolean; // true = active; false = backlog
}

export interface TplSchedule {
  name: string;
  title?: string;
  description?: string;
  cron: string;
  assignee?: string;
  assignee_type?: string;
}

// ── Apply requests / results ──

// ApplySquadOverrides: the squad dialog's fields.
export interface ApplySquadOverrides {
  squad_name?: string; // overrides spec.squad.name
  rename?: Record<string, string>; // template agent name → per-apply name
  strategy?: string; // overrides spec.strategy
}

// ApplyProjectOverrides: the project dialog's fields.
export interface ApplyProjectOverrides {
  name: string; // required: the domain name
  git_url?: string; // required unless spec.repo.create
  git_credentials?: string;
  repo_token?: string; // the repo-creation token (GitCode)
  template_token?: string; // unused by apply (list phase); kept for symmetry
  repo?: Partial<TplRepoCreate>; // overlay on spec.repo
  rename?: Record<string, string>;
  strategy?: string;
  goal_start?: boolean; // overlay on every goal's start
}

// ApplyItem is one entity's per-step outcome.
export interface ApplyItem {
  kind: string; // skill | agent | squad | domain | repo | goal | schedule | member | domain.issue_assignee
  name: string;
  id?: string;
  action: string; // created | updated | skipped | failed
  error?: string;
}

export interface ApplySquadResult {
  squad?: Squad;
  agents?: Agent[];
  skills?: Skill[];
  items: ApplyItem[];
}

export interface ApplyProjectResult {
  domain?: Domain;
  repo_url?: string; // set when the repo was created
  agents?: Agent[];
  squad?: Squad;
  goals?: Goal[];
  schedules?: Schedule[];
  items: ApplyItem[];
}

// TemplateFetchResult reports one refresh.
export interface TemplateFetchResult {
  fetched: number;
  errors?: string[];
  fetched_at: string;
}

// WS event shape from the hub: {"topic":"goal:created","payload":{...}}
export type WSTopic =
  | "goal:created" | "goal:assigned" | "goal:finished"
  | "goal:retrying" | "goal:retry_failed" | "goal:deleted"
  | "goal:reviewing" | "goal:review_ready" | "goal:approved" | "goal:review_resolved"
  | "goal:delivered" | "goal:deliver_failed"
  | "run:enqueued" | "run:coalesced" | "run:claimed" | "run:discarded" | "run:event" | "run:cancelled" | "run.terminal"
  | "sub_goal.created" | "sub_goal.verifying" | "sub_goal.verified" | "sub_goal.rejected" | "sub_goal.retrying" | "sub_goal.failed" | "sub_goal.cancelled"
  | "change.ready" | "change.integrated" | "change.conflict"
  | "comment:created"
  | "log:line"
  | "agent:created" | "agent:deleted" | "agent:pin_changed"
  | "squad:created" | "squad:deleted" | "squad:member_added" | "squad:member_removed"
  | "schedule:created" | "schedule:fired"
  | "domain:created" | "domain:deleted" | "domain:compiled" | "domain:compile_failed"
  | "team:import_enqueued" | "team:imported" | "team:import_failed";

export interface WSEvent {
  topic: WSTopic;
  payload: Record<string, unknown>;
}

// TimelineItem is one event in a goal's execution flow — a run segment (an
// agent's turn), an action point (created/handoff/review entry/…), or a gate
// decision (approve/reject). Served by GET /goals/{id}/timeline, merged and
// time-ordered by the backend.
export interface TimelineItem {
  at: string;                    // RFC3339 — the event's point in time
  kind: "run" | "action" | "decision";
  run_id?: string;               // run: the run row (for detail fetch)
  agent_id?: string;             // run: the executing agent
  run_status?: string;           // run: queued|running|completed|failed|cancelled
  role?: string;                 // run: owner|subgoal|consult|review|verify
  attempt?: number;              // run: machine-retry counter
  started_at?: string;           // run: execution window
  finished_at?: string;
  actor_type?: string;           // action: human|agent|system
  actor_id?: string;             // action: which agent/human ('' for system)
  action?: string;               // action: created|handoff|entered_review|…
  detail?: string;
  gate_rule?: string;            // decision: which rule fired
  decision?: string;             // decision: approve|reject|redirect
  reason?: string;               // decision: the human's words
  review_duration_s?: number;    // decision: seconds spent in review
}
