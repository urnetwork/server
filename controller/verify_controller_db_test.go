package controller

// verify_controller_db_test.go — pg/redis-backed end-to-end tests of the
// `POST /verify` protocol (the pure helpers are tested in
// verify_controller_test.go): a real SEED → EXTEND… → FINAL walk with all
// four Ed25519 signatures verified, the §4.3 idempotent replay, the §9
// poison paths (V1/V2), and the hard rate limit (V5). Run with the local
// services from test.sh (see model/st_model_db_test.go header).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// testVerifyInstallServerKey swaps in a deterministic server signing key and
// returns its public key. Set inside each test body (the swappable instance
// persists per process).
func testVerifyInstallServerKey() ed25519.PublicKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0x42
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	SetVerifyServerKeys([]*VerifyServerKey{{ServerKeyId: 7, PrivateKey: privateKey}})
	return privateKey.Public().(ed25519.PublicKey)
}

// testVerifyProvider registers an eligible provider: provide modes + a clean
// single-ip egress claim.
func testVerifyProvider(ctx context.Context, ip netip.Addr, settings *model.VerifySettings) server.Id {
	clientId := server.NewId()
	modesJson, err := json.Marshal([]model.ProvideMode{model.ProvideModePublic})
	server.Raise(err)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, fmt.Sprintf("{pm_%s}pms", clientId), modesJson, 0).Err())
	})
	model.FeedVerifyEgress(ctx, clientId, ip, settings)
	return clientId
}

// testVerifyValidator registers a validator identity: a fresh vpk keypair
// bound to a client id (D-7: the SEED's vpk must equal the registered key).
func testVerifyValidator(ctx context.Context) (server.Id, ed25519.PublicKey, ed25519.PrivateKey) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	server.Raise(err)
	clientId := server.NewId()
	model.SetClientPublicKey(ctx, clientId, publicKey)
	return clientId, publicKey, privateKey
}

func testVerifySession(ctx context.Context, ip string) *session.ClientSession {
	return session.NewLocalClientSession(ctx, ip+":40000", nil)
}

func testVerifySeedArgs(t testing.TB, validatorId server.Id, vpk ed25519.PublicKey, vpkKey ed25519.PrivateKey, m int) *VerifyArgs {
	clientNonce := make([]byte, connect.VerifyNonceSize)
	for i := range clientNonce {
		clientNonce[i] = byte(i)
	}
	seedMessage, err := connect.BuildVerifySeedMessage(vpk, clientNonce, byte(m))
	if err != nil {
		t.Fatal(err)
	}
	return &VerifyArgs{
		ClientId:    validatorId,
		Vpk:         vpk,
		ClientNonce: clientNonce,
		SeedSig:     connect.SignVerifyMessage(vpkKey, seedMessage),
		M:           m,
	}
}

func testVerifyExtendArgs(t testing.TB, validatorId server.Id, vpk ed25519.PublicKey, vpkKey ed25519.PrivateKey, assign *connect.VerifyAssignResult) *VerifyArgs {
	trail := make([]server.Id, 0, len(assign.Trail)+1)
	for _, hop := range assign.Trail {
		trail = append(trail, server.Id(hop))
	}
	trail = append(trail, server.Id(assign.NextHop))
	extendMessage, err := connect.BuildVerifyExtendMessage(
		assign.TrailId,
		assign.ServerNonce,
		vpk,
		byte(assign.M),
		verifyConnectIds(trail),
	)
	if err != nil {
		t.Fatal(err)
	}
	trailId := server.Id(assign.TrailId)
	return &VerifyArgs{
		ClientId:  validatorId,
		TrailId:   &trailId,
		Trail:     trail,
		ExtendSig: connect.SignVerifyMessage(vpkKey, extendMessage),
	}
}

// TestVerifyControllerFullTrailFlow walks one real trail end to end at
// M = VerifyMMin: SEED through a provider's egress, follow every server
// assignment from the assigned provider's ip, receive the FINAL proof, and
// replay the final EXTEND idempotently. Verifies the assign signatures and
// the coverage-bound FINAL signature exactly as a third party would.
func TestVerifyControllerFullTrailFlow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		serverPub := testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		SetVerifySettings(settings)

		const m = connect.VerifyMMin // 4: seed + 3 assigned hops

		// providers on distinct egress ips; the seed provider plus enough
		// eligible next hops to fill the trail without replacement
		providerIps := map[server.Id]string{}
		var seedProviderId server.Id
		for i := 0; i < m; i++ {
			ip := fmt.Sprintf("203.0.113.%d", 10+10*i)
			providerId := testVerifyProvider(ctx, netip.MustParseAddr(ip), settings)
			providerIps[providerId] = ip
			if i == 0 {
				seedProviderId = providerId
			}
		}
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)

		// SEED arrives from the seed provider's egress ip
		result, err := Verify(
			testVerifySeedArgs(t, validatorId, vpk, vpkKey, m),
			testVerifySession(ctx, providerIps[seedProviderId]),
		)
		if err != nil {
			t.Fatal(err)
		}
		assign, ok := result.(*connect.VerifyAssignResult)
		if !ok {
			t.Fatalf("seed result = %T", result)
		}
		if len(assign.Trail) != 1 || server.Id(assign.Trail[0]) != seedProviderId || assign.M != m {
			t.Fatalf("seed assign = %+v, want trail [seed] at M=%d", assign, m)
		}

		// follow the assignments to depth M, verifying each ASSIGN signature
		var final *connect.VerifyFinalResult
		var finalArgs *VerifyArgs
		var finalIp string
		for step := 0; ; step++ {
			if m < step {
				t.Fatal("trail did not finalize within M steps")
			}
			assignMessage, err := connect.BuildVerifyAssignMessage(
				assign.ServerKeyId, assign.TrailId, assign.ServerNonce, vpk, byte(assign.M),
				append(append([]connect.Id{}, assign.Trail...), assign.NextHop),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !connect.VerifyVerifyMessageSignature(serverPub, assignMessage, assign.AssignSig) {
				t.Fatalf("assign signature invalid at step %d", step)
			}

			nextHopIp, ok := providerIps[server.Id(assign.NextHop)]
			if !ok {
				t.Fatalf("assigned hop %s is not a registered provider", server.Id(assign.NextHop))
			}
			extendArgs := testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign)
			result, err := Verify(extendArgs, testVerifySession(ctx, nextHopIp))
			if err != nil {
				t.Fatal(err)
			}
			if next, ok := result.(*connect.VerifyAssignResult); ok {
				assign = next
				continue
			}
			final, ok = result.(*connect.VerifyFinalResult)
			if !ok {
				t.Fatalf("extend result = %T", result)
			}
			finalArgs = extendArgs
			finalIp = nextHopIp
			break
		}

		// the FINAL proof: depth M, coverage M-1, signature over the effort
		// digest (coverage-bound, review A2) under the trail's server key
		if final.Status != connect.VerifyStatusComplete {
			t.Fatalf("final status = %s", final.Status)
		}
		proof := final.Proof
		if len(proof.Hops) != m || proof.Coverage != uint64(m-1) {
			t.Fatalf("proof depth %d coverage %d, want %d / %d", len(proof.Hops), proof.Coverage, m, m-1)
		}
		if server.Id(proof.Hops[0].ClientId) != seedProviderId {
			t.Fatalf("proof seed hop = %s", server.Id(proof.Hops[0].ClientId))
		}
		// D27: every proof hop (seed included — it egressed from its own ip)
		// carries a non-zero egress-IP-hash equal to the hash of the provider's
		// egress ip at the configured granularity. The FINAL signature check
		// below re-derives from these hops, so it also proves the server signed
		// over them.
		for i, hop := range proof.Hops {
			providerIp, ok := providerIps[server.Id(hop.ClientId)]
			if !ok {
				t.Fatalf("proof hop %d client %s is not a registered provider", i, server.Id(hop.ClientId))
			}
			wantHash := model.VerifyEgressIpHash(
				netip.MustParseAddr(providerIp), settings.EgressHashV4Prefix, settings.EgressHashV6Prefix)
			if hop.EgressIpHash != wantHash {
				t.Fatalf("proof hop %d egress hash = %x, want %x", i, hop.EgressIpHash, wantHash)
			}
			if hop.EgressIpHash == ([32]byte{}) {
				t.Fatalf("proof hop %d egress hash must be non-zero", i)
			}
		}
		finalMessage, err := connect.BuildVerifyFinalMessage(
			proof.ServerKeyId, proof.Header.TrailId, proof.Header.ServerNonce,
			proof.Header.Vpk, byte(proof.Header.M), proof.Hops,
		)
		if err != nil {
			t.Fatal(err)
		}
		finalDigest := connect.VerifyFinalDigest(finalMessage)
		effortDigest := connect.VerifyEffortDigest(finalDigest, proof.Coverage)
		if !connect.VerifyVerifyMessageSignature(serverPub, effortDigest[:], proof.FinalSig) {
			t.Fatal("final signature must verify over the coverage-bound effort digest")
		}

		// durable publication (§6.2)
		trailId := server.Id(proof.Header.TrailId)
		row := model.GetVerifyTrailRow(ctx, trailId)
		if row == nil || row.Status != model.VerifyTrailRowStatusComplete || row.Depth != m ||
			row.CompleteTime == nil || len(row.FinalSig) == 0 || len(row.VerifierSig) == 0 {
			t.Fatalf("durable proof row = %+v", row)
		}

		// §4.3: replaying the final EXTEND (same source, same body) returns the
		// cached FINAL — no double count, no state change
		replay, err := Verify(finalArgs, testVerifySession(ctx, finalIp))
		if err != nil {
			t.Fatal(err)
		}
		replayFinal, ok := replay.(*connect.VerifyFinalResult)
		if !ok || string(replayFinal.Proof.FinalSig) != string(proof.FinalSig) {
			t.Fatalf("replay = %T, want the cached FINAL", replay)
		}

		// §7: every assigned hop earned exactly one assignment + one
		// confirmation; the validator-chosen seed hop earned none (§7.6)
		now := server.NowUtc()
		model.RollupVerifyProviderStats(ctx, now, settings)
		for providerId := range providerIps {
			rows := model.GetVerifyProviderStats(ctx, providerId)
			if providerId == seedProviderId {
				if len(rows) != 0 {
					t.Fatalf("seed hop stats = %+v, want none (§7.6)", rows)
				}
				continue
			}
			// sum across rows: the walk may straddle a stats-period boundary
			var assignments, confirmations int64
			for _, row := range rows {
				assignments += row.Assignments
				confirmations += row.Confirmations
			}
			if assignments != 1 || confirmations != 1 {
				t.Fatalf("provider %s stats = %+v, want 1/1", providerId, rows)
			}
		}
	})
}

// TestVerifyControllerPoisonAndFailurePaths covers the §9 poison guarantees:
// a poison SEED is response-shaped like a real one with a STABLE synthetic
// seed hop (V2), a poison EXTEND fails with the same error as a real trail's
// wrong-source EXTEND (V1), poison trails are never durably published, and
// the hard per-ip/vpk rate limit refuses outright (V5).
func TestVerifyControllerPoisonAndFailurePaths(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		settings.SeedRateHardLimit = 2
		SetVerifySettings(settings)
		defer SetVerifySettings(model.DefaultVerifySettings())

		p1 := testVerifyProvider(ctx, netip.MustParseAddr("203.0.113.10"), settings)
		p2 := testVerifyProvider(ctx, netip.MustParseAddr("203.0.113.20"), settings)
		p3 := testVerifyProvider(ctx, netip.MustParseAddr("203.0.113.30"), settings)
		providerIps := map[server.Id]string{
			p1: "203.0.113.10", p2: "203.0.113.20", p3: "203.0.113.30",
		}

		// --- V2: an unresolved-source SEED poisons with a deterministic
		// synthetic trail[0], stable across identical seeds
		poisonValidator, poisonVpk, poisonKey := testVerifyValidator(ctx)
		unresolvedIp := "198.18.0.9" // never fed to the egress index
		seedOnce := func() *connect.VerifyAssignResult {
			result, err := Verify(
				testVerifySeedArgs(t, poisonValidator, poisonVpk, poisonKey, connect.VerifyMMin),
				testVerifySession(ctx, unresolvedIp),
			)
			if err != nil {
				t.Fatal(err)
			}
			assign, ok := result.(*connect.VerifyAssignResult)
			if !ok {
				t.Fatalf("poison seed result = %T, want the normal assign shape", result)
			}
			return assign
		}
		poison1 := seedOnce()
		poison2 := seedOnce()
		if poison1.Trail[0] != poison2.Trail[0] {
			t.Fatal("poison trail[0] must be stable across identical seeds (V2)")
		}
		if id := server.Id(poison1.Trail[0]); providerIps[id] != "" {
			t.Fatal("poison seed hop must be synthetic, never a real provider")
		}

		// --- V5: the next seed crosses the hard limit → refused outright
		if _, err := Verify(
			testVerifySeedArgs(t, poisonValidator, poisonVpk, poisonKey, connect.VerifyMMin),
			testVerifySession(ctx, unresolvedIp),
		); err == nil || err.Error() != "429 rate limit exceeded" {
			t.Fatalf("hard-limit seed err = %v", err)
		}

		// --- a real trail whose EXTEND arrives from the wrong source ip
		realValidator, realVpk, realKey := testVerifyValidator(ctx)
		result, err := Verify(
			testVerifySeedArgs(t, realValidator, realVpk, realKey, connect.VerifyMMin),
			testVerifySession(ctx, providerIps[p1]),
		)
		if err != nil {
			t.Fatal(err)
		}
		realAssign := result.(*connect.VerifyAssignResult)
		wrongIp := providerIps[p2]
		if server.Id(realAssign.NextHop) == p2 {
			wrongIp = providerIps[p3]
		}
		_, realExtendErr := Verify(
			testVerifyExtendArgs(t, realValidator, realVpk, realKey, realAssign),
			testVerifySession(ctx, wrongIp),
		)
		if realExtendErr == nil {
			t.Fatal("wrong-source EXTEND must fail")
		}

		// --- V1: extending a poison trail from a real provider's ip fails with
		// the IDENTICAL error (no observable branch on poison)
		_, poisonExtendErr := Verify(
			testVerifyExtendArgs(t, poisonValidator, poisonVpk, poisonKey, poison1),
			testVerifySession(ctx, providerIps[p3]),
		)
		if poisonExtendErr == nil || poisonExtendErr.Error() != realExtendErr.Error() {
			t.Fatalf("poison extend err %q must equal real wrong-source err %q", poisonExtendErr, realExtendErr)
		}

		// the failed REAL trail is durably recorded as expired; the poison
		// trails never touch the durable store (§9)
		realRow := model.GetVerifyTrailRow(ctx, server.Id(realAssign.TrailId))
		if realRow == nil || realRow.Status != model.VerifyTrailRowStatusExpired || realRow.Depth != 1 {
			t.Fatalf("failed real trail row = %+v, want expired at depth 1", realRow)
		}
		if model.GetVerifyTrailRow(ctx, server.Id(poison1.TrailId)) != nil ||
			model.GetVerifyTrailRow(ctx, server.Id(poison2.TrailId)) != nil {
			t.Fatal("poison trails must never be durably persisted")
		}
	})
}
