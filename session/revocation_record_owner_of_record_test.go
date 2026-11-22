package session

// The revocation record's mark of a team's tagged lineage (QD-1194): a client
// field's fourth part "t", which only the SQL fallback writes, once the state
// query took the credential by its exception. The verdict takes the mark only
// with a credential whose own claims the exception takes, and never past a
// tombstone, a deleted network or another user.

import (
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The verdict of one HMGET reply for credential, each field the query reads
// given its value in fieldValues, nil for an absent field.
func testOwnerOfRecordVerdict(credential *ByJwt, fieldValues map[string]any) revocationVerdict {
	query := newRevocationRecordQuery(DefaultRevocationRecordSettings(), credential, *credential.RootClientId, nil)
	values := make([]any, len(query.fields))
	for index, field := range query.fields {
		values[index] = fieldValues[field]
	}
	verdict, _ := query.verdict(credential, values)
	return verdict
}

func TestRevocationRecordVerdictTakesTheTaggedLineageMarkWithTheExceptionsClaims(t *testing.T) {
	networkId, ownerOfRecordId, deviceId := server.NewId(), server.NewId(), server.NewId()
	// the owner of record's change time, past every token
	changeMicros := strconv.FormatInt(time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC).UnixMicro(), 10)
	issuer := NewByJwt(networkId, ownerOfRecordId, "team", false, false)
	issuer.SetStrictExp(true)
	tagged := issuer.Client(deviceId, server.NewId())
	window := tagged.Client(deviceId, server.NewId())
	if *tagged.RootClientId != *tagged.ClientId || *window.RootClientId != *tagged.ClientId {
		t.Fatalf("the lineage's root claims: %v %v", tagged.RootClientId, window.RootClientId)
	}
	taggedField := revocationClientField(*tagged.ClientId)
	windowField := revocationClientField(*window.ClientId)
	rootField := revocationRootField(*tagged.ClientId)
	taggedMarked := "a " + deviceId.String() + " - t"
	windowMarked := "a " + deviceId.String() + " " + tagged.ClientId.String() + " t"
	network := func(fields map[string]any) map[string]any {
		fields[revocationFieldUser] = ownerOfRecordId.String()
		fields[revocationFieldCredential] = changeMicros
		return fields
	}
	withoutStrictExp := tagged.Renew()
	withoutStrictExp.StrictExp = false
	withMember := tagged.Renew()
	memberId := server.NewId()
	withMember.MemberId = &memberId
	otherUser := network(map[string]any{taggedField: taggedMarked})
	otherUser[revocationFieldUser] = server.NewId().String()
	deleted := network(map[string]any{taggedField: taggedMarked})
	deleted[revocationFieldDeleted] = "1"

	for _, c := range []struct {
		name       string
		credential *ByJwt
		fields     map[string]any
		verdict    revocationVerdict
	}{
		{"the marked tagged client", tagged, network(map[string]any{taggedField: taggedMarked}), revocationValid},
		{"its marked window client, its root active", window, network(map[string]any{windowField: windowMarked, rootField: revocationActive}), revocationValid},
		{"the marked client without strict_exp", withoutStrictExp, network(map[string]any{taggedField: taggedMarked}), revocationRotated},
		{"the marked client with a member claim", withMember, network(map[string]any{taggedField: taggedMarked}), revocationRotated},
		{"an unmarked client", tagged, network(map[string]any{taggedField: "a " + deviceId.String() + " -"}), revocationRotated},
		{"another fourth part", tagged, network(map[string]any{taggedField: "a " + deviceId.String() + " - z"}), revocationMiss},
		{"a fifth part", tagged, network(map[string]any{taggedField: taggedMarked + " t"}), revocationMiss},
		{"a tombstoned client", tagged, network(map[string]any{taggedField: revocationTombstone}), revocationNoActiveRow},
		{"its window client under a tombstoned root", window, network(map[string]any{windowField: windowMarked, rootField: revocationTombstone}), revocationNoActiveRow},
		{"its window client, its root unread", window, network(map[string]any{windowField: windowMarked}), revocationMiss},
		{"another user's record", tagged, otherUser, revocationNoActiveRow},
		{"a deleted network", tagged, deleted, revocationNoActiveRow},
		{"no network fields", tagged, map[string]any{taggedField: taggedMarked}, revocationMiss},
	} {
		if verdict := testOwnerOfRecordVerdict(c.credential, c.fields); verdict != c.verdict {
			t.Errorf("%s: verdict %d, want %d", c.name, verdict, c.verdict)
		}
	}
}
