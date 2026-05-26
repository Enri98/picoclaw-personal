---
name: daily-briefing
description: "Scheduled morning briefing (default 08:00 local), reminders via /remind or reminder_set, and heartbeat monitoring."
---

# Daily briefing

## When to use

This skill covers three related behaviors:

1. **The daily briefing** — a scheduled, deterministic digest sent each morning.
2. **Reminders** — user-typed `/remind` or the `reminder_set` tool called from
   natural conversation.
3. **Heartbeat monitoring** — staleness detection embedded in the briefing.

The assistant uses `reminder_set` when the user expresses a future intent in
conversation. It does not use this tool for queries about existing reminders.

---

## Daily briefing

### Schedule and assembly

Fires once per day at the configured time (default `08:00` local, Europe/Rome).
Assembly is **deterministic templating** — no model call is involved. Each section
is filled from the relevant tool output and formatted with the fixed Italian
template below.

### Template sections

| Section | Source | Stub on failure |
|---|---|---|
| ☕ greeting | Static, includes day-of-week and date | — |
| 📅 today's events | `gcal_today` | `[non disponibile]` |
| ✉️ unread emails | `gmail_list_unread` (both accounts) + `outlook_list_unread` | `[non disponibile]` per account |
| 🔔 GitHub activity | `github_open_prs` + `github_open_issues` counts across watched repos | `[non disponibile]` |
| 📌 wiki inbox | `wiki_inbox_count` | `[non disponibile]` |
| ⚠️ heartbeat warning | `state/heartbeat` mtime check | Omitted when fresh |

Each section is **independent**. If one API call fails, that section shows the
stub and the rest of the briefing is sent normally — a broken calendar API does
not suppress the email count, and so on.

### Heartbeat warning

The main process writes an ISO8601 timestamp to `state/heartbeat` every 60 s.
The briefing assembler reads that file at send-time. If the timestamp is more
than 5 minutes old, a ⚠️ line is appended:

```
⚠️ Heartbeat stale: ultimo aggiornamento XX min fa
```

If the file is fresh, this line is omitted entirely.

### Example output (schematic)

```
☕ Buongiorno! Lunedì 26 maggio 2026

📅 Oggi
  • 10:00 — Stand-up
  • 14:30 — Dentista

✉️ Email non lette
  darra2: 3 nuove
  chiunque: 0 nuove
  Outlook: 1 nuova

🔔 GitHub
  picoclaw: 2 PR aperte, 1 issue aperta

📌 Wiki inbox
  4 proposte in attesa
```

---

## Reminders

### Two entry paths

**Path 1 — `/remind` slash command**

User types the command directly:

```
/remind 2h prendi le medicine
/remind tomorrow 9:00 prep riunione
/remind domani 9:00 prep riunione
/remind tra 2 ore chiama il medico
/remind in 30 minutes check the build
```

Time parsing uses a regex first (covers durations like `2m`, `1h30m`, `10s`;
absolute forms like `tomorrow 9:00`, `domani 9:00`, `tra N ore`, `in N minutes`).
If the regex does not match, the string is passed to the model for a one-shot
parse into a UTC fire-time. The result is always stored as an RFC3339 UTC
timestamp.

**Path 2 — `reminder_set` tool**

The assistant calls this tool when the user expresses a future intent in
natural conversation, without using `/remind`.

```
reminder_set({fire_at: "<RFC3339 UTC>", text: "<reminder text>"})
```

### When to call `reminder_set`

Call it when the user's message encodes a clear intent to be reminded at a
specific future time. Confirm the inferred time before calling if there is
any ambiguity.

| User message | Action |
|---|---|
| "remind me to feed the cat at 6pm" | `reminder_set({fire_at: "...18:00Z", text: "feed the cat"})` |
| "tomorrow at 9am I have a meeting prep" | `reminder_set({fire_at: "...09:00Z", text: "meeting prep"})` |
| "show me my reminders" | Do NOT call `reminder_set` — this is a query. No listing tool exists in v1; tell the user. |
| "delete the reminder about cat food" | Do NOT call `reminder_set` — v1 has no cancel tool. Tell the user they can edit `state/reminders.json` manually if needed. |

### Storage and delivery

- Persisted to `state/reminders.json`. Survives process restart.
- Tick interval: 60 s. Fire-time resolution is ±60 s.
- One-shot only (v1). Each reminder fires once and is removed.
- No cancellation tool in v1. Manual edit of `state/reminders.json` is the
  escape hatch.

---

## Heartbeat and process monitoring

The main process touches `state/heartbeat` every 60 s with an ISO8601
timestamp. The daily briefing reads this file; see the ⚠️ rule above.

Process-crash detection is handled at the systemd level by a separate
`picoclaw-alert.service` unit. On `OnFailure`, that unit posts to a dedicated
alert Telegram bot. The model is not involved in crash detection — it only
surfaces staleness in the morning briefing.

---

## Language handling

The briefing is always in Italian (fixed template). Reminder fire notifications
are sent in the language the user used when setting the reminder. Interactive
replies from the assistant follow the language of the user's most recent message.
