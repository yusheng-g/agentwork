package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Agent is a runtime + a persona. Under the per-task connection model, creating
// an agent does NOT launch a process: a fresh connection is opened per run.
// status/pid columns are deliberately gone — they belong to the future
// long-lived-session model and would be dead columns today.
type Agent struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	// Description is a human-facing one-liner shown in the web list — what
	// this agent does in a glance. Distinct from SystemPrompt (the persona
	// shipped to the model): description is for the operator browsing agents,
	// not the agent itself.
	Description  string            `json:"description"`
	RuntimeID    string            `json:"runtime_id"`
	SystemPrompt string            `json:"system_prompt"`
	Model        string            `json:"model"`
	Env          map[string]string `json:"env"`
	// McpServers are EXTRA MCP servers advertised at session/new alongside
	// the platform's workspace server — the agent's own tools (browser,
	// database, an external ACP agent via an MCP bridge, ...). Type speaks
	// acp.McpServer (type stdio|http|sse, name, url or command/args).
	McpServers    []acp.McpServer `json:"mcp_servers"`
	// Skills are the platform-managed skill ids selected for this agent
	// (CLI 分支 Phase 4) — pushed to the agent's machine via config.push.
	Skills        []string        `json:"skills"`
	MaxConcurrent int             `json:"max_concurrent"`
	CreatedAt     string          `json:"created_at"`
}

type AgentService struct {
	st  *store.Store
	bus *events.Bus
}

func NewAgentService(st *store.Store, bus *events.Bus) *AgentService {
	return &AgentService{st: st, bus: bus}
}

func (s *AgentService) Create(ctx context.Context, a Agent) (*Agent, error) {
	if a.Name == "" || a.RuntimeID == "" {
		return nil, NewValidationError("name and runtime_id are required")
	}
	// Verify runtime exists.
	var rtID string
	err := s.st.DB().QueryRowContext(ctx, `SELECT id FROM runtime WHERE id=?`, a.RuntimeID).Scan(&rtID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	if a.MaxConcurrent < 1 {
		a.MaxConcurrent = 1
	}
	if a.McpServers == nil {
		a.McpServers = []acp.McpServer{}
	}
	if a.Skills == nil {
		a.Skills = []string{}
	}
	a.ID = newID()
	a.CreatedAt = now()
	envJSON, _ := json.Marshal(a.Env)
	mcpJSON, _ := json.Marshal(a.McpServers)
	skillsJSON, _ := json.Marshal(a.Skills)
	_, err = s.st.DB().ExecContext(ctx,
		`INSERT INTO agent (id,name,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Description, a.RuntimeID, a.SystemPrompt, a.Model, string(envJSON), string(mcpJSON), skillsJSON, a.MaxConcurrent, a.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert agent: %w", err)
	}
	// Notify the daemon that an agent exists (it rebuilds per-agent workers on
	// startup; this is the live-create path). Under the per-task model no
	// process is launched here.
	s.bus.Publish(ctx, events.Event{Topic: "agent:created", Payload: a})
	return &a, nil
}

// Update edits an agent's identity/persona (name, system_prompt,
// max_concurrent, env). A changed system_prompt takes effect on the agent's
// NEXT run — in-flight runs keep their snapshot.
func (s *AgentService) Update(ctx context.Context, id string, a Agent) (*Agent, error) {
	if a.Name == "" {
		return nil, NewValidationError("name is required")
	}
	if a.MaxConcurrent < 1 {
		a.MaxConcurrent = 1
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, id, "agent"); err != nil {
		return nil, err
	}
	envJSON, _ := json.Marshal(a.Env)
	mcpJSON, _ := json.Marshal(a.McpServers)
	skillsJSON, _ := json.Marshal(a.Skills)
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE agent SET name=?, description=?, system_prompt=?, max_concurrent=?, env=?, mcp_servers=?, skills=? WHERE id=?`,
		a.Name, a.Description, a.SystemPrompt, a.MaxConcurrent, string(envJSON), string(mcpJSON), string(skillsJSON), id); err != nil {
		return nil, fmt.Errorf("update agent: %w", err)
	}
	return s.Get(ctx, id)
}

// UpsertByName creates or updates an agent by name (the team-import path).
// When the agent exists, its description/system_prompt/skills are updated;
// runtime_id is left unchanged (the team repo does not define machines). When
// it does not exist, a new agent is created bound to the supplied runtimeID.
// Returns the agent.
func (s *AgentService) UpsertByName(ctx context.Context, name, description, systemPrompt, runtimeID string, skillIDs []string) (*Agent, error) {
	if name = strings.TrimSpace(name); name == "" {
		return nil, NewValidationError("agent name is required")
	}
	var existingID string
	_ = s.st.DB().QueryRowContext(ctx, `SELECT id FROM agent WHERE name=?`, name).Scan(&existingID)
	if existingID != "" {
		if skillIDs == nil {
			skillIDs = []string{}
		}
		skillsJSON, _ := json.Marshal(skillIDs)
		if _, err := s.st.DB().ExecContext(ctx,
			`UPDATE agent SET description=?, system_prompt=?, skills=? WHERE id=?`,
			description, systemPrompt, string(skillsJSON), existingID); err != nil {
			return nil, fmt.Errorf("update agent %q: %w", name, err)
		}
		return s.Get(ctx, existingID)
	}
	if runtimeID == "" {
		return nil, NewValidationError(fmt.Sprintf("runtime_id is required for new agent %q", name))
	}
	a := Agent{
		Name:         name,
		Description:  description,
		RuntimeID:    runtimeID,
		SystemPrompt: systemPrompt,
		Skills:       skillIDs,
		MaxConcurrent: 1,
	}
	return s.Create(ctx, a)
}

func (s *AgentService) List(ctx context.Context) ([]Agent, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at
		 FROM agent ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		var envJSON, mcpJSON, skillsJSON string
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &envJSON, &mcpJSON, &skillsJSON, &a.MaxConcurrent, &a.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(envJSON), &a.Env)
		_ = json.Unmarshal([]byte(mcpJSON), &a.McpServers)
		_ = json.Unmarshal([]byte(skillsJSON), &a.Skills)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *AgentService) Get(ctx context.Context, id string) (*Agent, error) {
	var a Agent
	var envJSON, mcpJSON, skillsJSON string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,name,description,runtime_id,system_prompt,model,env,mcp_servers,skills,max_concurrent,created_at
		 FROM agent WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Description, &a.RuntimeID, &a.SystemPrompt, &a.Model, &envJSON, &mcpJSON, &skillsJSON, &a.MaxConcurrent, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(envJSON), &a.Env)
	_ = json.Unmarshal([]byte(mcpJSON), &a.McpServers)
	_ = json.Unmarshal([]byte(skillsJSON), &a.Skills)
	return &a, nil
}

func (s *AgentService) Delete(ctx context.Context, id string) error {
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Refuse if this agent leads a squad — forcing the caller to fix the squad
	// first is safer than silently orphaning it (squad.leader_id is RESTRICT).
	var squadCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM squad WHERE leader_id=?`, id).Scan(&squadCount); err != nil {
		return fmt.Errorf("check leader role: %w", err)
	}
	if squadCount > 0 {
		return NewValidationError(fmt.Sprintf("agent %s leads %d squad(s); delete or reassign them first", id, squadCount))
	}
	// chat_message references run; run references agent + goal. Schedules and
	// goals reference agent by id (no FK) — null those assignee pointers so
	// the goals survive as human-assigned rather than dangling.
	for _, stmt := range []string{
		`DELETE FROM chat_message WHERE run_id IN (SELECT id FROM run WHERE agent_id=?)`,
		`DELETE FROM run WHERE agent_id=?`,
		`DELETE FROM squad_member WHERE member_type='agent' AND member_id=?`,
		`DELETE FROM schedule_run WHERE schedule_id IN (SELECT id FROM schedule WHERE assignee_type='agent' AND assignee_id=?)`,
		`DELETE FROM schedule WHERE assignee_type='agent' AND assignee_id=?`,
		// v2 (决策 6-1): sub_goal has no FK on assignee/verifier — a deleted
		// agent must not leave work items that later enqueue a run on a dead
		// agent id (the run's agent FK would reject it and the sub-goal would
		// stall running forever). Cancel the agent's non-terminal sub-goals;
		// terminal history (verified/failed/cancelled) stays, same as
		// CancelSubGoal.
		`UPDATE sub_goal SET status='cancelled' WHERE (assignee_id=?1 OR verifier_id=?1) AND status NOT IN ('verified','cancelled','failed')`,
		`UPDATE goal SET assignee_type='human', assignee_id='' WHERE assignee_type='agent' AND assignee_id=?`,
		`DELETE FROM agent WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("delete agent dependents: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Tell the daemon to drop this agent's worker.
	s.bus.Publish(ctx, events.Event{Topic: "agent:deleted", Payload: map[string]string{"id": id}})
	return nil
}
