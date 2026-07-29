"use client";

import { useState } from "react";
import { useAgents, useRuntimes, useDeleteAgent, useGoalEvents } from "@/lib/queries";
import { AgentForm } from "@/components/agent-form";
import { Button, PageHeader, Empty } from "@/components/ui";
import type { Agent } from "@/lib/types";

export default function AgentsPage() {
  useGoalEvents();
  const { data: agents, isLoading } = useAgents();
  const { data: runtimes } = useRuntimes();
  const del = useDeleteAgent();
  const [showForm, setShowForm] = useState(false);

  const runtimeName = (id: string) => runtimes?.find((r) => r.id === id)?.name ?? id;

  return (
    <div className="p-8">
      <PageHeader
        title="Agent"
        action={<Button onClick={() => setShowForm(true)}>+ 新建</Button>}
      />

      {showForm && <AgentForm onClose={() => setShowForm(false)} />}

      {isLoading && <p className="text-sm text-zinc-400">加载中…</p>}

      {agents && agents.length > 0 && (
        <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-zinc-100 bg-zinc-50/50 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                <th className="px-4 py-3">名称</th>
                <th className="px-4 py-3">Runtime</th>
                <th className="px-4 py-3">Model</th>
                <th className="px-4 py-3">并发</th>
                <th className="px-4 py-3">创建时间</th>
                <th className="px-4 py-3 w-20"></th>
              </tr>
            </thead>
            <tbody>
              {agents.map((a: Agent) => (
                <tr key={a.id} className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
                  <td className="px-4 py-3 font-medium text-zinc-900">{a.name}</td>
                  <td className="px-4 py-3 text-zinc-600">{runtimeName(a.runtime_id)}</td>
                  <td className="px-4 py-3 text-zinc-600">{a.model || "-"}</td>
                  <td className="px-4 py-3 text-zinc-600">{a.max_concurrent}</td>
                  <td className="px-4 py-3 text-zinc-400">{new Date(a.created_at).toLocaleString()}</td>
                  <td className="px-4 py-3 text-right">
                    <button
                      onClick={() => del.mutate(a.id)}
                      className="text-xs text-zinc-400 hover:text-red-600 transition-colors"
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {agents && agents.length === 0 && (
        <Empty>还没有 agent。点「+ 新建」创建一个。</Empty>
      )}
    </div>
  );
}
