// Client execution-environment proxy (DESIGN.md 决策 4-8): the run's
// agent (local stdio or remote ws/tcp) performs file reads/writes and
// command execution through Agent→Client RPCs handled here. The worktree
// always stays on the platform machine — the work a remote agent does is
// the work the daemon can verify and commit.
//
// Trust boundary: daemon user permissions, exactly like a stdio
// subprocess. No path whitelists/blacklists (same risk surface as stdio —
// sensitive-file filtering is a deferred item in DESIGN.md).
//
// Run context is injected at terminal/create (env): the run identity
// (AGENTWORK_GOAL_ID/RUN_ID/AGENT_ID) and the platform's server URL. The
// agent collaborates through the MCP collaboration tools, not a CLI
// (决策 4-13) — the CLI binary is deliberately absent from the agent
// environment.
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eushing/agentwork/internal/acp"
)

// runEnvironment implements acp.ClientRequestHandler for one run. Created
// per run in runTask, registered on the ACP session, and dropped at
// session close — terminal leftovers are killed by tm.cleanup at the same
// moment.
type runEnvironment struct {
	runID, goalID, agentID string
	workdir                string // the run's per-run worktree, <runID> (default terminal cwd)
	serverURL              string // AGENTWORK_SERVER_URL for the CLI
	tm                     *terminalManager
}

// newRunEnvironment builds the per-run handler.
func newRunEnvironment(runID, goalID, agentID, workdir, serverURL string) *runEnvironment {
	return &runEnvironment{
		runID:     runID,
		goalID:    goalID,
		agentID:   agentID,
		workdir:   workdir,
		serverURL: serverURL,
		tm:        newTerminalManager(),
	}
}

// runEnv builds the environment for spawned commands: platform base +
// agent-requested env + run context injected last (authoritative).
func (e *runEnvironment) runEnv(agentEnv []acp.EnvVariable) []string {
	env := os.Environ()
	for _, kv := range agentEnv {
		env = append(env, kv.Name+"="+kv.Value)
	}
	// Run context — last wins over anything the agent passes.
	env = append(env,
		"AGENTWORK_GOAL_ID="+e.goalID,
		"AGENTWORK_RUN_ID="+e.runID,
		"AGENTWORK_AGENT_ID="+e.agentID,
		"AGENTWORK_SERVER_URL="+e.serverURL,
	)
	return env
}

// ── fs ──

func (e *runEnvironment) HandleReadTextFile(ctx context.Context, req acp.ReadTextFileRequest) (*acp.ReadTextFileResponse, error) {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return nil, err
	}
	content := string(data)
	if req.Line != nil || req.Limit != nil {
		content = sliceLines(content, deref(req.Line, 1), deref(req.Limit, 0))
	}
	return &acp.ReadTextFileResponse{Content: content}, nil
}

func (e *runEnvironment) HandleWriteTextFile(ctx context.Context, req acp.WriteTextFileRequest) (*acp.WriteTextFileResponse, error) {
	if dir := filepath.Dir(req.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("write %s: %w", req.Path, err)
		}
	}
	if err := os.WriteFile(req.Path, []byte(req.Content), 0o644); err != nil {
		return nil, err
	}
	return &acp.WriteTextFileResponse{}, nil
}

// sliceLines returns lines [line, line+limit) (1-based). limit <= 0 means
// "to the end".
func sliceLines(content string, line, limit int) string {
	lines := strings.Split(content, "\n")
	if line < 1 {
		line = 1
	}
	if line > len(lines) {
		return ""
	}
	end := len(lines)
	if limit > 0 && line+limit-1 < end {
		end = line + limit - 1
	}
	return strings.Join(lines[line-1:end], "\n")
}

func deref(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// ── terminal ──

func (e *runEnvironment) HandleCreateTerminal(ctx context.Context, req acp.CreateTerminalRequest) (*acp.CreateTerminalResponse, error) {
	cwd := e.workdir
	if req.Cwd != nil && *req.Cwd != "" {
		cwd = *req.Cwd
	}
	byteLimit := 0
	if req.OutputByteLimit != nil {
		byteLimit = *req.OutputByteLimit
	}
	id, err := e.tm.create(req.Command, req.Args, e.runEnv(req.Env), cwd, byteLimit)
	if err != nil {
		return nil, err
	}
	return &acp.CreateTerminalResponse{TerminalID: id}, nil
}

func (e *runEnvironment) HandleTerminalOutput(ctx context.Context, req acp.TerminalOutputRequest) (*acp.TerminalOutputResponse, error) {
	// ACP v1 carries no cursor — the buffer's internal cursor gives the
	// stateless incremental semantics (each call returns what's new).
	resp, _, _, err := e.tm.output(req.TerminalID, nil)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (e *runEnvironment) HandleWaitForTerminalExit(ctx context.Context, req acp.WaitForTerminalExitRequest) (*acp.WaitForTerminalExitResponse, error) {
	return e.tm.wait(req.TerminalID)
}

func (e *runEnvironment) HandleKillTerminal(ctx context.Context, req acp.KillTerminalRequest) (*acp.KillTerminalResponse, error) {
	if err := e.tm.kill(req.TerminalID); err != nil {
		return nil, err
	}
	return &acp.KillTerminalResponse{}, nil
}

func (e *runEnvironment) HandleReleaseTerminal(ctx context.Context, req acp.ReleaseTerminalRequest) (*acp.ReleaseTerminalResponse, error) {
	if err := e.tm.release(req.TerminalID); err != nil {
		return nil, err
	}
	return &acp.ReleaseTerminalResponse{}, nil
}

// HandleRequestPermission auto-approves tool execution. This is the ACP
// CLIENT's tool-permission model, NOT the platform's approval checkpoint:
// the execution-environment proxy's trust boundary is daemon-user
// permissions — exactly like a stdio subprocess — so the agent's tools
// (fs/terminal RPCs) run without a client-side permission gate. Rejecting
// here blocked every remote agent's write/shell (a live 8787-ws run
// surfaced it). Platform approvals (goal review / approve) remain human,
// see DESIGN.md.
func (e *runEnvironment) HandleRequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	// Prefer an allow-always option, else fall back to the first offered.
	for _, o := range req.Options {
		if o.Kind == acp.PermissionAllowAlways {
			id := o.OptionID
			return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{OptionID: &id}}, nil
		}
	}
	if len(req.Options) > 0 {
		id := req.Options[0].OptionID
		return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{OptionID: &id}}, nil
	}
	// No options offered: an empty outcome (neither selected nor cancelled).
	return &acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{}}, nil
}
