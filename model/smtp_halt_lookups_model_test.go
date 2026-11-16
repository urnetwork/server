package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// §14.8's halt rule's reads on a private database (SMTP-M9-16): a network's
// complaints by arrival and by Message-ID hash with no sealed copy, its halts
// since a time and a lifted one gone, its cut sends counted, and its sending
// clients, none of another network's.
func TestSmtpHaltLookupsReadOneNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
			return
		}
		ctx := context.Background()
		store := NewSmtpEdgeStore(nil)
		networkId := sidecar.NewId()
		otherId := sidecar.NewId()
		now := server.NowUtc().Truncate(time.Millisecond)
		hash := smtpedgeservice.SmtpMessageIdHash("<one@quietnet.root.example>")
		complaint := func(network sidecar.Id, complaintType string, arrival time.Time, messageIdHash []byte) *smtpedgeservice.SmtpComplaint {
			return &smtpedgeservice.SmtpComplaint{
				ComplaintId:   sidecar.NewId(),
				NetworkId:     network,
				Reporter:      "loop.example",
				Type:          complaintType,
				ArrivalTime:   arrival,
				Domain:        "quietnet.root.example",
				MessageIdHash: messageIdHash,
				JoinResult:    "high",
				SealedReport:  []byte("sealed"),
			}
		}
		for _, row := range []*smtpedgeservice.SmtpComplaint{
			complaint(networkId, smtpedgeservice.SmtpComplaintTypeAbuse, now.Add(-30*time.Hour), hash),
			complaint(networkId, smtpedgeservice.SmtpComplaintTypeAbuse, now.Add(-time.Hour), hash),
			complaint(networkId, smtpedgeservice.SmtpComplaintTypeNotSpam, now.Add(-time.Minute), smtpedgeservice.SmtpMessageIdHash("<two@quietnet.root.example>")),
			complaint(otherId, smtpedgeservice.SmtpComplaintTypeAbuse, now.Add(-time.Minute), hash),
		} {
			if err := store.InsertComplaint(ctx, row); err != nil {
				t.Fatal(err)
			}
		}
		recent, err := store.FindComplaints(ctx, &smtpedgeservice.SmtpComplaintKey{NetworkId: networkId, Since: now.Add(-24 * time.Hour)})
		if err != nil || len(recent) != 2 || recent[0].Type != smtpedgeservice.SmtpComplaintTypeAbuse || recent[1].Type != smtpedgeservice.SmtpComplaintTypeNotSpam {
			t.Fatalf("recent %+v, %v", recent, err)
		}
		for _, row := range recent {
			if row.SealedReport != nil || row.NetworkId != networkId {
				t.Fatalf("a row %+v", row)
			}
		}
		byHash, err := store.FindComplaints(ctx, &smtpedgeservice.SmtpComplaintKey{NetworkId: networkId, MessageIdHash: hash})
		if err != nil || len(byHash) != 2 {
			t.Fatalf("by hash %d, %v", len(byHash), err)
		}

		halts := []*smtpedgeservice.SmtpHalt{
			{HaltId: sidecar.NewId(), NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerCommunityComplaint, StartTime: now.Add(-40 * 24 * time.Hour)},
			{HaltId: sidecar.NewId(), NetworkId: networkId, Label: "otter-1234", Trigger: smtpedgeservice.SmtpHaltTriggerLabelComplaint, StartTime: now.Add(-2 * time.Hour)},
			{HaltId: sidecar.NewId(), NetworkId: networkId, Trigger: smtpedgeservice.SmtpHaltTriggerRecordCuts, StartTime: now.Add(-time.Hour)},
			{HaltId: sidecar.NewId(), NetworkId: otherId, Trigger: smtpedgeservice.SmtpHaltTriggerRecordCuts, StartTime: now.Add(-time.Hour)},
		}
		for _, halt := range halts {
			if err := store.SetHalt(ctx, halt); err != nil {
				t.Fatal(err)
			}
		}
		found, err := store.FindHalts(ctx, networkId, now.Add(-30*24*time.Hour))
		if err != nil || len(found) != 2 || found[0].HaltId != halts[1].HaltId || found[0].Label != "otter-1234" || found[1].HaltId != halts[2].HaltId {
			t.Fatalf("halts %+v, %v", found, err)
		}
		if err := store.DeleteHalt(ctx, halts[1].HaltId); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteHalt(ctx, halts[1].HaltId); err != nil {
			t.Fatalf("a second delete: %v", err)
		}
		found, err = store.FindHalts(ctx, networkId, now.Add(-30*24*time.Hour))
		if err != nil || len(found) != 1 || found[0].HaltId != halts[2].HaltId {
			t.Fatalf("after the lift %+v, %v", found, err)
		}

		sends := []*smtpedgeservice.SmtpSendRecord{smtpTestSend(networkId, now.Add(-time.Hour)), smtpTestSend(networkId, now.Add(-2*time.Hour)), smtpTestSend(networkId, now.Add(-30*time.Hour)), smtpTestSend(otherId, now.Add(-time.Hour))}
		sends[0].Result = "cut"
		sends[2].Result = "cut"
		sends[3].Result = "cut"
		if err := store.InsertSends(ctx, sends); err != nil {
			t.Fatal(err)
		}
		cuts, err := store.CountSends(ctx, networkId, now.Add(-24*time.Hour), "cut")
		if err != nil || cuts != 1 {
			t.Fatalf("cuts %d, %v", cuts, err)
		}
		clientIds, err := store.SendingClients(ctx, networkId, now.Add(-24*time.Hour))
		slices.SortFunc(clientIds, func(a sidecar.Id, b sidecar.Id) int { return a.Cmp(b) })
		want := []sidecar.Id{sends[0].ClientId, sends[1].ClientId}
		slices.SortFunc(want, func(a sidecar.Id, b sidecar.Id) int { return a.Cmp(b) })
		if err != nil || !slices.Equal(clientIds, want) {
			t.Fatalf("clients %v, %v", clientIds, err)
		}
	})
}
