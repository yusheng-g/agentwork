"use client";

import { useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { useSquads, useAgents, useSquadMembers, useAddSquadMember, useUpdateSquad, useRemoveSquadMember, useGoalEvents } from "@/lib/queries";
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
  const updateSquad = useUpdateSquad();
  const removeMember = useRemoveSquadMember();
  const [showAdd, setShowAdd] = useState(false);
  const [showEdit, setShowEdit] = useState(false);
  const [editName, setEditName] = useState("");
  const [editLeader, setEditLeader] = useState("");
  const [editDesc, setEditDesc] = useState("");
  const [editInstr, setEditInstr] = useState("");

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
        <div className="flex items-center justify-between">
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
          <Button variant="outline" onClick={() => { setEditName(squad.name); setEditLeader(squad.leader_id); setEditDesc(squad.description || ""); setEditInstr(squad.instructions || ""); setShowEdit(true); }}>
            编辑
          </Button>
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
                  <th className="px-3 py-2"></th>
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
                    <td className="px-3 py-2">
                      <button
                        onClick={() => removeMember.mutate({ squadId: id, memberId: m.member_id })}
                        disabled={removeMember.isPending && removeMember.variables?.memberId === m.member_id}
                        className="text-red-500 hover:text-red-700 text-xs disabled:opacity-50"
                        title="移除该成员"
                      >✕</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {/* 成员移除失败必须显示——后端 400 错误静默等于「点了没反应」。 */}
      {removeMember.isError && (
        <p className="text-sm text-red-500">{String(removeMember.error)}</p>
      )}

      {/* Add member dialog */}
      {showAdd && (
        <AddMemberDialog
          squadId={id}
          agents={agents}
          onClose={() => setShowAdd(false)}
        />
      )}

      {showEdit && (
        <Dialog
          title={`编辑 Squad：${squad.name}`}
          onClose={() => setShowEdit(false)}
          footer={
            <>
              <Button variant="outline" onClick={() => setShowEdit(false)}>取消</Button>
              <Button
                onClick={() =>
                  updateSquad.mutate(
                    { id, name: editName, description: editDesc, leader_id: editLeader, instructions: editInstr },
                    { onSuccess: () => setShowEdit(false) }
                  )
                }
                disabled={updateSquad.isPending || !editName.trim() || !editLeader}
              >
                {updateSquad.isPending ? "保存中…" : "保存"}
              </Button>
            </>
          }
        >
          <div className="space-y-4">
            <Field label="名称" hint="必填">
              <input value={editName} onChange={(e) => setEditName(e.target.value)} className={inputCls} placeholder="Squad 名称…" />
            </Field>
            <Field label="Leader" hint="必填，leader 必须是 agent（角色动态生效：旧 leader 的 in-flight run 会孤儿化）">
              <select value={editLeader} onChange={(e) => setEditLeader(e.target.value)} className={inputCls}>
                <option value="">选择…</option>
                {agents?.map((a) => (
                  <option key={a.id} value={a.id}>{a.name}</option>
                ))}
              </select>
            </Field>
            <Field label="描述">
              <textarea value={editDesc} onChange={(e) => setEditDesc(e.target.value)} className={inputCls} rows={2} />
            </Field>
            <Field label="Instructions" hint="leader 运行时会注入这些 instructions">
              <textarea value={editInstr} onChange={(e) => setEditInstr(e.target.value)} className={inputCls} rows={3} placeholder="Squad 工作说明…" />
            </Field>
            {/* 保存失败必须显示——leader 校验等后端 400 错误静默等于「点了没反应」。 */}
            {updateSquad.isError && (
              <p className="text-sm text-red-500">{String(updateSquad.error)}</p>
            )}
          </div>
        </Dialog>
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
  const [role, setRole] = useState("member");

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
          <select value={role} onChange={(e) => setRole(e.target.value)} className={inputCls}>
            <option value="member">执行者（被 leader 分派干活）</option>
            <option value="reviewer">审核者（进审批时被平台自动拉去审查）</option>
          </select>
        </Field>
        {addMember.isError && (
          <p className="text-sm text-red-500">{String(addMember.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
