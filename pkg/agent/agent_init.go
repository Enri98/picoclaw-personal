// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/interfaces"
	"github.com/sipeed/picoclaw/pkg/audio/tts"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/commands"
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/skills"
	"github.com/sipeed/picoclaw/pkg/state"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func NewAgentLoop(
	cfg *config.Config,
	msgBus *bus.MessageBus,
	provider providers.LLMProvider,
	opts ...AgentLoopOption,
) *AgentLoop {
	registry := NewAgentRegistry(cfg, provider)

	// Set up shared fallback chain with rate limiting.
	cooldown := providers.NewCooldownTracker()
	rl := providers.NewRateLimiterRegistry()
	// Register rate limiters for all agents' candidates so that RPM limits
	// configured in ModelConfig are enforced before each LLM call.
	for _, agentID := range registry.ListAgentIDs() {
		if agent, ok := registry.GetAgent(agentID); ok {
			rl.RegisterCandidates(agent.Candidates)
			rl.RegisterCandidates(agent.LightCandidates)
		}
	}
	fallbackChain := providers.NewFallbackChain(cooldown, rl)

	// Create state manager using default agent's workspace for channel recording
	defaultAgent := registry.GetDefaultAgent()
	var stateManager *state.Manager
	if defaultAgent != nil {
		stateManager = state.NewManager(defaultAgent.Workspace)
	}

	bridge, err := newEvolutionBridge(registry, cfg, provider)
	if err != nil {
		logger.WarnCF("agent", "Failed to initialize evolution bridge", map[string]any{
			"error": err.Error(),
		})
	}

	// Determine worker pool size from config (default: 1 = sequential)
	workerPoolSize := cfg.Agents.Defaults.MaxParallelTurns
	if workerPoolSize <= 0 {
		workerPoolSize = 1
	}

	al := &AgentLoop{
		bus:               msgBus,
		cfg:               cfg,
		registry:          registry,
		state:             stateManager,
		fallback:          fallbackChain,
		cmdRegistry:       commands.NewRegistry(commands.BuiltinDefinitions()),
		evolution:         bridge,
		steering:          newSteeringQueue(parseSteeringMode(cfg.Agents.Defaults.SteeringMode)),
		workerSem:         make(chan struct{}, workerPoolSize),
		ownsRuntimeEvents: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(al)
		}
	}
	if al.runtimeEvents == nil {
		al.runtimeEvents = runtimeevents.NewBus()
		al.ownsRuntimeEvents = true
	}
	if bridge != nil {
		bridge.setCurrentCheck(al.isCurrentEvolutionBridge)
		if err := bridge.subscribeRuntimeEvents(al.runtimeEvents.Channel()); err != nil {
			logger.WarnCF("agent", "Failed to subscribe evolution bridge to runtime events", map[string]any{
				"error": err.Error(),
			})
		}
	}
	al.refreshRuntimeEventLogger(cfg)
	al.providerFactory = providers.CreateProviderFromConfig
	al.hooks = NewHookManager(al.runtimeEvents.Channel())
	configureHookManagerFromConfig(al.hooks, cfg)
	if defaultAgent != nil && defaultAgent.Workspace != "" {
		alertFn := func(ctx context.Context, inbound *bus.InboundContext, msg string) {
			if inbound == nil {
				return
			}
			_ = msgBus.PublishOutbound(ctx, bus.OutboundMessage{
				Channel: inbound.Channel,
				ChatID:  inbound.ChatID,
				Context: *inbound,
				Content: msg,
			})
		}
		cb := newCircuitBreaker(defaultAgent.Workspace, alertFn)
		al.circuitBreaker = cb
		_ = al.hooks.Mount(NamedHook("circuit-breaker", cb))

		uh := newUsageHook(defaultAgent.Workspace)
		al.usageHook = uh
		_ = al.hooks.Mount(NamedHook("usage-tracker", uh))

		tl := NewTurnLock()
		al.turnLock = tl
		_ = al.hooks.Mount(HookRegistration{
			Name:     "turn-lock",
			Priority: 100,
			Source:   HookSourceInProcess,
			Hook:     &turnLockHook{lock: tl},
		})

		if cfg.Tools.IsToolEnabled("wiki") && cfg.Tools.Wiki.Dir != "" {
			al.wikiToolset = tools.NewWikiToolset(cfg.Tools.Wiki.Dir, defaultAgent.Workspace)
		}

		if cfg.Tools.IsToolEnabled("bash") {
			al.bashTool = tools.NewBashTool(defaultAgent.Workspace, nil)
		}

		if cfg.Tools.IsToolEnabled("gmail") && len(cfg.Tools.Gmail.Accounts) > 0 {
			clientID := os.Getenv("GMAIL_OAUTH_CLIENT_ID")
			clientSecret := os.Getenv("GMAIL_OAUTH_CLIENT_SECRET")
			if clientID == "" || clientSecret == "" {
				logger.WarnCF("agent", "Gmail tool enabled but GMAIL_OAUTH_CLIENT_ID/SECRET env vars missing; skipping", nil)
			} else {
				accounts := make([]tools.GmailAccount, 0, len(cfg.Tools.Gmail.Accounts))
				accMap := make(map[string]tools.GmailAccount)
				for _, a := range cfg.Tools.Gmail.Accounts {
					ga := tools.GmailAccount{Name: a.Name, RefreshTokenEnv: a.RefreshTokenEnv}
					accounts = append(accounts, ga)
					accMap[a.Name] = ga
				}
				if client, err := tools.NewGmailAPIClient(context.Background(), clientID, clientSecret, accMap); err != nil {
					logger.WarnCF("agent", "Failed to construct Gmail client", map[string]any{"error": err.Error()})
				} else if ts, err := tools.NewGmailToolset(accounts, client); err != nil {
					logger.WarnCF("agent", "Failed to construct Gmail toolset", map[string]any{"error": err.Error()})
				} else {
					al.gmailToolset = ts
				}
			}
		}

		if cfg.Tools.IsToolEnabled("outlook") {
			clientID := os.Getenv("OUTLOOK_OAUTH_CLIENT_ID")
			refreshToken := os.Getenv("OUTLOOK_REFRESH_TOKEN")
			persistPath := filepath.Join(defaultAgent.Workspace, "state", "outlook_refresh_token")
			if clientID == "" || refreshToken == "" {
				logger.WarnCF("agent", "Outlook tool enabled but OUTLOOK_OAUTH_CLIENT_ID/OUTLOOK_REFRESH_TOKEN missing; skipping", nil)
			} else if client, err := tools.NewOutlookGraphClient(context.Background(), clientID, refreshToken, persistPath); err != nil {
				logger.WarnCF("agent", "Failed to construct Outlook client", map[string]any{"error": err.Error()})
			} else if ts, err := tools.NewOutlookToolset(client); err != nil {
				logger.WarnCF("agent", "Failed to construct Outlook toolset", map[string]any{"error": err.Error()})
			} else {
				al.outlookToolset = ts
			}
		}

		if cfg.Tools.IsToolEnabled("gcal") {
			clientID := os.Getenv("GMAIL_OAUTH_CLIENT_ID")
			clientSecret := os.Getenv("GMAIL_OAUTH_CLIENT_SECRET")
			refreshToken := os.Getenv("GCAL_REFRESH_TOKEN")
			calendarID := cfg.Tools.GCal.CalendarID
			if calendarID == "" {
				calendarID = "primary"
			}
			if clientID == "" || clientSecret == "" || refreshToken == "" {
				logger.WarnCF("agent", "GCal tool enabled but GMAIL_OAUTH_CLIENT_ID/SECRET or GCAL_REFRESH_TOKEN missing; skipping", nil)
			} else if client, err := tools.NewGCalAPIClient(context.Background(), clientID, clientSecret, refreshToken); err != nil {
				logger.WarnCF("agent", "Failed to construct GCal client", map[string]any{"error": err.Error()})
			} else if ts, err := tools.NewGCalToolset(client, calendarID, defaultAgent.Workspace); err != nil {
				logger.WarnCF("agent", "Failed to construct GCal toolset", map[string]any{"error": err.Error()})
			} else {
				al.gcalToolset = ts
			}
		}

		if cfg.Tools.IsToolEnabled("github") {
			patEnv := cfg.Tools.GitHub.PATEnv
			if patEnv == "" {
				patEnv = "GITHUB_PAT"
			}
			pat := os.Getenv(patEnv)
			if pat == "" {
				logger.WarnCF("agent", "GitHub tool enabled but "+patEnv+" env var missing; skipping", nil)
			} else if ts, err := tools.NewGitHubToolset(pat, cfg.Tools.GitHub.WatchedRepos, filepath.Join(defaultAgent.Workspace, "state")); err != nil {
				logger.WarnCF("agent", "Failed to construct GitHub toolset", map[string]any{"error": err.Error()})
			} else {
				al.githubToolset = ts
				stateDir := filepath.Join(defaultAgent.Workspace, "state")
				if poller, pollerErr := NewGitHubPoller(pat, nil, stateDir); pollerErr != nil {
					logger.WarnCF("agent", "Failed to construct GitHub poller", map[string]any{"error": pollerErr.Error()})
				} else {
					al.githubPoller = poller
				}
			}
		}

		if cfg.Tools.IsToolEnabled("scheduler") {
			schedStateDir := filepath.Join(defaultAgent.Workspace, "state")
			heartbeatPath := cfg.Tools.Scheduler.HeartbeatPath
			if heartbeatPath == "" {
				heartbeatPath = filepath.Join(schedStateDir, "heartbeat")
			} else if !filepath.IsAbs(heartbeatPath) {
				// Config-supplied relative paths (e.g. "state/heartbeat") resolve
				// against the workspace root, not the process CWD — the briefing's
				// os.Stat must see the same file the tick goroutine writes,
				// regardless of where the binary is started from.
				heartbeatPath = filepath.Join(defaultAgent.Workspace, heartbeatPath)
			}

			rs, rsErr := NewReminderStore(schedStateDir)
			if rsErr != nil {
				logger.WarnCF("agent", "Failed to construct reminder store; skipping scheduler",
					map[string]any{"error": rsErr.Error()})
			} else {
				briefing := NewBriefingAssembler(
					al.gcalToolset,
					al.gmailToolset,
					al.outlookToolset,
					al.githubToolset,
					al.wikiToolset,
					heartbeatPath,
				)

				tick := time.Duration(cfg.Tools.Scheduler.ReminderTickSec) * time.Second

				sched, schedErr := NewScheduler(SchedulerOptions{
					StateDir:          schedStateDir,
					BriefingTime:      cfg.Tools.Scheduler.BriefingTime,
					ReminderTick:      tick,
					HeartbeatPath:     heartbeatPath,
					ReminderStore:     rs,
					BriefingAssembler: briefing,
					Provider:          provider,
					Model:             cfg.Tools.Scheduler.ParseModel,
				})
				if schedErr != nil {
					logger.WarnCF("agent", "Failed to construct scheduler",
						map[string]any{"error": schedErr.Error()})
				} else {
					al.scheduler = sched

					adapter := &reminderStoreAdapter{store: rs}
					al.remindersToolset = tools.NewRemindersToolset(adapter)
					primaryChatID := cfg.Tools.Scheduler.PrimaryChatID
					if primaryChatID != "" {
						al.remindersToolset.SetChatID(primaryChatID)
					}
				}
			}
		}
	}
	al.contextManager = al.resolveContextManager()

	// Register shared tools to all agents (now that al is created)
	registerSharedTools(al, cfg, msgBus, registry, provider)

	return al
}

func registerSharedTools(
	al *AgentLoop,
	cfg *config.Config,
	msgBus interfaces.MessageBus,
	registry *AgentRegistry,
	provider providers.LLMProvider,
) {
	allowReadPaths := buildAllowReadPatterns(cfg)
	var ttsProvider tts.TTSProvider
	if cfg.Tools.IsToolEnabled("send_tts") {
		ttsProvider = tts.DetectTTS(cfg)
		if ttsProvider == nil {
			logger.WarnCF("voice-tts", "send_tts enabled but no TTS provider configured", nil)
		}
	}

	for _, agentID := range registry.ListAgentIDs() {
		agent, ok := registry.GetAgent(agentID)
		if !ok {
			continue
		}

		if cfg.Tools.IsToolEnabled("web") {
			searchTool, err := tools.NewWebSearchTool(tools.WebSearchToolOptionsFromConfig(cfg))
			if err != nil {
				logger.ErrorCF("agent", "Failed to create web search tool", map[string]any{"error": err.Error()})
			} else if searchTool != nil {
				agent.Tools.Register(searchTool)
			}
		}
		if cfg.Tools.IsToolEnabled("web_fetch") {
			fetchTool, err := tools.NewWebFetchToolWithProxy(
				50000,
				cfg.Tools.Web.Proxy,
				cfg.Tools.Web.Format,
				cfg.Tools.Web.FetchLimitBytes,
				cfg.Tools.Web.PrivateHostWhitelist)
			if err != nil {
				logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
			} else {
				agent.Tools.Register(fetchTool)
			}
		}

		// Hardware tools (I2C, SPI) - Linux only, returns error on other platforms
		if cfg.Tools.IsToolEnabled("i2c") {
			agent.Tools.Register(tools.NewI2CTool())
		}
		if cfg.Tools.IsToolEnabled("spi") {
			agent.Tools.Register(tools.NewSPITool())
		}
		if cfg.Tools.IsToolEnabled("serial") {
			agent.Tools.Register(tools.NewSerialTool())
		}

		// Message tool
		if cfg.Tools.IsToolEnabled("message") {
			messageTool := tools.NewMessageTool()
			messageTool.SetSendCallback(func(
				ctx context.Context,
				channel, chatID, content, replyToMessageID string,
			) error {
				pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer pubCancel()
				outboundCtx := bus.NewOutboundContext(channel, chatID, replyToMessageID)
				outboundAgentID, outboundSessionKey, outboundScope := outboundTurnMetadata(
					tools.ToolAgentID(ctx),
					tools.ToolSessionKey(ctx),
					tools.ToolSessionScope(ctx),
				)
				return msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
					Context:          outboundCtx,
					AgentID:          outboundAgentID,
					SessionKey:       outboundSessionKey,
					Scope:            outboundScope,
					Content:          content,
					ReplyToMessageID: replyToMessageID,
				})
			})
			agent.Tools.Register(messageTool)
		}
		if cfg.Tools.IsToolEnabled("reaction") {
			reactionTool := tools.NewReactionTool()
			reactionTool.SetReactionCallback(func(ctx context.Context, channel, chatID, messageID string) error {
				if al.channelManager == nil {
					return fmt.Errorf("channel manager not configured")
				}
				ch, ok := al.channelManager.GetChannel(channel)
				if !ok {
					return fmt.Errorf("channel %s not found", channel)
				}
				rc, ok := ch.(channels.ReactionCapable)
				if !ok {
					return fmt.Errorf("channel %s does not support reactions", channel)
				}
				_, err := rc.ReactToMessage(ctx, chatID, messageID)
				return err
			})
			agent.Tools.Register(reactionTool)
		}

		// Send file tool (outbound media via MediaStore — store injected later by SetMediaStore)
		if cfg.Tools.IsToolEnabled("send_file") {
			sendFileTool := tools.NewSendFileTool(
				agent.Workspace,
				cfg.Agents.Defaults.RestrictToWorkspace,
				cfg.Agents.Defaults.GetMaxMediaSize(),
				nil,
				allowReadPaths,
			)
			agent.Tools.Register(sendFileTool)
		}

		if ttsProvider != nil {
			agent.Tools.Register(tools.NewSendTTSTool(ttsProvider, nil))
		}

		if cfg.Tools.IsToolEnabled("load_image") {
			loadImageTool := tools.NewLoadImageTool(
				agent.Workspace,
				cfg.Agents.Defaults.RestrictToWorkspace,
				cfg.Agents.Defaults.GetMaxMediaSize(),
				nil,
				allowReadPaths,
			)
			agent.Tools.Register(loadImageTool)
		}

		// Skill discovery and installation tools
		skills_enabled := cfg.Tools.IsToolEnabled("skills")
		find_skills_enable := cfg.Tools.IsToolEnabled("find_skills")
		install_skills_enable := cfg.Tools.IsToolEnabled("install_skill")
		if skills_enabled && (find_skills_enable || install_skills_enable) {
			registryMgr := skills.NewRegistryManagerFromToolsConfig(cfg.Tools.Skills)

			if find_skills_enable {
				searchCache := skills.NewSearchCache(
					cfg.Tools.Skills.SearchCache.MaxSize,
					time.Duration(cfg.Tools.Skills.SearchCache.TTLSeconds)*time.Second,
				)
				agent.Tools.Register(tools.NewFindSkillsTool(registryMgr, searchCache))
			}

			if install_skills_enable {
				agent.Tools.Register(tools.NewInstallSkillTool(registryMgr, agent.Workspace))
			}
		}

		// Spawn and spawn_status tools share a SubagentManager.
		// Construct it when either tool is enabled (both require subagent).
		spawnEnabled := cfg.Tools.IsToolEnabled("spawn")
		spawnStatusEnabled := cfg.Tools.IsToolEnabled("spawn_status")
		if (spawnEnabled || spawnStatusEnabled) && cfg.Tools.IsToolEnabled("subagent") {
			subagentManager := tools.NewSubagentManager(provider, agent.Model, agent.Workspace)
			subagentManager.SetLLMOptions(agent.MaxTokens, agent.Temperature)

			// Inject a media resolver so the legacy RunToolLoop fallback path can
			// resolve media:// refs in the same way the main AgentLoop does.
			// This keeps subagent vision support working even when the optimized
			// sub-turn spawner path is unavailable.
			subagentManager.SetMediaResolver(func(msgs []providers.Message) []providers.Message {
				return resolveMediaRefs(msgs, al.mediaStore, cfg.Agents.Defaults.GetMaxMediaSize())
			})

			// Set the spawner that links into AgentLoop's turnState
			subagentManager.SetSpawner(func(
				ctx context.Context,
				task, label, targetAgentID string,
				tls *tools.ToolRegistry,
				maxTokens int,
				temperature float64,
				hasMaxTokens, hasTemperature bool,
			) (*tools.ToolResult, error) {
				// 1. Recover parent Turn State from Context
				parentTS := turnStateFromContext(ctx)
				if parentTS == nil {
					// Fallback: If no turnState exists in context, create an isolated ad-hoc root turn state
					// so that the tool can still function outside of an agent loop (e.g. tests, raw invocations).
					parentTS = &turnState{
						ctx:            ctx,
						turnID:         "adhoc-root",
						depth:          0,
						session:        nil, // Ephemeral session not needed for adhoc spawn
						pendingResults: make(chan *tools.ToolResult, 16),
						concurrencySem: make(chan struct{}, 5),
					}
				}

				// 2. Build Tools slice from registry
				var tlSlice []tools.Tool
				for _, name := range tls.List() {
					if t, ok := tls.Get(name); ok {
						tlSlice = append(tlSlice, t)
					}
				}

				// 3. System Prompt
				systemPrompt := "You are a subagent. Complete the given task independently and report the result.\n" +
					"You have access to tools - use them as needed to complete your task.\n" +
					"After completing the task, provide a clear summary of what was done.\n\n" +
					"Task: " + task

				// 4. Resolve Model
				modelToUse := agent.Model
				if targetAgentID != "" {
					if targetAgent, ok := al.GetRegistry().GetAgent(targetAgentID); ok {
						modelToUse = targetAgent.Model
					}
				}

				// 5. Build SubTurnConfig
				cfg := SubTurnConfig{
					Model:        modelToUse,
					Tools:        tlSlice,
					SystemPrompt: systemPrompt,
				}
				if hasMaxTokens {
					cfg.MaxTokens = maxTokens
				}

				// 6. Spawn SubTurn
				return spawnSubTurn(ctx, al, parentTS, cfg)
			})

			// Clone the parent's tool registry so subagents can use all
			// tools registered so far (file, web, etc.) but NOT spawn/
			// spawn_status which are added below — preventing recursive
			// subagent spawning.
			subagentManager.SetTools(agent.Tools.Clone())
			if spawnEnabled {
				spawnTool := tools.NewSpawnTool(subagentManager)
				spawnTool.SetSpawner(NewSubTurnSpawner(al))
				currentAgentID := agentID
				spawnTool.SetAllowlistChecker(func(targetAgentID string) bool {
					return registry.CanSpawnSubagent(currentAgentID, targetAgentID)
				})

				agent.Tools.Register(spawnTool)

				// Also register the synchronous subagent tool
				subagentTool := tools.NewSubagentTool(subagentManager)
				subagentTool.SetSpawner(NewSubTurnSpawner(al))
				agent.Tools.Register(subagentTool)
			}
			if spawnStatusEnabled {
				agent.Tools.Register(tools.NewSpawnStatusTool(subagentManager))
			}
		} else if (spawnEnabled || spawnStatusEnabled) && !cfg.Tools.IsToolEnabled("subagent") {
			logger.WarnCF("agent", "spawn/spawn_status tools require subagent to be enabled", nil)
		}

		// Register delegate tool for multi-agent setups.
		// Auto-enabled when multiple agents exist. Delegation uses the SubTurn
		// mechanism directly (not SubagentManager) and is independent of the
		// subagent tool.
		if len(registry.ListAgentIDs()) > 1 {
			delegateTool := tools.NewDelegateTool()
			delegateTool.SetSpawner(NewSubTurnSpawner(al))
			currentAgentID := agentID
			delegateTool.SetSelfAgentID(currentAgentID)
			delegateTool.SetAllowlistChecker(func(targetAgentID string) bool {
				return registry.CanSpawnSubagent(currentAgentID, targetAgentID)
			})
			agent.Tools.Register(delegateTool)
		}

		if al.wikiToolset != nil {
			for _, wt := range al.wikiToolset.Tools() {
				agent.Tools.Register(wt)
			}
		}

		if al.bashTool != nil {
			agent.Tools.Register(al.bashTool)
		}

		if al.gmailToolset != nil {
			for _, gt := range al.gmailToolset.Tools() {
				agent.Tools.Register(gt)
			}
		}

		if al.outlookToolset != nil {
			for _, gt := range al.outlookToolset.Tools() {
				agent.Tools.Register(gt)
			}
		}

		if al.gcalToolset != nil {
			for _, gt := range al.gcalToolset.Tools() {
				agent.Tools.Register(gt)
			}
		}

		if al.githubToolset != nil {
			for _, gt := range al.githubToolset.Tools() {
				agent.Tools.Register(gt)
			}
		}

		if al.remindersToolset != nil {
			for _, rt := range al.remindersToolset.Tools() {
				agent.Tools.Register(rt)
			}
		}

		warnOnUnknownAgentToolDeclarations(agentID, agent.Workspace, agent.Definition, agent.Tools)
	}
}
