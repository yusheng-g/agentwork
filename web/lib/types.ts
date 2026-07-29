// API types — mirror the Go structs in internal/service.

export interface Runtime {
  id: string;
  name: string;
  transport: string; // stdio | ws | tcp
  provider: string;  // acp | jsonl | jsonrpc
  executable: string;
  args: string[];
  endpoint: string;
  env: Record<string, string>;
  created_at: string;
}

export interface Agent {
  id: string;
  name: string;
  description: string;
  runtime_id: string;
  system_prompt: string;
  model: string;
  workdir_base: string;
  env: Record<string, string>;
  max_concurrent: number;
  created_at: string;
}

export type GoalStatus =
  | "backlog"
  | "active"
  | "blocked"
  | "done"
  | "failed"
  | "cancelled";

export interface Goal {
  id: string;
  title: string;
  description: string;
  parent_id: string;
  assignee_type: string; // agent | squad | human
  assignee_id: string;
  status: GoalStatus;
  handoff_note: string;
  created_by_type: string;
  created_by_id: string;
  created_at: string;
}

export type RunStatus = "queued" | "running" | "completed" | "failed" | "cancelled";

export interface Run {
  id: string;
  goal_id: string;
  agent_id: string;
  session_id: string;
  workdir: string;
  status: RunStatus;
  attempt: number;
  result_summary: string;
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
}

export interface Squad {
  id: string;
  name: string;
  description: string;
  leader_id: string;
  instructions: string;
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
  cron_expression: string;
  timezone: string;
  enabled: boolean;
  next_run_at: string;
  last_run_at: string;
  created_at: string;
}

// WS event shape from the hub: {"topic":"goal:created","payload":{...}}
export type WSTopic =
  | "goal:created" | "goal:assigned" | "goal:finished"
  | "goal:retrying" | "goal:retry_failed" | "goal:waiting" | "goal:deleted"
  | "run:enqueued" | "run:coalesced" | "run:discarded" | "run:event"
  | "comment:created"
  | "agent:created" | "agent:deleted"
  | "squad:created" | "squad:deleted" | "squad:member_added"
  | "schedule:created" | "schedule:fired";

export interface WSEvent {
  topic: WSTopic;
  payload: Record<string, unknown>;
}
