package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Root cause, 2026-11-18: during the fleet-wide bursts of "redis: connection
// pool: failed to dial ... i/o timeout", a tagged client credential whose
// revocation record could not be read was refused connect admission with 503
// (connectClientAuthentication) after the two second authentication budget,
// and the lease's final check and authority publication refused it again.
// Owner decision 3b: both admit it, within the check budget. Needs neither
// PostgreSQL nor Redis: the record check accepts without SQL, and the
// authentication pool is the stalled-dial stand-in.
func TestConnectAdmissionAcceptsATaggedClientWhileRedisDialsTimeOut(t *testing.T) {
	defer session.Testing_SetRevocationRecordEnabled(true)()
	defer session.Testing_SetRejectExpired(false)()
	_, pop := server.Testing_StallAuthRedisDials(48)
	defer pop()
	for _, source := range []session.StateQuerySource{session.StateQueryConnectH1, session.StateQueryConnectH3} {
		ctx := session.WithStateQuerySource(t.Context(), source)
		client := session.NewByJwt(server.NewId(), server.NewId(), "dial-outage", false, false).Client(server.NewId(), server.NewId())
		start := time.Now()
		_, status, err := connectClientAuthentication(ctx, client, server.NewId().Bytes())
		if status != 0 || err != nil {
			t.Fatalf("the handshake check answered %d during the Redis dial outage: %v", status, err)
		}
		if elapsed := time.Since(start); 2*time.Second <= elapsed {
			t.Errorf("the handshake check waited %s for Redis", elapsed)
		}
		start = time.Now()
		closed := make(chan int, 1)
		lease, err := session.StartConnectionAuthorizationLease(ctx, client, server.NewId(), 1, func(code int) { closed <- code })
		if err != nil {
			t.Fatalf("the lease's final check refused admission during the Redis dial outage: %v", err)
		}
		if elapsed := time.Since(start); 2*time.Second <= elapsed {
			t.Errorf("the lease's final check and publication waited %s for Redis", elapsed)
		}
		lease.Close()
		select {
		case code := <-closed:
			t.Fatalf("the admitted transport was retired with %d", code)
		default:
		}
	}
}
