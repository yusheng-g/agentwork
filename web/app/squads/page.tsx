"use client";

import { useState } from "react";
import Link from "next/link";
import { useSquads, useAgents, useCreateSquad, useDeleteSquad, useGoalEvents } from "@/lib/queries";
import { Button, PageHeader, Empty, Dialog, Field, inputCls, ConfirmDialog } from "@/components/ui";
import type { Squad } from "@/lib/types";

export default function SquadsPage() {
  useGoalEvents();
  const { data: squads, isLoading } = useSquads();
  const { data: agents } = useAgents();
  const createSquad = useCreateSquad();
  const deleteSquad = useDeleteSquad();
  const [showForm, setShowForm] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;

  return (
    <div className="p-8">
      <PageHeader
        title="Squad"
        action={<Button onClick={() => setShowForm(true)}>+ 新建</Button>}
      />

      {isLoading ? (
        <div className="text-sm text-zinc-400 py-16 text-center">加载中…</div>
      ) : !squads || squads.length === 0 ? (
        <Empty>暂无 Squad。点「+ 新建」创建一个。</Empty>
      ) : (
        <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-zinc-100 bg-zinc-50/50 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                <th className="px-4 py-3">名称</th>
                <th className="px-4 py-3">Leader</th>
                <th className="px-4 py-3">描述</th>
                <th className="px-4 py-3">创建时间</th>
                <th className="px-4 py-3 w-20"></th>
              </tr>
            </thead>
            <tbody>
              {squads.map((s: Squad) => (
                <tr key={s.id} className="border-b border-zinc-50 last:border-0 hover:bg-zinc-50/60">
                  <td className="px-4 py-3 font-medium text-zinc-900">
                    <Link href={`/squads/${s.id}`} className="hover:text-blue-600 hover:underline">
                      {s.name}
                    </Link>
                  </td>
                  <td className="px-4 py-3 text-zinc-600">{agentName(s.leader_id)}</td>
                  <td className="px-4 py-3 text-zinc-500 max-w-[200px] truncate">{s.description || "-"}</td>
                  <td className="px-4 py-3 text-zinc-400">{new Date(s.created_at).toLocaleString()}</td>
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

      {showForm && <NewSquadForm agents={agents} onClose={() => setShowForm(false)} />}
      {deleteTarget && (
        <ConfirmDialog
          title="确认删除"
          message="确定要删除此 Squad 吗？"
          onConfirm={() => deleteSquad.mutate(deleteTarget, { onSuccess: () => setDeleteTarget(null) })}
          onClose={() => setDeleteTarget(null)}
          loading={deleteSquad.isPending}
        />
      )}
    </div>
  );
}

function NewSquadForm({
  agents,
  onClose,
}: {
  agents?: { id: string; name: string }[];
  onClose: () => void;
}) {
  const createSquad = useCreateSquad();
  const [name, setName] = useState("");
  const [leaderId, setLeaderId] = useState("");
  const [description, setDescription] = useState("");
  const [instructions, setInstructions] = useState("");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createSquad.mutate(
      { name, leader_id: leaderId, description, instructions },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="新建 Squad"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="squad-form" disabled={createSquad.isPending}>
            {createSquad.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="squad-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称" hint="必填">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} required placeholder="Squad 名称…" />
        </Field>
        <Field label="Leader" hint="必填，leader 必须是 agent">
          <select value={leaderId} onChange={(e) => setLeaderId(e.target.value)} className={inputCls} required>
            <option value="">选择…</option>
            {agents?.map((a) => (
              <option key={a.id} value={a.id}>{a.name}</option>
            ))}
          </select>
        </Field>
        <Field label="描述">
          <textarea value={description} onChange={(e) => setDescription(e.target.value)} className={inputCls} rows={2} />
        </Field>
        <Field label="Instructions" hint="leader 运行时会注入这些 instructions">
          <textarea value={instructions} onChange={(e) => setInstructions(e.target.value)} className={inputCls} rows={3} placeholder="Squad 工作说明…" />
        </Field>
        {createSquad.isError && (
          <p className="text-sm text-red-500">{String(createSquad.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
