package issue

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SourceRefKind distinguishes an issue-sourced goal from a
// pull-request-sourced one. The kind decides the writeback routing (a PR's
// dialogue lives on a separate API surface) and the delivery semantics (an
// issue closes on deliver; a PR never does — merge/close is the human's
// decision).
type SourceRefKind string

const (
	RefIssue SourceRefKind = "issue"
	RefPull  SourceRefKind = "pull"
)

// Provider abstracts a Git-hosting platform's issue API (M4-B): the adapter
// must not be GitHub-only — GitCode, GitLab, Gitee etc. speak different REST
// shapes and webhook signatures. The domain config selects the provider; the
// rest of the platform (poller, closer, comment API, webhook) talks to the
// interface only.
type Provider interface {
	ListOpenIssues(ctx context.Context, repo string) ([]Issue, error)
	ListComments(ctx context.Context, repo string, number int) ([]Comment, error)
	CreateComment(ctx context.Context, repo string, number int, body string) error
	CloseIssue(ctx context.Context, repo string, number int) error
	// CommitURL builds the human-clickable commit link — the close comment's
	// fix evidence. The domain is the provider's own (github.com,
	// gitcode.com, ...), never guessed by the caller.
	CommitURL(repo, sha string) string

	// ── Pull-request surface (PR pull) ──

	// ListOpenPulls returns the repo's open pull requests — the PR-poll
	// trigger (the pull-request analogue of ListOpenIssues). GitHub serves
	// PRs through /pulls (they also appear in the issues list, filtered
	// there); GitCode keeps a separate /pulls collection.
	ListOpenPulls(ctx context.Context, repo string) ([]Pull, error)
	// ListPullComments returns the PR's comments. GitHub serves PR comments
	// through the issues-comments endpoint; GitCode has a dedicated
	// /pulls/:n/comments one — the provider difference this surface exists
	// for.
	ListPullComments(ctx context.Context, repo string, number int) ([]Comment, error)
	// CreatePullComment posts a comment on the PR.
	CreatePullComment(ctx context.Context, repo string, number int, body string) error
}

// NewProvider builds the provider named by the domain's issue_provider
// setting ("github" | "gitcode").
func NewProvider(name, token string) (Provider, error) {
	switch name {
	case "", "github":
		return NewClient(token), nil
	case "gitcode":
		return NewGitCodeClient(token), nil
	default:
		return nil, fmt.Errorf("unknown issue provider %q (github|gitcode)", name)
	}
}

// SourceRef encodes an issue as the goal's source_ref, provider-qualified so
// the closer/comment paths can route back to the right API.
func SourceRef(provider, repo string, number int) string {
	return provider + ":" + repo + "#" + strconv.Itoa(number)
}

// PullSourceRef encodes a pull request as the goal's source_ref. The "!"
// suffix (the merge-request convention) keeps PR refs distinct from issue
// refs: issue and PR numbers are SEPARATE sequences on GitCode
// (GitLab-style), so "gitcode:repo#7" would be ambiguous between the two.
func PullSourceRef(provider, repo string, number int) string {
	return provider + ":" + repo + "!" + strconv.Itoa(number)
}

// ParseSourceRef decodes a source_ref ("github:owner/repo#123" /
// "gitcode:owner/repo#123" for issues, "github:owner/repo!4" /
// "gitcode:owner/repo!4" for pull requests); ok=false for anything else.
// The kind routes every writeback path (comment endpoint, close-vs-not).
func ParseSourceRef(ref string) (provider, repo string, number int, kind SourceRefKind, ok bool) {
	rest, found := strings.CutPrefix(ref, "github:")
	if !found {
		rest, found = strings.CutPrefix(ref, "gitcode:")
		if !found {
			return "", "", 0, "", false
		}
		provider = "gitcode"
	} else {
		provider = "github"
	}
	repoPart, numPart, found := strings.Cut(rest, "#")
	kind = RefIssue
	if !found {
		repoPart, numPart, found = strings.Cut(rest, "!")
		if !found {
			return "", "", 0, "", false
		}
		kind = RefPull
	}
	if repoPart == "" {
		return "", "", 0, "", false
	}
	n, err := strconv.Atoi(numPart)
	if err != nil {
		return "", "", 0, "", false
	}
	return provider, repoPart, n, kind, true
}
