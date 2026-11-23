// The API key notice's pure rules: how a key's name is shown, and which code
// may create or delete a key.
package model

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// Every statement in the server's source that creates or deletes an API key is
// in CreateApiKey or DeleteApiKey, which owe the API key notice in their
// transaction, in RemoveExpiredApiKeys, whose expiry is no change anyone made
// and owes none (§7.8's expiry), or in apikey.Testing_CreateApiKey, a test
// fixture. A new way to create or delete a key must owe the notice too, and be
// counted here once it does.
func TestEveryApiKeyWriteOwesTheSecurityNotice(t *testing.T) {
	write := regexp.MustCompile(`(?i)\b(insert\s+into|delete\s+from|truncate(\s+table)?)\s+account_api_key\b`)
	want := map[string]int{
		// CreateApiKey's insert, with MG0's columns and without them,
		// DeleteApiKey's delete, and RemoveExpiredApiKeys' delete
		"model/account_api_key_model.go": 4,
		// Testing_CreateApiKey
		"apikey/apikey.go": 1,
	}
	root := ".."
	got := map[string]int{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if count := len(write.FindAllIndex(source, -1)); 0 < count {
			relativePath, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			got[filepath.ToSlash(relativePath)] = count
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, count := range got {
		if want[path] != count {
			t.Errorf("%s writes account_api_key %d times, want %d: a new way to create or delete an API key must owe the security notice (security_notice_api_key.go)", path, count, want[path])
		}
	}
	for path, count := range want {
		if got[path] != count {
			t.Errorf("%s writes account_api_key %d times, want %d", path, got[path], count)
		}
	}
}

// A key's name is free text that whoever made the key chose, so a notice keeps
// it to one line in reading order: line breaks, tabs, direction overrides and
// zero-width marks count as space, space runs collapse, and the ends are
// trimmed. A long name is cut short of the limit and ends in an ellipsis.
// Applying the rule again changes nothing.
func TestSecurityNoticeApiKeyNameKeepsOneLine(t *testing.T) {
	maxNameLength := DefaultSecurityApiKeyNoticeSettings().MaxNameLength
	long := strings.Repeat("é", MaxApiKeyNameLength)
	for _, c := range []struct {
		name string
		key  string
		want string
	}{
		{"plain", "deploy-bot", "deploy-bot"},
		{"spaces inside", "deploy bot 2", "deploy bot 2"},
		{"line breaks and tabs", " ops\r\n\tbot \n", "ops bot"},
		{"a forged line", "ci\nChange: Deleted", "ci Change: Deleted"},
		{"direction override", "evil\u202ecod.exe", "evil cod.exe"},
		{"zero-width marks", "zero\u200bwidth\ufeff", "zero width"},
		{"line separator", "one\u2028two", "one two"},
		{"only space", " \t\n", ""},
		{"empty", "", ""},
		{"at the limit", strings.Repeat("k", maxNameLength), strings.Repeat("k", maxNameLength)},
		{"two-byte characters past the limit", long, strings.Repeat("é", maxNameLength-1) + "…"},
		{"a cut at a space", strings.Repeat("k", maxNameLength-2) + " " + strings.Repeat("k", 10), strings.Repeat("k", maxNameLength-2) + "…"},
	} {
		got := SecurityNoticeApiKeyName(c.key)
		if got != c.want {
			t.Errorf("%s: SecurityNoticeApiKeyName(%q) = %q, want %q", c.name, c.key, got, c.want)
		}
		if again := SecurityNoticeApiKeyName(got); again != got {
			t.Errorf("%s: applied again, %q became %q", c.name, got, again)
		}
		if maxNameLength < utf8.RuneCountInString(got) {
			t.Errorf("%s: %q is longer than %d characters", c.name, got, maxNameLength)
		}
		if strings.ContainsAny(got, "\r\n\t\u202e\u200b\ufeff\u2028") {
			t.Errorf("%s: %q keeps a control or format character", c.name, got)
		}
	}
}

// The name's limit is the settings' MaxNameLength: a shorter limit cuts the
// name short of it, and a limit below 1 counts as 1, which shows a longer name
// as the ellipsis alone and a one-character name whole. Applying the rule
// again under the same settings changes nothing.
func TestSecurityNoticeApiKeyNameTakesItsLimitFromTheSettings(t *testing.T) {
	for _, c := range []struct {
		maxNameLength int
		key           string
		want          string
	}{
		{4, "deploy-bot", "dep…"},
		{4, "ci k", "ci k"},
		{4, "ci  bot", "ci…"},
		{1, "deploy-bot", "…"},
		{0, "deploy-bot", "…"},
		{-1, "deploy-bot", "…"},
		{0, "k", "k"},
		{0, "", ""},
	} {
		settings := &SecurityApiKeyNoticeSettings{MaxNameLength: c.maxNameLength}
		got := securityNoticeApiKeyNameOf(c.key, settings)
		if got != c.want {
			t.Errorf("under a limit of %d, %q is shown as %q, want %q", c.maxNameLength, c.key, got, c.want)
		}
		if again := securityNoticeApiKeyNameOf(got, settings); again != got {
			t.Errorf("under a limit of %d, applied again, %q became %q", c.maxNameLength, got, again)
		}
	}
}
