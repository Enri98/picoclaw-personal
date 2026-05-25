// PicoClaw - Ultra-lightweight personal AI agent

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakeGCalClient — in-memory test double.
// ---------------------------------------------------------------------------

type fakeGCalClient struct {
	todayEvents  []GCalEvent
	weekEvents   []GCalEvent
	created      []GCalNewEvent
	todayErr     error
	weekErr      error
	createErr    error
}

func (f *fakeGCalClient) Today(_ context.Context, _ string) ([]GCalEvent, error) {
	return f.todayEvents, f.todayErr
}

func (f *fakeGCalClient) Week(_ context.Context, _ string) ([]GCalEvent, error) {
	return f.weekEvents, f.weekErr
}

func (f *fakeGCalClient) CreateEvent(_ context.Context, _ string, ev GCalNewEvent) (GCalEvent, error) {
	if f.createErr != nil {
		return GCalEvent{}, f.createErr
	}
	f.created = append(f.created, ev)
	return GCalEvent{
		ID:    "fake-id-1",
		Title: ev.Title,
		Start: ev.Start,
		End:   ev.End,
	}, nil
}

// makeToolset returns a GCalToolset wired to the given fake client and a temp
// directory for proposal state.
func makeToolset(t *testing.T, fake *fakeGCalClient) *GCalToolset {
	t.Helper()
	dir := t.TempDir()
	ts, err := NewGCalToolset(fake, "primary", dir)
	if err != nil {
		t.Fatalf("NewGCalToolset: %v", err)
	}
	return ts
}

// ---------------------------------------------------------------------------
// gcal_today
// ---------------------------------------------------------------------------

func TestGCalToday_HappyPath(t *testing.T) {
	now := time.Now()
	fake := &fakeGCalClient{
		todayEvents: []GCalEvent{
			{ID: "1", Title: "Standup", Start: now, End: now.Add(30 * time.Minute)},
			{ID: "2", Title: "Lunch", Start: now.Add(4 * time.Hour), End: now.Add(5 * time.Hour)},
			{ID: "3", Title: "1:1", Start: now.Add(6 * time.Hour), End: now.Add(7 * time.Hour)},
		},
	}
	ts := makeToolset(t, fake)
	var tool Tool
	for _, tt := range ts.Tools() {
		if tt.Name() == "gcal_today" {
			tool = tt
		}
	}
	if tool == nil {
		t.Fatal("gcal_today tool not found")
	}
	result := tool.Execute(context.Background(), map[string]any{})
	if result == nil || result.IsError {
		t.Fatalf("expected success, got: %v", result)
	}
	var events []GCalEvent
	if err := json.Unmarshal([]byte(result.ForLLM), &events); err != nil {
		t.Fatalf("failed to parse result JSON: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Title != "Standup" {
		t.Errorf("expected first event Standup, got %q", events[0].Title)
	}
}

// ---------------------------------------------------------------------------
// gcal_week
// ---------------------------------------------------------------------------

func TestGCalWeek_HappyPath(t *testing.T) {
	now := time.Now()
	fake := &fakeGCalClient{
		weekEvents: []GCalEvent{
			{ID: "w1", Title: "Monday meeting", Start: now, End: now.Add(time.Hour)},
			{ID: "w2", Title: "Wednesday lunch", Start: now.Add(48 * time.Hour), End: now.Add(49 * time.Hour)},
		},
	}
	ts := makeToolset(t, fake)
	var tool Tool
	for _, tt := range ts.Tools() {
		if tt.Name() == "gcal_week" {
			tool = tt
		}
	}
	if tool == nil {
		t.Fatal("gcal_week tool not found")
	}
	result := tool.Execute(context.Background(), map[string]any{})
	if result == nil || result.IsError {
		t.Fatalf("expected success, got: %v", result)
	}
	var events []GCalEvent
	if err := json.Unmarshal([]byte(result.ForLLM), &events); err != nil {
		t.Fatalf("failed to parse result JSON: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

// ---------------------------------------------------------------------------
// gcal_create_event_proposal — validation
// ---------------------------------------------------------------------------

func findProposalTool(ts *GCalToolset) Tool {
	for _, tt := range ts.Tools() {
		if tt.Name() == "gcal_create_event_proposal" {
			return tt
		}
	}
	return nil
}

func TestGCalCreateEventProposal_RejectsMissingFields(t *testing.T) {
	ts := makeToolset(t, &fakeGCalClient{})
	tool := findProposalTool(ts)
	if tool == nil {
		t.Fatal("gcal_create_event_proposal tool not found")
	}

	cases := []map[string]any{
		{},
		{"title": "Meeting"},
		{"title": "Meeting", "start": time.Now().Add(time.Hour).Format(time.RFC3339)},
	}
	for _, args := range cases {
		result := tool.Execute(context.Background(), args)
		if result == nil || !result.IsError {
			t.Errorf("expected error for args %v, got: %v", args, result)
		}
	}
}

func TestGCalCreateEventProposal_RejectsBadTimeOrder(t *testing.T) {
	ts := makeToolset(t, &fakeGCalClient{})
	tool := findProposalTool(ts)
	future := time.Now().Add(2 * time.Hour)
	// start after end
	result := tool.Execute(context.Background(), map[string]any{
		"title": "Bad order",
		"start": future.Add(time.Hour).Format(time.RFC3339),
		"end":   future.Format(time.RFC3339),
	})
	if result == nil || !result.IsError {
		t.Errorf("expected error for start>=end, got: %v", result)
	}
}

func TestGCalCreateEventProposal_RejectsPastStart(t *testing.T) {
	ts := makeToolset(t, &fakeGCalClient{})
	tool := findProposalTool(ts)
	pastStart := time.Now().Add(-10 * time.Minute)
	result := tool.Execute(context.Background(), map[string]any{
		"title": "Past event",
		"start": pastStart.Format(time.RFC3339),
		"end":   pastStart.Add(time.Hour).Format(time.RFC3339),
	})
	if result == nil || !result.IsError {
		t.Errorf("expected error for past start, got: %v", result)
	}
}

func TestGCalCreateEventProposal_CreatesProposal(t *testing.T) {
	fake := &fakeGCalClient{}
	ts := makeToolset(t, fake)
	tool := findProposalTool(ts)
	future := time.Now().Add(2 * time.Hour)
	result := tool.Execute(context.Background(), map[string]any{
		"title": "Team sync",
		"start": future.Format(time.RFC3339),
		"end":   future.Add(time.Hour).Format(time.RFC3339),
	})
	if result == nil || result.IsError {
		t.Fatalf("expected success, got: %v", result)
	}
	if !strings.Contains(result.ForUser, "Proposal ID:") {
		t.Errorf("expected ForUser to contain 'Proposal ID:', got: %s", result.ForUser)
	}
	if len(fake.created) != 0 {
		t.Errorf("CreateEvent must not be called during proposal; got %d calls", len(fake.created))
	}
}

// ---------------------------------------------------------------------------
// GCalProposalStore tests
// ---------------------------------------------------------------------------

func makeProposalStore(t *testing.T, fake *fakeGCalClient) (*GCalProposalStore, string) {
	t.Helper()
	dir := t.TempDir()
	ps := NewGCalProposalStore(dir, "primary", fake)
	return ps, dir
}

func futureProposal(offset time.Duration) GCalProposal {
	start := time.Now().Add(offset)
	end := start.Add(time.Hour)
	return GCalProposal{
		Title:  "Test event",
		Start:  start.Format(time.RFC3339),
		End:    end.Format(time.RFC3339),
		Reason: "test",
	}
}

func TestGCalProposalStore_RoundTrip(t *testing.T) {
	fake := &fakeGCalClient{}
	ps, dir := makeProposalStore(t, fake)

	p, err := ps.Propose(futureProposal(2 * time.Hour))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.ID == "" {
		t.Fatal("expected non-empty ID")
	}

	// File must exist.
	if _, statErr := os.Stat(filepath.Join(dir, "state", "gcal_proposals.json")); statErr != nil {
		t.Fatalf("proposals file not found: %v", statErr)
	}

	ev, err := ps.Apply(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ev.Title != "Test event" {
		t.Errorf("expected title 'Test event', got %q", ev.Title)
	}
	if len(fake.created) != 1 {
		t.Errorf("expected 1 CreateEvent call, got %d", len(fake.created))
	}
	if fake.created[0].Title != "Test event" {
		t.Errorf("expected CreateEvent title 'Test event', got %q", fake.created[0].Title)
	}

	// Store must now be empty.
	if remaining := ps.List(); len(remaining) != 0 {
		t.Errorf("expected empty store after Apply, got %d proposals", len(remaining))
	}
}

func TestGCalProposalStore_Expiry(t *testing.T) {
	fake := &fakeGCalClient{}
	ps, dir := makeProposalStore(t, fake)

	// Manually write an already-expired proposal to the file.
	expired := GCalProposal{
		ID:        "expired-id",
		Title:     "Old event",
		Start:     time.Now().Add(time.Hour).Format(time.RFC3339),
		End:       time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		Reason:    "test",
		ExpiresAt: time.Now().Add(-1 * time.Minute).Unix(), // already expired
	}
	data, _ := json.MarshalIndent([]GCalProposal{expired}, "", "  ")
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "gcal_proposals.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ps.List()
	if len(got) != 0 {
		t.Errorf("expected expired proposal to be purged; got %d", len(got))
	}
}

func TestGCalProposalStore_Reject_DoesNotCreate(t *testing.T) {
	fake := &fakeGCalClient{}
	ps, _ := makeProposalStore(t, fake)

	p, err := ps.Propose(futureProposal(2 * time.Hour))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if err := ps.Reject(p.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if len(fake.created) != 0 {
		t.Errorf("Reject must not call CreateEvent; got %d calls", len(fake.created))
	}
	if remaining := ps.List(); len(remaining) != 0 {
		t.Errorf("expected empty store after Reject, got %d proposals", len(remaining))
	}
}

func TestGCalProposalStore_ApplyMissing(t *testing.T) {
	fake := &fakeGCalClient{}
	ps, _ := makeProposalStore(t, fake)

	_, err := ps.Apply(context.Background(), "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for unknown ID")
	}
	if !strings.Contains(err.Error(), "no active proposal") {
		t.Errorf("expected 'no active proposal' in error, got: %v", err)
	}
}

// Apply must re-validate start time so a proposal made for a soon-after time
// that the user takes 20 minutes to /apply doesn't silently create a past event.
func TestGCalProposalStore_ApplyRejectsPastStart(t *testing.T) {
	fake := &fakeGCalClient{}
	ps, dir := makeProposalStore(t, fake)

	// Manually write a proposal whose start has slipped into the past but
	// whose ExpiresAt is still in the future (within the 15-min TTL).
	pastStart := GCalProposal{
		ID:        "past-start-id",
		Title:     "Stale event",
		Start:     time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
		End:       time.Now().Add(25 * time.Minute).Format(time.RFC3339),
		Reason:    "test",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	data, _ := json.MarshalIndent([]GCalProposal{pastStart}, "", "  ")
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "gcal_proposals.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ps.Apply(context.Background(), "past-start-id")
	if err == nil {
		t.Fatal("expected error when applying proposal with past start")
	}
	if !strings.Contains(err.Error(), "in the past") {
		t.Errorf("expected past-start error, got: %v", err)
	}
	if len(fake.created) != 0 {
		t.Errorf("CreateEvent must not be called for past-start proposal; got %d calls", len(fake.created))
	}
}

// Expired proposals must produce a distinct error so the user sees the TTL
// window elapsed rather than thinking the ID never existed.
func TestGCalProposalStore_ApplyExpiredHasDistinctError(t *testing.T) {
	fake := &fakeGCalClient{}
	ps, dir := makeProposalStore(t, fake)

	expired := GCalProposal{
		ID:        "expired-id",
		Title:     "Old event",
		Start:     time.Now().Add(time.Hour).Format(time.RFC3339),
		End:       time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		Reason:    "test",
		ExpiresAt: time.Now().Add(-1 * time.Minute).Unix(),
	}
	data, _ := json.MarshalIndent([]GCalProposal{expired}, "", "  ")
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "gcal_proposals.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ps.Apply(context.Background(), "expired-id")
	if err == nil {
		t.Fatal("expected error when applying expired proposal")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected expired-specific error, got: %v", err)
	}
}

// Attendee email validation rejects bare names before storing the proposal,
// so a bad address doesn't survive to apply time and consume the proposal.
func TestGCalCreateEventProposal_RejectsMalformedAttendee(t *testing.T) {
	fake := &fakeGCalClient{}
	ts := makeToolset(t, fake)
	tool := findProposalTool(ts)

	start := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	end := time.Now().Add(3 * time.Hour).Format(time.RFC3339)
	result := tool.Execute(context.Background(), map[string]any{
		"title":     "Lunch",
		"start":     start,
		"end":       end,
		"attendees": []any{"marco@example.com", "not-an-email"},
	})
	if !result.IsError {
		t.Fatal("expected error for malformed attendee")
	}
	if !strings.Contains(result.ForLLM, "not a valid email") {
		t.Errorf("expected validation message, got: %s", result.ForLLM)
	}
	if len(ts.proposals.List()) != 0 {
		t.Error("malformed-attendee proposal must not be stored")
	}
}

func TestLooksLikeEmail(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"marco@example.com", true},
		{"a@b.co", true},
		{"marco", false},
		{"", false},
		{"@example.com", false},
		{"marco@", false},
		{"marco@example", false},
		{"a@b@c.com", false},
	}
	for _, tc := range cases {
		if got := looksLikeEmail(tc.s); got != tc.want {
			t.Errorf("looksLikeEmail(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
