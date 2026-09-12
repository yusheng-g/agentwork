package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eushing/agentwork/internal/events"
)

// ── P0 regression batch (决策 6-15): the execution-safety gate fixes ──
//
// Each test pins one hazard the batch closed:
//   TestClaimGateBlocksNonActiveGoals      P0-1 (Claim is the only admission gate)
//   TestClaimGatePassesReviewRuns          P0-1 (the platform's review request runs during the freeze)
//   TestMentionRunOnOwnerHasNoGoalAuthority P0-4 (Invariant 6: consult ≠ owner authority)
//   TestHumanConsultDoesNotBlockGate       P0-6 (the guard protects the OWNER's plan only)
//   TestFinishDropsLateResult              P0-5 (late results cannot overwrite a terminal stamp)
//   TestAssignEnqueuesSuccessorInTx        P0-2 (handoff + successor = one transaction)
//   TestHandoffLoopApproveReleasesIntent   P0-2/D-1 (the parked handoff's intent survives to approval)
//   TestReviewMentionQueuesIntent          D-1 (freeze protects execution, not intent)
//   TestSubGoalDispatchCommentChains       P2-1/P2-2 (dispatch → run trigger; pingpong exemption)

// TestClaimGateBlocksNonActiveGoals: a queued run on a review goal must NOT
// claim (the freeze); a queued run on a terminal goal must not claim either.
func TestClaimGateBlocksNonActiveGoals(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "A")
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	// Park the goal in review (handoff-loop park shape — any review entry works).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='handoff_loop: test' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	// A queued owner run must NOT claim while the goal is frozen.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,role,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"frozen-owner", g.ID, a, "worker", "queued", "owner", 1, now(), now()); err != nil {
		t.Fatal(err)
	}
	if c, err := rs.Claim(ctx, []string{a}); err != nil {
		t.Fatal(err)
	} else if c != nil {
		t.Fatalf("a queued run must not claim while the goal is in review, claimed %s", c.RunID)
	}
	// A terminal goal admits nothing either (belt-and-braces: transitions
	// drop queued runs, the gate is the final word).
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='done' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if c, err := rs.Claim(ctx, []string{a}); err != nil {
		t.Fatal(err)
	} else if c != nil {
		t.Fatalf("a queued run must not claim on a terminal goal, claimed %s", c.RunID)
	}
	// Processor runs (no goal) are unaffected by the gate.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,role,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"proc-1", "", a, "processor", "queued", "", 1, now(), now()); err != nil {
		t.Fatal(err)
	}
	if c, err := rs.Claim(ctx, []string{a}); err != nil {
		t.Fatal(err)
	} else if c == nil || c.RunID != "proc-1" {
		t.Fatalf("processor runs claim regardless of goal state, got %+v", c)
	}
}

// TestClaimGatePassesReviewRuns: role='review' is the ONE exemption — the
// platform's squad review request must run while the goal sits in review
// (it IS the approval window's evidence).
func TestClaimGatePassesReviewRuns(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "A")
	b := seedAgent(t, st, "B")
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='merge' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	// The review run: a system-authored trigger comment → role='review'.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO comment (id,goal_id,author_type,author_id,parent_id,content,created_at) VALUES (?,?,'system','',NULL,?,?)`,
		"rev-c", g.ID, "please review", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO run (id,goal_id,agent_id,run_kind,status,role,attempt,queued_at,created_at,trigger_comment_id) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"rev-r", g.ID, b, "worker", "queued", "review", 1, now(), now(), "rev-c"); err != nil {
		t.Fatal(err)
	}
	if c, err := rs.Claim(ctx, []string{b}); err != nil {
		t.Fatal(err)
	} else if c == nil || c.RunID != "rev-r" {
		t.Fatalf("the review run must claim during the freeze window, got %+v", c)
	}
}

// TestMentionRunOnOwnerHasNoGoalAuthority: a mention-triggered consult run
// landing on the goal's OWN assignee is a GUEST — its completion must not
// advance the goal (live hazard: a human asking the owner a question during
// the timeout-cancelled window "completed" the goal on an empty diff).
func TestMentionRunOnOwnerHasNoGoalAuthority(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	// The timeout-cancelled window: the born run is cancelled, goal stays
	// active with no pending run (决策 2-6 — the human decides).
	var born string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND role='owner' LIMIT 1`, g.ID).Scan(&born); err != nil {
		t.Fatalf("create must birth the owner run in-tx: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled', cancel_reason='timeout', finished_at=? WHERE id=?`, now(), born); err != nil {
		t.Fatal(err)
	}
	if err := gs.ReconcileOnRunEnd(ctx, goalRunContext{RunID: born, GoalID: g.ID, AgentID: a, Role: "owner", Status: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	// A human asks the owner a question — a consult run on the assignee.
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		Content: "[@owner](mention://agent/" + a + ") 任务状态如何？",
	}); err != nil {
		t.Fatalf("human mention: %v", err)
	}
	var consultRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND role='consult' LIMIT 1`, g.ID, a).Scan(&consultRun); err != nil {
		t.Fatalf("the mention must enqueue a consult run on the owner: %v", err)
	}
	// The consult completes — the goal must NOT advance (no goal authority).
	if err := rs.Finish(ctx, consultRun, "completed", "任务进展：还差一步"); err != nil {
		t.Fatalf("finish consult: %v", err)
	}
	g1, err := gs.Get(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g1.Status != "active" {
		t.Fatalf("a consult run on the assignee must not complete the goal (Invariant 6), got %q", g1.Status)
	}
}

// TestHumanConsultDoesNotBlockGate: the finalization guard protects the
// OWNER's unfinished plan — a HUMAN's consult (no consult_request row, no
// resume) must not hold the goal open (it used to: the guest completed with
// nobody to resume, leaving the goal dead active).
func TestHumanConsultDoesNotBlockGate(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	b := seedAgent(t, st, "guest")
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	ownerRun := enqueueFirst(t, rs, g)
	// A HUMAN mentions b while the owner works (no consult_request row).
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		Content: "[@guest](mention://agent/" + b + ") 帮忙看下这个方案",
	}); err != nil {
		t.Fatalf("human mention: %v", err)
	}
	// The owner ends its turn while the human's consult is still in flight —
	// the gate fires anyway (the guest's answer lands in the feed for the
	// human; nobody's plan is waiting on it).
	if err := rs.Finish(ctx, ownerRun.ID, "completed", "finished the work"); err != nil {
		t.Fatalf("finish owner: %v", err)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Status != "done" {
		t.Fatalf("a human consult must not hold the gate, goal is %q (want done — no gates configured)", g1.Status)
	}
}

// TestFinishDropsLateResult: once another writer terminalized the run (the
// runaway reaper / the handoff window stamp), a LATE result from the still-
// running process must not overwrite the terminal state nor reconcile.
func TestFinishDropsLateResult(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "A")
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	run := enqueueFirst(t, rs, g)
	if _, err := rs.Claim(ctx, []string{a}); err != nil {
		t.Fatal(err)
	}
	// The reaper terminalizes the row behind the process's back.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE run SET status='cancelled', cancel_reason='runaway', finished_at=? WHERE id=? AND status='running'`,
		now(), run.ID); err != nil {
		t.Fatal(err)
	}
	// The zombie process reports completed — the stamp refuses it.
	err = rs.Finish(ctx, run.ID, "completed", "I actually finished!")
	if !errors.Is(err, ErrRunAlreadyTerminal) {
		t.Fatalf("Finish must drop the late result, got %v", err)
	}
	var status, reason string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status, cancel_reason FROM run WHERE id=?`, run.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || reason != "runaway" {
		t.Fatalf("the reaper's stamp must stand, got %s/%s", status, reason)
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Status != "active" {
		t.Fatalf("the late result must not advance the goal, got %q", g1.Status)
	}
}

// TestAssignEnqueuesSuccessorInTx: the ownership change and the new owner's
// run are one transaction — no caller-side enqueue, and the handoff audit
// row is complete (to_run_id) the moment Assign returns.
func TestAssignEnqueuesSuccessorInTx(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "A")
	b := seedAgent(t, st, "B")
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gs.Assign(ctx, g.ID, "agent", b, "接力", "", ""); err != nil {
		t.Fatalf("assign: %v", err)
	}
	var queued string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND role='owner' AND status='queued' LIMIT 1`,
		g.ID, b).Scan(&queued); err != nil {
		t.Fatalf("the new owner's run must exist right after Assign: %v", err)
	}
	var toRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT to_run_id FROM handoff_event WHERE goal_id=? ORDER BY created_at DESC LIMIT 1`, g.ID).Scan(&toRun); err != nil {
		t.Fatal(err)
	}
	if toRun != queued {
		t.Fatalf("handoff_event.to_run_id must be back-filled in-tx, got %q want %q", toRun, queued)
	}
}

// TestHandoffLoopApproveReleasesIntent: the 8th handoff parks the goal with
// the new owner's run QUEUED (a durable intent, not a lost run) — the human
// approves, the goal releases, and the intent claims. This is the dead-active
// state the batch closed (approve used to release with NO run at all).
func TestHandoffLoopApproveReleasesIntent(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agents := []string{}
	for _, name := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I"} {
		agents = append(agents, seedAgent(t, st, name))
	}
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agents[0], Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	// 8 handoffs (human actor) — the 8th parks the goal in review.
	var last string
	for i := 1; i <= 8; i++ {
		target := agents[i%len(agents)]
		if _, err := gs.Assign(ctx, g.ID, "agent", target, "", "", ""); err != nil {
			t.Fatalf("handoff %d: %v", i, err)
		}
		last = target
	}
	g1, _ := gs.Get(ctx, g.ID)
	if g1.Status != "review" {
		t.Fatalf("the 8th handoff must park the goal in review, got %q", g1.Status)
	}
	// The intent queued for the FINAL owner (the loop's stale queued runs
	// were superseded in the same transactions).
	var queued string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id, agent_id FROM run WHERE goal_id=? AND role='owner' AND status='queued' LIMIT 1`,
		g.ID).Scan(&queued, &g1.AssigneeID); err != nil {
		t.Fatalf("the 8th handoff's intent must be queued: %v", err)
	}
	if g1.AssigneeID != last {
		t.Fatalf("goal assignee %q, want the final handoff target %q", g1.AssigneeID, last)
	}
	// While frozen, the intent cannot claim.
	if c, err := rs.Claim(ctx, []string{last}); err != nil {
		t.Fatal(err)
	} else if c != nil {
		t.Fatalf("the intent must wait for the human decision, claimed %s", c.RunID)
	}
	// The human approves the loop — the goal releases and the intent claims.
	if _, err := gs.ResolveReview(ctx, g.ID, "", "approve", "继续", "human"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	c, err := rs.Claim(ctx, []string{last})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || c.RunID != queued {
		t.Fatalf("the intent must claim after approve, got %+v", c)
	}
}

// TestReviewMentionQueuesIntent (D-1, 决策 2-3 revised): a mention landing
// while the goal is frozen enqueues a durable intent instead of being
// silently dropped — it waits at the Claim gate for the human's release.
func TestReviewMentionQueuesIntent(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "A")
	b := seedAgent(t, st, "B")
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE goal SET status='review', review_request='merge' WHERE id=?`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		Content: "[@guest](mention://agent/" + b + ") 帮我看一下这个方案",
	}); err != nil {
		t.Fatalf("mention during review: %v", err)
	}
	var queued string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND role='consult' AND status='queued' LIMIT 1`,
		g.ID, b).Scan(&queued); err != nil {
		t.Fatalf("the review-window mention must queue as an intent: %v", err)
	}
	// Claim gate holds it during the freeze.
	if c, err := rs.Claim(ctx, []string{b}); err != nil {
		t.Fatal(err)
	} else if c != nil {
		t.Fatalf("the intent must wait for the release, claimed %s", c.RunID)
	}
}

// TestSubGoalDispatchCommentChains (P2-1/P2-2): the dispatch comment is born
// with the sub-goal and its first run carries it as trigger — the report
// threads to it — and the dispatch does NOT inflate the mention-cycle count
// (workflow execution ≠ consult churn).
func TestSubGoalDispatchCommentChains(t *testing.T) {
	gs, _, cs, st := newTestCluster(t)
	ctx := context.Background()
	owner := seedAgent(t, st, "owner")
	worker := seedAgent(t, st, "worker")
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: owner, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "子任务一", "实现登录", worker, "", "agent", owner)
	if err != nil {
		t.Fatalf("create sub-goal: %v", err)
	}
	// The dispatch comment exists, authored by the dispatcher, with a mention.
	var dispatchID, dispatchContent string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id, content FROM comment WHERE goal_id=? AND author_type='agent' AND author_id=? ORDER BY created_at DESC LIMIT 1`,
		g.ID, owner).Scan(&dispatchID, &dispatchContent); err != nil {
		t.Fatalf("dispatch comment: %v", err)
	}
	if !strings.Contains(dispatchContent, "mention://agent/"+worker) {
		t.Fatalf("the dispatch comment must mention the assignee, got %q", dispatchContent)
	}
	// The first run's trigger IS the dispatch comment.
	var trigger string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT trigger_comment_id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	if trigger != dispatchID {
		t.Fatalf("the sub-goal run's trigger must be the dispatch comment, got %q", trigger)
	}
	// Pingpong exemption: the agent-authored dispatch must not count as churn.
	if n, err := cs.MentionCycleCount(ctx, g.ID); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("sub-goal dispatch must not count toward the mention cycle, got %d", n)
	}
}

// TestRejectSpawnsOwnerDespitePendingConsult: the D-1 × reject interplay —
// a human's consult intent queued on the OWNER during review must NOT absorb
// the reject's owner run (the cross-role coalesce used to merge the work run
// into the read-only consult, stranding the goal active with no run).
func TestRejectSpawnsOwnerDespitePendingConsult(t *testing.T) {
	gs, rs, cs, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "owner")
	domID := seedDomainWithGates(t, st)
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatal(err)
	}
	finishWithMergeGate(t, st, rs, enqueueFirst(t, rs, g), "ok")
	if got, _ := gs.Get(ctx, g.ID); got.Status != "review" {
		t.Fatalf("setup: expected review, got %q", got.Status)
	}
	// The human asks the OWNER something while the goal is frozen — a
	// consult intent queues on the owner (D-1).
	if _, err := cs.Create(ctx, Comment{
		GoalID: g.ID, AuthorType: "human", AuthorID: "ui",
		Content: "[@owner](mention://agent/" + a + ") 顺便补个文档",
	}); err != nil {
		t.Fatalf("mention during review: %v", err)
	}
	var consult string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND role='consult' AND status='queued' LIMIT 1`,
		g.ID, a).Scan(&consult); err != nil {
		t.Fatalf("the consult intent must queue: %v", err)
	}
	// The human rejects — the owner's WORK run must be born, NOT merged
	// into the pending consult.
	if _, err := gs.ResolveReview(ctx, g.ID, "", "reject", "方向调整一下", "human"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	var ownerRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND agent_id=? AND role='owner' AND status='queued' LIMIT 1`,
		g.ID, a).Scan(&ownerRun); err != nil {
		t.Fatalf("the reject must spawn the owner's work run: %v", err)
	}
	if ownerRun == consult {
		t.Fatal("the owner run must not coalesce into the pending consult")
	}
}

// TestScratchDomainValidation: a scratch domain needs no git_url; issue
// tracking is repo-only; a repo domain still requires git_url.
func TestScratchDomainValidation(t *testing.T) {
	st := newTestStore(t)
	ds := NewDomainService(st, events.NewBus())
	ctx := context.Background()

	d, err := ds.Create(ctx, Domain{Name: "research", Type: "scratch", PolicyText: "总结要客观"})
	if err != nil {
		t.Fatalf("a scratch domain needs no git_url: %v", err)
	}
	if d.Type != "scratch" || d.GitURL != "" {
		t.Fatalf("scratch domain shape wrong: %s %q", d.Type, d.GitURL)
	}
	// Issue tracking is meaningless without a repo.
	if _, err := ds.Create(ctx, Domain{Name: "bad-issue", Type: "scratch", IssueRepo: "o/r"}); err == nil {
		t.Fatal("scratch domains must reject issue tracking")
	}
	// A repo domain still requires git_url.
	if _, err := ds.Create(ctx, Domain{Name: "no-url", Type: "repo"}); err == nil {
		t.Fatal("repo domains must require git_url")
	}
}

// TestScratchGoalForcesHumanGate: the scratch deliverable is a report — no
// diff-based gate can ever fire, so the human checkpoint is unconditional
// (a strong scratch domain with configured gates must NOT auto-done).
func TestScratchGoalForcesHumanGate(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "A")
	ds := NewDomainService(st, events.NewBus())
	d, err := ds.Create(ctx, Domain{Name: "research", Type: "scratch"})
	if err != nil {
		t.Fatal(err)
	}
	// Strong strength + configured gates — on a repo domain this would
	// auto-done with no gate hit; on scratch it MUST still park.
	if _, err := ds.FreezeChecks(ctx, d.ID, Checks{
		Verify: []string{"test -f report.md"},
		Gates:  []GateRule{{Name: "merge", When: "人工审批"}},
	}, "strong"); err != nil {
		t.Fatal(err)
	}
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: d.ID})
	if err != nil {
		t.Fatal(err)
	}
	run := enqueueFirst(t, rs, g)
	if err := rs.Finish(ctx, run.ID, "completed", "调研完成，报告见目录"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := gs.Get(ctx, g.ID)
	if got.Status != "review" {
		t.Fatalf("a scratch goal must park in review regardless of strength/gates, got %q", got.Status)
	}
}

// TestScratchSubGoalsAreNoCode: a scratch goal CAN split — each sub-goal
// works in its own sg/<subGoalID> directory and verifies with NO Change
// (决策 6-8's no-code sub-goal: the deliverable is the files + report, not
// a merged Change). The wrap-up attention edge wakes the owner to review.
func TestScratchSubGoalsAreNoCode(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	a := seedAgent(t, st, "A")
	b := seedAgent(t, st, "B")
	ds := NewDomainService(st, events.NewBus())
	d, err := ds.Create(ctx, Domain{Name: "research", Type: "scratch"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: a, Status: "active", DomainID: d.ID})
	if err != nil {
		t.Fatal(err)
	}
	sg, err := gs.CreateSubGoal(ctx, g.ID, "拆活", "子任务", b, "", "agent", a)
	if err != nil {
		t.Fatalf("scratch goals must split (no-code sub-goals): %v", err)
	}
	// The sub-goal's run completes with no base/head refs (no git) — it
	// verifies WITHOUT a Change.
	var sgRun string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE sub_goal_id=? AND role='subgoal' LIMIT 1`, sg.ID).Scan(&sgRun); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(ctx, sgRun, "completed", "调研完成，产物在 sg 目录"); err != nil {
		t.Fatalf("finish sg run: %v", err)
	}
	got, _ := gs.GetSubGoal(ctx, sg.ID)
	if got.Status != "verified" {
		t.Fatalf("the no-code sub-goal must verify, got %q", got.Status)
	}
	var changes int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM change WHERE sub_goal_id=?`, sg.ID).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if changes != 0 {
		t.Fatalf("a scratch sub-goal must produce NO Change, got %d", changes)
	}
	// The owner is woken by the wrap-up attention edge (verified sub-goal,
	// no Change) — the finalization guard keeps the goal open meanwhile.
	after, _ := gs.Get(ctx, g.ID)
	if after.Status != "active" {
		t.Fatalf("the goal stays active (pending sub-goals), got %q", after.Status)
	}
}

// TestSubGoalRejectsReviewerAssignee (REVIEWER-ONLY RULE): a squad's
// reviewer members review — they are never handed work items. Dispatching
// to a reviewer is rejected loudly; a non-reviewer member is fine.
func TestSubGoalRejectsReviewerAssignee(t *testing.T) {
	gs, _, _, st := newTestCluster(t)
	ctx := context.Background()
	leader := seedAgent(t, st, "leader")
	reviewer := seedAgent(t, st, "reviewer")
	worker := seedAgent(t, st, "worker")
	squadSvc := NewSquadService(st, events.NewBus())
	sq, err := squadSvc.Create(ctx, Squad{Name: "dev-team", LeaderID: leader})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", reviewer, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := squadSvc.AddMember(ctx, sq.ID, "agent", worker, "worker"); err != nil {
		t.Fatal(err)
	}
	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "squad", AssigneeID: sq.ID, Status: "active", DomainID: seedDomain(t, st)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gs.CreateSubGoal(ctx, g.ID, "拆给审查者", "工作项", reviewer, "", "agent", leader); err == nil {
		t.Fatal("dispatching work to a reviewer must be rejected (reviewers only review)")
	}
	if _, err := gs.CreateSubGoal(ctx, g.ID, "拆给干活的", "工作项", worker, "", "agent", leader); err != nil {
		t.Fatalf("dispatching to a non-reviewer member must work: %v", err)
	}
}
