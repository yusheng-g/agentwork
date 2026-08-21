// Package handler is the HTTP boundary for goal/run/agent/runtime/squad/
// comment/schedule CRUD.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/daemon"
	"github.com/eushing/agentwork/internal/issue"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/service"
)

type Handlers struct {
	Runtime  *service.RuntimeService
	Agent    *service.AgentService
	Goal     *service.GoalService
	Run      *service.RunService
	Comment  *service.CommentService
	Squad    *service.SquadService
	Schedule *service.ScheduleService
	Domain   *service.DomainService
	Settings *service.SettingsService
	IM       *notify.Connector
	// IssueWebhooks are the real-time issue triggers (M4-B), keyed by
	// provider ("github" | "gitcode"); absent = that provider's webhook
	// disabled (polling still covers intake).
	IssueWebhooks map[string]*issue.WebhookHandler
	// Daemon backs the git-side checks the CRUD layer cannot do itself
	// (决策 6-24: the domain git test runs git ls-remote — the daemon owns
	// git exec). nil = the check is skipped (tests construct handlers
	// without it).
	Daemon *daemon.Daemon
	// Machines is the remote-machine registry (the agentwork CLI's link,
	// CLI 分支 Phase 1).
	Machines *service.MachineService
	// Skills is the skills library (CLI 分支 Phase 4).
	Skills *service.SkillService
	// TeamImport is the team-definition-repo import processor-run lifecycle.
	TeamImport *service.TeamImportService
}

func (h *Handlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /runtimes", h.listRuntimes)
	mux.HandleFunc("POST /runtimes", h.createRuntime)
	mux.HandleFunc("GET /runtimes/{id}", h.getRuntime)
	mux.HandleFunc("DELETE /runtimes/{id}", h.deleteRuntime)

	mux.HandleFunc("GET /agents", h.listAgents)
	mux.HandleFunc("POST /agents", h.createAgent)
	mux.HandleFunc("GET /agents/{id}", h.getAgent)
	mux.HandleFunc("PUT /agents/{id}", h.updateAgent)
	mux.HandleFunc("DELETE /agents/{id}", h.deleteAgent)

	mux.HandleFunc("GET /goals", h.listGoals)
	mux.HandleFunc("POST /goals", h.createGoal)
	mux.HandleFunc("GET /goals/{id}", h.getGoal)
	mux.HandleFunc("DELETE /goals/{id}", h.deleteGoal)
	mux.HandleFunc("POST /goals/{id}/assign", h.assignGoal)
	mux.HandleFunc("POST /goals/{id}/cancel", h.cancelGoal)
	mux.HandleFunc("POST /goals/{id}/review", h.resolveGoalReview)
	mux.HandleFunc("POST /goals/{id}/reopen", h.reopenGoal)
	mux.HandleFunc("POST /goals/{id}/activate", h.activateGoal)
	mux.HandleFunc("GET /goals/{id}/runs", h.listRuns)
	mux.HandleFunc("GET /goals/{id}/runs/{runId}/messages", h.listRunMessages)
	mux.HandleFunc("GET /runs/{runId}/messages", h.listRunMessagesByID)
	mux.HandleFunc("GET /goals/{id}/timeline", h.goalTimeline)
	mux.HandleFunc("GET /goals/{id}/sub-goals", h.listSubGoals)
	mux.HandleFunc("POST /goals/{id}/sub-goals", h.createSubGoal)
	mux.HandleFunc("GET /goals/{id}/sub-goals/{subGoalId}/verifications", h.listSubGoalVerifications)
	mux.HandleFunc("GET /goals/{id}/changes", h.listGoalChanges)
	mux.HandleFunc("GET /goals/{id}/comments", h.listComments)
	mux.HandleFunc("POST /goals/{id}/comments", h.createComment)

	mux.HandleFunc("GET /squads", h.listSquads)
	mux.HandleFunc("POST /squads", h.createSquad)
	mux.HandleFunc("GET /squads/{id}", h.getSquad)
	mux.HandleFunc("PUT /squads/{id}", h.updateSquad)
	mux.HandleFunc("DELETE /squads/{id}", h.deleteSquad)
	mux.HandleFunc("POST /squads/{id}/members", h.addSquadMember)
	mux.HandleFunc("GET /squads/{id}/members", h.listSquadMembers)
	mux.HandleFunc("DELETE /squads/{id}/members/{memberId}", h.removeSquadMember)

	mux.HandleFunc("GET /schedules", h.listSchedules)
	mux.HandleFunc("POST /schedules", h.createSchedule)
	mux.HandleFunc("GET /schedules/{id}", h.getSchedule)
	mux.HandleFunc("GET /schedules/{id}/runs", h.listScheduleRuns)
	mux.HandleFunc("DELETE /schedules/{id}", h.deleteSchedule)
	mux.HandleFunc("PUT /schedules/{id}", h.updateSchedule)
	mux.HandleFunc("PUT /schedules/{id}/enabled", h.setScheduleEnabled)

	mux.HandleFunc("GET /domains", h.listDomains)
	mux.HandleFunc("GET /machines", h.listMachines)
	mux.HandleFunc("GET /skills", h.listSkills)
	mux.HandleFunc("POST /skills", h.createSkill)
	mux.HandleFunc("DELETE /skills/{id}", h.deleteSkill)
	mux.HandleFunc("POST /teams/import", h.importTeam)
	mux.HandleFunc("GET /teams/import/{runId}", h.getTeamImport)
	mux.HandleFunc("POST /domains", h.createDomain)
	mux.HandleFunc("GET /domains/{id}", h.getDomain)
	mux.HandleFunc("PUT /domains/{id}", h.updateDomain)
	mux.HandleFunc("DELETE /domains/{id}", h.deleteDomain)
	mux.HandleFunc("POST /domains/{id}/checks", h.freezeDomainChecks)
	mux.HandleFunc("POST /domains/{id}/compile", h.compileDomainPolicy)
	mux.HandleFunc("GET /domains/{id}/compile-run", h.compileRunStatus)

	mux.HandleFunc("GET /gate-decisions/stats", h.gateStats)

	mux.HandleFunc("POST /issue-comments", h.createIssueComment)
	mux.HandleFunc("POST /webhooks/github", h.githubWebhook)
	mux.HandleFunc("POST /webhooks/gitcode", h.gitcodeWebhook)
	mux.HandleFunc("GET /settings/platform", h.getPlatformSettings)
	mux.HandleFunc("PUT /settings/platform", h.putPlatformSettings)
	mux.HandleFunc("GET /im/feishu/status", h.imStatus)
	mux.HandleFunc("POST /im/feishu/connect", h.imConnect)
	mux.HandleFunc("DELETE /im/feishu/connect", h.imDisconnect)
}

// ── runtime ──

func (h *Handlers) createRuntime(w http.ResponseWriter, r *http.Request) {
	var rt service.Runtime
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Runtime.Create(r.Context(), rt)
	writeJSON(w, out, err)
}
func (h *Handlers) listRuntimes(w http.ResponseWriter, r *http.Request) {
	out, err := h.Runtime.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getRuntime(w http.ResponseWriter, r *http.Request) {
	out, err := h.Runtime.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteRuntime(w http.ResponseWriter, r *http.Request) {
	if err := h.Runtime.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── agent ──

func (h *Handlers) createAgent(w http.ResponseWriter, r *http.Request) {
	var a service.Agent
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Agent.Create(r.Context(), a)
	if err == nil && h.Daemon != nil {
		go h.Daemon.PushAgentSkills(context.Background(), out.ID)
	}
	writeJSON(w, out, err)
}
func (h *Handlers) listAgents(w http.ResponseWriter, r *http.Request) {
	out, err := h.Agent.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getAgent(w http.ResponseWriter, r *http.Request) {
	out, err := h.Agent.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) updateAgent(w http.ResponseWriter, r *http.Request) {
	var a service.Agent
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Agent.Update(r.Context(), r.PathValue("id"), a)
	if err == nil && h.Daemon != nil {
		go h.Daemon.PushAgentSkills(context.Background(), out.ID)
	}
	writeJSON(w, out, err)
}
func (h *Handlers) deleteAgent(w http.ResponseWriter, r *http.Request) {
	// Resolve the machine BEFORE the delete — after it, the agent row is
	// gone and the machine's skill dirs would keep the stale union.
	machineID := ""
	if h.Daemon != nil {
		machineID = h.Daemon.AgentMachineID(r.Context(), r.PathValue("id"))
	}
	if err := h.Agent.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if h.Daemon != nil && machineID != "" {
		go h.Daemon.PushMachineSkills(context.Background(), machineID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── goal ──

func (h *Handlers) createGoal(w http.ResponseWriter, r *http.Request) {
	var g service.Goal
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Goal.Create(r.Context(), g)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	// P0-2 (决策 6-15②): an active agent/squad goal's first run is born IN
	// Create's transaction — no caller-side enqueue here anymore.
	writeJSON(w, out, nil)
}

// listGoals returns all goals, or the N most recent when ?limit=N is given.
// limit must be a positive integer if present (0/absent means all).
func (h *Handlers) listGoals(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("limit must be a positive integer, got %q", raw))
			return
		}
		limit = n
	}
	out, err := h.Goal.List(r.Context())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, out, nil)
}
func (h *Handlers) getGoal(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteGoal(w http.ResponseWriter, r *http.Request) {
	if err := h.Goal.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handlers) assignGoal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AssigneeType string `json:"assignee_type"`
		AssigneeID   string `json:"assignee_id"`
		HandoffNote  string `json:"handoff_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// HTTP assign = the human's action (决策 5-6: the HTTP surface carries no
	// agent identity; agent handoffs go through the MCP handoff_goal tool).
	out, err := h.Goal.Assign(r.Context(), r.PathValue("id"), body.AssigneeType, body.AssigneeID, body.HandoffNote, "", "")
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	// P0-2 (决策 6-15②): the new owner's run and the handoff_event back-fill
	// happen inside Assign's transaction — one handoff semantic, no
	// caller-side enqueue (or CompleteHandoff) here anymore.
	writeJSON(w, out, nil)
}
func (h *Handlers) cancelGoal(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.Cancel(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

func (h *Handlers) resolveGoalReview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Goal.ResolveReview(r.Context(), r.PathValue("id"), "", body.Decision, body.Reason)
	writeJSON(w, out, err)
}

// reopenGoal restarts a failed/cancelled goal (the human take-over path).
func (h *Handlers) reopenGoal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	out, err := h.Goal.Reopen(r.Context(), r.PathValue("id"), body.Reason, "")
	writeJSON(w, out, err)
}

// activateGoal moves a backlog goal into execution (决策 6-14: the missing
// backlog → active edge — a goal created without an assignee had no path
// back into the pipeline).
func (h *Handlers) activateGoal(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.Activate(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

func (h *Handlers) listRuns(w http.ResponseWriter, r *http.Request) {
	out, err := h.Run.List(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// goalTimeline returns the goal's execution flow — runs, human/system
// actions, and gate decisions merged in time order (see GoalService.Timeline).
func (h *Handlers) goalTimeline(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.Timeline(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// listRunMessages returns the run's live interaction stream — the Web run
// detail's "what is the agent doing right now" view.
func (h *Handlers) listRunMessages(w http.ResponseWriter, r *http.Request) {
	out, err := h.Run.ListMessages(r.Context(), r.PathValue("runId"))
	writeJSON(w, out, err)
}

// listRunMessagesByID is the goal-less variant for processor runs (the
// acceptance-policy compile, intake) — they have no goal to scope under,
// and the compile progress panel needs their stream.
func (h *Handlers) listRunMessagesByID(w http.ResponseWriter, r *http.Request) {
	out, err := h.Run.ListMessages(r.Context(), r.PathValue("runId"))
	writeJSON(w, out, err)
}

// ── comment ──

func (h *Handlers) createComment(w http.ResponseWriter, r *http.Request) {
	var c service.Comment
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c.GoalID = r.PathValue("id")
	out, err := h.Comment.Create(r.Context(), c)
	writeJSON(w, out, err)
}
func (h *Handlers) listComments(w http.ResponseWriter, r *http.Request) {
	out, err := h.Comment.List(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// ── squad ──

func (h *Handlers) createSquad(w http.ResponseWriter, r *http.Request) {
	var sq service.Squad
	if err := json.NewDecoder(r.Body).Decode(&sq); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Squad.Create(r.Context(), sq)
	writeJSON(w, out, err)
}
func (h *Handlers) listSquads(w http.ResponseWriter, r *http.Request) {
	out, err := h.Squad.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getSquad(w http.ResponseWriter, r *http.Request) {
	out, err := h.Squad.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) updateSquad(w http.ResponseWriter, r *http.Request) {
	var sq service.Squad
	if err := json.NewDecoder(r.Body).Decode(&sq); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Squad.Update(r.Context(), r.PathValue("id"), sq)
	writeJSON(w, out, err)
}
func (h *Handlers) deleteSquad(w http.ResponseWriter, r *http.Request) {
	if err := h.Squad.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handlers) removeSquadMember(w http.ResponseWriter, r *http.Request) {
	if err := h.Squad.RemoveMember(r.Context(), r.PathValue("id"), r.PathValue("memberId")); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handlers) addSquadMember(w http.ResponseWriter, r *http.Request) {
	var m service.SquadMember
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Squad.AddMember(r.Context(), r.PathValue("id"), m.MemberType, m.MemberID, m.Role)
	writeJSON(w, out, err)
}
func (h *Handlers) listSquadMembers(w http.ResponseWriter, r *http.Request) {
	out, err := h.Squad.ListMembers(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// ── schedule ──

func (h *Handlers) createSchedule(w http.ResponseWriter, r *http.Request) {
	var sch service.Schedule
	if err := json.NewDecoder(r.Body).Decode(&sch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Schedule.Create(r.Context(), sch)
	writeJSON(w, out, err)
}
func (h *Handlers) updateSchedule(w http.ResponseWriter, r *http.Request) {
	var sch service.Schedule
	if err := json.NewDecoder(r.Body).Decode(&sch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Schedule.Update(r.Context(), r.PathValue("id"), sch)
	writeJSON(w, out, err)
}
func (h *Handlers) listSchedules(w http.ResponseWriter, r *http.Request) {
	out, err := h.Schedule.List(r.Context())
	writeJSON(w, out, err)
}
func (h *Handlers) getSchedule(w http.ResponseWriter, r *http.Request) {
	out, err := h.Schedule.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// listScheduleRuns returns a schedule's firing history — each firing's
// planned time, the goal it produced and that goal's current status (the
// web schedule detail's history view).
func (h *Handlers) listScheduleRuns(w http.ResponseWriter, r *http.Request) {
	out, err := h.Schedule.ListRuns(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := h.Schedule.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setScheduleEnabled toggles a schedule without deleting it (the IM intake
// "停掉定时任务" flow and the Web toggle share this).
func (h *Handlers) setScheduleEnabled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, nil, service.NewValidationError("invalid body: "+err.Error()))
		return
	}
	out, err := h.Schedule.SetEnabled(r.Context(), r.PathValue("id"), body.Enabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, out, nil)
}

// ── domain ──

func (h *Handlers) createDomain(w http.ResponseWriter, r *http.Request) {
	var d service.Domain
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 决策 6-24 延伸：repo 域必须通过 git 连接测试才能创建——仓库/分支/token
	// 在配置期验证，而不是留给第一次 run 的 clone/fetch 失败。scratch 无
	// git_url 跳过；URL 留空交给 Create 自己的校验（报错更准确）。
	if d.Type != "scratch" {
		if _, ok := h.testGitAndRespond(w, r, d.GitURL, d.GitCredentials, &d.DefaultBranch); !ok {
			return
		}
	}
	out, err := h.Domain.Create(r.Context(), d)
	writeJSON(w, out, err)
}
func (h *Handlers) listDomains(w http.ResponseWriter, r *http.Request) {
	out, err := h.Domain.List(r.Context())
	writeJSON(w, out, err)
}
// listMachines returns the registered remote machines (CLI 分支 Phase 1):
// connection status + the agent CLIs each machine probed.
func (h *Handlers) listMachines(w http.ResponseWriter, r *http.Request) {
	if h.Machines == nil {
		writeJSON(w, []service.Machine{}, nil)
		return
	}
	out, err := h.Machines.List(r.Context())
	writeJSON(w, out, err)
}

// ── skills (CLI 分支 Phase 4) ──

func (h *Handlers) listSkills(w http.ResponseWriter, r *http.Request) {
	if h.Skills == nil {
		writeJSON(w, []service.Skill{}, nil)
		return
	}
	out, err := h.Skills.List(r.Context())
	writeJSON(w, out, err)
}

func (h *Handlers) createSkill(w http.ResponseWriter, r *http.Request) {
	// Two upload modes: a multipart form carrying the skill as a ZIP
	// archive (scripts + binary assets — the way the web UI uploads), or
	// the legacy JSON body of text files (=== blocks) which the service
	// archives the same way. The skill's name/description are PARSED from
	// the archive's SKILL.md by the service — the caller does not supply
	// them. A zip without a valid SKILL.md is rejected as "not a skill".
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("file is required: %w", err))
			return
		}
		defer file.Close()
		zipData, err := io.ReadAll(file)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := h.Skills.CreateFromZip(r.Context(), zipData)
		writeJSON(w, out, err)
		return
	}
	var body struct {
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Skills.Create(r.Context(), body.Files)
	writeJSON(w, out, err)
}

func (h *Handlers) deleteSkill(w http.ResponseWriter, r *http.Request) {
	skillID := r.PathValue("id")
	// Collect the users BEFORE the delete; after it, each affected agent's
	// machine gets a config.push whose expected set no longer contains the
	// skill — the machine removes the directory.
	agents := []string{}
	if h.Daemon != nil {
		agents = h.Daemon.SkillAgents(r.Context(), skillID)
	}
	if err := h.Skills.Delete(r.Context(), skillID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if h.Daemon != nil {
		for _, id := range agents {
			go h.Daemon.PushAgentSkills(context.Background(), id)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── team import (processor-run-driven) ──

func (h *Handlers) importTeam(w http.ResponseWriter, r *http.Request) {
	var req service.ImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if h.TeamImport == nil {
		writeErr(w, http.StatusInternalServerError, errors.New("team import service not configured"))
		return
	}
	if _, ok := h.testGitAndRespond(w, r, req.GitURL, req.GitCredentials, &req.DefaultBranch); !ok {
		return
	}
	ti, run, err := h.TeamImport.ImportTeam(r.Context(), req)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"team_import": ti, "run": run}, nil)
}

func (h *Handlers) getTeamImport(w http.ResponseWriter, r *http.Request) {
	if h.TeamImport == nil {
		writeErr(w, http.StatusInternalServerError, errors.New("team import service not configured"))
		return
	}
	out, err := h.TeamImport.GetByRun(r.Context(), r.PathValue("runId"))
	writeJSON(w, out, err)
}

func (h *Handlers) getDomain(w http.ResponseWriter, r *http.Request) {
	out, err := h.Domain.Get(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}
func (h *Handlers) deleteDomain(w http.ResponseWriter, r *http.Request) {
	if err := h.Domain.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateDomain edits a domain's mutable configuration (issue handler etc.).
func (h *Handlers) updateDomain(w http.ResponseWriter, r *http.Request) {
	var d service.Domain
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 决策 6-24 延伸：创建门槛不能被编辑绕过——git 配置（URL/分支/token）
	// 有变化时用同一个探针验证新值；没动 git 配置则跳过（仓库已失效时
	// 仍允许改 issue 处理方这类无关字段）。
	if h.Daemon != nil {
		if old, err := h.Domain.Get(r.Context(), r.PathValue("id")); err == nil {
			gitChanged := old.GitURL != d.GitURL ||
				old.DefaultBranch != d.DefaultBranch ||
				old.GitCredentials != d.GitCredentials
			if gitChanged {
				if _, ok := h.testGitAndRespond(w, r, d.GitURL, d.GitCredentials, &d.DefaultBranch); !ok {
					return
				}
			}
		}
	}
	out, err := h.Domain.Update(r.Context(), r.PathValue("id"), d)
	writeJSON(w, out, err)
}

// compileDomainPolicy starts acceptance-policy compilation for a domain
// (DESIGN.md §5.3): the processor agent compiles the NL intent into
// checks, which stay UNFROZEN until the owner confirms via FreezeChecks.
func (h *Handlers) compileDomainPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PolicyText       string `json:"policy_text"`
		ProcessorAgentID string `json:"processor_agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Domain.CompilePolicy(r.Context(), r.PathValue("id"), body.PolicyText, body.ProcessorAgentID)
	writeJSON(w, out, err)
}

// compileRunStatus returns the domain's latest compile processor run (决策
// 6-23) — the compile panel restores its in-flight banner from this after a
// page refresh. null when the domain has never compiled.
func (h *Handlers) compileRunStatus(w http.ResponseWriter, r *http.Request) {
	out, err := h.Run.LatestCompileRun(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// freezeDomainChecks stores the compiled acceptance policy after the owner
// confirms the processor agent's output (DESIGN.md §5.3). The confirmation
// card is the guard that keeps the "define" role with the human.
func (h *Handlers) freezeDomainChecks(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Checks               service.Checks `json:"checks"`
		VerificationStrength string         `json:"verification_strength"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.Domain.FreezeChecks(r.Context(), r.PathValue("id"), body.Checks, body.VerificationStrength)
	writeJSON(w, out, err)
}

// ── gate health (M2) ──

func (h *Handlers) gateStats(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.GateStats(r.Context())
	writeJSON(w, out, err)
}

// ── IM (Feishu connect flow — the Web-driven QR connect) ──

// ── issue comments (M4-B) ──

// createIssueComment posts a comment on the issue behind a goal: the goal's
// source_ref names the repo+number, the domain's git_credentials is the
// GitHub token — the agent never touches either (the platform executes the
// structured side effect).
func (h *Handlers) createIssueComment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GoalID string `json:"goal_id"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, nil, service.NewValidationError("invalid body: "+err.Error()))
		return
	}
	if body.GoalID == "" || strings.TrimSpace(body.Text) == "" {
		writeJSON(w, nil, service.NewValidationError("goal_id and text are required"))
		return
	}
	ref, token, isIssue, err := h.Goal.IssueSource(r.Context(), body.GoalID)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if !isIssue {
		writeJSON(w, nil, service.NewValidationError("goal has no issue source"))
		return
	}
	provider, repo, number, ok := issue.ParseSourceRef(ref)
	if !ok {
		writeJSON(w, nil, service.NewValidationError("goal source is not an issue ref"))
		return
	}
	if token == "" {
		writeJSON(w, nil, service.NewValidationError("domain has no git_credentials (the platform token)"))
		return
	}
	client, err := issue.NewProvider(provider, token)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if err := client.CreateComment(r.Context(), repo, number, body.Text); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// githubWebhook / gitcodeWebhook are the real-time issue triggers (M4-B):
// the hosting platform pushes `issues` events; the handler verifies the
// provider-specific signature and creates the goal immediately (the poller
// stays as the safety net). Comment events are ignored — the run-start
// comment fetch covers the dialogue.
func (h *Handlers) githubWebhook(w http.ResponseWriter, r *http.Request) {
	h.handleWebhook(w, r, "github", "X-Hub-Signature-256")
}

func (h *Handlers) gitcodeWebhook(w http.ResponseWriter, r *http.Request) {
	h.handleWebhook(w, r, "gitcode", "X-GitCode-Signature-256")
}

func (h *Handlers) handleWebhook(w http.ResponseWriter, r *http.Request, provider, sigHeader string) {
	wh := h.IssueWebhooks[provider]
	if wh == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("webhook %s not configured", provider))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := wh.Handle(r.Context(), body, r.Header.Get(sigHeader), r.Header.Get("X-GitCode-Token")); err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── platform settings (M3: intake agent + digest time) ──

// platformSettingsKey is the JSON blob under which the platform-wide M3
// settings live (the global inbound parser agent, the daily digest time).
const platformSettingsKey = "platform.m3"

// webhookSecretKey is the standalone key for the platform webhook secret —
// shared across providers (github/gitcode endpoints verify with the same
// secret on a single-user platform). Kept apart from the m3 blob: it is a
// credential, not a toggle.
const webhookSecretKey = "platform.webhook_secret"

type platformSettings struct {
	IntakeAgent string `json:"intake_agent"` // agent id: IM inbound parser ('' = unset)
	DigestTime  string `json:"digest_time"`  // HH:MM local, '' = default 09:00
	// WebhookSecret verifies GitHub's X-Hub-Signature-256 ('' = webhook
	// disabled; polling still covers issue intake).
	WebhookSecret string `json:"webhook_secret"`
}

func (h *Handlers) getPlatformSettings(w http.ResponseWriter, r *http.Request) {
	var out platformSettings
	if raw, err := h.Settings.Get(r.Context(), platformSettingsKey); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	if v, _ := h.Settings.Get(r.Context(), webhookSecretKey); v != "" {
		out.WebhookSecret = v
	}
	writeJSON(w, out, nil)
}

func (h *Handlers) putPlatformSettings(w http.ResponseWriter, r *http.Request) {
	var body platformSettings
	// Merge-write: preload the existing blob, then overlay the request body.
	// Go's json.Decode leaves absent fields untouched and only overwrites on
	// an explicit "". This stops a partial PUT (e.g. just digest_time) from
	// wiping a configured intake_agent back to "".
	if raw, err := h.Settings.Get(r.Context(), platformSettingsKey); err == nil && raw != "" {
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			logging.Warnf("settings: platform settings blob corrupt, merge-write starts fresh: %v", err)
		}
	}
	if v, _ := h.Settings.Get(r.Context(), webhookSecretKey); v != "" {
		body.WebhookSecret = v
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, nil, service.NewValidationError("invalid body: "+err.Error()))
		return
	}
	if body.IntakeAgent != "" {
		if _, err := h.Agent.Get(r.Context(), body.IntakeAgent); err != nil {
			writeJSON(w, nil, service.NewValidationError("intake agent does not exist"))
			return
		}
	}
	if body.DigestTime != "" {
		if _, err := time.Parse("15:04", body.DigestTime); err != nil {
			writeJSON(w, nil, service.NewValidationError("digest_time must be HH:MM"))
			return
		}
	}
	// The webhook secret lives on its own key (a credential), the rest in
	// the m3 blob.
	if err := h.Settings.Set(r.Context(), webhookSecretKey, body.WebhookSecret); err != nil {
		writeJSON(w, nil, err)
		return
	}
	body.WebhookSecret = ""
	raw, _ := json.Marshal(body)
	if err := h.Settings.Set(r.Context(), platformSettingsKey, string(raw)); err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, body, nil)
}

func (h *Handlers) imStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.IM.Status(), nil)
}

func (h *Handlers) imConnect(w http.ResponseWriter, r *http.Request) {
	// The registration runs for up to 10 minutes — it MUST outlive this HTTP
	// request, so it gets its own context, not r.Context() (which is cancelled
	// when the response returns).
	_, qr, err := h.IM.StartRegistration(context.Background())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"qr": qr, "status": h.IM.Status()["status"]}, nil)
}

func (h *Handlers) imDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := h.IM.Disconnect(r.Context()); err != nil {
		writeJSON(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ──

// testGitAndRespond runs the config-time git probe (决策 6-24) and writes an
// error response on failure. Returns the resolved branch and true on success,
// "" and false on failure (the error response is already written). When the
// caller's branch field is empty, it is back-filled with the remote's HEAD.
func (h *Handlers) testGitAndRespond(w http.ResponseWriter, r *http.Request, gitURL, gitCredentials string, branch *string) (string, bool) {
	if gitURL == "" || h.Daemon == nil {
		return "", true
	}
	res := h.Daemon.TestDomainGit(r.Context(), gitURL, *branch, gitCredentials)
	if !res.OK {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("git connection test failed: %s", res.Error))
		return "", false
	}
	if !res.BranchExists {
		if len(res.Refs) == 0 {
			writeErr(w, http.StatusBadRequest, errors.New("repository is empty (no branches) — cannot be used as a project repo"))
		} else {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("branch %q does not exist on the repository (remote branches: %s)", res.ResolvedBranch, strings.Join(res.Refs, ", ")))
		}
		return "", false
	}
	if *branch == "" {
		*branch = res.ResolvedBranch
	}
	return res.ResolvedBranch, true
}

// writeJSON writes v as JSON, or an error response if err is non-nil. err is
// mapped: ErrNotFound→404, ErrValidation→400, else 500.
func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, service.ErrNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, service.ErrValidation) {
			code = http.StatusBadRequest
		}
		writeErr(w, code, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (h *Handlers) listSubGoals(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.ListSubGoals(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// listGoalChanges returns the goal's changes with their revision history —
// the Web change panel (the owner's integration view, v2 决策 6-3).
func (h *Handlers) listGoalChanges(w http.ResponseWriter, r *http.Request) {
	out, err := h.Goal.ListChangeDetails(r.Context(), r.PathValue("id"))
	writeJSON(w, out, err)
}

// listSubGoalVerifications returns a sub-goal's verification rounds — the
// Web sub-goal panel's audit trail (v2 决策 6-5).
func (h *Handlers) listSubGoalVerifications(w http.ResponseWriter, r *http.Request) {
	goalID := r.PathValue("id")
	subGoalID := r.PathValue("subGoalId")
	sg, err := h.Goal.GetSubGoal(r.Context(), subGoalID)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if sg.GoalID != goalID {
		writeJSON(w, nil, service.ErrNotFound)
		return
	}
	out, err := h.Goal.ListVerificationResults(r.Context(), subGoalID)
	writeJSON(w, out, err)
}

func (h *Handlers) createSubGoal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		AssigneeID  string `json:"assignee_id"`
		VerifierID  string `json:"verifier_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// HTTP = the human's surface (决策 6-1: human can split work).
	out, err := h.Goal.CreateSubGoal(r.Context(), r.PathValue("id"), body.Title, body.Description, body.AssigneeID, body.VerifierID, "human", "")
	writeJSON(w, out, err)
}
