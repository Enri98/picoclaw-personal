package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// mockRegistrar — captures calls and returns configured values.
// ---------------------------------------------------------------------------

type mockRegistrar struct {
	text      string
	chatID    string
	fireAt    time.Time
	returnID  string
	returnErr error
}

func (m *mockRegistrar) Register(text string, fireAt time.Time, chatID string) (string, error) {
	m.text = text
	m.fireAt = fireAt
	m.chatID = chatID
	return m.returnID, m.returnErr
}

// makeRemindersToolset returns a RemindersToolset wired to the given registrar
// and reminder_set tool extracted for convenience.
func makeRemindersToolset(t *testing.T, reg ReminderRegistrar, chatID string) (*RemindersToolset, Tool) {
	t.Helper()
	ts := NewRemindersToolset(reg)
	ts.SetChatID(chatID)
	var tool Tool
	for _, tt := range ts.Tools() {
		if tt.Name() == "reminder_set" {
			tool = tt
		}
	}
	if tool == nil {
		t.Fatal("reminder_set tool not found")
	}
	return ts, tool
}

// ---------------------------------------------------------------------------
// TestReminderSet_RejectsPast
// ---------------------------------------------------------------------------

func TestReminderSet_RejectsPast(t *testing.T) {
	reg := &mockRegistrar{returnID: "id-1"}
	_, tool := makeRemindersToolset(t, reg, "chat1")

	past := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	result := tool.Execute(context.Background(), map[string]any{
		"fire_at": past,
		"text":    "should fail",
	})
	if result == nil || !result.IsError {
		t.Fatalf("expected error for past fire_at, got: %v", result)
	}
}

// ---------------------------------------------------------------------------
// TestReminderSet_Rejects90DayHorizon
// ---------------------------------------------------------------------------

func TestReminderSet_Rejects90DayHorizon(t *testing.T) {
	reg := &mockRegistrar{returnID: "id-1"}
	_, tool := makeRemindersToolset(t, reg, "chat1")

	far := time.Now().UTC().Add(91 * 24 * time.Hour).Format(time.RFC3339)
	result := tool.Execute(context.Background(), map[string]any{
		"fire_at": far,
		"text":    "too far",
	})
	if result == nil || !result.IsError {
		t.Fatalf("expected error for fire_at beyond 90 days, got: %v", result)
	}
}

// ---------------------------------------------------------------------------
// TestReminderSet_RejectsEmptyText
// ---------------------------------------------------------------------------

func TestReminderSet_RejectsEmptyText(t *testing.T) {
	reg := &mockRegistrar{returnID: "id-1"}
	_, tool := makeRemindersToolset(t, reg, "chat1")

	future := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)

	for _, text := range []string{"", "   "} {
		result := tool.Execute(context.Background(), map[string]any{
			"fire_at": future,
			"text":    text,
		})
		if result == nil || !result.IsError {
			t.Errorf("expected error for text=%q, got: %v", text, result)
		}
	}
}

// ---------------------------------------------------------------------------
// TestReminderSet_RejectsLongText
// ---------------------------------------------------------------------------

func TestReminderSet_RejectsLongText(t *testing.T) {
	reg := &mockRegistrar{returnID: "id-1"}
	_, tool := makeRemindersToolset(t, reg, "chat1")

	future := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	long := strings.Repeat("a", 201)
	result := tool.Execute(context.Background(), map[string]any{
		"fire_at": future,
		"text":    long,
	})
	if result == nil || !result.IsError {
		t.Fatalf("expected error for text > 200 chars, got: %v", result)
	}
}

// ---------------------------------------------------------------------------
// TestReminderSet_RejectsEmptyChatID
// ---------------------------------------------------------------------------

func TestReminderSet_RejectsEmptyChatID(t *testing.T) {
	reg := &mockRegistrar{returnID: "id-1"}
	_, tool := makeRemindersToolset(t, reg, "") // chatID deliberately empty

	future := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	result := tool.Execute(context.Background(), map[string]any{
		"fire_at": future,
		"text":    "remind me",
	})
	if result == nil || !result.IsError {
		t.Fatalf("expected error for empty chatID, got: %v", result)
	}
	if !strings.Contains(result.ForLLM, "primary_chat_id") {
		t.Errorf("expected 'primary_chat_id' in error message, got: %s", result.ForLLM)
	}
}

// ---------------------------------------------------------------------------
// TestReminderSet_Success
// ---------------------------------------------------------------------------

func TestReminderSet_Success(t *testing.T) {
	reg := &mockRegistrar{returnID: "reminder-abc"}
	_, tool := makeRemindersToolset(t, reg, "chat99")

	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	result := tool.Execute(context.Background(), map[string]any{
		"fire_at": future.Format(time.RFC3339),
		"text":    "call the dentist",
	})
	if result == nil || result.IsError {
		t.Fatalf("expected success, got: %v", result)
	}
	if !strings.Contains(result.ForLLM, "reminder-abc") {
		t.Errorf("expected ID 'reminder-abc' in result, got: %s", result.ForLLM)
	}
	if reg.text != "call the dentist" {
		t.Errorf("registrar text: want %q, got %q", "call the dentist", reg.text)
	}
	if reg.chatID != "chat99" {
		t.Errorf("registrar chatID: want %q, got %q", "chat99", reg.chatID)
	}
	if !reg.fireAt.Equal(future) {
		t.Errorf("registrar fireAt: want %v, got %v", future, reg.fireAt)
	}
}

// ---------------------------------------------------------------------------
// TestReminderSet_InvalidRFC3339
// ---------------------------------------------------------------------------

func TestReminderSet_InvalidRFC3339(t *testing.T) {
	reg := &mockRegistrar{returnID: "id-1"}
	_, tool := makeRemindersToolset(t, reg, "chat1")

	result := tool.Execute(context.Background(), map[string]any{
		"fire_at": "not a date",
		"text":    "remind me",
	})
	if result == nil || !result.IsError {
		t.Fatalf("expected error for invalid RFC3339, got: %v", result)
	}
}
