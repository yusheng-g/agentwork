"use client";

// ChatPanel — the web's ACP chat with one agent (Phase 6). The browser is
// a real ACP client: it connects to /agents/{id}/acp, lists the agent's
// sessions (the CLI's own store, keyed by the agent's chat directory),
// loads or creates a session, streams the conversation, and answers the
// agent's permission requests (allow/reject, per option).

import { useEffect, useRef, useState } from "react";
import { AcpChatClient, ChatEvent, PermissionRequest, SessionInfo } from "@/lib/acp";
import { Button, Dialog, inputCls } from "@/components/ui";
import { StreamCards } from "@/lib/run-messages";

interface Msg extends ChatEvent {
  id: number;
}

export function ChatPanel({ agentId, agentName, onClose }: { agentId: string; agentName: string; onClose: () => void }) {
  const [phase, setPhase] = useState<"connecting" | "pick" | "chat">("connecting");
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const [current, setCurrent] = useState<string>("");
  const [messages, setMessages] = useState<Msg[]>([]);
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [perm, setPerm] = useState<{ req: PermissionRequest; respond: (o: { optionId?: string; cancelled?: boolean }) => void } | null>(null);
  const [error, setError] = useState<string>("");
  const clientRef = useRef<AcpChatClient | null>(null);
  const msgID = useRef(0);
  // Whether the CURRENT turn produced any agent-side event (text, thought,
  // tool call). A turn that fails with zero agent activity failed at the
  // input layer — the retry-on-fresh-session decision keys on this, not on
  // provider error strings.
  const agentActivity = useRef(false);
  // The connect effect's cleanup closes over the MOUNT-time state — the
  // unmount cleanup needs the CURRENT turn, so mirror it in refs.
  const currentRef = useRef(current);
  const busyRef = useRef(busy);
  useEffect(() => {
    currentRef.current = current;
    busyRef.current = busy;
  }, [current, busy]);


  const push = (ev: ChatEvent) => {
    msgID.current += 1;
    const nextId = msgID.current;
    if (ev.kind !== "user") agentActivity.current = true;
    setMessages((prev) => {
      // The agent's reply streams in dozens of small session/update
      // chunks — merge each contiguous run of agent/thought chunks into
      // ONE bubble, or the transcript reads as a pile of fragments.
      const last = prev[prev.length - 1];
      if (last && last.kind === ev.kind && (ev.kind === "agent" || ev.kind === "thought")) {
        return [...prev.slice(0, -1), { ...last, text: last.text + ev.text }];
      }
      // A tool_result (isResult) merges into the preceding tool_use with the
      // SAME callId — one card per tool call (request + response), not two.
      if (ev.kind === "tool" && ev.isResult && ev.callId) {
        const idx = prev.findLastIndex((m) => m.kind === "tool" && m.callId === ev.callId && !m.isResult);
        if (idx >= 0) {
          const target = prev[idx];
          const next = [...prev];
          next[idx] = { ...target, output: ev.output, isResult: true };
          return next;
        }
      }
      return [...prev, { ...ev, id: nextId }];
    });
  };

  useEffect(() => {
    const client = new AcpChatClient({
      onEvent: push,
      onPermissionRequest: (req, respond) => setPerm({ req, respond }),
      onClose: () => {
        setError((e) => e || "连接已断开");
        setPhase((p) => (p === "chat" ? p : "pick"));
      },
    });
    clientRef.current = client;
    const host = window.location.host;
    // /backend/* is the Next rewrite to the daemon (7373) — REST calls and
    // the /ws event hub use it; the chat WS must ride the same tunnel or
    // the upgrade hits the Next service and hangs.
    client
      .connect(`ws${window.location.protocol === "https:" ? "s" : ""}://${host}/backend/agents/${agentId}/acp`)
      .then(async () => {
        try {
          const list = await client.listSessions();
          setSessions(list);
          setPhase("pick");
        } catch {
          setSessions([]);
          setPhase("pick");
        }
      })
      .catch((e) => setError(`连接失败：${e.message}`));
    return () => {
      // Graceful close: cancel the in-flight turn FIRST — the cancel is a
      // notification, and the browser delivers queued frames before the
      // close frame — so the CLI ends the turn cleanly and persists an
      // intact history instead of being killed mid-write.
      if (busyRef.current && currentRef.current) client.cancel(currentRef.current);
      client.close();
      clientRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId]);

  const openSession = async (s: SessionInfo) => {
    const client = clientRef.current;
    if (!client) return;
    setBusy(true);
    setMessages([]);
    setCurrent(s.sessionId);
    try {
      await client.loadSession(s.sessionId, s.cwd);
      setPhase("chat");
    } catch (e) {
      setError(`加载会话失败：${(e as Error).message}`);
    }
    setBusy(false);
  };

  const newSession = async () => {
    const client = clientRef.current;
    if (!client) return;
    setBusy(true);
    setMessages([]);
    try {
      const sid = await client.newSession();
      setCurrent(sid);
      setPhase("chat");
    } catch (e) {
      setError(`创建会话失败：${(e as Error).message}`);
    }
    setBusy(false);
  };

  const send = async () => {
    const client = clientRef.current;
    const text = input.trim();
    // busy 必须挡在入口——Enter 键不走 Button 的 disabled，双击/回车
    // 会在同一会话上叠加第二个 prompt。
    if (!client || !current || !text || busy) return;
    setInput("");
    agentActivity.current = false;
    push({ kind: "user", text });
    setBusy(true);
    try {
      await client.prompt(current, text);
    } catch (e) {
      const msg = (e as Error).message;
      // Poisoned history: the turn fails at the INPUT layer — the agent
      // never produced anything (no chunks, no tool calls), so the
      // session's history can no longer be accepted. Retrying on a fresh
      // session is safe exactly because nothing ran yet: no side effects
      // to duplicate. A turn that DID act before failing is a real work
      // failure — surface it instead of retrying.
      if (!agentActivity.current) {
        push({ kind: "agent", text: "（会话历史已损坏——上次回合被中断，自动新建会话重试）" });
        try {
          const sid = await client.newSession();
          setCurrent(sid);
          await client.prompt(sid, text);
        } catch (e2) {
          setError(`发送失败：${(e2 as Error).message}`);
        }
        setBusy(false);
        return;
      }
      setError(`发送失败：${msg}`);
    }
    setBusy(false);
  };

  return (
    <Dialog
      title={`与 ${agentName} 聊天`}
      onClose={onClose}
      wide
      footer={<Button variant="outline" onClick={onClose}>关闭</Button>}
    >
      <div className="flex flex-col h-[480px]">
        {error && <p className="text-xs text-red-500 mb-2">{error}</p>}
        {phase === "connecting" && <p className="text-sm text-zinc-400">连接中…</p>}

        {phase === "pick" && (
          <div className="flex-1 overflow-y-auto space-y-2">
            <Button onClick={newSession} disabled={busy}>＋ 新建会话</Button>
            {sessions.length > 0 && <p className="text-xs text-zinc-400 pt-2">历史会话（agent CLI 的会话存储，刷新不丢）：</p>}
            {sessions.map((s) => (
              <button
                key={s.sessionId}
                onClick={() => openSession(s)}
                disabled={busy}
                className="block w-full text-left px-3 py-2 rounded-lg border border-zinc-200 hover:border-indigo-300 text-sm disabled:opacity-50"
              >
                <span className="font-medium text-zinc-800">{s.title || s.sessionId.slice(0, 12)}</span>
                <span className="ml-2 text-xs text-zinc-400">{s.updatedAt ? new Date(s.updatedAt).toLocaleString() : ""}</span>
              </button>
            ))}
          </div>
        )}

        {phase === "chat" && (
          <>
            <div className="flex-1 overflow-y-auto space-y-2 rounded-lg bg-zinc-50 border border-zinc-200 p-3">
              {messages.map((m) => {
                // User + agent text stay as chat bubbles (the conversational
                // surface). Thought + tool render as collapsible StreamCards
                // (the agent's interaction stream) — same style as the
                // run-detail live stream and the timeline side card.
                if (m.kind === "thought") {
                  return (
                    <StreamCards
                      key={m.id}
                      items={[{ kind: "thought", key: String(m.id), content: m.text, isStreaming: busy }]}
                    />
                  );
                }
                if (m.kind === "tool") {
                  return (
                    <StreamCards
                      key={m.id}
                      items={[{
                        kind: "tool",
                        key: m.callId ?? String(m.id),
                        toolName: m.toolName ?? "tool",
                        input: "",
                        output: m.output ?? "",
                        hasResult: !!m.isResult,
                      }]}
                    />
                  );
                }
                return (
                  <div key={m.id} className={m.kind === "user" ? "text-right" : "text-left"}>
                    <span
                      className={`inline-block max-w-[85%] px-3 py-1.5 rounded-xl text-sm whitespace-pre-wrap break-words ${
                        m.kind === "user"
                          ? "bg-indigo-600 text-white"
                          : "bg-white border border-zinc-200 text-zinc-800"
                      }`}
                    >
                      {m.text}
                    </span>
                  </div>
                );
              })}
              {busy && (
                <div className="flex items-center gap-3">
                  <p className="text-xs text-zinc-400">agent 思考中…</p>
                  <button
                    onClick={() => {
                      const client = clientRef.current;
                      if (client && current) client.cancel(current);
                    }}
                    className="text-xs text-red-500 hover:text-red-700"
                  >
                    停止
                  </button>
                </div>
              )}
            </div>
            <div className="flex gap-2 mt-2">
              <input
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && send()}
                className={`${inputCls} flex-1`}
                placeholder="发消息…（Enter 发送）"
              />
              <Button onClick={send} disabled={busy || !input.trim()}>发送</Button>
            </div>
          </>
        )}
      </div>

      {perm && (
        <div className="fixed inset-0 bg-black/30 flex items-center justify-center z-50">
          <div className="bg-white rounded-xl border border-zinc-200 p-4 w-96 shadow-lg">
            <p className="text-sm font-medium text-zinc-800">agent 请求权限</p>
            <p className="mt-1 text-xs text-zinc-500">
              {perm.req.toolCall?.title || "tool"}（{perm.req.toolCall?.kind || "unknown"}）
            </p>
            <div className="mt-3 flex gap-2 justify-end">
              {(perm.req.options ?? []).filter((o) => o.kind?.startsWith("allow")).map((o) => (
                <Button
                  key={o.optionId}
                  onClick={() => {
                    perm.respond({ optionId: o.optionId });
                    setPerm(null);
                  }}
                >
                  {o.name || "允许"}
                </Button>
              ))}
              {(perm.req.options ?? []).filter((o) => o.kind?.startsWith("reject")).map((o) => (
                <Button
                  key={o.optionId}
                  variant="outline"
                  onClick={() => {
                    perm.respond({ optionId: o.optionId });
                    setPerm(null);
                  }}
                >
                  {o.name || "拒绝"}
                </Button>
              ))}
              <Button
                variant="outline"
                onClick={() => {
                  perm.respond({ cancelled: true });
                  setPerm(null);
                }}
              >
                取消
              </Button>
            </div>
          </div>
        </div>
      )}
    </Dialog>
  );
}
