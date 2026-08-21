"use client";

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  listRuntimes,
  createRuntime,
  deleteRuntime,
  listMachines,
  listSkills,
  createSkill,
  deleteSkill,
  listAgents,
  createAgent,
  updateAgent,
  deleteAgent,
  listGoals,
  getGoal,
  createGoal,
  deleteGoal,
  assignGoal,
  cancelGoal,
  resolveGoalReview,
  reopenGoal,
  activateGoal,
  continueGoal,
  listGoalRuns,
  listGoalRunMessages,
  listGoalComments,
  listGoalTimeline,
  createGoalComment,
  listSquads,
  createSquad,
  deleteSquad,
  addSquadMember,
  listSquadMembers,
  updateSquad,
  removeSquadMember,
  listSchedules,
  createSchedule,
  updateSchedule,
  deleteSchedule,
  setScheduleEnabled,
  listScheduleRuns,
  listDomains,
  listSubGoals,
  createSubGoal,
  listSubGoalVerifications,
  listGoalChanges,
  getDomain,
  createDomain,
  updateDomain,
  deleteDomain,
  compileDomainPolicy,
  getDomainCompileRun,
  freezeDomainChecks,
  getImStatus,
  connectFeishu,
  disconnectFeishu,
  getPlatformSettings,
  savePlatformSettings,
  getGateStats,
  importTeam,
  getTeamImport,
} from "./api";
import { useWSEvent } from "./ws";
import type { WSEvent } from "./types";

// ── Query keys ──
export const qk = {
  runtimes: ["runtimes"] as const,
  machines: ["machines"] as const,
  skills: ["skills"] as const,
  agents: ["agents"] as const,
  goals: ["goals"] as const,
  goal: (id: string) => ["goals", id] as const,
  goalRuns: (goalId: string) => ["goals", goalId, "runs"] as const,
  goalComments: (goalId: string) => ["goals", goalId, "comments"] as const,
  goalTimeline: (goalId: string) => ["goals", goalId, "timeline"] as const,
  goalSubGoals: (goalId: string) => ["goals", goalId, "sub-goals"] as const,
  goalChanges: (goalId: string) => ["goals", goalId, "changes"] as const,
  subGoalVerifications: (goalId: string, subGoalId: string) =>
    ["goals", goalId, "sub-goals", subGoalId, "verifications"] as const,
  squads: ["squads"] as const,
  squad: (id: string) => ["squads", id] as const,
  squadMembers: (squadId: string) => ["squads", squadId, "members"] as const,
  schedules: ["schedules"] as const,
  domains: ["domains"] as const,
  domain: (id: string) => ["domains", id] as const,
  domainCompileRun: (id: string) => ["domains", id, "compile-run"] as const,
  im: ["im"] as const,
  platformSettings: ["platform-settings"] as const,
  gateStats: ["gate-stats"] as const,
  teamImport: (runId: string) => ["team-import", runId] as const,
};

// ── Platform settings (M3) ──
export function usePlatformSettings() {
  return useQuery({ queryKey: qk.platformSettings, queryFn: getPlatformSettings });
}
export function useSavePlatformSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: savePlatformSettings,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.platformSettings }),
  });
}

// ── Runtime hooks ──
export function useRuntimes() {
  return useQuery({ queryKey: qk.runtimes, queryFn: listRuntimes });
}
export function useMachines() {
  return useQuery({ queryKey: qk.machines, queryFn: listMachines, refetchInterval: 10000 });
}
export function useSkills() {
  return useQuery({ queryKey: qk.skills, queryFn: listSkills });
}
export function useCreateSkill() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createSkill,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.skills }),
  });
}
export function useDeleteSkill() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteSkill,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.skills }),
  });
}
export function useCreateRuntime() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createRuntime,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.runtimes }),
  });
}
export function useDeleteRuntime() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteRuntime,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.runtimes }),
  });
}

// ── Agent hooks ──
export function useAgents() {
  return useQuery({ queryKey: qk.agents, queryFn: listAgents });
}
export function useCreateAgent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createAgent,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.agents }),
  });
}
export function useUpdateAgent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string; name: string; runtime_id: string; system_prompt: string; model: string; env: Record<string, string>; max_concurrent: number }) =>
      updateAgent(id, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.agents }),
  });
}

export function useDeleteAgent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteAgent,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.agents }),
  });
}

// ── Goal hooks ──
export function useGoals() {
  return useQuery({ queryKey: qk.goals, queryFn: listGoals });
}
export function useGoal(id: string) {
  return useQuery({ queryKey: qk.goal(id), queryFn: () => getGoal(id) });
}
export function useCreateGoal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createGoal,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.goals }),
  });
}
export function useDeleteGoal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteGoal,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.goals }),
  });
}
export function useAssignGoal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string; assignee_type: string; assignee_id: string; handoff_note?: string }) =>
      assignGoal(id, body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.goals });
    },
  });
}
export function useCancelGoal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: cancelGoal,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.goals }),
  });
}
export function useReopenGoal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => reopenGoal(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.goals });
    },
  });
}
export function useActivateGoal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: activateGoal,
    onSuccess: (_data, id) => {
      qc.invalidateQueries({ queryKey: qk.goals });
      qc.invalidateQueries({ queryKey: qk.goal(id) });
    },
  });
}
// Continue a paused goal (run stopped, goal still active): enqueues a fresh
// owner run. Invalidate runs + goal so the runs panel + the "正在执行" chip flip.
export function useContinueGoal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: continueGoal,
    onSuccess: (_data, id) => {
      qc.invalidateQueries({ queryKey: qk.goals });
      qc.invalidateQueries({ queryKey: qk.goal(id) });
      qc.invalidateQueries({ queryKey: ["goals", id, "runs"] });
    },
  });
}
export function useResolveGoalReview() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string; decision: "approve" | "reject" | "redirect"; reason?: string }) =>
      resolveGoalReview(id, body),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: qk.goals });
      qc.invalidateQueries({ queryKey: qk.goal(vars.id) });
    },
  });
}

// ── Run hooks ──
export function useGoalRuns(goalId: string) {
  return useQuery({
    queryKey: qk.goalRuns(goalId),
    queryFn: () => listGoalRuns(goalId),
  });
}
export function useGoalRunMessages(goalId: string, runId: string, enabled = true) {
  return useQuery({
    queryKey: ["goal-runs", runId, "messages"],
    queryFn: () => listGoalRunMessages(goalId, runId),
    enabled: enabled && !!runId,
  });
}

// ── Sub-goal hooks ──
export function useSubGoals(goalId: string) {
  return useQuery({
    queryKey: qk.goalSubGoals(goalId),
    queryFn: () => listSubGoals(goalId),
  });
}
export function useCreateSubGoal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ goalId, ...body }: { goalId: string; title: string; description?: string; assignee_id: string; verifier_id?: string }) =>
      createSubGoal(goalId, body),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: qk.goalSubGoals(vars.goalId) });
      qc.invalidateQueries({ queryKey: qk.goalChanges(vars.goalId) });
    },
  });
}

// A sub-goal's verification rounds — fetched lazily when its row expands.
export function useSubGoalVerifications(goalId: string, subGoalId: string, enabled = true) {
  return useQuery({
    queryKey: qk.subGoalVerifications(goalId, subGoalId),
    queryFn: () => listSubGoalVerifications(goalId, subGoalId),
    enabled: enabled && !!subGoalId,
  });
}

// ── Change hooks (v2) ──
export function useGoalChanges(goalId: string) {
  return useQuery({
    queryKey: qk.goalChanges(goalId),
    queryFn: () => listGoalChanges(goalId),
  });
}

// ── Comment hooks ──
export function useGoalComments(goalId: string) {
  return useQuery({
    queryKey: qk.goalComments(goalId),
    queryFn: () => listGoalComments(goalId),
  });
}
export function useGoalTimeline(goalId: string) {
  return useQuery({
    queryKey: qk.goalTimeline(goalId),
    queryFn: () => listGoalTimeline(goalId),
  });
}
export function useCreateGoalComment() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ goalId, ...body }: { goalId: string; author_type: string; author_id: string; content: string; parent_id?: string }) =>
      createGoalComment(goalId, body),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: qk.goalComments(vars.goalId) });
    },
  });
}

// ── Squad hooks ──
export function useSquads() {
  return useQuery({ queryKey: qk.squads, queryFn: listSquads });
}
export function useCreateSquad() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createSquad,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.squads }),
  });
}
export function useDeleteSquad() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteSquad,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.squads }),
  });
}
export function useSquadMembers(squadId: string) {
  return useQuery({
    queryKey: qk.squadMembers(squadId),
    queryFn: () => listSquadMembers(squadId),
  });
}
export function useAddSquadMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ squadId, ...body }: { squadId: string; member_type: string; member_id: string; role?: string }) =>
      addSquadMember(squadId, body),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: qk.squadMembers(vars.squadId) });
    },
  });
}

export function useUpdateSquad() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string; name: string; description: string; leader_id: string; instructions: string }) =>
      updateSquad(id, body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.squads });
    },
  });
}

export function useRemoveSquadMember() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ squadId, memberId }: { squadId: string; memberId: string }) =>
      removeSquadMember(squadId, memberId),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: qk.squadMembers(vars.squadId) });
    },
  });
}

// ── Domain hooks ──
export function useDomains() {
  return useQuery({ queryKey: qk.domains, queryFn: listDomains });
}
export function useDomain(id: string) {
  return useQuery({ queryKey: qk.domain(id), queryFn: () => getDomain(id), enabled: !!id });
}
// Latest compile processor run for a domain (决策 6-23): the compile panel's
// refresh-restore source of truth.
export function useDomainCompileRun(id: string) {
  return useQuery({
    queryKey: qk.domainCompileRun(id),
    queryFn: () => getDomainCompileRun(id),
    enabled: !!id,
  });
}
export function useCreateDomain() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createDomain,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.domains }),
  });
}
export function useUpdateDomain() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: Parameters<typeof updateDomain>[1] }) =>
      updateDomain(id, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.domains }),
  });
}
export function useDeleteDomain() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteDomain,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.domains }),
  });
}
export function useCompileDomainPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string; policy_text: string; processor_agent_id: string }) =>
      compileDomainPolicy(id, body),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: qk.domain(vars.id) });
    },
  });
}
export function useFreezeDomainChecks() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string; checks: import("./types").Checks; verification_strength: string }) =>
      freezeDomainChecks(id, body),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: qk.domain(vars.id) });
      qc.invalidateQueries({ queryKey: qk.domains });
    },
  });
}

// ── Gate health hooks ──
export function useGateStats() {
  return useQuery({ queryKey: qk.gateStats, queryFn: getGateStats });
}

// ── IM hooks ──
export function useImStatus() {
  return useQuery({
    queryKey: qk.im,
    queryFn: getImStatus,
    refetchInterval: 3000, // the connect flow advances server-side (QR → connected)
  });
}
export function useConnectFeishu() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: connectFeishu,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.im }),
  });
}
export function useDisconnectFeishu() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: disconnectFeishu,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.im }),
  });
}

// ── Schedule hooks ──
export function useSchedules() {
  return useQuery({ queryKey: qk.schedules, queryFn: listSchedules });
}
export function useCreateSchedule() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createSchedule,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedules }),
  });
}
export function useUpdateSchedule() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...body }: Parameters<typeof updateSchedule>[1] & { id: string }) =>
      updateSchedule(id, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedules }),
  });
}
export function useDeleteSchedule() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteSchedule,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedules }),
  });
}
export function useSetScheduleEnabled() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => setScheduleEnabled(id, enabled),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedules }),
  });
}
// A schedule's firing history — fetched lazily when its detail opens.
export function useScheduleRuns(scheduleId: string, enabled = true) {
  return useQuery({
    queryKey: ["schedules", scheduleId, "runs"],
    queryFn: () => listScheduleRuns(scheduleId),
    enabled: enabled && !!scheduleId,
  });
}

// ── Team import hooks ──
export function useImportTeam() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: importTeam,
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: qk.agents });
      qc.invalidateQueries({ queryKey: qk.squads });
      qc.invalidateQueries({ queryKey: qk.skills });
      qc.invalidateQueries({ queryKey: qk.domains });
      qc.setQueryData(qk.teamImport(data.team_import.run_id), data.team_import);
    },
  });
}
export function useTeamImport(runId: string) {
  return useQuery({
    queryKey: qk.teamImport(runId),
    queryFn: () => getTeamImport(runId),
    enabled: !!runId,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status === "pending" ? 2000 : false;
    },
  });
}

// ── WebSocket event → cache invalidation ──
export function useGoalEvents() {
  const qc = useQueryClient();
  // Goal-scoped events: invalidate the list AND the specific goal's detail —
  // the detail page (attention badge, handoff note, review state) must not
  // go stale until a page reload. Payloads carry goal_id (goal:created and
  // goal:assigned carry the Goal struct under `id` instead).
  const invalidateGoal = (p: WSEvent["payload"]) => {
    qc.invalidateQueries({ queryKey: qk.goals });
    const m = p as Record<string, unknown> | undefined;
    const id = m?.goal_id ?? m?.id;
    if (typeof id === "string" && id) {
      qc.invalidateQueries({ queryKey: qk.goal(id) });
      qc.invalidateQueries({ queryKey: qk.goalChanges(id) });
    }
  };
  useWSEvent("goal:created", invalidateGoal);
  useWSEvent("goal:assigned", invalidateGoal);
  useWSEvent("goal:finished", invalidateGoal);
  useWSEvent("goal:retrying", invalidateGoal);
  useWSEvent("goal:retry_failed", invalidateGoal);
  useWSEvent("goal:deleted", invalidateGoal);
  useWSEvent("goal:reviewing", invalidateGoal);
  useWSEvent("goal:approved", invalidateGoal);
  useWSEvent("goal:review_resolved", invalidateGoal);
  useWSEvent("goal:delivered", invalidateGoal);
  useWSEvent("goal:deliver_failed", invalidateGoal);
  // run.terminal re-derives the goal's attention (the Coordinator persists
  // it) — the detail badge follows without a reload. NOTE the dot: the
  // backend publishes "run.terminal" (P1-2, 决策 6-15⑧).
  useWSEvent("run.terminal", (p) => {
    const m = p as Record<string, unknown> | undefined;
    const id = m?.goal_id;
    if (typeof id === "string" && id) {
      qc.invalidateQueries({ queryKey: qk.goal(id) });
      qc.invalidateQueries({ queryKey: qk.goalRuns(id) });
      qc.invalidateQueries({ queryKey: qk.goalChanges(id) });
    }
  });
  // The review window's PHASE derives from the goal's review runs (决策 6-19
  // 延伸): claim flips awaiting_review → reviewing, terminal flips to
  // awaiting_approval. The goal row and the runs panel refresh together so
  // the phase badge and the approval gating follow live.
  const invalidateRuns = (p: WSEvent["payload"]) => {
    const m = p as Record<string, unknown> | undefined;
    const id = m?.goal_id;
    if (typeof id === "string" && id) {
      qc.invalidateQueries({ queryKey: qk.goal(id) });
      qc.invalidateQueries({ queryKey: qk.goalRuns(id) });
      qc.invalidateQueries({ queryKey: qk.goals });
    }
  };
  useWSEvent("run:claimed", invalidateRuns);
  useWSEvent("run:enqueued", invalidateRuns);
  useWSEvent("run:cancelled", invalidateRuns);
  // change.* events re-derive attention too, and refresh the change panel.
  useWSEvent("change.ready", invalidateGoal);
  useWSEvent("change.integrated", invalidateGoal);
  useWSEvent("change.conflict", invalidateGoal);
  // Agent lifecycle events
  useWSEvent("agent:created", () => qc.invalidateQueries({ queryKey: qk.agents }));
  useWSEvent("agent:deleted", () => qc.invalidateQueries({ queryKey: qk.agents }));
  // Squad events — the member events invalidate the whole "squads" prefix so
  // an open squad detail page sees roster changes too (P1-2, 决策 6-15⑧).
  useWSEvent("squad:created", () => qc.invalidateQueries({ queryKey: qk.squads }));
  useWSEvent("squad:deleted", () => qc.invalidateQueries({ queryKey: qk.squads }));
  useWSEvent("squad:member_added", () => qc.invalidateQueries({ queryKey: ["squads"] }));
  useWSEvent("squad:member_removed", () => qc.invalidateQueries({ queryKey: ["squads"] }));
  // Schedule events
  useWSEvent("schedule:created", () => qc.invalidateQueries({ queryKey: qk.schedules }));
  // Domain events
  useWSEvent("domain:created", () => qc.invalidateQueries({ queryKey: qk.domains }));
  useWSEvent("domain:deleted", () => qc.invalidateQueries({ queryKey: qk.domains }));
  useWSEvent("domain:compiled", () => qc.invalidateQueries({ queryKey: qk.domains }));
  useWSEvent("domain:compile_failed", () => qc.invalidateQueries({ queryKey: qk.domains }));
  // Team import events
  useWSEvent("team:imported", () => {
    qc.invalidateQueries({ queryKey: qk.agents });
    qc.invalidateQueries({ queryKey: qk.squads });
    qc.invalidateQueries({ queryKey: qk.skills });
    qc.invalidateQueries({ queryKey: qk.domains });
    qc.invalidateQueries({ queryKey: ["team-import"] });
  });
  useWSEvent("team:import_failed", () => qc.invalidateQueries({ queryKey: ["team-import"] }));
}
