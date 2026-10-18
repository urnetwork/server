package model

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

func contractMetadataAcquires(t testing.TB) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_pg_pool_acquires_total" {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["pool"] == "default" && labels["outcome"] == "acquired" {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatal("opened database pool has no acquisition counter")
	return 0
}

// Differential coverage uses the actual old readers and the new SQL/Redis path.
// Count actual pool acquisitions as well as values: batching must reduce two
// acquisitions to one rather than merely move the parallelism elsewhere.
func TestClientContractMetadataLegacyOneAcquisition(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		for _, published := range []bool{false, true} {
			var cert, signature, key []byte
			if published {
				cert, signature, key = []byte("test certificate"), []byte("test signature"), bytes.Repeat([]byte{8}, 32)
			}
			SetClientTlsCertificateWithSignature(tb.Context(), input.ClientID, cert, signature)
			SetClientPublicKey(tb.Context(), input.ClientID, key)
			before := contractMetadataAcquires(tb)
			oldCert, oldSignature, certErr := GetClientTlsCertificateAndSignature(tb.Context(), input.ClientID)
			oldKey, keyErr := GetClientPublicKey(tb.Context(), input.ClientID)
			if certErr != nil || keyErr != nil || contractMetadataAcquires(tb)-before != 2 {
				tb.Fatal("legacy path did not perform its expected two database acquisitions")
			}
			before = contractMetadataAcquires(tb)
			got, err := GetClientContractMetadata(tb.Context(), input.ClientID)
			if err != nil || !bytes.Equal(got.TLSCertificatePEM, oldCert) || !bytes.Equal(got.ClientKeySignedTLSCertificate, oldSignature) || !bytes.Equal(got.PublicKey, oldKey) {
				tb.Fatalf("combined legacy projection differs: %v", err)
			}
			if delta := contractMetadataAcquires(tb) - before; delta != 1 {
				tb.Fatalf("combined metadata used %v database acquisitions, want 1", delta)
			}
		}
	})
}

func TestClientContractMetadataSignedRotationAndTombstone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		SetClientTlsCertificateWithSignature(tb.Context(), input.ClientID, []byte("cert"), []byte("signature"))
		// Deliberately keep a stale legacy key; signed history owns every result.
		SetClientPublicKey(tb.Context(), input.ClientID, bytes.Repeat([]byte{7}, 32))
		for _, key := range [][]byte{input.PublicKey, bytes.Repeat([]byte{10}, 32), nil} {
			input.PublicKey = key
			input.Boundary.Block++
			input.Boundary.Hash[0]++
			if _, err := StoreStClientKeyRegistration(tb.Context(), input); err != nil {
				tb.Fatal(err)
			}
			old, oldErr := GetClientPublicKey(tb.Context(), input.ClientID)
			got, err := GetClientContractMetadata(tb.Context(), input.ClientID)
			if err != nil || oldErr != nil || !bytes.Equal(got.PublicKey, old) || !bytes.Equal(got.PublicKey, key) || !bytes.Equal(got.TLSCertificatePEM, []byte("cert")) || !bytes.Equal(got.ClientKeySignedTLSCertificate, []byte("signature")) {
				tb.Fatalf("signed metadata rotation/tombstone differs: %v", errors.Join(err, oldErr))
			}
		}
	})
}

func TestClientContractMetadataRetiredAndInactiveAuthority(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "retired"}[retired], func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(tb testing.TB) {
				input := newStClientKeyHistoryTestInput(tb)
				SetClientPublicKey(tb.Context(), input.ClientID, bytes.Repeat([]byte{7}, 32))
				if _, err := StoreStClientKeyRegistration(tb.Context(), input); err != nil {
					tb.Fatal(err)
				}
				server.Tx(tb.Context(), func(tx server.PgTx) {
					if retired {
						server.RaisePgResult(tx.Exec(tb.Context(), "UPDATE st_client_key_head SET retired=true WHERE client_id=$1", input.ClientID))
					} else {
						server.RaisePgResult(tx.Exec(tb.Context(), "UPDATE network_client SET active=false WHERE client_id=$1", input.ClientID))
					}
				})
				got, err := GetClientContractMetadata(tb.Context(), input.ClientID)
				if err != nil || len(got.PublicKey) != 0 {
					tb.Fatalf("inactive signed authority fell through to legacy key: %v", err)
				}
			})
		})
	}
}

func TestClientContractMetadataLegacyRedisFailurePreservesCertificate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		SetClientTlsCertificateWithSignature(tb.Context(), input.ClientID, []byte("cert"), []byte("signature"))
		server.Redis(tb.Context(), func(r server.RedisClient) {
			server.Raise(r.LPush(tb.Context(), clientPublicKeyRedisKey(input.ClientID), "wrong type").Err())
		})
		got, err := GetClientContractMetadata(tb.Context(), input.ClientID)
		if err == nil || len(got.PublicKey) != 0 || !bytes.Equal(got.TLSCertificatePEM, []byte("cert")) || !bytes.Equal(got.ClientKeySignedTLSCertificate, []byte("signature")) {
			tb.Fatal("legacy key failure discarded valid certificate or returned a key")
		}
	})
}

func TestClientContractMetadataCanceledBeforeAcquisition(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := GetClientContractMetadata(ctx, server.NewId())
	if !errors.Is(err, context.Canceled) || len(got.PublicKey)+len(got.TLSCertificatePEM)+len(got.ClientKeySignedTLSCertificate) != 0 {
		t.Fatal("canceled metadata lookup did work or returned material")
	}
}

func TestClientContractMetadataReservedControlCertificate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		clientID := server.Id{}
		SetClientTlsCertificateWithSignature(tb.Context(), clientID, []byte("control cert"), []byte("control signature"))
		oldCert, oldSignature, oldErr := GetClientTlsCertificateAndSignature(tb.Context(), clientID)
		if oldErr != nil {
			tb.Fatal(oldErr)
		}
		got, err := GetClientContractMetadata(tb.Context(), clientID)
		if err == nil || len(got.PublicKey) != 0 || !bytes.Equal(got.TLSCertificatePEM, oldCert) || !bytes.Equal(got.ClientKeySignedTLSCertificate, oldSignature) {
			tb.Fatal("reserved control identity lost certificate or acquired a public key")
		}
	})
}

func TestClientContractMetadataSQLFailurePreservesCertificate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		SetClientTlsCertificateWithSignature(tb.Context(), input.ClientID, []byte("cert"), []byte("signature"))
		SetClientPublicKey(tb.Context(), input.ClientID, bytes.Repeat([]byte{7}, 32))
		// An actual relation-resolution failure reproduces a partial metadata
		// outage. The certificate table and legacy Redis remain readable.
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), "ALTER TABLE st_client_key_history RENAME TO st_client_key_history_metadata_fault"))
		})
		defer server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), "ALTER TABLE st_client_key_history_metadata_fault RENAME TO st_client_key_history"))
		})
		before := contractMetadataAcquires(tb)
		got, err := GetClientContractMetadata(tb.Context(), input.ClientID)
		if err == nil || len(got.PublicKey) != 0 || !bytes.Equal(got.TLSCertificatePEM, []byte("cert")) || !bytes.Equal(got.ClientKeySignedTLSCertificate, []byte("signature")) {
			tb.Fatal("combined SQL failure lost certificate or admitted stale unsigned key")
		}
		if delta := contractMetadataAcquires(tb) - before; delta != 2 {
			tb.Fatalf("SQL failure used %v acquisitions, want one read and one cert-only fallback", delta)
		}
	})
}

func TestClientContractMetadataSignedProjectionRejectsTamper(t *testing.T) {
	// The decoder remains fail closed after moving it outside the DB callback.
	projection := stClientKeyCurrentProjection{found: true, clientExists: true, registrationBytes: []byte("malformed")}
	if key, err := projection.publicKey(server.NewId()); err == nil || key != nil {
		t.Fatal("malformed signed metadata returned a key")
	}
}

func TestClientContractMetadataBenchmark(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		SetClientTlsCertificateWithSignature(tb.Context(), input.ClientID, bytes.Repeat([]byte("certificate"), 100), bytes.Repeat([]byte{1}, 64))
		if _, err := StoreStClientKeyRegistration(tb.Context(), input); err != nil {
			tb.Fatal(err)
		}
		for _, combined := range []bool{false, true} {
			result := testing.Benchmark(func(b *testing.B) {
				before := contractMetadataAcquires(b)
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if combined {
							if _, err := GetClientContractMetadata(b.Context(), input.ClientID); err != nil {
								b.Error(err)
							}
						} else {
							var wait sync.WaitGroup
							wait.Add(2)
							go func() {
								defer wait.Done()
								if _, _, err := GetClientTlsCertificateAndSignature(b.Context(), input.ClientID); err != nil {
									b.Error(err)
								}
							}()
							go func() {
								defer wait.Done()
								if _, err := GetClientPublicKey(b.Context(), input.ClientID); err != nil {
									b.Error(err)
								}
							}()
							wait.Wait()
						}
					}
				})
				b.StopTimer()
				b.ReportMetric((contractMetadataAcquires(b)-before)/float64(b.N), "pg-acquires/op")
			})
			tb.Logf("%s: %s %s", map[bool]string{false: "previous_parallel", true: "combined"}[combined], result.String(), result.MemString())
		}
	})
}
