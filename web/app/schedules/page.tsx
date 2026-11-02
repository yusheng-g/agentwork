"use client";

import { useState } from "react";
import Link from "next/link";
import { useSchedules, useAgents, useSquads, useDomains, useCreateSchedule, useUpdateSchedule, useDeleteSchedule, useSetScheduleEnabled, useGoalEvents, useScheduleRuns } from "@/lib/queries";
import { Button, PageHeader, Empty, Dialog, Field, inputCls, ConfirmDialog, Badge } from "@/components/ui";
import type { Schedule } from "@/lib/types";

export default function SchedulesPage() {
  useGoalEvents();
  const { data: schedules, isLoading } = useSchedules();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const { data: domains } = useDomains();
  const createSchedule = useCreateSchedule();
  const deleteSchedule = useDeleteSchedule();
  const setEnabled = useSetScheduleEnabled();
  const [showForm, setShowForm] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [detail, setDetail] = useState<Schedule | null>(null);
  const [editTarget, setEditTarget] = useState<Schedule | null>(null);

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;
  const squadName = (sid: string) => squads?.find((s) => s.id === sid)?.name ?? sid;
  const domainName = (did: string) => domains?.find((d) => d.id === did)?.name ?? did;
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
                <th className="px-4 py-3">项目</th>
                <th className="px-4 py-3">时区</th>
                <th className="px-4 py-3">下次触发</th>
                <th className="px-4 py-3">创建时间</th>
                <th className="px-4 py-3 w-28"></th>
              </tr>
            </thead>
            <tbody>
              {schedules.map((s: Schedule) => (
                <tr key={s.id} className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
                  <td className="px-4 py-3">
                    <button
                      onClick={() => setDetail(s)}
                      className="font-medium text-zinc-900 hover:text-indigo-700 hover:underline transition-colors text-left"
                    >
                      {s.name}
                    </button>
                  </td>
                  <td className="px-4 py-3">
                    <code className="text-xs bg-zinc-100 px-1.5 py-0.5 rounded text-zinc-700">{s.cron_expression}</code>
                  </td>
                  <td className="px-4 py-3 text-zinc-600">{assigneeLabel(s)}</td>
                  <td className="px-4 py-3 text-zinc-600">{domainName(s.domain_id)}</td>
                  <td className="px-4 py-3 text-zinc-500">{s.timezone || "UTC"}</td>
                  <td className="px-4 py-3 text-zinc-400 text-xs">
                    {s.next_run_at ? new Date(s.next_run_at).toLocaleString("zh-CN") : "-"}
                  </td>
                  <td className="px-4 py-3 text-zinc-400 text-xs">
                    {new Date(s.created_at).toLocaleString("zh-CN")}
                  </td>
                  <td className="px-4 py-3 text-right space-x-3">
                    <button
                      onClick={() => setEnabled.mutate({ id: s.id, enabled: !s.enabled })}
                      disabled={setEnabled.isPending && setEnabled.variables?.id === s.id}
                      className={`text-xs transition-colors disabled:opacity-50 ${
                        s.enabled ? "text-emerald-600 hover:text-emerald-700" : "text-zinc-400 hover:text-zinc-600"
                      }`}
                    >
                      {s.enabled ? "启用中" : "已停用"}
                    </button>
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

      {showForm && <NewScheduleForm agents={agents} squads={squads} domains={domains} onClose={() => setShowForm(false)} />}
      {detail && (
        <ScheduleDetail
          schedule={detail}
          agentName={agentName}
          squadName={squadName}
          domainName={domainName}
          onClose={() => setDetail(null)}
          onEdit={() => { setEditTarget(detail); setDetail(null); }}
        />
      )}
      {editTarget && (
        <EditScheduleDialog
          schedule={editTarget}
          agents={agents}
          squads={squads}
          domains={domains}
          onClose={() => setEditTarget(null)}
        />
      )}
      {deleteTarget && (
        <ConfirmDialog
          title="确认删除"
          message="确定要删除此 Schedule 吗？"
          onConfirm={() => deleteSchedule.mutate(deleteTarget, { onSuccess: () => setDeleteTarget(null) })}
          onClose={() => setDeleteTarget(null)}
          loading={deleteSchedule.isPending}
        />
      )}

      {/* 启停/删除失败必须显示——静默等于「点了没反应」。 */}
      {(setEnabled.isError || deleteSchedule.isError) && (
        <p className="text-sm text-red-500 mt-3">
          {String(setEnabled.error ?? deleteSchedule.error)}
        </p>
      )}
    </div>
  );
}

// ScheduleDetail is the schedule's firing history view (clicking the name):
// the template config at the top, each firing below with the goal it
// produced and that goal's current status, linked to the goal detail.
function ScheduleDetail({
  schedule,
  agentName,
  squadName,
  domainName,
  onClose,
  onEdit,
}: {
  schedule: Schedule;
  agentName: (id: string) => string;
  squadName: (id: string) => string;
  domainName: (id: string) => string;
  onClose: () => void;
  onEdit: () => void;
}) {
  const { data: runs, isLoading } = useScheduleRuns(schedule.id);
  const assigneeLabel =
    schedule.assignee_type === "squad" ? (squadName(schedule.assignee_id) || schedule.assignee_id) : (agentName(schedule.assignee_id) || schedule.assignee_id);

  return (
    <Dialog
      title={`定时任务：${schedule.name}`}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onEdit}>编辑</Button>
          <Button variant="outline" onClick={onClose}>关闭</Button>
        </>
      }
    >
      <div className="space-y-3">
        {/* Template config */}
        <div className="grid grid-cols-2 gap-x-4 gap-y-1.5 text-sm text-zinc-600">
          <span><span className="text-zinc-400 text-xs">Cron：</span><code className="text-xs bg-zinc-100 px-1.5 py-0.5 rounded">{schedule.cron_expression}</code>（{schedule.timezone || "UTC"}）</span>
          <span><span className="text-zinc-400 text-xs">负责人：</span>{assigneeLabel}</span>
          <span><span className="text-zinc-400 text-xs">项目：</span>{domainName(schedule.domain_id)}</span>
          <span>
            <span className="text-zinc-400 text-xs">状态：</span>
            <Badge status={schedule.enabled ? "running" : "cancelled"} className="scale-90" />
          </span>
          <span><span className="text-zinc-400 text-xs">下次触发：</span>{schedule.next_run_at ? new Date(schedule.next_run_at).toLocaleString("zh-CN") : "-"}</span>
          <span><span className="text-zinc-400 text-xs">上次触发：</span>{schedule.last_run_at ? new Date(schedule.last_run_at).toLocaleString("zh-CN") : "从未"}</span>
        </div>
        {schedule.title_template && (
          <p className="text-xs text-zinc-500">
            <span className="text-zinc-400">标题模板：</span>{schedule.title_template}
          </p>
        )}

        {/* Firing history */}
        <div className="border-t border-zinc-100 pt-2">
          <p className="text-xs font-medium text-zinc-500 uppercase tracking-wide mb-2">
            触发历史{runs && runs.length > 0 && `（${runs.length}）`}
          </p>
          {isLoading ? (
            <p className="text-xs text-zinc-400 py-3">加载中…</p>
          ) : !runs || runs.length === 0 ? (
            <p className="text-xs text-zinc-400 py-3">还没有触发过。</p>
          ) : (
            <ul className="space-y-1 max-h-64 overflow-y-auto">
              {runs.map((r) => (
                <li key={r.id} className="flex items-center gap-2 text-xs">
                  <span className="text-zinc-400 shrink-0">{new Date(r.planned_at).toLocaleString("zh-CN")}</span>
                  <Badge status={r.goal_status || "cancelled"} className="scale-90 shrink-0" />
                  <Link
                    href={`/goals/${r.goal_id}`}
                    onClick={onClose}
                    className="text-zinc-700 hover:text-indigo-700 hover:underline truncate"
                  >
                    {r.goal_title || r.goal_id.slice(0, 8)}
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </Dialog>
  );
}

// ScheduleFormFields is the shared field set for create + edit. It receives
// the form state + setters from the parent (the parent owns submission).
function ScheduleFormFields({
  form,
  set,
  agents,
  squads,
  domains,
}: {
  form: ScheduleFormState;
  set: (patch: Partial<ScheduleFormState>) => void;
  agents?: { id: string; name: string }[];
  squads?: { id: string; name: string }[];
  domains?: { id: string; name: string }[];
}) {
  return (
    <>
      <Field label="名称" hint="必填">
        <input value={form.name} onChange={(e) => set({ name: e.target.value })} className={inputCls} required placeholder="定时任务名称…" />
      </Field>
      <Field label="标题模板" hint="必填，每次触发时使用此模板创建 Goal 标题">
        <input value={form.title_template} onChange={(e) => set({ title_template: e.target.value })} className={inputCls} required placeholder="每日站会 {{date}}" />
      </Field>
      <Field label="描述">
        <textarea value={form.description} onChange={(e) => set({ description: e.target.value })} className={inputCls} rows={2} />
      </Field>
      <Field label="负责人类型">
        <select value={form.assignee_type} onChange={(e) => { set({ assignee_type: e.target.value, assignee_id: "" }); }} className={inputCls}>
          <option value="agent">Agent</option>
          <option value="squad">Squad</option>
        </select>
      </Field>
      {form.assignee_type === "agent" && (
        <Field label="选择 Agent" hint="必填">
          <select value={form.assignee_id} onChange={(e) => set({ assignee_id: e.target.value })} className={inputCls} required>
            <option value="">选择…</option>
            {agents?.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </Field>
      )}
      {form.assignee_type === "squad" && (
        <Field label="选择 Squad" hint="必填">
          <select value={form.assignee_id} onChange={(e) => set({ assignee_id: e.target.value })} className={inputCls} required>
            <option value="">选择…</option>
            {squads?.map((s) => (
              <option key={s.id} value={s.id}>{s.name}</option>
            ))}
          </select>
        </Field>
      )}
      <Field label="所属项目" hint="必填，每次触发时在此项目的仓库上执行（验收策略 + worktree 来自该项目）">
        <select value={form.domain_id} onChange={(e) => set({ domain_id: e.target.value })} className={inputCls} required>
          <option value="">选择…</option>
          {domains?.map((d) => (
            <option key={d.id} value={d.id}>{d.name}</option>
          ))}
        </select>
      </Field>
      <Field label="Cron 表达式" hint="5 字段 cron: 分 时 日 月 星期（必填）">
        <input value={form.cron_expression} onChange={(e) => set({ cron_expression: e.target.value })} className={`${inputCls} font-mono`} required placeholder="0 9 * * 1-5" />
      </Field>
      <Field label="时区">
        <input value={form.timezone} onChange={(e) => set({ timezone: e.target.value })} className={inputCls} placeholder="UTC" />
      </Field>
    </>
  );
}

type ScheduleFormState = {
  name: string;
  title_template: string;
  description: string;
  assignee_type: string;
  assignee_id: string;
  domain_id: string;
  cron_expression: string;
  timezone: string;
};

function NewScheduleForm({
  agents,
  squads,
  domains,
  onClose,
}: {
  agents?: { id: string; name: string }[];
  squads?: { id: string; name: string }[];
  domains?: { id: string; name: string }[];
  onClose: () => void;
}) {
  const createSchedule = useCreateSchedule();
  const [form, setForm] = useState<ScheduleFormState>({
    name: "", title_template: "", description: "",
    assignee_type: "agent", assignee_id: "", domain_id: "",
    cron_expression: "", timezone: "UTC",
  });
  const set = (patch: Partial<ScheduleFormState>) => setForm((f) => ({ ...f, ...patch }));

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createSchedule.mutate(form, { onSuccess: onClose });
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
        <ScheduleFormFields form={form} set={set} agents={agents} squads={squads} domains={domains} />
        {createSchedule.isError && (
          <p className="text-sm text-red-500">{String(createSchedule.error)}</p>
        )}
      </form>
    </Dialog>
  );
}

// EditScheduleDialog预填当前 schedule 的值，提交调 updateSchedule。
// 改 assignee/domain 只影响后续触发——用提示说明，后端不限制。
function EditScheduleDialog({
  schedule,
  agents,
  squads,
  domains,
  onClose,
}: {
  schedule: Schedule;
  agents?: { id: string; name: string }[];
  squads?: { id: string; name: string }[];
  domains?: { id: string; name: string }[];
  onClose: () => void;
}) {
  const updateSchedule = useUpdateSchedule();
  const [form, setForm] = useState<ScheduleFormState>({
    name: schedule.name,
    title_template: schedule.title_template,
    description: schedule.description,
    assignee_type: schedule.assignee_type || "agent",
    assignee_id: schedule.assignee_id,
    domain_id: schedule.domain_id,
    cron_expression: schedule.cron_expression,
    timezone: schedule.timezone || "UTC",
  });
  const set = (patch: Partial<ScheduleFormState>) => setForm((f) => ({ ...f, ...patch }));

  // assignee/domain 与原值不同时显示"仅影响后续触发"提示。
  const assigneeChanged = form.assignee_id !== schedule.assignee_id || form.assignee_type !== schedule.assignee_type;
  const domainChanged = form.domain_id !== schedule.domain_id;

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    updateSchedule.mutate({ id: schedule.id, ...form }, { onSuccess: onClose });
  };

  return (
    <Dialog
      title={`编辑：${schedule.name}`}
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="schedule-edit-form" disabled={updateSchedule.isPending}>
            {updateSchedule.isPending ? "保存中…" : "保存"}
          </Button>
        </>
      }
    >
      <form id="schedule-edit-form" onSubmit={handleSubmit} className="space-y-4">
        <ScheduleFormFields form={form} set={set} agents={agents} squads={squads} domains={domains} />
        {(assigneeChanged || domainChanged) && (
          <p className="text-xs text-amber-600 bg-amber-50 border border-amber-200 rounded px-3 py-2">
            改动负责人或项目仅影响<strong>后续触发</strong>，已派生的 goal 保持原值不变。
          </p>
        )}
        {updateSchedule.isError && (
          <p className="text-sm text-red-500">{String(updateSchedule.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
