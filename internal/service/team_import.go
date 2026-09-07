package service

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
	GoalID          string `json:"goal_id"`
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

// TeamImportService owns the team-import lifecycle: creating a system-task
// goal, ingesting the agent's team.json artifact into agent/squad/skill rows,
// and publishing completion events. Implements SystemTaskHandler so the
// daemon can dispatch and complete import runs generically.
type TeamImportService struct {
	st       *store.Store
	bus      *events.Bus
	runSvc   *RunService
	agentSvc *AgentService
	skillSvc *SkillService
	squadSvc *SquadService
	goalSvc  *GoalService
}

func NewTeamImportService(st *store.Store, bus *events.Bus) *TeamImportService {
	return &TeamImportService{st: st, bus: bus}
}

// SetDependencies wires the back-references (circular constructor order —
// same pattern as DomainService.SetRunService).
func (s *TeamImportService) SetDependencies(runSvc *RunService, agentSvc *AgentService, skillSvc *SkillService, squadSvc *SquadService, goalSvc *GoalService) {
	s.runSvc = runSvc
	s.agentSvc = agentSvc
	s.skillSvc = skillSvc
	s.squadSvc = squadSvc
	s.goalSvc = goalSvc
}

// ImportRequest is the user's input for importing a team repo.
type ImportRequest struct {
	GitURL         string `json:"git_url"`
	GitCredentials string `json:"git_credentials"`
	DefaultBranch  string `json:"default_branch"`
}

// ImportTeam kicks off a team-import as a system-task goal:
//  1. Find the system-internal steward agent (type=steward); error if none.
//  2. Ensure its runtime is active (reassign if needed); error if no active runtime.
//  3. Gather active runtime names for the prompt (steward assigns each agent).
//  4. Clean up old completed/failed imports (team_import rows + goals + domains).
//  5. Insert a team_import row with the git config (goal_id/run_id still empty).
//  6. Create a system-task goal (CreateSystemTaskGoal) — creates a temp scratch
//     domain + an active goal assigned to the steward, auto-enqueuing the first
//     run with run_type="import".
//  7. Back-fill the goal_id and run_id onto the team_import row.
//
// Returns the team_import row and the goal.
func (s *TeamImportService) ImportTeam(ctx context.Context, req ImportRequest) (*TeamImport, *Goal, error) {
	if s.goalSvc == nil {
		return nil, nil, errors.New("teamImportSvc dependencies not wired")
	}
	if strings.TrimSpace(req.GitURL) == "" {
		return nil, nil, NewValidationError("git_url is required")
	}

	// The steward agent runs the exploration task. It is auto-seeded at
	// daemon startup; if it's missing, the user hasn't connected a machine.
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

	// Clean up old completed/failed imports: delete their goals + temp
	// domains, then remove the team_import tracking rows.
	s.cleanupOldImports(ctx)

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
		`INSERT INTO team_import (id,run_id,goal_id,git_url,git_credentials,default_branch,status,result,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		ti.ID, "", "", ti.GitURL, ti.GitCredentials, ti.DefaultBranch, ti.Status, ti.Result, ti.CreatedAt); err != nil {
		return nil, nil, fmt.Errorf("insert team_import: %w", err)
	}

	// Create a system-task goal: temp scratch domain + active goal assigned
	// to the steward. Create() enqueues the first run; CreateSystemTaskGoal
	// stamps run_type="import" on it.
	title := SystemTaskGoalTitle("import", gitutil.SanitizeURL(req.GitURL))
	desc := "导入团队仓库 " + gitutil.SanitizeURL(req.GitURL) + " 的 agent/skill/squad 定义"
	goal, err := s.goalSvc.CreateSystemTaskGoal(ctx, title, desc, steward.ID, "import")
	if err != nil {
		_, _ = s.st.DB().ExecContext(ctx, `DELETE FROM team_import WHERE id=?`, ti.ID)
		return nil, nil, fmt.Errorf("create import goal: %w", err)
	}

	// Back-fill goal_id and run_id onto the team_import row. The run may
	// already be 'running' if Claim raced ahead — query by goal_id, not
	// by status.
	var runID string
	_ = s.st.DB().QueryRowContext(ctx,
		`SELECT id FROM run WHERE goal_id=? ORDER BY created_at DESC LIMIT 1`, goal.ID).Scan(&runID)
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE team_import SET run_id=?, goal_id=? WHERE id=?`, runID, goal.ID, ti.ID); err != nil {
		return nil, nil, fmt.Errorf("backfill team_import run_id/goal_id: %w", err)
	}
	ti.RunID = runID
	ti.GoalID = goal.ID
	s.bus.Publish(ctx, events.Event{Topic: "team:import_enqueued", Payload: ti})
	logging.Infof("team-import: created goal %s, run %s for repo %s (steward=%s, runtimes=%v)", goal.ID, runID, gitutil.SanitizeURL(req.GitURL), steward.ID, runtimeNames)
	return ti, goal, nil
}

// cleanupOldImports deletes terminal import goals and their temp domains,
// then removes the team_import tracking rows. Called at the start of each
// ImportTeam — the table is temporary tracking, and old import goals
// (done/failed) are stale history that should not clutter the goal list.
func (s *TeamImportService) cleanupOldImports(ctx context.Context) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT goal_id FROM team_import WHERE status IN ('completed','failed') AND goal_id != ''`)
	if err != nil {
		logging.Warnf("team-import: cleanup old imports: %v", err)
		return
	}
	var goalIDs []string
	for rows.Next() {
		var gid string
		_ = rows.Scan(&gid)
		if gid != "" {
			goalIDs = append(goalIDs, gid)
		}
	}
	rows.Close()
	for _, gid := range goalIDs {
		s.goalSvc.DeleteSystemTaskGoal(ctx, gid)
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`DELETE FROM team_import WHERE status IN ('completed','failed')`); err != nil {
		logging.Warnf("team-import: cleanup old team_import rows: %v", err)
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

// ImportPrompt builds the instruction for the steward agent. The agent clones
// the team repo (the platform handles the clone) and explores it with its file
// tools, then writes team.json. runtimeNames is the list of active runtimes
// the steward can assign to each imported agent.
func ImportPrompt(runtimeNames []string) string {
	var b strings.Builder
	b.WriteString("You are the agentwork team-import processor. The current working directory is a team-definition repository.\n\n")
	b.WriteString("Explore the repository, understand the team structure, and produce team.json in the current working directory (file is the result — do NOT output to stdout).\n\n")
	b.WriteString("Steps:\n")
	b.WriteString("1. Find and read team.md (or TEAM.md) — the team's entry file.\n")
	b.WriteString("2. Follow the references in team.md to read all role definition files and skill definition files.\n")
	b.WriteString("3. Understand the collaboration structure: who is the Leader, who is the Reviewer, who are the Members.\n\n")
	b.WriteString("team.json structure:\n")
	b.WriteString(`{
  "name": "<team name>",
  "description": "<one-line team description>",
  "skills": [
    {
      "name": "<skill name>",
      "description": "<skill description>",
      "files": {"SKILL.md": "<full original SKILL.md content>", ...}
    }
  ],
  "agents": [
    {
      "name": "<agent name>",
      "description": "<one-line description>",
      "system_prompt": "<full original content of the role definition file — do not rewrite or translate>",
      "skills": ["<skill name>", ...],
      "role": "leader|reviewer|member",
      "runtime": "<runtime name from the list below>"
    }
  ],
  "squad": {
    "name": "<squad name>",
    "description": "<squad description>",
    "leader": "<leader agent name>",
    "instructions": "<Instructions section from TEAM.md, or equivalent>",
    "members": [
      {"agent": "<agent name>", "role": "reviewer|"}
    ]
  }
}`)
	b.WriteString("\n\nRules:\n")
	b.WriteString("- system_prompt = the full original content of the role definition file (do not rewrite or translate).\n")
	b.WriteString("- skills = the list of skill names this agent can use (infer from team.md or role definitions; if unclear, leave an empty array).\n")
	b.WriteString("- role=\"leader\" → squad.leader; role=\"reviewer\" → the platform auto-pulls into review checkpoints; role=\"member\" → regular member.\n")
	b.WriteString("- skills[].files must include ALL files of that skill (at least SKILL.md), with original file contents.\n")
	b.WriteString("- squad.members does NOT include the leader (the leader is in squad.leader).\n")
	b.WriteString("- You are the import processor, NOT a team member. Do NOT include yourself in the agents list or squad — only include agents defined in the team repo.\n")
	b.WriteString("- The repo format is not fixed — use your understanding to map any format to the schema above.\n")
	b.WriteString("- runtime: assign each agent a runtime from the list below. If the team definition specifies a preference (e.g. \"this role needs a coding CLI\"), match it to the most suitable runtime. If no preference is stated, pick any runtime (random is fine). Every agent MUST have a runtime.\n")
	b.WriteString("  Available runtimes: " + strings.Join(runtimeNames, ", ") + "\n")
	b.WriteString("- End with a one-sentence summary of your import rationale.\n")
	return b.String()
}

// IngestImport reads the agent's team.json artifact and upserts all entities.
// Called by the daemon's finishSystemTaskRun via IngestArtifacts.
func (s *TeamImportService) IngestImport(ctx context.Context, runID string, artifacts map[string]string, summary string) (*TeamImport, *teamJSON, error) {
	var ti TeamImport
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, run_id, goal_id, status FROM team_import WHERE run_id=?`, runID).
		Scan(&ti.ID, &ti.RunID, &ti.GoalID, &ti.Status)
	if err != nil {
		return nil, nil, fmt.Errorf("team_import row for run %s: %w", runID, err)
	}

	var tj teamJSON
	if err := s.parseTeamArtifact(ctx, artifacts, &ti, &tj); err != nil {
		return nil, nil, err
	}

	skillIDs, err := s.upsertSkills(ctx, &ti, tj.Skills)
	if err != nil {
		return nil, nil, err
	}
	agentIDs, err := s.upsertAgents(ctx, &ti, tj.Agents, skillIDs)
	if err != nil {
		return nil, nil, err
	}
	if err := s.upsertSquad(ctx, &ti, tj.Squad, agentIDs); err != nil {
		return nil, nil, err
	}
	if err := s.completeImport(ctx, &ti, &tj, summary); err != nil {
		return nil, nil, err
	}
	return &ti, &tj, nil
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
func (s *TeamImportService) completeImport(ctx context.Context, ti *TeamImport, tj *teamJSON, summary string) error {
	squadName := ""
	if tj.Squad != nil {
		squadName = tj.Squad.Name
	}
	result, _ := json.Marshal(map[string]any{
		"agents":     len(tj.Agents),
		"skills":     len(tj.Skills),
		"has_squad":  tj.Squad != nil,
		"squad_name": squadName,
		"summary":    summary,
	})
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE team_import SET status='completed', result=? WHERE id=?`, string(result), ti.ID); err != nil {
		return fmt.Errorf("update team_import status: %w", err)
	}
	ti.Result = string(result)
	s.bus.Publish(ctx, events.Event{Topic: "team:imported", Payload: map[string]any{
		"team_import_id": ti.ID, "run_id": ti.RunID, "goal_id": ti.GoalID,
	}})
	logging.Infof("team-import: run %s completed — %d agent(s), %d skill(s), squad=%v", ti.RunID, len(tj.Agents), len(tj.Skills), tj.Squad != nil)
	return nil
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
		`SELECT id, run_id, goal_id, git_url, git_credentials, default_branch, status, result, created_at FROM team_import WHERE run_id=?`, runID).
		Scan(&ti.ID, &ti.RunID, &ti.GoalID, &ti.GitURL, &ti.GitCredentials, &ti.DefaultBranch, &ti.Status, &ti.Result, &ti.CreatedAt)
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
		`SELECT id, run_id, goal_id, git_url, git_credentials, default_branch, status, result, created_at FROM team_import WHERE status='pending' ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamImport
	for rows.Next() {
		var ti TeamImport
		if err := rows.Scan(&ti.ID, &ti.RunID, &ti.GoalID, &ti.GitURL, &ti.GitCredentials, &ti.DefaultBranch, &ti.Status, &ti.Result, &ti.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ti)
	}
	return out, rows.Err()
}

// ── SystemTaskHandler implementation ──

// ArtifactFiles returns the file the import agent must produce.
func (s *TeamImportService) ArtifactFiles() []string {
	return []string{"team.json"}
}

// BuildPrompt returns the exploration instruction for the steward. Called
// at dispatch time by the daemon.
func (s *TeamImportService) BuildPrompt(ctx context.Context, runID string) string {
	runtimeNames, err := s.agentSvc.ListActiveRuntimeNames(ctx)
	if err != nil || len(runtimeNames) == 0 {
		runtimeNames = []string{"(none — connect a machine)"}
	}
	return ImportPrompt(runtimeNames)
}

// IngestArtifacts processes the agent's team.json and upserts all entities.
// Returns a human-readable summary for the goal feed, or an error (the run
// is marked failed).
func (s *TeamImportService) IngestArtifacts(ctx context.Context, runID string, artifacts map[string]string, agentSummary string) (string, error) {
	_, tj, err := s.IngestImport(ctx, runID, artifacts, agentSummary)
	if err != nil {
		return "", err
	}
	return formatImportSummary(tj), nil
}

func formatImportSummary(tj *teamJSON) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ 团队导入完成：%d 个 agent、%d 个 skill", len(tj.Agents), len(tj.Skills))
	if tj.Squad == nil {
		return b.String()
	}
	b.WriteString("、1 个 squad")
	sq := tj.Squad
	fmt.Fprintf(&b, "\n\n**👥 %s**", sq.Name)
	if sq.Description != "" {
		fmt.Fprintf(&b, "\n\n**描述：** %s", firstLine(sq.Description))
	}
	fmt.Fprintf(&b, "\n\n**队长：** %s", sq.Leader)
	if sq.Instructions != "" {
		fmt.Fprintf(&b, "\n\n**协作规则：** %s", truncateStr(sq.Instructions, 200))
	}
	if len(sq.Members) > 0 {
		b.WriteString("\n\n**成员：**")
		for _, m := range sq.Members {
			role := m.Role
			if role == "" {
				role = "成员"
			}
			fmt.Fprintf(&b, "  \n　- %s（%s）", m.Agent, role)
		}
	} else {
		b.WriteString("\n\n**成员：** （无）")
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
