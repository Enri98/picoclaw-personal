// Package modelrouter registers a BeforeLLM hook that selects Gemini Flash or
// Pro before any LLM call, based on first-match rules applied to the last
// user-role message in the conversation.
//
// Rules (first match wins):
//  1. Content starts with "/pro " or "/think " (case-insensitive) → Pro; prefix stripped.
//  2. Content contains "[voice]" → Pro (voice notes routed to Pro even before
//     transcription tooling is wired in §5).
//  3. Content length > 4000 bytes → Pro.
//  4. Default → Flash (unchanged).
//
// The hook is registered under the name "model-router" and must be enabled in
// picoclaw-config.json:
//
//	"hooks": {
//	  "enabled": true,
//	  "builtins": { "model-router": { "enabled": true } }
//	}
package modelrouter

import (
	"context"
	"log/slog"
	"strings"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

const (
	hookName    = "model-router"
	modelFlash  = "vertex-gemini-flash"
	modelPro    = "vertex-gemini-pro"
	voiceMarker = "[voice]"
	longMsgLen  = 4000
)

func init() {
	if err := agent.RegisterBuiltinHook(hookName, newModelRouter); err != nil {
		// Only panics if the hook name is already registered (programming error).
		panic("modelrouter: " + err.Error())
	}
}

// modelRouter implements agent.LLMInterceptor.
type modelRouter struct{}

func newModelRouter(_ context.Context, _ config.BuiltinHookConfig) (any, error) {
	return &modelRouter{}, nil
}

// BeforeLLM inspects the last user message and rewrites the model name when needed.
func (r *modelRouter) BeforeLLM(_ context.Context, req *agent.LLMHookRequest) (*agent.LLMHookRequest, agent.HookDecision, error) {
	content, idx := lastUserContent(req.Messages)
	rule, targetModel, newContent := route(content)

	if targetModel == "" || targetModel == req.Model {
		slog.Debug("model-router: no change", "model", req.Model, "rule", rule)
		return req, agent.HookDecision{Action: agent.HookActionContinue}, nil
	}

	slog.Info("model-router: routing",
		"rule", rule,
		"from", req.Model,
		"to", targetModel,
	)

	req.Model = targetModel
	if newContent != content && idx >= 0 {
		msgs := make([]providers.Message, len(req.Messages))
		copy(msgs, req.Messages)
		msgs[idx].Content = newContent
		req.Messages = msgs
	}

	return req, agent.HookDecision{Action: agent.HookActionModify}, nil
}

// AfterLLM is a no-op; required by the LLMInterceptor interface.
func (r *modelRouter) AfterLLM(_ context.Context, resp *agent.LLMHookResponse) (*agent.LLMHookResponse, agent.HookDecision, error) {
	return resp, agent.HookDecision{Action: agent.HookActionContinue}, nil
}

// route returns the routing rule name, the target model name, and the
// (possibly prefix-stripped) content. An empty targetModel means "no change".
func route(content string) (rule, targetModel, newContent string) {
	lower := strings.ToLower(content)

	for _, prefix := range []string{"/pro ", "/think "} {
		if strings.HasPrefix(lower, prefix) {
			stripped := strings.TrimSpace(content[len(prefix):])
			return prefix[:len(prefix)-1], modelPro, stripped
		}
	}
	// Also handle the bare "/pro" or "/think" with no trailing text.
	for _, bare := range []string{"/pro", "/think"} {
		if strings.EqualFold(strings.TrimSpace(content), bare) {
			return bare, modelPro, content
		}
	}

	if strings.Contains(content, voiceMarker) {
		return "voice", modelPro, content
	}

	if len(content) > longMsgLen {
		return "long-message", modelPro, content
	}

	return "default", "", content
}

// lastUserContent returns the content and slice index of the last user-role
// message, or ("", -1) when none is found.
func lastUserContent(msgs []providers.Message) (string, int) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content, i
		}
	}
	return "", -1
}
