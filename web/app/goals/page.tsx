"use client";

import { useState } from "react";
import Link from "next/link";
import { useGoals, useAgents, useSquads, useCreateGoal, useGoalEvents } from "@/lib/queries";
import { Badge, Button, PageHeader, Empty, Dialog, Field, inputCls } from "@/components/ui";
import type { Goal, GoalStatus } from "@/lib/types";

const STATUS_TABS: { label: string; value: GoalStatus | "all" }[] = [
  { label: "全部", value: "all" },
  { label: "backlog", value: "backlog" },
  { label: "active", value: "active" },
  { label: "blocked", value: "blocked" },
  { label: "done", value: "done" },
  { label: "failed", value: "failed" },
  { label: "cancelled", value: "cancelled" },
];

export default function GoalsPage() {
  useGoalEvents();
  const { data: goals, isLoading } = useGoals();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const createGoal = useCreateGoal();
  const [filter, setFilter] = useState<GoalStatus | "all">("all");
  const [showForm, setShowForm] = useState(false);

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;
  const squadName = (sid: string) => squads?.find((s) => s.id === sid)?.name ?? sid;

  const filtered = goals?.filter((g) => filter === "all" || g.status === filter) ?? [];

  return (
    <div className="p-8">
      <PageHeader
        title="Goal"
        action={
          <Button onClick={() => setShowForm(true)}>+ 新建</Button>
        }
      />

      {/* Status filter tabs */}
      <div className="flex gap-1 mb-4 flex-wrap">
        {STATUS_TABS.map((tab) => {
          const active = filter === tab.value;
          const count = tab.value === "all"
            ? goals?.length ?? 0
            : goals?.filter((g) => g.status === tab.value).length ?? 0;
          return (
            <button
              key={tab.value}
              onClick={() => setFilter(tab.value)}
              className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                active
                  ? "bg-zinc-900 text-white"
                  : "bg-zinc-100 text-zinc-600 hover:bg-zinc-200"
              }`}
            >
              {tab.label}
              {count > 0 && (
                <span className={`ml-1.5 ${active ? "text-zinc-300" : "text-zinc-400"}`}>
                  {count}
                </span>
              )}
            </button>
          );
        })}
      </div>

      {/* Goals table */}
      {isLoading ? (
        <div className="text-sm text-zinc-400 py-16 text-center">加载中…</div>
      ) : filtered.length === 0 ? (
        <Empty>
          {filter === "all" ? "暂无 Goal，点击「+ 新建」创建第一个。" : "没有符合条件的 Goal。"}
        </Empty>
      ) : (
        <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-zinc-100 bg-zinc-50/50">
                <th className="text-left px-4 py-2.5 text-xs font-medium text-zinc-500 uppercase tracking-wide">标题</th>
                <th className="text-left px-4 py-2.5 text-xs font-medium text-zinc-500 uppercase tracking-wide">负责人</th>
                <th className="text-left px-4 py-2.5 text-xs font-medium text-zinc-500 uppercase tracking-wide">状态</th>
                <th className="text-left px-4 py-2.5 text-xs font-medium text-zinc-500 uppercase tracking-wide">创建时间</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((g: Goal) => (
                <tr key={g.id} className="border-b border-zinc-50 hover:bg-zinc-50/50 transition-colors">
                  <td className="px-4 py-2.5">
                    <Link href={`/goals/${g.id}`} className="font-medium text-zinc-900 hover:text-blue-600 hover:underline">
                      {g.title}
                    </Link>
                  </td>
                  <td className="px-4 py-2.5 text-zinc-500">
                    {g.assignee_type === "squad"
                      ? (squadName(g.assignee_id) || g.assignee_id)
                      : g.assignee_id
                        ? (agentName(g.assignee_id) || g.assignee_id)
                        : "-"}
                  </td>
                  <td className="px-4 py-2.5">
                    <Badge status={g.status} />
                  </td>
                  <td className="px-4 py-2.5 text-zinc-400 text-xs">
                    {g.created_at ? new Date(g.created_at).toLocaleString("zh-CN") : "-"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Create Goal dialog */}
      {showForm && <NewGoalForm agents={agents} squads={squads} onClose={() => setShowForm(false)} />}

      {createGoal.isError && (
        <p className="text-sm text-red-500 mt-2">{String(createGoal.error)}</p>
      )}
    </div>
  );
}

function NewGoalForm({
  agents,
  squads,
  onClose,
}: {
  agents?: { id: string; name: string }[];
  squads?: { id: string; name: string }[];
  onClose: () => void;
}) {
  const createGoal = useCreateGoal();
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [assigneeType, setAssigneeType] = useState("");
  const [assigneeId, setAssigneeId] = useState("");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const body: Record<string, string> = { title };
    if (description) body.description = description;
    if (assigneeType && assigneeId) {
      body.assignee_type = assigneeType;
      body.assignee_id = assigneeId;
      body.status = "active";
    }
    createGoal.mutate(body as Record<string, string> & { title: string }, { onSuccess: onClose });
  };

  return (
    <Dialog
      title="新建 Goal"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="new-goal-form" disabled={createGoal.isPending}>
            {createGoal.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="new-goal-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="标题" hint="必填">
          <input value={title} onChange={(e) => setTitle(e.target.value)} className={inputCls} required placeholder="Goal 标题…" />
        </Field>
        <Field label="描述">
          <textarea value={description} onChange={(e) => setDescription(e.target.value)} className={inputCls} rows={3} placeholder="可选描述…" />
        </Field>
        <Field label="负责人类型">
          <select value={assigneeType} onChange={(e) => { setAssigneeType(e.target.value); setAssigneeId(""); }} className={inputCls}>
            <option value="">无（进入 backlog）</option>
            <option value="agent">Agent</option>
            <option value="squad">Squad</option>
          </select>
        </Field>
        {assigneeType === "agent" && (
          <Field label="选择 Agent">
            <select value={assigneeId} onChange={(e) => setAssigneeId(e.target.value)} className={inputCls} required>
              <option value="">选择…</option>
              {agents?.map((a) => (
                <option key={a.id} value={a.id}>{a.name}</option>
              ))}
            </select>
          </Field>
        )}
        {assigneeType === "squad" && (
          <Field label="选择 Squad">
            <select value={assigneeId} onChange={(e) => setAssigneeId(e.target.value)} className={inputCls} required>
              <option value="">选择…</option>
              {squads?.map((s) => (
                <option key={s.id} value={s.id}>{s.name}</option>
              ))}
            </select>
          </Field>
        )}
        {createGoal.isError && (
          <p className="text-sm text-red-500">{String(createGoal.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
