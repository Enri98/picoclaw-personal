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

// newWikiSetup creates a temp wiki dir, a temp workspace dir, and a WikiToolset.
// It also creates stateDir for proposals.
func newWikiSetup(t *testing.T) (ws *WikiToolset, wikiDir, workspaceDir string) {
	t.Helper()
	wikiDir = t.TempDir()
	workspaceDir = t.TempDir()
	// Create state dir upfront so tests can pre-populate it
	if err := os.MkdirAll(filepath.Join(workspaceDir, "state"), 0o755); err != nil {
		t.Fatalf("MkdirAll state: %v", err)
	}
	ws = NewWikiToolset(wikiDir, workspaceDir)
	return
}

// writeWikiFile writes content to a path relative to wikiDir.
func writeWikiFile(t *testing.T, wikiDir, rel, content string) {
	t.Helper()
	abs := filepath.Join(wikiDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("MkdirAll for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", rel, err)
	}
}

// ---------------------------------------------------------------------------
// wiki_read
// ---------------------------------------------------------------------------

func TestWikiRead_ValidPath(t *testing.T) {
	ws, wikiDir, _ := newWikiSetup(t)
	content := "# Hello\n\nThis is a test page."
	writeWikiFile(t, wikiDir, "test.md", content)

	tools := ws.Tools()
	var readTool Tool
	for _, tool := range tools {
		if tool.Name() == "wiki_read" {
			readTool = tool
			break
		}
	}
	if readTool == nil {
		t.Fatal("wiki_read tool not found")
	}

	result := readTool.Execute(context.Background(), map[string]any{"path": "test.md"})
	if result.IsError {
		t.Errorf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "Hello") {
		t.Errorf("result missing expected content: %q", result.ForLLM)
	}
}

func TestWikiRead_PathTraversal_Rejected(t *testing.T) {
	ws, _, _ := newWikiSetup(t)

	tools := ws.Tools()
	var readTool Tool
	for _, tool := range tools {
		if tool.Name() == "wiki_read" {
			readTool = tool
			break
		}
	}
	if readTool == nil {
		t.Fatal("wiki_read tool not found")
	}

	result := readTool.Execute(context.Background(), map[string]any{"path": "../etc/passwd"})
	if !result.IsError {
		t.Errorf("expected error for path traversal, but got success: %q", result.ForLLM)
	}
}

func TestWikiRead_NonExistentFile(t *testing.T) {
	ws, _, _ := newWikiSetup(t)

	tools := ws.Tools()
	var readTool Tool
	for _, tool := range tools {
		if tool.Name() == "wiki_read" {
			readTool = tool
			break
		}
	}
	if readTool == nil {
		t.Fatal("wiki_read tool not found")
	}

	result := readTool.Execute(context.Background(), map[string]any{"path": "does-not-exist.md"})
	if !result.IsError {
		t.Errorf("expected error for missing file, but got success: %q", result.ForLLM)
	}
}

// ---------------------------------------------------------------------------
// wiki_append_to_inbox
// ---------------------------------------------------------------------------

func TestWikiAppendToInbox_CreatesFile(t *testing.T) {
	ws, wikiDir, _ := newWikiSetup(t)

	tools := ws.Tools()
	var inboxTool Tool
	for _, tool := range tools {
		if tool.Name() == "wiki_append_to_inbox" {
			inboxTool = tool
			break
		}
	}
	if inboxTool == nil {
		t.Fatal("wiki_append_to_inbox tool not found")
	}

	result := inboxTool.Execute(context.Background(), map[string]any{
		"text":   "remember to water the plants",
		"source": "telegram",
	})
	// The tool should not fail fatally even if git commit fails (no git repo in temp dir)
	if result.IsError {
		t.Errorf("unexpected error result: %s", result.ForLLM)
	}

	inboxPath := filepath.Join(wikiDir, "inbox.md")
	data, err := os.ReadFile(inboxPath)
	if err != nil {
		t.Fatalf("inbox.md not created: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "remember to water the plants") {
		t.Errorf("inbox.md missing note text: %q", body)
	}
	if !strings.Contains(body, "telegram") {
		t.Errorf("inbox.md missing source label: %q", body)
	}
}

// ---------------------------------------------------------------------------
// wiki_propose_write
// ---------------------------------------------------------------------------

func TestWikiProposeWrite_CreatesProposal(t *testing.T) {
	ws, _, workspaceDir := newWikiSetup(t)

	tools := ws.Tools()
	var proposeTool Tool
	for _, tool := range tools {
		if tool.Name() == "wiki_propose_write" {
			proposeTool = tool
			break
		}
	}
	if proposeTool == nil {
		t.Fatal("wiki_propose_write tool not found")
	}

	result := proposeTool.Execute(context.Background(), map[string]any{
		"path":    "notes/new-page.md",
		"content": "# New Page\n\nSome content.",
		"reason":  "user requested new page",
	})
	if result.IsError {
		t.Fatalf("propose_write failed: %s", result.ForLLM)
	}

	// ForLLM must NOT contain the proposal ID (security: don't leak approval token)
	// ForUser contains the ID. Extract it from ForUser.
	if !strings.Contains(result.ForUser, "/apply") {
		t.Errorf("ForUser missing '/apply': %q", result.ForUser)
	}

	// Verify proposals.json exists and has the entry
	proposalsPath := filepath.Join(workspaceDir, "state", "proposals.json")
	data, err := os.ReadFile(proposalsPath)
	if err != nil {
		t.Fatalf("proposals.json not created: %v", err)
	}
	var proposals []proposal
	if err := json.Unmarshal(data, &proposals); err != nil {
		t.Fatalf("unmarshal proposals.json: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("expected 1 proposal, got %d", len(proposals))
	}
	p := proposals[0]

	// The ID must NOT appear in ForLLM
	if strings.Contains(result.ForLLM, p.ID) {
		t.Errorf("ForLLM must not contain proposal ID %q, but it does: %q", p.ID, result.ForLLM)
	}

	if p.Path != "notes/new-page.md" {
		t.Errorf("proposal path = %q, want notes/new-page.md", p.Path)
	}
}

// ---------------------------------------------------------------------------
// ApplyProposal
// ---------------------------------------------------------------------------

func TestApplyProposal_WritesFileAndRemovesProposal(t *testing.T) {
	ws, wikiDir, workspaceDir := newWikiSetup(t)

	// Pre-populate a proposal
	id := "deadbeef-0001-4000-8000-aabbccddeeff"
	p := proposal{
		ID:        id,
		Path:      "projects/picobot.md",
		Content:   "# PicoBot\n\nProject notes.",
		Reason:    "initial write",
		ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
	}
	data, _ := json.MarshalIndent([]proposal{p}, "", "  ")
	if err := os.WriteFile(filepath.Join(workspaceDir, "state", "proposals.json"), data, 0o644); err != nil {
		t.Fatalf("pre-populate proposals.json: %v", err)
	}

	// Ensure target directory exists in wiki
	if err := os.MkdirAll(filepath.Join(wikiDir, "projects"), 0o755); err != nil {
		t.Fatalf("MkdirAll projects: %v", err)
	}

	msg, err := ws.ApplyProposal(id)
	if err != nil {
		t.Fatalf("ApplyProposal returned error: %v", err)
	}
	_ = msg // success message may vary based on git availability

	// File must exist with correct content
	written, readErr := os.ReadFile(filepath.Join(wikiDir, "projects", "picobot.md"))
	if readErr != nil {
		t.Fatalf("written file not found: %v", readErr)
	}
	if string(written) != p.Content {
		t.Errorf("file content mismatch: got %q, want %q", string(written), p.Content)
	}

	// Proposals.json must no longer contain the ID
	remaining, readErr := os.ReadFile(filepath.Join(workspaceDir, "state", "proposals.json"))
	if readErr != nil {
		t.Fatalf("proposals.json missing after apply: %v", readErr)
	}
	var remainingProposals []proposal
	if err := json.Unmarshal(remaining, &remainingProposals); err != nil {
		t.Fatalf("unmarshal remaining proposals: %v", err)
	}
	for _, rp := range remainingProposals {
		if rp.ID == id {
			t.Errorf("proposal %s still present after apply", id)
		}
	}
}

func TestApplyProposal_ExpiredProposalReturnsError(t *testing.T) {
	ws, _, workspaceDir := newWikiSetup(t)

	// Pre-populate an expired proposal
	id := "expired0-0001-4000-8000-aabbccddeeff"
	p := proposal{
		ID:        id,
		Path:      "notes/expired.md",
		Content:   "# Expired",
		Reason:    "test",
		ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(), // already expired
	}
	data, _ := json.MarshalIndent([]proposal{p}, "", "  ")
	if err := os.WriteFile(filepath.Join(workspaceDir, "state", "proposals.json"), data, 0o644); err != nil {
		t.Fatalf("pre-populate proposals.json: %v", err)
	}

	_, err := ws.ApplyProposal(id)
	if err == nil {
		t.Error("expected error for expired proposal, got nil")
	}
	if !strings.Contains(err.Error(), "no active proposal") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestApplyProposal_FabricatedIDReturnsError(t *testing.T) {
	ws, _, _ := newWikiSetup(t)

	_, err := ws.ApplyProposal("totally-fake-id-00000000")
	if err == nil {
		t.Error("expected error for fabricated ID, got nil")
	}
	if !strings.Contains(err.Error(), "no active proposal") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// wiki_search
// ---------------------------------------------------------------------------

func TestWikiSearch_MatchesFrontmatterTitle(t *testing.T) {
	ws, wikiDir, _ := newWikiSetup(t)

	writeWikiFile(t, wikiDir, "picobot.md", `---
title: PicoBot Project
tags: [raspberry-pi, assistant]
---

This is the PicoBot project page.
`)
	writeWikiFile(t, wikiDir, "unrelated.md", `---
title: Shopping List
---

Milk, eggs, bread.
`)

	tools := ws.Tools()
	var searchTool Tool
	for _, tool := range tools {
		if tool.Name() == "wiki_search" {
			searchTool = tool
			break
		}
	}
	if searchTool == nil {
		t.Fatal("wiki_search tool not found")
	}

	result := searchTool.Execute(context.Background(), map[string]any{"query": "PicoBot"})
	if result.IsError {
		t.Fatalf("search failed: %s", result.ForLLM)
	}

	var results []wikiSearchResult
	if err := json.Unmarshal([]byte(result.ForLLM), &results); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result, got none")
	}

	found := false
	for _, r := range results {
		if r.Path == "picobot.md" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("picobot.md not in results: %+v", results)
	}
}

// ---------------------------------------------------------------------------
// wiki_list
// ---------------------------------------------------------------------------

func TestWikiList_ReturnsMdFiles(t *testing.T) {
	ws, wikiDir, _ := newWikiSetup(t)

	writeWikiFile(t, wikiDir, "alpha.md", "# Alpha")
	writeWikiFile(t, wikiDir, "beta.md", "# Beta")
	// Non-md file — should be excluded
	if err := os.WriteFile(filepath.Join(wikiDir, "ignore.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("WriteFile ignore.txt: %v", err)
	}

	tools := ws.Tools()
	var listTool Tool
	for _, tool := range tools {
		if tool.Name() == "wiki_list" {
			listTool = tool
			break
		}
	}
	if listTool == nil {
		t.Fatal("wiki_list tool not found")
	}

	result := listTool.Execute(context.Background(), map[string]any{"dir": ""})
	if result.IsError {
		t.Fatalf("list failed: %s", result.ForLLM)
	}

	var entries []wikiListEntry
	if err := json.Unmarshal([]byte(result.ForLLM), &entries); err != nil {
		t.Fatalf("unmarshal entries: %v", err)
	}

	mdCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Path, ".md") {
			mdCount++
		}
		if e.Path == "ignore.txt" {
			t.Error("ignore.txt should not appear in wiki_list results")
		}
	}
	if mdCount < 2 {
		t.Errorf("expected at least 2 .md entries, got %d (entries: %+v)", mdCount, entries)
	}
}
