package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Squad is a routing group: it does no work itself. Assigning a goal to a
// squad or @mentioning a squad routes only to its leader agent, who delegates
// by assigning sub-goals. See DESIGN.md §2. The leader must be an agent;
// squads cannot nest.
type Squad struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	LeaderID     string `json:"leader_id"`
	Instructions string `json:"instructions"`
	CreatedAt    string `json:"created_at"`
}

// SquadMember is one member of a squad. Polymorphic: member_type ∈ {agent,human}.
type SquadMember struct {
	ID         string `json:"id"`
	SquadID    string `json:"squad_id"`
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
	Role       string `json:"role"`
	CreatedAt  string `json:"created_at"`
}

type SquadService struct {
	st  *store.Store
	bus *events.Bus
}

func NewSquadService(st *store.Store, bus *events.Bus) *SquadService {
	return &SquadService{st: st, bus: bus}
}

func (s *SquadService) Create(ctx context.Context, sq Squad) (*Squad, error) {
	if sq.Name == "" {
		return nil, NewValidationError("name is required")
	}
	if sq.LeaderID == "" {
		return nil, NewValidationError("leader_id is required")
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, sq.LeaderID, "leader agent"); err != nil {
		return nil, err
	}
	sq.ID = newID()
	sq.CreatedAt = now()
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO squad (id,name,description,leader_id,instructions,created_at) VALUES (?,?,?,?,?,?)`,
		sq.ID, sq.Name, sq.Description, sq.LeaderID, sq.Instructions, sq.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert squad: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "squad:created", Payload: sq})
	return &sq, nil
}

// UpsertByName creates or updates a squad by name (the team-import path).
// leaderAgentID must be an existing agent id. When the squad exists, its
// description/leader/instructions are updated and ALL members are cleared and
// re-added (the team repo is the source of truth for the roster). When it does
// not exist, a new squad is created. Returns the squad.
func (s *SquadService) UpsertByName(ctx context.Context, name, description, leaderAgentID, instructions string, members []SquadMember) (*Squad, error) {
	if name = strings.TrimSpace(name); name == "" {
		return nil, NewValidationError("squad name is required")
	}
	if leaderAgentID == "" {
		return nil, NewValidationError("leader_agent_id is required")
	}
	var existingID string
	_ = s.st.DB().QueryRowContext(ctx, `SELECT id FROM squad WHERE name=?`, name).Scan(&existingID)
	if existingID != "" {
		if _, err := s.st.DB().ExecContext(ctx,
			`UPDATE squad SET description=?, leader_id=?, instructions=? WHERE id=?`,
			description, leaderAgentID, instructions, existingID); err != nil {
			return nil, fmt.Errorf("update squad %q: %w", name, err)
		}
		if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM squad_member WHERE squad_id=?`, existingID); err != nil {
			return nil, fmt.Errorf("clear squad members: %w", err)
		}
		for _, m := range members {
			m.SquadID = existingID
			m.ID = newID()
			m.CreatedAt = now()
			if _, err := s.st.DB().ExecContext(ctx,
				`INSERT INTO squad_member (id,squad_id,member_type,member_id,role,created_at) VALUES (?,?,?,?,?,?)`,
				m.ID, m.SquadID, m.MemberType, m.MemberID, m.Role, m.CreatedAt); err != nil {
				return nil, fmt.Errorf("insert squad_member: %w", err)
			}
		}
		return s.Get(ctx, existingID)
	}
	sq := Squad{
		Name:         name,
		Description:  description,
		LeaderID:     leaderAgentID,
		Instructions: instructions,
	}
	out, err := s.Create(ctx, sq)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		m.SquadID = out.ID
		if _, err := s.AddMember(ctx, out.ID, m.MemberType, m.MemberID, m.Role); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *SquadService) List(ctx context.Context) ([]Squad, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,name,description,leader_id,instructions,created_at FROM squad ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Squad{}
	for rows.Next() {
		var sq Squad
		if err := rows.Scan(&sq.ID, &sq.Name, &sq.Description, &sq.LeaderID, &sq.Instructions, &sq.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sq)
	}
	return out, rows.Err()
}

func (s *SquadService) Get(ctx context.Context, id string) (*Squad, error) {
	var sq Squad
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,name,description,leader_id,instructions,created_at FROM squad WHERE id=?`, id).
		Scan(&sq.ID, &sq.Name, &sq.Description, &sq.LeaderID, &sq.Instructions, &sq.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sq, nil
}

// Update edits a squad's identity (name / description / leader / instructions).
// A leader change takes effect dynamically: goal ownership is judged against
// the CURRENT leader at reconcile time, so an old leader's in-flight run
// becomes orphaned (its result discarded) — no cancel is sent (leader-change
// termination is a documented follow-up).
func (s *SquadService) Update(ctx context.Context, id string, sq Squad) (*Squad, error) {
	if sq.Name == "" {
		return nil, NewValidationError("name is required")
	}
	if sq.LeaderID == "" {
		return nil, NewValidationError("leader_id is required")
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, id, "squad"); err != nil {
		return nil, err
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, sq.LeaderID, "leader agent"); err != nil {
		return nil, err
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE squad SET name=?, description=?, leader_id=?, instructions=? WHERE id=?`,
		sq.Name, sq.Description, sq.LeaderID, sq.Instructions, id); err != nil {
		return nil, fmt.Errorf("update squad: %w", err)
	}
	return s.Get(ctx, id)
}

// RemoveMember detaches a member from the squad (the leader is stored in
// squad.leader_id, not squad_member — removal only touches ordinary members).
func (s *SquadService) RemoveMember(ctx context.Context, squadID, memberID string) error {
	if _, err := s.st.DB().ExecContext(ctx,
		`DELETE FROM squad_member WHERE squad_id=? AND member_id=?`, squadID, memberID); err != nil {
		return fmt.Errorf("remove squad member: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "squad:member_removed", Payload: map[string]string{"squad_id": squadID, "member_id": memberID}})
	return nil
}

// AddMember attaches a member (agent or human) to a squad.
func (s *SquadService) AddMember(ctx context.Context, squadID, memberType, memberID, role string) (*SquadMember, error) {
	if memberType != "agent" && memberType != "human" {
		return nil, NewValidationError("member_type must be agent or human")
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, squadID, "squad"); err != nil {
		return nil, err
	}
	if memberType == "agent" {
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, memberID, "member agent"); err != nil {
			return nil, err
		}
	}
	m := SquadMember{
		ID:         newID(),
		SquadID:    squadID,
		MemberType: memberType,
		MemberID:   memberID,
		Role:       role,
		CreatedAt:  now(),
	}
	if _, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO squad_member (id,squad_id,member_type,member_id,role,created_at) VALUES (?,?,?,?,?,?)`,
		m.ID, m.SquadID, m.MemberType, m.MemberID, m.Role, m.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert squad_member: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "squad:member_added", Payload: m})
	return &m, nil
}

func (s *SquadService) ListMembers(ctx context.Context, squadID string) ([]SquadMember, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,squad_id,member_type,member_id,role,created_at FROM squad_member WHERE squad_id=? ORDER BY created_at`, squadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SquadMember{}
	for rows.Next() {
		var m SquadMember
		if err := rows.Scan(&m.ID, &m.SquadID, &m.MemberType, &m.MemberID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *SquadService) Delete(ctx context.Context, id string) error {
	// On delete, goals assigned to this squad fall back to human (a squadless
	// goal must not dispatch). The leader agent and members are untouched.
	tx, err := s.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE goal SET assignee_type='human', assignee_id='' WHERE assignee_type='squad' AND assignee_id=?`, id); err != nil {
		return fmt.Errorf("orphan squad goals: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM squad_member WHERE squad_id=?`, id); err != nil {
		return fmt.Errorf("delete squad members: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM squad WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete squad: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.Event{Topic: "squad:deleted", Payload: map[string]string{"id": id}})
	return nil
}

// ── Briefing ──

// rosterRow is one line in a leader's roster, describing a squad member.
type rosterRow struct {
	Name   string
	Kind   string // "agent" | "human"
	Role   string
	Skills []string
	ID     string // member id for the mention URI
	Type   string // "agent" | "human"
}

// BuildLeaderBriefing assembles the "Squad Operating Protocol + Roster +
// Instructions" block injected into a leader run's opening prompt. Mirrors
// multica's squad_briefing.go structure (per DESIGN.md).
//
// ownsStatus gates whether the briefing grants the parent goal's status
// authority: a squad that OWNS the goal (assignee_type==squad &&
// assignee_id==squad.id) may push it to done/active; a squad merely @mentioned
// into someone else's goal is a guest and gets the "do NOT change status"
// clause instead.
func (s *SquadService) BuildLeaderBriefing(ctx context.Context, squadID string, ownsStatus bool) (string, error) {
	sq, err := s.Get(ctx, squadID)
	if err != nil {
		return "", err
	}
	rows, err := s.ListMembers(ctx, squadID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("## Squad Operating Protocol\n\n")
	b.WriteString(squadOperatingProtocolHeader)
	switch {
	case ownsStatus:
		b.WriteString(squadParentStatusOwned)
	default:
		b.WriteString(squadParentStatusNotOwned)
	}
	b.WriteString("\n")

	b.WriteString("### Roster\n\n")
	b.WriteString(s.renderRoster(ctx, sq, rows))

	if strings.TrimSpace(sq.Instructions) != "" {
		b.WriteString("\n## Squad Instructions\n\n")
		b.WriteString(sq.Instructions)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (s *SquadService) renderRoster(ctx context.Context, sq *Squad, members []SquadMember) string {
	// Resolve names + skills per member. A small N+1 here is fine for a
	// single-user system; multica batches it.
	var rows []rosterRow
	// Leader first (the leader may also be a member; render once).
	leaderName := s.agentName(ctx, sq.LeaderID)
	if leaderName != "" {
		rows = append(rows, rosterRow{Name: leaderName, Kind: "agent", Role: "leader", Skills: s.agentSkillNames(ctx, sq.LeaderID), ID: sq.LeaderID, Type: "agent"})
	}
	for _, m := range members {
		if m.MemberID == sq.LeaderID && m.MemberType == "agent" {
			continue // already rendered as leader
		}
		switch m.MemberType {
		case "agent":
			name := s.agentName(ctx, m.MemberID)
			if name == "" {
				continue // vanished agent — skip
			}
			rows = append(rows, rosterRow{Name: name, Kind: "agent", Role: m.Role, Skills: s.agentSkillNames(ctx, m.MemberID), ID: m.MemberID, Type: "agent"})
		case "human":
			rows = append(rows, rosterRow{Name: "human:" + m.MemberID, Kind: "human", Role: m.Role, ID: m.MemberID, Type: "human"})
		}
	}
	if len(rows) == 0 {
		return "(empty squad)\n"
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "- %s — %s", r.Name, r.Kind)
		if r.Role != "" {
			fmt.Fprintf(&b, ", role: %q", r.Role)
		}
		if r.Kind == "agent" {
			if len(r.Skills) > 0 {
				fmt.Fprintf(&b, " — skills: %s", strings.Join(r.Skills, ", "))
			} else {
				b.WriteString(" — no skills assigned")
			}
		}
		fmt.Fprintf(&b, " — [@%s](mention://%s/%s)\n", r.Name, r.Type, r.ID)
	}
	return b.String()
}

// agentSkillNames resolves an agent's selected skills (agent.skills JSON →
// skill table names) for the roster — the leader divides work by what
// members can actually do.
func (s *SquadService) agentSkillNames(ctx context.Context, agentID string) []string {
	var raw string
	if err := s.st.DB().QueryRowContext(ctx,
		`SELECT skills FROM agent WHERE id=?`, agentID).Scan(&raw); err != nil || raw == "" || raw == "[]" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil || len(ids) == 0 {
		return nil
	}
	var names []string
	for _, id := range ids {
		var name string
		if err := s.st.DB().QueryRowContext(ctx, `SELECT name FROM skill WHERE id=?`, id).Scan(&name); err == nil && name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *SquadService) agentName(ctx context.Context, agentID string) string {
	var name string
	err := s.st.DB().QueryRowContext(ctx, `SELECT name FROM agent WHERE id=?`, agentID).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

// Briefing text constants. Echo multica's two-lane owns/guest distinction
// (briefing injection vs. status authority — DESIGN.md + multica
// squad_briefing.go). Wording is plain so it's easy to tune.

const squadOperatingProtocolHeader = `You are the LEADER (coordinator) of a squad. Break the goal into parts and
dispatch them with the agentwork CLI:
  agentwork subgoal create --title T --assignee <agent-id> [--description D]
(each work item runs on its own worktree with machine verification and
produces a Change; the platform wakes you when changes are ready — merge
each with ` + "`agentwork change integrate <id>`" + `). Ask teammates (read-only
consult; the platform resumes you after the answer) by commenting a
mention:
  agentwork goal comment --text "[@Name](mention://agent/<id>)"
You do NOT implement the whole task yourself — members do the work;
members are NOT auto-dispatched, you delegate explicitly.

REVIEWER-ONLY RULE: members with role="reviewer" REVIEW ONLY — never
dispatch work items to them (subgoal create rejects it). After your turn
ends the platform automatically pulls the reviewers in, then the human
approves; you never hand work to a reviewer yourself.

FINAL MESSAGE = your run report: your final message is posted to the feed
verbatim when your turn ends. If this turn only dispatched sub-goals, end
with AT MOST one short sentence, e.g. "派发完成，等待变更。" — the platform
already wrote the dispatch comments, so do NOT repeat the delegation, do
NOT recap the plan, and NEVER write ids (goal/sub-goal/agent/squad ids are
system handles — humans cannot use them and read the feed as prose).
`

const squadParentStatusOwned = `You drive this goal's execution. When you finish your part, END your turn —
completion is JUDGED, not declared: the platform's machine verification +
the domain's gate rules + the human's approval decide whether the goal is
done. You never set the goal's status yourself, and you do not wait for a
"final confirmation" to close it — your run ending is your part done, the
rest is the platform's and the human's.
`

const squadParentStatusNotOwned = `You were consulted on a goal someone else owns (guest run, READ-ONLY — your
edits are discarded by the platform). Do NOT change this goal's status, do NOT
hand it off, do NOT split it — answer the question via the agentwork goal
comment command and end your turn. No agent sets goal status; completion is
judged by the platform + human.
`
