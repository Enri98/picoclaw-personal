package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// FakeRunner for testing — no real sudo or runshell binary required.
// ---------------------------------------------------------------------------

type fakeRunner struct {
	result RunResult
	err    error
	Called bool
	LastCwdMode string
	LastCmd     string
}

func (f *fakeRunner) Run(_ context.Context, cwdMode, cmd string) (RunResult, error) {
	f.Called = true
	f.LastCwdMode = cwdMode
	f.LastCmd = cmd
	return f.result, f.err
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func newBashSetup(t *testing.T, r Runner) (*BashTool, string) {
	t.Helper()
	workspaceDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceDir, "state"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tool := NewBashTool(workspaceDir, r)
	return tool, workspaceDir
}

// ---------------------------------------------------------------------------
// 1. TestIsDestructive
// ---------------------------------------------------------------------------

func TestIsDestructive(t *testing.T) {
	cases := []struct {
		cmd     string
		want    bool
		name    string
	}{
		// rm -rf variants — should match
		{cmd: "rm -rf /", want: true, name: "rm-rf-root"},
		{cmd: "rm -rf /*", want: true, name: "rm-rf-root"},
		{cmd: "rm -rf /home", want: true, name: "rm-rf-root"},
		{cmd: "rm -fr /boot", want: true, name: "rm-rf-root"},

		// rm -rf on /tmp/foo — must NOT match (the path starts at /, but it's not root itself)
		// The regex matches /[^\s]* so /tmp/foo will match. Document current behavior:
		// rm -rf /tmp/foo DOES match because the regex targets any /path, not just bare /.
		// This is intentionally permissive — see bash_destructive.go header.
		{cmd: "rm -rf /tmp/foo", want: true, name: "rm-rf-root"},

		// rm -rf on relative path — should NOT match
		{cmd: "rm -rf tmp/foo", want: false, name: ""},
		{cmd: "rm -f /tmp/foo", want: false, name: ""}, // -f only, not -rf

		// dd of=/dev/ variants
		{cmd: "dd if=/dev/zero of=/dev/sda", want: true, name: "dd-block-device"},
		{cmd: "dd of=/dev/sdb bs=512", want: true, name: "dd-block-device"},

		// dd without /dev/ — should NOT match
		{cmd: "dd if=input.img of=output.img", want: false, name: ""},

		// mkfs variants
		{cmd: "mkfs /dev/sda1", want: true, name: "mkfs"},
		{cmd: "mkfs.ext4 /dev/sda1", want: true, name: "mkfs"},
		{cmd: "mkfs.vfat /dev/mmcblk0p1", want: true, name: "mkfs"},

		// mkfsx — must NOT match (not a word boundary)
		{cmd: "mkfsx something", want: false, name: ""},

		// chmod 777 /
		{cmd: "chmod 777 /", want: true, name: "chmod-777-root"},
		{cmd: "chmod 777 /etc", want: true, name: "chmod-777-root"},

		// chmod 777 on relative path — should NOT match
		{cmd: "chmod 777 mydir", want: false, name: ""},
		{cmd: "chmod 755 /", want: false, name: ""},

		// redirect to system dirs
		{cmd: "echo foo > /etc/passwd", want: true, name: "redirect-to-system-dir"},
		{cmd: "cat x >> /usr/local/bin/tool", want: true, name: "redirect-to-system-dir"},
		{cmd: "cat x > /boot/config.txt", want: true, name: "redirect-to-system-dir"},
		{cmd: "cat x > /bin/sh", want: true, name: "redirect-to-system-dir"},
		{cmd: "cat x > /sbin/init", want: true, name: "redirect-to-system-dir"},

		// redirect to safe dirs — should NOT match
		{cmd: "echo foo > /tmp/out.txt", want: false, name: ""},
		{cmd: "echo foo > /home/picoclaw/scratch/out.txt", want: false, name: ""},

		// shutdown / reboot / halt / poweroff
		{cmd: "shutdown now", want: true, name: "system-power"},
		{cmd: "reboot", want: true, name: "system-power"},
		{cmd: "halt", want: true, name: "system-power"},
		{cmd: "poweroff", want: true, name: "system-power"},
		{cmd: "sudo reboot", want: true, name: "system-power"},

		// 'shutdown' appearing inside a longer word — should NOT match
		{cmd: "echo shutdown_hook", want: false, name: ""},
	}

	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			got, pname := IsDestructive(tc.cmd)
			if got != tc.want {
				t.Errorf("IsDestructive(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
			if tc.want && tc.name != "" && pname != tc.name {
				t.Errorf("IsDestructive(%q) pattern = %q, want %q", tc.cmd, pname, tc.name)
			}
		})
	}
}

// 2. TestIsDestructive_KnownBypasses
// Documenting intentional non-coverage; privsep is the security boundary, not the regex.

func TestIsDestructive_KnownBypasses(t *testing.T) {
	// Note: "bash -c 'rm -rf /'" and "eval 'rm -rf /'" ARE caught by the
	// rm-rf-root regex because the literal substring matches. They are NOT
	// listed here. Bypasses below are ones the regex genuinely misses because
	// the payload is opaque to a literal-text regex.
	bypasses := []string{
		// Whitespace trick: r''m -rf /  (shell evaluates concatenation; regex sees no "rm")
		"r''m -rf /",
		// Base64 encoded command piped to bash (payload not visible to regex)
		"echo cm0gLXJmIC8= | base64 -d | bash",
		// Variable indirection: cmd stored elsewhere, only reference visible here
		"eval $DANGEROUS_CMD",
	}

	for _, cmd := range bypasses {
		t.Run(cmd, func(t *testing.T) {
			got, _ := IsDestructive(cmd)
			if got {
				t.Errorf("IsDestructive(%q) = true: this bypass was unexpectedly caught; update test if regexes are intentionally hardened", cmd)
			}
			// These NOT being caught is expected behavior.
			// The privsep boundary (picoclaw-shell uid, runshell rlimits) is what actually contains harm.
		})
	}
}

// ---------------------------------------------------------------------------
// 3. TestBashTool_HappyPath
// ---------------------------------------------------------------------------

func TestBashTool_HappyPath(t *testing.T) {
	fake := &fakeRunner{result: RunResult{Stdout: "hello\n", ExitCode: 0}}
	tool, _ := newBashSetup(t, fake)

	result := tool.Execute(context.Background(), map[string]any{"cmd": "echo hello"})
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "hello") {
		t.Errorf("expected output to contain 'hello', got: %q", result.ForLLM)
	}
	if !fake.Called {
		t.Error("expected runner to be called")
	}
}

// ---------------------------------------------------------------------------
// 4. TestBashTool_RejectsInvalidArgs
// ---------------------------------------------------------------------------

func TestBashTool_RejectsInvalidArgs(t *testing.T) {
	fake := &fakeRunner{result: RunResult{Stdout: "ok", ExitCode: 0}}

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "empty cmd",
			args: map[string]any{"cmd": ""},
		},
		{
			name: "NUL byte in cmd",
			args: map[string]any{"cmd": "echo \x00foo"},
		},
		{
			name: "oversized cmd",
			args: map[string]any{"cmd": strings.Repeat("a", 65537)},
		},
		{
			name: "invalid cwd_mode",
			args: map[string]any{"cmd": "echo hi", "cwd_mode": "dangerous"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, _ := newBashSetup(t, fake)
			fake.Called = false
			result := tool.Execute(context.Background(), tc.args)
			if !result.IsError {
				t.Errorf("expected error, got success: %s", result.ForLLM)
			}
			if fake.Called {
				t.Error("runner must not be called on invalid input")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. TestBashTool_DestructiveCreatesProposal
// ---------------------------------------------------------------------------

func TestBashTool_DestructiveCreatesProposal(t *testing.T) {
	fake := &fakeRunner{}
	tool, _ := newBashSetup(t, fake)

	result := tool.Execute(context.Background(), map[string]any{"cmd": "rm -rf /"})
	if result.IsError {
		t.Fatalf("expected proposal result, got error: %s", result.ForLLM)
	}
	if fake.Called {
		t.Error("runner must NOT be called for destructive commands")
	}
	if !strings.Contains(result.ForUser, "proposal_id") && !strings.Contains(result.ForUser, "Proposal ID") {
		t.Errorf("ForUser should contain proposal_id, got: %q", result.ForUser)
	}
	if !strings.Contains(result.ForLLM, "rm-rf-root") {
		t.Errorf("ForLLM should mention the matched pattern, got: %q", result.ForLLM)
	}
}

// ---------------------------------------------------------------------------
// 6. TestBashProposalStore_RoundTrip
// ---------------------------------------------------------------------------

func TestBashProposalStore_RoundTrip(t *testing.T) {
	fake := &fakeRunner{result: RunResult{Stdout: "round-trip-ok\n", ExitCode: 0}}
	workspaceDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceDir, "state"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	store := NewBashProposalStore(workspaceDir, fake)

	p, err := store.Propose("echo hello", "scratch", "rm-rf-root", "test reason")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if p.ID == "" {
		t.Fatal("expected non-empty proposal ID")
	}

	// Verify JSON file exists with one entry.
	data, err := os.ReadFile(filepath.Join(workspaceDir, "state", "bash_proposals.json"))
	if err != nil {
		t.Fatalf("read proposals file: %v", err)
	}
	var proposals []BashProposal
	if err := json.Unmarshal(data, &proposals); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("expected 1 proposal in file, got %d", len(proposals))
	}

	// Apply — file should be empty after, runner should be called.
	result, err := store.Apply(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Stdout != "round-trip-ok\n" {
		t.Errorf("Apply result stdout = %q, want %q", result.Stdout, "round-trip-ok\n")
	}
	if !fake.Called {
		t.Error("runner must be called by Apply")
	}
	if fake.LastCmd != "echo hello" {
		t.Errorf("Apply used cmd %q, want %q", fake.LastCmd, "echo hello")
	}

	// File should now have zero entries.
	data, err = os.ReadFile(filepath.Join(workspaceDir, "state", "bash_proposals.json"))
	if err != nil {
		t.Fatalf("read after apply: %v", err)
	}
	if err := json.Unmarshal(data, &proposals); err != nil {
		t.Fatalf("unmarshal after apply: %v", err)
	}
	if len(proposals) != 0 {
		t.Errorf("expected 0 proposals after Apply, got %d", len(proposals))
	}
}

// ---------------------------------------------------------------------------
// 7. TestBashProposalStore_Expiry
// ---------------------------------------------------------------------------

func TestBashProposalStore_Expiry(t *testing.T) {
	fake := &fakeRunner{}
	workspaceDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceDir, "state"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	store := NewBashProposalStore(workspaceDir, fake)

	// Write a proposal directly with ExpiresAt in the past.
	expired := BashProposal{
		ID:        "expired-id-1234",
		Cmd:       "rm -rf /",
		CwdMode:   "scratch",
		Pattern:   "rm-rf-root",
		Reason:    "test",
		ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(),
	}
	data, _ := json.MarshalIndent([]BashProposal{expired}, "", "  ")
	if err := os.WriteFile(filepath.Join(workspaceDir, "state", "bash_proposals.json"), data, 0o644); err != nil {
		t.Fatalf("write proposals: %v", err)
	}

	list := store.List()
	if len(list) != 0 {
		t.Errorf("expected 0 proposals after expiry purge, got %d", len(list))
	}
}

// ---------------------------------------------------------------------------
// 8. TestBashProposalStore_RejectedDoesNotExecute
// ---------------------------------------------------------------------------

func TestBashProposalStore_RejectedDoesNotExecute(t *testing.T) {
	fake := &fakeRunner{}
	workspaceDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceDir, "state"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	store := NewBashProposalStore(workspaceDir, fake)

	p, err := store.Propose("dd if=/dev/zero of=/dev/sda", "scratch", "dd-block-device", "test")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if err := store.Reject(p.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	// Runner must not have been called.
	if fake.Called {
		t.Error("runner must NOT be called by Reject")
	}

	// Proposal must be gone.
	list := store.List()
	for _, lp := range list {
		if lp.ID == p.ID {
			t.Error("rejected proposal still present in store")
		}
	}

	// Apply after reject must fail.
	if _, err := store.Apply(context.Background(), p.ID); err == nil {
		t.Error("Apply after Reject should return error")
	}
}

// ---------------------------------------------------------------------------
// 9. TestBashTool_OutputTruncation
// ---------------------------------------------------------------------------

func TestBashTool_OutputTruncation(t *testing.T) {
	fake := &fakeRunner{result: RunResult{
		Stdout:    "lots of output...",
		ExitCode:  0,
		Truncated: true,
	}}
	tool, _ := newBashSetup(t, fake)

	result := tool.Execute(context.Background(), map[string]any{"cmd": "cat bigfile"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "truncated") {
		t.Errorf("expected truncation warning in output, got: %q", result.ForLLM)
	}
}

// ---------------------------------------------------------------------------
// 10. TestBashTool_Timeout
// ---------------------------------------------------------------------------

func TestBashTool_Timeout(t *testing.T) {
	fake := &fakeRunner{result: RunResult{
		ExitCode: 124,
		TimedOut: true,
	}}
	tool, _ := newBashSetup(t, fake)

	result := tool.Execute(context.Background(), map[string]any{"cmd": "sleep 999"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "timed out") {
		t.Errorf("expected timeout warning in output, got: %q", result.ForLLM)
	}
}
