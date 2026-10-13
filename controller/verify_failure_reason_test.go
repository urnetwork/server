// Failed EXTENDs retain exact internal diagnosis while preserving the generic
// protocol response and the normal terminal trail failure semantics.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A source lease disappearing after assignment must still fail, never become
// an accepted replay. The same wire response covers path and signature attacks.
func TestVerifyControllerExtendFailureReasonsStayPrivate(t *testing.T) {
	for _, reason := range []string{"source-egress-unresolved", "source-egress-mismatch", "signature-mismatch", "path-mismatch"} {
		t.Run(reason, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				testVerifyInstallServerKey()
				settings := model.DefaultVerifySettings()
				SetVerifySettings(settings)
				ips := map[server.Id]string{}
				var seedProvider server.Id
				for index := 0; index < connect.VerifyMMin; index++ {
					ip := fmt.Sprintf("192.0.2.%d", 65+index*8)
					provider := testVerifyProvider(ctx, netip.MustParseAddr(ip), settings)
					ips[provider] = ip
					if index == 0 {
						seedProvider = provider
					}
				}
				validator, key, private := testVerifyValidator(ctx)
				value, err := Verify(testVerifySeedArgs(t, validator, key, private, connect.VerifyMMin), testVerifySession(ctx, ips[seedProvider]))
				if err != nil {
					t.Fatal(err)
				}
				assign := value.(*connect.VerifyAssignResult)
				extend := testVerifyExtendArgs(t, validator, key, private, assign)
				pending := server.Id(assign.NextHop)
				source := ips[pending]
				switch reason {
				case "source-egress-unresolved":
					// the retryable answers first (IPV6.md C7), then the failure
					model.ClearVerifyEgress(ctx, pending, netip.MustParseAddr(source), settings)
					for attempt := 0; attempt < model.VerifyUnattributedAttemptLimit; attempt++ {
						if _, err := Verify(extend, testVerifySession(ctx, source)); err == nil || err.Error() != "421 source-egress-unresolved" {
							t.Fatalf("unresolved attempt %d = %v, want the retryable answer", attempt+1, err)
						}
					}
				case "source-egress-mismatch":
					// another provider's address is retryable first too (N4)
					source = ips[seedProvider]
					for attempt := 0; attempt < model.VerifyUnattributedAttemptLimit; attempt++ {
						if _, err := Verify(extend, testVerifySession(ctx, source)); err == nil || err.Error() != "421 source-egress-unresolved" {
							t.Fatalf("mismatched attempt %d = %v, want the retryable answer", attempt+1, err)
						}
					}
				case "signature-mismatch":
					extend.ExtendSig[0] ^= 1
				case "path-mismatch":
					extend.Trail[0] = server.NewId()
				}
				_, err = Verify(extend, testVerifySession(ctx, source))
				var failure *verifyExtendFailure
				if !errors.As(err, &failure) || failure.reason != reason || err.Error() != "400 trail failed" {
					t.Fatalf("diagnostic leaked or lost: reason=%s error=%v", reason, err)
				}
				trail := model.GetVerifyTrail(ctx, server.Id(assign.TrailId))
				if trail == nil || trail.Status == model.VerifyTrailStatusActive || len(trail.Hops) != 1 {
					t.Fatalf("rejected EXTEND changed confirmed history: %+v", trail)
				}
			})
		})
	}
}
