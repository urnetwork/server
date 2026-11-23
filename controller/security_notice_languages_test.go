// The security notices ship in every language the account emails ship in. The
// account emails are localized by layout: each language is a directory under
// email_templates that holds its own _layout.html, whose lang attribute names
// it, and its own copy of each template. Today that is one language, English, at
// the root (EMAIL1.md §7 designs the others). A language added later must carry
// the security notices along with the other account emails, and its notices
// about a network must name it (QD-1216).
package controller

import (
	htmltemplate "html/template"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
	texttemplate "text/template"

	"github.com/urnetwork/server/model"
)

// The security notice templates (model/security_notice_model.go and
// auth_password_set), by name, and the parts of each email.
var securityNoticeTemplateNames = []string{
	"security_payout_wallet_changed",
	"security_sign_in_changed",
	"security_api_key_changed",
	"security_auth_key_changed",
	"security_network_authority_changed",
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

// The template rendered from the language's directory as RenderEmailTemplate
// renders it from the root: the subject on its own, trimmed to one line, and
// each body inside the language's layout.
func renderAccountEmailInLanguage(t *testing.T, dir string, template Template) (subject string, bodyHtml string, bodyText string) {
	t.Helper()
	funcs := TemplateFuncs(template)
	subjectFile, err := fs.ReadFile(emailTemplates, path.Join(dir, template.Name()+".subject.txt"))
	if err != nil {
		t.Fatalf("%s (%s): %v", template.Name(), dir, err)
	}
	subjectTemplate, err := texttemplate.New("subject").Funcs(funcs).Parse(string(subjectFile))
	if err != nil {
		t.Fatalf("%s (%s): subject: %v", template.Name(), dir, err)
	}
	subjectOut := &strings.Builder{}
	if err := subjectTemplate.Execute(subjectOut, template); err != nil {
		t.Fatalf("%s (%s): subject: %v", template.Name(), dir, err)
	}
	subject = strings.TrimSpace(subjectOut.String())

	htmlTemplate, err := htmltemplate.New("_layout.html").Funcs(funcs).ParseFS(
		emailTemplates,
		path.Join(dir, "_layout.html"),
		path.Join(dir, template.Name()+".html"),
	)
	if err != nil {
		t.Fatalf("%s (%s): html: %v", template.Name(), dir, err)
	}
	htmlOut := &strings.Builder{}
	if err := htmlTemplate.ExecuteTemplate(htmlOut, "_layout.html", template); err != nil {
		t.Fatalf("%s (%s): html: %v", template.Name(), dir, err)
	}

	textTemplate, err := texttemplate.New("_layout.txt").Funcs(funcs).ParseFS(
		emailTemplates,
		path.Join(dir, "_layout.txt"),
		path.Join(dir, template.Name()+".txt"),
	)
	if err != nil {
		t.Fatalf("%s (%s): text: %v", template.Name(), dir, err)
	}
	textOut := &strings.Builder{}
	if err := textTemplate.ExecuteTemplate(textOut, "_layout.txt", template); err != nil {
		t.Fatalf("%s (%s): text: %v", template.Name(), dir, err)
	}
	return subject, htmlOut.String(), textOut.String()
}

// Every security notice renders in every language, each on a one-line
// subject, and each notice about a network names it in its subject, its html
// part and its text part (QD-1216), whether one change or several. The root's
// language renders as the sender renders it.
func TestSecurityNoticesRenderAndNameTheirNetworkInEveryAccountEmailLanguage(t *testing.T) {
	networkNotices := func() []Template {
		wallet := securityWalletNoticeSample()
		apiKey := securityApiKeyNoticeSample(model.SecurityApiKeyCreated, model.SecurityApiKeyDeleted)
		authKey := securityAuthKeyNoticeSample(model.SecurityAuthKeyRevoked)
		authority := securityNetworkAuthorityNoticeSample()
		return []Template{wallet, apiKey, authKey, authority}
	}
	accountNotices := func() []Template {
		return []Template{
			securitySignInNoticeSample(model.SecuritySignInAdded),
			&AuthPasswordSetTemplate{},
		}
	}
	for language, dir := range accountEmailLanguages(t) {
		rendered := map[string]bool{}
		for _, notice := range append(networkNotices(), accountNotices()...) {
			subject, bodyHtml, bodyText := renderAccountEmailInLanguage(t, dir, notice)
			rendered[notice.Name()] = true
			if subject == "" || strings.ContainsAny(subject, "\r\n") {
				t.Errorf("%s: %s's subject is not one line: %q", language, notice.Name(), subject)
			}
			if dir == emailTemplateDir {
				wantSubject, wantHtml, wantText, err := RenderEmailTemplate(notice)
				if err != nil {
					t.Fatalf("%s: %s: %v", language, notice.Name(), err)
				}
				if subject != wantSubject || bodyHtml != wantHtml || bodyText != wantText {
					t.Errorf("%s: %s renders from its directory unlike the sender", language, notice.Name())
				}
			}
			if _, ok := notice.(accountMessageNetworkNamer); !ok {
				continue
			}
			for part, body := range map[string]string{"subject": subject, "html": bodyHtml, "text": bodyText} {
				if !strings.Contains(body, securityNoticeSampleNetworkName) {
					t.Errorf("%s: %s's %s does not name the network %s:\n%s", language, notice.Name(), part, securityNoticeSampleNetworkName, body)
				}
			}
		}
		for _, name := range securityNoticeTemplateNames {
			if !rendered[name] {
				t.Errorf("%s: %s was not rendered", language, name)
			}
		}
	}
}
