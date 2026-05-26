package agent

import (
	"strings"
	"testing"
)

func TestCheckTimezone(t *testing.T) {
	tests := []struct {
		name     string
		actual   string
		expected string
		wantOK   bool
		wantSub  string // substring that must appear in message (empty = no check)
	}{
		{
			name:     "check disabled when expected empty",
			actual:   "Europe/Rome",
			expected: "",
			wantOK:   true,
			wantSub:  "",
		},
		{
			name:     "timezone matches",
			actual:   "Europe/Rome",
			expected: "Europe/Rome",
			wantOK:   true,
			wantSub:  "timezone OK",
		},
		{
			name:     "UTC fallback when non-UTC expected",
			actual:   "UTC",
			expected: "Europe/Rome",
			wantOK:   false,
			wantSub:  "timedatectl",
		},
		{
			name:     "wrong non-UTC timezone",
			actual:   "America/New_York",
			expected: "Europe/Rome",
			wantOK:   false,
			wantSub:  "Reminders",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, msg := checkTimezoneStrings(tt.actual, tt.expected)
			if ok != tt.wantOK {
				t.Fatalf("checkTimezoneStrings(%q, %q): ok=%v, want %v (msg=%q)",
					tt.actual, tt.expected, ok, tt.wantOK, msg)
			}
			if tt.wantSub == "" && msg != "" && tt.expected == "" {
				// check disabled: message should be empty
				if msg != "" {
					t.Fatalf("expected empty message when check disabled, got %q", msg)
				}
			}
			if tt.wantSub != "" && !strings.Contains(msg, tt.wantSub) {
				t.Fatalf("checkTimezoneStrings(%q, %q): message %q does not contain %q",
					tt.actual, tt.expected, msg, tt.wantSub)
			}
		})
	}
}
