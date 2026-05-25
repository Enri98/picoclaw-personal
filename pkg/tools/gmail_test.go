package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakeGmailClient — in-memory GmailClient for tests.
// ---------------------------------------------------------------------------

type fakeGmailClient struct {
	// Inbox is keyed by account name → list of messages.
	inbox map[string][]GmailMessage
	// Bodies is keyed by account name → id → body.
	bodies map[string]map[string]GmailBody

	// Captured args from the most recent ListUnread call.
	lastListAccount    string
	lastListSince      time.Time
	lastListMaxResults int

	// If set, ListUnread returns this error.
	listErr error
	// If set, GetBody returns this error.
	bodyErr error
}

func newFakeClient() *fakeGmailClient {
	return &fakeGmailClient{
		inbox:  make(map[string][]GmailMessage),
		bodies: make(map[string]map[string]GmailBody),
	}
}

func (f *fakeGmailClient) ListUnread(
	_ context.Context, account string, since time.Time, maxResults int,
) ([]GmailMessage, error) {
	f.lastListAccount = account
	f.lastListSince = since
	f.lastListMaxResults = maxResults
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.inbox[account], nil
}

func (f *fakeGmailClient) GetBody(
	_ context.Context, account string, id string,
) (GmailBody, error) {
	if f.bodyErr != nil {
		return GmailBody{}, f.bodyErr
	}
	accBodies, ok := f.bodies[account]
	if !ok {
		return GmailBody{}, fmt.Errorf("not found: account %q", account)
	}
	b, ok := accBodies[id]
	if !ok {
		return GmailBody{}, fmt.Errorf("not found: id %q", id)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestToolset(t *testing.T, client GmailClient, accounts ...GmailAccount) *GmailToolset {
	t.Helper()
	if len(accounts) == 0 {
		accounts = []GmailAccount{
			{Name: "darra2", RefreshTokenEnv: "GMAIL_REFRESH_TOKEN_DARRA2"},
			{Name: "chiunque", RefreshTokenEnv: "GMAIL_REFRESH_TOKEN_CHIUNQUE"},
		}
	}
	ts, err := NewGmailToolset(accounts, client)
	if err != nil {
		t.Fatalf("NewGmailToolset: %v", err)
	}
	return ts
}

func toolByName(t *testing.T, ts *GmailToolset, name string) Tool {
	t.Helper()
	for _, tool := range ts.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found in toolset", name)
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestGmailToolset_RejectsUnknownAccount(t *testing.T) {
	client := newFakeClient()
	ts := newTestToolset(t, client)
	tool := toolByName(t, ts, "gmail_list_unread")

	result := tool.Execute(context.Background(), map[string]any{"account": "unknown"})
	if !result.IsError {
		t.Fatal("expected error result for unknown account")
	}
	if !strings.Contains(result.ForLLM, "unknown") {
		t.Errorf("error message should mention unknown account, got: %s", result.ForLLM)
	}
}

func TestGmailToolset_RejectsMissingAccount(t *testing.T) {
	client := newFakeClient()
	ts := newTestToolset(t, client)
	tool := toolByName(t, ts, "gmail_list_unread")

	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error result for missing account")
	}
}

func TestGmailListUnread_HappyPath(t *testing.T) {
	client := newFakeClient()
	now := time.Now().UTC()
	client.inbox["darra2"] = []GmailMessage{
		{ID: "id1", From: "alice@example.com", Subject: "Hello", Snippet: "Hi there", Received: now},
		{ID: "id2", From: "bob@example.com", Subject: "Invoice", Snippet: "Please pay", Received: now},
		{ID: "id3", From: "carol@example.com", Subject: "Meeting", Snippet: "Tomorrow", Received: now},
	}

	ts := newTestToolset(t, client)
	tool := toolByName(t, ts, "gmail_list_unread")

	result := tool.Execute(context.Background(), map[string]any{"account": "darra2"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	var msgs []GmailMessage
	if err := json.Unmarshal([]byte(result.ForLLM), &msgs); err != nil {
		t.Fatalf("failed to parse result JSON: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	ids := map[string]bool{"id1": true, "id2": true, "id3": true}
	subjects := map[string]bool{"Hello": true, "Invoice": true, "Meeting": true}
	for _, m := range msgs {
		if !ids[m.ID] {
			t.Errorf("unexpected id: %s", m.ID)
		}
		if !subjects[m.Subject] {
			t.Errorf("unexpected subject: %s", m.Subject)
		}
	}
}

func TestGmailListUnread_RespectsMax(t *testing.T) {
	client := newFakeClient()
	ts := newTestToolset(t, client)
	tool := toolByName(t, ts, "gmail_list_unread")

	// max: 5 should be passed through.
	tool.Execute(context.Background(), map[string]any{
		"account": "darra2",
		"max":     float64(5),
	})
	if client.lastListMaxResults != 5 {
		t.Errorf("expected maxResults=5, got %d", client.lastListMaxResults)
	}

	// max: 9999 should be capped at 50.
	tool.Execute(context.Background(), map[string]any{
		"account": "darra2",
		"max":     float64(9999),
	})
	if client.lastListMaxResults != 50 {
		t.Errorf("expected maxResults=50 (capped), got %d", client.lastListMaxResults)
	}
}

func TestGmailListUnread_DefaultsSinceTo24hAgo(t *testing.T) {
	client := newFakeClient()
	ts := newTestToolset(t, client)
	tool := toolByName(t, ts, "gmail_list_unread")

	before := time.Now().Add(-24 * time.Hour)
	tool.Execute(context.Background(), map[string]any{"account": "darra2"})
	after := time.Now().Add(-24 * time.Hour)

	got := client.lastListSince
	// Allow a 5 second window around the expected value.
	if got.Before(before.Add(-5*time.Second)) || got.After(after.Add(5*time.Second)) {
		t.Errorf("since should be ~now-24h; got %v, window [%v, %v]", got, before, after)
	}
}

func TestGmailGetBody_HappyPath(t *testing.T) {
	client := newFakeClient()
	client.bodies["darra2"] = map[string]GmailBody{
		"msg1": {
			From:    "sender@example.com",
			To:      "me@example.com",
			Subject: "Test subject",
			Date:    time.Now().UTC(),
			Text:    "This is the body text.",
		},
	}

	ts := newTestToolset(t, client)
	tool := toolByName(t, ts, "gmail_get_body")

	result := tool.Execute(context.Background(), map[string]any{
		"account": "darra2",
		"id":      "msg1",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "From:") {
		t.Errorf("result should contain 'From:', got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Subject:") {
		t.Errorf("result should contain 'Subject:', got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "This is the body text.") {
		t.Errorf("result should contain body text, got: %s", result.ForLLM)
	}
}

func TestGmailGetBody_NotFound(t *testing.T) {
	client := newFakeClient()
	client.bodyErr = fmt.Errorf("message not found")

	ts := newTestToolset(t, client)
	tool := toolByName(t, ts, "gmail_get_body")

	result := tool.Execute(context.Background(), map[string]any{
		"account": "darra2",
		"id":      "nonexistent",
	})
	if !result.IsError {
		t.Fatal("expected error result for not-found message")
	}
}

func TestHtmlToText_Basic(t *testing.T) {
	// Trace: </p> → \n, <br> → \n, strip tags
	// Input:  <p>hello</p><br>world
	// After </p>→\n:  <p>hello\n<br>world
	// After <br>→\n:  <p>hello\n\nworld
	// After strip <[^>]+>: hello\n\nworld
	input := "<p>hello</p><br>world"
	got := htmlToText(input)
	want := "hello\n\nworld"
	if got != want {
		t.Errorf("htmlToText(%q) = %q, want %q", input, got, want)
	}
}
