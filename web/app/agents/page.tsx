"use client";

import { useState } from "react";
import { useAgents, useRuntimes, useDeleteAgent, usePinnedAgents, useSetAgentPin, useGoalEvents } from "@/lib/queries";
import { AgentForm } from "@/components/agent-form";
import { ChatPanel } from "@/components/chat-panel";
import { Button, PageHeader, Empty } from "@/components/ui";
import { Pin, PinOff, Bot } from "lucide-react";
import type { Agent } from "@/lib/types";

export default function AgentsPage() {
  useGoalEvents();
  const { data: agents, isLoading } = useAgents();
  const { data: runtimes } = useRuntimes();
  const { data: pinnedIds } = usePinnedAgents();
  const del = useDeleteAgent();
  const toggle = useSetAgentPin();
  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<Agent | null>(null);
  const [chatting, setChatting] = useState<Agent | null>(null);

  const pinnedSet = new Set(pinnedIds ?? []);
  const runtimeName = (id: string) => runtimes?.find((r) => r.id === id)?.name ?? id;
  const pinnedAgents = (agents ?? []).filter((a) => pinnedSet.has(a.id));

  const togglePin = (a: Agent) => {
    const next = !pinnedSet.has(a.id);
    toggle.mutate({ id: a.id, pinned: next });
  };

  return (
    <div className="p-8">
      <PageHeader
        title="Agent"
        action={<Button onClick={() => setShowForm(true)}>+ 新建</Button>}
      />

      <div className="flex gap-6 items-start">
        {/* 左侧：固定的 agent 侧边栏 —— 点击直接开聊。pinned 状态来自后端
            agent_pin 表，刷新/重启后仍在。 */}
        <aside className="w-60 shrink-0 bg-white rounded-xl border border-zinc-200 overflow-hidden sticky top-8">
          <div className="px-4 py-3 border-b border-zinc-100 bg-zinc-50/50 flex items-center gap-2">
            <Pin className="h-4 w-4 text-indigo-600" />
            <span className="text-xs font-semibold text-zinc-700 uppercase tracking-wide">
              固定 ({pinnedAgents.length})
            </span>
          </div>
          {pinnedAgents.length === 0 && (
            <div className="px-4 py-8 text-center text-xs text-zinc-400 leading-relaxed">
              还没有固定的 agent。
              <br />
              在右侧列表点 📌 固定，即可在此快速对话。
            </div>
          )}
          <ul className="divide-y divide-zinc-50">
            {pinnedAgents.map((a) => (
              <li key={a.id} className="group">
                <div className="px-4 py-3 flex items-center gap-2">
                  <button
                    onClick={() => setChatting(a)}
                    className="flex items-center gap-2 min-w-0 flex-1 text-left hover:text-indigo-600 transition-colors"
                    title={`与 ${a.name} 对话`}
                  >
                    <Bot className="h-4 w-4 text-zinc-400 shrink-0" />
                    <div className="min-w-0">
                      <div className="text-sm font-medium text-zinc-900 truncate">{a.name}</div>
                      {a.description && (
                        <div className="text-xs text-zinc-400 truncate">{a.description}</div>
                      )}
                    </div>
                  </button>
                  <button
                    onClick={() => togglePin(a)}
                    className="text-zinc-300 hover:text-red-600 transition-colors opacity-0 group-hover:opacity-100 shrink-0"
                    title="取消固定"
                  >
                    <PinOff className="h-4 w-4" />
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </aside>

        {/* 右侧：全部 agent 表格，每行带固定/取消固定按钮 */}
        <div className="flex-1 min-w-0">
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
                    <th className="px-4 py-3 w-32 text-right">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {agents.map((a: Agent) => {
                    const pinned = pinnedSet.has(a.id);
                    const isSteward = a.type === "steward";
                    return (
                      <tr key={a.id} className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
                        <td className="px-4 py-3">
                          <div className="flex items-center gap-2">
                            <button onClick={() => setEditing(a)} className="font-medium text-zinc-900 hover:text-indigo-600 hover:underline" title="点击编辑 agent">
                              {a.name}
                            </button>
                            {isSteward && (
                              <span className="inline-flex items-center px-2 py-0.5 text-xs font-medium rounded-full bg-indigo-50 text-indigo-700 border border-indigo-200">
                                管家
                              </span>
                            )}
                          </div>
                          {a.description && (
                            <div className="text-xs text-zinc-400 mt-0.5 line-clamp-1">{a.description}</div>
                          )}
                        </td>
                        <td className="px-4 py-3 text-zinc-600">{runtimeName(a.runtime_id)}</td>
                        <td className="px-4 py-3 text-zinc-600">{a.model || "-"}</td>
                        <td className="px-4 py-3 text-zinc-600">{a.max_concurrent}</td>
                        <td className="px-4 py-3 text-zinc-400">{new Date(a.created_at).toLocaleString()}</td>
                        <td className="px-4 py-3 text-right space-x-2 whitespace-nowrap">
                          <button
                            onClick={() => setChatting(a)}
                            className="text-xs text-indigo-600 hover:text-indigo-800 transition-colors"
                          >
                            聊天
                          </button>
                          <button
                            onClick={() => togglePin(a)}
                            disabled={toggle.isPending && toggle.variables?.id === a.id}
                            className={
                              "inline-flex items-center gap-1 px-2 py-0.5 text-xs font-medium rounded-full transition-colors " +
                              (pinned
                                ? "bg-indigo-50 text-indigo-700 hover:bg-indigo-100"
                                : "text-zinc-400 hover:text-indigo-600")
                            }
                            title={pinned ? "取消固定" : "固定到侧边栏"}
                          >
                            <Pin className={"h-3 w-3 " + (pinned ? "fill-indigo-500 text-indigo-600" : "")} />
                            {pinned ? "已固定" : "固定"}
                          </button>
                          <button
                            onClick={() => {
                              if (confirm(`删除 agent「${a.name}」？被 goal/schedule/squad 引用时会被拒绝。`)) del.mutate(a.id);
                            }}
                            className="text-xs text-zinc-400 hover:text-red-600 transition-colors"
                          >
                            删除
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}

          {agents && agents.length === 0 && (
            <Empty>还没有 agent。点「+ 新建」创建一个。</Empty>
          )}

          {/* 删除失败必须显示——后端守卫（agent 被 goal/schedule/squad 引用）
              返回 400 coded error，静默等于「点了没反应」。 */}
          {del.isError && (
            <p className="text-sm text-red-500 mt-3">{String(del.error)}</p>
          )}
          {/* 固定/取消固定失败同样要显示（侧栏按钮无任何反馈路径）。 */}
          {toggle.isError && (
            <p className="text-sm text-red-500 mt-3">{String(toggle.error)}</p>
          )}
        </div>
      </div>

      {showForm && <AgentForm onClose={() => setShowForm(false)} />}
      {editing && <AgentForm agent={editing} onClose={() => setEditing(null)} />}
      {chatting && (
        <ChatPanel agentId={chatting.id} agentName={chatting.name} onClose={() => setChatting(null)} />
      )}
    </div>
  );
}
