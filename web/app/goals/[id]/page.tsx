"use client";

import { useParams } from "next/navigation";
import Link from "next/link";
import { useGoal, useGoals, useAgents, useSquads, useGoalEvents } from "@/lib/queries";
import { GoalActions } from "@/components/goal-actions";
import { GoalComments } from "@/components/goal-comments";
import { GoalRuns } from "@/components/goal-runs";
import { Badge, Empty } from "@/components/ui";
import type { Goal } from "@/lib/types";

export default function GoalDetailPage() {
  useGoalEvents();
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data: goal, isLoading } = useGoal(id);
  const { data: allGoals } = useGoals();
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

  const children = allGoals?.filter((t) => t.parent_id === id) ?? [];

  return (
    <div className="p-8 space-y-5 max-w-4xl">
      {/* Breadcrumb */}
      <div>
        <Link href="/goals" className="text-sm text-zinc-400 hover:text-zinc-700 hover:underline">
          ← 返回列表
        </Link>
        <div className="flex items-center gap-3 mt-3">
          <h1 className="text-lg font-semibold text-zinc-900">{goal.title}</h1>
          <Badge status={goal.status} />
        </div>
        <p className="text-sm text-zinc-500 mt-1">
          分配给：{assigneeLabel}
        </p>
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

      {/* Actions */}
      <GoalActions goal={goal} />

      {/* Comments */}
      <GoalComments goalId={id} />

      {/* Runs */}
      <GoalRuns goalId={id} />

      {/* Sub-goals */}
      <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
        <div className="px-4 py-2.5 border-b border-zinc-100 text-xs font-medium text-zinc-500 uppercase tracking-wide">
          子 Goal{children.length > 0 && `（${children.length}）`}
        </div>
        <div className="p-4">
          {children.length === 0 ? (
            <Empty>没有子 Goal。</Empty>
          ) : (
            <ul className="space-y-2">
              {children.map((c: Goal) => (
                <li key={c.id} className="flex items-center gap-2 text-sm">
                  <Link href={`/goals/${c.id}`} className="font-medium text-zinc-900 hover:text-blue-600 hover:underline">
                    {c.title}
                  </Link>
                  <Badge status={c.status} />
                  <span className="text-zinc-400 text-xs">
                    {c.assignee_type === "squad"
                      ? (squadName(c.assignee_id) || c.assignee_id)
                      : c.assignee_id
                        ? (agentName(c.assignee_id) || c.assignee_id)
                        : "-"}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </div>
  );
}
