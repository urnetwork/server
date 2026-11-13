// The security notices ship in every language the account emails ship in. The
// account emails are localized by layout: each language is a directory under
// email_templates that holds its own _layout.html, whose lang attribute names
// it, and its own copy of each template. Today that is one language, English, at
// the root (EMAIL1.md §7 designs the others). A language added later must carry
// the security notices along with the other account emails.
package controller

import (
	"io/fs"
	"path"
	"regexp"
	"testing"
)

// The security notice templates (model/security_notice_model.go and
// auth_password_set), by name, and the parts of each email.
var securityNoticeTemplateNames = []string{
	"security_payout_wallet_changed",
	"security_sign_in_changed",
	"security_api_key_changed",
	"auth_password_set",
}

var accountEmailParts = []string{".subject.txt", ".html", ".txt"}

// The languages the account emails ship in: each directory with a layout, by
// the language its layout declares.
func accountEmailLanguages(t *testing.T) map[string]string {
	t.Helper()
	langAttribute := regexp.MustCompile(`<html[^>]*\blang="([^"]+)"`)
	languages := map[string]string{}
	err := fs.WalkDir(emailTemplates, emailTemplateDir, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || path.Base(filePath) != "_layout.html" {
			return nil
		}
		layout, err := fs.ReadFile(emailTemplates, filePath)
		if err != nil {
			return err
		}
		match := langAttribute.FindSubmatch(layout)
		if match == nil {
			t.Errorf("%s declares no language", filePath)
			return nil
		}
		languages[string(match[1])] = path.Dir(filePath)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(languages) == 0 {
		t.Fatal("found no account email layout")
	}
	return languages
}

func TestSecurityNoticeTemplatesExistForEveryAccountEmailLanguage(t *testing.T) {
	for language, dir := range accountEmailLanguages(t) {
		// the language ships account emails: the welcome is one of them
		if _, err := fs.Stat(emailTemplates, path.Join(dir, "network_welcome.html")); err != nil {
			t.Errorf("%s (%s): the layout's directory holds no account email", language, dir)
			continue
		}
		for _, name := range securityNoticeTemplateNames {
			for _, part := range accountEmailParts {
				filePath := path.Join(dir, name+part)
				if _, err := fs.Stat(emailTemplates, filePath); err != nil {
					t.Errorf("%s: the security notice %s has no %s (%s)", language, name, part, filePath)
				}
			}
		}
	}
}
