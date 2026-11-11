package controller

import "strings"

// The readiness answers (readiness_failed: "competition hold status is
// unavailable" and the like) hide the error behind them, so the api logs it.
// On 2026-11-11 every main api replica answered that way with no log line, and
// the cause could not be read from the serving processes. One line, collapsed
// whitespace, at most 400 bytes; errors here carry no credentials.
func competitionLogError(err error) string {
	if err == nil {
		return "<nil>"
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}
