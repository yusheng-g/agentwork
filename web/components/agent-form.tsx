"use client";

import { useState } from "react";
import { useCreateAgent, useRuntimes } from "@/lib/queries";
import { Button, Dialog, Field, inputCls } from "@/components/ui";

export function AgentForm({ onClose }: { onClose: () => void }) {
  const create = useCreateAgent();
  const { data: runtimes } = useRuntimes();
  const [name, setName] = useState("");
  const [runtimeId, setRuntimeId] = useState("");
  const [systemPrompt, setSystemPrompt] = useState("");
  const [model, setModel] = useState("");
  const [workdirBase, setWorkdirBase] = useState("");
  const [env, setEnv] = useState("{}");
  const [maxConcurrent, setMaxConcurrent] = useState("1");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    let parsedEnv: Record<string, string> = {};
    try { parsedEnv = JSON.parse(env || "{}"); } catch { /* keep default */ }
    create.mutate(
      {
        name,
        description: "",
        runtime_id: runtimeId,
        system_prompt: systemPrompt,
        model,
        workdir_base: workdirBase,
        env: parsedEnv,
        max_concurrent: parseInt(maxConcurrent) || 1,
      },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="新建 Agent"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="agent-form" disabled={create.isPending}>
            {create.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="agent-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} required />
        </Field>

        <Field label="Runtime">
          <select value={runtimeId} onChange={(e) => setRuntimeId(e.target.value)} className={inputCls} required>
            <option value="">选择…</option>
            {runtimes?.map((rt) => (
              <option key={rt.id} value={rt.id}>{rt.name}</option>
            ))}
          </select>
        </Field>

        <Field label="System Prompt">
          <textarea value={systemPrompt} onChange={(e) => setSystemPrompt(e.target.value)} className={`${inputCls} font-mono`} rows={3} />
        </Field>

        <Field label="Model" hint="留空用 runtime 默认">
          <input value={model} onChange={(e) => setModel(e.target.value)} className={inputCls} />
        </Field>

        <Field label="Workdir Base">
          <input value={workdirBase} onChange={(e) => setWorkdirBase(e.target.value)} className={`${inputCls} font-mono`} placeholder="/tmp/agentwork/work" />
        </Field>

        <Field label="Env (JSON)">
          <input value={env} onChange={(e) => setEnv(e.target.value)} className={`${inputCls} font-mono`} placeholder="{}" />
        </Field>

        <Field label="最大并发">
          <input value={maxConcurrent} onChange={(e) => setMaxConcurrent(e.target.value)} className={inputCls} type="number" min="1" />
        </Field>

        {create.isError && (
          <p className="text-sm text-red-500">{String(create.error)}</p>
        )}
      </form>
    </Dialog>
  );
}