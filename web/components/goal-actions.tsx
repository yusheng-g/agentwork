"use client";

import { useState } from "react";
import { useAgents, useSquads, useAssignGoal, useCancelGoal, useWaitGoalChildren, useDeleteGoal } from "@/lib/queries";
import { Button, Dialog, Field, inputCls, ConfirmDialog } from "@/components/ui";
import type { Goal } from "@/lib/types";

export function GoalActions({ goal }: { goal: Goal }) {
  const assign = useAssignGoal();
  const cancel = useCancelGoal();
  const wait = useWaitGoalChildren();
  const deleteGoal = useDeleteGoal();
  const [showAssign, setShowAssign] = useState(false);
  const [showDelete, setShowDelete] = useState(false);

  const isTerminal =
    goal.status === "done" || goal.status === "failed" || goal.status === "cancelled";
  const canWait = goal.status === "active";

  return (
    <div className="flex gap-2 flex-wrap items-center">
      <Button onClick={() => setShowAssign(true)}>分配</Button>

      {!isTerminal && (
        <Button variant="danger" onClick={() => cancel.mutate(goal.id)} disabled={cancel.isPending}>
          {cancel.isPending ? "取消中…" : "取消 Goal"}
        </Button>
      )}

      {canWait && (
        <Button variant="outline" onClick={() => wait.mutate(goal.id)} disabled={wait.isPending}>
          {wait.isPending ? "…" : "等待子 Goal"}
        </Button>
      )}

      <Button variant="ghost" onClick={() => setShowDelete(true)}>
        删除
      </Button>

      {showAssign && <AssignDialog goal={goal} onClose={() => setShowAssign(false)} />}
      {showDelete && (
        <ConfirmDialog
          title="确认删除"
          message={`确定要删除 Goal「${goal.title}」吗？此操作不可撤销。`}
          onConfirm={() => deleteGoal.mutate(goal.id, { onSuccess: () => setShowDelete(false) })}
          onClose={() => setShowDelete(false)}
          loading={deleteGoal.isPending}
        />
      )}

      {(assign.isError || cancel.isError || wait.isError || deleteGoal.isError) && (
        <p className="text-sm text-red-500 w-full">
          {String(assign.error ?? cancel.error ?? wait.error ?? deleteGoal.error)}
        </p>
      )}
    </div>
  );
}

function AssignDialog({ goal, onClose }: { goal: Goal; onClose: () => void }) {
  const assign = useAssignGoal();
  const { data: agents } = useAgents();
  const { data: squads } = useSquads();
  const [assigneeType, setAssigneeType] = useState(goal.assignee_type || "agent");
  const [assigneeId, setAssigneeId] = useState(goal.assignee_id || "");
  const [handoff, setHandoff] = useState(goal.handoff_note || "");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    assign.mutate(
      { id: goal.id, assignee_type: assigneeType, assignee_id: assigneeId, handoff_note: handoff },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="分配 Goal"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="assign-form" disabled={assign.isPending}>
            {assign.isPending ? "分配中…" : "分配"}
          </Button>
        </>
      }
    >
      <form id="assign-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="负责人类型">
          <select value={assigneeType} onChange={(e) => { setAssigneeType(e.target.value); setAssigneeId(""); }} className={inputCls}>
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
        <Field label="Handoff Note" hint="告诉 agent 这次运行的范围/约束">
          <textarea value={handoff} onChange={(e) => setHandoff(e.target.value)} className={inputCls} rows={4} placeholder="告诉 agent 这次运行的范围/约束…" />
        </Field>
        {assign.isError && (
          <p className="text-sm text-red-500">{String(assign.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
