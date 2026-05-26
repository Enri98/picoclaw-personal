package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func makeGitHubProposalStore(t *testing.T) (*GitHubProposalStore, string) {
	t.Helper()
	dir := t.TempDir()
	// ts is nil intentionally; Apply is skipped in these tests.
	ps := NewGitHubProposalStore(dir, nil)
	return ps, dir
}

func sampleGitHubProposal() GitHubProposal {
	return GitHubProposal{
		Repo:  "owner/repo",
		Title: "Test issue title",
		Body:  "Test issue body",
	}
}

func TestGitHubProposalStore_Propose_MintsID(t *testing.T) {
	ps, _ := makeGitHubProposalStore(t)
	p, err := ps.Propose(sampleGitHubProposal())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.ID == "" {
		t.Fatal("expected non-empty ID after Propose")
	}
	if !uuidV4Re.MatchString(p.ID) {
		t.Fatalf("ID %q does not match UUID v4 format", p.ID)
	}
}

func TestGitHubProposalStore_Propose_PersistsToDisk(t *testing.T) {
	ps, dir := makeGitHubProposalStore(t)
	p, err := ps.Propose(sampleGitHubProposal())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	// Fresh store on same directory.
	ps2 := NewGitHubProposalStore(dir, nil)
	list := ps2.List()
	if len(list) == 0 {
		t.Fatal("expected proposal to persist to disk; fresh store returned empty list")
	}
	found := false
	for _, item := range list {
		if item.ID == p.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("proposal ID %q not found in fresh store; got %+v", p.ID, list)
	}
}

func TestGitHubProposalStore_Reject(t *testing.T) {
	ps, _ := makeGitHubProposalStore(t)
	p, err := ps.Propose(sampleGitHubProposal())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if err := ps.Reject(p.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	list := ps.List()
	if len(list) != 0 {
		t.Fatalf("expected empty store after Reject, got %d entries", len(list))
	}
}

func TestGitHubProposalStore_PurgeExpired(t *testing.T) {
	ps, dir := makeGitHubProposalStore(t)

	// Write an already-expired proposal directly to disk.
	expired := GitHubProposal{
		ID:        "expired-gh-id",
		Repo:      "owner/repo",
		Title:     "Old issue",
		Body:      "body",
		ExpiresAt: time.Now().Add(-2 * time.Minute).Unix(),
	}
	data, _ := json.MarshalIndent([]GitHubProposal{expired}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "github_proposals.json"), data, 0o644); err != nil {
		t.Fatalf("writing expired proposal: %v", err)
	}

	// Propose a fresh one — Propose calls proposeWithID which runs purgeExpired
	// before appending.
	fresh, err := ps.Propose(sampleGitHubProposal())
	if err != nil {
		t.Fatalf("Propose after expired entry: %v", err)
	}

	list := ps.List()
	for _, item := range list {
		if item.ID == expired.ID {
			t.Fatalf("expired proposal %q should have been purged, but is still present", expired.ID)
		}
	}
	found := false
	for _, item := range list {
		if item.ID == fresh.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("fresh proposal %q missing from list after purge", fresh.ID)
	}
}
