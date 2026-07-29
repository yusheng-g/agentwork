"use client";

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  listRuntimes,
  createRuntime,
  deleteRuntime,
  listAgents,
  createAgent,
  deleteAgent,
  listGoals,
  getGoal,
  createGoal,
  deleteGoal,
  assignGoal,
  cancelGoal,
  waitGoalChildren,
  listGoalRuns,
  listGoalComments,
  createGoalComment,
  listSquads,
  createSquad,
  deleteSquad,
  addSquadMember,
  listSquadMembers,
  listSchedules,
  createSchedule,
  deleteSchedule,
} from "./api";
import { useWSEvent } from "./ws";

// ── Query keys ──
export const qk = {
  runtimes: ["runtimes"] as const,
  agents: ["agents"] as const,
  goals: ["goals"] as const,
  goal: (id: string) => ["goals", id] as const,
  goalRuns: (goalId: string) => ["goals", goalId, "runs"] as const,
  goalComments: (goalId: string) => ["goals", goalId, "comments"] as const,
  squads: ["squads"] as const,
  squad: (id: string) => ["squads", id] as const,
  squadMembers: (squadId: string) => ["squads", squadId, "members"] as const,
  schedules: ["schedules"] as const,
};

// ── Runtime hooks ──
export function useRuntimes() {
  return useQuery({ queryKey: qk.runtimes, queryFn: listRuntimes });
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
export function useWaitGoalChildren() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: waitGoalChildren,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.goals }),
  });
}

// ── Run hooks ──
export function useGoalRuns(goalId: string) {
  return useQuery({
    queryKey: qk.goalRuns(goalId),
    queryFn: () => listGoalRuns(goalId),
  });
}

// ── Comment hooks ──
export function useGoalComments(goalId: string) {
  return useQuery({
    queryKey: qk.goalComments(goalId),
    queryFn: () => listGoalComments(goalId),
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
export function useDeleteSchedule() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: deleteSchedule,
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedules }),
  });
}

// ── WebSocket event → cache invalidation ──
export function useGoalEvents() {
  const qc = useQueryClient();
  // Goal lifecycle events
  useWSEvent("goal:created", () => qc.invalidateQueries({ queryKey: qk.goals }));
  useWSEvent("goal:assigned", () => qc.invalidateQueries({ queryKey: qk.goals }));
  useWSEvent("goal:finished", () => qc.invalidateQueries({ queryKey: qk.goals }));
  useWSEvent("goal:retrying", () => qc.invalidateQueries({ queryKey: qk.goals }));
  useWSEvent("goal:retry_failed", () => qc.invalidateQueries({ queryKey: qk.goals }));
  useWSEvent("goal:waiting", () => qc.invalidateQueries({ queryKey: qk.goals }));
  useWSEvent("goal:deleted", () => qc.invalidateQueries({ queryKey: qk.goals }));
  // Agent lifecycle events
  useWSEvent("agent:created", () => qc.invalidateQueries({ queryKey: qk.agents }));
  useWSEvent("agent:deleted", () => qc.invalidateQueries({ queryKey: qk.agents }));
  // Squad events
  useWSEvent("squad:created", () => qc.invalidateQueries({ queryKey: qk.squads }));
  useWSEvent("squad:deleted", () => qc.invalidateQueries({ queryKey: qk.squads }));
  // Schedule events
  useWSEvent("schedule:created", () => qc.invalidateQueries({ queryKey: qk.schedules }));
}
