package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/store"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Goal is a work item (the product plane). It is the SOLE holder of state
// authority: any change to its status flows through ReconcileOnRunEnd, which
// checks whether the reporting run still belongs to the current assignee
// before touching status. See DESIGN.md §9.
type Goal struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	DomainID        string `json:"domain_id"`     // owning domain (required for agent/squad goals — v2)
	AssigneeType    string `json:"assignee_type"` // agent | squad | human
	AssigneeID      string `json:"assignee_id"`
	Status          string `json:"status"` // backlog|active|done|failed|review|cancelled
	HandoffNote     string `json:"handoff_note"`
	ReviewRequest   string `json:"review_request"`   // gate trigger reason / deliver-failure note
	HumanIterations int    `json:"human_iterations"` // reject iterations (separate from run.attempt)
	CreatedByType   string `json:"created_by_type"`  // human | agent
	CreatedByID     string `json:"created_by_id"`
	CreatedAt       string `json:"created_at"`
	SourceRef       string `json:"source_ref"` // external source (M4-B): "github:owner/repo#123"
	// Attention is the v2 derived OwnerAttention persisted by ReconcileGoal
	// (决策 6-8): '' | integration | wrapup | recovery | user_action (comma-joined).
	// integration = ready changes to merge; wrapup = verified sub-goals with no
	// change (no-code completion, deliverable in the feed) — split so the UI chip
	// and changes panel no longer disagree (决策 6-17 aligns flag with wake note).
	Attention string `json:"attention"`
	// ReviewPhase is the review window's DERIVED phase (决策 6-19 延伸): the
	// platform's own judgment rendered for the Web —
	// awaiting_review (review runs enqueued, none started) | reviewing (a
	// review run is running) | awaiting_approval (no pending review runs —
	// opinions are in, or no reviewer exists). '' when not in review.
	ReviewPhase string `json:"review_phase,omitempty"`
	// CurrentAgentID is the agent of the goal's latest running/queued run —
	// the list card's "who is working right now" ('' = nobody in flight).
	CurrentAgentID string `json:"current_agent_id"`
}

// goalRunContext is what ReconcileOnRunEnd reasons about. Carried separately
// so the reconciliation logic is testable without a live run row.
type goalRunContext struct {
	RunID            string
	GoalID           string
	AgentID          string
	IsLeaderRun      bool
	SquadID          string
	Status           string // run's terminal status: completed|failed|cancelled
	Attempt          int
	Summary          string
	TriggerCommentID string // mention/协作来源（guest run 失败留痕用）
	// SessionID is the agent's ACP session id ('' when the run was cut before
	// the agent ever ran — a launch failure, a watchdog/timeout before the
	// backend started). The issue writeback uses it to tell an agent-authored
	// summary (relay it to the issue) from a platform-authored one (a machine
	// diagnostic the issue's author cannot act on — stay silent).
	SessionID string
	// Sub-goal runs (决策 6-1/6-9): the sub-goal this run executes and the
	// Change revision refs the daemon stamped at run end.
	Role      string
	SubGoalID string
	BaseRef   string
	HeadRef   string
	// CancelReason is the structured cancellation cause
	// (idle_watchdog|handoff|stopped|timeout|runaway|goal_terminal|goal_cancelled)
	// — read by the cancelled reconcile branch to post a system feed comment
	// and by Finish to publish run:cancelled with a structured reason_code.
	CancelReason string
}

const maxAttempts = 3

// retryBackoffFor spaces retry attempts: attempt 2 waits 30s, attempt 3
// waits 60s (capped at 5m). A transient failure (dead link, machine
// stall) must not burn the whole retry budget in one second.
func retryBackoffFor(attempt int) time.Duration {
	delay := 30 * time.Second << (attempt - 2)
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}

// Handoff-cycle guard thresholds (决策 5-7): ≥4 ownership transitions write a
// system warning comment; ≥8 park the goal in review for a human decision —
// a collaboration anomaly is not a task failure (unlike the mention pingpong
// guard, 决策 4-2).
const (
	handoffWarnThreshold  = 4
	handoffCycleThreshold = 8
)

// StaleConflictResolver identifies conflict changes whose head_ref is already
// merged into the goal branch (stale conflicts) — the daemon layer knows the
// git worktree, the service layer does not. The resolver runs OUTSIDE the
// reconcile transaction (git operates on the worktree, not the DB); the
// returned change IDs are marked integrated inside the tx.
type StaleConflictResolver interface {
	StaleConflictChanges(ctx context.Context, goalID string) ([]string, error)
}

type GoalService struct {
	st     *store.Store
	bus    *events.Bus
	runSvc *RunService // back-reference for retry/wake enqueue (same package)

	// staleConf clears stale conflict changes during reconcile. nil in tests
	// (no git worktree) — the cleanup is skipped and reconcile proceeds as
	// before. Wired by the daemon via SetStaleConflictResolver.
	staleConf StaleConflictResolver

	// reconcileLocks serializes ReconcileGoal per goal (single-process): an
	// event storm fires run.terminal + sub_goal.* concurrently, and racing
	// write transactions surface as SQLITE_BUSY under load.
	reconcileMu    sync.Mutex
	reconcileLocks map[string]*sync.Mutex
}

func NewGoalService(st *store.Store, bus *events.Bus) *GoalService {
	return &GoalService{st: st, bus: bus, reconcileLocks: make(map[string]*sync.Mutex)}
}

// lockReconcile serializes one goal's reconciles (cheap map entry per goal).
// The mutex is NEVER deleted (P0-3, 决策 6-15③): deleting it races — a waiter
// blocked on the old mutex while a newcomer creates a fresh one ends up
// running two reconciles of the same goal in parallel, defeating the exact
// serialization this mutex exists for (the SQLITE_BUSY storm mitigation).
// One mutex per goal ever reconciled is cheap at single-user scale.
func (s *GoalService) lockReconcile(goalID string) func() {
	s.reconcileMu.Lock()
	m, ok := s.reconcileLocks[goalID]
	if !ok {
		m = &sync.Mutex{}
		s.reconcileLocks[goalID] = m
	}
	s.reconcileMu.Unlock()
	m.Lock()
	return m.Unlock
}

// SetRunService wires the RunService back-reference once both exist. Kept
// explicit (not in the constructor) to avoid a constructor-order chicken/egg.
func (s *GoalService) SetRunService(rs *RunService) { s.runSvc = rs }

// SetStaleConflictResolver wires the git-ancestry checker. Kept explicit (not
// in the constructor) so tests construct GoalService without a git worktree;
// a nil resolver simply skips stale-conflict cleanup.
func (s *GoalService) SetStaleConflictResolver(r StaleConflictResolver) { s.staleConf = r }

// Create inserts a goal. backlog (the semantic invariant) does not enqueue a
// run; an ACTIVE agent/squad goal births its first run IN the same
// transaction (P0-2, 决策 6-15②) — no caller-side enqueue exists anymore.
func (s *GoalService) Create(ctx context.Context, g Goal) (*Goal, error) {
	if g.Title == "" {
		return nil, NewFieldRequiredError("title")
	}
	if g.AssigneeType == "" {
		if g.AssigneeID == "" {
			// No assignee at all — an unassigned backlog item (the web
			// form's "无（进入 backlog）" option). Type "human" carries the
			// placeholder semantics: no run can ever enqueue until a real
			// assignee is chosen (Assign validates the domain then).
			g.AssigneeType = "human"
		} else {
			// An id without a type — convenience default.
			g.AssigneeType = "agent"
		}
	}
	if g.Status == "" {
		g.Status = "backlog"
	}
	if g.CreatedByType == "" {
		g.CreatedByType = "human"
	}
	// Note: review is a transitive status (the checkpoint gate) — reject it at
	// creation so callers can't plant a goal in a waiting state. (blocked was
	// retired with the wait-children model, 决策 6-10; the check stays as a
	// belt-and-braces guard against stale clients.)
	if g.Status == "blocked" || g.Status == "review" {
		return nil, NewValidationError("cannot create a goal in blocked/review status")
	}
	switch g.AssigneeType {
	case "agent":
		if g.AssigneeID == "" {
			return nil, NewFieldRequiredError("assignee_id")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, g.AssigneeID, "agent"); err != nil {
			return nil, err
		}
	case "squad":
		if g.AssigneeID == "" {
			return nil, NewFieldRequiredError("assignee_id")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, g.AssigneeID, "squad"); err != nil {
			return nil, err
		}
	case "human":
		// human-assigned goals have no agent run; assigning one is a manual placeholder.
	default:
		return nil, NewValidationError("assignee_type must be agent, squad, or human")
	}
	// v2: agent/squad-executed goals must belong to a domain — the domain owns
	// the worktree and the acceptance policy (DESIGN.md §2). Human/backlog
	// goals may be domain-less.
	if g.AssigneeType != "human" {
		if g.DomainID == "" {
			return nil, NewFieldRequiredError("domain_id")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM domain WHERE id=?`, g.DomainID, "domain"); err != nil {
			return nil, err
		}
	}

	g.ID = newID()
	g.CreatedAt = now()

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var domainID any
	if g.DomainID != "" {
		domainID = g.DomainID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO goal (id,title,description,domain_id,assignee_type,assignee_id,status,handoff_note,created_by_type,created_by_id,created_at,source_ref)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Title, g.Description, domainID, g.AssigneeType, g.AssigneeID, g.Status, g.HandoffNote, g.CreatedByType, g.CreatedByID, g.CreatedAt, g.SourceRef); err != nil {
		return nil, fmt.Errorf("insert goal: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,?,?,?)`,
		newID(), g.ID, g.CreatedByType, g.CreatedByID, "created", "{}", g.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	// The creation instruction lands in the comment feed AS A MENTION — the
	// same coordination shape an agent produces: [@Name](mention://agent|squad
	// /<id>) + instruction. Assigning a goal IS the first mention, so the
	// feed's timeline is uniform: "@dev-team 给 test-repo 添加 …" renders as a
	// highlighted chip, exactly like an agent-to-agent handoff. Written
	// directly (not via CommentService.Create) so creation never
	// dispatch-triggers: the assignee's run is born in THIS transaction
	// (below), and a description that mentions other agents must not
	// double-trigger at creation.
	//
	// title is the always-present task statement (Create validates it non-empty
	// at the top); description, when present, carries the fuller instruction.
	// Both land in the comment so the feed has a task statement even when the
	// creator only wrote a title — without it the default wake anchor branch
	// (assemblePrompt: first comment ASC) finds nothing and the agent starts
	// blind with no (comment <id>) handle to pull history.
	if g.AssigneeType == "agent" || g.AssigneeType == "squad" {
		label, err := s.assigneeLabel(ctx, tx, g.AssigneeType, g.AssigneeID)
		if err != nil {
			return nil, fmt.Errorf("resolve assignee label: %w", err)
		}
		content := "[@" + label + "](mention://" + g.AssigneeType + "/" + g.AssigneeID + ") " + g.Title
		if g.Description != "" {
			content += "\n" + g.Description
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,?,NULL,?,?)`,
			newID(), g.ID, g.CreatedByType, g.CreatedByID, content, g.CreatedAt); err != nil {
			return nil, fmt.Errorf("insert creation comment: %w", err)
		}
	}
	// P0-2 (决策 6-15②): an ACTIVE agent/squad goal is born with its first
	// run IN the same transaction — a crash after the commit can no longer
	// leave a run-less active goal (the startup reconciles cannot resurrect
	// it: attention derives only from changes/failed sub-goals). The run
	// event is published after the commit (invariant 13).
	var runEv *events.Event
	if g.Status == "active" && (g.AssigneeType == "agent" || g.AssigneeType == "squad") {
		_, ev, err := s.enqueueOwnerIntentTx(ctx, tx, g.ID, g.AssigneeType, g.AssigneeID, g.Status, "", "", "active")
		if err != nil {
			return nil, fmt.Errorf("enqueue first run: %w", err)
		}
		runEv = ev
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	if runEv != nil {
		s.bus.Publish(ctx, *runEv)
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:created", Payload: g})
	return &g, nil
}

func (s *GoalService) List(ctx context.Context) ([]Goal, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT g.id,g.title,g.description,g.domain_id,g.assignee_type,g.assignee_id,g.status,g.handoff_note,g.review_request,g.human_iterations,g.created_by_type,g.created_by_id,g.created_at,g.source_ref,g.attention,
		        (SELECT r.agent_id FROM run r WHERE r.goal_id = g.id AND r.status IN ('running','queued') ORDER BY r.created_at DESC LIMIT 1),
		        CASE WHEN g.status != 'review' THEN ''
		     WHEN (SELECT COUNT(*) FROM run r WHERE r.goal_id=g.id AND r.role='review' AND r.status IN ('queued','running')) = 0 THEN 'awaiting_approval'
		     WHEN (SELECT COUNT(*) FROM run r WHERE r.goal_id=g.id AND r.role='review' AND r.status='running') > 0 THEN 'reviewing'
		     ELSE 'awaiting_review' END
		 FROM goal g ORDER BY g.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Goal{}
	for rows.Next() {
		var g Goal
		var domainID, currentAgent sql.NullString
		if err := rows.Scan(&g.ID, &g.Title, &g.Description, &domainID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.ReviewRequest, &g.HumanIterations, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt, &g.SourceRef, &g.Attention, &currentAgent, &g.ReviewPhase); err != nil {
			return nil, err
		}
		g.DomainID = domainID.String
		g.CurrentAgentID = currentAgent.String
		out = append(out, g)
	}
	return out, rows.Err()
}

// TimelineItem is one event in a goal's execution flow: a run segment
// (an agent's turn), an action point (created / handoff / review entry /
// reopened / commented / cancelled), or a gate decision (approve / reject).
// The frontend renders the merged, time-ordered stream as the goal's
// execution timeline — who handled it, for how long, and who holds it now.
type TimelineItem struct {
	At         string `json:"at"`                   // RFC3339 — the event's point in time
	Kind       string `json:"kind"`                 // run | action | decision
	RunID      string `json:"run_id,omitempty"`     // run: the run row (for detail fetch)
	AgentID    string `json:"agent_id,omitempty"`   // run: the executing agent
	RunStatus  string `json:"run_status,omitempty"` // run: queued|running|completed|failed|cancelled
	Role       string `json:"role,omitempty"`       // run: owner|subgoal|consult|review|verify
	Attempt    int    `json:"attempt,omitempty"`    // run: machine-retry counter
	StartedAt  string `json:"started_at,omitempty"` // run: execution window
	FinishedAt string `json:"finished_at,omitempty"`
	ActorType  string `json:"actor_type,omitempty"` // action: human|agent|system
	ActorID    string `json:"actor_id,omitempty"`
	Action     string `json:"action,omitempty"` // action: created|handoff|entered_review|requested_review|parked_review|reopened|cancelled|commented|mention_cycle_failed
	Detail     string `json:"detail,omitempty"`
	GateRule   string `json:"gate_rule,omitempty"`         // decision: which rule fired
	Decision   string `json:"decision,omitempty"`          // decision: approve|reject|redirect
	Reason     string `json:"reason,omitempty"`            // decision: the human's words
	ReviewDurS int    `json:"review_duration_s,omitempty"` // decision: seconds spent in review
}

// Timeline merges the goal's runs (execution segments), activity log
// (human/system action points), and gate decisions (checkpoint verdicts)
// into one time-ordered execution flow. The current holder is derived by the
// frontend from the goal's status plus the latest non-terminal run.
func (s *GoalService) Timeline(ctx context.Context, goalID string) ([]TimelineItem, error) {
	items := []TimelineItem{}

	// 1. runs — execution segments (an agent's turn on the goal).
	rrows, err := s.st.DB().QueryContext(ctx,
		`SELECT id, agent_id, status, attempt, queued_at, started_at, finished_at, role
		 FROM run WHERE goal_id=? AND goal_id<>''`, goalID)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var it TimelineItem
		var q, st, fin sql.NullString
		if err := rrows.Scan(&it.RunID, &it.AgentID, &it.RunStatus, &it.Attempt, &q, &st, &fin, &it.Role); err != nil {
			return nil, err
		}
		it.Kind = "run"
		// NOTE: the columns hold "" (Go zero value), not NULL — judge by
		// non-empty, never by sql.NullString.Valid (empty string is Valid).
		it.StartedAt, it.FinishedAt = st.String, fin.String
		// The segment's anchor: started_at once begun, queued_at before that.
		it.At = q.String
		if st.String != "" {
			it.At = st.String
		}
		items = append(items, it)
	}
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	// 2. activity log — human/system action points. Handoffs are excluded:
	// they render from the richer handoff_event rows (step 4, 决策 5-9).
	arows, err := s.st.DB().QueryContext(ctx,
		`SELECT actor_type, actor_id, action, detail, created_at
		 FROM activity_log WHERE goal_id=? AND action != 'handoff'`, goalID)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var it TimelineItem
		var det sql.NullString
		if err := arows.Scan(&it.ActorType, &it.ActorID, &it.Action, &det, &it.At); err != nil {
			return nil, err
		}
		it.Kind = "action"
		it.Detail = det.String
		items = append(items, it)
	}
	if err := arows.Err(); err != nil {
		return nil, err
	}

	// 3. handoff events — ownership transitions (决策 5-9, the rich audit row:
	// from/to + reason + both run links). Rendered as action points.
	hrows, err := s.st.DB().QueryContext(ctx,
		`SELECT from_type, from_id, to_type, to_id, from_run_id, to_run_id, reason, actor_type, actor_id, created_at
		 FROM handoff_event WHERE goal_id=?`, goalID)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var it TimelineItem
		var ft, fi, tt, ti, fr, tr, rsn sql.NullString
		if err := hrows.Scan(&ft, &fi, &tt, &ti, &fr, &tr, &rsn, &it.ActorType, &it.ActorID, &it.At); err != nil {
			return nil, err
		}
		it.Kind = "action"
		it.Action = "handoff"
		det, _ := json.Marshal(map[string]string{
			"from":        ft.String + "/" + fi.String,
			"to":          tt.String + "/" + ti.String,
			"reason":      rsn.String,
			"from_run_id": fr.String,
			"to_run_id":   tr.String,
		})
		it.Detail = string(det)
		items = append(items, it)
	}
	if err := hrows.Err(); err != nil {
		return nil, err
	}

	// 4. gate decisions — human checkpoint verdicts (approve / reject).
	drows, err := s.st.DB().QueryContext(ctx,
		`SELECT gate_rule, decision, reason, decided_by, decided_at, review_duration
		 FROM gate_decision WHERE goal_id=?`, goalID)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var it TimelineItem
		var rsn sql.NullString
		if err := drows.Scan(&it.GateRule, &it.Decision, &rsn, &it.ActorType, &it.At, &it.ReviewDurS); err != nil {
			return nil, err
		}
		it.Kind = "decision"
		it.ActorID = "" // decided_by is "human"; the frontend renders the human node
		it.Reason = rsn.String
		items = append(items, it)
	}
	if err := drows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].At < items[j].At })
	return items, nil
}

func (s *GoalService) Get(ctx context.Context, id string) (*Goal, error) {
	var g Goal
	var domainID, currentAgent sql.NullString
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT g.id,g.title,g.description,g.domain_id,g.assignee_type,g.assignee_id,g.status,g.handoff_note,g.review_request,g.human_iterations,g.created_by_type,g.created_by_id,g.created_at,g.source_ref,g.attention,
		        (SELECT r.agent_id FROM run r WHERE r.goal_id = g.id AND r.status IN ('running','queued') ORDER BY r.created_at DESC LIMIT 1),
		        CASE WHEN g.status != 'review' THEN ''
		     WHEN (SELECT COUNT(*) FROM run r WHERE r.goal_id=g.id AND r.role='review' AND r.status IN ('queued','running')) = 0 THEN 'awaiting_approval'
		     WHEN (SELECT COUNT(*) FROM run r WHERE r.goal_id=g.id AND r.role='review' AND r.status='running') > 0 THEN 'reviewing'
		     ELSE 'awaiting_review' END
		 FROM goal g WHERE g.id=?`, id).
		Scan(&g.ID, &g.Title, &g.Description, &domainID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.HandoffNote, &g.ReviewRequest, &g.HumanIterations, &g.CreatedByType, &g.CreatedByID, &g.CreatedAt, &g.SourceRef, &g.Attention, &currentAgent, &g.ReviewPhase)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.DomainID = domainID.String
	g.CurrentAgentID = currentAgent.String
	return &g, nil
}

// Assign changes a goal's assignee (the handoff path). The SERVICE layer does
// NOT cancel in-flight runs — correctness comes from ReconcileOnRunEnd: an
// orphaned run reporting in later sees run.agent != goal.assignee and its
// result is discarded without touching goal.status. The DAEMON layer does the
// resource-level cut (决策 4-9): on the goal:assigned event it terminates the
// old owner's running run via the runCancels registry, so the new owner's
// queued run is not deadlocked behind per-goal serialization.
// The new owner's run is enqueued IN THIS TRANSACTION (P0-2, 决策 6-15②) —
// both the HTTP assign surface and the MCP handoff_goal tool go through here,
// one handoff semantic.
//
// actorType/actorID name WHO performs the handoff (决策 5-6, Invariant 2):
// "agent" enforces that only the goal's CURRENT owner may hand it off
// (agent-assigned → the assignee; squad-assigned → the squad's current
// leader; human-assigned → no agent may). The HTTP handler passes "" (human)
// — single-user, the HTTP surface is the human's; the MCP handoff_goal tool
// passes the calling run's agent.
func (s *GoalService) Assign(ctx context.Context, goalID, assigneeType, assigneeID, handoffNote, actorType, actorID string) (*Goal, error) {
	if actorType == "" {
		actorType = "human"
	}
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	switch assigneeType {
	case "agent":
		if assigneeID == "" {
			return nil, NewFieldRequiredError("assignee_id")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, assigneeID, "agent"); err != nil {
			return nil, err
		}
	case "squad":
		if assigneeID == "" {
			return nil, NewFieldRequiredError("assignee_id")
		}
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, assigneeID, "squad"); err != nil {
			return nil, err
		}
	case "human":
		// unassign / placeholder
	default:
		return nil, NewValidationError("assignee_type must be agent, squad, or human")
	}
	// v2: a goal handed to an agent/squad must belong to a domain — the domain
	// owns the worktree and the acceptance policy. Assigning a domain-less
	// goal to an agent would produce a run with no worktree, no verification,
	// and no deliver (the scratch-dir dead path).
	if assigneeType != "human" && g.DomainID == "" {
		return nil, NewValidationError("cannot assign to agent/squad: the goal has no domain (attach a domain first)")
	}

	// Handoff permission (决策 5-6): an agent actor must be the goal's current
	// owner. Human actors (the HTTP surface) are not checked — single-user.
	if actorType == "agent" {
		owns, err := s.AgentOwnsGoal(ctx, g, actorID)
		if err != nil {
			return nil, err
		}
		if !owns {
			return nil, NewValidationError("only the goal's current owner can hand it off")
		}
	}

	// P0-2 (决策 6-15②): the ownership change, the audit rows, the
	// handoff-cycle park, the comments AND the new owner's run are born in
	// ONE transaction — a crash after the commit can no longer leave a
	// reassigned goal without its successor run (the startup reconciles
	// cannot resurrect it: attention derives only from changes/failed
	// sub-goals). The run event is published after the commit (invariant 13).
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var handoffLoopEvs []events.Event

	if _, err := tx.ExecContext(ctx,
		`UPDATE goal SET assignee_type=?, assignee_id=?, handoff_note=? WHERE id=?`,
		assigneeType, assigneeID, handoffNote, goalID); err != nil {
		return nil, fmt.Errorf("assign goal: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"from": g.AssigneeType + "/" + g.AssigneeID, "to": assigneeType + "/" + assigneeID})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'handoff',?,?)`,
		newID(), goalID, actorType, actorID, string(detail), now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	// Handoff audit (决策 5-9): the append-only ownership-transition record,
	// richer than the activity row — from/to run links for the timeline and
	// the cycle counter. from_run_id = the old owner's running run (the one
	// the daemon terminates); to_run_id is back-filled IN THIS TRANSACTION
	// from the enqueued run below (P0-2 — the audit row is complete the
	// moment it commits; the caller-side CompleteHandoff is gone).
	var fromRunID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND status='running' ORDER BY started_at DESC LIMIT 1`,
		goalID).Scan(&fromRunID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("find from_run for handoff: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO handoff_event (id,goal_id,from_type,from_id,to_type,to_id,from_run_id,to_run_id,reason,actor_type,actor_id,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), goalID, g.AssigneeType, g.AssigneeID, assigneeType, assigneeID, fromRunID, "", handoffNote, actorType, actorID, now()); err != nil {
		return nil, fmt.Errorf("insert handoff event: %w", err)
	}
	// Handoff-cycle detection (决策 5-7): ≥4 ownership transitions write a
	// system warning comment; ≥8 park the goal in review for a HUMAN decision
	// (approve continues, reject fails it) — a collaboration anomaly is not a
	// task failure, unlike the mention pingpong guard (决策 4-2).
	var handoffs int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM handoff_event WHERE goal_id=?`, goalID).Scan(&handoffs); err != nil {
		return nil, fmt.Errorf("count handoffs: %w", err)
	}
	if handoffs == handoffWarnThreshold {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
			newID(), goalID, fmt.Sprintf("⚠️ 所有权已交接 %d 次——协作循环风险。请确认任务方向，避免在 agent 之间来回推。", handoffs), now()); err != nil {
			return nil, fmt.Errorf("insert handoff warning: %w", err)
		}
	} else if handoffs >= handoffCycleThreshold {
		reason := fmt.Sprintf("handoff_loop: 所有权已交接 %d 次（超过上限 %d）——协作循环，需要人工决策", handoffs, handoffCycleThreshold)
		if res, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='review', review_request=? WHERE id=? AND status='active'`, reason, goalID); err != nil {
			return nil, fmt.Errorf("park handoff loop: %w", err)
		} else if n, _ := res.RowsAffected(); n > 0 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,'system','','entered_review',?,?)`,
				newID(), goalID, `{"reason":"handoff_loop"}`, now()); err != nil {
				return nil, fmt.Errorf("insert handoff-loop activity: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
				newID(), goalID, "任务停靠待审：agent 间所有权交接形成循环，请决定——批准继续（交接生效）/ 驳回（任务失败）。", now()); err != nil {
				return nil, fmt.Errorf("insert handoff-loop comment: %w", err)
			}
			logging.Infof("review: goal %q parked (handoff loop: %d handoffs — human must decide)", g.Title, handoffs)
			// The park publishes goal:reviewing like the gate park (决策 4-4:
			// handoff_loop 停靠同样发卡 — the human MUST decide). The squad
			// checkpoint skips it (handoff_loop prefix — no code to review);
			// the card fires with no reviewer hint. No run_id: no run parked
			// this goal, the handoff chain did.
			handoffLoopEvs = append(handoffLoopEvs, events.Event{Topic: "goal:reviewing", Payload: map[string]any{
				"goal_id": goalID, "reason": reason,
			}})
		}
	}
	// The handoff note is the actor's words — it belongs in the comment
	// feed (the collaboration surface), not only in the handoff_note field.
	// Written directly (not via CommentService.Create) so the mention never
	// double-triggers: the successor run is enqueued below, in-tx.
	if handoffNote != "" {
		var label string
		_ = tx.QueryRowContext(ctx,
			`SELECT name FROM agent WHERE id=?`, assigneeID).Scan(&label)
		if label == "" {
			_ = tx.QueryRowContext(ctx,
				`SELECT name FROM squad WHERE id=?`, assigneeID).Scan(&label)
		}
		if label != "" {
			content := "[@" + label + "](mention://" + assigneeType + "/" + assigneeID + ") " + handoffNote
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,?,NULL,?,?)`,
				newID(), goalID, actorType, actorID, content, now()); err != nil {
				return nil, fmt.Errorf("insert handoff comment: %w", err)
			}
		}
	}

	// The handoff supersedes any QUEUED owner-role runs of the previous
	// owner (the daemon cuts the RUNNING one via goal:assigned) — otherwise
	// the stale intents compete with the new one at claim time, and after a
	// handoff-loop approve the OLDEST queued run wins, handing the goal back
	// to the loop's first agent. Sub-goal/consult/verify runs are untouched
	// (决策 6-6: handoff only cuts the owner role). Queued-cancelled runs
	// have no reconcile semantics (never started) — the stamp is audit.
	if _, err := tx.ExecContext(ctx,
		`UPDATE run SET status='cancelled', cancel_reason='handoff' WHERE goal_id=? AND role='owner' AND status='queued'`,
		goalID); err != nil {
		return nil, fmt.Errorf("cancel superseded owner runs: %w", err)
	}

	// The successor enqueue (P0-2 + 决策 2-3 revised): the goal's FINAL
	// status decides — the handoff-loop park above may just have flipped it.
	// active → the run claims immediately; review → the run queues as a
	// DURABLE INTENT (Claim admits it when the human releases the freeze);
	// backlog/terminal → no run (Activate / comment-triggered Reopen are
	// those entries).
	var runID string
	var runEv *events.Event
	if assigneeType == "agent" || assigneeType == "squad" {
		var finalStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, goalID).Scan(&finalStatus); err != nil {
			return nil, fmt.Errorf("load goal status for handoff enqueue: %w", err)
		}
		run, ev, err := s.enqueueOwnerIntentTx(ctx, tx, goalID, assigneeType, assigneeID, finalStatus, "", "", "active", "review")
		if err != nil {
			return nil, fmt.Errorf("enqueue new owner run: %w", err)
		}
		if run != nil {
			runID = run.ID
		}
		runEv = ev
	}
	if runID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE handoff_event SET to_run_id=? WHERE goal_id=? AND to_run_id='' AND id=(SELECT id FROM handoff_event WHERE goal_id=? AND to_run_id='' ORDER BY created_at DESC LIMIT 1)`,
			runID, goalID, goalID); err != nil {
			return nil, fmt.Errorf("back-fill to_run_id: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if runEv != nil {
		s.bus.Publish(ctx, *runEv)
	}
	for _, ev := range handoffLoopEvs {
		s.bus.Publish(ctx, ev)
	}
	// The event payload carries the FRESH row — the park above may have
	// changed the status inside this transaction.
	out, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:assigned", Payload: out})
	return out, nil
}

// CompleteHandoff back-fills the new owner's run id on the goal's latest
// handoff_event (决策 5-9). Retired in production by P0-2 (决策 6-15②):
// Assign enqueues the successor IN its transaction and back-fills to_run_id
// itself — the audit row is complete the moment it commits. Kept for tests.
func (s *GoalService) CompleteHandoff(ctx context.Context, goalID, toRunID string) error {
	_, err := s.st.DB().ExecContext(ctx,
		`UPDATE handoff_event SET to_run_id=? WHERE id=(
		   SELECT id FROM handoff_event WHERE goal_id=? AND to_run_id='' ORDER BY created_at DESC LIMIT 1)`,
		toRunID, goalID)
	return err
}

// Cancel marks a non-terminal goal cancelled. Like handoff, this does not kill
// in-flight runs: correctness comes from ReconcileOnRunEnd seeing goal.status
// cancelled and discarding the result. (Killing the process is a resource
// optimization, not a correctness requirement.)
func (s *GoalService) Cancel(ctx context.Context, goalID string) (*Goal, error) {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='cancelled' WHERE id=? AND status NOT IN ('done','failed','cancelled')`,
		goalID)
	if err != nil {
		return nil, fmt.Errorf("cancel goal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	// Drop queued runs; running runs are left to report in (their result is
	// discarded by reconcile). A cancelled goal must not dispatch.
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled', cancel_reason='goal_cancelled' WHERE goal_id=? AND status='queued'`, goalID); err != nil {
		return nil, fmt.Errorf("cancel queued runs: %w", err)
	}
	// v2 (决策 6-8): cascade-cancel the goal's sub-goals — terminal ones keep
	// their history (verified/cancelled/failed stay), active work stops.
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='cancelled' WHERE goal_id=? AND status NOT IN ('verified','cancelled','failed')`, goalID); err != nil {
		return nil, fmt.Errorf("cancel sub-goals: %w", err)
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'cancelled','{}',?)`,
		newID(), goalID, "human", "", now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:finished", Payload: map[string]any{
		"goal_id": goalID, "status": "cancelled", "summary": "",
	}})
	return s.Get(ctx, goalID)
}

// Delete removes a goal and dependents. The goal's running runs are NOT
// terminated here — their rows are gone by the time the daemon could act, so
// the ids are captured BEFORE the cascade and travel in the goal:deleted
// payload; the daemon cuts the processes (same mechanism as goal cancel,
// 决策 4-12). A deleted goal must not keep agents burning compute on work
// whose rows no longer exist. The domain identity ALSO travels: a scratch
// goal's persistent project directory must be removed with the row.
func (s *GoalService) Delete(ctx context.Context, goalID string) error {
	// Load the goal title + domain identity BEFORE the cascade deletes the
	// rows. The deleted event carries these so the daemon can clean the goal's
	// git branch (决策 7-4) and scratch dir — by the time the handler runs the
	// rows are gone. git_url/git_credentials feed the remote branch delete.
	var goalTitle, domainID, domainType, domainName, gitURL, gitCredentials string
	_ = s.st.DB().QueryRowContext(ctx,
		`SELECT g.title, d.id, COALESCE(d.type,''), COALESCE(d.name,''), COALESCE(d.git_url,''), COALESCE(d.git_credentials,'') FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`, goalID).
		Scan(&goalTitle, &domainID, &domainType, &domainName, &gitURL, &gitCredentials)
	runningRows, err := s.st.DB().QueryContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND status='running'`, goalID)
	if err != nil {
		return fmt.Errorf("collect running runs: %w", err)
	}
	var runningRunIDs []string
	for runningRows.Next() {
		var id string
		if err := runningRows.Scan(&id); err != nil {
			runningRows.Close()
			return err
		}
		runningRunIDs = append(runningRunIDs, id)
	}
	runningRows.Close()
	if err := runningRows.Err(); err != nil {
		return err
	}

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM chat_message WHERE run_id IN (SELECT id FROM run WHERE goal_id=?)`,
		`DELETE FROM gate_decision WHERE goal_id=?`,
		`DELETE FROM run WHERE goal_id=?`,
		`DELETE FROM comment WHERE goal_id=?`,
		`DELETE FROM activity_log WHERE goal_id=?`,
		`DELETE FROM schedule_run WHERE goal_id=?`,
		// v2 tables (决策 6-1/6-3/6-5) — FK order: revisions → changes →
		// verifications → sub-goals, then the collaboration audit rows.
		`DELETE FROM change_revision WHERE change_id IN (SELECT id FROM change WHERE goal_id=?)`,
		`DELETE FROM change WHERE goal_id=?`,
		`DELETE FROM verification_result WHERE goal_id=?`,
		`DELETE FROM sub_goal WHERE goal_id=?`,
		`DELETE FROM consult_request WHERE goal_id=?`,
		`DELETE FROM handoff_event WHERE goal_id=?`,
		`DELETE FROM goal WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, goalID); err != nil {
			return fmt.Errorf("delete goal dependents: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:deleted", Payload: map[string]any{
		"goal_id": goalID, "run_ids": runningRunIDs,
		"goal_title": goalTitle,
		"domain_id":  domainID, "domain_type": domainType, "domain_name": domainName,
		"git_url": gitURL, "git_credentials": gitCredentials,
	}})
	return nil
}

// AgentOwnsGoal reports whether agentID is the goal's current owner
// (决策 5-6, Collaboration.md Invariant 2): agent-assigned → the assignee;
// squad-assigned → the squad's CURRENT leader (dynamic, like ownRunByGoal);
// human-assigned → nobody (no agent owns it). The ownership permission anchor
// for handoff/consult/sub-goal MCP tools.
func (s *GoalService) AgentOwnsGoal(ctx context.Context, g *Goal, agentID string) (bool, error) {
	switch g.AssigneeType {
	case "agent":
		return g.AssigneeID == agentID, nil
	case "squad":
		var leaderID string
		err := s.st.DB().QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, g.AssigneeID).Scan(&leaderID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("load squad leader: %w", err)
		}
		return leaderID == agentID, nil
	default:
		return false, nil
	}
}

// ownRunByGoal reports whether the reporting run still belongs to the goal's
// current assignee AND carries owner authority. This is the gate that makes
// handoff/cancel/reassign self-consistent without an external authority
// (DESIGN.md).
//
//	agent-assigned goal: the run's agent must equal the goal's assignee AND
//	  the run must be role='owner' (P0-4, 决策 6-15④, Invariant 6). A
//	  mention-triggered run (role=consult/review) landing on the assignee is
//	  a GUEST — it is read-only and its completion must not advance the goal
//	  (live hazard: a human asking the owner a question during the
//	  timeout-cancelled window "completed" the goal on an empty diff).
//	  All platform spawn paths (EnqueueOwnerRun/retry/consult-resume) enqueue
//	  with an empty trigger and resolve to owner — unaffected.
//	squad-assigned goal: the run's agent must be the squad's CURRENT leader
//	  (judged dynamically at reconcile time, NOT from the run's static
//	  is_leader_run mark) AND role='owner' — same Invariant-6 gate as the
//	  agent branch: a mention://agent/<leader> consult (role=consult) is a
//	  guest, while a mention://squad/<id> mention (isLeader=true) and all
//	  platform spawns resolve to owner and keep authority. A leader change
//	  orphans the prior leader's in-flight run for free.
//	human-assigned goal: never has an agent-run owner — fall through to false.
func (s *GoalService) ownRunByGoal(ctx context.Context, tx *sql.Tx, rc goalRunContext, g Goal) (bool, error) {
	switch g.AssigneeType {
	case "agent":
		return rc.Role == "owner" && rc.AgentID == g.AssigneeID && !rc.IsLeaderRun, nil
	case "squad":
		var leaderID string
		err := tx.QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, g.AssigneeID).Scan(&leaderID)
		if err != nil {
			return false, fmt.Errorf("load squad leader: %w", err)
		}
		return rc.Role == "owner" && rc.AgentID == leaderID, nil
	default:
		return false, nil // human-assigned
	}
}

// ReconcileOnRunEnd is the ONLY place that advances a goal's status based on a
// run's terminal outcome. It is called by the daemon after a run reaches a
// terminal status (completed/failed/cancelled). Everything else — handoff,
// cancel — changes goal.status without consulting a run's result. See DESIGN.md
//
// Rules:
//
//	sub-goal runs report to the sub-goal layer (ReconcileSubGoalRun);
//	verify runs just land their report (ReconcileVerifyRun) — neither
//	  touches goal.status
//	run.agent != current assignee  → discard (handoff/reassign orphaned run)
//	goal.status == cancelled       → discard
//	run completed, no pending sub-goals/changes → gate judgment → done | review
//	run completed, work still pending → stay active (the Coordinator's
//	  attention loop owns the next wake, 决策 6-4/6-8)
//	run failed, attempts left → enqueue a retry run (attempt+1)
//	run failed, attempts exhausted → goal → failed
//	run cancelled → the goal stays where it is (timeout/handoff — 决策 2-6/6-6)
//
// insertRunResultComment was RETIRED (决策 4-4 revised): the platform no
// longer extracts and posts the agent's final message as a comment. The
// agent communicates results via `goal comment` (its own CLI call); the
// run's output stream lives in chat_message. The platform only writes a
// comment on FAILURE (system comment — the agent had no chance to post).

// cancelReasonDescription maps a structured cancel_reason code to a short
// human-readable phrase (English — platform text stays fixed per 决策 6-18).
// Used by the cancelled-run feed comment and the run:cancelled event reason.
func cancelReasonDescription(code string) string {
	switch code {
	case "idle_watchdog":
		return "agent was silent beyond the idle window"
	case "timeout":
		return "exceeded max run duration"
	case "handoff":
		return "goal reassigned to another agent"
	case "stopped":
		return "stopped by user"
	case "runaway":
		return "run exceeded time budget and was reaped"
	case "goal_terminal":
		return "goal reached a terminal state"
	case "goal_cancelled":
		return "goal was cancelled"
	default:
		return code
	}
}

// insertCancelledRunComment posts a SYSTEM comment for a cancelled run — the
// platform's voice (author_type='system'), not the agent's. A cancelled run's
// summary is "cancelled by platform" (platform noise, not the agent's words),
// so it must NOT be authored as the agent. The comment threads to the run's
// trigger comment (or goal root / wake anchor fallback — same threading logic
// the retired insertRunResultComment used). Returns early when there is no
// structured cancel_reason to report.
func insertCancelledRunComment(ctx context.Context, tx *sql.Tx, rc goalRunContext) (string, error) {
	if rc.CancelReason == "" {
		return "", nil
	}
	id := newID()
	var parentID any
	if rc.TriggerCommentID != "" {
		parentID = rc.TriggerCommentID
	} else {
		var root string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM comment WHERE goal_id=? ORDER BY created_at ASC LIMIT 1`, rc.GoalID).Scan(&root); err == nil && root != "" {
			parentID = root
		} else if rc.RunID != "" {
			var anchor string
			if err := tx.QueryRowContext(ctx, `SELECT wake_anchor FROM run WHERE id=?`, rc.RunID).Scan(&anchor); err == nil && anchor != "" {
				parentID = anchor
			}
		}
	}
	content := "Run cancelled: " + cancelReasonDescription(rc.CancelReason) + " (" + rc.CancelReason + ")"
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?,'system','',?,?,?,?)`,
		id, rc.GoalID, parentID, content, now(), rc.RunID); err != nil {
		return "", fmt.Errorf("insert cancelled-run comment: %w", err)
	}
	return id, nil
}

// ReconcileOnRunEnd runs under withBusyRetry for the same snapshot-upgrade
// reason as ReconcileGoal — a BUSY here would drop the reconcile AND the
// run.terminal publish (Finish returns the error), which is a worse failure
// than the retry's cost.
func (s *GoalService) ReconcileOnRunEnd(ctx context.Context, rc goalRunContext) error {
	return withBusyRetry(func() error { return s.reconcileOnRunEndOnce(ctx, rc) })
}

func (s *GoalService) reconcileOnRunEndOnce(ctx context.Context, rc goalRunContext) error {
	// Events are collected here and published ONLY after the tx commits, so a
	// failed commit can never leave the bus with an event whose DB change
	// rolled back (DESIGN "bus.Publish after commit"). Failure-side effects
	// that need a fresh transaction (retry enqueue) are also deferred to after
	// commit.
	var pendingEvents []events.Event
	var afterCommit []func()

	// Sub-goal runs report to the SUB-GOAL layer (决策 6-1): the goal's state
	// machine is not their business — a sub-goal run completing verifies the
	// work item, never the goal. Verify runs (决策 6-5) just land their report
	// — the verdict tool already made the transition.
	if rc.Role == "subgoal" {
		return s.ReconcileSubGoalRun(ctx, rc)
	}
	if rc.Role == "verify" {
		return s.ReconcileVerifyRun(ctx, rc)
	}

	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var g Goal
	err = tx.QueryRowContext(ctx,
		`SELECT id,assignee_type,assignee_id,status,title FROM goal WHERE id=?`, rc.GoalID).
		Scan(&g.ID, &g.AssigneeType, &g.AssigneeID, &g.Status, &g.Title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // goal vanished; nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("load goal for reconcile: %w", err)
	}
	owns, err := s.ownRunByGoal(ctx, tx, rc, g)
	if err != nil {
		return err
	}
	if !owns {
		// Orphaned run (handoff/reassign/leader-change) or a guest run
		// (mention-triggered collaboration). Its result has no authority over
		// the goal. A FAILED collaboration run is not silent though — the
		// human waiting at a checkpoint must see that the review/help run
		// failed, not an empty request (the guest-failure path: no retry, no
		// goal effect, but a durable trace in the feed).
		if rc.Status == "failed" && rc.TriggerCommentID != "" {
			summary := strings.TrimSpace(rc.Summary)
			if len(summary) > 200 {
				summary = summary[:200] + "…"
			}
			content := "协作 run 失败：" + summary
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at,run_id) VALUES (?,?,'system','',NULL,?,?,?)`,
				newID(), rc.GoalID, content, now(), rc.RunID); err != nil {
				return fmt.Errorf("insert guest-failure comment: %w", err)
			}
		}
		// A COMPLETED run's report is NOT posted by the platform — the agent
		// communicates results via `goal comment` (its own CLI call). The run's
		// output stream lives in chat_message (run detail view). Only failures
		// leave a platform comment (the guest-failure comment above).
		// Consult closure (决策 5-8): a completed GUEST run answers a consult —
		// the guest's answer is the agent's own `goal comment` (carrying
		// run_id=guest_run_id, consultStatus joins on it). Auto-resume the
		// requester (a fresh run, attempt 1, no trigger comment — the resume
		// must not stack the mention-cycle counter) when the requester still
		// owns an active goal. A FAILED/CANCELLED guest is itself the answer
		// ("your consult did not come back"; the feed carries the guest-failure
		// comment) — the requester resumes on ANY terminal guest outcome, or
		// its plan silently dies.
		if rc.Status == "completed" || rc.Status == "failed" || rc.Status == "cancelled" {
			var requesterAgent, requesterRunID string
			err := tx.QueryRowContext(ctx,
				`SELECT requester_agent_id, requester_run_id FROM consult_request WHERE guest_run_id=?`,
				rc.RunID).Scan(&requesterAgent, &requesterRunID)
			if err == nil && requesterAgent != "" {
				owns, oerr := s.AgentOwnsGoal(ctx, &g, requesterAgent)
				if oerr != nil {
					return oerr
				}
				if owns && g.Status == "active" {
					// P0-3 (决策 6-13): the resume run is born IN this
					// transaction — a crash can no longer lose the requester's
					// successor run. An enqueue failure is only logged (the
					// consult's outcome is already in the feed).
					_, runEv, err := s.runSvc.EnqueueExistingTx(ctx, tx, rc.GoalID, requesterAgent, 1, false, "", "", "")
					if err != nil {
						afterCommit = append(afterCommit, func() {
							logging.Infof("goal: consult resume enqueue for %s: %v", rc.GoalID, err)
						})
					} else if runEv != nil {
						pendingEvents = append(pendingEvents, *runEv)
					}
				}
			}
		}
		pendingEvents = append(pendingEvents, events.Event{Topic: "run:discarded", Payload: map[string]any{
			"run_id": rc.RunID, "goal_id": rc.GoalID, "reason": "orphaned",
		}})
		return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit, rc.RunID)
	}
	if g.Status == "cancelled" {
		// Goal was cancelled while this run was in flight. Discard.
		pendingEvents = append(pendingEvents, events.Event{Topic: "run:discarded", Payload: map[string]any{
			"run_id": rc.RunID, "goal_id": rc.GoalID, "reason": "cancelled",
		}})
		return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit, rc.RunID)
	}

	// Stale-conflict cleanup: a conflict change whose head_ref is already an
	// ancestor of the goal branch HEAD is stale — the content landed via
	// another path but the row was never marked integrated. These block the
	// finalization guard below forever (the failed sub-goal can't re-run to
	// produce a new revision). The ancestry check runs OUTSIDE the tx (git
	// operates on the worktree); only the UPDATE is inside. Non-fatal on
	// resolver error — proceed without cleanup rather than blocking
	// reconcile (a missing worktree or git fault should not strand the goal).
	// Runs BEFORE the status switch so a cancelled/failed owner run (e.g. an
	// idle-watchdog cancel) also clears the deadlock — the stale conflicts are
	// independent of how this particular run ended.
	if s.staleConf != nil {
		staleIDs, stErr := s.staleConf.StaleConflictChanges(ctx, rc.GoalID)
		if stErr != nil {
			logging.Infof("goal: %q stale-conflict resolve skipped: %v", g.Title, stErr)
		} else if len(staleIDs) > 0 {
			for _, id := range staleIDs {
				if _, err := tx.ExecContext(ctx,
					`UPDATE change SET status='integrated' WHERE id=? AND status='conflict'`, id); err != nil {
					return fmt.Errorf("clear stale conflict: %w", err)
				}
			}
			logging.Infof("goal: %q cleared %d stale conflict change(s)", g.Title, len(staleIDs))
		}
	}

	switch rc.Status {
	case "completed":
		// The run's report is NOT posted by the platform — the agent
		// communicates results via `goal comment`. The run's output stream
		// lives in chat_message (run detail view).
		// v2 finalization guard (决策 6-1/6-8): the owner run only REACHES
		// the acceptance judgment when nothing is pending — non-terminal
		// sub-goals (still working), ready/conflicted changes (not yet
		// integrated), or IN-FLIGHT CONSULTS (the owner ended its turn
		// waiting for an answer; the resume only lands when the guest goes
		// terminal — success or failure) keep the goal active. The
		// Coordinator's attention spawn (run.terminal → ReconcileGoal) wakes
		// the owner again for integration/wrapup/recovery; a sub-goal completing
		// never completes the goal, and the human gate fires only on the
		// FINAL state.
		var pendingSG, pendingChanges, pendingConsults int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sub_goal WHERE goal_id=? AND status NOT IN ('verified','cancelled','failed')`,
			rc.GoalID).Scan(&pendingSG); err != nil {
			return fmt.Errorf("count pending sub-goals: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM change WHERE goal_id=? AND status IN ('ready','conflict')`,
			rc.GoalID).Scan(&pendingChanges); err != nil {
			return fmt.Errorf("count pending changes: %w", err)
		}
		// P0-6 (决策 6-15⑤): the guard protects the OWNER's unfinished plan —
		// only consults THIS owner requested (consult_request.requester_agent_id
		// = the completing run's agent) hold the goal open. A HUMAN's consult
		// has no consult_request row and must not block the gate: its guest
		// completes without a resume, and a counted-but-never-resumed consult
		// left the goal dead active (no attention signal wakes the owner).
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run r
			 JOIN consult_request cr ON cr.guest_run_id = r.id
			 WHERE r.goal_id=? AND r.role='consult' AND r.status IN ('queued','running')
			   AND cr.requester_agent_id=?`,
			rc.GoalID, rc.AgentID).Scan(&pendingConsults); err != nil {
			return fmt.Errorf("count pending consults: %w", err)
		}
		// 决策 7-3 延伸: an agent's --ask question to the human is a WAIT the
		// owner is mid-flight on — the owner run ends its turn asking, and the
		// goal must NOT park in review / promote to done before the human
		// replies (their reply wakes the owner via parent_id routing). A
		// pending ask = an agent-authored ask_human comment with NO human reply
		// threading under it. Once the human replies (parent_id → ask comment,
		// author human) the ask is answered and no longer holds — the owner's
		// reply-triggered successor run reaches the gate normally when IT ends.
		// This mirrors the consult hold above but for the agent→human channel
		// (which has no consult_request row — decision 7-3 D).
		//
		// Stale-ask narrowing: only ask_human from THIS run holds — an ask
		// from a PRIOR run is stale (that run ended, a newer run completed
		// past it; the question's context is gone). Without this, an answered-
		// in-effect ask (the deadlock it asked about was resolved by a later
		// run) strands the goal at the guard forever (live: a 2026-08-28 ask
		// about conflict changes blocked review after all conflicts cleared).
		var pendingAskHuman int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM comment ask
			 WHERE ask.goal_id=? AND ask.ask_human=1 AND ask.author_type='agent'
			   AND ask.run_id=?
			   AND NOT EXISTS (
			     SELECT 1 FROM comment rep
			     WHERE rep.parent_id = ask.id AND rep.author_type='human'
			   )`,
			rc.GoalID, rc.RunID).Scan(&pendingAskHuman); err != nil {
			return fmt.Errorf("count pending ask-human: %w", err)
		}
		if pendingSG > 0 || pendingChanges > 0 || pendingConsults > 0 || pendingAskHuman > 0 {
			// A WAIT is open: the run ended but the goal holds active until
			// the pending work lands — log the hold (otherwise a finished
			// run with no advancement looks like a hang).
			logging.Infof("goal: %q run %s done but finalization guard holds (sub-goals=%d changes=%d consults=%d ask-human=%d) — no gate yet",
				g.Title, rc.RunID, pendingSG, pendingChanges, pendingConsults, pendingAskHuman)
			break // stay active — the attention loop owns the next step
		}
		// A completed run that passed machine verification has reached the
		// acceptance-judgment step (D2: inside this transaction). If the
		// goal's domain has checkpoint gates, the goal parks in review and
		// the human decides; otherwise it promotes to done as before.
		// Note: machine verification failures never reach here — the daemon
		// runs verify before finishing the run and a red verify ends the run
		// failed, flowing through the retry branch below.
		hit, reason, err := s.gatesForGoal(ctx, tx, rc)
		if err != nil {
			return fmt.Errorf("check gates: %w", err)
		}
		if hit {
			// Park in review. The handoff/wakeup note is NOT cleared — if the
			// human rejects, the next run resumes from it.
			res, err := tx.ExecContext(ctx,
				`UPDATE goal SET status='review', review_request=? WHERE id=? AND status NOT IN ('done','failed','cancelled','review')`,
				reason, rc.GoalID)
			if err != nil {
				return fmt.Errorf("park goal in review: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				// The goal is not parkable (already review / terminal):
				// the gate cannot fire here. NO activity, NO
				// reviewing event — a fake park must not mislead the review
				// trigger into firing on a non-review goal.
				break
			}
			// The activity trail must record the REAL reason that parked the
			// goal — a hardcoded {"gate":"merge"} mislabels diff_contains hits,
			// the unfrozen-policy gate, and the weak-strength default. Same
			// {"reason": ...} shape as requested_review / parked_review.
			detail, _ := json.Marshal(map[string]string{"reason": reason})
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'entered_review',?,?)`,
				newID(), rc.GoalID, "system", "", string(detail), now()); err != nil {
				return fmt.Errorf("insert review activity: %w", err)
			}
			// The squad's reviewer runs are the park's SUCCESSOR (决策 6-13/
			// 6-19): born in THIS transaction, so the goal:reviewing consumers
			// (the approval card's pending-reviewer hint) always see them — an
			// event-handler enqueue raced the notify handler and fired the
			// card WITHOUT the "审查中" hint while the reviewer was starting.
			if err := s.enqueueSquadReviewTx(ctx, tx, rc.GoalID, rc.RunID, reason); err != nil {
				return fmt.Errorf("enqueue squad review: %w", err)
			}
			logging.Infof("review: goal %q parked (gate hit: %q)", g.Title, trimLog(reason, 80))
			pendingEvents = append(pendingEvents, events.Event{Topic: "goal:reviewing", Payload: map[string]any{
				"goal_id": rc.GoalID, "run_id": rc.RunID, "reason": reason,
			}})
			break
		}
		// Clear the handoff/wakeup note as part of the same transaction that
		// promotes the goal — once this turn legitimately completes, the note
		// (a scoping instruction or child summary) is consumed. (See
		// DESIGN: the daemon no longer clears handoff_note itself; only the
		// goal layer, after confirming the run owns the goal, does.)
		if res, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='done', handoff_note='' WHERE id=? AND status NOT IN ('done','failed','cancelled','review')`,
			rc.GoalID); err != nil {
			return fmt.Errorf("promote goal done: %w", err)
		} else if n, _ := res.RowsAffected(); n == 0 {
			break // the goal moved elsewhere (review/terminal raced) — no promote
		}
		// The goal reached a terminal state — queued runs (a mention that
		// raced ahead) must not be claimed onto a finished goal.
		if _, err := tx.ExecContext(ctx,
			`UPDATE run SET status='cancelled', cancel_reason='goal_terminal' WHERE goal_id=? AND status='queued'`, rc.GoalID); err != nil {
			return fmt.Errorf("cancel queued runs on done: %w", err)
		}
		// goal:finished fires ONLY when the goal actually reached a terminal
		// state (决策 5-10): review-parked / cancelled run ends emit no
		// goal-level event.
		// The summary for the done card is the agent's latest comment (its
		// result/answer — 决策 4-4 revised: completed runs no longer carry a
		// platform-extracted result_summary; the agent communicates via goal
		// comment). The notify layer renders this as the Feishu completion
		// card's body (lark_md).
		doneSummary := ""
		_ = tx.QueryRowContext(ctx,
			`SELECT content FROM comment WHERE goal_id=? AND author_type='agent' ORDER BY created_at DESC LIMIT 1`,
			rc.GoalID).Scan(&doneSummary)
		pendingEvents = append(pendingEvents, events.Event{Topic: "goal:finished", Payload: map[string]any{
			"goal_id": rc.GoalID, "status": "done", "summary": doneSummary,
		}})

	case "failed":
		if rc.Attempt < maxAttempts {
			// Retry: enqueue a fresh run at attempt+1 on the same agent so
			// history is preserved. P0-3 (决策 6-13): the retry run is born
			// IN this transaction — a crash after the commit can no longer
			// lose the successor run. The retry events publish after the
			// commit (invariant 13, via commitAndEmit).
			attempt := rc.Attempt + 1
			run, runEv, err := s.runSvc.EnqueueExistingTx(ctx, tx, rc.GoalID, rc.AgentID, attempt, rc.IsLeaderRun, rc.SquadID, "", "")
			if err != nil {
				pendingEvents = append(pendingEvents, events.Event{Topic: "goal:retry_failed", Payload: map[string]any{
					"goal_id": rc.GoalID, "error": err.Error(),
				}})
			} else {
				// Backoff: the successor run is NOT claimable until its
				// queued_at is due. Without it, a transient failure burns
				// the whole retry budget in one second (live: three dispatch
				// writes timed out on a frozen link and the goal died
				// instantly — "重试" was theater).
				delay := retryBackoffFor(attempt)
				if _, err := tx.ExecContext(ctx,
					`UPDATE run SET queued_at=? WHERE id=?`,
					time.Now().UTC().Add(delay).Format(time.RFC3339Nano), run.ID); err != nil {
					pendingEvents = append(pendingEvents, events.Event{Topic: "goal:retry_failed", Payload: map[string]any{
						"goal_id": rc.GoalID, "error": fmt.Sprintf("backoff: %v", err),
					}})
				} else {
					if runEv != nil {
						pendingEvents = append(pendingEvents, *runEv)
					}
					pendingEvents = append(pendingEvents, events.Event{Topic: "goal:retrying", Payload: map[string]any{
						"goal_id": rc.GoalID, "attempt": attempt, "backoff_seconds": int(delay.Seconds()),
					}})
				}
			}
		} else {
			if _, err := tx.ExecContext(ctx,
				`UPDATE goal SET status='failed' WHERE id=? AND status NOT IN ('done','failed','cancelled','review')`,
				rc.GoalID); err != nil {
				return fmt.Errorf("fail goal: %w", err)
			}
			// Terminal state — drop queued runs (same rule as the done path).
			if _, err := tx.ExecContext(ctx,
				`UPDATE run SET status='cancelled', cancel_reason='goal_terminal' WHERE goal_id=? AND status='queued'`, rc.GoalID); err != nil {
				return fmt.Errorf("cancel queued runs on fail: %w", err)
			}
			// The goal ACTUALLY failed here (attempts exhausted) — the failure
			// card fires once, not once per failed attempt (决策 5-10).
			pendingEvents = append(pendingEvents, events.Event{Topic: "goal:finished", Payload: map[string]any{
				"goal_id": rc.GoalID, "status": "failed", "summary": rc.Summary,
				"agent_ran": rc.SessionID != "",
			}})
		}
	case "cancelled":
		// The run ended without completing or failing the goal: a timeout /
		// watchdog cut or a handoff cut (cancel_reason='handoff', 决策 6-6).
		// The goal stays exactly where it is — no goal-level event (决策 5-10).
		// Post a SYSTEM feed comment so the cancellation cause is visible in
		// the goal feed (the summary "cancelled by platform" is platform noise,
		// not the agent's words — author as system, not agent).
		if _, err := insertCancelledRunComment(ctx, tx, rc); err != nil {
			return err
		}
	}

	return s.commitAndEmit(ctx, tx, pendingEvents, afterCommit, rc.RunID)
}

// gatesForGoal decides whether a completed run parks the goal in review
// (DESIGN.md §5, M2 rule engine):
//
//  1. The daemon evaluated the gate rules against the run's diff and
//     recorded the fired gates on the run row (run.gates_hit) — merge always
//     fires, diff_* fire on pattern match. The goal layer reads that result:
//     the daemon computes, the goal layer judges.
//  2. Strength linkage (§5.4): a weak-verification domain with no configured
//     gates still gets a default merge gate — weak verification must not run
//     unattended.
//
// (The `request` gate is handled directly at the approval request site; it
// no longer exists — agents cannot request approval, 决策 4-11.)
func (s *GoalService) gatesForGoal(ctx context.Context, tx *sql.Tx, rc goalRunContext) (bool, string, error) {
	var gatesHitJSON string
	err := tx.QueryRowContext(ctx, `SELECT gates_hit FROM run WHERE id=?`, rc.RunID).Scan(&gatesHitJSON)
	if err == nil && gatesHitJSON != "" && gatesHitJSON != "[]" {
		var hit []string
		if json.Unmarshal([]byte(gatesHitJSON), &hit) == nil && len(hit) > 0 {
			return true, strings.Join(hit, "; "), nil
		}
	}
	// No gate fired for this run. Strength linkage: weak verification with no
	// gates at all still demands a human checkpoint.
	var checksJSON, strength, compiledAt, domainType string
	err = tx.QueryRowContext(ctx,
		`SELECT d.checks, d.verification_strength, d.checks_compiled_at, COALESCE(d.type,'') FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`, rc.GoalID).
		Scan(&checksJSON, &strength, &compiledAt, &domainType)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil // no domain → no gates
	}
	if err != nil {
		return false, "", fmt.Errorf("load domain gates: %w", err)
	}
	// A scratch domain has no diff — its diff-based gates NEVER fire, and
	// the deliverable is a subjective artifact the machine cannot judge. The
	// human checkpoint is unconditional here (regardless of strength or
	// configured gates): zero-checkpoint scratch goals must not auto-done.
	// No review_request text — the approval panel itself communicates "your
	// decision is needed"; the internal reason (no git / no policy / weak
	// strength) is platform jargon the user does not need.
	if domainType == "scratch" {
		return true, "", nil
	}
	// The confirmation gate (决策 2-4/2-5): an UNFROZEN acceptance policy is
	// no acceptance policy — nothing was run against it (the daemon skips
	// verification for unfrozen domains), so no machine judgment exists and
	// the goal must NOT promote unattended. The human checkpoint is the
	// only safe default.
	if compiledAt == "" {
		return true, "", nil
	}
	var checks Checks
	if err := json.Unmarshal([]byte(checksJSON), &checks); err != nil {
		return false, "", fmt.Errorf("parse domain checks: %w", err)
	}
	if len(checks.Gates) > 0 {
		return false, "", nil // gates configured but none fired for this run
	}
	if strength == "weak" {
		return true, "", nil
	}
	return false, "", nil
}

// ResolveReview handles a human checkpoint decision (DESIGN.md §4/§5):
//
//	approve  → record the gate_decision, keep the goal in review, publish
//	           goal:approved — the daemon runs the deliver step (merge +
//	           re-verify + push) and closes with MarkDelivered.
//	reject/redirect → record the gate_decision, bump human_iterations (the
//	           reject counter, SEPARATE from run.attempt), move the goal back
//	           to active with the reason as handoff_note, and enqueue a new
//	           run on the current assignee — the agent continues in the same
//	           worktree, working from the decision note.
//
// runID links the decision to the evidence run the human judged (the audit
// chain, gate_decision.run_id). The Web resolves the goal without naming a
// run (” — its review panel shows the latest completed run); the IM
// approval card carries the run id in the button value, so the decision
// lands on exactly the run whose evidence the card displayed.
//
// decidedBy is "human" (the Web/IM approval) or "system" (digest/import
// auto-approve). It stamps gate_decision.decided_by AND the decision comment's
// author_type — a system auto-approve must not masquerade as a human comment.
func (s *GoalService) ResolveReview(ctx context.Context, goalID, runID, decision, reason, decidedBy string) (*Goal, error) {
	if decision != "approve" && decision != "reject" && decision != "redirect" {
		return nil, NewValidationError("decision must be approve, reject, or redirect")
	}
	if decidedBy == "" {
		decidedBy = "human"
	}
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if g.Status != "review" {
		logging.Warnf("review: goal %q decision=%s REJECTED (goal is %s, not review)", g.Title, decision, g.Status)
		return nil, NewCodedError(CodeGoalNotInReview, "goal is not in review")
	}
	// Duplicate-decision guard: the deliver step runs ASYNC after an approve,
	// and the goal stays in review until it finishes — a second decision in
	// that window would race the merge. Both directions are guarded:
	//   - re-approve: the human clicking again (the page shows no feedback
	//     while deliver runs) would pile up gate_decision rows.
	//   - reject after approve: the goal would go back to active + a new run
	//     while the deliver may already have pushed — the agent would then
	//     continue on a branch whose work is already in the default branch.
	// EXCEPTION: a FAILED deliver annotates review_request ("deliver: ...")
	// and BOTH decisions must be allowed — re-approve retries the deliver,
	// reject sends the agent back to fix the conflict (the designed paths).
	deliverFailed := strings.HasPrefix(g.ReviewRequest, "deliver:")
	// A handoff_loop park (决策 5-7) has no deliver step at all — the
	// duplicate-decision guard (which protects the async merge) does not apply.
	handoffLoopPark := strings.HasPrefix(g.ReviewRequest, "handoff_loop:")
	if decision == "approve" || decision == "reject" || decision == "redirect" {
		var lastDecision, lastRunID string
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT decision, run_id FROM gate_decision WHERE goal_id=? ORDER BY decided_at DESC LIMIT 1`, goalID).
			Scan(&lastDecision, &lastRunID)
		// The duplicate-decision guard protects the ASYNC deliver window of
		// the CURRENT park — it must not fire across park cycles. A goal
		// reopened by mention parks again with a NEW evidence run; the old
		// cycle's approve is history (live: the reopened goal's approve was
		// rejected with "already approved" because the FIRST cycle's row was
		// the latest decision).
		if err == nil && !deliverFailed && !handoffLoopPark && lastRunID == runID {
			if decision == "approve" && lastDecision == "approve" {
				// A human action with NO trace (no decision row, no log) is
				// indistinguishable from a broken button — the rejected
				// click must leave a log line with the reason.
				logging.Warnf("review: goal %q decision=%s REJECTED (already approved — deliver in flight or its result pending)", g.Title, decision)
				return nil, NewValidationError("goal already approved — waiting for the deliver step (or check the deliver result)")
			}
			if (decision == "reject" || decision == "redirect") && lastDecision == "approve" {
				logging.Warnf("review: goal %q decision=%s REJECTED (being delivered — available after the deliver finishes or fails)", g.Title, decision)
				return nil, NewValidationError("goal is being delivered — reject is available after the deliver finishes or fails")
			}
		}
	}
	ts := now()
	// gate_rule is WHICH rule actually parked the goal — resolved from the
	// evidence run's gates_hit (the daemon records the fired gate names
	// there), NOT hardcoded: the health-learning aggregation (GateStats)
	// groups by rule, and a diff_contains decision recorded as "merge" would
	// corrupt the learning data.
	rule, err := s.resolveGateRule(ctx, goalID, runID)
	if err != nil {
		return nil, err
	}
	// review_duration: seconds spent in review before this decision — the
	// health-learning data source (gate_decision.review_duration). Measured
	// from the most recent entry into review (any of the three park paths).
	duration := 0
	var enteredAt string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT created_at FROM activity_log WHERE goal_id=? AND action IN ('entered_review','requested_review','parked_review','review_ready') ORDER BY created_at DESC LIMIT 1`,
		goalID).Scan(&enteredAt); err == nil && enteredAt != "" {
		if et, err := time.Parse(time.RFC3339Nano, enteredAt); err == nil {
			if dt, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				if secs := int(dt.Sub(et).Seconds()); secs > 0 {
					duration = secs
				}
			}
		}
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO gate_decision (id,goal_id,run_id,gate_rule,decision,reason,decided_by,decided_at,review_duration) VALUES (?,?,?,?,?,?,?,?,?)`,
		newID(), goalID, runID, rule, decision, reason, decidedBy, ts, duration); err != nil {
		return nil, fmt.Errorf("insert gate_decision: %w", err)
	}
	// The decision is what CLOSES the review wait (approve → deliver,
	// reject → agent re-runs) — the checkpoint's resolution must be in the
	// log, not just the gate_decision row.
	reasonSuffix := ""
	if reason != "" {
		reasonSuffix = fmt.Sprintf(" (reason=%q)", trimLog(reason, 60))
	}
	logging.Infof("review: goal %q decision=%s by %s%s", g.Title, decision, decidedBy, reasonSuffix)
	// The decision's reason is the human's words — the comment feed is where
	// the agent's next run reads recent human comments, so the reject reason
	// must be there (not only in gate_decision). 决策 7-2: reject/redirect
	// ALWAYS leaves a comment (even with an empty reason — a bare "驳回") so
	// the reject wake has an anchor (the comment id rides wake_anchor into the
	// owner successor run). reject does NOT write goal.handoff_note (the
	// handoff field is reserved for ownership transitions; reusing it caused
	// "reject treated as handoff" semantic corruption and let a real handoff
	// overwrite the reject reason). approve also leaves a comment for the
	// audit feed (unchanged behavior when reason is non-empty).
	decisionCommentID := ""
	if decision == "reject" || decision == "redirect" || reason != "" {
		verb := map[string]string{"approve": "批准", "reject": "驳回", "redirect": "改判"}[decision]
		decisionCommentID = newID()
		content := verb
		if reason != "" {
			content = verb + "：" + reason
		}
		if _, err := s.st.DB().ExecContext(ctx,
			`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'',NULL,?,?)`,
			decisionCommentID, goalID, decidedBy, content, ts); err != nil {
			return nil, fmt.Errorf("insert decision comment: %w", err)
		}
	}

	switch decision {
	case "approve":
		// Handoff-loop park (决策 5-7): there is nothing to deliver — the
		// handoff is already in effect. Approve releases the goal back to
		// active (the new owner's run proceeds); the deliver step must NOT
		// run for this park type.
		if strings.HasPrefix(g.ReviewRequest, "handoff_loop:") {
			if _, err := s.st.DB().ExecContext(ctx,
				`UPDATE goal SET status='active', review_request='' WHERE id=? AND status='review'`, goalID); err != nil {
				return nil, fmt.Errorf("release handoff loop: %w", err)
			}
			break
		}
		// Stay in review; the daemon's deliver step is the only mover from
		// here (MarkDelivered closes it).
		s.bus.Publish(ctx, events.Event{Topic: "goal:approved", Payload: map[string]any{
			"goal_id": goalID, "reason": reason,
		}})
	case "reject", "redirect":
		// Handoff-loop park: reject = the human stops the loop — the goal
		// fails (a human decision, not an automatic failure).
		if strings.HasPrefix(g.ReviewRequest, "handoff_loop:") {
			loopNote := "Handoff loop rejected"
			if reason != "" {
				loopNote += ": " + reason
			}
			if _, err := s.st.DB().ExecContext(ctx,
				`UPDATE goal SET status='failed', handoff_note=?, review_request='' WHERE id=? AND status='review'`,
				loopNote, goalID); err != nil {
				return nil, fmt.Errorf("fail handoff loop: %w", err)
			}
			if _, err := s.st.DB().ExecContext(ctx,
				`UPDATE run SET status='cancelled', cancel_reason='goal_terminal' WHERE goal_id=? AND status='queued'`, goalID); err != nil {
				return nil, fmt.Errorf("cancel queued runs on handoff-loop fail: %w", err)
			}
			s.bus.Publish(ctx, events.Event{Topic: "goal:finished", Payload: map[string]any{
				"goal_id": goalID, "status": "failed", "summary": loopNote,
				"agent_ran": false,
			}})
			break
		}
		// 决策 7-2: reject does NOT write goal.handoff_note — that field is
		// reserved for ownership transitions (handoff). Reusing it caused
		// "reject treated as handoff" in assemblePrompt (the handoff wake
		// branch fired, injecting "Previous owner's last report" — wrong
		// label: the owner was never changed, only paused for review) and let
		// a subsequent real handoff overwrite the reject reason. The reject
		// reason rides the comment feed (the reject comment's id is the wake
		// anchor for the owner successor run).
		// P0-2 (决策 6-15②): the review → active transition and the successor
		// run are born in ONE transaction — a crash between the UPDATE and
		// the enqueue used to leave a run-less active goal that no startup
		// reconcile can resurrect.
		tx, err := s.st.DB().BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		// Clear handoff_note on reject: a prior handoff's note must not leak
		// into the reject wake (assemblePrompt's handoff branch checks
		// handoff_note != ""). human_iterations is the reject counter
		// (SEPARATE from run.attempt, DESIGN.md §4).
		res, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='active', handoff_note='', human_iterations=human_iterations+1, review_request='' WHERE id=? AND status='review'`,
			goalID)
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("reject review: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			_ = tx.Rollback()
			return nil, NewValidationError("goal is no longer in review")
		}
		// Continue on the current assignee (the unified owner-run spawn, P0.5).
		// attempt resets to 1: a reject iteration is a fresh human-directed
		// cycle, not a machine retry — the reject count lives in
		// goal.human_iterations (DESIGN.md §4). wake_anchor = the reject
		// comment id (决策 7-2): assemblePrompt's isReject branch reads it as
		// the wake anchor + fetches the reject reason from the comment.
		_, runEv, err := s.enqueueOwnerIntentTx(ctx, tx, goalID, g.AssigneeType, g.AssigneeID, "active", "", decisionCommentID, "active")
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("enqueue after reject: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		if runEv != nil {
			s.bus.Publish(ctx, *runEv)
		}
	}
	s.bus.Publish(ctx, events.Event{Topic: "goal:review_resolved", Payload: map[string]any{
		"goal_id": goalID, "decision": decision,
	}})
	return s.Get(ctx, goalID)
}

// IssueSource returns the goal's external source reference and the owning
// domain's git_credentials (M4-B: the issue identity + the GitHub token the
// platform uses to act on it). ok=false when the goal is not issue-sourced.
func (s *GoalService) IssueSource(ctx context.Context, goalID string) (ref, token string, ok bool, err error) {
	err = s.st.DB().QueryRowContext(ctx,
		`SELECT g.source_ref, d.git_credentials FROM goal g JOIN domain d ON d.id=g.domain_id WHERE g.id=?`, goalID).
		Scan(&ref, &token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return ref, token, ref != "", nil
}

// Activate moves a BACKLOG goal to active (决策 6-14): the missing
// backlog → active edge — a goal created without an assignee, or un-assigned
// and parked, had no path back into execution (Create and Reopen were the
// only entries to active). Conditional transition (backlog only); the
// assignee's run goes through the unified owner-run spawn (P0.5), so
// human-assigned goals activate without a run (manual placeholder), exactly
// like creation.
func (s *GoalService) Activate(ctx context.Context, goalID string) (*Goal, error) {
	// P0-2 (决策 6-15②): the backlog → active transition and its successor
	// run are born in ONE transaction — a crash between them used to leave a
	// run-less active goal that no startup reconcile can resurrect.
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE goal SET status='active' WHERE id=? AND status='backlog'`, goalID)
	if err != nil {
		return nil, fmt.Errorf("activate goal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		g, gerr := s.Get(ctx, goalID)
		if gerr != nil {
			return nil, gerr
		}
		return nil, NewCodedErrorDetail(CodeGoalNotActivatable, fmt.Sprintf("cannot activate: the goal is %s (only backlog goals can be activated)", g.Status), map[string]any{"status": g.Status})
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'activated','{}',?)`,
		newID(), goalID, "human", "", now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	var g Goal
	if err := tx.QueryRowContext(ctx,
		`SELECT id, assignee_type, assignee_id FROM goal WHERE id=?`, goalID).
		Scan(&g.ID, &g.AssigneeType, &g.AssigneeID); err != nil {
		return nil, fmt.Errorf("load goal for activate enqueue: %w", err)
	}
	_, runEv, err := s.enqueueOwnerIntentTx(ctx, tx, goalID, g.AssigneeType, g.AssigneeID, "active", "", "", "active")
	if err != nil {
		return nil, fmt.Errorf("enqueue after activate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if runEv != nil {
		s.bus.Publish(ctx, *runEv)
	}
	return s.Get(ctx, goalID)
}

// Reopen restarts a terminal goal — failed/cancelled (the failed-goal human
// take-over path, DESIGN.md §13) AND done (the comment-triggered reopen:
// a mention on a done goal is "this task is not over" — an追加需求, GitHub's
// reopen-and-comment): back to active with the reason as handoff_note, and a
// fresh run on the current assignee (attempt resets — a reopen is a new
// human-directed cycle, like a reject).
// CompleteFollowUp closes a follow-up round that produced NO changes
// (决策 4-1 修订): the human's comment was answered in the feed — nothing to
// merge, nothing new to approve — so the goal returns to done instead of
// re-parking in review. The DAEMON calls this only when its structural
// judgment holds (run has a HUMAN-authored trigger comment + zero changes);
// the transition itself is a plain conditional on status='active'.
func (s *GoalService) CompleteFollowUp(ctx context.Context, goalID string) (bool, error) {
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE goal SET status='done', handoff_note='', review_request='' WHERE id=? AND status='active'`,
		goalID)
	if err != nil {
		return false, fmt.Errorf("complete follow-up: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	_, _ = s.st.DB().ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'follow_up_completed','{}',?)`,
		newID(), goalID, "system", "", now())
	return true, nil
}

// Reopen returns a terminal goal to active with a fresh owner run. When the
// reopen came from a human COMMENT (决策 4-1), triggerCommentID names that
// comment — the spawn carries it as the run's trigger with an EXPLICIT
// owner role (the human-authored trigger would derive 'consult'). The
// trigger is the STRUCTURAL marker of a follow-up run: its presence (not
// any handoff-note string) lets the daemon distinguish "the human asked a
// follow-up" from first-time work.
func (s *GoalService) Reopen(ctx context.Context, goalID, reason, triggerCommentID string) (*Goal, error) {
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if g.Status != "failed" && g.Status != "cancelled" && g.Status != "done" {
		return nil, NewCodedError(CodeGoalNotReopenable, "only done, failed, or cancelled goals can be reopened")
	}
	note := "Reopened"
	if reason != "" {
		note += ": " + reason
	}
	// P0-2 (决策 6-15②): the terminal → active transition, the audit rows
	// and the successor run are born in ONE transaction — a crash between the
	// UPDATE and the enqueue used to leave a run-less active goal that no
	// startup reconcile can resurrect.
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE goal SET status='active', handoff_note=?, review_request='' WHERE id=? AND status IN ('done','failed','cancelled')`,
		note, goalID); err != nil {
		return nil, fmt.Errorf("reopen goal: %w", err)
	}
	// Clear the prior cycle's gate_decision rows. A done goal reached review,
	// was approved (gate_decision), and delivered; a failed/cancelled goal may
	// have parked in review with a recorded decision too. Those rows are
	// SUPERSEDED by the reopen — the next review is a fresh cycle with a fresh
	// evidence run, and leaving the old approve in place breaks three readers:
	//   - ResolveReview's duplicate-decision guard (the lastDecision lookup)
	//     would reject the new cycle's approve as "already approved" when the
	//     human re-clicks the still-visible old card (whose button carries the
	//     OLD evidence run_id, so lastRunID == runID holds).
	//   - maybeFireReviewReady's `decided > 0` gate would suppress the second
	//     cycle's review-window-ready notification, so the card never patches
	//     to the new run and the human is stuck on the stale card.
	//   - deliver's "latest gate_decision" lookup would read the stale approve.
	// Deleting here (same as the full-goal Delete cascade at ~L788) makes the
	// new cycle's gate_decision the only/latest row once the human decides.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM gate_decision WHERE goal_id=?`, goalID); err != nil {
		return nil, fmt.Errorf("clear gate_decision on reopen: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (id,goal_id,actor_type,actor_id,action,detail,created_at) VALUES (?,?,?,?,'reopened',?,?)`,
		newID(), goalID, "human", "", "{}", now()); err != nil {
		return nil, fmt.Errorf("insert activity: %w", err)
	}
	// A non-empty reason (the explicit Reopen button's words) lands in the
	// feed. The comment-triggered reopen passes NO reason — the human's
	// comment itself is already the reason (and the run's trigger); a
	// duplicate "重开：…" comment was feed noise.
	if reason != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,?,'',NULL,?,?)`,
			newID(), goalID, "human", "重开："+reason, now()); err != nil {
			return nil, fmt.Errorf("insert reopen comment: %w", err)
		}
	}
	// Fresh run on the current assignee (the unified owner-run spawn, P0.5;
	// human-assigned goals stay manual). A comment-triggered reopen stamps
	// the run with the comment as its trigger + an explicit owner role —
	// the follow-up marker the daemon reads structurally.
	var runEv *events.Event
	if agentID, isLeader, squadID, err := s.runSvc.resolveLeaderTx(ctx, tx, g.AssigneeType, g.AssigneeID); err != nil {
		return nil, fmt.Errorf("resolve assignee after reopen: %w", err)
	} else if agentID != "" {
		_, runEv, err = s.runSvc.enqueueTx(ctx, tx, goalID, agentID, 1, isLeader, squadID, triggerCommentID, "", "", "owner")
		if err != nil {
			return nil, fmt.Errorf("enqueue after reopen: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if runEv != nil {
		s.bus.Publish(ctx, *runEv)
	}
	trigger := "manual"
	if triggerCommentID != "" {
		trigger = "trigger comment " + triggerCommentID
	}
	logging.Infof("goal: %q reopened (%s)", g.Title, trigger)
	return s.Get(ctx, goalID)
}

// EnqueueOwnerRun is the SINGLE owner-run spawn entry (P0.5, 决策 6-4):
// every path that hands the goal's owner a fresh run — creation, handoff,
// reopen, reject, the Coordinator's attention spawn — goes through here.
// It is the conditional enqueue: active goals only, coalesced on the pending
// (goal, agent) pair — idempotent under event storms (running it twice still
// yields one run).
func (s *GoalService) EnqueueOwnerRun(ctx context.Context, goalID, wakeNote string) (*Run, error) {
	g, err := s.Get(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if g.Status != "active" {
		return nil, nil // review/terminal goals take no fresh owner run
	}
	// Human-assigned goals are manual placeholders — no run to spawn. The
	// check lives here (not in resolveLeader, which REJECTS non-agent/squad
	// assignees) because this entry's contract is "no-op for human" — the
	// same shape Reopen/Activate rely on.
	if g.AssigneeType == "human" {
		return nil, nil
	}
	agentID, isLeader, squadID, err := s.runSvc.resolveLeader(ctx, g.AssigneeType, g.AssigneeID)
	if err != nil {
		return nil, err
	}
	// wakeNote (决策 6-17) is the WHY this run was woken: "" = bare spawn
	// (the default branch renders the goal desc as the wake line); a non-
	// empty note rides the run row and the prompt's wakeNote branch renders
	// it. The "continue after a human stop" path passes a pause-resume note
	// so the owner picks up its worktree state rather than starting over.
	// The transactional enqueue is required for a non-empty note: enqueueTx's
	// coalesce path only stamps wake_note when it is non-empty.
	if wakeNote == "" {
		return s.runSvc.enqueue(ctx, goalID, agentID, 1, isLeader, squadID, "")
	}
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, ev, err := s.runSvc.EnqueueExistingTx(ctx, tx, goalID, agentID, 1, isLeader, squadID, wakeNote, "")
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

// enqueueOwnerIntentTx is the shared transactional owner-run spawn (P0-2,
// 决策 6-15②): it enqueues when the goal's status (read inside the CALLER's
// transaction — the caller owns the snapshot) is in the allowed set. The
// statuses name the semantics: the Coordinator passes active-only (attention
// spawns fire only for active goals); the direct workflow transitions
// (assign/activate/reopen/reject, create-active) pass what their transition
// allows — a handoff landing while the goal is frozen passes active+review,
// queuing a DURABLE INTENT that Claim admits when the human releases
// (决策 2-3 revised: review freezes execution, not intent).
// The run event is RETURNED — the caller publishes after its commit
// (invariant 13).
func (s *GoalService) enqueueOwnerIntentTx(ctx context.Context, tx *sql.Tx, goalID, assigneeType, assigneeID, status, wakeNote, wakeAnchor string, allowedStatuses ...string) (*Run, *events.Event, error) {
	if assigneeType != "agent" && assigneeType != "squad" {
		return nil, nil, nil // human-assigned: manual placeholder
	}
	if s.runSvc == nil {
		// Unwired service (bare test constructions) — the daemon always wires
		// it; skip so unwired tests keep their old caller-side enqueue shape.
		return nil, nil, nil
	}
	allowed := false
	for _, st := range allowedStatuses {
		if status == st {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, nil, nil // review/terminal/backlog take no fresh run (per the caller's set)
	}
	// The leader lookup runs ON the caller's tx — a pooled read here would
	// deadlock single-connection test stores while the tx holds the only
	// connection.
	agentID, isLeader, squadID, err := s.runSvc.resolveLeaderTx(ctx, tx, assigneeType, assigneeID)
	if err != nil {
		return nil, nil, err
	}
	if agentID == "" {
		return nil, nil, nil
	}
	return s.runSvc.EnqueueExistingTx(ctx, tx, goalID, agentID, 1, isLeader, squadID, wakeNote, wakeAnchor)
}

// EnqueueOwnerRunTx is the Coordinator's active-only spawn inside a caller's
// transaction (决策 6-4): attention spawns fire only for active goals.
// wakeNote (决策 6-17) is the reason compiled at spawn time — the prompt's
// "why you were woken" snapshot, immune to later attention re-derivations.
func (s *GoalService) EnqueueOwnerRunTx(ctx context.Context, tx *sql.Tx, goalID, assigneeType, assigneeID, status, wakeNote, wakeAnchor string) (*Run, *events.Event, error) {
	return s.enqueueOwnerIntentTx(ctx, tx, goalID, assigneeType, assigneeID, status, wakeNote, wakeAnchor, "active")
}

// ReconcileGoal is the Coordinator core (决策 6-4): events are only WAKEUP
// HINTS — the authoritative decision is recomputed from DB state inside ONE
// transaction, and every transition is conditional (running ReconcileGoal 100
// times yields the same end state). The attention derivation covers
// sub-goal/change state (决策 6-1/6-3 — the predicates grow with the sub-goal
// flow). Owner run creation goes through this path ONLY (P0.5).
//
// The body runs under withBusyRetry: a WAL snapshot-upgrade race fires
// SQLITE_BUSY immediately (busy_timeout does not apply), and because the
// transaction is idempotent the retry is sound (the live 16:13:07
// "persist attention: database is locked (5)" failure is exactly this).
func (s *GoalService) ReconcileGoal(ctx context.Context, goalID string) error {
	unlock := s.lockReconcile(goalID)
	defer unlock()
	return withBusyRetry(func() error { return s.reconcileGoalOnce(ctx, goalID) })
}

func (s *GoalService) reconcileGoalOnce(ctx context.Context, goalID string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pendingEvents []events.Event

	var assigneeType, assigneeID, status string
	err = tx.QueryRowContext(ctx,
		`SELECT assignee_type, assignee_id, status FROM goal WHERE id=?`, goalID).
		Scan(&assigneeType, &assigneeID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // goal vanished — nothing to reconcile
	}
	if err != nil {
		return fmt.Errorf("load goal for reconcile: %w", err)
	}

	// Stale-conflict cleanup (also runs here, not just on run-end): a
	// run.terminal or sub_goal.* event fires ReconcileGoal, and the goal may
	// have stale conflicts from a prior failed sub-goal that never got a
	// completed owner run to clear them. Clearing here lets the next owner
	// run (or the attention spawn below) see a clean pendingChanges count.
	if s.staleConf != nil && status == "active" {
		staleIDs, stErr := s.staleConf.StaleConflictChanges(ctx, goalID)
		if stErr != nil {
			logging.Infof("goal: %s stale-conflict resolve skipped: %v", goalID, stErr)
		} else if len(staleIDs) > 0 {
			for _, id := range staleIDs {
				if _, err := tx.ExecContext(ctx,
					`UPDATE change SET status='integrated' WHERE id=? AND status='conflict'`, id); err != nil {
					return fmt.Errorf("clear stale conflict: %w", err)
				}
			}
			logging.Infof("goal: %s cleared %d stale conflict change(s)", goalID, len(staleIDs))
		}
	}

	// Attention is armed only for ACTIVE goals: terminal goals have no owner
	// to wake (a done goal with a leftover ready change — the pre-guard era's
	// stale artifact — showed "待集成变更" forever), and review goals are
	// frozen (the owner cannot run while the human judges; the review_request
	// is the signal there).
	attention := ""
	if status == "active" {
		attention, err = s.deriveOwnerAttentionTx(ctx, tx, goalID)
		if err != nil {
			return err
		}
	}
	// The derived attention is persisted (决策 6-8): the UI badge and the IM
	// notification dedup read it; human owners get NO run — attention + notify.
	if _, err := tx.ExecContext(ctx,
		`UPDATE goal SET attention=? WHERE id=?`, attention, goalID); err != nil {
		return fmt.Errorf("persist attention: %w", err)
	}

	// Owner needed AND the goal can take a run AND the owner is idle → one
	// conditional enqueue (coalesced on the pending (goal, agent) pair — the
	// idempotency guard; two racing reconciles still produce ONE run).
	// Loop guards:
	//   - a CANCELLED owner run (timeout/handoff) leaves the goal active with
	//     attention still pending — auto-spawning would loop the stuck agent
	//     (决策 2-6: a timeout is the human's call);
	//   - PROGRESS guard: spawn only when an attention SIGNAL (a change
	//     revision or a failed sub-goal's run) is NEWER than the goal's last
	//     owner-run spawn — an owner woken for THIS set of changes that made
	//     no progress must not be re-spawned in a loop (the E2E spun 7 wakes).
	//     A new revision / failure re-arms the spawn.
	lastOwnerCancelled := false
	var newestSignal, lastOwnerSpawn string
	if attention != "" {
		var lastStatus string
		err := tx.QueryRowContext(ctx,
			`SELECT status FROM run WHERE goal_id=? AND role='owner' ORDER BY created_at DESC LIMIT 1`,
			goalID).Scan(&lastStatus)
		if err == nil && lastStatus == "cancelled" {
			lastOwnerCancelled = true
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT MAX(x.ts) FROM (
			  SELECT COALESCE((SELECT MAX(r2.created_at) FROM change_revision r2 WHERE r2.change_id = c.id), c.created_at) AS ts
			  FROM change c WHERE c.goal_id=? AND c.status='ready'
			  UNION ALL
			  SELECT MAX(COALESCE((SELECT MAX(r3.finished_at) FROM run r3 WHERE r3.sub_goal_id = sg.id AND r3.role='subgoal'), sg.created_at))
			  FROM sub_goal sg WHERE sg.goal_id=? AND sg.status='failed'
			  UNION ALL
			  SELECT MAX(COALESCE((SELECT MAX(r4.finished_at) FROM run r4 WHERE r4.sub_goal_id = sg2.id AND r4.role='subgoal'), ''))
			  FROM sub_goal sg2 WHERE sg2.goal_id=? AND sg2.status='verified'
			) x`, goalID, goalID, goalID).Scan(&newestSignal); err != nil {
			return fmt.Errorf("attention signal recency: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(queued_at),'') FROM run WHERE goal_id=? AND role='owner'`,
			goalID).Scan(&lastOwnerSpawn); err != nil {
			return fmt.Errorf("last owner spawn: %w", err)
		}
	}
	if attention != "" && !lastOwnerCancelled && newestSignal > lastOwnerSpawn {
		// The wake note (决策 6-17) is compiled HERE, in the same transaction
		// as the spawn — the snapshot of WHY this run was woken travels on
		// the run row. Reading goal.attention at claim time instead is a
		// race: the wrapup predicate self-invalidates once this spawn lands
		// (its queued_at resets the recency window), so the very events that
		// follow the spawn (sub_goal.verified, run.terminal) re-derive the
		// attention to '' and the woken owner loses its wake context.
		wakeNote, wakeAnchor, err := s.compileWakeNoteTx(ctx, tx, goalID, attention)
		if err != nil {
			return fmt.Errorf("compile wake note: %w", err)
		}
		// P0-2: the spawn is born IN this transaction — the run event is
		// published after the commit (invariant 13), so the frontend also
		// sees Coordinator spawns (previously the event was dropped here).
		_, runEv, err := s.EnqueueOwnerRunTx(ctx, tx, goalID, assigneeType, assigneeID, status, wakeNote, wakeAnchor)
		if err != nil {
			return fmt.Errorf("reconcile enqueue owner: %w", err)
		}
		if runEv != nil {
			pendingEvents = append(pendingEvents, *runEv)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, e := range pendingEvents {
		s.bus.Publish(ctx, e)
	}
	// Human owners get NO run — the attention event drives IM + the UI badge
	// (决策 6-8). Published after commit (invariant 13).
	if attention != "" && assigneeType == "human" {
		s.bus.Publish(ctx, events.Event{Topic: "goal.attention_needed", Payload: map[string]any{
			"goal_id": goalID, "attention": attention,
		}})
	}
	return nil
}

// ReconcileAllActive re-derives owner attention for EVERY active goal — the
// startup recovery face of Event≠Truth (决策 6-4, P0-3 决策 6-13): a crash
// can lose latch events (sub_goal.verified, run.terminal, ...) AFTER their
// transactions committed, and no replay resurrects them. The DB state is the
// truth: re-running the idempotent ReconcileGoal per goal re-arms attention
// and re-spawns whatever the state demands. Called once at daemon startup.
func (s *GoalService) ReconcileAllActive(ctx context.Context) (int, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id FROM goal WHERE status='active'`)
	if err != nil {
		return 0, fmt.Errorf("scan active goals: %w", err)
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
		if err := s.ReconcileGoal(ctx, id); err != nil {
			logging.Infof("service: reconcile all active (%s): %v", id, err)
			continue
		}
		n++
	}
	return n, nil
}

// enqueueSquadReviewTx enqueues the squad's reviewer runs INSIDE the park
// transaction (决策 6-13/6-19): the review run is the park's successor — by
// the time goal:reviewing publishes, the run already EXISTS, so the
// approval card's pending-reviewer hint (notify reads pending review runs)
// can never race an empty list (the live failure: the card fired at park
// time without the "审查中" hint because the event-handler enqueue lost the
// race against the notify handler). The trigger anchor is the parking run's
// report comment — the completion declaration itself (决策 6-19). A
// handoff_loop park has no code change to review — skipped (the human's
// call is the collaboration itself).
func (s *GoalService) enqueueSquadReviewTx(ctx context.Context, tx *sql.Tx, goalID, parkRunID, reviewReason string) error {
	if strings.HasPrefix(reviewReason, "handoff_loop:") {
		return nil
	}
	if s.runSvc == nil {
		return nil
	}
	var assigneeType, assigneeID string
	if err := tx.QueryRowContext(ctx,
		`SELECT assignee_type, assignee_id FROM goal WHERE id=?`, goalID).Scan(&assigneeType, &assigneeID); err != nil {
		return err
	}
	if assigneeType != "squad" || assigneeID == "" {
		return nil // not squad-owned — no squad rule applies
	}
	// The squad's leader (a reviewer who IS the leader would review its own
	// work — excluded, the review must come from a different member).
	var leaderID string
	_ = tx.QueryRowContext(ctx, `SELECT leader_id FROM squad WHERE id=?`, assigneeID).Scan(&leaderID)

	// The squad's reviewers (members declared with role=reviewer). Collected
	// BEFORE the enqueues: each enqueue writes rows, and a query cursor held
	// open across writes breaks the single-connection in-memory test stores.
	rows, err := tx.QueryContext(ctx,
		`SELECT m.member_id, a.name FROM squad_member m
		 JOIN agent a ON a.id = m.member_id
		 WHERE m.squad_id=? AND m.member_type='agent' AND LOWER(TRIM(m.role))='reviewer'
		 ORDER BY m.created_at`, assigneeID)
	if err != nil {
		return err
	}
	var reviewers []struct{ id, name string }
	for rows.Next() {
		var r struct{ id, name string }
		if err := rows.Scan(&r.id, &r.name); err != nil {
			rows.Close()
			return err
		}
		reviewers = append(reviewers, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// The trigger anchor (决策 6-19): the parking run's completion comment —
	// the agent's own `goal comment` posted during the run (决策 4-4 revised:
	// the platform no longer posts insertRunResultComment; the agent comments
	// itself). Falls back to the goal's latest run-associated comment when
	// the parking run's agent left none.
	trigger := ""
	if parkRunID != "" {
		_ = tx.QueryRowContext(ctx,
			`SELECT id FROM comment WHERE goal_id=? AND run_id=? ORDER BY created_at DESC LIMIT 1`,
			goalID, parkRunID).Scan(&trigger)
	}
	if trigger == "" {
		_ = tx.QueryRowContext(ctx,
			`SELECT c.id FROM comment c JOIN run r ON r.id = c.run_id
			 WHERE c.goal_id=? ORDER BY r.finished_at DESC, c.created_at DESC LIMIT 1`,
			goalID).Scan(&trigger)
	}

	enqueued := 0
	for _, r := range reviewers {
		if r.id == leaderID {
			continue
		}
		// The reviewer already has a pending REVIEW request on this goal — a
		// second enqueue would duplicate the ask (coalescing on the run row
		// handles queued ones; the explicit check keeps the semantics local).
		var pending int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND role='review' AND status IN ('queued','running')`,
			goalID, r.id).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			continue
		}
		if _, _, err := s.runSvc.EnqueueForMentionRoleTx(ctx, tx, goalID, r.id, trigger, "review"); err != nil {
			return err
		}
		enqueued++
	}
	if enqueued > 0 {
		// The review wait's START — the card's "审查中" state and the
		// window-close both hang on these runs.
		logging.Infof("review: enqueued %d review run(s) for goal %s", enqueued, goalID)
	}
	return nil
}

// compileWakeNoteTx renders the derived attention bits into the owner's wake
// note (决策 6-17) — one bullet per bit, naming the concrete sub-goals and
// change counts. Platform text is English (决策 6-18); the sub-goal TITLES
// carry the materials' own language. The note travels on the
// run row as the prompt's "why you were woken" snapshot, together with the
// wake ANCHOR (决策 6-22): the latest sub-goal report comment — the
// get_comments(after=) handle for the woken owner. All reads run on the
// caller's transaction.
func (s *GoalService) compileWakeNoteTx(ctx context.Context, tx *sql.Tx, goalID, attention string) (string, string, error) {
	if attention == "" {
		return "", "", nil
	}
	// Platform text is English (决策 6-18) — the note names the MATERIALS
	// (sub-goal titles) verbatim; their language is the leader's own.
	var bullets []string
	for _, bit := range strings.Split(attention, ",") {
		switch bit {
		case "recovery":
			titles, err := s.subGoalTitlesTx(ctx, tx, goalID, "failed")
			if err != nil {
				return "", "", err
			}
			if len(titles) == 0 {
				continue // raced to rework — nothing to name
			}
			bullets = append(bullets, fmt.Sprintf("- Sub-goal(s) %q failed — decide: cancel or re-create (inspect with `agentwork subgoal get <id>`, cancel with `agentwork subgoal cancel <id>`)", strings.Join(titles, ", ")))
		case "integration":
			// ready changes to merge (决策 6-8). The no-code wrapup face is its
			// own bit now (see case "wrapup") — the flag layer was split to match
			// the wake-note layer's already-distinct semantics (决策 6-17).
			var ready int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM change WHERE goal_id=? AND status='ready'`, goalID).Scan(&ready); err != nil {
				return "", "", err
			}
			if ready > 0 {
				bullets = append(bullets, fmt.Sprintf("- %d change(s) ready to integrate — inspect with `agentwork change list`, merge each with `agentwork change integrate <id>`", ready))
			}
		case "wrapup":
			// no-code wrapups: verified sub-goals whose deliverable lives in
			// the feed / scratch dir — 决策 6-8.
			titles, err := s.noChangeVerifiedTitlesTx(ctx, tx, goalID)
			if err != nil {
				return "", "", err
			}
			if len(titles) > 0 {
				bullets = append(bullets, fmt.Sprintf("- Sub-goal(s) %q verified with no changes — the deliverable lives in the feed (its report); review the conclusion and wrap up", strings.Join(titles, ", ")))
			}
		}
	}
	// The wake anchor (决策 6-22): the latest agent comment on a sub-goal run
	// — the agent's own `goal comment` (its result/answer, 决策 4-4 revised).
	// If the sub-goal's agent did not comment, there is no anchor (the owner
	// relies on the wake note bullets + pulling the feed). Recovery-only wakes
	// (failed runs produce no agent comment) get no anchor.
	var anchor string
	_ = tx.QueryRowContext(ctx,
		`SELECT c.id FROM comment c JOIN run r ON r.id = c.run_id
		 WHERE r.goal_id=? AND r.role='subgoal'
		 ORDER BY c.created_at DESC LIMIT 1`, goalID).Scan(&anchor)
	return strings.Join(bullets, "\n"), anchor, nil
}

// subGoalTitlesTx loads the titles of the goal's sub-goals in the given
// status — the wake note names work items by title (humans and agents read
// titles; ids are system handles).
func (s *GoalService) subGoalTitlesTx(ctx context.Context, tx *sql.Tx, goalID, status string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT title FROM sub_goal WHERE goal_id=? AND status=? ORDER BY created_at`, goalID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var titles []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		titles = append(titles, t)
	}
	return titles, rows.Err()
}

// noChangeVerifiedTitlesTx lists VERIFIED sub-goals that produced NO Change —
// the wrapup bullet's exact predicate (决策 6-8). A verified sub-goal WITH a
// ready Change is the integration bullet's subject, not "no changes" — mixing
// them described the same work item as both (live wording bug).
// noChangeVerifiedTitlesTx returns the verified sub-goals that drive the
// `wrapup` wake bullet — the same set deriveOwnerAttentionTx counts for the
// `wrapup` bit (决策 6-17): a verified sub-goal whose latest verification
// landed after the owner's last spawn with no READY change behind it. "No
// READY change", not "no change row at all": an already-integrated change is
// the owner's done work (the rework-against-integrated case still wraps up).
// Kept as a title list so the wake note can name the concrete sub-goals.
func (s *GoalService) noChangeVerifiedTitlesTx(ctx context.Context, tx *sql.Tx, goalID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT sg.title FROM sub_goal sg
		 WHERE sg.goal_id=? AND sg.status='verified'
		   AND NOT EXISTS (SELECT 1 FROM change c WHERE c.sub_goal_id=sg.id AND c.status='ready')
		   AND COALESCE((SELECT MAX(r.finished_at) FROM run r WHERE r.sub_goal_id=sg.id AND r.role='subgoal'),'')
		     > COALESCE((SELECT MAX(r2.queued_at) FROM run r2 WHERE r2.goal_id=sg.goal_id AND r2.role='owner'),'')
		 ORDER BY sg.created_at`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var titles []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		titles = append(titles, t)
	}
	return titles, rows.Err()
}

// deriveOwnerAttentionTx computes the v2 OwnerAttention bits (决策 6-4/6-8)
// from authoritative DB state. Empty = nothing needs the owner.
func (s *GoalService) deriveOwnerAttentionTx(ctx context.Context, tx *sql.Tx, goalID string) (string, error) {
	var bits []string
	// need_recovery: a sub-goal failed — the owner decides (cancel/retry/reassign).
	var failed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sub_goal WHERE goal_id=? AND status='failed'`, goalID).Scan(&failed); err != nil {
		return "", fmt.Errorf("attention: sub-goal failed: %w", err)
	}
	if failed > 0 {
		bits = append(bits, "recovery")
	}
	// need_integration: changes READY for the owner to merge. A conflicted
	// change is the ASSIGNEE's rework, not the owner's work (P1-3) — the
	// owner has nothing actionable during the conflict window and is woken
	// only when the new revision returns the change to ready.
	var ready int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change WHERE goal_id=? AND status='ready'`, goalID).Scan(&ready); err != nil {
		return "", fmt.Errorf("attention: changes ready: %w", err)
	}
	if ready > 0 {
		bits = append(bits, "integration")
	}
	// need_wrapup: a verification round completed AFTER the owner's last
	// spawn and left no READY change behind — the no-code completion case
	// (决策 6-8: the deliverable lives in the feed / scratch dir), INCLUDING
	// the rework-against-already-integrated case (a conflict round verified
	// again with base==head → no NEW ready change, the old change stays
	// integrated). The guard is "no READY change", not "no change row at
	// all": an integrated change is already the owner's done work, not a
	// pending need, so it must not block wrapup. This is the exact complement
	// of `integration` (which arms on a READY change) — the two bits are
	// mutually exclusive per sub-goal, so a ready-change goal lights only
	// "待集成变更" and a no-ready-change verified goal lights only "待收尾确认"
	// (决策 6-17: aligns the flag with the wake-note layer's distinct face).
	// Without this edge the owner is never woken to close the goal out (live:
	// a goal parked active forever after its rework round verified against an
	// already-integrated change). The spawn guard uses the same signal, so the
	// owner waking and finishing clears it — no loop.
	var wrapup int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sub_goal sg
		WHERE sg.goal_id=? AND sg.status='verified'
		  AND NOT EXISTS (SELECT 1 FROM change c WHERE c.sub_goal_id=sg.id AND c.status='ready')
		  AND COALESCE((SELECT MAX(r.finished_at) FROM run r WHERE r.sub_goal_id=sg.id AND r.role='subgoal'),'')
		    > COALESCE((SELECT MAX(r2.queued_at) FROM run r2 WHERE r2.goal_id=sg.goal_id AND r2.role='owner'),'')`,
		goalID).Scan(&wrapup); err != nil {
		return "", fmt.Errorf("attention: verified wrap-up: %w", err)
	}
	if wrapup > 0 {
		bits = append(bits, "wrapup")
	}
	return strings.Join(bits, ","), nil
}

// resolveGateRule names the rule that actually parked the goal in review:
//   - the named run's gates_hit[0] (the IM card carries the evidence run),
//   - else the goal's latest completed run's gates_hit[0] (the Web panel),
//   - else "merge" (the strength-linkage default gate fires with empty
//     gates_hit).
func (s *GoalService) resolveGateRule(ctx context.Context, goalID, runID string) (string, error) {
	if runID == "" {
		// The Web panel resolves without naming a run — use the latest
		// completed run, whose gates_hit the daemon recorded.
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT id FROM run WHERE goal_id=? AND status='completed'
			 ORDER BY finished_at DESC LIMIT 1`, goalID).Scan(&runID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("find evidence run: %w", err)
		}
	}
	if runID != "" {
		var hitJSON string
		err := s.st.DB().QueryRowContext(ctx,
			`SELECT gates_hit FROM run WHERE id=?`, runID).Scan(&hitJSON)
		if err == nil && hitJSON != "" && hitJSON != "[]" {
			var hit []string
			if json.Unmarshal([]byte(hitJSON), &hit) == nil && len(hit) > 0 {
				if name, _, ok := strings.Cut(hit[0], ":"); ok && strings.TrimSpace(name) != "" {
					return strings.TrimSpace(name), nil
				}
				return hit[0], nil
			}
		}
	}
	// No gates_hit recorded — the strength-linkage default gate (weak domain)
	// or the unfrozen-policy checkpoint fired with empty gates_hit.
	return "merge", nil
}

// GateStat is one gate rule's decision history — the health-learning data
// source (DESIGN.md §13): a gate approved every time is a candidate for
// removal; one rejected repeatedly is a candidate for tightening.
type GateStat struct {
	Rule     string `json:"rule"` // gate_rule as recorded (merge, diff_contains, ...)
	Total    int    `json:"total"`
	Approved int    `json:"approved"`
	Rejected int    `json:"rejected"`
}

// GateStats aggregates gate_decision by rule. Sorted by total descending so
// the busiest gates surface first.
func (s *GoalService) GateStats(ctx context.Context) ([]GateStat, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT gate_rule,
		        COUNT(*) AS total,
		        SUM(CASE WHEN decision='approve' THEN 1 ELSE 0 END) AS approved,
		        SUM(CASE WHEN decision IN ('reject','redirect') THEN 1 ELSE 0 END) AS rejected
		 FROM gate_decision
		 GROUP BY gate_rule
		 ORDER BY total DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GateStat{}
	for rows.Next() {
		var g GateStat
		if err := rows.Scan(&g.Rule, &g.Total, &g.Approved, &g.Rejected); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MarkDelivered closes the deliver step (DESIGN.md §7), called by the
// daemon after its deterministic merge + re-verify + push:
//
//	success → review → done (handoff_note cleared)
//	failure → stays in review with the reason annotated (review_request),
//	          so the human can retry deliver or reject the change back.
//
// fixCommits ("<full sha> <title>") is the fix evidence the daemon collected;
// the goal layer passes it through verbatim to the delivered event — the
// issue closer links it. The goal layer never parses it (not its domain).
func (s *GoalService) MarkDelivered(ctx context.Context, goalID string, success bool, note string, fixCommits []string) (*Goal, error) {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var statusCheck string
	err = tx.QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, goalID).Scan(&statusCheck)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load goal: %w", err)
	}
	_ = statusCheck

	event := "goal:deliver_failed"
	if success {
		// A delivered goal is done: the deliver already merged this goal's
		// branch. Pending sub-goals cannot exist here — the finalization guard
		// (决策 6-8) keeps the goal out of the gate judgment while work is
		// pending.
		if _, err := tx.ExecContext(ctx,
			`UPDATE goal SET status='done', handoff_note='', review_request='' WHERE id=? AND status='review'`, goalID); err != nil {
			return nil, fmt.Errorf("mark delivered: %w", err)
		}
		// Terminal state — drop queued runs (same rule as the reconcile paths).
		if _, err := tx.ExecContext(ctx,
			`UPDATE run SET status='cancelled', cancel_reason='goal_terminal' WHERE goal_id=? AND status='queued'`, goalID); err != nil {
			return nil, fmt.Errorf("cancel queued runs on delivered: %w", err)
		}
		event = "goal:delivered"
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE goal SET review_request=? WHERE id=? AND status='review'`, note, goalID); err != nil {
			return nil, fmt.Errorf("annotate deliver failure: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.bus.Publish(ctx, events.Event{Topic: event, Payload: map[string]any{
		"goal_id": goalID, "note": note, "commits": fixCommits,
	}})
	return s.Get(ctx, goalID)
}

// commitAndEmit commits the transaction and, only on success, publishes the
// collected events and runs the after-commit callbacks. This keeps the
// "bus.Publish after commit" invariant: a rolled-back transaction (commit
// error) emits nothing.
//
// runID (non-empty) stamps the run's reconciled_at INSIDE the same
// transaction (P0-1, 决策 6-11): the run's terminal outcome and its
// reconcile become one atomic unit — a crash before this commit leaves
// reconciled_at=” and the startup replay re-runs the reconcile; the report
// comment and the stamp live or die together, so a replay never duplicates
// them.
func (s *GoalService) commitAndEmit(ctx context.Context, tx *sql.Tx, evs []events.Event, after []func(), runID string) error {
	if runID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE run SET reconciled_at=? WHERE id=?`, now(), runID); err != nil {
			return fmt.Errorf("stamp reconciled_at: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, e := range evs {
		s.bus.Publish(ctx, e)
	}
	for _, f := range after {
		f()
	}
	return nil
}

// assigneeLabel resolves an assignee's display name for the creation comment
// (agent name / squad name).
func (s *GoalService) assigneeLabel(ctx context.Context, tx *sql.Tx, atype, aid string) (string, error) {
	var name string
	var err error
	switch atype {
	case "agent":
		err = tx.QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, aid).Scan(&name)
	case "squad":
		err = tx.QueryRowContext(ctx, `SELECT name FROM squad WHERE id=?`, aid).Scan(&name)
	default:
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return name, nil
}

// IsSQLiteUniqueViolation reports whether err is a UNIQUE-constraint
// violation — the idempotency signal for guarded inserts (schedule_run
// uq(schedule_id, planned_at): a concurrent tick's duplicate firing is a
// no-op, NOT a failure to swallow).
func IsSQLiteUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT
	}
	return false
}

// isSQLiteBusy reports whether err is a SQLITE_BUSY / SQLITE_BUSY_SNAPSHOT
// failure. WAL snapshot-upgrade races fail IMMEDIATELY with SQLITE_BUSY —
// the busy_timeout pragma does not apply to them (the tx took a read
// snapshot, another writer committed, and the write upgrade collides). The
// live failure: ReconcileGoal's attention persist hit (5) SQLITE_BUSY at
// exactly the moment the review-run enqueue committed alongside it.
func isSQLiteBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqlite3.SQLITE_BUSY || se.Code() == sqlite3.SQLITE_BUSY_SNAPSHOT
	}
	return false
}

// withBusyRetry re-runs fn when it fails with SQLITE_BUSY. Safe ONLY for
// callers whose transaction is a conditional transition (rolled back on
// failure, so the retry re-runs from a fresh snapshot) — the reconcile
// family is idempotent by design (决策 6-4), which is exactly what makes the
// retry sound.
func withBusyRetry(fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = fn()
		if err == nil || !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// mustExist is a tiny existence-check helper shared by the services.
func mustExist(ctx context.Context, st *store.Store, query, id, label string) error {
	var n int
	if err := st.DB().QueryRowContext(ctx, query, id).Scan(&n); err != nil {
		return fmt.Errorf("check %s: %w", label, err)
	}
	if n == 0 {
		return NewValidationError(fmt.Sprintf("%s %q does not exist", label, id))
	}
	return nil
}
