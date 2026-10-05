package connect

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// This opt-in local control holds a real PostgreSQL relation lock while the
// production resident heartbeat owns a real Redis lease and attached route.
// Neither endpoint is accepted without an exact loopback authority.
func TestResidentMetadataTailCannotStarveOwnerHeartbeat(t *testing.T) {
	ctx, pg := privateResidentHeartbeatResources(t)
	lock, err := pg.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err := lock.Exec(ctx, "LOCK TABLE network_client IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	settings := DefaultExchangeSettingsWithBufferSize(4)
	settings.ExchangeResidentTtl = 2 * time.Second
	settings.EnableNetworkPeers = true
	residentCtx, residentCancel := context.WithCancel(ctx)
	defer residentCancel()
	resident := newResidentCallbackLifecycleFixture(t, residentCtx, settings)
	resident.residentId, resident.instanceId = server.NewId(), server.NewId()
	networkId := server.NewId()
	resident.peerNetworkId = &networkId
	resident.peerCategory = model.NetworkPeerCategoryClient
	send, receive, detach, err := resident.AddTransport()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		detach()
		residentCancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer joinCancel()
		if err := resident.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		returnReadyPooledMessages(send)
		returnReadyPooledMessages(receive)
	}()
	nominee := &model.NetworkClientResident{ClientId: resident.clientId, InstanceId: resident.instanceId, ResidentId: resident.residentId, ResidentHost: "127.0.0.1", ResidentService: "connect", ResidentBlock: "private-heartbeat"}
	if !model.NominateResident(ctx, nil, nominee, settings.ExchangeResidentTtl) {
		t.Fatal("private resident nomination failed")
	}
	var completed atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for resident.exchange.refreshResidentRegistration(resident) {
			completed.Add(1)
			select {
			case <-residentCtx.Done():
				return
			case <-time.After(settings.ExchangeResidentTtl / 4):
			}
		}
	}()
	joined := false
	defer func() {
		residentCancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("resident heartbeat did not join before releasing the relation lock")
			}
		}
	}()
	// This connection owns the lock and can inspect the blocked peer query.
	observed := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		var blocked int
		if err := lock.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND wait_event_type = 'Lock' AND position('network_client.network_id' in query) > 0`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			observed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !observed {
		t.Fatal("production peer profile query never reached the held relation lock")
	}
	// Routing reads remain passive throughout. Wait beyond the original lease
	// while leaving the metadata lock, resident lifetime, and route untouched.
	time.Sleep(2300 * time.Millisecond)
	current := model.GetResidentForClient(ctx, resident.clientId, 0)
	ownerAlive := !resident.IsDone() && resident.TransportCount() == 1
	leaseAlive := current != nil && current.ResidentId == resident.residentId
	iterations := completed.Load()
	t.Logf("held_real_pg_relation_lock=true scaled_ttl_ms=2000 poll_ms=500 optional_profile_query_observed_blocked=true owner_context_and_route_alive=%t lease_alive_after_original_expiry=%t completed_heartbeat_tails=%d routing_ttl=0", ownerAlive, leaseAlive, iterations)
	residentCancel()
	select {
	case <-done:
		joined = true
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat query did not join while metadata relation remained locked")
	}
	t.Log("heartbeat_joined_before_relation_unlock=true")
	if !ownerAlive || !leaseAlive || iterations < 2 {
		t.Fatal("optional peer metadata blocked the live resident's lease heartbeat")
	}
}

// A completed or expired metadata child must never become the resident's
// transport/SDK lifetime. A fresh metadata child must still restore peer state.
func TestResidentMetadataDeadlinePreservesHealthyPeerAndNativeACK(t *testing.T) {
	ctx, pg := privateResidentHeartbeatResources(t)
	settings := DefaultExchangeSettingsWithBufferSize(4)
	settings.ExchangeResidentTtl = 2 * time.Second
	settings.EnableNetworkPeers = true
	resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
	resident.residentId, resident.instanceId = server.NewId(), server.NewId()
	networkId := server.NewId()
	resident.peerNetworkId = &networkId
	resident.peerCategory = model.NetworkPeerCategoryClient
	if _, err := pg.Exec(ctx, `INSERT INTO network_client (client_id, network_id, principal, active, auth_time) VALUES ($1, $2, 'private-heartbeat', true, now())`, resident.clientId, networkId); err != nil {
		t.Fatal(err)
	}
	send, receive, detach, err := resident.AddTransport()
	if err != nil {
		t.Fatal(err)
	}
	peerSettings := clientconnect.DefaultClientSettingsWithBufferSize(4)
	peerSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	peerSettings.ControlPingTimeout = 0
	peerSettings.Log = clientconnect.NewNoopLogger()
	peer := clientconnect.NewClient(ctx, clientconnect.Id(resident.clientId), clientconnect.NewNoContractClientOob(), peerSettings)
	peer.ContractManager().AddNoContractPeer(clientconnect.ControlId)
	resident.client.ContractManager().AddNoContractPeer(clientconnect.Id(resident.clientId))
	peer.RouteManager().UpdateTransport(clientconnect.NewSendGatewayTransport(), []clientconnect.Route{receive})
	peer.RouteManager().UpdateTransport(clientconnect.NewReceiveGatewayTransport(), []clientconnect.Route{send})
	defer func() {
		detach()
		resident.Cancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer joinCancel()
		if err := peer.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		if err := resident.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		returnReadyPooledMessages(send)
		returnReadyPooledMessages(receive)
	}()
	nominee := &model.NetworkClientResident{ClientId: resident.clientId, InstanceId: resident.instanceId, ResidentId: resident.residentId, ResidentHost: "127.0.0.1", ResidentService: "connect", ResidentBlock: "private-heartbeat"}
	if !model.NominateResident(ctx, nil, nominee, settings.ExchangeResidentTtl) {
		t.Fatal("private resident nomination failed")
	}
	for attempt := range 2 {
		if !resident.exchange.refreshResidentRegistration(resident) {
			t.Fatal("healthy metadata did not retain ownership")
		}
		member := model.GetNetworkPeerMember(ctx, networkId, resident.clientId)
		if member == nil || member.DisconnectTime != nil || member.Principal != "private-heartbeat" || model.GetNetworkConnectedCount(ctx, networkId) != 1 {
			t.Fatalf("healthy metadata registration missing: %+v", member)
		}
		// This exceeds the metadata child's entire budget while its resident
		// and route remain admitted. Removing the first registration makes
		// the next refresh exercise profile load and re-add with a new child.
		time.Sleep(650 * time.Millisecond)
		if attempt == 0 {
			model.RemoveNetworkPeer(ctx, networkId, resident.clientId, resident.residentId)
		}
	}
	if resident.IsDone() || resident.TransportCount() != 1 {
		t.Fatal("metadata child cancellation retired the resident transport")
	}
	delivered := make(chan struct{}, 1)
	peer.AddReceiveCallback(func(_ clientconnect.TransferPath, frames []*protocol.Frame, _ clientconnect.Peer) {
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				delivered <- struct{}{}
			}
		}
	})
	ack := make(chan error, 1)
	frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "healthy metadata handoff"})
	witness := retainResidentPoolWitness(frame.MessageBytes)
	if !resident.client.SendWithTimeout(frame, clientconnect.Id(resident.clientId), func(err error) { ack <- err }, time.Second) {
		clientconnect.MessagePoolReturn(frame.MessageBytes)
		t.Fatal("resident transport refused traffic after metadata child completed")
	}
	select {
	case err := <-ack:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("native transfer did not ACK after metadata child completed")
	}
	select {
	case <-delivered:
	case <-ctx.Done():
		t.Fatal("native transfer was not delivered")
	}
	requireResidentPoolOwnerReturned(t, witness, "healthy metadata child handoff")
	t.Log("healthy_profile_readd=true native_delivery_and_ACK_after_metadata_budget=true resident_transport_retained=true payload_owner_returned=true")
}

func privateResidentHeartbeatResources(t *testing.T) (context.Context, *pgx.Conn) {
	t.Helper()
	redisAddress, pgAddress := os.Getenv("PRIVATE_HEARTBEAT_REDIS"), os.Getenv("PRIVATE_HEARTBEAT_POSTGRES")
	if redisAddress == "" || pgAddress == "" {
		t.Skip("requires the privately owned heartbeat fixture runner")
	}
	for _, address := range []string{redisAddress, pgAddress} {
		host, _, err := net.SplitHostPort(address)
		if err != nil || host != "127.0.0.1" || os.Getenv("WARP_ENV") != "local" {
			t.Fatal("heartbeat fixture must use exact local loopback resources")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	server.RedisReset()
	server.PgReset()
	popRedis := server.Vault.PushSimpleResource("redis.yml", []byte(fmt.Sprintf("authority: %q\npassword: \"\"\ndb: 0\ncluster: false\n", redisAddress)))
	popRedisConfig := server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 4\nmax_retries: -1\n"))
	popPg := server.Vault.PushSimpleResource("pg.yml", []byte(fmt.Sprintf("authority: %q\nuser: postgres\npassword: urnetwork-local-test\ndb: postgres\n", pgAddress)))
	popPgConfig := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 4\n"))
	t.Cleanup(func() {
		server.PgReset()
		server.RedisReset()
		popPgConfig()
		popPg()
		popRedisConfig()
		popRedis()
	})
	pg, err := pgx.Connect(ctx, "postgres://postgres:urnetwork-local-test@"+pgAddress+"/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close(context.Background()) })
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS network_client (client_id uuid PRIMARY KEY, network_id uuid, source_client_id uuid, principal text, device_id uuid, active boolean, auth_time timestamptz)`,
		`CREATE TABLE IF NOT EXISTS device (device_id uuid PRIMARY KEY, device_name text, device_spec text)`,
		`CREATE TABLE IF NOT EXISTS proxy_device_config (client_id uuid PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS network_client_role (client_id uuid, role text)`,
		`CREATE TABLE IF NOT EXISTS provide_key (client_id uuid, provide_mode integer)`,
	} {
		if _, err := pg.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	// Establish the application pool before taking the lock, so this measures
	// the metadata query wait rather than constructor or DNS behavior.
	server.Db(ctx, func(conn server.PgConn) {
		var one int
		if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
			t.Fatalf("private pool warmup: %d %v", one, err)
		}
	})
	return ctx, pg
}
