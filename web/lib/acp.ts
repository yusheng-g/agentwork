// A minimal ACP client over WebSocket (Phase 6 chat). Speaks the STANDARD
// ACP lifecycle — initialize, session/list, session/new, session/load,
// session/prompt, event streaming, permission requests — against our own
// daemon endpoint (GET /agents/{id}/acp), which relays the frames
// unparsed to the machine. Protocol shapes mirror internal/acp.

export interface SessionInfo {
  sessionId: string;
  cwd: string;
  title?: string;
  updatedAt?: string;
}

export interface PermissionOption {
  optionId: string;
  name: string;
  kind: "allow_once" | "allow_always" | "reject_once" | "reject_always";
}

export interface PermissionRequest {
  sessionId: string;
  toolCall: { title?: string; kind?: string; status?: string };
  options: PermissionOption[];
}

export interface ChatEvent {
  kind: "user" | "agent" | "thought" | "tool";
  text: string;
  // tool-only fields — let the chat panel pair a tool_call (use) with its
  // tool_call_update (result) by callId, the same way the run-detail stream
  // pairs chat_message rows. Without these, a tool call rendered as two
  // unrelated bubbles (a "🔧 title" line + a "↳ output" line).
  callId?: string;
  toolName?: string;
  output?: string;
  isResult?: boolean;
}

export interface AcpChatCallbacks {
  onEvent?: (ev: ChatEvent) => void;
  onPermissionRequest?: (
    req: PermissionRequest,
    respond: (outcome: { optionId?: string; cancelled?: boolean }) => void,
  ) => void;
  onClose?: () => void;
}

export class AcpChatClient {
  private ws?: WebSocket;
  private id = 0;
  // closed marks an abandoned connect: React StrictMode runs the effect
  // twice (mount → cleanup → mount) — the first connect is closed before
  // its socket opens, and the late onopen must self-close instead of
  // leaking a machine-side chat channel.
  private closed = false;
  private pending = new Map<
    number,
    { resolve: (v: unknown) => void; reject: (e: Error) => void }
  >();
  private cb: AcpChatCallbacks;

  constructor(cb: AcpChatCallbacks) {
    this.cb = cb;
  }

  connect(url: string): Promise<void> {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      let settled = false;
      ws.onopen = () => {
        if (this.closed) {
          ws.close();
          return;
        }
        this.ws = ws;
        this.initialize().then(
          () => {
            settled = true;
            resolve();
          },
          (e) => {
            settled = true;
            reject(e);
          },
        );
      };
      ws.onerror = () => {
        if (this.closed) return;
        if (!settled) {
          settled = true;
          reject(new Error("连接失败"));
        }
      };
      ws.onclose = () => {
        // The StrictMode ghost: this connect was abandoned before its
        // socket opened, and the late onopen self-closed it. That is not
        // a real death — it must not fire the panel's "连接已断开" banner
        // while the live connection is fine.
        if (this.closed) return;
        if (!settled) {
          settled = true;
          reject(new Error("连接已关闭"));
        }
        // A dead connection rejects every in-flight request: the UI
        // unblocks the moment the socket actually dies — this replaces
        // artificial time limits (an agent turn takes as long as it takes).
        this.pending.forEach((p) => p.reject(new Error("连接已关闭")));
        this.pending.clear();
        this.cb.onClose?.();
      };
      ws.onmessage = (e) => this.onMessage(String(e.data));
    });
  }

  close() {
    this.closed = true;
    this.ws?.close();
  }

  private send(method: string, params: unknown): Promise<unknown> {
    return new Promise((resolve, reject) => {
      if (!this.ws) return reject(new Error("not connected"));
      const id = ++this.id;
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ jsonrpc: "2.0", id, method, params }));
      // No artificial deadline — the agent takes as long as it takes.
      // onclose rejects every pending request the moment the connection
      // actually dies, so the UI never hangs silently.
    });
  }

  private onMessage(raw: string) {
    let msg: any;
    try {
      msg = JSON.parse(raw);
    } catch {
      return;
    }
    if (typeof msg.id !== "undefined" && msg.id !== null && msg.method === undefined) {
      // Response to one of our requests.
      const p = this.pending.get(msg.id);
      if (!p) return;
      this.pending.delete(msg.id);
      if (msg.error) p.reject(new Error(msg.error.message ?? "acp error"));
      else p.resolve(msg.result);
      return;
    }
    if (msg.method === "session/update") {
      const u = msg.params?.update ?? {};
      const ev = updateToEvent(u);
      if (ev) this.cb.onEvent?.(ev);
      return;
    }
    if (msg.method === "session/request_permission") {
      const req = msg.params as PermissionRequest;
      const opts = req?.options ?? [];
      this.cb.onPermissionRequest?.(req, (outcome) => {
        // The ACP outcome is a tagged union: {"outcome":"selected",
        // "optionId":...} or {"outcome":"cancelled"} — the discriminant
        // is REQUIRED (live: omitting it made opencode read our approvals
        // as rejections; the VS Code client sends "selected" and works).
        // Also validate against the request's own options: a foreign or
        // empty optionId must not masquerade as a selection.
        let oc: Record<string, unknown>;
        if (
          outcome.optionId !== undefined &&
          opts.some((o) => o.optionId === outcome.optionId && o.optionId !== "")
        ) {
          oc = { outcome: "selected", optionId: outcome.optionId };
        } else {
          oc = { outcome: "cancelled" };
        }
        this.ws?.send(
          JSON.stringify({
            jsonrpc: "2.0",
            id: msg.id,
            result: { outcome: oc },
          }),
        );
      });
      return;
    }
    // fs/terminal delegations and other agent→client requests: respond
    // with an error so the agent's tool call fails visibly instead of
    // hanging.
    if (typeof msg.id !== "undefined" && msg.id !== null && msg.method) {
      this.ws?.send(
        JSON.stringify({
          jsonrpc: "2.0",
          id: msg.id,
          error: { code: -32601, message: "not supported by the web chat client" },
        }),
      );
    }
  }

  initialize(): Promise<void> {
    return this.send("initialize", {
      protocolVersion: 2,
      capabilities: { session: {} },
      info: { name: "agentwork-web", title: "Agentwork Web", version: "1.0.0" },
      clientInfo: { name: "agentwork-web", title: "Agentwork Web", version: "1.0.0" },
    }).then(() => undefined);
  }

  // The web never sends a cwd: the machine spawns the CLI with its process
  // cwd already set to the agent's chat directory, and the ACP session
  // defaults to it. Sending a path made the CLI resolve a doubled relative
  // path (a '~' the CLI does not expand) — lstat ENOENT, "service failure".
  listSessions(): Promise<SessionInfo[]> {
    return this.send("session/list", {}).then((r) => (r as { sessions: SessionInfo[] }).sessions ?? []);
  }

  newSession(): Promise<string> {
    return this.send("session/new", { mcpServers: [] }).then((r) => (r as { sessionId: string }).sessionId);
  }

  loadSession(sessionId: string, cwd: string): Promise<void> {
    // History replays as session/update events during the load response.
    // cwd is REQUIRED by the CLIs (the session store is keyed by project
    // directory) — take it from the session/list entry. Only ABSOLUTE
    // paths are trusted: a '~' form (a poisoned entry from an early bug,
    // or any non-absolute value) would be resolved by the CLI against the
    // process cwd into a doubled path — omit it and the CLI falls back to
    // the spawn cwd (the agent's chat directory), where chat sessions
    // actually live.
    const params: Record<string, unknown> = { sessionId, mcpServers: [] };
    if (cwd.startsWith("/")) params.cwd = cwd;
    return this.send("session/load", params).then(() => undefined);
  }

  prompt(sessionId: string, text: string): Promise<void> {
    // The ACP shape: a flat array of ContentBlock — the same shape the
    // Go client sends (verified against claude/opencode in the run flow).
    return this.send("session/prompt", {
      sessionId,
      prompt: [{ type: "text", text }],
    }).then(() => undefined);
  }

  cancel(sessionId: string): void {
    // session/cancel is a NOTIFICATION per the spec (the Go client sends
    // it without an id): no response comes back. The interrupted turn's
    // session/prompt response (stopReason cancelled) ends the busy state.
    if (!this.ws) return;
    this.ws.send(
      JSON.stringify({
        jsonrpc: "2.0",
        method: "session/cancel",
        params: { sessionId },
      }),
    );
  }
}

// updateToEvent maps one ACP session/update to a chat transcript entry
// ('' for updates the panel does not render).
function updateToEvent(u: any): ChatEvent | null {
  const kind = u.sessionUpdate;
  switch (kind) {
    case "user_message_chunk":
    case "agent_message_chunk": {
      const block = u.content;
      const text = typeof block?.text === "string" ? block.text : "";
      if (!text) return null;
      return { kind: kind === "user_message_chunk" ? "user" : "agent", text };
    }
    case "agent_thought_chunk": {
      const t = typeof u.content === "string" ? u.content : "";
      return t ? { kind: "thought", text: t } : null;
    }
    case "tool_call": {
      // Carry the callId + toolName so the panel pairs this use with its
      // later result update — no emoji in the text (the renderer styles it).
      return { kind: "tool", text: "", callId: u.toolCallId, toolName: u.title ?? "tool" };
    }
    case "tool_call_update": {
      const st = u.status ?? "";
      // failed 也是终态——丢弃会让工具卡永远停在「运行中…」。
      if (st !== "completed" && st !== "failed") return null;
      const raw = typeof u.rawOutput === "string" ? u.rawOutput : JSON.stringify(u.rawOutput ?? "");
      const out = st === "failed" ? `（失败）${raw}` : raw;
      return { kind: "tool", text: "", callId: u.toolCallId, output: out, isResult: true };
    }
    default:
      return null;
  }
}
