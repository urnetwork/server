package model

// QD-1162's page command of GET /network/members: a one-command admin token's body names at most the member_id it
// reads after, so a directory request carries nothing of a policy, no email among it; the route's body check, which
// runs before any consent shows the command, takes exactly what the read takes. Synthetic values only.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// An empty body and {} read the first page and {"after": <member_id>} the page past it; anything else is refused by
// the read and by the consent's body check alike, naming no value.
func TestNetworkMembersCommandArgsTakeOnlyAfter(t *testing.T) {
	after := server.NewId()
	for body, want := range map[string]*server.Id{
		"":                                    nil,
		"  ":                                  nil,
		"{}":                                  nil,
		` {"after":"` + after.String() + `"} `: &after,
	} {
		args, err := NetworkMembersCommandArgs([]byte(body))
		if err != nil || (want == nil) != (args.After == nil) || (want != nil && *args.After != *want) {
			t.Errorf("%q: %v, %v", body, args, err)
		}
		if err := session.CheckAdminCommandBody(http.MethodGet, "/network/members", []byte(body)); err != nil {
			t.Errorf("%q: the consent's check refused it: %v", body, err)
		}
	}
	for _, body := range []string{
		`{"after":"not-a-member-id"}`,
		`{"after":""}`,
		`{"after":"` + (server.Id{}).String() + `"}`,
		`{"after":5}`,
		`{"after":"` + after.String() + `","email":"person@example.com"}`,
		`{"emails":["person@example.com"]}`,
		`{"before":"` + after.String() + `"}`,
		`[]`,
		`null`,
		`"after"`,
		`{"after":"` + after.String() + `"} {}`,
		`{"after":"` + after.String() + `"}}`,
	} {
		_, err := NetworkMembersCommandArgs([]byte(body))
		if !errors.Is(err, ErrNetworkMembersCommandBody) || strings.Contains(err.Error(), "example.com") {
			t.Errorf("%q: %v", body, err)
		}
		if err := session.CheckAdminCommandBody(http.MethodGet, "/network/members", []byte(body)); err == nil {
			t.Errorf("%q: the consent's check took it", body)
		}
	}
	if !session.AdminCommandRouteRegistered(http.MethodGet, "/network/members") {
		t.Error("the members read is no admin command route")
	}
}
