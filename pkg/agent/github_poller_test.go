package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// PollEntry JSON round-trip
// ---------------------------------------------------------------------------

func TestPollEntry_RoundTrip(t *testing.T) {
	original := PollEntry{
		ID:                 "test-id-1234",
		Owner:              "acme",
		Repo:               "myrepo",
		IssueNumber:        42,
		CreatedAt:          time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC),
		LastCommentIDSeen:  999,
		TTL:                24 * time.Hour,
		ChatID:             "chat-abc",
		ExpiryNotified:     false,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded PollEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != original.ID {
		t.Errorf("ID: got %q, want %q", decoded.ID, original.ID)
	}
	if decoded.Owner != original.Owner {
		t.Errorf("Owner: got %q, want %q", decoded.Owner, original.Owner)
	}
	if decoded.Repo != original.Repo {
		t.Errorf("Repo: got %q, want %q", decoded.Repo, original.Repo)
	}
	if decoded.IssueNumber != original.IssueNumber {
		t.Errorf("IssueNumber: got %d, want %d", decoded.IssueNumber, original.IssueNumber)
	}
	if !decoded.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt: got %v, want %v", decoded.CreatedAt, original.CreatedAt)
	}
	if decoded.LastCommentIDSeen != original.LastCommentIDSeen {
		t.Errorf("LastCommentIDSeen: got %d, want %d", decoded.LastCommentIDSeen, original.LastCommentIDSeen)
	}
	if decoded.TTL != original.TTL {
		t.Errorf("TTL: got %v, want %v", decoded.TTL, original.TTL)
	}
	if decoded.ChatID != original.ChatID {
		t.Errorf("ChatID: got %q, want %q", decoded.ChatID, original.ChatID)
	}
	if decoded.ExpiryNotified != original.ExpiryNotified {
		t.Errorf("ExpiryNotified: got %v, want %v", decoded.ExpiryNotified, original.ExpiryNotified)
	}
}

// ---------------------------------------------------------------------------
// Load/save round-trip via Register + fresh poller
// ---------------------------------------------------------------------------

func TestGitHubPoller_LoadSave(t *testing.T) {
	dir := t.TempDir()
	p1, err := NewGitHubPoller("dummy", nil, dir)
	if err != nil {
		t.Fatalf("NewGitHubPoller: %v", err)
	}

	entry := PollEntry{
		Owner:       "acme",
		Repo:        "myrepo",
		IssueNumber: 7,
		ChatID:      "chat-42",
	}
	if err := p1.Register(entry); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Verify file was written.
	if _, err := os.Stat(filepath.Join(dir, "github_polls.json")); err != nil {
		t.Fatalf("expected github_polls.json to exist: %v", err)
	}

	// Fresh poller on same dir should load the entry.
	p2, err := NewGitHubPoller("dummy", nil, dir)
	if err != nil {
		t.Fatalf("NewGitHubPoller (second): %v", err)
	}

	p2.mu.Lock()
	entries := make([]PollEntry, len(p2.entries))
	copy(entries, p2.entries)
	p2.mu.Unlock()

	if len(entries) == 0 {
		t.Fatal("expected entry to be loaded by second poller")
	}
	found := false
	for _, e := range entries {
		if e.Owner == "acme" && e.Repo == "myrepo" && e.IssueNumber == 7 {
			found = true
		}
	}
	if !found {
		t.Fatalf("registered entry not found after load; entries: %+v", entries)
	}
}

// ---------------------------------------------------------------------------
// dropEntry removes one entry and persists
// ---------------------------------------------------------------------------

func TestGitHubPoller_DropEntry(t *testing.T) {
	dir := t.TempDir()
	p, err := NewGitHubPoller("dummy", nil, dir)
	if err != nil {
		t.Fatalf("NewGitHubPoller: %v", err)
	}

	e1 := PollEntry{Owner: "owner", Repo: "repo1", IssueNumber: 1, ChatID: "c1"}
	e2 := PollEntry{Owner: "owner", Repo: "repo2", IssueNumber: 2, ChatID: "c2"}

	if err := p.Register(e1); err != nil {
		t.Fatalf("Register e1: %v", err)
	}
	if err := p.Register(e2); err != nil {
		t.Fatalf("Register e2: %v", err)
	}

	p.mu.Lock()
	var id1 string
	for _, e := range p.entries {
		if e.Repo == "repo1" {
			id1 = e.ID
		}
	}
	p.mu.Unlock()

	if id1 == "" {
		t.Fatal("could not find ID for repo1 entry")
	}

	p.dropEntry(id1)

	p.mu.Lock()
	remaining := make([]PollEntry, len(p.entries))
	copy(remaining, p.entries)
	p.mu.Unlock()

	if len(remaining) != 1 {
		t.Fatalf("expected 1 entry after drop, got %d", len(remaining))
	}
	if remaining[0].Repo != "repo2" {
		t.Fatalf("expected repo2 to remain, got %q", remaining[0].Repo)
	}
}

// ---------------------------------------------------------------------------
// TTL expiry check: skipped because the expiry logic runs inside tick() which
// requires a real channels.Manager to send the expiry notice.
// ---------------------------------------------------------------------------

func TestGitHubPoller_TTLExpiry(t *testing.T) {
	t.Skip("expiry check is wired through tick() and channel manager; covered by integration tests")
}
