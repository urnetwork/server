package model

import (
	"strings"
	"testing"
)

// A record's result is a word a row holds: lowercase letters, digits and underscores, at most the settings'
// ResultMaxByteCount, by default the result column's width, varchar(32); the write reads the bound from the
// settings it is given.
func TestMailConnectionResultWordFitsItsSettings(t *testing.T) {
	settings := DefaultMailConnectionSettings()
	if settings.ResultMaxByteCount != 32 {
		t.Fatalf("the default width %d is not the column's", settings.ResultMaxByteCount)
	}
	for result, valid := range map[string]bool{
		"relayed":               true,
		"tls_failed_2":          true,
		strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false,
		"":                      false,
		"Spliced!":              false,
		"relay ed":              false,
	} {
		if mailConnectionResultValid(result, settings) != valid {
			t.Errorf("%q valid %t", result, !valid)
		}
	}
	settings.ResultMaxByteCount = len("relayed") - 1
	if mailConnectionResultValid("relayed", settings) {
		t.Error("a word past a narrower setting is valid")
	}
}
