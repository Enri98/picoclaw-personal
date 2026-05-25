package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	googleapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// htmlTagRe matches any HTML tag. Compiled once at init.
var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

// GmailMessage is a brief summary of a Gmail message.
type GmailMessage struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	Subject  string    `json:"subject"`
	Snippet  string    `json:"snippet"`
	Received time.Time `json:"received"`
}

// GmailBody is the full body of a Gmail message.
type GmailBody struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	Text    string    `json:"text"`
}

// GmailAccount holds the configuration for a single Gmail account.
type GmailAccount struct {
	Name            string
	RefreshTokenEnv string
}

// GmailClient is the interface used by the toolset. The default implementation
// calls the Gmail API; tests inject a fake.
type GmailClient interface {
	ListUnread(ctx context.Context, account string, since time.Time, maxResults int) ([]GmailMessage, error)
	GetBody(ctx context.Context, account string, id string) (GmailBody, error)
}

// GmailToolset holds the two Gmail tools that share a GmailClient.
type GmailToolset struct {
	client   GmailClient
	accounts map[string]GmailAccount
}

// NewGmailToolset constructs a GmailToolset. accounts must be non-empty.
func NewGmailToolset(accounts []GmailAccount, client GmailClient) (*GmailToolset, error) {
	if len(accounts) == 0 {
		return nil, fmt.Errorf("gmail: at least one account is required")
	}
	if client == nil {
		return nil, fmt.Errorf("gmail: client must not be nil")
	}
	m := make(map[string]GmailAccount, len(accounts))
	for _, a := range accounts {
		m[a.Name] = a
	}
	return &GmailToolset{client: client, accounts: m}, nil
}

// Tools returns the two Gmail tool implementations.
func (ts *GmailToolset) Tools() []Tool {
	return []Tool{
		&gmailListUnreadTool{ts: ts},
		&gmailGetBodyTool{ts: ts},
	}
}

// accountNames returns the sorted list of known account names for use in
// error messages and parameter enums.
func (ts *GmailToolset) accountNames() []string {
	names := make([]string, 0, len(ts.accounts))
	for k := range ts.accounts {
		names = append(names, k)
	}
	return names
}

// validateAccount checks that account is non-empty and known.
func (ts *GmailToolset) validateAccount(account string) error {
	if account == "" {
		return fmt.Errorf("account is required; valid values: %s", strings.Join(ts.accountNames(), ", "))
	}
	if _, ok := ts.accounts[account]; !ok {
		return fmt.Errorf("unknown account %q; valid values: %s", account, strings.Join(ts.accountNames(), ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// gmail_list_unread
// ---------------------------------------------------------------------------

type gmailListUnreadTool struct{ ts *GmailToolset }

func (t *gmailListUnreadTool) Name() string { return "gmail_list_unread" }
func (t *gmailListUnreadTool) Description() string {
	return "List unread Gmail messages for the given account. Returns a JSON array of messages with id, from, subject, snippet, and received time."
}
func (t *gmailListUnreadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"account": map[string]any{
				"type":        "string",
				"description": "Gmail account to query. Valid values: darra2, chiunque.",
			},
			"since": map[string]any{
				"type":        "string",
				"description": "RFC3339 timestamp; only messages received after this time are returned. Defaults to 24 hours ago.",
			},
			"max": map[string]any{
				"type":        "integer",
				"description": "Maximum number of messages to return (1–50). Defaults to 20.",
			},
		},
		"required": []string{"account"},
	}
}

const (
	gmailListDefaultMax = 20
	gmailListCapMax     = 50
)

func (t *gmailListUnreadTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	account, _ := args["account"].(string)
	account = strings.TrimSpace(account)
	if err := t.ts.validateAccount(account); err != nil {
		return ErrorResult(err.Error())
	}

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
	max := gmailListDefaultMax
	switch v := args["max"].(type) {
	case float64:
		max = int(v)
	case int:
		max = v
	case int64:
		max = int(v)
	}
	if max <= 0 {
		max = gmailListDefaultMax
	}
	if max > gmailListCapMax {
		max = gmailListCapMax
	}

	msgs, err := t.ts.client.ListUnread(ctx, account, since, max)
	if err != nil {
		return ErrorResult(fmt.Sprintf("gmail_list_unread: %s", err.Error()))
	}
	if msgs == nil {
		msgs = []GmailMessage{}
	}

	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize messages: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// gmail_get_body
// ---------------------------------------------------------------------------

type gmailGetBodyTool struct{ ts *GmailToolset }

func (t *gmailGetBodyTool) Name() string { return "gmail_get_body" }
func (t *gmailGetBodyTool) Description() string {
	return "Fetch the full body of a Gmail message by ID. Returns plaintext content with headers."
}
func (t *gmailGetBodyTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"account": map[string]any{
				"type":        "string",
				"description": "Gmail account to query. Valid values: darra2, chiunque.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Gmail message ID.",
			},
		},
		"required": []string{"account", "id"},
	}
}

func (t *gmailGetBodyTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	account, _ := args["account"].(string)
	account = strings.TrimSpace(account)
	if err := t.ts.validateAccount(account); err != nil {
		return ErrorResult(err.Error())
	}

	id, _ := args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrorResult("id is required")
	}

	body, err := t.ts.client.GetBody(ctx, account, id)
	if err != nil {
		return ErrorResult(fmt.Sprintf("gmail_get_body: %s", err.Error()))
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
// htmlToText — strip HTML tags, normalise line breaks.
// ---------------------------------------------------------------------------

func htmlToText(s string) string {
	// Replace block-level closing tags with newlines before stripping tags.
	s = strings.ReplaceAll(s, "</p>", "\n")
	s = strings.ReplaceAll(s, "</P>", "\n")
	s = strings.ReplaceAll(s, "</div>", "\n")
	s = strings.ReplaceAll(s, "</DIV>", "\n")

	// Replace <br> variants with newlines.
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<BR />", "\n")
	s = strings.ReplaceAll(s, "<BR/>", "\n")
	s = strings.ReplaceAll(s, "<BR>", "\n")

	// Strip remaining tags.
	s = htmlTagRe.ReplaceAllString(s, "")

	// Collapse runs of 3+ newlines to 2.
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}

	return s
}

// ---------------------------------------------------------------------------
// gmailAPIClient — production GmailClient backed by the Gmail REST API.
// ---------------------------------------------------------------------------

type gmailAPIClient struct {
	mu       sync.Mutex
	cfg      *oauth2.Config
	accounts map[string]GmailAccount
	services map[string]*googleapi.Service
}

// NewGmailAPIClient creates a production Gmail API client. It initialises
// per-account services lazily on first use.
func NewGmailAPIClient(
	_ context.Context,
	clientID, clientSecret string,
	accounts map[string]GmailAccount,
) (*gmailAPIClient, error) {
	if clientID == "" {
		return nil, fmt.Errorf("gmail: GMAIL_OAUTH_CLIENT_ID must not be empty")
	}
	if clientSecret == "" {
		return nil, fmt.Errorf("gmail: GMAIL_OAUTH_CLIENT_SECRET must not be empty")
	}
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{googleapi.GmailReadonlyScope},
		Endpoint:     google.Endpoint,
	}
	return &gmailAPIClient{
		cfg:      cfg,
		accounts: accounts,
		services: make(map[string]*googleapi.Service),
	}, nil
}

// serviceFor returns (or lazily creates) the gmail.Service for the given account.
func (c *gmailAPIClient) serviceFor(ctx context.Context, accountName string) (*googleapi.Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if svc, ok := c.services[accountName]; ok {
		return svc, nil
	}

	acc, ok := c.accounts[accountName]
	if !ok {
		return nil, fmt.Errorf("gmail: unknown account %q", accountName)
	}

	rt := os.Getenv(acc.RefreshTokenEnv)
	if rt == "" {
		return nil, fmt.Errorf("gmail: refresh token env var %s is empty for account %q", acc.RefreshTokenEnv, accountName)
	}

	tok := &oauth2.Token{
		RefreshToken: rt,
		TokenType:    "Bearer",
	}
	// The cached service outlives the first request's context, so the
	// TokenSource and Service must be bound to context.Background to avoid
	// silent token-refresh failures if the first ctx is cancelled.
	bgCtx := context.Background()
	ts := c.cfg.TokenSource(bgCtx, tok)

	svc, err := googleapi.NewService(bgCtx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("gmail: failed to create service for account %q: %w", accountName, err)
	}

	c.services[accountName] = svc
	return svc, nil
}

// ListUnread implements GmailClient.
func (c *gmailAPIClient) ListUnread(ctx context.Context, account string, since time.Time, maxResults int) ([]GmailMessage, error) {
	svc, err := c.serviceFor(ctx, account)
	if err != nil {
		return nil, err
	}

	// Gmail search query: unread after `since`.
	sinceUnix := since.Unix()
	query := fmt.Sprintf("is:unread after:%d", sinceUnix)

	resp, err := svc.Users.Messages.List("me").
		Q(query).
		MaxResults(int64(maxResults)).
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("gmail list: %w", err)
	}

	var msgs []GmailMessage
	for _, m := range resp.Messages {
		msg, err := svc.Users.Messages.Get("me", m.Id).
			Format("metadata").
			MetadataHeaders("From", "Subject", "Date").
			Context(ctx).
			Do()
		if err != nil {
			continue
		}

		var from, subject string
		var received time.Time
		for _, h := range msg.Payload.Headers {
			switch strings.ToLower(h.Name) {
			case "from":
				from = h.Value
			case "subject":
				subject = h.Value
			case "date":
				if t, err := parseEmailDate(h.Value); err == nil {
					received = t
				}
			}
		}

		msgs = append(msgs, GmailMessage{
			ID:       msg.Id,
			From:     from,
			Subject:  subject,
			Snippet:  msg.Snippet,
			Received: received,
		})
	}
	return msgs, nil
}

// GetBody implements GmailClient.
func (c *gmailAPIClient) GetBody(ctx context.Context, account string, id string) (GmailBody, error) {
	svc, err := c.serviceFor(ctx, account)
	if err != nil {
		return GmailBody{}, err
	}

	msg, err := svc.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
	if err != nil {
		return GmailBody{}, fmt.Errorf("gmail get: %w", err)
	}

	var from, to, subject string
	var date time.Time
	for _, h := range msg.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			from = h.Value
		case "to":
			to = h.Value
		case "subject":
			subject = h.Value
		case "date":
			if t, err := parseEmailDate(h.Value); err == nil {
				date = t
			}
		}
	}

	text := extractText(msg.Payload)

	return GmailBody{
		From:    from,
		To:      to,
		Subject: subject,
		Date:    date,
		Text:    text,
	}, nil
}

// extractText walks a message payload tree to find the best plaintext content.
func extractText(part *googleapi.MessagePart) string {
	if part == nil {
		return ""
	}
	// Try text/plain first across the whole tree.
	if t := findPartByMIME(part, "text/plain"); t != "" {
		return t
	}
	// Fall back to text/html, stripping tags.
	if t := findPartByMIME(part, "text/html"); t != "" {
		return htmlToText(t)
	}
	return ""
}

// findPartByMIME does a depth-first search for the first part matching mimeType.
func findPartByMIME(part *googleapi.MessagePart, mimeType string) string {
	if part == nil {
		return ""
	}
	if strings.EqualFold(part.MimeType, mimeType) && part.Body != nil && part.Body.Data != "" {
		decoded, err := base64.URLEncoding.DecodeString(part.Body.Data)
		if err != nil {
			// Try standard encoding as fallback.
			decoded, err = base64.StdEncoding.DecodeString(part.Body.Data)
			if err != nil {
				return ""
			}
		}
		return string(decoded)
	}
	for _, child := range part.Parts {
		if t := findPartByMIME(child, mimeType); t != "" {
			return t
		}
	}
	return ""
}

// parseEmailDate parses common email date header formats.
var emailDateFormats = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 MST",
}

func parseEmailDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, fmt := range emailDateFormats {
		if t, err := time.Parse(fmt, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse date %q", s)
}
