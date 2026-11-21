package oauth

// QD-1162 through ur-cli's authorization and api's session: a members page command names the member_id it reads after
// in its body, which the consent shows and the token's cmd digest binds, so the token authenticates that page's request
// alone, never another page's, the first page's or a query cursor's; and a body carrying an email is refused before
// any consent shows it. Synthetic values only.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A request of method at url carrying body, as ur-cli sends a command: the token and a fresh proof by login's key.
func newTestAdminRequestOf(t testing.TB, login *testUrcliLogin, token string, method string, url string, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// a documentation address (RFC 5737)
	request.RemoteAddr = "192.0.2.1:1"
	request.Header.Set("Authorization", "DPoP "+token)
	request.Header.Set(session.DpopHeader, session.Testing_SignDpopProof(login.privateKey, method, strings.Split(url, "?")[0], token, server.NewId().String(), time.Now()))
	return request
}

func TestMembersPageCommandBindsItsAfter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		ctx := context.Background()
		after, other := server.NewId(), server.NewId()
		body := `{"after":"` + after.String() + `"}`
		command := &sidecar.AdminCommand{Method: http.MethodGet, Path: "/network/members", Body: []byte(body)}
		login := newTestUrcliLogin(t, command)
		login.request.Scopes = []string{UrcliScopeAll}
		if _, err := ValidateUrcliRequest(login.request); err != nil {
			t.Fatalf("a later page's command: %v", err)
		}
		for _, refused := range []string{
			`{"after":"` + after.String() + `","email":"person@example.com"}`,
			`{"emails":["person@example.com"]}`,
			`{"after":"not-a-member-id"}`,
		} {
			refusedLogin := newTestUrcliLogin(t, &sidecar.AdminCommand{Method: http.MethodGet, Path: "/network/members", Body: []byte(refused)})
			refusedLogin.request.Scopes = []string{UrcliScopeAll}
			_, err := ValidateUrcliRequest(refusedLogin.request)
			if !errors.Is(err, ErrUrcliCommandRefused) {
				t.Errorf("%s: %v", refused, err)
			} else if strings.Contains(err.Error(), "example.com") {
				t.Errorf("the refusal names the email: %s", err)
			}
		}

		cmd, err := command.Digest()
		if err != nil {
			t.Fatal(err)
		}
		token, _, err := MintAdminToken(&MintAdminTokenArgs{
			NetworkId: server.NewId(), MemberId: server.NewId(), Scopes: []string{UrcliScopeAll},
			Jkt: login.jkt, Cmd: cmd, Duration: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		url := testUrcliApiOrigin + "/network/members"
		use, err := authenticateAdminToken(ctx, newTestAdminRequestOf(t, login, token, http.MethodGet, url, body), token)
		if err != nil || use.Cmd != cmd {
			t.Fatalf("the page's own request: %v", err)
		}
		for name, request := range map[string]*http.Request{
			"another page":   newTestAdminRequestOf(t, login, token, http.MethodGet, url, `{"after":"`+other.String()+`"}`),
			"the first page": newTestAdminRequestOf(t, login, token, http.MethodGet, url, ""),
			"a query cursor": newTestAdminRequestOf(t, login, token, http.MethodGet, url+"?after="+after.String(), ""),
			"both":           newTestAdminRequestOf(t, login, token, http.MethodGet, url+"?after="+other.String(), body),
		} {
			if _, err := authenticateAdminToken(ctx, request, token); !errors.Is(err, ErrInvalidAdminToken) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}
