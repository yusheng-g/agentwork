package service

import (
	"context"
	"fmt"
	"sync"
)

// SystemTaskHandler defines the lifecycle of a system-task goal: a goal
// assigned to a system agent (steward) that executes a processor-style task
// (artifact files, no code verification) with goal-plane visibility. The
// daemon dispatches generically via the SystemTaskRegistry; each handler
// provides the task-type-specific behavior.
//
// A system-task goal differs from a normal worker goal:
//   - No git verification/gates (the task produces artifacts, not code changes)
//   - Auto-completes on run end (done/failed — no human review)
//   - No retry on failure
//   - Uses the machine's proc dir (Proc=true), not a goal-branch worktree
//
// To add a new system task type:
//  1. Implement this interface.
//  2. Register at daemon startup: registry.Register("my_type", &MyHandler{})
//  3. Trigger (intake/HTTP/scheduler) calls CreateSystemTaskGoal(..., "my_type")
//
// No daemon dispatch, completion, or reconcile changes are needed.
type SystemTaskHandler interface {
	// ArtifactFiles returns the file names the agent must produce in the
	// proc dir. The machine uploads these with run.finished.
	ArtifactFiles() []string

	// GitConfigForRun returns the git config for the run's worktree clone.
	// ok=false means the task works in a plain proc dir (no git clone).
	// The git config was persisted at enqueue time and survives the queued
	// interval.
	GitConfigForRun(ctx context.Context, runID string) (gitURL, gitCredentials, defaultBranch string, ok bool)

	// BuildPrompt returns the instruction sent to the agent. Called at
	// dispatch time (when the run is claimed), so it can read live state
	// (e.g. active runtime names for the import prompt).
	BuildPrompt(ctx context.Context, runID string) string

	// IngestArtifacts processes the agent's output files. Returns a
	// human-readable result summary (landed in the goal's comment feed)
	// and an error (the run is marked failed if non-nil). Called by the
	// daemon's completion pipeline before Finish triggers the goal
	// reconcile.
	IngestArtifacts(ctx context.Context, runID string, artifacts map[string]string, agentSummary string) (resultSummary string, err error)
}

// SystemTaskRegistry maps run_type strings to their SystemTaskHandler.
// The daemon looks up the handler at dispatch and completion time.
type SystemTaskRegistry struct {
	mu       sync.RWMutex
	handlers map[string]SystemTaskHandler
}

func NewSystemTaskRegistry() *SystemTaskRegistry {
	return &SystemTaskRegistry{handlers: make(map[string]SystemTaskHandler)}
}

// Register associates a run_type with its handler. Called once at daemon
// startup. A duplicate registration for the same run_type overwrites the
// previous handler (useful for tests).
func (r *SystemTaskRegistry) Register(runType string, h SystemTaskHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[runType] = h
}

// Lookup returns the handler for a run_type, or nil/false if unregistered.
func (r *SystemTaskRegistry) Lookup(runType string) (SystemTaskHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[runType]
	return h, ok
}

// SystemTaskGoalTitle builds a human-readable goal title for a system task.
// Visible in the goal list and timeline.
func SystemTaskGoalTitle(taskType, subject string) string {
	switch taskType {
	case "import":
		return "团队导入: " + subject
	default:
		return fmt.Sprintf("%s: %s", taskType, subject)
	}
}

// systemTaskDomainName returns a human-readable domain name for a
// system-task's temporary scratch domain. Maps task types to Chinese
// labels; unknown types fall back to a generic "内部任务".
func systemTaskDomainName(taskType, idSuffix string) string {
	switch taskType {
	case "import":
		return "团队导入-" + idSuffix
	default:
		return "内部任务-" + idSuffix
	}
}
