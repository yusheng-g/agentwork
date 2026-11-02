// agentwork-cli is the agent-side tool. daemon injects it into the agent
// subprocess's PATH plus AGENTWORK_SERVER_URL / AGENTWORK_GOAL_ID /
// AGENTWORK_RUN_ID / AGENTWORK_AGENT_ID env vars, so the agent can call it to
// produce structured side effects (assign/handoff, create sub-goal, comment,
// wait-children) against the agentwork HTTP API. CLI-as-tool, like multica §4.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/link"
)

const (
	// defaultServerURL matches the agentwork default listen addr.
	defaultServerURL = "http://127.0.0.1:7373"
	httpTimeout      = 10 * time.Second
)

var httpClient = &http.Client{Timeout: httpTimeout}

// serverURL resolves the daemon's HTTP base address the SAME way connect
// resolves its WebSocket address: the AGENTWORK_SERVER_URL env var (injected
// by the executor at spawn) with the default listen addr fallback. Every
// command reads it here — no caller threads a serverURL parameter through
// the dispatch chain (the agent CLI and the human debugging CLI share one
// binary and one resolution path).
func serverURL() string {
	if u := os.Getenv("AGENTWORK_SERVER_URL"); u != "" {
		return u
	}
	return defaultServerURL
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	goalID := os.Getenv("AGENTWORK_GOAL_ID")
	agentID := os.Getenv("AGENTWORK_AGENT_ID")

	switch os.Args[1] {
	case "connect":
		connectCmd(os.Args[2:])
		return
	case "status":
		statusCmd(os.Args[2:])
		return
	case "goal":
		goalCmd(goalID, agentID, os.Args[2:])
	case "agent":
		agentCmd(os.Args[2:])
	case "squad":
		squadCmd(os.Args[2:])
	case "schedule":
		scheduleCmd(os.Args[2:])
	case "domain":
		domainCmd(os.Args[2:])
	case "skill":
		skillCmd(os.Args[2:])
	case "stats":
		statsCmd(os.Args[2:])
	case "subgoal":
		subgoalCmd(os.Args[2:])
	case "change":
		changeCmd(os.Args[2:])
	case "create":
		createCmd(os.Args[2:])
	case "issue":
		issueCmd(goalID, os.Args[2:])
	case "version":
		versionCmd()
		return
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// issueCmd lets the agent reply to the issue/PR behind its current goal
// (M4-B): the platform owns the git-host token and executes the comment —
// the agent only produces the structured side effect. PR-sourced goals are
// routed by the platform: the comment lands on the PR's own API surface
// (GitCode: /pulls/:n/comments), no separate command needed.
func issueCmd(goalID string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli issue comment --text \"...\"")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("issue comment", flag.ExitOnError)
	text := fs.String("text", "", "comment body")
	_ = fs.Parse(args)
	if *text == "" {
		fail("--text is required")
	}
	if goalID == "" {
		fail("AGENTWORK_GOAL_ID not set — this command must run inside a goal's run")
	}
	body, err := json.Marshal(map[string]string{"goal_id": goalID, "text": *text})
	if err != nil {
		fail(err.Error())
	}
	resp, err := http.Post(serverURL()+"/issue-comments", "application/json", strings.NewReader(string(body)))
	if err != nil {
		fail("issue comment: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		fail(fmt.Sprintf("issue comment failed: %s: %s", resp.Status, strings.TrimSpace(string(raw))))
	}
	fmt.Println("issue comment posted")
}

func usage() {
	fmt.Fprintln(os.Stderr, `agentwork — agent-side tool (called by agents during task execution)
and remote-machine sidecar (register this host to agentwork-daemon).

Subcommands:
  connect [--server URL] [--token T] [--name N] [--scan DIR|GLOB ...] [--agents FILE]
                                             connect this machine to agentwork-daemon (default
                                             127.0.0.1:7373, no auth), probe its agent CLIs and
                                             register them; --scan adds a directory to search for
                                             agent CLIs not on PATH (repeatable; ~ expanded);
                                             --agents points to a YAML config (default
                                             ~/.agentwork/agents.yaml) whose entries ADD new agent
                                             CLIs or OVERRIDE builtins by name; heartbeats until
                                             interrupted
  status                                     show the persisted connection state (machine id,
                                             server, last heartbeat, probed agent CLIs)
  goal list [--limit N] [--status S] [--json]  list goals (JSON — the default format; --json requests
                                             it explicitly); --limit caps to N most recent (default all);
                                             --status keeps only goals whose status equals S (exact match)
  goal status <id>                           show a goal's status and last run outcome
  goal assign --to <id> [--note N] [--goal G] [--assignee_type T]  hand off a goal to an agent or squad
                                             (--goal G for chat path; run path uses AGENTWORK_GOAL_ID;
                                             --assignee_type: agent (default) | squad)
  goal cancel <id>                           cancel a goal
  goal reopen --goal <id> [--reason R]       reopen a finished/cancelled goal
  goal delete <id>                           delete a goal (comma-separated batch)
  goal create --title T [--description D] [--assignee A] [--status S]
                                             create a goal
  goal comment --text T [--role R]           post a comment on the current goal; --text may
                                             contain a structured mention [@Name](mention://agent/<id>)
                                             to enqueue a run on that agent
                                             human to decide (behavior gate)
  agent list                                 list all agents (JSON)
  agent history [--limit N] [--status S]    your recent runs joined to their goals
                                             [--agent ID]                 (JSON; default agent = AGENTWORK_AGENT_ID)
  agent delete <name>                        delete an agent (comma-separated batch)
  agent update --name <name> [--description D] [--system_prompt P] [--runtime R]
                                             update an agent's persona-level fields
  squad list                                 list all squads (JSON)
  squad detail <name>                        show squad detail (JSON)
  squad delete <name>                        delete a squad (comma-separated batch)
  squad update --squad <name> [--leader L] [--description D] [--instructions I]
  squad add-member --squad <name> --agent <id>       add member(s) to a squad
  squad remove-member --squad <name> --agent <id>    remove member(s) from a squad
  schedule list                              list all schedules (JSON)
  schedule stop <name>                       stop (disable) a schedule
  schedule enable <name>                     re-enable a stopped schedule
  schedule delete <name>                     delete a schedule (comma-separated batch)
  domain list                                list all domains (JSON)
  domain delete <name>                       delete a domain (comma-separated batch)
  skill list                                 list all skills (JSON)
  skill delete <name>                        delete a skill (comma-separated batch)
  subgoal list|get <id>|create --title T --assignee A [--description D] [--verifier V]
                                             list/create/read work items (the owner splits)
  subgoal cancel <id>                        cancel a work item
  subgoal verify <id> --verdict passed|rejected [--summary S] [--evidence E]
                                             the verifier's verdict
  subgoal verifications <id>                 a work item's verification rounds
  change list                                the goal's changes (ready/integrating/…)
  change integrate <id>                      merge a change into THIS worktree locally;
                                             conflict → the assignee is woken to rework
  stats                                      goal/run status statistics (JSON): goal totals
                                             + counts per status (backlog/active/blocked/done/
                                             failed/cancelled) and run totals + counts per status
                                             (queued/running/completed/failed/cancelled)
  issue comment --text T                     reply to the issue/PR behind the current goal
                                             (the platform owns the token and routes by source
                                             kind — a PR goal's comment lands on the PR; only
                                             for source-tracked goals, M4-B)

  version                                    print the CLI build version and exit

Environment (injected by daemon):
  AGENTWORK_SERVER_URL   server base URL (default http://127.0.0.1:7373)
  AGENTWORK_GOAL_ID      current goal id (product plane)
  AGENTWORK_RUN_ID       current run id (execution plane)
  AGENTWORK_AGENT_ID     current agent id`)
}

// ── goal ──

func goalCmd(goalID, agentID string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli goal <list|status|assign|cancel|reopen|delete|create|comment|comments|wait>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		goalList(args[1:])
	case "status":
		goalStatus(args[1:])
	case "assign":
		goalAssign(goalID, args[1:])
	case "cancel":
		goalCancel(args[1:])
	case "reopen":
		goalReopen(args[1:])
	case "delete":
		goalDelete(args[1:])
	case "create":
		goalCreate(goalID, agentID, args[1:])
	case "comment":
		goalComment(goalID, args[1:])
	case "comments":
		goalComments(goalID, args[1:])
	case "wait":
		goalWait(goalID, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown goal subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// goalList implements `goal list [--limit N] [--status S] [--json]`. --limit
// truncates to the N most recent goals; absent (or 0) means all. --status
// filters to goals whose status exactly matches S. The server's /goals
// endpoint only supports ?limit, so the status filter is applied
// client-side, and --limit then truncates the filtered list to the N most
// recent matches. Output is JSON — the CLI's native format (agents parse
// stdout), so it is also the default; --json is the explicit selector for
// that format.
func goalList(args []string) {
	fs := flag.NewFlagSet("goal list", flag.ExitOnError)
	limit := fs.Int("limit", 0, "max number of goals to return (0 = all)")
	status := fs.String("status", "", "only list goals with this status (exact match)")
	jsonOut := fs.Bool("json", false, "output goals as JSON (the default output format)")
	fs.Parse(args)
	// JSON is both the default and the only format; --json pins it
	// explicitly for scripted callers. GET /goals always responds with
	// JSON, so the body is streamed through unchanged.
	_ = jsonOut
	if *status == "" {
		get(goalListURL(*limit))
		return
	}
	var goals []json.RawMessage
	if err := getJSON(goalListURL(0), &goals); err != nil {
		fail("%v", err)
	}
	out := filterGoalsByStatus(goals, *status)
	if *limit > 0 && len(out) > *limit {
		out = out[:*limit]
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fail("encode: %v", err)
	}
}

// filterGoalsByStatus returns the goals whose status equals want, preserving
// order. The status is probed via a minimal unmarshal so each goal object
// passes through byte-for-byte (no field loss, no key reordering).
func filterGoalsByStatus(goals []json.RawMessage, want string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(goals))
	for _, g := range goals {
		var probe struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(g, &probe); err != nil {
			continue // malformed goal object: skip
		}
		if probe.Status == want {
			out = append(out, g)
		}
	}
	return out
}

// goalListURL builds the GET /goals URL, appending ?limit=N when N > 0.
func goalListURL(limit int) string {
	url := serverURL() + "/goals"
	if limit > 0 {
		url += fmt.Sprintf("?limit=%d", limit)
	}
	return url
}

// goalAssign hands a goal off to another agent or squad. Two paths:
//   - Run path (no --goal flag): uses AGENTWORK_GOAL_ID env, goes through /rpc
//     with the per-run token (the daemon resolves actor=agent + owner-check).
//   - Chat path (--goal flag): goes through POST /intake/dispatch →
//     intakeAssignGoal handler (actor=human). The steward
//     in chat has no run token, so /rpc is not an option.
func goalAssign(goalID string, args []string) {
	fs := flag.NewFlagSet("goal assign", flag.ExitOnError)
	note := fs.String("note", "", "handoff note for the next agent")
	goalIDFlag := fs.String("goal", "", "goal id (chat path; run path uses AGENTWORK_GOAL_ID)")
	toAgent := fs.String("to", "", "assignee agent or squad id (required)")
	assigneeType := fs.String("assignee_type", "", "agent (default) | squad")
	fs.Parse(args)
	if *toAgent == "" {
		fail("usage: agentwork-cli goal assign --to <id> [--note N] [--goal G] [--assignee_type T]")
	}
	at := *assigneeType
	if at == "" {
		at = "agent"
	}
	if *goalIDFlag != "" {
		post(serverURL()+"/intake/dispatch", map[string]any{
			"intent":  "goal_assign",
			"goal_id": *goalIDFlag,
			"goal":    map[string]any{"assignee_id": *toAgent, "assignee_type": at, "description": *note},
		})
		return
	}
	if goalID == "" {
		fail("AGENTWORK_GOAL_ID not set — pass --goal for chat path")
	}
	var out map[string]any
	if err := rpcCall(link.MethodGoalAssign, link.GoalAssignParams{
		RPCToken:     rpcToken(),
		AssigneeType: at,
		AssigneeID:   *toAgent,
		HandoffNote:  *note,
	}, &out); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(out)
}

func goalCreate(goalID, agentID string, args []string) {
	fs := flag.NewFlagSet("goal create", flag.ExitOnError)
	title := fs.String("title", "", "goal title (required)")
	description := fs.String("description", "", "goal description (the work to do)")
	assignee := fs.String("assignee", "", "assignee agent id (defaults to current agent)")
	// Sub-goals are SLEEVED (DESIGN.md 决策 3-6): creation no longer
	// defaults to a child of the current goal — an agent-created goal is an
	// independent item, not a fan-out child (a defaulted parent used to make
	// every agent-created goal a sub-goal that blocks its parent).
	parent := fs.String("parent", "", "parent goal id (explicit sub-goal; NOT defaulted)")
	status := fs.String("status", "active", "goal status")
	fs.Parse(args)
	if *title == "" {
		fail("--title is required")
	}
	if *assignee == "" {
		*assignee = agentID
	}
	if *assignee == "" {
		fail("--assignee is required (or AGENTWORK_AGENT_ID must be set)")
	}
	body := map[string]string{
		"title":           *title,
		"description":     *description,
		"assignee_type":   "agent",
		"assignee_id":     *assignee,
		"parent_id":       *parent,
		"status":          *status,
		"created_by_type": "agent",
		"created_by_id":   agentID,
	}
	post(serverURL()+"/goals", body)
}

// goalComment posts a comment on the run's goal via /rpc — the per-run
// token (env AGENTWORK_TOKEN) is the identity; the daemon resolves it to
// the run's goal and agent.
func goalComment(goalID string, args []string) {
	fs := flag.NewFlagSet("goal comment", flag.ExitOnError)
	text := fs.String("text", "", "comment text (required; may contain a structured mention)")
	parent := fs.String("parent", "", "parent comment id (optional — replies thread under it)")
	ask := fs.Bool("ask", false, "this comment is a question to the human (goal creator) — the platform notifies them and their reply wakes you (决策 7-3)")
	fs.Parse(args)
	if *text == "" {
		fail("--text is required")
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := rpcCall(link.MethodGoalComment, link.GoalCommentParams{
		RPCToken: rpcToken(),
		Text:     *text,
		ParentID: *parent,
		AskHuman: *ask,
	}, &res); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(res)
}

// goalComments pulls the run's goal comment feed via /rpc — the shared
// context. --after reads incrementally from the last seen comment id.
func goalComments(goalID string, args []string) {
	fs := flag.NewFlagSet("goal comments", flag.ExitOnError)
	after := fs.String("after", "", "only comments after this id (incremental read)")
	limit := fs.Int("limit", 50, "max comments to return")
	fs.Parse(args)
	var out []map[string]any
	if err := rpcCall(link.MethodGoalComments, link.GoalCommentsParams{
		RPCToken: rpcToken(),
		After:    *after,
		Limit:    *limit,
	}, &out); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(out)
}

// goalWait parks until the goal's sub-goals settle (or the server-side
// timeout) via /rpc, then prints their states.
func goalWait(goalID string, args []string) {
	var states []map[string]any
	if err := rpcCall(link.MethodGoalWait, link.GoalWaitParams{RPCToken: rpcToken()}, &states); err != nil {
		fail("%v", err)
	}
	rpcPrintJSON(states)
}

// goalStatus queries a goal's status and last run outcome via dispatch.
func goalStatus(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli goal status <id>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent":  "goal_status",
		"goal_id": args[0],
	})
}

// goalCancel cancels a goal via dispatch.
func goalCancel(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli goal cancel <id>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent":  "goal_cancel",
		"goal_id": args[0],
	})
}

// goalReopen reopens a finished/cancelled goal via dispatch.
func goalReopen(args []string) {
	fs := flag.NewFlagSet("goal reopen", flag.ExitOnError)
	goalID := fs.String("goal", "", "goal id (required)")
	reason := fs.String("reason", "", "reopen reason")
	fs.Parse(args)
	if *goalID == "" {
		fail("usage: agentwork-cli goal reopen --goal <id> [--reason R]")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent":  "goal_reopen",
		"goal_id": *goalID,
		"goal":    map[string]any{"description": *reason},
	})
}

// goalDelete deletes a goal (supports comma-separated batch) via dispatch.
func goalDelete(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli goal delete <id>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent":  "goal_delete",
		"goal_id": args[0],
	})
}

// ── agent / squad ──

func agentCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli agent <list|history|delete|update>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL() + "/agents")
	case "history":
		agentHistory(args[1:])
	case "delete":
		agentDelete(args[1:])
	case "update":
		agentUpdate(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown agent subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// agentHistory implements `agent history [--limit N] [--status S] [--agent ID]`.
// It returns the calling agent's recent runs joined to their goals — the chat
// surface's "what have I done" view. The agent id defaults to
// AGENTWORK_AGENT_ID (set by the executor at spawn, both run and chat paths);
// --agent overrides it for manual invocation. Output is JSON (agents parse
// stdout), matching `goal list`.
func agentHistory(args []string) {
	fs := flag.NewFlagSet("agent history", flag.ExitOnError)
	limit := fs.Int("limit", 20, "max number of runs to return")
	status := fs.String("status", "", "only runs with this status (exact match)")
	agentID := fs.String("agent", "", "agent id (default: AGENTWORK_AGENT_ID)")
	fs.Parse(args)
	id := *agentID
	if id == "" {
		id = os.Getenv("AGENTWORK_AGENT_ID")
	}
	if id == "" {
		fail("agent id is required: pass --agent or set AGENTWORK_AGENT_ID")
	}
	u := serverURL() + "/agents/" + id + "/history?limit=" + strconv.Itoa(*limit)
	if *status != "" {
		u += "&status=" + url.QueryEscape(*status)
	}
	get(u)
}

// agentDelete deletes an agent (supports comma-separated batch) via dispatch.
func agentDelete(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli agent delete <name>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent": "agent_delete",
		"agent":  map[string]any{"name": args[0]},
	})
}

// agentUpdate updates an agent's persona-level fields via dispatch.
func agentUpdate(args []string) {
	fs := flag.NewFlagSet("agent update", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	description := fs.String("description", "", "new description")
	systemPrompt := fs.String("system_prompt", "", "new system prompt / persona")
	runtime := fs.String("runtime", "", "new runtime id or name")
	fs.Parse(args)
	if *name == "" {
		fail("usage: agentwork-cli agent update --name <name> [--description D] [--system_prompt P] [--runtime R]")
	}
	agentFields := map[string]any{"name": *name}
	if *description != "" {
		agentFields["description"] = *description
	}
	if *systemPrompt != "" {
		agentFields["system_prompt"] = *systemPrompt
	}
	if *runtime != "" {
		agentFields["runtime_id"] = *runtime
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent": "agent_update",
		"agent":  agentFields,
	})
}

func squadCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli squad <list|detail|delete|update|add-member|remove-member>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL() + "/squads")
	case "detail":
		squadDetail(args[1:])
	case "delete":
		squadDelete(args[1:])
	case "update":
		squadUpdate(args[1:])
	case "add-member":
		squadMember(true, args[1:])
	case "remove-member":
		squadMember(false, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown squad subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// squadDetail queries a squad's detail via dispatch.
func squadDetail(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli squad detail <name>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent": "squad_detail",
		"squad":  map[string]any{"name": args[0]},
	})
}

// squadDelete deletes a squad (supports comma-separated batch) via dispatch.
func squadDelete(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli squad delete <name>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent": "squad_delete",
		"squad":  map[string]any{"name": args[0]},
	})
}

// squadUpdate updates a squad's leader/description/instructions via dispatch.
func squadUpdate(args []string) {
	fs := flag.NewFlagSet("squad update", flag.ExitOnError)
	squadName := fs.String("squad", "", "squad name (required)")
	leader := fs.String("leader", "", "new leader agent id or name")
	description := fs.String("description", "", "new description")
	instructions := fs.String("instructions", "", "new collaboration instructions")
	fs.Parse(args)
	if *squadName == "" {
		fail("usage: agentwork-cli squad update --squad <name> [--leader L] [--description D] [--instructions I]")
	}
	squadFields := map[string]any{"name": *squadName}
	if *leader != "" {
		squadFields["leader_id"] = *leader
	}
	if *description != "" {
		squadFields["description"] = *description
	}
	if *instructions != "" {
		squadFields["instructions"] = *instructions
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent": "squad_update",
		"squad":  squadFields,
	})
}

// squadMember adds or removes members from a squad via dispatch.
// --agent accepts comma-separated ids for batch operation.
func squadMember(add bool, args []string) {
	fs := flag.NewFlagSet("squad member", flag.ExitOnError)
	squadName := fs.String("squad", "", "squad name (required)")
	agentFlag := fs.String("agent", "", "agent id(s), comma-separated (required)")
	fs.Parse(args)
	action := "add-member"
	if !add {
		action = "remove-member"
	}
	if *squadName == "" || *agentFlag == "" {
		fail("usage: agentwork-cli squad %s --squad <name> --agent <id>", action)
	}
	var memberIDs []string
	for _, s := range strings.Split(*agentFlag, ",") {
		if s = strings.TrimSpace(s); s != "" {
			memberIDs = append(memberIDs, s)
		}
	}
	intent := "squad_add_member"
	if !add {
		intent = "squad_remove_member"
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent": intent,
		"squad":  map[string]any{"name": *squadName, "member_ids": memberIDs},
	})
}

// ── schedule / domain / skill ──

func scheduleCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli schedule <list|stop|enable|delete>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL() + "/schedules")
	case "stop":
		scheduleToggle("schedule_stop", args[1:])
	case "enable":
		scheduleToggle("schedule_enable", args[1:])
	case "delete":
		scheduleDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown schedule subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func scheduleToggle(intent string, args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli schedule <stop|enable> <name>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent":   intent,
		"schedule": map[string]any{"name": args[0]},
	})
}

func scheduleDelete(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli schedule delete <name>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent":   "schedule_delete",
		"schedule": map[string]any{"name": args[0]},
	})
}

func domainCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli domain <list|delete>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL() + "/domains")
	case "delete":
		domainDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown domain subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func domainDelete(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli domain delete <name>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent": "domain_delete",
		"domain": map[string]any{"name": args[0]},
	})
}

func skillCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork-cli skill <list|delete>")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		get(serverURL() + "/skills")
	case "delete":
		skillDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown skill subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func skillDelete(args []string) {
	if len(args) < 1 {
		fail("usage: agentwork-cli skill delete <name>")
	}
	post(serverURL()+"/intake/dispatch", map[string]any{
		"intent": "skill_delete",
		"skill":  map[string]any{"name": args[0]},
	})
}

// ── stats ──

// cliGoal is the minimal goal shape `stats` needs from GET /goals.
type cliGoal struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// cliRun is the minimal run shape `stats` needs from GET /goals/{id}/runs.
type cliRun struct {
	Status string `json:"status"`
}

// knownGoalStatuses / knownRunStatuses are the status dimensions `stats`
// reports. Every known status is always present in the output (zero-filled),
// so the JSON is self-describing; unknown statuses the server may return are
// still counted under their own key.
var (
	knownGoalStatuses = []string{"backlog", "active", "blocked", "done", "failed", "cancelled"}
	knownRunStatuses  = []string{"queued", "running", "completed", "failed", "cancelled"}
)

// statusBucket is a total plus per-status counts for one dimension.
type statusBucket struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
}

// statsOutput is the JSON shape emitted by `stats`.
type statsOutput struct {
	Goals statusBucket `json:"goals"`
	Runs  statusBucket `json:"runs"`
}

// newStatusBucket initializes a bucket with zero counts for every known status.
func newStatusBucket(known []string) statusBucket {
	byStatus := make(map[string]int, len(known))
	for _, s := range known {
		byStatus[s] = 0
	}
	return statusBucket{ByStatus: byStatus}
}

// bucketGoals tallies a goal list into a statusBucket.
func bucketGoals(goals []cliGoal) statusBucket {
	b := newStatusBucket(knownGoalStatuses)
	b.Total = len(goals)
	for _, g := range goals {
		b.ByStatus[g.Status]++
	}
	return b
}

// bucketRuns tallies a run list into a statusBucket.
func bucketRuns(runs []cliRun) statusBucket {
	b := newStatusBucket(knownRunStatuses)
	b.Total = len(runs)
	for _, r := range runs {
		b.ByStatus[r.Status]++
	}
	return b
}

// runsListURL builds the GET /goals/{id}/runs URL.
func runsListURL(goalID string) string {
	return serverURL() + "/goals/" + goalID + "/runs"
}

// statsCmd implements `stats`: goal stats come from GET /goals; run stats are
// aggregated by fanning out to GET /goals/{id}/runs for every goal and
// summing the per-status counts. Output is a single JSON object, matching the
// JSON output style of the other commands.
func statsCmd(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	fs.Parse(args)

	var goals []cliGoal
	if err := getJSON(goalListURL(0), &goals); err != nil {
		fail("%v", err)
	}

	var allRuns []cliRun
	for _, g := range goals {
		var runs []cliRun
		if err := getJSON(runsListURL(g.ID), &runs); err != nil {
			fail("%v", err)
		}
		allRuns = append(allRuns, runs...)
	}

	out := statsOutput{Goals: bucketGoals(goals), Runs: bucketRuns(allRuns)}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fail("encode stats: %v", err)
	}
}

// ── HTTP helpers ──

func get(url string) {
	resp, err := httpClient.Get(url)
	if err != nil {
		fail("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		fail("GET %s: HTTP %d: %s", url, resp.StatusCode, body)
	}
	io.Copy(os.Stdout, resp.Body)
}

// getJSON performs GET and decodes the JSON body into v. Mirrors get's
// failure mode: transport errors and non-2xx responses are returned as
// errors (the caller decides how to surface them).
func getJSON(url string, v any) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("GET %s: decode: %v", url, err)
	}
	return nil
}

func post(url string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		fail("marshal: %v", err)
	}
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		fail("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		fail("POST %s: HTTP %d: %s", url, resp.StatusCode, rb)
	}
	io.Copy(os.Stdout, resp.Body)
}

func postNoBody(url string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		fail("marshal: %v", err)
	}
	resp, err := httpClient.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		fail("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		fail("POST %s: HTTP %d: %s", url, resp.StatusCode, rb)
	}
}

// createCmd implements `agentwork create <kind> [flags]` — the chat-as-tool
// entry point (决策7-5). The steward agent calls this to create platform
// entities with draft/merge ask-once support. It parses kind-specific flags
// into a full intakeAction JSON (intent + sub-struct) and POSTs it to
// /intake/dispatch — the daemon's DispatchIntake is a thin pass-through to
// intakeReg.dispatch, so no per-kind mapping on the daemon side.
//
// This is DISTINCT from `agentwork goal create` (which POSTs /goals directly
// — used by run-path agents creating sub-goals with all fields known).
func createCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentwork create <goal|agent|squad|domain|schedule|team> [flags]")
		os.Exit(2)
	}
	kind := args[0]
	body := buildCreateAction(kind, args[1:])
	post(serverURL()+"/intake/dispatch", body)
}

// intentForKind maps a CLI kind to the intakeReg intent string. This is the
// ONLY place that knows the mapping — the daemon side is mapping-free.
func intentForKind(kind string) (string, bool) {
	switch kind {
	case "goal":
		return "create_goal", true
	case "agent":
		return "create_agent", true
	case "squad":
		return "create_squad", true
	case "domain":
		return "domain_create", true
	case "schedule":
		return "create_schedule", true
	case "team":
		return "import_team", true
	default:
		return "", false
	}
}

// buildCreateAction parses kind-specific flags into a full intakeAction JSON
// (intent + the matching sub-struct). The daemon unmarshals this directly.
func buildCreateAction(kind string, args []string) map[string]any {
	intent, ok := intentForKind(kind)
	if !ok {
		fail("unknown create kind: %s (valid: goal, agent, squad, domain, schedule, team)", kind)
	}
	fs := flag.NewFlagSet("create "+kind, flag.ExitOnError)
	var s1, s2, s3, s4, s5, s6, s7 string

	switch kind {
	case "goal":
		fs.StringVar(&s1, "title", "", "goal title (required)")
		fs.StringVar(&s2, "description", "", "goal description")
		fs.StringVar(&s3, "assignee", "", "assignee agent/squad id or name")
		fs.StringVar(&s4, "assignee_type", "", "agent (default) | squad")
		fs.StringVar(&s5, "domain", "", "domain id or name (required)")
		fs.Parse(args)
		return map[string]any{"intent": intent, "goal": map[string]any{"title": s1, "description": s2, "assignee_id": s3, "assignee_type": s4, "domain_id": s5}}
	case "agent":
		fs.StringVar(&s1, "name", "", "agent name (required)")
		fs.StringVar(&s2, "runtime", "", "runtime id or name (required)")
		fs.StringVar(&s3, "description", "", "agent description")
		fs.StringVar(&s4, "system_prompt", "", "agent persona/system prompt")
		fs.StringVar(&s5, "skills", "", "comma-separated skill ids (optional; pass 'none' to decline)")
		fs.Parse(args)
		agentFields := map[string]any{"name": s1, "runtime_id": s2, "description": s3, "system_prompt": s4}
		if s5 != "" {
			if strings.TrimSpace(s5) == "none" {
				agentFields["skills"] = []string{}
				agentFields["skills_specified"] = true
			} else {
				var skills []string
				for _, s := range strings.Split(s5, ",") {
					if s = strings.TrimSpace(s); s != "" {
						skills = append(skills, s)
					}
				}
				agentFields["skills"] = skills
				agentFields["skills_specified"] = true
			}
		}
		return map[string]any{"intent": intent, "agent": agentFields}
	case "squad":
		fs.StringVar(&s1, "name", "", "squad name (required)")
		fs.StringVar(&s2, "leader", "", "leader agent id or name (required)")
		fs.StringVar(&s3, "description", "", "squad description")
		fs.StringVar(&s4, "instructions", "", "squad collaboration instructions")
		fs.Parse(args)
		return map[string]any{"intent": intent, "squad": map[string]any{"name": s1, "leader_id": s2, "description": s3, "instructions": s4}}
	case "domain":
		fs.StringVar(&s1, "name", "", "domain name (required)")
		fs.StringVar(&s2, "type", "", "repo (default) | scratch")
		fs.StringVar(&s3, "git_url", "", "git repo URL (required for repo type)")
		fs.StringVar(&s4, "default_branch", "", "default branch (default: main)")
		fs.Parse(args)
		return map[string]any{"intent": intent, "domain": map[string]any{"name": s1, "type": s2, "git_url": s3, "default_branch": s4}}
	case "schedule":
		fs.StringVar(&s1, "name", "", "schedule name (required)")
		fs.StringVar(&s2, "title", "", "goal title template per trigger (required)")
		fs.StringVar(&s3, "description", "", "schedule description")
		fs.StringVar(&s4, "cron", "", "cron expression (required)")
		fs.StringVar(&s5, "assignee", "", "assignee agent/squad id or name")
		fs.StringVar(&s6, "assignee_type", "", "agent (default) | squad")
		fs.StringVar(&s7, "domain", "", "domain id or name (required)")
		fs.Parse(args)
		return map[string]any{"intent": intent, "schedule": map[string]any{"name": s1, "title": s2, "description": s3, "cron": s4, "assignee_id": s5, "assignee_type": s6, "domain_id": s7}}
	case "team":
		fs.StringVar(&s1, "url", "", "git repo URL (required)")
		fs.StringVar(&s2, "branch", "", "branch (default: repo default)")
		fs.StringVar(&s3, "credentials", "", "git credentials (private repos)")
		fs.Parse(args)
		return map[string]any{"intent": intent, "import_team": map[string]any{"git_url": s1, "branch": s2, "credentials": s3}}
	default:
		fail("unknown create kind: %s (valid: goal, agent, squad, domain, schedule, team)", kind)
	}
	return nil
}

// versionCmd prints just the build version (e.g. "v0.0.2") on stdout — the
// machine-capture form (VER=$(agentwork version)). The binary name is dropped
// so the output is the version alone, matching `git --version`/`go version`
// convention (stdout, not stderr).
func versionCmd() {
	fmt.Printf("v%s\n", cliVersion)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agentwork-cli: "+format+"\n", args...)
	os.Exit(1)
}
