// Package mcp exposes the run's workspace as an MCP server over streamable
// HTTP, built on the official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// This is the ACP-v2-endorsed way for a client (agentwork) to hand its
// workspace to an agent that does not delegate its tools to client
// fs/terminal RPCs (opencode's tools are local by design) — MCP servers are
// a standard fixture of every mainstream agent, so the workspace tools
// appear in the agent's tool registry automatically. The server URL is
// advertised at ACP session/new (McpServers).
//
// The server is STATELESS (StreamableHTTPOptions.Stateless): each request
// maps to an Executor bound to one run's worktree + environment. See
// DESIGN.md 决策 4-8.
//
// Command execution is ASYNC and terminal-shaped (the same model as the ACP
// terminal RPCs): terminal_create starts a command and returns its id
// immediately, terminal_output polls incremental output + exit status,
// terminal_release kills and forgets. A synchronous run_command was retired:
// it hung the HTTP request for the command's whole lifetime, had no
// command-level timeout, and duplicated the terminal engine with a second
// implementation. The terminal tools share the daemon's per-run
// terminalManager — one engine, two channels, the run's cleanup kills both.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/service"
)

// TerminalHost is the command-execution surface the MCP terminal tools use.
// The daemon's terminalManager implements it (the same per-run pool the ACP
// terminal RPCs use — one engine, two channels; the run's cleanup kills
// both). Defined here so mcp stays importable by the daemon (no cycle).
type TerminalHost interface {
	Create(command string, args []string, env []string, cwd string, byteLimit int) (acp.TerminalId, error)
	Output(id acp.TerminalId, cursor *int64) (*acp.TerminalOutputResponse, *int64, int64, error)
	Release(id acp.TerminalId) error
}

// Executor binds the workspace tools to ONE run: the worktree as the
// filesystem basis and the run environment for commands. The daemon builds
// one per run and registers it under the run id.
type Executor struct {
	// Worktree is the run's worktree root — the execution directory.
	Worktree string
	// Env is the command environment (platform base + run context, PATH
	// prepended with the agentwork-cli dir).
	Env []string
	// Run context: collaboration tools act on THIS goal as THIS agent.
	GoalID  string
	AgentID string
	RunID   string
	// Services for the collaboration tools (comment / assign / list). The
	// daemon injects them — the tools act in-process, no CLI, no HTTP hop.
	// The CLI was the old collaboration channel; an agent had to learn its
	// command syntax (and sometimes read the platform's source to figure it
	// out). MCP tools carry their own schema — zero learning cost.
	commentSvc *service.CommentService
	goalSvc    *service.GoalService
	runSvc     *service.RunService
	agentSvc   *service.AgentService
	squadSvc   *service.SquadService
	// host executes terminal commands — the daemon's per-run terminalManager.
	host TerminalHost
}

// NewExecutor binds an Executor to one run's workspace + terminal pool.
func NewExecutor(workdir string, env []string, host TerminalHost) *Executor {
	return &Executor{Worktree: workdir, Env: env, host: host}
}

// SetCollaboration wires the services the collaboration tools need.
func (e *Executor) SetCollaboration(goalID, agentID, runID string, commentSvc *service.CommentService, goalSvc *service.GoalService, runSvc *service.RunService, agentSvc *service.AgentService, squadSvc *service.SquadService) {
	e.GoalID, e.AgentID, e.RunID = goalID, agentID, runID
	e.commentSvc, e.goalSvc, e.runSvc, e.agentSvc, e.squadSvc = commentSvc, goalSvc, runSvc, agentSvc, squadSvc
}

// requireOwner enforces the handoff/consult/sub-goal/wait permission on THIS
// run's goal (决策 5-6): the calling run's agent must be the goal's current
// owner. Guest runs (consults/reviews) and non-owner agents are rejected.
func (e *Executor) requireOwner(ctx context.Context) error {
	return e.requireOwnerOf(ctx, e.GoalID)
}

// requireOwnerOf is requireOwner for an explicit goal id (create_sub_goal may
// split from a named parent).
func (e *Executor) requireOwnerOf(ctx context.Context, goalID string) error {
	if e.goalSvc == nil {
		return errors.New("collaboration not wired for this run")
	}
	g, err := e.goalSvc.Get(ctx, goalID)
	if err != nil {
		return err
	}
	owns, err := e.goalSvc.AgentOwnsGoal(ctx, g, e.AgentID)
	if err != nil {
		return err
	}
	if !owns {
		return errors.New("permission denied: only the goal's current owner can do this (guest/consult runs cannot)")
	}
	return nil
}

// NewServer builds the MCP server with the workspace tools registered.
func NewServer(exec *Executor) *gmcp.Server {
	srv := gmcp.NewServer(&gmcp.Implementation{Name: "agentwork", Version: "0.1"}, nil)

	addFileTools(srv, exec)

	// defaultCreateWait is how long terminal_create waits (synchronously) for
	// a short command before handing the terminal id back: most tool calls are
	// short (ls, git status, one test), and a synchronous result saves the
	// agent the create→poll→release dance. Commands past the budget switch to
	// the async path automatically. The agent controls the command's real
	// lifetime — release (kill) an overlong command; the platform only bounds
	// the turn (run maxRunDuration / idle watchdog) and the concurrent count.
	const defaultCreateWait = 10 * time.Second

	// The shell terminal_create runs commands through — resolved from the
	// MACHINE's own OS, stated verbatim in the tool description (the agent
	// never guesses which OS this is).
	sh, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		sh, flag = "cmd.exe", "/C"
	}

	type createArgs struct {
		Command string `json:"command" jsonschema:"the SHELL command line — pipes, redirects, && and quoting all work; no separate args field"`
		Cwd     *string `json:"cwd,omitempty" jsonschema:"working directory override (defaults to the workspace root)"`
		// Timeout is `any` (not `*int64`) so the JSON schema has no type
		// constraint — some models serialize numbers as strings ("15" instead
		// of 15), which the SDK's schema validation rejects for `*int64`
		// ("type: 15 has type string, want one of null, integer"). parseBudget
		// coerces both forms; the description still tells the model to send an
		// integer.
		Timeout any `json:"timeout,omitempty" jsonschema:"sync-wait budget in seconds (default 10): if the command finishes within it the result is returned directly; otherwise a terminal_id comes back and you poll terminal_output. 0 = return the id immediately (pure async)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name: "terminal_create",
		Description: "Start a SHELL command on the platform machine with the workspace as the working directory " +
			"(runs via " + sh + " " + flag + " on this machine — pipes, redirects and && work). " +
			"Waits up to the timeout budget (default 10s) for a quick result; commands that finish in time return their " +
			"output and exit status directly (exited=true). Longer commands return a terminal_id with exited=false — " +
			"poll terminal_output (pass the returned cursor back) until exited=true, then terminal_release. " +
			"Commands have no platform time limit of their own: release an overlong one.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args createArgs) (*gmcp.CallToolResult, any, error) {
		if args.Command == "" {
			return nil, nil, errEmptyCommand
		}
		// The command is a SHELL command line — the platform picks the shell
		// from the machine's OS (no separate argv: agents repeatedly
		// mis-split command/args, e.g. 'find find .', and shell syntax needs
		// the shell anyway).
		argv := []string{flag, args.Command}
		cwd := exec.Worktree
		if args.Cwd != nil && *args.Cwd != "" {
			cwd = *args.Cwd
		}
		budget := defaultCreateWait
		if args.Timeout != nil {
			budget = parseBudget(args.Timeout, defaultCreateWait)
		}
		id, err := exec.host.Create(sh, argv, exec.Env, cwd, 0)
		if err != nil {
			return nil, nil, err
		}
		var zeroCursor int64
		cursor := &zeroCursor
		var out strings.Builder
		var elapsed int64
		deadline := time.Now().Add(budget)
		for budget > 0 {
			resp, next, el, err := exec.host.Output(id, cursor)
			if err != nil {
				return nil, nil, err
			}
			cursor = next
			elapsed = el
			out.WriteString(resp.Output)
			if resp.ExitStatus != nil {
				return toolResult(map[string]any{
					"terminal_id": string(id), "output": out.String(), "cursor": *next,
					"exited": true, "exit_code": derefInt(resp.ExitStatus.ExitCode),
					"signal": derefStr(resp.ExitStatus.Signal), "elapsed": elapsed,
				}), nil, nil
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Budget exhausted — the command is still running: hand back the id +
		// everything seen so far; the agent continues via terminal_output.
		return toolResult(map[string]any{
			"terminal_id": string(id), "output": out.String(), "cursor": *cursor,
			"exited": false, "elapsed": elapsed,
		}), nil, nil
	})

	type outputArgs struct {
		TerminalID string `json:"terminal_id" jsonschema:"the terminal id from terminal_create"`
		// Cursor is `any` (not `*int64`) so the JSON schema has no type
		// constraint — some models serialize numbers as strings. parseInt64Ptr
		// coerces both forms; nil = first poll (no cursor).
		Cursor any `json:"cursor,omitempty" jsonschema:"opaque cursor from the previous terminal_output call; omit on the first poll"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name: "terminal_output",
		Description: "Poll a command's output: returns the bytes since the given cursor (omit on the first call), " +
			"the next cursor to pass back, and the exit status once finished. Repeat until exited=true, then terminal_release. " +
			"The cursor makes retries safe — re-polling with an old cursor never skips or duplicates bytes.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args outputArgs) (*gmcp.CallToolResult, any, error) {
		resp, next, elapsed, err := exec.host.Output(acp.TerminalId(args.TerminalID), parseInt64Ptr(args.Cursor))
		if err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{
			"output": resp.Output, "cursor": *next, "truncated": resp.Truncated,
			"exited":    resp.ExitStatus != nil,
			"exit_code": derefInt(exitCode(resp)), "signal": derefStr(exitSignal(resp)),
			"elapsed": elapsed,
		}), nil, nil
	})

	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "terminal_release",
		Description: "Kill the command (if still running) and forget it. Always call after terminal_output reports exited.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args outputArgs) (*gmcp.CallToolResult, any, error) {
		if err := exec.host.Release(acp.TerminalId(args.TerminalID)); err != nil {
			return nil, nil, err
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: "released " + args.TerminalID}}}, nil, nil
	})

	// ── Collaboration tools (决策 5-2, the four-behavior model): Comment（说）
	// / Consult（问）/ Handoff（接力）/ Sub-goal（拆活）. Structured args,
	// schema-described, permission-checked — the calling run's agent (exec)
	// is the identity anchor (决策 5-6: only the goal's owner can consult /
	// hand off / split / wait).
	type commentArgs struct {
		Content string `json:"content" jsonschema:"the comment text — a plain coordination message. Mentions in it do NOT trigger runs; use consult_agent to ask another agent."`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "comment_goal",
		Description: "Comment on THIS goal as your agent — say something (progress, findings, notes). Never triggers another agent's run, never changes ownership.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args commentArgs) (*gmcp.CallToolResult, any, error) {
		if exec.commentSvc == nil || exec.GoalID == "" {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		// Pure Comment (决策 5-2): the description promises mentions never
		// trigger runs — persist without mention dispatch so the contract and
		// the behavior are the same thing.
		if _, err := exec.commentSvc.CreateNoDispatch(ctx, service.Comment{
			GoalID: exec.GoalID, AuthorType: "agent", AuthorID: exec.AgentID, Content: args.Content, RunID: exec.RunID,
		}); err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{"ok": true}), nil, nil
	})

	type getCommentsArgs struct {
		GoalID string `json:"goal_id,omitempty" jsonschema:"the goal to read comments from — defaults to THIS goal"`
		After  string `json:"after,omitempty" jsonschema:"a comment id cursor — only comments NEWER than it ('' = from the start)"`
		// Limit is `any` (not `*int`) so the JSON schema has no type
		// constraint — some models serialize numbers as strings ("100").
		Limit any `json:"limit,omitempty" jsonschema:"max comments (default 50, hard max 100)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "get_comments",
		Description: "Read the goal's comment feed (READ-ONLY): pull NEW comments during a long run — your prompt snapshot is fixed at claim, this is the live view. Pass the last comment id you saw as `after` for incremental reads; `limit` caps the batch.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args getCommentsArgs) (*gmcp.CallToolResult, any, error) {
		if exec.commentSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		goalID := args.GoalID
		if goalID == "" {
			if exec.GoalID == "" {
				return nil, nil, errors.New("no goal context — pass goal_id")
			}
			goalID = exec.GoalID
		}
		limit := 50
		if p := parseInt64Ptr(args.Limit); p != nil && *p > 0 {
			limit = int(*p)
		}
		list, err := exec.commentSvc.ListAfter(ctx, goalID, args.After, limit)
		if err != nil {
			return nil, nil, err
		}
		out := []map[string]any{}
		for _, c := range list {
			out = append(out, map[string]any{
				"id": c.ID, "author": c.AuthorType + "/" + c.AuthorID,
				"content": c.Content, "parent_id": c.ParentID, "created_at": c.CreatedAt,
			})
		}
		return toolResult(map[string]any{"comments": out}), nil, nil
	})

	type consultArgs struct {
		AgentID  string `json:"agent_id" jsonschema:"the agent to consult (resolve with agent_list)"`
		Question string `json:"question" jsonschema:"what you need to know / want judged"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "consult_agent",
		Description: "Consult another agent — ask for information/judgment. The platform posts a mention comment for you and enqueues a READ-ONLY guest run on that agent; their answer lands in the comment feed, and the platform automatically resumes YOUR next run after they answer. Only the goal's owner can consult.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args consultArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil || exec.commentSvc == nil || exec.GoalID == "" {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		if err := exec.requireOwner(ctx); err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(args.AgentID) == "" || strings.TrimSpace(args.Question) == "" {
			return nil, nil, errors.New("consult_agent: agent_id and question are required")
		}
		label := args.AgentID
		if exec.agentSvc != nil {
			if a, err := exec.agentSvc.Get(ctx, args.AgentID); err == nil && a.Name != "" {
				label = a.Name
			}
		}
		content := "[@" + label + "](mention://agent/" + args.AgentID + ") " + args.Question
		c, err := exec.commentSvc.Create(ctx, service.Comment{
			GoalID: exec.GoalID, AuthorType: "agent", AuthorID: exec.AgentID, Content: content, RunID: exec.RunID,
		})
		if err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{"ok": true, "comment_id": c.ID, "note": "guest run enqueued; your next run resumes automatically after the answer"}), nil, nil
	})

	type handoffArgs struct {
		AssigneeType string `json:"assignee_type" jsonschema:"agent | squad | human"`
		AssigneeID   string `json:"assignee_id" jsonschema:"the new owner's id ('' with human)"`
		Reason       string `json:"reason,omitempty" jsonschema:"why you're handing off — becomes the new owner's scope"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "handoff_goal",
		Description: "Hand THIS goal's ownership to another agent/squad (or back to the human). Only the current owner can. END YOUR TURN immediately after: the platform terminates your run and enqueues the new owner's run; the goal's branch state is preserved.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args handoffArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil || exec.runSvc == nil || exec.GoalID == "" {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		if err := exec.requireOwner(ctx); err != nil {
			return nil, nil, err
		}
		// P0-2 (决策 6-15②): Assign owns the successor — the new owner's run
		// is born in its transaction (queued as a durable intent when the goal
		// is frozen in review, 决策 2-3 revised). No caller-side enqueue.
		if _, err := exec.goalSvc.Assign(ctx, exec.GoalID, args.AssigneeType, args.AssigneeID, args.Reason, "agent", exec.AgentID); err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{"ok": true}), nil, nil
	})

	type subGoalArgs struct {
		ParentGoalID string `json:"parent_goal_id,omitempty" jsonschema:"the goal to split work off — defaults to THIS goal"`
		Title        string `json:"title" jsonschema:"the sub-goal's title"`
		Description  string `json:"description,omitempty" jsonschema:"what the sub-goal is about"`
		AssigneeType string `json:"assignee_type" jsonschema:"agent — the sub-goal's assignee (executes the work item)"`
		AssigneeID   string `json:"assignee_id" jsonschema:"the assignee agent's id"`
		VerifierID   string `json:"verifier_id,omitempty" jsonschema:"optional agent verifier id ('' = machine verification)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "create_sub_goal",
		Description: "Split a work item off a goal — a sub-goal with its own assignee, run, worktree and machine verification; a verified sub-goal produces a Change the goal owner integrates. The goal's owner never changes; the platform wakes the owner when changes are ready. Only the goal's owner can split.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args subGoalArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil || exec.GoalID == "" {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		parentID := args.ParentGoalID
		if parentID == "" {
			parentID = exec.GoalID
		}
		if err := exec.requireOwnerOf(ctx, parentID); err != nil {
			return nil, nil, err
		}
		if args.AssigneeType != "" && args.AssigneeType != "agent" {
			return nil, nil, errors.New("sub-goal assignee must be an agent")
		}
		sg, err := exec.goalSvc.CreateSubGoal(ctx, parentID, args.Title, args.Description, args.AssigneeID, args.VerifierID, "agent", exec.AgentID)
		if err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{"ok": true, "sub_goal_id": sg.ID, "note": "the platform wakes you when the change is ready for integration"}), nil, nil
	})

	type cancelSubGoalArgs struct {
		SubGoalID string `json:"sub_goal_id" jsonschema:"the sub-goal to cancel"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "cancel_sub_goal",
		Description: "Cancel one of THIS goal's sub-goals (owner management): the work item stops, its queued run is dropped and a running one is terminated. History (verification rounds, changes) is kept. Only the goal's owner can cancel.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args cancelSubGoalArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil || exec.GoalID == "" {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		sg, err := exec.goalSvc.GetSubGoal(ctx, args.SubGoalID)
		if err != nil {
			return nil, nil, err
		}
		if sg.GoalID != exec.GoalID {
			return nil, nil, errors.New("the sub-goal does not belong to this goal")
		}
		if err := exec.requireOwner(ctx); err != nil {
			return nil, nil, err
		}
		if _, err := exec.goalSvc.CancelSubGoal(ctx, args.SubGoalID); err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{"ok": true, "sub_goal_id": args.SubGoalID}), nil, nil
	})

	type getSubGoalArgs struct {
		SubGoalID string `json:"sub_goal_id,omitempty" jsonschema:"a sub-goal id; omit to list THIS goal's sub-goals"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "get_sub_goal",
		Description: "Inspect sub-goals: one by id, or THIS goal's sub-goals (status, assignee, verification state — the resume context's detail view).",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args getSubGoalArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		if args.SubGoalID != "" {
			sg, err := exec.goalSvc.GetSubGoal(ctx, args.SubGoalID)
			if err != nil {
				return nil, nil, err
			}
			return toolResult(map[string]any{"sub_goal": map[string]any{"id": sg.ID, "title": sg.Title, "description": sg.Description, "assignee_id": sg.AssigneeID, "verifier_id": sg.VerifierID, "status": sg.Status, "execution_attempt": sg.ExecutionAttempt, "quality_iteration": sg.QualityIteration}}), nil, nil
		}
		if exec.GoalID == "" {
			return nil, nil, errors.New("no goal context — pass a sub_goal_id")
		}
		list, err := exec.goalSvc.ListSubGoals(ctx, exec.GoalID)
		if err != nil {
			return nil, nil, err
		}
		out := []map[string]any{}
		for _, sg := range list {
			out = append(out, map[string]any{"id": sg.ID, "title": sg.Title, "status": sg.Status, "assignee_id": sg.AssigneeID, "quality_iteration": sg.QualityIteration})
		}
		return toolResult(map[string]any{"sub_goals": out}), nil, nil
	})

	type getVerificationArgs struct {
		SubGoalID string `json:"sub_goal_id" jsonschema:"the sub-goal whose verification rounds to inspect"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "get_verification",
		Description: "Inspect a sub-goal's verification rounds (verdict, summary, evidence) — the audit trail behind verified/rejected.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args getVerificationArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		list, err := exec.goalSvc.ListVerificationResults(ctx, args.SubGoalID)
		if err != nil {
			return nil, nil, err
		}
		out := []map[string]any{}
		for _, v := range list {
			out = append(out, map[string]any{"status": v.Status, "summary": v.Summary, "evidence": v.Evidence, "created_at": v.CreatedAt})
		}
		return toolResult(map[string]any{"verifications": out}), nil, nil
	})

	type verifyArgs struct {
		Verdict  string `json:"verdict" jsonschema:"passed | rejected"`
		Summary  string `json:"summary" jsonschema:"what was verified, or the CONCRETE problems on rejection (the assignee fixes from this)"`
		Evidence string `json:"evidence,omitempty" jsonschema:"key evidence: test output excerpts, file references"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "verify_sub_goal",
		Description: "Issue your verifier verdict on THIS sub-goal — the structured judgment, never stdout. Only a verify run's agent can call this; the platform makes the verified/rejected transition from it. Give it ONCE, then end your turn.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args verifyArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		if err := exec.goalSvc.VerifySubGoal(ctx, exec.RunID, args.Verdict, args.Summary, args.Evidence); err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{"ok": true, "verdict": args.Verdict}), nil, nil
	})

	type integrateArgs struct {
		ChangeID string `json:"change_id" jsonschema:"the change to integrate (resolve with agentwork_get_change)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "integrate_change",
		Description: "Integrate a sub-goal's Change into THIS run's worktree: the platform merges the change's head ref into your branch — success marks the Change integrated; a conflict marks it conflicted and wakes the sub-goal's assignee to rework. Only the goal's owner can integrate.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args integrateArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil || exec.GoalID == "" {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		if err := exec.requireOwner(ctx); err != nil {
			return nil, nil, err
		}
		ch, err := exec.goalSvc.GetChange(ctx, args.ChangeID)
		if err != nil {
			return nil, nil, err
		}
		if ch.GoalID != exec.GoalID {
			return nil, nil, errors.New("the change does not belong to this goal")
		}
		if ch.Status == "integrated" {
			return toolResult(map[string]any{"ok": true, "status": "integrated", "note": "already integrated"}), nil, nil
		}
		if ch.Status == "conflict" {
			return toolResult(map[string]any{"ok": false, "status": "conflict", "note": "the assignee is reworking this change — wait for the new Revision before integrating"}), nil, nil
		}
		if err := exec.goalSvc.MarkChangeIntegrating(ctx, ch.ID); err != nil {
			return nil, nil, err
		}
		// The deterministic merge runs in THIS run's worktree (the owner's
		// workspace — the change head is a ref in the shared bare repo, so no
		// fetch is needed).
		cmd := execCommand(ctx, exec.Worktree, "git", "merge", "--no-ff", ch.HeadRef, "-m", "Integrate "+ch.ID)
		out, mergeErr := cmd.CombinedOutput()
		if mergeErr != nil {
			// Conflict: abort the merge in the worktree, record the Change
			// conflict — the assignee gets woken to rework on a new base.
			_ = execCommand(ctx, exec.Worktree, "git", "merge", "--abort").Run()
			if err := exec.goalSvc.MarkChangeIntegrated(ctx, ch.ID, false); err != nil {
				return nil, nil, err
			}
			return toolResult(map[string]any{"ok": false, "status": "conflict", "output": string(out), "note": "change marked conflicted — the sub-goal assignee has been woken to rework"}), nil, nil
		}
		if err := exec.goalSvc.MarkChangeIntegrated(ctx, ch.ID, true); err != nil {
			return nil, nil, err
		}
		return toolResult(map[string]any{"ok": true, "status": "integrated", "output": string(out)}), nil, nil
	})

	type getChangeArgs struct {
		ChangeID string `json:"change_id,omitempty" jsonschema:"a change id; omit to list THIS goal's changes"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "get_change",
		Description: "Inspect changes: one by id, or THIS goal's changes (the owner's integration list — status + head ref).",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args getChangeArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		if args.ChangeID != "" {
			c, err := exec.goalSvc.GetChange(ctx, args.ChangeID)
			if err != nil {
				return nil, nil, err
			}
			return toolResult(map[string]any{"change": map[string]any{"id": c.ID, "sub_goal_id": c.SubGoalID, "status": c.Status, "head_ref": c.HeadRef}}), nil, nil
		}
		if exec.GoalID == "" {
			return nil, nil, errors.New("no goal context — pass a change_id")
		}
		list, err := exec.goalSvc.ListChanges(ctx, exec.GoalID)
		if err != nil {
			return nil, nil, err
		}
		out := []map[string]any{}
		for _, c := range list {
			out = append(out, map[string]any{"id": c.ID, "sub_goal_id": c.SubGoalID, "status": c.Status, "head_ref": c.HeadRef})
		}
		return toolResult(map[string]any{"changes": out}), nil, nil
	})

	type listArgs struct {
		Status *string `json:"status,omitempty" jsonschema:"filter by status (backlog|active|review|done|failed|cancelled)"`
		// Limit is `any` (not `*int`) so the JSON schema has no type
		// constraint — some models serialize numbers as strings.
		Limit any `json:"limit,omitempty" jsonschema:"max results (default all)"`
	}
	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "goal_list",
		Description: "List goals (optionally filtered) — see what is waiting, what is active, what needs attention.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args listArgs) (*gmcp.CallToolResult, any, error) {
		if exec.goalSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		goals, err := exec.goalSvc.List(ctx)
		if err != nil {
			return nil, nil, err
		}
		var limit int
		if p := parseInt64Ptr(args.Limit); p != nil {
			limit = int(*p)
		}
		var out []map[string]any
		for _, g := range goals {
			if args.Status != nil && g.Status != *args.Status {
				continue
			}
			out = append(out, map[string]any{"id": g.ID, "title": g.Title, "status": g.Status, "assignee": g.AssigneeType + "/" + g.AssigneeID})
			if limit > 0 && len(out) >= limit {
				break
			}
		}
		return toolResult(map[string]any{"goals": out}), nil, nil
	})

	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "agent_list",
		Description: "List all agents — resolve agent uuids for consults (consult_agent) and handoffs (handoff_goal).",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args struct{}) (*gmcp.CallToolResult, any, error) {
		if exec.agentSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		agents, err := exec.agentSvc.List(ctx)
		if err != nil {
			return nil, nil, err
		}
		var out []map[string]any
		for _, a := range agents {
			out = append(out, map[string]any{"id": a.ID, "name": a.Name, "description": a.Description})
		}
		return toolResult(map[string]any{"agents": out}), nil, nil
	})

	gmcp.AddTool(srv, &gmcp.Tool{
		Name:        "squad_list",
		Description: "List all squads — resolve squad uuids for consults and handoffs.",
	}, func(ctx context.Context, req *gmcp.CallToolRequest, args struct{}) (*gmcp.CallToolResult, any, error) {
		if exec.squadSvc == nil {
			return nil, nil, errors.New("collaboration not wired for this run")
		}
		squads, err := exec.squadSvc.List(ctx)
		if err != nil {
			return nil, nil, err
		}
		var out []map[string]any
		for _, sq := range squads {
			out = append(out, map[string]any{"id": sq.ID, "name": sq.Name, "leader_id": sq.LeaderID})
		}
		return toolResult(map[string]any{"squads": out}), nil, nil
	})

	return srv
}

// errEmptyCommand guards the terminal_create tool against a missing executable.
var errEmptyCommand = errors.New("terminal_create: command is required")

// toolResult marshals the tool's JSON response (the SDK delivers text
// content; a structured string keeps the schema simple).
func toolResult(v map[string]any) *gmcp.CallToolResult {
	raw, _ := json.Marshal(v)
	return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: string(raw)}}}
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func derefStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func exitCode(r *acp.TerminalOutputResponse) *int {
	if r.ExitStatus == nil {
		return nil
	}
	return r.ExitStatus.ExitCode
}

func exitSignal(r *acp.TerminalOutputResponse) *string {
	if r.ExitStatus == nil {
		return nil
	}
	return r.ExitStatus.Signal
}

// HTTPHandler wraps the run's workspace server as a streamable-HTTP handler
// for /mcp/{runID}. Stateless: every request resolves the run's server.
func HTTPHandler(exec *Executor) http.Handler {
	return gmcp.NewStreamableHTTPHandler(func(*http.Request) *gmcp.Server {
		return NewServer(exec)
	}, &gmcp.StreamableHTTPOptions{Stateless: true})
}

// execCommand runs a command in dir (the integrate_change tool executes the
// deterministic git merge in the owner run's worktree — the run's workspace
// is the integration surface, per 决策 6-3).
func execCommand(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd
}

var _ = exec.Command // keep the import bound (execCommand covers all uses)
