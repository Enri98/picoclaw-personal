// PicoClaw - Ultra-lightweight personal AI agent
//
// End-to-end integration tests (hook wired into a live AgentLoop processing a
// full turn) are deferred to the orchestrator review. These unit tests cover the
// TurnLock data structure and the turnLockHook observer in isolation.

package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func init() {
	// Register synthetic names for testing so real tool names are not required.
	untrustedFetchTools["test_untrusted_fetch"] = true
	writableToolsLockedOnUntrustedFetch["test_writable"] = true
}

func TestTurnLock_NotLockedByDefault(t *testing.T) {
	l := NewTurnLock()
	if l.IsLocked("turn-1") {
		t.Fatal("expected turn-1 to not be locked by default")
	}
}

func TestTurnLock_MarkAndCheck(t *testing.T) {
	l := NewTurnLock()
	l.markLocked("turn-1")
	if !l.IsLocked("turn-1") {
		t.Fatal("expected turn-1 to be locked after markLocked")
	}
	if l.IsLocked("turn-2") {
		t.Fatal("expected turn-2 to remain unlocked")
	}
}

func TestTurnLock_ClearTurn(t *testing.T) {
	l := NewTurnLock()
	l.markLocked("turn-1")
	l.clearTurn("turn-1")
	if l.IsLocked("turn-1") {
		t.Fatal("expected turn-1 to be unlocked after clearTurn")
	}
}

func TestTurnLock_Apply_NotLocked_PassesThrough(t *testing.T) {
	l := NewTurnLock()
	tools := []providers.ToolDefinition{
		{Function: providers.ToolFunctionDefinition{Name: "test_writable"}},
		{Function: providers.ToolFunctionDefinition{Name: "safe_tool"}},
	}
	result := l.Apply(tools, "turn-unlocked")
	if len(result) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result))
	}
	// Same slice returned (not a copy) when not locked.
	if &result[0] != &tools[0] {
		t.Fatal("expected same slice to be returned when not locked")
	}
}

func TestTurnLock_Apply_Locked_StripsWritable(t *testing.T) {
	l := NewTurnLock()
	l.markLocked("turn-locked")
	tools := []providers.ToolDefinition{
		{Function: providers.ToolFunctionDefinition{Name: "test_writable"}},
		{Function: providers.ToolFunctionDefinition{Name: "safe_tool"}},
		{Function: providers.ToolFunctionDefinition{Name: "bash"}},
	}
	result := l.Apply(tools, "turn-locked")
	if len(result) != 1 {
		t.Fatalf("expected 1 tool after filtering, got %d", len(result))
	}
	if result[0].Function.Name != "safe_tool" {
		t.Fatalf("expected safe_tool, got %q", result[0].Function.Name)
	}
}

func TestTurnLock_Concurrent(t *testing.T) {
	l := NewTurnLock()
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			id := "turn-concurrent"
			l.markLocked(id)
			_ = l.IsLocked(id)
			l.clearTurn(id)
		}(i)
	}
	wg.Wait()
}

func TestTurnLockHook_AfterTool_RecordsLock(t *testing.T) {
	l := NewTurnLock()
	h := &turnLockHook{lock: l}

	result := &ToolResultHookResponse{
		Meta: HookMeta{TurnID: "turn-42"},
		Tool: "test_untrusted_fetch",
	}
	out, decision, err := h.AfterTool(context.Background(), result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision.Action != HookActionContinue {
		t.Fatalf("expected HookActionContinue, got %v", decision.Action)
	}
	if out != result {
		t.Fatal("expected same result pointer to be returned")
	}
	if !l.IsLocked("turn-42") {
		t.Fatal("expected turn-42 to be locked after untrusted fetch")
	}
}

func TestTurnLockHook_AfterTool_IgnoresNonUntrusted(t *testing.T) {
	l := NewTurnLock()
	h := &turnLockHook{lock: l}

	result := &ToolResultHookResponse{
		Meta: HookMeta{TurnID: "turn-99"},
		Tool: "safe_tool",
	}
	_, _, err := h.AfterTool(context.Background(), result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.IsLocked("turn-99") {
		t.Fatal("expected turn-99 to remain unlocked after non-untrusted tool")
	}
}

func TestTurnLockHook_AfterTool_IgnoresEmptyTurnID(t *testing.T) {
	l := NewTurnLock()
	h := &turnLockHook{lock: l}

	result := &ToolResultHookResponse{
		Meta: HookMeta{TurnID: ""},
		Tool: "test_untrusted_fetch",
	}
	_, _, err := h.AfterTool(context.Background(), result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No entry should have been written for the empty key.
	if l.IsLocked("") {
		t.Fatal("expected empty TurnID to not be locked")
	}
}

// When a grandchild sub-turn fires an untrusted fetch, the lock must propagate
// up the full ancestry chain so the root inbound turn cannot write either.
func TestTurnLock_DeepAncestry(t *testing.T) {
	l := NewTurnLock()
	l.RegisterChild("child", "root")
	l.RegisterChild("grandchild", "child")

	h := &turnLockHook{lock: l}
	result := &ToolResultHookResponse{
		Meta: HookMeta{TurnID: "grandchild", ParentTurnID: "child"},
		Tool: "test_untrusted_fetch",
	}
	if _, _, err := h.AfterTool(context.Background(), result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range []string{"grandchild", "child", "root"} {
		if !l.IsLocked(id) {
			t.Errorf("expected %s to be locked via ancestry walk", id)
		}
	}
}

// When an untrusted-fetch tool fires inside a sub-turn, the parent turn's next
// LLM iteration will see the fetched content in its context. Both the
// sub-turn and the parent must therefore be locked.
func TestTurnLockHook_AfterTool_LocksParentTurn(t *testing.T) {
	l := NewTurnLock()
	h := &turnLockHook{lock: l}

	result := &ToolResultHookResponse{
		Meta: HookMeta{TurnID: "child-1", ParentTurnID: "parent-1"},
		Tool: "test_untrusted_fetch",
	}
	if _, _, err := h.AfterTool(context.Background(), result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !l.IsLocked("child-1") {
		t.Fatal("expected child-1 to be locked")
	}
	if !l.IsLocked("parent-1") {
		t.Fatal("expected parent-1 to be locked (sub-turn fetch must propagate up)")
	}
}
