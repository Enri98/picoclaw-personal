package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	flashPriceInPerM  = 0.075 // USD per 1M input tokens
	flashPriceOutPerM = 0.300 // USD per 1M output tokens
	proPriceInPerM    = 1.250 // USD per 1M input tokens
	proPriceOutPerM   = 5.000 // USD per 1M output tokens
)

type usageModelRecord struct {
	Calls     int `json:"calls"`
	InTokens  int `json:"in_tokens"`
	OutTokens int `json:"out_tokens"`
}

type usageDay struct {
	Date  string           `json:"date"`
	Flash usageModelRecord `json:"flash"`
	Pro   usageModelRecord `json:"pro"`
}

// UsageTracker records Vertex AI token usage per day and formats receipts.
// It is safe for concurrent use. The zero value is not usable; construct via
// NewUsageTracker.
type UsageTracker struct {
	mu        sync.Mutex
	workspace string
}

// NewUsageTracker constructs a tracker for workspace and deletes state files
// older than 7 days.
func NewUsageTracker(workspace string) *UsageTracker {
	ut := &UsageTracker{workspace: workspace}
	ut.pruneOldFiles()
	return ut
}

func (ut *UsageTracker) stateDir() string {
	return filepath.Join(ut.workspace, "state")
}

func (ut *UsageTracker) filePath(date time.Time) string {
	return filepath.Join(ut.stateDir(), "usage_"+date.UTC().Format("2006-01-02")+".json")
}

func (ut *UsageTracker) pruneOldFiles() {
	cutoff := time.Now().UTC().AddDate(0, 0, -7)
	pattern := filepath.Join(ut.stateDir(), "usage_*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, path := range matches {
		base := filepath.Base(path)
		dateStr := strings.TrimSuffix(strings.TrimPrefix(base, "usage_"), ".json")
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

func bucketModel(model string) string {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "flash") {
		return "flash"
	}
	if strings.Contains(lower, "pro") {
		return "pro"
	}
	return ""
}

// RecordUsage aggregates inTokens and outTokens into today's (UTC) daily state
// file. If model is neither flash nor pro, the call is silently skipped.
func (ut *UsageTracker) RecordUsage(model string, inTokens, outTokens int) {
	bucket := bucketModel(model)
	if bucket == "" {
		return
	}

	ut.mu.Lock()
	defer ut.mu.Unlock()

	now := time.Now().UTC()
	path := ut.filePath(now)
	dateStr := now.Format("2006-01-02")

	day := usageDay{Date: dateStr}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &day)
	}

	switch bucket {
	case "flash":
		day.Flash.Calls++
		day.Flash.InTokens += inTokens
		day.Flash.OutTokens += outTokens
	case "pro":
		day.Pro.Calls++
		day.Pro.InTokens += inTokens
		day.Pro.OutTokens += outTokens
	}

	data, err := json.MarshalIndent(day, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(ut.stateDir(), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// FormatReceipt reads the daily usage file for date (UTC) and returns a
// formatted receipt string per §23. Returns a no-data message when absent.
func (ut *UsageTracker) FormatReceipt(date time.Time) string {
	date = date.UTC()
	dateStr := date.Format("2006-01-02")
	path := ut.filePath(date)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("No usage recorded for %s.", dateStr)
	}

	var day usageDay
	if err := json.Unmarshal(data, &day); err != nil {
		return fmt.Sprintf("No usage recorded for %s.", dateStr)
	}

	flashCost := float64(day.Flash.InTokens)/1e6*flashPriceInPerM +
		float64(day.Flash.OutTokens)/1e6*flashPriceOutPerM
	proCost := float64(day.Pro.InTokens)/1e6*proPriceInPerM +
		float64(day.Pro.OutTokens)/1e6*proPriceOutPerM
	totalCost := flashCost + proCost
	totalIn := day.Flash.InTokens + day.Pro.InTokens
	totalOut := day.Flash.OutTokens + day.Pro.OutTokens

	displayDate := date.Format("2 January 2006")
	thin := "────────────────────────────"
	thick := "════════════════════════════"

	return fmt.Sprintf(
		"🧾 Daily receipt — %s\n%s\n  Gemini Flash   ×%-5d  $%.3f\n  Gemini Pro      ×%-4d  $%.3f\n%s\n  Tokens in:   %s\n  Tokens out:   %s\n  TOTAL              $%.3f\n%s\n  * Estimate. Verify at console.cloud.google.com",
		displayDate,
		thin,
		day.Flash.Calls, flashCost,
		day.Pro.Calls, proCost,
		thin,
		formatTokens(totalIn),
		formatTokens(totalOut),
		totalCost,
		thick,
	)
}

func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	result := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}
