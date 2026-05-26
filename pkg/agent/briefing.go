package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// italianWeekdays maps time.Weekday to the Italian name.
var italianWeekdays = [7]string{
	"domenica",
	"lunedì",
	"martedì",
	"mercoledì",
	"giovedì",
	"venerdì",
	"sabato",
}

// italianMonths maps time.Month (1-based) to the Italian name.
var italianMonths = [13]string{
	"",
	"gennaio",
	"febbraio",
	"marzo",
	"aprile",
	"maggio",
	"giugno",
	"luglio",
	"agosto",
	"settembre",
	"ottobre",
	"novembre",
	"dicembre",
}

// BriefingAssembler assembles the daily morning briefing message.
// Each toolset pointer is optional; sections are stubbed if nil.
type BriefingAssembler struct {
	gcalToolset    *tools.GCalToolset
	gmailToolset   *tools.GmailToolset
	outlookToolset *tools.OutlookToolset
	githubToolset  *tools.GitHubToolset
	wikiToolset    *tools.WikiToolset
	heartbeatPath  string
}

// NewBriefingAssembler creates a BriefingAssembler. All toolset arguments may be nil.
func NewBriefingAssembler(
	gcalToolset *tools.GCalToolset,
	gmailToolset *tools.GmailToolset,
	outlookToolset *tools.OutlookToolset,
	githubToolset *tools.GitHubToolset,
	wikiToolset *tools.WikiToolset,
	heartbeatPath string,
) *BriefingAssembler {
	return &BriefingAssembler{
		gcalToolset:    gcalToolset,
		gmailToolset:   gmailToolset,
		outlookToolset: outlookToolset,
		githubToolset:  githubToolset,
		wikiToolset:    wikiToolset,
		heartbeatPath:  heartbeatPath,
	}
}

// Assemble produces the briefing message. It never panics; failed sections
// produce stub lines. now is expected to be in local time (or at least the
// location should be set correctly for display).
func (b *BriefingAssembler) Assemble(ctx context.Context, now time.Time) string {
	loc := now.Location()
	nowLocal := now.In(loc)

	weekday := italianWeekdays[nowLocal.Weekday()]
	month := italianMonths[nowLocal.Month()]
	header := fmt.Sprintf("☕ Buongiorno. %s %d %s %d.",
		strings.ToUpper(weekday[:1])+weekday[1:],
		nowLocal.Day(),
		month,
		nowLocal.Year(),
	)

	var sections []string
	sections = append(sections, header)
	sections = append(sections, "")

	// --- Calendar section ---
	sections = append(sections, b.calendarSection(ctx, nowLocal))

	// --- Email section ---
	sections = append(sections, b.emailSection(ctx, now))

	// --- GitHub section ---
	sections = append(sections, b.githubSection(ctx))

	// --- Wiki inbox section ---
	sections = append(sections, b.inboxSection())

	// --- Heartbeat section (conditional) ---
	if hb := b.heartbeatSection(nowLocal); hb != "" {
		sections = append(sections, hb)
	}

	return strings.Join(sections, "\n")
}

// ---------------------------------------------------------------------------
// Calendar
// ---------------------------------------------------------------------------

func (b *BriefingAssembler) calendarSection(ctx context.Context, nowLocal time.Time) string {
	if b.gcalToolset == nil {
		return "📅 Calendario: [non disponibile]"
	}

	var events []tools.GCalEvent
	var fetchErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				fetchErr = fmt.Errorf("panic: %v", r)
			}
		}()
		events, fetchErr = b.gcalToolset.TodayDirect(ctx)
	}()

	if fetchErr != nil {
		logger.WarnCF("briefing", "Calendar fetch failed",
			map[string]any{"error": fetchErr.Error()})
		return "📅 Calendario: [non disponibile]"
	}

	if len(events) == 0 {
		return "📅 Oggi (0):\n  Nessun evento."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📅 Oggi (%d):\n", len(events))
	for _, ev := range events {
		start := ev.Start.In(nowLocal.Location())
		fmt.Fprintf(&sb, "  %s — %s\n", start.Format("15:04"), ev.Title)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ---------------------------------------------------------------------------
// Email
// ---------------------------------------------------------------------------

func (b *BriefingAssembler) emailSection(ctx context.Context, now time.Time) string {
	since := now.Add(-24 * time.Hour)
	gmailCount := 0
	outlookCount := 0
	gmailOK := false
	outlookOK := false

	// Track senders across providers for deduplication.
	senderFreq := make(map[string]int)

	// Gmail
	if b.gmailToolset != nil {
		func() {
			defer func() { recover() }() //nolint:errcheck
			accounts := b.gmailToolset.AccountNames()
			for _, account := range accounts {
				msgs, err := b.gmailToolset.ListUnreadDirect(ctx, account, since, 50)
				if err != nil {
					logger.WarnCF("briefing", "Gmail fetch failed",
						map[string]any{"account": account, "error": err.Error()})
					continue
				}
				gmailCount += len(msgs)
				for _, m := range msgs {
					senderFreq[normalizeSender(m.From)]++
				}
				gmailOK = true
			}
		}()
	}

	// Outlook
	if b.outlookToolset != nil {
		func() {
			defer func() { recover() }() //nolint:errcheck
			msgs, err := b.outlookToolset.ListUnreadDirect(ctx, since, 50)
			if err != nil {
				logger.WarnCF("briefing", "Outlook fetch failed",
					map[string]any{"error": err.Error()})
				return
			}
			outlookCount = len(msgs)
			for _, m := range msgs {
				senderFreq[normalizeSender(m.From)]++
			}
			outlookOK = true
		}()
	}

	if b.gmailToolset == nil && b.outlookToolset == nil {
		return "✉️ Email: [non disponibile]"
	}
	if !gmailOK && !outlookOK {
		return "✉️ Email: [non disponibile]"
	}

	total := gmailCount + outlookCount
	var sb strings.Builder
	fmt.Fprintf(&sb, "✉️ Non letti dalle 24h: %d (Gmail %d, Outlook %d)\n", total, gmailCount, outlookCount)

	top5 := topSenders(senderFreq, 5)
	if len(top5) > 0 {
		fmt.Fprintf(&sb, "   Mittenti: %s", strings.Join(top5, ", "))
	}

	return strings.TrimRight(sb.String(), "\n")
}

// normalizeSender extracts a display name or bare email from a From header.
func normalizeSender(from string) string {
	from = strings.TrimSpace(from)
	// "Name <email>" → "Name"
	if idx := strings.Index(from, "<"); idx > 0 {
		name := strings.TrimSpace(from[:idx])
		if name != "" {
			return name
		}
	}
	// bare email → domain stripped
	if at := strings.Index(from, "@"); at > 0 {
		return from[:at]
	}
	return from
}

// topSenders returns up to n sender names sorted by frequency (descending).
func topSenders(freq map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(freq))
	for k, v := range freq {
		pairs = append(pairs, kv{k, v})
	}
	// Simple insertion sort — list is tiny.
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j].v > pairs[j-1].v; j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}
	result := make([]string, 0, n)
	for i := 0; i < len(pairs) && i < n; i++ {
		result = append(result, pairs[i].k)
	}
	return result
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

func (b *BriefingAssembler) githubSection(ctx context.Context) string {
	if b.githubToolset == nil {
		return "🔔 GitHub: [non disponibile]"
	}

	repos := b.githubToolset.WatchedRepos()
	if len(repos) == 0 {
		return "🔔 GitHub: nessun repo configurato."
	}

	type repoSummary struct {
		name   string
		prs    int
		issues int
		err    bool
	}

	summaries := make([]repoSummary, 0, len(repos))
	totalPRs := 0
	totalIssues := 0
	fetchFailed := true

	for _, repo := range repos {
		var rs repoSummary
		rs.name = repo

		func() {
			defer func() {
				if r := recover(); r != nil {
					rs.err = true
				}
			}()
			prs, err := b.githubToolset.OpenPRsDirect(ctx, repo)
			if err != nil {
				logger.WarnCF("briefing", "GitHub PR fetch failed",
					map[string]any{"repo": repo, "error": err.Error()})
				rs.err = true
				return
			}
			issues, err := b.githubToolset.OpenIssuesDirect(ctx, repo)
			if err != nil {
				logger.WarnCF("briefing", "GitHub issue fetch failed",
					map[string]any{"repo": repo, "error": err.Error()})
				rs.err = true
				return
			}
			rs.prs = prs
			rs.issues = issues
			totalPRs += prs
			totalIssues += issues
			fetchFailed = false
		}()

		summaries = append(summaries, rs)
	}

	if fetchFailed {
		return "🔔 GitHub: [non disponibile]"
	}

	successCount := 0
	for _, s := range summaries {
		if !s.err {
			successCount++
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "🔔 GitHub: %d PR aperte, %d issue aperte su %d repo\n",
		totalPRs, totalIssues, successCount)

	for _, s := range summaries {
		if s.err {
			fmt.Fprintf(&sb, "   %s: [non disponibile]\n", s.name)
		} else if s.prs > 0 || s.issues > 0 {
			fmt.Fprintf(&sb, "   %s: %d PR, %d issue\n", s.name, s.prs, s.issues)
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// ---------------------------------------------------------------------------
// Wiki inbox
// ---------------------------------------------------------------------------

func (b *BriefingAssembler) inboxSection() string {
	if b.wikiToolset == nil {
		return "📌 Inbox: [non disponibile]"
	}

	var count int
	var fetchErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				fetchErr = fmt.Errorf("panic: %v", r)
			}
		}()
		count, fetchErr = b.wikiToolset.InboxCountDirect()
	}()

	if fetchErr != nil {
		logger.WarnCF("briefing", "Wiki inbox count failed",
			map[string]any{"error": fetchErr.Error()})
		return "📌 Inbox: [non disponibile]"
	}

	return fmt.Sprintf("📌 Inbox: %d elementi in attesa.", count)
}

// ---------------------------------------------------------------------------
// Heartbeat
// ---------------------------------------------------------------------------

func (b *BriefingAssembler) heartbeatSection(nowLocal time.Time) string {
	if b.heartbeatPath == "" {
		return ""
	}

	info, err := os.Stat(b.heartbeatPath)
	if err != nil {
		// File doesn't exist yet — not stale.
		return ""
	}

	age := nowLocal.Sub(info.ModTime())
	if age > 5*time.Minute {
		mins := int(age.Minutes())
		return fmt.Sprintf("⚠️ Heartbeat obsoleto (ultimo aggiornamento %d min fa)", mins)
	}
	return ""
}
