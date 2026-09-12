package daemon

import (
	"context"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/google/uuid"
)

// ── Squad review checkpoint (platform-enforced, not agent discretion) ──
//
// The reviewer is a member of the squad — the squad OWNS the "who reviews"
// rule (member role="reviewer", set when the squad is built), and the
// PLATFORM enforces it: when a squad-owned goal parks in review (a gate
// fired on the completed owner run), the daemon enqueues each reviewer's
// review run automatically. No prompt text has to tell the leader to "hand
// off to the reviewer" — the squad's structure IS the instruction, and it
// fires every round (reject → new leader run → review again → reviewers
// re-triggered).
//
// The review run's causal anchor (决策 6-19) is the COMPLETION DECLARATION:
// the report comment of the run whose end parked the goal — "leader thinks
// it's done" IS the review trigger, the reviewer's report threads to it. No
// platform comment is minted; review = the platform's consult, asked with
// the leader's own words as its anchor. The run is enqueued with an
// EXPLICIT role='review' stamp (the agent-authored trigger would otherwise
// derive 'consult').
//
// The review run runs in its own read-only worktree while the goal sits in
// review (the human's approval window — that is exactly when the review
// opinions must be visible), and its result is discarded by reconcile like
// any guest run — the opinions live in the comments, which the approval
// card and the human read.
//
// The review runs are enqueued IN the park transaction (决策 6-19 — the
// goal layer's enqueueSquadReviewTx): when goal:reviewing publishes, the
// runs already exist, so the approval card's pending-reviewer hint never
// races an empty list. A handoff_loop park (决策 5-7, published by Assign)
// enqueues no reviewers — no code change to review there — but still opens
// the window: the human must decide the collaboration. The coalesce on
// (goal, reviewer) keeps re-parks from stacking runs.

// onGoalReviewing reacts to a goal parking in review: if the goal belongs to
// a squad with reviewer members, trigger the squad's review checkpoint, then
// open the reviewer-first approval window (Option B): the human's card fires
// on goal:review_ready — after this window's review runs are terminal — not
// at park time with an empty opinion section.
func (d *Daemon) onGoalReviewing(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	goalID, _ := m["goal_id"].(string)
	if goalID == "" {
		return
	}
	// No agent run needs cutting here (决策 4-11): the gate that parks the
	// goal fired on the run that just completed, and per-goal serialization
	// means no other run is live — the review run (enqueued IN the park
	// transaction, 决策 6-19) claims within a dispatch tick.
	d.openReviewWindow(d.ctx, goalID)
}

// recoverReviewWindows re-opens the review window for every goal still in
// review — the startup face of Option B's Event≠Truth recovery: the ready
// publish, the fallback timers and the fired flags are in-memory, so a crash
// between a window's park and its ready publish would leave the human's card
// permanently unpatched. maybeFireReviewReady is idempotent (DB-derived).
func (d *Daemon) recoverReviewWindows(ctx context.Context) (int, error) {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT id FROM goal WHERE status='review'`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		d.maybeFireReviewReady(ctx, id)
	}
	return len(ids), nil
}

// openReviewWindow starts a NEW review window for the goal: the per-window
// card dedupe resets, and when no review runs are pending (no squad, no
// reviewers, or all excluded) the card fires immediately.
func (d *Daemon) openReviewWindow(ctx context.Context, goalID string) {
	d.mu.Lock()
	if d.reviewReadyFired == nil {
		d.reviewReadyFired = make(map[string]bool)
	}
	delete(d.reviewReadyFired, goalID) // a new park = a new window
	d.mu.Unlock()
	d.maybeFireReviewReady(ctx, goalID)
}

// maybeFireReviewReady publishes goal:review_ready when the goal is still in
// review, the human has not approved, and no review runs are pending. Called
// from openReviewWindow (park time, no-reviewer windows) and onRunTerminal
// (a review run finishing — the ONLY closer). A hung reviewer is NOT special:
// it is an ordinary agent run and dies by the same lifecycle as any worker
// (idle watchdog, per-domain max_run_duration reaper); its terminal event
// closes the window. The fired flag makes the card exactly-once per window.
func (d *Daemon) maybeFireReviewReady(ctx context.Context, goalID string) {
	// Auto-approve goals (digest + team import) never reach a human: their
	// checkpoint is auto-approved right after Finish — don't flash the card in
	// the window before the approve lands.
	if d.isAutoApproveGoal(ctx, goalID) {
		return
	}
	var status, goalTitle string
	if err := d.st.DB().QueryRowContext(ctx, `SELECT status, title FROM goal WHERE id=?`, goalID).Scan(&status, &goalTitle); err != nil {
		return
	}
	if status != "review" {
		return // resolved/terminal — no card (the human already acted)
	}
	// An approve already on record means deliver is in flight (or done) —
	// the human decided without the card; don't send it post-hoc.
	var decided int
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gate_decision WHERE goal_id=? AND decision='approve'`, goalID).Scan(&decided); err != nil {
		return
	}
	if decided > 0 {
		return
	}
	var pending int
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='review' AND status IN ('queued','running')`,
		goalID).Scan(&pending); err != nil {
		logging.Infof("daemon: review-ready pending check %s: %v", goalID, err)
		return
	}
	if pending > 0 {
		return // the reviewer's own run lifecycle closes the window
	}
	d.publishReviewReady(goalID)
}

// publishReviewReady fires the human's card trigger exactly once per window.
func (d *Daemon) publishReviewReady(goalID string) {
	d.mu.Lock()
	if d.reviewReadyFired[goalID] {
		d.mu.Unlock()
		return
	}
	d.reviewReadyFired[goalID] = true
	d.mu.Unlock()
	var goalTitle string
	_ = d.st.DB().QueryRowContext(context.Background(), `SELECT title FROM goal WHERE id=?`, goalID).Scan(&goalTitle)
	logging.Infof("daemon: review window ready for %q (%s) — notifying the human", goalTitle, goalID)
	// The review_duration anchor: the human's decision window STARTS here
	// (the reviewer's run time is not the human's decision time — the health
	// metric must measure the latter). An invisible activity row (the
	// timeline renders only its known action kinds).
	if _, err := d.st.DB().ExecContext(context.Background(),
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,'system','','review_ready','{}',?)`,
		uuid.NewString(), goalID, nowStr()); err != nil {
		logging.Infof("daemon: review-ready anchor for %s: %v", goalID, err)
	}
	d.bus.Publish(context.Background(), events.Event{Topic: "goal:review_ready", Payload: map[string]any{
		"goal_id": goalID,
	}})
}

// onGoalFinished reacts to a goal reaching a terminal state. For CANCELLED
// goals (human Cancel, 决策 4-12), any still-running run is terminated —
// a cancelled goal must not keep an agent burning compute on work already
// decided dead. Done/failed goals have no live runs by construction (the
// terminal run just finished; per-goal serialization), so they are no-ops.
func (d *Daemon) onGoalFinished(_ context.Context, e events.Event) {
	m, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	// A terminal goal has no more wakes (决策 6-21 — sessions retired with
	// the local execution path, CLI 分支).
	status, _ := m["status"].(string)
	if status != "cancelled" {
		return
	}
	goalID, _ := m["goal_id"].(string)
	if goalID == "" {
		return
	}
	rows, err := d.st.DB().QueryContext(d.ctx,
		`SELECT id FROM run WHERE goal_id=? AND status='running'`, goalID)
	if err != nil {
		logging.Infof("daemon: cancel scan %s: %v", goalID, err)
		return
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		logging.Infof("daemon: goal cancelled — stopping run %s", id)
		d.cancelRun(id, "stopped")
	}
	// Cleanup the goal's feat branch (决策 7-4): cancelled is terminal — no
	// cancelled→active transition — so the branch is garbage. The goal title
	// and domain git config are loaded for the cleanup logs and the remote
	// delete. A scratch domain (git_url="") has no branch to clean; a missing
	// domain row (deleted first) is a no-op.
	go d.cleanupCancelledGoalBranches(goalID)
}

// cleanupCancelledGoalBranches loads a cancelled goal's domain git config and
// hands off to cleanupGoalBranches. Run in its own goroutine so the event
// handler returns immediately — git ops (fetch/lock/delete) can take seconds.
// Acquires the per-domain lock itself: this goroutine is NOT inside a
// deliverGoal section, so cleanupGoalBranches (which no longer locks — 决策
// 7-4 LOCK CONTRACT) relies on the caller here.
func (d *Daemon) cleanupCancelledGoalBranches(goalID string) {
	ctx := d.ctx
	var goalTitle, domainID, gitURL, gitCredentials string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT g.title, d.id, d.git_url, d.git_credentials FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`, goalID).
		Scan(&goalTitle, &domainID, &gitURL, &gitCredentials)
	if err != nil {
		// Goal row gone (deleted concurrently) or domain missing — nothing to
		// clean. Not an error worth surfacing.
		logging.Infof("git: cleanup cancelled %s: load domain: %v", goalID, err)
		return
	}
	unlock := d.lockDomain(domainID)
	defer unlock()
	d.cleanupGoalBranches(ctx, goalID, goalTitle, domainID, gitURL, gitCredentials)
}
