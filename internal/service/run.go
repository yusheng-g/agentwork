package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/store"
)

// Run is one execution of a goal by one agent (the execution plane). It has
// NO authority over goal status: on a terminal status the daemon calls
// GoalService.ReconcileOnRunEnd, which is the sole path that advances a goal.
// See DESIGN.md §9.

// goalTitleForLog resolves a goal's title for log lines — ids are system
// handles, humans read logs. '' when the goal is gone (or processor runs).
func (s *RunService) goalTitleForLog(ctx context.Context, goalID string) string {
	if goalID == "" {
		return ""
	}
	var t string
	if err := s.st.DB().QueryRowContext(ctx, `SELECT title FROM goal WHERE id=?`, goalID).Scan(&t); err != nil {
		return ""
	}
	return t
}

// trimLog caps a string for a log line (summaries and wake notes are full
// sentences; the log keeps the opening, the feed carries the rest).
func trimLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
type Run struct {
	ID        string `json:"id"`
	GoalID    string `json:"goal_id"`
	AgentID   string `json:"agent_id"`
	RunKind   string `json:"run_kind"` // worker|processor (platform-internal)
	RunType   string `json:"run_type"` // processor tasks: compile|intake (M3)
	DomainID  string `json:"domain_id"`
	Prompt    string `json:"prompt"` // the assembled task message — processor runs: the processing instruction; worker runs: stored at dispatch so the human can inspect what the agent was told
	SessionID string `json:"session_id"`
	Workdir   string `json:"workdir"`
	Status    string `json:"status"`
	// CancelReason is the structured cancellation cause (idle_watchdog|
	// handoff|stopped|timeout|runaway|goal_terminal|goal_cancelled) — the
	// runs panel badges it so a dropped intent's fate is visible.
	CancelReason string `json:"cancel_reason,omitempty"`
	// Role is the run's collaboration role stamped at enqueue (决策 5-4):
	// owner|subgoal|consult|review|verify ('' for processor runs).
	// Informational snapshot — goal authority is judged DYNAMICALLY at
	// reconcile (ownRunByGoal), never from this column.
	Role string `json:"role"`
	// SubGoalID is the sub-goal this run executes ('' for goal-level runs).
	SubGoalID string `json:"sub_goal_id"`
	// BaseRef/HeadRef are the Change revision refs the daemon stamps at a
	// sub-goal run's end (merge-base of goal branch and the sub-goal branch,
	// and the branch head SHA).
	BaseRef       string `json:"base_ref"`
	HeadRef       string `json:"head_ref"`
	Attempt       int    `json:"attempt"`
	ResultSummary string `json:"result_summary"`
	Evidence      string `json:"evidence"` // JSON: diff stats + verify output + summary
	// WakeNote is the owner-wake reason compiled in the spawn transaction
	// (决策 6-17): the prompt reads THIS snapshot, not the mutable
	// goal.attention — a later reconcile must not erase "why you were woken".
	WakeNote string `json:"wake_note,omitempty"`
	// WakeAnchor is the comment the wake refers to (决策 6-22: the
	// get_comments(after=) handle; '' = no comment anchor).
	WakeAnchor       string `json:"wake_anchor,omitempty"`
	TriggerCommentID string `json:"trigger_comment_id"`
	IsLeaderRun      bool   `json:"is_leader_run"`
	SquadID          string `json:"squad_id"`
	QueuedAt         string `json:"queued_at"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
	CreatedAt        string `json:"created_at"`
}

type RunService struct {
	st      *store.Store
	bus     *events.Bus
	goalSvc *GoalService
}

func NewRunService(st *store.Store, bus *events.Bus) *RunService {
	return &RunService{st: st, bus: bus}
}

// SetGoalService wires the back-reference once both exist. GoalService needs
// runSvc for retry/wake enqueue; RunService needs goalSvc for reconcile.
func (s *RunService) SetGoalService(gs *GoalService) { s.goalSvc = gs }

// resolveLeader returns the agent id that should run a goal assigned to a
// squad (always the squad's leader). For agent/human goals the assignee agent
// is used directly (or empty for human / an unmatched case).
func (s *RunService) resolveLeader(ctx context.Context, assigneeType, assigneeID string) (agentID string, isLeader bool, squadID string, err error) {
	switch assigneeType {
	case "agent":
		return assigneeID, false, "", nil
	case "squad":
		var leader string
		if e := s.st.DB().QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, assigneeID).Scan(&leader); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return "", false, "", NewValidationError("squad not found")
			}
			return "", false, "", fmt.Errorf("load squad leader: %w", e)
		}
		return leader, true, assigneeID, nil
	default:
		return "", false, "", NewValidationError("goal assignee is not an agent or squad")
	}
}

// resolveLeaderTx is resolveLeader inside a caller's transaction — the squad
// leader lookup runs ON the tx: a pooled read while the tx holds the only
// connection deadlocks the single-connection in-memory test stores.
func (s *RunService) resolveLeaderTx(ctx context.Context, tx *sql.Tx, assigneeType, assigneeID string) (agentID string, isLeader bool, squadID string, err error) {
	switch assigneeType {
	case "agent":
		return assigneeID, false, "", nil
	case "squad":
		var leader string
		if e := tx.QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, assigneeID).Scan(&leader); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return "", false, "", NewValidationError("squad not found")
			}
			return "", false, "", fmt.Errorf("load squad leader: %w", e)
		}
		return leader, true, assigneeID, nil
	default:
		return "", false, "", NewValidationError("goal assignee is not an agent or squad")
	}
}

// hasPending reports whether goalID already has a queued/running run on agentID.
// Retired from enqueueTx (the coalesce became per-(goal,agent,ROLE) — each
// role's pending run is a distinct ask); kept for callers that still need the
// coarse shape.
func (s *RunService) hasPending(ctx context.Context, tx *sql.Tx, goalID, agentID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND status IN ('queued','running')`,
		goalID, agentID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// enqueueTx is the atomic enqueue core: inserts a run row under the caller's
// transaction. It performs the per-(goal,agent) pending coalesce check INSIDE
// the same tx so it sees the caller's un-committed state — the Coordinator's
// conditional spawn (决策 6-4) relies on this: two racing reconciles see each
// other's pending run and coalesce into one.
//
// The run event (enqueued/coalesced) is RETURNED, not published: publishing
// inside the tx would violate the "bus.Publish after commit" invariant (a
// rolled-back tx must not emit). The caller publishes after its commit.
// Note: EnqueueExistingTx (the Coordinator's path inside a goal tx) does not
// publish — the goal layer's commitAndEmit covers goal events.
func (s *RunService) enqueueTx(ctx context.Context, tx *sql.Tx, goalID, agentID string, attempt int, isLeader bool, squadID, triggerCommentID, wakeNote, wakeAnchor, roleOverride string) (*Run, *events.Event, error) {
	// The role is derived from the trigger comment's author UNLESS the caller
	// stamps it explicitly (决策 6-19: the platform's review request is
	// enqueued as role='review' even though its trigger — the parking run's
	// report — is agent-authored).
	role := roleOverride
	if role == "" {
		var derr error
		role, derr = s.resolveRunRole(ctx, tx, isLeader, triggerCommentID)
		if derr != nil {
			return nil, nil, derr
		}
	}
	var err error
	// Coalesce per (goal, agent, ROLE): each role's pending run is a DISTINCT
	// ask. A cross-role merge is a correctness bug, not a dedupe: an owner
	// spawn merging into a pending consult swallows the WORK run and strands
	// the goal (D-1 makes this reachable — a human's consult intent queued
	// during review would absorb the reject/handoff/attention-spawn run, and
	// the queued_at bump would then convince the no-progress guard the
	// signal was answered — permanently); a review request merging into a
	// consult left the round without its reviewer (P1-2). Same-role
	// coalescing preserves the double-spawn idempotency.
	var id, pstatus string
	err = tx.QueryRowContext(ctx,
		`SELECT id, status FROM run WHERE goal_id=? AND agent_id=? AND role=? AND status IN ('queued','running') ORDER BY queued_at LIMIT 1`,
		goalID, agentID, role).Scan(&id, &pstatus)
	if err == nil {
		if id != "" && pstatus == "queued" {
			// A coalesced intent IS the answer to the current signal — bump
			// the pending run's spawn timestamp. Without it, a pending run
			// created BEFORE the signal leaves lastOwnerSpawn stale, and
			// after it finishes without progress the Coordinator re-wakes
			// forever (the no-progress guard compares signal recency against
			// this timestamp). A RUNNING run is NOT bumped — its prompt was
			// built at claim, before the signal existed; the signal must
			// survive its finish.
			//
			// The wake note travels with the bump (决策 6-17): the coalesced
			// run's prompt is built at CLAIM (not enqueue), so it must carry
			// the LATEST signal's reason. A new note overwrites; an empty one
			// keeps the pending run's original context.
			if wakeNote != "" {
				_, _ = tx.ExecContext(ctx, `UPDATE run SET queued_at=?, wake_note=?, wake_anchor=? WHERE id=?`, now(), wakeNote, wakeAnchor, id)
			} else {
				_, _ = tx.ExecContext(ctx, `UPDATE run SET queued_at=? WHERE id=?`, now(), id)
			}
		}
		ev := &events.Event{Topic: "run:coalesced", Payload: map[string]any{
			"goal_id": goalID, "agent_id": agentID,
		}}
		// goal ID only — this runs INSIDE the caller's transaction, and a
		// pool-side title lookup would deadlock single-connection stores
		// (:memory: tests). The daemon's claimed line carries the title.
		logging.Infof("run: coalesced %s goal=%s role=%s agent=%s (already %s)",
			id, goalID, role, agentID, pstatus)
		return &Run{ID: id, GoalID: goalID, AgentID: agentID}, ev, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	ts := now()
	leaderFlag := 0
	if isLeader {
		leaderFlag = 1
	}
	r := Run{
		ID:               newID(),
		GoalID:           goalID,
		AgentID:          agentID,
		Role:             role,
		Attempt:          attempt,
		IsLeaderRun:      isLeader,
		SquadID:          squadID,
		TriggerCommentID: triggerCommentID,
		WakeNote:         wakeNote,
		WakeAnchor:       wakeAnchor,
		Status:           "queued",
		QueuedAt:         ts,
		CreatedAt:        ts,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,domain_id,session_id,workdir,status,role,attempt,result_summary,trigger_comment_id,wake_note,wake_anchor,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.GoalID, r.AgentID, "worker", "", "", "", "", r.Status, r.Role, r.Attempt, r.ResultSummary, r.TriggerCommentID, r.WakeNote, r.WakeAnchor, leaderFlag, r.SquadID, r.QueuedAt, r.StartedAt, r.FinishedAt, r.CreatedAt); err != nil {
		return nil, nil, fmt.Errorf("insert run: %w", err)
	}
	// Every queued run is a potential WAIT (a run nobody claims is invisible
	// otherwise): the enqueue line is the scheduling ledger — goal, role,
	// agent, attempt, and for owner spawns the WAKE REASON (wake_note).
	// goal ID only — this runs INSIDE the caller's transaction, and a
	// pool-side title lookup would deadlock single-connection stores
	// (:memory: tests). The daemon's claimed line carries the title.
	wake := ""
	if r.WakeNote != "" {
		wake = fmt.Sprintf(" wake=%q", trimLog(r.WakeNote, 60))
	}
	logging.Infof("run: enqueued %s goal=%s role=%s agent=%s attempt=%d%s",
		r.ID, r.GoalID, r.Role, r.AgentID, r.Attempt, wake)
	ev := &events.Event{Topic: "run:enqueued", Payload: r}
	return &r, ev, nil
}

// resolveRunRole derives the run's collaboration role at enqueue time
// (决策 5-4/6-9): leader runs and untriggered runs are owner runs; trigger-
// comment runs are review (system-authored — the platform's review request)
// or consult (agent/human-authored — a consult). Informational snapshot only.
func (s *RunService) resolveRunRole(ctx context.Context, tx *sql.Tx, isLeader bool, triggerCommentID string) (string, error) {
	if isLeader || triggerCommentID == "" {
		return "owner", nil
	}
	var authorType string
	if err := tx.QueryRowContext(ctx,
		`SELECT author_type FROM comment WHERE id=?`, triggerCommentID).Scan(&authorType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "consult", nil // trigger comment vanished — treat as consult
		}
		return "", fmt.Errorf("resolve trigger author: %w", err)
	}
	if authorType == "system" {
		return "review", nil
	}
	return "consult", nil
}

// EnqueueForGoal creates a run for a goal based on its current assignee.
// Idempotent: if a pending run already exists for this (goal,agent), it
// coalesces (returns it as a no-op success). P0-2 (决策 6-15②) moved the
// production callers' successors INTO their transitions' transactions
// (Create/Assign/Activate/Reopen/Reject); this entry remains for tests and
// any future direct use — it carries NO goal-status gate (Claim is the
// execution gate).
func (s *RunService) EnqueueForGoal(ctx context.Context, g Goal) (*Run, error) {
	if g.AssigneeType != "agent" && g.AssigneeType != "squad" {
		return nil, nil // human-assigned: no run
	}
	agentID, isLeader, squadID, err := s.resolveLeader(ctx, g.AssigneeType, g.AssigneeID)
	if err != nil {
		return nil, err
	}
	return s.enqueue(ctx, g.ID, agentID, 1, isLeader, squadID, "")
}

// EnqueueForGoalTx is EnqueueForGoal under the caller's transaction — the
// schedule-firing path uses it (P0-3, 决策 6-13): the fired goal and its
// first run are born ATOMICALLY, so a crash after the commit can no longer
// leave a run-less active goal (the startup sweeps cannot resurrect it —
// attention derives only from changes/failed sub-goals). resolveLeader runs
// outside the tx (read-only, idempotent); the run event is RETURNED for the
// caller to publish after commit (invariant 13).
func (s *RunService) EnqueueForGoalTx(ctx context.Context, tx *sql.Tx, g Goal) (*Run, *events.Event, error) {
	if g.AssigneeType != "agent" && g.AssigneeType != "squad" {
		return nil, nil, nil // human-assigned: no run
	}
	agentID, isLeader, squadID, err := s.resolveLeader(ctx, g.AssigneeType, g.AssigneeID)
	if err != nil {
		return nil, nil, err
	}
	return s.enqueueTx(ctx, tx, g.ID, agentID, 1, isLeader, squadID, "", "", "", "")
}

// EnqueueExisting enqueues a run on an explicit agent (used by retry and
// parent-wake: the agent id is already known). Coalesces if a pending run
// exists for this (goal,agent).
func (s *RunService) EnqueueExisting(ctx context.Context, goalID, agentID string, attempt int, isLeader bool, squadID string) error {
	_, err := s.enqueue(ctx, goalID, agentID, attempt, isLeader, squadID, "")
	return err
}

// EnqueueForMention creates a run on an explicitly-mentioned agent for the
// same goal, sourced from a comment. trigger_comment_id records provenance.
// Per DESIGN.md §2 this enqueues on the mentioned agent (NOT the goal's
// current assignee) and does NOT cancel any in-flight run.
func (s *RunService) EnqueueForMention(ctx context.Context, goalID, agentID, triggerCommentID string) (*Run, error) {
	return s.enqueue(ctx, goalID, agentID, 1, false, "", triggerCommentID)
}

// EnqueueForMentionRole is EnqueueForMention with an EXPLICIT role stamp
// (决策 6-19): the squad review request's trigger is now the parking run's
// report comment — AGENT-authored — so resolveRunRole would derive
// 'consult'. The review run must stay role='review' (claim gate, window
// pending count, prompt — all keyed on role); the platform enqueues it as
// such explicitly.
func (s *RunService) EnqueueForMentionRole(ctx context.Context, goalID, agentID, triggerCommentID, role string) (*Run, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, ev, err := s.EnqueueForMentionRoleTx(ctx, tx, goalID, agentID, triggerCommentID, role)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if ev != nil {
		s.bus.Publish(ctx, *ev)
	}
	return r, nil
}

// EnqueueForMentionRoleTx is the in-transaction variant (决策 6-13/6-19):
// the review run is the PARK's successor — it is born in the park
// transaction, so when goal:reviewing publishes, the run already EXISTS and
// the approval card's pending-reviewer hint can never race an empty list.
func (s *RunService) EnqueueForMentionRoleTx(ctx context.Context, tx *sql.Tx, goalID, agentID, triggerCommentID, role string) (*Run, *events.Event, error) {
	return s.enqueueTx(ctx, tx, goalID, agentID, 1, false, "", triggerCommentID, "", "", role)
}

// EnqueueSubGoalRun creates a sub-goal execution run (决策 6-1/6-9): role
// subgoal, bound to the sub_goal via run.sub_goal_id, on the sub-goal's
// assignee. Coalesces on a pending run for the same sub-goal (per-sub-goal
// single-flight — the Claim guard enforces the rest). run.attempt mirrors the
// sub-goal's execution_attempt+1 (informational ordinal).
func (s *RunService) EnqueueSubGoalRun(ctx context.Context, subGoalID string) (*Run, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, ev, err := s.enqueueSubGoalRunTx(ctx, tx, subGoalID, "")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if ev != nil {
		s.bus.Publish(ctx, *ev)
	}
	return r, nil
}

// enqueueSubGoalRunTx inserts a sub-goal execution run under the caller's
// transaction (P0-2: CreateSubGoal uses it so the sub_goal row and its first
// run are born atomically). triggerCommentID (” for retry/rework rounds)
// links the FIRST round to the dispatch comment (P2-1, 决策 6-15⑩): the
// run's report threads to it, closing the leader→assignee causal chain. The
// run event is RETURNED — the caller publishes after its commit (invariant 13).
func (s *RunService) enqueueSubGoalRunTx(ctx context.Context, tx *sql.Tx, subGoalID, triggerCommentID string) (*Run, *events.Event, error) {
	var goalID, assigneeID string
	var execAttempt int
	err := tx.QueryRowContext(ctx,
		`SELECT goal_id, assignee_id, execution_attempt FROM sub_goal WHERE id=?`, subGoalID).
		Scan(&goalID, &assigneeID, &execAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load sub-goal: %w", err)
	}

	// Coalesce per (sub_goal, ROLE): a pending VERIFY run must not swallow the
	// assignee's rework run (the reject path enqueues it while the verifier is
	// still finishing). Cross-role concurrency is governed by Claim's
	// per-sub-goal single-flight, not by the enqueue coalesce.
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' AND status IN ('queued','running') LIMIT 1`, subGoalID).Scan(&existing)
	if err == nil {
		return &Run{ID: existing, GoalID: goalID, AgentID: assigneeID, Role: "subgoal", SubGoalID: subGoalID, Status: "queued"}, nil, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("check pending sub-goal run: %w", err)
	}

	ts := now()
	r := Run{
		ID:               newID(),
		GoalID:           goalID,
		AgentID:          assigneeID,
		Role:             "subgoal",
		SubGoalID:        subGoalID,
		Attempt:          execAttempt + 1,
		TriggerCommentID: triggerCommentID,
		Status:           "queued",
		QueuedAt:         ts,
		CreatedAt:        ts,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,domain_id,session_id,workdir,status,role,sub_goal_id,attempt,result_summary,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.GoalID, r.AgentID, "worker", "", "", "", "", r.Status, r.Role, r.SubGoalID, r.Attempt, "", r.TriggerCommentID, 0, "", r.QueuedAt, "", "", r.CreatedAt); err != nil {
		return nil, nil, fmt.Errorf("insert sub-goal run: %w", err)
	}
	ev := &events.Event{Topic: "run:enqueued", Payload: r}
	return &r, ev, nil
}

// EnqueueVerifyRun creates a verifier run (决策 6-5): role=verify, on the
// sub-goal's named verifier agent, coalesced per sub-goal (the Claim guard's
// per-sub-goal single-flight keeps it serialized with the assignee's runs —
// the verifier judges a stable state).
func (s *RunService) EnqueueVerifyRun(ctx context.Context, subGoalID string) (*Run, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, ev, err := s.enqueueVerifyRunTx(ctx, tx, subGoalID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if ev != nil {
		s.bus.Publish(ctx, *ev)
	}
	return r, nil
}

// enqueueVerifyRunTx inserts a verifier run under the caller's transaction
// (P0-3, 决策 6-13: the reconcile parks the sub-goal verifying and births
// the verifier run ATOMICALLY — a crash after the commit can no longer
// leave a verifying sub-goal with no verifier run). The run event is
// RETURNED — the caller publishes after its commit (invariant 13).
func (s *RunService) enqueueVerifyRunTx(ctx context.Context, tx *sql.Tx, subGoalID string) (*Run, *events.Event, error) {
	var goalID, verifierID string
	err := tx.QueryRowContext(ctx,
		`SELECT goal_id, verifier_id FROM sub_goal WHERE id=?`, subGoalID).
		Scan(&goalID, &verifierID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load sub-goal: %w", err)
	}
	if verifierID == "" {
		return nil, nil, NewValidationError("the sub-goal has no agent verifier")
	}

	// Coalesce per (sub_goal, role) — see enqueueSubGoalRunTx.
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='verify' AND status IN ('queued','running') LIMIT 1`, subGoalID).Scan(&existing)
	if err == nil {
		return &Run{ID: existing, GoalID: goalID, AgentID: verifierID, Role: "verify", SubGoalID: subGoalID, Status: "queued"}, nil, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("check pending sub-goal run: %w", err)
	}

	ts := now()
	r := Run{
		ID:        newID(),
		GoalID:    goalID,
		AgentID:   verifierID,
		Role:      "verify",
		SubGoalID: subGoalID,
		Attempt:   1,
		Status:    "queued",
		QueuedAt:  ts,
		CreatedAt: ts,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,domain_id,session_id,workdir,status,role,sub_goal_id,attempt,result_summary,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.GoalID, r.AgentID, "worker", "", "", "", "", r.Status, r.Role, r.SubGoalID, r.Attempt, "", "", 0, "", r.QueuedAt, "", "", r.CreatedAt); err != nil {
		return nil, nil, fmt.Errorf("insert verify run: %w", err)
	}
	ev := &events.Event{Topic: "run:enqueued", Payload: r}
	return &r, ev, nil
}

// EnqueueProcessorRun creates a platform-internal processor run (DESIGN.md
// §8): no goal, a fixed prompt, associated with a domain being processed
// (compile) or carrying the platform context for the task (intake, M3).
// runType discriminates the processor task (compile|intake); the daemon's
// runProcessorTask dispatches on it. Coalesces: if the same (run_type,
// domain/agent) already has a queued/running processor run, it is returned
// instead of duplicating (a second compile of the same domain would race the
// first; a backlog of intake runs on the same agent would duplicate work).
func (s *RunService) EnqueueProcessorRun(ctx context.Context, runType, domainID, agentID, prompt string) (*Run, error) {
	if runType == "" {
		runType = "compile" // default: the original processor task
	}
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM run WHERE run_kind='processor' AND run_type=? AND domain_id=? AND agent_id=? AND status IN ('queued','running') LIMIT 1`,
		runType, domainID, agentID).Scan(&existing)
	if err == nil {
		return &Run{ID: existing, DomainID: domainID, AgentID: agentID, Status: "queued"}, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check pending processor run: %w", err)
	}

	ts := now()
	r := Run{
		ID:        newID(),
		DomainID:  domainID,
		AgentID:   agentID,
		RunKind:   "processor",
		RunType:   runType,
		Prompt:    prompt,
		Status:    "queued",
		QueuedAt:  ts,
		CreatedAt: ts,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,run_type,domain_id,prompt,session_id,workdir,status,role,attempt,result_summary,evidence,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, "", r.AgentID, r.RunKind, r.RunType, r.DomainID, r.Prompt, "", "", r.Status, "", 1, "", "", "", 0, "", r.QueuedAt, "", "", r.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert processor run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *RunService) enqueue(ctx context.Context, goalID, agentID string, attempt int, isLeader bool, squadID, triggerCommentID string) (*Run, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, ev, err := s.enqueueTx(ctx, tx, goalID, agentID, attempt, isLeader, squadID, triggerCommentID, "", "", "")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Publish only after commit (invariant 13): a rolled-back tx emits nothing.
	if ev != nil {
		s.bus.Publish(ctx, *ev)
	}
	return r, nil
}

// EnqueueExistingTx is the same-package atomic variant for a caller that
// already holds a transaction (e.g. the parent-wake path in GoalService, which
// flips blocked→active and enqueues in one tx). The run event is RETURNED —
// the caller's tx owns the publish-after-commit contract and publishes after
// its commit (invariant 13).
func (s *RunService) EnqueueExistingTx(ctx context.Context, tx *sql.Tx, goalID, agentID string, attempt int, isLeader bool, squadID, wakeNote, wakeAnchor string) (*Run, *events.Event, error) {
	return s.enqueueTx(ctx, tx, goalID, agentID, attempt, isLeader, squadID, "", wakeNote, wakeAnchor, "")
}

// ClaimedRow is a claimed run row handed to the daemon's runTask.
type ClaimedRow struct {
	RunID   string
	GoalID  string
	AgentID string
	Attempt int
	// Token is the per-run execution credential (CLI 分支 Phase 2): issued
	// at claim, delivered to the executor, carried by the agent's CLI
	// commands to /rpc. Valid only while the run is 'running'.
	Token string
}

// Claim atomically claims the oldest queued run for one of the ready
// (has-worker, not crashed/deleted) agents AND has a free concurrency slot.
// Per DESIGN.md the claim avoids the old global head-of-line blocking by
// letting the daemon pass the set of agents with free capacity and claiming
// only within that set. Returns (nil, nil) when nothing is claimable.
//
// SERIALIZATION (决策 6-2, per-workspace): every run gets its OWN worktree, so
// the old per-goal lock is gone. The remaining guards:
//   - an OWNER run is not claimed while another owner run of the same goal is
//     running (owner single-flight — the goal branch has one writer);
//   - a SUB-GOAL run is not claimed while another run of the same sub-goal is
//     running (per-sub-goal single-flight);
//   - consult/review/verify runs are read-only snapshots and run freely in
//     parallel.
//
// EXECUTION GATE (P0-1, 决策 6-15①): Claim is the ONLY place that decides
// whether a queued run may execute NOW — producers may enqueue intents, the
// gate admits them. A run claims only when its goal is active; on a review
// goal only the platform's review-request runs (role='review' — the squad
// checkpoint, which exists to run during the freeze window) pass; terminal
// goals admit nothing. The freeze therefore protects the branch under the
// human's judgment while intents queue durably (决策 2-3 revised).
//
// Processor runs (goal_id=”) are unaffected.
func (s *RunService) Claim(ctx context.Context, readyAgents []string) (*ClaimedRow, error) {
	if len(readyAgents) == 0 {
		return nil, nil
	}
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// IN-list for the ready agents.
	placeholders, args := inPlaceholders(readyAgents)
	var r ClaimedRow
	err = tx.QueryRowContext(ctx,
		`UPDATE run
		 SET status='running', started_at=?, token=lower(hex(randomblob(16)))
		 WHERE id = (
		   SELECT r.id FROM run r
		   JOIN agent a ON a.id = r.agent_id
		   JOIN runtime rt ON rt.id = a.runtime_id
		   LEFT JOIN goal g ON g.id = r.goal_id
		   WHERE r.status='queued' AND r.queued_at <= ?   -- retry backoff: future-dated runs wait
		     AND r.agent_id IN (`+placeholders+`)
		     AND rt.status != 'absent'                   -- the machine's probe no longer sees the CLI
		     AND (rt.machine_id = '' OR EXISTS (         -- machine-owned runtimes claim only while
		          SELECT 1 FROM machine m                 -- their machine is online (CLI 分支)
		          WHERE m.id = rt.machine_id AND m.status='connected'))
		     AND (g.id IS NULL                              -- processor runs have no goal
		          OR g.status = 'active'
		          OR (g.status = 'review' AND r.role = 'review'))
		     AND NOT EXISTS (
		       SELECT 1 FROM run r2
		       WHERE r2.status='running'
		         AND ((r.role = 'owner' AND r2.role = 'owner' AND r2.goal_id != '' AND r2.goal_id = r.goal_id)
		              OR (r.sub_goal_id != '' AND r2.sub_goal_id = r.sub_goal_id))
		     )
		   ORDER BY r.queued_at
		   LIMIT 1
		 )
		 RETURNING id, goal_id, agent_id, attempt, token`, append([]any{now(), now()}, args...)...).
		Scan(&r.RunID, &r.GoalID, &r.AgentID, &r.Attempt, &r.Token)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Publish AFTER commit (invariant 13): the frontend needs to know a run
	// went queued→running (the review window's "审查中" state depends on it) —
	// without this event the flow card shows the run as queued/parked while
	// the agent is already working.
	s.bus.Publish(ctx, events.Event{Topic: "run:claimed", Payload: map[string]any{
		"run_id": r.RunID, "goal_id": r.GoalID, "agent_id": r.AgentID,
		"attempt": r.Attempt, "started_at": now(),
	}})
	return &r, nil
}

// RecoverStuckRunning reclaims runs left 'running' by a previous daemon that
// died without finishing them. Reset to queued so dispatch re-claims them.
// Note: attempt is preserved — a run on its last attempt that the daemon lost
// still has its remaining retry credit (DELTA from multica: their HandleFailedTasks
// resets to todo; here we just keep the run queued, attempt unchanged).
//
// P0-4: only runs of goals that can still execute (active/review) are
// requeued — a run left 'running' on a goal the human CANCELLED (or that
// went terminal) while the daemon was down must NOT be resurrected by the
// restart: it would burn compute on already-decided work (live: the
// cancelled smoke goal's leftover run was re-claimed and re-ran after the
// restart). Terminal-goal leftovers are stamped cancelled instead.
func (s *RunService) RecoverStuckRunning(ctx context.Context) (int, error) {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled', cancel_reason='goal_terminal', finished_at=?
		 WHERE status='running'
		   AND goal_id IN (SELECT id FROM goal WHERE status IN ('done','failed','cancelled'))`, now())
	if err != nil {
		return 0, fmt.Errorf("cancel terminal-goal runs: %w", err)
	}
	res, err = s.st.DB().ExecContext(ctx,
		`UPDATE run SET status='queued', started_at='' WHERE status='running'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ExpireStaleScheduledRuns closes schedule-fired runs that sat queued past
// the TTL: a fire born while its machine was offline is a MISS, not a
// backlog item (the user's cron semantics: miss = miss). The fired goal is
// cancelled (no retry chain — a stale fire must not burn attempts hours
// later) and its schedule_run row is stamped missed. Returns how many were
// expired.
func (s *RunService) ExpireStaleScheduledRuns(ctx context.Context, before time.Time) (int, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT r.id, r.goal_id, sr.schedule_id FROM run r
		 JOIN schedule_run sr ON sr.goal_id = r.goal_id
		 WHERE r.status='queued' AND r.queued_at != '' AND r.queued_at < ?`,
		before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("scan stale scheduled runs: %w", err)
	}
	type staleRow struct{ runID, goalID, scheduleID string }
	var stale []staleRow
	for rows.Next() {
		var r staleRow
		if err := rows.Scan(&r.runID, &r.goalID, &r.scheduleID); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, r)
	}
	rows.Close()

	n := 0
	for _, r := range stale {
		if s.goalSvc != nil {
			if _, err := s.goalSvc.Cancel(ctx, r.goalID); err != nil && !errors.Is(err, ErrNotFound) {
				logging.Infof("run: expire stale scheduled run %s: cancel goal %s: %v", r.runID, r.goalID, err)
				continue
			}
		}
		if _, err := s.st.DB().ExecContext(ctx,
			`UPDATE schedule_run SET status='missed' WHERE schedule_id=? AND goal_id=?`,
			r.scheduleID, r.goalID); err != nil {
			logging.Infof("run: expire stale scheduled run %s: stamp missed: %v", r.runID, err)
			continue
		}
		logging.Infof("run: schedule fire %s missed (goal %s cancelled — queued past the TTL, machine offline)", r.scheduleID, r.goalID)
		n++
	}
	return n, nil
}

// MarkSession stamps the protocol-returned session id once runTask has opened
// a session (for history / future long-lived resume).
func (s *RunService) MarkSession(ctx context.Context, runID, sessionID, workdir string) error {
	_, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET session_id=?, workdir=? WHERE id=?`, sessionID, workdir, runID)
	return err
}

// ErrRunAlreadyTerminal reports that a late result was dropped: the run was
// already terminalized by another writer (the runaway reaper, or the handoff
// claim→register stamp) and the caller's outcome must not overwrite the
// terminal state nor reconcile as if it had won (P0-5, 决策 6-15⑥).
var ErrRunAlreadyTerminal = errors.New("run already terminal — late result dropped")

// Finish records a run's terminal status + summary and reconciles the goal.
// This is the daemon's single chokepoint to end a run: it writes the run row
// then hands the outcome to the goal layer for authoritative state change.
//
// The stamp is CONDITIONAL on the run being non-terminal: once another
// writer (runaway reaper / handoff window stamp) terminalized the row, a
// LATE result from the still-running process must not resurrect it — the
// stamp is the authority, the outcome is dropped (ErrRunAlreadyTerminal),
// and the reaper's stamp stands as the run's terminal truth.
func (s *RunService) Finish(ctx context.Context, runID, status, summary string) error {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET status=?, result_summary=?, finished_at=? WHERE id=? AND status IN ('queued','running')`,
		status, summary, now(), runID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRunAlreadyTerminal
	}
	if err := s.reconcileRun(ctx, runID); err != nil {
		return err
	}
	rc, err := s.loadRunContext(ctx, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	// The terminal event of the scheduling ledger — a run ending is what
	// closes (or opens) every wait: reviewer windows, deliver, retries.
	sum := ""
	if rc.Summary != "" {
		sum = fmt.Sprintf(" summary=%q", trimLog(rc.Summary, 80))
	}
	if rc.Status == "cancelled" && rc.CancelReason != "" {
		sum += fmt.Sprintf(" cancel_reason=%s", rc.CancelReason)
	}
	logging.Infof("run: finished %s goal=%q (%s) role=%s status=%s attempt=%d%s",
		runID, s.goalTitleForLog(ctx, rc.GoalID), rc.GoalID, rc.Role, rc.Status, rc.Attempt, sum)
	// run:cancelled — the structured cancellation event for the notify layer
	// (Feishu "任务中断" card). Finish is the single terminal chokepoint: a
	// stamp that won (RowsAffected > 0) reaches here; a late result that lost
	// the race returned ErrRunAlreadyTerminal above. So publishing here covers
	// every cancelled path that flows through Finish — including the machine
	// watchdog (idle/timeout) which previously had NO event and left notify
	// blind. StopRun and the runaway reaper publish their OWN events before
	// the stamp, but their late Finish returns ErrRunAlreadyTerminal and never
	// reaches here — no double publish. notify skips reason_code=handoff/stopped.
	if rc.Status == "cancelled" && rc.CancelReason != "" && rc.GoalID != "" {
		s.bus.Publish(ctx, events.Event{Topic: "run:cancelled", Payload: map[string]any{
			"run_id": rc.RunID, "goal_id": rc.GoalID,
			"reason": cancelReasonDescription(rc.CancelReason),
			"reason_code": rc.CancelReason,
		}})
	}
	// run.terminal is the Coordinator's wakeup hint (决策 6-4, P2-5): the
	// latch's second edge — "owner run terminal → Reconcile" — subscribes here.
	// Published AFTER the reconcile committed (invariant 13).
	if rc.GoalID != "" {
		s.bus.Publish(ctx, events.Event{Topic: "run.terminal", Payload: map[string]any{
			"run_id": rc.RunID, "goal_id": rc.GoalID, "status": rc.Status,
		}})
	}
	return nil
}

// RunIdentity is the (goal, agent, role) identity a per-run token resolves
// to — the authority behind every /rpc collaboration call (CLI 分支 Phase 2).
type RunIdentity struct {
	RunID   string
	GoalID  string
	AgentID string
	Role    string
}

// ResolveRunToken validates a per-run credential: it must belong to a run
// that is STILL RUNNING (terminal runs' tokens are void). The resolved
// identity is the only truth /rpc methods act on — self-reported ids in
// params are ignored.
func (s *RunService) ResolveRunToken(ctx context.Context, token string) (*RunIdentity, error) {
	if token == "" {
		return nil, NewFieldRequiredError("token")
	}
	var id RunIdentity
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, goal_id, agent_id, role FROM run WHERE token=? AND status='running'`, token).
		Scan(&id.RunID, &id.GoalID, &id.AgentID, &id.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewValidationError("invalid or expired token")
	}
	if err != nil {
		return nil, fmt.Errorf("resolve run token: %w", err)
	}
	return &id, nil
}

// loadRunContext re-reads the minimal context the goal layer needs to
// reconcile (shared by Finish and the startup replay).
func (s *RunService) loadRunContext(ctx context.Context, runID string) (goalRunContext, error) {
	var rc goalRunContext
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, goal_id, agent_id, is_leader_run, squad_id, status, attempt, result_summary, trigger_comment_id, role, sub_goal_id, base_ref, head_ref, session_id, cancel_reason, run_type
		 FROM run WHERE id=?`, runID).
		Scan(&rc.RunID, &rc.GoalID, &rc.AgentID, &rc.IsLeaderRun, &rc.SquadID, &rc.Status, &rc.Attempt, &rc.Summary, &rc.TriggerCommentID, &rc.Role, &rc.SubGoalID, &rc.BaseRef, &rc.HeadRef, &rc.SessionID, &rc.CancelReason, &rc.RunType)
	if err != nil {
		return rc, err
	}
	return rc, nil
}

// reconcileRun replays a run's terminal outcome through the goal layer — the
// single entry both Finish and the startup replay use (P0-1, 决策 6-11). The
// reconcile stamps run.reconciled_at inside its own transaction, so a crash
// between the terminal UPDATE (Finish) and this call leaves the marker empty
// and ReconcilePendingTerminal re-runs it.
func (s *RunService) reconcileRun(ctx context.Context, runID string) error {
	rc, err := s.loadRunContext(ctx, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // run vanished (goal deleted) — nothing to reconcile
		}
		return err
	}
	if s.goalSvc == nil {
		return errors.New("runSvc.goalSvc not wired")
	}
	return s.goalSvc.ReconcileOnRunEnd(ctx, rc)
}

// ReconcilePendingTerminal replays the reconcile for runs whose terminal
// state never got reconciled (daemon crash between the terminal UPDATE and
// the reconcile transaction — P0-1). The replay is safe: every transition
// is conditional, and the report comment commits atomically with
// reconciled_at, so a crash mid-reconcile never duplicates anything.
// Runs cancelled while still QUEUED (never started) have no reconcile
// semantics and are skipped. Returns how many runs were replayed.
func (s *RunService) ReconcilePendingTerminal(ctx context.Context) (int, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id FROM run WHERE reconciled_at=''
		   AND (status IN ('completed','failed') OR (status='cancelled' AND started_at != ''))`)
	if err != nil {
		return 0, fmt.Errorf("scan unreconciled runs: %w", err)
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
	n := 0
	for _, id := range ids {
		if err := s.reconcileRun(ctx, id); err != nil {
			logging.Infof("service: replay reconcile %s: %v", id, err)
			continue
		}
		// Republish the Coordinator's latch edge (idempotent — ReconcileGoal
		// recomputes from DB state) so attention follows the replayed state.
		rc, err := s.loadRunContext(ctx, id)
		if err == nil && rc.GoalID != "" {
			s.bus.Publish(ctx, events.Event{Topic: "run.terminal", Payload: map[string]any{
				"run_id": rc.RunID, "goal_id": rc.GoalID, "status": rc.Status,
			}})
		}
		n++
	}
	return n, nil
}

func (s *RunService) List(ctx context.Context, goalID string) ([]Run, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,goal_id,agent_id,run_kind,run_type,domain_id,prompt,session_id,workdir,status,role,attempt,result_summary,cancel_reason,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at
		 FROM run WHERE goal_id=? ORDER BY queued_at`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		var leaderFlag int
		if err := rows.Scan(&r.ID, &r.GoalID, &r.AgentID, &r.RunKind, &r.RunType, &r.DomainID, &r.Prompt, &r.SessionID, &r.Workdir, &r.Status, &r.Role, &r.Attempt, &r.ResultSummary, &r.CancelReason, &r.TriggerCommentID, &leaderFlag, &r.SquadID, &r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.IsLeaderRun = leaderFlag != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestCompileRun returns the domain's most recent compile processor run
// (决策 6-23): the compile panel restores its in-flight state from this
// after a page refresh — queued/running means still compiling, failed
// shows the failure, completed needs no banner (the domain query itself
// carries the compiled checks). (nil, nil) when the domain has never
// compiled.
func (s *RunService) LatestCompileRun(ctx context.Context, domainID string) (*Run, error) {
	row := s.st.DB().QueryRowContext(ctx,
		`SELECT id,goal_id,agent_id,run_kind,run_type,domain_id,session_id,workdir,status,role,attempt,result_summary,cancel_reason,trigger_comment_id,is_leader_run,squad_id,queued_at,started_at,finished_at,created_at
		 FROM run WHERE run_kind='processor' AND run_type='compile' AND domain_id=? ORDER BY created_at DESC LIMIT 1`,
		domainID)
	var r Run
	var leaderFlag int
	if err := row.Scan(&r.ID, &r.GoalID, &r.AgentID, &r.RunKind, &r.RunType, &r.DomainID, &r.SessionID, &r.Workdir, &r.Status, &r.Role, &r.Attempt, &r.ResultSummary, &r.CancelReason, &r.TriggerCommentID, &leaderFlag, &r.SquadID, &r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.IsLeaderRun = leaderFlag != 0
	return &r, nil
}

// RunMessage is one row of a run's interaction stream (chat_message) — the
// Web run detail's "what is the agent doing right now" view.
type RunMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls string `json:"tool_calls"`
	CreatedAt string `json:"created_at"`
}

// ListMessages returns the run's persisted interaction stream, oldest first.
func (s *RunService) ListMessages(ctx context.Context, runID string) ([]RunMessage, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT role, content, tool_calls, created_at FROM chat_message WHERE run_id=? ORDER BY created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunMessage{}
	for rows.Next() {
		var m RunMessage
		if err := rows.Scan(&m.Role, &m.Content, &m.ToolCalls, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AgentHistoryItem is one entry of an agent's past work — the chat surface's
// "what have I done" view. It joins a run to its goal so the agent sees the
// task title alongside the run outcome, without pulling the full Run row.
type AgentHistoryItem struct {
	GoalID        string `json:"goal_id"`
	GoalTitle     string `json:"goal_title"`
	GoalStatus    string `json:"goal_status"`
	RunID         string `json:"run_id"`
	RunStatus     string `json:"run_status"`
	RunRole       string `json:"run_role"`
	Attempt       int    `json:"attempt"`
	ResultSummary string `json:"result_summary"`
	FinishedAt    string `json:"finished_at"`
}

// HistoryByAgent returns the agent's recent runs joined to their goals, newest
// finished first. limit<=0 defaults to 20; status filters by run.status ('' =
// all). Processor runs (run_kind='processor', no goal) are excluded — they are
// platform-internal, not the agent's own work. Runs still queued/running carry
// an empty finished_at and sort after finished ones (NULLs last in DESC).
func (s *RunService) HistoryByAgent(ctx context.Context, agentID string, limit int, status string) ([]AgentHistoryItem, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT r.goal_id, g.title, g.status, r.id, r.status, r.role, r.attempt, r.result_summary, r.finished_at
	      FROM run r JOIN goal g ON g.id=r.goal_id
	      WHERE r.agent_id=? AND r.run_kind='worker'`
	args := []any{agentID}
	if status != "" {
		q += " AND r.status=?"
		args = append(args, status)
	}
	q += " ORDER BY r.finished_at DESC, r.created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.st.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentHistoryItem{}
	for rows.Next() {
		var it AgentHistoryItem
		if err := rows.Scan(&it.GoalID, &it.GoalTitle, &it.GoalStatus, &it.RunID, &it.RunStatus, &it.RunRole, &it.Attempt, &it.ResultSummary, &it.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Status returns the run's current status and result summary. Used by the
// Web intake polling endpoint (GET /intake/{runId}).
func (s *RunService) Status(ctx context.Context, runID string) (status, summary string, err error) {
	err = s.st.DB().QueryRowContext(ctx,
		`SELECT status, COALESCE(result_summary,'') FROM run WHERE id=?`, runID).
		Scan(&status, &summary)
	return
}

// inPlaceholders builds "?,?,?" for a slice and returns the args slice.
func inPlaceholders(ids []string) (string, []any) {
	ph := ""
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args[i] = id
	}
	return ph, args
}
