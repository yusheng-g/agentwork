package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// ── Collaboration semantics tests (决策 5-x, Collaboration.md adopted) ──

// TestAssignPermissionOnlyOwner: an agent actor can only hand off a goal it
// owns (决策 5-6) — agent-assigned → the assignee; anyone else is rejected;
// human actors (HTTP surface) stay unrestricted.
func TestAssignPermissionOnlyOwner(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	owner := seedAgent(t, st, "owner")
	intruder := seedAgent(t, st, "intruder")
	target := seedAgent(t, st, "target")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: owner, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}

	// A non-owner agent actor is rejected and ownership does not move.
	if _, err := gs.Assign(ctx, g.ID, "agent", target, "mine now", "agent", intruder); err == nil {
		t.Fatal("non-owner agent handoff must be rejected")
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.AssigneeID != owner {
		t.Fatalf("ownership moved by an intruder: %s", after.AssigneeID)
	}

	// The owner agent actor succeeds.
	if _, err := gs.Assign(ctx, g.ID, "agent", target, "backend work", "agent", owner); err != nil {
		t.Fatalf("owner handoff must succeed: %v", err)
	}
	// Human actor (HTTP surface) unrestricted — hands it back.
	if _, err := gs.Assign(ctx, g.ID, "agent", owner, "", "", ""); err != nil {
		t.Fatalf("human handoff must succeed: %v", err)
	}

	// Squad goals: only the squad's CURRENT leader may hand off.
	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: owner})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	sg, err := gs.Create(ctx, Goal{Title: "sg", Description: "d", DomainID: domID, AssigneeType: "squad", AssigneeID: sq.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create squad goal: %v", err)
	}
	if _, err := gs.Assign(ctx, sg.ID, "agent", target, "", "agent", intruder); err == nil {
		t.Fatal("non-leader handoff of a squad goal must be rejected")
	}
	if _, err := gs.Assign(ctx, sg.ID, "agent", target, "", "agent", owner); err != nil {
		t.Fatalf("leader handoff of a squad goal must succeed: %v", err)
	}
}

// TestHandoffEventRecorded: every handoff appends a handoff_event with
// from/to + the terminated run; CompleteHandoff back-fills the new owner's
// run (决策 5-9).
func TestHandoffEventRecorded(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// The owner's run is enqueued and claimed (running) — it is the handoff's
	// from_run.
	ownerRun, err := rs.EnqueueForGoal(ctx, *g)
	if err != nil {
		t.Fatalf("enqueue owner: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=? WHERE id=?`, now(), ownerRun.ID); err != nil {
		t.Fatalf("stamp running: %v", err)
	}

	if _, err := gs.Assign(ctx, g.ID, "agent", b, "take it", "human", ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	var fromID, toID, fromRunID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT from_id, to_id, from_run_id FROM handoff_event WHERE goal_id=?`, g.ID).Scan(&fromID, &toID, &fromRunID); err != nil {
		t.Fatalf("handoff_event row: %v", err)
	}
	if fromID != a || toID != b || fromRunID != ownerRun.ID {
		t.Fatalf("handoff_event mismatch: from=%s to=%s from_run=%s (want %s→%s run %s)", fromID, toID, fromRunID, a, b, ownerRun.ID)
	}

	// P0-2 (决策 6-15②): Assign enqueues the successor and back-fills
	// to_run_id IN its transaction — no caller-side EnqueueForGoal /
	// CompleteHandoff exists anymore; the audit row is complete now.
	var toRunID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT to_run_id FROM handoff_event WHERE goal_id=?`, g.ID).Scan(&toRunID); err != nil || toRunID == "" {
		t.Fatalf("to_run_id must be back-filled by Assign in-tx: %q err=%v", toRunID, err)
	}
	var pending int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND role='owner' AND status='queued'`,
		g.ID, b).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("the new owner's run must be queued by Assign, got %d (err=%v)", pending, err)
	}
}

// TestHandoffEventRecordsAgentActor is the regression for the goal-assign-via-
// HTTP identity loss. When an agent hands off over /rpc, the run token resolves
// to (actorType="agent", actorID=<owner agent>); the HTTP surface passed ("","")
// and the actor defaulted to "human" — so the audit row's actor_type/actor_id
// was always "human"/"" even for an agent-initiated handoff, breaking the
// audit chain. This test pins that an agent actor is recorded faithfully.
func TestHandoffEventRecordsAgentActor(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	ownerRun, err := rs.EnqueueForGoal(ctx, *g)
	if err != nil {
		t.Fatalf("enqueue owner: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running', started_at=? WHERE id=?`, now(), ownerRun.ID); err != nil {
		t.Fatalf("stamp running: %v", err)
	}

	// The agent-initiated handoff: actorType="agent", actorID=a (the owner).
	// Over HTTP this arrived as ("","") → "human"/"" and the owner check was
	// skipped; over /rpc the token resolves to the real agent actor.
	if _, err := gs.Assign(ctx, g.ID, "agent", b, "I'll hand this to b", "agent", a); err != nil {
		t.Fatalf("agent handoff must succeed: %v", err)
	}
	var actorType, actorID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT actor_type, actor_id FROM handoff_event WHERE goal_id=?`, g.ID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("handoff_event row: %v", err)
	}
	if actorType != "agent" || actorID != a {
		t.Fatalf("agent handoff must record actor_type=agent actor_id=%s, got %q/%q — the audit chain breaks when the actor is lost (the HTTP-surface bug)", a, actorType, actorID)
	}
}

// TestHandoffCycleWarnAndPark: ≥4 handoffs write a system warning comment;
// ≥8 park the goal in review — approve releases it (NO deliver), reject
// fails it (决策 5-7: a collaboration anomaly is not an automatic failure).
func TestHandoffCycleWarnAndPark(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	handoff := func(to string) {
		t.Helper()
		if _, err := gs.Assign(ctx, g.ID, "agent", to, "", "", ""); err != nil {
			t.Fatalf("assign → %s: %v", to, err)
		}
	}
	// 1..3: silent. 4th: warning comment.
	handoff(b) // 1
	handoff(a) // 2
	handoff(b) // 3
	handoff(a) // 4
	var warns int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND author_type='system' AND content LIKE '⚠️ 所有权已交接%'`, g.ID).Scan(&warns); err != nil || warns != 1 {
		t.Fatalf("expected 1 warning comment at 4 handoffs, got %d (err=%v)", warns, err)
	}
	// 5..8: the 8th parks the goal in review.
	handoff(b) // 5
	handoff(a) // 6
	handoff(b) // 7
	handoff(a) // 8
	parked, _ := gs.Get(ctx, g.ID)
	if parked.Status != "review" || !strings.HasPrefix(parked.ReviewRequest, "handoff_loop:") {
		t.Fatalf("8th handoff must park review, got %q / %q", parked.Status, parked.ReviewRequest)
	}
	// Approve releases WITHOUT deliver: goal back to active, no goal:approved.
	var approved bool
	unsub := eventsBusTest(gs, "goal:approved", func() { approved = true })
	defer unsub()
	if _, err := gs.ResolveReview(ctx, g.ID, "", "approve", "继续", "human"); err != nil {
		t.Fatalf("approve handoff loop: %v", err)
	}
	if approved {
		t.Fatal("handoff-loop approve must NOT trigger deliver (goal:approved)")
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("approve must release to active, got %q", after.Status)
	}
	// 9th handoff parks again (≥8). Reject fails the goal.
	handoff(b) // 9 — parks again
	if _, err := gs.ResolveReview(ctx, g.ID, "", "reject", "停", "human"); err != nil {
		t.Fatalf("reject handoff loop: %v", err)
	}
	failed, _ := gs.Get(ctx, g.ID)
	if failed.Status != "failed" {
		t.Fatalf("reject must fail the goal, got %q", failed.Status)
	}
	// A failed goal takes no queued runs.
	runs, _ := rs.List(ctx, g.ID)
	for _, r := range runs {
		if r.Status == "queued" {
			t.Fatalf("queued run survived the handoff-loop failure: %s", r.ID)
		}
	}
}

// TestCreateSubGoal: the SubGoal entity (决策 6-1) — a sub_goal row bound to
// the goal (NOT a child goal), started running with a sub-goal run enqueued
// on the assignee; the goal's owner/status are untouched.
func TestCreateSubGoal(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	parent, err := gs.Create(ctx, Goal{Title: "p", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, parent.ID, "work item", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	if sg.GoalID != parent.ID || sg.Status != "running" || sg.AssigneeID != b {
		t.Fatalf("sub-goal shape wrong: goal=%s status=%s assignee=%s", sg.GoalID, sg.Status, sg.AssigneeID)
	}
	// The sub-goal's run is role=subgoal + bound via sub_goal_id, on B.
	var runGoalID, role, boundID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT goal_id, role, sub_goal_id FROM run WHERE id=(
		   SELECT id FROM run WHERE sub_goal_id=? LIMIT 1)`, sg.ID).
		Scan(&runGoalID, &role, &boundID); err != nil {
		t.Fatalf("sub-goal run row: %v", err)
	}
	if runGoalID != parent.ID || role != "subgoal" || boundID != sg.ID {
		t.Fatalf("sub-goal run shape wrong: goal=%s role=%s bound=%s", runGoalID, role, boundID)
	}
	// The goal's run list includes the owner's born run AND the sub-goal run
	// (run.goal_id = the goal) — distinguishable by role=subgoal (P0-2: the
	// owner run is born with Create now).
	runs, _ := rs.List(ctx, parent.ID)
	var found bool
	for _, r := range runs {
		if r.Role == "subgoal" && r.AgentID == b {
			found = true
		}
	}
	if !found {
		t.Fatalf("the goal's runs must include the sub-goal run (role=subgoal, agent=%s), got %d runs", b, len(runs))
	}
	// Parent ownership untouched; the goal stays active (owner keeps working —
	// no wait, no blocked).
	p2, _ := gs.Get(ctx, parent.ID)
	if p2.AssigneeID != a || p2.Status != "active" {
		t.Fatalf("sub-goal creation must not change the parent: owner=%s status=%s", p2.AssigneeID, p2.Status)
	}
}

// TestSubGoalRunLifecycle: a completed sub-goal run (machine verification
// passed in the daemon) → sub-goal verified + Change + Revision created
// ATOMICALLY (Ready ⇔ persisted Revision) → the goal's attention flips to
// integration → the Coordinator spawns the owner (决策 6-1/6-3/6-4).
func TestSubGoalRunLifecycle(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	var runID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? LIMIT 1`, sg.ID).Scan(&runID); err != nil {
		t.Fatalf("sub-goal run: %v", err)
	}
	// The daemon stamps the revision refs at run end; simulate it.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='base-sha', head_ref='head-sha' WHERE id=?`, runID); err != nil {
		t.Fatalf("stamp refs: %v", err)
	}
	if err := rs.Finish(ctx, runID, "completed", "done the work item"); err != nil {
		t.Fatalf("finish sub-goal run: %v", err)
	}
	// Verified + Change + Revision (atomic — one tx in ReconcileSubGoalRun).
	after, err := gs.GetSubGoal(ctx, sg.ID)
	if err != nil || after.Status != "verified" {
		t.Fatalf("sub-goal must be verified, got %q err=%v", after.Status, err)
	}
	var changeID, revBase, revHead string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM change WHERE sub_goal_id=?`, sg.ID).Scan(&changeID); err != nil {
		t.Fatalf("change row: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT base_ref, head_ref FROM change_revision WHERE change_id=?`, changeID).Scan(&revBase, &revHead); err != nil {
		t.Fatalf("revision row: %v", err)
	}
	if revBase != "base-sha" || revHead != "head-sha" {
		t.Fatalf("revision refs wrong: base=%q head=%q", revBase, revHead)
	}
	// The sub_goal.verified event → daemon's ReconcileGoal (simulate the
	// subscription by calling directly): attention=integration + owner spawned.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var attention string
	if err := st.DB().QueryRowContext(ctx, `SELECT attention FROM goal WHERE id=?`, g.ID).Scan(&attention); err != nil || attention != "integration" {
		t.Fatalf("attention must be integration, got %q err=%v", attention, err)
	}
	runs, _ := rs.List(ctx, g.ID)
	var ownerRuns int
	for _, r := range runs {
		if r.Role == "owner" {
			ownerRuns++
		}
	}
	if ownerRuns != 1 {
		t.Fatalf("owner must be spawned for integration, got %d owner runs", ownerRuns)
	}
}

// TestSubGoalRetryThenFail: machine failures (failed/cancelled) drive the
// execution_attempt chain ≤3 → sub-goal failed → recovery attention (决策
// 6-5/6-9 — the two-layer failure semantics).
func TestSubGoalRetryThenFail(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// Fail the run 3 times — each failure bumps execution_attempt and
	// enqueues a retry (coalescing keeps one pending at a time).
	for i := 0; i < 3; i++ {
		var runID string
		if err := st.DB().QueryRowContext(ctx,
			`SELECT id FROM run WHERE sub_goal_id=? AND status='queued' LIMIT 1`, sg.ID).Scan(&runID); err != nil {
			t.Fatalf("pending sub-goal run %d: %v", i, err)
		}
		if err := rs.Finish(ctx, runID, "failed", "verification failed"); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
	}
	after, err := gs.GetSubGoal(ctx, sg.ID)
	if err != nil {
		t.Fatalf("get sub-goal: %v", err)
	}
	if after.Status != "failed" || after.ExecutionAttempt != 3 {
		t.Fatalf("exhausted sub-goal must fail with 3 attempts, got status=%q attempts=%d", after.Status, after.ExecutionAttempt)
	}
	// Recovery attention.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var attention string
	if err := st.DB().QueryRowContext(ctx, `SELECT attention FROM goal WHERE id=?`, g.ID).Scan(&attention); err != nil || attention != "recovery" {
		t.Fatalf("attention must be recovery, got %q err=%v", attention, err)
	}
}

// TestConsultAutoResume: an owner's consult of another agent creates a guest
// run; when the guest completes, the platform auto-resumes the requester
// (attempt 1, trigger empty — must not stack the mention-cycle counter). The
// guest's answer is its own `goal comment` (carrying run_id=guest_run_id);
// consultStatus joins on comment.run_id — response_comment_id is no longer
// back-filled (决策 4-4 revised).
func TestConsultAutoResume(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	owner := seedAgent(t, st, "owner")
	expert := seedAgent(t, st, "expert")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: owner, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// The owner's comment (from its run) consults the expert.
	c, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: owner, RunID: "owner-run-1", Content: "[@e](mention://agent/" + expert + ") how?"})
	if err != nil {
		t.Fatalf("consult comment: %v", err)
	}
	var guestID, requesterRun, response string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT guest_run_id, requester_run_id, response_comment_id FROM consult_request WHERE trigger_comment_id=?`, c.ID).
		Scan(&guestID, &requesterRun, &response); err != nil {
		t.Fatalf("consult_request row: %v", err)
	}
	if guestID == "" || requesterRun != "owner-run-1" || response != "" {
		t.Fatalf("consult_request shape: guest=%q requester_run=%q response=%q", guestID, requesterRun, response)
	}
	var guestRole string
	if err := st.DB().QueryRowContext(ctx, `SELECT role FROM run WHERE id=?`, guestID).Scan(&guestRole); err != nil || guestRole != "consult" {
		t.Fatalf("consult run must be role=consult, got %q err=%v", guestRole, err)
	}

	// The guest completes → requester auto-resumed (coalesced onto the
	// pending owner run, no new run created) and the resume run carries NO
	// trigger comment. response_comment_id stays empty (决策 4-4 revised).
	// Capture the run count BEFORE finish so the coalesce assertion is exact.
	var runsBefore int
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE goal_id=?`, g.ID).Scan(&runsBefore)
	if err := rs.Finish(ctx, guestID, "completed", "the answer is X"); err != nil {
		t.Fatalf("finish guest: %v", err)
	}
	var runsAfter int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE goal_id=?`, g.ID).Scan(&runsAfter); err != nil {
		t.Fatalf("run count after finish: %v", err)
	}
	if runsAfter != runsBefore {
		t.Fatalf("consult closure must coalesce, not create a new run: runs %d → %d", runsBefore, runsAfter)
	}
	var resumeTrigger, resumeAttempt, resumeAgent string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT trigger_comment_id, attempt, agent_id FROM run WHERE goal_id=? AND agent_id=? AND id != ? ORDER BY queued_at DESC LIMIT 1`,
		g.ID, owner, guestID).Scan(&resumeTrigger, &resumeAttempt, &resumeAgent); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if resumeTrigger != "" || resumeAgent != owner {
		t.Fatalf("resume run must carry no trigger and belong to the requester: trigger=%q agent=%q", resumeTrigger, resumeAgent)
	}
	// response_comment_id is no longer back-filled (决策 4-4 revised):
	// insertRunResultComment was removed. The guest's answer is the agent's
	// own `goal comment` (carrying run_id=guest_run_id); consultStatus joins
	// on comment.run_id, not response_comment_id.
	// The resume must not count toward the mention cycle (决策 4-2 counter
	// only counts agent-TRIGGERED runs).
	if n, _ := cs.MentionCycleCount(ctx, g.ID); n != 1 {
		t.Fatalf("mention cycle count must stay 1 (the consult itself), got %d", n)
	}
}

// eventsBusTest subscribes a topic on the cluster's bus for the duration of a
// test, returning an unsubscribe func.
func eventsBusTest(gs *GoalService, topic string, fn func()) func() {
	return gs.bus.Subscribe(topic, func(_ context.Context, _ events.Event) { fn() })
}

var _ = fmt.Sprintf // keep fmt imported if assertions change

// TestReconcileGoalIdempotent: the Coordinator core (决策 6-4) — no
// sub-goals/changes means empty attention and NO run; running the reconcile
// repeatedly changes nothing (Reconcile is idempotent by construction).
func TestReconcileGoalIdempotent(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	var attention string
	if err := st.DB().QueryRowContext(ctx, `SELECT attention FROM goal WHERE id=?`, g.ID).Scan(&attention); err != nil || attention != "" {
		t.Fatalf("no attention expected, got %q err=%v", attention, err)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) != 1 {
		// P0-2 (决策 6-15②): Create births the owner run in-tx — the
		// reconcile must have added NOTHING beyond it (no attention → no
		// spawn; three reconciles → still exactly the one birth run).
		t.Fatalf("reconcile must not spawn runs without attention, got %d", len(runs))
	}
}

// TestReconcileGoalSpawnsOwnerOnAttention: attention (a failed sub-goal) +
// idle owner → exactly ONE owner run; repeated reconciles coalesce (决策 6-4
// conditional enqueue — event storms produce a single run).
func TestReconcileGoalSpawnsOwnerOnAttention(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// A failed sub-goal → need_recovery attention (raw row — the sub-goal
	// flow fills these; the predicate reads authoritative state).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO sub_goal (id,goal_id,title,assignee_id,status,created_at) VALUES (?,?,?,?,?,?)`,
		"sg-1", g.ID, "broken", a, "failed", now()); err != nil {
		t.Fatalf("seed sub-goal: %v", err)
	}
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var attention string
	if err := st.DB().QueryRowContext(ctx, `SELECT attention FROM goal WHERE id=?`, g.ID).Scan(&attention); err != nil || attention != "recovery" {
		t.Fatalf("attention must be recovery, got %q err=%v", attention, err)
	}
	// Storm: reconcile again (the run.terminal of the spawned run would fire
	// it once the run ends) — still ONE owner run.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) != 1 || runs[0].Role != "owner" {
		t.Fatalf("exactly one owner run expected, got %d (role=%q)", len(runs), runs[0].Role)
	}
}

// TestReconcileGoalSkipsBusyOwner: attention + an already-pending owner run
// → no duplicate (the latch's ownerIdle condition).
func TestReconcileGoalSkipsBusyOwner(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := rs.EnqueueForGoal(ctx, *g); err != nil {
		t.Fatalf("enqueue owner: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO sub_goal (id,goal_id,title,assignee_id,status,created_at) VALUES (?,?,?,?,?,?)`,
		"sg-1", g.ID, "broken", a, "failed", now()); err != nil {
		t.Fatalf("seed sub-goal: %v", err)
	}
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) != 1 {
		t.Fatalf("busy owner must not be double-spawned, got %d runs", len(runs))
	}
}

// TestReconcileGoalHumanOwner: attention persists but NO run spawns for a
// human owner (决策 6-8 — attention + notify, never a run).
func TestReconcileGoalHumanOwner(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "human", Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO sub_goal (id,goal_id,title,assignee_id,status,created_at) VALUES (?,?,?,?,?,?)`,
		"sg-1", g.ID, "broken", "anyone", "failed", now()); err != nil {
		t.Fatalf("seed sub-goal: %v", err)
	}
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var attention string
	if err := st.DB().QueryRowContext(ctx, `SELECT attention FROM goal WHERE id=?`, g.ID).Scan(&attention); err != nil || attention != "recovery" {
		t.Fatalf("human owner attention must persist, got %q err=%v", attention, err)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) != 0 {
		t.Fatalf("human owner must never get a spawned run, got %d", len(runs))
	}
}

// TestRunTerminalEvent: every terminal run publishes run.terminal (决策 6-4,
// P2-5) — the latch's second edge (owner run terminal → Reconcile).
func TestRunTerminalEvent(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r, err := rs.EnqueueForGoal(ctx, *g)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got := make(chan string, 1)
	unsub := eventsBusTest(gs, "run.terminal", func() { got <- r.ID })
	defer unsub()
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("run.terminal not published")
	}
}

// TestVerifierFlow: the optional agent verifier (决策 6-5) — machine checks
// pass → verifying + verify run; rejected → verification_result +
// quality_iteration++ + assignee rework; passed → verified + Change.
func TestVerifierFlow(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	qa := seedAgent(t, st, "qa")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, qa, "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// Assignee run completes (machine checks passed in the daemon).
	var assigneeRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&assigneeRun); err != nil {
		t.Fatalf("assignee run: %v", err)
	}
	// The daemon stamps the revision refs at run end; simulate it.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, assigneeRun); err != nil {
		t.Fatalf("stamp refs: %v", err)
	}
	if err := rs.Finish(ctx, assigneeRun, "completed", "implemented"); err != nil {
		t.Fatalf("finish assignee: %v", err)
	}
	after, _ := gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "verifying" {
		t.Fatalf("named verifier must park verifying, got %q", after.Status)
	}
	var verifyRunID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='verify' LIMIT 1`, sg.ID).Scan(&verifyRunID); err != nil {
		t.Fatalf("verify run must be enqueued: %v", err)
	}
	// The verdict tool is only reachable while the run is LIVE — claim it
	// (P1-2: a non-running verify run has no verdict window).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running' WHERE id=?`, verifyRunID); err != nil {
		t.Fatal(err)
	}

	// Round 1: rejected.
	if err := gs.VerifySubGoal(ctx, verifyRunID, "rejected", "error handling is wrong", "see auth.go"); err != nil {
		t.Fatalf("verdict rejected: %v", err)
	}
	after, _ = gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "running" || after.QualityIteration != 1 {
		t.Fatalf("reject must rework: status=%q iter=%d", after.Status, after.QualityIteration)
	}
	var vrStatus string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM verification_result WHERE sub_goal_id=? ORDER BY created_at DESC LIMIT 1`, sg.ID).Scan(&vrStatus); err != nil || vrStatus != "rejected" {
		t.Fatalf("verification_result must record rejected: %q err=%v", vrStatus, err)
	}
	// The verifier ends its turn after the verdict (production shape) — the
	// terminal run frees the coalesce for the next round's verify run.
	if err := rs.Finish(ctx, verifyRunID, "completed", "rejected verdict given"); err != nil {
		t.Fatalf("finish verify run: %v", err)
	}
	// The assignee's successor run is queued; complete it → verifying again.
	var assigneeRun2 string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' AND status='queued' LIMIT 1`, sg.ID).Scan(&assigneeRun2); err != nil {
		t.Fatalf("assignee rework run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b2', head_ref='h2' WHERE id=?`, assigneeRun2); err != nil {
		t.Fatalf("stamp refs 2: %v", err)
	}
	if err := rs.Finish(ctx, assigneeRun2, "completed", "fixed"); err != nil {
		t.Fatalf("finish rework: %v", err)
	}
	after, _ = gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "verifying" {
		t.Fatalf("rework must park verifying again, got %q", after.Status)
	}
	var verifyRunID2 string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='verify' AND status='queued' LIMIT 1`, sg.ID).Scan(&verifyRunID2); err != nil {
		t.Fatalf("second verify run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running' WHERE id=?`, verifyRunID2); err != nil {
		t.Fatal(err)
	}
	// Round 2: passed → verified + Change.
	if err := gs.VerifySubGoal(ctx, verifyRunID2, "passed", "looks good", "tests green"); err != nil {
		t.Fatalf("verdict passed: %v", err)
	}
	after, _ = gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "verified" {
		t.Fatalf("pass must verify, got %q", after.Status)
	}
	var chStatus string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM change WHERE sub_goal_id=?`, sg.ID).Scan(&chStatus); err != nil || chStatus != "ready" {
		t.Fatalf("change must be ready, got %q err=%v", chStatus, err)
	}
}

// TestVerifySubGoalPermission: only the sub-goal's verify run can issue the
// verdict (决策 6-5 — the authority lives in the run, not the caller's word).
func TestVerifySubGoalPermission(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	qa := seedAgent(t, st, "qa")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, qa, "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	var assigneeRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&assigneeRun); err != nil {
		t.Fatalf("assignee run: %v", err)
	}
	// The ASSIGNEE's run must not issue the verdict (sub-goal is running —
	// also a status guard; the role guard fires first).
	if err := gs.VerifySubGoal(ctx, assigneeRun, "passed", "trust me", ""); err == nil {
		t.Fatal("a non-verify run must not issue verdicts")
	}
	// And once in verifying, a verdict from an unrelated run is refused too.
	if err := rs.Finish(ctx, assigneeRun, "completed", "done"); err != nil {
		t.Fatalf("finish assignee: %v", err)
	}
	after, _ := gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "verifying" {
		t.Fatalf("must park verifying, got %q", after.Status)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,role,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"owner-r", g.ID, a, "worker", "queued", "owner", 1, now(), now()); err != nil {
		t.Fatalf("insert owner run: %v", err)
	}
	if err := gs.VerifySubGoal(ctx, "owner-r", "passed", "hijack", ""); err == nil {
		t.Fatal("an owner run must not issue sub-goal verdicts")
	}
	// Double verdict from the SAME verify run is refused (conditional status).
	var verifyRunID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='verify' LIMIT 1`, sg.ID).Scan(&verifyRunID); err != nil {
		t.Fatalf("verify run: %v", err)
	}
	// The verdict tool is only reachable while the run is LIVE (P1-2).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='running' WHERE id=?`, verifyRunID); err != nil {
		t.Fatal(err)
	}
	if err := gs.VerifySubGoal(ctx, verifyRunID, "passed", "ok", ""); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if err := gs.VerifySubGoal(ctx, verifyRunID, "rejected", "changed my mind", ""); err == nil {
		t.Fatal("a second verdict from the same run must be refused")
	}
}

// TestMarkChangeIntegratedConflict: an integration conflict sends the
// sub-goal back to running (quality_iteration++) and wakes the assignee;
// success marks the Change integrated (决策 6-3).
func TestMarkChangeIntegratedConflict(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	var runID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? LIMIT 1`, sg.ID).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, runID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := rs.Finish(ctx, runID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	var changeID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM change WHERE sub_goal_id=?`, sg.ID).Scan(&changeID); err != nil {
		t.Fatalf("change: %v", err)
	}

	// Conflict → rework round.
	if err := gs.MarkChangeIntegrated(ctx, changeID, false); err != nil {
		t.Fatalf("mark conflict: %v", err)
	}
	after, _ := gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "running" || after.QualityIteration != 1 {
		t.Fatalf("conflict must rework: status=%q iter=%d", after.Status, after.QualityIteration)
	}
	var chStatus string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM change WHERE id=?`, changeID).Scan(&chStatus); err != nil || chStatus != "conflict" {
		t.Fatalf("change must be conflicted, got %q err=%v", chStatus, err)
	}
	var reworkRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' AND status='queued' LIMIT 1`, sg.ID).Scan(&reworkRun); err != nil {
		t.Fatalf("assignee rework run: %v", err)
	}

	// Rework completes → the SAME change gets revision seq 2 (base = the new
	// integration base) and returns to ready (决策 6-3).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b2', head_ref='h2' WHERE id=?`, reworkRun); err != nil {
		t.Fatalf("stamp 2: %v", err)
	}
	if err := rs.Finish(ctx, reworkRun, "completed", "fixed"); err != nil {
		t.Fatalf("finish rework: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM change WHERE id=?`, changeID).Scan(&chStatus); err != nil || chStatus != "ready" {
		t.Fatalf("the conflicted change must return to ready, got %q err=%v", chStatus, err)
	}
	var seq int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT MAX(seq) FROM change_revision WHERE change_id=?`, changeID).Scan(&seq); err != nil || seq != 2 {
		t.Fatalf("revision 2 must be appended, got seq=%d err=%v", seq, err)
	}
	var revBase string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT base_ref FROM change_revision WHERE change_id=? ORDER BY seq DESC LIMIT 1`, changeID).Scan(&revBase); err != nil || revBase != "b2" {
		t.Fatalf("revision 2 must rebase onto the new base, got %q err=%v", revBase, err)
	}
	// Success path: the same change integrates.
	if err := gs.MarkChangeIntegrated(ctx, changeID, true); err != nil {
		t.Fatalf("mark integrated: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM change WHERE id=?`, changeID).Scan(&chStatus); err != nil || chStatus != "integrated" {
		t.Fatalf("the change must be integrated, got %q err=%v", chStatus, err)
	}
	// A change count of exactly ONE for the whole lifecycle.
	var count int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change WHERE sub_goal_id=?`, sg.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("the lifecycle must be ONE change with revisions, got %d err=%v", count, err)
	}
}

// TestGoalCancelDeleteCascade: goal cancel cascades to sub-goals (terminal
// ones keep history); goal delete cascades all v2 rows (决策 6-8).
func TestGoalCancelDeleteCascade(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	var runID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? LIMIT 1`, sg.ID).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, runID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := rs.Finish(ctx, runID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	// Cancel: the verified sub-goal keeps its status (terminal history), the
	// goal dies.
	if _, err := gs.Cancel(ctx, g.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	after, _ := gs.GetSubGoal(ctx, sg.ID)
	if after.Status != "verified" {
		t.Fatalf("verified sub-goal keeps history on cancel, got %q", after.Status)
	}
	// A second goal with a RUNNING sub-goal: cancel stops the work item.
	g2, err := gs.Create(ctx, Goal{Title: "g2", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal2: %v", err)
	}
	sg2, err := gs.CreateSubGoal(ctx, g2.ID, "work item 2", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal 2: %v", err)
	}
	if _, err := gs.Cancel(ctx, g2.ID); err != nil {
		t.Fatalf("cancel 2: %v", err)
	}
	after2, _ := gs.GetSubGoal(ctx, sg2.ID)
	if after2.Status != "cancelled" {
		t.Fatalf("running sub-goal must cancel with the goal, got %q", after2.Status)
	}

	// Delete: all v2 rows cascade (sub_goal/change/revision/verification +
	// handoff/consult audit).
	if _, err := gs.Assign(ctx, g2.ID, "agent", b, "", "", ""); err != nil {
		t.Fatalf("assign (for handoff rows): %v", err)
	}
	if err := gs.Delete(ctx, g2.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, q := range []struct{ table, where string }{
		{"sub_goal", "goal_id=?"}, {"change", "goal_id=?"}, {"change_revision", "change_id IN (SELECT id FROM change WHERE goal_id=?)"},
		{"verification_result", "goal_id=?"}, {"handoff_event", "goal_id=?"}, {"consult_request", "goal_id=?"},
	} {
		var n int
		_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+q.table+` WHERE `+q.where, g2.ID).Scan(&n)
		if n != 0 {
			t.Fatalf("%s rows survived the goal delete: %d", q.table, n)
		}
	}
}

// TestOwnerRunStaysActiveWithPendingWork: the v2 finalization guard — an owner
// run completing while sub-goals are still working or changes are ready must
// NOT reach the gates/done judgment; the goal stays active and the attention
// loop owns the next step. Only when nothing is pending does the owner run
// finalize (决策 6-1/6-8).
func TestOwnerRunStaysActiveWithPendingWork(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	// A FROZEN no-gate domain: a completed owner run would normally go done.
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// The owner's dispatch run completes while the sub-goal is still running.
	ownerRun, err := rs.EnqueueForGoal(ctx, *g)
	if err != nil {
		t.Fatalf("enqueue owner: %v", err)
	}
	if err := rs.Finish(ctx, ownerRun.ID, "completed", "dispatched"); err != nil {
		t.Fatalf("finish owner: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("pending sub-goal must keep the goal active, got %q", after.Status)
	}
	// Sub-goal finishes + verified → change ready → the owner is still not
	// finalized; the attention spawn fires (ReconcileGoal).
	var sgRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&sgRun); err != nil {
		t.Fatalf("sub-goal run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, sgRun); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := rs.Finish(ctx, sgRun, "completed", "done the work item"); err != nil {
		t.Fatalf("finish sub-goal: %v", err)
	}
	after, _ = gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("ready change must keep the goal active, got %q", after.Status)
	}
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The owner is woken for integration.
	var ownerRuns int
	runs, _ := rs.List(ctx, g.ID)
	for _, r := range runs {
		if r.Role == "owner" && (r.Status == "queued" || r.Status == "running") {
			ownerRuns++
		}
	}
	if ownerRuns != 1 {
		t.Fatalf("the owner must be woken for integration, got %d pending owner runs", ownerRuns)
	}
	// Mark the change integrated → the NEXT owner completion finalizes (done).
	var changeID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM change WHERE sub_goal_id=?`, sg.ID).Scan(&changeID); err != nil {
		t.Fatalf("change: %v", err)
	}
	if err := gs.MarkChangeIntegrated(ctx, changeID, true); err != nil {
		t.Fatalf("integrate: %v", err)
	}
	var woken string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND role='owner' AND status='queued' LIMIT 1`, g.ID).Scan(&woken); err != nil {
		t.Fatalf("woken owner run: %v", err)
	}
	if err := rs.Finish(ctx, woken, "completed", "integrated"); err != nil {
		t.Fatalf("finish woken owner: %v", err)
	}
	after, _ = gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("nothing pending + completed owner run must finalize done, got %q", after.Status)
	}
}

// TestReconcileGoalNoRespawnWithoutProgress: the progress guard — an owner
// woken for a set of changes that made NO progress (the changes are still
// ready, nothing new arrived) must not be re-spawned; a NEW revision re-arms
// the spawn (the E2E's 7-wake busy loop).
func TestReconcileGoalNoRespawnWithoutProgress(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	var sgRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&sgRun); err != nil {
		t.Fatalf("sub-goal run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, sgRun); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := rs.Finish(ctx, sgRun, "completed", "done"); err != nil {
		t.Fatalf("finish sub-goal: %v", err)
	}
	// First reconcile: the change is NEW → owner spawned.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var woken string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND role='owner' AND status='queued' LIMIT 1`, g.ID).Scan(&woken); err != nil {
		t.Fatalf("owner must be woken: %v", err)
	}
	// The woken owner completes WITHOUT integrating (no progress).
	if err := rs.Finish(ctx, woken, "completed", "ended without integrating"); err != nil {
		t.Fatalf("finish woken: %v", err)
	}
	// The run.terminal reconcile must NOT re-spawn: same signals, no progress.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	var pending int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND role='owner' AND status IN ('queued','running')`, g.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("no-progress wake must not re-spawn, got %d pending (err=%v)", pending, err)
	}
	// The attention persists for the human/UI.
	var attention string
	if err := st.DB().QueryRowContext(ctx, `SELECT attention FROM goal WHERE id=?`, g.ID).Scan(&attention); err != nil || attention != "integration" {
		t.Fatalf("attention must persist, got %q err=%v", attention, err)
	}
}

// TestAttentionClearsAfterIntegration: the latch's second edge — a change
// integrated in the owner run must clear the goal's attention when the owner
// run goes terminal (run.terminal → ReconcileGoal). The E2E watcher saw
// attention=integration persist ~40s past the integration; this pins the
// exact sequence: ready → attention+spawn → integrate → owner terminal →
// reconcile → attention empty.
func TestAttentionClearsAfterIntegration(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	b := seedAgent(t, st, "worker")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work item", "sub", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	var sgRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&sgRun); err != nil {
		t.Fatalf("sub-goal run: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET base_ref='b1', head_ref='h1' WHERE id=?`, sgRun); err != nil {
		t.Fatalf("stamp refs: %v", err)
	}
	// Sub-goal verified (machine) → Change ready → event.
	if err := rs.Finish(ctx, sgRun, "completed", "implemented"); err != nil {
		t.Fatalf("finish sub-goal run: %v", err)
	}
	// Coordinator (latch edge 1): attention + owner spawn.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Attention != "integration" {
		t.Fatalf("ready change must arm attention=integration, got %q", g1.Attention)
	}
	var ownerRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND role='owner' ORDER BY queued_at DESC LIMIT 1`, g.ID).Scan(&ownerRun); err != nil {
		t.Fatalf("owner run must be spawned: %v", err)
	}

	// The owner integrates the change in its run.
	var changeID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM change WHERE goal_id=? AND status='ready' LIMIT 1`, g.ID).Scan(&changeID); err != nil {
		t.Fatalf("ready change: %v", err)
	}
	if err := gs.MarkChangeIntegrating(ctx, changeID); err != nil {
		t.Fatalf("mark integrating: %v", err)
	}
	if err := gs.MarkChangeIntegrated(ctx, changeID, true); err != nil {
		t.Fatalf("mark integrated: %v", err)
	}
	// Owner run terminal (the run.terminal latch edge).
	if err := rs.Finish(ctx, ownerRun, "completed", "integrated the change"); err != nil {
		t.Fatalf("finish owner run: %v", err)
	}
	// The daemon's run.terminal subscription calls ReconcileGoal — simulate it.
	if err := gs.ReconcileGoal(ctx, g.ID); err != nil {
		t.Fatalf("reconcile after terminal: %v", err)
	}
	g2, _ := gs.Get(ctx, g.ID)
	if g2.Attention != "" {
		t.Fatalf("attention must clear once the change is integrated and the owner run is terminal, got %q", g2.Attention)
	}
}

// TestStaleAskHumanDoesNotBlockReview: an ask_human from a PRIOR run must not
// hold the finalization guard when a newer owner run completes. The guard
// narrows to ask.run_id = rc.RunID (the current run); stale asks from old
// runs are ignored. Without this, a 3-day-old ask about a since-resolved
// deadlock stranded the goal at active forever.
func TestStaleAskHumanDoesNotBlockReview(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// A stale ask_human from a prior run (simulating the 2026-08-28 consult
	// ask about conflicts that were later resolved). No human reply threads
	// under it — the old guard would count it.
	oldRunID := "old-consult-run-1"
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: a, RunID: oldRunID,
		Content: "stale ask about conflicts", AskHuman: true,
	}); err != nil {
		t.Fatalf("seed stale ask_human: %v", err)
	}
	// The current owner run completes. Under the old guard, the stale ask
	// would hold (pendingAskHuman=1). Under the narrowed guard, only THIS
	// run's asks count — it has none → guard passes → goal promotes.
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) == 0 {
		t.Fatalf("expected birth owner run")
	}
	curRun := runs[0]
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', finished_at=? WHERE id=?`, now(), curRun.ID); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	if err := gs.ReconcileOnRunEnd(ctx, goalRunContext{
		RunID: curRun.ID, GoalID: g.ID, AgentID: a, Role: "owner", Status: "completed",
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var status string
	if err := st.DB().QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, g.ID).Scan(&status); err != nil {
		t.Fatalf("load goal: %v", err)
	}
	if status == "active" {
		t.Fatalf("goal must leave active (stale ask_human must not hold the guard), still active")
	}
}

// TestProbeOldGuardCountsStaleAsk: SQL proof that the old guard (no run_id
// filter) counts the stale ask, while the new guard (ask.run_id = ?) does
// not. This is the causal evidence that the narrowing is what unblocks it.
func TestProbeOldGuardCountsStaleAsk(t *testing.T) {
	gs, _, cs, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "agent", AuthorID: a, RunID: "old-run",
		Content: "stale ask", AskHuman: true,
	}); err != nil {
		t.Fatalf("seed ask: %v", err)
	}
	var oldCount, newCount int
	// Old guard: all unanswered agent ask_human on the goal.
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment ask
		 WHERE ask.goal_id=? AND ask.ask_human=1 AND ask.author_type='agent'
		   AND NOT EXISTS (SELECT 1 FROM comment rep WHERE rep.parent_id=ask.id AND rep.author_type='human')`,
		g.ID).Scan(&oldCount); err != nil {
		t.Fatalf("old guard: %v", err)
	}
	// New guard: only the current run's ask_human.
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment ask
		 WHERE ask.goal_id=? AND ask.ask_human=1 AND ask.author_type='agent'
		   AND ask.run_id='current-run'
		   AND NOT EXISTS (SELECT 1 FROM comment rep WHERE rep.parent_id=ask.id AND rep.author_type='human')`,
		g.ID).Scan(&newCount); err != nil {
		t.Fatalf("new guard: %v", err)
	}
	if oldCount != 1 {
		t.Fatalf("old guard must count the stale ask, got %d", oldCount)
	}
	if newCount != 0 {
		t.Fatalf("new guard must not count the stale ask, got %d", newCount)
	}
}

// TestRetrySubGoalResetsFailed: RetrySubGoal on a terminal-failed sub-goal
// resets execution_attempt=0, status='running', and enqueues a fresh run.
func TestRetrySubGoalResetsFailed(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work", "desc", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// Force the sub-goal to terminal-failed (bypass the 3-failure loop).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='failed', execution_attempt=3 WHERE id=?`, sg.ID); err != nil {
		t.Fatalf("fail sub-goal: %v", err)
	}
	sg2, err := gs.RetrySubGoal(ctx, sg.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if sg2.Status != "running" || sg2.ExecutionAttempt != 0 {
		t.Fatalf("retry must reset to running/attempt=0, got status=%q attempt=%d", sg2.Status, sg2.ExecutionAttempt)
	}
	var queuedRuns int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run WHERE sub_goal_id=? AND status='queued'`, sg.ID).Scan(&queuedRuns); err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if queuedRuns != 1 {
		t.Fatalf("retry must enqueue exactly one run, got %d", queuedRuns)
	}
}

// TestRetrySubGoalRejectsNonFailed: RetrySubGoal on a non-failed sub-goal
// returns a validation error (the sub-goal is not in a retryable state).
func TestRetrySubGoalRejectsNonFailed(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	b := seedAgent(t, st, "b")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "work", "desc", b, "", "agent", a)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// Sub-goal is 'running' (its birth state) — retry must reject.
	if _, err := gs.RetrySubGoal(ctx, sg.ID); err == nil {
		t.Fatalf("retry must reject non-failed sub-goal")
	}
	// Force to verified — also rejected.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE sub_goal SET status='verified' WHERE id=?`, sg.ID); err != nil {
		t.Fatalf("verify sub-goal: %v", err)
	}
	if _, err := gs.RetrySubGoal(ctx, sg.ID); err == nil {
		t.Fatalf("retry must reject verified sub-goal")
	}
}

// TestStaleConflictResolverNilSafe: a nil staleConf (tests, no git worktree)
// must not block reconcile — the cleanup is skipped and the goal proceeds
// as before. This guards every test that constructs GoalService without
// SetStaleConflictResolver.
func TestStaleConflictResolverNilSafe(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "a")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "d", DomainID: domID, AssigneeType: "agent", AssigneeID: a, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// Seed a conflict change — without a resolver, it stays conflict and
	// holds the guard. The point is reconcile does NOT crash with nil.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO sub_goal (id,goal_id,title,assignee_id,status,created_at) VALUES (?,?,?,?,?,?)`,
		"sg-x", g.ID, "done-work", a, "failed", now()); err != nil {
		t.Fatalf("seed sub-goal: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO change (id,goal_id,sub_goal_id,status,created_at) VALUES (?,?,?,?,?)`,
		"ch-1", g.ID, "sg-x", "conflict", now()); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) == 0 {
		t.Fatalf("expected birth run")
	}
	curRun := runs[0]
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', finished_at=? WHERE id=?`, now(), curRun.ID); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	// Must not panic/err — nil resolver is skipped, not dereferenced.
	if err := gs.ReconcileOnRunEnd(ctx, goalRunContext{
		RunID: curRun.ID, GoalID: g.ID, AgentID: a, Role: "owner", Status: "completed",
	}); err != nil {
		t.Fatalf("reconcile with nil resolver: %v", err)
	}
	// The conflict holds the guard (no resolver to clear it) — goal stays
	// active. This confirms nil-safety without depending on git.
	var status string
	_ = st.DB().QueryRowContext(ctx, `SELECT status FROM goal WHERE id=?`, g.ID).Scan(&status)
	if status != "active" {
		t.Fatalf("with nil resolver, conflict must hold guard — goal stays active, got %q", status)
	}
}
