package service

// The team-import one-click flow (Path D, task-based): a team-definition repo
// is imported by upgrading the import to a WORKER run backed by a GOAL — so it
// appears in the task bar, auto-approves to done (digest-style), and the agent
// clones the repo itself (credentials ride env, never the prompt). The
// team_import row stays the temporary git-config + status tracker, back-filled
// with the goal's first run id.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/gitutil"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/store"
)

// importDomainName / importKeyDomain are the seeded scratch import domain's
// identity + app_settings marker (mirrors the digest scratch domain pattern in
// digest_seed.go). The domain is scratch (no git repo): the agent clones the
// IMPORT repo into a repo/ subdir itself; the platform never touches the
// domain's git. The marker (not the name) is the authority for built-in
// recognition.
const (
	importDomainName = "团队导入"
	importKeyDomain  = "builtin.teamimport.domain_id"
	// ImportCreatedByID is the stable creator id stamped on every import goal
	// (created_by_type='system'). The daemon recognizes import goals by this
	// pair — no schedule lookup needed (unlike digest, whose created_by_id is
	// the schedule id read from app_settings).
	ImportCreatedByID = "team_import"
	// importGoalTitle is the title of every import goal.
	importGoalTitle = "团队导入"
)

// TeamImport is a TEMPORARY row tracking a team-definition-repo import
// processor run: the run that clones a team repo, has the steward agent
// explore it, and produces team.json. The platform reads team.json and
// upserts the entities by name. git_url/credentials/branch persist from
// the HTTP request to daemon dispatch (the run may sit queued for
// seconds/minutes before claim). ImportTeam cleans up old completed/failed
// rows at the start of each import — the table holds at most one pending +
// zero/one just-finished row.
type TeamImport struct {
	ID              string `json:"id"`
	RunID           string `json:"run_id"`
	GitURL          string `json:"git_url"`
	GitCredentials  string `json:"git_credentials"`
	DefaultBranch   string `json:"default_branch"`
	Status          string `json:"status"` // pending|completed|failed
	Result          string `json:"result"` // JSON summary
	CreatedAt       string `json:"created_at"`
}

// teamJSON is the structured artifact the import agent produces (team.json).
// The agent explores the team repo freely and maps whatever format it finds
// into this schema.
type teamJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Skills      []struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Files       map[string]string `json:"files"`
	} `json:"skills"`
	Agents []struct {
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		SystemPrompt string   `json:"system_prompt"`
		Skills       []string `json:"skills"`
		Role         string   `json:"role"`    // leader|reviewer|member
		Runtime      string   `json:"runtime"` // runtime name (e.g. "claude@laptop") — steward decides; empty = platform random-assigns
	} `json:"agents"`
	Squad *struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Leader       string `json:"leader"`
		Instructions string `json:"instructions"`
		Members      []struct {
			Agent string `json:"agent"`
			Role  string `json:"role"` // reviewer|""
		} `json:"members"`
	} `json:"squad"`
}

// TeamImportService owns the team-import lifecycle: creating the import goal +
// first worker run, and ingesting the agent's team.json artifact into
// agent/squad/skill rows.
type TeamImportService struct {
	st        *store.Store
	bus       *events.Bus
	runSvc    *RunService
	goalSvc   *GoalService
	domainSvc *DomainService
	agentSvc  *AgentService
	skillSvc  *SkillService
	squadSvc  *SquadService
}

func NewTeamImportService(st *store.Store, bus *events.Bus) *TeamImportService {
	return &TeamImportService{st: st, bus: bus}
}

// SetDependencies wires the back-references (circular constructor order —
// same pattern as DomainService.SetRunService). goalSvc + domainSvc are needed
// to create the import goal (on the seeded scratch import domain).
func (s *TeamImportService) SetDependencies(runSvc *RunService, goalSvc *GoalService, domainSvc *DomainService, agentSvc *AgentService, skillSvc *SkillService, squadSvc *SquadService) {
	s.runSvc = runSvc
	s.goalSvc = goalSvc
	s.domainSvc = domainSvc
	s.agentSvc = agentSvc
	s.skillSvc = skillSvc
	s.squadSvc = squadSvc
}

// ImportRequest is the user's input for importing a team repo.
type ImportRequest struct {
	GitURL         string `json:"git_url"`
	GitCredentials string `json:"git_credentials"`
	DefaultBranch  string `json:"default_branch"`
}

// ImportTeam kicks off a team-import worker run backed by a goal (Path D):
//
//  1. Find the system-internal steward agent (type=steward); error if none.
//  2. Ensure its runtime is active (reassign if needed); error if no active runtime.
//  3. Gather active runtime names for the prompt (steward assigns each agent).
//  4. Clean up old completed/failed rows (the table is temporary).
//  5. Insert a team_import row with the git config (run_id still empty).
//  6. Seed the scratch import domain (idempotent; mirrors digest's s_digestDomain).
//  7. Create an active system goal (created_by_type='system',
//     created_by_id='team_import', assignee=steward, domain=scratch import
//     domain, description=importPrompt). GoalService.Create enqueues the first
//     owner run IN the creation transaction (P0-2) — that run IS the import
//     run (run_kind='worker', has goal_id → appears in the task bar).
//  8. Back-fill the run_id onto the team_import row.
//
// Returns the team_import row and the first run.
func (s *TeamImportService) ImportTeam(ctx context.Context, req ImportRequest) (*TeamImport, *Run, error) {
	if strings.TrimSpace(req.GitURL) == "" {
		return nil, nil, NewValidationError("git_url is required")
	}

	// The steward agent runs the exploration task. It is auto-seeded at daemon
	// startup; if it's missing, the user hasn't connected a machine yet.
	steward, err := s.agentSvc.GetSteward(ctx)
	if err != nil {
		return nil, nil, NewValidationError("steward agent does not exist — connect a machine and restart the daemon")
	}
	if err := s.agentSvc.EnsureStewardRuntime(ctx); err != nil {
		return nil, nil, err
	}
	runtimeNames, err := s.agentSvc.ListActiveRuntimeNames(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list active runtimes: %w", err)
	}
	if len(runtimeNames) == 0 {
		return nil, nil, NewValidationError("no active runtime available — connect a machine first")
	}

	// Clean up old completed/failed rows — the table is temporary tracking.
	if _, err := s.st.DB().ExecContext(ctx,
		`DELETE FROM team_import WHERE status IN ('completed','failed')`); err != nil {
		return nil, nil, fmt.Errorf("cleanup old team_import rows: %w", err)
	}

	branch := req.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	ti := &TeamImport{
		ID:             newID(),
		GitURL:         req.GitURL,
		GitCredentials: req.GitCredentials,
		DefaultBranch:  branch,
		Status:         "pending",
		CreatedAt:      now(),
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO team_import (id,run_id,git_url,git_credentials,default_branch,status,result,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		ti.ID, "", ti.GitURL, ti.GitCredentials, ti.DefaultBranch, ti.Status, ti.Result, ti.CreatedAt); err != nil {
		return nil, nil, fmt.Errorf("insert team_import: %w", err)
	}

	if s.goalSvc == nil || s.domainSvc == nil {
		return nil, nil, errors.New("teamImportSvc.goalSvc/domainSvc not wired")
	}
	// Seed the scratch import domain (idempotent — mirrors digest's s_digestDomain).
	domain := s.seedImportDomain(ctx)
	if domain == nil {
		return nil, nil, fmt.Errorf("seed import scratch domain: aborted (name %q unavailable)", importDomainName)
	}

	// Create the import goal: a system goal on the steward, active (so
	// GoalService.Create enqueues the first owner run in-transaction), with the
	// importPrompt as the description (runTask's assemblePrompt uses the goal
	// description as the task — same as digest). The created_by_id='team_import'
	// marker is what the daemon recognizes import goals by (isImportGoal).
	prompt := importPrompt(runtimeNames)
	goal, err := s.goalSvc.Create(ctx, Goal{
		Title:         importGoalTitle,
		Description:   prompt,
		DomainID:      domain.ID,
		AssigneeType:  "agent",
		AssigneeID:    steward.ID,
		Status:        "active",
		CreatedByType: "system",
		CreatedByID:   ImportCreatedByID,
	})
	if err != nil {
		_, _ = s.st.DB().ExecContext(ctx, `DELETE FROM team_import WHERE id=?`, ti.ID)
		return nil, nil, fmt.Errorf("create import goal: %w", err)
	}

	// The first run was enqueued inside Create (P0-2). Load it — it is the
	// goal's latest queued/running owner run.
	var runID string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? AND role='owner' ORDER BY queued_at DESC LIMIT 1`, goal.ID).Scan(&runID); err != nil {
		// Symmetric cleanup: the goal (and its enqueued run) were created in
		// step 2 but the run lookup failed — delete the goal so it doesn't
		// strand as an active system goal with no team_import tracking row,
		// then delete the team_import row.
		_ = s.goalSvc.Delete(ctx, goal.ID)
		_, _ = s.st.DB().ExecContext(ctx, `DELETE FROM team_import WHERE id=?`, ti.ID)
		return nil, nil, fmt.Errorf("locate import goal's first run: %w", err)
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE team_import SET run_id=? WHERE id=?`, runID, ti.ID); err != nil {
		return nil, nil, fmt.Errorf("backfill team_import run_id: %w", err)
	}
	ti.RunID = runID
	s.bus.Publish(ctx, events.Event{Topic: "team:import_enqueued", Payload: ti})
	logging.Infof("team-import: created goal %s run %s for repo %s (steward=%s, runtimes=%v)", goal.ID, runID, sanitizeGitURL(req.GitURL), steward.ID, runtimeNames)
	return ti, &Run{ID: runID, GoalID: goal.ID, AgentID: steward.ID, RunKind: "worker", Status: "queued"}, nil
}

// seedImportDomain resolves (and if needed creates) the team-import scratch
// domain — mirrors digest's s_digestDomain (digest_seed.go:205). Returns nil
// when seeding must abort (a non-scratch domain owns the name). Idempotent via
// the builtin.teamimport.domain_id app_settings marker.
func (s *TeamImportService) seedImportDomain(ctx context.Context) *Domain {
	// Marker hit: the row must still exist and still be scratch.
	if id := importMarkerValue(ctx, s.st, importKeyDomain); id != "" {
		if d, err := s.domainSvc.Get(ctx, id); err == nil && d.Type == "scratch" {
			return d
		}
		clearImportMarker(ctx, s.st, importKeyDomain)
	}
	// By name.
	rows, err := s.st.DB().QueryContext(ctx, `SELECT id, type FROM domain WHERE name=?`, importDomainName)
	if err != nil {
		logging.Warnf("seed import: lookup domain: %v", err)
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
			logging.Warnf("seed import: domain %q exists with type %q (not scratch) — seeding aborted, the user's domain wins", importDomainName, foundType)
			return nil
		}
		setImportMarker(ctx, s.st, importKeyDomain, foundID)
		return &Domain{ID: foundID, Type: "scratch", Name: importDomainName}
	}
	// Create the scratch domain. git_url is meaningless for scratch.
	d, err := s.domainSvc.Create(ctx, Domain{
		Name: importDomainName,
		Type: "scratch",
	})
	if err != nil {
		// A create race with the same name → re-lookup once.
		rows, lerr := s.st.DB().QueryContext(ctx, `SELECT id, type FROM domain WHERE name=?`, importDomainName)
		if lerr == nil {
			for rows.Next() {
				_ = rows.Scan(&foundID, &foundType)
			}
			rows.Close()
		}
		if foundID != "" && foundType == "scratch" {
			setImportMarker(ctx, s.st, importKeyDomain, foundID)
			return &Domain{ID: foundID, Type: "scratch", Name: importDomainName}
		}
		logging.Warnf("seed import: create domain: %v", err)
		return nil
	}
	setImportMarker(ctx, s.st, importKeyDomain, d.ID)
	return d
}

// importMarkerValue reads one builtin.teamimport.* app_settings value. The
// value is JSON-encoded ("\"<id>\"") to stay compatible with SettingsService
// writes; bare quotes-trim decoding matches how the daemon reads settings.
func importMarkerValue(ctx context.Context, st *store.Store, key string) string {
	var v string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key=?`, key).Scan(&v); err != nil {
		return ""
	}
	return strings.Trim(v, `"`)
}

func setImportMarker(ctx context.Context, st *store.Store, key, id string) {
	b, _ := json.Marshal(id)
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO app_settings (key,value,updated_at) VALUES (?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, string(b), now()); err != nil {
		logging.Warnf("seed import: set %s: %v", key, err)
	}
}

func clearImportMarker(ctx context.Context, st *store.Store, key string) {
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM app_settings WHERE key=?`, key); err != nil {
		logging.Warnf("seed import: clear %s: %v", key, err)
	}
}

// GitConfigForRun returns the git config stored on the team_import row for a
// given run ID. Called by the daemon's runProcessorTask at dispatch time —
// the git config was persisted at enqueue (HTTP request) time and survives
// the queued interval.
func (s *TeamImportService) GitConfigForRun(ctx context.Context, runID string) (gitURL, gitCredentials, defaultBranch string, ok bool) {
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT git_url, git_credentials, default_branch FROM team_import WHERE run_id=?`, runID).
		Scan(&gitURL, &gitCredentials, &defaultBranch)
	if err != nil {
		logging.Warnf("team-import: no team_import row for run %s — git config unavailable", runID)
		return "", "", "", false
	}
	return gitURL, gitCredentials, defaultBranch, true
}

// importPrompt builds the instruction for the steward agent. The agent clones
// the import repo ITSELF (Path D — credentials ride the AGENTWORK_GIT_* env,
// never the prompt) into a repo/ subdir, explores it with its file tools, then
// writes team.json to the workdir ROOT (the artifact upload root — NOT inside
// repo/). runtimeNames is the list of active runtimes the steward can assign
// to each imported agent.
func importPrompt(runtimeNames []string) string {
	var b strings.Builder
	b.WriteString("你是 agentwork 的团队导入处理器。当前工作目录是一个空的 scratch 目录。\n\n")
	b.WriteString("第 0 步：克隆导入仓库。执行 `git clone $AGENTWORK_GIT_URL repo` 将团队定义仓库克隆到 `repo/` 子目录，然后 `cd repo` 再开始探索。如果设置了 $AGENTWORK_GIT_CREDENTIALS，它已经嵌入在 $AGENTWORK_GIT_URL 里——不要再额外添加。如果克隆失败，报告错误并停止。\n\n")
	b.WriteString("探索克隆下来的仓库，理解团队结构，然后在工作目录根目录（你启动时的目录，不是 repo/ 里面）生成 team.json。平台从工作目录根读取 team.json；如果你写到了 repo/ 里面，它不会被找到。\n\n")
	b.WriteString("高效探索（重要——大仓库不要逐个读所有文件）：\n")
	b.WriteString("- 先用 list_files / find / ls 扫描仓库文件清单，建立全局视图，再决定读哪些。\n")
	b.WriteString("- 只读「定义文件」：团队入口（team.md/squad.md/README.md）、角色定义文件、每个 skill 的 SKILL.md。\n")
	b.WriteString("- 跳过程序源码、测试文件、配置文件、构建产物、依赖目录（node_modules/vendor/dist 等）——这些不是团队定义，不需要读。\n")
	b.WriteString("- skill 的 files 字段只需包含该 skill 目录下的定义文件（至少 SKILL.md）；不要把程序源码塞进去。\n")
	b.WriteString("- 用批量读取策略：如果工具支持一次读多个文件或读目录，优先用；不要一个文件一个工具调用地串行读。\n\n")
	b.WriteString("步骤：\n")
	b.WriteString("1. 找到并读取团队入口文件——查找 team.md、TEAM.md、squad.md 或 README.md。\n")
	b.WriteString("2. 根据入口文件里的引用，读取所有角色定义文件和技能定义文件（只读定义，跳过源码/测试/配置）。\n")
	b.WriteString("3. 理解协作结构：谁是 Leader，谁是 Reviewer，谁是普通 Member。\n\n")
	b.WriteString("team.json 结构：\n")
	b.WriteString(`{
  "name": "<团队名>",
  "description": "<一行团队描述>",
  "skills": [
    {
      "name": "<技能名>",
      "description": "<技能描述>",
      "files": {"SKILL.md": "<完整的原始 SKILL.md 内容>", ...}
    }
  ],
  "agents": [
    {
      "name": "<agent 名>",
      "description": "<一行描述>",
      "system_prompt": "<角色定义文件的完整原始内容——不要改写或翻译>",
      "skills": ["<技能名>", ...],
      "role": "leader|reviewer|member",
      "runtime": "<下方列表中的 runtime 名>"
    }
  ],
  "squad": {
    "name": "<小队名>",
    "description": "<小队描述>",
    "leader": "<leader agent 名>",
    "instructions": "<TEAM.md 里的 Instructions 部分，或等价内容>",
    "members": [
      {"agent": "<agent 名>", "role": "reviewer|"}
    ]
  }
}`)
	b.WriteString("\n\n规则：\n")
	b.WriteString("- system_prompt = 角色定义文件的完整原始内容（不要改写或翻译）。\n")
	b.WriteString("- skills = 该 agent 可使用的技能名列表（从 team.md 或角色定义推断；不确定就留空数组）。\n")
	b.WriteString("- role=\"leader\" → squad.leader；role=\"reviewer\" → 平台在审查环节自动拉取；role=\"member\" → 普通成员。\n")
	b.WriteString("- skills[].files 只需包含该技能目录下的定义文件（至少 SKILL.md），保留原始文件内容；不要塞入程序源码、测试或配置文件。\n")
	b.WriteString("- squad.members 不包含 leader（leader 在 squad.leader 里）。\n")
	b.WriteString("- 你是导入处理器，不是团队成员。不要把自己放进 agents 列表或 squad——只包含团队仓库里定义的 agent。\n")
	b.WriteString("- 仓库格式不固定——用你的理解把任何格式映射到上面的 schema。\n")
	b.WriteString("- runtime：从下方列表里给每个 agent 分配一个 runtime。如果团队定义指定了偏好（如\"这个角色需要编码 CLI\"），匹配最合适的 runtime。没指定就随便选一个。每个 agent 必须有 runtime。\n")
	b.WriteString("  可用 runtime：" + strings.Join(runtimeNames, ", ") + "\n")
	b.WriteString("- 最后用一句话总结你的导入理由。\n")
	return b.String()
}

// IngestImport reads the agent's team.json artifact and upserts all entities.
// Called by the daemon's finishMachineRun worker branch when the run belongs
// to an import goal. It does NOT stamp the run status — the worker path's
// runSvc.Finish owns the run terminal stamp (calling this after Finish would
// double-stamp). On error it marks team_import failed (failImport) and
// returns the error so the caller can flip the run to failed.
func (s *TeamImportService) IngestImport(ctx context.Context, runID string, artifacts map[string]string, summary string) (*TeamImport, string, error) {
	var ti TeamImport
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, run_id, status FROM team_import WHERE run_id=?`, runID).
		Scan(&ti.ID, &ti.RunID, &ti.Status)
	if err != nil {
		return nil, "", fmt.Errorf("team_import row for run %s: %w", runID, err)
	}

	var tj teamJSON
	if err := s.parseTeamArtifact(ctx, artifacts, &ti, &tj); err != nil {
		return nil, "", err
	}

	skillIDs, err := s.upsertSkills(ctx, &ti, tj.Skills)
	if err != nil {
		return nil, "", err
	}
	agentIDs, err := s.upsertAgents(ctx, &ti, tj.Agents, skillIDs)
	if err != nil {
		return nil, "", err
	}
	if err := s.upsertSquad(ctx, &ti, tj.Squad, agentIDs); err != nil {
		return nil, "", err
	}
	if err := s.completeImport(ctx, &ti, &tj, summary); err != nil {
		return nil, "", err
	}
	squadName := ""
	if tj.Squad != nil {
		squadName = tj.Squad.Name
	}
	return &ti, squadName, nil
}

// parseTeamArtifact extracts and validates team.json from the artifact map.
func (s *TeamImportService) parseTeamArtifact(ctx context.Context, artifacts map[string]string, ti *TeamImport, tj *teamJSON) error {
	raw, ok := artifacts["team.json"]
	if !ok || strings.TrimSpace(raw) == "" {
		return s.failImport(ctx, ti, "team.json: artifact missing — the import agent did not produce it")
	}
	if err := json.Unmarshal([]byte(raw), tj); err != nil {
		return s.failImport(ctx, ti, "parse team.json: "+err.Error())
	}
	if len(tj.Agents) == 0 {
		return s.failImport(ctx, ti, "team.json: no agents defined")
	}
	return nil
}

// upsertSkills creates/updates all skills and returns a name→ID map.
func (s *TeamImportService) upsertSkills(ctx context.Context, ti *TeamImport, skills []struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Files       map[string]string `json:"files"`
}) (map[string]string, error) {
	skillIDs := map[string]string{}
	for _, sk := range skills {
		out, err := s.skillSvc.UpsertByName(ctx, sk.Name, sk.Description, sk.Files)
		if err != nil {
			return nil, s.failImport(ctx, ti, fmt.Sprintf("upsert skill %q: %v", sk.Name, err))
		}
		skillIDs[sk.Name] = out.ID
	}
	return skillIDs, nil
}

// upsertAgents creates/updates all agents and returns a name→ID map.
// Each agent's runtime name (from team.json) is resolved to a runtime ID;
// if the name is empty or not found, the first active runtime is assigned.
func (s *TeamImportService) upsertAgents(ctx context.Context, ti *TeamImport, agents []struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SystemPrompt string   `json:"system_prompt"`
	Skills       []string `json:"skills"`
	Role         string   `json:"role"`
	Runtime      string   `json:"runtime"`
}, skillIDs map[string]string) (map[string]string, error) {
	activeIDs, err := s.agentSvc.ListActiveRuntimeIDs(ctx)
	if err != nil {
		return nil, s.failImport(ctx, ti, fmt.Sprintf("list active runtimes: %v", err))
	}
	if len(activeIDs) == 0 {
		return nil, s.failImport(ctx, ti, "no active runtime available — connect a machine first")
	}
	agentIDs := map[string]string{}
	for _, a := range agents {
		var sids []string
		for _, sname := range a.Skills {
			if id, ok := skillIDs[sname]; ok {
				sids = append(sids, id)
			}
		}
		rtID := s.resolveRuntime(ctx, a.Runtime, activeIDs)
		out, err := s.agentSvc.UpsertByName(ctx, a.Name, a.Description, a.SystemPrompt, rtID, sids)
		if err != nil {
			return nil, s.failImport(ctx, ti, fmt.Sprintf("upsert agent %q: %v", a.Name, err))
		}
		agentIDs[a.Name] = out.ID
	}
	return agentIDs, nil
}

// resolveRuntime maps a runtime name to its ID. If the name is empty or not
// found among active runtimes, the first active runtime ID is returned.
func (s *TeamImportService) resolveRuntime(ctx context.Context, name string, fallbackIDs []string) string {
	if name != "" {
		var id string
		if err := s.st.DB().QueryRowContext(ctx,
			`SELECT id FROM runtime WHERE name=? AND status='active'`, name).Scan(&id); err == nil && id != "" {
			return id
		}
	}
	return fallbackIDs[0]
}

// upsertSquad creates/updates the squad and its members.
func (s *TeamImportService) upsertSquad(ctx context.Context, ti *TeamImport, sq *struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Leader       string `json:"leader"`
	Instructions string `json:"instructions"`
	Members      []struct {
		Agent string `json:"agent"`
		Role  string `json:"role"`
	} `json:"members"`
}, agentIDs map[string]string) error {
	if sq == nil {
		return nil
	}
	leaderID, ok := agentIDs[sq.Leader]
	if !ok {
		return s.failImport(ctx, ti, fmt.Sprintf("squad leader %q not found in agents", sq.Leader))
	}
	var members []SquadMember
	for _, m := range sq.Members {
		mid, ok := agentIDs[m.Agent]
		if !ok {
			continue
		}
		role := m.Role
		if role == "" {
			role = "member"
		}
		members = append(members, SquadMember{MemberType: "agent", MemberID: mid, Role: role})
	}
	if _, err := s.squadSvc.UpsertByName(ctx, sq.Name, sq.Description, leaderID, sq.Instructions, members); err != nil {
		return s.failImport(ctx, ti, fmt.Sprintf("upsert squad %q: %v", sq.Name, err))
	}
	return nil
}

// completeImport stamps the result and publishes the completion event.
// The UPDATE is guarded by status='pending' so a late success report cannot
// overwrite an already-determined failure (a machine-level failure that raced
// ahead via FailImportByRun). A no-op on a terminal row drops the late event
// rather than re-publishing team:imported.
func (s *TeamImportService) completeImport(ctx context.Context, ti *TeamImport, tj *teamJSON, summary string) error {
	result, _ := json.Marshal(map[string]any{
		"agents":    len(tj.Agents),
		"skills":    len(tj.Skills),
		"has_squad": tj.Squad != nil,
		"summary":   summary,
	})
	res, err := s.st.DB().ExecContext(ctx,
		`UPDATE team_import SET status='completed', result=? WHERE id=? AND status='pending'`,
		string(result), ti.ID)
	if err != nil {
		return fmt.Errorf("update team_import status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		logging.Infof("team-import: run %s already terminal — dropping late success", ti.RunID)
		return nil
	}
	ti.Result = string(result)
	s.bus.Publish(ctx, events.Event{Topic: "team:imported", Payload: map[string]any{
		"team_import_id": ti.ID, "run_id": ti.RunID,
	}})
	logging.Infof("team-import: run %s completed — %d agent(s), %d skill(s), squad=%v", ti.RunID, len(tj.Agents), len(tj.Skills), tj.Squad != nil)
	return nil
}

// FailImportByRun marks a team-import run failed by its run ID. Called by the
// daemon's failImportRun when a machine-dispatched import run fails. If the
// row is still pending, it delegates to failImport (UPDATE + publish
// team:import_failed); an already-terminal row is a no-op — a late failure
// must not overwrite a completed import (symmetric with completeImport's guard).
func (s *TeamImportService) FailImportByRun(ctx context.Context, runID, reason string) error {
	var ti TeamImport
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, run_id, status FROM team_import WHERE run_id=?`, runID).
		Scan(&ti.ID, &ti.RunID, &ti.Status)
	if err != nil {
		return fmt.Errorf("team_import row for run %s: %w", runID, err)
	}
	if ti.Status != "pending" {
		logging.Infof("team-import: run %s already %s — dropping late failure", runID, ti.Status)
		return nil
	}
	return s.failImport(ctx, &ti, reason)
}

// failImport marks the import as failed and publishes an event.
func (s *TeamImportService) failImport(ctx context.Context, ti *TeamImport, reason string) error {
	logging.Errorf("team-import: run %s failed: %s", ti.RunID, reason)
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE team_import SET status='failed', result=? WHERE id=?`, reason, ti.ID); err != nil {
		return fmt.Errorf("update team_import failed: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "team:import_failed", Payload: map[string]any{
		"team_import_id": ti.ID, "run_id": ti.RunID, "error": reason,
	}})
	return NewValidationError(reason)
}

// GetByRun returns the team_import row for a run ID (the HTTP status endpoint).
func (s *TeamImportService) GetByRun(ctx context.Context, runID string) (*TeamImport, error) {
	var ti TeamImport
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, run_id, git_url, git_credentials, default_branch, status, result, created_at FROM team_import WHERE run_id=?`, runID).
		Scan(&ti.ID, &ti.RunID, &ti.GitURL, &ti.GitCredentials, &ti.DefaultBranch, &ti.Status, &ti.Result, &ti.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ti, nil
}

// ListActive returns team_import rows that are still pending. Completed/failed
// rows are cleaned up at the start of the next import; until then they're
// stale history, not active state — the frontend should not resurface them
// after a refresh.
func (s *TeamImportService) ListActive(ctx context.Context) ([]TeamImport, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id, run_id, git_url, git_credentials, default_branch, status, result, created_at FROM team_import WHERE status='pending' ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamImport
	for rows.Next() {
		var ti TeamImport
		if err := rows.Scan(&ti.ID, &ti.RunID, &ti.GitURL, &ti.GitCredentials, &ti.DefaultBranch, &ti.Status, &ti.Result, &ti.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ti)
	}
	return out, rows.Err()
}

// sanitizeGitURL strips embedded credentials before logging.
func sanitizeGitURL(raw string) string {
	return gitutil.SanitizeURL(raw)
}
