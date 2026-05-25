package tools

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WikiToolset holds the five wiki tools that share a wikiDir root and a
// workspaceDir for proposal state.  Construct via NewWikiToolset; do not use
// the zero value.
type WikiToolset struct {
	wikiDir      string
	workspaceDir string
	mu           sync.Mutex
}

// NewWikiToolset returns a WikiToolset rooted at wikiDir.  workspaceDir is the
// agent workspace used for proposals.json and other state files.
func NewWikiToolset(wikiDir, workspaceDir string) *WikiToolset {
	return &WikiToolset{
		wikiDir:      filepath.Clean(wikiDir),
		workspaceDir: filepath.Clean(workspaceDir),
	}
}

// Tools returns all five wiki tool implementations.
func (ws *WikiToolset) Tools() []Tool {
	return []Tool{
		&wikiSearchTool{ws: ws},
		&wikiReadTool{ws: ws},
		&wikiListTool{ws: ws},
		&wikiAppendInboxTool{ws: ws},
		&wikiProposeTool{ws: ws},
	}
}

// canonicalize resolves path relative to wikiDir and rejects traversal.
// Returns the cleaned absolute path, or an error if traversal is detected.
func (ws *WikiToolset) canonicalize(raw string) (string, error) {
	if strings.Contains(raw, "..") {
		return "", fmt.Errorf("path must not contain ..")
	}
	abs := filepath.Join(ws.wikiDir, filepath.FromSlash(raw))
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(ws.wikiDir, abs)
	if err != nil {
		return "", fmt.Errorf("invalid path")
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes wiki directory")
	}
	return abs, nil
}

func (ws *WikiToolset) stateDir() string {
	return filepath.Join(ws.workspaceDir, "state")
}

func (ws *WikiToolset) proposalsPath() string {
	return filepath.Join(ws.stateDir(), "proposals.json")
}

// ---------------------------------------------------------------------------
// Frontmatter helpers
// ---------------------------------------------------------------------------

type frontmatter struct {
	title string
	tags  []string
}

func parseFrontmatter(path string) frontmatter {
	f, err := os.Open(path)
	if err != nil {
		return frontmatter{}
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return frontmatter{}
	}
	if strings.TrimSpace(sc.Text()) != "---" {
		return frontmatter{}
	}

	var fm frontmatter
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "title:") {
			fm.title = strings.TrimSpace(line[len("title:"):])
		} else if strings.HasPrefix(lower, "tags:") {
			raw := strings.TrimSpace(line[len("tags:"):])
			raw = strings.Trim(raw, "[]")
			for _, t := range strings.Split(raw, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					fm.tags = append(fm.tags, t)
				}
			}
		}
	}
	return fm
}

func snippetFrom(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	inFront := false
	pastFront := false
	var lines []string
	for sc.Scan() {
		line := sc.Text()
		if !pastFront {
			if strings.TrimSpace(line) == "---" {
				if !inFront {
					inFront = true
					continue
				}
				pastFront = true
				continue
			}
			if inFront {
				continue
			}
			pastFront = true
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) == 3 {
			break
		}
	}
	return strings.Join(lines, " ")
}

// ---------------------------------------------------------------------------
// Proposal state
// ---------------------------------------------------------------------------

type proposal struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	Reason    string `json:"reason"`
	ExpiresAt int64  `json:"expires_at"` // Unix seconds
}

func (ws *WikiToolset) loadProposals() ([]proposal, error) {
	data, err := os.ReadFile(ws.proposalsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []proposal
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (ws *WikiToolset) saveProposals(proposals []proposal) error {
	if err := os.MkdirAll(ws.stateDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(proposals, "", "  ")
	if err != nil {
		return err
	}
	tmp := ws.proposalsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ws.proposalsPath())
}

func newProposalID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// RFC 4122 version 4
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// purgeExpired removes proposals past their expiry; caller holds ws.mu.
func (ws *WikiToolset) purgeExpired(proposals []proposal) []proposal {
	now := time.Now().Unix()
	out := proposals[:0]
	for _, p := range proposals {
		if p.ExpiresAt > now {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Git helper
// ---------------------------------------------------------------------------

func gitCommit(dir, message string) error {
	add := exec.Command("git", "-C", dir, "add", "-A")
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	commit := exec.Command("git", "-C", dir, "commit", "-m", message)
	if out, err := commit.CombinedOutput(); err != nil {
		outStr := strings.TrimSpace(string(out))
		if strings.Contains(outStr, "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %w: %s", err, outStr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// wiki_search
// ---------------------------------------------------------------------------

type wikiSearchTool struct{ ws *WikiToolset }

func (t *wikiSearchTool) Name() string { return "wiki_search" }
func (t *wikiSearchTool) Description() string {
	return "Search wiki notes by keyword. Returns up to 10 results with path, title, tags, and a short snippet. Prefers matches in frontmatter (title, tags) over body text."
}
func (t *wikiSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query. Case-insensitive substring match against title, tags, and file content.",
			},
		},
		"required": []string{"query"},
	}
}

type wikiSearchResult struct {
	Path    string   `json:"path"`
	Title   string   `json:"title,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Snippet string   `json:"snippet,omitempty"`
}

func (t *wikiSearchTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	query, _ := args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return ErrorResult("query is required")
	}
	lower := strings.ToLower(query)

	type scored struct {
		res   wikiSearchResult
		score int
	}

	var results []scored

	_ = filepath.WalkDir(t.ws.wikiDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}

		rel, err := filepath.Rel(t.ws.wikiDir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		fm := parseFrontmatter(path)
		score := 0

		if strings.Contains(strings.ToLower(fm.title), lower) {
			score += 2
		}
		for _, tag := range fm.tags {
			if strings.Contains(strings.ToLower(tag), lower) {
				score += 2
				break
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if score == 0 && !strings.Contains(strings.ToLower(string(data)), lower) {
			return nil
		}
		if score == 0 {
			score = 1
		}

		results = append(results, scored{
			res: wikiSearchResult{
				Path:    rel,
				Title:   fm.title,
				Tags:    fm.tags,
				Snippet: snippetFrom(path),
			},
			score: score,
		})
		return nil
	})

	// Sort: higher score first, then alphabetical by path
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && (results[j].score > results[j-1].score ||
			(results[j].score == results[j-1].score && results[j].res.Path < results[j-1].res.Path)); j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	top := results
	if len(top) > 10 {
		top = top[:10]
	}

	out := make([]wikiSearchResult, len(top))
	for i, s := range top {
		out[i] = s.res
	}

	if len(out) == 0 {
		return NewToolResult(fmt.Sprintf("No wiki pages matched %q.", query))
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize results: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// wiki_read
// ---------------------------------------------------------------------------

type wikiReadTool struct{ ws *WikiToolset }

func (t *wikiReadTool) Name() string { return "wiki_read" }
func (t *wikiReadTool) Description() string {
	return "Read the full contents of a wiki page. Path is relative to the wiki root."
}
func (t *wikiReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Relative path to the wiki file, e.g. \"projects/picobot.md\".",
			},
		},
		"required": []string{"path"},
	}
}

func (t *wikiReadTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	raw, _ := args["path"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrorResult("path is required")
	}

	abs, err := t.ws.canonicalize(raw)
	if err != nil {
		return ErrorResult(err.Error())
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrorResult(fmt.Sprintf("wiki page not found: %s", raw))
		}
		return ErrorResult("read error: " + err.Error())
	}

	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// wiki_list
// ---------------------------------------------------------------------------

type wikiListTool struct{ ws *WikiToolset }

func (t *wikiListTool) Name() string { return "wiki_list" }
func (t *wikiListTool) Description() string {
	return "List wiki pages in a directory. Returns path and title for each .md file. dir is relative to the wiki root; use \"\" or \".\" for the root."
}
func (t *wikiListTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"dir": map[string]any{
				"type":        "string",
				"description": "Relative directory path within the wiki, e.g. \"projects\". Use \"\" for the root.",
			},
		},
		"required": []string{},
	}
}

type wikiListEntry struct {
	Path  string `json:"path"`
	Title string `json:"title,omitempty"`
}

func (t *wikiListTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	rawDir, _ := args["dir"].(string)
	rawDir = strings.TrimSpace(rawDir)

	var absDir string
	if rawDir == "" || rawDir == "." {
		absDir = t.ws.wikiDir
	} else {
		var err error
		absDir, err = t.ws.canonicalize(rawDir)
		if err != nil {
			return ErrorResult(err.Error())
		}
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrorResult(fmt.Sprintf("directory not found: %s", rawDir))
		}
		return ErrorResult("read error: " + err.Error())
	}

	var results []wikiListEntry
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			rel, _ := filepath.Rel(t.ws.wikiDir, filepath.Join(absDir, name))
			results = append(results, wikiListEntry{Path: filepath.ToSlash(rel) + "/"})
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		abs := filepath.Join(absDir, name)
		rel, _ := filepath.Rel(t.ws.wikiDir, abs)
		fm := parseFrontmatter(abs)
		results = append(results, wikiListEntry{
			Path:  filepath.ToSlash(rel),
			Title: fm.title,
		})
	}

	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// wiki_append_to_inbox
// ---------------------------------------------------------------------------

type wikiAppendInboxTool struct{ ws *WikiToolset }

func (t *wikiAppendInboxTool) Name() string { return "wiki_append_to_inbox" }
func (t *wikiAppendInboxTool) Description() string {
	return "Append a note directly to the wiki inbox (inbox.md). Use this for quick captures that will be processed later. Always commits to git automatically."
}
func (t *wikiAppendInboxTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{
				"type":        "string",
				"description": "The note text to append to inbox.md.",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "Optional source or context label (e.g. \"telegram\", \"voice\").",
			},
		},
		"required": []string{"text"},
	}
}

func (t *wikiAppendInboxTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	text, _ := args["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return ErrorResult("text is required")
	}
	source, _ := args["source"].(string)
	source = strings.TrimSpace(source)

	inboxPath := filepath.Join(t.ws.wikiDir, "inbox.md")

	now := time.Now().UTC().Format("2006-01-02 15:04")
	var entry string
	if source != "" {
		entry = fmt.Sprintf("\n- [%s] (%s) %s\n", now, source, text)
	} else {
		entry = fmt.Sprintf("\n- [%s] %s\n", now, text)
	}

	t.ws.mu.Lock()
	defer t.ws.mu.Unlock()

	f, err := os.OpenFile(inboxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return ErrorResult("failed to open inbox: " + err.Error())
	}
	if _, err := f.WriteString(entry); err != nil {
		f.Close()
		return ErrorResult("failed to write inbox: " + err.Error())
	}
	f.Close()

	if err := gitCommit(t.ws.wikiDir, "inbox: add note"); err != nil {
		return NewToolResult(fmt.Sprintf("Appended to inbox. Warning: git commit failed: %s", err.Error()))
	}

	return NewToolResult("Appended to inbox.md and committed.")
}

// ---------------------------------------------------------------------------
// wiki_propose_write
// ---------------------------------------------------------------------------

type wikiProposeTool struct{ ws *WikiToolset }

func (t *wikiProposeTool) Name() string { return "wiki_propose_write" }
func (t *wikiProposeTool) Description() string {
	return "Propose writing or overwriting a wiki page. Does not write immediately — creates a pending proposal that Enrico must approve with /apply <id>. The proposal expires in 15 minutes."
}
func (t *wikiProposeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Relative path of the wiki page to write, e.g. \"projects/picobot.md\".",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full markdown content for the page.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Brief explanation of why this write is being proposed.",
			},
		},
		"required": []string{"path", "content", "reason"},
	}
}

func (t *wikiProposeTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	rawPath, _ := args["path"].(string)
	rawPath = strings.TrimSpace(rawPath)
	content, _ := args["content"].(string)
	reason, _ := args["reason"].(string)
	reason = strings.TrimSpace(reason)

	if rawPath == "" {
		return ErrorResult("path is required")
	}
	if content == "" {
		return ErrorResult("content is required")
	}
	if reason == "" {
		return ErrorResult("reason is required")
	}

	// Validate target path without creating the file
	if _, err := t.ws.canonicalize(rawPath); err != nil {
		return ErrorResult(err.Error())
	}

	id, err := newProposalID()
	if err != nil {
		return ErrorResult("failed to generate proposal ID: " + err.Error())
	}

	p := proposal{
		ID:        id,
		Path:      rawPath,
		Content:   content,
		Reason:    reason,
		ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
	}

	t.ws.mu.Lock()
	defer t.ws.mu.Unlock()

	existing, err := t.ws.loadProposals()
	if err != nil {
		return ErrorResult("failed to load proposals: " + err.Error())
	}
	existing = t.ws.purgeExpired(existing)
	existing = append(existing, p)
	if err := t.ws.saveProposals(existing); err != nil {
		return ErrorResult("failed to save proposal: " + err.Error())
	}

	return &ToolResult{
		ForLLM:  fmt.Sprintf("Proposal queued for %s. Awaiting user approval via Telegram.", rawPath),
		ForUser: fmt.Sprintf("Wiki write proposal ready.\nPath: %s\nReason: %s\nRun /apply %s to confirm (expires in 15 min).", rawPath, reason, id),
	}
}

// ---------------------------------------------------------------------------
// ApplyProposal and RejectProposal — called by the command handler
// ---------------------------------------------------------------------------

// ApplyProposal finds proposal by id, writes the file, commits to git, and removes the proposal.
// Returns a user-facing result string, or an error string.
func (ws *WikiToolset) ApplyProposal(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("proposal ID is required")
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	proposals, err := ws.loadProposals()
	if err != nil {
		return "", fmt.Errorf("failed to load proposals: %w", err)
	}
	proposals = ws.purgeExpired(proposals)

	var found proposal
	foundOK := false
	remaining := make([]proposal, 0, len(proposals))
	for _, p := range proposals {
		if !foundOK && p.ID == id {
			found = p
			foundOK = true
		} else {
			remaining = append(remaining, p)
		}
	}
	if !foundOK {
		return "", fmt.Errorf("no active proposal with ID %s", id)
	}

	abs, err := ws.canonicalize(found.Path)
	if err != nil {
		return "", fmt.Errorf("invalid path in proposal: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("failed to create directory: %w", err)
	}

	tmp := abs + ".tmp"
	if err := os.WriteFile(tmp, []byte(found.Content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		return "", fmt.Errorf("failed to rename file: %w", err)
	}

	msg := fmt.Sprintf("wiki: write %s", found.Path)
	if found.Reason != "" {
		msg += "\n\n" + found.Reason
	}
	if err := gitCommit(ws.wikiDir, msg); err != nil {
		// File is written; git failure is non-fatal but worth reporting
		if saveErr := ws.saveProposals(remaining); saveErr == nil {
			return fmt.Sprintf("Written: %s\nWarning: git commit failed: %s", found.Path, err.Error()), nil
		}
	}

	if err := ws.saveProposals(remaining); err != nil {
		return fmt.Sprintf("Written and committed: %s\nWarning: could not clean up proposal: %s", found.Path, err.Error()), nil
	}

	return fmt.Sprintf("Applied. Written and committed: %s", found.Path), nil
}

// RejectProposal removes a proposal by id without writing anything.
func (ws *WikiToolset) RejectProposal(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("proposal ID is required")
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	proposals, err := ws.loadProposals()
	if err != nil {
		return "", fmt.Errorf("failed to load proposals: %w", err)
	}
	proposals = ws.purgeExpired(proposals)

	remaining := proposals[:0]
	found := false
	for _, p := range proposals {
		if p.ID == id {
			found = true
		} else {
			remaining = append(remaining, p)
		}
	}
	if !found {
		return "", fmt.Errorf("no active proposal with ID %s", id)
	}

	if err := ws.saveProposals(remaining); err != nil {
		return "", fmt.Errorf("failed to update proposals: %w", err)
	}
	return fmt.Sprintf("Rejected proposal %s.", id), nil
}

// ListProposals returns a human-readable summary of active proposals.
func (ws *WikiToolset) ListProposals() string {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	proposals, err := ws.loadProposals()
	if err != nil {
		return "Failed to load proposals: " + err.Error()
	}
	proposals = ws.purgeExpired(proposals)
	if len(proposals) == 0 {
		return "No active wiki proposals."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d active proposal(s):\n", len(proposals)))
	for _, p := range proposals {
		expires := time.Unix(p.ExpiresAt, 0).UTC().Format("15:04 UTC")
		sb.WriteString(fmt.Sprintf("  %s  %s  (expires %s)\n    Reason: %s\n",
			p.ID, p.Path, expires, p.Reason))
	}
	return sb.String()
}

// AppendToInbox is the non-LLM path called by the /note command handler.
func (ws *WikiToolset) AppendToInbox(text, source string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("note text is required")
	}
	source = strings.TrimSpace(source)

	inboxPath := filepath.Join(ws.wikiDir, "inbox.md")

	now := time.Now().UTC().Format("2006-01-02 15:04")
	var entry string
	if source != "" {
		entry = fmt.Sprintf("\n- [%s] (%s) %s\n", now, source, text)
	} else {
		entry = fmt.Sprintf("\n- [%s] %s\n", now, text)
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	f, err := os.OpenFile(inboxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("failed to open inbox: %w", err)
	}
	if _, err := f.WriteString(entry); err != nil {
		f.Close()
		return "", fmt.Errorf("failed to write inbox: %w", err)
	}
	f.Close()

	if err := gitCommit(ws.wikiDir, "inbox: add note"); err != nil {
		return fmt.Sprintf("Noted. (git commit failed: %s)", err.Error()), nil
	}

	return "Noted and committed to inbox.", nil
}
