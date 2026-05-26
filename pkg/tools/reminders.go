package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ReminderRegistrar is the interface the reminders toolset uses to persist a
// reminder. The agent package wires its concrete store to this interface.
type ReminderRegistrar interface {
	Register(text string, fireAt time.Time, chatID string) (id string, err error)
}

// RemindersToolset holds the reminder tool and its dependencies.
type RemindersToolset struct {
	registrar ReminderRegistrar
	chatID    string
}

// NewRemindersToolset constructs a RemindersToolset backed by the given registrar.
func NewRemindersToolset(registrar ReminderRegistrar) *RemindersToolset {
	return &RemindersToolset{registrar: registrar}
}

// SetChatID injects the primary user chat ID so the tool can associate
// reminders with the correct Telegram conversation.
func (ts *RemindersToolset) SetChatID(chatID string) {
	ts.chatID = chatID
}

// Tools returns the single tool exposed by this toolset.
func (ts *RemindersToolset) Tools() []Tool {
	return []Tool{&reminderSetTool{ts: ts}}
}

// ---------------------------------------------------------------------------
// reminder_set
// ---------------------------------------------------------------------------

type reminderSetTool struct{ ts *RemindersToolset }

func (t *reminderSetTool) Name() string { return "reminder_set" }

func (t *reminderSetTool) Description() string {
	return "Register a one-shot reminder that fires via the same Telegram chat at the requested time. " +
		"Use when the user asks to be reminded about something at a specific time. " +
		"You must resolve any relative phrase (e.g. \"tomorrow at 3pm\") into an absolute UTC ISO8601 " +
		"timestamp before calling — this tool does not parse natural language. " +
		"Reminders fire within ±60s of the target time."
}

func (t *reminderSetTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"fire_at": map[string]any{
				"type":        "string",
				"description": "Absolute UTC time to fire the reminder, in ISO8601/RFC3339 format (e.g. 2026-05-27T14:32:00Z). Required.",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "What to remind the user about. 1–200 characters. Required.",
			},
		},
		"required": []string{"fire_at", "text"},
	}
}

func (t *reminderSetTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	fireAtStr, _ := args["fire_at"].(string)
	fireAtStr = strings.TrimSpace(fireAtStr)
	if fireAtStr == "" {
		return ErrorResult("reminder_set: fire_at is required")
	}
	fireAt, err := time.Parse(time.RFC3339, fireAtStr)
	if err != nil {
		return ErrorResult(fmt.Sprintf("reminder_set: fire_at %q is not a valid RFC3339 timestamp", fireAtStr))
	}

	now := time.Now().UTC()
	fireAt = fireAt.UTC()

	if !now.Before(fireAt) {
		return ErrorResult("reminder_set: fire_at must be in the future")
	}
	if fireAt.After(now.Add(90 * 24 * time.Hour)) {
		return ErrorResult("reminder_set: fire_at must be within 90 days from now")
	}

	text, _ := args["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return ErrorResult("reminder_set: text is required")
	}
	if len(text) > 200 {
		return ErrorResult(fmt.Sprintf("reminder_set: text is %d characters; maximum is 200", len(text)))
	}

	chatID := t.ts.chatID
	if chatID == "" {
		// Without a chat ID the reminder would persist but never deliver.
		// Fail loudly so the model surfaces the misconfiguration instead of
		// quietly storing an undeliverable reminder.
		return ErrorResult("reminder_set: no delivery chat configured; set scheduler.primary_chat_id in deploy/config.yaml")
	}
	id, err := t.ts.registrar.Register(text, fireAt, chatID)
	if err != nil {
		return ErrorResult("reminder_set: failed to register reminder: " + err.Error())
	}

	type confirmPayload struct {
		ID     string `json:"id"`
		FireAt string `json:"fire_at"`
		Text   string `json:"text"`
	}
	payload := confirmPayload{
		ID:     id,
		FireAt: fireAt.Format(time.RFC3339),
		Text:   text,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ErrorResult("reminder_set: failed to serialize confirmation: " + err.Error())
	}

	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("Reminder set for %s: %s", fireAt.Format(time.RFC3339), text),
	}
}
