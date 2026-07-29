import type { Runtime, Agent, Goal, Run, Comment, Squad, SquadMember, Schedule } from "./types";

const API_BASE = process.env.NEXT_PUBLIC_API_URL || "http://localhost:7373";

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`${res.status}: ${text}`);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

// ── Runtime ──
export const listRuntimes = () => api<Runtime[]>("/runtimes");
export const getRuntime = (id: string) => api<Runtime>(`/runtimes/${id}`);
export const createRuntime = (body: Omit<Runtime, "id" | "created_at">) =>
  api<Runtime>("/runtimes", { method: "POST", body: JSON.stringify(body) });
export const deleteRuntime = (id: string) =>
  api<void>(`/runtimes/${id}`, { method: "DELETE" });

// ── Agent ──
export const listAgents = () => api<Agent[]>("/agents");
export const getAgent = (id: string) => api<Agent>(`/agents/${id}`);
export const createAgent = (body: Omit<Agent, "id" | "created_at">) =>
  api<Agent>("/agents", { method: "POST", body: JSON.stringify(body) });
export const deleteAgent = (id: string) =>
  api<void>(`/agents/${id}`, { method: "DELETE" });

// ── Goal ──
export const listGoals = () => api<Goal[]>("/goals");
export const getGoal = (id: string) => api<Goal>(`/goals/${id}`);
export const createGoal = (body: {
  title: string;
  description?: string;
  parent_id?: string;
  assignee_type?: string;
  assignee_id?: string;
  status?: string;
  handoff_note?: string;
  created_by_type?: string;
  created_by_id?: string;
}) => api<Goal>("/goals", { method: "POST", body: JSON.stringify(body) });
export const deleteGoal = (id: string) =>
  api<void>(`/goals/${id}`, { method: "DELETE" });
export const assignGoal = (
  id: string,
  body: { assignee_type: string; assignee_id: string; handoff_note?: string }
) => api<Goal>(`/goals/${id}/assign`, { method: "POST", body: JSON.stringify(body) });
export const cancelGoal = (id: string) =>
  api<Goal>(`/goals/${id}/cancel`, { method: "POST" });
export const waitGoalChildren = (id: string) =>
  api<void>(`/goals/${id}/wait`, { method: "POST" });

// ── Run ──
export const listGoalRuns = (goalId: string) =>
  api<Run[]>(`/goals/${goalId}/runs`);

// ── Comment ──
export const listGoalComments = (goalId: string) =>
  api<Comment[]>(`/goals/${goalId}/comments`);
export const createGoalComment = (
  goalId: string,
  body: { author_type: string; author_id: string; content: string; parent_id?: string }
) => api<Comment>(`/goals/${goalId}/comments`, { method: "POST", body: JSON.stringify(body) });

// ── Squad ──
export const listSquads = () => api<Squad[]>("/squads");
export const getSquad = (id: string) => api<Squad>(`/squads/${id}`);
export const createSquad = (body: {
  name: string;
  leader_id: string;
  description?: string;
  instructions?: string;
}) => api<Squad>("/squads", { method: "POST", body: JSON.stringify(body) });
export const deleteSquad = (id: string) =>
  api<void>(`/squads/${id}`, { method: "DELETE" });
export const addSquadMember = (
  squadId: string,
  body: { member_type: string; member_id: string; role?: string }
) => api<SquadMember>(`/squads/${squadId}/members`, { method: "POST", body: JSON.stringify(body) });
export const listSquadMembers = (squadId: string) =>
  api<SquadMember[]>(`/squads/${squadId}/members`);

// ── Schedule ──
export const listSchedules = () => api<Schedule[]>("/schedules");
export const getSchedule = (id: string) => api<Schedule>(`/schedules/${id}`);
export const createSchedule = (body: {
  name: string;
  title_template: string;
  description?: string;
  assignee_type?: string;
  assignee_id: string;
  cron_expression: string;
  timezone?: string;
}) => api<Schedule>("/schedules", { method: "POST", body: JSON.stringify(body) });
export const deleteSchedule = (id: string) =>
  api<void>(`/schedules/${id}`, { method: "DELETE" });
