//go:build sidecar_pending

package model

// §7.5's roles at the server (TEAM-M6-03), on Postgres with MG6: the role read
// is network_member's mirror, and only a landed log entry changes it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// MemberRole reads the mirrored role of a live membership, and nothing for a
// pending one, another network's or none.
func TestMemberRoleReadsTheMirror(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		auditor := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleAuditor, false)
		pending := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleMember, true)

		role, ok := MemberRole(ctx, teamId, auditor.MemberId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, role, NetworkMemberRoleAuditor)
		_, ok = MemberRole(ctx, teamId, pending.MemberId)
		connect.AssertEqual(t, ok, false)
		_, ok = MemberRole(ctx, server.NewId(), auditor.MemberId)
		connect.AssertEqual(t, ok, false)
		connect.AssertEqual(t, NetworkIsTeam(ctx, teamId), true)
		connect.AssertEqual(t, NetworkIsTeam(ctx, server.NewId()), false)
	})
}

// A role changes only from a landed entry: the effects step mirrors a role the
// log set (an AdminSet's or MemberAdmit's), a role the log did not set is
// untouched, and no other code writes network_member's role after the row is
// made.
func TestARoleChangesOnlyFromALandedEntry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		member := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleMember, false)
		other := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleAuditor, false)

		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(mirrorConfigMemberRolesInTx(ctx, tx, teamId, map[server.Id]bridge.MemberRole{
				member.MemberId: bridge.MemberRole_MEMBER_ROLE_ADMIN,
				// a role this build cannot read mirrors nothing
				other.MemberId: bridge.MemberRole(99),
			}))
		})
		role, ok := MemberRole(ctx, teamId, member.MemberId)
		connect.AssertEqual(t, ok && role == NetworkMemberRoleAdmin, true)
		role, ok = MemberRole(ctx, teamId, other.MemberId)
		connect.AssertEqual(t, ok && role == NetworkMemberRoleAuditor, true)
	})

	// the source: only the effects step updates a role
	writer := regexp.MustCompile(`(?is)UPDATE\s+network_member\s+SET[^;` + "`" + `]*\brole\b`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	writers := []string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if writer.Match(src) {
			writers = append(writers, file)
		}
	}
	connect.AssertEqual(t, strings.Join(writers, ","), "config_log_effects.go")
}
