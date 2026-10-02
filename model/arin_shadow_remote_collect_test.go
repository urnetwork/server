package model

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/urnetwork/server"
)

func arinRemoteSocket(t testing.TB, ctx context.Context, service *server.ArinShadowRPCService) (server.ArinShadowRPCRoundTrip, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "arin-fleet-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	owned, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			func() {
				defer connection.Close()
				connection.SetDeadline(time.Now().Add(5 * time.Second))
				request, err := server.ReadArinShadowRPCFrame(connection, server.ArinShadowRPCRequestLimit)
				if err != nil {
					return
				}
				reply, err := service.Handle(owned, request)
				if err == nil {
					server.WriteArinShadowRPCFrame(connection, reply, server.ArinShadowRPCResponseLimit)
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); <-done; os.Remove(path); os.Remove(dir) })
	return server.ArinShadowUnixRoundTrip(path), path
}

// A real child process runs the production socket multiplexer. Startup and
// per-request sleeps are explicit synthetic SSH/network latency, not claimed
// as measurements of Main. Actual PG, framed pipes and Unix sockets are used.
type arinRemoteMuxEndpoint struct {
	ProcessNonce server.Id
	Socket       string
}

func TestArinRemoteMuxHelper(t *testing.T) {
	args := os.Args
	index := -1
	for i, a := range args {
		if a == "--arin-mux-helper" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	if len(args) != index+2 {
		os.Exit(3)
	}
	data, err := os.ReadFile(args[index+1])
	if err != nil {
		os.Exit(4)
	}
	var endpoints []arinRemoteMuxEndpoint
	if json.Unmarshal(data, &endpoints) != nil {
		os.Exit(5)
	}
	sockets := map[server.Id]string{}
	for _, endpoint := range endpoints {
		sockets[endpoint.ProcessNonce] = endpoint.Socket
	}
	time.Sleep(150 * time.Millisecond)
	_ = server.ServeArinShadowRPCBridge(context.Background(), os.Stdin, os.Stdout, sockets, "")
	os.Exit(0)
}

func arinRemoteRecorder(t testing.TB, epoch int64) *server.ArinShadowRecorder {
	t.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "urnetwork arindb", BuildEpoch: epoch, Description: map[string]string{"en": "synthetic full shadow"}, IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	_, prefix, _ := net.ParseCIDR("192.0.2.0/24")
	if w.Insert(prefix, mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("subscriber"), "non_quality": mmdbtype.Bool(false), "risk": mmdbtype.Bool(false)}) != nil {
		t.Fatal("fixture insert")
	}
	var buf bytes.Buffer
	if _, err = w.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "capture.mmdb")
	if os.WriteFile(path, buf.Bytes(), 0600) != nil {
		t.Fatal("fixture write")
	}
	sum := sha256.Sum256(buf.Bytes())
	pin := hex.EncodeToString(sum[:])
	r, err := server.OpenArinShadowCaptureRecorder(path, pin, path, pin, server.NowUtc(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func TestArinRemoteFullPopulationActualPGAndTwentyUnixOwners(t *testing.T) {
	arinRemoteFullPopulation(t, 0)
}

func TestArinRemoteFullPopulationTwoHostPipesRolloverAndLatency(t *testing.T) {
	arinRemoteFullPopulation(t, 4)
}

func TestArinRemoteFullPopulationEightHostsWithinTwoPipes(t *testing.T) {
	arinRemoteFullPopulation(t, 8)
}

func arinRemoteFullPopulation(t *testing.T, hosts int) {
	remote := hosts > 0
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		city := egressTestCity(ctx, "Remote", "Remote", "Remote", "zz")
		seed := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		epoch := server.NowUtc().Add(-72 * time.Hour).Unix()
		lookup := server.NowUtc().Add(-48 * time.Hour).Truncate(time.Microsecond)
		server.Raise(SetConnectionLocation(ctx, seed.connectionId, city.LocationId, &ConnectionLocationScores{ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: epoch, ArinQualityVerified: true}))
		var originalHandler server.Id
		handlers := make([]server.Id, 20)
		for i := range handlers {
			handlers[i] = server.NewId()
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT handler_id FROM network_client_connection WHERE connection_id=$1`, seed.connectionId).Scan(&originalHandler))
			// Clone faithful real rows, including every required modern column.
			// All 100001 connections remain real source members in these tables.
			for _, spec := range []struct {
				table, key string
				id         server.Id
				count      int
			}{
				{"network_client_handler", "handler_id", originalHandler, 20},
				{"network_client", "client_id", seed.clientId, 100000},
				{"provide_key", "client_id", seed.clientId, 100000},
				{"network_client_connection", "connection_id", seed.connectionId, 100000},
				{"network_client_location", "connection_id", seed.connectionId, 100000},
			} {
				var columns, values []string
				rows, err := conn.Query(ctx, `SELECT quote_ident(attname) FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`, spec.table)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var col string
						server.Raise(rows.Scan(&col))
						columns = append(columns, col)
						switch {
						case spec.table == "network_client_handler" && col == "handler_id":
							values = append(values, `($2::uuid[])[n]`)
						case col == "client_id" || col == "connection_id":
							values = append(values, `md5('arin-full-'||n)::uuid`)
						case spec.table == "network_client_connection" && col == "handler_id":
							values = append(values, `($2::uuid[])[get_byte(uuid_send(md5('arin-full-'||n)::uuid),0)%20+1]`)
						default:
							values = append(values, "seed."+col)
						}
					}
				})
				where := ""
				if spec.table == "provide_key" {
					where = " AND provide_mode=3"
				}
				// The unused array is bound with an explicit type in all copies.
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO `+spec.table+` (`+strings.Join(columns, ",")+`) SELECT `+strings.Join(values, ",")+` FROM `+spec.table+` seed CROSS JOIN generate_series(1,$3::integer) n WHERE seed.`+spec.key+`=$1 AND cardinality($2::uuid[])=20`+where, spec.id, handlers, spec.count))
			}
		})
		extra := map[server.Id]server.Id{}
		if remote {
			for n := 1; n <= 5000; n++ {
				connection := md5.Sum([]byte("arin-extra-" + strconv.Itoa(n)))
				client := md5.Sum([]byte("arin-full-" + strconv.Itoa(n)))
				extra[server.Id(connection)] = server.Id(client)
			}
			server.Db(ctx, func(conn server.PgConn) {
				for _, table := range []string{"network_client_connection", "network_client_location"} {
					var cols, values []string
					rows, err := conn.Query(ctx, `SELECT quote_ident(attname) FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`, table)
					server.WithPgResult(rows, err, func() {
						for rows.Next() {
							var col string
							server.Raise(rows.Scan(&col))
							cols = append(cols, col)
							if col == "connection_id" {
								values = append(values, `md5('arin-extra-'||n)::uuid`)
							} else {
								values = append(values, "seed."+col)
							}
						}
					})
					server.RaisePgResult(conn.Exec(ctx, `INSERT INTO `+table+` (`+strings.Join(cols, ",")+`) SELECT `+strings.Join(values, ",")+` FROM generate_series(1,5000) n JOIN `+table+` seed ON seed.connection_id=md5('arin-full-'||n)::uuid`))
				}
			})
		}
		recorder := arinRemoteRecorder(t, epoch)
		var key [32]byte
		key[0] = 71
		run := server.NewId()
		newIdentity := func(role string) server.ArinShadowRPCIdentity {
			return server.ArinShadowRPCIdentity{ProcessNonce: server.NewId(), StartedAt: server.NowUtc(), Revision: strings.Repeat("c", 40), Role: role}
		}
		nativeHandler, closeNative, err := NewArinShadowNativeRPC(ctx, 110000)
		if err != nil {
			t.Fatal(err)
		}
		defer closeNative()
		attempt := beginArinShadowScoreCapture()
		capture := attempt.owner
		target := map[server.Id]map[server.Id]*ClientScore{server.NewId(): {}}
		var targetRows map[server.Id]*ClientScore
		for _, rows := range target {
			targetRows = rows
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT client_id FROM network_client WHERE active AND source_client_id IS NULL`)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					server.Raise(rows.Scan(&id))
					score := shadowScoreTestRow(id)
					score.PassesMinimums = map[string]bool{RankModeQuality: true, RankModeSpeed: true}
					score.Online = true
					country := "zz"
					passed := true
					attempt.observe(score, &country, &providerEgressFacts{egressQuality: &passed}, true)
					targetRows[id] = score
				}
			})
		})
		now := server.NowUtc()
		census := newClientScoreNativeCensus(now.Add(-time.Second), now, now, map[server.Id]ProviderEgressHealthCounts{}, target)
		server.Raise(writeClientScoreNativeCensus(ctx, census, 2*time.Minute))
		attempt.publish(census, target)
		if capture.Snapshot() == nil || len(targetRows) != 100001 {
			t.Fatal("full native source fixture missing")
		}
		identity := newIdentity("native")
		identity.Host = "host-0"
		nativeService, err := server.NewArinShadowRPCService(ctx, run, key, identity, nativeHandler)
		if err != nil {
			t.Fatal(err)
		}
		nativeTransport, nativeSocket := arinRemoteSocket(t, ctx, nativeService)
		hostSockets := map[string]map[server.Id]string{}
		for i := 0; i < max(1, hosts); i++ {
			hostSockets["host-"+strconv.Itoa(i)] = map[server.Id]string{}
		}
		hostSockets[identity.Host][identity.ProcessNonce] = nativeSocket
		var pool *server.ArinShadowRPCPipePool
		wrap := func(identity server.ArinShadowRPCIdentity, transport server.ArinShadowRPCRoundTrip) server.ArinShadowRPCRoundTrip {
			return func(ctx context.Context, request []byte) ([]byte, error) {
				if remote {
					select {
					case <-time.After(20 * time.Millisecond):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return pool.RoundTrip(identity.Host)(ctx, request)
				}
				return transport(ctx, request)
			}
		}
		nativeClient, _ := server.NewArinShadowRPCClient(run, key, identity, wrap(identity, nativeTransport))
		fleet := ArinShadowRemoteFleet{Native: nativeClient, Handlers: map[server.Id]*server.ArinShadowRPCClient{}, InventoryComplete: true}
		var factCalls, keys atomic.Int64
		for hi, handlerId := range handlers {
			identity := newIdentity("connect")
			identity.Host = "host-" + strconv.Itoa(hi*max(1, hosts)/20)
			service, err := server.NewArinShadowRPCService(ctx, run, key, identity, func(ctx context.Context, method string, input json.RawMessage) (any, error) {
				return recorder.CaptureRPC(ctx, input, func(ctx context.Context, ids []server.Id) ([]server.ArinShadowCaptureTarget, error) {
					targets := make([]server.ArinShadowCaptureTarget, len(ids))
					for i, id := range ids {
						clientId := id
						if value, ok := extra[id]; ok {
							clientId = value
						}
						ownerHandler := handlers[int(clientId[0])%20]
						if id == seed.connectionId {
							clientId = seed.clientId
							ownerHandler = originalHandler
						}
						owner := &arinCollectTestOwner{available: true, current: server.ArinShadowOwnerSnapshot{ConnectionId: id, ClientId: clientId, HandlerId: ownerHandler, Address: netip.MustParseAddr("192.0.2.1")}}
						targets[i] = server.ArinShadowCaptureTarget{ConnectionId: id, Owner: owner}
					}
					return targets, nil
				}, func(ctx context.Context, ids []server.Id) ([]server.ArinShadowCaptureFacts, error) {
					if remote {
						select {
						case <-time.After(10 * time.Millisecond):
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					factCalls.Add(1)
					keys.Add(int64(len(ids)))
					return ReadArinShadowCaptureFacts(ctx, ids)
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			transport, socket := arinRemoteSocket(t, ctx, service)
			hostSockets[identity.Host][identity.ProcessNonce] = socket
			client, _ := server.NewArinShadowRPCClient(run, key, identity, wrap(identity, transport))
			fleet.Handlers[handlerId] = client
		}
		fleet.Handlers[originalHandler] = fleet.Handlers[handlers[0]]
		if remote {
			commands := map[string][]string{}
			for host, sockets := range hostSockets {
				path := filepath.Join(t.TempDir(), "sockets.json")
				var endpoints []arinRemoteMuxEndpoint
				for nonce, socket := range sockets {
					endpoints = append(endpoints, arinRemoteMuxEndpoint{nonce, socket})
				}
				encoded, encodeErr := json.Marshal(endpoints)
				server.Raise(encodeErr)
				server.Raise(os.WriteFile(path, encoded, 0600))
				commands[host] = []string{os.Args[0], "-test.run=^TestArinRemoteMuxHelper$", "--", "--arin-mux-helper", path}
			}
			pool, err = server.NewArinShadowRPCPipePool(ctx, commands)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		var peak atomic.Uint64
		peak.Store(before.HeapAlloc)
		memoryDone := make(chan struct{})
		memoryStop := make(chan struct{})
		go func() {
			defer close(memoryDone)
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					var sample runtime.MemStats
					runtime.ReadMemStats(&sample)
					if sample.HeapAlloc > peak.Load() {
						peak.Store(sample.HeapAlloc)
					}
				case <-memoryStop:
					return
				}
			}
		}()
		started := time.Now()
		rolloverDone := make(chan struct{})
		if remote {
			go func() {
				defer close(rolloverDone)
				select {
				case <-time.After(20 * time.Second):
				case <-ctx.Done():
					return
				}
				now := server.NowUtc()
				replacement := newClientScoreNativeCensus(now.Add(-time.Second), now, now, map[server.Id]ProviderEgressHealthCounts{}, target)
				if err := writeClientScoreNativeCensus(ctx, replacement, 2*time.Minute); err != nil {
					t.Error("healthy publisher fixture failed")
					return
				}
				attempt.publish(replacement, target)
			}()
		} else {
			close(rolloverDone)
		}
		report, err := CollectArinShadowRemotePublic(ctx, recorder, fleet, []string{"all", "zz"})
		elapsed := time.Since(started)
		close(memoryStop)
		<-memoryDone
		<-rolloverDone
		runtime.ReadMemStats(&after)
		connections := int64(100001 + len(extra))
		if remote {
			started, peak := pool.Counts()
			t.Logf("transport_elapsed=%s;hosts=%d;pipe_starts=%d;peak_pipes=%d;providers=%d;connections=%d;census_complete=%t;lookup_complete=%t;native_complete=%t;native_changed=%t", elapsed, hosts, started, peak, report.Providers, report.CapturedConnections, report.CensusComplete, report.ObservationComplete, report.NativeMembershipComplete, report.NativeGenerationChanged)
		}
		if err != nil || !report.CensusComplete || !report.ObservationComplete || !report.NativeMembershipComplete || report.Providers != 100001 || report.CapturedConnections != connections || keys.Load() != connections || report.ActualMainCoverage {
			t.Fatal("full actual-PG/socket capture failed", err, "providers", report.Providers, "captured", report.CapturedConnections, "reasons", report.Reasons)
		}
		for _, bucket := range report.Buckets {
			if bucket.VerifiedSubscriber != 100001 || bucket.ActiveQuality != 100001 || bucket.CandidateQuality != 100001 {
				t.Fatal("full native denominator lost")
			}
		}
		budget := 60 * time.Second
		if remote {
			budget = 85 * time.Second
		}
		if elapsed > budget {
			t.Fatal("full capture left insufficient ninety-second owner budget")
		}
		if remote {
			started, peak := pool.Counts()
			if peak > 2 || started < 4 || !report.NativeGenerationChanged {
				t.Fatal("host/session/rollover boundary failed", started, peak, report.NativeGenerationChanged)
			}
			t.Logf("%d host multiplexers;peak_sessions=%d;session_starts=%d;synthetic_setup=150ms;synthetic_network=20ms;synthetic_DB_latency=10ms;connections=%d;rollover=%t", hosts, peak, started, connections, report.NativeGenerationChanged)
		}
		t.Logf("100001 real PG connections/providers;20 Unix owners;elapsed=%s;point_batches=%d;allocation_bytes=%d;heap_after=%d;peak_sampled_heap=%d;heap_before=%d;fresh_clock_separate_from_lookup=%t", elapsed, factCalls.Load(), after.TotalAlloc-before.TotalAlloc, after.HeapAlloc, peak.Load(), before.HeapAlloc, report.EarliestLookupAt.Equal(lookup))
	})
}
