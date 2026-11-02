package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/gitcodeapi"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/store"
	"gopkg.in/yaml.v3"
)

// templateSpec is the shared envelope of both template kinds: api_version +
// metadata + spec. The spec is decoded per kind (squadSpec / projectSpec)
// with KnownFields(true) — a misspelled field must fail loudly, not silently
// drop its config (R5 in TEMPLATE-PLAN.md).
type templateSpec struct {
	APIVersion string `yaml:"api_version"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		ID          string   `yaml:"id"`
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Version     string   `yaml:"version"`
		Tags        []string `yaml:"tags"`
		Icon        string   `yaml:"icon"`
	} `yaml:"metadata"`
	Spec yaml.Node `yaml:"spec"`
}

// ConflictStrategy picks the name-collision behavior: create fails loudly on
// an existing name (the existing AW codes surface); upsert updates by name
// (the team-import semantics). Default upsert.
const (
	conflictCreate = "create"
	conflictUpsert = "upsert"
)

// ── squad-template spec ──

type squadSpec struct {
	Strategy     string             `yaml:"strategy"` // create | upsert (default upsert)
	Agents       []tplAgent         `yaml:"agents"`
	Squad        tplSquad           `yaml:"squad"`
	SuggestedGoal *tplSuggestedGoal `yaml:"suggested_goal"` // optional: pre-fill the goal create dialog after apply
}

// tplSuggestedGoal is the suggested first-goal prompt a template carries —
// after applying an agent/squad template the frontend jumps to the goal create
// page with this title+description pre-filled (user edits then confirms).
type tplSuggestedGoal struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
}

// ── agent-template spec ──

// agentSpec defines a standalone single-agent template (no squad, no project).
// apply creates the agent + a same-named scratch domain (the goal default
// project) and returns the suggested goal prompt.
type agentSpec struct {
	Strategy      string             `yaml:"strategy"` // create | upsert (default upsert)
	Agent         tplAgent           `yaml:"agent"`
	SuggestedGoal *tplSuggestedGoal  `yaml:"suggested_goal"`
}

// tplAgent is one agent to create/update. Runtime names the runtime
// (mismatched/absent → first active runtime — team_import.resolveRuntime
// semantics); MaxConcurrent defaults to the service's own default.
type tplAgent struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	SystemPrompt  string            `yaml:"system_prompt"`
	Skills        []string          `yaml:"skills"`
	Runtime       string            `yaml:"runtime"`
	MaxConcurrent int               `yaml:"max_concurrent"`
	Model         string            `yaml:"model"`
	Env           map[string]string `yaml:"env"`
	McpServers    []map[string]any  `yaml:"mcp_servers"`
}

type tplSquad struct {
	Name         string      `yaml:"name"`
	Description  string      `yaml:"description"`
	Leader       string      `yaml:"leader"`
	Instructions string      `yaml:"instructions"`
	Members      []tplMember `yaml:"members"`
}

type tplMember struct {
	Name string `yaml:"name"`
	Role string `yaml:"role"`
}

// ── project-template spec ──

type projectSpec struct {
	Repo      *tplRepoCreate `yaml:"repo"` // nil = user supplies git_url
	Domain    tplDomain      `yaml:"domain"`
	Team      *squadSpec     `yaml:"team"` // optional agents+squad riding the project
	Goals     []tplGoal      `yaml:"goals"`
	Schedules []tplSchedule  `yaml:"schedules"`
}

type tplRepoCreate struct {
	Create            bool   `yaml:"create"`
	Visibility        string `yaml:"visibility"` // private | public
	AutoInit          *bool  `yaml:"auto_init"`  // nil = true (an empty repo cannot pass the git probe)
	GitignoreTemplate string `yaml:"gitignore_template"`
	Org               string `yaml:"org"` // '' = token's user
	// Name/Description are NOT template fields — apply stamps them from the
	// (required) domain name. They exist on the struct so applyRepo can carry
	// the overlay input as one value.
	Name        string `yaml:"-"`
	Description string `yaml:"-"`
}

type tplDomain struct {
	Type              string `yaml:"type"` // repo | scratch (default repo)
	DefaultBranch     string `yaml:"default_branch"`
	GitIdentity       string `yaml:"git_identity"`
	IssueAssignee     string `yaml:"issue_assignee"`
	IssueAssigneeType string `yaml:"issue_assignee_type"`
	PolicyText        string `yaml:"policy_text"`
}

type tplGoal struct {
	Title        string `yaml:"title"`
	Description  string `yaml:"description"`
	Assignee     string `yaml:"assignee"`      // agent or squad name
	AssigneeType string `yaml:"assignee_type"` // agent | squad (default agent)
	Start        bool   `yaml:"start"`         // true = active (executes immediately); false = backlog
}

type tplSchedule struct {
	Name         string `yaml:"name"`
	Title        string `yaml:"title"`
	Description  string `yaml:"description"`
	Cron         string `yaml:"cron"`
	Assignee     string `yaml:"assignee"`
	AssigneeType string `yaml:"assignee_type"` // agent | squad (default agent)
}

// ── apply requests / results ──

// ApplySquadOverrides are the user's per-apply inputs (the dialog's fields).
type ApplySquadOverrides struct {
	// SquadName overrides spec.squad.name ('' = template value).
	SquadName string `json:"squad_name,omitempty"`
	// Rename maps template agent names to per-apply names ({"backend-leader":
	// "订单-负责人"}). Unlisted names keep their template value.
	Rename map[string]string `json:"rename,omitempty"`
	// Strategy overrides spec.strategy.
	Strategy string `json:"strategy,omitempty"`
}

// ApplyProjectOverrides are the project dialog's fields.
type ApplyProjectOverrides struct {
	Name           string            `json:"name"`              // required: the domain name
	GitURL         string            `json:"git_url,omitempty"` // required unless spec.repo.create
	GitCredentials string            `json:"git_credentials,omitempty"`
	RepoToken      string            `json:"repo_token,omitempty"`     // the repo-creation token (gitcode)
	TemplateToken  string            `json:"template_token,omitempty"` // unused by apply (list phase); kept for symmetry
	Repo           *tplRepoCreate    `json:"repo,omitempty"`           // overlay on spec.repo
	Rename         map[string]string `json:"rename,omitempty"`
	Strategy       string            `json:"strategy,omitempty"`
	GoalStart      *bool             `json:"goal_start,omitempty"` // overlay on every goal's start
}

// ApplyItem is one entity's per-step outcome.
type ApplyItem struct {
	Kind   string `json:"kind"` // skill | agent | squad | domain | repo | goal | schedule
	Name   string `json:"name"`
	ID     string `json:"id,omitempty"`
	Action string `json:"action"` // created | updated | skipped | failed
	Error  string `json:"error,omitempty"`
}

// ApplyResult is the apply response: the created roots + every step.
type ApplySquadResult struct {
	Squad         *Squad            `json:"squad,omitempty"`
	Domain        *Domain           `json:"domain,omitempty"`        // auto-created scratch domain (goal default project)
	Agents        []*Agent          `json:"agents,omitempty"`
	Skills        []*Skill          `json:"skills,omitempty"`
	SuggestedGoal *tplSuggestedGoal `json:"suggested_goal,omitempty"` // pre-fill the goal create dialog after apply
	Items         []ApplyItem       `json:"items"`
}

type ApplyProjectResult struct {
	Domain    *Domain     `json:"domain,omitempty"`
	RepoURL   string      `json:"repo_url,omitempty"` // set when the repo was created
	Agents    []*Agent    `json:"agents,omitempty"`
	Squad     *Squad      `json:"squad,omitempty"`
	Goals     []*Goal     `json:"goals,omitempty"`
	Schedules []*Schedule `json:"schedules,omitempty"`
	Items     []ApplyItem `json:"items"`
}

// ApplyAgentOverrides are the user's per-apply inputs for an agent template.
type ApplyAgentOverrides struct {
	// AgentName overrides spec.agent.name ('' = template value).
	AgentName string `json:"agent_name,omitempty"`
	// Strategy overrides spec.strategy.
	Strategy string `json:"strategy,omitempty"`
}

// ApplyAgentResult is the apply-agent response: the created agent + the
// auto-created scratch domain (the goal default project) + the suggested goal
// prompt the frontend pre-fills.
type ApplyAgentResult struct {
	Domain        *Domain            `json:"domain,omitempty"`
	Agent         *Agent             `json:"agent,omitempty"`
	Skills        []*Skill           `json:"skills,omitempty"`
	SuggestedGoal *tplSuggestedGoal  `json:"suggested_goal,omitempty"`
	Items         []ApplyItem        `json:"items"`
}

// TemplateApplyService orchestrates a template apply through the existing
// services (the same layer the HTTP CRUD handlers call — no shortcuts).
// Failure policy: best-effort sequence with per-item results; the domain/repo
// step (the apply's root) is terminal — failing it aborts the rest.
type TemplateApplyService struct {
	st         *store.Store
	templates  *TemplateService
	agentSvc   *AgentService
	skillSvc   *SkillService
	squadSvc   *SquadService
	domainSvc  *DomainService
	goalSvc    *GoalService
	schedSvc   *ScheduleService
	gitTester  GitTester
	repoProv   gitcodeapi.RepoProvider
	pushSkills func(agentID string) // ships persona+skills to the machine (the daemon's PushAgentSkills); nil in tests
	// pushSkillUpdate re-pushes one skill's CURRENT archive to every agent
	// that has it selected (the daemon's PushSkillUpdate). The stale-copy gap
	// it closes: a skill whose files are rewritten by UpsertByName (the
	// template's embedded package wins over the library copy) only reached
	// machines on the next agent edit/connect, so every run in between staged
	// the OLD version while the library showed the new one. nil in tests.
	pushSkillUpdate func(skillID string)
}

// GitTester abstracts the daemon's TestDomainGit probe (the create-domain
// gate) so the service stays daemon-free in tests.
type GitTester interface {
	TestDomainGit(ctx context.Context, gitURL, defaultBranch, credentials string) *DomainGitProbeResult
}

// DomainGitProbeResult mirrors daemon.DomainGitTestResult (the daemon package
// imports service, so the interface uses this service-side copy — one field
// mapping at the wiring site).
type DomainGitProbeResult struct {
	OK             bool
	BranchExists   bool
	ResolvedBranch string
	Refs           []string
	Error          string
}

// NewTemplateApplyService wires the orchestration (nil repoProv = repo
// creation unavailable → ApplyProject with spec.repo.create fails cleanly).
func NewTemplateApplyService(
	st *store.Store,
	templates *TemplateService,
	agentSvc *AgentService,
	skillSvc *SkillService,
	squadSvc *SquadService,
	domainSvc *DomainService,
	goalSvc *GoalService,
	schedSvc *ScheduleService,
	gitTester GitTester,
) *TemplateApplyService {
	return &TemplateApplyService{
		st: st, templates: templates, agentSvc: agentSvc, skillSvc: skillSvc,
		squadSvc: squadSvc, domainSvc: domainSvc, goalSvc: goalSvc,
		schedSvc: schedSvc, gitTester: gitTester,
		repoProv: gitcodeapi.New(),
	}
}

// SetGitTester wires the daemon's git probe after construction (the daemon
// is created after the services in main.go — same late-wiring pattern as
// SetDependencies). nil = the probe is skipped (tests).
func (s *TemplateApplyService) SetGitTester(t GitTester) { s.gitTester = t }

// SetSkillPusher wires the daemon's PushAgentSkills so apply can ship persona
// + skills to the machine right after creating/updating an agent — the same
// step the HTTP CRUD handlers do (go h.Daemon.PushAgentSkills(...)). nil
// (tests) = no immediate push; the next machine register back-fills.
func (s *TemplateApplyService) SetSkillPusher(p func(agentID string)) { s.pushSkills = p }

// SetSkillUpdatePusher wires the daemon's PushSkillUpdate so a skill whose
// files UpsertByName rewrote is re-pushed to every agent that has it selected
// (detached + best-effort, like SetSkillPusher). nil (tests) = no re-push;
// the next agent edit/connect back-fills.
func (s *TemplateApplyService) SetSkillUpdatePusher(p func(skillID string)) { s.pushSkillUpdate = p }

// ── parsing helpers ──

// parseTemplate decodes and validates a template YAML document into its meta
// + decoded spec (strict fields per kind).
func parseTemplate(raw string) (TemplateMeta, any, error) {
	var spec templateSpec
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return TemplateMeta{}, nil, fmt.Errorf("模板格式错误：%v", err)
	}
	if spec.APIVersion != "" && spec.APIVersion != "agentwork/v1" {
		return TemplateMeta{}, nil, fmt.Errorf("不支持的 api_version %q", spec.APIVersion)
	}
	if spec.Metadata.ID == "" {
		return TemplateMeta{}, nil, fmt.Errorf("模板缺少 metadata.id")
	}
	// Decode spec per kind with strict fields. yaml.Node.Decode does not
	// inherit the document decoder's KnownFields, so the spec node is
	// re-encoded through a strict decoder explicitly.
	var out any
	strictSpec := func(node yaml.Node, v any) error {
		buf, err := yaml.Marshal(&node)
		if err != nil {
			return err
		}
		d := yaml.NewDecoder(bytes.NewReader(buf))
		d.KnownFields(true)
		return d.Decode(v)
	}
	switch spec.Kind {
	case TemplateKindSquad:
		sq := &squadSpec{}
		if err := strictSpec(spec.Spec, sq); err != nil {
			return TemplateMeta{}, nil, fmt.Errorf("spec 校验失败：%v", err)
		}
		out = sq
	case TemplateKindProject:
		pr := &projectSpec{}
		if err := strictSpec(spec.Spec, pr); err != nil {
			return TemplateMeta{}, nil, fmt.Errorf("spec 校验失败：%v", err)
		}
		out = pr
	case TemplateKindAgent:
		ag := &agentSpec{}
		if err := strictSpec(spec.Spec, ag); err != nil {
			return TemplateMeta{}, nil, fmt.Errorf("spec 校验失败：%v", err)
		}
		out = ag
	default:
		return TemplateMeta{}, nil, fmt.Errorf("模板 kind %q 不受支持", spec.Kind)
	}
	meta := TemplateMeta{
		APIVersion: spec.APIVersion, Kind: spec.Kind,
		ID: spec.Metadata.ID, Name: spec.Metadata.Name,
		Description: spec.Metadata.Description, Version: spec.Metadata.Version,
	}
	return meta, out, nil
}

// applyStrategy resolves the effective conflict strategy.
func applyStrategy(specStrategy, override string) string {
	if override == conflictCreate || override == conflictUpsert {
		return override
	}
	if specStrategy == conflictCreate {
		return conflictCreate
	}
	return conflictUpsert
}

// rename applies the rename map: the mapped name wins; unmapped keep theirs.
func rename(m map[string]string, name string) string {
	if n, ok := m[name]; ok && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(n)
	}
	return name
}

// resolveSkillID maps a template skill reference to a platform skill id.
// Order: an embedded package from the template snapshot (upserted by name) →
// an existing platform skill by name. ok=false when neither resolves.
func (s *TemplateApplyService) resolveSkillID(ctx context.Context, name string) (string, bool, error) {
	if pkg, ok, err := s.templates.SkillPackage(ctx, name); err != nil {
		return "", false, err
	} else if ok {
		sk, err := s.skillSvc.UpsertByName(ctx, name, "", pkg)
		if err != nil {
			return "", false, fmt.Errorf("导入模板内嵌 skill %q：%v", name, err)
		}
		// The skill's files were just (re)written — re-push it to every agent
		// that has it selected so their machines' staging refreshes (nil in
		// tests; a NEW skill has no selectors yet → the re-push is a no-op).
		if s.pushSkillUpdate != nil {
			s.pushSkillUpdate(sk.ID)
		}
		return sk.ID, true, nil
	}
	var id string
	err := s.st.DB().QueryRowContext(ctx, `SELECT id FROM skill WHERE name=?`, name).Scan(&id)
	if err == nil && id != "" {
		return id, true, nil
	}
	return "", false, nil
}

// resolveAgentSkillIDs resolves every skill reference for one agent; unknown
// names fail the agent (a silent drop would ship an agent missing its tools).
func (s *TemplateApplyService) resolveAgentSkillIDs(ctx context.Context, skills []string) ([]string, error) {
	var ids []string
	for _, name := range skills {
		id, ok, err := s.resolveSkillID(ctx, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("skill %q 不存在（平台与模板包中都没有）", name)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// usableRuntimeIDs lists runtime ids whose owning machine is online — the
// same availability test the claim gate uses (run.go Claim: runtime not
// absent AND machine connected). A runtime whose machine went offline is
// still status='active' in the DB (offline is machine-side, read-derived),
// so filtering only on runtime.status would bind agents to dead machines
// that Claim then refuses to dispatch. Legacy/local runtimes (machine_id='')
// are always usable.
func (s *TemplateApplyService) usableRuntimeIDs(ctx context.Context) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT r.id FROM runtime r
		 LEFT JOIN machine m ON m.id = r.machine_id
		 WHERE r.status='active' AND (r.machine_id='' OR m.status='connected')
		 ORDER BY r.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// resolveRuntime maps a template runtime name to an id. The name lookup
// applies the same availability test as the claim gate (runtime not absent
// AND machine connected) — a name that resolves to an offline machine's
// runtime is treated as unmatched, falling through to a usable runtime
// instead of binding the agent to a dead machine. Empty/unmatched/
// offline-name all fall back to a RANDOM pick from the usable pool (spread
// load across connected machines instead of concentrating on the
// earliest-created runtime). Requires at least one usable runtime — there
// is nowhere to run otherwise.
func (s *TemplateApplyService) resolveRuntime(ctx context.Context, name string, usableIDs []string) (string, error) {
	if name != "" {
		var id string
		if err := s.st.DB().QueryRowContext(ctx,
			`SELECT r.id FROM runtime r
			 LEFT JOIN machine m ON m.id = r.machine_id
			 WHERE r.name=? AND r.status='active'
			   AND (r.machine_id='' OR m.status='connected')`, name).Scan(&id); err == nil && id != "" {
			return id, nil
		}
	}
	if len(usableIDs) == 0 {
		return "", NewValidationError("没有可用的 runtime——所有机器离线或未连接，请先 agentwork connect")
	}
	return pickRandom(usableIDs), nil
}

// agentIDByName resolves a platform agent id by exact name (” = not found).
func (s *TemplateApplyService) agentIDByName(ctx context.Context, name string) string {
	var id string
	_ = s.st.DB().QueryRowContext(ctx, `SELECT id FROM agent WHERE name=?`, name).Scan(&id)
	return id
}

// squadByName resolves a platform squad by exact name (nil = not found).
func (s *TemplateApplyService) squadByName(ctx context.Context, name string) (*Squad, error) {
	all, err := s.squadSvc.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, nil
}

// upsertAgent creates or updates one agent per strategy; returns the agent
// and the action label. It builds the full Agent{} (every tplAgent field
// carried through, mcp_servers converted to []acp.McpServer) and calls the
// same AgentService.Create / Update the HTTP CRUD handlers use — so the
// template's env/model/mcp_servers/max_concurrent land in the DB the same way
// a manually-created agent's do. The upsert path does NOT use UpsertByName
// (that team-import method's narrow signature drops those four fields).
func (s *TemplateApplyService) upsertAgent(ctx context.Context, strategy string, a tplAgent, finalName string, usableIDs []string) (*Agent, string, error) {
	skillIDs, err := s.resolveAgentSkillIDs(ctx, a.Skills)
	if err != nil {
		return nil, "", err
	}
	runtimeID, err := s.resolveRuntime(ctx, a.Runtime, usableIDs)
	if err != nil {
		return nil, "", err
	}
	desired := Agent{
		Name: finalName, Description: a.Description, RuntimeID: runtimeID,
		SystemPrompt: a.SystemPrompt, Model: a.Model, Env: a.Env,
		McpServers: tplMcpServersToAcp(a.McpServers),
		Skills: skillIDs, MaxConcurrent: a.MaxConcurrent,
	}
	if strategy == conflictCreate {
		out, err := s.agentSvc.Create(ctx, desired)
		if err != nil {
			return nil, "", err
		}
		return out, "created", nil
	}
	// Upsert by name: existing → Update (writes all 10 mutable columns); new
	// → Create. Both carry the template's full field set, same as the handlers.
	if existing := s.agentIDByName(ctx, finalName); existing != "" {
		out, err := s.agentSvc.Update(ctx, existing, desired)
		if err != nil {
			return nil, "", err
		}
		return out, "updated", nil
	}
	out, err := s.agentSvc.Create(ctx, desired)
	if err != nil {
		return nil, "", err
	}
	return out, "created", nil
}

// tplMcpServersToAcp converts the loose YAML shape ([]map[string]any, as
// decoded from tplAgent.mcp_servers) into the strongly-typed []acp.McpServer
// the Agent model stores. The two share field names (name/type/command/args/
// url/env/headers), so a marshal→unmarshal round-trip does the conversion
// without hand-mapping each field. Malformed entries are dropped (a config
// typo is logged and skipped at run time already — never fatal to the apply).
func tplMcpServersToAcp(raw []map[string]any) []acp.McpServer {
	if len(raw) == 0 {
		return nil
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		logging.Infof("template-apply: marshal mcp_servers: %v", err)
		return nil
	}
	var out []acp.McpServer
	if err := json.Unmarshal(buf, &out); err != nil {
		logging.Infof("template-apply: parse mcp_servers: %v", err)
		return nil
	}
	return out
}

// applySquadSpec is the shared squad/team section (used by both apply kinds).
// teamFinalName overrides the squad name (” = template value with rename).
func (s *TemplateApplyService) applySquadSpec(ctx context.Context, spec *squadSpec, strategy string, renameMap map[string]string, teamFinalName string) (*ApplySquadResult, error) {
	if len(spec.Agents) == 0 {
		return nil, NewValidationError("模板 spec.agents 不能为空")
	}
	res := &ApplySquadResult{}
	usableIDs, err := s.usableRuntimeIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出可用 runtime：%v", err)
	}
	// Skill packages ride first so agent skill resolution sees fresh ids.
	agentIDs := map[string]string{}
	agentNameSet := map[string]bool{}
	for _, a := range spec.Agents {
		finalName := rename(renameMap, a.Name)
		if agentNameSet[finalName] {
			res.Items = append(res.Items, ApplyItem{Kind: "agent", Name: finalName, Action: "failed", Error: "模板内 agent 名重复"})
			continue
		}
		agentNameSet[finalName] = true
		out, action, err := s.upsertAgent(ctx, strategy, a, finalName, usableIDs)
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "agent", Name: finalName, Action: "failed", Error: err.Error()})
			continue
		}
		agentIDs[a.Name] = out.ID // roster references use TEMPLATE names
		res.Items = append(res.Items, ApplyItem{Kind: "agent", Name: finalName, ID: out.ID, Action: action})
		res.Agents = append(res.Agents, out)
		// Ship persona + skills to the machine now — the HTTP CRUD handlers
		// do the same (go h.Daemon.PushAgentSkills(...)). Without this an
		// apply-triggered goal run can start before the machine has the
		// AGENTS.md / skill dirs. Detached + best-effort; nil in tests.
		if s.pushSkills != nil {
			go s.pushSkills(out.ID)
		}
	}
	if len(agentIDs) == 0 {
		return res, NewValidationError("模板中没有任何 agent 创建成功，小队不创建")
	}
	// Squad: the leader must resolve among the CREATED agents. The squad is
	// the apply's root — a missing leader aborts the rest (the per-agent
	// items above remain reported).
	leaderID, ok := agentIDs[spec.Squad.Leader]
	if !ok {
		res.Items = append(res.Items, ApplyItem{Kind: "squad", Name: rename(renameMap, spec.Squad.Name), Action: "failed", Error: fmt.Sprintf("squad.leader %q 未在模板 agents 中定义或创建失败", spec.Squad.Leader)})
		return res, NewValidationError(fmt.Sprintf("squad.leader %q 未在模板 agents 中定义或创建失败", spec.Squad.Leader))
	}
	squadName := teamFinalName
	if squadName == "" {
		squadName = rename(renameMap, spec.Squad.Name)
	}
	var members []SquadMember
	for _, m := range spec.Squad.Members {
		if m.Name == spec.Squad.Leader {
			continue // leader is squad.leader_id, never a member row (platform invariant)
		}
		mid, ok := agentIDs[m.Name]
		if !ok {
			res.Items = append(res.Items, ApplyItem{Kind: "member", Name: rename(renameMap, m.Name), Action: "failed", Error: "agent 未创建成功，成员未加入"})
			continue
		}
		role := m.Role
		if role == "" {
			role = "member"
		}
		members = append(members, SquadMember{MemberType: "agent", MemberID: mid, Role: role})
	}
	var sq *Squad
	if strategy == conflictCreate {
		sq, err = s.squadSvc.Create(ctx, Squad{
			Name: squadName, Description: spec.Squad.Description,
			LeaderID: leaderID, Instructions: spec.Squad.Instructions,
		})
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "squad", Name: squadName, Action: "failed", Error: err.Error()})
			return res, err
		}
	} else {
		sq, err = s.squadSvc.UpsertByName(ctx, squadName, spec.Squad.Description, leaderID, spec.Squad.Instructions, members)
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "squad", Name: squadName, Action: "failed", Error: err.Error()})
			return res, err
		}
	}
	res.Items = append(res.Items, ApplyItem{Kind: "squad", Name: squadName, ID: sq.ID, Action: "created"})
	if strategy == conflictCreate {
		for _, m := range members {
			if _, err := s.squadSvc.AddMember(ctx, sq.ID, m.MemberType, m.MemberID, m.Role); err != nil {
				res.Items = append(res.Items, ApplyItem{Kind: "member", Name: m.MemberID, Action: "failed", Error: err.Error()})
				continue
			}
			res.Items = append(res.Items, ApplyItem{Kind: "member", Name: m.MemberID, Action: "created"})
		}
	}
	res.Squad = sq
	return res, nil
}

// ApplySquad applies a squad template: agents (+embedded skills) → squad →
// members, then auto-creates a same-named scratch domain (the goal default
// project — agent/squad goals require a domain) and surfaces the template's
// suggested goal prompt. Best-effort: per-agent failures are reported and
// skipped; the squad is created when its leader (and the roster) resolved.
func (s *TemplateApplyService) ApplySquad(ctx context.Context, templateID string, ov ApplySquadOverrides) (*ApplySquadResult, error) {
	raw, err := s.templates.LoadRaw(ctx, TemplateKindSquad, templateID)
	if err != nil {
		return nil, err
	}
	_, specAny, err := parseTemplate(raw)
	if err != nil {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, err.Error(), map[string]any{"id": templateID})
	}
	spec, ok := specAny.(*squadSpec)
	if !ok {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, "模板不是 squad-template", map[string]any{"id": templateID})
	}
	strategy := applyStrategy(spec.Strategy, ov.Strategy)
	res, err := s.applySquadSpec(ctx, spec, strategy, ov.Rename, ov.SquadName)
	if err != nil && res == nil {
		return nil, err
	}
	// A scratch domain is the goal default project — agent/squad goals require
	// a domain_id. Created only when the squad resolved (res.Squad != nil); a
	// failed apply leaves the domain uncreated so the user isn't left with a
	// phantom project. The domain rides the squad's final name.
	if res.Squad != nil {
		dom, derr := s.ensureScratchDomain(ctx, res.Squad.Name)
		if derr != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "domain", Name: res.Squad.Name, Action: "failed", Error: derr.Error()})
		} else {
			res.Domain = dom
			res.Items = append(res.Items, ApplyItem{Kind: "domain", Name: dom.Name, ID: dom.ID, Action: "created"})
		}
	}
	res.SuggestedGoal = spec.SuggestedGoal
	return res, err
}

// ensureScratchDomain returns a scratch domain named `name`, creating it if
// missing. A same-named domain (any type) is reused as-is — the quick-start
// flow only needs SOME domain to bind the goal to; re-applying a template
// should not fail on the domain nor create a duplicate. On a name collision
// during Create (a concurrent apply won the race), the name is suffixed
// "-2", "-3", ... up to 20 attempts — the domain is the apply's convenience
// default, not a user-named artifact, so a disambiguating suffix is fine.
//
// Only the public ApplyAgent/ApplySquad call this. The shared applySquadSpec
// (also ApplyProject's team section) does NOT, so a project template never
// builds a second domain alongside the repo domain it already created.
func (s *TemplateApplyService) ensureScratchDomain(ctx context.Context, name string) (*Domain, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, NewValidationError("scratch domain 名不能为空")
	}
	// Reuse an existing same-named domain of any type.
	if id := s.domainIDByName(ctx, name); id != "" {
		if d, err := s.domainSvc.Get(ctx, id); err == nil && d != nil {
			return d, nil
		}
	}
	candidate := name
	for i := 0; i < 20; i++ {
		d, err := s.domainSvc.Create(ctx, Domain{Type: "scratch", Name: candidate})
		if err == nil {
			return d, nil
		}
		// A name collision (UNIQUE constraint) → try the next suffix. Any
		// other error (validation, store) surfaces immediately.
		var ce CodedError
		if !errors.As(err, &ce) || ce.Code() != CodeDomainNameExists {
			return nil, err
		}
		candidate = fmt.Sprintf("%s-%d", name, i+2)
	}
	return nil, NewCodedErrorDetail(CodeDomainNameExists,
		fmt.Sprintf("scratch domain %q 的所有变体名都被占用", name),
		map[string]any{"name": name})
}

// domainIDByName resolves a domain id by exact name ('' = not found).
func (s *TemplateApplyService) domainIDByName(ctx context.Context, name string) string {
	var id string
	_ = s.st.DB().QueryRowContext(ctx, `SELECT id FROM domain WHERE name=?`, name).Scan(&id)
	return id
}

// ApplyAgent applies an agent template: upsert the single agent, auto-create
// a same-named scratch domain (the goal default project), and surface the
// template's suggested goal prompt. The agent rides the same upsertAgent
// path as squad/project agents (full field set, runtime availability gate,
// skill resolution, persona push).
func (s *TemplateApplyService) ApplyAgent(ctx context.Context, templateID string, ov ApplyAgentOverrides) (*ApplyAgentResult, error) {
	raw, err := s.templates.LoadRaw(ctx, TemplateKindAgent, templateID)
	if err != nil {
		return nil, err
	}
	_, specAny, err := parseTemplate(raw)
	if err != nil {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, err.Error(), map[string]any{"id": templateID})
	}
	spec, ok := specAny.(*agentSpec)
	if !ok {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, "模板不是 agent-template", map[string]any{"id": templateID})
	}
	if strings.TrimSpace(spec.Agent.Name) == "" {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, "agent.name 不能为空", map[string]any{"id": templateID})
	}
	strategy := applyStrategy(spec.Strategy, ov.Strategy)
	finalName := strings.TrimSpace(ov.AgentName)
	if finalName == "" {
		finalName = spec.Agent.Name
	}
	res := &ApplyAgentResult{}
	usableIDs, err := s.usableRuntimeIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出可用 runtime：%v", err)
	}
	out, action, err := s.upsertAgent(ctx, strategy, spec.Agent, finalName, usableIDs)
	if err != nil {
		res.Items = append(res.Items, ApplyItem{Kind: "agent", Name: finalName, Action: "failed", Error: err.Error()})
		return res, NewCodedErrorDetail(CodeTemplateInvalid,
			fmt.Sprintf("创建 agent 失败：%v", err),
			map[string]any{"id": templateID, "name": finalName})
	}
	res.Items = append(res.Items, ApplyItem{Kind: "agent", Name: finalName, ID: out.ID, Action: action})
	res.Agent = out
	if s.pushSkills != nil {
		go s.pushSkills(out.ID)
	}
	// The scratch domain is the goal default project — without it the user
	// can't create an agent-typed goal (GoalService.Create requires domain_id).
	dom, derr := s.ensureScratchDomain(ctx, finalName)
	if derr != nil {
		res.Items = append(res.Items, ApplyItem{Kind: "domain", Name: finalName, Action: "failed", Error: derr.Error()})
		// The agent is created; the domain failure is reported but does not
		// void the apply — the user can still pick another project at goal time.
	} else {
		res.Domain = dom
		res.Items = append(res.Items, ApplyItem{Kind: "domain", Name: dom.Name, ID: dom.ID, Action: "created"})
	}
	res.SuggestedGoal = spec.SuggestedGoal
	domainID := "-"
	if res.Domain != nil {
		domainID = res.Domain.ID
	}
	logging.Infof("template-apply: agent template %s applied as agent %s + domain %s (%d item(s))",
		templateID, out.ID, domainID, len(res.Items))
	return res, nil
}

// applyRepo creates the project repo when spec.repo.create (the token comes
// from the apply request; auto_init defaults true — an empty repo cannot
// pass the domain git probe). Returns the clone URL.
//
// Override: when the request carries an explicit git_url, the user chose to
// attach an existing repo instead of creating one — skip creation and let the
// caller bind git_url as the domain's repo (field-level override wins, the
// same rename-overrides-agent-name semantics). Without this a create-type
// template could never be applied against an existing repo, even though the
// frontend offers that choice.
//
// Repo name: ov.Name is the domain name (platform allows Chinese), but the
// GitCode repo slug accepts only letters/digits/_/-/. — Chinese and other
// non-ASCII are stripped to a '-' separator. A name like "Go 微服务项目12"
// becomes "go-12"; a fully non-ASCII name falls back to the template id so
// the slug is never empty. The domain name is untouched.
func (s *TemplateApplyService) applyRepo(ctx context.Context, spec *projectSpec, templateID string, ov *ApplyProjectOverrides, items *[]ApplyItem) (string, error) {
	if spec.Repo == nil || !spec.Repo.Create {
		return "", nil
	}
	if strings.TrimSpace(ov.GitURL) != "" {
		return "", nil // existing-repo override: skip creation, bind git_url
	}
	if s.repoProv == nil {
		return "", NewCodedError(CodeRepoCreateFailed, "仓库创建服务不可用")
	}
	token := ov.RepoToken
	if token == "" {
		return "", NewValidationError("该模板需要在 GitCode 新建仓库——请提供 repo_token")
	}
	in := *spec.Repo
	if ov.Repo != nil { // the request overlay wins field-by-field
		ov.Repo.Create = true
		if ov.Repo.Visibility != "" {
			in.Visibility = ov.Repo.Visibility
		}
		if ov.Repo.AutoInit != nil {
			in.AutoInit = ov.Repo.AutoInit
		}
		if ov.Repo.GitignoreTemplate != "" {
			in.GitignoreTemplate = ov.Repo.GitignoreTemplate
		}
	}
	if in.AutoInit == nil {
		in.AutoInit = boolPtr(true)
	}
	in.Name = repoSlug(ov.Name, templateID)
	in.Description = ov.Name // the human-facing description keeps the full (Chinese) name
	cloneURL, err := s.repoProv.CreateRepo(ctx, token, gitcodeapi.CreateRepoInput{
		Org: in.Org, Name: in.Name, Description: in.Description,
		Private: in.Visibility != "public", AutoInit: *in.AutoInit,
		GitignoreTemplate: in.GitignoreTemplate,
	})
	if err != nil {
		*items = append(*items, ApplyItem{Kind: "repo", Name: in.Name, Action: "failed", Error: err.Error()})
		return "", NewCodedErrorDetail(CodeRepoCreateFailed, fmt.Sprintf("GitCode 建仓失败：%v", err),
			map[string]any{"name": in.Name})
	}
	*items = append(*items, ApplyItem{Kind: "repo", Name: in.Name, Action: "created"})
	return cloneURL, nil
}

// probeGit runs the same config-time git gate the POST /domains handler
// applies (决策 6-24 延伸) — misconfiguration surfaces here, not at first run.
func (s *TemplateApplyService) probeGit(ctx context.Context, gitURL, branch, credentials string) error {
	if s.gitTester == nil || gitURL == "" {
		return nil
	}
	res := s.gitTester.TestDomainGit(ctx, gitURL, branch, credentials)
	if !res.OK {
		return NewValidationError("仓库连接测试失败：" + res.Error)
	}
	if !res.BranchExists {
		if len(res.Refs) == 0 {
			return NewValidationError("仓库为空（没有任何分支）——不能用作物项目仓；建仓请开启 auto_init 或先推送一个提交")
		}
		return NewValidationError(fmt.Sprintf("分支 %q 不存在（远端分支：%s）", res.ResolvedBranch, strings.Join(res.Refs, ", ")))
	}
	return nil
}

// ApplyProject applies a project template:
// repo(optional) → probe → domain → team(agents+squad) → goals → schedules.
// The repo/domain step is terminal (everything hangs off the domain);
// goal/schedule failures are reported per-item and skipped.
func (s *TemplateApplyService) ApplyProject(ctx context.Context, templateID string, ov ApplyProjectOverrides) (*ApplyProjectResult, error) {
	raw, err := s.templates.LoadRaw(ctx, TemplateKindProject, templateID)
	if err != nil {
		return nil, err
	}
	_, specAny, err := parseTemplate(raw)
	if err != nil {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, err.Error(), map[string]any{"id": templateID})
	}
	spec, ok := specAny.(*projectSpec)
	if !ok {
		return nil, NewCodedErrorDetail(CodeTemplateInvalid, "模板不是 project-template", map[string]any{"id": templateID})
	}
	res := &ApplyProjectResult{}
	if strings.TrimSpace(ov.Name) == "" {
		return nil, NewFieldRequiredError("name")
	}
	// 1. Repo (optional).
	gitURL := strings.TrimSpace(ov.GitURL)
	if created, err := s.applyRepo(ctx, spec, templateID, &ov, &res.Items); err != nil {
		return res, err
	} else if created != "" {
		gitURL = created
		res.RepoURL = created
		// The repo-creation token IS the project's git credential going
		// forward (read/push) — when the user supplied no separate
		// git_credentials, reuse repo_token so the probe + domain bind and
		// later runs can authenticate. Otherwise the freshly-created repo
		// fails the probe with "could not read Username".
		if strings.TrimSpace(ov.GitCredentials) == "" && ov.RepoToken != "" {
			ov.GitCredentials = ov.RepoToken
		}
	}
	// 2. Domain config.
	dType := spec.Domain.Type
	if dType == "" {
		dType = "repo"
	}
	if dType != "repo" && dType != "scratch" {
		return res, NewValidationError("domain.type 必须是 repo 或 scratch")
	}
	if dType == "repo" && gitURL == "" {
		return res, NewValidationError("repo 类型项目需要 git_url（或模板声明 repo.create 并提供 repo_token）")
	}
	branch := spec.Domain.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	// 3. Probe (repo domains only — the create-domain gate).
	if dType == "repo" {
		if err := s.probeGit(ctx, gitURL, branch, ov.GitCredentials); err != nil {
			return res, err
		}
	}
	// 4. Domain. Domain names are UNIQUE by design (no upsert variant
	// exists) — the existing AW.10000010 surfaces on a repeat apply.
	// IssueAssignee is intentionally left blank here: the template's value is
	// a NAME (forward-referencing a team agent created in step 5), but
	// DomainService.Create → validateIssueTracking validates it as an id via
	// mustExist(...WHERE id=?), which rejects the name before the team is
	// built. The resolved id is patched in by the post-team Update (step 5b).
	domain := Domain{
		Type: dType, Name: ov.Name, GitURL: gitURL,
		DefaultBranch: branch, GitIdentity: spec.Domain.GitIdentity,
		GitCredentials: ov.GitCredentials, PolicyText: spec.Domain.PolicyText,
	}
	// IssueAssignee is NOT set on Create (see above) — the assignee name
	// resolves to an id only after the team section builds its agents.
	createdDomain, err := s.domainSvc.Create(ctx, domain)
	if err != nil {
		res.Items = append(res.Items, ApplyItem{Kind: "domain", Name: ov.Name, Action: "failed", Error: err.Error()})
		return res, err
	}
	res.Items = append(res.Items, ApplyItem{Kind: "domain", Name: createdDomain.Name, ID: createdDomain.ID, Action: "created"})
	res.Domain = createdDomain
	// 5. Team (optional): reuse the squad path.
	if spec.Team != nil {
		teamStrategy := applyStrategy(spec.Team.Strategy, ov.Strategy)
		sqRes, err := s.applySquadSpec(ctx, spec.Team, teamStrategy, ov.Rename, "")
		if err != nil {
			res.Items = append(res.Items, sqRes.Items...) // per-agent outcomes already recorded
			return res, err
		}
		res.Agents = sqRes.Agents
		res.Squad = sqRes.Squad
		res.Items = append(res.Items, sqRes.Items...)
	}
	// Issue assignee: resolve the template's agent/squad NAME to an id now
	// (after the team section) and patch the domain. Skipped when unresolved
	// (validateIssueTracking would reject a phantom id).
	if dType == "repo" && spec.Domain.IssueAssignee != "" {
		if id := s.resolveIssueAssignee(ctx, spec.Domain.IssueAssignee, spec.Domain.IssueAssigneeType); id != "" {
			updated, err := s.domainSvc.Update(ctx, createdDomain.ID, Domain{
				GitURL: createdDomain.GitURL, DefaultBranch: createdDomain.DefaultBranch,
				GitIdentity: createdDomain.GitIdentity, GitCredentials: ov.GitCredentials,
				IssueRepo: createdDomain.IssueRepo, IssueAssignee: id,
				IssueAssigneeType: spec.Domain.IssueAssigneeType,
				IssueProvider:     createdDomain.IssueProvider,
			})
			if err != nil {
				res.Items = append(res.Items, ApplyItem{Kind: "domain.issue_assignee", Name: spec.Domain.IssueAssignee, Action: "failed", Error: err.Error()})
			} else {
				res.Domain = updated
				res.Items = append(res.Items, ApplyItem{Kind: "domain.issue_assignee", Name: spec.Domain.IssueAssignee, ID: id, Action: "updated"})
			}
		} else {
			res.Items = append(res.Items, ApplyItem{Kind: "domain.issue_assignee", Name: spec.Domain.IssueAssignee, Action: "failed", Error: "未找到对应的 agent/squad"})
		}
	}
	// 6. Goals. goal_start (when set) overrides every template start flag —
	// the dialog's "start all now / keep all in backlog" switch.
	for _, g := range spec.Goals {
		assigneeType := g.AssigneeType
		if assigneeType == "" {
			assigneeType = "agent"
		}
		assigneeID := s.resolveAssigneeName(ctx, assigneeType, g.Assignee, res)
		start := g.Start
		if ov.GoalStart != nil {
			start = *ov.GoalStart
		}
		status := "backlog"
		if start {
			status = "active"
		}
		goal, err := s.goalSvc.Create(ctx, Goal{
			Title: g.Title, Description: g.Description,
			DomainID: createdDomain.ID, AssigneeType: assigneeType,
			AssigneeID: assigneeID, Status: status, CreatedByType: "human",
		})
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "goal", Name: g.Title, Action: "failed", Error: err.Error()})
			continue
		}
		res.Items = append(res.Items, ApplyItem{Kind: "goal", Name: g.Title, ID: goal.ID, Action: "created"})
		res.Goals = append(res.Goals, goal)
	}
	// 7. Schedules.
	for _, sc := range spec.Schedules {
		assigneeType := sc.AssigneeType
		if assigneeType == "" {
			assigneeType = "agent"
		}
		assigneeID := s.resolveAssigneeName(ctx, assigneeType, sc.Assignee, res)
		sch, err := s.schedSvc.Create(ctx, Schedule{
			Name: sc.Name, TitleTemplate: sc.Title, Description: sc.Description,
			AssigneeType: assigneeType, AssigneeID: assigneeID,
			DomainID: createdDomain.ID, CronExpression: sc.Cron,
			Timezone: localTimezone(), Enabled: true,
		})
		if err != nil {
			res.Items = append(res.Items, ApplyItem{Kind: "schedule", Name: sc.Name, Action: "failed", Error: err.Error()})
			continue
		}
		res.Items = append(res.Items, ApplyItem{Kind: "schedule", Name: sc.Name, ID: sch.ID, Action: "created"})
		res.Schedules = append(res.Schedules, sch)
	}
	logging.Infof("template-apply: project template %s applied as domain %s (%d item(s))", templateID, createdDomain.ID, len(res.Items))
	return res, nil
}

// resolveIssueAssignee maps a template issue-assignee name (agent or squad)
// to an id — team members created by THIS apply resolve naturally (they are
// platform rows now); pre-existing entities resolve by name.
func (s *TemplateApplyService) resolveIssueAssignee(ctx context.Context, name, idType string) string {
	t := idType
	if t == "" {
		t = "agent"
	}
	switch t {
	case "agent":
		return s.agentIDByName(ctx, name)
	case "squad":
		if sq, err := s.squadByName(ctx, name); err == nil && sq != nil {
			return sq.ID
		}
	}
	return ""
}

// resolveAssigneeName maps a goal/schedule assignee name to an id. Template-
// created entities are platform rows by this point, so one name lookup path
// covers both. Unresolved names pass through — the service layer rejects
// them with its own validation message (surfaced per-item).
func (s *TemplateApplyService) resolveAssigneeName(ctx context.Context, assigneeType, name string, res *ApplyProjectResult) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	switch assigneeType {
	case "agent":
		if id := s.agentIDByName(ctx, name); id != "" {
			return id
		}
	case "squad":
		if sq, err := s.squadByName(ctx, name); err == nil && sq != nil {
			return sq.ID
		}
	}
	return name
}

// localTimezone mirrors the intake path: schedules speak the daemon's local
// time (the owner's wall clock on a single-user machine).
func localTimezone() string {
	tz := time.Local.String()
	if tz == "" {
		return "UTC"
	}
	return tz
}

func boolPtr(b bool) *bool { return &b }

// repoSlug turns a domain name (which may contain Chinese and other
// non-ASCII) into a GitCode-acceptable repo slug: letters/digits/_/-/.
// only, lowercased, non-ASCII and other separators → '-', runs collapsed,
// trimmed. "Go 微服务项目12" → "go-12". A name that yields an empty slug
// (fully non-ASCII) falls back to the template id so creation never fails on
// the slug alone.
func repoSlug(name, fallback string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		case r == '_' || r == '-' || r == '.':
			b.WriteRune(r)
		default: // spaces, Chinese, punctuation, etc.
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	// collapse runs of separators produced by stripped characters
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	if len(out) > 63 {
		out = out[:63]
	}
	out = strings.Trim(out, "-_.")
	if out == "" {
		out = fallback
	}
	return out
}

// ParseTemplateForTest exposes parseTemplate to the validation harness
// (cmd-level tooling lives in package main and cannot reach internal
// unexported symbols otherwise).
func ParseTemplateForTest(raw string) (TemplateMeta, any, error) {
	return parseTemplate(raw)
}
