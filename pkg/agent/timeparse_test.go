package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// panicProvider satisfies providers.LLMProvider but panics on Chat,
// so any test that accidentally invokes the LLM is immediately visible.
type panicProvider struct{}

func (p *panicProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	panic("panicProvider: Chat must not be called in this test")
}

func (p *panicProvider) GetDefaultModel() string { return "panic-model" }

// ---------------------------------------------------------------------------
// TestParseRelative_Table
// ---------------------------------------------------------------------------

func TestParseRelative_Table(t *testing.T) {
	tp := NewTimeParser(nil, "")
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.Local)

	cases := []struct {
		input    string
		wantTime time.Time
	}{
		{"5m", now.Add(5 * time.Minute)},
		{"30s", now.Add(30 * time.Second)},
		{"1h", now.Add(1 * time.Hour)},
		{"1h30m", now.Add(90 * time.Minute)},
		{"2d", now.Add(48 * time.Hour)},
		{"in 30 minutes", now.Add(30 * time.Minute)},
		{"in 2 hours", now.Add(2 * time.Hour)},
		{"tra 30 minuti", now.Add(30 * time.Minute)},
		{"tra 2 ore", now.Add(2 * time.Hour)},
		{"tra 1 ora", now.Add(1 * time.Hour)},
		// "14:32" is after 10:00, so should be today.
		{"14:32", time.Date(2026, 5, 27, 14, 32, 0, 0, time.Local).UTC()},
		// "08:00" is before 10:00, so tomorrow.
		{"08:00", time.Date(2026, 5, 28, 8, 0, 0, 0, time.Local).UTC()},
		{"tomorrow 9:00", time.Date(2026, 5, 28, 9, 0, 0, 0, time.Local).UTC()},
		{"domani 9:00", time.Date(2026, 5, 28, 9, 0, 0, 0, time.Local).UTC()},
		{"today 14:00", time.Date(2026, 5, 27, 14, 0, 0, 0, time.Local).UTC()},
		{"oggi 14:00", time.Date(2026, 5, 27, 14, 0, 0, 0, time.Local).UTC()},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := tp.ParseRelative(now, tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Compare in UTC.
			want := tc.wantTime.UTC()
			gotUTC := got.UTC()
			if !gotUTC.Equal(want) {
				t.Errorf("ParseRelative(%q) = %v, want %v", tc.input, gotUTC, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestParseRelative_Errors
// ---------------------------------------------------------------------------

func TestParseRelative_Errors(t *testing.T) {
	tp := NewTimeParser(nil, "")
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.Local)

	t.Run("empty string", func(t *testing.T) {
		_, err := tp.ParseRelative(now, "")
		if err == nil {
			t.Fatal("expected error for empty input")
		}
	})

	t.Run("garbage", func(t *testing.T) {
		_, err := tp.ParseRelative(now, "garbage")
		if !errors.Is(err, ErrNoRegexMatch) {
			t.Fatalf("expected ErrNoRegexMatch, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// TestParse_BoundsRejection — 365d triggers validate via Parse().
// Parse will match the regex then call validate which rejects >90d.
// No LLM is called because the regex matched first.
// ---------------------------------------------------------------------------

func TestParse_BoundsRejection(t *testing.T) {
	tp := NewTimeParser(&panicProvider{}, "")
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.Local)

	_, err := tp.Parse(context.Background(), now, "365d")
	if err == nil {
		t.Fatal("expected error for 365d (>90 day horizon)")
	}
	if !strings.Contains(err.Error(), "more than") && !strings.Contains(err.Error(), "future") {
		t.Errorf("expected bounds error message, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestParseWithLLM_DisabledWhenModelEmpty
// ---------------------------------------------------------------------------

func TestParseWithLLM_DisabledWhenModelEmpty(t *testing.T) {
	tp := NewTimeParser(&panicProvider{}, "")
	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.Local)

	_, err := tp.ParseWithLLM(context.Background(), now, "next Tuesday")
	if err == nil {
		t.Fatal("expected error when model is empty")
	}
	msg := err.Error()
	if !strings.Contains(msg, "disabled") && !strings.Contains(msg, "parse_model is empty") {
		t.Errorf("expected 'disabled' or 'parse_model is empty' in error, got: %v", err)
	}
}
