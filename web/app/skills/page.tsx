"use client";

import { useState } from "react";
import { useSkills, useCreateSkill, useDeleteSkill } from "@/lib/queries";
import { PageHeader, Empty, Button, Dialog, Field, inputCls } from "@/components/ui";

export default function SkillsPage() {
  const { data: skills, isLoading } = useSkills();
  const del = useDeleteSkill();
  const [showCreate, setShowCreate] = useState(false);

  return (
    <div className="p-6 max-w-4xl mx-auto space-y-4">
      <PageHeader title="Skills" />
      <p className="text-sm text-gray-500 -mt-3">
        平台技能库：上传 skill 包（SKILL.md 指令），勾选给 agent 后下发到它所在机器；run 启动时按原名装入工作目录的项目级 skills 目录（.claude/skills / .opencode/skill / .agents/skills，取决于 agent CLI），不碰你机器上手动装的全局 skills。
      </p>
      <div className="mb-4">
        <Button onClick={() => setShowCreate(true)}>上传 Skill</Button>
      </div>

      {isLoading ? (
        <p className="text-gray-500 text-sm">加载中…</p>
      ) : !skills?.length ? (
        <Empty>技能库为空——上传一个 skill 包开始。</Empty>
      ) : (
        <div className="space-y-3">
          {skills.map((sk) => (
            <div key={sk.id} className="rounded-xl border border-zinc-200/80 bg-white shadow-sm p-4">
              <div className="flex items-center gap-3">
                <span className="font-medium text-sm text-zinc-900">{sk.name}</span>
                {sk.description && <span className="text-xs text-zinc-500">— {sk.description}</span>}
                <span className="ml-auto text-xs text-zinc-400">{new Date(sk.created_at).toLocaleString("zh-CN")}</span>
                <Button
                  variant="danger"
                  className="!px-2.5 !py-1 text-xs"
                  disabled={del.isPending && del.variables === sk.id}
                  onClick={() => {
                    if (confirm(`删除 skill「${sk.name}」？`)) del.mutate(sk.id);
                  }}
                >
                  删除
                </Button>
              </div>
              {/* 错误只挂到对应行——全局 isPending/isError 会扩散到所有行。 */}
              {del.isError && del.variables === sk.id && (
                <p className="text-xs text-red-500 mt-1">{String(del.error)}</p>
              )}
            </div>
          ))}
        </div>
      )}

      {showCreate && <CreateSkillDialog onClose={() => setShowCreate(false)} />}
    </div>
  );
}

// CreateSkillDialog uploads a skill package as a ZIP archive. The skill's
// name and description are PARSED by the platform from the archive's
// SKILL.md frontmatter — the user only picks the zip. A zip without a valid
// SKILL.md (or without a name in its frontmatter) is rejected as "not a
// skill package".
function CreateSkillDialog({ onClose }: { onClose: () => void }) {
  const create = useCreateSkill();
  const [file, setFile] = useState<File | null>(null);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!file) return;
    create.mutate(
      { file },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="上传 Skill"
      onClose={onClose}
      wide
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="skill-form" disabled={create.isPending || !file}>
            {create.isPending ? "上传中…" : "上传"}
          </Button>
        </>
      }
    >
      <form id="skill-form" onSubmit={submit} className="space-y-4">
        <Field label="Skill 包（.zip）" hint="包内必须有 SKILL.md，且其 frontmatter 含 name（可选 description）。平台解析 SKILL.md 自动回填名称与描述，原样下发到机器解压。">
          <input
            type="file"
            accept=".zip"
            onChange={(e) => setFile(e.target.files?.[0] ?? null)}
            className={`${inputCls} file:mr-3 file:rounded-lg file:border-0 file:bg-indigo-50 file:px-3 file:py-1 file:text-sm file:text-indigo-700`}
            required
          />
          {file && <p className="text-xs text-zinc-500">已选择：{file.name}（{(file.size / 1024).toFixed(0)} KB）</p>}
        </Field>
        <p className="text-xs text-zinc-500">
          SKILL.md 示例：<br />
          <code className="font-mono">{"---"}</code><br />
          <code className="font-mono">name: code-review-checklist</code><br />
          <code className="font-mono">description: 一句话说明这个 skill 干什么</code><br />
          <code className="font-mono">{"---"}</code><br />
          （以下是 skill 指令正文）
        </p>
        {create.isError && <p className="text-sm text-red-500">{String(create.error)}</p>}
      </form>
    </Dialog>
  );
}
