package controller

import (
	"errors"
	"strings"
	"testing"
)

func TestCompetitionLogErrorIsOneBoundedLine(t *testing.T) {
	if got := competitionLogError(nil); got != "<nil>" {
		t.Fatalf("nil error logged as %q", got)
	}
	if got := competitionLogError(errors.New("round\n  baseline\tmissing ")); got != "round baseline missing" {
		t.Fatalf("multi-line error logged as %q", got)
	}
	got := competitionLogError(errors.New(strings.Repeat("x", 1000)))
	if len(got) != 403 || !strings.HasSuffix(got, "...") || strings.Contains(got, "\n") {
		t.Fatalf("long error not bounded to one 400-byte line: %d bytes", len(got))
	}
}
