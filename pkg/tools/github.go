package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHubIssue is a brief summary of a GitHub issue.
type GitHubIssue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

// GitHubPR is a brief summary of a GitHub pull request.
type GitHubPR struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	Author     string `json:"author"`
	CreatedAt  string `json:"created_at"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
}

// GitHubCommit is a brief summary of a GitHub commit.
type GitHubCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// GitHubCIStatus is the combined status and check-run summary for a ref.
type GitHubCIStatus struct {
	State      string             `json:"state"`
	CheckRuns  []GitHubCheckRun   `json:"check_runs"`
	Statuses   []GitHubStatus     `json:"statuses"`
}

// GitHubCheckRun is a brief summary of a single check run.
type GitHubCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
}

// GitHubStatus is a brief summary of a single legacy status.
type GitHubStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description,omitempty"`
}

// GitHubToolset holds the GitHub tools that share a PAT and watched-repo list.
type GitHubToolset struct {
	pat          string
	watchedRepos []string
	stateDir     string
	proposals    *GitHubProposalStore
	httpClient   *http.Client
}

// NewGitHubToolset constructs a GitHubToolset.
// pat must be non-empty; stateDir is created if it does not exist.
func NewGitHubToolset(pat string, watchedRepos []string, stateDir string) (*GitHubToolset, error) {
	if pat == "" {
		return nil, fmt.Errorf("github: PAT must not be empty")
	}
	if stateDir == "" {
		return nil, fmt.Errorf("github: stateDir must not be empty")
	}
	ts := &GitHubToolset{
		pat:          pat,
		watchedRepos: watchedRepos,
		stateDir:     stateDir,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
	}
	ts.proposals = NewGitHubProposalStore(stateDir, ts)
	return ts, nil
}

// Tools returns all GitHub tool implementations.
func (ts *GitHubToolset) Tools() []Tool {
	return []Tool{
		&githubWatchedReposTool{ts: ts},
		&githubOpenIssuesTool{ts: ts},
		&githubOpenPRsTool{ts: ts},
		&githubRecentCommitsTool{ts: ts},
		&githubCIStatusTool{ts: ts},
		&githubGetIssueBodyTool{ts: ts},
		&githubCreateIssueProposalTool{ts: ts},
	}
}

// Proposals returns the proposal store for /apply and /reject dispatch.
func (ts *GitHubToolset) Proposals() *GitHubProposalStore {
	return ts.proposals
}

// resolveRepo accepts "owner/repo" (full form) or "repo" (short form).
// Short form is matched against the watched list: exactly one match required.
// Returns an error if the resolved repo is not in the watched list.
// ResolveRepo exports resolveRepo for callers outside the package
// (e.g. the agent's /claude slash command handler).
func (ts *GitHubToolset) ResolveRepo(input string) (string, error) {
	return ts.resolveRepo(input)
}

func (ts *GitHubToolset) resolveRepo(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("repo is required")
	}
	if strings.Contains(input, "/") {
		// Full form — must be in the watched list.
		for _, r := range ts.watchedRepos {
			if strings.EqualFold(r, input) {
				return r, nil
			}
		}
		return "", fmt.Errorf("repo %q is not in the watched list", input)
	}
	// Short form — scan for matches.
	var matches []string
	for _, r := range ts.watchedRepos {
		parts := strings.SplitN(r, "/", 2)
		if len(parts) == 2 && strings.EqualFold(parts[1], input) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("repo %q not found in the watched list", input)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("repo name %q is ambiguous; use full owner/repo form: %s", input, strings.Join(matches, ", "))
	}
}

// apiGet performs an authenticated GET request to the GitHub REST API.
// path must start with "/" and is relative to https://api.github.com.
func (ts *GitHubToolset) apiGet(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("github: failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+ts.pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := ts.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("github: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("github: failed to read response: %w", err)
	}
	return body, resp.StatusCode, nil
}

// CreateIssue creates a GitHub issue directly (called by ApplyGitHubProposal).
func (ts *GitHubToolset) CreateIssue(ctx context.Context, repo, title, body string) (int, string, error) {
	payload := map[string]string{"title": title, "body": body}
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, "", fmt.Errorf("github: failed to encode issue payload: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://api.github.com/repos/"+repo+"/issues",
		strings.NewReader(string(data)),
	)
	if err != nil {
		return 0, "", fmt.Errorf("github: failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+ts.pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("github: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("github: failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return 0, "", fmt.Errorf("github: create issue returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, "", fmt.Errorf("github: failed to parse created issue: %w", err)
	}
	return result.Number, result.HTMLURL, nil
}

// ---------------------------------------------------------------------------
// github_watched_repos
// ---------------------------------------------------------------------------

type githubWatchedReposTool struct{ ts *GitHubToolset }

func (t *githubWatchedReposTool) Name() string { return "github_watched_repos" }
func (t *githubWatchedReposTool) Description() string {
	return "Return the list of configured watched GitHub repositories."
}
func (t *githubWatchedReposTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
func (t *githubWatchedReposTool) Execute(_ context.Context, _ map[string]any) *ToolResult {
	repos := t.ts.watchedRepos
	if repos == nil {
		repos = []string{}
	}
	data, err := json.MarshalIndent(repos, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize repos: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// github_open_issues
// ---------------------------------------------------------------------------

type githubOpenIssuesTool struct{ ts *GitHubToolset }

func (t *githubOpenIssuesTool) Name() string { return "github_open_issues" }
func (t *githubOpenIssuesTool) Description() string {
	return "List open issues for a watched GitHub repository. Returns number, title, author, and created_at."
}
func (t *githubOpenIssuesTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{
				"type":        "string",
				"description": "Repository in owner/repo form, or just the repo name if unambiguous.",
			},
		},
		"required": []string{"repo"},
	}
}
func (t *githubOpenIssuesTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	repoArg, _ := args["repo"].(string)
	repo, err := t.ts.resolveRepo(repoArg)
	if err != nil {
		return ErrorResult(err.Error())
	}

	body, status, err := t.ts.apiGet(ctx, "/repos/"+repo+"/issues?state=open&per_page=50&pulls=false")
	if err != nil {
		return ErrorResult(fmt.Sprintf("github_open_issues: %s", err.Error()))
	}
	if status != http.StatusOK {
		return ErrorResult(fmt.Sprintf("github_open_issues: API returned %d", status))
	}

	var raw []struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		PullRequest *struct{} `json:"pull_request,omitempty"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ErrorResult("github_open_issues: failed to parse response: " + err.Error())
	}

	issues := make([]GitHubIssue, 0, len(raw))
	for _, r := range raw {
		// The issues endpoint returns PRs too; exclude them.
		if r.PullRequest != nil {
			continue
		}
		issues = append(issues, GitHubIssue{
			Number:    r.Number,
			Title:     r.Title,
			Author:    r.User.Login,
			CreatedAt: r.CreatedAt,
		})
	}

	data, err := json.MarshalIndent(issues, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize issues: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// github_open_prs
// ---------------------------------------------------------------------------

type githubOpenPRsTool struct{ ts *GitHubToolset }

func (t *githubOpenPRsTool) Name() string { return "github_open_prs" }
func (t *githubOpenPRsTool) Description() string {
	return "List open pull requests for a watched GitHub repository. Returns number, title, author, created_at, and branch info."
}
func (t *githubOpenPRsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{
				"type":        "string",
				"description": "Repository in owner/repo form, or just the repo name if unambiguous.",
			},
		},
		"required": []string{"repo"},
	}
}
func (t *githubOpenPRsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	repoArg, _ := args["repo"].(string)
	repo, err := t.ts.resolveRepo(repoArg)
	if err != nil {
		return ErrorResult(err.Error())
	}

	body, status, err := t.ts.apiGet(ctx, "/repos/"+repo+"/pulls?state=open&per_page=50")
	if err != nil {
		return ErrorResult(fmt.Sprintf("github_open_prs: %s", err.Error()))
	}
	if status != http.StatusOK {
		return ErrorResult(fmt.Sprintf("github_open_prs: API returned %d", status))
	}

	var raw []struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ErrorResult("github_open_prs: failed to parse response: " + err.Error())
	}

	prs := make([]GitHubPR, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, GitHubPR{
			Number:     r.Number,
			Title:      r.Title,
			Author:     r.User.Login,
			CreatedAt:  r.CreatedAt,
			HeadBranch: r.Head.Ref,
			BaseBranch: r.Base.Ref,
		})
	}

	data, err := json.MarshalIndent(prs, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize PRs: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// github_recent_commits
// ---------------------------------------------------------------------------

type githubRecentCommitsTool struct{ ts *GitHubToolset }

func (t *githubRecentCommitsTool) Name() string { return "github_recent_commits" }
func (t *githubRecentCommitsTool) Description() string {
	return "List recent commits for a watched GitHub repository since the given date."
}
func (t *githubRecentCommitsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{
				"type":        "string",
				"description": "Repository in owner/repo form, or just the repo name if unambiguous.",
			},
			"since": map[string]any{
				"type":        "string",
				"description": "RFC3339 timestamp; only commits after this time are returned. Defaults to 7 days ago.",
			},
		},
		"required": []string{"repo"},
	}
}
func (t *githubRecentCommitsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	repoArg, _ := args["repo"].(string)
	repo, err := t.ts.resolveRepo(repoArg)
	if err != nil {
		return ErrorResult(err.Error())
	}

	since := time.Now().AddDate(0, 0, -7).UTC().Format(time.RFC3339)
	if sinceArg, ok := args["since"].(string); ok && strings.TrimSpace(sinceArg) != "" {
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(sinceArg)); err != nil {
			return ErrorResult(fmt.Sprintf("invalid since value %q: must be RFC3339", sinceArg))
		}
		since = strings.TrimSpace(sinceArg)
	}

	path := fmt.Sprintf("/repos/%s/commits?since=%s&per_page=50", repo, since)
	body, status, err := t.ts.apiGet(ctx, path)
	if err != nil {
		return ErrorResult(fmt.Sprintf("github_recent_commits: %s", err.Error()))
	}
	if status != http.StatusOK {
		return ErrorResult(fmt.Sprintf("github_recent_commits: API returned %d", status))
	}

	var raw []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
				Date string `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ErrorResult("github_recent_commits: failed to parse response: " + err.Error())
	}

	commits := make([]GitHubCommit, 0, len(raw))
	for _, r := range raw {
		short := r.SHA
		if len(short) > 7 {
			short = short[:7]
		}
		// Use first line of commit message only.
		msg := r.Commit.Message
		if idx := strings.Index(msg, "\n"); idx >= 0 {
			msg = msg[:idx]
		}
		commits = append(commits, GitHubCommit{
			SHA:     short,
			Message: msg,
			Author:  r.Commit.Author.Name,
			Date:    r.Commit.Author.Date,
		})
	}

	data, err := json.MarshalIndent(commits, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize commits: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// github_ci_status
// ---------------------------------------------------------------------------

type githubCIStatusTool struct{ ts *GitHubToolset }

func (t *githubCIStatusTool) Name() string { return "github_ci_status" }
func (t *githubCIStatusTool) Description() string {
	return "Return the combined CI status for a commit ref in a watched repository. Includes both legacy statuses and check-runs."
}
func (t *githubCIStatusTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{
				"type":        "string",
				"description": "Repository in owner/repo form, or just the repo name if unambiguous.",
			},
			"ref": map[string]any{
				"type":        "string",
				"description": "Commit SHA, branch name, or tag to check CI status for.",
			},
		},
		"required": []string{"repo", "ref"},
	}
}
func (t *githubCIStatusTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	repoArg, _ := args["repo"].(string)
	repo, err := t.ts.resolveRepo(repoArg)
	if err != nil {
		return ErrorResult(err.Error())
	}

	ref, _ := args["ref"].(string)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ErrorResult("ref is required")
	}

	// Fetch combined status (legacy).
	statusBody, statusCode, err := t.ts.apiGet(ctx, "/repos/"+repo+"/commits/"+ref+"/status")
	if err != nil {
		return ErrorResult(fmt.Sprintf("github_ci_status: %s", err.Error()))
	}
	if statusCode != http.StatusOK {
		return ErrorResult(fmt.Sprintf("github_ci_status: combined status API returned %d", statusCode))
	}

	var combinedStatus struct {
		State    string `json:"state"`
		Statuses []struct {
			Context     string `json:"context"`
			State       string `json:"state"`
			Description string `json:"description"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(statusBody, &combinedStatus); err != nil {
		return ErrorResult("github_ci_status: failed to parse combined status: " + err.Error())
	}

	// Fetch check-runs (modern CI).
	checkBody, checkCode, err := t.ts.apiGet(ctx, "/repos/"+repo+"/commits/"+ref+"/check-runs?per_page=50")
	if err != nil {
		return ErrorResult(fmt.Sprintf("github_ci_status: %s", err.Error()))
	}
	if checkCode != http.StatusOK {
		return ErrorResult(fmt.Sprintf("github_ci_status: check-runs API returned %d", checkCode))
	}

	var checkResp struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(checkBody, &checkResp); err != nil {
		return ErrorResult("github_ci_status: failed to parse check-runs: " + err.Error())
	}

	statuses := make([]GitHubStatus, 0, len(combinedStatus.Statuses))
	for _, s := range combinedStatus.Statuses {
		statuses = append(statuses, GitHubStatus{
			Context:     s.Context,
			State:       s.State,
			Description: s.Description,
		})
	}

	checkRuns := make([]GitHubCheckRun, 0, len(checkResp.CheckRuns))
	for _, c := range checkResp.CheckRuns {
		checkRuns = append(checkRuns, GitHubCheckRun{
			Name:       c.Name,
			Status:     c.Status,
			Conclusion: c.Conclusion,
		})
	}

	result := GitHubCIStatus{
		State:     combinedStatus.State,
		CheckRuns: checkRuns,
		Statuses:  statuses,
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize CI status: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// github_get_issue_body
// ---------------------------------------------------------------------------

type githubGetIssueBodyTool struct{ ts *GitHubToolset }

func (t *githubGetIssueBodyTool) Name() string { return "github_get_issue_body" }
func (t *githubGetIssueBodyTool) Description() string {
	return "Fetch the full body text of a single GitHub issue by number."
}
func (t *githubGetIssueBodyTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{
				"type":        "string",
				"description": "Repository in owner/repo form, or just the repo name if unambiguous.",
			},
			"issue_number": map[string]any{
				"type":        "integer",
				"description": "Issue number to fetch.",
			},
		},
		"required": []string{"repo", "issue_number"},
	}
}
func (t *githubGetIssueBodyTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	repoArg, _ := args["repo"].(string)
	repo, err := t.ts.resolveRepo(repoArg)
	if err != nil {
		return ErrorResult(err.Error())
	}

	var issueNum int
	switch v := args["issue_number"].(type) {
	case float64:
		issueNum = int(v)
	case int:
		issueNum = v
	case int64:
		issueNum = int(v)
	}
	if issueNum <= 0 {
		return ErrorResult("issue_number must be a positive integer")
	}

	body, status, err := t.ts.apiGet(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, issueNum))
	if err != nil {
		return ErrorResult(fmt.Sprintf("github_get_issue_body: %s", err.Error()))
	}
	if status == http.StatusNotFound {
		return ErrorResult(fmt.Sprintf("issue #%d not found in %s", issueNum, repo))
	}
	if status != http.StatusOK {
		return ErrorResult(fmt.Sprintf("github_get_issue_body: API returned %d", status))
	}

	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		CreatedAt string `json:"created_at"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ErrorResult("github_get_issue_body: failed to parse response: " + err.Error())
	}

	type issueDetail struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Author    string `json:"author"`
		CreatedAt string `json:"created_at"`
		State     string `json:"state"`
		Body      string `json:"body"`
	}
	result := issueDetail{
		Number:    raw.Number,
		Title:     raw.Title,
		Author:    raw.User.Login,
		CreatedAt: raw.CreatedAt,
		State:     raw.State,
		Body:      raw.Body,
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize issue: " + err.Error())
	}
	return NewToolResult(string(data))
}
