package daemon

// The built-in digest schedule's runtime hooks: executor picking at fire
// time, artifact collection at run finish, auto-approval of the review
// checkpoint, and startup sweeps. Named *_digest_* / digest* (never
// dispatchDigest — the M3 notify digest owns that name).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/service"
)

// digestMu serializes articles.json read-modify-writes across concurrent
// run finishes (two digest runs can never overlap — single schedule — but a
// retried report and the startup sweep can race one).
var digestMu sync.Mutex

// ── marker helpers ──

// digestScheduleID returns the built-in schedule's id ('' = not seeded).
func (d *Daemon) digestScheduleID(ctx context.Context) string {
	var v string
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key=?`, service.DigestKeySchedule).Scan(&v); err != nil {
		return ""
	}
	return trimQuotes(v)
}

func (d *Daemon) isDigestScheduleID(ctx context.Context, scheduleID string) bool {
	id := d.digestScheduleID(ctx)
	return id != "" && id == scheduleID
}

func trimQuotes(s string) string {
	for len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return s
}

// isDigestGoal reports whether the goal was fired by the built-in schedule.
// The goal row may be deleted behind this check (the caller's event lags the
// cascade) — any read error is a plain no.
func (d *Daemon) isDigestGoal(ctx context.Context, goalID string) bool {
	return d.digestGoalIDForGoal(ctx, goalID) != ""
}

// digestGoalIDForGoal returns goalID if the goal is a digest firing, else ''.
func (d *Daemon) digestGoalIDForGoal(ctx context.Context, goalID string) string {
	var createdByType, createdByID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT created_by_type, COALESCE(created_by_id,'') FROM goal WHERE id=?`, goalID).
		Scan(&createdByType, &createdByID)
	if err != nil || createdByType != "system" || createdByID == "" {
		return ""
	}
	id := d.digestScheduleID(ctx)
	if id != "" && id == createdByID {
		return goalID
	}
	return ""
}

// digestGoalIDForRun resolves a finished run's goal and returns it when it
// is a digest firing ('' = not digest / no goal).
func (d *Daemon) digestGoalIDForRun(ctx context.Context, runID string) string {
	var goalID, createdByType, createdByID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.goal_id, g.created_by_type, COALESCE(g.created_by_id,'')
		 FROM run r JOIN goal g ON g.id = r.goal_id WHERE r.id=?`, runID).
		Scan(&goalID, &createdByType, &createdByID)
	if err != nil || createdByType != "system" || createdByID == "" {
		return ""
	}
	id := d.digestScheduleID(ctx)
	if id != "" && id == createdByID {
		return goalID
	}
	return ""
}

// ── executor picking (fire time) ──

// digestAgentAvailability is the WHERE fragment shared by the steward-first
// and fallback executor queries. Same gate the claim path uses (run.Claim):
// the runtime must still be probed (not absent) and its machine connected —
// a machine-less runtime can never execute.
const digestAgentAvailability = `
  JOIN runtime rt ON rt.id = a.runtime_id
  JOIN machine m ON m.id = rt.machine_id
  WHERE a.type = ? AND rt.status != 'absent' AND m.status = 'connected'`

// pickDigestExecutor chooses who runs this digest firing: the steward
// (AI SHELL) when its runtime+machine are available, else the first
// standard agent that is. ok=false = nothing available this tick — the
// firing is skipped (next_run_at still advances; the next cron boundary
// retries).
func (d *Daemon) pickDigestExecutor(ctx context.Context) (agentID string, steward bool, ok bool) {
	// Steward first.
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT a.id FROM agent a`+digestAgentAvailability+` LIMIT 1`, "steward").Scan(&agentID); err == nil {
		return agentID, true, true
	}
	// Fallback: first standard agent, oldest first (stable choice).
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT a.id FROM agent a`+digestAgentAvailability+` ORDER BY a.created_at LIMIT 1`, "standard").Scan(&agentID); err == nil {
		return agentID, false, true
	}
	return "", false, false
}

// maybePickDigestExecutor returns the assignee a digest firing should use.
// Non-digest schedules pass through untouched (their assignee is explicit).
// ok=false = the digest firing must be skipped entirely (no agent available).
func (d *Daemon) maybePickDigestExecutor(ctx context.Context, scheduleID, assigneeType, assigneeID string) (string, string, bool) {
	if assigneeType != "agent" || !d.isDigestScheduleID(ctx, scheduleID) {
		return assigneeType, assigneeID, true
	}
	picked, steward, ok := d.pickDigestExecutor(ctx)
	if !ok {
		return assigneeType, assigneeID, false
	}
	if !steward {
		logging.Infof("daemon: digest schedule %s: steward unavailable — falling back to agent %s", scheduleID, picked)
	}
	return assigneeType, picked, true
}

// ── collection (run finish) ──

// digestRoot is where collected articles land: ~/.agentwork/digest/. The
// daemon host's filesystem is what the reading frontend can reach (the
// artifacts arrive over the link — the sandbox is opaque to us).
func digestRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agentwork-digest")
	}
	return filepath.Join(home, ".agentwork", "digest")
}

// collectDigestBatch lands one firing's uploaded artifacts into the stable
// digest directory and prepends its entries to articles.json. Idempotent
// per goal: a batch already in the json is not appended twice (a retried
// run.finished re-enters here). Best-effort: a manifestless or partial
// batch collects what exists and never blocks the goal's auto-approval.
func (d *Daemon) collectDigestBatch(ctx context.Context, goalID string, artifacts map[string]string) {
	digestMu.Lock()
	defer digestMu.Unlock()

	root := digestRoot()
	entries := parseDigestManifest(artifacts["manifest.json"])
	// No parseable manifest → nothing indexable, but the md files may still
	// be there. Fall back to whatever artifacts arrived under the fixed
	// article names (manifest.json itself is never an article) so the batch
	// is not lost entirely; titles are recovered from each md's H1 line.
	if len(entries) == 0 {
		logging.Warnf("digest: goal %s: manifest.json missing or unparseable — collecting files without metadata", goalID)
		for _, name := range service.DigestArtifactFiles {
			if name == "manifest.json" {
				continue
			}
			content, ok := artifacts[name]
			if !ok || content == "" {
				continue
			}
			entries = append(entries, service.DigestManifestItem{File: name, Title: digestH1Title(content)})
		}
		if len(entries) == 0 {
			logging.Warnf("digest: goal %s: no artifacts to collect", goalID)
			return
		}
	}

	batchDir := filepath.Join(root, goalID)
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		logging.Warnf("digest: goal %s: mkdir %s: %v", goalID, batchDir, err)
		return
	}

	articles, err := readDigestArticles(root)
	if err != nil {
		logging.Warnf("digest: articles.json unreadable (%v) — resetting", err)
		articles = nil
	}
	// Idempotency: this goal's batch already collected → skip the append
	// (the files below are rewritten in place, same content).
	for _, a := range articles {
		if filepath.Base(filepath.Dir(a.Path)) == goalID {
			logging.Infof("digest: goal %s batch already collected — skipping append", goalID)
			return
		}
	}

	createTime := time.Now().UTC().Format(time.RFC3339Nano)
	fresh := make([]service.DigestArticle, 0, len(entries))
	for _, m := range entries {
		content, ok := artifacts[m.File]
		if !ok || content == "" {
			logging.Warnf("digest: goal %s: manifest lists %q but no artifact arrived — skipped", goalID, m.File)
			continue
		}
		if filepath.Base(filepath.Clean(m.File)) != m.File {
			logging.Warnf("digest: goal %s: manifest file %q is not a plain filename — skipped", goalID, m.File)
			continue
		}
		if err := os.WriteFile(filepath.Join(batchDir, m.File), []byte(content), 0o644); err != nil {
			logging.Warnf("digest: goal %s: write %s: %v", goalID, m.File, err)
			continue
		}
		fresh = append(fresh, service.DigestArticle{
			Title:      m.Title,
			Summary:    m.Summary,
			Path:       filepath.Join(batchDir, m.File),
			CreateTime: createTime,
		})
	}
	if len(fresh) == 0 {
		logging.Warnf("digest: goal %s: no article collected (manifest pointed at missing artifacts)", goalID)
		return
	}

	// Prepend (newest first) and cap.
	updated := append(fresh, articles...)
	if len(updated) > service.DigestMaxArticles {
		updated = updated[:service.DigestMaxArticles]
	}
	if err := writeDigestArticles(root, updated); err != nil {
		logging.Warnf("digest: goal %s: write articles.json: %v", goalID, err)
		return
	}
	logging.Infof("digest: goal %s: %d article(s) collected → %s", goalID, len(fresh), filepath.Join(root, "articles.json"))

	// Prune batch directories no longer referenced by the json (old
	// articles are disposable; the user said so) — including this batch's
	// predecessors dropped off the cap.
	digestPruneOrphanBatches(root, updated)

	// The artifacts are collected — the scratch goal dir is disposable.
	// Best-effort (the goal delete cascade would clean it too, but this
	// keeps runs/scratch from growing between fires).
	d.pruneDigestScratchDir(ctx, goalID)
}

// parseDigestManifest decodes the executor's manifest.json. A malformed
// manifest first goes through repairDigestJson (unescaped quotes inside
// string values — the one mistake the executor model actually makes) and is
// retried before giving up; a still-unparseable manifest yields no entries
// (the caller falls back / warns).
func parseDigestManifest(raw string) []service.DigestManifestItem {
	if raw == "" {
		return nil
	}
	var metas []service.DigestManifestItem
	if err := json.Unmarshal([]byte(raw), &metas); err != nil {
		// Agent-written manifests occasionally carry unescaped double quotes
		// inside titles (中文引号习惯漂移成英文引号) — one repair pass
		// before falling back to the metadata-less collection path.
		repaired, rerr := repairDigestJson(raw)
		if rerr == nil && json.Unmarshal(repaired, &metas) == nil {
			logging.Infof("digest: manifest.json parsed after quote repair (%d item(s))", len(metas))
		} else {
			logging.Warnf("digest: manifest.json unparseable: %v", err)
			return nil
		}
	}
	out := metas[:0]
	for _, m := range metas {
		if m.File == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// repairDigestJson re-escapes double quotes that appear INSIDE string values
// (after a value has started, before its structural closing quote) and that
// are not already escaped. Walks the raw bytes as a state machine so
// structural quotes are left untouched; the first structural error wins —
// anything the pass can't make valid is returned unchanged. E.g.
//   {"title":"跨越"关键阈值"的模型","file":"1.md"}
// becomes
//   {"title":"跨越\"关键阈值\"的模型","file":"1.md"}
func repairDigestJson(raw string) ([]byte, error) {
	// Breakpoints between structural tokens: quote ends a string, colon
	// follows an object key, comma/brackets separate items. A quote preceded
	// by one of these OPENED a value (structural); a quote followed by one
	// of these CLOSED it (structural); anything else is an interior quote
	// needing an escape.
	const breaks = `,:[]{}`
	var b strings.Builder
	b.Grow(len(raw) + 8)
	inStr := false
	esc := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inStr {
			b.WriteByte(c)
			if c == '"' {
				inStr = true
				esc = false
			}
			continue
		}
		// Inside a string.
		if esc {
			b.WriteByte(c)
			esc = false
			continue
		}
		switch c {
		case '\\':
			b.WriteByte(c)
			esc = true
		case '"':
			// Structural close iff the next significant byte is a break —
			// anything else is interior (escape it).
			j := i + 1
			for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t' || raw[j] == '\n' || raw[j] == '\r') {
				j++
			}
			if j < len(raw) && strings.IndexByte(breaks, raw[j]) >= 0 {
				inStr = false
				b.WriteByte(c)
			} else {
				b.WriteByte('\\')
				b.WriteByte('"')
			}
		default:
			b.WriteByte(c)
		}
	}
	out := []byte(b.String())
	if !json.Valid(out) {
		return nil, errors.New("repair left invalid json")
	}
	return out, nil
}

// digestH1Title extracts the first markdown H1 line ("# 标题") from an
// article body for the metadata-less fallback path; '' when there is none.
func digestH1Title(content string) string {
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(t, "# "))
		}
	}
	return ""
}

// readDigestArticles loads articles.json; a missing file is a fresh start.
func readDigestArticles(root string) ([]service.DigestArticle, error) {
	b, err := os.ReadFile(filepath.Join(root, "articles.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var articles []service.DigestArticle
	if err := json.Unmarshal(b, &articles); err != nil {
		// Corrupt file: keep a forensic copy, start over.
		_ = os.Rename(filepath.Join(root, "articles.json"), filepath.Join(root, "articles.json.corrupt"))
		return nil, err
	}
	return articles, nil
}

// writeDigestArticles atomically replaces articles.json (temp + rename).
func writeDigestArticles(root string, articles []service.DigestArticle) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(articles, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(root, "articles.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(root, "articles.json"))
}

// digestPruneOrphanBatches removes batch directories the json no longer
// references (cap-evicted batches; the current batch stays).
func digestPruneOrphanBatches(root string, articles []service.DigestArticle) {
	live := map[string]bool{}
	for _, a := range articles {
		live[filepath.Base(filepath.Dir(a.Path))] = true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !live[e.Name()] {
			dir := filepath.Join(root, e.Name())
			if err := os.RemoveAll(dir); err != nil {
				logging.Infof("digest: prune batch %s: %v", dir, err)
			}
		}
	}
}

// pruneDigestScratchDir removes the fired goal's scratch project directory
// (the artifacts are collected; the deliverable is the digest dir now).
// The domain name is read from the goal row — never derived from constants.
func (d *Daemon) pruneDigestScratchDir(ctx context.Context, goalID string) {
	var domainName string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT d.name FROM goal g JOIN domain d ON d.id = g.domain_id WHERE g.id=?`, goalID).Scan(&domainName)
	if err != nil {
		return
	}
	if domainName == "" {
		return
	}
	dir := service.ScratchGoalDir(domainName, goalID)
	if err := os.RemoveAll(dir); err != nil {
		logging.Infof("digest: remove scratch goal dir %s: %v", dir, err)
	}
}

// ── auto-approval (the scratch human checkpoint) ──

// approveDigestGoal closes the digest goal's mandatory review checkpoint.
// The reconcile that parks the goal into review runs inside runSvc.Finish
// (invariant 13) — the caller invokes this AFTER Finish, but the park lands
// asynchronously enough (reconcile → publish → bus goroutine) that a short
// poll covers the gap. A duplicate approve (crash after the decision row,
// before deliver) is a success, not an error.
func (d *Daemon) approveDigestGoal(ctx context.Context, goalID, runID string) {
	var lastErr error
	for i := 0; i < 5; i++ {
		_, err := d.goalSvc.ResolveReview(ctx, goalID, runID, "approve", "内置任务自动验收", "system")
		if err == nil {
			logging.Infof("digest: goal %s auto-approved", goalID)
			return
		}
		lastErr = err
		// Not in review YET is retryable; other coded errors (duplicate
		// approve on the same run) are terminal-but-fine.
		if !service.IsRetryableReviewWait(err) {
			logging.Infof("digest: goal %s auto-approve: %v", goalID, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	logging.Warnf("digest: goal %s auto-approve gave up: %v", goalID, lastErr)
}

// sweepStuckDigestGoals closes the crash window between Finish (collection
// done) and the auto-approve: a digest goal parked in review with no
// approve decision gets approved at startup. Collected batches never wait
// for a human who was never meant to see this checkpoint.
func (d *Daemon) sweepStuckDigestGoals(ctx context.Context) {
	marker := d.digestScheduleID(ctx)
	if marker == "" {
		return
	}
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT g.id FROM goal g
		 WHERE g.status='review' AND g.created_by_type='system' AND g.created_by_id=?
		   AND NOT EXISTS (SELECT 1 FROM gate_decision gd WHERE gd.goal_id=g.id AND gd.decision='approve')`, marker)
	if err != nil {
		logging.Infof("digest: stuck-goal sweep: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		// The run id is whatever the goal's last run was — ResolveReview
		// stamps the decision on it; the duplicate-decision guard keys on
		// (goal, run) so any of the goal's runs works for a fresh park.
		var runID string
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT id FROM run WHERE goal_id=? ORDER BY queued_at DESC LIMIT 1`, id).Scan(&runID)
		if runID == "" {
			continue
		}
		d.approveDigestGoal(ctx, id, runID)
	}
	logging.Infof("digest: swept %d stuck review goal(s)", len(ids))
}

// digestCatchUpOnStartup fires the daily digest once when the machine (which
// is not on 24/7) boots after missing today's planned fire moment and no
// articles were collected today. This closes the "powered off across the fire
// window → dispatchSchedules skips it as a miss → the day silently produces
// nothing" gap. The fire is the SAME mechanism first-seeding uses (FireNow
// stamps next_run_at=now; the 5s schedule tick then runs fireSchedule, which
// advances next_run_at back to tomorrow on its own). Idempotent across
// restarts within a day: once today's articles land, condition 5 blocks the
// next restart from firing again.
func (d *Daemon) digestCatchUpOnStartup(ctx context.Context) {
	schedID := d.digestScheduleID(ctx)
	if schedID == "" {
		return
	}
	var cronExpr, tz, nextRunAt string
	var enabled int
	if err := d.st.DB().QueryRowContext(ctx,
		`SELECT cron_expression, timezone, next_run_at, enabled FROM schedule WHERE id=?`,
		schedID).Scan(&cronExpr, &tz, &nextRunAt, &enabled); err != nil {
		logging.Infof("digest catch-up: read schedule %s: %v", schedID, err)
		return
	}
	if enabled == 0 {
		return // user disabled — respect it
	}
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		logging.Infof("digest catch-up: timezone %q: %v", tz, err)
		return
	}
	now := time.Now().In(loc)
	// Today's planned moment is derived from the cron_expression (which carries
	// the per-instance random minute rolled at first seed). ComputeNextRun is
	// strictly-AFTER its anchor, so anchor at one second before today's 00:00 —
	// the next cron hit after that is today's 07:XX (not tomorrow's). Stable
	// regardless of whether today already fired.
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	scheduledToday, err := service.ComputeNextRun(cronExpr, tz, startOfToday.Add(-time.Second))
	if err != nil {
		logging.Infof("digest catch-up: compute today's moment: %v", err)
		return
	}
	if !now.After(scheduledToday) {
		return // not yet due today — let the normal tick fire at the planned moment
	}
	// The schedule_fireTTL/miss-grace window: if SeedDigestSchedule.FireNow
	// just stamped next_run_at to now (first-seed path), a duplicate fire here
	// would race it. Skip while next_run_at sits inside now±1min.
	if nextRunAt != "" {
		if nr, perr := time.Parse(time.RFC3339Nano, nextRunAt); perr == nil {
			if diff := time.Since(nr); diff < time.Minute && diff > -time.Minute {
				return
			}
		}
	}
	// Today's articles: the latest articles.json entry whose createTime falls
	// on today (in the schedule timezone) means the day already produced —
	// skip. A fire that produced no articles (agent failed / no executor) does
	// NOT write a today entry, so this stays satisfiable and the catch-up
	// retries on the next restart.
	if d.digestHasTodayArticle(ctx, loc, now) {
		return
	}
	// In-flight digest goal: a fired run still executing means today's work is
	// not done but is in progress — a second fire would clone a parallel goal.
	// Wait for the in-flight one to finish (it then either writes today's
	// article → condition above blocks, or fails → next restart retries).
	if d.digestHasInflightGoal(ctx, schedID) {
		return
	}
	if err := d.schedSvc.FireNow(ctx, schedID); err != nil {
		logging.Warnf("digest catch-up: fire-now %s: %v", schedID, err)
		return
	}
	logging.Infof("digest catch-up: machine missed today's %s window — firing now (next_run_at stamped to now; schedule tick will dispatch)",
		scheduledToday.Format("15:04 MST"))
}

// digestHasTodayArticle reports whether articles.json's newest entry was
// collected today (schedule timezone). The createTime is stamped by
// collectDigestBatch at write time in UTC RFC3339Nano; a missing/empty file
// means no batch yet → false.
func (d *Daemon) digestHasTodayArticle(ctx context.Context, loc *time.Location, now time.Time) bool {
	articles, err := readDigestArticles(digestRoot())
	if err != nil || len(articles) == 0 {
		return false
	}
	today := now.Format("2006-01-02")
	// Newest-first ordering is maintained by collectDigestBatch's prepend; the
	// first entry is the most recent. A today-dated newest entry ⟺ today has
	// produced. (Older today entries behind a non-today newest cannot happen —
	// prepend keeps today ahead of any older day.)
	for _, a := range articles {
		if a.CreateTime == "" {
			continue
		}
		t, perr := time.Parse(time.RFC3339Nano, a.CreateTime)
		if perr != nil {
			continue
		}
		return t.In(loc).Format("2006-01-02") == today
	}
	return false
}

// digestHasInflightGoal reports whether any digest goal is still executing
// today (status active or review). review is included because the auto-approve
// is async to Finish — a review-parked goal is still "today's run in flight"
// until approveDigestGoal closes it.
func (d *Daemon) digestHasInflightGoal(ctx context.Context, schedID string) bool {
	var n int
	_ = d.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal
		 WHERE created_by_type='system' AND created_by_id=?
		   AND status IN ('active','review')`, schedID).Scan(&n)
	return n > 0
}
