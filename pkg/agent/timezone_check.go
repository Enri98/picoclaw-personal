package agent

import (
	"fmt"
	"time"
)

// CheckTimezone logs a warning if time.Local does not match the configured
// expected timezone, or if it has fallen back to UTC unexpectedly. Returns
// true if everything is fine, false if a warning was logged. Useful for both
// agent startup and for the picoclaw doctor subcommand to call.
func CheckTimezone(expected string) (ok bool, message string) {
	return checkTimezoneStrings(time.Now().Location().String(), expected)
}

// checkTimezoneStrings is the pure, testable core of CheckTimezone.
func checkTimezoneStrings(actual, expected string) (ok bool, message string) {
	if expected == "" {
		return true, ""
	}
	if actual == "UTC" && expected != "UTC" {
		return false, fmt.Sprintf(
			"time.Local is UTC; expected %q. Set the system timezone with: sudo timedatectl set-timezone %s",
			expected, expected,
		)
	}
	if actual != expected {
		return false, fmt.Sprintf(
			"time.Local is %q; expected %q. Reminders and the daily briefing will use %q.",
			actual, expected, actual,
		)
	}
	return true, fmt.Sprintf("timezone OK: %s", actual)
}
