package tools

// Destructive-pattern detection is a UX gate, not a security boundary.
// The real security boundaries are:
//   - picoclaw-shell uid (cannot read secrets.env, cannot write outside scratch)
//   - prlimits on bash via picoclaw-runshell
//   - sudoers restricts what picoclaw can run as picoclaw-shell
// Bypasses to these regexes exist (whitespace tricks, eval, base64) and we do
// NOT chase them. The regex catches obvious mistakes and forces /apply review.

import "regexp"

type destructivePattern struct {
	name string
	re   *regexp.Regexp
}

var destructivePatterns = []destructivePattern{
	{
		name: "rm-rf-root",
		// rm -rf / or rm -fr / or variants with flags interleaved; path starts at root.
		re: regexp.MustCompile(`(?i)\brm\b.*-[a-z]*[rf][a-z]*[rf][a-z]*\s+/[^\s]*`),
	},
	{
		name: "dd-block-device",
		// dd ... of=/dev/...
		re: regexp.MustCompile(`(?i)\bdd\b.*\bof=/dev/`),
	},
	{
		name: "mkfs",
		// mkfs or mkfs.ext4 etc., as a word (not mkfsx)
		re: regexp.MustCompile(`(?i)(^|\s|\|)mkfs(\.[a-z0-9]+)?(\s|$)`),
	},
	{
		name: "chmod-777-root",
		// chmod 777 /  (root-relative)
		re: regexp.MustCompile(`(?i)\bchmod\b\s+777\s+/`),
	},
	{
		name: "redirect-to-system-dir",
		// > or >> redirect targeting /etc/, /usr/, /boot/, /bin/, /sbin/
		re: regexp.MustCompile(`>{1,2}\s*/(?:etc|usr|boot|bin|sbin)/`),
	},
	{
		name: "system-power",
		// shutdown, reboot, halt, poweroff as standalone commands
		re: regexp.MustCompile(`(?i)(^|\s|\|)\s*(shutdown|reboot|halt|poweroff)\b`),
	},
}

// IsDestructive checks cmd against known destructive patterns.
// Returns (true, patternName) if matched, (false, "") otherwise.
// This is a UX gate only — see file header.
func IsDestructive(cmd string) (bool, string) {
	for _, p := range destructivePatterns {
		if p.re.MatchString(cmd) {
			return true, p.name
		}
	}
	return false, ""
}
