package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// ---------------------------------------------------------------------------
// fakeOutlookClient — in-memory OutlookClient for tests.
// ---------------------------------------------------------------------------

type fakeOutlookClient struct {
	messages []OutlookMessage
	bodies   map[string]OutlookBody

	// Captured args from the most recent ListUnread call.
	lastListSince      time.Time
	lastListMaxResults int

	// If set, ListUnread returns this error.
	listErr error
	// If set, GetBody returns this error.
	bodyErr error
}

func newFakeOutlookClient() *fakeOutlookClient {
	return &fakeOutlookClient{
		bodies: make(map[string]OutlookBody),
	}
}

func (f *fakeOutlookClient) ListUnread(
	_ context.Context, since time.Time, maxResults int,
) ([]OutlookMessage, error) {
	f.lastListSince = since
	f.lastListMaxResults = maxResults
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.messages, nil
}

func (f *fakeOutlookClient) GetBody(
	_ context.Context, id string,
) (OutlookBody, error) {
	if f.bodyErr != nil {
		return OutlookBody{}, f.bodyErr
	}
	b, ok := f.bodies[id]
	if !ok {
		return OutlookBody{}, fmt.Errorf("not found: id %q", id)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestOutlookToolset(t *testing.T, client OutlookClient) *OutlookToolset {
	t.Helper()
	ts, err := NewOutlookToolset(client)
	if err != nil {
		t.Fatalf("NewOutlookToolset: %v", err)
	}
	return ts
}

func outlookToolByName(t *testing.T, ts *OutlookToolset, name string) Tool {
	t.Helper()
	for _, tool := range ts.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found in outlook toolset", name)
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestOutlookListUnread_HappyPath(t *testing.T) {
	client := newFakeOutlookClient()
	now := time.Now().UTC()
	client.messages = []OutlookMessage{
		{ID: "oid1", From: "alice@example.com", Subject: "Hello", Preview: "Hi there", Received: now},
		{ID: "oid2", From: "bob@example.com", Subject: "Invoice", Preview: "Please pay", Received: now},
		{ID: "oid3", From: "carol@example.com", Subject: "Meeting", Preview: "Tomorrow at 3pm", Received: now},
	}

	ts := newTestOutlookToolset(t, client)
	tool := outlookToolByName(t, ts, "outlook_list_unread")

	result := tool.Execute(context.Background(), map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	var msgs []OutlookMessage
	if err := json.Unmarshal([]byte(result.ForLLM), &msgs); err != nil {
		t.Fatalf("failed to parse result JSON: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	ids := map[string]bool{"oid1": true, "oid2": true, "oid3": true}
	for _, m := range msgs {
		if !ids[m.ID] {
			t.Errorf("unexpected id: %s", m.ID)
		}
	}
}

func TestOutlookListUnread_RespectsMax(t *testing.T) {
	client := newFakeOutlookClient()
	ts := newTestOutlookToolset(t, client)
	tool := outlookToolByName(t, ts, "outlook_list_unread")

	// max: 5 should be passed through.
	tool.Execute(context.Background(), map[string]any{
		"max": float64(5),
	})
	if client.lastListMaxResults != 5 {
		t.Errorf("expected maxResults=5, got %d", client.lastListMaxResults)
	}

	// max: 9999 should be capped at 50.
	tool.Execute(context.Background(), map[string]any{
		"max": float64(9999),
	})
	if client.lastListMaxResults != 50 {
		t.Errorf("expected maxResults=50 (capped), got %d", client.lastListMaxResults)
	}
}

func TestOutlookListUnread_DefaultsSinceTo24hAgo(t *testing.T) {
	client := newFakeOutlookClient()
	ts := newTestOutlookToolset(t, client)
	tool := outlookToolByName(t, ts, "outlook_list_unread")

	before := time.Now().Add(-24 * time.Hour)
	tool.Execute(context.Background(), map[string]any{})
	after := time.Now().Add(-24 * time.Hour)

	got := client.lastListSince
	// Allow a 5-second window around the expected value.
	if got.Before(before.Add(-5*time.Second)) || got.After(after.Add(5*time.Second)) {
		t.Errorf("since should be ~now-24h; got %v, window [%v, %v]", got, before, after)
	}
}

func TestOutlookGetBody_HappyPath(t *testing.T) {
	client := newFakeOutlookClient()
	client.bodies["omsg1"] = OutlookBody{
		From:    "sender@example.com",
		To:      "me@outlook.com",
		Subject: "Test Outlook subject",
		Date:    time.Now().UTC(),
		Text:    "This is the Outlook body text.",
	}

	ts := newTestOutlookToolset(t, client)
	tool := outlookToolByName(t, ts, "outlook_get_body")

	result := tool.Execute(context.Background(), map[string]any{
		"id": "omsg1",
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
	if !strings.Contains(result.ForLLM, "This is the Outlook body text.") {
		t.Errorf("result should contain body text, got: %s", result.ForLLM)
	}
}

func TestOutlookGetBody_Required_ID(t *testing.T) {
	client := newFakeOutlookClient()
	ts := newTestOutlookToolset(t, client)
	tool := outlookToolByName(t, ts, "outlook_get_body")

	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error result when id is missing")
	}
	if !strings.Contains(result.ForLLM, "id is required") {
		t.Errorf("error message should mention 'id is required', got: %s", result.ForLLM)
	}
}

func TestOutlookGetBody_NotFound(t *testing.T) {
	client := newFakeOutlookClient()
	client.bodyErr = fmt.Errorf("message not found")

	ts := newTestOutlookToolset(t, client)
	tool := outlookToolByName(t, ts, "outlook_get_body")

	result := tool.Execute(context.Background(), map[string]any{
		"id": "nonexistent-id",
	})
	if !result.IsError {
		t.Fatal("expected error result for not-found message")
	}
}

// stubTokenSource returns a token with a controllable refresh token so the
// persisting wrapper can be tested without a live token endpoint.
type stubTokenSource struct {
	tok *oauth2.Token
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) { return s.tok, nil }

func TestPersistingTokenSource_WritesRotatedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rt")

	stub := &stubTokenSource{tok: &oauth2.Token{RefreshToken: "rt-v1", TokenType: "Bearer"}}
	p := &persistingTokenSource{inner: stub, path: path, last: "rt-v0"}

	if _, err := p.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "rt-v1" {
		t.Errorf("expected file to contain rotated token rt-v1, got %q", string(got))
	}

	// Rotating again writes the new token.
	stub.tok = &oauth2.Token{RefreshToken: "rt-v2", TokenType: "Bearer"}
	if _, err := p.Token(); err != nil {
		t.Fatalf("Token v2: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "rt-v2" {
		t.Errorf("expected file to be updated to rt-v2, got %q", string(got))
	}
}

func TestReadPersistedRefreshToken_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if got := readPersistedRefreshToken(filepath.Join(dir, "nope")); got != "" {
		t.Errorf("expected empty string for missing file, got %q", got)
	}
}

func TestReadPersistedRefreshToken_StripsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rt")
	if err := os.WriteFile(path, []byte("  tok-with-newline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readPersistedRefreshToken(path); got != "tok-with-newline" {
		t.Errorf("expected stripped, got %q", got)
	}
}
