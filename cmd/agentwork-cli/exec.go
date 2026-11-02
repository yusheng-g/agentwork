package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eushing/agentwork/internal/link"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/proto/acpbackend"
	"github.com/eushing/agentwork/internal/runtime"
)

// executor is the machine's run engine (CLI 分支 Phase 2, PULL model):
// the run.poll response carries the dispatch; execute spawns the ACP
// runtime with the daemon-assembled prompt, streams events back (per-run
// seq, 100ms batches), and reports the terminal state via run.finished.
// The machine NEVER queries the platform — the dispatch payload is a
// complete readonly snapshot.
type executor struct {
	peer *link.Peer
	// serverURL is the platform address THIS machine dialed (connect
	// --server / default) — the same address the agent's CLI callbacks use.
	serverURL string

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc // runID → turn cancel
	sessions map[string]string             // sessionID → the in-flight runID holding it (resume mutual exclusion)
}

func newExecutor(peer *link.Peer, serverURL string) *executor {
	return &executor{peer: peer, serverURL: serverURL, cancels: map[string]context.CancelFunc{}, sessions: map[string]string{}}
}

// reprobeAfterSpawnFailure re-probes the machine and pushes a fresh probe
// report after a spawn failure — an uninstalled CLI should be marked
// absent immediately, not at the next 1-minute probe tick.
func (e *executor) reprobeAfterSpawnFailure() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		st, err := loadState()
		if err != nil || st.MachineID == "" {
			return
		}
		_ = e.peer.Notify(ctx, link.MethodMachineProbeUpdate, link.ProbeUpdateParams{
			MachineID: st.MachineID,
			CLIs:      probeCLIs(ctx, nil),
		})
	}()
}

// shutdown cancels every in-flight run (the link died — the platform will
// reclaim them via RecoverStuckRunning).
func (e *executor) shutdown() {
	e.mu.Lock()
	n := len(e.cancels)
	for _, cancel := range e.cancels {
		cancel()
	}
	e.cancels = map[string]context.CancelFunc{}
	e.mu.Unlock()
	if n > 0 {
		cliLogf("exec: link died — cancelling %d in-flight run(s)", n)
	}
}

// startIfNew launches a polled dispatch unless the run is already in
// flight — the poll delivery is at-least-once (a lost response makes the
// daemon re-serve the same dispatch) and the executor is the dedupe.
func (e *executor) startIfNew(p link.RunDispatchParams) {
	if p.RunID == "" || p.Prompt == "" || len(p.ACPSpawn) == 0 {
		cliLogf("exec: malformed dispatch dropped (run_id=%q)", p.RunID)
		return
	}
	e.mu.Lock()
	_, running := e.cancels[p.RunID]
	e.mu.Unlock()
	if running {
		return // already in flight — duplicate serve from a lost response
	}
	cliLogf("exec: run %s dispatch received (role=%s attempt=%d agent=%s proc=%v)", p.RunID, p.Role, p.Attempt, p.AgentID, p.Proc)
	go e.execute(p)
}

// cancelRun implements run.cancel — delivered in the poll response.
func (e *executor) cancelRun(p link.RunCancelParams) {
	cliLogf("exec: run %s cancel received (reason=%s)", p.RunID, p.Reason)
	e.mu.Lock()
	cancel := e.cancels[p.RunID]
	delete(e.cancels, p.RunID)
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// execute runs one dispatched run to completion.
func (e *executor) execute(p link.RunDispatchParams) {
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancels[p.RunID] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.cancels, p.RunID)
		for sid, rid := range e.sessions {
			if rid == p.RunID {
				delete(e.sessions, sid)
			}
		}
		e.mu.Unlock()
		cancel()
	}()

	// Workdir: processor runs get the proc dir (compile-on-repo = a
	// detached worktree of origin/<default>); scratch domains derive the
	// local directory layout (mirrors the daemon's); git domains get a
	// worktree on the goal branch — and the completed run's changes are
	// committed + pushed as agentwork/<branch> (中转, Phase 3).
	var workdir, repo, branch, sessionID string
	var cleanupGit func()
	if p.Proc {
		workdir = machineProcWorkdir(p.RunID)
		if p.Scratch {
			if err := os.MkdirAll(workdir, 0o755); err != nil {
				e.finish(p, "", "", "failed", fmt.Sprintf("workdir: %v", err))
				return
			}
		} else {
			var err error
			repo, err = ensureBareRepo(ctx, p)
			if err != nil {
				e.finish(p, "", "", "failed", fmt.Sprintf("git: %v", err))
				return
			}
			def := p.DefaultBranch
			if def == "" {
				def = "main"
			}
			if out, err := runGit(ctx, repo, "worktree", "add", "--force", "--detach", workdir, "origin/"+def); err != nil {
				e.finish(p, "", "", "failed", fmt.Sprintf("worktree add: %v: %s", err, out))
				return
			}
			defer func() {
				_, _ = runGit(context.Background(), repo, "worktree", "remove", "--force", workdir)
			}()
		}
		// The artifact's ABSOLUTE paths — the proc dir is opaque to the
		// agent (it once guessed a path and the artifact never landed).
		if len(p.ArtifactFiles) > 0 {
			p.Prompt += fmt.Sprintf("\n\nThe artifact files' ABSOLUTE directory: %s\n(Write %s there with your file tools; do NOT guess the working directory.)\n",
				workdir, strings.Join(p.ArtifactFiles, ", "))
		}
	} else if p.Scratch {
		var err error
		workdir, err = scratchWorkdir(p)
		if err != nil {
			e.finish(p, "", "", "failed", fmt.Sprintf("workdir: %v", err))
			return
		}
	} else {
		var err error
		workdir, repo, branch, cleanupGit, err = ensureGitWorkdir(ctx, p)
		if err != nil {
			e.finish(p, "", "", "failed", fmt.Sprintf("git: %v", err))
			return
		}
		// The cleanup runs AFTER the finish report — a hang here (repo lock
		// contention with the daemon's completion worktrees) stalls the
		// sidecar's hottest window. Probe it: the timeout keeps the process
		// responsive and the log line tells us where the stall was.
		defer func() {
			done := make(chan struct{})
			go func() { cleanupGit(); close(done) }()
			select {
			case <-done:
				cliLogf("exec: run %s git cleanup done", p.RunID)
			case <-time.After(60 * time.Second):
				cliLogf("exec: run %s git cleanup TIMED OUT after 60s (worktree left for the daemon's sweeps)", p.RunID)
			}
		}()
	}

	// The agent's persona (pushed via config.push) rides AGENTS.md in the
	// workdir — the runtime loads it natively. The run's CONTEXT layer
	// (platform background, goal, team, role contract, tool surface —
	// shipped in the dispatch) is persona material too and is merged into
	// the same file; the prompt carries the task only.
	pushedProfile := ""
	if !p.Proc {
		pushedProfile = syncAgentProfile(p.AgentID, workdir)
		if p.RunProfile != "" {
			// Merge RunProfile into the workdir's AGENTS.md. If the agent
			// had no pushed system prompt, syncAgentProfile wrote nothing
			// and the file may not exist yet — in that case CREATE it with
			// RunProfile alone (the squad instructions / team playbook ride
			// here when the leader agent has no persona of its own).
			// Previously a missing file silently dropped RunProfile, losing
			// the squad instructions entirely.
			agentsMD := filepath.Join(workdir, "AGENTS.md")
			if b, err := os.ReadFile(agentsMD); err == nil {
				pushedProfile = string(b) + "\n\n" + p.RunProfile
				cliLogf("exec: run %s RunProfile merged (%d bytes) into existing AGENTS.md → %s", p.RunID, len(p.RunProfile), agentsMD)
			} else {
				pushedProfile = p.RunProfile
				cliLogf("exec: run %s RunProfile created AGENTS.md (%d bytes, no agent persona) → %s", p.RunID, len(p.RunProfile), agentsMD)
			}
			_ = os.WriteFile(agentsMD, []byte(pushedProfile), 0o644)
		}
		// Skills ride the workdir too (project-level): staged from the
		// pushed dir under their ORIGINAL names — the user's own global
		// skills are never touched. Scratch workdirs are platform-owned
		// and wiped first so deselected skills disappear; repo workdirs
		// are overwrite-only (the repo may have its own files there).
		// The dir comes from the machine's probe report (dispatch);
		// '' = legacy daemon — fall back to the local CLI mapping.
		subdir := p.ProjectSkillsDir
		if subdir == "" {
			subdir = skillSubdir(p.ACPSpawn[0])
		}
		copyAgentSkills(p.AgentID, workdir, subdir, p.Scratch)
	}

	// openSession spawns the CLI and opens its ACP session (used for the
	// initial open AND the post-resume fallback).
	openSession := func() (proto.Session, error) {
		conn, err := runtime.Open(ctx, runtime.Spec{
			Transport:  "stdio",
			Executable: p.ACPSpawn[0],
			Args:       p.ACPSpawn[1:],
			Env:        p.Env,
			Cwd:        workdir,
		}, e.buildEnv(p, workdir))
		if err != nil {
			return nil, fmt.Errorf("runtime: %w", err)
		}
		backend := &acpbackend.Backend{}
		// autopermit answers the CLI's permission requests (allow the
		// strongest option) and implements the delegated fs/terminal RPCs
		// — the machine runs autonomously, nobody clicks "allow", and the
		// platform judges the OUTCOME, not individual tool calls (live:
		// an unhandled permission request came back as "The user rejected
		// permission" and killed the agent's work).
		sess, err := backend.OpenSession(ctx, proto.SessionSpec{
			Conn:          conn,
			Cwd:           workdir,
			ClientHandler: newAutopermit(workdir),
			McpServers:    p.McpServers,
		})
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("session: %w", err)
		}
		return sess, nil
	}

	sess, err := openSession()
	if err != nil {
		// The spawn failed — likely an uninstalled CLI. Re-probe and push
		// a fresh report so the platform marks the runtime absent and
		// stops dispatching to it (multica's runtime_not_found recovery,
		// without waiting for the periodic probe).
		e.reprobeAfterSpawnFailure()
		e.finish(p, "", "", "failed", err.Error())
		return
	}
	sessionID = sess.SessionID()

	// Session resume (multica-style): a prior session id resolves ONLY in
	// the exact workdir it was recorded against — agent CLI session stores
	// are keyed by cwd. Mismatch: drop the pointer and say so in the
	// prompt (never silently restart). Load failure: the CLI's fresh
	// process cannot resolve the id — continue on the fresh session; the
	// cwd-keyed project store still gives partial continuity.
	resumed := false
	if p.PriorSessionID != "" && !p.Proc {
		if p.PriorWorkDir == workdir {
			// Mutual exclusion: one session, one in-flight run. A prior
			// run that the daemon expired while its exec kept running
			// still HOLDS the session — resuming it collides two runs on
			// one conversation (live: the collision's killed turn left a
			// poisoned history).
			e.mu.Lock()
			_, held := e.sessions[p.PriorSessionID]
			e.mu.Unlock()
			if held {
				cliLogf("exec: run %s prior session %s still in flight — continuing fresh", p.RunID, shortSHA(p.PriorSessionID))
			} else if err := sess.LoadSession(ctx, p.PriorSessionID); err != nil {
				cliLogf("exec: run %s session/load %s failed: %v — continuing fresh", p.RunID, shortSHA(p.PriorSessionID), err)
			} else {
				resumed = true
				e.mu.Lock()
				e.sessions[p.PriorSessionID] = p.RunID
				e.mu.Unlock()
				cliLogf("exec: run %s session resumed (%s)", p.RunID, shortSHA(p.PriorSessionID))
			}
		} else {
			p.Prompt += "\n\nYour previous session could not be resumed: the working directory for this turn differs from the recorded one — you are starting fresh in this workspace."
			cliLogf("exec: run %s prior session dropped (workdir %s != prior %s)", p.RunID, workdir, p.PriorWorkDir)
		}
	}

	// Probe like the git cleanup: a session close that hangs (child process
	// refusing to die) must not stall the sidecar.
	defer func() {
		done := make(chan struct{})
		go func() { sess.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			cliLogf("exec: run %s session close TIMED OUT after 30s", p.RunID)
		}
	}()

	// Ack the daemon: execution really started (run.claimed — the review
	// window's "审查中" flip hangs on it).
	if err := callRPC(e.peer, link.MethodRunClaimed, link.RunClaimedParams{RunID: p.RunID}, nil, 15*time.Second); err != nil {
		cliLogf("exec: run %s claimed ack failed: %v", p.RunID, err)
	}

	run, err := sess.Prompt(ctx, p.Prompt)
	if err != nil && resumed {
		// A resumed session can be POISONED by a killed turn: the CLI's
		// persisted history carries an empty assistant message and EVERY
		// future prompt on the session fails (live: "message at position
		// 26 with role 'assistant' must not be empty" burned the attempt).
		// Fall back to a fresh session ONCE — the workdir continuity
		// survives, only the conversation memory is lost.
		cliLogf("exec: run %s prompt failed on resumed session (%v) — reopening fresh", p.RunID, err)
		sess.Close()
		sess, err = openSession()
		if err != nil {
			e.finish(p, workdir, "", "failed", "fresh session after resume: "+err.Error())
			return
		}
		sessionID = sess.SessionID()
		run, err = sess.Prompt(ctx, p.Prompt)
	}
	if err != nil {
		e.finish(p, workdir, sessionID, "failed", fmt.Sprintf("prompt: %v", err))
		return
	}

	// Event pump: per-run monotonic seq, batched every ~100ms (or 64
	// events) — the per-token stream must not become per-token frames.
	seq := int64(0)
	buf := []link.RunEvent{}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	flushErrLogged := false
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := e.peer.Notify(ctx, link.MethodRunEventBatch, link.RunEventBatchParams{
			RunID: p.RunID, SeqStart: buf[0].Seq, Events: buf,
		}); err != nil && !flushErrLogged {
			// First failure only — a dead link would otherwise flood the
			// log every 100ms.
			cliLogf("exec: run %s event batch failed (seq %d..%d, %d events): %v", p.RunID, buf[0].Seq, buf[len(buf)-1].Seq, len(buf), err)
			flushErrLogged = true
		}
		buf = nil
	}
	defer flush()
loop:
	for {
		select {
		case ev, ok := <-run.Events:
			if !ok {
				flush()
				break loop
			}
			seq++
			buf = append(buf, link.RunEvent{
				Seq: seq, Kind: string(ev.Type), Text: ev.Text,
				Tool: ev.Tool, CallID: ev.CallID, Input: ev.Input, Output: ev.Output,
			})
			if len(buf) >= 64 {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			_ = sess.Cancel(context.Background())
			e.finish(p, workdir, sessionID, "cancelled", "cancelled by platform")
			return
		}
	}

	result, ok := <-run.Result
	if !ok {
		e.finish(p, workdir, sessionID, "failed", "backend closed result channel")
		return
	}
	status := "completed"
	switch result.Status {
	case proto.StatusFailed, proto.StatusAborted:
		status = "failed"
	case proto.StatusCancelled:
		status = "cancelled"
	}
	headSHA := ""
	if status == "completed" && !p.Scratch && !p.Proc {
		sha, err := commitAndPush(context.Background(), p, workdir, repo, branch, p.RunID, pushedProfile)
		if err != nil {
			cliLogf("exec: run %s commit/push failed: %v", p.RunID, err)
			e.finish(p, workdir, sessionID, "failed", "commit/push: "+err.Error())
			return
		}
		if sha != "" {
			cliLogf("exec: run %s pushed %s → agentwork/%s", p.RunID, shortSHA(sha), branch)
		}
		headSHA = sha
	}
	// result.Output is empty for a completed agent run (the output stream is
	// internal — chat_message; the agent communicates via goal comment). For
	// a failed/cancelled run it carries the error/stderr (the platform stamps
	// it into result_summary + writes a system failure comment).
	// Processor runs upload their FILE results (the platform reads
	// structured side effects, never agent stdout). Digest goal runs too:
	// the dispatch lists the goal-directory files (manifest + articles) the
	// daemon collects — same channel, not a processor run.
	finishParams := link.RunFinishedParams{RunID: p.RunID, Status: status, Summary: result.Output, Token: p.Token, HeadSHA: headSHA, SessionID: sessionID, WorkDir: workdir}
	if (p.Proc || len(p.ArtifactFiles) > 0) && status == "completed" {
		finishParams.Artifacts = map[string]string{}
		for _, f := range p.ArtifactFiles {
			if b, err := os.ReadFile(filepath.Join(workdir, f)); err == nil {
				finishParams.Artifacts[f] = string(b)
			}
		}
	}
	if err := callRPC(e.peer, link.MethodRunFinished, finishParams, nil, 30*time.Second); err != nil {
		// A lost terminal report would leave the run 'running' until the
		// daemon restarts — stash it for the next register.
		cliLogf("exec: run %s → %s (report stashed — link down: %v)", p.RunID, status, err)
		stashPendingReport(finishParams)
		return
	}
	cliLogf("exec: run %s → %s", p.RunID, status)
}

// finish uploads run.finished — the daemon runs Finish + reconcile. If the
// link is down at this exact moment, the report is STASHED to disk and
// flushed on the next successful register (a lost terminal report would
// leave the run hanging 'running' until the daemon restarts). The report
// echoes the dispatch's per-run TOKEN: the daemon swallows stale reports
// (an exec killed before a daemon restart must not cancel the attempt the
// daemon re-dispatched after recovery).
func (e *executor) finish(p link.RunDispatchParams, workdir, sessionID, status, summary string) {
	report := link.RunFinishedParams{RunID: p.RunID, Status: status, Summary: summary, Token: p.Token, SessionID: sessionID, WorkDir: workdir}
	err := callRPC(e.peer, link.MethodRunFinished, report, nil, 30*time.Second)
	if err == nil {
		cliLogf("exec: run %s → %s", p.RunID, status)
		return
	}
	cliLogf("exec: run %s → %s (report stashed — link down: %v)", p.RunID, status, err)
	stashPendingReport(report)
}

// shortSHA trims a commit sha for log lines.
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// pendingReportFile is where unreported terminal states wait for the next
// connection.
func pendingReportFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "pending-reports.json"
	}
	return filepath.Join(home, ".agentwork", "pending-reports.json")
}

// stashPendingReport appends a terminal report that could not be delivered.
func stashPendingReport(p link.RunFinishedParams) {
	reports, _ := loadPendingReports()
	reports = append(reports, p)
	savePendingReports(reports)
}

func loadPendingReports() ([]link.RunFinishedParams, error) {
	b, err := os.ReadFile(pendingReportFile())
	if err != nil {
		return nil, err
	}
	var out []link.RunFinishedParams
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func savePendingReports(reports []link.RunFinishedParams) {
	_ = os.MkdirAll(filepath.Dir(pendingReportFile()), 0o755)
	if len(reports) == 0 {
		_ = os.Remove(pendingReportFile())
		return
	}
	b, _ := json.MarshalIndent(reports, "", "  ")
	_ = os.WriteFile(pendingReportFile(), b, 0o644)
}

// flushPendingReports re-sends stashed terminal reports over a fresh link
// (called after a successful register). Leftovers stay stashed.
func flushPendingReports(peer *link.Peer) {
	reports, err := loadPendingReports()
	if err != nil || len(reports) == 0 {
		return
	}
	var remaining []link.RunFinishedParams
	for _, r := range reports {
		if err := callRPC(peer, link.MethodRunFinished, r, nil, 30*time.Second); err != nil {
			remaining = append(remaining, r)
		} else {
			fmt.Printf("flushed pending report for run %s (%s)\n", r.RunID, r.Status)
		}
	}
	savePendingReports(remaining)
}

// agentProfileDir is where config.push lands the agent's AGENTS.md
// (~/.agentwork/agents/<agentID>/).
func agentProfileDir(agentID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentwork", "agents", agentID)
}

// writeAgentProfile persists the pushed system prompt as AGENTS.md
// ((empty) removes the file — an empty prompt leaves no profile).
func writeAgentProfile(agentID, systemPrompt string) error {
	dir := agentProfileDir(agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "AGENTS.md")
	if strings.TrimSpace(systemPrompt) == "" {
		_ = os.Remove(path)
		return nil
	}
	return os.WriteFile(path, []byte(systemPrompt), 0o644)
}

// syncAgentProfile APPENDS the agent's pushed AGENTS.md into the run's
// workdir (the runtime's profile resolver walks up from cwd) — a
// repo-owned AGENTS.md in the checkout survives (its instructions are the
// project's ground truth and must not be clobbered; the platform profile
// rides below it). Persistent workdirs and the stable chat directory are
// staged REPEATEDLY: if the persona is already present, return the file
// as-is instead of appending a duplicate. Returns the content so the
// commit step can recognize the platform-written suffix.
func syncAgentProfile(agentID, workdir string) string {
	staging := filepath.Join(agentProfileDir(agentID), "AGENTS.md")
	b, err := os.ReadFile(staging)
	if err != nil {
		cliLogf("exec: syncAgentProfile agent=%s: no staging AGENTS.md at %s (agent has no persona)", agentID, staging)
		return ""
	}
	persona := string(b)
	path := filepath.Join(workdir, "AGENTS.md")
	existing, err := os.ReadFile(path)
	if err == nil && strings.TrimSpace(string(existing)) != "" {
		if strings.Contains(string(existing), persona) {
			cliLogf("exec: syncAgentProfile agent=%s: already present in %s (skip)", agentID, path)
			return string(existing) // already staged — never duplicate
		}
		merged := string(existing) + "\n\n" + persona
		_ = os.WriteFile(path, []byte(merged), 0o644)
		cliLogf("exec: syncAgentProfile agent=%s: merged %d bytes into existing %s", agentID, len(persona), path)
		return merged
	}
	_ = os.WriteFile(path, []byte(persona), 0o644)
	cliLogf("exec: syncAgentProfile agent=%s: wrote %d bytes → %s", agentID, len(persona), path)
	return persona
}

// machineSkillRoot is where config.push lands an agent's skill packages on
// the machine (~/.agentwork/skills/<agentID>/<skillName>/...) — the
// platform-managed staging dir; the executor copies them into each run's
// workdir at spawn.
func machineSkillRoot(agentID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentwork", "skills", agentID)
}

// skillSubdir maps a probed CLI (the acp_spawn executable) to its
// project-level skill directory inside a run workdir — the legacy-daemon
// fallback when the dispatch carries no ProjectSkillsDir. Project skills
// load from cwd — no global install, no collision with the user's own
// skills.
func skillSubdir(cli string) string {
	base := filepath.Base(cli)
	switch {
	case strings.HasPrefix(base, "opencode"):
		return ".opencode/skill"
	case strings.HasPrefix(base, "claude"):
		return ".claude/skills"
	default:
		return ".agents/skills" // the AgentSkills standard (npx skills add)
	}
}

// skillManagedMarker marks a workdir skill dir as platform-copied:
// copyAgentSkills writes it into every dir it copies; removeStaleSkillCopies
// removes ONLY marker-carrying dirs. The repository's own skill dirs (no
// marker) are never touched.
const skillManagedMarker = ".agentwork-managed"

// copyAgentSkills stages the agent's pushed skill packages into the
// workdir's project-level skill directory. wipe removes the target subdir
// first (platform-owned scratch workdirs — a full projection of the staging
// set). Repo workdirs overwrite + deselect-clean: every copied dir carries
// skillManagedMarker, so dirs the platform previously copied but that are no
// longer in the staging set (the skill was deselected) are removed — without
// the cleanup a deselected skill kept loading from the stale workdir copy;
// the repository's own dirs (no marker) are preserved.
func copyAgentSkills(agentID, workdir, subdir string, wipe bool) {
	root := machineSkillRoot(agentID)
	entries, err := os.ReadDir(root)
	if err != nil {
		cliLogf("exec: copyAgentSkills agent=%s: no staging dir %s (no skills pushed)", agentID, root)
		return // no skills pushed — nothing to stage
	}
	// count skill subdirs (skip files)
	var skillDirs []string
	for _, ent := range entries {
		if ent.IsDir() {
			skillDirs = append(skillDirs, ent.Name())
		}
	}
	target := filepath.Join(workdir, subdir)
	cliLogf("exec: copyAgentSkills agent=%s: %d skill(s) %v → %s (wipe=%v)", agentID, len(skillDirs), skillDirs, target, wipe)
	if wipe {
		_ = os.RemoveAll(target)
	} else {
		removeStaleSkillCopies(target, skillDirs)
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		src := filepath.Join(root, ent.Name())
		dst := filepath.Join(target, ent.Name())
		_ = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil // never follow symlinks
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			out := filepath.Join(dst, rel)
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return nil
			}
			_ = os.WriteFile(out, b, 0o644)
			return nil
		})
		// Mark the copy platform-owned: the next non-wipe run may remove it
		// when the skill is no longer in the staging set.
		_ = os.WriteFile(filepath.Join(dst, skillManagedMarker),
			[]byte("platform-managed by agentwork — removed automatically when the skill is deselected\n"), 0o644)
	}
}

// removeStaleSkillCopies deletes deselected platform skill copies from a
// non-wipe (repo) workdir: dirs carrying skillManagedMarker that are no
// longer in the machine staging set. The repository's own dirs are never
// touched (no marker). Best-effort — a failed removal leaves the copy; the
// next run retries.
func removeStaleSkillCopies(target string, current []string) {
	entries, err := os.ReadDir(target)
	if err != nil {
		return // no target dir yet — nothing stale
	}
	live := make(map[string]bool, len(current))
	for _, n := range current {
		live[n] = true
	}
	removed := 0
	for _, ent := range entries {
		if !ent.IsDir() || live[ent.Name()] {
			continue
		}
		dir := filepath.Join(target, ent.Name())
		if _, err := os.Stat(filepath.Join(dir, skillManagedMarker)); err != nil {
			continue // no marker — the repository's own dir, keep it
		}
		if err := os.RemoveAll(dir); err == nil {
			removed++
		}
	}
	if removed > 0 {
		cliLogf("exec: copyAgentSkills: removed %d deselected skill dir(s) from %s", removed, target)
	}
}

// machineProcWorkdir is where a processor run works on the machine.
func machineProcWorkdir(runID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentwork", "runs", "proc", runID)
}

// scratchWorkdir derives the local workdir for a scratch-domain run
// (mirrors the daemon's layout: runs/scratch/<domain>/goals/<goalID>,
// sub-goals under sg/<subGoalID>).
func scratchWorkdir(p link.RunDispatchParams) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(home, ".agentwork", "runs", "scratch", sanitizeName(p.DomainName), "goals", p.GoalID)
	if p.SubGoalID != "" {
		base = filepath.Join(base, "sg", p.SubGoalID)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	return base, nil
}

// buildEnv assembles the spawn environment: base + runtime/agent env +
// the run context (CLI 回调 + token) + a PATH entry pointing at an
// `agentwork` shim of this binary (the agent's terminal must find the CLI).
func (e *executor) buildEnv(p link.RunDispatchParams, workdir string) []string {
	env := os.Environ()
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	shimDir, err := ensureAgentworkShim()
	if err == nil && shimDir != "" {
		env = append(env, "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	env = append(env,
		"AGENTWORK_SERVER_URL="+e.serverURL,
		"AGENTWORK_GOAL_ID="+p.GoalID,
		"AGENTWORK_RUN_ID="+p.RunID,
		"AGENTWORK_AGENT_ID="+p.AgentID,
		"AGENTWORK_TOKEN="+p.Token,
	)
	return env
}

// ensureAgentworkShim symlinks this binary as `agentwork` under
// ~/.agentwork/bin so the agent's terminal has the command (the fixed
// block says "run `agentwork help`"). Returns the shim directory.
func ensureAgentworkShim() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".agentwork", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	shim := filepath.Join(dir, "agentwork")
	if _, err := os.Lstat(shim); err == nil {
		return dir, nil // exists (symlink or binary) — reuse
	}
	_ = os.Remove(shim)
	if err := os.Symlink(exe, shim); err != nil {
		// Fallback: copy the binary (some filesystems lack symlinks).
		if data, rerr := os.ReadFile(exe); rerr == nil {
			if werr := os.WriteFile(shim, data, 0o755); werr != nil {
				return "", werr
			}
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

// callRPC wraps peer.Call with a bounded ctx (one-shot acks must not hang
// the executor forever).
func callRPC(peer *link.Peer, method string, params, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return peer.Call(ctx, method, params, out)
}

// sanitizeName strips path-hostile characters from a domain name for the
// local directory layout. It MUST mirror the daemon's sanitizeDirName
// (internal/daemon/daemon.go): the daemon embeds ITS computed path into the
// run's AGENTS.md ("your artifacts live in <that dir>") and the agent obeys
// that absolute path over the spawn cwd — a mismatch between the two
// sanitizers makes the agent write its artifacts one directory tree away
// from where the CLI then looks for them (a CJK domain name diverges: the
// old ASCII-only sanitizer produced AI____ while the daemon meant AI知识精选).
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r >= 0x4e00 && r <= 0x9fff, // CJK unified — kept, like the daemon's
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_-")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		out = "domain"
	}
	return out
}
