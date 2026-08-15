// Package daemon dispatches queued runs to agent runtimes. MVP uses the
// per-run subprocess model: each run opens a fresh transport connection via
// runtime.Open, hands it to the protocol Backend for one Prompt, and tears
// it down when the turn ends. There is no long-lived per-agent server.
//
// Concurrency is per-agent: each agent has a worker goroutine with a
// semaphore sized to agent.max_concurrent, so one agent's runs run in parallel
// up to its limit while different agents are independent.
//
// State authority is NOT here: when a run reaches a terminal status the daemon
// calls RunService.Finish, which stamps the run row then hands the outcome to
// GoalService.ReconcileOnRunEnd — the sole place that advances goal.status.
// See DESIGN.md
package daemon

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eushing/agentwork/internal/logging"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/issue"
	"github.com/eushing/agentwork/internal/mcp"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/runtime"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
	"github.com/google/uuid"
)

// dispatchTickInterval is how often the daemon claims queued runs. Claims are
// per-agent (only within the set of agents with free worker slots), so this
// bounds perceived latency without hot-looping.
const dispatchTickInterval = 500 * time.Millisecond

// scheduleTickInterval is how often the daemon scans schedule for due firings.
const scheduleTickInterval = 5 * time.Second

// worktreeCleanupInterval is how often the daemon sweeps expired goal
// worktrees (M1: every 6h).
const worktreeCleanupInterval = 6 * time.Hour

// digestTickInterval is how often the daemon checks whether the daily digest
// is due (M3: once a minute; the digest fires at most once per day).
const digestTickInterval = time.Minute

// digestDefaultTime is the daily digest time (HH:MM, local) when the owner
// has not configured notify.digest_time (DESIGN.md §11 M3).
const digestDefaultTime = "09:00"

// issuePollInterval is the default issue-trigger latency (M4-B: the trigger
// is a poll — no public webhook on a single-user machine). Default 30s so an
// issue becomes a goal quickly; the owner can raise it via app_settings
// (platform.issue_poll_interval, seconds) for many tracked repos + rate
// limits. issuePollMinInterval is the tick floor (rate-limit protection).
const (
	issuePollInterval    = 30 * time.Second
	issuePollMinInterval = 15 * time.Second
)

// worktreeRetentionDays is how long a terminal goal's worktree is kept after
// its last run (DESIGN.md §13 — M1 value: 7 days; kept for review/debug).
const worktreeRetentionDays = 7

// workerQueueDepth bounds how many queued runs one agent's worker holds before
// back-pressuring the dispatcher.
const workerQueueDepth = 64

// defaultListenAddr is used when no addr is configured.
const defaultListenAddr = ":7373"

// defaultMaxRunDurationSeconds is the platform's default per-run budget when
// no domain configures one (DESIGN.md §4: 2h).
const defaultMaxRunDurationSeconds = 7200

// runawayScanInterval is how often the runaway reaper scans for runs whose
// process outlived its budget (P1, 决策 6-15⑦) — a cheap scan, minute-scale.
const runawayScanInterval = time.Minute

// runawayGrace is the reaper's tolerance past the run's own budget: the
// promptCtx timeout (maxRunDuration) should have ended the run at 1× — a run
// still 'running' past budget+grace means the context-cancellation chain
// broke and only the DB-level reaper can free the owner single-flight.
const runawayGrace = 5 * time.Minute

// idleWindow is the no-activity budget after which the idle watchdog cancels
// a hung turn. An agent that emits nothing for this long is presumed stuck.
const idleWindow = 2 * time.Minute

// idleToolWindow extends the budget while a tool is in flight (a long-running
// tool is legitimately silent between tool_use and tool_result).
const idleToolWindow = 10 * time.Minute

// maxAttempts bounds per-run retries; mirrored from service. A run that fails
// this many times leaves the goal failed for human inspection.
const maxAttempts = 3

// Daemon owns per-agent workers and the run dispatch loop.
type Daemon struct {
	st         *store.Store
	bus        *events.Bus
	addr       string
	protoReg   *proto.Registry
	goalSvc    *service.GoalService
	runSvc     *service.RunService
	commentSvc *service.CommentService
	agentSvc   *service.AgentService
	squadSvc   *service.SquadService
	schedSvc   *service.ScheduleService
	im         *notify.Connector // M3: daily digest + intake replies (the notifier
	// is born when the long connection connects; fetch it live)
	qs            notify.QueryStore     // M3: digest aggregation (may be nil)
	issuePoll     *issue.Poller         // M4-B: open issues → goals
	issueCloser   *issue.Closer         // M4-B: delivered goal → close its issue
	intakeSvc     *notify.IntakeService // M4-B: multi-domain clarification draft store
	lastIssuePoll time.Time             // last poll time (the interval is configurable)

	mu          sync.Mutex
	workers     map[string]*agentWorker // agentID → per-agent scheduler
	domainLocks map[string]*domainLock  // per-domain git lock (fetch + deliver)
	msgBuffers  map[string]*msgBuffer   // runID → aggregated text row (persistEvent)
	// runCancels maps runID → the run's prompt cancel (registered by runTask,
	// used to terminate a running run when its goal changes hands — a handed
	// off agent that keeps running deadlocks the new owner's queued run behind
	// per-goal serialization; the platform cuts the old run instead of waiting
	// on the agent's good behavior). Guarded by mu.
	runCancels map[string]context.CancelFunc
	// runCancelReasons maps runID → why the run was cancelled ("idle
	// watchdog" / "handoff" / "approval"). The cancelled branch reads it ONCE
	// to stamp the real reason into result_summary — without it every cancel
	// (maxRunDuration deadline, handoff, approval) would be mislabeled "idle
	// watchdog", which both lies in the feed and poisons the convergence
	// counter (a handoff cancel must not count as a watchdog stall). Guarded
	// by mu.
	runCancelReasons map[string]string
	// reviewReadyTimers / reviewReadyFired implement the reviewer-first
	// approval card (Option A): the human's card waits until this window's
	// review runs are terminal (or the fallback elapses / no reviewer
	// exists). The timer covers a hung reviewer; the fired flag dedupes the
	// card to one per window. Guarded by mu.
	reviewReadyTimers map[string]*time.Timer
	reviewReadyFired  map[string]bool
	// mcpExecs maps runID → the run's workspace MCP executor (DESIGN.md
	// 决策 4-8: agents that don't delegate tools to client RPCs get the
	// workspace through an MCP server advertised at session/new). Guarded
	// by mu.
	mcpExecs map[string]*mcp.Executor
	stopped  bool
	ctx      context.Context
}

// agentWorker schedules one agent's runs with a concurrency semaphore.
type agentWorker struct {
	agentID   string
	sem       chan struct{} // capacity = max_concurrent
	queue     chan *service.ClaimedRow
	ctx       context.Context
	cancel    context.CancelFunc
	daemonCtx context.Context
	run       func(context.Context, *service.ClaimedRow)
	maxConc   int
}

// New wires the daemon. im + qs are the M3 IM surfaces: the connector is the
// owner of the notifier (born when the long connection connects), qs feeds
// the daily digest and intake queries. Both may be nil (notify not wired).
func New(st *store.Store, bus *events.Bus, addr string, protoReg *proto.Registry, goalSvc *service.GoalService, runSvc *service.RunService, commentSvc *service.CommentService, agentSvc *service.AgentService, squadSvc *service.SquadService, schedSvc *service.ScheduleService, im *notify.Connector, qs notify.QueryStore, intakeSvc *notify.IntakeService) *Daemon {
	d := &Daemon{
		st: st, bus: bus, addr: addr,
		protoReg: protoReg, goalSvc: goalSvc, runSvc: runSvc, commentSvc: commentSvc, agentSvc: agentSvc,
		squadSvc: squadSvc, schedSvc: schedSvc,
		im:                im,
		qs:                qs,
		intakeSvc:         intakeSvc,
		issuePoll:         issue.NewPoller(st, goalSvc, runSvc),
		issueCloser:       issue.NewCloser(st),
		workers:           make(map[string]*agentWorker),
		msgBuffers:        make(map[string]*msgBuffer),
		mcpExecs:          make(map[string]*mcp.Executor),
		runCancels:        make(map[string]context.CancelFunc),
		runCancelReasons:  make(map[string]string),
		reviewReadyTimers: make(map[string]*time.Timer),
		reviewReadyFired:  make(map[string]bool),
	}
	bus.Subscribe("agent:created", d.onAgentCreated)
	bus.Subscribe("agent:deleted", d.onAgentDeleted)
	bus.Subscribe("domain:deleted", d.onDomainDeleted)
	// run.terminal → ReconcileGoal (决策 6-4): the latch's second edge — any
	// terminal run re-evaluates whether the goal needs its owner. The event is
	// only a wakeup hint; ReconcileGoal recomputes from DB state.
	bus.Subscribe("run.terminal", d.onRunTerminal)
	// sub-goal state changes → ReconcileGoal (决策 6-4): the latch's first
	// edge — a verified sub-goal (change ready) or a failed one (recovery)
	// re-evaluates owner attention.
	bus.Subscribe("sub_goal.verified", d.onSubGoalStateChanged)
	bus.Subscribe("sub_goal.failed", d.onSubGoalStateChanged)
	// change state changes → ReconcileGoal (决策 6-4, the Latch hard rule:
	// ANY state change that can alter the attention judgment must reconcile).
	// change.ready re-arms attention the moment a change materializes;
	// change.integrated/conflict clear/re-arm it the moment the owner's
	// integrate_change lands — without these edges the attention badge waits
	// for the next run.terminal to catch up (the E2E watcher saw a ~40s stale
	// "integration" after a successful integration).
	bus.Subscribe("change.ready", d.onSubGoalStateChanged)
	bus.Subscribe("change.integrated", d.onSubGoalStateChanged)
	bus.Subscribe("change.conflict", d.onSubGoalStateChanged)
	// A cancelled sub-goal's running run must stop (owner management, 决策
	// 6-1) — same stop mechanism as goal cancel.
	bus.Subscribe("sub_goal.cancelled", d.onSubGoalCancelled)
	bus.Subscribe("goal:approved", d.onGoalApproved)
	// Handoff (human reassign or `goal assign`): the goal's new owner takes
	// over — the previous owner's running run must NOT keep going (it would
	// deadlock the new run behind per-goal serialization: an agent that
	// handed off believes its turn is over, so nothing stops its run, and the
	// new owner's run waits queued forever). The platform cuts the old run.
	bus.Subscribe("goal:assigned", d.onGoalAssigned)
	// Squad review checkpoint: a squad-owned goal parking in review triggers
	// the squad's role=reviewer members (the squad's own rule, enforced by
	// the platform — not by the leader's discretion).
	bus.Subscribe("goal:reviewing", d.onGoalReviewing)
	// Cancel now terminates the goal's running run too (决策 4-12): a
	// cancelled goal must not keep an agent burning compute on work that is
	// already decided dead. Same stop mechanism as StopRun.
	bus.Subscribe("goal:finished", d.onGoalFinished)
	// Delete likewise: the goal:deleted payload carries the running run ids
	// captured before the cascade removed their rows (the DB can no longer
	// answer the query by the time this handler fires).
	bus.Subscribe("goal:deleted", d.onGoalDeleted)
	// M4-B: a delivered issue-sourced goal closes its GitHub issue (the
	// work is merged — the issue is done). The fix commits (structured, from
	// the deliver) travel into the close comment so the issue records WHAT
	// was done with clickable links.
	bus.Subscribe("goal:delivered", func(_ context.Context, e events.Event) {
		m, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		goalID, _ := m["goal_id"].(string)
		note, _ := m["note"].(string)
		var commits []string
		if raw, ok := m["commits"].([]string); ok {
			commits = raw
		}
		if goalID != "" {
			d.issueCloser.OnDelivered(context.Background(), goalID, note, commits)
		}
	})
	return d
}

// Run starts the dispatch loop. Blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	d.recoverWorkers(ctx)
	d.sweepDeliverWorktrees(ctx)
	d.sweepRunWorktrees(ctx)
	if n, err := d.runSvc.RecoverStuckRunning(ctx); err != nil {
		logging.Errorf("daemon: recover stuck running: %v", err)
	} else if n > 0 {
		logging.Infof("daemon: recovered %d stuck running run(s)", n)
	}
	// P0-1 (决策 6-11): terminal runs whose reconcile never happened (a crash
	// between the terminal UPDATE and the reconcile transaction) replay their
	// reconcile here — every transition is conditional, so the replay is
	// idempotent. This closes the durable-execution window.
	if n, err := d.runSvc.ReconcilePendingTerminal(ctx); err != nil {
		logging.Errorf("daemon: reconcile pending terminal runs: %v", err)
	} else if n > 0 {
		logging.Infof("daemon: replayed reconcile for %d unreconciled terminal run(s)", n)
	}
	// P0-3 (决策 6-13): latch events lost in a crash (their transactions
	// committed but the publish never ran) are re-armed from DB truth —
	// ReconcileGoal is idempotent, so re-deriving every active goal's
	// attention re-spawns exactly what the state demands.
	if n, err := d.goalSvc.ReconcileAllActive(ctx); err != nil {
		logging.Errorf("daemon: reconcile all active goals: %v", err)
	} else if n > 0 {
		logging.Infof("daemon: reconciled %d active goal(s)", n)
	}
	// Option B recovery (Event≠Truth on the review window): the ready
	// trigger, the fallback timers and the fired flags are all in-memory — a
	// crash between a window's park and its ready publish leaves the human's
	// card permanently unpatched. Re-derive from DB truth: every goal still
	// in review re-opens its window (ready fires immediately when no review
	// runs are pending; the fallback timer re-arms otherwise).
	if n, err := d.recoverReviewWindows(ctx); err != nil {
		logging.Errorf("daemon: recover review windows: %v", err)
	} else if n > 0 {
		logging.Infof("daemon: recovered %d review window(s)", n)
	}
	// Decision 2-9, trigger side: an approve followed by a crash leaves the
	// goal in review with the approve recorded and no deliver — re-run the
	// deliver (its merge/push idempotency makes the replay safe).
	if n, err := d.recoverPendingDelivers(ctx); err != nil {
		logging.Errorf("daemon: recover pending delivers: %v", err)
	} else if n > 0 {
		logging.Infof("daemon: re-delivering %d goal(s) whose approve never delivered", n)
	}
	dispatchTick := time.NewTicker(dispatchTickInterval)
	scheduleTick := time.NewTicker(scheduleTickInterval)
	cleanupTick := time.NewTicker(worktreeCleanupInterval)
	digestTick := time.NewTicker(digestTickInterval)
	issueTick := time.NewTicker(issuePollInterval)
	runawayTick := time.NewTicker(runawayScanInterval)
	d.runLogTailer(ctx)
	defer dispatchTick.Stop()
	defer scheduleTick.Stop()
	defer cleanupTick.Stop()
	defer digestTick.Stop()
	defer issueTick.Stop()
	defer runawayTick.Stop()
	for {
		select {
		case <-ctx.Done():
			logging.Infof("daemon: shutting down")
			d.stopAll()
			return ctx.Err()
		case <-dispatchTick.C:
			d.dispatchOnce(ctx)
		case <-scheduleTick.C:
			d.dispatchSchedules(ctx)
		case <-cleanupTick.C:
			d.cleanupWorktrees(ctx)
		case <-digestTick.C:
			d.dispatchDigest(ctx)
		case <-issueTick.C:
			d.dispatchIssues(ctx)
		case <-runawayTick.C:
			d.reapRunawayRuns(ctx)
		}
	}
}

// runLogTailer streams new log lines to the bus (log:line) — the live
// pane of the Web logs panel. A polling tailer: the writer is a plain file
// and inotify is overkill at 500ms.
func (d *Daemon) runLogTailer(ctx context.Context) {
	path := logging.DefaultPath()
	var off int64
	go func() {
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				f, err := os.Open(path)
				if err != nil {
					continue // not created yet / rolled away
				}
				st, err := f.Stat()
				if err != nil {
					f.Close()
					continue
				}
				if st.Size() < off {
					off = 0 // rolled — restart from the new file's start
				}
				if _, err := f.Seek(off, 0); err == nil && st.Size() > off {
					sc := bufio.NewScanner(f)
					sc.Buffer(make([]byte, 64*1024), 1024*1024)
					for sc.Scan() {
						l := logging.ParseLine(sc.Text())
						d.bus.Publish(ctx, events.Event{Topic: "log:line", Payload: map[string]any{
							"ts":    l.TS.Format(time.RFC3339),
							"level": l.Level,
							"text":  l.Text,
						}})
					}
					off = st.Size()
				}
				f.Close()
			}
		}
	}()
}

// dispatchIssues polls tracked repos for new open issues and turns them into
// goals (M4-B). The interval bounds how quickly a new issue reaches the
// queue — no public webhook needed on a single-user machine. The ticker
// fires at the minimum interval; the effective interval (default 30s,
// app_settings platform.issue_poll_interval in seconds, floor 15s) gates the
// actual poll.
func (d *Daemon) dispatchIssues(ctx context.Context) {
	interval := issuePollInterval
	var raw string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='platform.issue_poll_interval'`).Scan(&raw); err == nil && raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil && sec >= int(issuePollMinInterval/time.Second) {
			interval = time.Duration(sec) * time.Second
		}
	}
	d.mu.Lock()
	now := time.Now()
	if now.Sub(d.lastIssuePoll) < interval {
		d.mu.Unlock()
		return
	}
	d.lastIssuePoll = now
	d.mu.Unlock()

	n, err := d.issuePoll.Poll(ctx)
	if err != nil {
		logging.Errorf("daemon: issue poll: %v", err)
		return
	}
	if n > 0 {
		logging.Infof("daemon: issue poll created %d goal(s)", n)
	}
}

// ── daily digest (M3-3) ──

// dispatchDigest fires the daily summary card once per day, at the
// configured digest time (app_settings notify.digest_time, default 09:00).
// The already-sent marker (notify.digest_last_sent, date) makes the fire
// idempotent across daemon restarts.
func (d *Daemon) dispatchDigest(ctx context.Context) {
	notifier := d.imNotifier()
	if notifier == nil || d.qs == nil {
		return
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	var last string
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='notify.digest_last_sent'`).Scan(&last)
	if last == today {
		return
	}
	hhmm := digestDefaultTime
	// The digest time lives in the platform.m3 settings blob (M3 settings
	// page: 设置 → 平台设置).
	var blob string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='platform.m3'`).Scan(&blob); err == nil && blob != "" {
		var st struct {
			DigestTime string `json:"digest_time"`
		}
		if json.Unmarshal([]byte(blob), &st) == nil && st.DigestTime != "" {
			hhmm = st.DigestTime
		}
	}
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		logging.Infof("daemon: digest time %q: %v", hhmm, err)
		return
	}
	digestAt := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if now.Before(digestAt) {
		return // not due yet today
	}
	// Window: yesterday 00:00 → today 00:00 (the digest is a morning summary;
	// a goal finishing this morning belongs to TOMORROW's digest).
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	card, err := notify.BuildDigestCard(ctx, d.qs, dayStart.Add(-24*time.Hour), dayStart, now)
	if err != nil {
		logging.Errorf("daemon: digest build: %v", err)
		return
	}
	if _, err := notifier.SendCard(card); err != nil {
		logging.Errorf("daemon: digest send: %v", err)
		return
	}
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO app_settings (key,value,updated_at) VALUES ('notify.digest_last_sent',?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		today, nowStr()); err != nil {
		logging.Errorf("daemon: digest marker: %v", err)
	}
	logging.Infof("daemon: daily digest sent (%s)", today)
}

// Poller exposes the issue poller for the server's webhook wiring (M4-B:
// both triggers share the same create-goal path).
func (d *Daemon) Poller() *issue.Poller { return d.issuePoll }

// MCPExecutor returns the workspace MCP executor for a run (nil if the run
// is not active). The server's /mcp/{runID} route resolves through this.
func (d *Daemon) MCPExecutor(runID string) *mcp.Executor {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mcpExecs[runID]
}

// imNotifier returns the live milestone pusher (nil before the long
// connection is up — digest and intake replies then no-op).
func (d *Daemon) imNotifier() *notify.Notifier {
	if d.im == nil {
		return nil
	}
	return d.im.Notifier()
}

// recoverWorkers rebuilds per-agent workers for every agent in the DB —
// otherwise a daemon restart has no workers for pre-existing agents.
func (d *Daemon) recoverWorkers(ctx context.Context) {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT id, max_concurrent FROM agent`)
	if err != nil {
		logging.Errorf("daemon: recover workers: %v", err)
		return
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var id string
		var maxConcurrent int
		if err := rows.Scan(&id, &maxConcurrent); err != nil {
			continue
		}
		d.ensureWorker(id, maxConcurrent)
		n++
	}
	if n > 0 {
		logging.Infof("daemon: recovered %d agent worker(s)", n)
	}
}

// ── agent worker lifecycle ──

func (d *Daemon) onAgentCreated(ctx context.Context, e events.Event) {
	a, ok := e.Payload.(service.Agent)
	if !ok {
		return
	}
	d.ensureWorker(a.ID, a.MaxConcurrent)
	logging.Infof("daemon: worker ready for agent %s", a.ID)
}

func (d *Daemon) onAgentDeleted(ctx context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]string)
	if !ok {
		return
	}
	id := m["id"]
	d.mu.Lock()
	w, ok := d.workers[id]
	if ok {
		delete(d.workers, id)
	}
	d.mu.Unlock()
	if w != nil {
		w.cancel() // stop the drain; in-flight runs finish on daemonCtx
	}
	logging.Infof("daemon: worker removed for agent %s", id)
}

// onRunTerminal funnels a terminal run into the Coordinator (决策 6-4): the
// event is a wakeup hint only — ReconcileGoal recomputes the authoritative
// state in its own transaction. DB work uses d.ctx (never the publisher's
// HTTP-scoped ctx).
func (d *Daemon) onRunTerminal(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	goalID, _ := m["goal_id"].(string)
	if goalID == "" {
		return
	}
	if err := d.goalSvc.ReconcileGoal(d.ctx, goalID); err != nil {
		logging.Infof("daemon: reconcile goal %s: %v", goalID, err)
	}
	// Option A (reviewer-first approval card): the last review run going
	// terminal closes the window — fire the human's card with the opinions
	// now in place.
	d.maybeFireReviewReady(d.ctx, goalID)
}

// onSubGoalStateChanged funnels sub-goal state changes into the Coordinator
// (决策 6-4): same wakeup-hint semantics as onRunTerminal.
func (d *Daemon) onSubGoalStateChanged(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	goalID, _ := m["goal_id"].(string)
	if goalID == "" {
		return
	}
	if err := d.goalSvc.ReconcileGoal(d.ctx, goalID); err != nil {
		logging.Infof("daemon: reconcile goal %s: %v", goalID, err)
	}
}

// onGoalDeleted terminates a deleted goal's running runs. Their rows are
// already gone (the Delete cascade removed them), so the ids come from the
// event payload — the cut is pure resource reclamation: the processes must
// not keep burning compute writing into rows that no longer exist.
func (d *Daemon) onGoalDeleted(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	ids, _ := m["run_ids"].([]string)
	for _, id := range ids {
		logging.Infof("daemon: goal deleted — stopping run %s", id)
		d.cancelRun(id, "stopped")
	}
	// A scratch goal's persistent project directory dies with the row (the
	// domain identity travels in the payload — the rows are already gone).
	if t, _ := m["domain_type"].(string); t == "scratch" {
		if name, _ := m["domain_name"].(string); name != "" {
			if goalID, _ := m["goal_id"].(string); goalID != "" {
				dir := service.ScratchGoalDir(name, goalID)
				if err := os.RemoveAll(dir); err != nil {
					logging.Infof("daemon: remove scratch goal dir %s: %v", dir, err)
				}
			}
		}
	}
}

// onDomainDeleted removes a scratch domain's project root once its row is
// gone (the name travels in the payload — repo domains have nothing on disk
// to remove here; their bare repos die with the goals that used them).
func (d *Daemon) onDomainDeleted(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]string)
	if !ok {
		return
	}
	name := m["name"]
	if name == "" {
		return
	}
	// The payload does not carry the type (the row is gone) — a repo domain
	// has no scratch/<name> dir, so the remove is a cheap no-op for it.
	if err := os.RemoveAll(service.ScratchDomainRoot(name)); err != nil {
		logging.Infof("daemon: remove scratch domain root %s: %v", name, err)
	}
}

// onSubGoalCancelled terminates a cancelled sub-goal's running run (the
// service already dropped queued ones and stamped the state).
func (d *Daemon) onSubGoalCancelled(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	subGoalID, _ := m["sub_goal_id"].(string)
	if subGoalID == "" {
		return
	}
	rows, err := d.st.DB().QueryContext(d.ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND status='running'`, subGoalID)
	if err != nil {
		logging.Infof("daemon: sub-goal cancel scan %s: %v", subGoalID, err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		logging.Infof("daemon: sub-goal cancelled — stopping run %s", id)
		d.cancelRun(id, "stopped")
	}
}

// onGoalAssigned reacts to a goal changing hands (human reassign or an
// agent's `goal assign`): the goal's OLD owner's running run must be cut.
// Without this a handed-off agent keeps running — it believes its turn is
// over (the handoff was the point of its turn), so nothing stops it, and the
// new owner's run waits queued forever behind per-goal serialization: a
// deadlock. The cancel flows through the normal terminal path (promptCtx
// cancel → backend reports cancelled → reconcile discards the orphaned run;
// no attempt consumed, no auto-retry — the convergence rule only counts
// idle-watchdog cancellations). The old run's worktree leftovers stay
// attributable (a prior cancelled run), so the new owner's dirty check
// passes.
func (d *Daemon) onGoalAssigned(_ context.Context, e events.Event) {
	g, ok := e.Payload.(*service.Goal)
	if !ok {
		return
	}
	// The goal's new owner as an agent id (the only runs allowed to keep
	// running on the goal). Human owner → no agent may keep running.
	// DB work uses d.ctx, NOT the published event's ctx — the publisher is
	// often an HTTP handler whose ctx is cancelled the moment it returns
	// (see onGoalReviewing).
	ownerAgent := ""
	if g.AssigneeType == "agent" {
		ownerAgent = g.AssigneeID
	} else if g.AssigneeType == "squad" {
		_ = d.st.DB().QueryRowContext(d.ctx, `SELECT leader_id FROM squad WHERE id=?`, g.AssigneeID).Scan(&ownerAgent)
	}
	rows, err := d.st.DB().QueryContext(d.ctx,
		// 决策 6-6: handoff only terminates the OWNER-role run — sub-goal,
		// consult, review and verify runs continue (they don't write the goal
		// branch; per-run workspaces make them safe in parallel).
		`SELECT id, agent_id FROM run WHERE goal_id=? AND status='running' AND role='owner'`, g.ID)
	if err != nil {
		logging.Infof("daemon: handoff cancel scan for %s: %v", g.ID, err)
		return
	}
	// Collect rows FIRST, then act: the stamp below writes to the DB, and a
	// single-connection store (in-memory tests) deadlocks if the write runs
	// while this cursor still holds the only connection.
	type runningRun struct{ id, agentID string }
	var toCut []runningRun
	for rows.Next() {
		var rr runningRun
		if err := rows.Scan(&rr.id, &rr.agentID); err != nil {
			continue
		}
		toCut = append(toCut, rr)
	}
	rows.Close()
	for _, rr := range toCut {
		if rr.agentID == ownerAgent {
			continue // the new owner's own run (re-assign to self) keeps going
		}
		d.mu.Lock()
		cancel, ok := d.runCancels[rr.id]
		if ok {
			d.runCancelReasons[rr.id] = "handoff"
		}
		d.mu.Unlock()
		if ok {
			logging.Infof("daemon: handoff cut run %s (agent %s no longer owns goal %q (%s))", rr.id, rr.agentID, g.Title, g.ID)
			cancel()
			continue
		}
		// The claim→register window: the run was claimed (status='running')
		// but runTask hasn't registered its cancel yet — the in-memory cut
		// missed it. Stamp the run terminal in the DB (决策 6-6: status stays
		// 'cancelled', the structured cancel_reason carries the semantics);
		// runTask's post-register self-check sees status != 'running' and
		// self-cancels. The stamp is the only writer besides runTask itself,
		// so no race with finishRun.
		if _, err := d.st.DB().ExecContext(d.ctx,
			`UPDATE run SET status='cancelled', cancel_reason='handoff', finished_at=? WHERE id=? AND status='running'`, nowStr(), rr.id); err != nil {
			logging.Infof("daemon: handoff terminal stamp %s: %v", rr.id, err)
		} else {
			logging.Infof("daemon: handoff stamped run %s terminal (claim→register window, goal %q)", rr.id, g.Title)
		}
	}
}

// StopRun terminates a run on human command (决策 4-12): the run cancels
// (no attempt consumed, no auto-retry — the convergence rule only counts
// idle-watchdog stalls), the goal state is untouched, and the worktree
// keeps its state — recovery is the human's call (re-trigger / hand off /
// re-review), same as a timeout per 决策 2-6.
//
// A QUEUED run has no live process to cut — the platform stamps it terminal
// directly (the cancel registry only holds running runs). This is the D-1
// veto path: a durable intent queued during the review freeze can be
// cancelled by the human before it ever claims.
func (d *Daemon) StopRun(goalID, runID string) error {
	var g, status string
	if err := d.st.DB().QueryRowContext(d.ctx, `SELECT goal_id, status FROM run WHERE id=?`, runID).Scan(&g, &status); err != nil {
		return fmt.Errorf("stop run: %v", err)
	}
	if g != goalID {
		return fmt.Errorf("run %s does not belong to goal %s", runID, goalID)
	}
	if status == "queued" {
		res, err := d.st.DB().ExecContext(d.ctx,
			`UPDATE run SET status='cancelled', cancel_reason='stopped', finished_at=? WHERE id=? AND status='queued'`,
			nowStr(), runID)
		if err != nil {
			return fmt.Errorf("cancel queued run: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("run %s is no longer queued", runID)
		}
		logging.Infof("daemon: human stopped queued run %s", runID)
		// notify skips reason_code=stopped — the human did it, no card.
		d.bus.Publish(d.ctx, events.Event{Topic: "run:cancelled", Payload: map[string]any{
			"run_id": runID, "goal_id": goalID, "reason": "stopped", "reason_code": "stopped",
		}})
		return nil
	}
	// The claim→register window: the run was claimed but runTask has not
	// registered its cancel yet — the in-memory cut would silently no-op and
	// the human's stop click would do NOTHING while reporting success. The
	// DB stamp is the same fallback the handoff cut uses; runTask's
	// post-register self-check sees the terminal stamp and self-cancels.
	d.mu.Lock()
	_, registered := d.runCancels[runID]
	d.mu.Unlock()
	if !registered {
		res, err := d.st.DB().ExecContext(d.ctx,
			`UPDATE run SET status='cancelled', cancel_reason='stopped', finished_at=? WHERE id=? AND status='running'`,
			nowStr(), runID)
		if err != nil {
			return fmt.Errorf("stop window stamp: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			logging.Infof("daemon: human stopped run %s (claim→register window stamp)", runID)
			return nil
		}
		return fmt.Errorf("run %s is no longer running", runID)
	}
	d.cancelRun(runID, "stopped")
	return nil
}

// cancelRun terminates a running run (if its cancel is registered), recording
// why. Idempotent.
func (d *Daemon) cancelRun(runID, reason string) {
	d.mu.Lock()
	cancel, ok := d.runCancels[runID]
	if ok {
		d.runCancelReasons[runID] = reason
	}
	d.mu.Unlock()
	if ok {
		logging.Infof("daemon: cut run %s (%s)", runID, reason)
		cancel()
	}
}

// takeCancelReason reads and clears the run's cancellation reason — the
// cancelled branch stamps it into result_summary once, so a handoff cut is
// recorded as a handoff, not as an idle-watchdog stall.
func (d *Daemon) takeCancelReason(runID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.runCancelReasons[runID]
	delete(d.runCancelReasons, runID)
	return r
}

func (d *Daemon) ensureWorker(agentID string, maxConcurrent int) *agentWorker {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if w, ok := d.workers[agentID]; ok {
		return w
	}
	w := &agentWorker{
		agentID:   agentID,
		sem:       make(chan struct{}, maxConcurrent),
		queue:     make(chan *service.ClaimedRow, workerQueueDepth),
		daemonCtx: d.ctx,
		maxConc:   maxConcurrent,
		run:       d.runTask,
	}
	w.ctx, w.cancel = context.WithCancel(d.ctx)
	d.workers[agentID] = w
	go w.loop()
	return w
}

func (w *agentWorker) loop() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case q, ok := <-w.queue:
			if !ok {
				return
			}
			w.sem <- struct{}{}
			go func(q *service.ClaimedRow) {
				defer func() { <-w.sem }()
				defer func() {
					if r := recover(); r != nil {
						logging.Infof("daemon: panic in runTask for run %s: %v", q.RunID, r)
					}
				}()
				w.run(w.daemonCtx, q)
			}(q)
		}
	}
}

func (d *Daemon) stopAll() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
}

// ── run dispatch ──

// dispatchOnce claims queued runs only for agents whose worker has free
// concurrency slots, then routes each to its agent's worker. This is the fix
// for the old global head-of-line blocking: a saturated agent can no longer
// stall other agents' dispatch (DESIGN.md).
func (d *Daemon) dispatchOnce(ctx context.Context) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	// ready = agents with at least one free slot right now.
	var ready []string
	type wc struct {
		id           string
		free, queued int
	}
	var dump []wc
	for id, w := range d.workers {
		free := cap(w.sem) - len(w.sem)
		queued := len(w.queue)
		if free > 0 && queued < workerQueueDepth {
			ready = append(ready, id)
		}
		dump = append(dump, wc{id, free, queued})
	}
	d.mu.Unlock()
	_ = dump

	if len(ready) == 0 {
		return
	}
	// Claim as many runs as we have free slots in total, capped per tick.
	totalFree := 0
	for _, id := range ready {
		d.mu.Lock()
		w := d.workers[id]
		d.mu.Unlock()
		if w != nil {
			totalFree += cap(w.sem) - len(w.sem)
		}
	}
	if totalFree == 0 {
		return
	}

	claimed := 0
	for claimed < totalFree {
		q, err := d.runSvc.Claim(ctx, ready)
		if err != nil {
			logging.Errorf("daemon: claim: %v", err)
			return
		}
		if q == nil {
			return // nothing left to claim
		}
		d.mu.Lock()
		w, ok := d.workers[q.AgentID]
		d.mu.Unlock()
		if !ok {
			// Agent lost its worker mid-dispatch; requeue by finishing as
			// failed+retry is wrong here — just leave queued for next tick.
			logging.Infof("daemon: no worker for agent %s (run %s)", q.AgentID, q.RunID)
			return
		}
		select {
		case w.queue <- q:
			claimed++
		default:
			// Worker queue full (rare — bounded by workerQueueDepth): the
			// claim already stamped the run 'running', so bailing would leave
			// a dead run that never reaches a worker (stuck until restart).
			// Return it to queued — the next tick re-claims it, attempt
			// untouched.
			logging.Infof("daemon: worker queue full for agent %s — returning run %s to queued", q.AgentID, q.RunID)
			if _, err := d.st.DB().ExecContext(ctx,
				`UPDATE run SET status='queued', started_at='' WHERE id=?`, q.RunID); err != nil {
				logging.Infof("daemon: requeue overflow run %s: %v", q.RunID, err)
			}
			return
		}
	}
}

// ── worktree model (DESIGN.md §6) ──
//
// v2 layout (决策 6-2, run-scoped workspaces):
//
//	{runsRoot}/<runID>/              per-run worktree (owner runs check out the
//	                                 goal branch; consult/review/verify runs
//	                                 detach from a ref — read-only)
//	{runsRoot}/repos/<domainID>/     shared bare repo (cloned once)
//	{runsRoot}/proc/<runID>/         processor scratch dirs
//
// The domain owns the shared bare repo; each RUN gets its own worktree —
// the execution-isolation unit (workspace ownership: the path <runID>
// belongs to that run, clean or not). The goal branch lives in the bare repo;
// checkpoints travel via commits (A5 revised: branch state, not file state).
// git operations on the shared repo (fetch, worktree add/remove, and every
// deliver) are serialized per domain (decision 2-10): concurrent fetches
// would collide on index.lock.

// runsRoot is where run worktrees and the domain bare repos live.
func runsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agentwork-runs")
	}
	return filepath.Join(home, ".agentwork", "runs")
}

func domainRepoPath(domainID string) string {
	return filepath.Join(runsRoot(), "repos", domainID)
}

// ── scratch domains (无仓库域) ──
//
// A scratch domain has no shared repository. Its persistent home is
// runs/scratch/<sanitized-name>/ — the HUMAN-maintained shared root (input
// material the agents read) plus per-goal directories under goals/. The
// mapping to the repo model: the shared root ≙ the bare repo's origin
// material; goals/<goalID> ≙ the goal's branch (durable state across runs);
// owner runs work DIRECTLY in their goal dir (single writer: owner
// single-flight), read-only runs get a copy snapshot.

// sanitizeDirName turns a domain name into a safe directory name — a
// hostile or odd name must not escape the scratch root (no /, no ..).
func sanitizeDirName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= 0x4e00 && r <= 0x9fff, // CJK unified
			r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_') // spaces, slashes, dots, everything else
		}
	}
	out := strings.Trim(b.String(), "_-")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		out = "domain"
	}
	return out
}

// scratchDomainRoot is a scratch domain's persistent project root (named by
// the domain name — unique per schema — so a human can browse it).
func scratchDomainRoot(domainName string) string {
	return filepath.Join(runsRoot(), "scratch", sanitizeDirName(domainName))
}

// scratchGoalDir is a scratch goal's persistent project directory — the
// branch analog: the goal's durable state lives HERE across runs.
func scratchGoalDir(domainName, goalID string) string {
	return filepath.Join(scratchDomainRoot(domainName), "goals", goalID)
}

// copyDir copies a directory tree (best-effort) — the read-only scratch
// snapshot. A torn copy while the owner writes is accepted for v1 (see the
// runTask branch).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// runWorktreePath is the run's execution worktree — the workspace (决策 6-2:
// path is the ownership boundary; a recovered run reuses its own dirty dir).
func runWorktreePath(runID string) string {
	return filepath.Join(runsRoot(), runID)
}

// deliverWorktreePath is the ephemeral worktree deliver uses to merge the
// goal branch into the default branch (removed after the deliver step).
func deliverWorktreePath(goalID string) string {
	return filepath.Join(runsRoot(), "deliver-"+goalID)
}

// goalBranchName is the branch a goal's worktree works on.
func goalBranchName(goalID string) string {
	if len(goalID) > 8 {
		goalID = goalID[:8]
	}
	return "feat-" + goalID
}

// domainLocks serializes git write operations per domain (fetch + deliver —
// decision 2-10). fetch and deliver on different domains run concurrently.
type domainLock struct {
	mu  sync.Mutex
	ref int
}

func (d *Daemon) lockDomain(domainID string) func() {
	d.mu.Lock()
	if d.domainLocks == nil {
		d.domainLocks = make(map[string]*domainLock)
	}
	l := d.domainLocks[domainID]
	if l == nil {
		l = &domainLock{}
		d.domainLocks[domainID] = l
	}
	l.ref++
	d.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		d.mu.Lock()
		l.ref--
		if l.ref == 0 {
			delete(d.domainLocks, domainID)
		}
		d.mu.Unlock()
	}
}

// ensureSharedRepo clones the domain repo ONCE as a BARE repository. Bare is
// the correct shape for the worktree model: a regular clone's main worktree
// holds one branch checked out, and git refuses to check that branch out in
// any other worktree ("already checked out") — which breaks deliver when the
// domain's default branch is the same one the main worktree sits on. A bare
// repo has no main worktree, so every branch is free to be checked out in any
// goal worktree. (git worktree add works against bare repos, git 2.5+.)
//
// Credentials (M4): the domain's git_credentials (a platform token) is
// injected into the HTTPS clone URL as the username — the machine-identity
// convention GitHub, GitLab and Gitee all accept. The credentialed URL
// persists in the bare repo's origin config, so EVERY later fetch/push
// (agent branches AND deliver's main push) inherits it — one credential
// configures the whole repo lifecycle. SSH repos keep their own auth
// (keys); git_credentials is a no-op there.
func (d *Daemon) ensureSharedRepo(ctx context.Context, domainID, gitURL, gitCredentials string) error {
	repo := domainRepoPath(domainID)
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
		return err
	}
	cloneURL := gitCloneURL(gitURL, gitCredentials)
	// The first pull of a domain's repo is the classic blockage point
	// (network/auth/大仓) — log it with the credentials stripped so the
	// panel shows exactly where a run is stuck.
	start := time.Now()
	logging.Infof("git: domain %s: clone --bare %s ...", domainID, sanitizeURL(cloneURL))
	cmd := exec.CommandContext(ctx, "git", "clone", "--bare", cloneURL, repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		logging.Errorf("git: domain %s: clone failed (%s): %s", domainID, time.Since(start).Round(time.Second), strings.TrimSpace(string(out)))
		return fmt.Errorf("git clone --bare %s: %w: %s", gitURL, err, string(out))
	}
	logging.Infof("git: domain %s: clone done (%s)", domainID, time.Since(start).Round(time.Second))
	// A bare clone mirrors remote branches into LOCAL refs/heads/ (its
	// remote.origin.fetch is "+refs/heads/*:refs/heads/*") and creates NO
	// refs/remotes/origin/* — so resolveDefaultBranch, worktree add
	// (origin/<branch>), and deliver would all fail to find the remote's
	// branches. Point the fetch refspec at refs/remotes/origin/* instead so
	// the rest of the code sees the usual remote-tracking namespace.
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		return fmt.Errorf("git config remote.origin.fetch: %w: %s", err, string(out))
	}
	// Fetch ONCE under the new refspec — until this runs, refs/remotes/
	// origin/* is EMPTY (the clone mirror wrote to refs/heads/*), and any
	// path that checks out origin/<branch> before a worker-run fetch would
	// fail "invalid reference" — the acceptance-policy compile being the
	// usual first operation on a fresh domain (live: fixing a typo'd
	// default_branch and recompiling "took no effect").
	start = time.Now()
	logging.Infof("git: domain %s: first fetch ...", domainID)
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "origin").CombinedOutput(); err != nil {
		logging.Errorf("git: domain %s: first fetch failed (%s): %s", domainID, time.Since(start).Round(time.Second), strings.TrimSpace(string(out)))
		return fmt.Errorf("git fetch: %w: %s", err, string(out))
	}
	logging.Infof("git: domain %s: first fetch done (%s)", domainID, time.Since(start).Round(time.Second))
	return nil
}

// gitCloneURL injects the domain's credentials into an HTTPS clone URL. The
// username convention differs PER HOST (surfaced by the first live GitCode
// run: the agent's push with the token as username was rejected, while the
// oauth2: prefix — the GitLab PAT format GitCode speaks — worked):
//
//	github.com → token as the username (machine-identity convention)
//	gitcode.com → oauth2:TOKEN (GitLab-style PAT)
//
// Unknown hosts fall back to token-as-username. A URL that already carries
// credentials (the owner embedded them explicitly) is left untouched; SSH
// URLs are returned as-is.
// sanitizeURL strips embedded credentials before a URL is logged — clone
// URLs carry the platform token, and it must never reach the log file.
func sanitizeURL(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.Index(rest, "@"); j >= 0 && !strings.Contains(rest[:j], "/") {
			return raw[:i+3] + rest[j+1:]
		}
	}
	return raw
}

func gitCloneURL(gitURL, credentials string) string {
	if credentials == "" || !strings.HasPrefix(gitURL, "https://") || strings.Contains(gitURL, "@") {
		return gitURL
	}
	cred := credentials
	if strings.Contains(gitURL, "gitcode.com") {
		cred = "oauth2:" + credentials
	}
	return "https://" + cred + "@" + strings.TrimPrefix(gitURL, "https://")
}

// ensureRunWorktreeFor allocates the run's worktree (决策 6-2): owner runs
// check out the goal branch (created from the domain's default branch on the
// first run; later runs reuse the branch — the A5 checkpoint now travels via
// commits, not file state). Sub-goal runs (subGoalID != ”) branch from the
// goal branch's current HEAD on their own sub-goal branch — that HEAD is the
// Change revision's integration base. Verify runs DETACH at the sub-goal
// branch head (read-only review of a stable state). A recovered run reuses
// its own directory as-is (workspace ownership: <runID> dirt belongs to
// that run). Returns the worktree path.
func (d *Daemon) ensureRunWorktreeFor(ctx context.Context, runID, domainID, goalID, subGoalID, role, gitURL, gitCredentials, defaultBranch string) (string, error) {
	wt := runWorktreePath(runID)
	if _, err := os.Stat(filepath.Join(wt, ".git")); err == nil {
		return wt, nil // recovery path: the run re-claims its own workspace
	}
	unlock := d.lockDomain(domainID)
	defer unlock()

	if err := d.ensureSharedRepo(ctx, domainID, gitURL, gitCredentials); err != nil {
		return "", err
	}
	repo := domainRepoPath(domainID)
	start := time.Now()
	logging.Infof("git: run %s: fetch domain %s ...", runID, domainID)
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "origin").CombinedOutput(); err != nil {
		logging.Errorf("git: run %s: fetch failed (%s): %s", runID, time.Since(start).Round(time.Second), strings.TrimSpace(string(out)))
		return "", fmt.Errorf("git fetch: %w: %s", err, string(out))
	}
	logging.Infof("git: run %s: fetch done (%s)", runID, time.Since(start).Round(time.Second))
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	if subGoalID != "" {
		if role == "verify" {
			// Verify workspace: a DETACHED snapshot of the sub-goal branch —
			// the verifier judges a stable state, never writes the branch.
			cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "--detach", wt, "refs/heads/"+subGoalBranchName(goalID, subGoalID))
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("git worktree add (verify): %w: %s", err, string(out))
			}
			return wt, nil
		}
		// Sub-goal workspace: branch from the goal branch's HEAD (falling back
		// to origin/<default> when the goal branch does not exist yet — the
		// owner split work before its first commit).
		base := "refs/heads/" + goalBranchName(goalID)
		if _, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", base).CombinedOutput(); err != nil {
			base = "origin/" + defaultBranch
		}
		cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "-b", subGoalBranchName(goalID, subGoalID), wt, base)
		if out, err := cmd.CombinedOutput(); err != nil {
			// The branch may exist from an earlier attempt (retry path).
			if exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", wt, subGoalBranchName(goalID, subGoalID)).Run() != nil {
				return "", fmt.Errorf("git worktree add (sub-goal): %w: %s", err, string(out))
			}
		}
		return wt, nil
	}

	// Consult/review runs are READ-ONLY participants (决策 6-2/6-7): DETACH
	// from the goal branch (a read snapshot) instead of checking it out —
	// the owner run may hold the branch's only checkout in parallel, and git
	// allows one checkout per branch (live: a parallel consult hit "a branch
	// named … already exists" because both runs tried to hold feat-<goal>).
	if role == "consult" || role == "review" {
		ref := "refs/heads/" + goalBranchName(goalID)
		if _, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", ref).CombinedOutput(); err != nil {
			// The owner split before its first commit — read the base.
			ref = "origin/" + defaultBranch
		}
		cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "--detach", wt, ref)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git worktree add (consult/review): %w: %s", err, string(out))
		}
		return wt, nil
	}

	// Owner workspace: the goal branch, created from the domain's configured
	// default branch (DESIGN.md §6: the domain owns default_branch). If
	// origin/{defaultBranch} does not exist, the error names it — the domain
	// config is wrong and the owner fixes it. No silent fallbacks.
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "-b", goalBranchName(goalID), wt, "origin/"+defaultBranch)
	if out, err := cmd.CombinedOutput(); err != nil {
		// The branch may already exist from an earlier run (checkpoint path).
		if exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", wt, goalBranchName(goalID)).Run() != nil {
			return "", fmt.Errorf("git worktree add: %w: %s", err, string(out))
		}
	}
	return wt, nil
}

// subGoalBranchName names the sub-goal's branch in the bare repo.
func subGoalBranchName(goalID, subGoalID string) string {
	if len(subGoalID) > 8 {
		subGoalID = subGoalID[:8]
	}
	return goalBranchName(goalID) + "-sg-" + subGoalID
}

// gitRun runs a git command in dir and returns its combined output.
func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustGitRun is gitRun for callers that need the (trimmed) output and can
// tolerate a failure returning "" (baseline lookups — an empty baseline is
// handled by the callers).
func mustGitRun(ctx context.Context, dir string, args ...string) string {
	out, _ := gitRun(ctx, dir, args...)
	return strings.TrimSpace(out)
}

// insertFiredGoal inserts the schedule-fired goal row inside the caller's
// transaction. Extracted as its own function because its column/value
// mapping regressed twice (excess VALUES were silently accepted and wrote
// 'active' into assignee_id with status left empty) — the regression test
// calls THIS, not a copy.
func insertFiredGoal(ctx context.Context, tx *sql.Tx, goalID, title, desc, domainID, assigneeType, assigneeID, scheduleID, ts string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO goal (id,title,description,domain_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at)
		 VALUES (?,?,?,?,?,?,'active','','system',?,?)`,
		goalID, title, desc, domainID, assigneeType, assigneeID, scheduleID, ts)
	return err
}

// ── worktree lifecycle (决策 6-2, run-scoped) ──

// cleanupWorktrees removes run worktrees whose run reached a terminal state
// more than worktreeRetentionDays ago (<runID> is the workspace — the
// branch state lives in the bare repo; only the checkout is reclaimed).
func (d *Daemon) cleanupWorktrees(ctx context.Context) {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT id, finished_at FROM run
		 WHERE status IN ('completed','failed','cancelled')
		   AND finished_at != ''`)
	if err != nil {
		logging.Errorf("daemon: cleanup worktrees: query: %v", err)
		return
	}
	type row struct{ runID, finished string }
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.runID, &r.finished); err != nil {
			continue
		}
		found = append(found, r)
	}
	rows.Close()

	cutoff := time.Now().Add(-worktreeRetentionDays * 24 * time.Hour)
	for _, r := range found {
		t, err := time.Parse(time.RFC3339Nano, r.finished)
		if err != nil || t.After(cutoff) {
			continue
		}
		wt := runWorktreePath(r.runID)
		if _, err := os.Stat(wt); err != nil {
			continue
		}
		// run worktrees were born in some domain's bare repo — find it via the
		// run's goal; removal goes through git (worktree bookkeeping). A
		// scratch domain's read-only snapshots are plain dirs — RemoveAll.
		var domainID, domainType string
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT g.domain_id, COALESCE(d.type,'') FROM goal g LEFT JOIN domain d ON d.id = g.domain_id JOIN run r ON r.goal_id = g.id WHERE r.id=?`, r.runID).Scan(&domainID, &domainType)
		if domainType == "scratch" {
			if err := os.RemoveAll(wt); err != nil {
				logging.Infof("daemon: cleanup scratch snapshot %s: %v", r.runID, err)
			}
			continue
		}
		if domainID == "" {
			continue
		}
		unlock := d.lockDomain(domainID)
		if out, err := gitRun(ctx, domainRepoPath(domainID), "worktree", "remove", "--force", wt); err != nil {
			logging.Infof("daemon: cleanup worktree %s: %v %s", r.runID, err, out)
		} else {
			logging.Infof("daemon: removed worktree for terminal run %s (retention expired)", r.runID)
		}
		unlock()
	}
}

// sweepRunWorktrees drops leftover RUN worktrees (a daemon crash leaves
// <runID> behind, still holding its branch checked out — the next run
// would fail to create its worktree). Called at startup BEFORE any dispatch.
// ORDER MATTERS: the dirs go first, THEN worktree prune — pruning while the
// dirs still exist is a no-op, and after RemoveAll the metadata turns
// 'prunable'; a stale prunable entry keeps git thinking the branch is still
// checked out and the next run's worktree add fails with "a branch named …
// already exists" (live: the cancelled smoke goal's recovered run hit this).
// The durable state is the commits; a crashed run's uncommitted WIP is lost
// (A5 recovery = transcript + committed state).
func (d *Daemon) sweepRunWorktrees(ctx context.Context) {
	repoRoot := filepath.Join(runsRoot(), "repos")
	// Run worktrees live directly under runsRoot() as <runID>/ dirs, alongside
	// the named siblings (repos/, proc/, scratch/, deliver-*). Sweep only the
	// worktree dirs — the reserved sibling names are skipped.
	reserved := map[string]bool{
		"repos": true, "proc": true, "scratch": true,
	}
	entries, err := os.ReadDir(runsRoot())
	if err != nil {
		logging.Errorf("daemon: sweep run worktrees: read runs root: %v", err)
	} else {
		swept := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if reserved[name] || strings.HasPrefix(name, "deliver-") {
				continue
			}
			if err := os.RemoveAll(filepath.Join(runsRoot(), name)); err != nil {
				logging.Infof("daemon: sweep run worktree %s: %v", name, err)
			} else {
				swept++
			}
		}
		logging.Infof("daemon: swept stale run worktrees (%d)", swept)
	}
	repoEntries, err := os.ReadDir(repoRoot)
	if err != nil {
		return
	}
	for _, e := range repoEntries {
		if !e.IsDir() {
			continue
		}
		repo := filepath.Join(repoRoot, e.Name())
		if _, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "prune").CombinedOutput(); err != nil {
			logging.Infof("daemon: worktree prune %s: %v", e.Name(), err)
		}
	}
}

// sweepDeliverWorktrees drops leftover ephemeral deliver worktrees (a deliver
// crashed mid-merge leaves runs/deliver-<goalID> behind). Called at startup —
// the worktree is recreated per deliver, so dropping is always safe.
func (d *Daemon) sweepDeliverWorktrees(ctx context.Context) {
	entries, err := os.ReadDir(runsRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "deliver-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(runsRoot(), e.Name())); err != nil {
			logging.Infof("daemon: sweep deliver worktree %s: %v", e.Name(), err)
		} else {
			logging.Infof("daemon: swept stale deliver worktree %s", e.Name())
		}
	}
}

// ── run execution ──

// runTask opens a transport, hands it to the protocol backend for one Prompt,
// drains events into chat_message + WS, and finishes the run (which triggers
// goal-layer reconciliation). Processor runs (no goal — run_kind=processor,
// e.g. the acceptance-policy compiler) take the runProcessorTask path.
func (d *Daemon) runTask(ctx context.Context, q *service.ClaimedRow) {
	if q.GoalID == "" {
		d.runProcessorTask(ctx, q)
		return
	}
	var title, desc, handoff, domainID, gitURL, defaultBranch, domainType, domainName, systemPrompt, transport, provider, execPath, argsJSON, endpoint, rtEnvJSON, sourceRef, gitCredentials string
	var triggerAuthor, triggerCommentID, triggerCommentContent, runRole, subGoalID, wakeNote string
	var maxConcurrent, maxRunDuration int
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT g.title, g.description, g.handoff_note, d.id, d.git_url, d.default_branch, COALESCE(d.type,''), COALESCE(d.name,''), a.system_prompt,
		        r.transport, r.provider, r.executable, r.args, r.endpoint, r.env, a.max_concurrent, d.max_run_duration,
		        g.source_ref, d.git_credentials,
		        r2.trigger_comment_id, COALESCE(c.author_type, ''), COALESCE(c.content, ''), r2.role, r2.sub_goal_id, r2.wake_note
		 FROM run r2
		 JOIN goal g ON g.id = r2.goal_id
		 LEFT JOIN domain d ON d.id = g.domain_id
		 JOIN agent a ON a.id = r2.agent_id
		 JOIN runtime r ON r.id = a.runtime_id
		 LEFT JOIN comment c ON c.id = r2.trigger_comment_id
		 WHERE r2.id = ?`, q.RunID).
		Scan(&title, &desc, &handoff, &domainID, &gitURL, &defaultBranch, &domainType, &domainName, &systemPrompt, &transport, &provider, &execPath, &argsJSON, &endpoint, &rtEnvJSON, &maxConcurrent, &maxRunDuration, &sourceRef, &gitCredentials, &triggerCommentID, &triggerAuthor, &triggerCommentContent, &runRole, &subGoalID, &wakeNote)
	// Claim visibility: which run, which agent, which role — the panel's
	// answer to "who is doing what right now". The TITLE travels with the
	// id: ids are for the system, humans read titles.
	logging.Infof("run %s claimed: agent=%s role=%s goal=%q (%s)", q.RunID, q.AgentID, runRole, title, q.GoalID)
	// Run role (决策 5-4/6-x, stamped at enqueue): review runs are the
	// platform's review requests (SYSTEM trigger comment — "请审查本次改动…
	// 只提意见"); consult runs are pulled in by an agent/human mention comment
	// — the trigger comment is this turn's instruction. Owner runs carry the
	// goal's execution authority (judged dynamically at reconcile, not here).
	// Sub-goal runs execute ONE work item on their own branch — the goal's
	// state machine is untouched by their outcome (决策 6-1).
	reviewRun := runRole == "review"
	consultRun := runRole == "consult"
	subGoalRun := runRole == "subgoal"
	verifyRun := runRole == "verify"
	// READ-ONLY runs (consult/review/verify) skip the whole machine-judgment
	// pipeline — they produce opinions, not work, and the platform's
	// verification judges work (决策 5-3/6-5). Declared early: the scratch
	// workdir branch below needs it.
	readOnlyRun := consultRun || reviewRun || verifyRun
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("load config: %v", err))
		return
	}
	// A sub-goal run's task is the WORK ITEM, not the goal (the goal's
	// description would re-execute the whole goal).
	if subGoalRun || verifyRun {
		var sgTitle, sgDesc string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT title, description FROM sub_goal WHERE id=?`, subGoalID).Scan(&sgTitle, &sgDesc); err != nil {
			d.failRun(ctx, q, fmt.Sprintf("load sub-goal: %v", err))
			return
		}
		title, desc = sgTitle, sgDesc
	}

	d.ensureWorker(q.AgentID, maxConcurrent)

	// Working directory (决策 6-2): every run gets its OWN worktree under
	// <runID> — the workspace. Owner runs check out the goal branch
	// (checkpoints travel via commits); sub-goal runs branch from the goal
	// branch's current HEAD (their Change's integration base); a recovered
	// run re-claims its own directory as-is (workspace ownership: its dirt
	// is its own). Fresh workspaces make the old unattributed-dirt park
	// unnecessary — there is no shared worktree a manual edit could pollute.
	if domainID == "" {
		d.failRun(ctx, q, "run's goal has no domain — cannot allocate a worktree")
		return
	}
	scratchDomain := domainType == "scratch"
	var runRowWorkdir string
	if scratchDomain {
		// A scratch goal's project directory IS the branch: owner runs work
		// DIRECTLY in it (persistent across runs — the file-state A5 model;
		// owner single-flight makes the goal dir single-writer), read-only
		// runs get a COPY snapshot (a torn copy is accepted for v1: report
		// files are small and a racy read is re-readable). SUB-GOALS work in
		// their own sg/<subGoalID> subdirectory under the goal dir — the
		// deliverable IS the files there (no Change/merge machinery; the
		// owner reviews the directory), verify runs snapshot a copy.
		if subGoalRun || verifyRun {
			sgDir := filepath.Join(scratchGoalDir(domainName, q.GoalID), "sg", subGoalID)
			if verifyRun {
				runRowWorkdir = runWorktreePath(q.RunID)
				if err := copyDir(sgDir, runRowWorkdir); err != nil {
					logging.Infof("daemon: scratch sg snapshot for run %s: %v", q.RunID, err)
					_ = os.MkdirAll(runRowWorkdir, 0o755)
				}
			} else {
				runRowWorkdir = sgDir
				if err := os.MkdirAll(runRowWorkdir, 0o755); err != nil {
					d.failRun(ctx, q, fmt.Sprintf("prepare scratch sg dir: %v", err))
					return
				}
			}
		} else if readOnlyRun {
			runRowWorkdir = runWorktreePath(q.RunID)
			if err := copyDir(scratchGoalDir(domainName, q.GoalID), runRowWorkdir); err != nil {
				logging.Infof("daemon: scratch snapshot for run %s: %v", q.RunID, err)
				_ = os.MkdirAll(runRowWorkdir, 0o755)
			}
		} else {
			runRowWorkdir = scratchGoalDir(domainName, q.GoalID)
			if err := os.MkdirAll(runRowWorkdir, 0o755); err != nil {
				d.failRun(ctx, q, fmt.Sprintf("prepare scratch dir: %v", err))
				return
			}
		}
	} else {
		runRowWorkdir, err = d.ensureRunWorktreeFor(ctx, q.RunID, domainID, q.GoalID, subGoalID, runRole, gitURL, gitCredentials, defaultBranch)
		if err != nil {
			d.failRun(ctx, q, fmt.Sprintf("prepare workdir: %v", err))
			return
		}
	}
	// Release the workspace when the run ends — git allows ONE checkout per
	// branch, and the goal/sub-goal branch is shared by every run of that
	// goal: the NEXT run cannot create its worktree while this one holds it
	// (the E2E hit "a branch named ... already exists" on the woken owner).
	// The worktree is EPHEMERAL — durable state lives in the commits; crash
	// leftovers are swept at startup (sweepRunWorktrees). Scratch: the owner
	// dir PERSISTS (it is the durable state); only the read-only copy goes.
	defer func() {
		if scratchDomain {
			if readOnlyRun {
				_ = os.RemoveAll(runRowWorkdir)
			}
			return
		}
		unlock := d.lockDomain(domainID)
		if out, err := exec.CommandContext(context.Background(), "git", "-C", domainRepoPath(domainID), "worktree", "remove", "--force", runRowWorkdir).CombinedOutput(); err != nil {
			logging.Infof("daemon: release worktree %s: %v %s", q.RunID, err, out)
		}
		unlock()
	}()

	// Environment readiness BEFORE the agent starts (决策 3-1, the setup half):
	// the acceptance policy's setup commands (dependency installs) prepare the
	// verification environment — but the agent needs the SAME environment to
	// self-verify while working (a pytest it cannot import is a blind run).
	// Run setup up front (idempotent; the verification stage re-runs it to
	// guarantee the judging environment is fresh). A failed setup here is an
	// environment failure — the run fails with that attribution, the retry
	// chain applies as usual.
	checks, timeout, baseline, checksFrozen := d.loadDomainChecks(ctx, domainID)
	if checksFrozen && len(checks.Setup) > 0 && !readOnlyRun {
		setupReport, ok := runSetupOnly(ctx, runRowWorkdir, checks, timeout)
		if !ok {
			d.finishRun(ctx, q, "failed", "environment setup failed:\n"+setupReport)
			return
		}
	}

	// The run's diff baseline: guards and evidence measure this run's changes
	// as baseSHA..HEAD (the agent may commit itself, and the daemon commits
	// leftover work at run end — both land in HEAD). Scratch domains have no
	// git — every git-derived judgment (guards, gates, diff evidence) degrades
	// to empty; the deliverable is the report.
	baseSHA := ""
	if !scratchDomain {
		baseSHA = strings.TrimSpace(mustGitRun(ctx, runRowWorkdir, "rev-parse", "HEAD"))
	}

	// Inject the agent's identity + team roster / squad briefing into the
	// workdir so the agent subprocess discovers who it is and who it can hand
	// off to (AGENTWORK.md). The guide is trimmed by run role (决策 6-20).
	roster := d.buildAgentGuide(ctx, q.AgentID, runRole)
	// Squad briefing is judged DYNAMICALLY — the run's agent must be the
	// goal's squad owner and its CURRENT leader. A leader mentioned by name
	// (mention://agent/<leader>) gets the same operating protocol as a leader
	// run triggered by assignment; authority and protocol stay consistent.
	briefing := ""
	if squadID, isLeader := d.leaderSquadFor(ctx, q.GoalID, q.AgentID); isLeader && squadID != "" {
		owns := d.goalOwnsSquadStatus(ctx, q.GoalID, squadID)
		if b, err := d.squadSvc.BuildLeaderBriefing(ctx, squadID, owns); err == nil {
			briefing = b
		}
	}
	if err := d.injectAgentProfile(runRowWorkdir, systemPrompt, roster, briefing); err != nil {
		logging.Infof("daemon: inject agent profile for run %s: %v", q.RunID, err)
	}

	// Parse runtime args + env.
	var args []string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		d.failRun(ctx, q, fmt.Sprintf("parse args: %v", err))
		return
	}
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(rtEnvJSON), &rtEnv)
	agentEnv, _ := d.loadAgentEnv(ctx, q.AgentID)

	// Build the task environment: inherit parent, layer agent env, inject
	// agentwork-cli context so the agent can call back into the server.
	taskEnv := os.Environ()
	for k, v := range agentEnv {
		taskEnv = append(taskEnv, k+"="+v)
	}
	addr := d.addr
	if addr == "" {
		addr = defaultListenAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("parse listen addr %q: %v", addr, err))
		return
	}
	serverURL := "http://" + net.JoinHostPort("127.0.0.1", port)
	taskEnv = append(taskEnv,
		"AGENTWORK_SERVER_URL="+serverURL,
		"AGENTWORK_GOAL_ID="+q.GoalID, // product-plane id (CLI comments/handoff)
		"AGENTWORK_RUN_ID="+q.RunID,   // execution-plane id
		"AGENTWORK_AGENT_ID="+q.AgentID,
	)
	// M4-B: issue-sourced goals carry the issue identity so the agent can
	// reply via `agentwork-cli issue comment` (the platform owns the token);
	// the LIVE issue comments are fetched at run start and injected into the
	// prompt — the issue stays the source of truth, nothing is stored.
	var issueRepo, issueNumber string
	var issueComments []issue.Comment
	if sourceRef != "" && gitCredentials != "" {
		if provider, repo, num, ok := issue.ParseSourceRef(sourceRef); ok {
			issueRepo, issueNumber = repo, strconv.Itoa(num)
			if client, err := issue.NewProvider(provider, gitCredentials); err == nil {
				if comments, err := client.ListComments(ctx, repo, num); err == nil {
					issueComments = comments
				} else if err != nil {
					logging.Infof("daemon: issue comments for %s: %v", sourceRef, err)
				}
			}
		}
	}
	if issueRepo != "" {
		taskEnv = append(taskEnv,
			"AGENTWORK_ISSUE_REPO="+issueRepo,
			"AGENTWORK_ISSUE_NUMBER="+issueNumber,
		)
	}

	// Execution-environment proxy (DESIGN.md 决策 4-8): EVERY run gets the
	// same execution model — the worktree lives on the client side and is
	// reached through the ACP fs/terminal capabilities the client declares
	// in the handshake. stdio and remote agents are treated identically:
	// one mechanism, one behaviour, and the handler path is exercised by
	// every run's traffic instead of only remote ones. (A stdio agent whose
	// local tools operate on its cwd — the worktree — is unaffected: same
	// directory, same result.) Leftover terminals are killed
	// unconditionally at session close (defer).
	env := newRunEnvironment(q.RunID, q.GoalID, q.AgentID, runRowWorkdir, serverURL)
	defer env.tm.cleanup()

	// Workspace MCP server (DESIGN.md 决策 4-8): every run advertises its
	// workspace as an MCP server over HTTP at session/new — agents that do
	// not delegate tools to client fs/terminal RPCs (opencode's tools are
	// local by design) still get read_file/write_file/run_command bound to
	// THIS run's worktree + environment. Registered for the run's
	// lifetime; the /mcp/{runID} route resolves it.
	mcpExec := mcp.NewExecutor(runRowWorkdir, env.runEnv(nil), env.tm)
	mcpExec.SetCollaboration(q.GoalID, q.AgentID, q.RunID, d.commentSvc, d.goalSvc, d.runSvc, d.agentSvc, d.squadSvc)
	d.mu.Lock()
	d.mcpExecs[q.RunID] = mcpExec
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.mcpExecs, q.RunID)
		d.mu.Unlock()
	}()

	// Open the transport (stdio/ws/tcp); the backend speaks the protocol.
	spec := runtime.Spec{
		Transport:  transport,
		Executable: execPath,
		Args:       args,
		Endpoint:   endpoint,
		Env:        rtEnv,
		Cwd:        runRowWorkdir, // the goal's worktree — see Spec.Cwd
	}
	conn, err := runtime.Open(ctx, spec, taskEnv)
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("open transport: %v", err))
		return
	}

	// Prompt the run: title + description + handoff/wakeup note, plus the
	// issue comments fetched at run start (M4-B: the issue is the
	// human-agent dialogue channel — the agent starts with the latest
	// conversation, nothing stored).
	// A REVIEW run (triggered by the platform's system review-request
	// comment) must NOT receive the goal's task description — it would
	// execute the task instead of reviewing it (a live failure: the opencode
	// reviewer re-created the file the leader had just written). Its prompt
	// is the review instruction only; the worktree, the comment feed
	// (injected below) and the workspace guidance give it everything else.
	prompt := buildPrompt(title, desc, handoff)
	// A COLLABORATION run (pulled in by a teammate's agent-authored comment —
	// a review request, a help request, a relay hand-back): the trigger
	// comment IS this turn's instruction. Without it the collaborator gets
	// the goal's original task description and executes THAT instead of the
	// request (a live failure: the leader-mentioned reviewer implemented the
	// Gomoku game instead of reviewing it). The original task is not
	// injected — it lives in the comment feed (the creation comment), which
	// is injected below as team context (决策 4-6): the collaborator reads
	// the background from the feed, and acts on the trigger comment.
	if consultRun {
		// A CONSULT (决策 5-1/5-2): the mention asked for information/judgment.
		// READ-ONLY by platform contract (决策 5-3) — the worktree is the
		// owner's state; edits made here are discarded at run end, commits are
		// flagged. The platform posts the final answer to the feed (threaded
		// to the question) — instructing a comment_goal here DOUBLE-posted
		// the answer (once by the agent, once as the run report): the feed
		// noise the agents produced.
		prompt = buildPrompt("Consultation request", "> "+triggerCommentContent, handoff) +
			"\n\nYou are the consulted expert in a READ-ONLY consult run: answer the question with your analysis and advice, then end your turn — the platform posts your final answer to the feed automatically (threaded to the question). Do NOT post a duplicate with agentwork_comment_goal. Do NOT modify files, do NOT commit, do NOT execute the task itself — your edits are discarded.\n" +
			"LANGUAGE: answer in the SAME language as the question."
	} else if reviewRun {
		// The review prompt carries the GOAL's identity — a reviewer who does
		// not know what the goal is about can only stare at the diff. A
		// no-code goal (self-introductions, a research summary) parks with an
		// empty diff: the reviewer must judge the CONVERSATION's deliverable
		// in the feed, not say "nothing to review" (live: the reviewer ran a
		// code review on an introduction task and answered "no implementation
		// changes to approve").
		inspectLine := "Inspect the changes in the worktree (diff, tests, quality) AND the goal's comment feed (the collaboration surface). "
		if scratchDomain {
			inspectLine = "This is a scratch project (no git repository) — inspect the project directory's ARTIFACTS (reports and files in the goal dir and its sg/ subdirectories) AND the goal's comment feed; there is no diff to look at. "
		}
		prompt = "You are a reviewer. Review the current goal's outcome (squad rule: after a member implements, a reviewer reviews).\n\n" +
			"Goal: " + title + "\n\n" +
			"Give your opinion ONLY — do not modify any file, do not execute the task itself.\n" +
			inspectLine +
			"If the diff is empty, the goal's deliverable lives in the feed — judge whether the goal's request was actually fulfilled there, " +
			"and say so explicitly; never report a missing diff as the answer itself.\n" +
			"End your turn with your opinion as the final message — the platform posts it to the feed (the approver reads it there). Do NOT post a duplicate with agentwork_comment_goal.\n" +
			"LANGUAGE: write your opinion in the SAME language as the goal's description and the human's comments.\n" +
			"Be specific: problems, risks, improvement suggestions. If the outcome looks good, say so explicitly."
	} else if verifyRun {
		// A VERIFIER (决策 6-5): the sub-goal's quality gate — machine checks
		// passed, now the named verifier judges. READ-ONLY workspace; the
		// verdict goes through the STRUCTURED tool (never stdout) — the
		// platform makes the verified/rejected transition from it.
		prompt = "You are the verifier for this sub-goal. Judge whether the work product meets its requirements.\n\n" +
			"Task: " + title + "\n" + desc + "\n\n" +
			"The worktree holds the sub-goal branch (READ-ONLY). Inspect the implementation, tests and quality — you may RUN tests, but do not modify any file and do not commit.\n\n" +
			"Then issue your verdict ONCE with agentwork_verify_sub_goal:\n" +
			"- verdict=\"passed\": summary states what you verified, evidence carries key artifacts (test output excerpts etc.)\n" +
			"- verdict=\"rejected\": summary must list the CONCRETE problems (the assignee fixes from them)\n\n" +
			"LANGUAGE: write the summary in the SAME language as the goal's description and the human's comments.\n" +
			"Give the verdict once, then end your turn immediately."
	}

	// Workspace guidance (DESIGN.md 决策 4-8), injected for EVERY run — one
	// execution model for stdio and remote alike. The worktree lives on the
	// client side and is reached through the ACP fs/terminal capabilities
	// the client declared during the handshake. The guidance names only the
	// ACP protocol capabilities (deterministic); the agent maps them to its
	// own tools — its toolset is its own business, agentwork states the
	// environment facts and the collaboration contract.
	//
	// First-contact injection (决策 6-20): the fixed contract blocks go in
	// FULL only on the agent's first run for this goal — later runs of the
	// same goal-session reuse the contract from AGENTWORK.md and get the
	// per-run facts plus a pointer. The comment feed stays FULL either way
	// (a fresh process has no other memory — the feed IS the session's
	// memory; delta feed must wait for session residency, Level 2).
	firstContact := true
	if q.GoalID != "" {
		var prior int
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND id != ?`,
			q.GoalID, q.AgentID, q.RunID).Scan(&prior); err == nil && prior > 0 {
			firstContact = false
		}
	}
	prompt += worktreeGuidance(runRowWorkdir, firstContact)
	if scratchDomain {
		prompt += "\n\n## Workspace contract (scratch domain)\nThis directory IS your task's persistent project directory — your artifacts (reports, notes, files) live HERE and survive between turns; the comment feed is only for coordination. Sub-goals work in their own sg/<subGoalID> subdirectories here (no code merge — the owner reviews the files directly). The PARENT directory is human-maintained shared material: READ-ONLY for you. Write only inside this directory."
	}

	// The domain's acceptance policy in NL (the "what counts as done" the
	// OWNER defined) — the agent works toward it instead of finding out at
	// verification time. Only the NL intent is injected; the compiled checks
	// (verify commands / guard patterns) stay invisible — an agent that sees
	// the exact patterns can satisfy the check instead of the intent
	// (triangle separation: define stays with the human, execute with the
	// agent, judge with the machine+human).
	var policyText string
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT d.policy_text FROM goal g JOIN domain d ON d.id=g.domain_id WHERE g.id=?`, q.GoalID).Scan(&policyText)
	if s := strings.TrimSpace(policyText); s != "" {
		prompt += "\n\n## Acceptance policy (this domain's definition of done, set by the domain owner)\n" + s +
			"\n\nThe machine will judge your changes against this — work toward it to avoid rework."
	}

	// Sub-goal rework context (决策 6-5/6-3): a rejected verdict or a
	// conflicted change is WHY this round runs — inject it so the assignee
	// fixes, not redoes.
	if subGoalRun && subGoalID != "" {
		var rejectSummary string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT summary FROM verification_result WHERE sub_goal_id=? AND status='rejected' ORDER BY created_at DESC LIMIT 1`,
			subGoalID).Scan(&rejectSummary); err == nil && strings.TrimSpace(rejectSummary) != "" {
			prompt += "\n\n## Why your previous round was rejected (fix from this — do NOT start over)\n" + rejectSummary
		}
		var chStatus string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT status FROM change WHERE sub_goal_id=? ORDER BY created_at DESC LIMIT 1`,
			subGoalID).Scan(&chStatus); err == nil && chStatus == "conflict" {
			prompt += "\n\n## Your previous Change conflicted at integration\nResolve it against the new integration base (the goal branch's current state); your new Revision replaces the old one."
		}
	}

	// The verification-failure feedback loop: a RETRY run (attempt > 1, the
	// previous run failed machine verification) must know WHY it failed —
	// otherwise it blindly re-runs the same work and likely fails again.
	// The last failed run's report (verify/guards output) is injected with
	// an explicit "fix, don't redo" framing.
	if q.Attempt > 1 {
		var lastFail string
		query := `SELECT result_summary FROM run WHERE goal_id=? AND status='failed' ORDER BY finished_at DESC LIMIT 1`
		args := []any{q.GoalID}
		if subGoalID != "" {
			// Sub-goal rework: the failure context must come from THIS
			// sub-goal's own failed round — another sub-goal's failure
			// summary would misdirect the fix ("fix the existing code"
			// pointing at someone else's work).
			query = `SELECT result_summary FROM run WHERE sub_goal_id=? AND status='failed' ORDER BY finished_at DESC LIMIT 1`
			args = []any{subGoalID}
		}
		if err := d.st.DB().QueryRowContext(ctx, query, args...).Scan(&lastFail); err == nil && strings.TrimSpace(lastFail) != "" {
			prompt += "\n\n## Why the previous round failed (machine verification did not pass — fix the existing code, do NOT start over)\n" + truncateIn(lastFail, 1500)
		}
	}

	prompt += d.commentsInjection(ctx, q.GoalID)
	if len(issueComments) > 0 {
		var b strings.Builder
		b.WriteString("\n\n## Latest issue conversation (from the remote)\n")
		for _, cm := range issueComments {
			fmt.Fprintf(&b, "- %s：%s\n", cm.User.Login, truncateIn(cm.Body, 300))
		}
		prompt += b.String()
	}
	// Owner Resume Context (决策 6-4): a spawned owner run needs to know WHY
	// it was woken. The wake note (决策 6-17) travels ON the run row — the
	// snapshot compiled at spawn time, immune to later attention
	// re-derivations. The live attention section below is only the fallback
	// for rows that predate the note (and the UI badge signal remains live
	// either way). Expand details on demand via get_change/get_sub_goal.
	if runRole == "owner" {
		if wakeNote != "" {
			prompt += "\n\n## Owner Attention (why you were woken)\n" + wakeNote + "\n"
		} else if attention := d.goalAttention(ctx, q.GoalID); attention != "" {
			prompt += "\n\n## Owner Attention (why you were woken)\n"
			changes, _ := d.goalSvc.ListChanges(ctx, q.GoalID)
			var ready, conflicted []string
			for _, c := range changes {
				if c.Status == "ready" {
					ready = append(ready, c.ID)
				} else if c.Status == "conflict" {
					conflicted = append(conflicted, c.ID)
				}
			}
			if len(ready) > 0 {
				prompt += fmt.Sprintf("- %d change(s) ready to integrate (inspect with agentwork_get_change, merge each with agentwork_integrate_change)\n", len(ready))
			}
			if len(conflicted) > 0 {
				prompt += fmt.Sprintf("- %d change(s) in conflict (the assignee was woken to rework; wait for the new Revision)\n", len(conflicted))
			}
			sgs, _ := d.goalSvc.ListSubGoals(ctx, q.GoalID)
			var failed int
			var verified int
			for _, sg := range sgs {
				if sg.Status == "failed" {
					failed++
				} else if sg.Status == "verified" {
					verified++
				}
			}
			if failed > 0 {
				prompt += fmt.Sprintf("- %d sub-goal(s) failed (inspect with agentwork_get_sub_goal, then cancel or re-create)\n", failed)
			}
			if verified > 0 {
				prompt += fmt.Sprintf("- %d sub-goal(s) verified", verified)
				// A verified sub-goal with NO Change produced no code — the
				// deliverable must live in the feed (决策 6-8) OR the round
				// spun without doing the work (the live .gitignore case: the
				// assignee "completed" without touching the repo). Name the
				// count so the owner checks the feed before integrating
				// nothing.
				var noChange int
				_ = d.st.DB().QueryRowContext(ctx,
					`SELECT COUNT(*) FROM sub_goal sg WHERE sg.goal_id=? AND sg.status='verified' AND NOT EXISTS (SELECT 1 FROM change c WHERE c.sub_goal_id=sg.id)`,
					q.GoalID).Scan(&noChange)
				if noChange > 0 {
					prompt += fmt.Sprintf("（其中 %d 个无代码变更——确认交付物在评论区，否则是空转）", noChange)
				}
				prompt += "\n"
			}
			prompt += "Handle these attention items, then continue the goal's own work or end your turn.\n"
		}
		// P1-3 (决策 6-15⑨): the owner wakes BECAUSE its consults resolved
		// (the finalization guard holds the goal until they do) — inject the
		// answers directly instead of making it hunt through the feed.
		if section := d.consultStatus(ctx, q.GoalID, q.AgentID, q.RunID); section != "" {
			prompt += section
		}
	}

	// Mention-cycle hint (soft tier): the goal's agent-triggered churn is
	// above the hint threshold — tell the agents to stop circular handoffs
	// instead of perpetuating them. (The hard tier fails the goal at the
	// trigger site.)
	if n, err := d.agentTriggeredRunCount(ctx, q.GoalID); err == nil && n > service.MaxMentionHints {
		prompt += fmt.Sprintf("\n\n⚠️ Collaboration warning: agents have handed this task back and forth %d times. Do NOT hand it off again — finish the remaining work yourself, or end your turn and leave it for a human.", n)
	}

	backend, err := d.protoReg.Get(provider)
	if err != nil {
		conn.Close()
		d.failRun(ctx, q, fmt.Sprintf("provider %q: %v", provider, err))
		return
	}
	// maxRunDuration (DESIGN.md §4, decision 2-6): the run's total time
	// budget, independent of activity — a run stuck in a tool loop must not
	// burn forever. The idle watchdog (silence) and this (total time) are
	// complementary cancellers of the same promptCtx. On timeout the backend
	// reports StatusCancelled; the run is cancelled WITHOUT consuming attempt
	// credit, the goal stays active, and the human is notified (M1 notify).
	if maxRunDuration <= 0 {
		maxRunDuration = 7200 // default 2h
	}
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var inFlightTools atomic.Int32
	promptCtx, promptCancel := context.WithTimeout(ctx, time.Duration(maxRunDuration)*time.Second)
	defer promptCancel()
	// Register the run's cancel so a handoff can cut it (goal:assigned →
	// onGoalAssigned); unregister when the run finishes. The same cancel the
	// idle watchdog fires.
	d.mu.Lock()
	d.runCancels[q.RunID] = promptCancel
	delete(d.runCancelReasons, q.RunID) // fresh run — no stale reason
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.runCancels, q.RunID)
		delete(d.runCancelReasons, q.RunID)
		d.mu.Unlock()
	}()
	// Post-register self-check: a handoff that landed in the claim→register
	// window stamped this run terminal in the DB (onGoalAssigned found no
	// registered cancel to cut). Honor the stamp — self-cancel so the new
	// owner's run claims immediately instead of waiting out this one. The
	// stamp is the only concurrent writer of status besides finishRun, and
	// finishRun runs after this check, so there is no race.
	var registeredStatus, stampedReason string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT status, cancel_reason FROM run WHERE id=?`, q.RunID).Scan(&registeredStatus, &stampedReason)
	if registeredStatus != "running" {
		// The stamp is the authority — carry ITS reason through the
		// cancellation branch (a handoff stamp reports handoff; a human
		// stop stamp reports stopped), not a hardcoded guess.
		if stampedReason == "" {
			stampedReason = "handoff"
		}
		logging.Infof("daemon: run %s terminal-stamped before registration — self-cancelling (%s)", q.RunID, stampedReason)
		d.mu.Lock()
		d.runCancelReasons[q.RunID] = stampedReason
		d.mu.Unlock()
		promptCancel()
	}
	go d.runIdleWatchdog(promptCtx, &lastActivity, &inFlightTools, promptCancel, q.RunID, env.tm.activeCount)

	run, err := backend.Execute(promptCtx, proto.ExecuteSpec{
		Conn:          conn,
		Cwd:           runRowWorkdir,
		Prompt:        prompt,
		ClientHandler: env,
		// The platform's workspace server first, then the agent's own
		// configured MCP servers (its specialty tools) — both advertised at
		// session/new, both live in the agent's tool registry.
		McpServers: append([]acp.McpServer{{
			Type: "http",
			Name: "agentwork",
			URL:  serverURL + "/mcp/" + q.RunID,
		}}, d.extraMcpServers(ctx, q.AgentID)...),
	})
	if err != nil {
		conn.Close()
		d.failRun(ctx, q, fmt.Sprintf("execute: %v", err))
		return
	}

	// Record workdir once the session is live (best-effort; the backend may not
	// return a session id until Result).
	_ = d.runSvc.MarkSession(ctx, q.RunID, "", runRowWorkdir)

	for ev := range run.Events {
		lastActivity.Store(time.Now().UnixNano())
		d.persistEvent(ctx, q.RunID, ev)
		d.trackToolInflight(&inFlightTools, ev)
		d.bus.Publish(ctx, events.Event{Topic: "run:event", Payload: map[string]any{
			"run_id": q.RunID, "event": ev,
		}})
	}

	result, ok := <-run.Result
	conn.Close()
	if !ok {
		result = proto.Result{Status: proto.StatusFailed, Output: "backend closed result channel"}
	}
	// The event stream is done — flush the aggregated text buffer so the last
	// message of the turn is persisted (persistEvent aggregates per-role).
	d.flushRunMessages(ctx, q.RunID)

	// Consult/review read-only enforcement (决策 6-2/6-7): the run's workspace
	// started CLEAN (fresh worktree from a ref) — whatever the outcome, reset
	// it to HEAD and clean untracked files (domain read-only: ephemeral writes
	// allowed, nothing survives). A guest that committed DIRECTLY is flagged
	// in the feed (not reverted — HEAD may carry the owner's history under
	// it... the fresh workspace makes baseSHA==claim-HEAD, so any commit is
	// the guest's own and only flagged for human visibility). Scratch
	// read-only runs are plain-dir COPIES — there is no git to commit into,
	// and the copy is removed by the run's cleanup anyway; the git check
	// would only mint a false "contract violated" alarm (fatal: not a git
	// repository).
	if (reviewRun || consultRun || verifyRun) && domainID != "" && !scratchDomain {
		resetGuestWorkspace(ctx, runRowWorkdir)
		if committed := guestCommittedLog(ctx, runRowWorkdir, baseSHA); committed != "" {
			// Platform text is English (决策 6-18) — the MATERIALS (the commit
			// log below) carry their own language.
			if _, err := d.st.DB().ExecContext(ctx,
				`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?,'system','',NULL,?,?,?)`,
				uuid.NewString(), q.GoalID, "⚠️ A read-only run committed changes directly (consult/review contract violated) — they stay on the goal branch and are visible before delivery:\n```\n"+committed+"\n```", nowStr(), q.RunID); err != nil {
				logging.Infof("daemon: guest commit warning for run %s: %v", q.RunID, err)
			}
		}
	}

	switch result.Status {
	case proto.StatusCompleted:
		// The handoff/wakeup note is consumed by the goal layer (ReconcileOnRunEnd
		// clears it only after confirming this run owns the goal and the goal
		// promotes to done). The daemon must NOT clear it here: on a handoff this
		// run no longer owns the goal, and clearing would wipe the new owner's
		// note (see P2 in the bug review).
		_ = d.runSvc.MarkSession(ctx, q.RunID, result.SessionID, runRowWorkdir)

		// The run's REPORT is its last assistant message — the agent's final
		// summary (what it did + how it verified), NOT the full transcript
		// (which opens with the agent's thinking). The approval card, the
		// comment feed, and the deliver note all read this; a transcript
		// opening made Feishu's approval card show "I'm the worker agent…"
		// instead of the work. Falls back to the backend output.
		var report string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT content FROM chat_message WHERE run_id=? AND role='assistant' AND content != '' ORDER BY created_at DESC LIMIT 1`,
			q.RunID).Scan(&report); err != nil || strings.TrimSpace(report) == "" {
			report = result.Output
		}

		if domainID != "" {
			// Make the agent's work durable on the goal branch (the agent is
			// guided to commit; the daemon guarantees it — deliver merges the
			// branch, and uncommitted work would deliver nothing). The
			// domain's declared excludes (checks.excludes, compiled + owner-
			// confirmed) keep dependency dirs out of the branch.
			// GUEST and REVIEW runs are exempt (决策 5-3): their product is
			// opinion/information, not code — nothing of theirs merges into
			// the goal branch; window changes were already discarded above.
			if !reviewRun && !consultRun && !verifyRun && !scratchDomain {
				if err := commitRunChanges(ctx, runRowWorkdir, d.domainGitIdentity(ctx, domainID), checks.Excludes); err != nil {
					d.finishRun(ctx, q, "failed", "commit run changes: "+err.Error())
					return
				}
			}
			// Machine verification (DESIGN.md §4/§5 (§9 invariant 14)): the
			// domain's verify commands run BEFORE the run is finished. A red
			// verify ends the run failed → retry chain. The goal layer only
			// ever sees 'completed' runs that passed.
			//
			// An UNFROZEN policy (checks_compiled_at empty — the owner never
			// confirmed the compiled checks) runs NOTHING: no setup/verify/
			// guards against an unconfirmed definition, and no gate evaluation
			// (the goal layer forces the human checkpoint instead). Evidence
			// still carries the diff + agent summary for that checkpoint.
			verifyReport, guardReport := "", ""
			if checksFrozen && !readOnlyRun {
				verifyReport, ok, policyIssue := runVerification(ctx, runRowWorkdir, checks, timeout)
				if !ok {
					// Objective policy defect (POSIX exit 127 — missing command /
					// script)? Flag it once — the owner fixes the policy instead
					// of the agent burning retries against an impossible check.
					if policyIssue {
						d.annotatePolicyIssue(ctx, q.GoalID)
					}
					d.finishRun(ctx, q, "failed", "verification failed:\n"+verifyReport)
					return
				}
				// Structural guards on the diff (DESIGN.md §5.1), measured as
				// baseSHA..HEAD — the run's own changes. git status would be empty
				// here: the daemon just committed the agent's work (and the agent
				// may have committed itself), so the worktree is clean. Scratch
				// domains have no diff — guards are repo-only.
				if !scratchDomain {
					guardReport, ok = checkGuards(ctx, runRowWorkdir, baseSHA, checks, baseline)
					if !ok {
						d.finishRun(ctx, q, "failed", "guards failed:\n"+guardReport)
						return
					}
				}
				// Gate evaluation (M2 rule engine): merge always fires; diff_*
				// fire on the run's changed paths. The fired gates are recorded on
				// the run row — the goal layer reads them in the reconcile
				// transaction (the daemon computes, the goal layer judges).
				// SUB-GOAL runs skip gates (决策 6-1): their work item has no
				// human checkpoint — verification is machine (or the optional
				// agent verifier, 决策 6-5), the human gates stay goal-level.
				if !subGoalRun && !verifyRun && !scratchDomain {
					gatesHit := evalGates(ctx, runRowWorkdir, baseSHA, checks)
					if len(gatesHit) > 0 {
						gatesJSON, _ := json.Marshal(gatesHit)
						if _, err := d.st.DB().ExecContext(ctx, `UPDATE run SET gates_hit=? WHERE id=?`, string(gatesJSON), q.RunID); err != nil {
							logging.Infof("daemon: record gates_hit for run %s: %v", q.RunID, err)
						}
					}
				}
			}
			// Sub-goal runs stamp the Change revision refs (决策 6-3): the
			// revision's integration base (merge-base of the goal branch and the
			// sub-goal branch) + the delivered head — the sub-goal layer creates
			// Change + Revision atomically from these.
			if subGoalRun && subGoalID != "" && !scratchDomain {
				goalBranch := goalBranchName(q.GoalID)
				base := strings.TrimSpace(mustGitRun(ctx, runRowWorkdir, "merge-base", goalBranch, "HEAD"))
				head := strings.TrimSpace(mustGitRun(ctx, runRowWorkdir, "rev-parse", "HEAD"))
				if _, err := d.st.DB().ExecContext(ctx,
					`UPDATE run SET base_ref=?, head_ref=? WHERE id=?`, base, head, q.RunID); err != nil {
					logging.Infof("daemon: stamp change refs for run %s: %v", q.RunID, err)
				}
			}
			// Evidence bundle for the approval card (decision 2-3).
			ev := buildEvidence(ctx, runRowWorkdir, baseSHA, report, verifyReport, guardReport)
			if _, err := d.st.DB().ExecContext(ctx, `UPDATE run SET evidence=? WHERE id=?`, ev, q.RunID); err != nil {
				logging.Infof("daemon: store evidence for run %s: %v", q.RunID, err)
			}
		}
		d.finishRunOK(ctx, q, report)
	case proto.StatusCancelled:
		// decision 2-6 + the "stuck active with no run" hole: a cancelled run
		// does NOT fail the goal, and does NOT consume attempt credit — the
		// requeue keeps the SAME attempt (a timeout is not a machine failure).
		// The convergence rule is separate: only the FIRST cancellation gets an
		// automatic retry; a second consecutive stall is systemic (the agent
		// keeps hanging) and surfaces to the owner instead of looping.
		// Only IDLE-WATCHDOG cancellations count toward the convergence rule —
		// the worktree-dirty park, handoff cuts, approval cuts, and human
		// cancels also mark runs cancelled (without the watchdog summary) and
		// must not consume the single automatic retry.
		// Structured cancellation reason (no string-matching semantics): the
		// code is stamped on the run row + carried in the event; the summary
		// text is display only.
		code := d.takeCancelReason(q.RunID)
		if code == "" {
			code = "timeout" // maxRunDuration deadline — no retry (决策 2-6)
		}
		display := map[string]string{
			"idle_watchdog": "idle watchdog",
			"handoff":       "handoff",
			"stopped":       "stopped", // human-initiated (StopRun / Cancel)
			"timeout":       "timeout",
			"runaway":       "runaway", // the reaper terminalized the run (P1, 决策 6-15⑦)
		}[code]
		summary := display + ": " + result.Output
		// P0-5: the reason stamp is conditional — a run the reaper (or the
		// handoff window stamp) already terminalized keeps THEIR reason; this
		// late cancellation is not the run's terminal truth.
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE run SET cancel_reason=? WHERE id=? AND status='running'`, code, q.RunID); err != nil {
			logging.Infof("daemon: stamp cancel_reason %s: %v", q.RunID, err)
		}
		var priorCancelled int
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run WHERE goal_id=? AND status='cancelled' AND cancel_reason='idle_watchdog'`, q.GoalID).Scan(&priorCancelled)
		if priorCancelled == 0 && code == "idle_watchdog" {
			var isLeader int
			var squadID string
			_ = d.st.DB().QueryRowContext(ctx, `SELECT is_leader_run, squad_id FROM run WHERE id=?`, q.RunID).Scan(&isLeader, &squadID)
			if err := d.runSvc.EnqueueExisting(ctx, q.GoalID, q.AgentID, q.Attempt, isLeader != 0, squadID); err != nil {
				logging.Infof("daemon: requeue cancelled run %s: %v", q.RunID, err)
			}
		}
		// 决策 6-6: a handoff cut keeps status='cancelled' — the structured
		// cancel_reason ('handoff') carries the semantics (handoff is a
		// Goal-level event, not a run lifecycle state). Same non-retry,
		// non-notify behavior as before.
		//
		// P0-5: the terminal event publishes ONLY when this writer's stamp
		// won — a reaper-terminalized run publishes its own run:cancelled
		// (from reapRunawayRuns); republishing here would double the notify
		// card.
		if d.finishRun(ctx, q, "cancelled", summary) {
			// Surface the stall so the notify layer can tell the owner a task
			// stalled (cancelled runs leave the goal active with no pending
			// run — the human decides, per decision 2-6).
			d.bus.Publish(ctx, events.Event{Topic: "run:cancelled", Payload: map[string]any{
				"run_id": q.RunID, "goal_id": q.GoalID, "reason": summary, "reason_code": code,
			}})
		}
	case proto.StatusFailed, proto.StatusAborted:
		d.finishRun(ctx, q, "failed", result.Output)
	}
}

// runProcessorTask executes a platform-internal processor run: opens the
// agent's transport, sends the run's fixed prompt, drains events, and then
// collects the FILE-based result — the compiled checks.json + strength.txt —
// from the run workdir and stores it on the associated domain in an UNFROZEN
// state (checks_compiled_at stays ”), publishing domain:compiled so the
// frontend can show the owner confirmation card. Structured output is read
// from files, never parsed from agent stdout (DESIGN.md §5.3, §9).
func (d *Daemon) runProcessorTask(ctx context.Context, q *service.ClaimedRow) {
	var prompt, runType, domainID, agentID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT r2.prompt, r2.run_type, r2.domain_id, r2.agent_id FROM run r2 WHERE r2.id=?`, q.RunID).
		Scan(&prompt, &runType, &domainID, &agentID)
	if err != nil {
		logging.Infof("daemon: processor run %s: load config: %v", q.RunID, err)
		d.failProcessorRun(ctx, q, "load config: "+err.Error())
		return
	}
	// The intake pipeline (M3-4) has no domain — its coalesce key is the
	// inbound message id (see IntakeService.Enqueue).
	if runType == "intake" {
		d.runIntakeTask(ctx, q, prompt, agentID)
		return
	}
	if prompt == "" || domainID == "" {
		d.failProcessorRun(ctx, q, "processor run missing prompt or domain_id")
		return
	}

	var systemPrompt, transport, provider, execPath, argsJSON, endpoint, rtEnvJSON string
	var maxConcurrent int
	err = d.st.DB().QueryRowContext(ctx,
		`SELECT a.system_prompt, r.transport, r.provider, r.executable, r.args, r.endpoint, r.env, a.max_concurrent
		 FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`, agentID).
		Scan(&systemPrompt, &transport, &provider, &execPath, &argsJSON, &endpoint, &rtEnvJSON, &maxConcurrent)
	if err != nil {
		d.failProcessorRun(ctx, q, "load agent runtime: "+err.Error())
		return
	}
	d.ensureWorker(agentID, maxConcurrent)

	// Workdir: compile runs work ON THE REAL REPO — a worktree of the domain's
	// shared bare repo, so the processor compiles against what the repo
	// actually is, not a guess from an empty scratch dir (the regression: an
	// empty dir produced "pip install -r requirements.txt" for a zero-
	// dependency repo and every verification failed). Intake runs keep the
	// scratch dir (no repo involved).
	runRowWorkdir := filepath.Join(runsRoot(), "proc", q.RunID)
	if runType == "compile" {
		var gitURL, gitCredentials, defaultBranch, domainType string
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT git_url, git_credentials, default_branch, COALESCE(type,'') FROM domain WHERE id=?`, domainID).
			Scan(&gitURL, &gitCredentials, &defaultBranch, &domainType)
		// A scratch domain has no repo for the compile agent to inspect —
		// it compiles from the policy text alone in its proc dir (the
		// compile prompt carries the scratch variant).
		if domainType != "scratch" && gitURL == "" {
			d.failProcessorRun(ctx, q, "compile run: domain has no git_url")
			return
		}
		if domainType == "scratch" {
			if err := os.MkdirAll(runRowWorkdir, 0o755); err != nil {
				d.failProcessorRun(ctx, q, "mkdir compile workdir: "+err.Error())
				return
			}
		} else {
			unlock := d.lockDomain(domainID)
			if err := d.ensureSharedRepo(ctx, domainID, gitURL, gitCredentials); err != nil {
				unlock()
				d.failProcessorRun(ctx, q, "prepare compile repo: "+err.Error())
				return
			}
			repo := domainRepoPath(domainID)
			if defaultBranch == "" {
				defaultBranch = "main"
			}
			// Fresh refs before the checkout — a corrected default_branch must
			// take effect on the next compile, not the one after (parity with the
			// worker path's fetch).
			if out, err := exec.CommandContext(ctx, "git", "-C", repo, "fetch", "origin").CombinedOutput(); err != nil {
				unlock()
				d.failProcessorRun(ctx, q, "compile fetch: "+err.Error()+": "+string(out))
				return
			}
			if out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", runRowWorkdir, "origin/"+defaultBranch).CombinedOutput(); err != nil {
				unlock()
				d.failProcessorRun(ctx, q, "compile worktree add: "+err.Error()+": "+string(out))
				return
			}
			unlock()
			defer func() {
				unlock := d.lockDomain(domainID)
				_, _ = exec.CommandContext(context.Background(), "git", "-C", repo, "worktree", "remove", "--force", runRowWorkdir).CombinedOutput()
				unlock()
			}()
		}
	} else if err := os.MkdirAll(runRowWorkdir, 0o755); err != nil {
		d.failProcessorRun(ctx, q, "mkdir workdir: "+err.Error())
		return
	}

	var args []string
	_ = json.Unmarshal([]byte(argsJSON), &args)
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(rtEnvJSON), &rtEnv)
	agentEnv, _ := d.loadAgentEnv(ctx, agentID)
	taskEnv := os.Environ()
	for k, v := range agentEnv {
		taskEnv = append(taskEnv, k+"="+v)
	}
	// The processor agent is not a goal worker: no AGENTWORK_GOAL_ID/RUN_ID
	// injection (it should not call back into the platform), just the server
	// URL and its own identity for orientation.
	addr := d.addr
	if addr == "" {
		addr = defaultListenAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		d.failProcessorRun(ctx, q, "parse listen addr: "+err.Error())
		return
	}
	serverURL := "http://" + net.JoinHostPort("127.0.0.1", port)
	taskEnv = append(taskEnv, "AGENTWORK_SERVER_URL="+serverURL)

	conn, err := runtime.Open(ctx, runtime.Spec{
		Transport: transport, Executable: execPath, Args: args, Endpoint: endpoint, Env: rtEnv,
		Cwd: runRowWorkdir, // the processor's scratch dir — see Spec.Cwd
	}, taskEnv)
	if err != nil {
		d.failProcessorRun(ctx, q, "open transport: "+err.Error())
		return
	}
	defer conn.Close()

	// Unified execution model (DESIGN.md 决策 4-8): a processor run gets the
	// same client execution environment + MCP workspace server + Workspace
	// contract as a worker run. Without the client handler the ACP handshake
	// declares no fs/terminal capabilities and the processor agent's
	// write/shell tools are rejected ("agent→client RPC not configured") —
	// the compile run cannot land its checks.json artifact (a live failure).
	env := newRunEnvironment(q.RunID, "", q.AgentID, runRowWorkdir, serverURL)
	defer env.tm.cleanup()
	d.mu.Lock()
	mcpExec := mcp.NewExecutor(runRowWorkdir, env.runEnv(nil), env.tm)
	mcpExec.SetCollaboration(q.GoalID, q.AgentID, q.RunID, d.commentSvc, d.goalSvc, d.runSvc, d.agentSvc, d.squadSvc)
	d.mcpExecs[q.RunID] = mcpExec
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.mcpExecs, q.RunID)
		d.mu.Unlock()
	}()
	// Processor runs are one-shot (no goal session) — the full contract.
	prompt += worktreeGuidance(runRowWorkdir, true)
	// The artifact's ABSOLUTE path: a processor's scratch dir is opaque to the
	// agent — told to "write intake.json in the current directory", it guessed
	// (a write_file call missing its path argument, then a raw shell heredoc
	// that terminal_create cannot execute — the command must be an
	// executable). State the file's full path so write_file's required path
	// argument is unambiguous.
	prompt += fmt.Sprintf("\n\nThe artifact file's ABSOLUTE path: %s\n(Write to this exact path with agentwork_write_file's path argument; do NOT guess the working directory, do NOT use shell redirection.)\n",
		filepath.Join(runRowWorkdir, "intake.json"))

	backend, err := d.protoReg.Get(provider)
	if err != nil {
		d.failProcessorRun(ctx, q, "provider "+provider+": "+err.Error())
		return
	}
	// Time bounds, same as worker runs (a compile agent stuck on a slow pip
	// index was running UNBOUNDED — no maxRunDuration, no idle watchdog, so
	// the run sat "running" forever and the page showed 编译中… indefinitely;
	// a live 15+ minute hang on pypi.org). The domain's max_run_duration is
	// the budget (default 2h); the idle watchdog cuts silent stalls, and the
	// terminal manager's activeCount widens its window like a worker run.
	maxRunDuration := 7200
	if domainID != "" {
		var d2 int
		_ = d.st.DB().QueryRowContext(ctx, `SELECT max_run_duration FROM domain WHERE id=?`, domainID).Scan(&d2)
		if d2 > 0 {
			maxRunDuration = d2
		}
	}
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var inFlightTools atomic.Int32
	procCtx, procCancel := context.WithTimeout(ctx, time.Duration(maxRunDuration)*time.Second)
	defer procCancel()
	go d.runIdleWatchdog(procCtx, &lastActivity, &inFlightTools, procCancel, q.RunID, env.tm.activeCount)

	run, err := backend.Execute(procCtx, proto.ExecuteSpec{
		Conn:          conn,
		Cwd:           runRowWorkdir,
		Prompt:        prompt,
		ClientHandler: env,
		McpServers: append([]acp.McpServer{{
			Type: "http", Name: "agentwork", URL: serverURL + "/mcp/" + q.RunID,
		}}, d.extraMcpServers(ctx, q.AgentID)...),
	})
	if err != nil {
		d.failProcessorRun(ctx, q, "execute: "+err.Error())
		return
	}
	for ev := range run.Events {
		lastActivity.Store(time.Now().UnixNano())
		d.persistEvent(ctx, q.RunID, ev)
		d.trackToolInflight(&inFlightTools, ev)
		d.bus.Publish(ctx, events.Event{Topic: "run:event", Payload: map[string]any{
			"run_id": q.RunID, "event": ev,
		}})
	}
	result, ok := <-run.Result
	if !ok {
		result = proto.Result{Status: proto.StatusFailed, Output: "backend closed result channel"}
	}
	d.flushRunMessages(ctx, q.RunID)
	switch result.Status {
	case proto.StatusCompleted:
		// Read the compiled policy from the run workdir (file = structured
		// side effect), validate, and store on the domain unfrozen.
		checksPath := filepath.Join(runRowWorkdir, "checks.json")
		raw, err := os.ReadFile(checksPath)
		if err != nil {
			d.failProcessorRun(ctx, q, "read checks.json: "+err.Error())
			return
		}
		var checks service.Checks
		if err := json.Unmarshal(raw, &checks); err != nil {
			d.failProcessorRun(ctx, q, "parse checks.json: "+err.Error())
			return
		}
		if len(checks.Verify) == 0 && len(checks.Guards) == 0 {
			d.failProcessorRun(ctx, q, "checks.json: no verify or guards produced")
			return
		}
		strength := "medium"
		if sraw, err := os.ReadFile(filepath.Join(runRowWorkdir, "strength.txt")); err == nil {
			if v := strings.TrimSpace(string(sraw)); v == "strong" || v == "medium" || v == "weak" {
				strength = v
			}
		}
		checksJSON, _ := json.Marshal(checks)
		// The compiled policy ALWAYS lands (a fresh compile cycle replaces
		// the previous one wholesale — DESIGN.md §5.3), and resets the
		// freeze stamp: the domain returns to the pending-confirmation state
		// so the owner's confirmation card reappears with the NEW product.
		// (Regression: the old UPDATE was gated on checks_compiled_at='',
		// which made a recompile AFTER freezing a silent no-op — the new
		// product was discarded and runs kept verifying with the old policy.)
		//
		// The evolution-metrics baseline (decision 2-15) is recorded alongside
		// (metrics.json — test count / coverage the processor measured at
		// compile time). Only the FIRST compile stamps the baseline: later
		// recompiles refresh the policy, not the evolution baseline.
		baseline := "{}"
		if raw, err := os.ReadFile(filepath.Join(runRowWorkdir, "metrics.json")); err == nil {
			var m struct {
				TestCount int     `json:"test_count"`
				Coverage  float64 `json:"coverage"`
			}
			if json.Unmarshal(raw, &m) == nil && (m.TestCount > 0 || m.Coverage > 0) {
				b, _ := json.Marshal(map[string]any{"test_count": m.TestCount, "coverage": m.Coverage})
				baseline = string(b)
			}
		}
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE domain SET checks=?, verification_strength=?, checks_compiled_at='',
			        metrics_baseline=CASE WHEN metrics_baseline='{}' OR metrics_baseline='' THEN ? ELSE metrics_baseline END
			 WHERE id=?`,
			string(checksJSON), strength, baseline, domainID); err != nil {
			d.failProcessorRun(ctx, q, "store compiled checks: "+err.Error())
			return
		}
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE run SET status='completed', result_summary=?, finished_at=? WHERE id=?`,
			result.Output, nowStr(), q.RunID); err != nil {
			logging.Infof("daemon: finish processor run %s: %v", q.RunID, err)
		}
		d.bus.Publish(ctx, events.Event{Topic: "domain:compiled", Payload: map[string]any{
			"domain_id": domainID, "run_id": q.RunID,
		}})
		logging.Infof("daemon: domain %s acceptance policy compiled (strength=%s)", domainID, strength)
	case proto.StatusCancelled:
		d.failProcessorRun(ctx, q, "idle watchdog: "+result.Output)
	case proto.StatusFailed, proto.StatusAborted:
		d.failProcessorRun(ctx, q, result.Output)
	}
}

// failProcessorRun marks a processor run failed and notifies the frontend that
// compilation did not complete (manual checks input remains the fallback).
func (d *Daemon) failProcessorRun(ctx context.Context, q *service.ClaimedRow, summary string) {
	logging.Infof("daemon: processor run %s failed: %s", q.RunID, summary)
	// P0-5: the stamp is conditional — a run the runaway reaper already
	// terminalized keeps the reaper's terminal state; this late failure is
	// dropped (and must not broadcast a stale compile-failed event).
	res, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET status='failed', result_summary=?, finished_at=? WHERE id=? AND status='running'`,
		summary, nowStr(), q.RunID)
	if err != nil {
		logging.Infof("daemon: mark processor run %s failed: %v", q.RunID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		logging.Infof("daemon: processor run %s already terminal — dropping late failure", q.RunID)
		return
	}
	var domainID string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT domain_id FROM run WHERE id=?`, q.RunID).Scan(&domainID)
	d.bus.Publish(ctx, events.Event{Topic: "domain:compile_failed", Payload: map[string]any{
		"run_id": q.RunID, "domain_id": domainID, "error": summary,
	}})
}

// worktreeGuidance is the execution-environment contract (DESIGN.md 决策
// 4-8/6-2) injected into EVERY run's prompt — worker AND processor alike (the
// unified model: every run registers the client handler, declares
// capabilities, and gets this section; a processor run without it cannot
// write its file artifact — a live failure: the compile agent's write/shell
// tools were rejected with "agent→client RPC not configured" and checks.json
// never landed). It names the platform's own tools (deterministic); the
// agent's own toolset is its business — the contract is the boundary.
func worktreeGuidance(workdir string, firstContact bool) string {
	b := fmt.Sprintf(`
## Workspace

Your worktree lives on the PLATFORM machine — it is not your environment:

- Worktree root: %s
- It contains the repository code and AGENTWORK.md, the coordination guide — read it first`, workdir)
	if !firstContact {
		// The contract is unchanged from the agent's previous turn on this
		// goal — AGENTWORK.md carries it; the per-run facts above are the
		// only variables (决策 6-20).
		b += `
- The collaboration contract (MCP tools, access channels, read-only rules) is UNCHANGED from your previous turn — the full contract lives in AGENTWORK.md; the platform tools are the same ones you already used
- Commands that touch the worktree run through the platform's execution channel, on the platform machine, with the worktree as their working directory
`
		return b
	}
	b += `
- COLLABORATE through the platform's MCP collaboration tools — the four behaviors (agentwork_comment_goal / agentwork_consult_agent / agentwork_handoff_goal / agentwork_create_sub_goal), integration and inspection (agentwork_integrate_change / agentwork_get_change / agentwork_get_sub_goal / agentwork_get_verification / agentwork_cancel_sub_goal / agentwork_verify_sub_goal), and the lists (agentwork_goal_list / agentwork_agent_list / agentwork_squad_list) — the full coordination contract lives in AGENTWORK.md
- ACCESS THE WORKTREE ONLY THROUGH THE PLATFORM'S CHANNELS:
  * MCP server "agentwork" (advertised at session start) — its tools operate on the worktree: agentwork_read_file (read a file), agentwork_write_file (write a file), agentwork_edit_file (edit a file), agentwork_list_dir (list a directory), agentwork_grep (search the codebase), and the command trio agentwork_terminal_create → agentwork_terminal_output → agentwork_terminal_release (commands are ASYNC: create returns a terminal id immediately, poll output until exited=true passing the returned cursor back, then release to clean up)
  * Client capabilities over ACP — fs/read_text_file, fs/write_text_file, terminal/*
  Your own local file/shell tools operate on YOUR environment — NOT the worktree. On a remote runtime your local tools cannot reach the worktree at all; locally they only happen to work when the working directory points at it
- Commands that touch the worktree run through the platform's execution channel (agentwork_terminal_create or terminal/*), on the platform machine, with the worktree as their working directory
- Verification, review and delivery read only what you wrote through these channels
`
	return b
}

// nowStr is the daemon-side UTC timestamp helper (service.now is private).
func nowStr() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// trackToolInflight maintains the in-flight-tool counter the idle watchdog
// reads to decide whether to use idleWindow or the larger idleToolWindow.
func (d *Daemon) trackToolInflight(n *atomic.Int32, ev proto.Event) {
	switch ev.Type {
	case proto.EventToolUse:
		n.Add(1)
	case proto.EventToolResult:
		n.Add(-1)
		if v := n.Load(); v < 0 {
			n.Store(0)
		}
	}
}

// persistEvent stores one protocol event into chat_message (the run detail
// view's data source). Consecutive text/thought chunks from the same role
// are AGGREGATED into one row (an ACP stream emits per-token chunks — a raw
// per-chunk insert produced 20k+ rows per run and destroyed transcript
// replay quality); tool events flush the pending buffer first.
func (d *Daemon) persistEvent(ctx context.Context, runID string, ev proto.Event) {
	switch ev.Type {
	case proto.EventMessage, proto.EventThought:
		role := "assistant"
		if ev.Type == proto.EventThought {
			role = "thought"
		}
		d.mu.Lock()
		b := d.msgBuffers[runID]
		if b == nil || b.role != role {
			d.flushMsgBuffer(ctx, runID)
			b = &msgBuffer{role: role}
			d.msgBuffers[runID] = b
		}
		b.content += ev.Text
		d.mu.Unlock()
	case proto.EventToolUse, proto.EventToolResult:
		d.mu.Lock()
		d.flushMsgBuffer(ctx, runID)
		d.mu.Unlock()
		tc, _ := json.Marshal(ev)
		d.insertChatMessage(ctx, runID, "tool", "", string(tc))
	default:
		d.insertChatMessage(ctx, runID, "assistant", ev.Text, "[]")
	}
}

// msgBuffer is the pending aggregated text row for a run (see persistEvent).
type msgBuffer struct {
	role    string
	content string
}

// flushMsgBuffer writes the pending aggregated text row (if any) for a run.
// Caller holds d.mu.
func (d *Daemon) flushMsgBuffer(ctx context.Context, runID string) {
	b := d.msgBuffers[runID]
	if b == nil || b.content == "" {
		return
	}
	d.insertChatMessage(ctx, runID, b.role, b.content, "[]")
	delete(d.msgBuffers, runID)
}

// flushRunMessages flushes and forgets a run's pending buffer (run end).
func (d *Daemon) flushRunMessages(ctx context.Context, runID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushMsgBuffer(ctx, runID)
	delete(d.msgBuffers, runID)
}

func (d *Daemon) insertChatMessage(ctx context.Context, runID, role, content, toolCalls string) {
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO chat_message (id, run_id, role, content, tool_calls, created_at) VALUES (?,?,?,?,?,?)`,
		uuid.NewString(), runID, role, content, toolCalls, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		logging.Infof("daemon: persist event for run %s: %v", runID, err)
	}
}

// extraMcpServers loads the agent's configured EXTRA MCP servers — the
// agent's own tools (browser, database, an external ACP agent via an MCP
// bridge, ...). The platform's workspace server is always advertised first;
// a config typo is logged and skipped, never fatal to the run.
func (d *Daemon) extraMcpServers(ctx context.Context, agentID string) []acp.McpServer {
	var raw string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT mcp_servers FROM agent WHERE id=?`, agentID).Scan(&raw); err != nil {
		return nil
	}
	var extra []acp.McpServer
	if err := json.Unmarshal([]byte(raw), &extra); err != nil {
		logging.Infof("daemon: agent %s mcp_servers parse: %v", agentID, err)
		return nil
	}
	return extra
}

func (d *Daemon) loadAgentEnv(ctx context.Context, agentID string) (map[string]string, error) {
	var envJSON string
	err := d.st.DB().QueryRowContext(ctx, `SELECT env FROM agent WHERE id=?`, agentID).Scan(&envJSON)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(envJSON), &m)
	return m, nil
}

// failRun records a launch-time failure (before any backend ran). Failed runs
// still flow through Finish → reconcile so the goal layer can retry/fail the
// goal authoritatively.
func (d *Daemon) failRun(ctx context.Context, q *service.ClaimedRow, summary string) {
	logging.Infof("daemon: run %s failed at launch: %s", q.RunID, summary)
	d.finishRun(ctx, q, "failed", summary)
}

// finishRunOK ends a successful run.
func (d *Daemon) finishRunOK(ctx context.Context, q *service.ClaimedRow, output string) {
	d.finishRun(ctx, q, "completed", output)
}

// finishRun writes the run's terminal status + hands the outcome to the goal
// layer. The goal layer (ReconcileOnRunEnd) is the SOLE authority over
// goal.status; the daemon never writes goal.status directly.
// Returns false when the stamp was refused — the run was already
// terminalized by another writer (the runaway reaper, or the handoff
// claim→register stamp) and this LATE outcome is dropped (P0-5, 决策 6-15⑥).
// Callers gate their terminal events on the return: only the writer whose
// stamp won publishes (the reaper publishes its own).
func (d *Daemon) finishRun(ctx context.Context, q *service.ClaimedRow, status, summary string) bool {
	err := d.runSvc.Finish(ctx, q.RunID, status, summary)
	if errors.Is(err, service.ErrRunAlreadyTerminal) {
		logging.Infof("daemon: run %s already terminal — dropping late %s result", q.RunID, status)
		return false
	}
	if err != nil {
		logging.Infof("daemon: finish run %s: %v", q.RunID, err)
	}
	return true
}

// goalOwnsSquadStatus mirrors multica's ownsIssueStatus: a leader run may only
// push the goal to done when the goal is assigned to THIS squad (DESIGN.md
// §9). A guest @mentioned squad gets the "do NOT change status" briefing.
func (d *Daemon) goalOwnsSquadStatus(ctx context.Context, goalID, squadID string) bool {
	var at, aid string
	err := d.st.DB().QueryRowContext(ctx, `SELECT assignee_type, assignee_id FROM goal WHERE id=?`, goalID).Scan(&at, &aid)
	if err != nil {
		return false
	}
	return at == "squad" && aid == squadID
}

// leaderSquadFor reports the squad the goal belongs to when the given agent
// is its CURRENT leader (dynamic — judged at run time, not from a static
// run mark).
func (d *Daemon) leaderSquadFor(ctx context.Context, goalID, agentID string) (string, bool) {
	var atype, aid string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT assignee_type, assignee_id FROM goal WHERE id=?`, goalID).Scan(&atype, &aid)
	if err != nil || atype != "squad" || aid == "" {
		return "", false
	}
	var leaderID string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT leader_id FROM squad WHERE id=?`, aid).Scan(&leaderID); err != nil {
		return "", false
	}
	return aid, leaderID == agentID
}

// annotatePolicyIssue posts ONE system comment flagging an objective
// acceptance-policy defect (a verify command/script that does not exist —
// POSIX exit 127) — deduped per goal so the retry chain does not spam the
// same finding. The owner sees it in the feed and fixes the policy; the run
// still fails normally (a report is not a waiver).
func (d *Daemon) annotatePolicyIssue(ctx context.Context, goalID string) {
	// Platform text is English (决策 6-18) — system notifications stay fixed;
	// the goal's language lives in the MATERIALS (agents' comments follow it
	// via the LANGUAGE rule).
	var n int
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system' AND content LIKE '⚠️ Likely acceptance-policy problem%'`, goalID).Scan(&n)
	if n > 0 {
		return
	}
	content := "⚠️ Likely acceptance-policy problem: a verification command/script does not exist (POSIX exit 127) — check the domain's acceptance policy and fix it, then reopen the task; agents cannot bypass this check."
	_, _ = d.st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
		uuid.NewString(), goalID, content, nowStr())
}

// agentTriggeredRunCount counts the goal's runs triggered by AGENT-authored
// comments (the mention-churn signal; platform system triggers excluded).
// Sub-goal/verify runs are EXEMPT (P2-2, 决策 6-15⑩): their trigger is the
// owner's dispatch comment — workflow execution, not mention churn. Review
// runs too (决策 6-19): platform-enqueued, anchored on the parking run's
// agent-authored report — the review round is the approval window's
// evidence, never churn. Same query as service.MentionCycleCount; keep them
// in lockstep.
func (d *Daemon) agentTriggeredRunCount(ctx context.Context, goalID string) (int, error) {
	var n int
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run r JOIN comment c ON c.id = r.trigger_comment_id
		 WHERE r.goal_id=? AND c.author_type='agent'
		   AND r.role NOT IN ('subgoal','verify','review')`, goalID).Scan(&n)
	return n, err
}

// consultStatus renders the owner's resolved consults since its last turn —
// the causal answer to "did my question come back?" (P1-3, 决策 6-15⑨). The
// query is CAUSAL: consult_request links requester → guest → response comment
// (response_comment_id is back-filled at guest run end); the finished_at
// comparison is only the "since your last turn" scope filter. An owner
// waking mid-consult cannot happen — the finalization guard holds the goal
// active until its OWN consults resolve (P0-6) — so every row here is
// resolved.
func (d *Daemon) consultStatus(ctx context.Context, goalID, agentID, runID string) string {
	rows, err := d.st.DB().QueryContext(ctx, `
		SELECT COALESCE(a.name, cr.target_agent_id), COALESCE(c.content, ''), COALESCE(rc.content, ''), r.status
		FROM consult_request cr
		JOIN run r ON r.id = cr.guest_run_id
		LEFT JOIN comment c ON c.id = cr.trigger_comment_id
		LEFT JOIN comment rc ON rc.id = cr.response_comment_id
		LEFT JOIN agent a ON a.id = cr.target_agent_id
		WHERE cr.goal_id=? AND cr.requester_agent_id=?
		  AND r.status IN ('completed','failed','cancelled')
		  AND r.finished_at > COALESCE((
		    SELECT MAX(r2.finished_at) FROM run r2
		    WHERE r2.goal_id=? AND r2.role='owner' AND r2.id != ?
		  ), '')
		ORDER BY r.finished_at`, goalID, agentID, goalID, runID)
	if err != nil {
		logging.Infof("daemon: consult status for %s: %v", goalID, err)
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var name, question, answer, status string
		if err := rows.Scan(&name, &question, &answer, &status); err != nil {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\n\n## Consult status (your questions since your last turn)\n")
		}
		if status == "completed" && answer != "" {
			fmt.Fprintf(&b, "- 问 %s：%s\n  回答：%s\n", name, truncateIn(question, 300), truncateIn(answer, 1000))
		} else {
			fmt.Fprintf(&b, "- 问 %s：%s\n  未成功（%s）——详见评论区\n", name, truncateIn(question, 300), status)
		}
	}
	return b.String()
}

// goalAttention reads the persisted OwnerAttention (” when none).
func (d *Daemon) goalAttention(ctx context.Context, goalID string) string {
	var a string
	_ = d.st.DB().QueryRowContext(ctx, `SELECT attention FROM goal WHERE id=?`, goalID).Scan(&a)
	return a
}

// commentsInjection renders the goal's FULL collaboration feed as the
// prompt's comment section — every comment, human AND agent authors, no
// count limit, in time order. This is the collaboration-chain guarantee
// (DESIGN.md 决策 4-6): an agent pulled in by another agent's mention must
// see what was asked of it — the earlier human-only + LIMIT 5 filter broke
// exactly that chain. Compression (决策 4-7, budget-based summarization) is
// designed but not implemented; full-feed injection is the guarantee until
// then.
func (d *Daemon) commentsInjection(ctx context.Context, goalID string) string {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT c.author_type, COALESCE(a.name, ''), c.content FROM comment c
		 LEFT JOIN agent a ON a.id = c.author_id
		 WHERE c.goal_id=?
		 ORDER BY c.created_at ASC`, goalID)
	if err != nil {
		logging.Infof("daemon: load comments for run prompt (goal %s): %v", goalID, err)
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var at, name, content string
		if rows.Scan(&at, &name, &content) == nil && strings.TrimSpace(content) != "" {
			// The feed is read by HUMANS and agents alike — the author label
			// must say WHO spoke: "user" for the human, the agent's NAME for
			// agents (bare "agent：" hides which teammate said what — the PM's
			// dispatch vs the coder's report became indistinguishable).
			label := at
			switch at {
			case "human":
				label = "user"
			case "agent":
				label = name
			case "system":
				label = "system"
			}
			if label == "" {
				label = at
			}
			b.WriteString("- " + label + "：" + content + "\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\n## Goal comments (the full collaboration feed)\n" + b.String()
}

// buildPrompt assembles the opening prompt for a run turn.
func buildPrompt(title, desc, handoff string) string {
	body := desc
	if strings.TrimSpace(body) == "" {
		// A sub-goal created via `goal create` may carry only a title. Without a
		// body the agent gets an empty task and may not terminate; give it an
		// explicit, completable instruction so the run reaches a terminal state.
		body = "Complete this sub-task. Do the work it implies, then finish your turn."
	}
	guide := "Read AGENTWORK.md in the working directory first — your identity, the team roster and the collaboration contract for this run live there."
	if handoff != "" {
		// A handoff/wakeup note scopes THIS turn. It is placed AHEAD of the
		// original description, which is now *context* (not a fresh to-do
		// list). This prevents the sub-goal loop: a woken parent that sees the
		// child-summary note must NOT blindly re-execute the original
		// description's "create a sub-task" steps. If the note reports the work
		// already complete, the agent ends its turn rather than fanning out
		// again.
		return guide + "\n\n" +
			"Task: " + title + "\n\n" +
			"Context (what this goal is about; do NOT blindly redo these steps):\n" + body + "\n\n" +
			"Scope for THIS run (follow the note; do not redo steps it describes as done):\n> " + handoff + "\n\n" +
			"If the note reports the work is already complete, do NOT start new work — end your turn immediately.\n"
	}
	// A title that IS the description (both trimmed) renders ONCE — the live
	// "Task: 看看README有没有问题\n\n看看README有没有问题" duplication is
	// pure noise for every reader of the prompt.
	if strings.TrimSpace(title) == strings.TrimSpace(body) {
		return guide + "\n\n" + fmt.Sprintf("Task: %s", title)
	}
	return guide + "\n\n" + fmt.Sprintf("Task: %s\n\n%s", title, body)
}

// buildAgentGuide writes the "## Team & Coordination" block that every run's
// AGENTWORK.md gets: the roster of teammates plus the coordination reference.
// This is the agent's only source of truth for how to coordinate — without
// it the agent doesn't know the collaboration tools exist, and it would
// never guess the mention:// URI format. See DESIGN.md §2.
//
// The block is TRIMMED by role (决策 6-20): the owner gets the full contract
// (four behaviors + Change pipeline); sub-goal/verify runs get their own
// output contract only; review/consult runs get the read-only answer
// contract. Trimming removes only the sections a role can never act on — a
// member reading "HANDOFF: OWNER ONLY" is dead text, and dead text invites
// dead tool calls (the permission layer rejects them, at token cost).
func (d *Daemon) buildAgentGuide(ctx context.Context, selfAgentID, role string) string {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT id, name, description FROM agent ORDER BY name`)
	if err != nil {
		logging.Errorf("daemon: build team roster: %v", err)
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	b.WriteString("## Team & Coordination\n\n")
	b.WriteString("You coordinate ONLY through the MCP tools of the \"agentwork\" server\n")
	b.WriteString("(advertised at session start) — structured side effects, never shell,\n")
	b.WriteString("never file edits to communicate intent.\n\n")
	b.WriteString("LANGUAGE: write every comment/report in the SAME language as the goal's\n")
	b.WriteString("description and the human's messages (do not switch to English when\n")
	b.WriteString("echoing tool output).\n\n")

	switch role {
	case "owner":
		b.WriteString("### The four behaviors (pick by intent)\n")
		b.WriteString("- COMMENT: agentwork_comment_goal(content) — say something DURING your\n")
		b.WriteString("  run (progress, findings, discussion). Never triggers another run.\n")
		b.WriteString("  Anyone may. NOTE: your final message automatically becomes your run's\n")
		b.WriteString("  report in the feed when you end your turn — do NOT re-post the same\n")
		b.WriteString("  summary with comment_goal (double reports are feed noise).\n")
		b.WriteString("- CONSULT: agentwork_consult_agent(agent_id, question) — ask for\n")
		b.WriteString("  information/judgment. A READ-ONLY guest run answers; the answer lands\n")
		b.WriteString("  in the feed and the platform auto-resumes YOUR next run. OWNER ONLY.\n")
		b.WriteString("  Never wait inside a run — ask, end your turn.\n")
		b.WriteString("- HANDOFF: agentwork_handoff_goal(assignee_type, assignee_id, reason) —\n")
		b.WriteString("  transfer this goal's ownership. END YOUR TURN right after; the platform\n")
		b.WriteString("  cuts your run and starts the new owner's. OWNER ONLY.\n")
		b.WriteString("- SUB-GOAL: agentwork_create_sub_goal(title, assignee_id, verifier_id?) —\n")
		b.WriteString("  split a work item off THIS goal (not a new goal). It runs on its own\n")
		b.WriteString("  branch with machine verification; a verified sub-goal produces a Change\n")
		b.WriteString("  and the platform wakes YOU to integrate. A sub-goal completing does NOT\n")
		b.WriteString("  complete the goal. OWNER ONLY. The platform writes the dispatch comment\n")
		b.WriteString("  for you — do NOT announce the delegation again in a comment. Your FINAL\n")
		b.WriteString("  message becomes your run report in the feed: after a dispatch-only turn\n")
		b.WriteString("  keep it minimal, and never write agent ids into it (humans read it).\n\n")

		b.WriteString("### Work the Change pipeline (owner)\n")
		b.WriteString("- Woken with an Owner Attention section? agentwork_get_change lists the\n")
		b.WriteString("  changes; agentwork_integrate_change(change_id) merges each ready one\n")
		b.WriteString("  into your worktree. A conflict wakes the assignee to rework\n")
		b.WriteString("  automatically — wait for the new Revision.\n")
		b.WriteString("- Inspect: agentwork_get_sub_goal / agentwork_get_verification; cancel a\n")
		b.WriteString("  stuck item with agentwork_cancel_sub_goal(sub_goal_id).\n\n")

		b.WriteString("### Team roster\n")
		b.WriteString("Delegating work = agentwork_create_sub_goal or agentwork_handoff_goal —\n")
		b.WriteString("never a mention in a comment (mentions are read-only consults).\n\n")
	case "subgoal":
		b.WriteString("### Your output\n")
		b.WriteString("- Do the work item. Do NOT comment progress noise; your final message\n")
		b.WriteString("  becomes your report when you end your turn.\n")
		b.WriteString("- The platform machine-verifies your branch and produces a Change; the\n")
		b.WriteString("  OWNER integrates it. You do not create sub-goals, hand off, or\n")
		b.WriteString("  integrate — those are the owner's tools.\n\n")
	case "review":
		b.WriteString("### Your output\n")
		b.WriteString("- Review ONLY — give your opinion. Your final message becomes your\n")
		b.WriteString("  opinion in the feed (the approver reads it there); do NOT re-post it\n")
		b.WriteString("  with agentwork_comment_goal.\n\n")
	case "consult":
		b.WriteString("### Your output\n")
		b.WriteString("- Answer the question — READ-ONLY. Your final message becomes your\n")
		b.WriteString("  answer in the feed (the requester reads it there); do NOT re-post it\n")
		b.WriteString("  with agentwork_comment_goal.\n\n")
	case "verify":
		b.WriteString("### Verify (verify runs only)\n")
		b.WriteString("- Judge the work item, then issue agentwork_verify_sub_goal(verdict,\n")
		b.WriteString("  summary, evidence) ONCE and end your turn.\n\n")
	}

	b.WriteString("### Lists\n")
	b.WriteString("- agentwork_goal_list / agentwork_agent_list / agentwork_squad_list —\n")
	b.WriteString("  resolve uuids with agent_list / squad_list before consults and handoffs.\n")
	b.WriteString("- agentwork_get_comments(goal_id?, after?, limit?) — READ-ONLY: pull NEW\n")
	b.WriteString("  comments during a long run (your prompt snapshot is fixed at claim;\n")
	b.WriteString("  pass the last comment id you saw as `after` for incremental reads).\n\n")

	b.WriteString("### Team roster\n")
	var n int
	for rows.Next() {
		var id, name, desc string
		if err := rows.Scan(&id, &name, &desc); err != nil {
			continue
		}
		if id == selfAgentID {
			continue
		}
		fmt.Fprintf(&b, "- %s (id: %s)", name, id)
		if desc != "" {
			b.WriteString(" — ")
			b.WriteString(desc)
		}
		b.WriteByte('\n')
		n++
	}
	if n == 0 {
		b.WriteString("(you are the only agent — no teammates to delegate to)\n")
	}
	return b.String()
}

// injectAgentProfile writes the agent's system prompt, team roster, and (for
// leader runs) squad briefing into {workdir}/AGENTWORK.md so the subprocess
// discovers its identity via its native config file.
func (d *Daemon) injectAgentProfile(workdir, systemPrompt, roster, briefing string) error {
	var b strings.Builder
	if systemPrompt != "" {
		b.WriteString(systemPrompt)
		b.WriteString("\n\n")
	}
	if briefing != "" {
		b.WriteString(briefing)
		b.WriteString("\n\n")
	}
	b.WriteString(roster)
	return os.WriteFile(filepath.Join(workdir, "AGENTWORK.md"), []byte(b.String()), 0o644)
}

// runIdleWatchdog cancels a Prompt if the agent emits nothing for idleWindow
// (or idleToolWindow while a tool is in flight or a terminal command is
// running). Terminal polling (Agent→Client RPC) never appears on the event
// stream — an agent waiting on `npm test` is silent to the daemon, so an
// in-flight terminal widens the budget exactly like an in-flight tool.
// It ticks at window/2.
func (d *Daemon) runIdleWatchdog(parent context.Context, lastActivity *atomic.Int64, inFlightTools *atomic.Int32, cancel context.CancelFunc, runID string, activeTerms func() int) {
	interval := idleWindow / 2
	if interval <= 0 {
		interval = idleWindow
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-ticker.C:
			threshold := idleWindow
			if inFlightTools.Load() > 0 || (activeTerms != nil && activeTerms() > 0) {
				threshold = idleToolWindow
			}
			last := time.Unix(0, lastActivity.Load())
			if time.Since(last) < threshold {
				continue
			}
			logging.Infof("daemon: idle watchdog firing for run %s (silent %s), force-stopping", runID, time.Since(last).Round(time.Second))
			d.mu.Lock()
			d.runCancelReasons[runID] = "idle_watchdog"
			d.mu.Unlock()
			cancel()
			return
		}
	}
}

// ── runaway reaper (P1, 决策 6-15⑦) ──

// reapRunawayRuns is the DB-level runaway reaper: the process-side kill
// chain (promptCtx → conn.Close → SIGKILL) is best-effort and can hang — a
// run left 'running' forever blocks the goal's owner single-flight (a new
// owner's run never claims). The reaper is the deterministic backstop: it
// terminalizes the DB row itself, so Claim's guards release regardless of
// whether the process ever dies.
//
// Threshold: the run's OWN budget (per-domain max_run_duration) + grace —
// the run's context timeout already fires at 1×; surviving past it means the
// cancellation chain broke (2× would just double the wait). The kill is
// best-effort and never blocks; the LATE-result guard (P0-5: Finish stamps
// conditionally on status='running') keeps a zombie process from
// overwriting this terminal state, and the run:cancelled event publishes
// exactly once (runTask's cancelled branch gates its publish on winning the
// stamp).
func (d *Daemon) reapRunawayRuns(ctx context.Context) {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT id, goal_id, domain_id, started_at FROM run WHERE status='running' AND started_at != ''`)
	if err != nil {
		logging.Errorf("daemon: runaway scan: %v", err)
		return
	}
	type runawayRow struct{ id, goalID, domainID, startedAt string }
	var candidates []runawayRow
	for rows.Next() {
		var rr runawayRow
		if err := rows.Scan(&rr.id, &rr.goalID, &rr.domainID, &rr.startedAt); err != nil {
			continue
		}
		candidates = append(candidates, rr)
	}
	rows.Close()

	for _, rr := range candidates {
		started, err := time.Parse(time.RFC3339Nano, rr.startedAt)
		if err != nil {
			continue
		}
		budget := d.runBudgetSeconds(ctx, rr.goalID, rr.domainID)
		if time.Since(started) < time.Duration(budget)*time.Second+runawayGrace {
			continue
		}
		// Stamp FIRST (conditional — the stamp is the authority; a lost race
		// means another writer terminalized it), then best-effort cut.
		res, err := d.st.DB().ExecContext(ctx,
			`UPDATE run SET status='cancelled', cancel_reason='runaway', finished_at=? WHERE id=? AND status='running'`,
			nowStr(), rr.id)
		if err != nil {
			logging.Infof("daemon: runaway stamp %s: %v", rr.id, err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // already terminal — someone else's business
		}
		var rGoalTitle string
		if rr.goalID != "" {
			_ = d.st.DB().QueryRowContext(ctx, `SELECT title FROM goal WHERE id=?`, rr.goalID).Scan(&rGoalTitle)
		}
		logging.Infof("daemon: runaway run %s (running %s, budget %ds) — goal %q (%s)", rr.id, time.Since(started).Round(time.Second), budget, rGoalTitle, rr.goalID)
		// Best-effort process cut through the cancel registry (the claim→
		// register window is covered: runTask's post-register self-check sees
		// the stamp and self-cancels).
		d.mu.Lock()
		cancel, ok := d.runCancels[rr.id]
		if ok {
			d.runCancelReasons[rr.id] = "runaway"
		}
		d.mu.Unlock()
		if ok {
			cancel()
		}
		if rr.goalID != "" {
			// The notify layer turns this into the 任务中断 card — the owner
			// must know the run was killed over budget (决策 2-6: the goal
			// stays active, the human decides).
			d.bus.Publish(ctx, events.Event{Topic: "run:cancelled", Payload: map[string]any{
				"run_id": rr.id, "goal_id": rr.goalID, "reason": "runaway", "reason_code": "runaway",
			}})
		} else if rr.domainID != "" {
			// A runaway COMPILE run must unstick the domain's compile panel
			// (it waits on domain:compile_failed — the same event the normal
			// failure path emits).
			d.bus.Publish(ctx, events.Event{Topic: "domain:compile_failed", Payload: map[string]any{
				"run_id": rr.id, "domain_id": rr.domainID, "error": "runaway: 编译超过 max_run_duration 被终止",
			}})
		}
	}
}

// runBudgetSeconds resolves a running run's own time budget: the goal's
// domain max_run_duration (worker runs), the processor run's domain, or the
// platform default (7200s).
func (d *Daemon) runBudgetSeconds(ctx context.Context, goalID, domainID string) int {
	var budget int
	if goalID != "" {
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT COALESCE(d.max_run_duration, 7200) FROM domain d JOIN goal g ON g.domain_id = d.id WHERE g.id=?`,
			goalID).Scan(&budget)
	} else if domainID != "" {
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT COALESCE(max_run_duration, 7200) FROM domain WHERE id=?`, domainID).Scan(&budget)
	}
	if budget <= 0 {
		budget = 7200
	}
	return budget
}

// ── schedule dispatch ──

// dispatchSchedules fires every enabled schedule whose next_run_at is due,
// cloning a fresh goal + run. Idempotent via uq_schedule_run_planned.
func (d *Daemon) dispatchSchedules(ctx context.Context) {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT id, title_template, description, assignee_type, assignee_id, domain_id, cron_expression, timezone, next_run_at
		 FROM schedule WHERE enabled=1 AND next_run_at != '' AND next_run_at <= ?`, nowStr)
	if err != nil {
		logging.Errorf("daemon: schedule query: %v", err)
		return
	}
	var due []scheduleDueRow
	for rows.Next() {
		var r scheduleDueRow
		if err := rows.Scan(&r.ScheduleID, &r.TitleTemplate, &r.Description, &r.AssigneeType, &r.AssigneeID, &r.DomainID, &r.CronExpression, &r.Timezone, &r.NextRunAt); err != nil {
			rows.Close()
			logging.Errorf("daemon: schedule scan: %v", err)
			return
		}
		due = append(due, r)
	}
	rows.Close()
	for _, r := range due {
		d.fireSchedule(ctx, r)
	}
}

type scheduleDueRow struct {
	ScheduleID, TitleTemplate, Description, AssigneeType, AssigneeID, DomainID, CronExpression, Timezone, NextRunAt string
}

func (d *Daemon) fireSchedule(ctx context.Context, r scheduleDueRow) {
	plannedAt := r.NextRunAt
	goalID := uuid.NewString()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := d.st.DB().BeginTx(ctx, nil)
	if err != nil {
		logging.Infof("daemon: schedule %s begin tx: %v", r.ScheduleID, err)
		return
	}
	defer tx.Rollback()
	// 11 columns → exactly 11 values: id,title,description,domain_id,
	// assignee_type,assignee_id as parameters; status='active',
	// handoff_note='', created_by_type='system' literal; created_by_id=
	// schedule id, created_at=ts as parameters.
	if err := insertFiredGoal(ctx, tx, goalID, r.TitleTemplate, r.Description, r.DomainID, r.AssigneeType, r.AssigneeID, r.ScheduleID, ts); err != nil {
		logging.Infof("daemon: schedule %s insert goal: %v", r.ScheduleID, err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schedule_run (id,schedule_id,goal_id,planned_at,status,created_at) VALUES (?,?,?,?,'dispatched',?)`,
		uuid.NewString(), r.ScheduleID, goalID, plannedAt, ts); err != nil {
		if service.IsSQLiteUniqueViolation(err) {
			// A concurrent tick already fired this planned_at. Roll the tx
			// back EXPLICITLY before the advance UPDATE below — the advance
			// runs on a separate connection, and an open tx would contend on
			// the write lock (and deadlock single-connection test stores).
			_ = tx.Rollback()
			logging.Infof("daemon: schedule %s already fired at %s, skipping", r.ScheduleID, plannedAt)
			d.advanceScheduleNextRun(ctx, r, plannedAt)
			return
		}
		// A REAL insert error: do NOT advance — the next tick retries this
		// firing instead of silently swallowing the failure and skipping it.
		logging.Infof("daemon: schedule %s insert schedule_run failed (not advanced): %v", r.ScheduleID, err)
		return
	}
	// P0-3 (决策 6-13): the fired goal and its FIRST run are born in ONE
	// transaction — a crash after the commit can no longer leave a run-less
	// active goal.
	_, runEv, err := d.runSvc.EnqueueForGoalTx(ctx, tx, service.Goal{
		ID: goalID, AssigneeType: r.AssigneeType, AssigneeID: r.AssigneeID,
	})
	if err != nil {
		logging.Infof("daemon: schedule %s enqueue run: %v", r.ScheduleID, err)
		return
	}
	if err := tx.Commit(); err != nil {
		logging.Infof("daemon: schedule %s commit: %v", r.ScheduleID, err)
		return
	}
	if runEv != nil {
		d.bus.Publish(ctx, *runEv)
	}
	d.advanceScheduleNextRun(ctx, r, plannedAt)
	d.bus.Publish(ctx, events.Event{Topic: "schedule:fired", Payload: map[string]any{
		"schedule_id": r.ScheduleID, "goal_id": goalID, "planned_at": plannedAt,
	}})
	logging.Infof("daemon: schedule %s fired, created goal %s", r.ScheduleID, goalID)
}

func (d *Daemon) advanceScheduleNextRun(ctx context.Context, r scheduleDueRow, plannedAt string) {
	anchor, err := time.Parse(time.RFC3339Nano, plannedAt)
	if err != nil {
		anchor = time.Now().UTC()
	}
	next, err := service.ComputeNextRun(r.CronExpression, r.Timezone, anchor)
	if err != nil {
		logging.Infof("daemon: schedule %s advance cron: %v", r.ScheduleID, err)
		return
	}
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE schedule SET next_run_at=?, last_run_at=? WHERE id=?`,
		next.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), r.ScheduleID); err != nil {
		logging.Infof("daemon: schedule %s advance next_run_at: %v", r.ScheduleID, err)
	}
}
