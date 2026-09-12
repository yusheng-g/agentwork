package service

import (
	"context"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// TestTimelineMergesRunsActionsAndDecisions: the execution flow merges three
// sources in time order — run segments (agent turns), activity action points
// (created / review entry / handoff), and gate decisions (approve). The
// frontend renders this as the goal's flow.
func TestTimelineMergesRunsActionsAndDecisions(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, err := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	run := enqueueFirst(t, rs, g)
	finishWithMergeGate(t, st, rs, run, "ok")
	if _, err := gs.ResolveReview(ctx, g.ID, run.ID, "approve", "", "human"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := gs.MarkDelivered(ctx, g.ID, true, "merged", nil); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	items, err := gs.Timeline(ctx, g.ID)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(items) < 4 {
		t.Fatalf("expected >=4 timeline items (created + run + entered_review + approve), got %d: %+v", len(items), items)
	}
	if items[0].Kind != "action" || items[0].Action != "created" {
		t.Fatalf("first item must be the creation action, got %+v", items[0])
	}
	if items[len(items)-1].Kind != "decision" || items[len(items)-1].Decision != "approve" {
		t.Fatalf("last item must be the approve decision, got %+v", items[len(items)-1])
	}
	// Time-ordered.
	for i := 1; i < len(items); i++ {
		if items[i].At < items[i-1].At {
			t.Fatalf("timeline not time-ordered at %d: %s > %s", i, items[i-1].At, items[i].At)
		}
	}
}

// TestTimelineRunsCarryExecutionWindow: run segments expose the agent and the
// started/finished window the frontend needs to show "who handled it, for how
// long".
func TestTimelineRunsCarryExecutionWindow(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomain(t, st) // no gates → completes straight to done

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	r := enqueueFirst(t, rs, g)
	// Simulate the daemon claiming the run (queued → running sets started_at).
	if _, err := st.DB().ExecContext(ctx, `UPDATE run SET status='running', started_at=? WHERE id=?`, now(), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := rs.Finish(ctx, r.ID, "completed", "ok"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	items, err := gs.Timeline(ctx, g.ID)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	var runItem *TimelineItem
	for i := range items {
		if items[i].Kind == "run" {
			runItem = &items[i]
			break
		}
	}
	if runItem == nil {
		t.Fatal("expected a run segment")
	}
	if runItem.AgentID != agentA || runItem.RunStatus != "completed" || runItem.Attempt != 1 {
		t.Fatalf("run segment must carry agent/status/attempt, got %+v", runItem)
	}
	if runItem.StartedAt == "" || runItem.FinishedAt == "" {
		t.Fatalf("run segment must carry the execution window, got %+v", runItem)
	}
}

// ensure now() used by seed helpers is available
var _ = time.Now
var _ = store.Open
var _ = events.NewBus

// TestTimelineRejectCarriesReviewDuration: a REJECT decision records the
// seconds spent in review too (approve/reject/redirect all do — the
// duration is measured before the decision branch), so the flow card shows
// "等待审批 X" on the reject node as well.
func TestTimelineRejectCarriesReviewDuration(t *testing.T) {
	gs, rs, _, st := newTestCluster(t)
	ctx := context.Background()
	agentA := seedAgent(t, st, "A")
	domID := seedDomainWithGates(t, st)

	g, _ := gs.Create(ctx, Goal{Title: "g", AssigneeType: "agent", AssigneeID: agentA, Status: "active", DomainID: domID})
	run := enqueueFirst(t, rs, g)
	finishWithMergeGate(t, st, rs, run, "ok")
	// Simulate a realistic review wait: backdate the review entry 5 minutes.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE activity_log SET created_at=? WHERE goal_id=? AND action='entered_review'`,
		time.Now().UTC().Add(-5*time.Minute).Format(time.RFC3339Nano), g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.ResolveReview(ctx, g.ID, run.ID, "reject", "方向不对", "human"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	items, err := gs.Timeline(ctx, g.ID)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	for _, it := range items {
		if it.Kind == "decision" && it.Decision == "reject" {
			if it.ReviewDurS <= 0 {
				t.Fatalf("reject decision must carry review_duration > 0, got %d", it.ReviewDurS)
			}
			return
		}
	}
	t.Fatal("reject decision not found in timeline")
}
