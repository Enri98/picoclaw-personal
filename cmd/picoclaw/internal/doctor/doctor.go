package doctor

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	pkgagent "github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/config"
)

// CheckResult holds the outcome of a single preflight check.
type CheckResult struct {
	Name    string
	Status  string // "PASS", "FAIL", "SKIP"
	Message string
}

// doctorConfig mirrors the actual shape of deploy/config.yaml. The main
// Config struct marks its Tools field with yaml:",inline", which means the
// yaml decoder expects tools sub-keys at the document root — but the deploy
// file nests them under "tools:". This wrapper provides the correct mapping.
type doctorConfig struct {
	ExpectedTimezone string             `yaml:"expected_timezone"`
	Tools            config.ToolsConfig `yaml:"tools"`
}

// loadYAMLConfig reads and parses a YAML deployment config file.
func loadYAMLConfig(path string) (*doctorConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var cfg doctorConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return &cfg, nil
}

// RunChecks executes all preflight checks and returns results. configPath
// is the path to the YAML deployment config.
func RunChecks(configPath string) []CheckResult {
	cfg, err := loadYAMLConfig(configPath)
	if err != nil {
		return []CheckResult{{
			Name:    "config",
			Status:  "FAIL",
			Message: err.Error(),
		}}
	}

	results := make([]CheckResult, 0, 7)
	results = append(results, checkSecretsPerms())
	results = append(results, checkStateGitignored(configPath))
	results = append(results, checkTimezone(cfg.ExpectedTimezone))
	results = append(results, checkOAuthTokens(cfg))
	results = append(results, checkTelegramAlert())
	results = append(results, checkSchedulerChatID(cfg))
	results = append(results, checkWatchedRepos(cfg))
	return results
}

// --- Check 1: secrets.env file permissions ---

const secretsEnvPath = "/etc/picoclaw/secrets.env"

func checkSecretsPerms() CheckResult {
	name := "secrets.env permissions"

	if runtime.GOOS == "windows" {
		return CheckResult{name, "SKIP", "not applicable on Windows"}
	}

	info, err := os.Stat(secretsEnvPath)
	if os.IsNotExist(err) {
		return CheckResult{name, "SKIP", secretsEnvPath + " not found (not deployed yet)"}
	}
	if err != nil {
		return CheckResult{name, "FAIL", "stat " + secretsEnvPath + ": " + err.Error()}
	}

	ok, msg := checkSecretsPermsMode(info.Mode())
	if ok {
		return CheckResult{name, "PASS", msg}
	}
	return CheckResult{name, "FAIL", msg}
}

// checkSecretsPermsMode is the pure, testable core.
func checkSecretsPermsMode(mode os.FileMode) (ok bool, message string) {
	perm := mode.Perm()
	// Allow 0600 (owner-only) or 0640 (owner + group-read for the picoclaw user).
	// Anything else is too permissive.
	if perm == 0600 || perm == 0640 {
		return true, fmt.Sprintf("%s has mode %04o", secretsEnvPath, perm)
	}
	return false, fmt.Sprintf("%s has mode %04o; want 0600 or 0640 — run: sudo chmod 0640 %s", secretsEnvPath, perm, secretsEnvPath)
}

// --- Check 2: state/ directory is gitignored ---

func checkStateGitignored(configPath string) CheckResult {
	name := "state/ gitignored"

	// Walk up from configPath to find .gitignore
	dir := filepath.Dir(configPath)
	for {
		candidate := filepath.Join(dir, ".gitignore")
		if data, err := os.ReadFile(candidate); err == nil {
			ok, msg := checkGitignoreContains(string(data))
			if ok {
				return CheckResult{name, "PASS", msg}
			}
			return CheckResult{name, "FAIL", msg}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return CheckResult{name, "SKIP", ".gitignore not found in any ancestor of " + configPath}
}

// checkGitignoreContains is the pure, testable core.
func checkGitignoreContains(content string) (ok bool, message string) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "state/" || line == "state" {
			return true, "state/ is listed in .gitignore"
		}
	}
	return false, "state/ is NOT listed in .gitignore — add 'state/' to prevent committing runtime data"
}

// --- Check 3: timezone ---

func checkTimezone(expected string) CheckResult {
	name := "timezone"
	if expected == "" {
		return CheckResult{name, "SKIP", "expected_timezone not set in config"}
	}
	ok, msg := pkgagent.CheckTimezone(expected)
	if ok {
		return CheckResult{name, "PASS", msg}
	}
	return CheckResult{name, "FAIL", msg}
}

// --- Check 4: OAuth tokens ---

func checkOAuthTokens(cfg *doctorConfig) CheckResult {
	name := "OAuth tokens"
	var failures []string
	var present []string
	skipped := 0

	// Gmail
	if cfg.Tools.Gmail.Enabled {
		clientID := os.Getenv("GMAIL_OAUTH_CLIENT_ID")
		clientSecret := os.Getenv("GMAIL_OAUTH_CLIENT_SECRET")
		if clientID == "" || clientSecret == "" {
			skipped++
		} else {
			for _, acc := range cfg.Tools.Gmail.Accounts {
				tok := os.Getenv(acc.RefreshTokenEnv)
				if tok == "" {
					failures = append(failures, fmt.Sprintf("gmail account %q: %s is empty", acc.Name, acc.RefreshTokenEnv))
				} else {
					present = append(present, fmt.Sprintf("gmail/%s", acc.Name))
				}
			}
		}
	}

	// Outlook
	if cfg.Tools.Outlook.Enabled {
		clientID := os.Getenv("OUTLOOK_OAUTH_CLIENT_ID")
		refreshToken := os.Getenv("OUTLOOK_REFRESH_TOKEN")
		if clientID == "" {
			skipped++
		} else if refreshToken == "" {
			failures = append(failures, "outlook: OUTLOOK_REFRESH_TOKEN is empty")
		} else {
			present = append(present, "outlook")
		}
	}

	// GCal
	if cfg.Tools.GCal.Enabled {
		clientID := os.Getenv("GMAIL_OAUTH_CLIENT_ID")
		clientSecret := os.Getenv("GMAIL_OAUTH_CLIENT_SECRET")
		refreshToken := os.Getenv("GCAL_REFRESH_TOKEN")
		if clientID == "" || clientSecret == "" {
			skipped++
		} else if refreshToken == "" {
			failures = append(failures, "gcal: GCAL_REFRESH_TOKEN is empty")
		} else {
			present = append(present, "gcal")
		}
	}

	if len(failures) > 0 {
		msg := strings.Join(failures, "; ")
		if len(present) > 0 {
			msg += " (present: " + strings.Join(present, ", ") + ")"
		}
		return CheckResult{name, "FAIL", msg}
	}

	noEnabled := !cfg.Tools.Gmail.Enabled && !cfg.Tools.Outlook.Enabled && !cfg.Tools.GCal.Enabled
	if noEnabled || (skipped > 0 && len(present) == 0) {
		return CheckResult{name, "SKIP", "no OAuth tools enabled or no client credentials in environment"}
	}

	if len(present) > 0 {
		return CheckResult{name, "PASS", "tokens present: " + strings.Join(present, ", ")}
	}
	return CheckResult{name, "SKIP", "no OAuth environment variables set"}
}

// --- Check 5: Telegram alert bot ---

const (
	telegramAlertTokenFile  = "/etc/picoclaw/telegram-alert-token"
	telegramAlertChatIDFile = "/etc/picoclaw/telegram-alert-chatid"
)

func checkTelegramAlert() CheckResult {
	name := "Telegram alert bot"

	if runtime.GOOS == "windows" {
		return CheckResult{name, "SKIP", "not applicable on Windows"}
	}

	tokenBytes, err := os.ReadFile(telegramAlertTokenFile)
	if os.IsNotExist(err) {
		return CheckResult{name, "SKIP", telegramAlertTokenFile + " not found (alert bot not configured)"}
	}
	if err != nil {
		return CheckResult{name, "FAIL", "reading " + telegramAlertTokenFile + ": " + err.Error()}
	}

	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return CheckResult{name, "FAIL", telegramAlertTokenFile + " is empty"}
	}

	chatIDBytes, err := os.ReadFile(telegramAlertChatIDFile)
	if os.IsNotExist(err) {
		return CheckResult{name, "FAIL", telegramAlertChatIDFile + " not found"}
	}
	if err != nil {
		return CheckResult{name, "FAIL", "reading " + telegramAlertChatIDFile + ": " + err.Error()}
	}

	chatID := strings.TrimSpace(string(chatIDBytes))
	if chatID == "" {
		return CheckResult{name, "FAIL", telegramAlertChatIDFile + " is empty"}
	}

	// Live reachability check: call getMe
	ok, msg := pingTelegramBot(token)
	if !ok {
		return CheckResult{name, "FAIL", msg}
	}
	return CheckResult{name, "PASS", msg}
}

func pingTelegramBot(token string) (ok bool, message string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", token)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false, "Telegram getMe failed: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("Telegram getMe returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return true, "Telegram alert bot reachable"
}

// --- Check 6: Scheduler primary_chat_id ---

func checkSchedulerChatID(cfg *doctorConfig) CheckResult {
	name := "scheduler primary_chat_id"

	if !cfg.Tools.Scheduler.Enabled {
		return CheckResult{name, "SKIP", "scheduler not enabled"}
	}

	if strings.TrimSpace(cfg.Tools.Scheduler.PrimaryChatID) == "" {
		return CheckResult{name, "FAIL", "scheduler.primary_chat_id is empty — set it to the Telegram chat ID of the primary user"}
	}
	return CheckResult{name, "PASS", "primary_chat_id is set"}
}

// --- Check 7: watched repos reachable ---

func checkWatchedRepos(cfg *doctorConfig) CheckResult {
	name := "watched repos reachable"

	if !cfg.Tools.GitHub.Enabled {
		return CheckResult{name, "SKIP", "github tool not enabled"}
	}

	repos := cfg.Tools.GitHub.WatchedRepos
	if len(repos) == 0 {
		return CheckResult{name, "SKIP", "watched_repos is empty"}
	}

	patEnv := cfg.Tools.GitHub.PATEnv
	pat := ""
	if patEnv != "" {
		pat = os.Getenv(patEnv)
	}
	if pat == "" {
		return CheckResult{name, "SKIP", "PAT env var " + patEnv + " not set"}
	}

	var failures []string
	var passed []string
	for _, repo := range repos {
		ok, msg := pingGitHubRepo(pat, repo)
		if ok {
			passed = append(passed, repo)
		} else {
			failures = append(failures, repo+": "+msg)
		}
	}

	if len(failures) > 0 {
		return CheckResult{name, "FAIL", strings.Join(failures, "; ")}
	}
	return CheckResult{name, "PASS", "reachable: " + strings.Join(passed, ", ")}
}

func pingGitHubRepo(pat, repo string) (ok bool, message string) {
	url := "https://api.github.com/repos/" + repo
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return true, "OK"
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// PrintResults writes check results to w in the plain-text format and returns
// the exit code (0 if all PASS/SKIP, 1 if any FAIL).
func PrintResults(w io.Writer, results []CheckResult) int {
	exitCode := 0
	for _, r := range results {
		fmt.Fprintf(w, "[ %-4s ] %s", r.Status, r.Name)
		if r.Message != "" {
			fmt.Fprintf(w, " — %s", r.Message)
		}
		fmt.Fprintln(w)
		if r.Status == "FAIL" {
			exitCode = 1
		}
	}

	// Summary line
	pass, fail, skip := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "PASS":
			pass++
		case "FAIL":
			fail++
		case "SKIP":
			skip++
		}
	}
	fmt.Fprintf(w, "\n%d passed, %d failed, %d skipped\n", pass, fail, skip)
	return exitCode
}
