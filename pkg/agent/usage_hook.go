package agent

import (
	"context"
	"time"

	"github.com/sipeed/picoclaw/pkg/tools"
)

// UsageHook implements LLMInterceptor. AfterLLM records token usage for every
// Vertex AI response. BeforeLLM is a no-op.
type UsageHook struct {
	tracker *tools.UsageTracker
}

func newUsageHook(workspace string) *UsageHook {
	return &UsageHook{
		tracker: tools.NewUsageTracker(workspace),
	}
}

// BeforeLLM implements LLMInterceptor (no-op).
func (uh *UsageHook) BeforeLLM(ctx context.Context, req *LLMHookRequest) (*LLMHookRequest, HookDecision, error) {
	return req, HookDecision{Action: HookActionContinue}, nil
}

// AfterLLM implements LLMInterceptor. Extracts token counts from the response
// and delegates to UsageTracker.RecordUsage.
func (uh *UsageHook) AfterLLM(ctx context.Context, resp *LLMHookResponse) (*LLMHookResponse, HookDecision, error) {
	if resp == nil || resp.Response == nil || resp.Response.Usage == nil {
		return resp, HookDecision{Action: HookActionContinue}, nil
	}
	usage := resp.Response.Usage
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return resp, HookDecision{Action: HookActionContinue}, nil
	}
	uh.tracker.RecordUsage(resp.Model, usage.PromptTokens, usage.CompletionTokens)
	return resp, HookDecision{Action: HookActionContinue}, nil
}

// FormatReceipt delegates to the underlying tracker.
func (uh *UsageHook) FormatReceipt(date time.Time) string {
	return uh.tracker.FormatReceipt(date)
}
