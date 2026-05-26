package agent

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Reminder represents a scheduled reminder.
type Reminder struct {
	ID     string    `json:"id"`
	Text   string    `json:"text"`
	FireAt time.Time `json:"fire_at"` // UTC
	Fired  bool      `json:"fired"`
	ChatID string    `json:"chat_id"`
}

// ReminderStore persists reminders to a JSON file.
type ReminderStore struct {
	path string
	mu   sync.Mutex
}

// NewReminderStore creates a ReminderStore backed by ${stateDir}/reminders.json.
// The state directory is created if it does not exist.
func NewReminderStore(stateDir string) (*ReminderStore, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("reminders: stateDir must not be empty")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("reminders: failed to create stateDir: %w", err)
	}
	return &ReminderStore{path: filepath.Join(stateDir, "reminders.json")}, nil
}

// Register stores a new reminder. A UUID v4 is minted if ID is empty.
func (s *ReminderStore) Register(r Reminder) (Reminder, error) {
	if r.ID == "" {
		id, err := newReminderID()
		if err != nil {
			return Reminder{}, fmt.Errorf("reminders: register: failed to generate ID: %w", err)
		}
		r.ID = id
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.load()
	if err != nil {
		return Reminder{}, fmt.Errorf("reminders: register: load: %w", err)
	}
	entries = s.gc(entries)
	entries = append(entries, r)
	if err := s.save(entries); err != nil {
		return Reminder{}, fmt.Errorf("reminders: register: save: %w", err)
	}
	return r, nil
}

// List returns all stored reminders (including fired ones within the GC window).
func (s *ReminderStore) List() ([]Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// FireDue marks all reminders whose FireAt <= now and Fired==false as fired,
// persists the change, and returns the newly fired set.
func (s *ReminderStore) FireDue(now time.Time) ([]Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.load()
	if err != nil {
		return nil, fmt.Errorf("reminders: fire_due: load: %w", err)
	}

	var fired []Reminder
	for i := range entries {
		if !entries[i].Fired && !entries[i].FireAt.After(now) {
			entries[i].Fired = true
			fired = append(fired, entries[i])
		}
	}

	if len(fired) == 0 {
		return nil, nil
	}

	entries = s.gc(entries)
	if err := s.save(entries); err != nil {
		return nil, fmt.Errorf("reminders: fire_due: save: %w", err)
	}
	return fired, nil
}

// Cancel marks a reminder as fired (without delivery) — used for explicit cancellation.
func (s *ReminderStore) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.load()
	if err != nil {
		return fmt.Errorf("reminders: cancel: load: %w", err)
	}

	found := false
	for i := range entries {
		if entries[i].ID == id {
			entries[i].Fired = true
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("reminders: cancel: no reminder with ID %s", id)
	}

	entries = s.gc(entries)
	return s.save(entries)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// gc removes entries that are fired AND older than 7 days.
func (s *ReminderStore) gc(entries []Reminder) []Reminder {
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	out := entries[:0]
	for _, r := range entries {
		if r.Fired && r.FireAt.Before(cutoff) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (s *ReminderStore) load() ([]Reminder, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Reminder
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *ReminderStore) save(entries []Reminder) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// newReminderID generates a UUID v4 hex string.
func newReminderID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
