// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"sync"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// untrustedFetchTools is the set of tool names whose results must not be trusted
// as safe content. When any of these fires during a turn, the turn is locked and
// all writable tools are stripped from subsequent LLM calls for that turn.
var untrustedFetchTools = map[string]bool{
	"gmail_get_body": true,
	// "outlook_get_body":       true,  // chunk 8
	// "github_get_issue_body": true,  // chunk 10
	// "link_fetch":            true,  // chunk 11
}

// writableToolsLockedOnUntrustedFetch is the set of tools that must be stripped
// from the LLM's tool list once a turn has been locked by an untrusted fetch.
var writableToolsLockedOnUntrustedFetch = map[string]bool{
	"bash":               true,
	"wiki_propose_write": true,
	// "gcal_create_event_proposal":    true,  // chunk 9
	// "github_create_issue_proposal":  true,  // chunk 10
}

// TurnLock tracks which turns have fetched untrusted content and therefore must
// have writable tools stripped for the remainder of the turn.
type TurnLock struct {
	mu     sync.RWMutex
	locked map[string]struct{}
}

// NewTurnLock returns a ready-to-use TurnLock.
func NewTurnLock() *TurnLock {
	return &TurnLock{locked: make(map[string]struct{})}
}

func (l *TurnLock) markLocked(turnID string) {
	l.mu.Lock()
	l.locked[turnID] = struct{}{}
	l.mu.Unlock()
}

// IsLocked reports whether the turn with the given ID has been locked.
func (l *TurnLock) IsLocked(turnID string) bool {
	l.mu.RLock()
	_, ok := l.locked[turnID]
	l.mu.RUnlock()
	return ok
}

func (l *TurnLock) clearTurn(turnID string) {
	l.mu.Lock()
	delete(l.locked, turnID)
	l.mu.Unlock()
}

// Apply returns tools unchanged when the turn is not locked. When the turn is
// locked it returns a filtered copy with writable tools removed.
func (l *TurnLock) Apply(tools []providers.ToolDefinition, turnID string) []providers.ToolDefinition {
	if !l.IsLocked(turnID) {
		return tools
	}
	filtered := make([]providers.ToolDefinition, 0, len(tools))
	for _, td := range tools {
		if !writableToolsLockedOnUntrustedFetch[td.Function.Name] {
			filtered = append(filtered, td)
		}
	}
	return filtered
}

// turnLockHook is an in-process ToolInterceptor that records turns in which an
// untrusted-fetch tool fired. It is a pure observer: BeforeTool is a no-op and
// AfterTool only writes to the TurnLock state.
type turnLockHook struct {
	lock *TurnLock
}

func (h *turnLockHook) BeforeTool(
	ctx context.Context,
	call *ToolCallHookRequest,
) (*ToolCallHookRequest, HookDecision, error) {
	return call, HookDecision{Action: HookActionContinue}, nil
}

func (h *turnLockHook) AfterTool(
	ctx context.Context,
	result *ToolResultHookResponse,
) (*ToolResultHookResponse, HookDecision, error) {
	if result == nil || result.Meta.TurnID == "" {
		return result, HookDecision{Action: HookActionContinue}, nil
	}
	if untrustedFetchTools[result.Tool] {
		h.lock.markLocked(result.Meta.TurnID)
	}
	return result, HookDecision{Action: HookActionContinue}, nil
}
