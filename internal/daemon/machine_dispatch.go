package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/service"
)

// machineDispatch is the daemon's half of the machine link (CLI 分支
// Phase 2): a registry of live /connect peers keyed by machine id, the
// run dispatch send, the ingestion of claimed/event-batch/finished
// uploads, and the watchdog for dispatched runs.

// RegisterMachinePeer binds a machine's live /connect peer (the server
// calls this after a successful machine.register). A stale connection for
// the same machine is displaced: the CLI reconnects with the SAME
// machine_id, and without this the old link's deferred unregister would
// later evict the NEW peer (identity-checked below) while its lingering
// frames double-report.
func (d *Daemon) RegisterMachinePeer(machineID string, p *link.Peer) {
	d.machineMu.Lock()
	defer d.machineMu.Unlock()
	if d.machinePeers == nil {
		d.machinePeers = map[string]*link.Peer{}
	}
	if old, ok := d.machinePeers[machineID]; ok && old != p {
		old.Close() // stale link — its unregister will no-op (peer identity)
	}
	d.machinePeers[machineID] = p
}

// UnregisterMachinePeer drops a machine's peer — ONLY if it is still the
// registered one. The peer identity check keeps a dying stale connection
// from evicting the live replacement (the same machine_id reconnecting
// while the old link is still draining).
func (d *Daemon) UnregisterMachinePeer(machineID string, p *link.Peer) {
	d.machineMu.Lock()
	defer d.machineMu.Unlock()
	if cur, ok := d.machinePeers[machineID]; ok && cur == p {
		delete(d.machinePeers, machineID)
	}
}

// MachinePeer returns the machine's live link peer (nil = offline).
func (d *Daemon) MachinePeer(machineID string) *link.Peer {
	d.machineMu.Lock()
	defer d.machineMu.Unlock()
	return d.machinePeers[machineID]
}

// pendingRun is one dispatch queued for a machine's poll.
type pendingRun struct {
	runID  string
	params link.RunDispatchParams
	enqAt  time.Time
}

// pickupTimeout is how long a dispatch waits in the machine's queue — the
// machine polls every second, so 60s rides out transient one-way stalls
// (a lost poll response is retried a second later) while still failing
// fast on a dead link.
const pickupTimeout = 60 * time.Second

// pollHoldTimeout is how long DequeueMachineDispatchWait holds an empty poll
// before returning: long enough to slash idle-poll RPC volume (~30× fewer
// than 1s ticking), short enough that a lost connection is re-detected
// within the heartbeat window. Must be < pickupTimeout so a held poll can't
// mask a dispatch expiry, and < the CLI's per-call timeout (35s) so the
// response always lands inside the client window.
const pollHoldTimeout = 30 * time.Second

// dispatchToMachine QUEUES an assembled run for the machine's poll
// (PULL model — multica-style: the daemon never writes the dispatch over
// the link; the push direction kept dying in one-way link stalls). The
// machine picks it up via run.poll; a dispatch never picked up within
// pickupTimeout fails the run. On a missing machine the run fails with a
// clear reason (the claim gate keeps offline machines from claiming in
// the first place — this is the narrow race where the machine dropped
// between claim and dispatch).
func (d *Daemon) dispatchToMachine(ctx context.Context, q *service.ClaimedRow, p link.RunDispatchParams, machineID string) {
	if d.MachinePeer(machineID) == nil {
		d.failRun(ctx, q, fmt.Sprintf("machine offline — the machine must run `agentwork connect` (machine %s)", machineID))
		return
	}
	// Stamp the ownership anchor BEFORE enqueueing: every /connect report
	// (claimed/events/finished) is checked against run.machine_id — no
	// other machine can complete or pollute this run.
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET machine_id=? WHERE id=? AND status='running'`, machineID, q.RunID); err != nil {
		logging.Infof("dispatch: stamp machine %s on run %s: %v", machineID, q.RunID, err)
	}
	d.machineLastEventMu.Lock()
	d.machineRunMachine[q.RunID] = machineID
	d.machineLastEventMu.Unlock()
	d.machinePendingMu.Lock()
	d.machinePending[machineID] = append(d.machinePending[machineID], &pendingRun{runID: q.RunID, params: p, enqAt: time.Now()})
	d.machinePendingMu.Unlock()
	d.wakeMachinePoll(machineID)
	logging.Infof("dispatch: run %s queued for machine %s (role=%s, attempt=%d — poll pickup)", q.RunID, machineID, p.Role, q.Attempt)
	// Unpicked dispatch must not hang the run: expire it after the
	// pickup window (covers a machine that never polls again). The
	// failure fires ONLY when the entry is still queued — a claimed ack
	// arriving inside the window removes it, and the machineRunMachine
	// map alone is NOT proof of non-pickup (it lives until the run
	// finishes; a claimed run must never be failed here).
	go func() {
		<-time.After(pickupTimeout)
		d.machinePendingMu.Lock()
		removed := false
		for i, pr := range d.machinePending[machineID] {
			if pr.runID == q.RunID {
				d.machinePending[machineID] = append(d.machinePending[machineID][:i], d.machinePending[machineID][i+1:]...)
				removed = true
				break
			}
		}
		d.machinePendingMu.Unlock()
		if !removed {
			return
		}
		// The machine may have picked the dispatch up while the claimed
		// ack was lost — cancel it so the orphan exec stops (a killed
		// turn otherwise leaves poisoned history in the CLI's session
		// store, and the NEXT run's resume dies on it).
		d.enqueueMachineCancel(machineID, link.RunCancelParams{RunID: q.RunID, Reason: "dispatch_timeout"})
		d.failRun(context.Background(), q, "the machine did not pick up the dispatch (60s poll timeout)")
	}()
}

// tryDequeue is the lock-held, non-blocking core of run.poll: sweep expired
// entries (failing them), keep the rest, and return the oldest dispatch +
// queued cancels if any. Returns the result and whether it is non-empty
// (Run != nil OR Cancels present) — the caller uses that to decide whether
// to hold for the long-poll wake or return immediately. The mu is taken
// inside; failRun is called with mu released (it may hit the DB / bus).
func (d *Daemon) tryDequeue(machineID string) (link.RunPollResult, bool) {
	var res link.RunPollResult
	d.machinePendingMu.Lock()
	defer d.machinePendingMu.Unlock()
	var kept []*pendingRun
	var expired []*pendingRun
	for _, pr := range d.machinePending[machineID] {
		if time.Since(pr.enqAt) > pickupTimeout {
			expired = append(expired, pr)
			continue
		}
		kept = append(kept, pr)
	}
	d.machinePending[machineID] = kept
	if len(kept) > 0 {
		res.Run = &kept[0].params
	}
	res.Cancels = d.machineCancels[machineID]
	d.machineCancels[machineID] = nil
	// Fail expired entries with mu released (failRun may publish to the bus).
	d.machinePendingMu.Unlock()
	for _, pr := range expired {
		d.failRun(context.Background(), &service.ClaimedRow{RunID: pr.runID}, "the machine did not pick up the dispatch (60s poll timeout)")
	}
	// Re-acquire for the deferred unlock (the deferred unlock runs on return,
	// so the mutex state must be "held" at this point — re-lock).
	d.machinePendingMu.Lock()
	nonEmpty := res.Run != nil || len(res.Cancels) > 0
	return res, nonEmpty
}

// wakeMachinePoll closes the machine's current wake chan (unblocking any
// held DequeueMachineDispatchWait) and installs a fresh chan for the next
// wait. Called after a dispatch or cancel is enqueued. Guarded by
// machinePendingMu.
func (d *Daemon) wakeMachinePoll(machineID string) {
	d.machinePendingMu.Lock()
	defer d.machinePendingMu.Unlock()
	if ch, ok := d.machinePollWake[machineID]; ok && ch != nil {
		close(ch)
	}
	d.machinePollWake[machineID] = make(chan struct{})
}

// pollWakeChan returns the machine's current wake chan, creating one if
// absent. The caller holds the chan reference and selects on it; a
// wakeMachinePoll call will close it. Guarded by machinePendingMu.
func (d *Daemon) pollWakeChan(machineID string) chan struct{} {
	d.machinePendingMu.Lock()
	defer d.machinePendingMu.Unlock()
	ch, ok := d.machinePollWake[machineID]
	if !ok || ch == nil {
		ch = make(chan struct{})
		d.machinePollWake[machineID] = ch
	}
	return ch
}

// DequeueMachineDispatchWait serves the machine's run.poll with a long-poll
// hold: if nothing is queued, block up to pollHoldTimeout (30s) for a
// dispatch/cancel to arrive — woken immediately by wakeMachinePoll. The
// request ctx cancels the hold on connection loss (no leaked goroutines).
// Protocol-compatible with the old DequeueMachineDispatch: an old CLI that
// polls every 1s gets immediate empty returns (tryDequeue is non-blocking);
// a new CLI gets the hold.
func (d *Daemon) DequeueMachineDispatchWait(ctx context.Context, machineID string) link.RunPollResult {
	if res, nonEmpty := d.tryDequeue(machineID); nonEmpty {
		return res
	}
	// Nothing queued — hold for the wake signal, ctx cancel, or timeout.
	wake := d.pollWakeChan(machineID)
	timer := time.NewTimer(pollHoldTimeout)
	defer timer.Stop()
	select {
	case <-wake:
		// A dispatch or cancel was enqueued — try again (may still be empty
		// on a rare race where another poller grabbed it first; return empty).
		res, _ := d.tryDequeue(machineID)
		return res
	case <-ctx.Done():
		return link.RunPollResult{}
	case <-timer.C:
		return link.RunPollResult{}
	}
}

// enqueueMachineCancel queues a run.cancel for the machine's next poll
// (the PULL equivalent of the old push notification).
func (d *Daemon) enqueueMachineCancel(machineID string, c link.RunCancelParams) {
	d.machinePendingMu.Lock()
	d.machineCancels[machineID] = append(d.machineCancels[machineID], c)
	d.machinePendingMu.Unlock()
	d.wakeMachinePoll(machineID)
}

// IngestRunClaimed — the machine's run.claimed ack: publish the bus event
// (the frontend's queued→running flip and the review window's "审查中"
// state hang on it). `mid` is the reporting machine: only the run's own
// machine may ack it, and only while the run is still running.
func (d *Daemon) IngestRunClaimed(ctx context.Context, mid string, p link.RunClaimedParams) *link.RPCError {
	var goalID, agentID, machineID, status string
	var attempt int
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT goal_id, agent_id, attempt, machine_id, status FROM run WHERE id=?`,
		p.RunID).Scan(&goalID, &agentID, &attempt, &machineID, &status); err != nil {
		return &link.RPCError{Code: link.CodeInternal, Message: "unknown run"}
	}
	if machineID != "" && machineID != mid {
		return &link.RPCError{Code: link.CodeForbidden, Message: "not this machine's run"}
	}
	if status != "running" {
		return nil // late ack from a stale exec — the run is already decided
	}
	// The claim IS the delivery ack for the pull queue: retire the pending
	// entry (a lost poll response re-serves; this stops the re-serving),
	// arm the watchdog's activity stamp, and start the watchdog — execution
	// really began only now.
	d.machinePendingMu.Lock()
	for i, pr := range d.machinePending[mid] {
		if pr.runID == p.RunID {
			d.machinePending[mid] = append(d.machinePending[mid][:i], d.machinePending[mid][i+1:]...)
			break
		}
	}
	d.machinePendingMu.Unlock()
	d.machineLastEventMu.Lock()
	d.machineLastEvent[p.RunID] = time.Now()
	d.machineLastEventMu.Unlock()
	go d.runMachineWatchdog(p.RunID, mid)
	d.bus.Publish(ctx, events.Event{Topic: "run:claimed", Payload: map[string]any{
		"run_id": p.RunID, "goal_id": goalID, "agent_id": agentID,
		"attempt": attempt, "started_at": time.Now().UTC().Format(time.RFC3339Nano),
	}})
	logging.Infof("machine: run %s claimed (remote execution started)", p.RunID)
	return nil
}

// IngestRunEvents persists a batch of stream events — the machine's
// per-run seq, contiguous within the batch — exactly like the local
// executor's persistEvent, refreshes the watchdog's activity stamp, and
// broadcasts run:event for the live panels. `mid` is the reporting
// machine; batches from any other machine are rejected, and batches for a
// run that is no longer running (a stale exec flushing after a daemon
// restart re-queued it) are dropped.
func (d *Daemon) IngestRunEvents(ctx context.Context, mid string, p link.RunEventBatchParams) *link.RPCError {
	if p.RunID == "" || len(p.Events) == 0 {
		return nil
	}
	// Ownership: the dispatch-time map entry (fast path). Missing = daemon
	// restarted since dispatch — re-derive from the run row and cache.
	d.machineLastEventMu.Lock()
	expect, seen := d.machineRunMachine[p.RunID]
	d.machineLastEventMu.Unlock()
	if !seen {
		var rm, status string
		if err := d.st.DB().QueryRowContext(ctx,
			`SELECT machine_id, status FROM run WHERE id=?`, p.RunID).Scan(&rm, &status); err != nil {
			return &link.RPCError{Code: link.CodeInvalidParams, Message: "unknown run"}
		}
		if status != "running" {
			return nil // stale batch — the run was re-queued for another dispatch
		}
		expect = rm
		d.machineLastEventMu.Lock()
		d.machineRunMachine[p.RunID] = rm
		d.machineLastEventMu.Unlock()
	}
	if expect == "" || expect != mid {
		return &link.RPCError{Code: link.CodeForbidden, Message: "not this machine's run"}
	}
	// Gap detection: the daemon expects contiguous seq per run. A gap means
	// frames were lost (link degradation) — logged; the transcript refill
	// (loadSession) lands in a later phase.
	d.machineLastEventMu.Lock()
	last, seen := d.machineLastSeq[p.RunID]
	if !seen {
		// First batch: the machine's seq starts at 1 — no gap to judge.
	} else if p.SeqStart != last+1 {
		logging.Infof("machine: run %s event seq gap (have %d, got %d) — %d event(s) lost on the wire", p.RunID, last, p.SeqStart, p.SeqStart-last-1)
	}
	d.machineLastSeq[p.RunID] = p.SeqStart + int64(len(p.Events)) - 1
	d.machineLastEvent[p.RunID] = time.Now()
	d.machineLastEventMu.Unlock()

	for _, ev := range p.Events {
		pev := proto.Event{Type: proto.EventType(ev.Kind), Text: ev.Text, Tool: ev.Tool, CallID: ev.CallID, Input: ev.Input, Output: ev.Output}
		broadcast := d.persistEvent(ctx, p.RunID, pev)
		// persistEvent returns a zero event when the update was an
		// aggregated tool-accumulation chunk (already broadcast on its first
		// update) — skip the WS push so the live stream does not duplicate
		// tool calls. Text/thought/first-tool-use/tool-result broadcast.
		if broadcast.Type != "" {
			d.bus.Publish(ctx, events.Event{Topic: "run:event", Payload: map[string]any{
				"run_id": p.RunID, "event": broadcast,
			}})
		}
	}
	return nil
}

// IngestRunFinished — the machine's terminal report. The platform is the
// only authority over run status: the report flows through the normal
// Finish (conditional stamp + reconcile). Before that, a COMPLETED repo
// run gets its gate evaluation (the daemon computes, the goal layer
// judges — the same division as the local path): the machine's transferred
// branch is adopted and diffed, and the fired gates land on the run row.
//
// The report is validated three ways: it must come from the run's OWN
// machine (run.machine_id), carry the run's CURRENT per-run token (a
// stashed report from an exec killed before a daemon restart must not
// finish the re-dispatched attempt), and the run must still be running.
func (d *Daemon) IngestRunFinished(ctx context.Context, mid string, p link.RunFinishedParams) *link.RPCError {
	if p.RunID == "" {
		return &link.RPCError{Code: link.CodeInvalidParams, Message: "run_id is required"}
	}
	var status, token, machineID, kind, runType, domainID string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT status, token, machine_id, run_kind, run_type, domain_id FROM run WHERE id=?`,
		p.RunID).Scan(&status, &token, &machineID, &kind, &runType, &domainID); err != nil {
		return &link.RPCError{Code: link.CodeInvalidParams, Message: "unknown run"}
	}
	if machineID != "" && machineID != mid {
		return &link.RPCError{Code: link.CodeForbidden, Message: "not this machine's run"}
	}
	// The token gates everything that writes run state — including the
	// session pointer. A stale report from a re-claimed run (old token, the
	// run was reset queued then re-claimed with a fresh token) must not
	// overwrite the new run's session pointer. The handoff-cut case (status
	// already 'cancelled', token STILL matches — onGoalAssigned does not
	// rotate the token) is the reason MarkSession lives here, before the
	// status guard: a handoff cut must still record the session the
	// still-running executor was using, or the next owner loses memory.
	if p.Token != "" && p.Token != token {
		logging.Infof("machine: run %s: stale terminal report dropped (token mismatch — the run was re-claimed)", p.RunID)
		return nil
	}
	if kind != "processor" && (p.SessionID != "" || p.WorkDir != "") {
		if err := d.runSvc.MarkSession(ctx, p.RunID, p.SessionID, p.WorkDir); err != nil {
			logging.Infof("machine: run %s: mark session: %v", p.RunID, err)
		}
	}
	if status != "running" {
		logging.Infof("machine: run %s: late terminal report (%s) dropped — run is %s", p.RunID, p.Status, status)
		return nil
	}
	// ACK NOW, process in the background: the completion pipeline
	// (verification = venv+pip, minutes on a cold cache) ran INSIDE the
	// link handler and the machine's 30s Call raced it (live: "context
	// deadline exceeded" stashes on every finish while the daemon was
	// verifying) — and the daemon's busiest window was exactly when the
	// next dispatch's write timed out. The guards above stay synchronous;
	// everything below is safe to defer (Finish is conditional and the
	// completion stamps are idempotent).
	go d.finishMachineRun(context.Background(), p, kind, runType, domainID)
	return nil
}

// finishMachineRun runs the completion pipeline for an accepted terminal
// report: processor artifacts OR the verification+gate evaluation+Finish
// chain. It must never run on the link handler goroutine.
func (d *Daemon) finishMachineRun(ctx context.Context, p link.RunFinishedParams, kind, runType, domainID string) {
	if kind == "processor" {
		d.ingestProcessorFinished(ctx, p, runType, domainID)
		return
	}
	if p.Status == "completed" {
		// The platform verifies (invariant 9 — the worker never verifies
		// its own work): setup+verify+guards run on the adopted branch,
		// and a red result flips the report to failed (the local path's
		// semantics: verification failure = run failure + retry chain).
		if failReport := d.processMachineRunCompletion(ctx, p.RunID, p.Summary, p.HeadSHA); failReport != "" {
			p.Status = "failed"
			p.Summary = failReport
		}
	}
	d.flushRunMessages(ctx, p.RunID)
	d.machineLastEventMu.Lock()
	delete(d.machineLastSeq, p.RunID)
	delete(d.machineLastEvent, p.RunID)
	delete(d.machineRunMachine, p.RunID)
	d.machineLastEventMu.Unlock()
	if err := d.runSvc.Finish(ctx, p.RunID, p.Status, p.Summary); err != nil && !errors.Is(err, service.ErrRunAlreadyTerminal) {
		logging.Infof("machine: finish run %s: %v", p.RunID, err)
		return
	}
	logging.Infof("machine: run %s finished (%s)", p.RunID, p.Status)
}

// ingestProcessorFinished completes a machine-dispatched processor run
// from its uploaded artifacts (checks.json / intake.json etc.). The
// caller already validated ownership, token, and status.
func (d *Daemon) ingestProcessorFinished(ctx context.Context, p link.RunFinishedParams, runType, domainID string) *link.RPCError {
	d.flushRunMessages(ctx, p.RunID)
	q := &service.ClaimedRow{RunID: p.RunID}
	if p.Status != "completed" {
		if runType == "intake" {
			d.failIntakeRun(ctx, q, p.Summary)
		} else {
			d.failProcessorRun(ctx, q, p.Summary)
		}
		return nil
	}
	if runType == "intake" {
		d.ingestIntakeArtifact(ctx, q, p.Artifacts["intake.json"])
		return nil
	}
	if runType == "import" {
		if d.teamImportSvc != nil {
			if err := d.teamImportSvc.IngestImport(ctx, p.RunID, p.Artifacts, p.Summary); err != nil {
				logging.Errorf("daemon: team import %s failed: %v", p.RunID, err)
				d.failProcessorRun(ctx, q, err.Error())
			} else {
				if _, err := d.st.DB().ExecContext(ctx,
					`UPDATE run SET status='completed', result_summary=?, finished_at=? WHERE id=?`,
					p.Summary, nowStr(), q.RunID); err != nil {
					logging.Errorf("daemon: finish import run %s: %v", q.RunID, err)
				} else {
					logging.Infof("daemon: team import run %s completed", q.RunID)
				}
			}
		} else {
			logging.Warnf("daemon: import run %s finished but teamImportSvc is nil — artifacts dropped", p.RunID)
		}
		return nil
	}
	d.storeProcessorArtifacts(ctx, q, domainID, p.Artifacts, p.Summary)
	return nil
}

// runMachineWatchdog supervises a dispatched run: no events for idleWindow
// or a total duration beyond maxRunDuration cancels it via the machine
// link (the local executor's watchdog semantics, re-applied across the
// wire). The cancel is a REQUEST, not a stamp: after firing it keeps
// ticking, and if the machine has not reported the terminal state within
// a grace period (dead link, or an executor that ignores run.cancel) it
// stamps cancelled locally — the run must never hang 'running' on an
// unresponsive machine.
func (d *Daemon) runMachineWatchdog(runID, machineID string) {
	tick := time.NewTicker(idleWindow / 2)
	defer tick.Stop()
	start := time.Now()
	cancelSent := time.Time{}
	for {
		<-tick.C
		// Terminal? The run's state is the truth — the watchdog dies when
		// its run is no longer running.
		var status string
		if err := d.st.DB().QueryRowContext(context.Background(),
			`SELECT status FROM run WHERE id=?`, runID).Scan(&status); err != nil || status != "running" {
			return
		}
		d.machineLastEventMu.Lock()
		last, ok := d.machineLastEvent[runID]
		d.machineLastEventMu.Unlock()
		fired := false
		reason := ""
		if ok && time.Since(last) > idleWindow {
			fired, reason = true, "idle_watchdog"
		} else if time.Since(start) > 2*time.Hour {
			fired, reason = true, "max_run_duration"
		}
		if !fired {
			continue
		}
		if cancelSent.IsZero() {
			logging.Infof("machine: watchdog firing for run %s (%s)", runID, reason)
			// PULL model: the cancel rides the machine's next poll.
			d.enqueueMachineCancel(machineID, link.RunCancelParams{RunID: runID, Reason: reason})
			cancelSent = time.Now()
			continue
		}
		// Cancel sent but no terminal report: grace, then stamp locally.
		if time.Since(cancelSent) > 2*time.Minute {
			logging.Infof("machine: watchdog stamping run %s cancelled locally (machine unresponsive after cancel)", runID)
			_ = d.runSvc.Finish(context.Background(), runID, "cancelled", "watchdog cancelled (machine unresponsive)")
			return
		}
	}
}

// processMachineRunCompletion is the daemon-side completion processing for
// a machine-executed run: adopt the transferred branch (retrying for the
// git host's ref-visibility delay), run setup+verify+guards on an ephemeral
// worktree, evaluate the gates (goal-level), and stamp refs/evidence/
// gates_hit. Returns a failure report when verification or guards fail —
// the caller flips the run to failed. The daemon computes, the goal layer
// judges; the worker never verifies its own work (invariant 9).
//
// headSHA is the transfer tip the machine reported: when non-empty, the
// adopted branch must point AT it, and a branch that cannot be adopted at
// all is a hard failure — a completed run that pushed work must never fall
// back to the zero-change path (a lost push is not "no changes").
func (d *Daemon) processMachineRunCompletion(ctx context.Context, runID, agentSummary, headSHA string) string {
	var goalID, role, subGoalID, domainID, gitURL, defaultBranch, gitCredentials, domainType string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.goal_id, r.role, r.sub_goal_id, d.id, d.git_url, d.default_branch, d.git_credentials, COALESCE(d.type,'')
		 FROM run r JOIN goal g ON g.id = r.goal_id JOIN domain d ON d.id = g.domain_id WHERE r.id=?`,
		runID).Scan(&goalID, &role, &subGoalID, &domainID, &gitURL, &defaultBranch, &gitCredentials, &domainType); err != nil {
		return "" // unknown run / no domain — the goal layer's judgment stands
	}
	if domainType == "scratch" || role == "consult" || role == "review" {
		return "" // scratch: no diff; read-only roles: nothing to verify or gate
	}
	checks, timeout, baseline, frozen := d.loadDomainChecks(ctx, domainID)
	if !frozen {
		// Unfrozen policy: no verification against an unconfirmed definition;
		// the goal layer forces the human checkpoint.
		return ""
	}
	subGoalRun := role == "subgoal"
	verifyRun := role == "verify"
	if verifyRun {
		return "" // the verifier's own run has no policy verification
	}

	// The domain lock covers ONLY the shared-repo git mutations (ensure,
	// fetch, branch adopt, worktree add) — verification and guards run on
	// this run's PRIVATE worktree and must not serialize every other run's
	// completion on the domain (a slow test suite once held the lock for
	// minutes). Released explicitly before verification below.
	unlock := d.lockDomain(domainID)
	repo := domainRepoPath(domainID)
	if err := d.ensureSharedRepo(ctx, domainID, gitURL, gitCredentials); err != nil {
		unlock()
		logging.Infof("machine: completion %s: prepare repo: %v", runID, err)
		return ""
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	branchName := goalBranchName(goalID)
	if subGoalRun {
		branchName = subGoalBranchName(goalID, subGoalID)
	}

	// Adopt the transferred branch (retries cover the git host's ref
	// visibility delay — live: a fetch at finished+0ms missed a push that
	// was already on the remote). When the run reports a HeadSHA the
	// TRANSFER REF IS AUTHORITATIVE: a local branch is only the PREVIOUS
	// turn's adoption and must not win over this run's push (live: the
	// stale local ref "won", the tip mismatch hard-failed the run). The
	// local ref is a fallback only when nothing was pushed (zero-change).
	adopted := false
	if headSHA == "" {
		if _, err := gitRun(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branchName); err == nil {
			adopted = true
		}
	}
	for attempt := 0; !adopted && attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				unlock()
				return ""
			case <-time.After(2 * time.Second):
			}
		}
		if _, err := gitRun(ctx, repo, "fetch", "origin"); err != nil {
			continue
		}
		if _, err := gitRun(ctx, repo, "branch", "-f", branchName, "origin/agentwork/"+branchName); err == nil {
			adopted = true
		}
	}
	// The machine reported a pushed commit: the adoption MUST reproduce it
	// exactly — a lost push must never fall through to the zero-change path
	// and complete silently.
	if headSHA != "" {
		if !adopted {
			unlock()
			return fmt.Sprintf("transfer branch missing: the machine pushed %s but origin/agentwork/%s was not visible after retries", headSHA, branchName)
		}
		tip := strings.TrimSpace(mustGit(ctx, repo, "rev-parse", "refs/heads/"+branchName))
		if tip != headSHA {
			unlock()
			return fmt.Sprintf("transfer mismatch: adopted branch tip %s does not match the pushed commit %s", tip, headSHA)
		}
	}
	// Verification target: the adopted branch, or origin/<default> when no
	// transfer exists (a zero-change run — the state equals the base; the
	// local path runs verification on zero-change runs too).
	target := branchName
	if !adopted {
		target = "origin/" + defaultBranch
	}
	wt := filepath.Join(runsRoot(), "gates", runID)
	if _, err := gitRun(ctx, repo, "worktree", "add", "--force", "--detach", wt, target); err != nil {
		unlock()
		logging.Infof("machine: completion %s: worktree: %v", runID, err)
		return ""
	}
	unlock()
	defer func() {
		_, _ = gitRun(context.Background(), repo, "worktree", "remove", "--force", wt)
	}()

	// The diff base: sub-goal runs measure against the goal branch's tip
	// (their integration base); goal-level runs against origin/<default>.
	base := strings.TrimSpace(mustGit(ctx, repo, "rev-parse", "origin/"+defaultBranch))
	if subGoalRun {
		if mb, err := gitRun(ctx, wt, "merge-base", "origin/"+defaultBranch, "HEAD"); err == nil && strings.TrimSpace(mb) != "" {
			base = strings.TrimSpace(mb)
		}
	}

	// Verification + guards — the local path's semantics: red = run failed.
	var verifyReport, guardReport string
	verifyReport, ok, policyIssue := runVerification(ctx, wt, checks, timeout)
	if !ok {
		if policyIssue {
			d.annotatePolicyIssue(ctx, goalID)
		}
		return "verification failed:\n" + verifyReport
	}
	// Guards measure the run's own diff (base..HEAD) — repo-only, and
	// scratch returned early, so they always run here.
	guardReport, ok = checkGuards(ctx, wt, base, checks, baseline)
	if !ok {
		return "guards failed:\n" + guardReport
	}

	// Gates (goal-level only, mirroring the local path) + refs + evidence.
	gatesHit := []string{}
	if !subGoalRun && !verifyRun {
		gatesHit = evalGates(ctx, wt, base, checks)
	}
	head := strings.TrimSpace(mustGit(ctx, wt, "rev-parse", "HEAD"))
	ev := buildEvidence(ctx, wt, base, agentSummary, verifyReport, guardReport)
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET gates_hit=?, base_ref=?, head_ref=?, evidence=? WHERE id=?`,
		mustJSON(gatesHit), base, head, ev, runID); err != nil {
		logging.Infof("machine: completion %s: stamp: %v", runID, err)
		return ""
	}
	logging.Infof("machine: completion %s: verified ok, %d gate(s) fired", runID, len(gatesHit))
	return ""
}

// mustGit runs git in dir and returns the output, trimmed ('' on error) —
// judgment paths must not crash on a missing ref.
func mustGit(ctx context.Context, dir string, args ...string) string {
	out, err := gitRun(ctx, dir, args...)
	if err != nil {
		return ""
	}
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ── config push (CLI 分支 Phase 4) ──

// PushMachineSkills full-syncs every agent whose runtime belongs to the
// machine (called on register — edits made while the machine was offline
// land here; the overwrite is idempotent).
func (d *Daemon) PushMachineSkills(ctx context.Context, machineID string) {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT a.id FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE r.machine_id=?`, machineID)
	if err != nil {
		logging.Infof("machine: skills sync %s: %v", machineID, err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		d.PushAgentSkills(ctx, id)
	}
}

// PushAgentSkills sends the agent's selected skill packages to its machine
// via config.push. No-op for local/legacy runtimes and offline machines
// (the register-time full sync covers those).
func (d *Daemon) PushAgentSkills(ctx context.Context, agentID string) {
	var skillsJSON, machineID, systemPrompt string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT a.skills, r.machine_id, a.system_prompt FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`,
		agentID).Scan(&skillsJSON, &machineID, &systemPrompt); err != nil || machineID == "" {
		return // local/legacy runtime — nothing to push
	}
	peer := d.MachinePeer(machineID)
	if peer == nil {
		return // offline — the register-time sync replays it
	}
	var skillIDs []string
	_ = json.Unmarshal([]byte(skillsJSON), &skillIDs)
	skillSvc := service.NewSkillService(d.st)
	var pushes []link.SkillPush
	for _, sid := range skillIDs {
		var name string
		_ = d.st.DB().QueryRowContext(ctx, `SELECT name FROM skill WHERE id=?`, sid).Scan(&name)
		// The push carries the skill's ORIGINAL zip archive (scripts and
		// binary assets included) — the machine extracts it with the same
		// safe extractor the platform used (zipx, both ends).
		archive, err := skillSvc.PackageZip(ctx, sid)
		if err != nil {
			logging.Infof("machine: skills push %s: archive %s: %v", agentID, sid, err)
			continue
		}
		pushes = append(pushes, link.SkillPush{Name: name, Archive: archive})
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	promptLen := len(systemPrompt)
	logging.Infof("machine: config.push %s: dispatching prompt=%d bytes, %d skill(s)", agentID, promptLen, len(pushes))
	var res link.ConfigPushResult
	if err := peer.Call(callCtx, link.MethodConfigPush, link.ConfigPushParams{
		AgentID:      agentID,
		SystemPrompt: systemPrompt,
		Skills:       pushes,
	}, &res); err != nil {
		logging.Infof("machine: config.push %s: rpc error: %v", agentID, err)
		return
	}
	if len(res.Errors) > 0 {
		logging.Infof("machine: config.push %s: %d error(s): %v", agentID, len(res.Errors), res.Errors)
		return
	}
	logging.Infof("machine: config.push %s: ok — %d skill(s) installed", agentID, len(res.Written))
}

// projectSkillsDirFor resolves a run's CLI's project-level skills
// directory from the machine's probe report (the CLI knowledge lives in
// the CLI's probe table; the daemon reads it back). '' = not probed —
// the executor falls back to its own mapping.
func (d *Daemon) projectSkillsDirFor(ctx context.Context, machineID string, spawn []string) string {
	if machineID == "" || len(spawn) == 0 {
		return ""
	}
	var probedJSON string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT probed_clis FROM machine WHERE id=?`, machineID).Scan(&probedJSON); err != nil {
		return ""
	}
	var clis []link.ProbeCLI
	if err := json.Unmarshal([]byte(probedJSON), &clis); err != nil {
		return ""
	}
	cliName := filepath.Base(spawn[0])
	for _, c := range clis {
		if c.Name == cliName || (len(c.ACPSpawn) > 0 && filepath.Base(c.ACPSpawn[0]) == cliName) {
			return c.ProjectSkillsDir
		}
	}
	return ""
}

// AgentMachineID resolves the machine an agent's runtime lives on ('' for
// local/legacy or unknown agents).
func (d *Daemon) AgentMachineID(ctx context.Context, agentID string) string {
	var machineID string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.machine_id FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`,
		agentID).Scan(&machineID); err != nil {
		return ""
	}
	return machineID
}

// SkillAgents returns the agents that have the skill selected (their
// machines get a re-push after a library deletion so the skill's directory
// is cleaned up).
func (d *Daemon) SkillAgents(ctx context.Context, skillID string) []string {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT id FROM agent WHERE skills LIKE ?`, `%"`+skillID+`"%`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
