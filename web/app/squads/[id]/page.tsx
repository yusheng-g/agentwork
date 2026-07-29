"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { useSquads, useAgents, useSquadMembers, useAddSquadMember, useGoalEvents } from "@/lib/queries";
import { Button, PageHeader, Empty, Dialog, Field, inputCls } from "@/components/ui";
import type { SquadMember } from "@/lib/types";

export default function SquadDetailPage() {
  useGoalEvents();
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data: squads } = useSquads();
  const { data: agents } = useAgents();
  const { data: members, isLoading } = useSquadMembers(id);
  const addMember = useAddSquadMember();
  const [showAdd, setShowAdd] = useState(false);

  const squad = squads?.find((s) => s.id === id);
  const agentName = (aid: string) => agents?.find((a) => a.id === aid)?.name ?? aid;

  if (!squad) {
    if (squads === undefined) return <div className="p-8 text-sm text-zinc-400">加载中…</div>;
    return <div className="p-8 text-sm text-zinc-400">找不到 Squad。</div>;
  }

  return (
    <div className="p-8 max-w-4xl space-y-5">
      <div>
        <Link href="/squads" className="text-sm text-zinc-400 hover:text-zinc-700 hover:underline">
          ← 返回列表
        </Link>
        <h1 className="text-lg font-semibold text-zinc-900 mt-3">{squad.name}</h1>
      </div>

      {/* Squad info */}
      <div className="bg-white rounded-xl border border-zinc-200 p-5 space-y-3">
        <div className="grid grid-cols-2 gap-3 text-sm">
          <div>
            <span className="text-zinc-400">Leader：</span>
            <span className="text-zinc-700">{agentName(squad.leader_id)}</span>
          </div>
          <div>
            <span className="text-zinc-400">创建时间：</span>
            <span className="text-zinc-700">{new Date(squad.created_at).toLocaleString()}</span>
          </div>
        </div>
        {squad.description && (
          <div>
            <span className="text-xs text-zinc-400 block mb-1">描述</span>
            <p className="text-sm text-zinc-600">{squad.description}</p>
          </div>
        )}
        {squad.instructions && (
          <div>
            <span className="text-xs text-zinc-400 block mb-1">Instructions</span>
            <p className="text-sm text-zinc-600 whitespace-pre-wrap">{squad.instructions}</p>
          </div>
        )}
      </div>

      {/* Members */}
      <div className="bg-white rounded-xl border border-zinc-200 overflow-hidden">
        <div className="px-4 py-2.5 border-b border-zinc-100 flex items-center justify-between">
          <span className="text-xs font-medium text-zinc-500 uppercase tracking-wide">
            成员{members && members.length > 0 && `（${members.length}）`}
          </span>
          <Button onClick={() => setShowAdd(true)}>+ 添加</Button>
        </div>
        <div className="p-4">
          {isLoading ? (
            <div className="text-sm text-zinc-400 text-center py-8">加载中…</div>
          ) : !members || members.length === 0 ? (
            <Empty>暂无成员。</Empty>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-zinc-100 text-left text-xs font-medium text-zinc-500 uppercase tracking-wide">
                  <th className="px-3 py-2">类型</th>
                  <th className="px-3 py-2">名称</th>
                  <th className="px-3 py-2">角色</th>
                  <th className="px-3 py-2">添加时间</th>
                </tr>
              </thead>
              <tbody>
                {members.map((m: SquadMember) => (
                  <tr key={m.id} className="border-b border-zinc-50">
                    <td className="px-3 py-2">
                      <span className={`px-2 py-0.5 text-xs rounded ${m.member_type === "agent" ? "bg-blue-50 text-blue-700" : "bg-zinc-100 text-zinc-600"}`}>
                        {m.member_type}
                      </span>
                    </td>
                    <td className="px-3 py-2 text-zinc-700">
                      {m.member_type === "agent" ? (agentName(m.member_id) || m.member_id) : m.member_id}
                    </td>
                    <td className="px-3 py-2 text-zinc-500">{m.role || "-"}</td>
                    <td className="px-3 py-2 text-zinc-400 text-xs">{new Date(m.created_at).toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {/* Add member dialog */}
      {showAdd && (
        <AddMemberDialog
          squadId={id}
          agents={agents}
          onClose={() => setShowAdd(false)}
        />
      )}
    </div>
  );
}

function AddMemberDialog({
  squadId,
  agents,
  onClose,
}: {
  squadId: string;
  agents?: { id: string; name: string }[];
  onClose: () => void;
}) {
  const addMember = useAddSquadMember();
  const [memberType, setMemberType] = useState("agent");
  const [memberId, setMemberId] = useState("");
  const [role, setRole] = useState("");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    addMember.mutate(
      { squadId, member_type: memberType, member_id: memberId, role },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="添加成员"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="add-member-form" disabled={addMember.isPending}>
            {addMember.isPending ? "…" : "添加"}
          </Button>
        </>
      }
    >
      <form id="add-member-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="成员类型">
          <select value={memberType} onChange={(e) => { setMemberType(e.target.value); setMemberId(""); }} className={inputCls}>
            <option value="agent">Agent</option>
            <option value="human">Human</option>
          </select>
        </Field>
        {memberType === "agent" && (
          <Field label="选择 Agent">
            <select value={memberId} onChange={(e) => setMemberId(e.target.value)} className={inputCls} required>
              <option value="">选择…</option>
              {agents?.map((a) => (
                <option key={a.id} value={a.id}>{a.name}</option>
              ))}
            </select>
          </Field>
        )}
        {memberType === "human" && (
          <Field label="Human ID">
            <input value={memberId} onChange={(e) => setMemberId(e.target.value)} className={inputCls} required placeholder="human id…" />
          </Field>
        )}
        <Field label="角色">
          <input value={role} onChange={(e) => setRole(e.target.value)} className={inputCls} placeholder="member, reviewer 等" />
        </Field>
        {addMember.isError && (
          <p className="text-sm text-red-500">{String(addMember.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
