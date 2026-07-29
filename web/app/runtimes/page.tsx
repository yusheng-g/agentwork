"use client";

import { useState } from "react";
import { useRuntimes, useDeleteRuntime, useGoalEvents } from "@/lib/queries";
import { RuntimeForm } from "@/components/runtime-form";
import { Button, PageHeader, Empty } from "@/components/ui";
import type { Runtime } from "@/lib/types";

export default function RuntimesPage() {
  useGoalEvents();
  const { data: runtimes, isLoading } = useRuntimes();
  const del = useDeleteRuntime();
  const [showForm, setShowForm] = useState(false);

  return (
    <div className="p-8">
      <PageHeader
        title="Runtime"
        action={<Button onClick={() => setShowForm(true)}>+ 新建</Button>}
      />

      {showForm && <RuntimeForm onClose={() => setShowForm(false)} />}

      {isLoading && <p className="text-sm text-zinc-400">加载中…</p>}

      {runtimes && runtimes.length > 0 && (
        <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-zinc-100 bg-zinc-50/50 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                <th className="px-4 py-3">名称</th>
                <th className="px-4 py-3">Transport</th>
                <th className="px-4 py-3">Provider</th>
                <th className="px-4 py-3">可执行 / Endpoint</th>
                <th className="px-4 py-3">创建时间</th>
                <th className="px-4 py-3 w-20"></th>
              </tr>
            </thead>
            <tbody>
              {runtimes.map((rt: Runtime) => (
                <tr key={rt.id} className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
                  <td className="px-4 py-3 font-medium text-zinc-900">{rt.name}</td>
                  <td className="px-4 py-3">
                    <span className="px-2 py-0.5 text-xs rounded bg-zinc-100 text-zinc-600">{rt.transport}</span>
                  </td>
                  <td className="px-4 py-3">
                    <span className="px-2 py-0.5 text-xs rounded bg-blue-50 text-blue-700">{rt.provider}</span>
                  </td>
                  <td className="px-4 py-3 text-zinc-600 font-mono text-xs">
                    {rt.transport === "stdio" ? rt.executable : rt.endpoint}
                  </td>
                  <td className="px-4 py-3 text-zinc-400">{new Date(rt.created_at).toLocaleString()}</td>
                  <td className="px-4 py-3 text-right">
                    <button
                      onClick={() => del.mutate(rt.id)}
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

      {runtimes && runtimes.length === 0 && (
        <Empty>还没有 runtime。点「+ 新建」创建一个。</Empty>
      )}
    </div>
  );
}
