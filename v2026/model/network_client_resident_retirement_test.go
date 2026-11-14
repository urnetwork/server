package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func retirementResidentFixture(t testing.TB) *NetworkClientResident {
	t.Helper()
	return &NetworkClientResident{ClientId: server.NewId(), InstanceId: server.NewId(), ResidentId: server.NewId(), ResidentHost: "synthetic-host", ResidentService: "connect", ResidentBlock: "synthetic", ResidentInternalPorts: []int{1234}}
}

func writeRetirementResident(t testing.TB, r *NetworkClientResident, ttl time.Duration) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(r); err != nil {
		t.Fatal(err)
	}
	server.Redis(context.Background(), func(client server.RedisClient) {
		if err := client.Set(context.Background(), residentKey(r.ClientId), buf.Bytes(), ttl).Err(); err != nil {
			t.Fatal(err)
		}
	})
	return bytes.Clone(buf.Bytes())
}

func TestResidentRetirementCaptureDoesNotRefreshTtl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		r := retirementResidentFixture(t)
		raw := writeRetirementResident(t, r, 20*time.Second)
		var before, after time.Duration
		server.Redis(ctx, func(client server.RedisClient) { before = client.PTTL(ctx, residentKey(r.ClientId)).Val() })
		token, err := CaptureResidentForClientRetirement(ctx, r.ClientId, r.InstanceId)
		server.Redis(ctx, func(client server.RedisClient) { after = client.PTTL(ctx, residentKey(r.ClientId)).Val() })
		if err != nil || token == nil || token.clientId != r.ClientId || token.instanceId != r.InstanceId || token.residentId != r.ResidentId || !bytes.Equal(token.value, raw) {
			t.Fatal("capture did not bind original exact owner")
		}
		if after <= 0 || after > before || after > 20*time.Second {
			t.Fatal("read-only capture refreshed residency")
		}
		removed, err := RemoveCapturedResidentForClient(ctx, token)
		if err != nil || !removed || GetResidentForClient(ctx, r.ClientId, 0) != nil {
			t.Fatal("captured owner was not atomically removed")
		}
		if removed, err := RemoveCapturedResidentForClient(ctx, token); err != nil || removed {
			t.Fatal("replay was not a safe no-op")
		}
	})
}

func TestResidentRetirementPreservesReplacementBytesAndIndependentClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, scenario := range []string{"instance", "resident", "metadata"} {
			original := retirementResidentFixture(t)
			writeRetirementResident(t, original, time.Minute)
			independent := retirementResidentFixture(t)
			independentRaw := writeRetirementResident(t, independent, time.Minute)
			token, err := CaptureResidentForClientRetirement(ctx, original.ClientId, original.InstanceId)
			if err != nil || token == nil {
				t.Fatal("failed original capture")
			}
			replacement := *original
			switch scenario {
			case "instance":
				replacement.InstanceId = server.NewId()
			case "resident":
				replacement.ResidentId = server.NewId()
			case "metadata":
				replacement.ResidentHost = "replacement-host"
			}
			replacementRaw := writeRetirementResident(t, &replacement, time.Minute)
			removed, err := RemoveCapturedResidentForClient(ctx, token)
			if err != nil || removed {
				t.Fatalf("%s replacement was removed", scenario)
			}
			server.Redis(ctx, func(client server.RedisClient) {
				got, err := client.Get(ctx, residentKey(original.ClientId)).Bytes()
				if err != nil || !bytes.Equal(got, replacementRaw) {
					t.Fatalf("%s replacement bytes changed", scenario)
				}
				got, err = client.Get(ctx, residentKey(independent.ClientId)).Bytes()
				if err != nil || !bytes.Equal(got, independentRaw) {
					t.Fatal("independent active client changed")
				}
			})
		}
	})
}

func TestResidentRetirementRejectsUnownedOrMalformedCapture(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, scenario := range []string{"missing", "instance", "client", "zero-resident", "corrupt"} {
			r := retirementResidentFixture(t)
			expectedClient, expectedInstance := r.ClientId, r.InstanceId
			switch scenario {
			case "missing":
			case "instance":
				r.InstanceId = server.NewId()
				writeRetirementResident(t, r, time.Minute)
			case "client":
				raw := writeRetirementResident(t, r, time.Minute)
				expectedClient = server.NewId()
				server.Redis(ctx, func(client server.RedisClient) { client.Set(ctx, residentKey(expectedClient), raw, time.Minute) })
			case "zero-resident":
				r.ResidentId = server.Id{}
				writeRetirementResident(t, r, time.Minute)
			case "corrupt":
				server.Redis(ctx, func(client server.RedisClient) { client.Set(ctx, residentKey(r.ClientId), "invalid-gob", time.Minute) })
			}
			token, err := CaptureResidentForClientRetirement(ctx, expectedClient, expectedInstance)
			if token != nil || (scenario == "corrupt" && err == nil) || (scenario != "corrupt" && err != nil) {
				t.Fatalf("%s accepted an unowned or malformed capture", scenario)
			}
		}
	})
}

func TestResidentRetirementCanceledAndEmptyTokensDoNotWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		r := retirementResidentFixture(t)
		raw := writeRetirementResident(t, r, time.Minute)
		token, err := CaptureResidentForClientRetirement(ctx, r.ClientId, r.InstanceId)
		if err != nil || token == nil {
			t.Fatal("capture failed")
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if got, err := CaptureResidentForClientRetirement(canceled, r.ClientId, r.InstanceId); got != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled capture was admitted")
		}
		if removed, err := RemoveCapturedResidentForClient(canceled, token); removed || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled commit was admitted")
		}
		if removed, err := RemoveCapturedResidentForClient(ctx, nil); removed || err != nil {
			t.Fatal("nil capture was not no-op")
		}
		if removed, err := RemoveCapturedResidentForClient(ctx, &NetworkClientResidentRetirement{}); removed || err == nil {
			t.Fatal("manufactured token was admitted")
		}
		server.Redis(ctx, func(client server.RedisClient) {
			got, err := client.Get(ctx, residentKey(r.ClientId)).Bytes()
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("rejected operation changed original resident")
			}
		})
	})
}
