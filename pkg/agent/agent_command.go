// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/commands"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func (al *AgentLoop) handleCommand(
	ctx context.Context,
	msg bus.InboundMessage,
	agent *AgentInstance,
	opts *processOptions,
) (string, bool) {
	normalizeProcessOptionsInPlace(opts)

	if !commands.HasCommandPrefix(msg.Content) {
		return "", false
	}

	if matched, handled, reply := al.applyExplicitSkillCommand(msg.Content, agent, opts); matched {
		return reply, handled
	}

	if cmd, ok := commands.CommandName(msg.Content); ok {
		switch cmd {
		case "panic":
			if al.circuitBreaker != nil {
				al.circuitBreaker.Activate()
			}
			return "Panic mode activated. All LLM/tool processing halted. Use /resume to re-enable.", true
		case "resume":
			if al.circuitBreaker != nil {
				al.circuitBreaker.Deactivate()
			}
			return "Circuit breaker reset. Processing re-enabled.", true
		case "receipt":
			if al.usageHook == nil {
				return "Usage tracking not initialized.", true
			}
			date := time.Now().UTC()
			parts := strings.Fields(strings.TrimSpace(msg.Content))
			if len(parts) >= 2 {
				if t, err := time.Parse("2006-01-02", parts[1]); err == nil {
					date = t.UTC()
				} else {
					return fmt.Sprintf("Invalid date %q — use YYYY-MM-DD.", parts[1]), true
				}
			}
			return al.usageHook.FormatReceipt(date), true
		case "note":
			if al.wikiToolset == nil {
				return "Wiki not configured.", true
			}
			parts := strings.SplitN(strings.TrimSpace(msg.Content), " ", 2)
			if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
				return "Usage: /note <text>", true
			}
			reply, err := al.wikiToolset.AppendToInbox(parts[1], "telegram")
			if err != nil {
				return fmt.Sprintf("Note failed: %s", err.Error()), true
			}
			return reply, true
		case "wiki":
			if al.wikiToolset == nil {
				return "Wiki not configured.", true
			}
			parts := strings.Fields(strings.TrimSpace(msg.Content))
			if len(parts) < 2 {
				return "Usage: /wiki <subcommand>  (subcommands: proposals)", true
			}
			switch strings.ToLower(parts[1]) {
			case "proposals":
				return al.wikiToolset.ListProposals(), true
			default:
				return fmt.Sprintf("Unknown wiki subcommand: %s", parts[1]), true
			}
		case "agenda":
			if al.gcalToolset == nil {
				return "Calendar not configured.", true
			}
			if al.circuitBreaker != nil {
				if ok, reason := al.circuitBreaker.CheckTool(); !ok {
					return fmt.Sprintf("Blocked: %s", reason), true
				}
			}
			events, err := al.gcalToolset.TodayDirect(ctx)
			if err != nil {
				return fmt.Sprintf("Agenda failed: %s", err.Error()), true
			}
			if len(events) == 0 {
				return "No events today.", true
			}
			var sb strings.Builder
			sb.WriteString("Today:\n")
			for _, e := range events {
				fmt.Fprintf(&sb, "  %s — %s\n", e.Start.Format("15:04"), e.Title)
			}
			return sb.String(), true
		case "apply":
			parts := strings.Fields(strings.TrimSpace(msg.Content))
			if len(parts) < 2 {
				return "Usage: /apply <proposal-id>", true
			}
			return al.applyProposal(ctx, parts[1]), true
		case "reject":
			parts := strings.Fields(strings.TrimSpace(msg.Content))
			if len(parts) < 2 {
				return "Usage: /reject <proposal-id>", true
			}
			return al.rejectProposal(parts[1]), true
		case "gh":
			if al.githubToolset == nil {
				return "GitHub integration not configured.", true
			}
			opts.ForcedSkills = append(opts.ForcedSkills, "github-monitor")
			rewritten := "Summarize the current state across all my watched GitHub repositories. " +
				"Start by calling github_watched_repos. Then for each repo, call github_open_issues, " +
				"github_open_prs, and (if there are recent pushes) github_ci_status on the default branch. " +
				"Group the output by repository. Be concise. Respond in the user's language."
			opts.Dispatch.UserMessage = rewritten
			opts.UserMessage = rewritten
			return "", false
		case "claude":
			if al.githubToolset == nil {
				return "GitHub integration not configured.", true
			}
			if al.githubPoller == nil {
				return "GitHub response poller not running.", true
			}
			repo, question, perr := parseClaudeCommand(msg.Content)
			if perr != "" {
				return perr, true
			}
			fullRepo, err := al.githubToolset.ResolveRepo(repo)
			if err != nil {
				return fmt.Sprintf("Unknown repo %q: %s", repo, err.Error()), true
			}
			owner, name, ok := splitOwnerRepo(fullRepo)
			if !ok {
				return fmt.Sprintf("Malformed repo %q (expected owner/repo).", fullRepo), true
			}
			title := truncateForTitle(question, 80)
			body := fmt.Sprintf("@claude\n\n%s\n\n---\nInquiry forwarded from PicoBot.", question)
			issueNum, issueURL, err := al.githubToolset.CreateIssue(ctx, fullRepo, title, body)
			if err != nil {
				return fmt.Sprintf("Failed to create issue: %s", err.Error()), true
			}
			watchID, regErr := newWatchID()
			if regErr != nil {
				logger.WarnCF("agent", "failed to mint watch ID; using fallback", map[string]any{"error": regErr.Error()})
				watchID = fmt.Sprintf("watch-%d", time.Now().UnixNano())
			}
			if err := al.githubPoller.Register(PollEntry{
				ID:          watchID,
				Owner:       owner,
				Repo:        name,
				IssueNumber: issueNum,
				CreatedAt:   time.Now().UTC(),
				TTL:         24 * time.Hour,
				ChatID:      msg.ChatID,
			}); err != nil {
				return fmt.Sprintf("Issue #%d created at %s but failed to register watch: %s", issueNum, issueURL, err.Error()), true
			}
			return fmt.Sprintf("Created issue #%d: %s\nWatching for a response (24h timeout).", issueNum, issueURL), true
		case "sh":
			if al.bashTool == nil {
				return "Bash tool not configured.", true
			}
			parts := strings.SplitN(strings.TrimSpace(msg.Content), " ", 2)
			if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
				return "Usage: /sh <command>", true
			}
			if al.circuitBreaker != nil {
				if ok, reason := al.circuitBreaker.CheckTool(); !ok {
					return fmt.Sprintf("Blocked: %s", reason), true
				}
			}
			result := al.bashTool.Execute(ctx, map[string]any{"cmd": parts[1]})
			if result == nil {
				return "Bash tool returned no result.", true
			}
			if result.ForUser != "" {
				return result.ForUser, true
			}
			return result.ForLLM, true
		}
	}

	if al.cmdRegistry == nil {
		return "", false
	}

	rt := al.buildCommandsRuntime(ctx, agent, opts)
	executor := commands.NewExecutor(al.cmdRegistry, rt)

	var commandReply string
	result := executor.Execute(ctx, commands.Request{
		Channel:  msg.Channel,
		ChatID:   msg.ChatID,
		SenderID: msg.SenderID,
		Text:     msg.Content,
		Reply: func(text string) error {
			commandReply = text
			return nil
		},
	})

	switch result.Outcome {
	case commands.OutcomeHandled:
		if result.Err != nil {
			return mapCommandError(result), true
		}
		if commandReply != "" {
			return commandReply, true
		}
		return "", true
	default: // OutcomePassthrough — let the message fall through to LLM
		return "", false
	}
}

// applyProposal walks the configured proposal stores (wiki → bash → gcal) and
// applies whichever one holds the given UUIDv4. The stores share a flat
// keyspace so collisions are astronomically unlikely. A load/parse error in
// an earlier store (e.g. a truncated JSON file) is logged but does NOT
// short-circuit the chain — the user's proposal in a later store would
// otherwise become permanently inapplicable until the corrupt store was
// fixed manually.
func (al *AgentLoop) applyProposal(ctx context.Context, id string) string {
	var lastNonFatal string
	if al.wikiToolset != nil {
		reply, err := al.wikiToolset.ApplyProposal(id)
		if err == nil {
			return reply
		}
		if isProposalNotFound(err) {
			// Empty store; try next.
		} else if isStoreLoadError(err) {
			logger.WarnCF("agent", "wiki proposal store error during /apply; trying next store", map[string]any{"error": err.Error()})
		} else {
			return fmt.Sprintf("Apply failed: %s", err.Error())
		}
	}
	if al.bashTool != nil {
		if al.circuitBreaker != nil {
			if ok, reason := al.circuitBreaker.CheckTool(); !ok {
				return fmt.Sprintf("Blocked: %s", reason)
			}
		}
		result, err := al.bashTool.Proposals().Apply(ctx, id)
		if err == nil {
			return formatRunResultForUser(result)
		}
		if isProposalNotFound(err) {
			// try next
		} else if isStoreLoadError(err) {
			logger.WarnCF("agent", "bash proposal store error during /apply; trying next store", map[string]any{"error": err.Error()})
		} else {
			lastNonFatal = err.Error()
		}
	}
	if al.gcalToolset != nil {
		if al.circuitBreaker != nil {
			if ok, reason := al.circuitBreaker.CheckTool(); !ok {
				return fmt.Sprintf("Blocked: %s", reason)
			}
		}
		ev, err := al.gcalToolset.Proposals().Apply(ctx, id)
		if err == nil {
			return fmt.Sprintf("Created event: %s at %s", ev.Title, ev.Start.Format(time.RFC1123))
		}
		if isProposalNotFound(err) {
			// fall through
		} else if isStoreLoadError(err) {
			logger.WarnCF("agent", "gcal proposal store error during /apply; trying next store", map[string]any{"error": err.Error()})
			lastNonFatal = err.Error()
		} else {
			return fmt.Sprintf("Apply failed: %s", err.Error())
		}
	}
	if al.githubToolset != nil {
		if al.circuitBreaker != nil {
			if ok, reason := al.circuitBreaker.CheckTool(); !ok {
				return fmt.Sprintf("Blocked: %s", reason)
			}
		}
		issueNum, issueURL, err := al.githubToolset.Proposals().Apply(ctx, id)
		if err == nil {
			return fmt.Sprintf("Created issue #%d: %s", issueNum, issueURL)
		}
		if isProposalNotFound(err) {
			// fall through to final message
		} else if isStoreLoadError(err) {
			logger.WarnCF("agent", "github proposal store error during /apply", map[string]any{"error": err.Error()})
			lastNonFatal = err.Error()
		} else {
			return fmt.Sprintf("Apply failed: %s", err.Error())
		}
	}
	if al.wikiToolset == nil && al.bashTool == nil && al.gcalToolset == nil && al.githubToolset == nil {
		return "No proposal stores configured."
	}
	if lastNonFatal != "" {
		return fmt.Sprintf("No active proposal with ID %s (also: %s)", id, lastNonFatal)
	}
	return fmt.Sprintf("No active proposal with ID %s", id)
}

// isProposalNotFound matches the canonical "no active proposal with ID" error
// used by every proposal store. Any other error is either a load/parse error
// in the store backing file or a real apply-time failure.
func isProposalNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no active proposal")
}

// isStoreLoadError matches errors raised by load/save of the on-disk JSON
// backing file (typically truncated or non-UTF8 corruption). These should not
// short-circuit the apply chain because a later store might still hold the ID.
func isStoreLoadError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to load proposals") ||
		strings.Contains(msg, "failed to remove proposal") ||
		strings.Contains(msg, "failed to update proposals")
}

func (al *AgentLoop) rejectProposal(id string) string {
	if al.wikiToolset != nil {
		reply, err := al.wikiToolset.RejectProposal(id)
		if err == nil {
			return reply
		}
		if isStoreLoadError(err) {
			logger.WarnCF("agent", "wiki proposal store error during /reject; trying next store", map[string]any{"error": err.Error()})
		} else if !isProposalNotFound(err) {
			return fmt.Sprintf("Reject failed: %s", err.Error())
		}
	}
	if al.bashTool != nil {
		if err := al.bashTool.Proposals().Reject(id); err == nil {
			return fmt.Sprintf("Rejected proposal %s.", id)
		} else if isStoreLoadError(err) {
			logger.WarnCF("agent", "bash proposal store error during /reject; trying next store", map[string]any{"error": err.Error()})
		} else if !isProposalNotFound(err) {
			return fmt.Sprintf("Reject failed: %s", err.Error())
		}
	}
	if al.gcalToolset != nil {
		if err := al.gcalToolset.Proposals().Reject(id); err == nil {
			return fmt.Sprintf("Rejected proposal %s.", id)
		} else if isStoreLoadError(err) {
			logger.WarnCF("agent", "gcal proposal store error during /reject; trying next store", map[string]any{"error": err.Error()})
		} else if !isProposalNotFound(err) {
			return fmt.Sprintf("Reject failed: %s", err.Error())
		}
	}
	if al.githubToolset != nil {
		if err := al.githubToolset.Proposals().Reject(id); err == nil {
			return fmt.Sprintf("Rejected proposal %s.", id)
		} else if isStoreLoadError(err) {
			logger.WarnCF("agent", "github proposal store error during /reject", map[string]any{"error": err.Error()})
		} else if !isProposalNotFound(err) {
			return fmt.Sprintf("Reject failed: %s", err.Error())
		}
	}
	if al.wikiToolset == nil && al.bashTool == nil && al.gcalToolset == nil && al.githubToolset == nil {
		return "No proposal stores configured."
	}
	return fmt.Sprintf("No active proposal with ID %s", id)
}

// parseClaudeCommand parses `/claude <repo> <question>` (the question may be
// wrapped in matching quotes). Returns the repo token, the question, or a
// user-facing error string. An empty error string means the parse succeeded.
func parseClaudeCommand(raw string) (repo string, question string, errMsg string) {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) < 3 {
		return "", "", `Usage: /claude <repo> "question"`
	}
	repo = parts[1]
	rest := strings.TrimSpace(strings.Join(parts[2:], " "))
	if len(rest) >= 2 {
		first, last := rest[0], rest[len(rest)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			rest = strings.TrimSpace(rest[1 : len(rest)-1])
		}
	}
	if rest == "" {
		return "", "", `Usage: /claude <repo> "question"`
	}
	return repo, rest, ""
}

// splitOwnerRepo splits "owner/repo" into its two components.
func splitOwnerRepo(full string) (owner string, name string, ok bool) {
	parts := strings.SplitN(full, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// truncateForTitle clips the input to maxLen runes for use as an issue title.
// If clipped, an ellipsis is appended.
func truncateForTitle(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-1]) + "…"
}

// newWatchID mints a UUID v4 string used as a poll-entry identifier.
func newWatchID() (string, error) {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func formatRunResultForUser(r tools.RunResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit_code: %d\n", r.ExitCode)
	if r.Stdout != "" {
		fmt.Fprintf(&sb, "stdout:\n%s\n", r.Stdout)
	}
	if r.Stderr != "" {
		fmt.Fprintf(&sb, "stderr:\n%s\n", r.Stderr)
	}
	if r.TimedOut {
		sb.WriteString("[WARNING: command timed out]\n")
	}
	if r.Truncated {
		sb.WriteString("[WARNING: output was truncated]\n")
	}
	return sb.String()
}

func (al *AgentLoop) applyExplicitSkillCommand(
	raw string,
	agent *AgentInstance,
	opts *processOptions,
) (matched bool, handled bool, reply string) {
	normalizeProcessOptionsInPlace(opts)

	cmdName, ok := commands.CommandName(raw)
	if !ok || cmdName != "use" {
		return false, false, ""
	}

	if agent == nil || agent.ContextBuilder == nil {
		return true, true, commandsUnavailableSkillMessage()
	}

	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) < 2 {
		return true, true, buildUseCommandHelp(agent)
	}

	arg := strings.TrimSpace(parts[1])
	if strings.EqualFold(arg, "clear") || strings.EqualFold(arg, "off") {
		if opts != nil {
			al.clearPendingSkills(opts.Dispatch.SessionKey)
		}
		return true, true, "Cleared pending skill override."
	}

	skillName, ok := agent.ContextBuilder.ResolveSkillName(arg)
	if !ok {
		return true, true, fmt.Sprintf("Unknown skill: %s\nUse /list skills to see installed skills.", arg)
	}

	if len(parts) < 3 {
		if opts == nil || strings.TrimSpace(opts.Dispatch.SessionKey) == "" {
			return true, true, commandsUnavailableSkillMessage()
		}
		al.setPendingSkills(opts.Dispatch.SessionKey, []string{skillName})
		return true, true, fmt.Sprintf(
			"Skill %q is armed for your next message. Send your next prompt normally, or use /use clear to cancel.",
			skillName,
		)
	}

	message := strings.TrimSpace(strings.Join(parts[2:], " "))
	if message == "" {
		return true, true, buildUseCommandHelp(agent)
	}

	if opts != nil {
		opts.ForcedSkills = append(opts.ForcedSkills, skillName)
		opts.Dispatch.UserMessage = message
		opts.UserMessage = message
	}

	return true, false, ""
}

func (al *AgentLoop) buildCommandsRuntime(
	ctx context.Context,
	agent *AgentInstance,
	opts *processOptions,
) *commands.Runtime {
	normalizeProcessOptionsInPlace(opts)

	registry := al.GetRegistry()
	cfg := al.GetConfig()
	rt := &commands.Runtime{
		Config:          cfg,
		ListAgentIDs:    registry.ListAgentIDs,
		ListDefinitions: al.cmdRegistry.Definitions,
		ListMCPServers: func(ctx context.Context) []commands.MCPServerInfo {
			if cfg == nil {
				return nil
			}

			if len(cfg.Tools.MCP.Servers) == 0 {
				return nil
			}

			if err := al.ensureMCPInitialized(ctx); err != nil {
				logger.WarnCF("agent", "Failed to refresh MCP status for command",
					map[string]any{
						"error": err.Error(),
					})
			}

			connected := make(map[string]int)
			if manager := al.mcp.getManager(); manager != nil {
				for serverName, conn := range manager.GetServers() {
					connected[serverName] = len(conn.Tools)
				}
			}

			servers := make([]commands.MCPServerInfo, 0, len(cfg.Tools.MCP.Servers))
			for serverName, serverCfg := range cfg.Tools.MCP.Servers {
				toolCount, isConnected := connected[serverName]
				servers = append(servers, commands.MCPServerInfo{
					Name:      serverName,
					Enabled:   serverCfg.Enabled,
					Deferred:  serverIsDeferred(cfg.Tools.MCP.Discovery.Enabled, serverCfg),
					Connected: isConnected,
					ToolCount: toolCount,
				})
			}

			sort.Slice(servers, func(i, j int) bool {
				return strings.ToLower(servers[i].Name) < strings.ToLower(servers[j].Name)
			})

			return servers
		},
		ListMCPTools: func(ctx context.Context, serverName string) ([]commands.MCPToolInfo, error) {
			if cfg == nil {
				return nil, fmt.Errorf("command unavailable: config not loaded")
			}

			serverName = strings.TrimSpace(serverName)
			if serverName == "" {
				return nil, fmt.Errorf("server name is required")
			}

			resolvedName := ""
			var serverCfg config.MCPServerConfig
			for name, candidate := range cfg.Tools.MCP.Servers {
				if strings.EqualFold(name, serverName) {
					resolvedName = name
					serverCfg = candidate
					break
				}
			}
			if resolvedName == "" {
				return nil, fmt.Errorf("MCP server '%s' is not configured", serverName)
			}
			if !serverCfg.Enabled {
				return nil, fmt.Errorf("MCP server '%s' is configured but disabled", resolvedName)
			}
			if !cfg.Tools.IsToolEnabled("mcp") {
				return nil, fmt.Errorf("MCP integration is disabled")
			}

			if err := al.ensureMCPInitialized(ctx); err != nil {
				logger.WarnCF("agent", "Failed to initialize MCP runtime for command",
					map[string]any{
						"server": resolvedName,
						"error":  err.Error(),
					})
			}

			manager := al.mcp.getManager()
			if manager == nil {
				return nil, fmt.Errorf("MCP server '%s' is configured but not connected", resolvedName)
			}

			conn, ok := manager.GetServer(resolvedName)
			if !ok {
				return nil, fmt.Errorf("MCP server '%s' is configured but not connected", resolvedName)
			}

			toolInfos := make([]commands.MCPToolInfo, 0, len(conn.Tools))
			for _, tool := range conn.Tools {
				if tool == nil {
					continue
				}
				name := strings.TrimSpace(tool.Name)
				if name == "" {
					continue
				}

				description := strings.TrimSpace(tool.Description)
				if description == "" {
					description = fmt.Sprintf("MCP tool from %s server", resolvedName)
				}

				toolInfos = append(toolInfos, commands.MCPToolInfo{
					Name:        name,
					Description: description,
					Parameters:  summarizeMCPToolParameters(tool.InputSchema),
				})
			}
			sort.Slice(toolInfos, func(i, j int) bool {
				return toolInfos[i].Name < toolInfos[j].Name
			})
			return toolInfos, nil
		},
		GetEnabledChannels: func() []string {
			if al.channelManager == nil {
				return nil
			}
			return al.channelManager.GetEnabledChannels()
		},
		GetActiveTurn: func() any {
			info := al.GetActiveTurn()
			if info == nil {
				return nil
			}
			return info
		},
		SwitchChannel: func(value string) error {
			if al.channelManager == nil {
				return fmt.Errorf("channel manager not initialized")
			}
			if _, exists := al.channelManager.GetChannel(value); !exists && value != "cli" {
				return fmt.Errorf("channel '%s' not found or not enabled", value)
			}
			return nil
		},
	}
	rt.StopActiveTurn = func() (commands.StopResult, error) {
		if opts == nil {
			return commands.StopResult{}, fmt.Errorf("process options not available")
		}
		return al.stopActiveTurnForSession(opts.Dispatch.SessionKey)
	}
	if agent != nil && agent.ContextBuilder != nil {
		rt.ListSkillNames = agent.ContextBuilder.ListSkillNames
	}
	rt.ReloadConfig = func() error {
		if al.reloadFunc == nil {
			return fmt.Errorf("reload not configured")
		}
		return al.reloadFunc()
	}
	if agent != nil {
		if agent.ContextBuilder != nil {
			rt.ListSkillNames = agent.ContextBuilder.ListSkillNames
		}
		rt.GetModelInfo = func() (string, string) {
			return agent.Model, resolvedCandidateProvider(agent.Candidates, cfg.Agents.Defaults.Provider)
		}
		rt.SwitchModel = func(value string) (string, error) {
			value = strings.TrimSpace(value)
			modelCfg, err := resolvedModelConfig(cfg, value, agent.Workspace)
			if err != nil {
				return "", err
			}

			nextProvider, _, err := providers.CreateProviderFromConfig(modelCfg)
			if err != nil {
				return "", fmt.Errorf("failed to initialize model %q: %w", value, err)
			}

			nextCandidates := resolveModelCandidates(cfg, cfg.Agents.Defaults.Provider, value, agent.Fallbacks)
			if len(nextCandidates) == 0 {
				return "", fmt.Errorf("model %q did not resolve to any provider candidates", value)
			}

			oldModel := agent.Model
			oldProvider := agent.Provider
			agent.Model = value
			agent.Provider = nextProvider
			agent.Candidates = nextCandidates
			agent.ThinkingLevel = parseThinkingLevel(modelCfg.ThinkingLevel)
			agent.ThinkingLevelConfigured = isConfiguredThinkingLevel(modelCfg.ThinkingLevel)

			if oldProvider != nil && oldProvider != nextProvider {
				if stateful, ok := oldProvider.(providers.StatefulProvider); ok {
					stateful.Close()
				}
			}
			return oldModel, nil
		}

		rt.ClearHistory = func() error {
			if opts == nil {
				return fmt.Errorf("process options not available")
			}
			return al.contextManager.Clear(ctx, opts.SessionKey)
		}

		rt.AskSideQuestion = func(ctx context.Context, question string) (string, error) {
			return al.askSideQuestion(ctx, agent, opts, question)
		}

		rt.GetContextStats = func() *commands.ContextStats {
			if opts == nil || agent.Sessions == nil {
				return nil
			}
			usage := computeContextUsage(agent, opts.SessionKey)
			if usage == nil {
				return nil
			}
			history := agent.Sessions.GetHistory(opts.SessionKey)
			return &commands.ContextStats{
				UsedTokens:       usage.UsedTokens,
				TotalTokens:      usage.TotalTokens,
				CompressAtTokens: usage.CompressAtTokens,
				UsedPercent:      usage.UsedPercent,
				MessageCount:     len(history),
			}
		}
	}
	return rt
}

func summarizeMCPToolParameters(schema any) []commands.MCPToolParameterInfo {
	schemaMap := normalizeMCPSchema(schema)
	properties, ok := schemaMap["properties"].(map[string]any)
	if !ok || len(properties) == 0 {
		return nil
	}

	required := make(map[string]struct{})
	switch raw := schemaMap["required"].(type) {
	case []string:
		for _, name := range raw {
			required[name] = struct{}{}
		}
	case []any:
		for _, value := range raw {
			name, ok := value.(string)
			if ok {
				required[name] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)

	params := make([]commands.MCPToolParameterInfo, 0, len(names))
	for _, name := range names {
		param := commands.MCPToolParameterInfo{Name: name}
		if propMap, ok := properties[name].(map[string]any); ok {
			if typeName, ok := propMap["type"].(string); ok {
				param.Type = strings.TrimSpace(typeName)
			}
			if desc, ok := propMap["description"].(string); ok {
				param.Description = strings.TrimSpace(desc)
			}
		}
		_, param.Required = required[name]
		params = append(params, param)
	}
	return params
}

func normalizeMCPSchema(schema any) map[string]any {
	if schema == nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		}
	}

	if schemaMap, ok := schema.(map[string]any); ok {
		return schemaMap
	}

	var jsonData []byte
	switch raw := schema.(type) {
	case json.RawMessage:
		jsonData = raw
	case []byte:
		jsonData = raw
	}

	if jsonData == nil {
		var err error
		jsonData, err = json.Marshal(schema)
		if err != nil {
			return map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			}
		}
	}

	var result map[string]any
	if err := json.Unmarshal(jsonData, &result); err != nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []string{},
		}
	}

	return result
}

func (al *AgentLoop) setPendingSkills(sessionKey string, skillNames []string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" || len(skillNames) == 0 {
		return
	}

	filtered := make([]string, 0, len(skillNames))
	for _, name := range skillNames {
		name = strings.TrimSpace(name)
		if name != "" {
			filtered = append(filtered, name)
		}
	}
	if len(filtered) == 0 {
		return
	}

	al.pendingSkills.Store(sessionKey, filtered)
}

func (al *AgentLoop) takePendingSkills(sessionKey string) []string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return nil
	}

	value, ok := al.pendingSkills.LoadAndDelete(sessionKey)
	if !ok {
		return nil
	}

	skills, ok := value.([]string)
	if !ok {
		return nil
	}

	return append([]string(nil), skills...)
}

func (al *AgentLoop) clearPendingSkills(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	al.pendingSkills.Delete(sessionKey)
}
