package service

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// newTestStore opens a fresh in-memory SQLite store for a test. Each test gets
// its own DB so they're independent. (modernc.org/sqlite supports ":memory:".)
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newTestCluster wires Goal+Run+Comment with their cross-references, plus a
// minimal runtime+agent fixture so goal enqueue has a real agent to target.
func newTestCluster(t *testing.T) (*GoalService, *RunService, *CommentService, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	bus := events.NewBus()
	gs := NewGoalService(st, bus)
	rs := NewRunService(st, bus)
	cs := NewCommentService(st, bus)
	gs.SetRunService(rs)
	rs.SetGoalService(gs)
	cs.SetRunService(rs)
	cs.SetGoalService(gs)
	return gs, rs, cs, st
}

// seedDomain inserts a domain with a FROZEN empty acceptance policy (NO
// gates — completed runs promote to done, so the pre-v2 test semantics
// hold) and returns its id. The freeze matters: an unfrozen policy forces
// the human checkpoint by design (决策 2-4/2-5 confirmation gate). v2:
// agent-executed goals require a domain (DESIGN.md §2). Review-path
// tests freeze gates separately.
func seedDomain(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	ds := NewDomainService(st, events.NewBus())
	d, err := ds.Create(ctx, Domain{Name: "test-domain", GitURL: "https://example.com/test.git"})
	if err != nil {
		t.Fatalf("seed domain: %v", err)
	}
	if _, err := ds.FreezeChecks(ctx, d.ID, Checks{}, "medium"); err != nil {
		t.Fatalf("freeze seed domain checks: %v", err)
	}
	return d.ID
}

// seedAgent inserts a runtime + agent and returns the agent id.
func seedAgent(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntimeService(st).Create(ctx, Runtime{Name: "rt-" + name, MachineID: "m1"})
	if err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	// Machine-owned runtimes claim only while their machine is connected —
	// seed the connected machine row the runtime points at, or the claim
	// gate silently blocks every run in the test (live regression: the CLI
	// branch moved the fixture to machine_id without seeding the machine).
	if err := NewMachineService(st).Register(ctx, Machine{ID: "m1", Name: "m1", Hostname: "host"}, "[]"); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	a, err := NewAgentService(st, events.NewBus()).Create(ctx, Agent{Name: name, RuntimeID: rt.ID, MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return a.ID
}

// enqueueFirst forces the first run for an already-created active goal and
// returns it. (The HTTP handler does this; tests call the service directly.)
func enqueueFirst(t *testing.T, rs *RunService, g *Goal) *Run {
	t.Helper()
	r, err := rs.EnqueueForGoal(context.Background(), *g)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return r
}

// TestReconcileNormalCompletion: A run completes while A is still the owner →
// goal flips to done. The happy path that proves reconcile DOES advance the
// goal when the run belongs to the current assignee.
func TestReconcileNormalCompletion(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "do thing", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)

	// Simulate the daemon finishing a successful run.
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("expected goal done, got %q", after.Status)
	}
}

// TestHandoffDoesNotClobber: A's run completes AFTER the goal was handed off
// to B. A's result must NOT flip the goal to done — A is no longer the owner,
// so reconcile discards it. This is the core self-consistency invariant
// (DESIGN.md §9): the design without an external authority.
func TestHandoffDoesNotClobber(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	rA := enqueueFirst(t, rs, g)
	// Claim A's run so it's "running" (an in-flight run).
	if _, err := rs.Claim(ctx, []string{agentA}); err != nil {
		t.Fatalf("claim A: %v", err)
	}

	// Hand off to B while A's run is in flight. This enqueues a B run.
	if _, err := gs.Assign(ctx, g.ID, "agent", agentB, "handoff note", "", ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := rs.EnqueueForGoal(ctx, Goal{ID: g.ID, AssigneeType: "agent", AssigneeID: agentB}); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}

	// A's in-flight run now reports completion. It must NOT change goal status.
	if err := rs.Finish(ctx, rA.ID, "completed", "A finished"); err != nil {
		t.Fatalf("finish A: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status == "done" {
		t.Fatalf("A's orphaned run must not mark goal done; status=%q", after.Status)
	}
	if after.AssigneeID != agentB {
		t.Fatalf("goal should still be assigned to B; got %q", after.AssigneeID)
	}
	// The handoff note must survive A's orphaned completion — A no longer owns
	// the goal, so its run must not clear the note meant for B. (P2 regression.)
	if after.HandoffNote != "handoff note" {
		t.Fatalf("handoff note must be preserved across A's orphaned finish; got %q", after.HandoffNote)
	}
}

// TestHandoffNoteClearedOnOwnerDone: when the owning run completes, the
// handoff/wakeup note is cleared (consumed) as part of the same reconcile that
// promotes the goal to done. Confirms handoff_note cleanup lives in the goal
// layer, gated on owns+done — not in the daemon.
func TestHandoffNoteClearedOnOwnerDone(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	// Pre-seed a handoff note directly (Assign takes one, or a child wake sets it).
	g, err := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active", HandoffNote: "scope note", DomainID: domID})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	mid, _ := gs.Get(ctx, g.ID)
	if mid.HandoffNote != "scope note" {
		t.Fatalf("note should be present at start; got %q", mid.HandoffNote)
	}
	// Owner A completes the run legitimately → goal done + note consumed.
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("expected done, got %q", after.Status)
	}
	if after.HandoffNote != "" {
		t.Fatalf("handoff note must be cleared on owner-done; got %q", after.HandoffNote)
	}
}

// TestCancelNotClobbered: a goal is cancelled while a run is in flight. The
// run completing must not flip the goal out of cancelled.
func TestCancelNotClobbered(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	r := enqueueFirst(t, rs, g)
	if _, err := rs.Claim(ctx, []string{agentA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := gs.Cancel(ctx, g.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The in-flight run reports completion.
	if err := rs.Finish(ctx, r.ID, "completed", "late"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %q (in-flight run clobbered)", after.Status)
	}
}

// TestCoalescePending: enqueuing a second run for the same (goal,agent) while
// one is pending coalesces — exactly one queued/running run per pair.
func TestCoalescePending(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	g, _ := gs.Create(ctx, Goal{Title: "x", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	r1 := enqueueFirst(t, rs, g)
	// Hand off to A again (re-enqueue) while r1 is still queued.
	if _, err := gs.Assign(ctx, g.ID, "agent", agentA, "again", "", ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := rs.EnqueueForGoal(ctx, Goal{ID: g.ID, AssigneeType: "agent", AssigneeID: agentA}); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	pending := 0
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "running" {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("expected exactly 1 pending run after coalesce, got %d (r1=%s)", pending, r1.Status)
	}
}

// TestReopenFailedGoal: the human take-over path — failed/cancelled → active
// with a fresh run (attempt resets), exactly like a reject iteration.
func TestReopenFailedGoal(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='failed' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	got, err := gs.Reopen(ctx, g.ID, "重试一下", "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got.Status != "active" || got.HandoffNote == "" {
		t.Fatalf("reopened goal must be active with a note, got %+v", got)
	}
	runs, _ := rs.List(ctx, g.ID)
	if len(runs) == 0 || runs[len(runs)-1].Status != "queued" || runs[len(runs)-1].Attempt != 1 {
		t.Fatalf("reopen must enqueue a fresh run at attempt 1, got %+v", runs)
	}
	// Done goals cannot be reopened.
	if _, err := gs.Reopen(ctx, g.ID, "x", ""); err == nil {
		t.Fatal("active goal must not be reopenable")
	}
}

// TestReopenClearsStaleGateDecision: a done goal reached review, was approved
// (a gate_decision row), and delivered to done. Reopening it for a new cycle
// MUST delete that superseded approve — otherwise three readers treat the goal
// as "already approved" of the OLD cycle: ResolveReview's duplicate-decision
// guard (the human re-clicks the stale card whose button carries the old
// evidence run_id, so lastRunID==runID holds → "already approved"),
// maybeFireReviewReady's `decided>0` gate (suppresses the new cycle's
// review-window-ready, so the card never patches), and deliver's latest-row
// lookup. This is the live regression behind goal 009aa8159a242edb.
func TestReopenClearsStaleGateDecision(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)

	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate the first review cycle: goal parked in review, the human
	// approved (a gate_decision row with the old evidence run_id), then the
	// goal delivered to done. We model the cycle's end state directly — the
	// gate_decision row is what matters, not how it got there.
	oldRunID := "run-oldcycle-evidence"
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO gate_decision (id,goal_id,run_id,gate_rule,decision,reason,decided_by,decided_at,review_duration) VALUES (?,?,?,?,?,?,?,?,0)`,
		"gd-1", g.ID, oldRunID, "diff_contains", "approve", "", "human", now()); err != nil {
		t.Fatalf("seed gate_decision: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='done' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}

	// Reopen for a new cycle.
	if _, err := gs.Reopen(ctx, g.ID, "下一轮", ""); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// The superseded approve MUST be gone — no row for this goal at all.
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gate_decision WHERE goal_id=?`, g.ID).Scan(&n); err != nil {
		t.Fatalf("count gate_decision: %v", err)
	}
	if n != 0 {
		t.Fatalf("reopen must clear the prior cycle's gate_decision rows, got %d", n)
	}

	// The new cycle's approve must not be rejected as "already approved" —
	// the guard finds no lastDecision, so the approve records cleanly. Put
	// the goal back in review (the new cycle's park) and approve on the new
	// evidence run.
	newRunID := "run-newcycle-evidence"
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.ResolveReview(ctx, g.ID, newRunID, "approve", "", "human"); err != nil {
		t.Fatalf("new cycle approve after reopen: %v", err)
	}
	// And the new approve is now the only row, on the new evidence run.
	var gotRunID, gotDecision string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT run_id, decision FROM gate_decision WHERE goal_id=? ORDER BY decided_at DESC LIMIT 1`, g.ID).
		Scan(&gotRunID, &gotDecision); err != nil {
		t.Fatalf("read new gate_decision: %v", err)
	}
	if gotRunID != newRunID || gotDecision != "approve" {
		t.Fatalf("new cycle approve did not record on the new evidence run, got run=%q decision=%q", gotRunID, gotDecision)
	}
}

// TestClaimPerGoalSerialization: a queued run of a goal is NOT claimed while
// another run of the same goal is running (the worktree is exclusive); it
// becomes claimable once the running run finishes. Processor runs (no goal)
// are never blocked.
func TestClaimOwnerSingleFlight(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	agentC := seedAgent(t, st, "C")

	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: seedDomainWithGates(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	first := enqueueFirst(t, rs, g)
	// The owner's run is claimed (running).
	if _, err := rs.Claim(ctx, []string{agentA}); err != nil {
		t.Fatal(err)
	}
	// OWNER single-flight (决策 6-2): a second owner-role run of the same goal
	// must NOT be claimable while the owner runs (one goal-branch writer).
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,role,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"owner-2", g.ID, agentB, "worker", "queued", "owner", 1, now(), now()); err != nil {
		t.Fatal(err)
	}
	if c, err := rs.Claim(ctx, []string{agentB}); err != nil {
		t.Fatal(err)
	} else if c != nil {
		t.Fatalf("a second owner run must wait while the owner runs, claimed %s", c.RunID)
	}
	// A CONSULT run IS claimable in parallel (read-only workspace snapshot) —
	// on a DIFFERENT agent than the waiting owner-2 run (the (goal,agent)
	// coalesce would otherwise swallow it).
	if _, err := rs.EnqueueForMention(ctx, g.ID, agentC, "c1"); err != nil {
		t.Fatal(err)
	}
	c, err := rs.Claim(ctx, []string{agentC})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || c.GoalID != g.ID {
		t.Fatalf("consult run must run in parallel with the owner, got %+v", c)
	}
	// Finish the consult run.
	if _, err := st.DB().ExecContext(ctx, `UPDATE run SET status='completed', finished_at=? WHERE id=?`, now(), c.RunID); err != nil {
		t.Fatal(err)
	}
	// Finish the owner's run → the second owner run becomes claimable.
	if _, err := st.DB().ExecContext(ctx, `UPDATE run SET status='completed', finished_at=? WHERE id=?`, now(), first.ID); err != nil {
		t.Fatal(err)
	}
	c, err = rs.Claim(ctx, []string{agentB})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || c.RunID != "owner-2" {
		t.Fatalf("the waiting owner run must claim after the owner finishes, got %+v", c)
	}
}

// TestDeleteCarriesRunningRunIDs: Delete removes the run rows in its cascade,
// so the daemon cannot find the running processes afterwards — the
// goal:deleted payload must carry the ids captured BEFORE the cascade (the
// daemon cuts the processes from them, same as goal cancel 决策 4-12).
func TestDeleteCarriesRunningRunIDs(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)

	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	// Mark it running (as the daemon's Claim would) so Delete must report it.
	if _, err := st.DB().ExecContext(ctx, `UPDATE run SET status='running' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}

	payloadCh := make(chan map[string]any, 1)
	gs.bus.Subscribe("goal:deleted", func(_ context.Context, e events.Event) {
		if m, ok := e.Payload.(map[string]any); ok {
			payloadCh <- m
		}
	})
	if err := gs.Delete(ctx, g.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	select {
	case m := <-payloadCh:
		ids, ok := m["run_ids"].([]string)
		if !ok || len(ids) != 1 || ids[0] != r.ID {
			t.Fatalf("goal:deleted must carry the running run id, got %v", m["run_ids"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("goal:deleted event never arrived")
	}
}

// TestCommentReopenStampsTriggerAndOwnerRole (决策 4-1 修订): a
// comment-triggered reopen's run carries the comment as its trigger with an
// EXPLICIT owner role — the structural follow-up marker (a human-authored
// trigger would derive 'consult'; the marker lets the daemon distinguish
// follow-ups without any string matching).
func TestCommentReopenStampsTriggerAndOwnerRole(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	owner := seedAgent(t, st, "owner")
	g, err := gs.Create(ctx, Goal{Title: "g", Description: "do it", AssigneeType: "agent", AssigneeID: owner, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	// Terminate (dropping the create-time queued run — the terminal
	// transition does this in production; the reopen must not coalesce into
	// a stale pending run), then reopen via a human comment.
	if _, err := st.DB().ExecContext(ctx, `UPDATE goal SET status='done' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled', cancel_reason='goal_terminal' WHERE goal_id=? AND status='queued'`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.Reopen(ctx, g.ID, "", "cmt-1"); err != nil {
		t.Fatal(err)
	}
	var role, trigger string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT role, trigger_comment_id FROM run WHERE goal_id=? AND status='queued' ORDER BY queued_at DESC LIMIT 1`, g.ID).Scan(&role, &trigger); err != nil {
		t.Fatal(err)
	}
	if role != "owner" {
		t.Fatalf("the reopened run must be an owner run, got %q", role)
	}
	if trigger != "cmt-1" {
		t.Fatalf("the reopened run must carry the comment as its trigger, got %q", trigger)
	}
	// No duplicate "重开：" comment — the human's comment itself is the
	// reason and the trigger.
	var dup int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comment WHERE goal_id=? AND content LIKE '重开：%'`, g.ID).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if dup != 0 {
		t.Fatalf("a comment-triggered reopen must not duplicate the reason, got %d", dup)
	}
	// CompleteFollowUp: the plain conditional promote (called by the daemon
	// when the follow-up produced no changes).
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("reopen must activate the goal, got %q", after.Status)
	}
	ok, err := gs.CompleteFollowUp(ctx, g.ID)
	if err != nil || !ok {
		t.Fatalf("CompleteFollowUp: ok=%v err=%v", ok, err)
	}
	after, _ = gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("a zero-change follow-up must return the goal to done, got %q", after.Status)
	}
	// A second call is a no-op (already done).
	if ok, _ := gs.CompleteFollowUp(ctx, g.ID); ok {
		t.Fatal("a done goal must not be re-promoted")
	}
}
