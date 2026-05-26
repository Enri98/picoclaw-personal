package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// OutlookMessage is a brief summary of an Outlook message.
type OutlookMessage struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	Subject  string    `json:"subject"`
	Preview  string    `json:"preview"`
	Received time.Time `json:"received"`
}

// OutlookBody is the full content of an Outlook message.
type OutlookBody struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	Text    string    `json:"text"`
}

// OutlookClient is the interface the LLM-facing tools use. Tests inject a fake.
type OutlookClient interface {
	ListUnread(ctx context.Context, since time.Time, maxResults int) ([]OutlookMessage, error)
	GetBody(ctx context.Context, id string) (OutlookBody, error)
}

// OutlookToolset holds the two Outlook tools that share an OutlookClient.
type OutlookToolset struct {
	client OutlookClient
}

// NewOutlookToolset constructs an OutlookToolset.
func NewOutlookToolset(client OutlookClient) (*OutlookToolset, error) {
	if client == nil {
		return nil, fmt.Errorf("outlook: client must not be nil")
	}
	return &OutlookToolset{client: client}, nil
}

// Tools returns the two Outlook tool implementations.
func (ts *OutlookToolset) Tools() []Tool {
	return []Tool{
		&outlookListUnreadTool{ts: ts},
		&outlookGetBodyTool{ts: ts},
	}
}

// ListUnreadDirect fetches unread Outlook messages since the provided time,
// bypassing the tool wrapper. Used by the briefing assembler.
func (ts *OutlookToolset) ListUnreadDirect(ctx context.Context, since time.Time, max int) ([]OutlookMessage, error) {
	return ts.client.ListUnread(ctx, since, max)
}

// ---------------------------------------------------------------------------
// outlook_list_unread
// ---------------------------------------------------------------------------

type outlookListUnreadTool struct{ ts *OutlookToolset }

func (t *outlookListUnreadTool) Name() string { return "outlook_list_unread" }
func (t *outlookListUnreadTool) Description() string {
	return "List unread Outlook messages from the inbox. Returns a JSON array of messages with id, from, subject, preview, and received time."
}
func (t *outlookListUnreadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"since": map[string]any{
				"type":        "string",
				"description": "RFC3339 timestamp; only messages received after this time are returned. Defaults to 24 hours ago.",
			},
			"max": map[string]any{
				"type":        "integer",
				"description": "Maximum number of messages to return (1–50). Defaults to 20.",
			},
		},
		"required": []string{},
	}
}

const (
	outlookListDefaultMax = 20
	outlookListCapMax     = 50
)

func (t *outlookListUnreadTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	// Parse since, default to now-24h.
	var since time.Time
	if sinceStr, ok := args["since"].(string); ok && sinceStr != "" {
		var err error
		since, err = time.Parse(time.RFC3339, strings.TrimSpace(sinceStr))
		if err != nil {
			return ErrorResult(fmt.Sprintf("invalid since value %q: must be RFC3339", sinceStr))
		}
	} else {
		since = time.Now().Add(-24 * time.Hour)
	}

	// Parse max.
	max := outlookListDefaultMax
	switch v := args["max"].(type) {
	case float64:
		max = int(v)
	case int:
		max = v
	case int64:
		max = int(v)
	}
	if max <= 0 {
		max = outlookListDefaultMax
	}
	if max > outlookListCapMax {
		max = outlookListCapMax
	}

	msgs, err := t.ts.client.ListUnread(ctx, since, max)
	if err != nil {
		return ErrorResult(fmt.Sprintf("outlook_list_unread: %s", err.Error()))
	}
	if msgs == nil {
		msgs = []OutlookMessage{}
	}

	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize messages: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// outlook_get_body
// ---------------------------------------------------------------------------

type outlookGetBodyTool struct{ ts *OutlookToolset }

func (t *outlookGetBodyTool) Name() string { return "outlook_get_body" }
func (t *outlookGetBodyTool) Description() string {
	return "Fetch the full body of an Outlook message by ID. Returns plaintext content with headers."
}
func (t *outlookGetBodyTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Outlook message ID.",
			},
		},
		"required": []string{"id"},
	}
}

func (t *outlookGetBodyTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	id, _ := args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrorResult("id is required")
	}

	body, err := t.ts.client.GetBody(ctx, id)
	if err != nil {
		return ErrorResult(fmt.Sprintf("outlook_get_body: %s", err.Error()))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "From: %s\n", body.From)
	fmt.Fprintf(&sb, "To: %s\n", body.To)
	fmt.Fprintf(&sb, "Subject: %s\n", body.Subject)
	fmt.Fprintf(&sb, "Date: %s\n", body.Date.Format(time.RFC1123Z))
	sb.WriteString("\n")
	sb.WriteString(body.Text)

	return NewToolResult(sb.String())
}

// ---------------------------------------------------------------------------
// outlookGraphClient — production OutlookClient backed by Microsoft Graph.
// ---------------------------------------------------------------------------

type outlookGraphClient struct {
	httpClient *http.Client
}

// persistingTokenSource wraps an oauth2.TokenSource and atomically writes the
// current refresh token to disk after every successful refresh. Microsoft
// rotates refresh tokens on every use; without writeback, only the in-memory
// chain has the live token and a process restart would fall back to a
// possibly-expired bootstrap token from env.
type persistingTokenSource struct {
	inner oauth2.TokenSource
	path  string
	mu    sync.Mutex
	last  string
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil {
		return nil, err
	}
	if tok != nil && tok.RefreshToken != "" {
		p.mu.Lock()
		changed := tok.RefreshToken != p.last
		if changed {
			p.last = tok.RefreshToken
		}
		p.mu.Unlock()
		if changed {
			_ = writeRefreshTokenAtomic(p.path, tok.RefreshToken)
		}
	}
	return tok, nil
}

func writeRefreshTokenAtomic(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readPersistedRefreshToken returns the stored refresh token if the sidecar
// file exists, otherwise "".
func readPersistedRefreshToken(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// NewOutlookGraphClient creates a production Outlook Graph client.
// It validates non-empty inputs and builds a token-refreshing HTTP client.
// No network call is made during construction; token refresh happens on first
// request. If persistPath is non-empty, rotated refresh tokens are written
// back there atomically so the next process start has the live token.
func NewOutlookGraphClient(_ context.Context, clientID, refreshToken, persistPath string) (*outlookGraphClient, error) {
	if clientID == "" {
		return nil, fmt.Errorf("outlook: OUTLOOK_OAUTH_CLIENT_ID must not be empty")
	}
	// Prefer a persisted (rotated) refresh token over the bootstrap env value
	// — the file holds the latest token issued by the last live refresh.
	if persistPath != "" {
		if persisted := readPersistedRefreshToken(persistPath); persisted != "" {
			refreshToken = persisted
		}
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("outlook: OUTLOOK_REFRESH_TOKEN must not be empty")
	}

	endpoint := oauth2.Endpoint{
		AuthURL:   "https://login.microsoftonline.com/consumers/oauth2/v2.0/authorize",
		TokenURL:  "https://login.microsoftonline.com/consumers/oauth2/v2.0/token",
		AuthStyle: oauth2.AuthStyleInParams,
	}
	cfg := &oauth2.Config{
		ClientID: clientID,
		Scopes:   []string{"Mail.Read", "offline_access"},
		Endpoint: endpoint,
	}

	// Bind to context.Background so the token source outlives any single
	// request context, matching the pattern used in the Gmail client.
	tok := &oauth2.Token{RefreshToken: refreshToken, TokenType: "Bearer"}
	var ts oauth2.TokenSource = cfg.TokenSource(context.Background(), tok)
	if persistPath != "" {
		ts = &persistingTokenSource{inner: ts, path: persistPath, last: refreshToken}
	}
	httpClient := oauth2.NewClient(context.Background(), ts)

	return &outlookGraphClient{httpClient: httpClient}, nil
}

// graphListResponse is used to parse the Graph API message list response.
type graphListResponse struct {
	Value []graphMessage `json:"value"`
}

type graphMessage struct {
	ID              string              `json:"id"`
	Subject         string              `json:"subject"`
	BodyPreview     string              `json:"bodyPreview"`
	ReceivedDT      string              `json:"receivedDateTime"`
	From            graphEmailWrapper   `json:"from"`
}

type graphEmailWrapper struct {
	EmailAddress graphEmailAddress `json:"emailAddress"`
}

type graphEmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// graphBodyResponse is used to parse the Graph API single message response.
type graphBodyResponse struct {
	ID           string              `json:"id"`
	Subject      string              `json:"subject"`
	ReceivedDT   string              `json:"receivedDateTime"`
	From         graphEmailWrapper   `json:"from"`
	ToRecipients []graphEmailWrapper `json:"toRecipients"`
	Body         struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
}

// ListUnread implements OutlookClient.
func (c *outlookGraphClient) ListUnread(ctx context.Context, since time.Time, maxResults int) ([]OutlookMessage, error) {
	if maxResults <= 0 {
		maxResults = outlookListDefaultMax
	}
	if maxResults > outlookListCapMax {
		maxResults = outlookListCapMax
	}

	sinceStr := since.UTC().Format(time.RFC3339)
	filter := fmt.Sprintf("isRead eq false and receivedDateTime ge %s", sinceStr)

	u, err := url.Parse("https://graph.microsoft.com/v1.0/me/mailFolders/inbox/messages")
	if err != nil {
		return nil, fmt.Errorf("outlook: failed to parse URL: %w", err)
	}
	q := url.Values{}
	q.Set("$filter", filter)
	q.Set("$top", fmt.Sprintf("%d", maxResults))
	q.Set("$select", "id,from,subject,bodyPreview,receivedDateTime")
	q.Set("$orderby", "receivedDateTime desc")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("outlook: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Request immutable IDs so a GetBody call after a folder move (user moves
	// the message on their phone between list and get) still resolves.
	req.Header.Set("Prefer", `IdType="ImmutableId"`)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("outlook: graph request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("outlook: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("outlook: graph returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var listResp graphListResponse
	if err := json.Unmarshal(raw, &listResp); err != nil {
		return nil, fmt.Errorf("outlook: parsing list response: %w", err)
	}

	msgs := make([]OutlookMessage, 0, len(listResp.Value))
	for _, m := range listResp.Value {
		received, _ := time.Parse(time.RFC3339, m.ReceivedDT)
		from := m.From.EmailAddress.Address
		if from == "" {
			from = m.From.EmailAddress.Name
		}
		msgs = append(msgs, OutlookMessage{
			ID:       m.ID,
			From:     from,
			Subject:  m.Subject,
			Preview:  m.BodyPreview,
			Received: received,
		})
	}
	return msgs, nil
}

// GetBody implements OutlookClient.
func (c *outlookGraphClient) GetBody(ctx context.Context, id string) (OutlookBody, error) {
	u := fmt.Sprintf(
		"https://graph.microsoft.com/v1.0/me/messages/%s?$select=from,toRecipients,subject,receivedDateTime,body",
		url.PathEscape(id),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return OutlookBody{}, fmt.Errorf("outlook: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", `IdType="ImmutableId"`)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return OutlookBody{}, fmt.Errorf("outlook: graph request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return OutlookBody{}, fmt.Errorf("outlook: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return OutlookBody{}, fmt.Errorf("outlook: graph returned HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var msg graphBodyResponse
	if err := json.Unmarshal(raw, &msg); err != nil {
		return OutlookBody{}, fmt.Errorf("outlook: parsing message response: %w", err)
	}

	received, _ := time.Parse(time.RFC3339, msg.ReceivedDT)

	from := msg.From.EmailAddress.Address
	if from == "" {
		from = msg.From.EmailAddress.Name
	}

	toAddrs := make([]string, 0, len(msg.ToRecipients))
	for _, r := range msg.ToRecipients {
		addr := r.EmailAddress.Address
		if addr == "" {
			addr = r.EmailAddress.Name
		}
		if addr != "" {
			toAddrs = append(toAddrs, addr)
		}
	}
	to := strings.Join(toAddrs, ", ")

	// Decode body; strip HTML if necessary.
	text := msg.Body.Content
	if strings.EqualFold(msg.Body.ContentType, "html") {
		text = htmlToText(text)
	}

	return OutlookBody{
		From:    from,
		To:      to,
		Subject: msg.Subject,
		Date:    received,
		Text:    text,
	}, nil
}
