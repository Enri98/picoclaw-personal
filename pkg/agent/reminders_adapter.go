package agent

import (
	"time"

	"github.com/sipeed/picoclaw/pkg/tools"
)

// reminderStoreAdapter wraps *ReminderStore to implement tools.ReminderRegistrar.
var _ tools.ReminderRegistrar = (*reminderStoreAdapter)(nil)

type reminderStoreAdapter struct {
	store *ReminderStore
}

// Register implements tools.ReminderRegistrar.
func (a *reminderStoreAdapter) Register(text string, fireAt time.Time, chatID string) (string, error) {
	r := Reminder{
		Text:   text,
		FireAt: fireAt,
		ChatID: chatID,
	}
	saved, err := a.store.Register(r)
	if err != nil {
		return "", err
	}
	return saved.ID, nil
}
