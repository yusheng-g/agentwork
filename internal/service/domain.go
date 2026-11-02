package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/store"
)

// Domain is an asset/evolution domain: a shared repo + acceptance policy
// (NL intent + compiled checks) + default gates. M0 implements type=repo.
// The acceptance policy is the SOURCE of "done": the owner defines it in
// natural language (PolicyText), the processor agent compiles it into
// executable Checks, and the owner's confirmation freezes it. See
// DESIGN.md §5.2 (triangle separation: define ≠ execute ≠ judge).
type Domain struct {
	ID            string `json:"id"`
	Type          string `json:"type"` // repo (M0); others deferred
	Name          string `json:"name"`
	GitURL        string `json:"git_url"`
	DefaultBranch string `json:"default_branch"`
	GitIdentity   string `json:"git_identity"` // "name <email>" for commits
	// GitCredentials is the platform's REMOTE-OPERATION identity (decision
	// 3-5): the token used for issue comments/close AND git push — every
	// remote action appears under this account. Configure a dedicated
	// agentwork-bot account's token (GitHub or GitCode — both platforms
	// render comments under the authenticating account) so the human's own
	// identity stays clean; commits are separately authored by git_identity.
	GitCredentials       string `json:"git_credentials"`
	PolicyText           string `json:"policy_text"`           // NL intent (source of truth)
	Checks               Checks `json:"checks"`                // compiled, frozen after confirmation
	VerificationStrength string `json:"verification_strength"` // strong|medium|weak
	MaxRunDuration       int    `json:"max_run_duration"`      // seconds per run
	VerifyTimeout        int    `json:"verify_timeout"`        // seconds per verify command
	ProcessorAgentID     string `json:"processor_agent_id"`    // per-domain override of the global processor agent
	ChecksCompiledAt     string `json:"checks_compiled_at"`    // '' = not compiled/frozen yet
	MetricsBaseline      string `json:"metrics_baseline"`      // JSON: test count / coverage at creation
	IssueRepo            string `json:"issue_repo"`            // M4-B: "owner/repo" whose open issues auto-become goals ('' = none)
	IssueAssignee        string `json:"issue_assignee"`        // M4-B: agent|squad id handling this repo's issues ('' = don't auto-create)
	IssueAssigneeType    string `json:"issue_assignee_type"`   // M4-B: agent | squad (default agent)
	IssueProvider        string `json:"issue_provider"`        // M4-B: github | gitcode (default github)
	PullAssignee         string `json:"pull_assignee"`         // M4-B PR pull: agent|squad id handling this repo's pull requests ('' = don't pull PRs)
	PullAssigneeType     string `json:"pull_assignee_type"`    // M4-B PR pull: agent | squad (default agent)
	CreatedAt            string `json:"created_at"`
	// ScratchDir is the scratch domain's persistent project root on disk
	// ('' for repo domains) — the UI shows it so the human can find the
	// artifacts.
	ScratchDir string `json:"scratch_dir,omitempty"`
}

// Checks is the compiled acceptance policy (DESIGN.md §5): the frozen,
// executable interpretation of the domain owner's NL intent. Produced by the
// processor agent, frozen by the owner's confirmation — never mutated in
// place after that.
type Checks struct {
	// Setup is the verification environment preparation (M3): dependency
	// installs and other prerequisites that must run BEFORE verify can judge.
	// The verification environment is part of the acceptance policy — a
	// clean worktree has no node_modules, so "npm run build" cannot run until
	// "npm install" did. Commands must be idempotent (npm install / pip
	// install are; the worktree keeps its installed state across runs).
	// Executed in order before verify; any failure = run failed (environment
	// attribution). Empty = no preparation needed.
	Setup []string `json:"setup"`
	// Excludes are path patterns the platform EXCLUDES when committing the
	// agent's work (git add pathspec excludes, repo-root-relative, globs).
	// They belong to the domain, NOT to the platform: the processor compiles
	// them from the repo's own .gitignore / observed dependency directories,
	// the owner confirms them on the confirmation card — the platform never
	// hardcodes "what a repo should ignore" (a repo may intentionally track
	// target/, and new dependency dirs would outgrow any hardcoded list).
	// The platform's own injected AGENTWORK.md is excluded unconditionally
	// (platform-owned namespace), everything else comes from here.
	Excludes []string   `json:"excludes"`
	Verify   []string   `json:"verify"` // machine verification commands (exit 0 = pass)
	Guards   []Guard    `json:"guards"` // structural constraints, checked by the daemon
	Gates    []GateRule `json:"gates"`  // human checkpoint rules
}

// Guard is a structural constraint — an objective, command-free check on the
// run's diff (DESIGN.md §5.1, second form).
type Guard struct {
	Type     string  `json:"type"`      // diff_contains | diff_excludes | coverage_delta
	Pattern  string  `json:"pattern"`   // glob matched against changed paths (diff_* guards)
	MinDelta float64 `json:"min_delta"` // required coverage percentage-point delta
}

// GateRule names a human checkpoint (DESIGN.md §5). Rule kinds (M2):
//
//	merge          — every completed run parks the goal in review (M0 default)
//	diff_contains  — the run's diff must contain a path matching Pattern
//	diff_excludes  — the run's diff must not contain a path matching Pattern
//
// diff_* conditions are evaluated by the daemon (it owns the git diff) and
// recorded on the run row (run.gates_hit); the goal layer only reads the
// result — the daemon computes, the goal layer judges.
type GateRule struct {
	Name    string `json:"name"`    // merge | diff_contains | diff_excludes
	When    string `json:"when"`    // human-readable condition description
	Pattern string `json:"pattern"` // diff_* gates: glob over changed paths
}

type DomainService struct {
	st     *store.Store
	bus    *events.Bus
	runSvc *RunService // back-reference for compile-run enqueue (same package)
}

func NewDomainService(st *store.Store, bus *events.Bus) *DomainService {
	return &DomainService{st: st, bus: bus}
}

// SetRunService wires the RunService back-reference (CompilePolicy enqueues a
// processor run). Explicit setter to avoid a constructor-order cycle.
func (s *DomainService) SetRunService(rs *RunService) { s.runSvc = rs }

// CompilePolicy kicks off the acceptance-policy compilation for a domain
// (DESIGN.md §5.3): records the NL intent, enqueues a processor run on the
// given processor agent. The daemon executes the run, reads checks.json from
// the run's workdir, and stores the result on the domain in an UNFROZEN state
// (checks_compiled_at stays ""); the owner's confirmation card then freezes
// it via FreezeChecks. Returns the processor run.
// The processor agent falls back to the domain's processor_agent_id, then the
// system steward (决策 2-17) — the user never has to pick one.
func (s *DomainService) CompilePolicy(ctx context.Context, domainID, policyText, processorAgentID string) (*Run, error) {
	if strings.TrimSpace(policyText) == "" {
		return nil, NewFieldRequiredError("policy_text")
	}
	d, err := s.Get(ctx, domainID)
	if err != nil {
		return nil, err
	}
	if processorAgentID == "" {
		processorAgentID = d.ProcessorAgentID
	}
	if processorAgentID == "" {
		processorAgentID, err = s.resolveStewardAgent(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, processorAgentID, "processor agent"); err != nil {
		return nil, err
	}
	if s.runSvc == nil {
		return nil, errors.New("domainSvc.runSvc not wired")
	}
	if _, err := s.st.DB().ExecContext(ctx, `UPDATE domain SET policy_text=? WHERE id=?`, policyText, domainID); err != nil {
		return nil, fmt.Errorf("update policy_text: %w", err)
	}
	return s.runSvc.EnqueueProcessorRun(ctx, "compile", domainID, processorAgentID, compilePrompt(d, policyText))
}

// resolveStewardAgent returns the system steward agent id — the bootstrap
// default processor when neither request nor domain names one. Mirrors
// intake/team_import. Returns a setup-hint error when no steward exists
// (no machine connected).
func (s *DomainService) resolveStewardAgent(ctx context.Context) (string, error) {
	var id string
	err := s.st.DB().QueryRowContext(ctx, `SELECT id FROM agent WHERE type='steward' LIMIT 1`).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", NewValidationError("no processor agent available — configure one on the domain, or connect a machine and restart the daemon")
		}
		return "", fmt.Errorf("resolve steward agent: %w", err)
	}
	return id, nil
}

// compilePrompt builds the instruction for the processor agent. The compiled
// policy goes to {workdir}/checks.json — a FILE, not stdout: the platform
// reads structured side effects, never agent output (DESIGN.md §5.3, §9).
func compilePrompt(d *Domain, policyText string) string {
	if d.Type == "scratch" {
		return compilePromptScratch(d, policyText)
	}
	var b strings.Builder
	b.WriteString("你是 agentwork 的验收策略编译器。用户用自然语言描述了这个域的验收要求：\n\n")
	b.WriteString(policyText)
	b.WriteString("\n\n请把它编译成结构化验收策略 JSON，写入当前工作目录的 checks.json 文件（不要输出到 stdout——文件即结果）。\n\n")
	b.WriteString(`checks.json 结构：
{
  "setup": ["<验证环境准备命令，幂等，如 cd web && npm install>", ...],
  "excludes": ["<提交时排除的路径 glob，从仓库自己的 .gitignore / 依赖目录推导，如 **/node_modules/**>", ...],
  "verify": ["<机器验证命令，exit 0 为通过，如 go test ./...>", ...],
  "guards": [{"type": "diff_contains|diff_excludes|coverage_delta", "pattern": "<glob，diff_* 必填>", "min_delta": <覆盖率百分点，仅 coverage_delta>}],
  "gates": [
    {"name": "merge", "when": "<该卡点触发条件的人话描述>"},
    {"name": "diff_contains", "when": "<人话描述>", "pattern": "<glob，如 config/*>"},
    {"name": "diff_excludes", "when": "<人话描述>", "pattern": "<glob，如 *.secret>"}
  ]
}`)
	b.WriteString("\n\n另把验证强度（strong|medium|weak）写入当前工作目录的 strength.txt——判断依据：verify 命令是否真实覆盖了任务的关键风险（echo ok / true 之类是 weak）。\n\n")
	b.WriteString("再统计当前仓库的演进指标基线，写入当前工作目录的 metrics.json（文件即结果）：\n")
	b.WriteString(`{"test_count": <测试用例数>, "coverage": <测试覆盖率百分比，数值>}`)
	b.WriteString("\n（用实际可运行的命令统计：go test -cover ./... 的输出、npm test 的 jest 报告等；无法统计就写 0/null。这是平台自举证明的数据层——基线 vs 后续演进。）\n\n规则：\n")
	b.WriteString("- setup 是验证环境准备：平台在干净 worktree 上执行验证，依赖不会自己存在。识别技术栈，把需要的依赖安装写进 setup（必须幂等：npm install / pip install -r requirements.txt / go mod download 这类；已安装时秒级跳过）。go/cargo 这类自动拉依赖的可留空。\n")
	b.WriteString("- excludes 是提交排除：平台在 run 结束后把 agent 的改动提交到 goal 分支（git add），setup 安装的依赖目录（node_modules 等）若仓库的 .gitignore 没覆盖，会被误提交进分支。读仓库的 .gitignore 和实际依赖目录，把需要排除的路径写进 excludes（glob，如 **/node_modules/**）。仓库 .gitignore 已覆盖的可省略。\n")
	b.WriteString("- verify 里的命令必须真实存在且可执行（结合该仓库技术栈推断），且假定 setup 已执行完\n")
	b.WriteString("- guards 表达无法用命令表达的结构化约束（禁止路径、diff 必须包含的内容、覆盖率下限）\n")
	b.WriteString("- gates 表达“机器无法判定、必须人工决策”的要求（如“性能不能下降”）——至少包含一条 merge 卡点\n")
	fmt.Fprintf(&b, "- 域类型：%s；仓库名：%s\n\n", d.Type, d.Name)
	b.WriteString("完成后用一句话说明编译依据。")
	return b.String()
}

// compilePromptScratch is the scratch-domain variant: there is NO repo for
// the compiler to inspect — no tech stack, no .gitignore, no diff-based
// guards/gates. The deliverable is a report; the human checkpoint is forced
// by the goal layer anyway (gatesForGoal). The compiled checks may still
// carry setup/verify commands that run in the goal's project directory.
func compilePromptScratch(d *Domain, policyText string) string {
	var b strings.Builder
	b.WriteString("你是 agentwork 的验收策略编译器。用户用自然语言描述了这个无仓库项目（scratch 域）的验收要求：\n\n")
	b.WriteString(policyText)
	b.WriteString("\n\n这个项目没有 git 仓库：任务的产物是项目目录里的文件（报告/笔记等），评论区只是协作面；没有代码 diff。请把要求编译成结构化验收策略 JSON，写入当前工作目录的 checks.json 文件（文件即结果，不要输出到 stdout）。\n\n")
	b.WriteString(`checks.json 结构（无仓库域的子集）：
{
  "setup": ["<验证环境准备命令，幂等；不需要就留空数组>", ...],
  "excludes": [],
  "verify": ["<机器验证命令，在任务的项目目录里执行，exit 0 为通过；如检查某报告文件存在与否>", ...],
  "guards": [],
  "gates": [{"name": "merge", "when": "<该卡点触发条件的人话描述>"}]
}`)
	b.WriteString("\n\n另把验证强度（strong|medium|weak）写入当前工作目录的 strength.txt。\n\n再写 metrics.json：{\"test_count\": 0, \"coverage\": 0}（无仓库，无需统计）。\n\n规则：\n")
	b.WriteString("- setup/verify 是真实可执行的命令；没有客观可验的就留空（平台会强制人工审批兜底）\n")
	b.WriteString("- 不产出 diff_* guards/gates（无 diff 可判定）\n")
	b.WriteString("- 完成后用一句话说明编译依据。")
	return b.String()
}

func (s *DomainService) Create(ctx context.Context, d Domain) (*Domain, error) {
	if d.Name == "" {
		return nil, NewFieldRequiredError("name")
	}
	if d.Type == "" {
		d.Type = "repo"
	}
	switch d.Type {
	case "repo":
		if d.GitURL == "" {
			return nil, NewFieldRequiredError("git_url")
		}
	case "scratch":
		// A scratch domain has NO shared repository — its persistent home is
		// runs/scratch/<name>/ (the human-maintained shared root + per-goal
		// directories). git_url is meaningless here.
	default:
		return nil, NewValidationError("domain type must be repo or scratch")
	}
	if d.DefaultBranch == "" {
		d.DefaultBranch = "main"
	}
	// Issue tracking identity is DERIVED from the repo URL — the human
	// already gave the URL; asking for owner/repo + platform again is
	// double entry. Explicit values (edit-form corrections) win.
	if d.IssueRepo == "" && d.GitURL != "" {
		d.IssueRepo, d.IssueProvider = deriveIssueSource(d.GitURL)
	}
	if d.VerificationStrength == "" {
		d.VerificationStrength = "medium"
	}
	switch d.VerificationStrength {
	case "strong", "medium", "weak":
	default:
		return nil, NewValidationError("verification_strength must be strong, medium, or weak")
	}
	if d.MaxRunDuration == 0 {
		d.MaxRunDuration = 7200 // 2h (DESIGN.md §4)
	}
	if d.VerifyTimeout == 0 {
		d.VerifyTimeout = 600 // 10min per verify command
	}
	if d.ProcessorAgentID != "" {
		if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, d.ProcessorAgentID, "processor agent"); err != nil {
			return nil, err
		}
	}
	// M4-B: issue tracking needs a repo AND an assignee (git_credentials is
	// independently valid — it also serves commit identity). Reject
	// half-configured issue tracking loudly instead of silently never
	// polling. The assignee may be an agent or a squad (issue_assignee_type).
	if err := s.validateIssueTracking(ctx, &d); err != nil {
		return nil, err
	}
	d.ID = newID()
	d.CreatedAt = now()
	checksJSON, _ := json.Marshal(d.Checks)
	_, err := s.st.DB().ExecContext(ctx,
		`INSERT INTO domain (id,type,name,git_url,default_branch,git_identity,git_credentials,policy_text,checks,verification_strength,max_run_duration,verify_timeout,processor_agent_id,checks_compiled_at,metrics_baseline,issue_repo,issue_assignee,issue_assignee_type,issue_provider,pull_assignee,pull_assignee_type,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Type, d.Name, d.GitURL, d.DefaultBranch, d.GitIdentity, d.GitCredentials, d.PolicyText, string(checksJSON), d.VerificationStrength, d.MaxRunDuration, d.VerifyTimeout, d.ProcessorAgentID, d.ChecksCompiledAt, d.MetricsBaseline, d.IssueRepo, d.IssueAssignee, d.IssueAssigneeType, d.IssueProvider, d.PullAssignee, d.PullAssigneeType, d.CreatedAt)
	if err != nil {
		if ve := dupNameCodedError(err, CodeDomainNameExists, "domain", d.Name); ve != nil {
			return nil, ve
		}
		return nil, fmt.Errorf("insert domain: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "domain:created", Payload: d})
	return &d, nil
}

// fillScratchDir computes the on-disk project root for a scratch domain.
func fillScratchDir(d *Domain) {
	if d.Type == "scratch" {
		d.ScratchDir = ScratchDomainRoot(d.Name)
	}
}

func (s *DomainService) List(ctx context.Context) ([]Domain, error) {
	rows, err := s.st.DB().QueryContext(ctx,
		`SELECT id,type,name,git_url,default_branch,git_identity,git_credentials,policy_text,checks,verification_strength,max_run_duration,verify_timeout,processor_agent_id,checks_compiled_at,metrics_baseline,issue_repo,issue_assignee,issue_assignee_type,issue_provider,pull_assignee,pull_assignee_type,created_at
		 FROM domain ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Domain{}
	for rows.Next() {
		var d Domain
		var checksJSON string
		if err := rows.Scan(&d.ID, &d.Type, &d.Name, &d.GitURL, &d.DefaultBranch, &d.GitIdentity, &d.GitCredentials, &d.PolicyText, &checksJSON, &d.VerificationStrength, &d.MaxRunDuration, &d.VerifyTimeout, &d.ProcessorAgentID, &d.ChecksCompiledAt, &d.MetricsBaseline, &d.IssueRepo, &d.IssueAssignee, &d.IssueAssigneeType, &d.IssueProvider, &d.PullAssignee, &d.PullAssigneeType, &d.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(checksJSON), &d.Checks)
		out = append(out, d)
	}
	for i := range out {
		fillScratchDir(&out[i])
	}
	return out, rows.Err()
}

func (s *DomainService) Get(ctx context.Context, id string) (*Domain, error) {
	var d Domain
	var checksJSON string
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT id,type,name,git_url,default_branch,git_identity,git_credentials,policy_text,checks,verification_strength,max_run_duration,verify_timeout,processor_agent_id,checks_compiled_at,metrics_baseline,issue_repo,issue_assignee,issue_assignee_type,issue_provider,pull_assignee,pull_assignee_type,created_at
		 FROM domain WHERE id=?`, id).
		Scan(&d.ID, &d.Type, &d.Name, &d.GitURL, &d.DefaultBranch, &d.GitIdentity, &d.GitCredentials, &d.PolicyText, &checksJSON, &d.VerificationStrength, &d.MaxRunDuration, &d.VerifyTimeout, &d.ProcessorAgentID, &d.ChecksCompiledAt, &d.MetricsBaseline, &d.IssueRepo, &d.IssueAssignee, &d.IssueAssigneeType, &d.IssueProvider, &d.PullAssignee, &d.PullAssigneeType, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(checksJSON), &d.Checks)
	fillScratchDir(&d)
	return &d, nil
}

func (s *DomainService) Delete(ctx context.Context, id string) error {
	// Refuse if goals reference this domain — deleting the domain would
	// silently orphan their worktrees and acceptance policies.
	var n int
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal WHERE domain_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("check goals: %w", err)
	}
	if n > 0 {
		// Resolve the name for a human-facing message — the id is opaque to
		// the operator. The query is best-effort (row may be gone in a race);
		// fall back to id so we never fail to report the conflict.
		var name string
		_ = s.st.DB().QueryRowContext(ctx, `SELECT name FROM domain WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeDomainHasGoals,
			fmt.Sprintf("domain %q has %d goal(s); delete its goals first", name, n),
			map[string]any{"name": name, "count": n})
	}
	// Refuse if schedules reference this domain — a schedule.domain_id is a
	// required execution precondition (fired goals need the domain's
	// acceptance policy + worktree), so deleting the domain would turn every
	// dependent schedule into a zombie that fails its next firing.
	if err := s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schedule WHERE domain_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("check schedules: %w", err)
	}
	if n > 0 {
		var name string
		_ = s.st.DB().QueryRowContext(ctx, `SELECT name FROM domain WHERE id=?`, id).Scan(&name)
		if name == "" {
			name = id
		}
		return NewCodedErrorDetail(CodeDomainHasSchedules,
			fmt.Sprintf("domain %q has %d schedule(s); delete or reassign them first", name, n),
			map[string]any{"name": name, "count": n})
	}
	// The name travels in the payload: the daemon removes a scratch
	// domain's project root after the row is gone.
	var name string
	_ = s.st.DB().QueryRowContext(ctx, `SELECT name FROM domain WHERE id=?`, id).Scan(&name)
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM domain WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete domain: %w", err)
	}
	s.bus.Publish(ctx, events.Event{Topic: "domain:deleted", Payload: map[string]string{"id": id, "name": name}})
	return nil
}

// validateIssueTracking validates the M4-B issue-tracking configuration
// (shared by Create and Update): a repo AND an assignee must come together,
// the platform token is required, the provider is github|gitcode, and the
// assignee may be an agent or a squad.
// deriveIssueSource infers the issue tracker identity (owner/repo +
// provider) from the repo URL: https://gitcode.com/eushing/test-repo.git →
// ("eushing/test-repo", "gitcode"). Handles https/http and ssh shapes.
func deriveIssueSource(gitURL string) (ownerRepo, provider string) {
	u := strings.TrimSpace(gitURL)
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.Index(u, "@"); i >= 0 { // git@host:owner/repo.git
		u = strings.Replace(u[i+1:], ":", "/", 1)
	}
	u = strings.TrimSuffix(u, ".git")
	parts := strings.Split(strings.Trim(u, "/"), "/")
	if len(parts) < 3 {
		return "", ""
	}
	host, owner, repo := parts[0], parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || repo == "" {
		return "", ""
	}
	switch {
	case strings.Contains(host, "gitcode"):
		provider = "gitcode"
	case strings.Contains(host, "github"):
		provider = "github"
	}
	return owner + "/" + repo, provider
}

func (s *DomainService) validateIssueTracking(ctx context.Context, d *Domain) error {
	if d.Type == "scratch" && (d.IssueRepo != "" || d.IssueAssignee != "" || d.PullAssignee != "") {
		return NewValidationError("issue tracking needs a repository — not available on a scratch domain")
	}
	if d.IssueAssignee != "" {
		// The ASSIGNEE is the opt-in switch: issue_repo is derived from the
		// repo URL (or corrected explicitly), so a repo domain always has
		// one — only a missing assignee-side pair is an error.
		if d.IssueRepo == "" {
			return NewValidationError("issue tracking needs an issue_repo (derivable from git_url)")
		}
		if d.GitCredentials == "" {
			return NewValidationError("issue tracking needs git_credentials (the platform token)")
		}
		if d.IssueProvider == "" {
			d.IssueProvider = "github"
		}
		if d.IssueProvider != "github" && d.IssueProvider != "gitcode" {
			return NewValidationError("issue_provider must be github or gitcode")
		}
		if d.IssueAssigneeType == "" {
			d.IssueAssigneeType = "agent"
		}
		switch d.IssueAssigneeType {
		case "agent":
			if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, d.IssueAssignee, "issue assignee"); err != nil {
				return err
			}
		case "squad":
			if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, d.IssueAssignee, "issue assignee squad"); err != nil {
				return err
			}
		default:
			return NewValidationError("issue_assignee_type must be agent or squad")
		}
	}
	// M4-B PR pull: mirrors the issue-tracking validation — the PR handler
	// reuses the issue tracking's repo/provider (one repo, one tracking
	// config), only the assignee side is new ('' = don't pull PRs).
	if d.PullAssignee != "" {
		if d.IssueRepo == "" {
			return NewValidationError("PR pulling needs a repository (derivable from git_url)")
		}
		if d.GitCredentials == "" {
			return NewValidationError("PR pulling needs git_credentials (the platform token)")
		}
		if d.IssueProvider == "" {
			d.IssueProvider = "github"
		}
		if d.IssueProvider != "github" && d.IssueProvider != "gitcode" {
			return NewValidationError("issue_provider must be github or gitcode")
		}
		if d.PullAssigneeType == "" {
			d.PullAssigneeType = "agent"
		}
		switch d.PullAssigneeType {
		case "agent":
			if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM agent WHERE id=?`, d.PullAssignee, "PR assignee"); err != nil {
				return err
			}
		case "squad":
			if err := mustExist(ctx, s.st, `SELECT COUNT(*) FROM squad WHERE id=?`, d.PullAssignee, "PR assignee squad"); err != nil {
				return err
			}
		default:
			return NewValidationError("pull_assignee_type must be agent or squad")
		}
	}
	return nil
}

// Update edits a domain's mutable configuration — the issue handler can be
// changed after creation (e.g. a single agent → a squad, the whole point of
// the edit path). Compile artifacts (checks/strength/baseline) are only
// touched by CompilePolicy/FreezeChecks; policy_text by CompilePolicy.
func (s *DomainService) Update(ctx context.Context, id string, d Domain) (*Domain, error) {
	// The edit path never changes type — repo/scratch is fixed at creation.
	// Fetching the old domain both checks existence and yields the DB's real
	// type for validation. The frontend edit form omits type, so the decoded
	// d.Type is "" and the scratch guard in validateIssueTracking would never
	// fire against that empty value — letting a scratch domain silently absorb
	// issue tracking config (a type ↔ capability contradiction). Overlaying the
	// real type makes the guard evaluate against the true domain type.
	old, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.Type != "" && d.Type != old.Type {
		return nil, NewValidationError("domain type cannot be changed after creation (repo ↔ scratch is not supported)")
	}
	d.Type = old.Type
	if err := s.validateIssueTracking(ctx, &d); err != nil {
		return nil, err
	}
	_, err = s.st.DB().ExecContext(ctx,
		`UPDATE domain SET git_url=?, default_branch=?, git_identity=?, git_credentials=?, issue_repo=?, issue_assignee=?, issue_assignee_type=?, issue_provider=?, pull_assignee=?, pull_assignee_type=? WHERE id=?`,
		d.GitURL, d.DefaultBranch, d.GitIdentity, d.GitCredentials, d.IssueRepo, d.IssueAssignee, d.IssueAssigneeType, d.IssueProvider, d.PullAssignee, d.PullAssigneeType, id)
	if err != nil {
		return nil, fmt.Errorf("update domain: %w", err)
	}
	return s.Get(ctx, id)
}

// FreezeChecks stores the compiled acceptance policy and stamps it frozen.
// Called after the owner confirms the processor agent's compilation output
// (DESIGN.md §5.3). Frozen checks are never mutated in place — a fresh
// compile cycle replaces them wholesale.
func (s *DomainService) FreezeChecks(ctx context.Context, id string, checks Checks, strength string) (*Domain, error) {
	switch strength {
	case "strong", "medium", "weak":
	default:
		return nil, NewValidationError("verification_strength must be strong, medium, or weak")
	}
	checksJSON, _ := json.Marshal(checks)
	if _, err := s.st.DB().ExecContext(ctx,
		`UPDATE domain SET checks=?, verification_strength=?, checks_compiled_at=? WHERE id=?`,
		string(checksJSON), strength, now(), id); err != nil {
		return nil, fmt.Errorf("freeze checks: %w", err)
	}
	return s.Get(ctx, id)
}
