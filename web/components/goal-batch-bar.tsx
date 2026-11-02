"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Dialog, Button } from "@/components/ui";
import { cancelGoal, activateGoal, reopenGoal, deleteGoal, resolveGoalReview } from "@/lib/api";
import { qk } from "@/lib/queries";
import { cn } from "@/lib/utils";
import type { Goal } from "@/lib/types";

// ── Goal 批量操作条（goals 列表多选，DESIGN.md 决策 7-6）──
// 积压审批（review）堆积时逐卡点「批准并自动合入」太碎：卡片 checkbox 多选
// + 底部 Gmail 式浮动批量条。批量是纯前端的 UI 便利层——逐个串行调既有单
// goal 人工端点（POST /goals/{id}/review approve、/activate、/cancel、
// /reopen、DELETE /goals/{id}），服务端语义（per-domain 锁、gate、review
// 回退、级联删）原样生效，daemon 零改动。

export type GoalBatchAction = "approve" | "activate" | "cancel" | "reopen" | "delete";

// 动作 → 适用状态（与卡片行内单操作的资格完全一致）。
const ACTION_STATUSES: Record<GoalBatchAction, (s: string) => boolean> = {
  approve: (s) => s === "review",
  activate: (s) => s === "backlog",
  cancel: (s) => s === "backlog" || s === "active" || s === "review",
  reopen: (s) => s === "done" || s === "failed" || s === "cancelled",
  delete: () => true,
};

const ACTIONS: GoalBatchAction[] = ["approve", "activate", "cancel", "reopen", "delete"];

const ACTION_META: Record<GoalBatchAction, { label: string; btnCls: string }> = {
  approve: { label: "批准合入", btnCls: "bg-indigo-500 hover:bg-indigo-400 text-white shadow-sm shadow-indigo-500/30" },
  activate: { label: "激活", btnCls: "border border-zinc-600 text-zinc-200 hover:border-zinc-400 hover:text-white" },
  cancel: { label: "取消", btnCls: "border border-zinc-600 text-zinc-200 hover:border-amber-400 hover:text-amber-300" },
  reopen: { label: "重开", btnCls: "border border-zinc-600 text-zinc-200 hover:border-indigo-400 hover:text-indigo-300" },
  delete: { label: "删除", btnCls: "bg-red-500/90 hover:bg-red-500 text-white shadow-sm shadow-red-500/30" },
};

export function CheckIcon({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 16 16" fill="none" className={className}>
      <path d="M3 8.5 6.5 12 13 4.5" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

// 卡片上的选择 checkbox。卡片整体是 Link——按钮必须 preventDefault +
// stopPropagation 阻止跳转（与行内「取消/重开/删除」同约定）。
export function GoalSelectCheckbox({
  checked,
  disabled,
  onToggle,
}: {
  checked: boolean;
  disabled?: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={checked}
      aria-label={checked ? "取消选择" : "选择"}
      disabled={disabled}
      onClick={(e) => {
        e.preventDefault();
        e.stopPropagation();
        if (!disabled) onToggle();
      }}
      className={cn(
        "mt-0.5 h-5 w-5 shrink-0 rounded-md border flex items-center justify-center transition-all duration-150",
        checked
          ? "bg-indigo-500 border-indigo-500 text-white shadow-sm shadow-indigo-500/25"
          : "bg-white border-zinc-300 hover:border-indigo-400 hover:bg-indigo-50/50",
        disabled && "opacity-50 cursor-not-allowed"
      )}
    >
      {checked && <CheckIcon className="h-3 w-3" />}
    </button>
  );
}

interface BatchOutcome {
  ok: string[];
  failed: { id: string; title: string; error: string }[];
  skipped: { id: string; title: string; reason: string }[];
}

export function GoalBatchBar({
  goals,
  selected,
  onClear,
  onSelectionChange,
  onBusyChange,
}: {
  /** 实时 goals 数据（react-query），用于计算各动作的合格数 */
  goals: Goal[];
  selected: Set<string>;
  onClear: () => void;
  /** 「仅保留失败项」把失败 id 回填选中集供重试 */
  onSelectionChange: (next: Set<string>) => void;
  /** 正在处理的 goal id（null = 空闲），列表页用来高亮/禁点卡片 */
  onBusyChange?: (goalId: string | null) => void;
}) {
  const qc = useQueryClient();
  const [pending, setPending] = useState<{ action: GoalBatchAction; ids: string[] } | null>(null);
  const [running, setRunning] = useState<{ action: GoalBatchAction; total: number; done: number; current: string } | null>(null);
  const [result, setResult] = useState<{ action: GoalBatchAction; outcome: BatchOutcome } | null>(null);
  // 组件卸载（导航走人）即中断批量——断环防孤儿请求继续轰炸 daemon。
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      onBusyChange?.(null);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const byId = useMemo(() => new Map(goals.map((g) => [g.id, g])), [goals]);

  // 各动作的合格数（对当前实时状态计算，选中快照过期的不计入）。
  const counts = useMemo(() => {
    const c: Record<GoalBatchAction, number> = { approve: 0, activate: 0, cancel: 0, reopen: 0, delete: 0 };
    for (const id of selected) {
      const g = byId.get(id);
      if (!g) continue;
      for (const a of ACTIONS) {
        if (a !== "delete" && ACTION_STATUSES[a](g.status)) c[a]++;
      }
      c.delete++;
    }
    return c;
  }, [selected, byId]);

  const eligibleIds = (action: GoalBatchAction) =>
    [...selected].filter((id) => {
      const g = byId.get(id);
      return g && ACTION_STATUSES[action](g.status);
    });

  const titleList = (ids: string[]) => {
    const names = ids.map((id) => byId.get(id)?.title ?? id);
    return names.slice(0, 5).map((t) => `「${t}」`).join("、") + (names.length > 5 ? ` 等 ${names.length} 个` : "");
  };

  const confirmMsg = (action: GoalBatchAction, ids: string[]) => {
    const list = titleList(ids);
    switch (action) {
      case "approve":
        return `将对 ${list} 执行「批准并自动合入」：平台会逐个 merge + 复验 + push（scratch 项目按汇报验收完成）。这是重量级操作（产生真实合并），请确认。`;
      case "activate":
        return `将激活 ${list}：backlog → active，并入队 owner run。`;
      case "cancel":
        return `将取消 ${list}：非终态 → cancelled（终态，需重开才能恢复）。`;
      case "reopen":
        return `将重开 ${list}：终态 → active，并入队 owner run。`;
      case "delete":
        return `将删除 ${list}，级联删除其全部 run/评论/子目标，不可恢复。`;
    }
  };

  // 串行执行：不并发轰炸 daemon（deliver 本就 per-domain 锁串行）。每个
  // goal 执行前从 query cache 重读最新状态再验资格——选中快照过期的跳过，
  // 不报错；后端仍会做最终校验（409/404 落进失败列表）。
  const runBatch = async (action: GoalBatchAction, ids: string[]) => {
    setPending(null);
    setRunning({ action, total: ids.length, done: 0, current: "" });
    const outcome: BatchOutcome = { ok: [], failed: [], skipped: [] };
    for (const id of ids) {
      if (!alive.current) return;
      const g = (qc.getQueryData<Goal[]>(qk.goals) ?? goals).find((x) => x.id === id);
      if (!g) {
        outcome.skipped.push({ id, title: id, reason: "Goal 已不存在（可能已被删除）" });
        continue;
      }
      if (!ACTION_STATUSES[action](g.status)) {
        outcome.skipped.push({ id, title: g.title, reason: `状态已变为 ${g.status}，不符合该动作条件，已跳过` });
        continue;
      }
      setRunning((r) => (r ? { ...r, current: g.title } : r));
      onBusyChange?.(id);
      try {
        if (action === "approve") await resolveGoalReview(id, { decision: "approve" });
        else if (action === "activate") await activateGoal(id);
        else if (action === "cancel") await cancelGoal(id);
        else if (action === "reopen") await reopenGoal(id);
        else await deleteGoal(id);
        outcome.ok.push(id);
      } catch (e) {
        outcome.failed.push({ id, title: g.title, error: String(e) });
      }
      setRunning((r) => (r ? { ...r, done: r.done + 1 } : r));
    }
    if (!alive.current) return;
    onBusyChange?.(null);
    setRunning(null);
    setResult({ action, outcome });
    qc.invalidateQueries({ queryKey: qk.goals });
  };

  if (selected.size === 0) return null;

  const busy = running !== null;

  return (
    <>
      {/* 浮动批量操作条（Gmail 式） */}
      <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-40 px-4 w-full sm:w-auto sm:px-0">
        <div className="flex items-center gap-3 bg-zinc-900 text-white rounded-2xl shadow-2xl border border-zinc-700 px-4 py-2.5">
          {running ? (
            <>
              <span className="text-sm font-medium whitespace-nowrap">
                {ACTION_META[running.action].label}中… {running.done}/{running.total}
              </span>
              <div className="w-32 sm:w-44 h-1.5 bg-zinc-700 rounded-full overflow-hidden shrink-0">
                <div
                  className="h-full bg-indigo-400 rounded-full transition-all duration-200"
                  style={{ width: `${running.total ? (running.done / running.total) * 100 : 0}%` }}
                />
              </div>
              <span className="text-xs text-zinc-400 max-w-[12rem] truncate hidden sm:block">{running.current}</span>
            </>
          ) : (
            <>
              <span className="text-sm font-medium whitespace-nowrap">已选 {selected.size} 项</span>
              <div className="h-5 w-px bg-zinc-700 shrink-0" />
              <div className="flex items-center gap-1.5 flex-wrap">
                {ACTIONS.filter((a) => counts[a] > 0).map((a) => (
                  <button
                    key={a}
                    type="button"
                    onClick={() => setPending({ action: a, ids: eligibleIds(a) })}
                    className={cn(
                      "px-2.5 py-1 rounded-full text-xs font-medium transition-all duration-150",
                      ACTION_META[a].btnCls
                    )}
                  >
                    {ACTION_META[a].label}
                    {/* 混合选中：按钮带合格数（「批准合入 3」= 5 选 3 合格） */}
                    {counts[a] !== selected.size && <span className="ml-1 opacity-75">{counts[a]}</span>}
                  </button>
                ))}
              </div>
              <button
                type="button"
                onClick={onClear}
                className="text-xs text-zinc-400 hover:text-white transition-colors ml-1 shrink-0"
              >
                清空
              </button>
            </>
          )}
        </div>
      </div>

      {/* 批量确认（批准合入/删除是重量级/不可恢复操作，其余为一致性也确认） */}
      {pending && (
        <Dialog
          title={`批量${ACTION_META[pending.action].label}`}
          onClose={() => setPending(null)}
          footer={
            <>
              <Button variant="outline" onClick={() => setPending(null)}>不执行</Button>
              <Button
                variant={pending.action === "delete" ? "danger" : "primary"}
                onClick={() => runBatch(pending.action, pending.ids)}
              >
                确认{ACTION_META[pending.action].label}
                {pending.ids.length > 0 ? `（${pending.ids.length} 项）` : ""}
              </Button>
            </>
          }
        >
          <p className="text-sm text-zinc-600">{confirmMsg(pending.action, pending.ids)}</p>
        </Dialog>
      )}

      {/* 结果对话框：成功/失败/跳过 分色，失败带后端错误原文 */}
      {result && (
        <Dialog
          title={`批量${ACTION_META[result.action].label}完成`}
          onClose={() => setResult(null)}
          footer={
            <>
              {result.outcome.failed.length > 0 && (
                <Button
                  variant="outline"
                  onClick={() => {
                    onSelectionChange(new Set(result.outcome.failed.map((f) => f.id)));
                    setResult(null);
                  }}
                >
                  仅保留失败项
                </Button>
              )}
              <Button onClick={() => { onClear(); setResult(null); }}>完成</Button>
            </>
          }
        >
          <div className="space-y-2.5">
            <p className="text-sm font-medium text-zinc-900">
              成功 {result.outcome.ok.length} 项
              {result.outcome.failed.length > 0 && <span className="text-red-600"> · 失败 {result.outcome.failed.length} 项</span>}
              {result.outcome.skipped.length > 0 && <span className="text-zinc-400"> · 跳过 {result.outcome.skipped.length} 项</span>}
            </p>
            {(result.outcome.failed.length > 0 || result.outcome.skipped.length > 0) && (
              <div className="max-h-56 overflow-y-auto space-y-1.5 text-xs">
                {result.outcome.failed.map((f) => (
                  <p key={f.id} className="text-red-600">
                    ✕ 「{f.title}」{f.error}
                  </p>
                ))}
                {result.outcome.skipped.map((s) => (
                  <p key={s.id} className="text-zinc-400">
                    → 「{s.title}」{s.reason}
                  </p>
                ))}
              </div>
            )}
          </div>
        </Dialog>
      )}
    </>
  );
}
