// Package issue is the GitHub issue adapter (M4-B): the external-entry half
// of the loop. Open issues in a domain's configured repo become goals
// (poller); the agent replies to issues through the platform (CreateComment,
// driven by agentwork-cli); a delivered goal closes its issue (closer).
//
// The adapter is a pure platform action: the agent never touches the GitHub
// token or the API — it produces structured side effects via agentwork-cli,
// the platform executes them (triangle separation).
package issue

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

// Issue is one GitHub issue (API shape, minimal).
type Issue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	// PullRequest is non-nil for PRs — the issues API returns them too and
	// they must be skipped (a PR is not a work request).
	PullRequest any `json:"pull_request"`
}

// Pull is one pull request (API shape, minimal). The head/target-branch
// context lets a PR goal's run fetch the source branch and review the actual
// diff — the platform never downloads the diff itself.
type Pull struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	HTMLURL      string `json:"html_url"`
	Author       string `json:"author"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	HeadSHA      string `json:"head_sha"`
}

// Comment is one issue comment (API shape, minimal).
type Comment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// Client talks to the GitHub REST API with a repo-scoped token (the domain's
// git_credentials). The HTTP client and base URL are injectable for tests.
type Client struct {
	token   string
	http    *http.Client
	baseURL string
}

func NewClient(token string) *Client {
	return &Client{token: token, http: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.github.com"}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ListOpenIssues returns the repo's open issues (PRs excluded).
func (c *Client) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	var issues []Issue
	err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/issues?state=open&per_page=100", nil, &issues)
	if err != nil {
		return nil, err
	}
	out := []Issue{}
	for _, i := range issues {
		if i.PullRequest == nil {
			out = append(out, i)
		}
	}
	return out, nil
}

// ListComments returns the issue's comments (newest last).
func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	var out []Comment
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number), nil, &out)
	return out, err
}

// CreateComment posts a comment on the issue.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number),
		map[string]string{"body": body}, nil)
}

// CloseIssue closes the issue.
func (c *Client) CloseIssue(ctx context.Context, repo string, number int) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number),
		map[string]string{"state": "closed"}, nil)
}

// CommitURL is the GitHub commit link.
func (c *Client) CommitURL(repo, sha string) string {
	return "https://github.com/" + repo + "/commit/" + sha
}

// ── Pull-request surface (PR pull) ──

// gitHubPull is the GitHub PR wire shape (minimal): number is an int, head
// carries the source branch + SHA, base the target branch.
type gitHubPull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// ListOpenPulls returns the repo's open pull requests. GitHub keeps PRs in
// their own /pulls collection (they ALSO appear in the issues list — which
// ListOpenIssues filters out — so the two never double-track one object).
func (c *Client) ListOpenPulls(ctx context.Context, repo string) ([]Pull, error) {
	var raw []gitHubPull
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?state=open&per_page=100", nil, &raw); err != nil {
		return nil, err
	}
	out := []Pull{}
	for _, pr := range raw {
		out = append(out, Pull{
			Number: pr.Number, Title: pr.Title, Body: pr.Body, HTMLURL: pr.HTMLURL,
			Author: pr.User.Login, SourceBranch: pr.Head.Ref, TargetBranch: pr.Base.Ref,
			HeadSHA: pr.Head.SHA,
		})
	}
	return out, nil
}

// ListPullComments returns the PR's comments. GitHub serves PR dialogue
// through the SAME issues-comments endpoint (a PR is an issue with a diff
// attached) — forward to the issue method.
func (c *Client) ListPullComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	return c.ListComments(ctx, repo, number)
}

// CreatePullComment posts a comment on the PR (the issues-comments endpoint).
func (c *Client) CreatePullComment(ctx context.Context, repo string, number int, body string) error {
	return c.CreateComment(ctx, repo, number, body)
}

// ── Poller: open issues → goals ──

// Poller turns a domain's open issues into goals (one goal per issue,
// idempotent via the unique source_ref). The poll is the trigger of the M4-B
// loop — no public webhook needed on a single-user machine.
type Poller struct {
	st      *store.Store
	goalSvc *service.GoalService
	runSvc  *service.RunService
	// newProvider is injectable for tests.
	newProvider func(provider, token string) (Provider, error)
}

func NewPoller(st *store.Store, goalSvc *service.GoalService, runSvc *service.RunService) *Poller {
	return &Poller{st: st, goalSvc: goalSvc, runSvc: runSvc, newProvider: NewProvider}
}

// trackedDomain is one polling candidate row (Poll's domain query): the
// issue handler and the PR handler ride the same repo/provider — only the
// assignee differs ('' = that side doesn't poll).
type trackedDomain struct {
	id, provider, repo, assignee, assigneeType, token, pullAssignee, pullAssigneeType string
}

// Poll scans every tracking domain once. Returns how many goals were created.
func (p *Poller) Poll(ctx context.Context) (int, error) {
	rows, err := p.st.DB().QueryContext(ctx,
		`SELECT id, issue_provider, issue_repo, issue_assignee, issue_assignee_type, git_credentials, COALESCE(pull_assignee,''), COALESCE(pull_assignee_type,'') FROM domain
		 WHERE issue_repo != '' AND git_credentials != '' AND (issue_assignee != '' OR COALESCE(pull_assignee,'') != '')`)
	if err != nil {
		return 0, err
	}
	var doms []trackedDomain
	for rows.Next() {
		var d trackedDomain
		if err := rows.Scan(&d.id, &d.provider, &d.repo, &d.assignee, &d.assigneeType, &d.token, &d.pullAssignee, &d.pullAssigneeType); err != nil {
			rows.Close()
			return 0, err
		}
		if d.assigneeType == "" {
			d.assigneeType = "agent"
		}
		if d.pullAssigneeType == "" {
			d.pullAssigneeType = "agent"
		}
		doms = append(doms, d)
	}
	rows.Close()

	created := 0
	for _, d := range doms {
		n, err := p.pollDomain(ctx, d)
		created += n
		if err != nil {
			return created, err
		}
	}
	return created, nil
}

func (p *Poller) pollDomain(ctx context.Context, d trackedDomain) (int, error) {
	created := 0
	if d.assignee != "" {
		n, err := p.pollDomainIssues(ctx, d)
		created += n
		if err != nil {
			return created, err
		}
	}
	if d.pullAssignee != "" {
		n, err := p.pollDomainPulls(ctx, d)
		created += n
		if err != nil {
			return created, err
		}
	}
	return created, nil
}

// pollDomainIssues turns the domain's open issues into goals.
func (p *Poller) pollDomainIssues(ctx context.Context, d trackedDomain) (int, error) {
	client, err := p.newProvider(d.provider, d.token)
	if err != nil {
		return 0, fmt.Errorf("domain %s: %w", d.id, err)
	}
	issues, err := client.ListOpenIssues(ctx, d.repo)
	if err != nil {
		return 0, fmt.Errorf("domain %s: %w", d.id, err)
	}
	created := 0
	for _, iss := range issues {
		ok, err := p.CreateGoalForIssue(ctx, d.id, d.assignee, d.assigneeType, d.provider, d.repo, iss)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}

// pollDomainPulls turns the domain's open pull requests into goals (M4-B PR
// pull) — the pull-request analogue of pollDomainIssues, gated by the
// domain's PR assignee (pull_assignee, '' = the repo's PRs are not tracked).
func (p *Poller) pollDomainPulls(ctx context.Context, d trackedDomain) (int, error) {
	client, err := p.newProvider(d.provider, d.token)
	if err != nil {
		return 0, fmt.Errorf("domain %s: %w", d.id, err)
	}
	pulls, err := client.ListOpenPulls(ctx, d.repo)
	if err != nil {
		return 0, fmt.Errorf("domain %s: %w", d.id, err)
	}
	created := 0
	for _, pr := range pulls {
		ok, err := p.CreateGoalForPull(ctx, d.id, d.pullAssignee, d.pullAssigneeType, d.provider, d.repo, pr)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}

// CreateGoalForIssue turns one issue into a goal (idempotent via the unique
// source_ref). Shared by the poller AND the webhook handler — both triggers
// converge on the same create path, so webhook + poll racing can never
// double-create. Returns whether a goal was created.
func (p *Poller) CreateGoalForIssue(ctx context.Context, domainID, assigneeID, assigneeType, provider, repo string, iss Issue) (bool, error) {
	ref := SourceRef(provider, repo, iss.Number)
	var exists int
	if err := p.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal WHERE source_ref=?`, ref).Scan(&exists); err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil // already tracked
	}
	// The goal's description carries the issue context; the run prompt
	// additionally injects the live comments (daemon side).
	title := iss.Title
	if title == "" {
		title = fmt.Sprintf("Issue #%d", iss.Number)
	}
	desc := fmt.Sprintf("来自 %s issue #%d：%s\n%s", providerLabel(provider), iss.Number, iss.Title, truncate(iss.Body, 1500))
	_, err := p.goalSvc.Create(ctx, service.Goal{
		Title:         title,
		Description:   desc,
		DomainID:      domainID,
		AssigneeType:  assigneeType, // agent | squad (domain 配置决定谁处理 issue)
		AssigneeID:    assigneeID,
		Status:        "active",
		CreatedByType: "system",
		CreatedByID:   "issue-poll",
		SourceRef:     ref,
	})
	if err != nil {
		// A unique-source_ref violation means the webhook and the poll raced
		// and the other side won — that IS the idempotent outcome, not an
		// error (the caller must not surface it as a failed delivery).
		if strings.Contains(err.Error(), "UNIQUE") {
			return false, nil
		}
		return false, fmt.Errorf("create goal for %s: %w", ref, err)
	}
	// P0-2 (决策 6-15②): the active goal's first run is born in Create's
	// transaction — no separate enqueue.
	return true, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// CreateGoalForPull turns one pull request into a goal (M4-B PR pull) — the
// pull-request analogue of CreateGoalForIssue, idempotent via the unique
// source_ref. The PR ref carries a "!" suffix (kind=pull): issue and PR
// numbers are separate sequences on GitCode, so a bare "#" would collide.
// The goal's description carries the PR context INCLUDING the source/target
// branches — the run's prompt section points the agent at them (fetch the
// source branch in the worktree to review the diff).
func (p *Poller) CreateGoalForPull(ctx context.Context, domainID, assigneeID, assigneeType, provider, repo string, pr Pull) (bool, error) {
	ref := PullSourceRef(provider, repo, pr.Number)
	var exists int
	if err := p.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goal WHERE source_ref=?`, ref).Scan(&exists); err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil // already tracked
	}
	title := pullGoalTitle(pr)
	desc := pullGoalDescription(provider, pr)
	_, err := p.goalSvc.Create(ctx, service.Goal{
		Title:         title,
		Description:   desc,
		DomainID:      domainID,
		AssigneeType:  assigneeType, // agent | squad (域配置决定谁处理 PR)
		AssigneeID:    assigneeID,
		Status:        "active",
		CreatedByType: "system",
		CreatedByID:   "pr-poll",
		SourceRef:     ref,
	})
	if err != nil {
		// A unique-source_ref violation is the idempotent outcome (another
		// poll won the race) — not an error, same as CreateGoalForIssue.
		if strings.Contains(err.Error(), "UNIQUE") {
			return false, nil
		}
		return false, fmt.Errorf("create goal for %s: %w", ref, err)
	}
	return true, nil
}

// pullGoalTitle is the goal title for a PR — the PR's own title, or a
// numbered fallback when the payload carries none.
func pullGoalTitle(pr Pull) string {
	if pr.Title != "" {
		return pr.Title
	}
	return fmt.Sprintf("PR !%d", pr.Number)
}

// pullGoalDescription builds the goal description for a PR — the projection of
// the PR's current state (title/body + branches + head SHA). Shared by the
// create and the update path so an update's fresh description is byte-identical
// to what a create would write: the equality IS the duplicate-delivery check,
// and the head-SHA line is what makes a synchronize push (new head, same body)
// a real change rather than a silent no-op.
func pullGoalDescription(provider string, pr Pull) string {
	desc := fmt.Sprintf("来自 %s PR !%d：%s\n%s", providerLabel(provider), pr.Number, pr.Title, truncate(pr.Body, 1500))
	if pr.SourceBranch != "" || pr.TargetBranch != "" {
		desc += fmt.Sprintf("\n\n分支：%s → %s", pr.SourceBranch, pr.TargetBranch)
	}
	if pr.HeadSHA != "" {
		desc += fmt.Sprintf("\n\n最新提交：%s", shortSHA(pr.HeadSHA))
	}
	return desc
}

// shortSHA is the human-clickable short form of a commit SHA (8 hex chars),
// guarded against short input.
func shortSHA(sha string) string {
	if len(sha) < 8 {
		return sha
	}
	return sha[:8]
}

// descBodyPart is the description minus its title line and head-SHA suffix —
// the body + branch part only, so the update detection can tell a title edit
// (the desc's first line carries the title) from a body/branch edit.
func descBodyPart(desc string) string {
	if i := strings.Index(desc, "\n"); i >= 0 {
		desc = desc[i+1:]
	}
	return stripHeadLine(desc)
}

// pullGoalUpdateComment is the goal-feed comment a PR update appends — the
// audit trail of what changed and the trigger the owner wake carries.
func pullGoalUpdateComment(pr Pull, titleChanged, headChanged, bodyChanged bool) string {
	var changes []string
	if headChanged {
		changes = append(changes, "新提交 "+shortSHA(pr.HeadSHA))
	}
	if bodyChanged {
		changes = append(changes, "描述有修改")
	}
	if titleChanged {
		changes = append(changes, "标题有修改")
	}
	return fmt.Sprintf("PR !%d 已更新（%s）——请重新拉取最新状态后继续。", pr.Number, strings.Join(changes, "，"))
}

// descHeadLine extracts the 最新提交 line's SHA from a goal description (''
// when the description predates the head-SHA line or carries none).
func descHeadLine(desc string) string {
	const marker = "\n\n最新提交："
	if i := strings.LastIndex(desc, marker); i >= 0 {
		return desc[i+len(marker):]
	}
	return ""
}

// stripHeadLine removes the 最新提交 suffix from a goal description, so the
// update detection can tell a push (head line changed) from a description
// edit (head line unchanged) without re-deriving the old payload.
func stripHeadLine(desc string) string {
	if i := strings.LastIndex(desc, "\n\n最新提交："); i >= 0 {
		return desc[:i]
	}
	return desc
}

// webhookCommentID is a fresh feed-comment id (16 random bytes hex — the same
// shape the CommentService's newID writes; the poller holds no CommentService,
// and a system comment needs no mention-dispatch, so the insert is direct SQL).
func webhookCommentID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// UpdateGoalForPull refreshes the goal tracking an UPDATED pull request (the
// webhook's synchronize/edited deliveries): the same PR pushed new commits or
// edited its description. The goal is the PR's persistent tracker — one goal
// per PR (source_ref unique) — so an update NEVER creates a second goal. The
// refresh rewrites the goal's title/description (the projection of the PR's
// current state: body + branches + head SHA) and appends a system comment to
// the goal feed (the audit trail + the update signal the owner wake carries).
// A duplicate delivery (same head AND body — the fresh description is
// byte-identical to the stored one) is a no-op. A goal that does not exist
// yet falls back to CreateGoalForPull (a missed `opened` delivery self-heals
// here). Returns whether the goal was refreshed.
func (p *Poller) UpdateGoalForPull(ctx context.Context, domainID, assigneeID, assigneeType, provider, repo string, pr Pull) (bool, error) {
	ref := PullSourceRef(provider, repo, pr.Number)
	var goalID string
	err := p.st.DB().QueryRowContext(ctx,
		`SELECT id FROM goal WHERE source_ref=?`, ref).Scan(&goalID)
	if errors.Is(err, sql.ErrNoRows) {
		// The goal does not exist (the `opened` webhook was missed): create it
		// with the FRESH state — the update self-heals into a create.
		return p.CreateGoalForPull(ctx, domainID, assigneeID, assigneeType, provider, repo, pr)
	}
	if err != nil {
		return false, err
	}
	title := pullGoalTitle(pr)
	fresh := pullGoalDescription(provider, pr)
	// M3: the dedupe+update is check-then-act — serialize it with the same
	// per-goal lock Relaunch takes, so two concurrent deliveries of the same
	// PR (platform retry) cannot both pass the dedupe and double-insert the
	// feed comment. The lock is released BEFORE the wake: wakeGoalOwner's
	// Relaunch takes the same lock (sync.Mutex is not reentrant).
	updated, commentID, status, err := func() (bool, string, string, error) {
		unlock := p.goalSvc.LockGoal(goalID)
		defer unlock()
		// Re-read under the lock: the state may have moved between the first
		// read and the lock (the reconcile, another delivery).
		var existingTitle, existingDesc, status string
		if err := p.st.DB().QueryRowContext(ctx,
			`SELECT title, description, status FROM goal WHERE id=?`, goalID).
			Scan(&existingTitle, &existingDesc, &status); err != nil {
			return false, "", "", err
		}
		if title == existingTitle && fresh == existingDesc {
			return false, "", status, nil // duplicate delivery (same head AND body) — nothing to do
		}
		// What changed, for the feed comment: a moved head SHA is a push, a
		// changed body part is a description/branch edit, a changed title line
		// is a title edit.
		titleChanged := title != existingTitle
		headChanged := pr.HeadSHA != "" && shortSHA(pr.HeadSHA) != descHeadLine(existingDesc)
		bodyChanged := descBodyPart(existingDesc) != descBodyPart(fresh)
		if _, err := p.st.DB().ExecContext(ctx,
			`UPDATE goal SET title=?, description=? WHERE id=?`, title, fresh, goalID); err != nil {
			return false, "", "", fmt.Errorf("refresh goal for %s: %w", ref, err)
		}
		// Append the update to the goal feed (audit trail + the wake trigger).
		// System author: no human-reply routing applies to it.
		commentID, cErr := p.insertSystemComment(ctx, goalID, pullGoalUpdateComment(pr, titleChanged, headChanged, bodyChanged))
		if cErr != nil {
			// The feed row is the audit trail; the goal is already fresh. Log
			// only — never fail the delivery on a feed write.
			logging.Infof("pr-update: feed comment for %s: %v", ref, cErr)
		}
		return true, commentID, status, nil
	}()
	if err != nil {
		return false, err
	}
	if !updated {
		return false, nil
	}
	// Wake the goal's owner (agent assignee or squad leader) so the RUNNING
	// team learns the PR changed — the leader holds the context and decides
	// how to re-route (one message per push, into the leader session). Terminal
	// goals are NOT woken or reopened: a re-run after a push is the human's
	// decision.
	p.wakeGoalOwner(ctx, goalID, assigneeID, assigneeType, commentID, status, ref)
	return true, nil
}

// insertSystemComment appends a platform system comment to the goal feed
// (author_type='system'): the update audit trail + the trigger the owner wake
// carries. Direct SQL — the poller holds no CommentService, and a system
// comment must never trigger the mention dispatch a CommentService.Create
// would run.
func (p *Poller) insertSystemComment(ctx context.Context, goalID, content string) (string, error) {
	id := webhookCommentID()
	var parentID any // NULL — the update is a top-level feed entry, not a reply
	_, err := p.st.DB().ExecContext(ctx,
		`INSERT INTO comment (id, goal_id, author_type, author_id, parent_id, content, created_at, run_id, ask_human)
		 VALUES (?, ?, 'system', 'pr-webhook', ?, ?, ?, '', 0)`,
		id, goalID, parentID, content, time.Now().UTC().Format(time.RFC3339Nano))
	return id, err
}

// wakeGoalOwner re-launches the goal's work on a PR update — in EVERY state
// (the PR-update requirement: a fresh push always re-launches the work, no
// state exempt). active → wake the assignee (agent) or squad leader with the
// update comment as the trigger (the same owner-resume routing a human reply
// uses, coalesced on the pending pair); every other state (backlog, review,
// done, failed, cancelled) → Relaunch: back to active with a fresh owner run
// in one transaction. Best-effort: the update is already recorded; a failed
// wake must not fail the delivery. Human-assigned goals get no run (no agent
// to auto-run) — the update is still recorded.
func (p *Poller) wakeGoalOwner(ctx context.Context, goalID, assigneeID, assigneeType, commentID, status, ref string) {
	if p.runSvc == nil || commentID == "" {
		return
	}
	if status != "active" {
		// Relaunch: the goal is re-runnable from any non-active state — the
		// fresh push supersedes whatever the goal reached (a parked review,
		// a done/cancelled terminal, a backlog item). The pr-webhook update
		// comment is already the reason and the run's trigger.
		if p.goalSvc != nil {
			if _, err := p.goalSvc.Relaunch(ctx, goalID, "", commentID, "system"); err != nil {
				logging.Infof("pr-update: relaunch %s from %s: %v", ref, status, err)
			} else {
				logging.Infof("pr-update: relaunched goal=%s from %s (%s)", goalID, status, ref)
			}
		}
		return
	}
	ownerAgentID := ""
	switch assigneeType {
	case "agent":
		ownerAgentID = assigneeID
	case "squad":
		_ = p.st.DB().QueryRowContext(ctx,
			`SELECT leader_id FROM squad WHERE id=?`, assigneeID).Scan(&ownerAgentID)
	}
	if ownerAgentID == "" {
		return
	}
	if _, err := p.runSvc.EnqueueForMentionRole(ctx, goalID, ownerAgentID, commentID, "owner"); err != nil {
		logging.Infof("pr-update: owner wake for %s: %v", ref, err)
	} else {
		logging.Infof("pr-update: owner wake goal=%s agent=%s (%s)", goalID, ownerAgentID, ref)
	}
}

// providerLabel maps a provider id to its human-facing platform name, so the
// goal's description names the real source ("GitHub" | "GitCode") instead of
// always saying GitHub.
func providerLabel(provider string) string {
	switch provider {
	case "gitcode":
		return "GitCode"
	case "", "github":
		return "GitHub"
	default:
		return provider
	}
}

// ── Closer: delivered goal → close its issue ──

// Closer closes the issue behind a goal once the goal is delivered (merged
// into the domain's default branch). Subscribes to goal:delivered.
type Closer struct {
	st *store.Store
	// newProvider is injectable for tests.
	newProvider func(provider, token string) (Provider, error)
}

func NewCloser(st *store.Store) *Closer {
	return &Closer{st: st, newProvider: NewProvider}
}

// issueTarget is a resolved source-ref goal's outbound identity: the
// provider client (holds the bot token) + the issue/PR coordinates. resolve
// returns ok=false when the goal has no source_ref or the domain/token is
// missing — the caller silently no-ops, same as OnDelivered always has.
type issueTarget struct {
	client   Provider
	provider string        // "github" | "gitcode" — log/diagnostic only
	kind     SourceRefKind // issue | pull — routes the writeback endpoint
	repo     string
	number   int
}

// resolveIssueTarget loads the goal's source_ref + the domain's bot token and
// builds the provider client. Shared by every source-ref writeback path
// (delivered close, failed/cancelled notice) so they all route through one
// lookup.
func (c *Closer) resolveIssueTarget(ctx context.Context, goalID string) (issueTarget, bool) {
	var ref, token string
	err := c.st.DB().QueryRowContext(ctx,
		`SELECT g.source_ref, d.git_credentials FROM goal g JOIN domain d ON d.id=g.domain_id WHERE g.id=?`, goalID).
		Scan(&ref, &token)
	if err != nil || ref == "" {
		return issueTarget{}, false // no domain or not source-ref-tracked
	}
	provider, repo, number, kind, ok := ParseSourceRef(ref)
	if !ok {
		return issueTarget{}, false
	}
	client, err := c.newProvider(provider, token)
	if err != nil {
		return issueTarget{}, false
	}
	return issueTarget{client: client, provider: provider, kind: kind, repo: repo, number: number}, true
}

// postComment routes the writeback comment to the right API surface: a PR's
// dialogue lives on a dedicated endpoint (GitHub: the issues-comments
// endpoint; GitCode: /pulls/:n/comments) — the issues endpoint does not
// cover PR comments on every provider.
func (t issueTarget) postComment(ctx context.Context, body string) error {
	if t.kind == RefPull {
		return t.client.CreatePullComment(ctx, t.repo, t.number, body)
	}
	return t.client.CreateComment(ctx, t.repo, t.number, body)
}

// OnDelivered is the bus handler for goal:delivered. commits ("<full sha>
// <title>", structured from the deliver — never parsed from the note text)
// become clickable fix links, the way an open-source maintainer closes an
// issue. Pull-request goals are NOT closed on delivery: a PR is a pending
// code change, not a work request — its merge/close is the human's decision;
// the delivery comment (fix evidence) lands on the PR instead.
func (c *Closer) OnDelivered(ctx context.Context, goalID, note string, commits []string) {
	t, ok := c.resolveIssueTarget(ctx, goalID)
	if !ok {
		return
	}
	if t.kind != RefPull {
		if err := t.client.CloseIssue(ctx, t.repo, t.number); err != nil {
			// Closing is best-effort: the work IS delivered; the issue closing can
			// be retried by hand. Log only — never fail the deliver.
			fmt.Printf("issue: close %s:%s#%d: %v\n", t.provider, t.repo, t.number, err)
			return
		}
	}
	comment := fmt.Sprintf("✅ 已由 agentwork 修复并合入（goal %s）。", short(goalID))
	if trimmed := strings.TrimSpace(note); trimmed != "" {
		comment += "\n\n" + trimmed
	}
	for _, line := range commits {
		if len(line) >= 40 && isHexSHA(line[:40]) {
			sha := line[:40]
			title, _, _ := strings.Cut(strings.TrimSpace(line[40:]), "\n")
			comment += fmt.Sprintf("\n- [%s %s](%s)", sha[:8], title, t.client.CommitURL(t.repo, sha))
		}
	}
	_ = t.postComment(ctx, comment)
}

// OnTerminal is the bus handler for goal:finished status=failed where the
// failing run actually ran an agent session (agent_ran=true). Unlike
// OnDelivered it does NOT close the issue — the work was not delivered, so
// the issue stays open for the human. It posts the run's terminal summary
// as the issue comment so the author sees what went wrong. The summary is
// the platform-generated error/stderr from the failed run's result_summary
// (决策 4-4: a failed run's agent had no chance to comment, so the platform
// supplies the failure reason — not the agent's own words). The comment
// carries NO goal id / platform branding. An empty summary is silently
// dropped (the caller already gated on agent_ran, but a run that started
// yet produced no error text still has nothing worth saying).
func (c *Closer) OnTerminal(ctx context.Context, goalID, summary string) {
	text := strings.TrimSpace(summary)
	if text == "" {
		return
	}
	t, ok := c.resolveIssueTarget(ctx, goalID)
	if !ok {
		return
	}
	if err := t.postComment(ctx, text); err != nil {
		fmt.Printf("issue: terminal comment %s:%s#%d: %v\n", t.provider, t.repo, t.number, err)
	}
}

// OnAgentQuestion is the bus handler for comment:agent_question: an agent
// posted an --ask comment on the goal's feed, asking the human a question.
// For an issue-sourced goal the question is mirrored onto the issue so the
// author sees it where they live (the git host), not only inside agentwork.
// The question is relayed verbatim — it is the agent's own words, the same as
// a developer commenting "I need X to proceed" on an issue. No platform
// branding, no goal id; the bot account's display name is the identity.
// Non-issue goals no-op (resolveIssueTarget returns false).
func (c *Closer) OnAgentQuestion(ctx context.Context, goalID, question string) {
	q := strings.TrimSpace(question)
	if q == "" {
		return
	}
	t, ok := c.resolveIssueTarget(ctx, goalID)
	if !ok {
		return
	}
	if err := t.postComment(ctx, q); err != nil {
		fmt.Printf("issue: agent question %s:%s#%d: %v\n", t.provider, t.repo, t.number, err)
	}
}

// isHexSHA reports whether s looks like a full git sha.
func isHexSHA(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// verifyWebhook checks the platform's signature header:
//
//	github:  X-Hub-Signature-256: sha256=<hex HMAC-SHA256(secret, body)>
//	gitcode: X-GitCode-Signature-256: sha256=<hex>  (signature-key mode)
//	         X-GitCode-Token: <password>            (password mode)
func verifyWebhook(provider, secret string, body []byte, signature, tokenHeader string) bool {
	if provider == "gitcode" {
		if tokenHeader != "" {
			return hmac.Equal([]byte(tokenHeader), []byte(secret))
		}
		return verifySignature(secret, body, signature)
	}
	return verifySignature(secret, body, signature)
}

// ── Webhook: real-time trigger (M4-B) ──

// WebhookHandler receives a hosting platform's `issues` webhook events
// (opened/reopened) and creates goals immediately — the real-time trigger,
// complementing the poller (the safety net for missed deliveries). One
// handler per provider (github / gitcode): the signature header and payload
// shape differ per platform. Comment events are ignored — the run-start
// comment fetch already covers the dialogue channel.
type WebhookHandler struct {
	provider string
	st       *store.Store
	poller   *Poller
	// getSecret resolves the shared webhook secret ('' = webhook disabled).
	getSecret func(ctx context.Context) (string, error)
}

func NewWebhookHandler(provider string, st *store.Store, poller *Poller, getSecret func(ctx context.Context) (string, error)) *WebhookHandler {
	return &WebhookHandler{provider: provider, st: st, poller: poller, getSecret: getSecret}
}

// webhookEvent is the GitHub `issues` / `pull_request` event payload
// (minimal shape). PullRequest is non-nil for PR events.
type webhookEvent struct {
	Action      string              `json:"action"`
	Issue       Issue               `json:"issue"`
	PullRequest *pullRequestPayload `json:"pull_request"`
	Repository  struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// pullRequestPayload is the PR object inside a `pull_request` webhook event
// (GitHub) or the GitCode merge_request conversion. Head.SHA is the PR's head
// commit AFTER the update — the update-detection signal (a synchronize push
// moves it; an edited description does not). Head.Ref/Base.Ref are the
// source/target branches (GitHub carries them; the GitCode conversion leaves
// them empty).
type pullRequestPayload struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// Handle processes one webhook delivery. Returns an HTTP status + a
// user-facing error (nil error = 200).
func (h *WebhookHandler) Handle(ctx context.Context, body []byte, signature, tokenHeader string) error {
	secret, err := h.getSecret(ctx)
	if err != nil {
		return fmt.Errorf("webhook secret lookup: %w", err)
	}
	if secret == "" {
		return fmt.Errorf("webhook not configured (no secret)")
	}
	// GitHub: X-Hub-Signature-256. GitCode: X-GitCode-Signature-256 (same
	// HMAC-SHA256) or X-GitCode-Token (plain password mode).
	if !verifyWebhook(h.provider, secret, body, signature, tokenHeader) {
		return fmt.Errorf("bad signature")
	}
	var ev webhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return fmt.Errorf("parse webhook payload: %w", err)
	}
	// GitCode merge_request webhooks use a completely different payload
	// format from GitHub. If the standard parse yielded nothing, try the
	// GitCode shape (event_type/object_kind = merge_request, iid,
	// project.path_with_namespace) and convert it into the standard shapes.
	if ev.Action == "" && ev.Issue.Number == 0 && ev.PullRequest == nil {
		var gc gitCodeWebhookEvent
		if err := json.Unmarshal(body, &gc); err == nil &&
			(gc.EventType == "merge_request" || gc.ObjectKind == "merge_request") && gc.MergeRequest.IID > 0 {
			// Map GitCode's MR action to the standard vocabulary: open→opened,
			// reopen→reopened, update→synchronize (the update signal); close and
			// merge pass through and are filtered below — the human's decisions.
			action := gc.MergeRequest.Action
			switch action {
			case "open":
				action = "opened"
			case "reopen", "reopened":
				action = "reopened"
			case "update":
				action = "synchronize"
			}
			ev.Action = action
			repo := gc.Project.PathWithNamespace
			if repo == "" {
				repo = gc.Repository.FullName
			}
			ev.Repository.FullName = repo
			ev.PullRequest = &pullRequestPayload{
				Number:  gc.MergeRequest.IID,
				Title:   gc.MergeRequest.Title,
				Body:    gc.MergeRequest.Description,
				HTMLURL: gc.MergeRequest.URL,
			}
			ev.PullRequest.Head.SHA = gc.MergeRequest.LastCommit.ID
		}
	}
	if ev.PullRequest != nil && ev.PullRequest.Number > 0 {
		// PR event — the shared Pull shape (branches + head SHA drive the
		// goal's description and the update detection).
		pr := Pull{
			Number:       ev.PullRequest.Number,
			Title:        ev.PullRequest.Title,
			Body:         ev.PullRequest.Body,
			HTMLURL:      ev.PullRequest.HTMLURL,
			SourceBranch: ev.PullRequest.Head.Ref,
			TargetBranch: ev.PullRequest.Base.Ref,
			HeadSHA:      ev.PullRequest.Head.SHA,
		}
		if ev.Repository.FullName == "" {
			return nil
		}
		return h.handlePullDelivery(ctx, ev.Action, ev.Repository.FullName, pr)
	}
	if ev.Action != "opened" && ev.Action != "reopened" {
		return nil // only issue creation triggers goals
	}
	if ev.Repository.FullName == "" || ev.Issue.Number == 0 {
		return nil
	}
	// Route to the domain tracking this repo (provider must match the
	// endpoint — a github delivery on the gitcode endpoint is not ours).
	var domainID, assignee, assigneeType string
	err = h.st.DB().QueryRowContext(ctx,
		`SELECT id, issue_assignee, issue_assignee_type FROM domain WHERE issue_repo=? AND issue_provider=?`, ev.Repository.FullName, h.provider).
		Scan(&domainID, &assignee, &assigneeType)
	if err != nil {
		return nil // repo not tracked — nothing to do (not an error)
	}
	if assignee == "" {
		return nil
	}
	if assigneeType == "" {
		assigneeType = "agent"
	}
	_, err = h.poller.CreateGoalForIssue(ctx, domainID, assignee, assigneeType, h.provider, ev.Repository.FullName, ev.Issue)
	return err
}

// gitCodeWebhookEvent is GitCode's proprietary merge_request webhook payload —
// a completely different shape from GitHub's pull_request event (verified
// against live GitCode deliveries): event_type/object_kind = "merge_request",
// the MR under merge_request (iid, action, title, description, last_commit),
// the repo under project.path_with_namespace.
type gitCodeWebhookEvent struct {
	EventType    string `json:"event_type"`
	ObjectKind   string `json:"object_kind"`
	MergeRequest struct {
		Action      string `json:"action"`
		IID         int    `json:"iid"`
		Title       string `json:"title"`
		Description string `json:"description"`
		URL         string `json:"url"`
		LastCommit  struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"merge_request"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// handlePullDelivery routes one PR webhook delivery: opened/reopened create
// the goal (idempotent — the poller's safety net converges on the same
// source_ref); synchronize/edited refresh the EXISTING goal (the same PR
// pushed new commits or edited its description — never a second goal).
// close/merge and everything else is the human's decision, not ours.
// The domain's PR 拉取处理方 column (pull_assignee) decides whether this
// domain takes PRs at all.
func (h *WebhookHandler) handlePullDelivery(ctx context.Context, action, repo string, pr Pull) error {
	var domainID, assignee, assigneeType string
	err := h.st.DB().QueryRowContext(ctx,
		`SELECT id, COALESCE(pull_assignee,''), COALESCE(pull_assignee_type,'') FROM domain WHERE issue_repo=? AND issue_provider=?`, repo, h.provider).
		Scan(&domainID, &assignee, &assigneeType)
	if err != nil {
		return nil // repo not tracked — nothing to do (not an error)
	}
	if assignee == "" {
		return nil // no PR handler configured — the domain takes issues only
	}
	if assigneeType == "" {
		assigneeType = "agent"
	}
	switch action {
	case "opened", "reopened":
		_, err := h.poller.CreateGoalForPull(ctx, domainID, assignee, assigneeType, h.provider, repo, pr)
		return err
	case "synchronize", "edited":
		_, err := h.poller.UpdateGoalForPull(ctx, domainID, assignee, assigneeType, h.provider, repo, pr)
		return err
	}
	return nil
}

// verifySignature checks X-Hub-Signature-256 ("sha256=<hex HMAC-SHA256>").
func verifySignature(secret string, body []byte, signature string) bool {
	got := "sha256=" + hmacSHA256Hex(secret, body)
	return hmac.Equal([]byte(got), []byte(signature))
}

func hmacSHA256Hex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
