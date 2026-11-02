"use client";

import { useState, Suspense } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useGoals, useAgents, useSquads, useDomains, useCreateGoal, useCancelGoal, useReopenGoal, useDeleteGoal, useGoalEvents } from "@/lib/queries";
import { Badge, AttentionChip, ReviewPhaseBadge, Button, PageHeader, Empty, Dialog, Field, inputCls } from "@/components/ui";
import { GoalStats } from "@/components/goal-stats";
import { GoalBatchBar, GoalSelectCheckbox, CheckIcon } from "@/components/goal-batch-bar";
import { cn } from "@/lib/utils";
import type { Goal, GoalStatus } from "@/lib/types";

const STATUS_TABS: { label: string; value: GoalStatus | "all" }[] = [
  { label: "全部", value: "all" },
  { label: "待审批", value: "review" },
  { label: "backlog", value: "backlog" },
  { label: "active", value: "active" },
  { label: "done", value: "done" },
  { label: "failed", value: "failed" },
  { label: "cancelled", value: "cancelled" },
];

// 卡片顶部状态色条（与 Badge 同色系，一眼可扫）
const STATUS_BAR: Record<string, string> = {
  backlog: "from-zinc-300 to-zinc-400",
  active: "from-blue-500 to-indigo-500",
  review: "from-purple-500 to-fuchsia-500",
  done: "from-emerald-500 to-teal-500",
  failed: "from-red-500 to-rose-500",
  cancelled: "from-zinc-300 to-zinc-400",
};

// 负责人头像首字母
const initials = (name: string) => (name ? name.slice(0, 1).toUpperCase() : "?");

export default function GoalsPage() {
  // useSearchParams 要求 Suspense 边界，否则静态渲染时 CSR bailout 会让构建失败。
  return (
    <Suspense fallback={<div className="p-8 text-sm text-zinc-400">加载中…</div>}>
      <GoalsPageInner />
    </Suspense>
  );
}

function GoalsPageInner() {
  useGoalEvents();
  const { data: goals, isLoading } = useGoals();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const { data: domains } = useDomains();
  const createGoal = useCreateGoal();
  const cancelGoal = useCancelGoal();
  const reopenGoal = useReopenGoal();
  const deleteGoalMut = useDeleteGoal();
  // 筛选状态放 URL（?status=review）：点卡片进详情再「返回列表」不丢筛选，
  // 刷新/分享链接也能还原。无效值回退「全部」。
  const router = useRouter();
  const searchParams = useSearchParams();
  const statusParam = searchParams.get("status");
  const filter = (
    STATUS_TABS.some((t) => t.value === statusParam) ? statusParam : "all"
  ) as GoalStatus | "all";
  const setFilter = (v: GoalStatus | "all") =>
    router.replace(v === "all" ? "/goals" : `/goals?status=${v}`, { scroll: false });
  const [showForm, setShowForm] = useState(false);

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;
  const squadName = (sid: string) => squads?.find((s) => s.id === sid)?.name ?? sid;

  const filtered = goals?.filter((g) => filter === "all" || g.status === filter) ?? [];

  // 多选批量操作（决策 7-6）：Set<goal id>，跨 tab 切换持久（Gmail 式），
  // 底部浮动批量条对选中项批量执行。batchBusy = 批量执行中（格子禁点）。
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [batchBusyId, setBatchBusyId] = useState<string | null>(null);
  const batchBusy = batchBusyId !== null;
  const toggleSelect = (id: string) => {
    if (batchBusy) return;
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };
  const allFilteredSelected = filtered.length > 0 && filtered.every((g) => selected.has(g.id));
  const selectAllFiltered = () => {
    if (batchBusy) return;
    setSelected((prev) => {
      const next = new Set(prev);
      if (allFilteredSelected) {
        for (const g of filtered) next.delete(g.id);
      } else {
        for (const g of filtered) next.add(g.id);
      }
      return next;
    });
  };

  return (
    <div className="p-8 page-enter">
      <PageHeader
        title="Goal"
        action={
          <Button onClick={() => setShowForm(true)}>+ 新建</Button>
        }
      />

      {/* Stats cards */}
      <GoalStats goals={goals} loading={isLoading} />

      {/* Status filter tabs + 全选（作用于当前筛选，配合多选批量条） */}
      <div className="flex items-center justify-between gap-3 mb-4 flex-wrap">
        <div className="flex gap-1 flex-wrap">
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
        {filtered.length > 0 && (
          <button
            type="button"
            disabled={batchBusy}
            onClick={selectAllFiltered}
            className="flex items-center gap-2 px-2.5 py-1.5 rounded-md text-xs font-medium text-zinc-500 hover:text-indigo-700 hover:bg-indigo-50/60 transition-colors disabled:opacity-50 disabled:hover:text-zinc-500 disabled:hover:bg-transparent"
          >
            <span
              className={cn(
                "h-4 w-4 rounded border flex items-center justify-center transition-colors",
                allFilteredSelected ? "bg-indigo-500 border-indigo-500 text-white" : "bg-white border-zinc-300"
              )}
            >
              {allFilteredSelected && <CheckIcon className="h-2.5 w-2.5" />}
            </span>
            {allFilteredSelected ? "取消全选" : "全选"}
          </button>
        )}
      </div>

      {/* Goals — 卡片：状态色条 + 谁在干 + 元信息（不是表格的伪卡片） */}
      {isLoading ? (
        <div className="text-sm text-zinc-400 py-16 text-center">加载中…</div>
      ) : filtered.length === 0 ? (
        <Empty>
          {filter === "all" ? "暂无 Goal，点击「+ 新建」创建第一个。" : "没有符合条件的 Goal。"}
        </Empty>
      ) : (
        <div className={cn("grid gap-3 md:grid-cols-2", batchBusy && "pointer-events-none")}>
          {filtered.map((g: Goal) => (
            <Link
              key={g.id}
              href={`/goals/${g.id}${filter === "all" ? "" : `?status=${filter}`}`}
              className={cn(
                "group relative flex bg-white rounded-2xl border border-zinc-200/80 shadow-sm overflow-hidden hover:shadow-lg hover:shadow-indigo-500/5 hover:-translate-y-0.5 hover:border-indigo-200 transition-all duration-200",
                selected.has(g.id) && "border-indigo-300 bg-indigo-50/30",
                batchBusyId === g.id && "ring-2 ring-indigo-400 border-indigo-300",
                batchBusy && batchBusyId !== g.id && "opacity-60"
              )}
            >
              {/* 状态在左侧：竖向渐变条（一眼可扫） */}
              <div className={`w-1.5 shrink-0 bg-gradient-to-b ${STATUS_BAR[g.status] ?? "from-zinc-300 to-zinc-400"}`} />
              <div className="p-4 flex-1 min-w-0">
                <div className="flex items-start justify-between gap-3">
                  <div className="flex items-start gap-2.5 min-w-0">
                    <GoalSelectCheckbox
                      checked={selected.has(g.id)}
                      disabled={batchBusy}
                      onToggle={() => toggleSelect(g.id)}
                    />
                    <h3 className="font-medium text-zinc-900 group-hover:text-indigo-700 transition-colors min-w-0">
                      {g.title}
                    </h3>
                  </div>
                  <div className="flex items-center gap-1.5 shrink-0">
                    <AttentionChip attention={g.attention} />
                    {g.status === "review" ? (
                      <ReviewPhaseBadge phase={g.review_phase} className="shrink-0" />
                    ) : (
                      <Badge status={g.status} className="shrink-0" />
                    )}
                  </div>
                </div>
                {g.status === "review" && g.review_request && (
                  <p className="mt-1.5 text-xs text-purple-700 line-clamp-1">{g.review_request}</p>
                )}
                {/* 元信息：谁在干 / 负责人 · 时间 */}
                <div className="mt-3 flex items-center gap-x-4 gap-y-1 flex-wrap text-xs text-zinc-400">
                  {g.current_agent_id ? (
                    <span className="flex items-center gap-1.5 font-medium text-emerald-600">
                      <span className="h-1.5 w-1.5 rounded-full bg-emerald-500 animate-pulse" />
                      {agentName(g.current_agent_id)} 正在执行
                    </span>
                  ) : (
                    <span className="flex items-center gap-1.5">
                      <span className="inline-block h-4 w-4 rounded-full bg-indigo-100 text-indigo-600 text-[10px] leading-4 text-center font-medium">
                        {initials(g.assignee_type === "squad" ? (squadName(g.assignee_id) || "?") : (agentName(g.assignee_id) || "?"))}
                      </span>
                      {g.assignee_type === "squad"
                        ? (squadName(g.assignee_id) || g.assignee_id)
                        : g.assignee_id
                          ? (agentName(g.assignee_id) || g.assignee_id)
                          : "未分配"}
                    </span>
                  )}
                  <span>
                    {g.created_at ? new Date(g.created_at).toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : ""}
                  </span>
                  {g.status === "done" && <span className="text-emerald-500 font-medium">✓ 已完成</span>}
                  {g.status === "failed" && <span className="text-red-500 font-medium">✕ 失败</span>}
                </div>
                {/* 行内操作：取消 / 重开 / 删除。卡片整体是 Link，
                    按钮必须 preventDefault + stopPropagation 阻止跳转。
                    cancel：非终态（backlog/active/review）；reopen：终态
                    （done/failed/cancelled，人类接管路径）；delete：任意状态
                    （级联删 run/评论/子目标，confirm 后执行）。 */}
                <div className="mt-2 flex items-center gap-3 justify-end">
                  {(g.status === "active" || g.status === "backlog" || g.status === "review") && (
                    <button
                      className="text-xs text-zinc-400 hover:text-amber-600 transition-colors disabled:opacity-50"
                      disabled={cancelGoal.isPending}
                      onClick={(e) => { e.preventDefault(); e.stopPropagation(); cancelGoal.mutate(g.id); }}
                    >
                      取消
                    </button>
                  )}
                  {(g.status === "done" || g.status === "failed" || g.status === "cancelled") && (
                    <button
                      className="text-xs text-zinc-400 hover:text-indigo-600 transition-colors disabled:opacity-50"
                      disabled={reopenGoal.isPending}
                      onClick={(e) => { e.preventDefault(); e.stopPropagation(); reopenGoal.mutate(g.id); }}
                    >
                      重开
                    </button>
                  )}
                  <button
                    className="text-xs text-zinc-400 hover:text-red-500 transition-colors disabled:opacity-50"
                    disabled={deleteGoalMut.isPending}
                    onClick={(e) => {
                      e.preventDefault();
                      e.stopPropagation();
                      if (confirm(`删除 Goal「${g.title}」？级联删除其全部 run/评论/子目标，不可恢复。`)) deleteGoalMut.mutate(g.id);
                    }}
                  >
                    删除
                  </button>
                </div>
              </div>
            </Link>
          ))}
        </div>
      )}

      {/* Create Goal dialog */}
      {showForm && <NewGoalForm agents={agents} squads={squads} domains={domains} onClose={() => setShowForm(false)} />}

      {createGoal.isError && (
        <p className="text-sm text-red-500 mt-2">{String(createGoal.error)}</p>
      )}
      {(cancelGoal.isError || reopenGoal.isError || deleteGoalMut.isError) && (
        <p className="text-sm text-red-500 mt-2">
          {String(cancelGoal.error ?? reopenGoal.error ?? deleteGoalMut.error)}
        </p>
      )}

      {/* 多选批量操作条（决策 7-6）：选中项 > 0 时出现，浮动底部 */}
      <GoalBatchBar
        goals={goals ?? []}
        selected={selected}
        onClear={() => setSelected(new Set())}
        onSelectionChange={setSelected}
        onBusyChange={setBatchBusyId}
      />
    </div>
  );
}

function NewGoalForm({
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
  const createGoal = useCreateGoal();
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [domainId, setDomainId] = useState("");
  const [domainErr, setDomainErr] = useState("");
  const [assigneeType, setAssigneeType] = useState("");
  const [assigneeId, setAssigneeId] = useState("");
  const [assigneeErr, setAssigneeErr] = useState("");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const body: Record<string, string> = { title };
    if (description) body.description = description;
    if (assigneeType && assigneeId) {
      body.assignee_type = assigneeType;
      body.assignee_id = assigneeId;
      body.status = "active";
    }
    // A chosen assignee TYPE without a chosen agent/squad would silently
    // drop the assignee and the backend rejects it as "agent goal without
    // assignee_id" — fail loudly here instead.
    if (assigneeType && !assigneeId) {
      setAssigneeErr(assigneeType === "agent" ? "请选择一个 Agent——或先到「Agent」页创建 runtime 和 agent" : "请选择一个 Squad——或先到「Squad」页创建小队");
      return;
    }
    // v2: agent/squad-executed goals must belong to a domain (DESIGN.md §2).
    if (assigneeType === "agent" || assigneeType === "squad") {
      if (!domainId) {
        setDomainErr("请选择所属项目——agent 执行的 Goal 必须挂在一个项目上（项目提供 worktree 与验收策略）");
        return;
      }
      body.domain_id = domainId;
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
        <Field label="所属项目" hint="agent/squad 执行的 Goal 必填">
          <select value={domainId} onChange={(e) => setDomainId(e.target.value)} className={inputCls}>
            <option value="">选择…</option>
            {domains?.map((d) => (
              <option key={d.id} value={d.id}>{d.name}</option>
            ))}
          </select>
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
        {(createGoal.isError || domainErr || assigneeErr) && (
          <p className="text-sm text-red-500">{domainErr || assigneeErr || String(createGoal.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
