package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
)

// TestMentionCycleFailsGoal: agent-to-agent mention churn above the hard
// threshold (MaxMentionCycle) fails the goal at the NEXT trigger — the run is
// refused, the failure reason names the cycle count, queued runs are dropped,
// and the failure lands in the feed. A↔B alternation with cancelled finishes
// (cancelled runs do not advance the goal) keeps the goal active throughout.
func TestMentionCycleFailsGoal(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "churn", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	mention := func(to string, i int) {
		t.Helper()
		if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: agentA, Content: fmt.Sprintf("[@X](mention://agent/%s) round %d", to, i)}); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
	}
	// The OWNER (A) consults B repeatedly (决策 5-2: only the owner's mentions
	// dispatch — the old A↔B guest relay chain is gone). Each consult run is
	// finished cancelled (no goal advance), building agent-triggered churn.
	for i := 0; i < MaxMentionCycle; i++ {
		mention(agentB, i)
		runs, _ := rs.List(ctx, g.ID)
		last := runs[len(runs)-1]
		if err := rs.Finish(ctx, last.ID, "cancelled", "stopped"); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
	}
	// The next trigger (cycle+1) is refused and fails the goal.
	mention(agentB, MaxMentionCycle)
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "failed" {
		t.Fatalf("churn must fail the goal, got %q", after.Status)
	}
	// The refused run was not created: B has exactly MaxMentionCycle runs.
	runs, _ := rs.List(ctx, g.ID)
	bRuns := 0
	for _, r := range runs {
		if r.AgentID == agentB {
			bRuns++
		}
	}
	if bRuns != MaxMentionCycle {
		t.Fatalf("expected %d runs on B (last refused), got %d", MaxMentionCycle, bRuns)
	}
	// The failure reason + feed comment exist.
	var sysComment string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT content FROM comment WHERE goal_id=? AND author_type='system' ORDER BY created_at DESC LIMIT 1`, g.ID).Scan(&sysComment); err != nil {
		t.Fatalf("system failure comment: %v", err)
	}
	// The goal is English — the comment follows its language (决策 6-18).
	if !strings.Contains(sysComment, fmt.Sprintf("collaboration cycle reached %d", MaxMentionCycle)) {
		t.Fatalf("failure comment should name the cycle count, got: %q", sysComment)
	}
}

// TestUnfrozenPolicyForcesReview: a domain whose acceptance policy was never
// confirmed (checks_compiled_at empty) must park completed runs in review —
// nothing ran against an unconfirmed definition, so no machine judgment
// exists and the goal must not promote unattended (决策 2-4/2-5 confirmation
// gate).
func TestUnfrozenPolicyForcesReview(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	ds := NewDomainService(st, events.NewBus())
	d, err := ds.Create(ctx, Domain{Name: "unfrozen-dom", GitURL: "https://example.com/unfrozen.git"})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	// No FreezeChecks — the policy stays unfrozen.
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: d.ID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "review" {
		t.Fatalf("unfrozen policy must park in review, got %q", after.Status)
	}
	// review_request is empty for human-checkpoint reasons (scratch / unfrozen
	// / weak strength) — the internal reason is platform jargon the user does
	// not need; the approval panel itself communicates "your decision is needed".
	if after.ReviewRequest != "" {
		t.Fatalf("review_request should be empty for human-checkpoint reasons, got: %q", after.ReviewRequest)
	}
}

// TestReviewDurationRecorded: gate_decision.review_duration measures the
// seconds the goal spent in review before the decision (the health-learning
// data source), not a hardcoded zero.
func TestReviewDurationRecorded(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)
	g, err := gs.Create(ctx, Goal{Title: "gated work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	finishWithMergeGate(t, st, rs, r, "done")
	// Backdate the review entry so the duration is measurable.
	tenSecAgo := time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE activity_log SET created_at=? WHERE goal_id=? AND action='entered_review'`, tenSecAgo, g.ID); err != nil {
		t.Fatalf("backdate review entry: %v", err)
	}
	if _, err := gs.ResolveReview(ctx, g.ID, "", "approve", "looks good", "human"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var duration int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT review_duration FROM gate_decision WHERE goal_id=? ORDER BY decided_at DESC LIMIT 1`, g.ID).Scan(&duration); err != nil {
		t.Fatalf("load gate_decision: %v", err)
	}
	if duration < 9 {
		t.Fatalf("review_duration should be ~10s, got %d", duration)
	}
}

// TestTerminalMentionReopensGoal: a HUMAN comment with an action mention on a
// terminal goal (done/failed/cancelled) reopens it — GitHub's
// reopen-and-comment — and the mention then triggers normally.
func TestTerminalMentionReopensGoal(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("goal should be done, got %q", after.Status)
	}

	// Human comment with an action mention → auto-reopen + mention run.
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "human", AuthorID: "ui", Content: "[@B](mention://agent/" + agentB + ") 追加需求"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	reopened, _ := gs.Get(ctx, g.ID)
	if reopened.Status != "active" {
		t.Fatalf("mention on terminal goal must reopen it, got %q", reopened.Status)
	}
	runs, _ := rs.List(ctx, g.ID)
	found := false
	for _, run := range runs {
		if run.AgentID == agentB && (run.Status == "queued" || run.Status == "running") {
			found = true
		}
	}
	if !found {
		t.Fatal("the mention must trigger a run on the mentioned agent after reopen")
	}
}

// TestTerminalPlainCommentLandsOnly: a HUMAN comment WITHOUT a mention on a
// terminal goal lands only — no reopen, no run (a stray remark must not burn
// a cycle).
func TestTerminalPlainCommentLandsOnly(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "human", AuthorID: "ui", Content: "做得不错"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("plain comment must NOT reopen a terminal goal, got %q", after.Status)
	}
	runs, _ := rs.List(ctx, g.ID)
	for _, run := range runs {
		if run.Status == "queued" || run.Status == "running" {
			t.Fatalf("plain comment must not trigger runs, found %s", run.Status)
		}
	}
}

// TestTerminalAgentMentionNoReopen: an AGENT comment on a terminal goal does
// not trigger the reopen (only the human's words reopen — an agent has no
// business speaking on a finished goal).
func TestTerminalAgentMentionNoReopen(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: agentA, Content: "[@B](mention://agent/" + agentB + ") 帮忙"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("agent comment must NOT reopen a terminal goal, got %q", after.Status)
	}
	runs, _ := rs.List(ctx, g.ID)
	for _, run := range runs {
		if run.AgentID == agentB && (run.Status == "queued" || run.Status == "running") {
			t.Fatalf("agent comment must not trigger runs on a terminal goal, found %s", run.Status)
		}
	}
}

// TestGuestFailedRunLeavesTrace: a mention-triggered (guest) run that FAILS
// leaves a system trace in the feed — the human waiting at a checkpoint sees
// "the collaboration run failed" instead of an empty request.
func TestGuestFailedRunLeavesTrace(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: agentA, Content: "[@B](mention://agent/" + agentB + ") 帮个忙"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	var guest *Run
	for i := range runs {
		if runs[i].Role == "consult" && runs[i].AgentID == agentB {
			guest = &runs[i]
		}
	}
	if guest == nil {
		// P0-2: the owner's birth run also exists — the GUEST run on B is
		// what the mention dispatched.
		t.Fatalf("expected a guest run on B, got %d runs", len(runs))
	}
	if err := rs.Finish(ctx, guest.ID, "failed", "boom: broken model"); err != nil {
		t.Fatalf("finish guest failed: %v", err)
	}
	var content string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT content FROM comment WHERE goal_id=? AND author_type='system'`, g.ID).Scan(&content); err != nil {
		t.Fatalf("system trace comment: %v", err)
	}
	if !strings.Contains(content, "协作 run 失败") || !strings.Contains(content, "broken model") {
		t.Fatalf("trace should carry the failure, got: %q", content)
	}
	// The guest failure must NOT advance the goal.
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("guest failure must not advance the goal, got %q", after.Status)
	}
}

// TestSquadLeaderMentionIsGuest: a squad leader mentioned BY NAME
// (mention://agent/<leader>) gets a CONSULT run — a guest. P0-4 (决策 6-15④,
// Invariant 6) stripped goal authority from mention-triggered runs even on
// the assignee/leader: its completion must NOT advance the goal (the old
// "authority is dynamic" behavior let a read-only run park the goal in
// review on an empty diff). The leader's full authority comes from role=
// owner runs (the birth run / mention://squad URIs).
func TestSquadLeaderMentionIsGuest(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	leaderID := seedAgent(t, st, "L")
	domID := seedDomainWithGates(t, st)
	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "sq", LeaderID: leaderID})
	if err != nil {
		t.Fatalf("create squad: %v", err)
	}
	g, err := gs.Create(ctx, Goal{Title: "squad work", Description: "do it", DomainID: domID, AssigneeType: "squad", AssigneeID: sq.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// The timeout-cancelled window: the born leader run is cancelled — the
	// goal stays active with no pending run, so the mention below dispatches
	// a FRESH consult instead of coalescing into the pending birth run.
	var born string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND role='owner' LIMIT 1`, g.ID).Scan(&born); err != nil {
		t.Fatalf("create must birth the leader run in-tx: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled', cancel_reason='timeout', finished_at=? WHERE id=?`, now(), born); err != nil {
		t.Fatal(err)
	}
	if err := gs.ReconcileOnRunEnd(ctx, goalRunContext{RunID: born, GoalID: g.ID, AgentID: leaderID, IsLeaderRun: true, Role: "owner", Status: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	// A HUMAN mentions the leader by name (决策 5-2: human mentions dispatch
	// freely; non-owner agent mentions would be suppressed).
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "human", AuthorID: "", Content: "[@L](mention://agent/" + leaderID + ") 你来处理"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	runs, _ := rs.List(ctx, g.ID)
	var consult *Run
	for i := range runs {
		if runs[i].Role == "consult" && runs[i].AgentID == leaderID {
			consult = &runs[i]
		}
	}
	if consult == nil {
		t.Fatalf("expected a consult run on the leader, got %d runs", len(runs))
	}
	if consult.IsLeaderRun {
		t.Fatal("mention run must NOT carry the leader mark (authority is dynamic)")
	}
	// The consult completes — the goal must NOT advance (guest semantics;
	// the empty-diff hazard the role gate closed).
	if err := rs.Finish(ctx, consult.ID, "completed", "here is my take"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("a mention consult on the leader must not advance the goal, got %q", after.Status)
	}
}

// TestDoneCancelsQueuedRuns: a goal reaching done drops its queued runs in
// the same transaction — a mention run that raced ahead must not be claimed
// onto a finished goal.
func TestDoneCancelsQueuedRuns(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	agentB := seedAgent(t, st, "B")
	domID := seedDomain(t, st)
	g, err := gs.Create(ctx, Goal{Title: "work", Description: "do it", DomainID: domID, AssigneeType: "agent", AssigneeID: agentA, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	// B's mention run queues ahead of A's completion.
	if _, err := cs.Create(ctx, Comment{GoalID: g.ID, AuthorType: "agent", AuthorID: agentA, Content: "[@B](mention://agent/" + agentB + ") 排队"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r := enqueueFirst(t, rs, g)
	// The in-flight consult keeps the goal active (the finalization guard,
	// 决策 6-8: the owner ended its turn with a consult outstanding).
	if err := rs.Finish(ctx, r.ID, "completed", "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("goal must stay active while the consult is pending, got %q", after.Status)
	}
	// The guest resolves the consult — the owner resumes and completes.
	var guestRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND role='consult' LIMIT 1`, g.ID, agentB).Scan(&guestRun); err != nil {
		t.Fatalf("guest run: %v", err)
	}
	if err := rs.Finish(ctx, guestRun, "completed", "answer"); err != nil {
		t.Fatalf("finish guest: %v", err)
	}
	var resumedRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND role='owner' AND status IN ('queued','running') LIMIT 1`,
		g.ID, agentA).Scan(&resumedRun); err != nil {
		t.Fatalf("requester must resume: %v", err)
	}
	if err := rs.Finish(ctx, resumedRun, "completed", "done"); err != nil {
		t.Fatalf("finish resumed owner: %v", err)
	}
	after, _ = gs.Get(ctx, g.ID)
	if after.Status != "done" {
		t.Fatalf("goal should be done once the consult resolved, got %q", after.Status)
	}
	runs, _ := rs.List(ctx, g.ID)
	for _, run := range runs {
		if run.AgentID == agentB && run.Status != "completed" {
			t.Fatalf("the guest run must be completed, got %q", run.Status)
		}
	}
}
