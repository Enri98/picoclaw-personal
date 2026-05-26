package doctor

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestCheckSecretsPermsMode verifies the pure permission-check logic.
func TestCheckSecretsPermsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on Windows")
	}

	tests := []struct {
		name    string
		mode    os.FileMode
		wantOK  bool
		wantSub string
	}{
		{"0600 owner-only", 0600, true, "0600"},
		{"0640 group-readable", 0640, true, "0640"},
		{"0644 world-readable", 0644, false, "want 0600 or 0640"},
		{"0666 world-rw", 0666, false, "want 0600 or 0640"},
		{"0700 owner-exec", 0700, false, "want 0600 or 0640"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, msg := checkSecretsPermsMode(tt.mode)
			if ok != tt.wantOK {
				t.Errorf("checkSecretsPermsMode(%04o) ok=%v, want %v; msg=%q", tt.mode, ok, tt.wantOK, msg)
			}
			if tt.wantSub != "" && !contains(msg, tt.wantSub) {
				t.Errorf("checkSecretsPermsMode(%04o) message %q does not contain %q", tt.mode, msg, tt.wantSub)
			}
		})
	}
}

// TestCheckGitignoreContains verifies the gitignore parsing logic.
func TestCheckGitignoreContains(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantOK  bool
	}{
		{
			name:    "state/ present with trailing slash",
			content: "bin/\nstate/\nsessions/\n",
			wantOK:  true,
		},
		{
			name:    "state without trailing slash",
			content: "bin/\nstate\n",
			wantOK:  true,
		},
		{
			name:    "state/ with leading whitespace",
			content: "bin/\n  state/\n",
			wantOK:  true,
		},
		{
			name:    "no state entry",
			content: "bin/\n*.log\n.DS_Store\n",
			wantOK:  false,
		},
		{
			name:    "empty file",
			content: "",
			wantOK:  false,
		},
		{
			name:    "stateful/ should not match",
			content: "stateful/\n",
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, msg := checkGitignoreContains(tt.content)
			if ok != tt.wantOK {
				t.Errorf("checkGitignoreContains() ok=%v, want %v; msg=%q", ok, tt.wantOK, msg)
			}
		})
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
