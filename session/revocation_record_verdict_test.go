package session

// The record's reading of a team member's fields (owner decision 561), with no
// store: what one HMGET reply decides for a member's credential, on api's path
// (verdict) and on sockets' miss (presentRefusal).

import (
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An m: value: a tombstone, a membership of another user and a reauth deadline
// at or before now refuse; a well-formed membership inside its deadline, or
// with none, is admitted; anything else is malformed, which the record cannot
// answer.
func TestRevocationMembershipVerdict(t *testing.T) {
	userId, otherUserId := server.NewId(), server.NewId()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Millisecond)
	later := now.Add(time.Millisecond)
	for _, c := range []struct {
		name     string
		value    string
		refuses  bool
		admitted bool
	}{
		{name: "a tombstone", value: revocationTombstone, refuses: true},
		{name: "no deadline", value: revocationMembershipValue(userId, nil), admitted: true},
		{name: "inside its deadline", value: revocationMembershipValue(userId, &later), admitted: true},
		{name: "at its deadline", value: revocationMembershipValue(userId, &now), refuses: true},
		{name: "past its deadline", value: revocationMembershipValue(userId, &earlier), refuses: true},
		{name: "another user", value: revocationMembershipValue(otherUserId, nil), refuses: true},
		{name: "no deadline field", value: "a " + userId.String()},
		{name: "a deadline that is no number", value: "a " + userId.String() + " soon"},
		{name: "an unknown state", value: "p " + userId.String() + " 0"},
		{name: "empty", value: ""},
	} {
		refuses, admitted := revocationMembershipVerdict(c.value, userId, now)
		if refuses != c.refuses || admitted != c.admitted {
			t.Errorf("%s (%q): refuses %v, admitted %v; want %v, %v", c.name, c.value, refuses, admitted, c.refuses, c.admitted)
		}
	}
	if value := revocationMembershipValue(userId, nil); value != "a "+userId.String()+" 0" {
		t.Errorf("a membership with no deadline reads %q", value)
	}
	if value := revocationMembershipValue(userId, &now); value != "a "+userId.String()+" "+strconv.FormatInt(now.UnixMilli(), 10) {
		t.Errorf("a membership with a deadline reads %q", value)
	}
}

// A member's client credential reads m: and u: in place of n:user and n:cred.
// The record answers it valid only with all its fields; a tombstone, another
// user or a passed deadline refuses whatever else it lacks, and a deleted
// network, a removed client or a later cutoff refuses; a missing field is
// otherwise a miss, which sockets still refuses when another field it holds
// does.
func TestRevocationRecordQueryReadsAMembersFields(t *testing.T) {
	networkId, userId, memberId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	member := NewByJwt(networkId, userId, "verdict", false, false)
	member.MemberId = &memberId
	client := member.Client(deviceId, clientId)
	query := newRevocationRecordQuery(DefaultRevocationRecordSettings(), client, *client.RootClientId, nil)
	wantFields := []string{revocationMemberField(memberId), revocationUserField(userId), revocationFieldDeleted, revocationClientField(clientId)}
	if len(query.fields) != len(wantFields) {
		t.Fatalf("a member's client reads %v, want %v", query.fields, wantFields)
	}
	for i := range wantFields {
		if query.fields[i] != wantFields[i] {
			t.Fatalf("a member's client reads %v, want %v", query.fields, wantFields)
		}
	}
	owner := NewByJwt(networkId, userId, "verdict", false, false).Client(deviceId, clientId)
	ownerQuery := newRevocationRecordQuery(DefaultRevocationRecordSettings(), owner, *owner.RootClientId, nil)
	if ownerQuery.fields[0] != revocationFieldUser || ownerQuery.fields[1] != revocationFieldCredential || ownerQuery.member {
		t.Fatalf("an owner's client reads %v", ownerQuery.fields)
	}

	now := client.CreateTime.Add(time.Minute)
	later := now.Add(time.Hour)
	passed := now.Add(-time.Second)
	admitted := revocationMembershipValue(userId, &later)
	cutoffBefore := strconv.FormatInt(client.CreateTime.Add(-time.Second).UnixMicro(), 10)
	cutoffAfter := strconv.FormatInt(client.CreateTime.Add(time.Second).UnixMicro(), 10)
	clientActive := revocationActive + " " + deviceId.String() + " " + revocationAbsent
	// m:, u:, n:del, c:; nil is a missing field
	reply := func(membership any, cutoff any, deleted any, clientValue any) []any {
		return []any{membership, cutoff, deleted, clientValue}
	}
	for _, c := range []struct {
		name    string
		values  []any
		verdict revocationVerdict
		present revocationVerdict
	}{
		{name: "every field", values: reply(admitted, cutoffBefore, nil, clientActive), verdict: revocationValid},
		{name: "a removed membership, its cutoff lapsed", values: reply(revocationTombstone, nil, nil, clientActive), verdict: revocationNoActiveRow},
		{name: "another user's membership", values: reply(revocationMembershipValue(server.NewId(), nil), cutoffBefore, nil, clientActive), verdict: revocationNoActiveRow},
		// the token's user has no cutoff field where the membership is another's
		{name: "another user's membership, no cutoff of the token's user", values: reply(revocationMembershipValue(server.NewId(), nil), nil, nil, clientActive), verdict: revocationNoActiveRow},
		{name: "a passed reauth deadline", values: reply(revocationMembershipValue(userId, &passed), cutoffBefore, nil, clientActive), verdict: revocationNoActiveRow},
		{name: "a passed reauth deadline, its cutoff lapsed", values: reply(revocationMembershipValue(userId, &passed), nil, nil, clientActive), verdict: revocationNoActiveRow},
		{name: "a deleted network", values: reply(nil, nil, "1", nil), verdict: revocationNoActiveRow},
		{name: "a removed client", values: reply(admitted, cutoffBefore, nil, revocationTombstone), verdict: revocationNoActiveRow},
		{name: "a cutoff after its creation", values: reply(admitted, cutoffAfter, nil, clientActive), verdict: revocationRotated},
		{name: "a lapsed membership", values: reply(nil, cutoffBefore, nil, clientActive), verdict: revocationMiss, present: revocationMiss},
		{name: "a lapsed cutoff", values: reply(admitted, nil, nil, clientActive), verdict: revocationMiss, present: revocationMiss},
		{name: "a lapsed membership and a later cutoff", values: reply(nil, cutoffAfter, nil, clientActive), verdict: revocationMiss, present: revocationRotated},
		{name: "a lapsed membership and a removed client", values: reply(nil, cutoffBefore, nil, revocationTombstone), verdict: revocationMiss, present: revocationNoActiveRow},
		{name: "a malformed membership", values: reply("a "+userId.String(), cutoffBefore, nil, clientActive), verdict: revocationMiss, present: revocationMiss},
	} {
		verdict, _ := query.verdict(client, c.values, now)
		if verdict != c.verdict {
			t.Errorf("%s: verdict %d, want %d", c.name, verdict, c.verdict)
		}
		if verdict == revocationMiss {
			if present, _ := query.presentRefusal(client, c.values, now); present != c.present {
				t.Errorf("%s: sockets' present refusal %d, want %d", c.name, present, c.present)
			}
		}
	}
}
