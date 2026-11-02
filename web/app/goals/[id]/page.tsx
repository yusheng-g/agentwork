"use client";

import { Suspense } from "react";
import { useParams, useSearchParams } from "next/navigation";
import Link from "next/link";
import { useGoal, useAgents, useSquads, useGoalEvents } from "@/lib/queries";
import { GoalActions } from "@/components/goal-actions";
import { GoalComments } from "@/components/goal-comments";
import { GoalRuns } from "@/components/goal-runs";
import { GoalSubGoals } from "@/components/goal-subgoals";
import { GoalChanges } from "@/components/goal-changes";
import { GoalStatusBar, GoalTimeline } from "@/components/goal-timeline";
import { Badge, AttentionChip } from "@/components/ui";
import type { Goal } from "@/lib/types";

export default function GoalDetailPage() {
  // useSearchParams 要求 Suspense 边界。
  return (
    <Suspense fallback={<div className="p-8 text-sm text-zinc-400">加载中…</div>}>
      <GoalDetailPageInner />
    </Suspense>
  );
}

function GoalDetailPageInner() {
  useGoalEvents();
  const params = useParams<{ id: string }>();
  const searchParams = useSearchParams();
  const id = params.id;
  const { data: goal, isLoading } = useGoal(id);
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();

  if (isLoading) return <div className="p-8 text-sm text-zinc-400">加载中…</div>;
  if (!goal) return <div className="p-8 text-sm text-zinc-400">找不到 Goal。</div>;

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;
  const squadName = (sid: string) => squads?.find((s) => s.id === sid)?.name ?? sid;
  const assigneeLabel = goal.assignee_type === "squad"
    ? (squadName(goal.assignee_id) || goal.assignee_id || "-")
    : goal.assignee_id
      ? (agentName(goal.assignee_id) || goal.assignee_id)
      : "-";

  return (
    <div className="p-8 space-y-5 max-w-4xl page-enter">
      {/* Breadcrumb */}
      <div>
        {/* 从带筛选的列表进来（?status=…）时，返回列表保持筛选不丢 */}
        <Link
          href={searchParams.get("status") ? `/goals?status=${searchParams.get("status")}` : "/goals"}
          className="text-sm text-zinc-400 hover:text-zinc-700 hover:underline"
        >
          ← 返回列表
        </Link>
        <div className="flex items-center gap-3 mt-3">
          <h1 className="text-lg font-semibold text-zinc-900">{goal.title}</h1>
          <Badge status={goal.status} />
          <AttentionChip attention={goal.attention} />
          <GoalStatusBar goalId={id} goalStatus={goal.status} />
        </div>
        <p className="text-sm text-zinc-500 mt-1">
          分配给：{assigneeLabel}
        </p>
        {goal.source_ref && (
          <p className="text-xs text-zinc-400 mt-1">
            来源：<span className="font-mono">{goal.source_ref}</span>
          </p>
        )}
        {goal.description && (
          <p className="text-sm text-zinc-600 mt-3 whitespace-pre-wrap">{goal.description}</p>
        )}
        {goal.handoff_note && (
          <div className="mt-3 p-3 bg-amber-50 border border-amber-200 rounded-lg text-sm text-amber-800 whitespace-pre-wrap">
            <span className="font-medium">Handoff note：</span>
            {goal.handoff_note}
          </div>
        )}
      </div>

      {/* Actions + 执行流入口 */}
      <div className="flex items-start gap-2 flex-wrap">
        <GoalActions goal={goal} />
        <GoalTimeline goalId={id} goalStatus={goal.status} />
      </div>

      {/* Comments */}
      <GoalComments goalId={id} />

      {/* Sub-goals（v2：goal 内部拆出的工作项，不是 child goal） */}
      <GoalSubGoals goalId={id} />

      {/* Changes（v2：子任务的逻辑交付物 + 修订史——owner 的集成视图） */}
      <GoalChanges goalId={id} />

      {/* Runs */}
      <GoalRuns goalId={id} />
    </div>
  );
}
