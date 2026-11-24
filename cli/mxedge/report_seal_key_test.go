package main

// The platform block's report seal key (plan-SMTP-report-seal-key, QD-075; §14.8): the key source needs api's url and
// the platform listener's own bearer file, naming what is missing and never the bearer; and an intake given it
// through sealThrough seals a network's aggregate through api's real report-key route to the key api answers, which
// the network mail key's private half opens, its other seams kept as they were.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/router"
)

func TestPlatformReportKeySourceNeedsTheUrlAndItsOwnBearer(t *testing.T) {
	if _, err := newPlatformReportKeySource(""); err == nil || !strings.Contains(err.Error(), "no api url") {
		t.Fatalf("no api url: %v", err)
	}
	fileName := server.InternalGateCredentialFileName(server.InternalGateCallerMxedgePlatform)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if _, err := newPlatformReportKeySource("https://api.report.example"); err == nil || !strings.Contains(err.Error(), fileName) {
			t.Fatalf("no bearer file: %v", err)
		}
		bearer := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, server.DefaultInternalGateSettings().BearerByteCount))
		pop := server.Vault.PushSimpleResource(fileName, []byte("current: "+bearer+"\n"))
		defer pop()
		source, err := newPlatformReportKeySource("https://api.report.example")
		if err != nil || source == nil {
			t.Fatalf("with the bearer: %v", err)
		}
		if _, err := newPlatformReportKeySource("api.report.example/"); err == nil || strings.Contains(err.Error(), bearer) {
			t.Fatalf("an api url that cannot be used: %v", err)
		}
	})
}

func TestReportIntakeSealsThroughApisReportKeyRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	keys, err := sidecartest.NewSmtpReportKeyFixture(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	world := newTestReportWorld()
	networkId := sidecar.NewId()
	recipient := world.tokens.Add(networkId) + "@rpt.report.example"

	// api's route on a verify file holding the platform listener's digest, answering the fixture's key for the network
	raw := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	platform := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(platform))
	verifier, err := server.NewInternalGateVerifier([]byte("mxedge-platform: {class: mxedge-platform, sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	routes := handlers.NewInternalGateReportKeyHandlers(&handlers.InternalGateReportKeyDeps{
		Verifier: verifier,
		Key: func(ctx context.Context, askedNetworkId sidecar.Id) (*smtpedgeservice.SmtpReportSealKey, error) {
			if askedNetworkId != networkId {
				return nil, nil
			}
			reply, err := keys.DropKeyReply(ctx, 0)
			if err != nil {
				return nil, err
			}
			return smtpedgeservice.CheckSmtpReportSealKey(keys.View(), keys.RepoId, reply, keys.Clock.Now(), sidecar.DefaultGkmSettings().DropKeyLifetime)
		},
	}).Routes()
	api := httptest.NewServer(router.NewRouter(ctx, routes))
	defer api.Close()
	source, err := smtpedgeservice.NewSmtpReportKeyGateSource(&smtpedgeservice.SmtpReportKeyGateSourceArgs{ApiUrl: api.URL, Bearer: platform, Clock: keys.Clock})
	if err != nil {
		t.Fatal(err)
	}

	store := sidecartest.NewFakeSmtpEdgeStore(nil, nil)
	intake, err := newPlatformReportIntake(store, testReportIntakeSettings(), world.txt, world.tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := intake.sealThrough(nil); err == nil {
		t.Fatal("an intake sealed through no source")
	}
	if err := intake.sealThrough(source); err != nil {
		t.Fatal(err)
	}
	if intake.deps.Tokens == nil || intake.deps.Txt == nil || intake.deps.Sink == nil || intake.deps.Keys != source {
		t.Fatal("sealThrough lost a seam or kept no source")
	}
	report := world.dmarcAggregate(recipient, "reporter.example", "sealed-1", []*sidecartest.SmtpFixtureDmarcRecord{
		{SourceIp: "192.0.2.10", Count: 5, HeaderFrom: "quietnet.root.example", Disposition: "none", DkimAligned: "pass", SpfAligned: "fail", DkimDomain: "quietnet.root.example", DkimSelector: "abcdefghijklmnopqrstuvwxyz", DkimResult: "pass"},
	})
	outcome, err := world.accept(t, intake, recipient, report)
	if err != nil || outcome != mxedgeservice.PlatformOutcomeTaken {
		t.Fatalf("%v, %v", outcome, err)
	}
	complaints := store.Complaints()
	if len(complaints) != 1 || complaints[0].NetworkId != networkId || complaints[0].Type != smtpedgeservice.SmtpComplaintTypeRua {
		t.Fatalf("%d sealed copies", len(complaints))
	}
	dropKey, err := keys.DropPrivateKey(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := sidecartest.OpenSmtpSealedReportFixture(dropKey, networkId, world.arrival, complaints[0].SealedReport)
	if err != nil || !bytes.Equal(opened, report) {
		t.Fatalf("the copy does not open under the network mail key: %v", err)
	}
}
