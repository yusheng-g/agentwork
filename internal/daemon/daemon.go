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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/logging"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/gitutil"
	"github.com/eushing/agentwork/internal/issue"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
	"github.com/google/uuid"
)

// DaemonVersion is the agentwork-daemon build version, echoed in register
// results — the CLI and the daemon warn each other on a mismatch (protocol
// drift between an old binary and a new daemon surfaces at connect time).
// var (not const): the release build stamps the real version via
// -ldflags "-X <pkg>.DaemonVersion=$AGENTWORK_COMPILE_VERSION" (build.sh)
// so the CLI and the daemon ship one shared version stamp.
var DaemonVersion = "0.0.1-beta.1"

// dispatchTickInterval is how often the daemon claims queued runs. Claims are
// per-agent (only within the set of agents with free worker slots), so this
// bounds perceived latency without hot-looping.
const dispatchTickInterval = 500 * time.Millisecond

// scheduleTickInterval is how often the daemon scans schedule for due firings.
const scheduleTickInterval = 5 * time.Second

// scheduleFireTTL is how long a schedule-fired run may sit queued before the
// fire is a miss ("cron: miss = miss"): a fire born while its machine was
// offline must not execute stale work hours later.
const scheduleFireTTL = 30 * time.Minute

// scheduleStaleSweepInterval is how often the daemon expires queued schedule
// fires past the TTL.
const scheduleStaleSweepInterval = 2 * time.Minute

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
	teamImportSvc *service.TeamImportService // team-import processor run lifecycle

	mu          sync.Mutex
	workers     map[string]*agentWorker // agentID → per-agent scheduler
	domainLocks map[string]*domainLock  // per-domain git lock (fetch + deliver)
	// sessionPool is the persistent (agent, goal) session pool (决策 6-21).
	// Its own mutex (sessionMu) guards it — see session_pool.go.
	msgBuffers   map[string]*msgBuffer          // runID → aggregated text row (persistEvent)
	toolBuffers  map[string]map[string]*toolRow // runID → CallID → aggregated tool row (persistEvent)
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
	stopped  bool
	ctx      context.Context

	// Machine link (CLI 分支 Phase 2): live /connect peers keyed by machine
	// id, plus the per-run upload bookkeeping (event seq / last activity)
	// for gap detection and the dispatched-run watchdog. Guarded by
	// machineMu / machineLastEventMu.
	machineMu     sync.Mutex
	machinePeers  map[string]*link.Peer
	machineLastEventMu sync.Mutex
	machineLastSeq     map[string]int64
	machineLastEvent   map[string]time.Time
	machineRunMachine  map[string]string // runID → the machine it was dispatched to (ownership anchor for report ingestion)
	// PULL dispatch (multica-style): the daemon never writes dispatches
	// over the link — the machine's run.poll serves from these queues.
	machinePendingMu sync.Mutex
	machinePending   map[string][]*pendingRun // machineID → queued dispatches
	machineCancels   map[string][]link.RunCancelParams
	// machinePollWake is the long-poll wake signal per machine: a chan closed
	// (and replaced) when a dispatch or cancel is enqueued, so a held
	// DequeueMachineDispatchWait returns immediately instead of waiting out
	// the 30s timeout. Guarded by machinePendingMu.
	machinePollWake map[string]chan struct{}

	// chat is the ACP chat relay (Phase 6): web sockets ↔ machine chat
	// channels, frames unparsed.
	chat chatRelay
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
		toolBuffers:       make(map[string]map[string]*toolRow),
		runCancels:        make(map[string]context.CancelFunc),
		runCancelReasons:  make(map[string]string),
		reviewReadyTimers: make(map[string]*time.Timer),
		reviewReadyFired:  make(map[string]bool),
		machinePeers:      make(map[string]*link.Peer),
		machineLastSeq:    make(map[string]int64),
		machineRunMachine: make(map[string]string),
		machinePending:    make(map[string][]*pendingRun),
		machineCancels:    make(map[string][]link.RunCancelParams),
		machinePollWake:   make(map[string]chan struct{}),
		machineLastEvent:  make(map[string]time.Time),
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
	// M4-B: a failed issue-sourced goal relays the agent's own terminal
	// summary as a comment on its issue (NOT a close — the work was not
	// delivered, the issue stays open). Without this the issue's author sees
	// the issue sit open forever with no signal: success closed + commented,
	// failure was silent. The comment carries the agent's words verbatim — no
	// platform branding, no goal id; the bot account's display name is the
	// identity on the host, the way a developer replies on an issue.
	// agent_ran (true only when the failing run actually started an agent
	// session) gates the writeback: a launch/infra failure produced a
	// platform diagnostic, not agent output — relaying it would dump machine
	// log on the issue's author, who cannot act on it. Silence there is
	// honest; the infra side is visible in daemon logs. done is NOT handled
	// here — a delivered issue-sourced goal goes through goal:delivered above
	// (close + commit links), and a finished-done without a deliver has no
	// merge to point at.
	bus.Subscribe("goal:finished", func(_ context.Context, e events.Event) {
		m, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		goalID, _ := m["goal_id"].(string)
		status, _ := m["status"].(string)
		summary, _ := m["summary"].(string)
		agentRan, _ := m["agent_ran"].(bool)
		if goalID == "" {
			return
		}
		if status == "failed" && agentRan {
			d.issueCloser.OnTerminal(context.Background(), goalID, summary)
		}
	})
	// M4-B: an agent's --ask question on an issue-sourced goal is mirrored
	// onto the issue so the author sees it on the git host (where they live),
	// not only inside agentwork. The agent's question is relayed verbatim —
	// the same as a developer commenting "I need X to proceed" on an issue.
	// Non-issue goals no-op (Closer.resolveIssueTarget returns false). The
	// notify subscription (Feishu card) runs independently for platform users.
	bus.Subscribe("comment:agent_question", func(_ context.Context, e events.Event) {
		m, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		goalID, _ := m["goal_id"].(string)
		question, _ := m["question"].(string)
		if goalID != "" {
			d.issueCloser.OnAgentQuestion(context.Background(), goalID, question)
		}
	})
	return d
}

// SetTeamImportService wires the team-import processor-run lifecycle (called
// after construction to avoid touching the already-long New signature).
func (d *Daemon) SetTeamImportService(svc *service.TeamImportService) {
	d.teamImportSvc = svc
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
	staleTick := time.NewTicker(scheduleStaleSweepInterval)
	cleanupTick := time.NewTicker(worktreeCleanupInterval)
	digestTick := time.NewTicker(digestTickInterval)
	issueTick := time.NewTicker(issuePollInterval)
	runawayTick := time.NewTicker(runawayScanInterval)
	d.runLogTailer(ctx)
	defer dispatchTick.Stop()
	defer scheduleTick.Stop()
	defer staleTick.Stop()
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
		case <-staleTick.C:
			d.expireStaleScheduledRuns(ctx)
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

// expireStaleScheduledRuns closes schedule fires that never dispatched
// because their machine was offline past the TTL.
func (d *Daemon) expireStaleScheduledRuns(ctx context.Context) {
	n, err := d.runSvc.ExpireStaleScheduledRuns(ctx, time.Now().Add(-scheduleFireTTL))
	if err != nil {
		logging.Infof("daemon: expire stale scheduled runs: %v", err)
	} else if n > 0 {
		logging.Infof("daemon: expired %d stale scheduled run(s) (queued past %s — machine offline at fire time)", n, scheduleFireTTL)
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
	// Handoff (决策 6-21): the old owner's session holds the goal branch's
	// single checkout — close it so the new owner's session can take it.
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
	// Terminal run (completed/failed/cancelled): the run is already stopped.
	// Return success (idempotent) — the human's click is a no-op, not an
	// error. Without this guard a stop on a completed run fell through to
	// the running branch: enqueueMachineCancel leaked a cancel to the machine
	// queue for an already-terminal run, and the UPDATE...WHERE status='running'
	// matched nothing, surfacing a 400 "is no longer running" to the web (the
	// "点取消没反应" symptom — the run had ended, but the goal stayed active
	// under the ask-hold so the runs panel still showed it).
	if status == "completed" || status == "failed" || status == "cancelled" {
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
		// Machine-dispatched runs (CLI 分支 Phase 2): the local cancel
		// registry knows nothing about them — send run.cancel over the
		// machine's link, then stamp cancelled locally (the stamp is the
		// authority; the machine's late report is dropped as
		// ErrRunAlreadyTerminal).
		var machineID string
		_ = d.st.DB().QueryRowContext(d.ctx,
			`SELECT r.machine_id FROM run ru JOIN agent a ON a.id=ru.agent_id JOIN runtime r ON r.id=a.runtime_id WHERE ru.id=?`,
			runID).Scan(&machineID)
		if machineID != "" {
			// PULL model: the cancel rides the machine's next poll.
			d.enqueueMachineCancel(machineID, link.RunCancelParams{RunID: runID, Reason: "stopped"})
			logging.Infof("daemon: human stopped machine run %s (cancel queued for %s)", runID, machineID)
		}
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

// ContinueGoal resumes a paused goal (决策 4-12 延伸): the human stopped the
// running run, the goal stayed active with no agent working. This enqueues a
// fresh owner run carrying a pause-resume wake note so the owner picks up its
// worktree state rather than restarting. The wake note is the ONLY signal —
// the cancelled run's summary is platform noise ("cancelled"), not the
// agent's work, so it is NOT injected (unlike handoff/reject memory); the
// owner's persistent workdir + the feed are its real memory. Returns nil for
// review/terminal/human goals (EnqueueOwnerRun's no-op contract).
func (d *Daemon) ContinueGoal(goalID string) (*service.Run, error) {
	const wakeNote = "You were paused by the user. Pick up your worktree state where you left it — pull the comment feed with `agentwork goal comments` if you need to recheck the latest coordination, then continue. Do NOT start over."
	return d.goalSvc.EnqueueOwnerRun(d.ctx, goalID, wakeNote)
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
// Delegates to gitutil (shared with the agentwork CLI — Phase 3).
func sanitizeURL(raw string) string { return gitutil.SanitizeURL(raw) }

// gitCloneURL embeds the domain's credentials into an HTTPS clone URL
// (github token-as-username / gitcode oauth2:). Delegates to gitutil
// (shared with the agentwork CLI — Phase 3).
func gitCloneURL(gitURL, credentials string) string { return gitutil.CloneURL(gitURL, credentials) }

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
	// First boot: the runs root does not exist yet — create it instead of
	// logging a spurious sweep error (live: a fresh daemon reported
	// "read runs root: no such file or directory").
	if err := os.MkdirAll(runsRoot(), 0o755); err != nil {
		logging.Errorf("daemon: sweep run worktrees: mkdir runs root: %v", err)
		return
	}
	repoRoot := filepath.Join(runsRoot(), "repos")
	// Run worktrees live directly under runsRoot() as <runID>/ dirs, alongside
	// the named siblings (repos/, proc/, scratch/, deliver-*). Sweep only the
	// worktree dirs — the reserved sibling names are skipped.
	reserved := map[string]bool{
		"repos": true, "proc": true, "scratch": true, "sessions": true,
	}
	// Sessions die with the process — their worktrees (runs/sessions/<goal>/
	// <agent>) are stale on restart and would hold branch checkouts hostage.
	if err := os.RemoveAll(filepath.Join(runsRoot(), "sessions")); err != nil {
		logging.Errorf("daemon: sweep session worktrees: %v", err)
	} else {
		logging.Infof("daemon: swept stale session worktrees")
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
	var title, desc, handoff, domainID, gitURL, defaultBranch, domainType, domainName, systemPrompt, argsJSON, rtEnvJSON, sourceRef, gitCredentials, gitIdentity, runtimeMachineID string
	var agentName, triggerAuthorName string
	var triggerAuthor, triggerCommentID, triggerCommentContent, runRole, subGoalID, wakeNote, wakeAnchorID string
	var maxConcurrent, maxRunDuration int
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT g.title, g.description, g.handoff_note, d.id, d.git_url, d.default_branch, COALESCE(d.type,''), COALESCE(d.name,''), a.system_prompt, a.name, d.git_identity,
		        r.args, r.env, COALESCE(r.machine_id,''), a.max_concurrent, d.max_run_duration,
		        g.source_ref, d.git_credentials,
		        r2.trigger_comment_id, COALESCE(c.author_type, ''), COALESCE(c.content, ''), COALESCE(ca.name,''), r2.role, r2.sub_goal_id, r2.wake_note, COALESCE(r2.wake_anchor,'')
		 FROM run r2
		 JOIN goal g ON g.id = r2.goal_id
		 LEFT JOIN domain d ON d.id = g.domain_id
		 JOIN agent a ON a.id = r2.agent_id
		 JOIN runtime r ON r.id = a.runtime_id
		 LEFT JOIN comment c ON c.id = r2.trigger_comment_id
		 LEFT JOIN agent ca ON ca.id = c.author_id
		 WHERE r2.id = ?`, q.RunID).
		Scan(&title, &desc, &handoff, &domainID, &gitURL, &defaultBranch, &domainType, &domainName, &systemPrompt, &agentName, &gitIdentity, &argsJSON, &rtEnvJSON, &runtimeMachineID, &maxConcurrent, &maxRunDuration, &sourceRef, &gitCredentials, &triggerCommentID, &triggerAuthor, &triggerCommentContent, &triggerAuthorName, &runRole, &subGoalID, &wakeNote, &wakeAnchorID)
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
	if err != nil {
		d.failRun(ctx, q, fmt.Sprintf("load config: %v", err))
		return
	}
	// The GOAL title survives the sub-goal override below — the fixed
	// context block always names the goal; the wake line names the work item.
	goalTitle := title
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
	// Parse runtime args + env.
	var args []string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		d.failRun(ctx, q, fmt.Sprintf("parse args: %v", err))
		return
	}
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(rtEnvJSON), &rtEnv)
	agentEnv, _ := d.loadAgentEnv(ctx, q.AgentID)

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

		var issueSection string
		if issueRepo != "" {
			// The run's goal tracks a PUBLIC issue on the git host. Anything
			// the agent posts there with `agentwork issue comment` is read by
			// humans outside the platform — the contract rides EVERY run that
			// can touch the issue, not the user-written persona (which is
			// easy to leave unconfigured).
			issueSection = "## Source issue (public)\n" +
				"This goal tracks a PUBLIC issue (" + issueRepo + "#" + issueNumber + ") on the git host. " +
				"Comments you post there with `agentwork issue comment` are read by people outside the platform — " +
				"write professionally and completely (current status / plan / blockers / conclusions), " +
				"never mention platform-internal ids or process details.\n"
		}
		if len(issueComments) > 0 {
			var ib strings.Builder
			ib.WriteString("\n## Latest issue conversation (from the remote)\n")
			for _, cm := range issueComments {
				fmt.Fprintf(&ib, "- %s：%s\n", cm.User.Login, truncateIn(cm.Body, 300))
			}
			issueSection += ib.String()
		}

	// The domain's acceptance policy in NL (the "what counts as done" the
	// OWNER defined) — the agent works toward it instead of finding out at
	// verification time. Only the NL intent is injected; the compiled checks
	// stay invisible (triangle separation: define stays with the human,
	// execute with the agent, judge with the machine+human).
	var policyText string
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT d.policy_text FROM goal g JOIN domain d ON d.id=g.domain_id WHERE g.id=?`, q.GoalID).Scan(&policyText)

	// CLI 分支 Phase 2: machine-owned runtimes execute on their registered
	// machine — assemble the SAME engineered prompt and dispatch it over
	// the link. This goroutine's job ends at the ack; the run's lifecycle
	// continues via the machine's claimed/event/finished uploads.
	if runtimeMachineID != "" {
		dispatchEnv := map[string]string{}
		for k, v := range rtEnv {
			dispatchEnv[k] = v
		}
		for k, v := range agentEnv {
			dispatchEnv[k] = v
		}
		if issueRepo != "" {
			dispatchEnv["AGENTWORK_ISSUE_REPO"] = issueRepo
			dispatchEnv["AGENTWORK_ISSUE_NUMBER"] = issueNumber
		}
		machinePrompt := d.assemblePrompt(ctx, q, promptInputs{
			agentName: agentName, systemPrompt: systemPrompt, runRole: runRole,
			goalTitle: goalTitle, policyText: policyText, domainType: domainType, domainName: domainName,
			triggerCommentID: triggerCommentID, triggerAuthor: triggerAuthor, triggerAuthorName: triggerAuthorName,
			triggerCommentContent: triggerCommentContent, title: title, desc: desc, handoff: handoff,
			wakeNote: wakeNote, wakeAnchorID: wakeAnchorID, subGoalID: subGoalID,
			consultRun: consultRun, reviewRun: reviewRun, verifyRun: verifyRun, subGoalRun: subGoalRun,
		})
		// The stored prompt is the TASK message only — the fixed block and
		// the team ride the workdir's AGENTS.md (buildRunProfile).
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE run SET prompt=? WHERE id=?`, machinePrompt, q.RunID); err != nil {
			logging.Infof("daemon: run %s: store prompt: %v", q.RunID, err)
		}
		// The resume pointer is a WRITABLE-role privilege: an ACP session
		// has ONE writer at a time — owner/subgoal runs load their own
		// (goal/subgoal, agent) session (single-flight serializes them).
		// Read-only roles (consult/review/verify) answer from a FRESH
		// session and must NEVER load a live session (they'd collide with
		// its writer — the old code sent the pointer to every role and was
		// only saved by the workdir-mismatch branch).
		priorSession, priorWorkdir := "", ""
		if runRole == "owner" || runRole == "subgoal" {
			priorSession, priorWorkdir = d.priorSessionFor(ctx, q.GoalID, q.AgentID, subGoalID)
		}
		d.dispatchToMachine(ctx, q, link.RunDispatchParams{
			RunID: q.RunID, GoalID: q.GoalID, AgentID: q.AgentID,
			Role: runRole, SubGoalID: subGoalID, Attempt: q.Attempt,
			Token: q.Token, Prompt: machinePrompt,
			Scratch: domainType == "scratch", DomainName: domainName,
			DomainID: domainID, GitURL: gitURL, GitCredentials: gitCredentials,
			DefaultBranch: defaultBranch, GitIdentity: gitIdentity,
			ACPSpawn: args, Env: dispatchEnv,
			ProjectSkillsDir: d.projectSkillsDirFor(ctx, runtimeMachineID, args),
			RunProfile:       d.buildRunProfile(ctx, q.GoalID, q.AgentID, agentName, runRole, goalTitle, policyText, domainType, domainName, issueSection),
			PriorSessionID:   priorSession,
			PriorWorkDir:     priorWorkdir,
		}, runtimeMachineID)
		return
	}

	// The unified execution model (CLI 分支): every run dispatches to its
	// machine over the /connect link. Legacy transports have no executor
	// anymore — fail with a pointer instead of pretending to run.
	d.failRun(ctx, q, "this runtime has no machine — run `agentwork connect` and point the agent at a machine-owned runtime")
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

	// The team-import run is a repo-domain processor run (same clone +
	// worktree path as compile); only the artifact file differs.
	artifactFiles := []string{"checks.json", "strength.txt", "metrics.json"}
	if runType == "import" {
		artifactFiles = []string{"team.json"}
	}

	var argsJSON, rtEnvJSON, procMachineID string
	var maxConcurrent int
	err = d.st.DB().QueryRowContext(ctx,
		`SELECT r.args, r.env, COALESCE(r.machine_id,''), a.max_concurrent
		 FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`, agentID).
		Scan(&argsJSON, &rtEnvJSON, &procMachineID, &maxConcurrent)
	if err != nil {
		d.failProcessorRun(ctx, q, "load agent runtime: "+err.Error())
		return
	}
	d.ensureWorker(agentID, maxConcurrent)

	// CLI 分支 Phase 2: machine-owned processor runtimes dispatch — the
	// machine works the proc dir (repo compile: a detached worktree of
	// origin/<default>) and uploads the artifact files with run.finished.
	if procMachineID != "" {
		// Git config source differs by run type: import runs read from the
		// team_import tracking row (no domain/project is created); compile
		// runs read from the domain row.
		var dType, gitURL, gitCredentials, defaultBranch string
		if runType == "import" {
			if d.teamImportSvc != nil {
				gitURL, gitCredentials, defaultBranch, _ = d.teamImportSvc.GitConfigForRun(ctx, q.RunID)
			}
			if gitURL == "" {
				logging.Warnf("daemon: import run %s has no git config — team_import row missing", q.RunID)
			}
		} else {
			_ = d.st.DB().QueryRowContext(ctx,
				`SELECT COALESCE(type,''), git_url, git_credentials, default_branch FROM domain WHERE id=?`, domainID).
				Scan(&dType, &gitURL, &gitCredentials, &defaultBranch)
		}
		var args []string
		_ = json.Unmarshal([]byte(argsJSON), &args)
		var rtEnv map[string]string
		_ = json.Unmarshal([]byte(rtEnvJSON), &rtEnv)
		agentEnv, _ := d.loadAgentEnv(ctx, agentID)
		dispatchEnv := map[string]string{}
		for k, v := range rtEnv {
			dispatchEnv[k] = v
		}
		for k, v := range agentEnv {
			dispatchEnv[k] = v
		}
		d.dispatchToMachine(ctx, q, link.RunDispatchParams{
			RunID: q.RunID, AgentID: q.AgentID, Attempt: q.Attempt, Token: q.Token,
			Prompt: prompt, Proc: true, Scratch: dType == "scratch",
			ArtifactFiles: artifactFiles,
			DomainID: domainID, GitURL: gitURL, GitCredentials: gitCredentials, DefaultBranch: defaultBranch,
			ACPSpawn: args, Env: dispatchEnv,
		}, procMachineID)
		return
	}

	// Legacy transports have no executor anymore (the unified model
	// dispatches everything to machines).
	d.failProcessorRun(ctx, q, "this runtime has no machine — run `agentwork connect` and point the agent at a machine-owned runtime")
}

// storeProcessorArtifacts completes a compile run from its FILE artifacts
// (checks.json + strength.txt + metrics.json) — the shared path for the
// local execution and the machine-dispatched upload (CLI 分支 Phase 2).
// The platform reads structured side effects, never agent stdout.
func (d *Daemon) storeProcessorArtifacts(ctx context.Context, q *service.ClaimedRow, domainID string, files map[string]string, summary string) {
	if files == nil {
		files = map[string]string{}
	}
	raw, ok := files["checks.json"]
	if !ok || strings.TrimSpace(raw) == "" {
		d.failProcessorRun(ctx, q, "checks.json: artifact missing — the compile agent did not produce it")
		return
	}
	var checks service.Checks
	if err := json.Unmarshal([]byte(raw), &checks); err != nil {
		d.failProcessorRun(ctx, q, "parse checks.json: "+err.Error())
		return
	}
	if len(checks.Verify) == 0 && len(checks.Guards) == 0 {
		d.failProcessorRun(ctx, q, "checks.json: no verify or guards produced")
		return
	}
	strength := "medium"
	if sraw, ok := files["strength.txt"]; ok {
		if v := strings.TrimSpace(sraw); v == "strong" || v == "medium" || v == "weak" {
			strength = v
		}
	}
	checksJSON, _ := json.Marshal(checks)
	// The compiled policy ALWAYS lands (a fresh compile cycle replaces the
	// previous one wholesale — DESIGN.md §5.3), and resets the freeze
	// stamp: the domain returns to the pending-confirmation state so the
	// owner's confirmation card reappears with the NEW product.
	//
	// The evolution-metrics baseline (decision 2-15) is recorded alongside
	// (metrics.json — test count / coverage the processor measured at
	// compile time). Only the FIRST compile stamps the baseline.
	baseline := "{}"
	if raw, ok := files["metrics.json"]; ok {
		var m struct {
			TestCount int     `json:"test_count"`
			Coverage  float64 `json:"coverage"`
		}
		if json.Unmarshal([]byte(raw), &m) == nil && (m.TestCount > 0 || m.Coverage > 0) {
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
		summary, nowStr(), q.RunID); err != nil {
		logging.Infof("daemon: finish processor run %s: %v", q.RunID, err)
	}
	d.bus.Publish(ctx, events.Event{Topic: "domain:compiled", Payload: map[string]any{
		"domain_id": domainID, "run_id": q.RunID,
	}})
	logging.Infof("daemon: domain %s acceptance policy compiled (strength=%s)", domainID, strength)
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

// priorSessionFor resolves the (session, workdir) a previous WRITABLE run
// of the same goal (or sub-goal) recorded — the multica-style resume
// pointer carried in the dispatch. The machine resumes only when its
// computed workdir matches PriorWorkDir.
//
// Only a COMPLETED run's session is a safe resume target: a cancelled run
// (handoff cut, watchdog, timeout) may carry a session POISONED by a killed
// turn — the CLI's persisted history can hold an empty assistant message
// that fails every future prompt (the executor's fresh-fallback exists for
// exactly this, but resuming a known-poisoned session is a wasted failure +
// memory loss). When no completed run exists yet, the pointer stays empty
// and the next run starts fresh — preferable to resuming a poisoned session.
func (d *Daemon) priorSessionFor(ctx context.Context, goalID, agentID, subGoalID string) (string, string) {
	var sessionID, workdir string
	if subGoalID != "" {
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT session_id, workdir FROM run WHERE sub_goal_id=? AND agent_id=? AND role='subgoal' AND status='completed' AND session_id != '' ORDER BY finished_at DESC LIMIT 1`,
			subGoalID, agentID).Scan(&sessionID, &workdir)
	} else {
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT session_id, workdir FROM run WHERE goal_id=? AND agent_id=? AND role='owner' AND status='completed' AND session_id != '' ORDER BY finished_at DESC LIMIT 1`,
			goalID, agentID).Scan(&sessionID, &workdir)
	}
	return sessionID, workdir
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
// view's data source) and returns the event to broadcast over WS run:event
// (the live panels' real-time feed). Consecutive text/thought chunks from
// the same role are AGGREGATED into one row (an ACP stream emits per-token
// chunks — a raw per-chunk insert produced 20k+ rows per run and destroyed
// transcript replay quality); tool events flush the pending buffer first.
//
// The returned event keeps the WS broadcast in sync with the DB aggregation:
// a tool_use that is an input-accumulation update (same CallID, row already
// exists) returns a zero event — the live stream already saw the call start,
// and the growing input has no value to show mid-stream; only the FIRST
// tool_use and the terminal tool_result broadcast. Without this the WS path
// (domains compile stream) showed every update, duplicating tool calls.
func (d *Daemon) persistEvent(ctx context.Context, runID string, ev proto.Event) proto.Event {
	switch ev.Type {
	case proto.EventMessage, proto.EventThought:
		role := "assistant"
		if ev.Type == proto.EventThought {
			role = "thought"
		}
		d.mu.Lock()
		if d.msgBuffers == nil {
			d.msgBuffers = make(map[string]*msgBuffer)
		}
		b := d.msgBuffers[runID]
		if b == nil || b.role != role {
			d.flushMsgBuffer(ctx, runID)
			b = &msgBuffer{role: role}
			d.msgBuffers[runID] = b
		}
		b.content += ev.Text
		d.mu.Unlock()
		// Text/thought chunks broadcast as-is — the live panels append them
		// per chunk (the DB aggregates, but the WS feed is the streaming
		// surface; a buffered append would add latency for no dedup gain
		// since renderRunEvent already truncates each line).
		return ev
	case proto.EventToolUse, proto.EventToolResult:
		// ACP emits MULTIPLE updates per tool call: a start (tool_call, maybe
		// with partial/empty input), input-accumulation updates
		// (tool_call_update), and a terminal update (status=completed/failed
		// with RawOutput → EventToolResult). The pre-aggregation code
		// INSERTed a new chat_message row per update — one tool call became
		// 2–4 rows (the live "tool 出现两遍" symptom).
		//
		// Aggregate by CallID, but KEEP tool_use and tool_result as TWO
		// separate rows: the feed renders them differently (tool_use → the
		// call name + input; tool_result → the output), and collapsing them
		// into one row (the first attempt) overwrote the tool_use row with
		// the tool_result, so the live stream showed outputs with no call
		// names ("tool 调用和输出到哪去了"). Each type aggregates its own
		// updates: multiple tool_use updates (input growing) merge into one
		// tool_use row; the tool_result update lands as its own row. A tool
		// call with no CallID falls back to one-row-per-event. Text is
		// flushed first so a tool call never lands ahead of the assistant
		// text that announced it.
		//
		// WS broadcast: only the FIRST tool_use (the call started) and the
		// tool_result (the output) broadcast; input-accumulation updates
		// return a zero event (the live stream dedups them — see the return
		// at the bottom of this branch).
		d.mu.Lock()
		d.flushMsgBuffer(ctx, runID)
		if d.toolBuffers == nil {
			d.toolBuffers = make(map[string]map[string]*toolRow)
		}
		tbl := d.toolBuffers[runID]
		if tbl == nil {
			tbl = make(map[string]*toolRow)
			d.toolBuffers[runID] = tbl
		}
		// The buffer key separates use from result so they do not collide:
		// CallID+":use" for tool_use, CallID+":result" for tool_result.
		bufKey := ev.CallID
		if bufKey != "" {
			if ev.Type == proto.EventToolResult {
				bufKey += ":result"
			} else {
				bufKey += ":use"
			}
		}
		row := tbl[bufKey]
		if row == nil {
			// First update for this (call, type) — INSERT, then remember the
			// row id so later updates target it. Released from under d.mu
			// (the INSERT hits the DB; holding d.mu across it would serialize
			// every run's tool stream on one lock). Broadcast this event
			// (the call started, or a standalone result landed).
			if ev.CallID == "" {
				d.mu.Unlock()
				tc, _ := json.Marshal(ev)
				d.insertChatMessage(ctx, runID, "tool", "", string(tc))
				return ev
			}
			id := uuid.NewString()
			tc, _ := json.Marshal(ev)
			d.mu.Unlock()
			d.insertChatMessageWithID(ctx, id, runID, "tool", "", string(tc))
			d.mu.Lock()
			tbl[bufKey] = &toolRow{id: id}
			d.mu.Unlock()
			return ev
		}
		// Subsequent update of the SAME type — rewrite the stored row's
		// tool_calls JSON with the latest event (input grew on a tool_use;
		// output may land in pieces on a tool_result). Do NOT broadcast:
		// the live stream already saw this (call, type) on its first update,
		// and the partial growth has no value mid-stream. The terminal
		// tool_result is a DIFFERENT bufKey (":result"), so it still
		// broadcasts on its own first insert above.
		tc, _ := json.Marshal(ev)
		d.mu.Unlock()
		d.updateChatMessageToolCalls(ctx, row.id, string(tc))
		return proto.Event{}
	default:
		d.insertChatMessage(ctx, runID, "assistant", ev.Text, "[]")
		return ev
	}
}

// msgBuffer is the pending aggregated text row for a run (see persistEvent).
type msgBuffer struct {
	role    string
	content string
}

// toolRow is the aggregated chat_message row for ONE tool call, keyed in
// toolRow is the aggregated chat_message row for ONE (CallID, type) pair,
// keyed in toolBuffers by "<callID>:use" / "<callID>:result" (see
// persistEvent). id is the chat_message row id stamped on the first update
// for that pair; subsequent updates of the same type rewrite it in place.
type toolRow struct {
	id string
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

// flushRunMessages flushes and forgets a run's pending buffers (run end).
// Tool rows are already persisted (each update rewrote its row in place);
// the toolBuffers map is just the in-memory index of row ids, dropped here.
func (d *Daemon) flushRunMessages(ctx context.Context, runID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushMsgBuffer(ctx, runID)
	delete(d.msgBuffers, runID)
	delete(d.toolBuffers, runID)
}

func (d *Daemon) insertChatMessage(ctx context.Context, runID, role, content, toolCalls string) {
	d.insertChatMessageWithID(ctx, uuid.NewString(), runID, role, content, toolCalls)
}

// insertChatMessageWithID is insertChatMessage with a caller-supplied row id
// — used by the tool aggregator so subsequent updates for the same tool call
// can target the row (updateChatMessageToolCalls).
func (d *Daemon) insertChatMessageWithID(ctx context.Context, id, runID, role, content, toolCalls string) {
	if _, err := d.st.DB().ExecContext(ctx,
		`INSERT INTO chat_message (id, run_id, role, content, tool_calls, created_at) VALUES (?,?,?,?,?,?)`,
		id, runID, role, content, toolCalls, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		logging.Infof("daemon: persist event for run %s: %v", runID, err)
	}
}

// updateChatMessageToolCalls rewrites a tool row's tool_calls JSON with the
// latest event for that tool call (input grew, or the type flipped to
// tool_result with output). The row id was stamped on the first update.
func (d *Daemon) updateChatMessageToolCalls(ctx context.Context, id, toolCalls string) {
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE chat_message SET tool_calls=? WHERE id=?`,
		toolCalls, id); err != nil {
		logging.Infof("daemon: update tool row %s: %v", id, err)
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
// Occurrences missed while the daemon was down are SKIPPED, not replayed
// (cron semantics — a miss is a miss; see skipMissedOccurrences).
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
		planned, err := time.Parse(time.RFC3339Nano, r.NextRunAt)
		if err == nil && time.Since(planned) > scheduleMissGrace {
			if n, err := d.skipMissedOccurrences(ctx, r); err != nil {
				logging.Infof("daemon: schedule %s skip missed: %v", r.ScheduleID, err)
			} else if n > 0 {
				logging.Infof("daemon: schedule %s skipped %d overdue occurrence(s) (daemon was down) — no replay, resuming at the next future occurrence", r.ScheduleID, n)
			}
			continue
		}
		d.fireSchedule(ctx, r)
	}
}

// scheduleMissGrace is how late a due firing may be before it counts as
// MISSED rather than mere tick lag: the tick runs every 5s, and a restart
// straddling the fire minute costs tens of seconds — within a minute the
// occurrence still fires. Beyond it the daemon was down across the window.
const scheduleMissGrace = time.Minute

// skipMissedOccurrences advances a schedule past its missed windows WITHOUT
// firing: replaying stacked overdue occurrences floods the queue with
// identical stale work items (live: a multi-hour downtime produced three
// "南京当前温度" goals five seconds apart). The schedule resumes at its
// next FUTURE occurrence. Returns how many occurrences were passed over.
func (d *Daemon) skipMissedOccurrences(ctx context.Context, r scheduleDueRow) (int, error) {
	anchor, err := time.Parse(time.RFC3339Nano, r.NextRunAt)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	skipped := 0
	for {
		next, err := service.ComputeNextRun(r.CronExpression, r.Timezone, anchor)
		if err != nil {
			return skipped, err
		}
		if next.After(now) {
			if skipped > 0 {
				if _, err := d.st.DB().ExecContext(ctx,
					`UPDATE schedule SET next_run_at=?, last_run_at=? WHERE id=?`,
					next.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), r.ScheduleID); err != nil {
					logging.Infof("daemon: schedule %s skip advance: %v", r.ScheduleID, err)
				}
			}
			return skipped, nil
		}
		skipped++
		anchor = next
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

// promptInputs carries everything the prompt assembly reads.
type promptInputs struct {
	agentName, systemPrompt, runRole, goalTitle, policyText, domainType, domainName string
	triggerCommentID, triggerAuthor, triggerAuthorName, triggerCommentContent string
	title, desc, handoff, wakeNote, wakeAnchorID, subGoalID string
	consultRun, reviewRun, verifyRun, subGoalRun bool
}

// assemblePrompt renders the complete run prompt — the fixed block (or, for
// a resident session's follow-up wake, the wake line alone) + wake line +
// extras. Shared by the local execution path and the machine dispatch
// (CLI 分支 Phase 2): the machine gets the SAME engineered context.
func (d *Daemon) assemblePrompt(ctx context.Context, q *service.ClaimedRow, in promptInputs) string {
	var wakeWho, wakeAnchor, wakeContent string
	// 决策 7-2: reject wake is distinguished from handoff wake BEFORE the
	// handoff branch fires. A reject successor run carries: no trigger
	// comment, no wake_note, no handoff_note (reject does not write it), and
	// a wake_anchor pointing at the reject comment. The authoritative signal
	// is the goal's latest gate_decision = reject|redirect — without it a
	// bare owner spawn (no trigger/note/handoff) would fall through to the
	// default assignment branch and the owner would start blind. isReject is
	// the ONLY branch that may read "you were rejected" semantics; the
	// handoff branch (in.handoff != "") is mutually exclusive because reject
	// clears handoff_note.
	isReject := false
	if in.runRole == "owner" && in.triggerCommentID == "" && in.wakeNote == "" && in.handoff == "" && in.wakeAnchorID != "" {
		var lastDecision string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT decision FROM gate_decision WHERE goal_id=? ORDER BY decided_at DESC LIMIT 1`,
			q.GoalID).Scan(&lastDecision); err == nil && (lastDecision == "reject" || lastDecision == "redirect") {
			isReject = true
		}
	}
	switch {
	case isReject:
		// Reject wake (决策 7-2): the owner's previous round was rejected by
		// the human — continue from the reject reason, do NOT start over. The
		// wake anchor is the reject comment (passed as wake_anchor from
		// ResolveReview); the content is the reject reason verbatim.
		wakeWho = "the platform"
		wakeAnchor = in.wakeAnchorID
		var rejectContent string
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT content FROM comment WHERE id=?`, in.wakeAnchorID).Scan(&rejectContent)
		if s := strings.TrimSpace(rejectContent); s != "" {
			wakeContent = "> " + s
		} else {
			wakeContent = "Your previous round was rejected. Review the goal's comments for the reason."
		}
	case in.runRole == "owner" && in.triggerCommentID != "" && in.triggerAuthor == "human":
		// A comment-triggered reopen (决策 4-1 修订): the human's follow-up
		// comment IS this turn's ask — same mention shape as a consult.
		wakeWho = "the user"
		wakeAnchor = in.triggerCommentID
		wakeContent = "> " + in.triggerCommentContent
	case in.consultRun:
		wakeWho = in.triggerAuthorName
		if wakeWho == "" {
			wakeWho = "the user"
		}
		wakeAnchor = in.triggerCommentID
		wakeContent = "> " + in.triggerCommentContent
	case in.reviewRun:
		wakeWho = "the platform"
		wakeAnchor = in.triggerCommentID
		wakeContent = "Review the goal's outcome — inspect the diff and the feed."
	case in.verifyRun:
		wakeWho = "the platform"
		wakeContent = "Verify this work item: " + in.title + "\n" + in.desc
	case in.subGoalRun:
		wakeWho = in.triggerAuthorName // the dispatcher (dispatch comment's author)
		if wakeWho == "" {
			wakeWho = "the platform"
		}
		wakeAnchor = in.triggerCommentID
		wakeContent = in.title + "\n\n" + in.desc
	case in.handoff != "":
		wakeWho = "the platform"
		// The handoff/reject note lands as the goal's latest comment — it IS
		// the anchor for the next owner.
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT id FROM comment WHERE goal_id=? ORDER BY created_at DESC LIMIT 1`, q.GoalID).Scan(&wakeAnchor)
		wakeContent = "> " + in.handoff
	case in.wakeNote != "":
		wakeWho = "the platform"
		wakeAnchor = in.wakeAnchorID // the spawn-time anchor (决策 6-22)
		wakeContent = in.wakeNote
	default:
		wakeWho = "the user"
		// The assignment statement: the goal's FIRST comment (the human's
		// creation words) anchors the run.
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT id FROM comment WHERE goal_id=? ORDER BY created_at ASC LIMIT 1`, q.GoalID).Scan(&wakeAnchor)
		if strings.TrimSpace(in.desc) != "" {
			wakeContent = in.desc
		} else {
			wakeContent = in.title
		}
	}
	wakeLine := buildWakeLine(wakeAnchor, wakeWho, wakeContent)


	// Wake extras: sub-goal rework context, the mention-cycle hint, and the
	// resolved-consult answers ride with the wake line.
	var extras strings.Builder
	if in.subGoalRun && in.subGoalID != "" {
		var rejectSummary string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT summary FROM verification_result WHERE sub_goal_id=? AND status='rejected' ORDER BY created_at DESC LIMIT 1`,
			in.subGoalID).Scan(&rejectSummary); err == nil && strings.TrimSpace(rejectSummary) != "" {
			extras.WriteString("\nYour previous round was REJECTED (fix from this — do NOT start over):\n" + rejectSummary + "\n")
		}
		var chStatus string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT status FROM change WHERE sub_goal_id=? ORDER BY created_at DESC LIMIT 1`,
			in.subGoalID).Scan(&chStatus); err == nil && chStatus == "conflict" {
			extras.WriteString("\nYour previous Change CONFLICTED at integration — resolve it against the new integration base; your new Revision replaces the old one.\n")
		}
		if q.Attempt > 1 && !strings.Contains(extras.String(), "REJECTED") {
			var lastFail string
			if err := d.st.DB().QueryRowContext(ctx,
				`SELECT result_summary FROM run WHERE sub_goal_id=? AND status='failed' ORDER BY finished_at DESC LIMIT 1`,
				in.subGoalID).Scan(&lastFail); err == nil && strings.TrimSpace(lastFail) != "" {
				extras.WriteString("\nYour previous round FAILED machine verification (fix the existing code, do NOT start over):\n" + truncateIn(lastFail, 1500) + "\n")
			}
		}
	}
	// Handoff memory (the cross-agent gap): a new owner's ACP session cannot
	// load the previous owner's session (different persona, different
	// persistent workdir keyed by (goal, agent) — the workdir-mismatch branch
	// in the executor drops the pointer). The previous owner's last run report
	// is the only memory that travels across the handoff, so inject it as
	// context — without it the new owner starts blind ("像没有记忆一样").
	// Only a COMPLETED run's report is real work context: a cancelled run's
	// summary is "cancelled by platform" (platform noise, not the agent's
	// words) and a failed run's summary is a verification trace — neither is
	// the "continue from this" handoff the new owner needs, and a cancelled
	// run ordered latest would mask the real completed report below it.
	if in.runRole == "owner" && in.handoff != "" {
		var prevSummary, prevAgentName string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT r.result_summary, COALESCE(a.name, '')
			   FROM run r LEFT JOIN agent a ON a.id = r.agent_id
			  WHERE r.goal_id=? AND r.role='owner' AND r.status='completed' AND r.result_summary != ''
			  ORDER BY r.finished_at DESC LIMIT 1`,
			q.GoalID).Scan(&prevSummary, &prevAgentName); err == nil {
			if s := strings.TrimSpace(prevSummary); s != "" {
				who := "the previous owner"
				if prevAgentName != "" {
					who = prevAgentName
				}
				extras.WriteString("\nPrevious owner's last report (" + who + ", do NOT start over — continue from this):\n" + truncateIn(s, 2000) + "\n")
			}
		}
	} else if isReject {
		// Reject memory (决策 7-2): the owner's OWN previous round was
		// rejected — inject that round's report so the owner fixes from it
		// rather than restarting. The label is "Your previous round was
		// REJECTED" (NOT "Previous owner" — the owner was never changed,
		// only paused for review; the handoff label would mislead the owner
		// into thinking a different agent owned the goal before them).
		// Same query as handoff memory: the latest owner completed run's
		// result_summary is the work context to continue from.
		var prevSummary string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT r.result_summary FROM run r
			  WHERE r.goal_id=? AND r.role='owner' AND r.status='completed' AND r.result_summary != ''
			  ORDER BY r.finished_at DESC LIMIT 1`,
			q.GoalID).Scan(&prevSummary); err == nil {
			if s := strings.TrimSpace(prevSummary); s != "" {
				extras.WriteString("\nYour previous round was REJECTED (fix from this — do NOT start over):\n" + truncateIn(s, 2000) + "\n")
			}
		}
	} else if in.runRole == "owner" && in.triggerAuthor == "human" && in.triggerCommentID != "" {
		// Ask-reply memory (决策 7-3 延伸): the user replied to the owner's
		// previous comment (parent_id → an agent comment — the --ask question
		// or a plain report). The wake line carries the user's reply, but
		// WITHOUT the agent's own previous words the reply is unmoored — "你
		// 认为呢？" answers nothing the owner can see. Inject the parent
		// comment (what the owner said that the user is replying to) so the
		// owner picks up its own thread. This is the agent→human analogue of
		// the reject memory above: the owner's own previous round, continued.
		//
		// The comment id is injected as the anchor (comment <id>) so the owner
		// can re-pull the full thread with `agentwork goal comments --after
		// <id>` if it needs more context than the snippet. No truncation — an
		// --ask question is short, and a report snippet that lost its tail
		// with no way to fetch the full text was worse than a slightly longer
		// prompt (the original truncateIn(s,2000) cut mid-content with no
		// anchor).
		var parentID, parentContent string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT p.id, p.content FROM comment c JOIN comment p ON p.id = c.parent_id
			  WHERE c.id=? AND c.parent_id != '' AND p.author_type='agent'`,
			in.triggerCommentID).Scan(&parentID, &parentContent); err == nil {
			if s := strings.TrimSpace(parentContent); s != "" {
				extras.WriteString(fmt.Sprintf("\nYour previous comment (comment %s — the user is replying to this; pull `agentwork goal comments --after %s` for the full thread if needed):\n> %s\n",
					parentID, parentID, s))
			}
		}
	}
	if n, err := d.agentTriggeredRunCount(ctx, q.GoalID); err == nil && n > service.MaxMentionHints {
		extras.WriteString(fmt.Sprintf("\n⚠️ Collaboration warning: agents have handed this task back and forth %d times. Do NOT hand it off again — finish the remaining work yourself, or end your turn and leave it for a human.\n", n))
	}
	if in.runRole == "owner" {
		if section := d.consultStatus(ctx, q.GoalID, q.AgentID, q.RunID); section != "" {
			extras.WriteString(section)
		}
	}

	// The user message is the TASK and nothing else: the fixed block
	// (background/goal/role contract/tools) and the team ride AGENTS.md
	// (buildRunProfile, shipped in the dispatch payload and merged into the
	// workdir's AGENTS.md at spawn). Every run is a fresh session, so the
	// wake line alone is the whole message.
	return wakeLine + extras.String()
}

