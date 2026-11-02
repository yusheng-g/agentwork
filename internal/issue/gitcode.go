package issue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitCodeClient is the GitCode (gitcode.com) issue API adapter (M4-B):
// api.gitcode.com/api/v5, access_token query auth. The provider implements
// the same Provider surface as GitHub, so the poller/closer/comment paths
// are provider-agnostic.
type GitCodeClient struct {
	token   string
	http    *http.Client
	baseURL string
}

func NewGitCodeClient(token string) *GitCodeClient {
	return &GitCodeClient{token: token, http: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.gitcode.com/api/v5"}
}

func (c *GitCodeClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var rdr io.Reader
	if form != nil {
		rdr = strings.NewReader(form.Encode())
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u := c.baseURL + path + sep + "access_token=" + url.QueryEscape(c.token)
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("gitcode %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// gitCodeIssue is GitCode's issue wire shape — its number is a STRING
// ("1"), unlike GitHub's int (the provider difference the adapter exists
// for). Parsed into the common Issue with an int number.
type gitCodeIssue struct {
	Number  string `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// ListOpenIssues returns the repo's open issues (GitCode issues have no PR
// rows — merge requests are a separate collection — so no PR filtering).
func (c *GitCodeClient) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	var raw []gitCodeIssue
	err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/issues?state=open&per_page=100", nil, &raw)
	if err != nil {
		return nil, err
	}
	out := []Issue{}
	for _, i := range raw {
		n, err := strconv.Atoi(i.Number)
		if err != nil {
			continue // a malformed number is not an issue we can track
		}
		out = append(out, Issue{Number: n, Title: i.Title, Body: i.Body, HTMLURL: i.HTMLURL})
	}
	return out, nil
}

// gitCodeComment mirrors GitCode's comment shape (id is a string too).
type gitCodeComment struct {
	ID   string `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (c *GitCodeClient) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	var raw []gitCodeComment
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number), nil, &raw)
	if err != nil {
		return nil, err
	}
	out := []Comment{}
	for _, cm := range raw {
		id, _ := strconv.Atoi(cm.ID)
		out = append(out, Comment{ID: id, Body: cm.Body, User: struct {
			Login string `json:"login"`
		}{Login: cm.User.Login}})
	}
	return out, nil
}

func (c *GitCodeClient) CreateComment(ctx context.Context, repo string, number int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number),
		url.Values{"body": {body}}, nil)
}

// CloseIssue: GitCode closes via PATCH /repos/{owner}/issues/{number} with
// formData (repo, number, state="close") — note the path carries owner but
// NOT the repo segment.
func (c *GitCodeClient) CloseIssue(ctx context.Context, repo string, number int) error {
	owner, repoName, ok := strings.Cut(repo, "/")
	if !ok {
		return fmt.Errorf("gitcode: bad repo %q (want owner/repo)", repo)
	}
	return c.do(ctx, http.MethodPatch, "/repos/"+owner+"/issues/"+strconv.Itoa(number),
		url.Values{"repo": {repoName}, "number": {strconv.Itoa(number)}, "state": {"close"}}, nil)
}

// CommitURL is the GitCode commit link (verified: /commit/<sha> resolves).
func (c *GitCodeClient) CommitURL(repo, sha string) string {
	return "https://gitcode.com/" + repo + "/commit/" + sha
}

// ── Pull-request surface (PR pull) ──

// flexInt decodes a JSON number OR a JSON string holding digits (GitCode's
// wire shapes have varied across revisions: the issue number is a STRING,
// the documented PR number an INTEGER — accept both instead of betting on
// one).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("flexInt: %q: %w", s, err)
	}
	*f = flexInt(n)
	return nil
}

// gitCodePull is GitCode's PR wire shape (minimal): head carries the source
// branch + SHA, base the target branch. A draft PR (draft=true) is skipped
// by the poller — a draft is not a work request yet.
type gitCodePull struct {
	Number  flexInt `json:"number"`
	Title   string  `json:"title"`
	Body    string  `json:"body"`
	HTMLURL string  `json:"html_url"`
	Draft   bool    `json:"draft"`
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

// ListOpenPulls returns the repo's open PRs. GitCode keeps pull requests in
// a separate /pulls collection (issues and PRs are separate sequences — PRs
// never appear in the issues list). Draft PRs are excluded: a draft is not
// ready for review.
func (c *GitCodeClient) ListOpenPulls(ctx context.Context, repo string) ([]Pull, error) {
	var raw []gitCodePull
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?state=open&per_page=100", nil, &raw); err != nil {
		return nil, err
	}
	out := []Pull{}
	for _, pr := range raw {
		if pr.Draft {
			continue
		}
		out = append(out, Pull{
			Number: int(pr.Number), Title: pr.Title, Body: pr.Body, HTMLURL: pr.HTMLURL,
			Author: pr.User.Login, SourceBranch: pr.Head.Ref, TargetBranch: pr.Base.Ref,
			HeadSHA: pr.Head.SHA,
		})
	}
	return out, nil
}

// ListPullComments returns the PR's comments. GitCode serves PR dialogue
// through a DEDICATED /pulls/:n/comments endpoint — the issues endpoint
// does NOT cover PR comments (the provider difference this surface exists
// for).
func (c *GitCodeClient) ListPullComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	var raw []gitCodeComment
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100", repo, number), nil, &raw); err != nil {
		return nil, err
	}
	out := []Comment{}
	for _, cm := range raw {
		id, _ := strconv.Atoi(cm.ID)
		out = append(out, Comment{ID: id, Body: cm.Body, User: struct {
			Login string `json:"login"`
		}{Login: cm.User.Login}})
	}
	return out, nil
}

// CreatePullComment posts a comment on the PR (the PR comments endpoint).
func (c *GitCodeClient) CreatePullComment(ctx context.Context, repo string, number int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/comments", repo, number),
		url.Values{"body": {body}}, nil)
}
