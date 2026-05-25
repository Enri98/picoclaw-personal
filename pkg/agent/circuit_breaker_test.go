package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// makeTestCB builds a CircuitBreaker pointed at a temp workspace with a
// controllable alert counter.
func makeTestCB(t *testing.T) (*CircuitBreaker, *atomic.Int64) {
	t.Helper()
	workspace := t.TempDir()
	var alertCount atomic.Int64
	alertFn := func(_ context.Context, _ *bus.InboundContext, _ string) {
		alertCount.Add(1)
	}
	cb := newCircuitBreaker(workspace, alertFn)
	return cb, &alertCount
}

func dummyInbound() *bus.InboundContext {
	return &bus.InboundContext{Channel: "test", ChatID: "42"}
}

func makeLLMReq(inbound *bus.InboundContext) *LLMHookRequest {
	tc := &TurnContext{Inbound: inbound}
	return &LLMHookRequest{Context: tc}
}

func makeToolReq(inbound *bus.InboundContext) *ToolCallHookRequest {
	tc := &TurnContext{Inbound: inbound}
	return &ToolCallHookRequest{Context: tc}
}

// --- Rate limit: LLM per-minute ---

func TestCircuitBreaker_LLMPerMinute_UnderLimitPasses(t *testing.T) {
	cb, _ := makeTestCB(t)
	req := makeLLMReq(dummyInbound())
	ctx := context.Background()

	for i := 0; i < maxLLMPerMinute-1; i++ {
		_, dec, err := cb.BeforeLLM(ctx, req)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if dec.Action != HookActionContinue {
			t.Fatalf("call %d: expected continue, got %s", i, dec.Action)
		}
	}
}

func TestCircuitBreaker_LLMPerMinute_AtLimitAborts(t *testing.T) {
	cb, alertCount := makeTestCB(t)
	req := makeLLMReq(dummyInbound())
	ctx := context.Background()

	// Saturate the window.
	for i := 0; i < maxLLMPerMinute; i++ {
		_, _, _ = cb.BeforeLLM(ctx, req)
	}
	// The next call should be aborted.
	_, dec, err := cb.BeforeLLM(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != HookActionHardAbort {
		t.Fatalf("expected hard_abort, got %s", dec.Action)
	}
	// Wait briefly for async alert goroutine.
	time.Sleep(50 * time.Millisecond)
	if alertCount.Load() == 0 {
		t.Fatal("expected at least one alert on first trip")
	}
}

// --- Rate limit: tool per-minute ---

func TestCircuitBreaker_ToolPerMinute_UnderLimitPasses(t *testing.T) {
	cb, _ := makeTestCB(t)
	req := makeToolReq(dummyInbound())
	ctx := context.Background()

	for i := 0; i < maxToolPerMinute-1; i++ {
		_, dec, err := cb.BeforeTool(ctx, req)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if dec.Action != HookActionContinue {
			t.Fatalf("call %d: expected continue, got %s", i, dec.Action)
		}
	}
}

func TestCircuitBreaker_ToolPerMinute_AtLimitAborts(t *testing.T) {
	cb, alertCount := makeTestCB(t)
	req := makeToolReq(dummyInbound())
	ctx := context.Background()

	for i := 0; i < maxToolPerMinute; i++ {
		_, _, _ = cb.BeforeTool(ctx, req)
	}
	_, dec, err := cb.BeforeTool(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != HookActionHardAbort {
		t.Fatalf("expected hard_abort, got %s", dec.Action)
	}
	time.Sleep(50 * time.Millisecond)
	if alertCount.Load() == 0 {
		t.Fatal("expected at least one alert on first trip")
	}
}

// --- Panic file: Activate blocks, Deactivate unblocks ---

func TestCircuitBreaker_PanicFile_BlocksAndUnblocks(t *testing.T) {
	cb, _ := makeTestCB(t)
	req := makeLLMReq(dummyInbound())
	ctx := context.Background()

	// Passes before activation.
	_, dec, _ := cb.BeforeLLM(ctx, req)
	if dec.Action != HookActionContinue {
		t.Fatalf("expected continue before activation, got %s", dec.Action)
	}

	cb.Activate()

	// Must abort while panic file exists.
	_, dec, _ = cb.BeforeLLM(ctx, req)
	if dec.Action != HookActionHardAbort {
		t.Fatalf("expected hard_abort while activated, got %s", dec.Action)
	}

	cb.Deactivate()

	// Must pass again after deactivation.
	_, dec, _ = cb.BeforeLLM(ctx, req)
	if dec.Action != HookActionContinue {
		t.Fatalf("expected continue after deactivation, got %s", dec.Action)
	}
}

func TestCircuitBreaker_PanicFile_BlocksBeforeTool(t *testing.T) {
	cb, _ := makeTestCB(t)
	req := makeToolReq(dummyInbound())
	ctx := context.Background()

	cb.Activate()

	_, dec, err := cb.BeforeTool(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != HookActionHardAbort {
		t.Fatalf("expected hard_abort, got %s", dec.Action)
	}
}

// --- Deactivate clears windows ---

func TestCircuitBreaker_Deactivate_ClearsWindows(t *testing.T) {
	cb, _ := makeTestCB(t)
	req := makeLLMReq(dummyInbound())
	ctx := context.Background()

	// Saturate the minute window.
	for i := 0; i < maxLLMPerMinute; i++ {
		_, _, _ = cb.BeforeLLM(ctx, req)
	}
	// Confirm aborted.
	_, dec, _ := cb.BeforeLLM(ctx, req)
	if dec.Action != HookActionHardAbort {
		t.Fatal("expected abort after saturation")
	}

	cb.Deactivate()

	// Windows cleared: should pass again up to limit.
	_, dec, _ = cb.BeforeLLM(ctx, req)
	if dec.Action != HookActionContinue {
		t.Fatalf("expected continue after Deactivate cleared windows, got %s", dec.Action)
	}
}

// --- Alert debounce: only first trip fires, subsequent trips do not ---

func TestCircuitBreaker_AlertDebounce_OnlyFirstTripAlerts(t *testing.T) {
	cb, alertCount := makeTestCB(t)
	req := makeLLMReq(dummyInbound())
	ctx := context.Background()

	// Saturate.
	for i := 0; i < maxLLMPerMinute; i++ {
		_, _, _ = cb.BeforeLLM(ctx, req)
	}
	// First over-limit call — should alert.
	_, _, _ = cb.BeforeLLM(ctx, req)
	time.Sleep(50 * time.Millisecond)
	firstCount := alertCount.Load()
	if firstCount == 0 {
		t.Fatal("expected alert on first trip")
	}

	// Second over-limit call — should NOT alert again (tripped).
	_, _, _ = cb.BeforeLLM(ctx, req)
	time.Sleep(50 * time.Millisecond)
	if alertCount.Load() != firstCount {
		t.Fatalf("expected no new alerts after first trip, got %d total", alertCount.Load())
	}
}

// --- tripped resets after a passing check (via Deactivate) ---

func TestCircuitBreaker_Tripped_ResetsAfterDeactivate(t *testing.T) {
	cb, alertCount := makeTestCB(t)
	req := makeLLMReq(dummyInbound())
	ctx := context.Background()

	// Trip it.
	for i := 0; i < maxLLMPerMinute+1; i++ {
		_, _, _ = cb.BeforeLLM(ctx, req)
	}
	time.Sleep(50 * time.Millisecond)
	afterFirst := alertCount.Load()

	// Reset.
	cb.Deactivate()

	// Re-saturate and trip again — should alert a second time.
	for i := 0; i < maxLLMPerMinute+1; i++ {
		_, _, _ = cb.BeforeLLM(ctx, req)
	}
	time.Sleep(50 * time.Millisecond)
	if alertCount.Load() <= afterFirst {
		t.Fatal("expected a second alert after reset and re-trip")
	}
}

// --- Panic file alert debounce ---

func TestCircuitBreaker_PanicAlert_Debounced(t *testing.T) {
	cb, alertCount := makeTestCB(t)
	req := makeLLMReq(dummyInbound())
	ctx := context.Background()

	cb.Activate()

	// First call: should alert.
	_, _, _ = cb.BeforeLLM(ctx, req)
	time.Sleep(50 * time.Millisecond)
	first := alertCount.Load()
	if first == 0 {
		t.Fatal("expected alert on first panic call")
	}

	// Subsequent calls while still panicked: no new alert.
	_, _, _ = cb.BeforeLLM(ctx, req)
	_, _, _ = cb.BeforeLLM(ctx, req)
	time.Sleep(50 * time.Millisecond)
	if alertCount.Load() != first {
		t.Fatalf("expected debounce, got %d alerts", alertCount.Load())
	}
}

// --- Nil-safety ---

func TestCircuitBreaker_NilRequest_DoesNotPanic(t *testing.T) {
	cb, _ := makeTestCB(t)
	ctx := context.Background()

	// nil req
	_, dec, err := cb.BeforeLLM(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != HookActionContinue {
		t.Fatalf("expected continue for nil req, got %s", dec.Action)
	}
}

func TestCircuitBreaker_NilTurnContext_DoesNotPanic(t *testing.T) {
	cb, _ := makeTestCB(t)
	ctx := context.Background()

	req := &LLMHookRequest{Context: nil}
	_, dec, err := cb.BeforeLLM(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Action != HookActionContinue {
		t.Fatalf("expected continue for nil context, got %s", dec.Action)
	}
}

// --- Deactivate tolerates missing panic file ---

func TestCircuitBreaker_Deactivate_ToleratesMissingFile(t *testing.T) {
	cb, _ := makeTestCB(t)
	// Ensure the file does not exist.
	_ = os.Remove(cb.panicFile)
	// Should not panic or error.
	cb.Deactivate()
}

// --- Panic file path is in the correct subdirectory ---

func TestCircuitBreaker_PanicFilePath(t *testing.T) {
	workspace := t.TempDir()
	cb := newCircuitBreaker(workspace, nil)
	expected := filepath.Join(workspace, "state", "panic")
	if cb.panicFile != expected {
		t.Fatalf("expected panicFile %q, got %q", expected, cb.panicFile)
	}
}
