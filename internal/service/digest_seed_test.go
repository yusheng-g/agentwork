package service

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// digestSeedFixture builds a store plus the three services the seed needs,
// with a runtime + machine row present when withRuntime is set (SeedSteward
// only creates the steward for an active runtime).
func digestSeedFixture(t *testing.T, withRuntime bool) (*store.Store, *AgentService, *DomainService, *ScheduleService) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "aw.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	agentSvc := NewAgentService(st, bus)
	domainSvc := NewDomainService(st, bus)
	schedSvc := NewScheduleService(st, bus)
	if withRuntime {
		if _, err := st.DB().Exec(`INSERT INTO machine (id,name,status,created_at) VALUES ('m1','box','connected','2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("insert machine: %v", err)
		}
		if _, err := st.DB().Exec(`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('r1','claude@box','m1','[]','{}','active','2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("insert runtime: %v", err)
		}
	}
	return st, agentSvc, domainSvc, schedSvc
}

func TestSeedDigestScheduleSkipsWithoutSteward(t *testing.T) {
	// A runtime exists but no steward was seeded: SeedDigestSchedule must
	// skip silently (the machine-registration path retries it) and create
	// nothing.
	st, agentSvc, domainSvc, schedSvc := digestSeedFixture(t, true)
	if err := SeedDigestSchedule(context.Background(), st, agentSvc, domainSvc, schedSvc); err != nil {
		t.Fatalf("seed without steward: %v (want silent skip)", err)
	}
	schs, _ := schedSvc.List(context.Background())
	if len(schs) != 0 {
		t.Fatalf("schedule created without steward: %d", len(schs))
	}
	doms, _ := domainSvc.List(context.Background())
	if len(doms) != 0 {
		t.Fatalf("domain created without steward: %d", len(doms))
	}
}

func TestSeedDigestScheduleCreatesAndIsIdempotent(t *testing.T) {
	st, agentSvc, domainSvc, schedSvc := digestSeedFixture(t, true)
	if err := agentSvc.SeedSteward(context.Background()); err != nil {
		t.Fatalf("seed steward: %v", err)
	}
	if err := SeedDigestSchedule(context.Background(), st, agentSvc, domainSvc, schedSvc); err != nil {
		t.Fatalf("seed digest: %v", err)
	}
	schs, err := schedSvc.List(context.Background())
	if err != nil || len(schs) != 1 {
		t.Fatalf("after seed: %d schedules (err=%v), want 1", len(schs), err)
	}
	sch := schs[0]
	if sch.Name != digestScheduleName || !sch.BuiltIn {
		t.Fatalf("schedule = %+v, want built-in %q", sch, digestScheduleName)
	}
	if sch.CronExpression == "" || sch.Timezone != digestTimezone {
		t.Fatalf("cron/tz = %s/%s, want non-empty cron and %s", sch.CronExpression, sch.Timezone, digestTimezone)
	}
	if m, ok := parseDigestCronMinute(t, sch.CronExpression); !ok || m < 0 || m > 59 {
		t.Fatalf("cron %q: want minute 0–59 in hour %d", sch.CronExpression, digestHour)
	}
	// 首次创建即先跑一次: the fresh schedule's next_run_at was stamped to
	// "now" (FireNow) so the daemon's tick fires it within seconds — the
	// user does not wait for the next 09:00 boundary for the first batch.
	first, err := time.Parse(time.RFC3339Nano, sch.NextRunAt)
	if err != nil {
		t.Fatalf("next_run_at %q unparseable: %v", sch.NextRunAt, err)
	}
	if d := time.Since(first); d < -30*time.Second || d > time.Minute {
		t.Fatalf("fresh next_run_at = %s (Δ%s), want ~now (first run fired immediately)", sch.NextRunAt, d)
	}
	steward, err := agentSvc.GetSteward(context.Background())
	if err != nil || sch.AssigneeID != steward.ID {
		t.Fatalf("assignee = %q, want steward %q (err=%v)", sch.AssigneeID, steward.ID, err)
	}
	// Domain seeded as scratch.
	doms, err := domainSvc.List(context.Background())
	if err != nil || len(doms) != 1 || doms[0].Type != "scratch" || doms[0].Name != digestDomainName {
		t.Fatalf("domains = %+v (err=%v), want one scratch %q", doms, err, digestDomainName)
	}

	// Second call: no new rows, no touch (enabled=false survives).
	if _, err := schedSvc.SetEnabled(context.Background(), sch.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	before := sch.NextRunAt
	if err := SeedDigestSchedule(context.Background(), st, agentSvc, domainSvc, schedSvc); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	schs2, _ := schedSvc.List(context.Background())
	if len(schs2) != 1 {
		t.Fatalf("reseed created a duplicate schedule (%d rows)", len(schs2))
	}
	if schs2[0].Enabled {
		t.Fatalf("reseed re-enabled a user-disabled schedule")
	}
	// Idempotent reseed must NOT re-fire-now or otherwise touch next_run_at
	// (a restart would otherwise run the digest at every daemon boot,
	// daily schedule or not).
	if schs2[0].NextRunAt != before {
		t.Fatalf("reseed moved next_run_at: %s → %s — idempotent seed must not re-fire", before, schs2[0].NextRunAt)
	}
}

func TestSeedDigestScheduleSelfHealsMarkerLoss(t *testing.T) {
	st, agentSvc, domainSvc, schedSvc := digestSeedFixture(t, true)
	if err := agentSvc.SeedSteward(context.Background()); err != nil {
		t.Fatalf("seed steward: %v", err)
	}
	if err := SeedDigestSchedule(context.Background(), st, agentSvc, domainSvc, schedSvc); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Self-heal C: the marker is lost, the row survives → marker restored.
	if _, err := st.DB().Exec(`DELETE FROM app_settings WHERE key=?`, digestKeySchedule); err != nil {
		t.Fatalf("drop marker: %v", err)
	}
	if err := SeedDigestSchedule(context.Background(), st, agentSvc, domainSvc, schedSvc); err != nil {
		t.Fatalf("reseed after marker loss: %v", err)
	}
	schs, _ := schedSvc.List(context.Background())
	if len(schs) != 1 || !schs[0].BuiltIn {
		t.Fatalf("marker restore failed: %d schedules, built_in=%v", len(schs), len(schs) > 0 && schs[0].BuiltIn)
	}

	// Self-heal B: the row is gone behind the marker (raw DB delete — the
	// API guard would refuse, this simulates the out-of-band path) → rebuilt.
	first := schs[0]
	if _, err := st.DB().Exec(`DELETE FROM schedule_run WHERE schedule_id=?`, first.ID); err != nil {
		t.Fatalf("raw delete schedule_run: %v", err)
	}
	if _, err := st.DB().Exec(`DELETE FROM schedule WHERE id=?`, first.ID); err != nil {
		t.Fatalf("raw delete schedule: %v", err)
	}
	if err := SeedDigestSchedule(context.Background(), st, agentSvc, domainSvc, schedSvc); err != nil {
		t.Fatalf("reseed after row loss: %v", err)
	}
	schs2, _ := schedSvc.List(context.Background())
	if len(schs2) != 1 || schs2[0].ID == first.ID {
		t.Fatalf("row rebuild failed: %d schedules", len(schs2))
	}
}

func TestSeedDigestScheduleAbortsOnNameCollision(t *testing.T) {
	st, agentSvc, domainSvc, schedSvc := digestSeedFixture(t, true)
	if err := agentSvc.SeedSteward(context.Background()); err != nil {
		t.Fatalf("seed steward: %v", err)
	}
	// The user owns a repo domain with the digest name first.
	if _, err := domainSvc.Create(context.Background(), Domain{Name: digestDomainName, Type: "repo", GitURL: "https://example.com/x.git"}); err != nil {
		t.Fatalf("pre-create colliding domain: %v", err)
	}
	if err := SeedDigestSchedule(context.Background(), st, agentSvc, domainSvc, schedSvc); err != nil {
		t.Fatalf("seed with collision: %v", err)
	}
	// The user's domain wins; no schedule is created.
	doms, _ := domainSvc.List(context.Background())
	if len(doms) != 1 || doms[0].Type != "repo" {
		t.Fatalf("user domain clobbered: %+v", doms)
	}
	schs, _ := schedSvc.List(context.Background())
	if len(schs) != 0 {
		t.Fatalf("schedule created despite domain collision: %d", len(schs))
	}
}

func TestBuiltInGuardsBlockUpdateAndDelete(t *testing.T) {
	st, agentSvc, domainSvc, schedSvc := digestSeedFixture(t, true)
	if err := agentSvc.SeedSteward(context.Background()); err != nil {
		t.Fatalf("seed steward: %v", err)
	}
	if err := SeedDigestSchedule(context.Background(), st, agentSvc, domainSvc, schedSvc); err != nil {
		t.Fatalf("seed: %v", err)
	}
	schs, _ := schedSvc.List(context.Background())
	id := schs[0].ID

	// Identity edits stay rejected: a renamed or reassigned body is refused
	// even when it echoes the rest of the row.
	if _, err := schedSvc.Update(context.Background(), id, Schedule{Name: "renamed", TitleTemplate: "x", AssigneeID: id, DomainID: schs[0].DomainID, CronExpression: "* * * * *"}); err == nil {
		t.Fatalf("Update on built-in schedule was not rejected")
	}
	if err := schedSvc.Delete(context.Background(), id); err == nil {
		t.Fatalf("Delete on built-in schedule was not rejected")
	}

	// The execution time IS editable: a body that carries the row's identity
	// fields (empty = carry-forward also works) with a new cron passes, and
	// next_run_at is recomputed from the new cron.
	newCron := "0 23 * * *"
	updated, err := schedSvc.Update(context.Background(), id, Schedule{
		CronExpression: newCron,
		Timezone:       "Asia/Shanghai",
	})
	if err != nil {
		t.Fatalf("built-in cron edit rejected: %v", err)
	}
	if updated.CronExpression != newCron || updated.Name != digestScheduleName {
		t.Fatalf("cron edit mangled the row: %+v", updated)
	}
	next, err := time.Parse(time.RFC3339Nano, updated.NextRunAt)
	if err != nil {
		t.Fatalf("next_run_at %q unparseable: %v", updated.NextRunAt, err)
	}
	if time.Until(next) <= 0 {
		t.Fatalf("next_run_at %s not in the future — cron change did not recompute it", updated.NextRunAt)
	}

	// A non-built-in schedule is unaffected by the guard.
	other, err := schedSvc.Create(context.Background(), Schedule{
		Name: "user-task", TitleTemplate: "t", AssigneeType: "agent", AssigneeID: id,
		DomainID: schs[0].DomainID, CronExpression: "0 9 * * *",
	})
	if err != nil {
		// AssigneeID must be a real agent — use the steward's id.
		steward, gerr := agentSvc.GetSteward(context.Background())
		if gerr != nil {
			t.Fatalf("steward lookup: %v", gerr)
		}
		other, err = schedSvc.Create(context.Background(), Schedule{
			Name: "user-task", TitleTemplate: "t", AssigneeType: "agent", AssigneeID: steward.ID,
			DomainID: schs[0].DomainID, CronExpression: "0 9 * * *",
		})
		if err != nil {
			t.Fatalf("create user schedule: %v", err)
		}
	}
	if err := schedSvc.Delete(context.Background(), other.ID); err != nil {
		t.Fatalf("user schedule delete blocked: %v", err)
	}
}

// parseDigestCronMinute extracts the minute field from a seeded digest cron
// expression ("M H * * *") and checks the hour matches digestHour. Used to
// assert the per-instance random minute stays in the 07:00–07:59 window.
func parseDigestCronMinute(t *testing.T, cron string) (int, bool) {
	t.Helper()
	var min, hour int
	if _, err := fmt.Sscanf(cron, "%d %d * * *", &min, &hour); err != nil {
		return 0, false
	}
	if hour != digestHour {
		t.Fatalf("cron %q hour = %d, want %d", cron, hour, digestHour)
	}
	return min, true
}
