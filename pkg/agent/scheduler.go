package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// SchedulerOptions configures the Scheduler at construction time.
type SchedulerOptions struct {
	// StateDir is the directory that holds scheduler.json and reminders.json.
	StateDir string
	// BriefingTime is the local "HH:MM" at which to send the daily briefing.
	// Empty string disables the briefing.
	BriefingTime string
	// ReminderTick is how often to check for due reminders (default 60s).
	ReminderTick time.Duration
	// HeartbeatPath is the file to touch on every tick. May be empty.
	HeartbeatPath string
	// ReminderStore is the store used by the scheduler for reminder delivery.
	// If nil, NewScheduler creates one from StateDir.
	ReminderStore *ReminderStore
	// BriefingAssembler assembles the daily briefing. May be nil.
	BriefingAssembler *BriefingAssembler
	// Provider is used by the TimeParser for LLM-assisted time parsing. May be nil.
	Provider providers.LLMProvider
	// Model is the model name passed to the provider for time-parse calls.
	Model string
}

// schedulerState is persisted to scheduler.json.
type schedulerState struct {
	LastBriefingDate string `json:"last_briefing_date"` // "YYYY-MM-DD" in local time
}

// Scheduler runs background work: reminder firing and the daily briefing.
// Lifecycle: NewScheduler → SetChannelManager → SetChatID → Start(ctx).
type Scheduler struct {
	reminderStore     *ReminderStore
	briefingAssembler *BriefingAssembler
	timeParser        *TimeParser
	channelManager    *channels.Manager
	chatID            string
	alertChatID       string
	briefingTime      string
	reminderTick      time.Duration
	heartbeatPath     string
	statePath         string

	mu        sync.Mutex
	startOnce sync.Once
	started   bool
}

// NewScheduler constructs a Scheduler from options.
func NewScheduler(opts SchedulerOptions) (*Scheduler, error) {
	if opts.StateDir == "" {
		return nil, fmt.Errorf("scheduler: StateDir must not be empty")
	}
	if err := os.MkdirAll(opts.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("scheduler: failed to create stateDir: %w", err)
	}

	tick := opts.ReminderTick
	if tick <= 0 {
		tick = 60 * time.Second
	}

	rs := opts.ReminderStore
	if rs == nil {
		var err error
		rs, err = NewReminderStore(opts.StateDir)
		if err != nil {
			return nil, fmt.Errorf("scheduler: failed to create reminder store: %w", err)
		}
	}

	tp := NewTimeParser(opts.Provider, opts.Model)

	return &Scheduler{
		reminderStore:     rs,
		briefingAssembler: opts.BriefingAssembler,
		timeParser:        tp,
		briefingTime:      opts.BriefingTime,
		reminderTick:      tick,
		heartbeatPath:     opts.HeartbeatPath,
		statePath:         filepath.Join(opts.StateDir, "scheduler.json"),
	}, nil
}

// SetChannelManager injects the channel manager. Must be called before Start.
func (s *Scheduler) SetChannelManager(cm *channels.Manager) {
	s.mu.Lock()
	s.channelManager = cm
	s.mu.Unlock()
}

// SetChatID sets the primary Telegram chat ID for reminder and briefing delivery.
func (s *Scheduler) SetChatID(chatID string) {
	s.mu.Lock()
	s.chatID = chatID
	if s.alertChatID == "" {
		s.alertChatID = chatID
	}
	s.mu.Unlock()
}

// SetAlertChatID overrides the chat ID used for operational alerts.
func (s *Scheduler) SetAlertChatID(chatID string) {
	s.mu.Lock()
	s.alertChatID = chatID
	s.mu.Unlock()
}

// Reminders returns the underlying reminder store so command handlers can register.
func (s *Scheduler) Reminders() *ReminderStore {
	return s.reminderStore
}

// TimeParser returns the scheduler's time parser for use by command handlers.
func (s *Scheduler) TimeParser() *TimeParser {
	return s.timeParser
}

// Start launches the background goroutine. Safe to call multiple times; only
// the first call has effect.
func (s *Scheduler) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		s.mu.Lock()
		s.started = true
		s.mu.Unlock()
		go s.run(ctx)
	})
}

// ---------------------------------------------------------------------------
// Background goroutine
// ---------------------------------------------------------------------------

func (s *Scheduler) run(ctx context.Context) {
	ticker := time.NewTicker(s.reminderTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now()

	// 1. Touch heartbeat file.
	s.touchHeartbeat(now)

	// 2. Fire due reminders.
	s.fireReminders(ctx, now)

	// 3. Send daily briefing if due.
	s.maybeSendBriefing(ctx, now)
}

// touchHeartbeat writes the current ISO8601 timestamp to the heartbeat file.
func (s *Scheduler) touchHeartbeat(now time.Time) {
	if s.heartbeatPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.heartbeatPath), 0o755); err != nil {
		logger.WarnCF("scheduler", "Failed to create heartbeat directory",
			map[string]any{"error": err.Error()})
		return
	}
	stamp := now.UTC().Format(time.RFC3339)
	if err := os.WriteFile(s.heartbeatPath, []byte(stamp), 0o644); err != nil {
		logger.WarnCF("scheduler", "Failed to write heartbeat file",
			map[string]any{"path": s.heartbeatPath, "error": err.Error()})
	}
}

// fireReminders checks for due reminders and delivers them via Telegram.
func (s *Scheduler) fireReminders(ctx context.Context, now time.Time) {
	fired, err := s.reminderStore.FireDue(now)
	if err != nil {
		logger.WarnCF("scheduler", "Failed to fire due reminders",
			map[string]any{"error": err.Error()})
		return
	}

	s.mu.Lock()
	cm := s.channelManager
	defaultChatID := s.chatID
	s.mu.Unlock()

	if cm == nil {
		return
	}

	for _, r := range fired {
		chatID := r.ChatID
		if chatID == "" {
			chatID = defaultChatID
		}
		if chatID == "" {
			logger.WarnCF("scheduler", "Reminder has no chat ID; skipping",
				map[string]any{"reminder_id": r.ID, "text": r.Text})
			continue
		}
		msg := fmt.Sprintf("🔔 Promemoria: %s", r.Text)
		if err := cm.SendToChannel(ctx, "telegram", chatID, msg); err != nil {
			logger.WarnCF("scheduler", "Failed to deliver reminder",
				map[string]any{"reminder_id": r.ID, "error": err.Error()})
		}
	}
}

// maybeSendBriefing sends the daily briefing if the conditions are met.
func (s *Scheduler) maybeSendBriefing(ctx context.Context, now time.Time) {
	if s.briefingTime == "" || s.briefingAssembler == nil {
		return
	}

	s.mu.Lock()
	cm := s.channelManager
	chatID := s.chatID
	s.mu.Unlock()

	if cm == nil || chatID == "" {
		return
	}

	// Parse briefing time in local.
	loc := now.Location()
	nowLocal := now.In(loc)
	var bHour, bMin int
	if _, err := fmt.Sscanf(s.briefingTime, "%d:%d", &bHour, &bMin); err != nil {
		logger.WarnCF("scheduler", "Invalid briefing_time format; expected HH:MM",
			map[string]any{"briefing_time": s.briefingTime, "error": err.Error()})
		return
	}

	// Check if we've passed briefing time today.
	briefingToday := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(),
		bHour, bMin, 0, 0, loc)
	if nowLocal.Before(briefingToday) {
		return
	}

	// Check if already sent today.
	todayStr := nowLocal.Format("2006-01-02")
	st, err := s.loadState()
	if err != nil {
		logger.WarnCF("scheduler", "Failed to load scheduler state",
			map[string]any{"error": err.Error()})
		return
	}
	if st.LastBriefingDate == todayStr {
		return
	}

	// Assemble and send.
	text := s.briefingAssembler.Assemble(ctx, nowLocal)
	if err := cm.SendToChannel(ctx, "telegram", chatID, text); err != nil {
		logger.WarnCF("scheduler", "Failed to send daily briefing",
			map[string]any{"error": err.Error()})
		return
	}

	// Persist.
	st.LastBriefingDate = todayStr
	if err := s.saveState(st); err != nil {
		logger.WarnCF("scheduler", "Failed to persist briefing date",
			map[string]any{"error": err.Error()})
	}
}

// ---------------------------------------------------------------------------
// State persistence
// ---------------------------------------------------------------------------

func (s *Scheduler) loadState() (schedulerState, error) {
	data, err := os.ReadFile(s.statePath)
	if os.IsNotExist(err) {
		return schedulerState{}, nil
	}
	if err != nil {
		return schedulerState{}, fmt.Errorf("scheduler: load state: %w", err)
	}
	var st schedulerState
	if err := json.Unmarshal(data, &st); err != nil {
		return schedulerState{}, fmt.Errorf("scheduler: parse state: %w", err)
	}
	return st, nil
}

func (s *Scheduler) saveState(st schedulerState) error {
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}
