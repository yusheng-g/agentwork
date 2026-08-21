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
// processor run: the run that clones a team repo, has an agent explore it,
// and produces team.json. The platform reads team.json and upserts the
// entities by name. git_url/credentials/branch persist from the HTTP request
// to daemon dispatch (the run may sit queued for seconds/minutes before
// claim). ImportTeam cleans up old completed/failed rows at the start of each
// import — the table holds at most one pending + zero/one just-finished row.
type TeamImport struct {
	ID              string `json:"id"`
	RunID           string `json:"run_id"`
	RuntimeID       string `json:"runtime_id"`
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
		Role         string   `json:"role"` // leader|reviewer|member
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

// TeamImportService owns the team-import lifecycle: enqueueing the processor
// run, and ingesting the agent's team.json artifact into agent/squad/skill rows.
type TeamImportService struct {
	st      *store.Store
	bus     *events.Bus
	runSvc  *RunService
	agentSvc *AgentService
	skillSvc *SkillService
	squadSvc *SquadService
}

func NewTeamImportService(st *store.Store, bus *events.Bus) *TeamImportService {
	return &TeamImportService{st: st, bus: bus}
}

// SetDependencies wires the back-references (circular constructor order —
// same pattern as DomainService.SetRunService).
func (s *TeamImportService) SetDependencies(runSvc *RunService, agentSvc *AgentService, skillSvc *SkillService, squadSvc *SquadService) {
	s.runSvc = runSvc
	s.agentSvc = agentSvc
	s.skillSvc = skillSvc
	s.squadSvc = squadSvc
}

// ImportRequest is the user's input for importing a team repo.
type ImportRequest struct {
	GitURL           string `json:"git_url"`
	GitCredentials   string `json:"git_credentials"`
	DefaultBranch    string `json:"default_branch"`
	ProcessorAgentID string `json:"processor_agent_id"`
	RuntimeID        string `json:"runtime_id"`
}

// ImportTeam kicks off a team-import processor run:
//  1. Clean up old completed/failed rows (the table is temporary).
//  2. Insert a team_import row with the git config (run_id still empty).
//  3. Enqueue a processor run (run_type="import") using the team_import ID
//     as the run's domain_id — the machine uses it as the bare-repo directory
//     key (~/.agentwork/repos/<id>/). No domain (project) row is created.
//  4. Back-fill the run_id onto the team_import row.
//
// Returns the team_import row and the processor run.
func (s *TeamImportService) ImportTeam(ctx context.Context, req ImportRequest) (*TeamImport, *Run, error) {
	if strings.TrimSpace(req.GitURL) == "" {
		return nil, nil, NewValidationError("git_url is required")
	}
	if req.ProcessorAgentID == "" {
		return nil, nil, NewValidationError("processor_agent_id is required")
	}
	if req.RuntimeID == "" {
		return nil, nil, NewValidationError("runtime_id is required (imported agents need a runtime to bind to)")
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, req.ProcessorAgentID, "processor agent"); err != nil {
		return nil, nil, err
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM runtime WHERE id=?`, req.RuntimeID, "runtime"); err != nil {
		return nil, nil, err
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
		ID:            newID(),
		RuntimeID:     req.RuntimeID,
		GitURL:        req.GitURL,
		GitCredentials: req.GitCredentials,
		DefaultBranch: branch,
		Status:        "pending",
		CreatedAt:     now(),
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO team_import (id,run_id,runtime_id,git_url,git_credentials,default_branch,status,result,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		ti.ID, "", ti.RuntimeID, ti.GitURL, ti.GitCredentials, ti.DefaultBranch, ti.Status, ti.Result, ti.CreatedAt); err != nil {
		return nil, nil, fmt.Errorf("insert team_import: %w", err)
	}

	if s.runSvc == nil {
		return nil, nil, errors.New("teamImportSvc.runSvc not wired")
	}
	prompt := importPrompt()
	// The team_import ID serves as the run's domain_id — the machine's
	// ensureBareRepo uses it as the bare-repo directory key. No domain row
	// exists; run.domain_id has no FK constraint.
	run, err := s.runSvc.EnqueueProcessorRun(ctx, "import", ti.ID, req.ProcessorAgentID, prompt)
	if err != nil {
		_, _ = s.st.DB().ExecContext(ctx, `DELETE FROM team_import WHERE id=?`, ti.ID)
		return nil, nil, fmt.Errorf("enqueue import run: %w", err)
	}

	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE team_import SET run_id=? WHERE id=?`, run.ID, ti.ID); err != nil {
		return nil, nil, fmt.Errorf("backfill team_import run_id: %w", err)
	}
	ti.RunID = run.ID
	s.bus.Publish(ctx, events.Event{Topic: "team:import_enqueued", Payload: ti})
	logging.Infof("team-import: enqueued run %s for repo %s (runtime=%s, agent=%s)", run.ID, sanitizeGitURL(req.GitURL), req.RuntimeID, req.ProcessorAgentID)
	return ti, run, nil
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

// importPrompt builds the instruction for the import processor agent. The
// agent clones the team repo (the platform handles the clone) and explores it
// with its file tools, then writes team.json.
func importPrompt() string {
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
      "role": "leader|reviewer|member"
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
	b.WriteString("- The repo format is not fixed — use your understanding to map any format to the schema above.\n")
	b.WriteString("- End with a one-sentence summary of your import rationale.\n")
	return b.String()
}

// IngestImport reads the agent's team.json artifact and upserts all entities.
// Called by the daemon's ingestProcessorFinished when run_type=="import".
func (s *TeamImportService) IngestImport(ctx context.Context, runID string, artifacts map[string]string, summary string) error {
	var ti TeamImport
	var runtimeID string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id, run_id, runtime_id, status FROM team_import WHERE run_id=?`, runID).
		Scan(&ti.ID, &ti.RunID, &runtimeID, &ti.Status)
	if err != nil {
		return fmt.Errorf("team_import row for run %s: %w", runID, err)
	}

	var tj teamJSON
	if err := s.parseTeamArtifact(artifacts, &ti, &tj); err != nil {
		return err
	}

	skillIDs, err := s.upsertSkills(ctx, &ti, tj.Skills)
	if err != nil {
		return err
	}
	agentIDs, err := s.upsertAgents(ctx, &ti, tj.Agents, runtimeID, skillIDs)
	if err != nil {
		return err
	}
	if err := s.upsertSquad(ctx, &ti, tj.Squad, agentIDs); err != nil {
		return err
	}
	return s.completeImport(ctx, &ti, &tj, summary)
}

// parseTeamArtifact extracts and validates team.json from the artifact map.
func (s *TeamImportService) parseTeamArtifact(artifacts map[string]string, ti *TeamImport, tj *teamJSON) error {
	raw, ok := artifacts["team.json"]
	if !ok || strings.TrimSpace(raw) == "" {
		return s.failImport(context.Background(), ti, "team.json: artifact missing — the import agent did not produce it")
	}
	if err := json.Unmarshal([]byte(raw), tj); err != nil {
		return s.failImport(context.Background(), ti, "parse team.json: "+err.Error())
	}
	if len(tj.Agents) == 0 {
		return s.failImport(context.Background(), ti, "team.json: no agents defined")
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
func (s *TeamImportService) upsertAgents(ctx context.Context, ti *TeamImport, agents []struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SystemPrompt string   `json:"system_prompt"`
	Skills       []string `json:"skills"`
	Role         string   `json:"role"`
}, runtimeID string, skillIDs map[string]string) (map[string]string, error) {
	agentIDs := map[string]string{}
	for _, a := range agents {
		var sids []string
		for _, sname := range a.Skills {
			if id, ok := skillIDs[sname]; ok {
				sids = append(sids, id)
			}
		}
		out, err := s.agentSvc.UpsertByName(ctx, a.Name, a.Description, a.SystemPrompt, runtimeID, sids)
		if err != nil {
			return nil, s.failImport(ctx, ti, fmt.Sprintf("upsert agent %q: %v", a.Name, err))
		}
		agentIDs[a.Name] = out.ID
	}
	return agentIDs, nil
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
		members = append(members, SquadMember{MemberType: "agent", MemberID: mid, Role: m.Role})
	}
	if _, err := s.squadSvc.UpsertByName(ctx, sq.Name, sq.Description, leaderID, sq.Instructions, members); err != nil {
		return s.failImport(ctx, ti, fmt.Sprintf("upsert squad %q: %v", sq.Name, err))
	}
	return nil
}

// completeImport stamps the result and publishes the completion event.
func (s *TeamImportService) completeImport(ctx context.Context, ti *TeamImport, tj *teamJSON, summary string) error {
	result, _ := json.Marshal(map[string]any{
		"agents":    len(tj.Agents),
		"skills":    len(tj.Skills),
		"has_squad": tj.Squad != nil,
		"summary":   summary,
	})
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE team_import SET status='completed', result=? WHERE id=?`, string(result), ti.ID); err != nil {
		return fmt.Errorf("update team_import status: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "team:imported", Payload: map[string]any{
		"team_import_id": ti.ID, "run_id": ti.RunID,
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
		`SELECT id, run_id, runtime_id, git_url, git_credentials, default_branch, status, result, created_at FROM team_import WHERE run_id=?`, runID).
		Scan(&ti.ID, &ti.RunID, &ti.RuntimeID, &ti.GitURL, &ti.GitCredentials, &ti.DefaultBranch, &ti.Status, &ti.Result, &ti.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ti, nil
}

// sanitizeGitURL strips embedded credentials before logging.
func sanitizeGitURL(raw string) string {
	return gitutil.SanitizeURL(raw)
}
