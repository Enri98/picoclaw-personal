package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TestBriefingAssembler_AllNilToolsets
// ---------------------------------------------------------------------------

func TestBriefingAssembler_AllNilToolsets(t *testing.T) {
	ba := NewBriefingAssembler(nil, nil, nil, nil, nil, "")
	out := ba.Assemble(context.Background(), time.Now())
	if out == "" {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(out, "Buongiorno") {
		t.Errorf("expected Italian 'Buongiorno' header in output, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// TestBriefingAssembler_HeartbeatStaleness
// ---------------------------------------------------------------------------

func TestBriefingAssembler_HeartbeatStaleness(t *testing.T) {
	dir := t.TempDir()
	hbPath := dir + "/heartbeat"

	// Create the file with an old mtime (10 minutes ago).
	f, err := os.Create(hbPath)
	if err != nil {
		t.Fatalf("create heartbeat file: %v", err)
	}
	f.Close()

	past := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(hbPath, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	ba := NewBriefingAssembler(nil, nil, nil, nil, nil, hbPath)
	// Use current time so the 10-min-old mtime is detected as stale.
	out := ba.Assemble(context.Background(), time.Now())
	if !strings.Contains(out, "Heartbeat") {
		t.Errorf("expected heartbeat warning in output for stale file, got:\n%s", out)
	}

	// Now update the mtime to now — heartbeat is fresh.
	now := time.Now()
	if err := os.Chtimes(hbPath, now, now); err != nil {
		t.Fatalf("Chtimes (fresh): %v", err)
	}
	out2 := ba.Assemble(context.Background(), time.Now())
	if strings.Contains(out2, "Heartbeat") {
		t.Errorf("expected NO heartbeat warning for fresh file, got:\n%s", out2)
	}
}

// ---------------------------------------------------------------------------
// TestBriefingAssembler_ItalianWeekday
// ---------------------------------------------------------------------------

func TestBriefingAssembler_ItalianWeekday(t *testing.T) {
	// 2026-05-25 is a Monday → "Lunedì" in the header.
	monday := time.Date(2026, 5, 25, 9, 0, 0, 0, time.Local)
	ba := NewBriefingAssembler(nil, nil, nil, nil, nil, "")
	out := ba.Assemble(context.Background(), monday)

	// The assembler capitalizes the first letter: "Lunedì".
	if !strings.Contains(out, "Lunedì") {
		t.Errorf("expected 'Lunedì' in output for Monday 2026-05-25, got:\n%s", out)
	}
}
