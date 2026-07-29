"use client";

import { useState } from "react";
import { useCreateRuntime } from "@/lib/queries";
import { Button, Dialog, Field, inputCls } from "@/components/ui";

export function RuntimeForm({ onClose }: { onClose: () => void }) {
  const create = useCreateRuntime();
  const [name, setName] = useState("");
  const [transport, setTransport] = useState("stdio");
  const [provider, setProvider] = useState("acp");
  const [executable, setExecutable] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [args, setArgs] = useState("[]");
  const [env, setEnv] = useState("{}");

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    let parsedArgs: string[] = [];
    let parsedEnv: Record<string, string> = {};
    try { parsedArgs = JSON.parse(args || "[]"); } catch { /* keep default */ }
    try { parsedEnv = JSON.parse(env || "{}"); } catch { /* keep default */ }
    create.mutate(
      {
        name,
        transport,
        provider,
        executable,
        endpoint,
        args: parsedArgs,
        env: parsedEnv,
      },
      { onSuccess: onClose }
    );
  };

  return (
    <Dialog
      title="新建 Runtime"
      onClose={onClose}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>取消</Button>
          <Button type="submit" form="runtime-form" disabled={create.isPending}>
            {create.isPending ? "创建中…" : "创建"}
          </Button>
        </>
      }
    >
      <form id="runtime-form" onSubmit={handleSubmit} className="space-y-4">
        <Field label="名称">
          <input value={name} onChange={(e) => setName(e.target.value)} className={inputCls} placeholder="openagent-cli" required />
        </Field>

        <Field label="Transport">
          <select value={transport} onChange={(e) => setTransport(e.target.value)} className={inputCls}>
            <option value="stdio">stdio</option>
            <option value="ws">ws</option>
            <option value="tcp">tcp</option>
          </select>
        </Field>

        <Field label="Provider" hint="协议类型（agent 使用的通信协议）">
          <select value={provider} onChange={(e) => setProvider(e.target.value)} className={inputCls}>
            <option value="acp">acp</option>
            <option value="jsonl">jsonl</option>
            <option value="jsonrpc">jsonrpc</option>
          </select>
        </Field>

        {transport === "stdio" ? (
          <Field label="Executable">
            <input value={executable} onChange={(e) => setExecutable(e.target.value)} className={inputCls} placeholder="/path/to/openagent-cli" required />
          </Field>
        ) : (
          <Field label="Endpoint">
            <input value={endpoint} onChange={(e) => setEndpoint(e.target.value)} className={inputCls} placeholder={transport === "ws" ? "ws://host:port" : "host:port"} required />
          </Field>
        )}

        {transport === "stdio" && (
          <Field label="Args (JSON)" hint="传给可执行文件的参数">
            <input value={args} onChange={(e) => setArgs(e.target.value)} className={`${inputCls} font-mono`} placeholder='["serve","--acp"]' />
          </Field>
        )}

        {transport === "stdio" && (
          <Field label="Env (JSON)" hint="环境变量">
            <input value={env} onChange={(e) => setEnv(e.target.value)} className={`${inputCls} font-mono`} placeholder="{}" />
          </Field>
        )}

        {create.isError && (
          <p className="text-sm text-red-500">{String(create.error)}</p>
        )}
      </form>
    </Dialog>
  );
}
