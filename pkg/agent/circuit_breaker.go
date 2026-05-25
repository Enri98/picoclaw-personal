package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	maxLLMPerMinute  = 30
	maxLLMPerDay     = 1000
	maxToolPerMinute = 100
)

// CircuitBreaker implements LLMInterceptor and ToolInterceptor to enforce rate
// limits and support a manual kill switch via a panic flag file.
type CircuitBreaker struct {
	mu         sync.Mutex
	llmMinute  []time.Time
	llmDay     []time.Time
	toolMinute []time.Time
	tripped    bool
	panicFile  string
	alertFunc  func(ctx context.Context, inbound *bus.InboundContext, msg string)
}

func newCircuitBreaker(workspace string, alertFn func(context.Context, *bus.InboundContext, string)) *CircuitBreaker {
	return &CircuitBreaker{
		panicFile: filepath.Join(workspace, "state", "panic"),
		alertFunc: alertFn,
	}
}

// Activate creates the panic flag file, halting all LLM and tool processing.
func (cb *CircuitBreaker) Activate() {
	dir := filepath.Dir(cb.panicFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.ErrorCF("circuit-breaker", "Failed to create state directory", map[string]any{
			"error": err.Error(),
			"dir":   dir,
		})
		return
	}
	if err := os.WriteFile(cb.panicFile, []byte("panic"), 0o644); err != nil {
		logger.ErrorCF("circuit-breaker", "Failed to write panic file", map[string]any{
			"error": err.Error(),
			"path":  cb.panicFile,
		})
		return
	}
	logger.ErrorCF("circuit-breaker", "Panic mode activated", map[string]any{
		"path": cb.panicFile,
	})
}

// Deactivate removes the panic flag file and resets all rate-limit windows.
func (cb *CircuitBreaker) Deactivate() {
	if err := os.Remove(cb.panicFile); err != nil && !os.IsNotExist(err) {
		logger.WarnCF("circuit-breaker", "Failed to remove panic file", map[string]any{
			"error": err.Error(),
			"path":  cb.panicFile,
		})
	}
	cb.mu.Lock()
	cb.llmMinute = nil
	cb.llmDay = nil
	cb.toolMinute = nil
	cb.tripped = false
	cb.mu.Unlock()
	logger.InfoCF("circuit-breaker", "Circuit breaker deactivated; windows cleared", nil)
}

// sendAlert fires an async alert via alertFunc with a 2-second timeout.
// It is a no-op if alertFunc or inbound is nil.
func (cb *CircuitBreaker) sendAlert(msg string, inbound *bus.InboundContext) {
	if cb.alertFunc == nil || inbound == nil {
		return
	}
	inboundCopy := *inbound
	fn := cb.alertFunc
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		fn(ctx, &inboundCopy, msg)
	}()
}

// pruneBefore returns a slice with all entries before cutoff removed.
func pruneBefore(window []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(window) && window[i].Before(cutoff) {
		i++
	}
	return window[i:]
}

// checkAndRecordLLM checks and records an LLM call under the rate limits.
// Must be called without holding mu; acquires it internally.
func (cb *CircuitBreaker) checkAndRecordLLM(now time.Time) (ok bool, reason string, shouldAlert bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.llmMinute = pruneBefore(cb.llmMinute, now.Add(-time.Minute))
	cb.llmDay = pruneBefore(cb.llmDay, now.Add(-24*time.Hour))
	if len(cb.llmMinute) >= maxLLMPerMinute {
		shouldAlert = !cb.tripped
		cb.tripped = true
		return false, fmt.Sprintf("LLM per-minute limit (%d) reached", maxLLMPerMinute), shouldAlert
	}
	if len(cb.llmDay) >= maxLLMPerDay {
		shouldAlert = !cb.tripped
		cb.tripped = true
		return false, fmt.Sprintf("LLM daily limit (%d) reached", maxLLMPerDay), shouldAlert
	}
	cb.llmMinute = append(cb.llmMinute, now)
	cb.llmDay = append(cb.llmDay, now)
	cb.tripped = false
	return true, "", false
}

// checkAndRecordTool checks and records a tool call under the rate limit.
func (cb *CircuitBreaker) checkAndRecordTool(now time.Time) (ok bool, reason string, shouldAlert bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.toolMinute = pruneBefore(cb.toolMinute, now.Add(-time.Minute))
	if len(cb.toolMinute) >= maxToolPerMinute {
		shouldAlert = !cb.tripped
		cb.tripped = true
		return false, fmt.Sprintf("tool per-minute limit (%d) reached", maxToolPerMinute), shouldAlert
	}
	cb.toolMinute = append(cb.toolMinute, now)
	cb.tripped = false
	return true, "", false
}

// BeforeLLM implements LLMInterceptor. It blocks calls when the panic file
// exists or when a rate limit is reached.
func (cb *CircuitBreaker) BeforeLLM(ctx context.Context, req *LLMHookRequest) (*LLMHookRequest, HookDecision, error) {
	var inbound *bus.InboundContext
	if req != nil && req.Context != nil {
		inbound = req.Context.Inbound
	}

	// Check panic file under mutex so tripped debounce is consistent.
	if _, err := os.Stat(cb.panicFile); err == nil {
		cb.mu.Lock()
		shouldAlert := !cb.tripped
		cb.tripped = true
		cb.mu.Unlock()
		if shouldAlert {
			cb.sendAlert("Panic mode active. Use /resume to re-enable.", inbound)
		}
		return req, HookDecision{Action: HookActionHardAbort}, nil
	}

	ok, reason, shouldAlert := cb.checkAndRecordLLM(time.Now())
	if !ok {
		if shouldAlert {
			cb.sendAlert("Rate limit hit: "+reason+". Use /resume to reset.", inbound)
		}
		return req, HookDecision{Action: HookActionHardAbort}, nil
	}

	return req, HookDecision{Action: HookActionContinue}, nil
}

// AfterLLM implements LLMInterceptor (no-op).
func (cb *CircuitBreaker) AfterLLM(ctx context.Context, resp *LLMHookResponse) (*LLMHookResponse, HookDecision, error) {
	return resp, HookDecision{Action: HookActionContinue}, nil
}

// BeforeTool implements ToolInterceptor. It blocks calls when the panic file
// exists or when the tool rate limit is reached.
func (cb *CircuitBreaker) BeforeTool(ctx context.Context, call *ToolCallHookRequest) (*ToolCallHookRequest, HookDecision, error) {
	var inbound *bus.InboundContext
	if call != nil && call.Context != nil {
		inbound = call.Context.Inbound
	}

	if _, err := os.Stat(cb.panicFile); err == nil {
		cb.mu.Lock()
		shouldAlert := !cb.tripped
		cb.tripped = true
		cb.mu.Unlock()
		if shouldAlert {
			cb.sendAlert("Panic mode active. Use /resume to re-enable.", inbound)
		}
		return call, HookDecision{Action: HookActionHardAbort}, nil
	}

	ok, reason, shouldAlert := cb.checkAndRecordTool(time.Now())
	if !ok {
		if shouldAlert {
			cb.sendAlert("Rate limit hit: "+reason+". Use /resume to reset.", inbound)
		}
		return call, HookDecision{Action: HookActionHardAbort}, nil
	}

	return call, HookDecision{Action: HookActionContinue}, nil
}

// AfterTool implements ToolInterceptor (no-op).
func (cb *CircuitBreaker) AfterTool(ctx context.Context, result *ToolResultHookResponse) (*ToolResultHookResponse, HookDecision, error) {
	return result, HookDecision{Action: HookActionContinue}, nil
}
