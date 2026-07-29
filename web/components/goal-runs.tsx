"use client";

import { useState, useMemo } from "react";
import { useGoalRuns } from "@/lib/queries";
import { useWSEvent } from "@/lib/ws";
import { Badge, Empty } from "@/components/ui";
import type { Run } from "@/lib/types";

export function GoalRuns({ goalId }: { goalId: string }) {
  const { data: runs, isLoading, refetch } = useGoalRuns(goalId);
  const [showPast, setShowPast] = useState(false);

  // Refresh on run events
  useWSEvent("run:enqueued", () => refetch());
  useWSEvent("run:event", () => refetch());

  const activeRuns = useMemo(() => runs?.filter((r) => r.status === "queued" || r.status === "running") ?? [], [runs]);
  const pastRuns = useMemo(
    () => (runs?.filter((r) => r.status !== "queued" && r.status !== "running") ?? [])
      .sort((a, b) => {
        const aTime = a.finished_at || a.started_at || a.created_at || "";
        const bTime = b.finished_at || b.started_at || b.created_at || "";
        return bTime.localeCompare(aTime);
      }),
    [runs]
  );

  return (
    <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
      <div className="px-4 py-2.5 border-b border-zinc-100 text-xs font-medium text-zinc-500 uppercase tracking-wide">
        运行历史{runs && runs.length > 0 && `（${runs.length}）`}
      </div>
      <div className="p-4">
        {isLoading ? (
          <div className="text-sm text-zinc-400 text-center py-8">加载中…</div>
        ) : !runs || runs.length === 0 ? (
          <Empty>尚未有运行记录。</Empty>
        ) : (
          <div className="space-y-3">
            {/* Active runs */}
            {activeRuns.length > 0 && (
              <div>
                <div className="text-xs font-medium text-zinc-500 mb-2">活跃运行</div>
                <RunTable runs={activeRuns} />
              </div>
            )}

            {/* Past runs (collapsible) */}
            {pastRuns.length > 0 && (
              <div>
                <button
                  onClick={() => setShowPast(!showPast)}
                  className="text-xs font-medium text-zinc-500 hover:text-zinc-700 mb-2 flex items-center gap-1"
                >
                  {showPast ? "▾" : "▸"} 历史运行（{pastRuns.length}）
                </button>
                {showPast && <RunTable runs={pastRuns} />}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function RunTable({ runs }: { runs: Run[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-zinc-100">
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">Agent</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">状态</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">尝试</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">结果</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">开始</th>
            <th className="text-left py-1.5 px-2 font-medium text-zinc-500">结束</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.id} className="border-b border-zinc-50 hover:bg-zinc-50/50">
              <td className="py-1.5 px-2 text-zinc-700 font-mono text-xs">{r.agent_id?.slice(0, 8) ?? "-"}</td>
              <td className="py-1.5 px-2"><Badge status={r.status} /></td>
              <td className="py-1.5 px-2 text-zinc-500">#{r.attempt}</td>
              <td className="py-1.5 px-2 text-zinc-500 max-w-[200px] truncate">{r.result_summary || "-"}</td>
              <td className="py-1.5 px-2 text-zinc-400">{r.started_at ? new Date(r.started_at).toLocaleString("zh-CN") : "-"}</td>
              <td className="py-1.5 px-2 text-zinc-400">{r.finished_at ? new Date(r.finished_at).toLocaleString("zh-CN") : "-"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
