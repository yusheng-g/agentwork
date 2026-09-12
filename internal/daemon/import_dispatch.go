package daemon

// The team-import goal's runtime hooks: goal recognition, artifact ingestion
// at run finish (worker branch), auto-approval of the review checkpoint, and
// startup sweeps. Mirrors the digest pattern (digest.go) — an import goal is a
// system goal (created_by_type='system', created_by_id='team_import') on a
// scratch domain whose worker run clones the import repo itself and uploads
// team.json. The checkpoint is auto-approved: no human ever sees the card
// (maybeFireReviewReady skips auto-approve goals via isAutoApproveGoal).

import (
	"context"
	"time"

	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/service"
)

// isImportGoal reports whether the goal was created by the team-import flow.
// The goal row may be deleted behind this check (the caller's event lags the
// cascade) — any read error is a plain no. Recognition keys on the
// created_by_type='system' AND created_by_id='team_import' pair (a stable
// constant, unlike digest whose created_by_id is the schedule id read from
// app_settings — import has no schedule).
func (d *Daemon) isImportGoal(ctx context.Context, goalID string) bool {
	var createdByType, createdByID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT created_by_type, COALESCE(created_by_id,'') FROM goal WHERE id=?`, goalID).
		Scan(&createdByType, &createdByID)
	if err != nil || createdByType != "system" || createdByID != service.ImportCreatedByID {
		return false
	}
	return true
}

// importGoalIDForRun resolves a finished run's goal and returns it when it is
// a team-import goal ('' = not import / no goal). Used by finishMachineRun's
// worker branch to detect import runs that need team.json ingestion.
func (d *Daemon) importGoalIDForRun(ctx context.Context, runID string) string {
	var goalID, createdByType, createdByID string
	err := d.st.DB().QueryRowContext(ctx,
		`SELECT r.goal_id, g.created_by_type, COALESCE(g.created_by_id,'')
		 FROM run r JOIN goal g ON g.id = r.goal_id WHERE r.id=?`, runID).
		Scan(&goalID, &createdByType, &createdByID)
	if err != nil || createdByType != "system" || createdByID != service.ImportCreatedByID {
		return ""
	}
	return goalID
}

// isAutoApproveGoal reports whether the goal's review checkpoint is
// auto-approved (never reaches a human). Digest and import goals both skip the
// review-ready card and auto-approve after Finish. maybeFireReviewReady calls
// this instead of isDigestGoal so both built-in auto-approve flows are covered.
func (d *Daemon) isAutoApproveGoal(ctx context.Context, goalID string) bool {
	return d.isDigestGoal(ctx, goalID) || d.isImportGoal(ctx, goalID)
}

// approveImportGoal closes the import goal's mandatory review checkpoint.
// Mirrors approveDigestGoal (digest.go:448): the reconcile that parks the goal
// into review runs inside runSvc.Finish (invariant 13) — the caller invokes
// this AFTER Finish, but the park lands asynchronously enough (reconcile →
// publish → bus goroutine) that a short poll covers the gap. A duplicate
// approve (crash after the decision row, before deliver) is a success, not an
// error.
func (d *Daemon) approveImportGoal(ctx context.Context, goalID, runID string) {
	var lastErr error
	for i := 0; i < 5; i++ {
		_, err := d.goalSvc.ResolveReview(ctx, goalID, runID, "approve", "团队导入自动验收", "system")
		if err == nil {
			logging.Infof("team-import: goal %s auto-approved", goalID)
			return
		}
		lastErr = err
		// Not in review YET is retryable; other coded errors (duplicate
		// approve on the same run) are terminal-but-fine.
		if !service.IsRetryableReviewWait(err) {
			logging.Infof("team-import: goal %s auto-approve: %v", goalID, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	logging.Warnf("team-import: goal %s auto-approve gave up: %v", goalID, lastErr)
}

// sweepStuckImportGoals closes the crash window between Finish (team.json
// ingested) and the auto-approve: an import goal parked in review with no
// approve decision gets approved at startup. Mirrors sweepStuckDigestGoals
// (digest.go:476).
func (d *Daemon) sweepStuckImportGoals(ctx context.Context) {
	rows, err := d.st.DB().QueryContext(ctx,
		`SELECT g.id FROM goal g
		 WHERE g.status='review' AND g.created_by_type='system' AND g.created_by_id=?
		   AND NOT EXISTS (SELECT 1 FROM gate_decision gd WHERE gd.goal_id=g.id AND gd.decision='approve')`,
		service.ImportCreatedByID)
	if err != nil {
		logging.Infof("team-import: stuck-goal sweep: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		// The run id is whatever the goal's last run was — ResolveReview
		// stamps the decision on it; the duplicate-decision guard keys on
		// (goal, run) so any of the goal's runs works for a fresh park.
		var runID string
		_ = d.st.DB().QueryRowContext(ctx,
			`SELECT id FROM run WHERE goal_id=? ORDER BY queued_at DESC LIMIT 1`, id).Scan(&runID)
		if runID == "" {
			continue
		}
		d.approveImportGoal(ctx, id, runID)
	}
	logging.Infof("team-import: swept %d stuck review goal(s)", len(ids))
}
