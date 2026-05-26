package agent

import (
	"regexp"
	"testing"
)

// ---------------------------------------------------------------------------
// parseClaudeCommand
// ---------------------------------------------------------------------------

func TestParseClaudeCommand(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantRepo    string
		wantQ       string
		wantErrMsg  bool
	}{
		{
			name:     "full owner/repo with double-quoted question",
			input:    `/claude foo/bar "hello"`,
			wantRepo: "foo/bar",
			wantQ:    "hello",
		},
		{
			name:     "short repo with double-quoted multi-word question",
			input:    `/claude foo "multi word"`,
			wantRepo: "foo",
			wantQ:    "multi word",
		},
		{
			name:     "short repo with unquoted single word",
			input:    `/claude foo single`,
			wantRepo: "foo",
			wantQ:    "single",
		},
		{
			name:     "short repo with single-quoted question",
			input:    `/claude foo 'with single quotes'`,
			wantRepo: "foo",
			wantQ:    "with single quotes",
		},
		{
			name:       "command only — no repo or question",
			input:      `/claude`,
			wantErrMsg: true,
		},
		{
			name:       "repo but no question",
			input:      `/claude foo`,
			wantErrMsg: true,
		},
		{
			name:       "repo with empty double-quoted question",
			input:      `/claude foo ""`,
			wantErrMsg: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, q, errMsg := parseClaudeCommand(tc.input)
			if tc.wantErrMsg {
				if errMsg == "" {
					t.Fatalf("expected error message, got repo=%q q=%q", repo, q)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error message %q", errMsg)
			}
			if repo != tc.wantRepo {
				t.Errorf("repo: got %q, want %q", repo, tc.wantRepo)
			}
			if q != tc.wantQ {
				t.Errorf("question: got %q, want %q", q, tc.wantQ)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// splitOwnerRepo
// ---------------------------------------------------------------------------

func TestSplitOwnerRepo(t *testing.T) {
	cases := []struct {
		input     string
		wantOwner string
		wantName  string
		wantOK    bool
	}{
		{"owner/repo", "owner", "repo", true},
		{"a/b/c", "a", "b/c", true}, // SplitN(2) keeps remainder in second part
		{"", "", "", false},
		{"owner", "", "", false},
		{"owner/", "", "", false},
		{"/repo", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			owner, name, ok := splitOwnerRepo(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if owner != tc.wantOwner {
				t.Errorf("owner: got %q, want %q", owner, tc.wantOwner)
			}
			if name != tc.wantName {
				t.Errorf("name: got %q, want %q", name, tc.wantName)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// truncateForTitle
// ---------------------------------------------------------------------------

func TestTruncateForTitle(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{
			name:   "short string unchanged",
			input:  "hello",
			maxLen: 80,
			want:   "hello",
		},
		{
			name:   "long string truncated with ellipsis",
			input:  "abcdefghij",
			maxLen: 5,
			// runes[:4] + "…"
			want: "abcd…",
		},
		{
			name:   "exact length unchanged",
			input:  "hello",
			maxLen: 5,
			want:   "hello",
		},
		{
			name:   "multibyte rune truncation",
			input:  "café",
			maxLen: 3,
			// runes[:2] + "…" = "ca…"
			want: "ca…",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateForTitle(tc.input, tc.maxLen)
			if got != tc.want {
				t.Errorf("truncateForTitle(%q, %d) = %q, want %q", tc.input, tc.maxLen, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// newWatchID
// ---------------------------------------------------------------------------

var watchIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewWatchID(t *testing.T) {
	id, err := newWatchID()
	if err != nil {
		t.Fatalf("newWatchID: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}
	if !watchIDRe.MatchString(id) {
		t.Fatalf("ID %q does not match UUID v4 pattern", id)
	}

	// Verify uniqueness across two calls.
	id2, err := newWatchID()
	if err != nil {
		t.Fatalf("newWatchID second call: %v", err)
	}
	if id == id2 {
		t.Fatalf("two consecutive newWatchID calls returned identical value %q", id)
	}
}
