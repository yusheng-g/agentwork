package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// digestDaemonFixture builds a Daemon struct over a fresh store with the
// marker set to the given schedule id ('' = unset) — enough for the digest
// helpers, which only touch app_settings / goal / run / agent / runtime /
// machine.
func digestDaemonFixture(t *testing.T, markerScheduleID string) (*Daemon, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "aw.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	goalSvc := service.NewGoalService(st, bus)
	d := &Daemon{st: st, bus: bus, goalSvc: goalSvc}
	if markerScheduleID != "" {
		if _, err := st.DB().Exec(
			`INSERT INTO app_settings (key,value,updated_at) VALUES (?,?,'2026-01-01T00:00:00Z')`,
			service.DigestKeySchedule, `"`+markerScheduleID+`"`); err != nil {
			t.Fatalf("set marker: %v", err)
		}
	}
	return d, st
}

// insertDigestAgents plants machine+runtime+agent rows: steward first (when
// withSteward), then one standard agent — availability is controlled per
// row (machine connected / runtime status). Call once per test; per-row
// tweaks afterwards go through direct SQL (e.g. UPDATE runtime SET status).
func insertDigestAgents(t *testing.T, st *store.Store, withSteward, stewardAvailable, standardAvailable bool) {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339)
	mk := func(mid, status string) {
		if _, err := st.DB().Exec(
			`INSERT INTO machine (id,name,status,created_at) VALUES (?,?,?,?)`, mid, mid, status, ts); err != nil {
			t.Fatalf("insert machine: %v", err)
		}
	}
	if withSteward {
		mk("m-steward", map[bool]string{true: "connected", false: "offline"}[stewardAvailable])
		if _, err := st.DB().Exec(
			`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-steward','claude@m-steward','m-steward','[]','{}','active',?)`, ts); err != nil {
			t.Fatalf("insert runtime: %v", err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO agent (id,name,type,runtime_id,created_at) VALUES ('a-steward','AI SHELL','steward','rt-steward',?)`, ts); err != nil {
			t.Fatalf("insert steward: %v", err)
		}
	}
	mk("m-std", map[bool]string{true: "connected", false: "offline"}[standardAvailable])
	if _, err := st.DB().Exec(
		`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-std','claude@m-std','m-std','[]','{}','active',?)`, ts); err != nil {
		t.Fatalf("insert runtime: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO agent (id,name,type,runtime_id,created_at) VALUES ('a-std','worker','standard','rt-std',?)`, ts); err != nil {
		t.Fatalf("insert standard: %v", err)
	}
}

func TestPickDigestExecutorStewardFirst(t *testing.T) {
	d, st := digestDaemonFixture(t, "sched-1")
	insertDigestAgents(t, st, true, true, true)
	agentID, steward, ok := d.pickDigestExecutor(context.Background())
	if !ok || !steward || agentID != "a-steward" {
		t.Fatalf("pick = (%q,%v,%v), want steward a-steward", agentID, steward, ok)
	}
}

func TestPickDigestExecutorFallsBackToStandard(t *testing.T) {
	d, st := digestDaemonFixture(t, "sched-1")
	// Steward's machine offline → unavailable; standard available.
	insertDigestAgents(t, st, true, false, true)
	agentID, steward, ok := d.pickDigestExecutor(context.Background())
	if !ok || steward || agentID != "a-std" {
		t.Fatalf("pick = (%q,%v,%v), want fallback a-std", agentID, steward, ok)
	}
	// Steward runtime absent → same fallback (no new fixture rows).
	if _, err := st.DB().Exec(`UPDATE runtime SET status='absent' WHERE id='rt-steward'`); err != nil {
		t.Fatalf("absent steward runtime: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE machine SET status='connected' WHERE id='m-steward'`); err != nil {
		t.Fatalf("reconnect steward machine: %v", err)
	}
	agentID, steward, ok = d.pickDigestExecutor(context.Background())
	if !ok || steward || agentID != "a-std" {
		t.Fatalf("absent-runtime pick = (%q,%v,%v), want fallback a-std", agentID, steward, ok)
	}
}

func TestPickDigestExecutorNoneAvailable(t *testing.T) {
	d, st := digestDaemonFixture(t, "sched-1")
	insertDigestAgents(t, st, true, false, false)
	if _, _, ok := d.pickDigestExecutor(context.Background()); ok {
		t.Fatalf("pick succeeded, want ok=false when every machine is offline")
	}
}

// insertDigestGoal plants a system-created goal (the digest firing shape)
// plus its run. The executor agent must exist (run.agent_id FK) — a plain
// standard agent row is planted if none is present yet.
func insertDigestGoal(t *testing.T, st *store.Store, goalID, scheduleID, runID string) {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339)
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM agent WHERE id='a-steward'`).Scan(&n)
	if n == 0 {
		// run.agent_id → agent.runtime_id → runtime.machine_id are all FKs —
		// plant a minimal valid chain for the executor.
		if _, err := st.DB().Exec(
			`INSERT INTO machine (id,name,status,created_at) VALUES ('m-x','m-x','connected',?)`, ts); err != nil {
			t.Fatalf("insert machine: %v", err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-x','claude@m-x','m-x','[]','{}','active',?)`, ts); err != nil {
			t.Fatalf("insert runtime: %v", err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO agent (id,name,type,runtime_id,created_at) VALUES ('a-steward','AI SHELL','steward','rt-x',?)`, ts); err != nil {
			t.Fatalf("insert executor agent: %v", err)
		}
	}
	if _, err := st.DB().Exec(
		`INSERT INTO goal (id,title,domain_id,assignee_type,assignee_id,status,created_by_type,created_by_id,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		goalID, "digest goal", "dom-1", "agent", "a-steward", "active", "system", scheduleID, ts); err != nil {
		t.Fatalf("insert goal: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO run (id,goal_id,agent_id,status,role,attempt,queued_at,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		runID, goalID, "a-steward", "running", "owner", 1, ts, ts); err != nil {
		t.Fatalf("insert run: %v", err)
	}
}

func TestDigestGoalIdentification(t *testing.T) {
	d, st := digestDaemonFixture(t, "sched-1")
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO domain (id,type,name,created_at) VALUES ('dom-1','scratch','AI知识精选',?)`, ts); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	insertDigestGoal(t, st, "g-digest", "sched-1", "r-digest")
	insertDigestGoal(t, st, "g-other", "sched-other", "r-other")

	if got := d.digestGoalIDForGoal(context.Background(), "g-digest"); got != "g-digest" {
		t.Fatalf("digest goal not recognized: %q", got)
	}
	if got := d.digestGoalIDForGoal(context.Background(), "g-other"); got != "" {
		t.Fatalf("non-digest goal misrecognized: %q", got)
	}
	if got := d.digestGoalIDForRun(context.Background(), "r-digest"); got != "g-digest" {
		t.Fatalf("digest run not recognized: %q", got)
	}
	if got := d.digestGoalIDForRun(context.Background(), "r-other"); got != "" {
		t.Fatalf("non-digest run misrecognized: %q", got)
	}
}

func TestCollectDigestBatchWritesAndIsIdempotent(t *testing.T) {
	// Isolate HOME — digestRoot() lands in the temp home.
	t.Setenv("HOME", t.TempDir())
	d, st := digestDaemonFixture(t, "sched-1")
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().Exec(`INSERT INTO domain (id,type,name,created_at) VALUES ('dom-1','scratch','AI知识精选',?)`, ts); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	insertDigestGoal(t, st, "g-1", "sched-1", "r-1")

	manifest := []service.DigestManifestItem{
		{Title: "动态一", Summary: "摘要一", File: "1.md"},
		{Title: "动态二", Summary: "摘要二", File: "2.md"},
	}
	mb, _ := json.Marshal(manifest)
	artifacts := map[string]string{
		"manifest.json": string(mb),
		"1.md":          "# 动态一\n正文",
		"2.md":          "# 动态二\n正文",
	}
	d.collectDigestBatch(context.Background(), "g-1", artifacts)

	// articles.json has both entries, newest first, absolute paths.
	root := digestRoot()
	b, err := os.ReadFile(filepath.Join(root, "articles.json"))
	if err != nil {
		t.Fatalf("read articles.json: %v", err)
	}
	var articles []service.DigestArticle
	if err := json.Unmarshal(b, &articles); err != nil {
		t.Fatalf("parse articles.json: %v", err)
	}
	if len(articles) != 2 {
		t.Fatalf("articles = %d, want 2", len(articles))
	}
	for i, a := range articles {
		if a.Title == "" || a.Summary == "" || a.Path == "" || a.CreateTime == "" {
			t.Fatalf("entry %d incomplete: %+v", i, a)
		}
		if !filepath.IsAbs(a.Path) {
			t.Fatalf("entry %d path not absolute: %q", i, a.Path)
		}
		if _, err := os.Stat(a.Path); err != nil {
			t.Fatalf("entry %d file missing: %v", i, err)
		}
	}
	// Batch dir exists with the md files.
	if _, err := os.Stat(filepath.Join(root, "g-1", "1.md")); err != nil {
		t.Fatalf("batch file missing: %v", err)
	}

	// Idempotent re-collect: same goal → no duplicate entries.
	d.collectDigestBatch(context.Background(), "g-1", artifacts)
	b2, _ := os.ReadFile(filepath.Join(root, "articles.json"))
	var articles2 []service.DigestArticle
	_ = json.Unmarshal(b2, &articles2)
	if len(articles2) != 2 {
		t.Fatalf("re-collect duplicated entries: %d", len(articles2))
	}
}

func TestCollectDigestBatchCapsAndPrunes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _ := digestDaemonFixture(t, "sched-1")
	root := digestRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fill the json exactly to the cap: 199 single-article "old-solo"
	// entries plus a "g-a" entry that lands LAST — the next collect pushes
	// the json to cap+1 and evicts exactly the g-a entry, orphaning its
	// directory.
	stale := make([]service.DigestArticle, service.DigestMaxArticles-1)
	for i := range stale {
		stale[i] = service.DigestArticle{Title: "old", Path: filepath.Join(root, "old-solo", "1.md"), CreateTime: "2026-01-01T00:00:00Z"}
	}
	stale = append(stale, service.DigestArticle{Title: "a", Path: filepath.Join(root, "g-a", "1.md"), CreateTime: "2026-01-01T00:00:00Z"})
	if err := writeDigestArticles(root, stale); err != nil {
		t.Fatalf("seed stale json: %v", err)
	}
	for _, batch := range []string{"old-solo", "g-a"} {
		if err := os.MkdirAll(filepath.Join(root, batch), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, batch, "1.md"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d.collectDigestBatch(context.Background(), "g-new", map[string]string{
		"manifest.json": `[{"title":"新动态","summary":"新摘要","file":"1.md"}]`,
		"1.md":          "# 新动态",
	})
	b, _ := os.ReadFile(filepath.Join(root, "articles.json"))
	var articles []service.DigestArticle
	if err := json.Unmarshal(b, &articles); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(articles) != service.DigestMaxArticles {
		t.Fatalf("articles = %d, want capped %d", len(articles), service.DigestMaxArticles)
	}
	if articles[0].Title != "新动态" {
		t.Fatalf("newest-first violated: entry 0 = %+v", articles[0])
	}
	// The cap-evicted batch directory is pruned; the survivors stay.
	if _, err := os.Stat(filepath.Join(root, "g-a")); !os.IsNotExist(err) {
		t.Fatalf("evicted batch not pruned (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "old-solo")); err != nil {
		t.Fatalf("surviving batch wrongly pruned: %v", err)
	}
}

func TestCollectDigestBatchToleratesCorruptJson(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _ := digestDaemonFixture(t, "sched-1")
	root := digestRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "articles.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.collectDigestBatch(context.Background(), "g-1", map[string]string{
		"manifest.json": `[{"title":"t","summary":"s","file":"1.md"}]`,
		"1.md":          "content",
	})
	b, err := os.ReadFile(filepath.Join(root, "articles.json"))
	if err != nil {
		t.Fatalf("articles.json not rebuilt: %v", err)
	}
	var articles []service.DigestArticle
	if err := json.Unmarshal(b, &articles); err != nil || len(articles) != 1 {
		t.Fatalf("corrupt json not reset (err=%v, n=%d)", err, len(articles))
	}
	if _, err := os.Stat(filepath.Join(root, "articles.json.corrupt")); err != nil {
		t.Fatalf("corrupt backup missing: %v", err)
	}
}

func TestCollectDigestBatchWithoutManifestFallsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, _ := digestDaemonFixture(t, "sched-1")
	d.collectDigestBatch(context.Background(), "g-1", map[string]string{
		"1.md": "orphan content",
	})
	// No manifest → fixed-name fallback: the md file is collected with the
	// filename as the (empty metadata) entry.
	var articles []service.DigestArticle
	b, err := os.ReadFile(filepath.Join(digestRoot(), "articles.json"))
	if err != nil {
		t.Fatalf("no articles.json: %v", err)
	}
	_ = json.Unmarshal(b, &articles)
	if len(articles) != 1 {
		t.Fatalf("fallback articles = %d, want 1", len(articles))
	}
	if _, err := os.Stat(filepath.Join(digestRoot(), "g-1", "1.md")); err != nil {
		t.Fatalf("fallback file missing: %v", err)
	}
}

func TestParseDigestManifestRepairsUnescapedQuotes(t *testing.T) {
	// The production failure: titles carrying unescaped double quotes make
	// strict unmarshal fail; the repair pass must recover all metadata.
	raw := `[
      {"title": "OpenAI 发布 GPT-6 Astra：首个跨越网络安全"关键阈值"的模型", "summary": "一句话"摘要"", "file": "1.md"},
      {"title": "普通标题", "summary": "普通摘要", "file": "2.md"}
    ]`
	entries := parseDigestManifest(raw)
	if len(entries) != 2 {
		t.Fatalf("repaired entries = %d, want 2", len(entries))
	}
	if entries[0].Title != "OpenAI 发布 GPT-6 Astra：首个跨越网络安全\"关键阈值\"的模型" {
		t.Fatalf("title not recovered verbatim: %q", entries[0].Title)
	}
	if entries[0].Summary != "一句话\"摘要\"" {
		t.Fatalf("summary not recovered: %q", entries[0].Summary)
	}
	if entries[1].Title != "普通标题" || entries[1].File != "2.md" {
		t.Fatalf("plain entry damaged: %+v", entries[1])
	}
}

func TestParseDigestManifestStillRejectsHopelessJson(t *testing.T) {
	// Broken beyond quote repair (missing colon) → no entries, no panic.
	if entries := parseDigestManifest(`[{"title" "x" "file" "1.md"}]`); entries != nil {
		t.Fatalf("hopeless json parsed: %+v", entries)
	}
	if entries := parseDigestManifest(""); entries != nil {
		t.Fatalf("empty raw parsed: %+v", entries)
	}
}

func TestCollectDigestBatchBadManifestKeepsMetadatalessButNoManifestEntry(t *testing.T) {
	// An UNPARSEABLE manifest (the production incident) must not be treated
	// as an article: fallback collects only the fixed md names, titles
	// recovered from each md's H1.
	t.Setenv("HOME", t.TempDir())
	d, _ := digestDaemonFixture(t, "sched-1")
	d.collectDigestBatch(context.Background(), "g-1", map[string]string{
		"manifest.json": `[{"title":"broken" "file":"1.md"}]`, // missing colon
		"1.md":          "# 从H1恢复的标题\n正文",
		"2.md":          "无H1正文",
	})
	b, err := os.ReadFile(filepath.Join(digestRoot(), "articles.json"))
	if err != nil {
		t.Fatalf("articles.json missing: %v", err)
	}
	var articles []service.DigestArticle
	if err := json.Unmarshal(b, &articles); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(articles) != 2 {
		t.Fatalf("articles = %d, want 2 (manifest.json excluded)", len(articles))
	}
	if articles[0].Title != "从H1恢复的标题" {
		t.Fatalf("H1 title not recovered: %+v", articles[0])
	}
	if articles[1].Title != "" {
		t.Fatalf("md without H1 should have empty title: %+v", articles[1])
	}
}

// catchUpFixture builds a Daemon with schedSvc wired (digestCatchUpOnStartup
// calls schedSvc.FireNow) over a fresh store, seeds the marker + a schedule
// row whose cron fires at 00:00 daily (so now.After(scheduledToday) is always
// true — the test isolates the catch-up DECISION logic, not wall-clock timing).
// nextRunAt is the schedule row's next_run_at (tomorrow = "already fired
// today" / past-miss = "missed today").
func catchUpFixture(t *testing.T, nextRunAt string) (*Daemon, *store.Store) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "aw.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	schedSvc := service.NewScheduleService(st, bus)
	d := &Daemon{st: st, bus: bus, schedSvc: schedSvc}
	if _, err := st.DB().Exec(
		`INSERT INTO app_settings (key,value,updated_at) VALUES (?,?,'2026-01-01T00:00:00Z')`,
		service.DigestKeySchedule, `"sched-1"`); err != nil {
		t.Fatalf("set marker: %v", err)
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	// FK targets: agent + domain the schedule row references.
	if _, err := st.DB().Exec(
		`INSERT INTO machine (id,name,status,created_at) VALUES ('m-x','m-x','connected',?)`, ts); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO runtime (id,name,machine_id,args,env,status,created_at) VALUES ('rt-x','c@m-x','m-x','[]','{}','active',?)`, ts); err != nil {
		t.Fatalf("insert runtime: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO agent (id,name,type,runtime_id,created_at) VALUES ('a-steward','AI SHELL','steward','rt-x',?)`, ts); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO domain (id,type,name,created_at) VALUES ('dom-1','scratch','AI知识精选',?)`, ts); err != nil {
		t.Fatalf("insert domain: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO schedule (id,name,title_template,description,assignee_type,assignee_id,domain_id,cron_expression,timezone,enabled,next_run_at,last_run_at,created_at)
		 VALUES ('sched-1','每日AI知识精选','t','d','agent','a-steward','dom-1','0 0 * * *','Asia/Shanghai',1,?,'',?)`,
		nextRunAt, ts); err != nil {
		t.Fatalf("insert schedule: %v", err)
	}
	return d, st
}

// catchUpNextRun reads the schedule row's next_run_at back for assertion.
func catchUpNextRun(t *testing.T, st *store.Store) string {
	t.Helper()
	var v string
	if err := st.DB().QueryRow(`SELECT next_run_at FROM schedule WHERE id='sched-1'`).Scan(&v); err != nil {
		t.Fatalf("read next_run_at: %v", err)
	}
	return v
}

// TestDigestCatchUpFiresWhenMissedToday: next_run_at in the past (missed
// today), no articles, no in-flight goal → FireNow stamps next_run_at to ~now.
func TestDigestCatchUpFiresWhenMissedToday(t *testing.T) {
	pastMiss := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	d, st := catchUpFixture(t, pastMiss)
	before := catchUpNextRun(t, st)
	d.digestCatchUpOnStartup(context.Background())
	after := catchUpNextRun(t, st)
	if after == before {
		t.Fatalf("catch-up did not stamp next_run_at: before=%q after=%q (want ~now)", before, after)
	}
	// ~now: within a minute of now (FireNow stamps time.Now().UTC()).
	if na, _ := time.Parse(time.RFC3339Nano, after); time.Since(na) > time.Minute {
		t.Fatalf("stamped next_run_at %q is not ~now", after)
	}
}

// TestDigestCatchUpSkipsWhenArticleAlreadyToday: today's article present →
// do not fire (next_run_at untouched).
func TestDigestCatchUpSkipsWhenArticleAlreadyToday(t *testing.T) {
	tomorrow := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	d, st := catchUpFixture(t, tomorrow)
	root := digestRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	todayEntry := service.DigestArticle{
		Title: "今日动态", Summary: "s", Path: filepath.Join(root, "1.md"),
		CreateTime: time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, _ := json.Marshal([]service.DigestArticle{todayEntry})
	if err := os.WriteFile(filepath.Join(root, "articles.json"), b, 0o644); err != nil {
		t.Fatalf("write articles.json: %v", err)
	}
	before := catchUpNextRun(t, st)
	d.digestCatchUpOnStartup(context.Background())
	if after := catchUpNextRun(t, st); after != before {
		t.Fatalf("catch-up fired despite today's article: before=%q after=%q", before, after)
	}
}

// TestDigestCatchUpSkipsWhenInflightGoal: an active digest goal → do not fire.
func TestDigestCatchUpSkipsWhenInflightGoal(t *testing.T) {
	tomorrow := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	d, st := catchUpFixture(t, tomorrow)
	// catchUpFixture already planted agent a-steward; just add an active goal
	// created by the digest schedule (in-flight).
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB().Exec(
		`INSERT INTO goal (id,title,domain_id,assignee_type,assignee_id,status,created_by_type,created_by_id,created_at)
		 VALUES ('g-inflight','d','dom-1','agent','a-steward','active','system','sched-1',?)`, ts); err != nil {
		t.Fatalf("insert inflight goal: %v", err)
	}
	before := catchUpNextRun(t, st)
	d.digestCatchUpOnStartup(context.Background())
	if after := catchUpNextRun(t, st); after != before {
		t.Fatalf("catch-up fired despite inflight goal: before=%q after=%q", before, after)
	}
}

// TestDigestCatchUpSkipsWhenNotYetDue: scheduledToday is in the future → the
// normal tick should fire it; catch-up must not pre-fire. Uses a cron 23h59m
// ahead by setting hour far in the future relative to now.
func TestDigestCatchUpSkipsWhenNotYetDue(t *testing.T) {
	// Cron at 23:59 daily: if now is before 23:59, scheduledToday is today
	// 23:59 (future). If now is after 23:59, scheduledToday is tomorrow 23:59
	// (future). Either way not-yet-due when next_run_at is that future moment.
	futureNextRun := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	d, st := catchUpFixture(t, futureNextRun)
	// Rewrite cron to 23:59 to guarantee scheduledToday is in the future
	// regardless of when the test runs.
	if _, err := st.DB().Exec(`UPDATE schedule SET cron_expression='59 23 * * *' WHERE id='sched-1'`); err != nil {
		t.Fatalf("update cron: %v", err)
	}
	before := catchUpNextRun(t, st)
	d.digestCatchUpOnStartup(context.Background())
	if after := catchUpNextRun(t, st); after != before {
		t.Fatalf("catch-up fired before today's planned moment: before=%q after=%q", before, after)
	}
}

// TestDigestCatchUpSkipsWhenDisabled: enabled=0 → respect user's disable.
func TestDigestCatchUpSkipsWhenDisabled(t *testing.T) {
	pastMiss := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	d, st := catchUpFixture(t, pastMiss)
	if _, err := st.DB().Exec(`UPDATE schedule SET enabled=0 WHERE id='sched-1'`); err != nil {
		t.Fatalf("disable: %v", err)
	}
	before := catchUpNextRun(t, st)
	d.digestCatchUpOnStartup(context.Background())
	if after := catchUpNextRun(t, st); after != before {
		t.Fatalf("catch-up fired despite disabled schedule: before=%q after=%q", before, after)
	}
}
