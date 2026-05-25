package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Runner is the interface for executing commands via picoclaw-runshell.
// The default implementation shells out to sudo+runshell; tests inject a fake.
type Runner interface {
	Run(ctx context.Context, cwdMode, cmd string) (RunResult, error)
}

// RunResult holds the structured output from picoclaw-runshell.
type RunResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	Truncated  bool
	DurationMs int
	TimedOut   bool
}

// sudoRunshellRunner is the default Runner that invokes picoclaw-runshell via sudo.
type sudoRunshellRunner struct {
	path string // path to picoclaw-runshell binary
	sudo string // path to sudo binary
	user string // user to run as
}

// runshellOutput is the JSON shape returned by picoclaw-runshell on stdout.
type runshellOutput struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	Truncated  bool   `json:"truncated"`
	DurationMs int    `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
}

func (r *sudoRunshellRunner) Run(ctx context.Context, cwdMode, cmd string) (RunResult, error) {
	args := []string{
		"-n",
		"-u", r.user,
		r.path,
		"--cwd=" + cwdMode,
	}
	c := exec.CommandContext(ctx, r.sudo, args...)
	c.Stdin = strings.NewReader(cmd)

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()
	// Contract: runshell exits 0 on success (bash exit is inside the JSON on
	// stdout); non-zero only on runshell-internal failure with a JSON error
	// object on stderr. sudo itself can emit benign warnings on stderr while
	// exiting 0 (e.g. "sudo: unable to resolve host" on a Pi with no hostname
	// in /etc/hosts) — we must NOT treat that as failure. Classify purely on
	// the exit code.
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return RunResult{}, fmt.Errorf("runshell error: %s", detail)
	}

	var out runshellOutput
	if jsonErr := json.Unmarshal(stdout.Bytes(), &out); jsonErr != nil {
		return RunResult{}, fmt.Errorf("runshell output parse error: %w (stderr: %s)", jsonErr, strings.TrimSpace(stderr.String()))
	}

	return RunResult{
		Stdout:     out.Stdout,
		Stderr:     out.Stderr,
		ExitCode:   out.ExitCode,
		Truncated:  out.Truncated,
		DurationMs: out.DurationMs,
		TimedOut:   out.TimedOut,
	}, nil
}

// BashTool is the LLM-facing bash tool that invokes commands via picoclaw-runshell.
//
// TODO: when chunks 7–10 land gmail/outlook/github/link_fetch, strip "bash" from
// the tool list on turns where any of those fired (plan §3.2 untrusted-content lock).
type BashTool struct {
	workspaceDir string
	runner       Runner
	proposals    *BashProposalStore
}

// NewBashTool constructs a BashTool. Pass nil for runner to use the default
// sudoRunshellRunner invoking /usr/local/bin/picoclaw-runshell.
func NewBashTool(workspaceDir string, runner Runner) *BashTool {
	if runner == nil {
		runner = &sudoRunshellRunner{
			path: "/usr/local/bin/picoclaw-runshell",
			sudo: "/usr/bin/sudo",
			user: "picoclaw-shell",
		}
	}
	return &BashTool{
		workspaceDir: workspaceDir,
		runner:       runner,
		proposals:    NewBashProposalStore(workspaceDir, runner),
	}
}

// Proposals returns the BashProposalStore so the /apply and /reject command
// handlers can apply or reject pending proposals without constructing a second
// store over the same directory (which would split mutex coordination).
func (t *BashTool) Proposals() *BashProposalStore {
	return t.proposals
}

func (t *BashTool) Name() string { return "bash" }

func (t *BashTool) Description() string {
	return "Run a shell command as the restricted picoclaw-shell user. " +
		"Output from stdout, stderr, and exit code are returned. " +
		"CWD is /home/picoclaw/scratch by default (writable) or " +
		"/home/picoclaw/wiki (read-only). " +
		"Destructive commands are gated behind a proposal/apply flow."
}

func (t *BashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cmd": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
			},
			"cwd_mode": map[string]any{
				"type":        "string",
				"enum":        []string{"scratch", "wiki_readonly"},
				"description": `Working directory mode: "scratch" (default, writable) or "wiki_readonly".`,
			},
		},
		"required": []string{"cmd"},
	}
}

const maxCmdBytes = 65536

func (t *BashTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	cmd, _ := args["cmd"].(string)
	cwdMode, _ := args["cwd_mode"].(string)

	// Validate cmd
	if cmd == "" {
		return ErrorResult("cmd is required")
	}
	if len(cmd) > maxCmdBytes {
		return ErrorResult(fmt.Sprintf("cmd exceeds maximum length of %d bytes", maxCmdBytes))
	}
	if bytes.ContainsRune([]byte(cmd), 0) {
		return ErrorResult("cmd must not contain NUL bytes")
	}

	// Validate cwd_mode
	if cwdMode == "" {
		cwdMode = "scratch"
	}
	if cwdMode != "scratch" && cwdMode != "wiki_readonly" {
		return ErrorResult(fmt.Sprintf("cwd_mode must be \"scratch\" or \"wiki_readonly\", got %q", cwdMode))
	}

	// Destructive pattern check
	if matched, pattern := IsDestructive(cmd); matched {
		id, err := newProposalID()
		if err != nil {
			return ErrorResult("failed to generate proposal ID: " + err.Error())
		}
		reason := fmt.Sprintf("Command matched destructive pattern %q and requires explicit approval.", pattern)
		p, err := t.proposals.proposeWithID(id, cmd, cwdMode, pattern, reason)
		if err != nil {
			return ErrorResult("failed to queue proposal: " + err.Error())
		}
		return &ToolResult{
			ForLLM: fmt.Sprintf(
				"Command blocked: matched destructive pattern %q. Proposal %s queued for user approval.",
				pattern, p.ID,
			),
			ForUser: fmt.Sprintf(
				"Destructive command detected (pattern: %s).\nCommand: %s\nProposal ID: %s\n\nRun /apply %s to execute, or /reject %s to cancel (expires in 15 min).",
				pattern, cmd, p.ID, p.ID, p.ID,
			),
		}
	}

	// Execute via runner
	result, err := t.runner.Run(ctx, cwdMode, cmd)
	if err != nil {
		return ErrorResult("execution error: " + err.Error())
	}

	return formatRunResult(result)
}

// formatRunResult builds a ToolResult from a RunResult.
func formatRunResult(r RunResult) *ToolResult {
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit_code: %d\n", r.ExitCode)
	if r.Stdout != "" {
		fmt.Fprintf(&sb, "stdout:\n%s\n", r.Stdout)
	}
	if r.Stderr != "" {
		fmt.Fprintf(&sb, "stderr:\n%s\n", r.Stderr)
	}
	if r.DurationMs > 0 {
		fmt.Fprintf(&sb, "duration_ms: %d\n", r.DurationMs)
	}
	if r.TimedOut {
		sb.WriteString("[WARNING: command timed out]\n")
	}
	if r.Truncated {
		sb.WriteString("[WARNING: output was truncated]\n")
	}
	content := sb.String()
	return &ToolResult{
		ForLLM:  content,
		ForUser: content,
	}
}
