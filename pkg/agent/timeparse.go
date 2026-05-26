package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ErrNoRegexMatch is returned by ParseRelative when no pattern matches.
var ErrNoRegexMatch = errors.New("no regex match")

// maxFutureDays is the furthest future time Parse will accept.
const maxFutureDays = 90

// TimeParser parses human-readable time expressions into absolute UTC times.
// It tries regex patterns first; if none match and an LLM provider is
// configured, it falls back to a model call.
type TimeParser struct {
	provider providers.LLMProvider
	model    string
}

// NewTimeParser returns a TimeParser. provider may be nil (disables LLM fallback).
func NewTimeParser(provider providers.LLMProvider, model string) *TimeParser {
	return &TimeParser{provider: provider, model: model}
}

// ---------------------------------------------------------------------------
// Pre-compiled patterns (compiled at init time)
// ---------------------------------------------------------------------------

var (
	// bare duration: "2m", "30s", "1h30m"
	reDuration = regexp.MustCompile(`(?i)^\s*(\d+h)?(\d+m)?(\d+s)?\s*$`)

	// plain seconds
	reSec = regexp.MustCompile(`(?i)^\s*(\d+)\s*s(?:ec(?:onds?)?)?\s*$`)

	// plain minutes
	reMin = regexp.MustCompile(`(?i)^\s*(\d+)\s*min(?:uti?|utes?)?\s*$`)

	// plain hours
	reHour = regexp.MustCompile(`(?i)^\s*(\d+)\s*h(?:r|ours?)?\s*$`)

	// plain days
	reDay = regexp.MustCompile(`(?i)^\s*(\d+)\s*(?:d(?:ays?)?|giorni?)\s*$`)

	// "in X minutes / hours / days" — English and Italian
	reInMinutes = regexp.MustCompile(`(?i)^\s*(?:in|tra)\s+(\d+)\s*min(?:uti?|utes?)?\s*$`)
	reInHours   = regexp.MustCompile(`(?i)^\s*(?:in|tra)\s+(\d+)\s*h(?:r|ours?|ore)?\s*$`)
	reInDays    = regexp.MustCompile(`(?i)^\s*(?:in|tra)\s+(\d+)\s*(?:d(?:ays?)?|giorni?)\s*$`)

	// "HH:MM" alone
	reHHMM = regexp.MustCompile(`(?i)^\s*(\d{1,2}):(\d{2})\s*$`)

	// "today HH:MM" / "oggi HH:MM"
	reTodayHHMM = regexp.MustCompile(`(?i)^\s*(?:today|oggi)\s+(\d{1,2}):(\d{2})\s*$`)

	// "tomorrow HH:MM" / "domani HH:MM"
	reTomorrowHHMM = regexp.MustCompile(`(?i)^\s*(?:tomorrow|domani)\s+(\d{1,2}):(\d{2})\s*$`)

	// Go's ParseDuration-compatible: "2h30m", "90m", "45s"
	reGoDuration = regexp.MustCompile(`(?i)^\s*(\d+h\d+m|\d+h|\d+m|\d+s)\s*$`)
)

// ParseRelative resolves common relative time expressions without an LLM.
// All returned times are UTC.
func (p *TimeParser) ParseRelative(now time.Time, when string) (time.Time, error) {
	loc := now.Location()
	nowLocal := now.In(loc)

	// Helper to build a local HH:MM time on a given base day, then convert UTC.
	localHHMM := func(base time.Time, h, m int) time.Time {
		t := time.Date(base.Year(), base.Month(), base.Day(), h, m, 0, 0, loc)
		return t.UTC()
	}

	// Go-compatible duration strings: "2h30m", "1h", "45m", "10s"
	if m := reGoDuration.FindStringSubmatch(when); m != nil {
		d, err := time.ParseDuration(strings.ToLower(m[1]))
		if err == nil {
			return now.Add(d), nil
		}
	}

	// Plain seconds: "30s", "30 sec"
	if m := reSec.FindStringSubmatch(when); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.Add(time.Duration(n) * time.Second), nil
	}

	// Plain minutes: "5m", "5 min", "5 minuti"
	if m := reMin.FindStringSubmatch(when); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.Add(time.Duration(n) * time.Minute), nil
	}

	// Plain hours: "2h", "2 hours"
	if m := reHour.FindStringSubmatch(when); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.Add(time.Duration(n) * time.Hour), nil
	}

	// Plain days: "3d", "3 days", "3 giorni"
	if m := reDay.FindStringSubmatch(when); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.Add(time.Duration(n) * 24 * time.Hour), nil
	}

	// "in X minutes" / "tra X minuti"
	if m := reInMinutes.FindStringSubmatch(when); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.Add(time.Duration(n) * time.Minute), nil
	}

	// "in X hours" / "tra X ore"
	if m := reInHours.FindStringSubmatch(when); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.Add(time.Duration(n) * time.Hour), nil
	}

	// "in X days" / "tra X giorni"
	if m := reInDays.FindStringSubmatch(when); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.Add(time.Duration(n) * 24 * time.Hour), nil
	}

	// "tomorrow HH:MM" / "domani HH:MM"
	if m := reTomorrowHHMM.FindStringSubmatch(when); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h > 23 || min > 59 {
			return time.Time{}, fmt.Errorf("timeparse: invalid time %02d:%02d", h, min)
		}
		tomorrow := nowLocal.AddDate(0, 0, 1)
		return localHHMM(tomorrow, h, min), nil
	}

	// "today HH:MM" / "oggi HH:MM"
	if m := reTodayHHMM.FindStringSubmatch(when); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h > 23 || min > 59 {
			return time.Time{}, fmt.Errorf("timeparse: invalid time %02d:%02d", h, min)
		}
		return localHHMM(nowLocal, h, min), nil
	}

	// Bare "HH:MM" — today if still in the future, tomorrow otherwise.
	if m := reHHMM.FindStringSubmatch(when); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h > 23 || min > 59 {
			return time.Time{}, fmt.Errorf("timeparse: invalid time %02d:%02d", h, min)
		}
		candidate := localHHMM(nowLocal, h, min)
		if candidate.Before(now) {
			candidate = candidate.Add(24 * time.Hour)
		}
		return candidate, nil
	}

	return time.Time{}, ErrNoRegexMatch
}

// ParseWithLLM asks the configured model to parse `when` relative to `now`.
// Returns an error if no provider or no model is configured — an empty model
// name disables the LLM fallback explicitly (matches the deploy/config.yaml
// documentation that `parse_model: ""` disables LLM time-parsing).
func (p *TimeParser) ParseWithLLM(ctx context.Context, now time.Time, when string) (time.Time, error) {
	if p.provider == nil {
		return time.Time{}, fmt.Errorf("timeparse: no LLM provider configured")
	}
	if p.model == "" {
		return time.Time{}, fmt.Errorf("timeparse: LLM fallback disabled (parse_model is empty)")
	}

	prompt := fmt.Sprintf(
		"Now: %s\nParse this relative or absolute time expression: %q\n\nRespond with ONE LINE containing either:\n- An ISO8601 timestamp in UTC (e.g., 2026-05-27T14:32:00Z)\n- The literal text ERROR if you cannot parse it.\n\nDo not include explanation, code fences, or any other text.",
		now.UTC().Format(time.RFC3339),
		when,
	)

	model := p.model

	resp, err := p.provider.Chat(ctx,
		[]providers.Message{{Role: "user", Content: prompt}},
		nil,
		model,
		nil,
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("timeparse: LLM call failed: %w", err)
	}

	raw := strings.TrimSpace(resp.Content)
	if strings.EqualFold(raw, "ERROR") || raw == "" {
		return time.Time{}, fmt.Errorf("timeparse: model could not parse %q", when)
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// try without seconds
		t, err = time.Parse("2006-01-02T15:04Z", raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("timeparse: model returned unparseable value %q: %w", raw, err)
		}
	}

	return p.validate(now, t)
}

// Parse tries ParseRelative; on ErrNoRegexMatch it falls back to ParseWithLLM.
func (p *TimeParser) Parse(ctx context.Context, now time.Time, when string) (time.Time, error) {
	t, err := p.ParseRelative(now, when)
	if err == nil {
		return p.validate(now, t)
	}
	if !errors.Is(err, ErrNoRegexMatch) {
		return time.Time{}, err
	}
	return p.ParseWithLLM(ctx, now, when)
}

// validate rejects times that are in the past or more than 90 days away.
func (p *TimeParser) validate(now, t time.Time) (time.Time, error) {
	if t.Before(now.Add(-time.Minute)) {
		return time.Time{}, fmt.Errorf("timeparse: resolved time %s is in the past", t.Format(time.RFC3339))
	}
	if t.After(now.Add(maxFutureDays * 24 * time.Hour)) {
		return time.Time{}, fmt.Errorf("timeparse: resolved time %s is more than %d days in the future", t.Format(time.RFC3339), maxFutureDays)
	}
	return t, nil
}
