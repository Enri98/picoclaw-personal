package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GitHubProposal holds a pending issue-creation proposal awaiting user approval.
type GitHubProposal struct {
	ID        string `json:"id"`         // UUIDv4
	Repo      string `json:"repo"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Reason    string `json:"reason"`
	ExpiresAt int64  `json:"expires_at"` // unix seconds, 15-min TTL
}

// GitHubProposalStore persists pending issue proposals to a JSON file.
type GitHubProposalStore struct {
	stateDir string
	ts       *GitHubToolset
	mu       sync.Mutex
}

// NewGitHubProposalStore returns a GitHubProposalStore backed by
// ${stateDir}/github_proposals.json.
func NewGitHubProposalStore(stateDir string, ts *GitHubToolset) *GitHubProposalStore {
	return &GitHubProposalStore{
		stateDir: filepath.Clean(stateDir),
		ts:       ts,
	}
}

func (s *GitHubProposalStore) proposalsPath() string {
	return filepath.Join(s.stateDir, "github_proposals.json")
}

func (s *GitHubProposalStore) load() ([]GitHubProposal, error) {
	data, err := os.ReadFile(s.proposalsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []GitHubProposal
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *GitHubProposalStore) save(proposals []GitHubProposal) error {
	if err := os.MkdirAll(s.stateDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(proposals, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.proposalsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.proposalsPath())
}

// purgeExpired removes proposals past their expiry; caller must hold s.mu.
func (s *GitHubProposalStore) purgeExpired(proposals []GitHubProposal) []GitHubProposal {
	now := time.Now().Unix()
	out := proposals[:0]
	for _, p := range proposals {
		if p.ExpiresAt > now {
			out = append(out, p)
		}
	}
	return out
}

// Propose creates a new proposal with a freshly generated UUID and stores it.
func (s *GitHubProposalStore) Propose(p GitHubProposal) (GitHubProposal, error) {
	id, err := newProposalID()
	if err != nil {
		return GitHubProposal{}, fmt.Errorf("failed to generate proposal ID: %w", err)
	}
	return s.proposeWithID(id, p)
}

// proposeWithID creates and stores a proposal using a caller-supplied ID.
func (s *GitHubProposalStore) proposeWithID(id string, p GitHubProposal) (GitHubProposal, error) {
	p.ID = id
	p.ExpiresAt = time.Now().Add(15 * time.Minute).Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.load()
	if err != nil {
		return GitHubProposal{}, fmt.Errorf("failed to load proposals: %w", err)
	}
	existing = s.purgeExpired(existing)
	existing = append(existing, p)
	if err := s.save(existing); err != nil {
		return GitHubProposal{}, fmt.Errorf("failed to save proposal: %w", err)
	}
	return p, nil
}

// Apply finds the proposal by id, removes it (replay-prevention), then
// calls CreateIssue on the GitHub API.
func (s *GitHubProposalStore) Apply(ctx context.Context, id string) (int, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, "", fmt.Errorf("proposal ID is required")
	}

	s.mu.Lock()

	proposals, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return 0, "", fmt.Errorf("failed to load proposals: %w", err)
	}

	// Distinguish expired-and-purged from never-existed.
	nowSec := time.Now().Unix()
	for _, p := range proposals {
		if p.ID == id && p.ExpiresAt <= nowSec {
			proposals = s.purgeExpired(proposals)
			_ = s.save(proposals)
			s.mu.Unlock()
			return 0, "", fmt.Errorf("proposal %s has expired; re-propose to proceed", id)
		}
	}
	proposals = s.purgeExpired(proposals)

	var found GitHubProposal
	foundOK := false
	remaining := make([]GitHubProposal, 0, len(proposals))
	for _, p := range proposals {
		if !foundOK && p.ID == id {
			found = p
			foundOK = true
		} else {
			remaining = append(remaining, p)
		}
	}
	if !foundOK {
		s.mu.Unlock()
		return 0, "", fmt.Errorf("no active proposal with ID %s", id)
	}

	// Remove proposal before calling the API — prevents replay on network error.
	if err := s.save(remaining); err != nil {
		s.mu.Unlock()
		return 0, "", fmt.Errorf("failed to remove proposal: %w", err)
	}
	s.mu.Unlock()

	return s.ts.CreateIssue(ctx, found.Repo, found.Title, found.Body)
}

// Reject removes a proposal by id without creating an issue.
func (s *GitHubProposalStore) Reject(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("proposal ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	proposals, err := s.load()
	if err != nil {
		return fmt.Errorf("failed to load proposals: %w", err)
	}
	proposals = s.purgeExpired(proposals)

	remaining := make([]GitHubProposal, 0, len(proposals))
	found := false
	for _, p := range proposals {
		if !found && p.ID == id {
			found = true
		} else {
			remaining = append(remaining, p)
		}
	}
	if !found {
		return fmt.Errorf("no active proposal with ID %s", id)
	}
	return s.save(remaining)
}

// List returns all active (non-expired) proposals.
func (s *GitHubProposalStore) List() []GitHubProposal {
	s.mu.Lock()
	defer s.mu.Unlock()

	proposals, err := s.load()
	if err != nil {
		return nil
	}
	proposals = s.purgeExpired(proposals)
	_ = s.save(proposals)
	return proposals
}

// ApplyGitHubProposal is a package-level wrapper for the /apply handler.
func ApplyGitHubProposal(ctx context.Context, store *GitHubProposalStore, id string) (string, error) {
	num, url, err := store.Apply(ctx, id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Issue #%d created: %s", num, url), nil
}

// ---------------------------------------------------------------------------
// github_create_issue_proposal
// ---------------------------------------------------------------------------

type githubCreateIssueProposalTool struct{ ts *GitHubToolset }

func (t *githubCreateIssueProposalTool) Name() string { return "github_create_issue_proposal" }
func (t *githubCreateIssueProposalTool) Description() string {
	return "Propose creating a GitHub issue. The issue is not created until the user runs /apply <proposal-id>."
}
func (t *githubCreateIssueProposalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{
				"type":        "string",
				"description": "Repository in owner/repo form, or just the repo name if unambiguous.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Issue title (required).",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Issue body in Markdown (required).",
			},
		},
		"required": []string{"repo", "title", "body"},
	}
}

func (t *githubCreateIssueProposalTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	repoArg, _ := args["repo"].(string)
	repo, err := t.ts.resolveRepo(repoArg)
	if err != nil {
		return ErrorResult(err.Error())
	}

	title, _ := args["title"].(string)
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrorResult("title is required")
	}

	body, _ := args["body"].(string)
	body = strings.TrimSpace(body)
	if body == "" {
		return ErrorResult("body is required")
	}

	p := GitHubProposal{
		Repo:   repo,
		Title:  title,
		Body:   body,
		Reason: fmt.Sprintf("Create issue in %s: %s", repo, title),
	}

	stored, err := t.ts.proposals.Propose(p)
	if err != nil {
		return ErrorResult("failed to store proposal: " + err.Error())
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Issue proposal ready.\n")
	fmt.Fprintf(&sb, "Repo: %s\n", stored.Repo)
	fmt.Fprintf(&sb, "Title: %s\n", stored.Title)
	fmt.Fprintf(&sb, "Proposal ID: %s\n", stored.ID)
	fmt.Fprintf(&sb, "Run /apply %s to confirm (expires in 15 min), or /reject %s to cancel.", stored.ID, stored.ID)

	return &ToolResult{
		ForLLM:  fmt.Sprintf("Issue proposal queued (ID: %s). Awaiting user approval via Telegram.", stored.ID),
		ForUser: sb.String(),
	}
}
