package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// claudeBotLogin is the GitHub username of the mention responder bot.
// GitHub Apps always post under "<app-name>[bot]"; verify against a real
// response before relying on it and adjust if the deployed app name differs.
const claudeBotLogin = "claude[bot]"

// defaultPollTTL is how long a watch entry lives before being dropped.
const defaultPollTTL = 24 * time.Hour

// pollInterval is how often the poller checks for new comments.
const pollInterval = 5 * time.Minute

// maxConsecutiveFailures is the threshold after which a poll entry is
// temporarily skipped for extra ticks (soft backoff).
const maxConsecutiveFailures = 5

// PollEntry describes a single active watch on a GitHub issue.
type PollEntry struct {
	ID                string        `json:"id"`                  // UUID v4 for this poll
	Owner             string        `json:"owner"`               // repository owner
	Repo              string        `json:"repo"`                // repository name
	IssueNumber       int           `json:"issue_number"`        // issue number to watch
	CreatedAt         time.Time     `json:"created_at"`          // when the watch was registered
	LastCommentIDSeen int64         `json:"last_comment_id_seen"` // highest GitHub comment ID forwarded; 0 = none
	TTL               time.Duration `json:"ttl"`                 // how long to watch; 0 uses defaultPollTTL
	ChatID            string        `json:"chat_id"`             // Telegram chat ID to notify
	ExpiryNotified    bool          `json:"expiry_notified"`     // true once the expiry notice was sent
}

// pollEntryState holds runtime state that is NOT persisted.
type pollEntryState struct {
	consecutiveFailures int
	skipTicksRemaining  int
}

// GitHubPoller polls GitHub issue comments in the background and forwards
// responses from the mention responder bot to a Telegram chat.
type GitHubPoller struct {
	pat            string
	channelManager *channels.Manager
	storePath      string
	httpClient     *http.Client

	mu      sync.Mutex
	entries []PollEntry
	states  map[string]*pollEntryState // keyed by PollEntry.ID

	started   bool
	startOnce sync.Once
}

// NewGitHubPoller constructs a GitHubPoller.
// stateDir is the directory that holds github_polls.json.
// channelManager may be nil at construction time and injected later via
// SetChannelManager before Start is called.
func NewGitHubPoller(pat string, channelManager *channels.Manager, stateDir string) (*GitHubPoller, error) {
	if pat == "" {
		return nil, fmt.Errorf("github_poller: PAT must not be empty")
	}
	if stateDir == "" {
		return nil, fmt.Errorf("github_poller: stateDir must not be empty")
	}

	p := &GitHubPoller{
		pat:            pat,
		channelManager: channelManager,
		storePath:      filepath.Join(stateDir, "github_polls.json"),
		httpClient:     &http.Client{Timeout: 15 * time.Second},
		states:         make(map[string]*pollEntryState),
	}

	if err := p.load(); err != nil {
		logger.WarnCF("github_poller", "Failed to load persisted poll entries; starting empty",
			map[string]any{"error": err.Error()})
	}

	return p, nil
}

// SetChannelManager injects the channel manager. Must be called before Start.
func (p *GitHubPoller) SetChannelManager(cm *channels.Manager) {
	p.mu.Lock()
	p.channelManager = cm
	p.mu.Unlock()
}

// Register appends a new watch entry and persists it.
// Called by the /claude command handler after creating an issue.
func (p *GitHubPoller) Register(entry PollEntry) error {
	if entry.TTL == 0 {
		entry.TTL = defaultPollTTL
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.ID == "" {
		id, err := newPollID()
		if err != nil {
			return fmt.Errorf("github_poller: failed to generate ID: %w", err)
		}
		entry.ID = id
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.entries = append(p.entries, entry)
	p.states[entry.ID] = &pollEntryState{}
	return p.save()
}

// Start launches the background polling goroutine. It is safe to call more
// than once; subsequent calls are no-ops.
func (p *GitHubPoller) Start(ctx context.Context) {
	p.startOnce.Do(func() {
		p.started = true
		go p.run(ctx)
	})
}

// ---------------------------------------------------------------------------
// Internal goroutine
// ---------------------------------------------------------------------------

func (p *GitHubPoller) run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

func (p *GitHubPoller) tick(ctx context.Context) {
	p.mu.Lock()
	cm := p.channelManager
	// Snapshot entries so we release the lock during HTTP calls.
	snapshot := make([]PollEntry, len(p.entries))
	copy(snapshot, p.entries)
	p.mu.Unlock()

	if cm == nil {
		// Channel manager not yet injected; skip this tick.
		return
	}

	for i := range snapshot {
		e := &snapshot[i]
		p.mu.Lock()
		st, ok := p.states[e.ID]
		if !ok {
			st = &pollEntryState{}
			p.states[e.ID] = st
		}
		// Soft backoff: skip this entry if it has accumulated skip ticks.
		if st.skipTicksRemaining > 0 {
			st.skipTicksRemaining--
			p.mu.Unlock()
			continue
		}
		p.mu.Unlock()

		ttl := e.TTL
		if ttl == 0 {
			ttl = defaultPollTTL
		}

		// Check expiry before polling.
		if time.Since(e.CreatedAt) > ttl {
			if !e.ExpiryNotified {
				msg := fmt.Sprintf(
					"[issue inquiry] No response on %s/%s#%d within %s. Dropping watch.",
					e.Owner, e.Repo, e.IssueNumber, ttl,
				)
				if err := cm.SendToChannel(ctx, "telegram", e.ChatID, msg); err != nil {
					logger.WarnCF("github_poller", "Failed to send expiry notice",
						map[string]any{"entry_id": e.ID, "error": err.Error()})
				}
				p.mu.Lock()
				// Update ExpiryNotified in the live entries slice.
				for j := range p.entries {
					if p.entries[j].ID == e.ID {
						p.entries[j].ExpiryNotified = true
					}
				}
				p.mu.Unlock()
			}
			p.dropEntry(e.ID)
			continue
		}

		// Poll for new comments.
		comments, err := p.fetchComments(ctx, e)
		if err != nil {
			logger.WarnCF("github_poller", "Failed to fetch comments; skipping entry this tick",
				map[string]any{
					"entry_id":    e.ID,
					"owner":       e.Owner,
					"repo":        e.Repo,
					"issue":       e.IssueNumber,
					"error":       err.Error(),
				})
			p.mu.Lock()
			st.consecutiveFailures++
			if st.consecutiveFailures >= maxConsecutiveFailures {
				st.skipTicksRemaining = 1 // skip the next tick
				st.consecutiveFailures = 0
			}
			p.mu.Unlock()
			continue
		}

		var maxID int64
		for _, c := range comments {
			if c.UserLogin != claudeBotLogin {
				continue
			}
			if c.ID <= e.LastCommentIDSeen {
				continue
			}
			formatted := fmt.Sprintf(
				"[issue inquiry response for %s/%s#%d]\n\n%s",
				e.Owner, e.Repo, e.IssueNumber, c.Body,
			)
			if sendErr := cm.SendToChannel(ctx, "telegram", e.ChatID, formatted); sendErr != nil {
				logger.WarnCF("github_poller", "Failed to send comment notification",
					map[string]any{"entry_id": e.ID, "comment_id": c.ID, "error": sendErr.Error()})
			}
			if c.ID > maxID {
				maxID = c.ID
			}
		}

		// Persist progress if we saw new comments.
		if maxID > 0 {
			p.mu.Lock()
			for j := range p.entries {
				if p.entries[j].ID == e.ID {
					p.entries[j].LastCommentIDSeen = maxID
					break
				}
			}
			_ = p.save()
			p.mu.Unlock()
		}

		// Reset failure counter on any successful poll.
		p.mu.Lock()
		st.consecutiveFailures = 0
		st.skipTicksRemaining = 0
		p.mu.Unlock()
	}
}

// dropEntry removes a PollEntry by ID from the live list and persists.
func (p *GitHubPoller) dropEntry(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	remaining := p.entries[:0]
	for _, e := range p.entries {
		if e.ID != id {
			remaining = append(remaining, e)
		}
	}
	p.entries = remaining
	delete(p.states, id)
	_ = p.save()
}

// ---------------------------------------------------------------------------
// GitHub REST helpers
// ---------------------------------------------------------------------------

type ghComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	UserLogin string // populated after parsing
}

// fetchComments fetches issue comments since entry.CreatedAt and returns them.
// Filtering by ID > LastCommentIDSeen is done by the caller.
func (p *GitHubPoller) fetchComments(ctx context.Context, e *PollEntry) ([]ghComment, error) {
	since := e.CreatedAt.UTC().Format(time.RFC3339)
	url := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/issues/%d/comments?since=%s&per_page=100",
		e.Owner, e.Repo, e.IssueNumber, since,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+p.pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate limited by GitHub (status %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var raw []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse comments: %w", err)
	}

	out := make([]ghComment, 0, len(raw))
	for _, r := range raw {
		out = append(out, ghComment{
			ID:        r.ID,
			Body:      r.Body,
			UserLogin: r.User.Login,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (p *GitHubPoller) load() error {
	data, err := os.ReadFile(p.storePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var entries []PollEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	p.entries = entries
	for _, e := range entries {
		if _, ok := p.states[e.ID]; !ok {
			p.states[e.ID] = &pollEntryState{}
		}
	}
	return nil
}

// save writes the current entries to disk atomically. Caller must hold p.mu.
func (p *GitHubPoller) save() error {
	if err := os.MkdirAll(filepath.Dir(p.storePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.storePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.storePath)
}

// ---------------------------------------------------------------------------
// UUID v4 helper (duplicated from pkg/tools; avoids cross-package coupling)
// ---------------------------------------------------------------------------

func newPollID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
