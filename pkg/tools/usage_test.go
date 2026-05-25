package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTrackerInTemp creates a UsageTracker whose workspace is t.TempDir().
// It also creates the state subdirectory so callers can pre-populate it.
func newTrackerInTemp(t *testing.T) (*UsageTracker, string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll state dir: %v", err)
	}
	return NewUsageTracker(dir), stateDir
}

// TestRecordUsage_Flash verifies RecordUsage writes correct JSON for a flash model.
func TestRecordUsage_Flash(t *testing.T) {
	ut, stateDir := newTrackerInTemp(t)
	now := time.Now().UTC()
	ut.RecordUsage("gemini-1.5-flash", 1000, 500)

	path := filepath.Join(stateDir, "usage_"+now.Format("2006-01-02")+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not found: %v", err)
	}

	var day usageDay
	if err := json.Unmarshal(data, &day); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if day.Flash.Calls != 1 {
		t.Errorf("Flash.Calls = %d, want 1", day.Flash.Calls)
	}
	if day.Flash.InTokens != 1000 {
		t.Errorf("Flash.InTokens = %d, want 1000", day.Flash.InTokens)
	}
	if day.Flash.OutTokens != 500 {
		t.Errorf("Flash.OutTokens = %d, want 500", day.Flash.OutTokens)
	}
	if day.Pro.Calls != 0 {
		t.Errorf("Pro.Calls = %d, want 0 (should not be touched)", day.Pro.Calls)
	}
}

// TestRecordUsage_Pro verifies RecordUsage writes correct JSON for a pro model.
func TestRecordUsage_Pro(t *testing.T) {
	ut, stateDir := newTrackerInTemp(t)
	now := time.Now().UTC()
	ut.RecordUsage("gemini-1.5-pro", 2000, 800)

	path := filepath.Join(stateDir, "usage_"+now.Format("2006-01-02")+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not found: %v", err)
	}

	var day usageDay
	if err := json.Unmarshal(data, &day); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if day.Pro.Calls != 1 {
		t.Errorf("Pro.Calls = %d, want 1", day.Pro.Calls)
	}
	if day.Pro.InTokens != 2000 {
		t.Errorf("Pro.InTokens = %d, want 2000", day.Pro.InTokens)
	}
	if day.Pro.OutTokens != 800 {
		t.Errorf("Pro.OutTokens = %d, want 800", day.Pro.OutTokens)
	}
	if day.Flash.Calls != 0 {
		t.Errorf("Flash.Calls = %d, want 0 (should not be touched)", day.Flash.Calls)
	}
}

// TestRecordUsage_Accumulates verifies two RecordUsage calls for the same model accumulate.
func TestRecordUsage_Accumulates(t *testing.T) {
	ut, stateDir := newTrackerInTemp(t)
	now := time.Now().UTC()
	ut.RecordUsage("gemini-flash", 100, 50)
	ut.RecordUsage("gemini-flash", 200, 75)

	path := filepath.Join(stateDir, "usage_"+now.Format("2006-01-02")+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not found: %v", err)
	}

	var day usageDay
	if err := json.Unmarshal(data, &day); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if day.Flash.Calls != 2 {
		t.Errorf("Flash.Calls = %d, want 2", day.Flash.Calls)
	}
	if day.Flash.InTokens != 300 {
		t.Errorf("Flash.InTokens = %d, want 300", day.Flash.InTokens)
	}
	if day.Flash.OutTokens != 125 {
		t.Errorf("Flash.OutTokens = %d, want 125", day.Flash.OutTokens)
	}
}

// TestFormatReceipt_EmptyOrMissingFile returns the "No usage recorded" message.
func TestFormatReceipt_EmptyOrMissingFile(t *testing.T) {
	ut, _ := newTrackerInTemp(t)
	// No RecordUsage call — file won't exist
	result := ut.FormatReceipt(time.Now().UTC())
	if !strings.HasPrefix(result, "No usage recorded") {
		t.Errorf("expected 'No usage recorded' prefix, got: %q", result)
	}
}

// TestFormatReceipt_PopulatedFile returns a receipt containing "TOTAL".
func TestFormatReceipt_PopulatedFile(t *testing.T) {
	ut, _ := newTrackerInTemp(t)
	now := time.Now().UTC()
	ut.RecordUsage("gemini-flash", 5000, 1500)
	ut.RecordUsage("gemini-pro", 3000, 900)

	receipt := ut.FormatReceipt(now)
	if !strings.Contains(receipt, "TOTAL") {
		t.Errorf("receipt missing 'TOTAL': %q", receipt)
	}
	if !strings.Contains(receipt, "Daily receipt") {
		t.Errorf("receipt missing 'Daily receipt': %q", receipt)
	}
}

// TestPruneOldFiles verifies that files older than 7 days are deleted by NewUsageTracker.
func TestPruneOldFiles(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Create an old file (8 days ago)
	oldDate := time.Now().UTC().AddDate(0, 0, -8).Format("2006-01-02")
	oldPath := filepath.Join(stateDir, "usage_"+oldDate+".json")
	if err := os.WriteFile(oldPath, []byte(`{"date":"`+oldDate+`"}`), 0o644); err != nil {
		t.Fatalf("WriteFile old: %v", err)
	}

	// Create a recent file (1 day ago) — should survive
	recentDate := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	recentPath := filepath.Join(stateDir, "usage_"+recentDate+".json")
	if err := os.WriteFile(recentPath, []byte(`{"date":"`+recentDate+`"}`), 0o644); err != nil {
		t.Fatalf("WriteFile recent: %v", err)
	}

	// NewUsageTracker calls pruneOldFiles during construction
	_ = NewUsageTracker(dir)

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old usage file should have been pruned but still exists: %s", oldPath)
	}
	if _, err := os.Stat(recentPath); os.IsNotExist(err) {
		t.Errorf("recent usage file should NOT have been pruned but is missing: %s", recentPath)
	}
}
