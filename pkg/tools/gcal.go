// PicoClaw - Ultra-lightweight personal AI agent

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	googlecal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// GCalEvent is a brief summary of a calendar event.
type GCalEvent struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Attendees   []string  `json:"attendees,omitempty"`
	Location    string    `json:"location,omitempty"`
	Description string    `json:"description,omitempty"`
}

// GCalNewEvent holds the fields required to create a calendar event.
type GCalNewEvent struct {
	Title       string
	Start       time.Time
	End         time.Time
	Attendees   []string
	Description string
	Location    string
}

// GCalClient is the interface used by the toolset. The default implementation
// calls the Calendar API; tests inject a fake.
type GCalClient interface {
	Today(ctx context.Context, calendarID string) ([]GCalEvent, error)
	Week(ctx context.Context, calendarID string) ([]GCalEvent, error)
	CreateEvent(ctx context.Context, calendarID string, ev GCalNewEvent) (GCalEvent, error)
}

// GCalToolset holds the three calendar tools that share a GCalClient.
type GCalToolset struct {
	client     GCalClient
	calendarID string
	proposals  *GCalProposalStore
}

// NewGCalToolset constructs a GCalToolset.
func NewGCalToolset(client GCalClient, calendarID, workspaceDir string) (*GCalToolset, error) {
	if client == nil {
		return nil, fmt.Errorf("gcal: client must not be nil")
	}
	if calendarID == "" {
		calendarID = "primary"
	}
	ps := NewGCalProposalStore(workspaceDir, calendarID, client)
	return &GCalToolset{
		client:     client,
		calendarID: calendarID,
		proposals:  ps,
	}, nil
}

// Tools returns the three calendar tool implementations.
func (ts *GCalToolset) Tools() []Tool {
	return []Tool{
		&gcalTodayTool{ts: ts},
		&gcalWeekTool{ts: ts},
		&gcalCreateEventProposalTool{ts: ts},
	}
}

// Proposals returns the proposal store for /apply and /reject dispatch.
func (ts *GCalToolset) Proposals() *GCalProposalStore {
	return ts.proposals
}

// TodayDirect fetches today's events without going through the tool wrapper.
// Used by the /agenda command so it bypasses turn-lock entirely.
func (ts *GCalToolset) TodayDirect(ctx context.Context) ([]GCalEvent, error) {
	return ts.client.Today(ctx, ts.calendarID)
}

// ---------------------------------------------------------------------------
// gcal_today
// ---------------------------------------------------------------------------

type gcalTodayTool struct{ ts *GCalToolset }

func (t *gcalTodayTool) Name() string { return "gcal_today" }
func (t *gcalTodayTool) Description() string {
	return "Return today's calendar events as a JSON array."
}
func (t *gcalTodayTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *gcalTodayTool) Execute(ctx context.Context, _ map[string]any) *ToolResult {
	events, err := t.ts.client.Today(ctx, t.ts.calendarID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("gcal_today: %s", err.Error()))
	}
	if events == nil {
		events = []GCalEvent{}
	}
	data, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize events: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// gcal_week
// ---------------------------------------------------------------------------

type gcalWeekTool struct{ ts *GCalToolset }

func (t *gcalWeekTool) Name() string { return "gcal_week" }
func (t *gcalWeekTool) Description() string {
	return "Return calendar events for the next 7 days as a JSON array."
}
func (t *gcalWeekTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *gcalWeekTool) Execute(ctx context.Context, _ map[string]any) *ToolResult {
	events, err := t.ts.client.Week(ctx, t.ts.calendarID)
	if err != nil {
		return ErrorResult(fmt.Sprintf("gcal_week: %s", err.Error()))
	}
	if events == nil {
		events = []GCalEvent{}
	}
	data, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return ErrorResult("failed to serialize events: " + err.Error())
	}
	return NewToolResult(string(data))
}

// ---------------------------------------------------------------------------
// gcal_create_event_proposal
// ---------------------------------------------------------------------------

type gcalCreateEventProposalTool struct{ ts *GCalToolset }

func (t *gcalCreateEventProposalTool) Name() string { return "gcal_create_event_proposal" }
func (t *gcalCreateEventProposalTool) Description() string {
	return "Propose creating a calendar event. The event is not created until the user runs /apply <proposal-id>."
}
func (t *gcalCreateEventProposalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "Event title (required).",
			},
			"start": map[string]any{
				"type":        "string",
				"description": "Event start time in RFC3339 format (required).",
			},
			"end": map[string]any{
				"type":        "string",
				"description": "Event end time in RFC3339 format (required).",
			},
			"attendees": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional list of attendee email addresses.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Optional event description.",
			},
			"location": map[string]any{
				"type":        "string",
				"description": "Optional event location.",
			},
		},
		"required": []string{"title", "start", "end"},
	}
}

func (t *gcalCreateEventProposalTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	title, _ := args["title"].(string)
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrorResult("title is required")
	}

	startStr, _ := args["start"].(string)
	startStr = strings.TrimSpace(startStr)
	if startStr == "" {
		return ErrorResult("start is required")
	}
	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return ErrorResult(fmt.Sprintf("invalid start value %q: must be RFC3339", startStr))
	}

	endStr, _ := args["end"].(string)
	endStr = strings.TrimSpace(endStr)
	if endStr == "" {
		return ErrorResult("end is required")
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		return ErrorResult(fmt.Sprintf("invalid end value %q: must be RFC3339", endStr))
	}

	if !start.Before(end) {
		return ErrorResult("start must be before end")
	}
	if start.Before(time.Now().Add(-1 * time.Minute)) {
		return ErrorResult("start is in the past")
	}

	var attendees []string
	if raw, ok := args["attendees"]; ok && raw != nil {
		switch v := raw.(type) {
		case []any:
			for _, a := range v {
				if s, ok := a.(string); ok && strings.TrimSpace(s) != "" {
					attendees = append(attendees, strings.TrimSpace(s))
				}
			}
		case []string:
			attendees = v
		}
	}
	// Reject malformed emails before storing the proposal. Without this, a
	// bad address makes Calendar's Events.Insert fail at /apply time, by which
	// point the proposal has already been removed for replay-prevention —
	// the user would be left with no recovery path beyond re-proposing from
	// scratch with no error context.
	for _, addr := range attendees {
		if !looksLikeEmail(addr) {
			return ErrorResult(fmt.Sprintf("attendee %q is not a valid email address", addr))
		}
	}

	description, _ := args["description"].(string)
	location, _ := args["location"].(string)

	p := GCalProposal{
		Title:       title,
		Start:       start.Format(time.RFC3339),
		End:         end.Format(time.RFC3339),
		Attendees:   attendees,
		Description: description,
		Location:    location,
		Reason:      fmt.Sprintf("Create event: %s", title),
	}

	stored, err := t.ts.proposals.Propose(p)
	if err != nil {
		return ErrorResult("failed to store proposal: " + err.Error())
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Event proposal ready.\n")
	fmt.Fprintf(&sb, "Title: %s\n", stored.Title)
	fmt.Fprintf(&sb, "Start: %s\n", stored.Start)
	fmt.Fprintf(&sb, "End: %s\n", stored.End)
	if len(stored.Attendees) > 0 {
		fmt.Fprintf(&sb, "Attendees: %s\n", strings.Join(stored.Attendees, ", "))
	}
	if stored.Location != "" {
		fmt.Fprintf(&sb, "Location: %s\n", stored.Location)
	}
	fmt.Fprintf(&sb, "Proposal ID: %s\n", stored.ID)
	fmt.Fprintf(&sb, "Run /apply %s to confirm (expires in 15 min), or /reject %s to cancel.", stored.ID, stored.ID)

	return &ToolResult{
		ForLLM:  fmt.Sprintf("Event proposal queued (ID: %s). Awaiting user approval via Telegram.", stored.ID),
		ForUser: sb.String(),
	}
}

// ---------------------------------------------------------------------------
// gcalAPIClient — production GCalClient backed by the Calendar REST API.
// ---------------------------------------------------------------------------

type gcalAPIClient struct {
	mu      sync.Mutex
	cfg     *oauth2.Config
	refresh string
	svc     *googlecal.Service
}

// NewGCalAPIClient creates a production Calendar API client. The service is
// initialised lazily on first use. clientID and clientSecret are the same OAuth
// client used for Gmail; refreshToken is the calendar-specific refresh token
// (GCAL_REFRESH_TOKEN).
func NewGCalAPIClient(_ context.Context, clientID, clientSecret, refreshToken string) (*gcalAPIClient, error) {
	if clientID == "" {
		return nil, fmt.Errorf("gcal: GMAIL_OAUTH_CLIENT_ID must not be empty")
	}
	if clientSecret == "" {
		return nil, fmt.Errorf("gcal: GMAIL_OAUTH_CLIENT_SECRET must not be empty")
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("gcal: GCAL_REFRESH_TOKEN must not be empty")
	}
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{googlecal.CalendarScope},
		Endpoint:     google.Endpoint,
	}
	return &gcalAPIClient{cfg: cfg, refresh: refreshToken}, nil
}

// service returns (or lazily creates) the calendar.Service.
func (c *gcalAPIClient) service() (*googlecal.Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.svc != nil {
		return c.svc, nil
	}
	tok := &oauth2.Token{
		RefreshToken: c.refresh,
		TokenType:    "Bearer",
	}
	// Bind to context.Background so the cached service survives the first
	// request's context being cancelled.
	bgCtx := context.Background()
	ts := c.cfg.TokenSource(bgCtx, tok)
	svc, err := googlecal.NewService(bgCtx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("gcal: failed to create service: %w", err)
	}
	c.svc = svc
	return svc, nil
}

// Today implements GCalClient — returns events from today 00:00 to tomorrow 00:00 local time.
func (c *gcalAPIClient) Today(ctx context.Context, calendarID string) ([]GCalEvent, error) {
	now := time.Now()
	y, m, d := now.Date()
	loc := now.Location()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	return c.listEvents(ctx, calendarID, start, end)
}

// Week implements GCalClient — returns events from now to now+7d.
func (c *gcalAPIClient) Week(ctx context.Context, calendarID string) ([]GCalEvent, error) {
	now := time.Now()
	return c.listEvents(ctx, calendarID, now, now.AddDate(0, 0, 7))
}

func (c *gcalAPIClient) listEvents(ctx context.Context, calendarID string, start, end time.Time) ([]GCalEvent, error) {
	svc, err := c.service()
	if err != nil {
		return nil, err
	}
	// Page through results — default Calendar page size is 250. A busy week
	// can easily exceed that with recurring events expanded by SingleEvents.
	// Bound the total loop to defend against runaway responses.
	var events []GCalEvent
	pageToken := ""
	for pages := 0; pages < 20; pages++ {
		call := svc.Events.List(calendarID).
			TimeMin(start.Format(time.RFC3339)).
			TimeMax(end.Format(time.RFC3339)).
			SingleEvents(true).
			OrderBy("startTime").
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("gcal list: %w", err)
		}
		for _, item := range resp.Items {
			if ev, ok := convertEvent(item); ok {
				events = append(events, ev)
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return events, nil
}

// CreateEvent implements GCalClient.
func (c *gcalAPIClient) CreateEvent(ctx context.Context, calendarID string, ev GCalNewEvent) (GCalEvent, error) {
	svc, err := c.service()
	if err != nil {
		return GCalEvent{}, err
	}

	item := &googlecal.Event{
		Summary:     ev.Title,
		Description: ev.Description,
		Location:    ev.Location,
		Start: &googlecal.EventDateTime{
			DateTime: ev.Start.Format(time.RFC3339),
		},
		End: &googlecal.EventDateTime{
			DateTime: ev.End.Format(time.RFC3339),
		},
	}
	for _, addr := range ev.Attendees {
		item.Attendees = append(item.Attendees, &googlecal.EventAttendee{Email: addr})
	}

	created, err := svc.Events.Insert(calendarID, item).Context(ctx).Do()
	if err != nil {
		return GCalEvent{}, fmt.Errorf("gcal create: %w", err)
	}
	result, ok := convertEvent(created)
	if !ok {
		return GCalEvent{}, fmt.Errorf("gcal: could not parse created event")
	}
	return result, nil
}

// convertEvent converts a *googlecal.Event to a GCalEvent.
// Returns false if the event has no parseable start time.
func convertEvent(item *googlecal.Event) (GCalEvent, bool) {
	if item == nil || item.Start == nil {
		return GCalEvent{}, false
	}
	var start, end time.Time
	var err error
	if item.Start.DateTime != "" {
		start, err = time.Parse(time.RFC3339, item.Start.DateTime)
		if err != nil {
			return GCalEvent{}, false
		}
	} else if item.Start.Date != "" {
		start, err = time.Parse("2006-01-02", item.Start.Date)
		if err != nil {
			return GCalEvent{}, false
		}
	} else {
		return GCalEvent{}, false
	}
	if item.End != nil {
		if item.End.DateTime != "" {
			end, _ = time.Parse(time.RFC3339, item.End.DateTime)
		} else if item.End.Date != "" {
			end, _ = time.Parse("2006-01-02", item.End.Date)
		}
	}

	var attendees []string
	for _, a := range item.Attendees {
		if a.Email != "" {
			attendees = append(attendees, a.Email)
		}
	}

	return GCalEvent{
		ID:          item.Id,
		Title:       item.Summary,
		Start:       start,
		End:         end,
		Attendees:   attendees,
		Location:    item.Location,
		Description: item.Description,
	}, true
}

// looksLikeEmail does a cheap structural check: exactly one '@', non-empty
// local and domain parts, and the domain contains a '.'. Not RFC 5322 — just
// enough to reject the kind of bare-name mistake an LLM is most likely to
// make (e.g. "marco" instead of "marco@example.com").
func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	if at <= 0 || at != strings.LastIndex(s, "@") || at == len(s)-1 {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}
