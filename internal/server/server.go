// Package server is the HTTP + WebSocket boundary. Routes task/agent/runtime
// CRUD and streams session events to the frontend over WS.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eushing/agentwork/internal/daemon"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/issue"
	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/server/handler"
	"github.com/eushing/agentwork/internal/server/ws"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
	"github.com/eushing/agentwork/internal/version"

	"github.com/gorilla/websocket"
)

type Server struct {
	st            *store.Store
	bus           *events.Bus
	d             *daemon.Daemon
	hub           *ws.Hub
	goalSvc       *service.GoalService
	runSvc        *service.RunService
	commentSvc    *service.CommentService
	squadSvc      *service.SquadService
	schedSvc      *service.ScheduleService
	domainSvc     *service.DomainService
	imConn        *notify.Connector
	teamImportSvc *service.TeamImportService
	skillSvc      *service.SkillService
	intakeSvc     *notify.IntakeService
}

func New(st *store.Store, bus *events.Bus, d *daemon.Daemon, goalSvc *service.GoalService, runSvc *service.RunService, commentSvc *service.CommentService, squadSvc *service.SquadService, schedSvc *service.ScheduleService, domainSvc *service.DomainService, imConn *notify.Connector, teamImportSvc *service.TeamImportService, skillSvc *service.SkillService, intakeSvc *notify.IntakeService) *Server {
	return &Server{st: st, bus: bus, d: d, hub: ws.NewHub(bus), goalSvc: goalSvc, runSvc: runSvc, commentSvc: commentSvc, squadSvc: squadSvc, schedSvc: schedSvc, domainSvc: domainSvc, imConn: imConn, teamImportSvc: teamImportSvc, skillSvc: skillSvc, intakeSvc: intakeSvc}
}

// ListenAndServe mounts routes and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	settingsSvc := service.NewSettingsService(s.st)
	machineSvc := service.NewMachineService(s.st)
	h := &handler.Handlers{
		Runtime:    service.NewRuntimeService(s.st),
		Agent:      service.NewAgentService(s.st, s.bus),
		Goal:       s.goalSvc,
		Run:        s.runSvc,
		Comment:    s.commentSvc,
		Squad:      s.squadSvc,
		Schedule:   s.schedSvc,
		Domain:     s.domainSvc,
		Settings:   settingsSvc,
		IM:         s.imConn,
		Daemon:     s.d,
		Machines:   machineSvc,
		Skills:     s.skillSvc,
		AgentPin:   service.NewAgentPinService(s.st, s.bus),
		TeamImport: s.teamImportSvc,
		Intake:     s.intakeSvc,
		// M4-B: the real-time issue triggers (github + gitcode) share the
		// poller's create path (source_ref idempotency makes webhook + poll
		// racing safe). The shared secret lives in app_settings
		// (platform.webhook_secret — shared across providers: one secret
		// configures every repo's webhook on a single-user platform);
		// empty = webhook disabled, polling still covers it.
		IssueWebhooks: map[string]*issue.WebhookHandler{
			"github": issue.NewWebhookHandler("github", s.st, s.d.Poller(),
				func(ctx context.Context) (string, error) { return settingsSvc.Get(ctx, "platform.webhook_secret") }),
			"gitcode": issue.NewWebhookHandler("gitcode", s.st, s.d.Poller(),
				func(ctx context.Context) (string, error) { return settingsSvc.Get(ctx, "platform.webhook_secret") }),
		},
	}

	// seedStewardIfCLI scans probed CLIs for a StewardSeedCLIName entry and seeds
	// the steward agent on its runtime. Shared by register and probe_update.
	seedStewardIfCLI := func(ctx context.Context, machineName string, clis []link.ProbeCLI) {
		for _, c := range clis {
			if c.Name == service.StewardSeedCLIName {
				if err := h.Agent.SeedStewardForRuntime(ctx, service.StewardSeedCLIName+"@"+machineName); err != nil {
					logging.Warnf("connect: seed steward for %s: %v", service.StewardSeedCLIName, err)
				}
				return
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Logs API: the Web logs panel's history (time/level filtered) and the
	// runtime level knob (persisted — the daemon restores it at startup).
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var after, before *time.Time
		if v := q.Get("after"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				after = &t
			}
		}
		if v := q.Get("before"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				before = &t
			}
		}
		limit := 500
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		minLevel := logging.ParseLevel(q.Get("level"))
		lines, err := logging.ReadLogs(logging.DefaultPath(), after, before, limit, minLevel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"level": logging.GetLevel().String(), "lines": lines})
	})
	mux.HandleFunc("GET /logs/level", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"level": logging.GetLevel().String()})
	})
	mux.HandleFunc("PUT /logs/level", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lv := logging.ParseLevel(body.Level)
		if lv.String() != body.Level {
			http.Error(w, "level must be debug, info, warn, or error", http.StatusBadRequest)
			return
		}
		logging.SetLevel(lv)
		if err := settingsSvc.Set(context.Background(), "logging.level", lv.String()); err != nil {
			logging.Errorf("server: persist log level: %v", err)
		}
		logging.Infof("logging: level set to %s (Web)", lv)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"level": lv.String()})
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		ws.ServeWS(s.hub, w, r)
	})
	// The agentwork CLI's link (CLI 分支 Phase 1): a JSON-RPC 2.0 over
	// WebSocket connection carrying machine registration, heartbeats, and
	// probe reports (run dispatch/config push land in later phases).
	// /connect auth is handled by authMiddleware on non-loopback (the machine
	// connect CLI passes ?token=worker_token). On loopback authMiddleware is
	// not installed — no check needed.
	connectUpgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true }, // single-user local
	}
	mux.HandleFunc("GET /connect", func(w http.ResponseWriter, r *http.Request) {
		// /connect auth is handled by authMiddleware on non-loopback (the
		// machine connect CLI passes ?token=worker_token, connect.go:431).
		// On loopback authMiddleware is not installed — no check needed.
		// No internal check here (was redundant + fail-open).
		conn, err := connectUpgrader.Upgrade(w, r, nil)
		if err != nil {
			logging.Infof("connect: upgrade: %v", err)
			return
		}
		peer := link.NewPeer(conn)
		// The peer binds to its machine on register; on link death the
		// daemon drops it (dispatches then fail-fast with "machine offline").
		// The registered id is written by the register handler and read by
		// the report handlers (both run on link goroutines) — atomic, never
		// a plain variable. Unregister passes the PEER too: a stale dying
		// connection must not evict the machine's live replacement.
		var registeredMachineID atomic.Value // string
		machineID := func() string {
			v, _ := registeredMachineID.Load().(string)
			return v
		}
		defer func() {
			if id := machineID(); id != "" {
				s.d.UnregisterMachinePeer(id, peer)
			}
		}()
		peer.Handle(link.MethodMachineRegister, func(ctx context.Context, params json.RawMessage) (any, *link.RPCError) {
			var p link.RegisterParams
			if err := json.Unmarshal(params, &p); err != nil || p.MachineID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "machine_id is required"}
			}
			if err := machineSvc.Register(ctx, service.Machine{ID: p.MachineID, Name: p.Name, Hostname: p.Hostname, Version: p.Version}, service.MarshalProbedCLIs(p.CLIs)); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			// Phase 2: each probed CLI becomes an executable runtime row
			// owned by this machine — runs on it dispatch over the link.
			// The reconcile also marks CLIs gone from the probe absent.
			if err := machineSvc.ReconcileProbeRuntimes(ctx, p.MachineID, p.Name, p.CLIs); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			seedStewardIfCLI(ctx, p.Name, p.CLIs)
			// Bind the live peer: dispatched runs for this machine's
			// runtimes flow over THIS link.
			s.d.RegisterMachinePeer(p.MachineID, peer)
			registeredMachineID.Store(p.MachineID)
			// Full skills sync (Phase 4): offline edits land on reconnect.
			go s.d.PushMachineSkills(context.Background(), p.MachineID)
			if p.Version != "" && p.Version != version.DaemonVersion {
				logging.Infof("connect: machine %s CLI version %s differs from daemon %s — protocol drift possible", p.MachineID, p.Version, version.DaemonVersion)
			}
			logging.Infof("connect: machine %q (%s) registered, %d agent CLI(s) probed", p.Name, p.Hostname, len(p.CLIs))
			return link.RegisterResult{OK: true, ServerVersion: version.DaemonVersion}, nil
		})
		peer.Handle(link.MethodChatFrame, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			var p link.ChatFrameParams
			_ = json.Unmarshal(raw, &p)
			s.d.MachineChatFrame(p)
			return nil, nil // notification — no reply
		})
		peer.Handle(link.MethodChatClosed, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			var p link.ChatClosedParams
			_ = json.Unmarshal(raw, &p)
			s.d.MachineChatClosed(p)
			return nil, nil // notification — no reply
		})
		peer.Handle(link.MethodRunPoll, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id := machineID()
			if id == "" {
				return nil, &link.RPCError{Code: link.CodeForbidden, Message: "register first"}
			}
			// The machine's identity is the CONNECTION's registered id —
			// a self-reported machine_id in the params must not steal
			// another machine's dispatch. ctx carries the connection's
			// lifetime: a dropped link cancels the hold (no leaked waiters).
			return s.d.DequeueMachineDispatchWait(ctx, id), nil
		})
		peer.Handle(link.MethodRunClaimed, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			var p link.RunClaimedParams
			if err := json.Unmarshal(raw, &p); err != nil || p.RunID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "run_id is required"}
			}
			id := machineID()
			if id == "" {
				return nil, &link.RPCError{Code: link.CodeForbidden, Message: "register first"}
			}
			return nil, s.d.IngestRunClaimed(ctx, id, p)
		})
		peer.Handle(link.MethodRunEventBatch, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			var p link.RunEventBatchParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "invalid event batch"}
			}
			id := machineID()
			if id == "" {
				return nil, &link.RPCError{Code: link.CodeForbidden, Message: "register first"}
			}
			return nil, s.d.IngestRunEvents(ctx, id, p)
		})
		peer.Handle(link.MethodRunFinished, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			var p link.RunFinishedParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "invalid finish report"}
			}
			id := machineID()
			if id == "" {
				return nil, &link.RPCError{Code: link.CodeForbidden, Message: "register first"}
			}
			return nil, s.d.IngestRunFinished(ctx, id, p)
		})
		peer.Handle(link.MethodMachineHeartbeat, func(ctx context.Context, params json.RawMessage) (any, *link.RPCError) {
			var p link.HeartbeatParams
			if err := json.Unmarshal(params, &p); err != nil || p.MachineID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "machine_id is required"}
			}
			if err := machineSvc.Heartbeat(ctx, p.MachineID); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return nil, nil // notification — no reply
		})
		peer.Handle(link.MethodMachineOffline, func(ctx context.Context, params json.RawMessage) (any, *link.RPCError) {
			var p link.HeartbeatParams // reuse: {machine_id} same shape as heartbeat
			if err := json.Unmarshal(params, &p); err != nil || p.MachineID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "machine_id is required"}
			}
			// The notification arrives just before the CLI closes the
			// WebSocket — the peer's ctx (passed in as ctx) is cancelled
			// the instant the read loop sees the close frame, racing the
			// DB write. Use a detached context so MarkOffline completes
			// even as the connection tears down.
			flipped, err := machineSvc.MarkOffline(context.Background(), p.MachineID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			if flipped {
				s.bus.Publish(context.Background(), events.Event{Topic: "machine:offline", Payload: map[string]string{"machine_id": p.MachineID}})
			}
			logging.Infof("connect: machine %s marked offline (graceful shutdown)", p.MachineID)
			return nil, nil // notification — no reply
		})
		peer.Handle(link.MethodMachineProbeUpdate, func(ctx context.Context, params json.RawMessage) (any, *link.RPCError) {
			var p link.ProbeUpdateParams
			if err := json.Unmarshal(params, &p); err != nil || p.MachineID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "machine_id is required"}
			}
			if err := machineSvc.UpdateProbe(ctx, p.MachineID, service.MarshalProbedCLIs(p.CLIs)); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			var machineName string
			if err := s.st.DB().QueryRowContext(ctx, `SELECT name FROM machine WHERE id=?`, p.MachineID).Scan(&machineName); err == nil {
				seedStewardIfCLI(ctx, machineName, p.CLIs)
			}
			logging.Infof("connect: machine %s probe update: %d agent CLI(s)", p.MachineID, len(p.CLIs))
			return link.RegisterResult{OK: true}, nil
		})
		peer.Wait()
	})
	// Chat keepalive: the ACP relay has no inherent traffic during an agent's
	// turn (all chats are pure pass-through — 决策7-5). A reverse proxy's
	// idle timeout (nginx 300s default)
	// drops the silently idle WebSocket, surfacing as 1006 abnormal closure on
	// both ends. The ping/pong pair mirrors /ws's ws/client.go: a 30s server
	// ping refreshes the proxy's idle timer; the browser's native WebSocket
	// auto-replies pong, which refreshes our read deadline. WriteControl is
	// gorilla-safe to call concurrently with the pump goroutine's WriteMessage
	// (doc.go: "The Close and WriteControl methods can be called concurrently
	// with all other methods").
	const (
		chatWriteWait  = 10 * time.Second
		chatPongWait   = 90 * time.Second
		chatPingPeriod = 30 * time.Second // well under the 300s infra timeout
	)

	// The agent chat (Phase 6): a WebSocket upgrade for web ACP clients —
	// the daemon opens a machine-side chat channel and relays ACP frames
	// UNPARSED in both directions. The agent id comes from the PATH; the
	// frames carry no platform metadata.
	mux.HandleFunc("GET /agents/{id}/acp", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		conn, err := connectUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Keepalive: set up BEFORE OpenChatForAgent — the CLI spawn can take
		// seconds, and an idle socket during spawn is the same timeout risk.
		_ = conn.SetReadDeadline(time.Now().Add(chatPongWait))
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(chatPongWait))
			return nil
		})
		pingDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(chatPingPeriod)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(chatWriteWait)); err != nil {
						return
					}
				case <-pingDone:
					return
				}
			}
		}()
		defer close(pingDone)
		chatID, err := s.d.OpenChatForAgent(r.Context(), agentID)
		if err != nil {
			// The machine is offline or the agent is unknown — the ACP
			// handshake never started (no pending JSON-RPC request to reject).
			// Just close the socket; the web's connect() promise rejects.
			logging.Infof("chat: agent %s: %v", agentID, err)
			_ = conn.Close()
			return
		}
		entry := s.d.BindChatSink(chatID,
			func(b []byte) error { return conn.WriteMessage(websocket.TextMessage, b) },
			func() {
				// Send a WS close frame BEFORE closing the TCP socket.
				// conn.Close() alone sends a TCP FIN — intermediaries
				// (ELB, nginx) may buffer it, so the browser doesn't
				// learn the connection died until their idle timeout
				// (300s) fires. A WS close frame is protocol-level data
				// that MUST be forwarded, triggering the browser's
				// onclose immediately → prompt reconnect.
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, "machine offline"),
					time.Now().Add(chatWriteWait))
				_ = conn.Close()
			})
		defer s.d.CloseChat(chatID, entry)
		// Web → machine: one ACP frame per text message.
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				// The error names who killed the socket: a close frame
				// (1000/1001) = the browser closed deliberately (panel
				// unmounted); EOF / connection reset = the wire dropped it
				// (the Next dev proxy sits in between); a protocol error =
				// the browser rejected a frame. One line decides the fix.
				logging.Infof("chat: %s web read: %v", chatID, err)
				return
			}
			if err := s.d.ChatWrite(chatID, msg); err != nil {
				// The machine link is dead (e.g. machine disconnected mid-chat).
				// Reply with a standard JSON-RPC error to the web's pending
				// request so its promise rejects with the real cause instead of
				// hanging until the socket closes. Parse the id from the frame
				// the web sent — it is a JSON-RPC request (session/prompt, etc.).
				if id := extractJSONRPCID(msg); id != nil {
					errMsg, _ := json.Marshal(map[string]any{
						"jsonrpc": "2.0",
						"id":      id,
						"error":   map[string]any{"code": -32000, "message": err.Error()},
					})
					_ = conn.WriteMessage(websocket.TextMessage, errMsg)
				}
				logging.Infof("chat: %s web→machine: %v", chatID, err)
				return
			}
		}
	})

	// The agent rpc (CLI 分支 Phase 2): one-shot JSON-RPC over WS — the
	// agent's collaboration commands dial this per invocation, carrying the
	// per-run token in params. The token is the ONLY identity: it resolves
	// to the run's (goal, agent, role), and self-reported ids are ignored.
	mux.HandleFunc("GET /rpc", func(w http.ResponseWriter, r *http.Request) {
		conn, err := connectUpgrader.Upgrade(w, r, nil)
		if err != nil {
			logging.Infof("rpc: upgrade: %v", err)
			return
		}
		peer := link.NewPeer(conn)
		agentSvc := service.NewAgentService(s.st, s.bus)

		resolve := func(raw json.RawMessage) (*service.RunIdentity, *link.RPCError) {
			var t link.RPCToken
			_ = json.Unmarshal(raw, &t)
			id, err := s.runSvc.ResolveRunToken(r.Context(), t.Token)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeAuthDenied, Message: err.Error()}
			}
			return id, nil
		}

		peer.Handle(link.MethodGoalComment, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.GoalCommentParams
			if err := json.Unmarshal(raw, &p); err != nil || strings.TrimSpace(p.Text) == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "text is required"}
			}
			c, err := s.commentSvc.Create(ctx, service.Comment{
				GoalID: id.GoalID, AuthorType: "agent", AuthorID: id.AgentID,
				Content: p.Text, ParentID: p.ParentID, RunID: id.RunID,
				AskHuman: p.AskHuman,
			})
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return map[string]any{"id": c.ID}, nil
		})

		peer.Handle(link.MethodGoalComments, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.GoalCommentsParams
			_ = json.Unmarshal(raw, &p)
			if p.Limit <= 0 {
				p.Limit = 50
			}
			out, err := s.commentSvc.ListAfter(ctx, id.GoalID, p.After, p.Limit)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return out, nil
		})

		// goal.assign — the agent handoff path. The HTTP /goals/{id}/assign
		// surface carries no agent identity (single-user, the human's action),
		// so an agent's `goal assign` via HTTP arrived with actor="" → defaulted
		// to "human" → the service-layer owner check (决策 5-6) was skipped and
		// ANY agent could grab ANY goal. Over /rpc the run token resolves the
		// actor, and Assign enforces "only the current owner can hand off".
		peer.Handle(link.MethodGoalAssign, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.GoalAssignParams
			if err := json.Unmarshal(raw, &p); err != nil || p.AssigneeType == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "assignee_type is required"}
			}
			if p.AssigneeType != "human" && p.AssigneeID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "assignee_id is required for agent/squad"}
			}
			out, err := s.goalSvc.Assign(ctx, id.GoalID, p.AssigneeType, p.AssigneeID, p.HandoffNote, "agent", id.AgentID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return out, nil
		})

		peer.Handle(link.MethodGoalList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			all, err := s.goalSvc.List(ctx)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return all, nil
		})

		peer.Handle(link.MethodAgentList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			all, err := agentSvc.List(ctx)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return all, nil
		})

		peer.Handle(link.MethodSquadList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			all, err := s.squadSvc.List(ctx)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return all, nil
		})

		peer.Handle(link.MethodSubGoalList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			all, err := s.goalSvc.ListSubGoals(ctx, id.GoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return all, nil
		})

		peer.Handle(link.MethodSubGoalCreate, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalCreateParams
			if err := json.Unmarshal(raw, &p); err != nil || p.Title == "" || p.AssigneeID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "title and assignee_id are required"}
			}
			sg, err := s.goalSvc.CreateSubGoal(ctx, id.GoalID, p.Title, p.Description, p.AssigneeID, p.VerifierID, "agent", id.AgentID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return sg, nil
		})

		peer.Handle(link.MethodSubGoalCancel, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalCancelParams
			if err := json.Unmarshal(raw, &p); err != nil || p.SubGoalID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "sub_goal_id is required"}
			}
			sg, err := s.goalSvc.CancelSubGoal(ctx, p.SubGoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return sg, nil
		})

		peer.Handle(link.MethodSubGoalRetry, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalRetryParams
			if err := json.Unmarshal(raw, &p); err != nil || p.SubGoalID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "sub_goal_id is required"}
			}
			sg, err := s.goalSvc.RetrySubGoal(ctx, p.SubGoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return sg, nil
		})

		peer.Handle(link.MethodSubGoalVerify, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalVerifyParams
			if err := json.Unmarshal(raw, &p); err != nil || p.SubGoalID == "" || (p.Verdict != "passed" && p.Verdict != "rejected") {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "sub_goal_id and verdict (passed|rejected) are required"}
			}
			// VerifySubGoal enforces that the CALLING run is the verifier run.
			if err := s.goalSvc.VerifySubGoal(ctx, id.RunID, p.Verdict, p.Summary, p.Evidence); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return map[string]any{"ok": true}, nil
		})

		peer.Handle(link.MethodSubGoalGet, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalGetParams
			if err := json.Unmarshal(raw, &p); err != nil || p.SubGoalID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "sub_goal_id is required"}
			}
			sg, err := s.goalSvc.GetSubGoal(ctx, p.SubGoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return sg, nil
		})

		peer.Handle(link.MethodSubGoalVerifications, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			var p link.SubGoalVerificationsParams
			if err := json.Unmarshal(raw, &p); err != nil || p.SubGoalID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "sub_goal_id is required"}
			}
			out, err := s.goalSvc.ListVerificationResults(ctx, p.SubGoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return out, nil
		})

		peer.Handle(link.MethodChangeList, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			out, err := s.goalSvc.ListChangeDetails(ctx, id.GoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return out, nil
		})

		peer.Handle(link.MethodChangeIntegrateBegin, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			var p link.ChangeIntegrateBeginParams
			if err := json.Unmarshal(raw, &p); err != nil || p.ChangeID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "change_id is required"}
			}
			ch, err := s.goalSvc.GetChange(ctx, p.ChangeID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			if ch.GoalID != id.GoalID {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "the change does not belong to this goal"}
			}
			if ch.Status == "integrated" {
				return link.ChangeIntegrateResult{OK: true, Status: "integrated", Note: "already integrated"}, nil
			}
			if ch.Status == "conflict" {
				return link.ChangeIntegrateResult{OK: false, Status: "conflict", Note: "the assignee is reworking this change — wait for the new Revision before integrating"}, nil
			}
			if err := s.goalSvc.MarkChangeIntegrating(ctx, ch.ID); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return link.ChangeIntegrateResult{OK: true, Status: "integrating", HeadRef: ch.HeadRef}, nil
		})

		peer.Handle(link.MethodChangeIntegrateFinish, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			var p link.ChangeIntegrateFinishParams
			if err := json.Unmarshal(raw, &p); err != nil || p.ChangeID == "" {
				return nil, &link.RPCError{Code: link.CodeInvalidParams, Message: "change_id is required"}
			}
			if err := s.goalSvc.MarkChangeIntegrated(ctx, p.ChangeID, p.OK); err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			if p.OK {
				return link.ChangeIntegrateResult{OK: true, Status: "integrated"}, nil
			}
			return link.ChangeIntegrateResult{OK: false, Status: "conflict", Note: "change marked conflicted — the sub-goal assignee has been woken to rework", Output: p.Output}, nil
		})

		peer.Handle(link.MethodGoalWait, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			id, rpcErr := resolve(raw)
			if rpcErr != nil {
				return nil, rpcErr
			}
			states, err := s.goalSvc.WaitChildren(ctx, id.GoalID)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			return states, nil
		})

		peer.Handle(link.MethodGoalStats, func(ctx context.Context, raw json.RawMessage) (any, *link.RPCError) {
			if _, rpcErr := resolve(raw); rpcErr != nil {
				return nil, rpcErr
			}
			all, err := s.goalSvc.List(ctx)
			if err != nil {
				return nil, &link.RPCError{Code: link.CodeInternal, Message: err.Error()}
			}
			counts := map[string]int{}
			for _, g := range all {
				counts[g.Status]++
			}
			return map[string]any{"goal_total": len(all), "goal_by_status": counts}, nil
		})

		peer.Wait()
	})

	// Stale sweep: machines that stopped heartbeating flip offline.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ids, err := machineSvc.MarkStale(ctx, time.Now().Add(-90*time.Second))
				if err != nil {
					logging.Infof("connect: machine stale sweep: %v", err)
				} else {
					for _, id := range ids {
						s.bus.Publish(ctx, events.Event{Topic: "machine:offline", Payload: map[string]string{"machine_id": id}})
					}
					if len(ids) > 0 {
						logging.Infof("connect: %d machine(s) offline", len(ids))
					}
				}
			}
		}
	}()
	// Human-initiated run stop (决策 4-12): terminates the running run (no
	// attempt consumed, no auto-retry), goal state untouched — recovery is
	// the human's call.
	mux.HandleFunc("POST /goals/{goalID}/runs/{runID}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := s.d.StopRun(r.PathValue("goalID"), r.PathValue("runID")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Continue a paused goal (run stopped by the human, goal still active):
	// enqueue a fresh owner run with a pause-resume wake note so the owner
	// picks up its worktree state. No-op for review/terminal/human goals.
	mux.HandleFunc("POST /goals/{goalID}/continue", func(w http.ResponseWriter, r *http.Request) {
		run, err := s.d.ContinueGoal(r.PathValue("goalID"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if run == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(run)
	})
	// The domain git check tests UNSAVED form values (create/edit dialogs),
	// so it carries the config in the body rather than a domain id.
	mux.HandleFunc("POST /domains/test", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			GitURL         string `json:"git_url"`
			DefaultBranch  string `json:"default_branch"`
			GitCredentials string `json:"git_credentials"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.d.TestDomainGit(r.Context(), body.GitURL, body.DefaultBranch, body.GitCredentials))
	})
	h.Mount(mux)

	go s.hub.Run(ctx)

	// Security gate: a non-loopback listen address (0.0.0.0, :7373, a LAN
	// IP) exposes the platform to the network. The single-user no-auth default
	// is ONLY safe on loopback — any other address REQUIRES a worker_token
	// (app_settings platform.worker_token) and every request is gated on it.
	// The token is checked as a query param (?token=…) or Authorization
	// header (Bearer …). The frontend (web/) assumes same-origin deployment
	// (Next.js rewrite / reverse proxy to the daemon) and does NOT pass a
	// token — non-loopback direct browser access requires a proxy that
	// injects the token, or a future frontend change to pass it.
	handler := corsMiddleware(mux)
	if !isLoopbackAddr(addr) {
		want, err := settingsSvc.Get(ctx, "platform.worker_token")
		if err != nil || strings.TrimSpace(want) == "" {
			return fmt.Errorf("refusing to listen on %s without platform.worker_token: a non-loopback address requires authentication (set the token in app_settings or bind to 127.0.0.1)", addr)
		}
		logging.Infof("server: non-loopback listen %s — auth required (worker_token)", addr)
		handler = authMiddleware(settingsSvc, handler)
	}

	srv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	logging.Infof("server: listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// extractJSONRPCID parses the "id" field from a JSON-RPC 2.0 request frame.
// Used when ChatWrite fails (machine dead) to reply with a standard error
// response matching the web's pending request id. Returns nil if the frame
// has no id (a notification — no response expected, nothing to reject).
func extractJSONRPCID(frame []byte) json.RawMessage {
	var probe struct {
		ID json.RawMessage `json:"id,omitempty"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		return nil
	}
	if len(probe.ID) == 0 || string(probe.ID) == "null" {
		return nil
	}
	return probe.ID
}

// corsMiddleware allows the Next.js dev server (localhost:3000) to call the
// API and WS endpoint cross-origin. Single-user local use; no credentialed
// requests, no origin allow-list beyond the dev port.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackAddr reports whether the listen address binds to a loopback
// interface only (127.0.0.1, localhost, ::1). A bare ":port" (all
// interfaces), "0.0.0.0:port", or a LAN IP all return false — those expose
// the platform to the network and require authentication.
func isLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" {
		return false // ":port" → all interfaces
	}
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "localhost", "127.0.0.1", "::1", "0:0:0:0:0:0:0:1":
		return true
	}
	// 127.x.x.x range
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// authMiddleware gates browser/HTTP requests on a shared secret
// (worker_token, read from app_settings on EVERY request — so rotation
// takes effect immediately). The token is accepted as a query parameter
// (?token=…) or an Authorization header (Bearer …). fail-closed: DB error
// or empty token → reject (not fall open).
//
// /rpc is EXEMPT: it is the agent/link channel with its own application-layer
// auth (per-run token in JSON-RPC params via ResolveRunToken). The executor
// env injects AGENTWORK_SERVER_URL but not worker_token — agent CLI's rpcCall
// dials /rpc with no HTTP-layer token. /connect is NOT exempt: the machine
// connect CLI passes ?token=worker_token explicitly (connect.go:431).
func authMiddleware(settingsSvc *service.SettingsService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Agent RPC channel: auth at the application layer, not here.
		if r.URL.Path == "/rpc" {
			next.ServeHTTP(w, r)
			return
		}
		// CORS preflight: browsers send OPTIONS without token (preflight
		// cannot carry custom headers/query). Let corsMiddleware handle it.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		want, err := settingsSvc.Get(r.Context(), "platform.worker_token")
		if err != nil {
			// DB故障时 fail-closed：拒绝请求，不降级为无认证。
			http.Error(w, "auth unavailable", http.StatusServiceUnavailable)
			return
		}
		if want == "" {
			// Token was removed at runtime — fail-closed (the startup guard
			// refused to listen without a token on non-loopback; clearing it
			// at runtime must not open the door).
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := r.URL.Query().Get("token")
		if got == "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				got = strings.TrimPrefix(h, "Bearer ")
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
