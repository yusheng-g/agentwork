"use client";

// GoalTimeline: the goal's execution FLOW — who handled it, in what order,
// for how long, and (when it happens) in parallel. Rendered as a popup card
// behind the "执行 flow" button, because the flow is an overview you consult
// on demand, not a persistent surface.
//
// The card is a VERTICAL FLOW of rows:
//   - a bucket row holds run segments whose time windows OVERLAP — they
//     render side by side with a "∥ 并行" mark (parallel processing);
//     a lone run renders as one node box;
//   - action rows (human review entry / approve / reject / …) sit between
//     buckets as small nodes.
// Repeats are LOOPS: the same holder appearing again keeps its color and
// carries a ↺ mark. The current holder pulses.
//
// Data: GET /goals/{id}/timeline (runs + activity + gate decisions merged
// by the backend). Comments stay in the comment feed; run-by-run detail
// stays in the run history — the flow card only carries what is unique to
// it: the transfer order, the parallel fan, and who holds it now.

import { Fragment, useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useGoalTimeline, useGoalRuns, useGoalRunMessages, useAgents, qk } from "@/lib/queries";
import { useWSEvent } from "@/lib/ws";
import { Badge, Button, Dialog } from "@/components/ui";
import type { TimelineItem } from "@/lib/types";
import { groupMessages, StreamCards } from "@/lib/run-messages";

/** 1min ago → "1min"; 3h12m → "3h 12m"; 2d → "2d". */
function dur(start: number, end: number): string {
  const s = Math.max(0, Math.floor((end - start) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}min`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}min`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}

function fmtTime(iso: string): string {
  return new Date(iso).toLocaleString("zh-CN", {
    month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit",
  });
}

// Shared flow state: the merged timeline + the computed active holder.
function useFlow(goalId: string, goalStatus: string) {
  const { data: items } = useGoalTimeline(goalId);
  const { data: agents } = useAgents();
  const [, setTick] = useState(0);

  // Live elapsed clock — the active segment's "已处理 X" keeps moving.
  useEffect(() => {
    const t = setInterval(() => setTick((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, []);

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid.slice(0, 8);
  const nowMs = Date.now();

  const runs = (items ?? []).filter((i) => i.kind === "run");
  const lastRun = runs[runs.length - 1];
  const reviewEntry = [...(items ?? [])].reverse().find((i) =>
    i.kind === "action" && ["entered_review", "requested_review", "parked_review"].includes(i.action ?? "")
  );
  const liveRun = lastRun && (lastRun.run_status === "running" || lastRun.run_status === "queued") ? lastRun : null;
  const waitingReview = goalStatus === "review" && reviewEntry ? reviewEntry : null;

  const active: { kind: "run" | "review"; node: TimelineItem } | null = liveRun
    ? { kind: "run", node: liveRun }
    : waitingReview
      ? { kind: "review", node: waitingReview }
      : null;
  return { items, active, agentName, nowMs };
}

// ── flow card ─────────────────────────────────────────────────────────

const FLOW_COLORS = [
  { border: "border-indigo-400", text: "text-indigo-700", bg: "bg-indigo-50", ring: "ring-indigo-200", dot: "bg-indigo-500" },
  { border: "border-violet-400", text: "text-violet-700", bg: "bg-violet-50", ring: "ring-violet-200", dot: "bg-violet-500" },
  { border: "border-teal-400", text: "text-teal-700", bg: "bg-teal-50", ring: "ring-teal-200", dot: "bg-teal-500" },
  { border: "border-sky-400", text: "text-sky-700", bg: "bg-sky-50", ring: "ring-sky-200", dot: "bg-sky-500" },
  { border: "border-rose-400", text: "text-rose-700", bg: "bg-rose-50", ring: "ring-rose-200", dot: "bg-rose-500" },
  { border: "border-emerald-400", text: "text-emerald-700", bg: "bg-emerald-50", ring: "ring-emerald-200", dot: "bg-emerald-500" },
];
const HUMAN_COLOR = { border: "border-amber-400", text: "text-amber-700", bg: "bg-amber-50", ring: "ring-amber-200", dot: "bg-amber-500" };
type FlowColor = (typeof FLOW_COLORS)[number];

// Actions that ARE processing steps (flow nodes). Comments and system
// chatter are not — they live in the comment feed.
const FLOW_ACTIONS = new Set([
  "created", "handoff", "entered_review",
  "reopened", "cancelled", "mention_cycle_failed",
]);
const ACTION_LABEL: Record<string, string> = {
  created: "📝 创建",
  handoff: "🔀 改派",
  entered_review: "🔔 等待审批",
  reopened: "↩️ 重开",
  cancelled: "🚫 取消",
  mention_cycle_failed: "⚠️ 协作循环判死",
};

type FlowRow =
  | { kind: "bucket"; runs: TimelineItem[]; parallel: boolean; at: string }
  | { kind: "action"; item: TimelineItem };

// Build the vertical flow: run segments bucketed by time-window overlap
// (overlapping windows → ONE row, rendered side by side = parallel), with
// human/system action points as their own rows between buckets.
function buildRows(items: TimelineItem[]): FlowRow[] {
  const rows: FlowRow[] = [];
  let open: TimelineItem[] | null = null;
  let openEnd = 0;

  const close = () => {
    if (open) {
      rows.push({ kind: "bucket", runs: open, parallel: open.length > 1, at: open[0].at });
      open = null;
    }
  };

  for (const it of items) {
    if (it.kind === "run" && it.agent_id) {
      const start = it.started_at ? Date.parse(it.started_at) : Date.parse(it.at);
      const end = it.finished_at ? Date.parse(it.finished_at) : start;
      if (open && start < openEnd) {
        // Windows overlap → the same time row: PARALLEL processing.
        open.push(it);
        openEnd = Math.max(openEnd, end);
      } else {
        close();
        open = [it];
        openEnd = end;
      }
    } else if (it.kind === "decision") {
      close();
      rows.push({ kind: "action", item: it });
    } else if (it.kind === "action" && it.action && FLOW_ACTIONS.has(it.action)) {
      close();
      rows.push({ kind: "action", item: it });
    }
  }
  close();
  return rows;
}

// Stable color per holder, in first-appearance order; humans always amber.
// Pre-scans the whole flow so the same holder keeps its color across rows.
function flowColors(rows: FlowRow[]) {
  const map = new Map<string, FlowColor>();
  let next = 0;
  const seen = new Set<string>();
  for (const r of rows) {
    if (r.kind !== "bucket") continue;
    for (const run of r.runs) {
      const key = "agent:" + (run.agent_id ?? "");
      if (!seen.has(key)) {
        seen.add(key);
        map.set(key, FLOW_COLORS[next++ % FLOW_COLORS.length]);
      }
    }
  }
  return (key: string) => map.get(key) ?? FLOW_COLORS[0];
}

export function GoalTimeline({ goalId, goalStatus }: { goalId: string; goalStatus: string }) {
  const { items, active, agentName, nowMs } = useFlow(goalId, goalStatus);
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);

  useWSEvent("run:enqueued", (p) => {
    if ((p as { goal_id?: string })?.goal_id === goalId)
      qc.invalidateQueries({ queryKey: qk.goalTimeline(goalId) });
  });
  // queued→running: the review window must flip from "等你审批" to
  // "审查中" the moment the reviewer's run is claimed.
  useWSEvent("run:claimed", (p) => {
    if ((p as { goal_id?: string })?.goal_id === goalId)
      qc.invalidateQueries({ queryKey: qk.goalTimeline(goalId) });
  });

  if (!items || items.length === 0) return null;

  return (
    <>
      <Button
        variant="outline"
        className="gap-1.5 border-indigo-200 bg-indigo-50/70 text-indigo-700 hover:border-indigo-300 hover:bg-indigo-100"
        onClick={() => setOpen(true)}
      >
        {active && (
          <span className={`h-1.5 w-1.5 rounded-full animate-pulse ${active.kind === "run" ? "bg-indigo-500" : "bg-amber-500"}`} aria-hidden />
        )}
        执行 flow
      </Button>

      {open && (
        <Dialog title="执行 flow" onClose={() => setOpen(false)} footer={null}>
          <div className="px-5 py-4 overflow-y-auto max-h-[70vh]">
            <FlowCard items={items} active={active} agentName={agentName} nowMs={nowMs} goalStatus={goalStatus} goalId={goalId} />
          </div>
        </Dialog>
      )}
    </>
  );
}

// ─────────────────────────────────────────────────────────────
// 横向流水线（电子流风格）：小而精致的圆节点 + 干净的 → 连线。
// 串行 = 节点 → 节点；并行 = 同一行并排（无需分叉线——横向布局的
// 天然优势）；环 = 名字重复出现 + ↺ 标记；当前持有者圆环脉冲。
// 颜色收敛：节点统一紫系渐变，状态用小圆点（绿/红/紫脉冲/灰）。
// ─────────────────────────────────────────────────────────────

// 横向流水线：buildRows 的桶在一行内并排（无箭头 = 并行），
// 桶之间与 action 节点用 → 连接；宽度不足自动换行。
// ─────────────────────────────────────────────────────────────
// 横向流水线（克制版）：处理者是圆节点（首字母），动作为箭头上的
// 小标签（钉钉审批流式），并行 = 同一行并排。无 emoji、无多余符号。
// ─────────────────────────────────────────────────────────────

// 段：一组时间重叠的处理者（并行时 runs>1）；action 是到达本段前
// 发生的动作（创建/等待审批/批准/驳回/…），渲染为箭头上的标签。
interface FlowSegment {
  runs: TimelineItem[];
  parallel: boolean;
  action?: TimelineItem; // 本段之前的动作（创建/等待审批/批准/驳回）
}

function buildSegments(items: TimelineItem[]): FlowSegment[] {
  const segs: FlowSegment[] = [];
  let open: TimelineItem[] | null = null;
  let openEnd = 0;
  let pending: TimelineItem | null = null; // 等待挂到下一段的动作

  const close = () => {
    if (open) {
      segs.push({ runs: open, parallel: open.length > 1, action: pending ?? undefined });
      open = null;
      pending = null;
    }
  };

  for (const it of items) {
    if (it.kind === "run" && it.agent_id) {
      const start = it.started_at ? Date.parse(it.started_at) : Date.parse(it.at);
      const end = it.finished_at ? Date.parse(it.finished_at) : start;
      if (open && start < openEnd) {
        open.push(it);
        openEnd = Math.max(openEnd, end);
      } else {
        close();
        open = [it];
        openEnd = end;
      }
    } else if (it.kind === "decision") {
      pending = it; // 批准/驳回 → 下一段的箭头标签
    } else if (it.kind === "action" && it.action && FLOW_ACTIONS.has(it.action)) {
      pending = it; // 等待审批/创建/… → 下一段的箭头标签
    }
  }
  close();
  if (pending) segs.push({ runs: [], parallel: false, action: pending }); // 尾部动作
  return segs;
}

function FlowCard({ items, active, agentName, nowMs, goalStatus, goalId }: {
  items: TimelineItem[];
  active: { kind: "run" | "review"; node: TimelineItem } | null;
  agentName: (id: string) => string;
  nowMs: number;
  goalStatus: string;
  goalId: string;
}) {
  const segs = buildSegments(items);
  const seen = new Set<string>();

  const holderLabel = active
    ? active.kind === "run"
      ? `${agentName(active.node.agent_id ?? "")} 正在执行 · 已运行 ${active.node.started_at ? dur(Date.parse(active.node.started_at), nowMs) : ""}`
      : `等你审批 · 已等待 ${dur(Date.parse(active.node.at), nowMs)}`
    : null;

  return (
    <div>
      {holderLabel && (
        <div className="mb-4 px-3 py-2 rounded-lg bg-zinc-50 border border-zinc-200 text-xs font-medium text-zinc-700 flex items-center gap-2">
          <span className={`h-1.5 w-1.5 rounded-full animate-pulse ${active?.kind === "run" ? "bg-indigo-500" : "bg-amber-500"}`} />
          {holderLabel}
        </div>
      )}

      {/*
        纵向电子流：一行 = 一个步骤。
        - 串行步骤 = 单节点行；并行步骤 = 一行内并排（并排即并行，无歧义）
        - 动作（等待审批/批准/驳回） = 独立行（居中 pill）
        - 行间 ↓ 推进；超长滚动（max-h + overflow-y-auto）
      */}
      <div className="max-h-[60vh] overflow-y-auto pr-1 -mr-1">
        {segs.map((seg, i) => {
          // 脉冲必须钉到当前那条 run 上（agent_id + at 双重身份）——
          // 只比 agent_id 会让同一 agent 的历史节点也一起脉冲。
          const isActive = seg.runs.some((r) => active?.kind === "run" && active.node.agent_id === r.agent_id && active.node.at === r.at);
          return (
            <div key={i} className="flex flex-col items-center">
              {i > 0 && <FlowArrow />}
              {seg.runs.length > 0 ? (
                <div className="flex items-start gap-2">
                  {seg.runs.map((run) => {
                    const key = "agent:" + (run.agent_id ?? "");
                    const loop = seen.has(key);
                    seen.add(key);
                    return (
                      <FlowNode key={run.agent_id + run.at} run={run} agentName={agentName} nowMs={nowMs} active={isActive && active?.kind === "run" && active.node.agent_id === run.agent_id && active.node.at === run.at} loop={loop} goalId={goalId} />
                    );
                  })}
                </div>
              ) : seg.action ? (
                <ActionTag it={seg.action} nowMs={nowMs} goalStatus={goalStatus} />
              ) : null}
            </div>
          );
        })}
      </div>
    </div>
  );
}

// 纵向行间箭头：竖线 + 实心三角。
function FlowArrow() {
  return (
    <div className="flex flex-col items-center py-0.5" aria-hidden>
      <div className="w-px h-2.5 bg-zinc-300" />
      <div className="w-0 h-0 border-l-[4px] border-r-[4px] border-t-[5px] border-l-transparent border-r-transparent border-t-zinc-400" />
    </div>
  );
}

// 动作标签：箭头上的小字（等待审批/批准/驳回 + 时长）。
function ActionTag({ it, nowMs, goalStatus }: { it: TimelineItem; nowMs: number; goalStatus: string }) {
  const decision = it.kind === "decision";
  const ok = it.decision === "approve";
  const waiting = goalStatus === "review" && !decision && it.action?.startsWith("entered_review");
  let label: string;
  let cls = "text-zinc-400";
  if (decision) {
    label = ok ? "批准" : it.decision === "reject" ? "驳回" : "改判";
    cls = ok ? "text-emerald-600" : "text-red-600";
  } else if (it.action === "entered_review") {
    label = "等待审批"; cls = "text-amber-600";
  } else if (it.action === "created") {
    label = "创建"; cls = "text-zinc-400";
  } else {
    label = ((ACTION_LABEL[it.action ?? ""] ?? "").replace(/^..\s*/, "")) || (it.action ?? "");
  }
  const pill = decision ? (ok ? "bg-emerald-50 text-emerald-700 border-emerald-200" : "bg-red-50 text-red-700 border-red-200")
    : it.action === "entered_review" ? "bg-amber-50 text-amber-700 border-amber-200"
    : "bg-zinc-50 text-zinc-500 border-zinc-200";
  const hasMore = it.reason || it.gate_rule;
  const [open, setOpen] = useState(false);
  return (
    <div className="flex flex-col items-center">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className={`text-[10px] font-medium whitespace-nowrap border rounded-full px-2 py-0.5 cursor-pointer ${pill} ${hasMore ? "" : "cursor-default"}`}
      >
        {label}
        {decision && !!it.review_duration_s && it.review_duration_s > 0 && ` · ${dur(0, it.review_duration_s * 1000)}`}
        {waiting && ` · ${dur(Date.parse(it.at), nowMs)}`}
      </button>
      {open && (it.reason || it.gate_rule) && (
        <div className="w-[240px] rounded-lg border border-zinc-200 bg-white shadow-md p-2.5 mt-1 mb-1 text-left space-y-1">
          {it.gate_rule && <div className="text-[10px] text-zinc-500">规则：<code className="text-zinc-400">{it.gate_rule}</code></div>}
          {it.reason && (
            <div className="text-[10px] text-zinc-600">
              <span className="font-medium text-zinc-500">{decision ? "理由" : "备注"}：</span>
              <span className="whitespace-pre-wrap">{it.reason}</span>
            </div>
          )}
          <div className="text-[9.5px] text-zinc-400 tabular-nums">{fmtTime(it.at)}</div>
        </div>
      )}
    </div>
  );
}

// 处理者节点：圆（首字母）+ 名字 + 状态文字（失败/完成）+ 轮次 + 时间。
// 点击展开该 run 的详情（汇报/证据/触发来源/交互流）。
function FlowNode({ run, agentName, nowMs, active, loop, goalId }: {
  run: TimelineItem;
  agentName: (id: string) => string;
  nowMs: number;
  active: boolean;
  loop: boolean;
  goalId: string;
}) {
  const [open, setOpen] = useState(false);
  const start = run.started_at ? Date.parse(run.started_at) : Date.parse(run.at);
  const end = run.finished_at ? Date.parse(run.finished_at) : nowMs;
  const status = run.run_status ?? "";
  const name = agentName(run.agent_id ?? "");

  const st: { text: string; cls: string; ring: string } =
    status === "completed" ? { text: "完成", cls: "text-emerald-600", ring: "border-emerald-300 bg-emerald-50" }
    : status === "failed" ? { text: "失败", cls: "text-red-600", ring: "border-red-300 bg-red-50" }
    : status === "running" ? { text: "执行中", cls: "text-indigo-600", ring: "border-indigo-400 bg-indigo-50" }
    : status === "queued" ? { text: "排队", cls: "text-zinc-400", ring: "border-zinc-300 bg-zinc-50" }
    : { text: "", cls: "text-zinc-400", ring: "border-indigo-200 bg-indigo-50" };

  const attempt = run.attempt && run.attempt > 1 ? ` ×${run.attempt}` : "";

  return (
    <div className="flex flex-col items-center">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex flex-col items-center gap-1 w-[76px] cursor-pointer group"
        title={run.run_id ? `run ${run.run_id.slice(0, 8)} · 点击看详情` : undefined}
      >
        <div className="relative">
          <div className={`h-9 w-9 rounded-full border flex items-center justify-center text-[13px] font-bold text-zinc-700 shadow-sm ${st.ring} ${active ? "ring-4 ring-indigo-100 animate-pulse" : ""} group-hover:ring-2 group-hover:ring-indigo-200 transition-shadow`}>
            {(name[0] ?? "?").toUpperCase()}
          </div>
        </div>
        <div className="text-[10px] font-medium text-zinc-700 truncate w-full text-center leading-tight">
          {name}
          {loop && <span className="text-violet-500 font-bold" title="此前已出现过（环）">↺</span>}
        </div>
        {(run.role === "subgoal" || run.role === "consult" || run.role === "review" || run.role === "verify") && (
          <span className="text-[9px] px-1 py-px rounded bg-violet-50 text-violet-600 border border-violet-100 leading-none">
            {run.role === "subgoal" ? "子任务" : run.role === "verify" ? "验证" : run.role === "review" ? "审查" : "咨询"}
          </span>
        )}
        <div className={`text-[9.5px] font-medium leading-tight ${st.cls}`}>
          {st.text}{attempt}
        </div>
        <div className="text-[9px] text-zinc-400 tabular-nums leading-tight">
          {fmtTime(run.started_at || run.at)}
          {run.finished_at ? `·${dur(start, end)}` : status === "running" ? `·${dur(start, nowMs)}` : ""}
        </div>
      </button>
      {open && run.run_id && <RunDetail goalId={goalId} runId={run.run_id} agentName={agentName} />}
    </div>
  );
}

// Run 详情：点击节点展开 —— 汇报（Markdown）/ 证据包 / 触发来源 / 交互流。
function RunDetail({ goalId, runId, agentName }: {
  goalId: string;
  runId: string;
  agentName: (id: string) => string;
}) {
  const { data: runs } = useGoalRuns(goalId);
  const full = runs?.find((r) => r.id === runId);
  const [showStream, setShowStream] = useState(false);
  const { data: messages } = useGoalRunMessages(goalId, runId, showStream);

  const evidence = (() => {
    try { return full?.evidence ? (JSON.parse(full.evidence) as Record<string, unknown>) : null; }
    catch { return null; }
  })();

  return (
    <div className="w-[260px] rounded-xl border border-zinc-200 bg-white shadow-md p-3 text-left space-y-2 mt-1 mb-2">
      <div className="flex items-center justify-between">
        <span className="text-[11px] font-semibold text-zinc-800">{agentName(full?.agent_id ?? "")}</span>
        <code className="text-[9px] text-zinc-400">{runId.slice(0, 8)}</code>
      </div>

      {evidence && !!(evidence.diff_stat || evidence.verify) && (
        <details>
          <summary className="text-[10px] font-medium text-zinc-500 cursor-pointer">证据包</summary>
          <pre className="mt-1 text-[9.5px] text-zinc-600 whitespace-pre-wrap bg-zinc-50 rounded p-1.5 max-h-40 overflow-y-auto">
            {String(evidence.diff_stat ?? "")}
            {"\n"}
            {String(evidence.verify ?? "")}
          </pre>
        </details>
      )}

      {full?.trigger_comment_id && (
        <div className="text-[10px] text-zinc-500">
          触发：<code className="text-zinc-400">{full.trigger_comment_id.slice(0, 8)}</code>（mention 评论）
        </div>
      )}

      <button
        type="button"
        onClick={() => setShowStream((v) => !v)}
        className="text-[10px] font-medium text-indigo-600 hover:underline"
      >
        {showStream ? "收起交互流" : "查看交互流"}
      </button>
      {showStream && (
        <div className="max-h-48 overflow-y-auto border-t border-zinc-100 pt-1.5">
          {(messages ?? []).length === 0 ? (
            <div className="text-[10px] text-zinc-400">尚无交互记录…</div>
          ) : (
            <StreamCards items={groupMessages(messages ?? [])} compact />
          )}
        </div>
      )}
    </div>
  );
}

// ── status bar（标题行常驻）─────────────────────────────────────────

// GoalStatusBar: the goal's current holder at a glance — "agent X 正在执行 ·
// 已运行 12min" / "等你审批 · 已等待 3h" / terminal state. Rendered right
// under the title so who holds the goal now is the first thing you see.
export function GoalStatusBar({ goalId, goalStatus }: { goalId: string; goalStatus: string }) {
  const { items, active, agentName, nowMs } = useFlow(goalId, goalStatus);
  const qc = useQueryClient();
  useWSEvent("run:enqueued", (p) => {
    if ((p as { goal_id?: string })?.goal_id === goalId)
      qc.invalidateQueries({ queryKey: qk.goalTimeline(goalId) });
  });
  useWSEvent("run:claimed", (p) => {
    if ((p as { goal_id?: string })?.goal_id === goalId)
      qc.invalidateQueries({ queryKey: qk.goalTimeline(goalId) });
  });

  if (active) return <ActiveBadge active={active} agentName={agentName} nowMs={nowMs} goalStatus={goalStatus} />;
  const meta: Record<string, { icon: string; text: string; cls: string }> = {
    done: { icon: "🏁", text: "已完成", cls: "text-emerald-700" },
    failed: { icon: "❌", text: "失败", cls: "text-red-700" },
    cancelled: { icon: "🚫", text: "已取消", cls: "text-zinc-500" },
    active: { icon: "⏸️", text: "等待中", cls: "text-zinc-500" },
    backlog: { icon: "📥", text: "未开始", cls: "text-zinc-400" },
    review: { icon: "🔔", text: "等待审批", cls: "text-purple-700" },
  };
  const m = meta[goalStatus] ?? { icon: "•", text: goalStatus, cls: "text-zinc-500" };
  return (
    <span className={`inline-flex items-center gap-1.5 text-xs font-medium ${m.cls}`}>
      <span>{m.icon}</span>
      {m.text}
    </span>
  );
}

function ActiveBadge({ active, agentName, nowMs, goalStatus }: {
  active: { kind: "run" | "review"; node: TimelineItem };
  agentName: (id: string) => string;
  nowMs: number;
  goalStatus: string;
}) {
  if (active.kind === "review") {
    const el = nowMs - new Date(active.node.at).getTime();
    return (
      <span className="inline-flex items-center gap-1.5 text-xs font-medium text-amber-700">
        <span className="h-2 w-2 rounded-full bg-amber-500 animate-pulse" />
        等你审批 · 已等待 {dur(nowMs - el, nowMs)}
      </span>
    );
  }
  const r = active.node;
  if (r.run_status === "queued") {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs font-medium text-zinc-500">
        <span className="h-2 w-2 rounded-full bg-zinc-400" />
        排队中
      </span>
    );
  }
  const el = r.started_at ? nowMs - new Date(r.started_at).getTime() : 0;
  // In the review window a running run is the reviewer collecting evidence —
  // label it "审查中", not the generic "正在执行".
  const verb = goalStatus === "review" ? "审查中" : "正在执行";
  return (
    <span className="inline-flex items-center gap-1.5 text-xs font-medium text-indigo-700">
      <span className="h-2 w-2 rounded-full bg-indigo-500 animate-pulse" />
      {agentName(r.agent_id ?? "")} {verb} · 已进行 {dur(nowMs - el, nowMs)}
    </span>
  );
}
