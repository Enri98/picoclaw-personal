package agent

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TestReminderStore_Register_MintsID
// ---------------------------------------------------------------------------

func TestReminderStore_Register_MintsID(t *testing.T) {
	dir := t.TempDir()
	store, err := NewReminderStore(dir)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}

	r1, err := store.Register(Reminder{
		Text:   "first",
		FireAt: time.Now().UTC().Add(1 * time.Hour),
		ChatID: "chat1",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r1.ID == "" {
		t.Fatal("expected non-empty ID for first reminder")
	}

	r2, err := store.Register(Reminder{
		Text:   "second",
		FireAt: time.Now().UTC().Add(2 * time.Hour),
		ChatID: "chat1",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r2.ID == "" {
		t.Fatal("expected non-empty ID for second reminder")
	}
	if r1.ID == r2.ID {
		t.Fatalf("expected distinct IDs, both got %q", r1.ID)
	}
}

// ---------------------------------------------------------------------------
// TestReminderStore_RoundTrip
// ---------------------------------------------------------------------------

func TestReminderStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store1, err := NewReminderStore(dir)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}

	fireAt := time.Now().UTC().Add(1 * time.Hour).Truncate(time.Second)
	r, err := store1.Register(Reminder{
		Text:   "walk the dog",
		FireAt: fireAt,
		ChatID: "chat42",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Open a second store on the same directory.
	store2, err := NewReminderStore(dir)
	if err != nil {
		t.Fatalf("NewReminderStore (second): %v", err)
	}

	list, err := store2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 reminder, got %d", len(list))
	}
	got := list[0]
	if got.ID != r.ID {
		t.Errorf("ID: want %q, got %q", r.ID, got.ID)
	}
	if got.Text != "walk the dog" {
		t.Errorf("Text: want %q, got %q", "walk the dog", got.Text)
	}
	if got.ChatID != "chat42" {
		t.Errorf("ChatID: want %q, got %q", "chat42", got.ChatID)
	}
	if !got.FireAt.UTC().Equal(fireAt) {
		t.Errorf("FireAt: want %v, got %v", fireAt, got.FireAt.UTC())
	}
}

// ---------------------------------------------------------------------------
// TestReminderStore_FireDue
// ---------------------------------------------------------------------------

func TestReminderStore_FireDue(t *testing.T) {
	dir := t.TempDir()
	store, err := NewReminderStore(dir)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}
	now := time.Now().UTC()

	// Due: 1 minute in the past, not yet fired.
	due, err := store.Register(Reminder{
		Text:   "past due",
		FireAt: now.Add(-1 * time.Minute),
		ChatID: "c1",
	})
	if err != nil {
		t.Fatalf("Register due: %v", err)
	}

	// Not yet due: 5 minutes in the future.
	_, err = store.Register(Reminder{
		Text:   "future",
		FireAt: now.Add(5 * time.Minute),
		ChatID: "c1",
	})
	if err != nil {
		t.Fatalf("Register future: %v", err)
	}

	// Already fired.
	_, err = store.Register(Reminder{
		Text:   "already fired",
		FireAt: now.Add(-2 * time.Minute),
		Fired:  true,
		ChatID: "c1",
	})
	if err != nil {
		t.Fatalf("Register fired: %v", err)
	}

	fired, err := store.FireDue(now)
	if err != nil {
		t.Fatalf("FireDue: %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("expected 1 fired reminder, got %d", len(fired))
	}
	if fired[0].ID != due.ID {
		t.Errorf("expected reminder %q to fire, got %q", due.ID, fired[0].ID)
	}
	if !fired[0].Fired {
		t.Error("expected Fired=true on returned entry")
	}

	// Calling FireDue again should return nothing (already marked fired).
	fired2, err := store.FireDue(now)
	if err != nil {
		t.Fatalf("FireDue second call: %v", err)
	}
	if len(fired2) != 0 {
		t.Errorf("expected 0 on second call, got %d", len(fired2))
	}
}

// ---------------------------------------------------------------------------
// TestReminderStore_GC
// ---------------------------------------------------------------------------

func TestReminderStore_GC(t *testing.T) {
	dir := t.TempDir()
	store, err := NewReminderStore(dir)
	if err != nil {
		t.Fatalf("NewReminderStore: %v", err)
	}

	// Register an already-fired reminder with a FireAt 10 days in the past.
	old := Reminder{
		Text:   "old fired",
		FireAt: time.Now().UTC().Add(-10 * 24 * time.Hour),
		Fired:  true,
		ChatID: "c1",
	}
	_, err = store.Register(old)
	if err != nil {
		t.Fatalf("Register old fired: %v", err)
	}

	// Register a current reminder — this second Register triggers GC on the store.
	fresh := Reminder{
		Text:   "fresh",
		FireAt: time.Now().UTC().Add(1 * time.Hour),
		ChatID: "c1",
	}
	saved, err := store.Register(fresh)
	if err != nil {
		t.Fatalf("Register fresh: %v", err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range list {
		if r.Text == "old fired" {
			t.Errorf("10-day-old fired reminder should have been GC'd, but found it in the list")
		}
	}

	found := false
	for _, r := range list {
		if r.ID == saved.ID {
			found = true
		}
	}
	if !found {
		t.Error("fresh reminder not found after GC")
	}
}

// ---------------------------------------------------------------------------
// TestReminderStore_RejectsEmptyStateDir
// ---------------------------------------------------------------------------

func TestReminderStore_RejectsEmptyStateDir(t *testing.T) {
	_, err := NewReminderStore("")
	if err == nil {
		t.Fatal("expected error for empty stateDir")
	}
}
