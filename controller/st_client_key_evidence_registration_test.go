// The actual ClientKey registration (authority Rpc, Postgres and the MinIO
// client) against a synthetic S3 endpoint selected through minio.yml: the
// registration returns once its generation is committed, without waiting for
// storage, and every committed wrapper is stored exactly once storage answers,
// through a request pool that refuses with 429.
package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	connectprotocol "github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// The registration controller fixture with remote storage at fake, and a
// writer with settings installed in the actual registration owner.
func newStClientKeyRemoteEvidenceFixtureTest(tb testing.TB, fake *stClientKeyEvidenceS3Test, settings stClientKeyEvidenceWriterSettings) (*stClientKeyHistoryRPCFixture, *stClientKeyRegistrationCohorts, *stClientKeyEvidenceWriter) {
	tb.Helper()
	fixture, _, _ := newStClientKeyHistoryControllerFixtureWithoutBlobStore(tb)
	pop := server.Vault.PushSimpleResource("minio.yml", []byte(fmt.Sprintf(`
authority: %s
bucket: synthetic
prefix: blob
access_key: synthetic-access
secret_key: synthetic-secret
`, fake.authority())))
	tb.Cleanup(pop)
	if store, ok := server.LoadBlobStore(); !ok || store.Authority() != fake.authority() {
		tb.Fatal("the registration store is not the synthetic endpoint")
	}
	cohorts := stClient().(*CoreStClient).registrationCohorts()
	writer := installStClientKeyEvidenceWriterTest(tb, cohorts, settings)
	return fixture, cohorts, writer
}

// Replaces the owner's writer with a new one.
func installStClientKeyEvidenceWriterTest(tb testing.TB, cohorts *stClientKeyRegistrationCohorts, settings stClientKeyEvidenceWriterSettings) *stClientKeyEvidenceWriter {
	tb.Helper()
	writer := newStClientKeyEvidenceWriterTest(tb, settings)
	cohorts.stateLock.Lock()
	defer cohorts.stateLock.Unlock()
	cohorts.evidence = writer
	return writer
}

// The committed history of one client.
func loadStClientKeyHistoryTest(tb testing.TB, fixture *stClientKeyHistoryRPCFixture, clientId server.Id) []model.StClientKeyHistoryRecord {
	tb.Helper()
	history, err := model.LoadStClientKeyHistory(tb.Context(), fixture.domain, clientId, model.MaxStClientKeyHistoryRegistrations, model.MaxStClientKeyHistoryBytes)
	if err != nil {
		tb.Fatal(err)
	}
	return history
}

// The stored object of one committed wrapper at the synthetic endpoint.
func stClientKeyRemoteEvidenceObjectTest(tb testing.TB, fake *stClientKeyEvidenceS3Test, record model.StClientKeyHistoryRecord) ([]byte, bool) {
	tb.Helper()
	store, ok := server.LoadBlobStore()
	if !ok {
		tb.Fatal("the registration store disappeared")
	}
	key, err := startifact.EvidenceContentKey(store, record.EvidenceHash)
	if err != nil {
		tb.Fatal(err)
	}
	return fake.object(store.Bucket() + "/" + key)
}

// SetClientKey returns while storage holds the evidence create; the committed
// wrapper is stored exactly after storage answers. An unchanged key's
// registration reads the stored object before it writes, and sends nothing
// once the writer has verified it.
func TestStClientKeyRegistrationReturnsBeforeRemoteEvidenceIsStored(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fake := newStClientKeyEvidenceS3Test(tb, 0, 0)
		gate := make(chan struct{})
		fake.holdCreates(gate)
		release := sync.OnceFunc(func() { close(gate) })
		tb.Cleanup(release)
		fixture, cohorts, writer := newStClientKeyRemoteEvidenceFixtureTest(tb, fake, stClientKeyEvidenceWriterSettingsTest())
		networkId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(tb.Context(), networkId, "remote-evidence-"+networkId.String(), server.NewId())
		model.Testing_CreateDevice(tb.Context(), networkId, deviceId, clientId, "remote-evidence", "test")
		key := bytes.Repeat([]byte{21}, 32)
		registered := make(chan error, 1)
		go func() {
			registered <- SetClientKey(tb.Context(), clientId, &connectprotocol.ClientKey{PublicKey: key})
		}()
		select {
		case err := <-registered:
			if err != nil {
				tb.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			tb.Fatal("the ClientKey registration waited for the held evidence store")
		}
		history := loadStClientKeyHistoryTest(tb, fixture, clientId)
		if len(history) != 1 || !history[0].Registration.Present || !bytes.Equal(history[0].Registration.PublicKey[:], key) {
			tb.Fatalf("the returned registration is not committed: %d records", len(history))
		}
		fake.awaitHeld(tb, 1)
		if _, stored := stClientKeyRemoteEvidenceObjectTest(tb, fake, history[0]); stored {
			tb.Fatal("the held create was stored")
		}
		release()
		waitStClientKeyEvidenceIdleTest(tb, writer)
		if content, stored := stClientKeyRemoteEvidenceObjectTest(tb, fake, history[0]); !stored || !bytes.Equal(content, history[0].EvidenceBytes) {
			tb.Fatal("the committed wrapper was not stored exactly")
		}
		if ok := stClientKeyEvidencePublicationsTest(writer, stClientKeyEvidenceOutcomeOk); ok != 1 {
			tb.Fatalf("ok outcomes = %d", ok)
		}

		// an unchanged key's registration verifies the stored object, and a
		// repeat after that sends nothing
		writer = installStClientKeyEvidenceWriterTest(tb, cohorts, stClientKeyEvidenceWriterSettingsTest())
		puts, gets := fake.methods()
		for range 2 {
			if err := SetClientKey(tb.Context(), clientId, &connectprotocol.ClientKey{PublicKey: key}); err != nil {
				tb.Fatal(err)
			}
			waitStClientKeyEvidenceIdleTest(tb, writer)
		}
		if retryPuts, retryGets := fake.methods(); retryPuts != puts || retryGets != gets+1 {
			tb.Fatalf("two unchanged registrations sent %d creates and %d reads", retryPuts-puts, retryGets-gets)
		}
		if exists := stClientKeyEvidencePublicationsTest(writer, stClientKeyEvidenceOutcomeExists); exists != 2 {
			tb.Fatalf("exists outcomes = %d", exists)
		}
		if retried := loadStClientKeyHistoryTest(tb, fixture, clientId); len(retried) != 1 || !bytes.Equal(retried[0].EvidenceBytes, history[0].EvidenceBytes) {
			tb.Fatal("an unchanged registration changed the committed history")
		}
	})
}

// Concurrent registrations through a request pool smaller than the writer's
// concurrency all return, and every committed wrapper is stored exactly.
func TestStClientKeyRegistrationEvidenceStoredThroughFullRequestPool(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		fake := newStClientKeyEvidenceS3Test(tb, 1, 5*time.Millisecond)
		settings := stClientKeyEvidenceWriterSettingsTest()
		settings.MaxAttempts = 1000
		fixture, _, writer := newStClientKeyRemoteEvidenceFixtureTest(tb, fake, settings)
		networkId := server.NewId()
		model.Testing_CreateNetwork(tb.Context(), networkId, "remote-evidence-"+networkId.String(), server.NewId())
		clientIds := make([]server.Id, 16)
		for index := range clientIds {
			clientIds[index] = server.NewId()
			model.Testing_CreateDevice(tb.Context(), networkId, server.NewId(), clientIds[index], fmt.Sprintf("remote-evidence-%d", index), "test")
		}
		var registrations sync.WaitGroup
		failures := make(chan error, len(clientIds))
		for index, clientId := range clientIds {
			registrations.Go(func() {
				key := bytes.Repeat([]byte{byte(40 + index)}, 32)
				if err := SetClientKey(tb.Context(), clientId, &connectprotocol.ClientKey{PublicKey: key}); err != nil {
					failures <- err
				}
			})
		}
		registrations.Wait()
		close(failures)
		for err := range failures {
			tb.Fatal(err)
		}
		waitStClientKeyEvidenceIdleTest(tb, writer)
		for _, clientId := range clientIds {
			history := loadStClientKeyHistoryTest(tb, fixture, clientId)
			if len(history) != 1 {
				tb.Fatalf("%d committed records", len(history))
			}
			if content, stored := stClientKeyRemoteEvidenceObjectTest(tb, fake, history[0]); !stored || !bytes.Equal(content, history[0].EvidenceBytes) {
				tb.Fatal("a committed wrapper was never stored")
			}
		}
		requests, maxArrivals, locations := fake.census()
		throttled := 0
		for _, request := range requests {
			if request.status == http.StatusTooManyRequests {
				throttled++
			}
		}
		if throttled == 0 || maxArrivals > writer.settings.Workers || locations != 0 {
			tb.Fatalf("throttled=%d maxArrivals=%d locations=%d", throttled, maxArrivals, locations)
		}
		// a create whose readback was refused is found stored by its retry
		ok := stClientKeyEvidencePublicationsTest(writer, stClientKeyEvidenceOutcomeOk)
		exists := stClientKeyEvidencePublicationsTest(writer, stClientKeyEvidenceOutcomeExists)
		if ok == 0 || ok+exists != len(clientIds) {
			tb.Fatalf("outcomes ok=%d exists=%d for %d registrations", ok, exists, len(clientIds))
		}
		for _, outcome := range []string{stClientKeyEvidenceOutcomeThrottled, stClientKeyEvidenceOutcomeError, stClientKeyEvidenceOutcomeDropped} {
			if failed := stClientKeyEvidencePublicationsTest(writer, outcome); failed != 0 {
				tb.Fatalf("%d publications %s", failed, outcome)
			}
		}
	})
}
