package service

// The built-in "每日AI知识精选" digest schedule: seeded at daemon startup,
// fires daily within 07:00–07:59 (Asia/Shanghai, minute randomized per
// instance), and collects fresh AI-community news into md articles plus an
// articles.json index (the machine uploads them via run.finished artifacts;
// the daemon writes ~/.agentwork/digest/). The per-instance random minute
// spreads concurrent deployments across the hour so token-gateway rate limits
// are not hit all at once. Seeding is idempotent and self-healing: the marker
// lives in app_settings so the schedule can be recognized (built_in flag /
// edit-delete guards) across restarts, and dangling markers rebuild the row —
// the random minute is picked once at first create and never changed after.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/store"
)

const (
	// digestDomainName / digestScheduleName are the seeded row identities.
	// The domain is a scratch domain (no git repo): the fired goal's project
	// directory IS the deliverable — exactly what a doc-producing task needs.
	digestDomainName   = "AI知识精选"
	digestScheduleName = "每日AI知识精选"
	digestHour         = 7 // daily fire hour; minute randomized per instance
	digestDefaultCron  = "0 7 * * *" // fallback if random generation fails
	digestTimezone     = "Asia/Shanghai"
	digestTitleTpl     = "每日 AI 知识精选（自动收集）"
	// app_settings marker keys — the builtin.* prefix keeps them clear of
	// the M3 notify digest keys (notify.digest_*). The marker (not the name)
	// is the authority for built_in recognition and the edit/delete guards.
	digestKeySchedule = "builtin.digest.schedule_id"
	digestKeyDomain   = "builtin.digest.domain_id"
)

// Exported for the daemon side (marker reads, artifact manifest): the
// marker key authority and the fixed artifact filename list the dispatch
// carries (the machine uploads exactly these from the goal directory).
const (
	DigestKeySchedule = digestKeySchedule
)

// digestRandomCron picks a per-instance minute within the 07:00–07:59 window.
// Spreading deployments across the hour avoids every instance hitting the
// token gateway at the same instant. Called once at first create; the random
// minute is then persisted in the schedule row and never re-rolled. Falls
// back to digestDefaultCron ("0 7 * * *") if random generation fails.
func digestRandomCron() (cron string) {
	defer func() {
		if r := recover(); r != nil {
			logging.Warnf("seed digest: random cron panicked (%v) — falling back to %s", r, digestDefaultCron)
			cron = digestDefaultCron
		}
	}()
	cron = fmt.Sprintf("%d %d * * *", rand.IntN(60), digestHour)
	if cron == "" {
		return digestDefaultCron
	}
	return cron
}

// DigestArtifactFiles are the goal-directory files uploaded with
// run.finished. Fixed names — the executor's prompt mandates them.
var DigestArtifactFiles = []string{"manifest.json", "1.md", "2.md", "3.md", "4.md", "5.md"}

// DigestMaxArticles caps the collected-articles index so daily batches do
// not grow it without bound (oldest entries drop off).
const DigestMaxArticles = 50

// DigestArticle is one entry of the collected-articles index
// (~/.agentwork/digest/articles.json) — the schema the reading frontend
// consumes. createTime is stamped by the daemon at collection time, never
// by the executor.
type DigestArticle struct {
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	Path       string `json:"path"`
	CreateTime string `json:"createTime"`
}

// DigestManifestItem is one manifest.json entry — the executor agent's
// declared article list (file names are relative to the goal directory).
type DigestManifestItem struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	File    string `json:"file"`
}

// digestDescription is the fired goal's task instruction — it travels to the
// executor agent verbatim inside the goal description. Filenames are FIXED
// because the dispatch lists them as artifact_files: the machine reads
// exactly these names from the goal directory and uploads them with
// run.finished. createTime is NOT agent-produced — the daemon stamps it
// uniformly at collection time.
const digestDescription = `你是本次「每日 AI 知识精选」的 AI 知识编辑，必须亲自完成收集，不要委派给其他 agent（不要用 handoff，不要创建子目标）。

任务：
1. 用你可用的联网检索工具，搜集最近 24 小时 AI 领域的重要动态（大模型发布与更新、重要产品与开源项目、行业政策与融资、有影响力的研究与技术进展）。
2. 从结果中精选最多 5 条，宁缺毋滥：只收录有真实来源、值得关注的动态。
3. 在当前项目目录下，为每条动态写一篇文章，文件名固定为 1.md、2.md、3.md、4.md、5.md（有几条写几个文件）。每篇结构：
   # <动态标题>
   ## 摘要
   （两三句话概括）
   ## 正文
   （来龙去脉、关键信息、影响分析）
   ## 来源
   - [来源名称](链接)
4. 在当前项目目录写 manifest.json，内容为 JSON 数组（只列实际写出的文件）：
   [
     {"title": "动态标题", "summary": "一句话摘要", "file": "1.md"},
     ...
   ]
要求：全部用中文；内容只能来自检索到的真实信息，必须附来源链接；不要编造；不要改动以上文件名。

manifest.json 是严格 JSON，平台会程序化解析，必须一次写对：
- title 和 summary 里不要出现英文双引号 " —— 需要引用时用中文引号「」或“”；
- 不要输出 markdown 代码块围栏，文件内容以 [ 开头、以 ] 结尾；
- 写完后可用工具校验一次 JSON 合法性。`

// digestMarkerValue reads one builtin.digest.* app_settings value. The value
// is JSON-encoded ("\"<id>\"") to stay compatible with SettingsService
// writes; bare quotes-trim decoding matches how the daemon reads settings.
func digestMarkerValue(ctx context.Context, st *store.Store, key string) string {
	var v string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key=?`, key).Scan(&v); err != nil {
		return ""
	}
	return strings.Trim(v, `"`)
}

func setDigestMarker(ctx context.Context, st *store.Store, key, id string) {
	b, _ := json.Marshal(id)
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO app_settings (key,value,updated_at) VALUES (?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, string(b), now()); err != nil {
		logging.Warnf("seed digest: set %s: %v", key, err)
	}
}

// SeedDigestSchedule ensures the built-in digest domain + schedule exist.
// Called at daemon startup (after SeedSteward) and again when a machine
// registers with a steward-capable CLI (the first startup may have had no
// active runtime and skipped). Idempotent: an existing marker-backed row is
// never touched (a user-disabled schedule must not be re-enabled by a
// restart). Self-heals the two dangling cases (marker without row; row
// without marker). A domain-name collision with a non-scratch domain is NOT
// taken over — the user's domain wins, seeding aborts with a warning.
func SeedDigestSchedule(ctx context.Context, st *store.Store, agentSvc *AgentService, domainSvc *DomainService, schedSvc *ScheduleService) error {
	// Self-heal A: no steward yet (first startup with no active runtime).
	// SeedStewardForRuntime re-seeds the steward on machine registration and
	// re-runs this function afterwards.
	steward, err := agentSvc.GetSteward(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			logging.Infof("seed digest: no steward agent yet — skipped (retried on machine registration)")
			return nil
		}
		return err
	}

	// Domain: marker first, then name, then create.
	domain := s_digestDomain(ctx, st, domainSvc)
	if domain == nil {
		return nil // name taken by a non-scratch domain — seeding aborted
	}

	// Schedule idempotency: a live marker-backed row means done.
	if id := digestMarkerValue(ctx, st, digestKeySchedule); id != "" {
		if _, err := schedSvc.Get(ctx, id); err == nil {
			return nil
		}
		// Self-heal B: marker without a row (the row was deleted behind the
		// guards, e.g. a raw DB delete) — clear the marker and rebuild.
		clearDigestMarker(ctx, st, digestKeySchedule)
	}

	// Self-heal C: the row exists by name but the marker was lost.
	existing, err := findScheduleByName(ctx, schedSvc, digestScheduleName)
	if err != nil {
		return err
	}
	if existing != nil {
		setDigestMarker(ctx, st, digestKeySchedule, existing.ID)
		return nil
	}

	sch, err := schedSvc.Create(ctx, Schedule{
		Name:           digestScheduleName,
		TitleTemplate:  digestTitleTpl,
		Description:    digestDescription,
		AssigneeType:   "agent",
		AssigneeID:     steward.ID,
		DomainID:       domain.ID,
		CronExpression: digestRandomCron(),
		Timezone:       digestTimezone,
	})
	if err != nil {
		// A same-name row may have raced us — treat it as self-heal C.
		if race, rerr := findScheduleByName(ctx, schedSvc, digestScheduleName); rerr == nil && race != nil {
			setDigestMarker(ctx, st, digestKeySchedule, race.ID)
			return nil
		}
		return fmt.Errorf("create digest schedule: %w", err)
	}
	setDigestMarker(ctx, st, digestKeySchedule, sch.ID)
	// 首次创建即先跑一次：stamp next_run_at to now so the daemon's schedule
	// tick fires the digest within seconds of the seed — the user does not
	// wait for the next 09:00 boundary. fireSchedule re-derives the
	// next_run_at from the cron afterwards, so the daily cadence is intact.
	if err := schedSvc.FireNow(ctx, sch.ID); err != nil {
		// Best-effort: the schedule still fires at its first cron boundary.
		logging.Warnf("seed digest: fire-now %s: %v", sch.ID, err)
	}
	logging.Infof("seed digest: schedule %q created (%s, cron %s %s) — first run dispatched now", digestScheduleName, sch.ID, sch.CronExpression, digestTimezone)
	return nil
}

// s_digestDomain resolves (and if needed creates) the digest scratch domain.
// Returns nil when seeding must abort (a non-scratch domain owns the name).
func s_digestDomain(ctx context.Context, st *store.Store, domainSvc *DomainService) *Domain {
	// Marker hit: the row must still exist and still be scratch.
	if id := digestMarkerValue(ctx, st, digestKeyDomain); id != "" {
		if d, err := domainSvc.Get(ctx, id); err == nil && d.Type == "scratch" {
			return d
		}
		clearDigestMarker(ctx, st, digestKeyDomain)
	}
	// By name.
	rows, err := st.DB().QueryContext(ctx, `SELECT id, type FROM domain WHERE name=?`, digestDomainName)
	if err != nil {
		logging.Warnf("seed digest: lookup domain: %v", err)
		return nil
	}
	var foundID, foundType string
	for rows.Next() {
		if err := rows.Scan(&foundID, &foundType); err != nil {
			rows.Close()
			return nil
		}
	}
	rows.Close()
	if foundID != "" {
		if foundType != "scratch" {
			logging.Warnf("seed digest: domain %q exists with type %q (not scratch) — seeding aborted, the user's domain wins", digestDomainName, foundType)
			return nil
		}
		setDigestMarker(ctx, st, digestKeyDomain, foundID)
		return &Domain{ID: foundID}
	}
	// Create the scratch domain. git_url is meaningless for scratch.
	d, err := domainSvc.Create(ctx, Domain{
		Name: digestDomainName,
		Type: "scratch",
	})
	if err != nil {
		// A create race with the same name → re-lookup once.
		rows, lerr := st.DB().QueryContext(ctx, `SELECT id, type FROM domain WHERE name=?`, digestDomainName)
		if lerr == nil {
			for rows.Next() {
				_ = rows.Scan(&foundID, &foundType)
			}
			rows.Close()
		}
		if foundID != "" && foundType == "scratch" {
			setDigestMarker(ctx, st, digestKeyDomain, foundID)
			return &Domain{ID: foundID}
		}
		logging.Warnf("seed digest: create domain: %v", err)
		return nil
	}
	setDigestMarker(ctx, st, digestKeyDomain, d.ID)
	return d
}

func clearDigestMarker(ctx context.Context, st *store.Store, key string) {
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM app_settings WHERE key=?`, key); err != nil {
		logging.Warnf("seed digest: clear %s: %v", key, err)
	}
}

func findScheduleByName(ctx context.Context, schedSvc *ScheduleService, name string) (*Schedule, error) {
	list, err := schedSvc.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}
