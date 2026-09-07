package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/service"
)

// runIntakeTask executes an inbound-message parse run: the parser
// agent reads the owner's message (the run prompt) and writes intake.json —
// the parsed action — into its scratch workdir. The PLATFORM executes the
// action (goal create / review list / goal status / team import — never the
// agent; the parser only understands intent and names ids) and replies over
// IM (Feishu path) or stores the result for Web polling (intake_web path).
//
// Structured output is read from the file, never from agent stdout
// (DESIGN.md §5.3, §9): the parser is a processor agent, same as the
// policy compiler.
func (d *Daemon) runIntakeTask(ctx context.Context, q *service.ClaimedRow, prompt, agentID, runType string) {
	// Scratch workdir (no repo): the parser works from the prompt alone and
	// writes its result file here.
	workdir := filepath.Join(runsRoot(), "proc", q.RunID)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		d.failIntakeRun(ctx, q, "mkdir workdir: "+err.Error(), runType)
		return
	}
	// The artifact's ABSOLUTE path: the scratch dir is opaque to the agent —
	// told to "write intake.json in the current directory" it guessed (a
	// write_file call missing its required path arg, then a raw shell heredoc
	// terminal_create cannot run — command must be an executable). State the
	// full path so the write_file path argument is unambiguous.
	prompt += fmt.Sprintf("\n\nArtifact file ABSOLUTE path: %s\n(Write it there with your file tools; do NOT guess the working directory, do NOT use shell redirection)\n",
		filepath.Join(workdir, "intake.json"))
	var argsJSON, rtEnvJSON, intakeMachineID string
	var maxConcurrent int
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.args, r.env, COALESCE(r.machine_id,''), a.max_concurrent
		 FROM agent a JOIN runtime r ON r.id = a.runtime_id WHERE a.id=?`, agentID).
		Scan(&argsJSON, &rtEnvJSON, &intakeMachineID, &maxConcurrent)
	if err != nil {
		d.failIntakeRun(ctx, q, "load agent runtime: "+err.Error(), runType)
		return
	}
	d.ensureWorker(agentID, maxConcurrent)

	var args []string
	_ = json.Unmarshal([]byte(argsJSON), &args)
	var rtEnv map[string]string
	_ = json.Unmarshal([]byte(rtEnvJSON), &rtEnv)
	agentEnv, _ := d.loadAgentEnv(ctx, agentID)

	// CLI 分支 Phase 2: machine-owned intake runtimes dispatch (same shape
	// as the compile processor — scratch proc dir + intake.json artifact).
	if intakeMachineID != "" {
		dispatchEnv := map[string]string{}
		for k, v := range rtEnv {
			dispatchEnv[k] = v
		}
		for k, v := range agentEnv {
			dispatchEnv[k] = v
		}
		d.dispatchToMachine(ctx, q, link.RunDispatchParams{
			RunID: q.RunID, AgentID: q.AgentID, Attempt: q.Attempt, Token: q.Token,
			Prompt: prompt, Proc: true, Scratch: true,
			ArtifactFiles: []string{"intake.json"},
			ACPSpawn:      args, Env: dispatchEnv,
			McpServers: d.extraMcpServers(ctx, q.AgentID),
		}, intakeMachineID)
		return
	}

	// Legacy transports have no executor anymore (the unified model
	// dispatches everything to machines).
	d.failIntakeRun(ctx, q, "this runtime has no machine — run `agentwork connect` and point the agent at a machine-owned runtime", runType)
}

// ingestIntakeArtifact completes an intake run from its FILE artifact
// (intake.json) — the shared path for local execution and the machine-
// dispatched upload (CLI 分支 Phase 2).
func (d *Daemon) ingestIntakeArtifact(ctx context.Context, q *service.ClaimedRow, artifactContent, runType string) {
	if strings.TrimSpace(artifactContent) == "" {
		logging.Infof("daemon: intake run %s produced empty intake.json — parser did not write the file", q.RunID)
		d.failIntakeRun(ctx, q, "管家未能输出结果，请重试", runType)
		return
	}
	var parsed intakeAction
	if err := json.Unmarshal([]byte(artifactContent), &parsed); err != nil {
		logging.Infof("daemon: intake run %s parse failed: %v\nraw: %s", q.RunID, err, artifactContent)
		d.failIntakeRun(ctx, q, "解析失败：管家输出的结果格式不正确，请重试", runType)
		return
	}
	d.replyIntake(ctx, q, parsed, runType)
}

// failIntakeRun marks the parse run failed AND tells the owner — the inbound
// flow already acknowledged the message ("⏳ 收到"), so a silent failure
// would leave the user waiting for a result that never comes. The failure
// detail is sent IN FULL (no truncation): the user debugging a parse failure
// needs the whole reason, including the path that failed.
func (d *Daemon) failIntakeRun(ctx context.Context, q *service.ClaimedRow, summary, runType string) {
	if runType != "intake_web" {
		if n := d.imNotifier(); n != nil {
			if err := n.Send("⚠️ 消息解析失败：" + summary); err != nil {
				logging.Errorf("daemon: intake failure reply: %v", err)
			}
		}
	}
	// Stamp the run failed directly (P0-5 conditional — a reaper stamp
	// wins) — NOT via failProcessorRun, whose domain:compile_failed event
	// is the compile path's signal and would mislabel an intake failure.
	res, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET status='failed', result_summary=?, finished_at=? WHERE id=? AND status IN ('queued','running')`,
		summary, nowStr(), q.RunID)
	if err != nil {
		logging.Infof("daemon: mark intake run %s failed: %v", q.RunID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		logging.Infof("daemon: intake run %s already terminal — dropping late failure", q.RunID)
	}
}

// intakeAction is the parser's output contract (see notify/intake.go's
// BuildPrompt for the shape the parser is instructed to produce). The create
// sub-structs are NAMED types so the draft-merge helpers can take them as
// parameters (an anonymous struct can't be a func arg) and the draft can
// round-trip a sub-struct as JSON via a concrete type.
type intakeAction struct {
	Intent string     `json:"intent"`
	Goal   goalAction `json:"goal"`
	GoalID string     `json:"goal_id"`
	// Schedule carries the parsed定时任务 fields (create_schedule / schedule_stop).
	// Kept anonymous — it does not participate in the create-draft/merge flow.
	Schedule struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Cron         string `json:"cron"`
		AssigneeID   string `json:"assignee_id"`
		AssigneeType string `json:"assignee_type"` // "agent" (default) | "squad"
		DomainID     string `json:"domain_id"`
	} `json:"schedule"`
	Agent      agentAction      `json:"agent"`
	Squad      squadAction      `json:"squad"`
	ImportTeam importTeamAction `json:"import_team"`
	Domain     domainAction     `json:"domain"`
	Skill      skillAction      `json:"skill"`
}

// skillAction carries the skill_list / skill_delete fields. Only name is
// needed (skills are identified by name, same as agents/squads).
type skillAction struct {
	Name string `json:"name"`
}

// importTeamAction carries the import_team fields parsed from NL.
type importTeamAction struct {
	GitURL      string `json:"git_url"`
	Branch      string `json:"branch"`
	Credentials string `json:"credentials"`
}

// goalAction is the create_goal sub-struct.
type goalAction struct {
	Title        string `json:"title"`
	Description  string `json:"description"`
	AssigneeID   string `json:"assignee_id"`
	AssigneeType string `json:"assignee_type"` // "agent" (default) | "squad"
	DomainID     string `json:"domain_id"`
}

// agentAction carries the create_agent fields. Technical config (env/model/
// mcp_servers/max_concurrent) is deliberately absent — NL builds a
// persona-bearing skeleton, the rest is filled in on the Web.
type agentAction struct {
	Name            string   `json:"name"`
	RuntimeID       string   `json:"runtime_id"`
	Description     string   `json:"description"`
	SystemPrompt    string   `json:"system_prompt"`
	Skills          []string `json:"skills"`
	SkillsSpecified bool     `json:"skills_specified"`
}

// squadAction carries the create_squad fields. Members are added after Create
// via AddMember (role="member"); the leader is held in LeaderID, never in
// MemberIDs.
type squadAction struct {
	Name         string   `json:"name"`
	LeaderID     string   `json:"leader_id"`
	Description  string   `json:"description"`
	Instructions string   `json:"instructions"`
	MemberIDs    []string `json:"member_ids"`
}

// domainAction carries the create_domain fields parsed from NL. Technical
// config (policy_text/processor_agent_id/issue_*) is absent — NL builds a
// repo skeleton, the rest is filled in on the Web.
type domainAction struct {
	Name           string `json:"name"`
	Type           string `json:"type"` // "repo" (default) | "scratch"
	GitURL         string `json:"git_url"`
	DefaultBranch  string `json:"default_branch"`
	GitIdentity    string `json:"git_identity"`
	GitCredentials string `json:"git_credentials"`
}

// intakeHandler is the uniform dispatch signature: each intake intent's
// adapter closure conforms to it, absorbing the handlers' non-uniform
// actual signatures (some take parsed, some take GoalID, some take nothing).
type intakeHandler func(d *Daemon, ctx context.Context, parsed intakeAction) string

// intakeCommand describes one IM intent: its hint text (for the fallback
// prompt) and its handler (for dispatch). One structure drives both —
// adding an intent means one entry here, and both dispatch and hint are
// covered. There is no parallel switch to drift from.
type intakeCommand struct {
	intent string
	hint   func() string
	handle intakeHandler
}

// intakeRegistry wraps the fixed IM-intent roster and owns both dispatch
// (find + call handler) and the fallback prompt assembly. Mirrors
// openagent's slash.Registry (cmds slice + a method that iterates to build
// the help text), extended with dispatch.
type intakeRegistry struct {
	cmds []intakeCommand
}

// intakeReg is the singleton. Order fixes the display order of the hint
// list and the scan order of dispatch (irrelevant for correctness —
// intents are mutually exclusive strings).
var intakeReg = &intakeRegistry{cmds: []intakeCommand{
	{"create_goal", func() string { return "创建任务 <标题>，让 <agent> 在 <domain> 上做 <描述>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeCreateGoal(ctx, p) }},
	{"goal_list", func() string { return "查看任务列表" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeListGoals(ctx) }},
	{"goal_cancel", func() string { return "取消任务 <id>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeCancelGoal(ctx, p.GoalID) }},
	{"goal_assign", func() string { return "把任务 <id> 转给 <agent>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeAssignGoal(ctx, p) }},
	{"review_list", func() string { return "查看待审批" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeReviewList(ctx) }},
	{"goal_status", func() string { return "查询任务状态 <id>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeGoalStatus(ctx, p.GoalID) }},
	{"create_schedule", func() string { return "每 1 个小时做 <任务>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeCreateSchedule(ctx, p) }},
	{"schedule_list", func() string { return "查看定时任务" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeScheduleList(ctx) }},
	{"schedule_stop", func() string { return "停掉定时任务 <名字>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeScheduleStop(ctx, p) }},
	{"create_agent", func() string { return "创建 agent <名字>，用 <运行时>，<人设描述>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeCreateAgent(ctx, p) }},
	{"agent_list", func() string { return "查看 agent 列表" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeListAgents(ctx) }},
	{"agent_delete", func() string { return "删掉 agent <名字>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeDeleteAgent(ctx, p) }},
	{"agent_update", func() string { return "把 agent <名字> 的人设/描述改成 <新值>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeUpdateAgent(ctx, p) }},
	{"create_squad", func() string { return "创建 squad <名字>，leader 是 <agent>，成员有 <agent1> <agent2>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeCreateSquad(ctx, p) }},
	{"squad_list", func() string { return "查看 squad 列表" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeSquadList(ctx) }},
	{"squad_detail", func() string { return "查看 squad <名字>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeSquadDetail(ctx, p) }},
	{"squad_update", func() string { return "修改 squad <名字>，leader 换成 <agent> / 描述改成 <描述>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeSquadUpdate(ctx, p) }},
	{"squad_add_member", func() string { return "给 squad <名字> 加成员 <agent>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeSquadAddMember(ctx, p) }},
	{"squad_remove_member", func() string { return "从 squad <名字> 移除成员 <agent>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeSquadRemoveMember(ctx, p) }},
	{"squad_delete", func() string { return "删除 squad <名字>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeSquadDelete(ctx, p) }},
	{"import_team", func() string { return "根据 <git URL> 创建一个 team" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeImportTeam(ctx, p) }},
	{"domain_create", func() string { return "创建项目 <名字>，仓库地址 <git url>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeCreateDomain(ctx, p) }},
	{"domain_list", func() string { return "查看项目列表" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeListDomains(ctx) }},
	{"goal_reopen", func() string { return "重开任务 <id>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeReopenGoal(ctx, p) }},
	{"goal_delete", func() string { return "删除任务 <id>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeDeleteGoal(ctx, p.GoalID) }},
	{"schedule_enable", func() string { return "启用定时任务 <名字>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeScheduleEnable(ctx, p) }},
	{"schedule_delete", func() string { return "删除定时任务 <名字>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeScheduleDelete(ctx, p) }},
	{"skill_list", func() string { return "查看 skill 列表" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeListSkills(ctx) }},
	{"skill_delete", func() string { return "删掉 skill <名字>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeDeleteSkill(ctx, p) }},
	{"domain_delete", func() string { return "删除项目 <名字>" },
		func(d *Daemon, ctx context.Context, p intakeAction) string { return d.intakeDeleteDomain(ctx, p) }},
}}

// dispatch finds the intent in the registry and calls its handler; if not
// found (parser returned unknown or a hallucinated intent), returns the
// fallback prompt. This replaces the former switch in replyIntake — the
// registry is now the single source of truth for both routing and hint.
func (r *intakeRegistry) dispatch(d *Daemon, ctx context.Context, parsed intakeAction) string {
	for _, c := range r.cmds {
		if c.intent == parsed.Intent {
			return c.handle(d, ctx, parsed)
		}
	}
	return r.fallbackReply()
}

// fallbackReply assembles the "didn't understand" prompt by iterating the
// registry's hint providers. Byte-identical to the former single-string
// literal (header + \n-joined items, no trailing \n).
func (r *intakeRegistry) fallbackReply() string {
	lines := make([]string, len(r.cmds))
	for i, c := range r.cmds {
		lines[i] = fmt.Sprintf("- “%s”", c.hint())
	}
	return "没听懂这条指令 😅 你可以这样问我：\n" + strings.Join(lines, "\n")
}

// replyIntake executes the parsed action and replies over IM. The run row is
// stamped completed here (the daemon owns processor-run finishing).
func (d *Daemon) replyIntake(ctx context.Context, q *service.ClaimedRow, parsed intakeAction, runType string) {
	reply := intakeReg.dispatch(d, ctx, parsed)
	if _, err := d.st.DB().ExecContext(ctx,
		`UPDATE run SET status='completed', result_summary=?, finished_at=? WHERE id=?`,
		reply, nowStr(), q.RunID); err != nil {
		logging.Infof("daemon: finish intake run %s: %v", q.RunID, err)
	}
	if runType != "intake_web" {
		if n := d.imNotifier(); n != nil {
			if err := n.Send(reply); err != nil {
				logging.Errorf("daemon: intake reply: %v", err)
			}
		}
	}
	logging.Infof("daemon: intake %s → %s", q.RunID, parsed.Intent)
}

// intakeCreateGoal creates the goal (active → first run enqueued) through the
// service layer — the goal layer validates assignee/domain, so a parser
// hallucinating an id fails here with the validator's message, not a
// platform crash. Two-branch ask-once structure: merge-from-draft on the
// clarification turn, else collect ALL missing fields and ask once.
func (d *Daemon) intakeCreateGoal(ctx context.Context, parsed intakeAction) string {
	g := parsed.Goal
	hasAgents := d.platformHasAgents(ctx)
	if draft, ok := d.loadDraftOfKind(ctx, "goal"); ok {
		merged := mergeGoal(draft.Payload, g)
		if d.intakeSvc != nil {
			_ = d.intakeSvc.ClearDraft(ctx)
		}
		return d.doCreateGoal(ctx, merged, hasAgents)
	}
	if missing := goalMissingFields(g, hasAgents); len(missing) > 0 {
		// No agents at all → there is nothing to ask about (the assignee slot
		// is empty and the roster is empty); surface the setup hint, not an ask.
		if !hasAgents && strings.TrimSpace(g.AssigneeID) == "" {
			return "创建任务失败：没有可用的 agent（先在 Web 配置 agent）"
		}
		return d.collectAndAsk(ctx, "goal", mustMarshal(g), missing)
	}
	return d.doCreateGoal(ctx, g, hasAgents)
}

// doCreateGoal calls the service layer and returns the reply. P0-2
// (决策 6-15②): the active goal's first run is born in Create's transaction
// — no separate enqueue. hasAgents gates the assignee check: a merge that
// still lacks an assignee (vague reply) fails here only when agents exist
// (otherwise the goal layer would reject it; we surface the clearer message).
func (d *Daemon) doCreateGoal(ctx context.Context, g goalAction, hasAgents bool) string {
	if strings.TrimSpace(g.Title) == "" {
		return "创建任务失败：缺少标题"
	}
	if strings.TrimSpace(g.DomainID) == "" {
		return "创建任务失败：缺少项目/仓库"
	}
	assigneeType := g.AssigneeType
	if strings.TrimSpace(assigneeType) == "" {
		assigneeType = "agent"
	}
	if assigneeType == "agent" && hasAgents && strings.TrimSpace(g.AssigneeID) == "" {
		return "创建任务失败：没有可用的 agent（先在 Web 配置 agent）"
	}
	created, err := d.goalSvc.Create(ctx, service.Goal{
		Title:         g.Title,
		Description:   g.Description,
		DomainID:      g.DomainID,
		AssigneeType:  assigneeType,
		AssigneeID:    g.AssigneeID,
		Status:        "active",
		CreatedByType: "human",
	})
	if err != nil {
		return "创建任务失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 已创建任务：%s（goal %s），%s 开始执行", created.Title, shortID(created.ID), assigneeType)
}

// intakeDomainList lists the available domains for the clarification ask.
func (d *Daemon) intakeDomainList(ctx context.Context) string {
	var b strings.Builder
	rows, err := d.st.DB().QueryContext(ctx, `SELECT name FROM domain ORDER BY name`)
	if err != nil {
		return "（当前没有可用项目——先在 Web 建域）"
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", name)
		n++
	}
	if n == 0 {
		return "（当前没有可用项目——先在 Web 建域）"
	}
	return b.String()
}

// intakeReviewList answers "待审批" with the current checkpoint queue.
func (d *Daemon) intakeReviewList(ctx context.Context) string {
	if d.qs == nil {
		return "平台未就绪（store 未接线）"
	}
	goals, err := d.qs.ReviewGoals(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	if len(goals) == 0 {
		return "✅ 当前没有待审批的卡点"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔔 待审批（%d 个）：\n", len(goals))
	for _, g := range goals {
		nameID := fmt.Sprintf("%s（%s）", g.Title, shortID(g.GoalID))
		fmt.Fprintf(&b, "- %s | %s\n", nameID, truncateIn(firstLineIn(g.Reason), listDescLimit))
	}
	b.WriteString("\n在飞书里点对应卡片的按钮，或打开 Web 审批队列处理。")
	return b.String()
}

// intakeGoalStatus answers "状态 <id>" (id or short id) with the goal's
// state and its last run's outcome.
func (d *Daemon) intakeGoalStatus(ctx context.Context, id string) string {
	if d.qs == nil {
		return "平台未就绪（store 未接线）"
	}
	if strings.TrimSpace(id) == "" {
		return "查询任务状态需要任务 id（如：查询任务状态 3f2a1b）"
	}
	v, err := d.qs.GoalStatus(ctx, strings.TrimSpace(id))
	if err != nil {
		return "查询失败：找不到该任务（" + err.Error() + "）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📌 %s（%s）\n状态：%s", v.Title, shortID(v.GoalID), v.Status)
	if v.ReviewRequest != "" {
		b.WriteString("\n待审：" + v.ReviewRequest)
	}
	if v.Summary != "" {
		b.WriteString("\n最近结果：" + truncateIn(v.Summary, 200))
	}
	return b.String()
}

// intakeCreateSchedule creates a cron schedule through the service layer —
// the parser converts natural-language frequency to cron, the platform
// validates (cron syntax, assignee/domain existence) and computes the first
// next_run_at.
func (d *Daemon) intakeCreateSchedule(ctx context.Context, parsed intakeAction) string {
	sch := parsed.Schedule
	if strings.TrimSpace(sch.Name) == "" || strings.TrimSpace(sch.Title) == "" {
		return "创建定时任务失败：缺少任务名或任务标题"
	}
	if strings.TrimSpace(sch.Cron) == "" {
		return "创建定时任务失败：缺少 cron 表达式（没听懂频率？）"
	}
	if strings.TrimSpace(sch.DomainID) == "" {
		return "创建定时任务失败：没有可用的 domain（先在 Web 建域并配置验收策略）"
	}
	if strings.TrimSpace(sch.AssigneeID) == "" {
		return "创建定时任务失败：缺少执行者（agent 或 squad）"
	}
	assigneeType := sch.AssigneeType
	if strings.TrimSpace(assigneeType) == "" {
		assigneeType = "agent"
	}
	// The schedule runs on the daemon machine's OWN local time — the owner
	// speaks in their local hours ("每天 9 点"), and on a single-user machine
	// that IS the daemon's zone. Hardcoding a zone (e.g. Asia/Shanghai)
	// silently mis-times every schedule on a machine in another zone.
	timezone := time.Local.String()
	if timezone == "" {
		timezone = "UTC"
	}
	s, err := d.schedSvc.Create(ctx, service.Schedule{
		Name:           sch.Name,
		TitleTemplate:  sch.Title,
		Description:    sch.Description,
		AssigneeType:   assigneeType,
		AssigneeID:     sch.AssigneeID,
		DomainID:       sch.DomainID,
		CronExpression: sch.Cron,
		Timezone:       timezone,
		Enabled:        true,
	})
	if err != nil {
		return "创建定时任务失败：" + err.Error()
	}
	next := s.NextRunAt
	if next != "" {
		if t, err := time.Parse(time.RFC3339Nano, next); err == nil {
			next = t.Local().Format("01-02 15:04")
		}
	}
	return fmt.Sprintf("✅ 已创建定时任务：%s（%s），下次执行 %s（本地时间）", s.Name, s.CronExpression, next)
}

// intakeScheduleList answers "查看定时任务" with the enabled schedules.
func (d *Daemon) intakeScheduleList(ctx context.Context) string {
	all, err := d.schedSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	enabled := []service.Schedule{}
	for _, s := range all {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	if len(enabled) == 0 {
		return "📭 当前没有启用的定时任务"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📅 启用的定时任务（%d 个）：\n", len(enabled))
	for _, s := range enabled {
		nameCron := fmt.Sprintf("%s（%s）", s.Name, s.CronExpression)
		desc := truncateIn(firstLineIn(s.Description), listDescLimit)
		if desc != "" {
			fmt.Fprintf(&b, "- %s | %s\n", nameCron, desc)
		} else {
			fmt.Fprintf(&b, "- %s\n", nameCron)
		}
	}
	b.WriteString("\n停掉某个：发送“停掉定时任务 <名字>”")
	return b.String()
}

// intakeScheduleStop disables a schedule by name (the row and firing history
// stay; dispatchSchedules only fires enabled rows).
func (d *Daemon) intakeScheduleStop(ctx context.Context, parsed intakeAction) string {
	name := strings.TrimSpace(parsed.Schedule.Name)
	if name == "" {
		return "停掉定时任务需要名字（如：停掉定时任务 每小时巡检）"
	}
	all, err := d.schedSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	var target *service.Schedule
	for i := range all {
		if all[i].Enabled && all[i].Name == name {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return fmt.Sprintf("没找到启用的定时任务 %q——先“查看定时任务”确认名字", name)
	}
	if _, err := d.schedSvc.SetEnabled(ctx, target.ID, false); err != nil {
		return "停用失败：" + err.Error()
	}
	return fmt.Sprintf("⏹ 已停用定时任务：%s（%s）", target.Name, target.CronExpression)
}

// intakeCreateAgent creates an agent (persona + runtime + optional skills)
// through the service layer. Two branches:
//   - Clarification turn (agent-kind draft exists): merge draft + this turn's
//     parsed fields, clear the draft, and commit — the "ask at most once"
//     guarantee means a vague reply still goes to Create (which validates);
//     it never re-asks.
//   - Fresh create: collect ALL missing required fields at once, save the
//     parser's partial output as a draft, and ask in ONE message.
func (d *Daemon) intakeCreateAgent(ctx context.Context, parsed intakeAction) string {
	a := parsed.Agent
	if draft, ok := d.loadDraftOfKind(ctx, "agent"); ok {
		merged := mergeAgent(draft.Payload, a)
		if d.intakeSvc != nil {
			_ = d.intakeSvc.ClearDraft(ctx)
		}
		return d.doCreateAgent(ctx, merged)
	}
	if missing := agentMissingFields(a, d.platformHasSkills(ctx)); len(missing) > 0 {
		return d.collectAndAsk(ctx, "agent", mustMarshal(a), missing)
	}
	return d.doCreateAgent(ctx, a)
}

// doCreateAgent calls the service layer and returns the reply — no
// validation, no draft logic. The service validates name/runtime_id and the
// runtime's existence; a hallucinated or still-empty id fails here with the
// validator's message (a terminal error, not a re-ask).
func (d *Daemon) doCreateAgent(ctx context.Context, a agentAction) string {
	created, err := d.agentSvc.Create(ctx, service.Agent{
		Name:         a.Name,
		RuntimeID:    a.RuntimeID,
		Description:  a.Description,
		SystemPrompt: a.SystemPrompt,
		Skills:       a.Skills,
	})
	if err != nil {
		return "创建 agent 失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 已创建 agent：%s（%s）", created.Name, shortID(created.ID))
}

// intakeCreateSquad creates a squad (leader + optional members). Same
// two-branch ask-once structure as the agent handler.
func (d *Daemon) intakeCreateSquad(ctx context.Context, parsed intakeAction) string {
	sq := parsed.Squad
	if draft, ok := d.loadDraftOfKind(ctx, "squad"); ok {
		merged := mergeSquad(draft.Payload, sq)
		if d.intakeSvc != nil {
			_ = d.intakeSvc.ClearDraft(ctx)
		}
		return d.doCreateSquad(ctx, merged)
	}
	if missing := squadMissingFields(sq); len(missing) > 0 {
		return d.collectAndAsk(ctx, "squad", mustMarshal(sq), missing)
	}
	return d.doCreateSquad(ctx, sq)
}

// doCreateSquad creates the squad then attaches members (skipping the leader,
// which is already squad.leader_id). Hallucinated members surface as a
// partial-success reply.
func (d *Daemon) doCreateSquad(ctx context.Context, sq squadAction) string {
	created, err := d.squadSvc.Create(ctx, service.Squad{
		Name:         sq.Name,
		LeaderID:     sq.LeaderID,
		Description:  sq.Description,
		Instructions: sq.Instructions,
	})
	if err != nil {
		return "创建 squad 失败：" + err.Error()
	}
	var failed []string
	for _, mid := range sq.MemberIDs {
		mid = strings.TrimSpace(mid)
		if mid == "" || mid == sq.LeaderID {
			continue
		}
		if _, err := d.squadSvc.AddMember(ctx, created.ID, "agent", mid, "member"); err != nil {
			failed = append(failed, mid)
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf("⚠️ 已创建 squad：%s（%s），但以下成员添加失败：%s", created.Name, shortID(created.ID), strings.Join(failed, "、"))
	}
	return fmt.Sprintf("✅ 已创建 squad：%s（%s）", created.Name, shortID(created.ID))
}

// resolveSquadByName finds a squad by exact name (the parser copies the
// user's wording). Same lookup pattern as intakeScheduleStop.
func (d *Daemon) resolveSquadByName(ctx context.Context, name string) (*service.Squad, error) {
	all, err := d.squadSvc.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("没找到 squad %q——先「查看 squad 列表」确认名字", name)
}

// agentDisplayName resolves an agent id to a name, falling back to the short
// id prefix when the QueryStore is nil or the agent was deleted.
func (d *Daemon) agentDisplayName(ctx context.Context, agentID string) string {
	if d.qs != nil {
		if n, err := d.qs.AgentName(ctx, agentID); err == nil && n != "" {
			return n
		}
	}
	return shortID(agentID)
}

// resolveAgentByName finds an agent by exact name (the parser copies the
// user's wording). Same lookup pattern as resolveSquadByName.
func (d *Daemon) resolveAgentByName(ctx context.Context, name string) (*service.Agent, error) {
	all, err := d.agentSvc.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("没找到 agent %q——先「查看 agent 列表」确认名字", name)
}

// assigneeDisplayName resolves an assignee id to a display name, branching on
// assignee type (agent / squad / human).
func (d *Daemon) assigneeDisplayName(ctx context.Context, assigneeType, assigneeID string) string {
	switch assigneeType {
	case "agent":
		return d.agentDisplayName(ctx, assigneeID)
	case "squad":
		if d.squadSvc != nil {
			if sq, err := d.squadSvc.Get(ctx, assigneeID); err == nil && sq != nil {
				return sq.Name
			}
		}
		return shortID(assigneeID)
	default:
		return "未分配"
	}
}

// intakeSquadList answers "查看 squad 列表" with all squads, their leader
// name, and member count.
func (d *Daemon) intakeSquadList(ctx context.Context) string {
	all, err := d.squadSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	if len(all) == 0 {
		return "📭 当前没有 squad"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "👥 squad 列表（%d 个）：\n", len(all))
	for _, sq := range all {
		leader := d.agentDisplayName(ctx, sq.LeaderID)
		members, _ := d.squadSvc.ListMembers(ctx, sq.ID)
		nameID := fmt.Sprintf("%s（%s）", sq.Name, shortID(sq.ID))
		detail := "leader: " + leader
		if n := len(members); n > 0 {
			detail += fmt.Sprintf("，成员 %d 人", n)
		}
		fmt.Fprintf(&b, "- %s | %s\n", nameID, detail)
	}
	b.WriteString("\n查看详情：发送「查看 squad <名字>」")
	return b.String()
}

// intakeSquadDetail answers "查看 squad <名字>" with the full roster.
func (d *Daemon) intakeSquadDetail(ctx context.Context, parsed intakeAction) string {
	name := strings.TrimSpace(parsed.Squad.Name)
	if name == "" {
		return "查看 squad 详情需要名字（如：查看 squad 审查组）"
	}
	return d.squadDetailText(ctx, name)
}

func (d *Daemon) squadDetailText(ctx context.Context, name string) string {
	sq, err := d.resolveSquadByName(ctx, name)
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "👥 %s\n", sq.Name)
	if sq.Description != "" {
		fmt.Fprintf(&b, "描述：\n    %s\n", firstLineIn(sq.Description))
	}
	fmt.Fprintf(&b, "leader：\n    %s\n", d.agentDisplayName(ctx, sq.LeaderID))
	if sq.Instructions != "" {
		fmt.Fprintf(&b, "协作规则：\n    %s\n", truncateIn(sq.Instructions, 200))
	}
	members, err := d.squadSvc.ListMembers(ctx, sq.ID)
	if err == nil && len(members) > 0 {
		b.WriteString("成员：\n")
		for _, m := range members {
			fmt.Fprintf(&b, "      - %s（%s）\n", d.agentDisplayName(ctx, m.MemberID), m.Role)
		}
	} else {
		b.WriteString("成员：（无）\n")
	}
	return b.String()
}

// intakeSquadUpdate updates a squad's leader/description/instructions. Only
// non-empty parsed fields are applied (partial update); rename is Web-only
// (squad.name is the lookup key, not the new name).
func (d *Daemon) intakeSquadUpdate(ctx context.Context, parsed intakeAction) string {
	name := strings.TrimSpace(parsed.Squad.Name)
	if name == "" {
		return "修改 squad 需要名字（如：把审查组的 leader 换成 agent2）"
	}
	sq, err := d.resolveSquadByName(ctx, name)
	if err != nil {
		return err.Error()
	}
	leaderID, description, instructions := sq.LeaderID, sq.Description, sq.Instructions
	changed := false
	if strings.TrimSpace(parsed.Squad.LeaderID) != "" {
		leaderID = parsed.Squad.LeaderID
		changed = true
	}
	if strings.TrimSpace(parsed.Squad.Description) != "" {
		description = parsed.Squad.Description
		changed = true
	}
	if strings.TrimSpace(parsed.Squad.Instructions) != "" {
		instructions = parsed.Squad.Instructions
		changed = true
	}
	if !changed {
		return "修改 squad 需要指定改什么（如：把审查组的 leader 换成 agent2 / 把审查组的描述改成 xxx）"
	}
	updated, err := d.squadSvc.Update(ctx, sq.ID, service.Squad{
		Name: sq.Name, LeaderID: leaderID, Description: description, Instructions: instructions,
	})
	if err != nil {
		return "修改 squad 失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 已更新 squad：%s（%s）", updated.Name, shortID(updated.ID))
}

// intakeSquadAddMember attaches agent members to an existing squad.
func (d *Daemon) intakeSquadAddMember(ctx context.Context, parsed intakeAction) string {
	name := strings.TrimSpace(parsed.Squad.Name)
	if name == "" {
		return "添加成员需要 squad 名字（如：给审查组加成员 agent2）"
	}
	sq, err := d.resolveSquadByName(ctx, name)
	if err != nil {
		return err.Error()
	}
	if len(parsed.Squad.MemberIDs) == 0 {
		return "添加成员需要指定 agent（如：给审查组加成员 agent2）"
	}
	var failed []string
	for _, mid := range parsed.Squad.MemberIDs {
		mid = strings.TrimSpace(mid)
		if mid == "" || mid == sq.LeaderID {
			continue
		}
		if _, err := d.squadSvc.AddMember(ctx, sq.ID, "agent", mid, "member"); err != nil {
			failed = append(failed, mid)
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf("⚠️ 已添加部分成员到 squad %s，但以下添加失败：%s", sq.Name, strings.Join(failed, "、"))
	}
	return fmt.Sprintf("✅ 已添加成员到 squad：%s（%s）", sq.Name, shortID(sq.ID))
}

// intakeSquadRemoveMember detaches members from a squad. Reports "not in
// squad" for members that aren't attached (not a silent no-op).
func (d *Daemon) intakeSquadRemoveMember(ctx context.Context, parsed intakeAction) string {
	name := strings.TrimSpace(parsed.Squad.Name)
	if name == "" {
		return "移除成员需要 squad 名字（如：从审查组移除 agent2）"
	}
	sq, err := d.resolveSquadByName(ctx, name)
	if err != nil {
		return err.Error()
	}
	if len(parsed.Squad.MemberIDs) == 0 {
		return "移除成员需要指定 agent（如：从审查组移除 agent2）"
	}
	current, _ := d.squadSvc.ListMembers(ctx, sq.ID)
	memberSet := make(map[string]bool, len(current))
	for _, m := range current {
		memberSet[m.MemberID] = true
	}
	var notIn, failed []string
	for _, mid := range parsed.Squad.MemberIDs {
		mid = strings.TrimSpace(mid)
		if mid == "" {
			continue
		}
		if !memberSet[mid] {
			notIn = append(notIn, mid)
			continue
		}
		if err := d.squadSvc.RemoveMember(ctx, sq.ID, mid); err != nil {
			failed = append(failed, mid)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ 已从 squad %s 移除成员", sq.Name)
	if len(notIn) > 0 {
		fmt.Fprintf(&b, "（以下不在 squad 里：%s）", strings.Join(notIn, "、"))
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "（以下移除失败：%s）", strings.Join(failed, "、"))
	}
	return b.String()
}

// intakeSquadDelete deletes a squad. Goals assigned to it fall back to human
// (the service layer handles this). Supports comma-separated batch delete.
func (d *Daemon) intakeSquadDelete(ctx context.Context, parsed intakeAction) string {
	raw := strings.TrimSpace(parsed.Squad.Name)
	if raw == "" {
		return "删除 squad 需要名字（如：删除 squad 审查组）"
	}
	deleteOne := func(name string) string {
		sq, err := d.resolveSquadByName(ctx, name)
		if err != nil {
			return err.Error()
		}
		if err := d.squadSvc.Delete(ctx, sq.ID); err != nil {
			return "删除 squad 失败：" + err.Error()
		}
		return fmt.Sprintf("✅ 已删除 squad：%s（%s）", sq.Name, shortID(sq.ID))
	}
	names := splitAndTrim(raw)
	if len(names) == 1 {
		return deleteOne(names[0])
	}
	return batchDelete(names, deleteOne)
}

// intakeImportTeam triggers a team-repo import from a git URL parsed out of
// the owner's NL message. The import is executed as a system-task goal
// assigned to the steward — the goal appears in the goal list with a
// timeline, and auto-completes when the import finishes.
func (d *Daemon) intakeImportTeam(ctx context.Context, parsed intakeAction) string {
	it := parsed.ImportTeam
	if strings.TrimSpace(it.GitURL) == "" {
		return "导入失败：缺少 git 仓库地址"
	}
	if d.teamImportSvc == nil {
		return "导入失败：team import 服务未接线"
	}
	_, goal, err := d.teamImportSvc.ImportTeam(ctx, service.ImportRequest{
		GitURL:         it.GitURL,
		DefaultBranch:  it.Branch,
		GitCredentials: it.Credentials,
	})
	if err != nil {
		return "导入失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 团队导入已启动，管家正在执行（任务 %s）", shortID(goal.ID))
}

// intakeListGoals answers "查看任务列表" with all goals (capped at 20 for IM
// readability), showing title, short id, status, and assignee display name.
func (d *Daemon) intakeListGoals(ctx context.Context) string {
	goals, err := d.goalSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	if len(goals) == 0 {
		return "📭 当前没有任务"
	}
	if len(goals) > 20 {
		goals = goals[:20]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📋 任务列表（显示最近 %d 个）：\n", len(goals))
	for _, g := range goals {
		assignee := d.assigneeDisplayName(ctx, g.AssigneeType, g.AssigneeID)
		nameID := fmt.Sprintf("%s（%s）", g.Title, shortID(g.ID))
		fmt.Fprintf(&b, "- %s | [%s] | 执行者：%s\n", nameID, g.Status, assignee)
	}
	b.WriteString("\n查询详情：发送「查询任务状态 <id>」")
	return b.String()
}

// intakeCancelGoal answers "取消任务 <id>" — resolves the short id via the
// query store, then calls the goal service's Cancel (which refuses terminal
// goals and cascades to queued runs and active sub-goals).
func (d *Daemon) intakeCancelGoal(ctx context.Context, id string) string {
	if strings.TrimSpace(id) == "" {
		return "取消任务需要任务 id（如：取消任务 3f2a1b）"
	}
	v, err := d.qs.GoalStatus(ctx, strings.TrimSpace(id))
	if err != nil {
		return "取消失败：找不到该任务（" + err.Error() + "）"
	}
	if _, err := d.goalSvc.Cancel(ctx, v.GoalID); err != nil {
		return "取消失败：" + err.Error()
	}
	return fmt.Sprintf("⏹ 已取消任务：%s（%s）", v.Title, shortID(v.GoalID))
}

// intakeAssignGoal answers "把任务 <id> 转给 <agent>" — resolves the short id,
// then calls the goal service's Assign (human actor, single-user platform).
// goal.description carries the optional handoff note.
func (d *Daemon) intakeAssignGoal(ctx context.Context, parsed intakeAction) string {
	id := strings.TrimSpace(parsed.GoalID)
	if id == "" {
		return "转交任务需要任务 id（如：把任务 3f2a1b 转给 agent2）"
	}
	v, err := d.qs.GoalStatus(ctx, id)
	if err != nil {
		return "转交失败：找不到该任务（" + err.Error() + "）"
	}
	assigneeID := strings.TrimSpace(parsed.Goal.AssigneeID)
	if assigneeID == "" {
		return "转交任务需要指定执行者（如：把任务 3f2a1b 转给 agent2）"
	}
	assigneeType := parsed.Goal.AssigneeType
	if strings.TrimSpace(assigneeType) == "" {
		assigneeType = "agent"
	}
	if _, err := d.goalSvc.Assign(ctx, v.GoalID, assigneeType, assigneeID,
		parsed.Goal.Description, "human", ""); err != nil {
		return "转交失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 已转交任务：%s（%s）→ %s",
		v.Title, shortID(v.GoalID), d.assigneeDisplayName(ctx, assigneeType, assigneeID))
}

// intakeListAgents answers "查看 agent 列表" with all agents and their
// descriptions.
func (d *Daemon) intakeListAgents(ctx context.Context) string {
	agents, err := d.agentSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	if len(agents) == 0 {
		return "📭 当前没有 agent（先创建一个：发送「创建 agent ...」）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🤖 agent 列表（%d 个）：\n", len(agents))
	for _, a := range agents {
		nameID := fmt.Sprintf("%s（%s）", a.Name, shortID(a.ID))
		desc := truncateIn(firstLineIn(a.Description), listDescLimit)
		if desc != "" {
			fmt.Fprintf(&b, "- %s | %s\n", nameID, desc)
		} else {
			fmt.Fprintf(&b, "- %s\n", nameID)
		}
	}
	b.WriteString("\n配置/查看详情：打开 Web Agents 页面")
	return b.String()
}

// intakeDeleteAgent answers "删掉 agent <名字>" — resolves by name, then calls
// the agent service's Delete (which has referential guards: goals, schedules,
// squads, running runs all block the delete with a coded error).
// Supports comma-separated batch delete: each result is reported on its own
// line; a single name produces the original one-line reply (no summary).
func (d *Daemon) intakeDeleteAgent(ctx context.Context, parsed intakeAction) string {
	raw := strings.TrimSpace(parsed.Agent.Name)
	if raw == "" {
		return "删除 agent 需要名字（如：删掉 agent worker1）"
	}
	deleteOne := func(name string) string {
		existing, err := d.resolveAgentByName(ctx, name)
		if err != nil {
			return err.Error()
		}
		if err := d.agentSvc.Delete(ctx, existing.ID); err != nil {
			return "删除 agent 失败：" + err.Error()
		}
		return fmt.Sprintf("✅ 已删除 agent：%s（%s）", existing.Name, shortID(existing.ID))
	}
	names := splitAndTrim(raw)
	if len(names) == 1 {
		return deleteOne(names[0])
	}
	return batchDelete(names, deleteOne)
}

// intakeUpdateAgent answers "把 agent <名字> 的人设/描述改成 <新值>" — resolves
// by name, overlays the parsed fields onto the existing agent (partial update:
// only non-empty parsed fields are applied), then calls the service's Update.
// Technical config (env/model/mcp_servers/skills/max_concurrent) is preserved
// from the existing row — NL only edits persona-level fields.
func (d *Daemon) intakeUpdateAgent(ctx context.Context, parsed intakeAction) string {
	name := strings.TrimSpace(parsed.Agent.Name)
	if name == "" {
		return "修改 agent 需要名字（如：把 worker1 的描述改成 xxx）"
	}
	existing, err := d.resolveAgentByName(ctx, name)
	if err != nil {
		return err.Error()
	}
	updated := *existing
	changed := false
	if strings.TrimSpace(parsed.Agent.Description) != "" {
		updated.Description = parsed.Agent.Description
		changed = true
	}
	if strings.TrimSpace(parsed.Agent.SystemPrompt) != "" {
		updated.SystemPrompt = parsed.Agent.SystemPrompt
		changed = true
	}
	if strings.TrimSpace(parsed.Agent.RuntimeID) != "" {
		updated.RuntimeID = parsed.Agent.RuntimeID
		changed = true
	}
	if !changed {
		return "修改 agent 需要指定改什么（如：把 worker1 的描述改成 xxx / 人设改成 yyy）"
	}
	result, err := d.agentSvc.Update(ctx, existing.ID, updated)
	if err != nil {
		return "修改 agent 失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 已更新 agent：%s（%s）", result.Name, shortID(result.ID))
}

// intakeCreateDomain answers "创建项目 <名字>，仓库地址 <git url>" — same
// two-branch ask-once structure as the other create handlers: merge-from-draft
// on the clarification turn, else collect ALL missing fields and ask once.
func (d *Daemon) intakeCreateDomain(ctx context.Context, parsed intakeAction) string {
	dm := parsed.Domain
	if draft, ok := d.loadDraftOfKind(ctx, "domain"); ok {
		merged := mergeDomain(draft.Payload, dm)
		if d.intakeSvc != nil {
			_ = d.intakeSvc.ClearDraft(ctx)
		}
		return d.doCreateDomain(ctx, merged)
	}
	if missing := domainMissingFields(dm); len(missing) > 0 {
		return d.collectAndAsk(ctx, "domain", mustMarshal(dm), missing)
	}
	return d.doCreateDomain(ctx, dm)
}

// doCreateDomain calls the service layer and returns the reply.
func (d *Daemon) doCreateDomain(ctx context.Context, dm domainAction) string {
	if strings.TrimSpace(dm.Name) == "" {
		return "创建项目失败：缺少名称"
	}
	domainType := dm.Type
	if strings.TrimSpace(domainType) == "" {
		domainType = "repo"
	}
	if domainType == "repo" && strings.TrimSpace(dm.GitURL) == "" {
		return "创建项目失败：缺少仓库地址"
	}
	if d.domainSvc == nil {
		return "创建项目失败：domain 服务未接线"
	}
	created, err := d.domainSvc.Create(ctx, service.Domain{
		Name:           dm.Name,
		Type:           domainType,
		GitURL:         dm.GitURL,
		DefaultBranch:  dm.DefaultBranch,
		GitIdentity:    dm.GitIdentity,
		GitCredentials: dm.GitCredentials,
	})
	if err != nil {
		return "创建项目失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 已创建项目：%s（%s）", created.Name, shortID(created.ID))
}

// intakeListDomains answers "查看项目列表" with all domains.
func (d *Daemon) intakeListDomains(ctx context.Context) string {
	if d.domainSvc == nil {
		return "查询失败：domain 服务未接线"
	}
	domains, err := d.domainSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	if len(domains) == 0 {
		return "📭 当前没有项目（先创建一个：发送「创建项目 ...」）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📁 项目列表（%d 个）：\n", len(domains))
	for _, dm := range domains {
		nameID := fmt.Sprintf("%s（%s）", dm.Name, shortID(dm.ID))
		detail := fmt.Sprintf("[%s]", dm.Type)
		if dm.GitURL != "" {
			detail += " " + dm.GitURL
		}
		fmt.Fprintf(&b, "- %s | %s\n", nameID, detail)
	}
	b.WriteString("\n创建任务时指定项目名即可")
	return b.String()
}

// intakeReopenGoal answers "重开任务 <id>" — resolves the short id, then calls
// the goal service's Reopen (which accepts only done/failed/cancelled goals).
// goal.description carries the optional reopen reason.
func (d *Daemon) intakeReopenGoal(ctx context.Context, parsed intakeAction) string {
	id := strings.TrimSpace(parsed.GoalID)
	if id == "" {
		return "重开任务需要任务 id（如：重开任务 3f2a1b）"
	}
	v, err := d.qs.GoalStatus(ctx, id)
	if err != nil {
		return "重开失败：找不到该任务（" + err.Error() + "）"
	}
	reason := strings.TrimSpace(parsed.Goal.Description)
	if _, err := d.goalSvc.Reopen(ctx, v.GoalID, reason, ""); err != nil {
		return "重开失败：" + err.Error()
	}
	return fmt.Sprintf("✅ 已重开任务：%s（%s）", v.Title, shortID(v.GoalID))
}

// intakeDeleteGoal answers "删除任务 <id>" — resolves the short id for the
// title (used in the reply), then calls the goal service's Delete (which
// cascades to runs, sub-goals, comments, activity logs, etc.).
// Supports comma-separated batch delete.
func (d *Daemon) intakeDeleteGoal(ctx context.Context, id string) string {
	raw := strings.TrimSpace(id)
	if raw == "" {
		return "删除任务需要任务 id（如：删除任务 3f2a1b）"
	}
	deleteOne := func(idOrShort string) string {
		v, err := d.qs.GoalStatus(ctx, idOrShort)
		if err != nil {
			return "删除失败：找不到该任务（" + err.Error() + "）"
		}
		if err := d.goalSvc.Delete(ctx, v.GoalID); err != nil {
			return "删除失败：" + err.Error()
		}
		logging.Infof("daemon: intake deleted goal %s (%s)", v.GoalID, v.Title)
		return fmt.Sprintf("🗑 已删除任务：%s（%s）", v.Title, shortID(v.GoalID))
	}
	ids := splitAndTrim(raw)
	if len(ids) == 1 {
		return deleteOne(ids[0])
	}
	return batchDelete(ids, deleteOne)
}

// intakeScheduleEnable answers "启用定时任务 <名字>" — finds the disabled
// schedule by name, re-enables it, and recomputes next_run_at from now (a
// schedule stopped long ago has a stale next_run_at that would fire
// immediately on the next dispatch tick).
func (d *Daemon) intakeScheduleEnable(ctx context.Context, parsed intakeAction) string {
	name := strings.TrimSpace(parsed.Schedule.Name)
	if name == "" {
		return "启用定时任务需要名字（如：启用定时任务 每小时巡检）"
	}
	all, err := d.schedSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	var target *service.Schedule
	for i := range all {
		if !all[i].Enabled && all[i].Name == name {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return fmt.Sprintf("没找到已停用的定时任务 %q——先「查看定时任务」确认名字", name)
	}
	s, err := d.schedSvc.SetEnabled(ctx, target.ID, true)
	if err != nil {
		return "启用失败：" + err.Error()
	}
	next, err := service.ComputeNextRun(s.CronExpression, s.Timezone, time.Now())
	if err != nil {
		logging.Warnf("daemon: schedule %s recompute next_run_at failed: %v", s.ID, err)
	} else {
		if _, err := d.st.DB().ExecContext(ctx,
			`UPDATE schedule SET next_run_at=? WHERE id=?`,
			next.Format(time.RFC3339Nano), s.ID); err != nil {
			logging.Errorf("daemon: schedule %s update next_run_at: %v", s.ID, err)
		}
	}
	nextStr := next.Format("01-02 15:04")
	return fmt.Sprintf("▶️ 已启用定时任务：%s（%s），下次执行 %s（本地时间）", s.Name, s.CronExpression, nextStr)
}

// intakeScheduleDelete answers "删除定时任务 <名字>" — finds the schedule by
// name (enabled or disabled), then calls the service's Delete (which removes
// the row and its firing history). Derived goals keep their original
// assignment. Supports comma-separated batch delete.
func (d *Daemon) intakeScheduleDelete(ctx context.Context, parsed intakeAction) string {
	raw := strings.TrimSpace(parsed.Schedule.Name)
	if raw == "" {
		return "删除定时任务需要名字（如：删除定时任务 每小时巡检）"
	}
	names := splitAndTrim(raw)
	if len(names) == 1 {
		return d.deleteScheduleByName(ctx, names[0])
	}
	return batchDelete(names, func(name string) string { return d.deleteScheduleByName(ctx, name) })
}

func (d *Daemon) deleteScheduleByName(ctx context.Context, name string) string {
	all, err := d.schedSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	var target *service.Schedule
	for i := range all {
		if all[i].Name == name {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return fmt.Sprintf("没找到定时任务 %q", name)
	}
	if err := d.schedSvc.Delete(ctx, target.ID); err != nil {
		return "删除失败：" + err.Error()
	}
	return fmt.Sprintf("🗑 已删除定时任务：%s（%s）", target.Name, target.CronExpression)
}

// intakeListSkills answers "查看 skill 列表" with all skill names (compact,
// one per line — IM/chat readability over detail; descriptions are Web-only).
func (d *Daemon) intakeListSkills(ctx context.Context) string {
	if d.skillSvc == nil {
		return "查询失败：skill 服务未接线"
	}
	skills, err := d.skillSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	if len(skills) == 0 {
		return "📭 当前没有 skill"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔧 skill 列表（%d 个）：\n", len(skills))
	for _, sk := range skills {
		fmt.Fprintf(&b, "- %s\n", sk.Name)
	}
	return b.String()
}

// intakeDeleteSkill answers "删掉 skill <名字>" — resolves by name, then calls
// the skill service's Delete (which has a referential guard: skills selected
// by any agent cannot be deleted). Supports comma-separated batch delete.
func (d *Daemon) intakeDeleteSkill(ctx context.Context, parsed intakeAction) string {
	if d.skillSvc == nil {
		return "删除失败：skill 服务未接线"
	}
	raw := strings.TrimSpace(parsed.Skill.Name)
	if raw == "" {
		return "删除 skill 需要名字（如：删掉 skill git-helper）"
	}
	names := splitAndTrim(raw)
	if len(names) == 1 {
		return d.deleteSkillByName(ctx, names[0])
	}
	return batchDelete(names, func(name string) string { return d.deleteSkillByName(ctx, name) })
}

func (d *Daemon) deleteSkillByName(ctx context.Context, name string) string {
	skills, err := d.skillSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	var target *service.Skill
	for i := range skills {
		if skills[i].Name == name {
			target = &skills[i]
			break
		}
	}
	if target == nil {
		return fmt.Sprintf("没找到 skill %q", name)
	}
	if err := d.skillSvc.Delete(ctx, target.ID); err != nil {
		return "删除 skill 失败：" + err.Error()
	}
	return fmt.Sprintf("🗑 已删除 skill：%s（%s）", target.Name, shortID(target.ID))
}

// intakeDeleteDomain answers "删除项目 <名字>" — resolves by name, then calls
// the domain service's Delete (which has referential guards: goals or
// schedules referencing the domain block the delete). Supports comma-separated
// batch delete.
func (d *Daemon) intakeDeleteDomain(ctx context.Context, parsed intakeAction) string {
	if d.domainSvc == nil {
		return "删除失败：domain 服务未接线"
	}
	raw := strings.TrimSpace(parsed.Domain.Name)
	if raw == "" {
		return "删除项目需要名字（如：删除项目 myrepo）"
	}
	names := splitAndTrim(raw)
	if len(names) == 1 {
		return d.deleteDomainByName(ctx, names[0])
	}
	return batchDelete(names, func(name string) string { return d.deleteDomainByName(ctx, name) })
}

func (d *Daemon) deleteDomainByName(ctx context.Context, name string) string {
	domains, err := d.domainSvc.List(ctx)
	if err != nil {
		return "查询失败：" + err.Error()
	}
	var target *service.Domain
	for i := range domains {
		if domains[i].Name == name {
			target = &domains[i]
			break
		}
	}
	if target == nil {
		return fmt.Sprintf("没找到项目 %q", name)
	}
	if err := d.domainSvc.Delete(ctx, target.ID); err != nil {
		return "删除项目失败：" + err.Error()
	}
	return fmt.Sprintf("🗑 已删除项目：%s（%s）", target.Name, shortID(target.ID))
}

// collectAndAsk builds ONE clarification message listing all missing required
// fields + the relevant rosters, saves the parser's partial output as a draft,
// and returns the message. The rosters included depend on kind and which
// fields are missing.
func (d *Daemon) collectAndAsk(ctx context.Context, kind, payloadJSON string, missing []string) string {
	var b strings.Builder
	switch kind {
	case "goal":
		b.WriteString("创建任务还需要以下信息：\n")
	case "agent":
		b.WriteString("创建 agent 还需要以下信息：\n")
	case "squad":
		b.WriteString("创建 squad 还需要以下信息：\n")
	case "domain":
		b.WriteString("创建项目还需要以下信息：\n")
	}
	for _, f := range missing {
		b.WriteString("- " + f + "\n")
	}
	b.WriteString("\n")
	// Rosters relevant to the kind.
	switch kind {
	case "goal":
		b.WriteString("当前可用项目：\n" + d.intakeDomainList(ctx))
		if d.platformHasAgents(ctx) {
			b.WriteString("当前可用 agent：\n" + d.intakeAgentList(ctx))
		}
	case "agent":
		b.WriteString("当前可用运行时：\n" + d.intakeRuntimeList(ctx))
		if d.platformHasSkills(ctx) {
			b.WriteString("\n平台 skill（选配，回复里带上要配的 skill 名，或明确回复“不要”）：\n" + d.intakeSkillList(ctx))
		}
	case "squad":
		b.WriteString("当前可用 agent：\n" + d.intakeAgentList(ctx))
	}
	b.WriteString("\n请一条消息回复所有信息。")
	if d.intakeSvc != nil {
		_ = d.intakeSvc.SaveDraft(ctx, notify.IntakeDraft{
			Kind: kind, Payload: payloadJSON, CreatedAt: nowStr(),
		})
	}
	return b.String()
}

// loadDraftOfKind returns the pending draft if its Kind matches. Other kinds
// (or none) are treated as absent — the flows do not interfere.
func (d *Daemon) loadDraftOfKind(ctx context.Context, kind string) (*notify.IntakeDraft, bool) {
	if d.intakeSvc == nil {
		return nil, false
	}
	draft, ok := d.intakeSvc.LoadDraft(ctx)
	if !ok || draft.Kind != kind {
		return nil, false
	}
	return draft, true
}

// mergeAgent merges a clarification reply onto a draft: the reply's non-empty
// fields override the draft's. Skills is taken from the reply only when
// SkillsSpecified is true (the reply explicitly selected or declined skills);
// otherwise the draft's skills/specified state is preserved.
func mergeAgent(draftPayload string, reply agentAction) agentAction {
	var draft agentAction
	_ = json.Unmarshal([]byte(draftPayload), &draft)
	if strings.TrimSpace(reply.Name) != "" {
		draft.Name = reply.Name
	}
	if strings.TrimSpace(reply.RuntimeID) != "" {
		draft.RuntimeID = reply.RuntimeID
	}
	if strings.TrimSpace(reply.Description) != "" {
		draft.Description = reply.Description
	}
	if strings.TrimSpace(reply.SystemPrompt) != "" {
		draft.SystemPrompt = reply.SystemPrompt
	}
	if reply.SkillsSpecified {
		draft.Skills = reply.Skills
		draft.SkillsSpecified = true
	}
	return draft
}

// mergeSquad merges a clarification reply onto a draft.
func mergeSquad(draftPayload string, reply squadAction) squadAction {
	var draft squadAction
	_ = json.Unmarshal([]byte(draftPayload), &draft)
	if strings.TrimSpace(reply.Name) != "" {
		draft.Name = reply.Name
	}
	if strings.TrimSpace(reply.LeaderID) != "" {
		draft.LeaderID = reply.LeaderID
	}
	if strings.TrimSpace(reply.Description) != "" {
		draft.Description = reply.Description
	}
	if strings.TrimSpace(reply.Instructions) != "" {
		draft.Instructions = reply.Instructions
	}
	if len(reply.MemberIDs) > 0 {
		draft.MemberIDs = reply.MemberIDs
	}
	return draft
}

// mergeGoal merges a clarification reply onto a draft.
func mergeGoal(draftPayload string, reply goalAction) goalAction {
	var draft goalAction
	_ = json.Unmarshal([]byte(draftPayload), &draft)
	if strings.TrimSpace(reply.Title) != "" {
		draft.Title = reply.Title
	}
	if strings.TrimSpace(reply.Description) != "" {
		draft.Description = reply.Description
	}
	if strings.TrimSpace(reply.AssigneeID) != "" {
		draft.AssigneeID = reply.AssigneeID
	}
	if strings.TrimSpace(reply.AssigneeType) != "" {
		draft.AssigneeType = reply.AssigneeType
	}
	if strings.TrimSpace(reply.DomainID) != "" {
		draft.DomainID = reply.DomainID
	}
	return draft
}

// agentMissingFields returns human names of required agent fields that are
// empty, plus skills when the library is non-empty and the owner did not
// mention skills (SkillsSpecified=false).
func agentMissingFields(a agentAction, hasSkills bool) []string {
	var out []string
	if strings.TrimSpace(a.Name) == "" {
		out = append(out, "名称")
	}
	if strings.TrimSpace(a.RuntimeID) == "" {
		out = append(out, "运行时")
	}
	if hasSkills && !a.SkillsSpecified {
		out = append(out, "skills（选配，或明确回复不要）")
	}
	return out
}

// squadMissingFields returns human names of required squad fields that are
// empty.
func squadMissingFields(sq squadAction) []string {
	var out []string
	if strings.TrimSpace(sq.Name) == "" {
		out = append(out, "名称")
	}
	if strings.TrimSpace(sq.LeaderID) == "" {
		out = append(out, "leader agent")
	}
	return out
}

// goalMissingFields returns human names of required goal fields that are
// empty. assignee_id is only required when the platform has at least one
// agent (otherwise there is nothing to ask — the platform is not configured,
// a hard error).
func goalMissingFields(g goalAction, hasAgents bool) []string {
	var out []string
	if strings.TrimSpace(g.Title) == "" {
		out = append(out, "标题")
	}
	if strings.TrimSpace(g.DomainID) == "" {
		out = append(out, "项目/仓库")
	}
	if hasAgents && strings.TrimSpace(g.AssigneeID) == "" {
		out = append(out, "执行的 agent")
	}
	return out
}

// mergeDomain merges a clarification reply onto a draft.
func mergeDomain(draftPayload string, reply domainAction) domainAction {
	var draft domainAction
	_ = json.Unmarshal([]byte(draftPayload), &draft)
	if strings.TrimSpace(reply.Name) != "" {
		draft.Name = reply.Name
	}
	if strings.TrimSpace(reply.Type) != "" {
		draft.Type = reply.Type
	}
	if strings.TrimSpace(reply.GitURL) != "" {
		draft.GitURL = reply.GitURL
	}
	if strings.TrimSpace(reply.DefaultBranch) != "" {
		draft.DefaultBranch = reply.DefaultBranch
	}
	if strings.TrimSpace(reply.GitIdentity) != "" {
		draft.GitIdentity = reply.GitIdentity
	}
	if strings.TrimSpace(reply.GitCredentials) != "" {
		draft.GitCredentials = reply.GitCredentials
	}
	return draft
}

// domainMissingFields returns human names of required domain fields that are
// empty. git_url is only required for repo type (scratch domains have no repo).
func domainMissingFields(dm domainAction) []string {
	var out []string
	if strings.TrimSpace(dm.Name) == "" {
		out = append(out, "名称")
	}
	domainType := dm.Type
	if strings.TrimSpace(domainType) == "" {
		domainType = "repo"
	}
	if domainType == "repo" && strings.TrimSpace(dm.GitURL) == "" {
		out = append(out, "仓库地址")
	}
	return out
}

// mustMarshal serializes a value to JSON, returning "{}" on error (should not
// happen for these struct types, but a draft must never fail to save).
func mustMarshal(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// platformHasSkills reports whether the skill library is non-empty.
func (d *Daemon) platformHasSkills(ctx context.Context) bool {
	var n int
	if err := d.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM skill`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// platformHasAgents reports whether at least one agent exists — a goal needs
// an assignee, and if none exist the platform is not configured (hard error,
// not an ask).
func (d *Daemon) platformHasAgents(ctx context.Context) bool {
	var n int
	if err := d.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM agent`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// intakeRuntimeList lists the active runtimes for the clarification ask.
func (d *Daemon) intakeRuntimeList(ctx context.Context) string {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT name FROM runtime WHERE status='active' ORDER BY name`)
	if err != nil {
		return "（当前没有可用运行时——先在 Web 配置 runtime）"
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", name)
		n++
	}
	if n == 0 {
		return "（当前没有可用运行时——先在 Web 配置 runtime）"
	}
	return b.String()
}

// intakeSkillList lists the platform skill library for the clarification ask.
func (d *Daemon) intakeSkillList(ctx context.Context) string {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT name FROM skill ORDER BY name`)
	if err != nil {
		return "（查询 skill 失败）"
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", name)
		n++
	}
	if n == 0 {
		return "（当前没有 skill）"
	}
	return b.String()
}

// intakeAgentList lists the agents for the squad-leader / goal-assignee ask.
func (d *Daemon) intakeAgentList(ctx context.Context) string {
	rows, err := d.st.DB().QueryContext(ctx, `SELECT name FROM agent ORDER BY name`)
	if err != nil {
		return "（当前没有可用 agent——先在 Web 配置 agent）"
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", name)
		n++
	}
	if n == 0 {
		return "（当前没有可用 agent——先在 Web 配置 agent）"
	}
	return b.String()
}

// listDescLimit is the max character count for description fields in list
// replies (IM/chat readability — full descriptions are on the Web).
const listDescLimit = 40

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// splitAndTrim splits a comma-separated string into trimmed non-empty parts.
func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// batchDelete runs deleteOne for each name and assembles a per-item result +
// summary. deleteOne returns a string starting with a success emoji (🗑/✅) on
// success, or an error message on failure. A single name skips the summary
// line (caller handles that path); this is only called for 2+ names.
func batchDelete(names []string, deleteOne func(name string) string) string {
	var b strings.Builder
	succeeded, failed := 0, 0
	for _, name := range names {
		r := deleteOne(name)
		if isDeleteSuccess(r) {
			b.WriteString(r + "\n")
			succeeded++
		} else {
			fmt.Fprintf(&b, "❌ %s：%s\n", name, firstLineIn(r))
			failed++
		}
	}
	fmt.Fprintf(&b, "共 %d 个：成功 %d，失败 %d", len(names), succeeded, failed)
	return b.String()
}

// isDeleteSuccess reports whether a deleteOne result string indicates success
// (starts with the deletion/success emoji).
func isDeleteSuccess(r string) bool {
	return strings.HasPrefix(r, "🗑") || strings.HasPrefix(r, "✅")
}

func firstLineIn(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func truncateIn(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
