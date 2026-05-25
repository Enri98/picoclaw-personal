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

// BashProposal holds a pending destructive-command proposal awaiting user approval.
type BashProposal struct {
	ID        string `json:"id"`          // UUIDv4
	Cmd       string `json:"cmd"`
	CwdMode   string `json:"cwd_mode"`
	Pattern   string `json:"pattern"`     // which destructive pattern matched
	Reason    string `json:"reason"`      // user-readable explanation
	ExpiresAt int64  `json:"expires_at"`  // unix seconds, 15-min TTL
}

// BashProposalStore persists pending bash proposals to a JSON file.
type BashProposalStore struct {
	workspaceDir string
	runner       Runner
	mu           sync.Mutex
}

// NewBashProposalStore returns a BashProposalStore backed by
// ${workspaceDir}/state/bash_proposals.json.
func NewBashProposalStore(workspaceDir string, runner Runner) *BashProposalStore {
	return &BashProposalStore{
		workspaceDir: filepath.Clean(workspaceDir),
		runner:       runner,
	}
}

func (s *BashProposalStore) proposalsPath() string {
	return filepath.Join(s.workspaceDir, "state", "bash_proposals.json")
}

func (s *BashProposalStore) stateDir() string {
	return filepath.Join(s.workspaceDir, "state")
}

func (s *BashProposalStore) load() ([]BashProposal, error) {
	data, err := os.ReadFile(s.proposalsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []BashProposal
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *BashProposalStore) save(proposals []BashProposal) error {
	if err := os.MkdirAll(s.stateDir(), 0o755); err != nil {
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
func (s *BashProposalStore) purgeExpired(proposals []BashProposal) []BashProposal {
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
func (s *BashProposalStore) Propose(cmd, cwdMode, pattern, reason string) (BashProposal, error) {
	id, err := newProposalID()
	if err != nil {
		return BashProposal{}, fmt.Errorf("failed to generate proposal ID: %w", err)
	}
	return s.proposeWithID(id, cmd, cwdMode, pattern, reason)
}

// proposeWithID creates and stores a proposal using a caller-supplied ID.
// Used by BashTool.Execute to keep ID generation in one place.
func (s *BashProposalStore) proposeWithID(id, cmd, cwdMode, pattern, reason string) (BashProposal, error) {
	p := BashProposal{
		ID:        id,
		Cmd:       cmd,
		CwdMode:   cwdMode,
		Pattern:   pattern,
		Reason:    reason,
		ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.load()
	if err != nil {
		return BashProposal{}, fmt.Errorf("failed to load proposals: %w", err)
	}
	existing = s.purgeExpired(existing)
	existing = append(existing, p)
	if err := s.save(existing); err != nil {
		return BashProposal{}, fmt.Errorf("failed to save proposal: %w", err)
	}
	return p, nil
}

// Apply finds the proposal by id, removes it (replay-prevention), then
// invokes the runner with the stored command.
func (s *BashProposalStore) Apply(ctx context.Context, id string) (RunResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return RunResult{}, fmt.Errorf("proposal ID is required")
	}

	s.mu.Lock()

	proposals, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return RunResult{}, fmt.Errorf("failed to load proposals: %w", err)
	}
	proposals = s.purgeExpired(proposals)

	var found BashProposal
	foundOK := false
	remaining := make([]BashProposal, 0, len(proposals))
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
		return RunResult{}, fmt.Errorf("no active proposal with ID %s", id)
	}

	// Remove proposal before running — prevents replay on runner error.
	if err := s.save(remaining); err != nil {
		s.mu.Unlock()
		return RunResult{}, fmt.Errorf("failed to remove proposal: %w", err)
	}
	s.mu.Unlock()

	return s.runner.Run(ctx, found.CwdMode, found.Cmd)
}

// Reject removes a proposal by id without executing anything.
func (s *BashProposalStore) Reject(id string) error {
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

	remaining := make([]BashProposal, 0, len(proposals))
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
func (s *BashProposalStore) List() []BashProposal {
	s.mu.Lock()
	defer s.mu.Unlock()

	proposals, err := s.load()
	if err != nil {
		return nil
	}
	proposals = s.purgeExpired(proposals)
	// Persist the purged list so the file stays clean.
	_ = s.save(proposals)
	return proposals
}
