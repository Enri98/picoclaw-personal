//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	maxInputBytes = 65536
	// truncationMarker is appended after the cap is hit. We reserve markerHeadroom
	// bytes of room so the appended marker still fits inside the 1 MB contract.
	truncationMarker = "\n[output truncated at 1MB]"
	markerHeadroom   = 32
	maxOutputBytes   = 1048576 - markerHeadroom

	prlimitPath = "/usr/bin/prlimit"
	bashPath    = "/bin/bash"

	cwdScratch      = "scratch"
	cwdWikiReadonly = "wiki_readonly"

	resolvedScratch = "/home/picoclaw/scratch"
	resolvedWiki    = "/home/picoclaw/wiki"

	timeoutSeconds = 30
	timeoutExitCode = 124
)

func stderrJSON(msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	fmt.Fprintln(os.Stderr, string(b))
}

// buildCommand prepares the prlimit+bash invocation for the given cwd and
// shell command string. prlimit scopes the resource limits to the child
// process tree; a direct Setrlimit call here would also constrain this
// binary's own Go runtime.
//
// On context expiry we must SIGKILL the entire process group, not just the
// prlimit leader: bash may spawn backgrounded children that inherit the pipe
// write-end and keep cmd.Wait blocked on pipe drain past the timeout. Cancel
// + WaitDelay together bound both the run time and the post-kill drain time.
func buildCommand(ctx context.Context, cwd, shellCmd string) *exec.Cmd {
	cmd := exec.CommandContext(ctx,
		prlimitPath,
		"--as=314572800",
		"--cpu=30",
		"--fsize=2097152",
		"--",
		bashPath, "-c", shellCmd,
	)
	cmd.Dir = cwd
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/home/picoclaw-shell",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// boundedWriter caps combined stdout+stderr at maxOutputBytes.
type boundedWriter struct {
	mu        *sync.Mutex
	buf       *bytes.Buffer
	other     *bytes.Buffer
	truncated *bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	combined := w.buf.Len() + w.other.Len()
	if combined >= maxOutputBytes {
		*w.truncated = true
		return len(p), nil
	}
	remaining := maxOutputBytes - combined
	if len(p) > remaining {
		*w.truncated = true
		p = p[:remaining]
	}
	return w.buf.Write(p)
}

type result struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
}

func main() {
	cwdFlag := flag.String("cwd", "", "working directory mode: scratch or wiki_readonly")
	flag.Parse()

	var resolvedCwd string
	switch *cwdFlag {
	case cwdScratch:
		resolvedCwd = resolvedScratch
	case cwdWikiReadonly:
		resolvedCwd = resolvedWiki
	default:
		stderrJSON("invalid --cwd")
		os.Exit(2)
	}

	// Read command from stdin, hard cap 64 KB.
	lr := io.LimitReader(os.Stdin, maxInputBytes+1)
	raw, err := io.ReadAll(lr)
	if err != nil {
		stderrJSON("read error")
		os.Exit(2)
	}
	if len(raw) > maxInputBytes {
		stderrJSON("command too large")
		os.Exit(2)
	}
	if bytes.IndexByte(raw, 0x00) >= 0 {
		stderrJSON("NUL byte in command")
		os.Exit(2)
	}
	shellCmd := string(raw)

	// Verify CWD exists and is a directory.
	info, err := os.Stat(resolvedCwd)
	if err != nil || !info.IsDir() {
		stderrJSON("cwd missing")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeoutSeconds*time.Second)
	defer cancel()

	cmd := buildCommand(ctx, resolvedCwd, shellCmd)

	var (
		mu            sync.Mutex
		stdoutBuf     bytes.Buffer
		stderrBuf     bytes.Buffer
		stdoutTrunc   bool
		stderrTrunc   bool
	)

	cmd.Stdout = &boundedWriter{mu: &mu, buf: &stdoutBuf, other: &stderrBuf, truncated: &stdoutTrunc}
	cmd.Stderr = &boundedWriter{mu: &mu, buf: &stderrBuf, other: &stdoutBuf, truncated: &stderrTrunc}

	start := time.Now()
	runErr := cmd.Start()
	if runErr != nil {
		stderrJSON("failed to start")
		os.Exit(2)
	}

	waitErr := cmd.Wait()
	durationMs := time.Since(start).Milliseconds()

	timedOut := false
	exitCode := 0

	if ctx.Err() == context.DeadlineExceeded {
		timedOut = true
		exitCode = timeoutExitCode
		// Process group SIGKILL was already issued by cmd.Cancel; no PID-reuse
		// race here.
	} else if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	truncated := stdoutTrunc || stderrTrunc
	if stdoutTrunc {
		stdoutBuf.WriteString(truncationMarker)
	}
	if stderrTrunc {
		stderrBuf.WriteString(truncationMarker)
	}

	out := result{
		Stdout:     stdoutBuf.String(),
		Stderr:     stderrBuf.String(),
		ExitCode:   exitCode,
		Truncated:  truncated,
		DurationMs: durationMs,
		TimedOut:   timedOut,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}
