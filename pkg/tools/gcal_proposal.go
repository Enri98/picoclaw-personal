// PicoClaw - Ultra-lightweight personal AI agent

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

// GCalProposal holds a pending calendar event creation proposal awaiting user approval.
type GCalProposal struct {
	ID          string   `json:"id"`                    // UUIDv4
	Title       string   `json:"title"`
	Start       string   `json:"start"`                 // RFC3339
	End         string   `json:"end"`                   // RFC3339
	Attendees   []string `json:"attendees,omitempty"`
	Description string   `json:"description,omitempty"`
	Location    string   `json:"location,omitempty"`
	Reason      string   `json:"reason"`
	ExpiresAt   int64    `json:"expires_at"`            // unix seconds, 15-min TTL
}

// GCalProposalStore persists pending calendar proposals to a JSON file.
type GCalProposalStore struct {
	workspaceDir string
	client       GCalClient
	calendarID   string
	mu           sync.Mutex
}

// NewGCalProposalStore returns a GCalProposalStore backed by
// ${workspaceDir}/state/gcal_proposals.json.
func NewGCalProposalStore(workspaceDir, calendarID string, client GCalClient) *GCalProposalStore {
	return &GCalProposalStore{
		workspaceDir: filepath.Clean(workspaceDir),
		client:       client,
		calendarID:   calendarID,
	}
}

func (s *GCalProposalStore) proposalsPath() string {
	return filepath.Join(s.workspaceDir, "state", "gcal_proposals.json")
}

func (s *GCalProposalStore) stateDir() string {
	return filepath.Join(s.workspaceDir, "state")
}

func (s *GCalProposalStore) load() ([]GCalProposal, error) {
	data, err := os.ReadFile(s.proposalsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []GCalProposal
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *GCalProposalStore) save(proposals []GCalProposal) error {
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
func (s *GCalProposalStore) purgeExpired(proposals []GCalProposal) []GCalProposal {
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
func (s *GCalProposalStore) Propose(p GCalProposal) (GCalProposal, error) {
	id, err := newProposalID()
	if err != nil {
		return GCalProposal{}, fmt.Errorf("failed to generate proposal ID: %w", err)
	}
	return s.proposeWithID(id, p)
}

// proposeWithID creates and stores a proposal using a caller-supplied ID.
func (s *GCalProposalStore) proposeWithID(id string, p GCalProposal) (GCalProposal, error) {
	p.ID = id
	p.ExpiresAt = time.Now().Add(15 * time.Minute).Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.load()
	if err != nil {
		return GCalProposal{}, fmt.Errorf("failed to load proposals: %w", err)
	}
	existing = s.purgeExpired(existing)
	existing = append(existing, p)
	if err := s.save(existing); err != nil {
		return GCalProposal{}, fmt.Errorf("failed to save proposal: %w", err)
	}
	return p, nil
}

// Apply finds the proposal by id, removes it (replay-prevention), then
// calls CreateEvent on the client.
func (s *GCalProposalStore) Apply(ctx context.Context, id string) (GCalEvent, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return GCalEvent{}, fmt.Errorf("proposal ID is required")
	}

	s.mu.Lock()

	proposals, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return GCalEvent{}, fmt.Errorf("failed to load proposals: %w", err)
	}

	// Distinguish expired-and-purged from never-existed so the user gets a
	// distinct error when they /apply just past the 15-min window.
	nowSec := time.Now().Unix()
	for _, p := range proposals {
		if p.ID == id && p.ExpiresAt <= nowSec {
			// Cleanup the stale entry while we hold the lock.
			proposals = s.purgeExpired(proposals)
			_ = s.save(proposals)
			s.mu.Unlock()
			return GCalEvent{}, fmt.Errorf("proposal %s has expired; re-propose to proceed", id)
		}
	}
	proposals = s.purgeExpired(proposals)

	var found GCalProposal
	foundOK := false
	remaining := make([]GCalProposal, 0, len(proposals))
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
		return GCalEvent{}, fmt.Errorf("no active proposal with ID %s", id)
	}

	// Remove proposal before calling the API — prevents replay on network error.
	if err := s.save(remaining); err != nil {
		s.mu.Unlock()
		return GCalEvent{}, fmt.Errorf("failed to remove proposal: %w", err)
	}
	s.mu.Unlock()

	start, err := time.Parse(time.RFC3339, found.Start)
	if err != nil {
		return GCalEvent{}, fmt.Errorf("proposal has invalid start time: %w", err)
	}
	end, err := time.Parse(time.RFC3339, found.End)
	if err != nil {
		return GCalEvent{}, fmt.Errorf("proposal has invalid end time: %w", err)
	}

	// Re-validate start time at apply: a proposal made for "5 minutes from now"
	// and /applied 20 minutes later would otherwise create a malformed
	// historical event. Calendar accepts past-start events silently.
	if start.Before(time.Now().Add(-1 * time.Minute)) {
		return GCalEvent{}, fmt.Errorf("proposal start time %s is in the past; re-propose with a future time", start.Format(time.RFC3339))
	}

	ev := GCalNewEvent{
		Title:       found.Title,
		Start:       start,
		End:         end,
		Attendees:   found.Attendees,
		Description: found.Description,
		Location:    found.Location,
	}
	return s.client.CreateEvent(ctx, s.calendarID, ev)
}

// Reject removes a proposal by id without creating an event.
func (s *GCalProposalStore) Reject(id string) error {
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

	remaining := make([]GCalProposal, 0, len(proposals))
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
func (s *GCalProposalStore) List() []GCalProposal {
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
