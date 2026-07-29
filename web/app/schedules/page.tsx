"use client";

import { useState } from "react";
import { useSchedules, useAgents, useSquads, useCreateSchedule, useDeleteSchedule, useGoalEvents } from "@/lib/queries";
import { Button, PageHeader, Empty, Dialog, Field, inputCls, ConfirmDialog } from "@/components/ui";
import type { Schedule } from "@/lib/types";

export default function SchedulesPage() {
  useGoalEvents();
  const { data: schedules, isLoading } = useSchedules();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const createSchedule = useCreateSchedule();
  const deleteSchedule = useDeleteSchedule();
  const [showForm, setShowForm] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;
  const squadName = (sid: string) => squads?.find((s) => s.id === sid)?.name ?? sid;
  const assigneeLabel = (s: Schedule) =>
    s.assignee_type === "squad" ? (squadName(s.assignee_id) || s.assignee_id) : (agentName(s.assignee_id) || s.assignee_id);

  return (
    <div className="p-8">
      <PageHeader
        title="Schedule"
        action={<Button onClick={() => setShowForm(true)}>+ 新建</Button>}
      />

      {isLoading ? (
        <div className="text-sm text-zinc-400 py-16 text-center">加载中…</div>
      ) : !schedules || schedules.length === 0 ? (
        <Empty>暂无定时任务。点「+ 新建」创建一个。</Empty>
      ) : (
        <div className="bg-white rounded-xl border border-zinc-200 overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-zinc-100 bg-zinc-50/50 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                <th className="px-4 py-3">名称</th>
                <th className="px-4 py-3">Cron</th>
                <th className="px-4 py-3">负责人</th>
                <th className="px-4 py-3">时区</th>
                <th className="px-4 py-3">下次触发</th>
                <th className="px-4 py-3">创建时间</th>
                <th className="px-4 py-3 w-20"></th>
              </tr>
            </thead>
            <tbody>
              {schedules.map((s: Schedule) => (
                <tr key={s.id} className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
                  <td className="px-4 py-3 font-medium text-zinc-900">{s.name}</td>
                  <td className="px-4 py-3">
                    <code className="text-xs bg-zinc-100 px-1.5 py-0.5 rounded text-zinc-700">{s.cron_expression}</code>
                  </td>
                  <td className="px-4 py-3 text-zinc-600">{assigneeLabel(s)}</td>
                  <td className="px-4 py-3 text-zinc-500">{s.timezone || "UTC"}</td>
                  <td className="px-4 py-3 text-zinc-400 text-xs">
                    {s.next_run_at ? new Date(s.next_run_at).toLocaleString("zh-CN") : "-"}
                  </td>
                  <td className="px-4 py-3 text-zinc-400 text-xs">
                    {new Date(s.created_at).toLocaleString("zh-CN")}
                  </td>
                  <td className="px-4 py-3 text-right">
                    <button
                      onClick={() => setDeleteTarget(s.id)}
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

      {showForm && <NewScheduleForm agents={agents} squads={squads} onClose={() => setShowForm(false)} />}
      {deleteTarget && (
        <ConfirmDialog
          title="确认删除"
          message="确定要删除此 Schedule 吗？"
          onConfirm={() => deleteSchedule.mutate(deleteTarget, { onSuccess: () => setDeleteTarget(null) })}
          onClose={() => setDeleteTarget(null)}
          loading={deleteSchedule.isPending}
        />
      )}
    </div>
  );
}

function NewScheduleForm({
  agents,
  squads,
  onClose,
}: {
  agents?: { id: string; name: string }[];
  squads?: { id: string; name: string }[];
  onClose: () => void;
}) {
  const createSchedule = useCreateSchedule();
  const [name, setName] = useState("");
  const [titleTemplate, setTitleTemplate] = useState("");
  const [description, setDescription] = useState("");
  const [assigneeType, setAssigneeType] = useState("agent");
  const [assigneeId, setAssigneeId] = useState("");
  const [cronExpression, setCronExpression] = useState("");
  const [timezone, setTimezone] = useState("UTC");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createSchedule.mutate(
      { name, title_template: titleTemplate, description, assignee_type: assigneeType, assignee_id: assigneeId, cron_expression: cronExpression, timezone },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="新建 Schedule"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="schedule-form" disabled={createSchedule.isPending}>
            {createSchedule.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="schedule-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称" hint="必填">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} required placeholder="定时任务名称…" />
        </Field>
        <Field label="标题模板" hint="必填，每次触发时使用此模板创建 Goal 标题">
          <input value={titleTemplate} onChange={(e) => setTitleTemplate(e.target.value)} className={inputCls} required placeholder="每日站会 {{date}}" />
        </Field>
        <Field label="描述">
          <textarea value={description} onChange={(e) => setDescription(e.target.value)} className={inputCls} rows={2} />
        </Field>
        <Field label="负责人类型">
          <select value={assigneeType} onChange={(e) => { setAssigneeType(e.target.value); setAssigneeId(""); }} className={inputCls}>
            <option value="agent">Agent</option>
            <option value="squad">Squad</option>
          </select>
        </Field>
        {assigneeType === "agent" && (
          <Field label="选择 Agent" hint="必填">
            <select value={assigneeId} onChange={(e) => setAssigneeId(e.target.value)} className={inputCls} required>
              <option value="">选择…</option>
              {agents?.map((a) => (
                <option key={a.id} value={a.id}>{a.name}</option>
              ))}
            </select>
          </Field>
        )}
        {assigneeType === "squad" && (
          <Field label="选择 Squad" hint="必填">
            <select value={assigneeId} onChange={(e) => setAssigneeId(e.target.value)} className={inputCls} required>
              <option value="">选择…</option>
              {squads?.map((s) => (
                <option key={s.id} value={s.id}>{s.name}</option>
              ))}
            </select>
          </Field>
        )}
        <Field label="Cron 表达式" hint="5 字段 cron: 分 时 日 月 星期（必填）">
          <input value={cronExpression} onChange={(e) => setCronExpression(e.target.value)} className={`${inputCls} font-mono`} required placeholder="0 9 * * 1-5" />
        </Field>
        <Field label="时区">
          <input value={timezone} onChange={(e) => setTimezone(e.target.value)} className={inputCls} placeholder="UTC" />
        </Field>
        {createSchedule.isError && (
          <p className="text-sm text-red-500">{String(createSchedule.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
