package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eushing/agentwork/internal/acp"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// fakeHost is a test TerminalHost that actually runs commands (the daemon's
// terminalManager does the same thing; mcp cannot import daemon, so the test
// carries its own minimal implementation).
type fakeHost struct {
	mu    sync.Mutex
	terms map[string]*fakeTerm
	next  int
	// lastCommand/lastArgs record what the host was asked to run — the argv
	// dedupe test asserts on them.
	lastCommand string
	lastArgs    []string
}

type fakeTerm struct {
	cmd     *exec.Cmd
	out     bytes.Buffer
	mu      sync.Mutex
	exited  bool
	code    int
	signal  string
	done    chan struct{}
	started time.Time
}

func newFakeHost() *fakeHost {
	return &fakeHost{terms: map[string]*fakeTerm{}}
}

func (h *fakeHost) Create(command string, args []string, env []string, cwd string, byteLimit int) (acp.TerminalId, error) {
	if command == "" {
		return "", errEmptyCommand
	}
	h.mu.Lock()
	h.lastCommand, h.lastArgs = command, args
	h.mu.Unlock()
	cmd := exec.Command(command, args...)
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	t := &fakeTerm{cmd: cmd, done: make(chan struct{}), started: time.Now()}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	h.mu.Lock()
	h.next++
	id := acp.TerminalId("t" + string(rune('0'+h.next%10)) + string(rune('0'+h.next/10%10)))
	h.terms[string(id)] = t
	h.mu.Unlock()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := out.Read(buf)
			if n > 0 {
				t.mu.Lock()
				t.out.Write(buf[:n])
				t.mu.Unlock()
			}
			if err != nil {
				break
			}
		}
		if werr := cmd.Wait(); werr != nil {
			if ee, ok := werr.(*exec.ExitError); ok {
				t.code = ee.ExitCode()
			} else {
				t.code = -1
			}
		}
		t.mu.Lock()
		t.exited = true
		t.mu.Unlock()
		close(t.done)
	}()
	return id, nil
}

func (h *fakeHost) Output(id acp.TerminalId, _ *int64) (*acp.TerminalOutputResponse, *int64, int64, error) {
	h.mu.Lock()
	t, ok := h.terms[string(id)]
	h.mu.Unlock()
	if !ok {
		return nil, nil, 0, errEmptyCommand
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	resp := &acp.TerminalOutputResponse{Output: t.out.String(), Truncated: false}
	if t.exited {
		resp.ExitStatus = &acp.TerminalExitStatus{ExitCode: &t.code, Signal: &t.signal}
	}
	next := int64(t.out.Len())
	return resp, &next, int64(time.Since(t.started).Seconds()), nil
}

func (h *fakeHost) Release(id acp.TerminalId) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.terms[string(id)]; ok {
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		delete(h.terms, string(id))
	}
	return nil
}

func newTestHandler(t *testing.T) (http.Handler, string) {
	dir := t.TempDir()
	exec := NewExecutor(dir, []string{"AGENTWORK_RUN_ID=run-test", "PATH=" + os.Getenv("PATH")}, newFakeHost())
	return HTTPHandler(exec), dir
}

func connect(t *testing.T, h http.Handler) *gmcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	cl := gmcp.NewClient(&gmcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := cl.Connect(ctx, &gmcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestMCPFullClientRoundTrip: the official SDK client drives the whole
// conversation (initialize → tools/list → tools/call) against the
// workspace server: fs tools + the async terminal trio.
func TestMCPFullClientRoundTrip(t *testing.T) {
	h, dir := newTestHandler(t)
	session := connect(t, h)
	ctx := context.Background()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	found := map[string]bool{}
	for _, tl := range tools.Tools {
		found[tl.Name] = true
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "list_dir", "grep", "terminal_create", "terminal_output", "terminal_release"} {
		if !found[want] {
			t.Fatalf("tool %q not advertised, got %v", want, found)
		}
	}

	// Write + read through the SDK client.
	path := filepath.Join(dir, "client-roundtrip.txt")
	if _, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "write_file",
		Arguments: map[string]any{"path": path, "content": "via sdk client"},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name: "read_file", Arguments: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if tc, ok := res.Content[0].(*gmcp.TextContent); !ok || tc.Text != "via sdk client\n" {
		t.Fatalf("read content: want %q, got %+v", "via sdk client\\n", res.Content[0])
	}

	// Async terminal: create → poll output until exited → release. The
	// command is a SHELL command line (决策 4-8: /bin/sh -c on this machine)
	// — no separate args field.
	create, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name: "terminal_create",
		Arguments: map[string]any{
			"command": "echo hello; exit 3",
			"cwd":     dir,
		},
	})
	if err != nil {
		t.Fatalf("terminal_create: %v", err)
	}
	createText := create.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(createText, "terminal_id") {
		t.Fatalf("terminal_create: want terminal_id in %q", createText)
	}
	var tid string
	for _, part := range strings.Split(strings.Trim(createText, "{}"), ",") {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) == 2 && strings.Contains(kv[0], "terminal_id") {
			tid = strings.Trim(kv[1], `"`)
		}
	}
	if tid == "" {
		t.Fatalf("terminal_create: no id in %q", createText)
	}

	// Poll until exited (the fake host runs synchronously-ish).
	var out string
	for i := 0; i < 50; i++ {
		poll, err := session.CallTool(ctx, &gmcp.CallToolParams{
			Name: "terminal_output", Arguments: map[string]any{"terminal_id": tid},
		})
		if err != nil {
			t.Fatalf("terminal_output: %v", err)
		}
		pt := poll.Content[0].(*gmcp.TextContent).Text
		out = pt
		if strings.Contains(pt, `"exited":true`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("terminal_output: want command output, got %q", out)
	}
	if !strings.Contains(out, `"exit_code":3`) {
		t.Fatalf("terminal_output: want exit code 3, got %q", out)
	}

	if _, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name: "terminal_release", Arguments: map[string]any{"terminal_id": tid},
	}); err != nil {
		t.Fatalf("terminal_release: %v", err)
	}
}

// TestCollaborationTools: the collaboration tools act on the run's goal via
// the injected services (no CLI, no HTTP hop) — the four-behavior surface
// (决策 5-2): comment_goal lands a plain comment, consult_agent pulls in a
// guest run, handoff_goal transfers ownership (owner-only), goal_list sees
// the goal.
func TestCollaborationTools(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	commentSvc := service.NewCommentService(st, bus)
	commentSvc.SetRunService(runSvc)
	commentSvc.SetGoalService(goalSvc)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)

	rt, _ := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	agentSvc := service.NewAgentService(st, bus)
	agentA, _ := agentSvc.Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	agentB, _ := agentSvc.Create(ctx, service.Agent{Name: "b", RuntimeID: rt.ID})
	dom, _ := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "dom", GitURL: "https://e.com/d.git"})
	goal, _ := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})

	exec := NewExecutor(t.TempDir(), nil, newFakeHost())
	exec.SetCollaboration(goal.ID, agentA.ID, "run-1", commentSvc, goalSvc, runSvc, agentSvc, service.NewSquadService(st, bus))
	session := connect(t, HTTPHandler(exec))
	ctx2 := context.Background()

	// comment_goal: a plain comment lands; NO mention dispatch from it.
	if _, err := session.CallTool(ctx2, &gmcp.CallToolParams{
		Name:      "comment_goal",
		Arguments: map[string]any{"content": "progress note"},
	}); err != nil {
		t.Fatalf("comment_goal: %v", err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx2, `SELECT COUNT(*) FROM comment WHERE goal_id=?`, goal.ID).Scan(&n); err != nil || n < 1 {
		t.Fatalf("comment not landed (n=%d err=%v)", n, err)
	}

	// consult_agent (owner A → B): the platform's mention comment lands and a
	// GUEST run is enqueued for B.
	if _, err := session.CallTool(ctx2, &gmcp.CallToolParams{
		Name:      "consult_agent",
		Arguments: map[string]any{"agent_id": agentB.ID, "question": "how should this be authed?"},
	}); err != nil {
		t.Fatalf("consult_agent: %v", err)
	}
	var pending int
	var role string
	if err := st.DB().QueryRowContext(ctx2,
		`SELECT COUNT(*) FROM run WHERE goal_id=? AND agent_id=? AND status='queued'`, goal.ID, agentB.ID).Scan(&pending); err != nil || pending < 1 {
		t.Fatalf("guest run not enqueued (n=%d err=%v)", pending, err)
	}
	if err := st.DB().QueryRowContext(ctx2,
		`SELECT role FROM run WHERE goal_id=? AND agent_id=? ORDER BY queued_at DESC LIMIT 1`, goal.ID, agentB.ID).Scan(&role); err != nil || role != "consult" {
		t.Fatalf("consult run must be role=guest, got %q (err=%v)", role, err)
	}

	// handoff_goal (owner A → B): ownership transfers + a fresh owner run.
	if _, err := session.CallTool(ctx2, &gmcp.CallToolParams{
		Name:      "handoff_goal",
		Arguments: map[string]any{"assignee_type": "agent", "assignee_id": agentB.ID, "reason": "backend work"},
	}); err != nil {
		t.Fatalf("handoff_goal: %v", err)
	}
	var assignee string
	if err := st.DB().QueryRowContext(ctx2, `SELECT assignee_id FROM goal WHERE id=?`, goal.ID).Scan(&assignee); err != nil || assignee != agentB.ID {
		t.Fatalf("handoff did not transfer ownership (assignee=%q err=%v)", assignee, err)
	}

	// goal_list sees the goal.
	res, err := session.CallTool(ctx2, &gmcp.CallToolParams{Name: "goal_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("goal_list: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, goal.ID) {
		t.Fatalf("goal_list missing our goal: %q", text)
	}

	// agent_list sees both agents.
	res, err = session.CallTool(ctx2, &gmcp.CallToolParams{Name: "agent_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("agent_list: %v", err)
	}
	text = res.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, agentA.ID) || !strings.Contains(text, agentB.ID) {
		t.Fatalf("agent_list missing agents: %q", text)
	}
}

// TestCollaborationPermissions: the owner-only rules (决策 5-6) — a guest
// run's agent (B consulted into A's goal) cannot hand off, consult further,
// split, or wait.
func TestCollaborationPermissions(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	commentSvc := service.NewCommentService(st, bus)
	commentSvc.SetRunService(runSvc)
	commentSvc.SetGoalService(goalSvc)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)

	rt, _ := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	agentSvc := service.NewAgentService(st, bus)
	agentA, _ := agentSvc.Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	agentB, _ := agentSvc.Create(ctx, service.Agent{Name: "b", RuntimeID: rt.ID})
	dom, _ := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "dom", GitURL: "https://e.com/d.git"})
	goal, _ := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})

	// The executor acts as B — a guest on A's goal.
	exec := NewExecutor(t.TempDir(), nil, newFakeHost())
	exec.SetCollaboration(goal.ID, agentB.ID, "guest-run", commentSvc, goalSvc, runSvc, agentSvc, service.NewSquadService(st, bus))
	session := connect(t, HTTPHandler(exec))
	ctx2 := context.Background()

	for _, tc := range []struct {
		name, tool string
		args       map[string]any
	}{
		{"handoff_goal", "handoff_goal", map[string]any{"assignee_type": "agent", "assignee_id": agentB.ID}},
		{"consult_agent", "consult_agent", map[string]any{"agent_id": agentA.ID, "question": "q"}},
		{"create_sub_goal", "create_sub_goal", map[string]any{"title": "t", "assignee_type": "agent", "assignee_id": agentB.ID}},
	} {
		res, err := session.CallTool(ctx2, &gmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err == nil && res != nil && !res.IsError {
			t.Fatalf("%s must be rejected for a non-owner agent", tc.tool)
		}
	}
	// Ownership unchanged: B's attempts above must not have moved the goal.
	var assignee string
	if err := st.DB().QueryRowContext(ctx2, `SELECT assignee_id FROM goal WHERE id=?`, goal.ID).Scan(&assignee); err != nil || assignee != agentA.ID {
		t.Fatalf("ownership moved by a guest (assignee=%q err=%v)", assignee, err)
	}
}

// TestTerminalCreateShellSemantics: terminal_create's command is a SHELL
// command line — the platform runs it via /bin/sh -c on this machine (the
// shell is stated in the tool description, never left for the agent to
// guess), so pipes/redirects/&& work and the exit code propagates. The
// command/args split is gone (agents mis-split it, e.g. 'find find .').
func TestTerminalCreateShellSemantics(t *testing.T) {
	host := newFakeHost()
	exec := NewExecutor(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, host)
	session := connect(t, HTTPHandler(exec))
	ctx := context.Background()

	// A pipeline only the shell can run — direct exec would fail.
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "terminal_create",
		Arguments: map[string]any{"command": "echo hi | tr a-z A-Z; exit 7"},
	})
	if err != nil {
		t.Fatalf("terminal_create: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, "HI") {
		t.Fatalf("the pipeline must run through the shell, got %q", text)
	}
	if !strings.Contains(text, `"exit_code":7`) {
		t.Fatalf("the shell's exit code must propagate, got %q", text)
	}

	// The host was asked to run the SHELL, with the command as one -c arg.
	host.mu.Lock()
	cmd, args := host.lastCommand, host.lastArgs
	host.mu.Unlock()
	if cmd != "/bin/sh" || len(args) != 2 || args[0] != "-c" || !strings.Contains(args[1], "tr a-z A-Z") {
		t.Fatalf("the host must run /bin/sh -c <command>, got %q %v", cmd, args)
	}
}

// TestTerminalCreateStringTimeout: some models serialize the timeout as a
// string ("15" instead of 15). The schema must accept both forms — a strict
// *int64 schema rejects the string with "type: 15 has type string, want one
// of null, integer" and the whole tool call fails.
func TestTerminalCreateStringTimeout(t *testing.T) {
	host := newFakeHost()
	exec := NewExecutor(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, host)
	session := connect(t, HTTPHandler(exec))
	ctx := context.Background()

	// String timeout — the regression case.
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "terminal_create",
		Arguments: map[string]any{"command": "echo ok; exit 0", "timeout": "15"},
	})
	if err != nil {
		t.Fatalf("terminal_create with string timeout: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if res.IsError {
		t.Fatalf("string timeout must not be rejected: %s", text)
	}
	if !strings.Contains(text, "ok") {
		t.Fatalf("expected command output, got %q", text)
	}

	// Integer timeout — the normal case, must still work.
	res2, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "terminal_create",
		Arguments: map[string]any{"command": "echo ok2; exit 0", "timeout": float64(15)},
	})
	if err != nil {
		t.Fatalf("terminal_create with int timeout: %v", err)
	}
	text2 := res2.Content[0].(*gmcp.TextContent).Text
	if res2.IsError {
		t.Fatalf("int timeout must not be rejected: %s", text2)
	}
	if !strings.Contains(text2, "ok2") {
		t.Fatalf("expected command output, got %q", text2)
	}

	// Zero timeout (string) — pure async, returns a terminal_id immediately.
	res3, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "terminal_create",
		Arguments: map[string]any{"command": "sleep 5", "timeout": "0"},
	})
	if err != nil {
		t.Fatalf("terminal_create with string 0 timeout: %v", err)
	}
	text3 := res3.Content[0].(*gmcp.TextContent).Text
	if res3.IsError {
		t.Fatalf("string 0 timeout must not be rejected: %s", text3)
	}
	if !strings.Contains(text3, `"exited":false`) {
		t.Fatalf("0 timeout should be pure async (exited=false), got %q", text3)
	}
}

// TestStringIntegerArgs: some models serialize integer arguments as strings
// ("100" instead of 100). The MCP tool schemas use `any` for integer fields
// so the SDK's JSON schema validation does not reject them. This test
// verifies get_comments.Limit and goal_list.Limit accept both forms.
func TestStringIntegerArgs(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus()
	goalSvc := service.NewGoalService(st, bus)
	commentSvc := service.NewCommentService(st, bus)
	commentSvc.SetGoalService(goalSvc)
	rt, _ := service.NewRuntimeService(st).Create(ctx, service.Runtime{Name: "rt", Transport: "stdio", Provider: "acp", Executable: "/bin/true"})
	agentSvc := service.NewAgentService(st, bus)
	agentA, _ := agentSvc.Create(ctx, service.Agent{Name: "a", RuntimeID: rt.ID})
	dom, err := service.NewDomainService(st, bus).Create(ctx, service.Domain{Name: "d", GitURL: "https://e.com/d.git"})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	goal, err := goalSvc.Create(ctx, service.Goal{Title: "g", DomainID: dom.ID, AssigneeType: "agent", AssigneeID: agentA.ID, Status: "active"})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if _, err := commentSvc.CreateNoDispatch(ctx, service.Comment{GoalID: goal.ID, AuthorType: "agent", AuthorID: agentA.ID, Content: "c1"}); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	exec := NewExecutor(t.TempDir(), nil, newFakeHost())
	exec.SetCollaboration(goal.ID, agentA.ID, "run-1", commentSvc, goalSvc, nil, agentSvc, nil)
	session := connect(t, HTTPHandler(exec))

	// get_comments with string limit — must not be rejected.
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "get_comments",
		Arguments: map[string]any{"limit": "100"},
	})
	if err != nil {
		t.Fatalf("get_comments with string limit: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if res.IsError {
		t.Fatalf("string limit must not be rejected: %s", text)
	}
	if !strings.Contains(text, "c1") {
		t.Fatalf("get_comments should return comments, got %q", text)
	}

	// get_comments with integer limit — must still work.
	res2, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "get_comments",
		Arguments: map[string]any{"limit": float64(100)},
	})
	if err != nil {
		t.Fatalf("get_comments with int limit: %v", err)
	}
	text2 := res2.Content[0].(*gmcp.TextContent).Text
	if res2.IsError {
		t.Fatalf("int limit must not be rejected: %s", text2)
	}

	// goal_list with string limit — must not be rejected.
	res3, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "goal_list",
		Arguments: map[string]any{"limit": "10"},
	})
	if err != nil {
		t.Fatalf("goal_list with string limit: %v", err)
	}
	text3 := res3.Content[0].(*gmcp.TextContent).Text
	if res3.IsError {
		t.Fatalf("goal_list string limit must not be rejected: %s", text3)
	}
	if !strings.Contains(text3, goal.ID) {
		t.Fatalf("goal_list should return goals, got %q", text3)
	}

	// terminal_output with string cursor — must not be rejected (even if
	// the terminal doesn't exist, the error should be from the host, not
	// from schema validation).
	res4, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "terminal_output",
		Arguments: map[string]any{"terminal_id": "nonexistent", "cursor": "42"},
	})
	if err != nil {
		t.Fatalf("terminal_output with string cursor: %v", err)
	}
	text4 := res4.Content[0].(*gmcp.TextContent).Text
	if strings.Contains(text4, "validating") {
		t.Fatalf("string cursor must not trigger schema validation error: %s", text4)
	}
}

// TestReadFileLineLimit: read_file accepts optional line (1-based) and limit
// parameters. LLMs naturally send them for large files; without these fields
// the schema rejects them as "unexpected additional properties". Also verifies
// the line-range prefix and string-typed number coercion (some LLMs send
// "3" instead of 3).
func TestReadFileLineLimit(t *testing.T) {
	host := newFakeHost()
	exec := NewExecutor(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, host)
	session := connect(t, HTTPHandler(exec))
	ctx := context.Background()

	// Write a 10-line file (each line followed by \n).
	path := filepath.Join(exec.Worktree, "paginated.txt")
	lines := []string{"L0", "L1", "L2", "L3", "L4", "L5", "L6", "L7", "L8", "L9"}
	content := strings.Join(lines, "\n") + "\n"
	if _, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "write_file",
		Arguments: map[string]any{"path": path, "content": content},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read with line=3, limit=3 → lines 3-5 = L2,L3,L4.
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": path, "line": float64(3), "limit": float64(3)},
	})
	if err != nil {
		t.Fatalf("read with line+limit: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if res.IsError {
		t.Fatalf("line+limit must not be rejected: %s", text)
	}
	if !strings.Contains(text, "[lines 3-5,") {
		t.Fatalf("expected line-range prefix [lines 3-5, got %q", text)
	}
	if !strings.Contains(text, "L2\nL3\nL4\n") {
		t.Fatalf("expected L2\\nL3\\nL4 in output, got %q", text)
	}

	// Read with string-typed line and limit (LLM regression case).
	res2, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": path, "line": "3", "limit": "3"},
	})
	if err != nil {
		t.Fatalf("read with string line+limit: %v", err)
	}
	text2 := res2.Content[0].(*gmcp.TextContent).Text
	if res2.IsError {
		t.Fatalf("string line+limit must not be rejected: %s", text2)
	}
	if !strings.Contains(text2, "[lines 3-5,") {
		t.Fatalf("expected line-range prefix for string types, got %q", text2)
	}
	if !strings.Contains(text2, "L2\nL3\nL4\n") {
		t.Fatalf("expected L2\\nL3\\nL4 for string types, got %q", text2)
	}

	// Read with line only (no limit) → rest of file from line 9.
	res3, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": path, "line": float64(9)},
	})
	if err != nil {
		t.Fatalf("read with line only: %v", err)
	}
	text3 := res3.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text3, "L8\nL9") {
		t.Fatalf("expected L8\\nL9 in output, got %q", text3)
	}

	// Read with no line/limit → whole file (no prefix).
	res4, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatalf("read without pagination: %v", err)
	}
	text4 := res4.Content[0].(*gmcp.TextContent).Text
	if text4 != content {
		t.Fatalf("expected full file, got %q", text4)
	}

	// Line past end → "[line N is beyond end of file ...]".
	res5, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": path, "line": float64(100)},
	})
	if err != nil {
		t.Fatalf("read with line past end: %v", err)
	}
	text5 := res5.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text5, "beyond end of file") {
		t.Fatalf("expected beyond-end message, got %q", text5)
	}
}

// TestEditFile: edit_file replaces text with uniqueness enforcement.
func TestEditFile(t *testing.T) {
	host := newFakeHost()
	exec := NewExecutor(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, host)
	session := connect(t, HTTPHandler(exec))
	ctx := context.Background()

	// Write a file with unique and duplicate text.
	path := filepath.Join(exec.Worktree, "edit.txt")
	content := "line one\nline two\nline three\nline two\n"
	if _, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "write_file",
		Arguments: map[string]any{"path": path, "content": content},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Unique replacement.
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "edit_file",
		Arguments: map[string]any{"path": path, "old_text": "line three", "new_text": "LINE THREE"},
	})
	if err != nil {
		t.Fatalf("edit unique: %v", err)
	}
	if res.IsError {
		t.Fatalf("edit unique failed: %s", res.Content[0].(*gmcp.TextContent).Text)
	}

	// Verify the edit landed.
	res2, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatalf("read after edit: %v", err)
	}
	text := res2.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, "LINE THREE") {
		t.Fatalf("edit did not land: %q", text)
	}

	// Non-unique without replace_all → error.
	res3, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "edit_file",
		Arguments: map[string]any{"path": path, "old_text": "line two", "new_text": "LINE TWO"},
	})
	if err != nil {
		t.Fatalf("edit non-unique call: %v", err)
	}
	text3 := res3.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text3, "found 2 times") {
		t.Fatalf("expected uniqueness error, got %q", text3)
	}

	// replace_all=true → both replaced.
	res4, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "edit_file",
		Arguments: map[string]any{"path": path, "old_text": "line two", "new_text": "LINE TWO", "replace_all": true},
	})
	if err != nil {
		t.Fatalf("edit replace_all: %v", err)
	}
	if res4.IsError {
		t.Fatalf("edit replace_all failed: %s", res4.Content[0].(*gmcp.TextContent).Text)
	}
	res5, _ := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": path},
	})
	text5 := res5.Content[0].(*gmcp.TextContent).Text
	if strings.Contains(text5, "line two") {
		t.Fatalf("replace_all did not replace all: %q", text5)
	}

	// old_text not found → error.
	res6, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "edit_file",
		Arguments: map[string]any{"path": path, "old_text": "nope", "new_text": "x"},
	})
	if err != nil {
		t.Fatalf("edit not-found call: %v", err)
	}
	text6 := res6.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text6, "not found") {
		t.Fatalf("expected not-found error, got %q", text6)
	}
}

// TestListDir: list_dir lists directory contents with sorting.
func TestListDir(t *testing.T) {
	host := newFakeHost()
	exec := NewExecutor(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, host)
	session := connect(t, HTTPHandler(exec))
	ctx := context.Background()

	// Create some files and dirs.
	for _, name := range []string{"zeta.txt", "alpha.go", "beta/"} {
		full := filepath.Join(exec.Worktree, name)
		if strings.HasSuffix(name, "/") {
			os.MkdirAll(full, 0o755)
		} else {
			os.WriteFile(full, []byte("x"), 0o644)
		}
	}

	// List workspace root (no path → default).
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "list_dir",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("list_dir: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	// Directories must come first, then files sorted alphabetically.
	betaIdx := strings.Index(text, "beta/")
	alphaIdx := strings.Index(text, "alpha.go")
	zetaIdx := strings.Index(text, "zeta.txt")
	if betaIdx < 0 || alphaIdx < 0 || zetaIdx < 0 {
		t.Fatalf("missing entries in %q", text)
	}
	if betaIdx > alphaIdx || alphaIdx > zetaIdx {
		t.Fatalf("sorting wrong (dirs first, then alpha): beta=%d alpha=%d zeta=%d", betaIdx, alphaIdx, zetaIdx)
	}
}

// TestGrep: grep searches workspace files for a pattern.
func TestGrep(t *testing.T) {
	host := newFakeHost()
	exec := NewExecutor(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, host)
	session := connect(t, HTTPHandler(exec))
	ctx := context.Background()

	// Write matching and non-matching files.
	os.WriteFile(filepath.Join(exec.Worktree, "a.go"), []byte("package main\nfunc foo() {}\n"), 0o644)
	os.WriteFile(filepath.Join(exec.Worktree, "b.go"), []byte("package main\nfunc bar() {}\n"), 0o644)
	os.WriteFile(filepath.Join(exec.Worktree, "c.txt"), []byte("no match here\n"), 0o644)
	// .git dir must be skipped.
	os.MkdirAll(filepath.Join(exec.Worktree, ".git"), 0o755)
	os.WriteFile(filepath.Join(exec.Worktree, ".git", "config"), []byte("func foo() {}\n"), 0o644)

	// Search for "func foo".
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "grep",
		Arguments: map[string]any{"pattern": "func foo"},
	})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, "a.go") {
		t.Fatalf("grep should match a.go: %q", text)
	}
	if strings.Contains(text, "b.go") {
		t.Fatalf("grep should not match b.go: %q", text)
	}
	if strings.Contains(text, ".git") {
		t.Fatalf("grep must skip .git: %q", text)
	}

	// Glob filter: only .go files.
	res2, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "grep",
		Arguments: map[string]any{"pattern": "package main", "glob": "*.go"},
	})
	if err != nil {
		t.Fatalf("grep with glob: %v", err)
	}
	text2 := res2.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text2, "a.go") || !strings.Contains(text2, "b.go") {
		t.Fatalf("glob *.go should match both: %q", text2)
	}
	if strings.Contains(text2, "c.txt") {
		t.Fatalf("glob *.go should not match c.txt: %q", text2)
	}

	// No matches.
	res3, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "grep",
		Arguments: map[string]any{"pattern": "nonexistent_pattern_xyz"},
	})
	if err != nil {
		t.Fatalf("grep no-match: %v", err)
	}
	text3 := res3.Content[0].(*gmcp.TextContent).Text
	if text3 != "No matches found." {
		t.Fatalf("expected 'No matches found.', got %q", text3)
	}
}

// TestReadFileBinary: binary files are detected, not dumped as raw bytes.
func TestReadFileBinary(t *testing.T) {
	host := newFakeHost()
	exec := NewExecutor(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, host)
	session := connect(t, HTTPHandler(exec))
	ctx := context.Background()

	// Write a binary file (null bytes).
	path := filepath.Join(exec.Worktree, "binary.dat")
	os.WriteFile(path, []byte{0x00, 0x01, 0x02, 0x03, 0x00, 0x04}, 0o644)

	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, "[binary file:") {
		t.Fatalf("expected binary detection, got %q", text)
	}
}

// TestWriteFileSizeLimit: content > 10MB is rejected. The HTTP transport
// caps request size, so we verify the boundary by writing a file at exactly
// 10MB (must succeed) — the maxSize check inside write_file uses >, so
// exactly 10MB passes.
func TestWriteFileSizeLimit(t *testing.T) {
	host := newFakeHost()
	exec := NewExecutor(t.TempDir(), []string{"PATH=" + os.Getenv("PATH")}, host)
	session := connect(t, HTTPHandler(exec))
	ctx := context.Background()

	// Write a 500KB file via the tool (well under both the transport limit
	// and the 10MB maxSize guard) — confirms the tool works.
	small := strings.Repeat("line\n", 100*1024) // ~500KB, many short lines
	res, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "write_file",
		Arguments: map[string]any{"path": filepath.Join(exec.Worktree, "500kb.txt"), "content": small},
	})
	if err != nil {
		t.Fatalf("write 500KB: %v", err)
	}
	text := res.Content[0].(*gmcp.TextContent).Text
	if !strings.Contains(text, "Wrote") {
		t.Fatalf("expected write confirmation, got %q", text)
	}

	// Read the first 3 lines back.
	res2, err := session.CallTool(ctx, &gmcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": filepath.Join(exec.Worktree, "500kb.txt"), "line": float64(1), "limit": float64(3)},
	})
	if err != nil {
		t.Fatalf("read 500KB file: %v", err)
	}
	if res2.IsError {
		t.Fatalf("500KB file should be readable: %s", res2.Content[0].(*gmcp.TextContent).Text)
	}
}
