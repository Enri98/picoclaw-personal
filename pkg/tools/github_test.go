package tools

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// resolveRepo
// ---------------------------------------------------------------------------

func TestResolveRepo(t *testing.T) {
	watched := []string{"acme/alpha", "acme/beta", "other/alpha"}

	makeTS := func() *GitHubToolset {
		ts, err := NewGitHubToolset("token", watched, t.TempDir())
		if err != nil {
			t.Helper()
			t.Fatalf("NewGitHubToolset: %v", err)
		}
		return ts
	}

	cases := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{
			name:  "full form match exact",
			input: "acme/alpha",
			want:  "acme/alpha",
		},
		{
			name:  "full form match case-insensitive",
			input: "ACME/Alpha",
			want:  "acme/alpha",
		},
		{
			name:    "full form not in watchlist",
			input:   "acme/gamma",
			wantErr: "not in the watched list",
		},
		{
			name:  "short form unique match",
			input: "beta",
			want:  "acme/beta",
		},
		{
			name:    "short form ambiguous",
			input:   "alpha",
			wantErr: "ambiguous",
		},
		{
			name:    "short form not found",
			input:   "delta",
			wantErr: "not found in the watched list",
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: "repo is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := makeTS()
			got, err := ts.ResolveRepo(tc.input)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (result %q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NewGitHubToolset constructor validation
// ---------------------------------------------------------------------------

func TestNewGitHubToolset_RejectsEmptyPAT(t *testing.T) {
	_, err := NewGitHubToolset("", nil, t.TempDir())
	if err == nil {
		t.Fatal("expected error for empty PAT, got nil")
	}
	if !strings.Contains(err.Error(), "PAT") {
		t.Fatalf("expected PAT in error message, got: %v", err)
	}
}

func TestNewGitHubToolset_RejectsEmptyStateDir(t *testing.T) {
	_, err := NewGitHubToolset("token", nil, "")
	if err == nil {
		t.Fatal("expected error for empty stateDir, got nil")
	}
	if !strings.Contains(err.Error(), "stateDir") {
		t.Fatalf("expected stateDir in error message, got: %v", err)
	}
}
