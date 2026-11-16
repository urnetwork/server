// Real append-only SQL custody is exercised with deterministic synthetic signed
// boundaries. Reopening uses public model functions, never an in-memory receipt.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// A known key owns one SDK generation; another independent key approves capture.
func providerWorkOriginalFixture(t testing.TB) (protocol.OriginalWorkCutSubmission, protocol.OriginalWorkRequest, ed25519.PrivateKey, ed25519.PrivateKey, time.Time) {
	t.Helper()
	approver := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{81}, 32))
	sdk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{82}, 32))
	now := time.Unix(1_800_000_000, 0).UTC()
	request, err := protocol.SignOriginalWorkRequest(protocol.OriginalWorkRequest{RequestId: [16]byte(server.NewId()), DomainHash: [32]byte{83}, ClientId: [16]byte(server.NewId()), Generation: [16]byte(server.NewId()), PublicKey: [32]byte(sdk.Public().(ed25519.PublicKey)), Epoch: 9, Kind: "start", Block: 21, BlockHash: [32]byte{84}, IssuedAtUnix: now.Unix() - 1, ExpiresAtUnix: now.Unix() + 300}, approver)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := protocol.SignOriginalWorkCut(t.Context(), protocol.OriginalWorkCut{DomainHash: request.DomainHash, ClientId: request.ClientId, Generation: request.Generation, Epoch: request.Epoch, Block: request.Block, BlockHash: request.BlockHash, Complete: true, Contracts: []protocol.OriginalWorkContract{}}, sdk)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := request.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	cutRaw, err := cut.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return protocol.OriginalWorkCutSubmission{Request: raw, Cut: cutRaw}, request, approver, sdk, now
}

// Lost HTTP acknowledgment can repeat both operations after capture expiry and
// process restart without creating a second cut, signature or boundary identity.
func TestProviderWorkOriginalRetryAfterExpiryRetainsExactReceipt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		submission, request, approver, _, now := providerWorkOriginalFixture(t)
		key := [32]byte(approver.Public().(ed25519.PublicKey))
		digest, err := RetainProviderWorkRequest(t.Context(), submission.Request, key, request.DomainHash, now)
		if err != nil {
			t.Fatal(err)
		}
		first, err := RetainProviderWorkCut(t.Context(), submission, key, request.DomainHash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkRequest(t.Context(), submission.Request, key, request.DomainHash, now.Add(2*time.Hour)); err != nil {
			t.Fatal("exact expired retry lost custody", err)
		}
		second, err := RetainProviderWorkCut(t.Context(), submission, key, request.DomainHash)
		if err != nil || second != first || first.RequestHash != digest || first.CutHash != sha256.Sum256(submission.Cut) {
			t.Fatal("reopened receipt changed", err)
		}
		for _, entry := range []struct {
			hash [32]byte
			cut  bool
			raw  []byte
		}{{hash: digest, raw: submission.Request}, {hash: first.CutHash, cut: true, raw: submission.Cut}} {
			got, err := GetProviderWorkOriginal(t.Context(), entry.hash, entry.cut)
			if err != nil || !bytes.Equal(got, entry.raw) {
				t.Fatal("original readback changed", err)
			}
		}
		owner := ProviderWorkOwner{DomainHash: request.DomainHash, ClientId: request.ClientId, Generation: request.Generation, PublicKey: request.PublicKey}
		list, err := ListProviderWorkRequests(t.Context(), owner, key, now)
		if err != nil || len(list.Requests) != 0 {
			t.Fatal("completed boundary remained capture work", err)
		}
		pair, err := GetProviderWorkBoundary(t.Context(), owner, request.Epoch, request.Kind, key)
		if err != nil || !bytes.Equal(pair.Request, submission.Request) || !bytes.Equal(pair.Cut, submission.Cut) {
			t.Fatal("boundary join lost original", err)
		}
	})
}

// A request signature never bypasses enrollment, and a new id or fresh SDK
// signature cannot reinterpret one original domain/client/generation boundary.
func TestProviderWorkOriginalRejectsUnenrolledAndReplacementCuts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		submission, request, approver, sdk, now := providerWorkOriginalFixture(t)
		key := [32]byte(approver.Public().(ed25519.PublicKey))
		if _, err := RetainProviderWorkCut(t.Context(), submission, key, request.DomainHash); !errors.Is(err, ErrProviderWorkMissing) {
			t.Fatal("cut enrolled its own request", err)
		}
		if _, err := RetainProviderWorkRequest(t.Context(), submission.Request, key, request.DomainHash, now); err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkCut(t.Context(), submission, key, request.DomainHash); err != nil {
			t.Fatal(err)
		}
		cut, err := protocol.DecodeOriginalWorkCut(t.Context(), submission.Cut)
		if err != nil {
			t.Fatal(err)
		}
		cut.Revision++
		cut, err = protocol.SignOriginalWorkCut(t.Context(), cut, sdk)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := cut.Bytes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkCut(t.Context(), protocol.OriginalWorkCutSubmission{Request: submission.Request, Cut: changed}, key, request.DomainHash); !errors.Is(err, ErrProviderWorkConflict) {
			t.Fatal("fresh cut replaced original boundary", err)
		}
		request.RequestId = [16]byte(server.NewId())
		request, err = protocol.SignOriginalWorkRequest(request, approver)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := request.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkRequest(t.Context(), raw, key, request.DomainHash, now); !errors.Is(err, ErrProviderWorkConflict) {
			t.Fatal("new request id renewed boundary", err)
		}
	})
}

// The independent key/domain and canonical raw bytes are checked before any
// database access. Cancellation must preserve the owning original error.
func TestProviderWorkOriginalRequiresIndependentCanonicalAuthority(t *testing.T) {
	submission, request, approver, _, now := providerWorkOriginalFixture(t)
	key := [32]byte(approver.Public().(ed25519.PublicKey))
	for _, entry := range []struct {
		raw         []byte
		key, domain [32]byte
	}{{raw: submission.Request, key: [32]byte{4}, domain: request.DomainHash}, {raw: submission.Request, key: key, domain: [32]byte{5}}, {raw: append(bytes.Clone(submission.Request), '\n'), key: key, domain: request.DomainHash}} {
		if _, err := RetainProviderWorkRequest(t.Context(), entry.raw, entry.key, entry.domain, now); !errors.Is(err, ErrProviderWorkInvalid) {
			t.Fatal("caller selected authority or alternate encoding", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := RetainProviderWorkCut(ctx, submission, key, request.DomainHash); !errors.Is(err, context.Canceled) {
		t.Fatal("cut verification lost cancellation", err)
	}
}

// The complete live poll is never truncated. Consuming one retained cut makes
// capacity available without deleting its request or permitting replacement.
func TestProviderWorkOriginalPollingCapacityAndGenerationIsolation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		_, request, approver, _, now := providerWorkOriginalFixture(t)
		key := [32]byte(approver.Public().(ed25519.PublicKey))
		for index := 0; index <= protocol.MaximumOriginalWorkRequests; index++ {
			request.Epoch = uint64(index)
			request.RequestId = [16]byte(server.NewId())
			signed, err := protocol.SignOriginalWorkRequest(request, approver)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := signed.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			_, err = RetainProviderWorkRequest(t.Context(), raw, key, request.DomainHash, now)
			if index == protocol.MaximumOriginalWorkRequests {
				if !errors.Is(err, ErrProviderWorkCapacity) {
					t.Fatal("outstanding capacity not enforced", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		}
		owner := ProviderWorkOwner{DomainHash: request.DomainHash, ClientId: request.ClientId, Generation: request.Generation, PublicKey: request.PublicKey}
		list, err := ListProviderWorkRequests(t.Context(), owner, key, now)
		if err != nil || len(list.Requests) != protocol.MaximumOriginalWorkRequests {
			t.Fatal("poll lost admitted requests", err)
		}
		owner.Generation[0] ^= 1
		list, err = ListProviderWorkRequests(t.Context(), owner, key, now)
		if err != nil || len(list.Requests) != 0 {
			t.Fatal("poll crossed SDK generation", err)
		}
	})
}

// Public model access cannot mutate or erase custody through any ordinary SQL
// write, including TRUNCATE which is outside row-level trigger coverage.
func TestProviderWorkOriginalSqlGuardsPreserveRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		submission, request, approver, _, now := providerWorkOriginalFixture(t)
		key := [32]byte(approver.Public().(ed25519.PublicKey))
		if _, err := RetainProviderWorkRequest(t.Context(), submission.Request, key, request.DomainHash, now); err != nil {
			t.Fatal(err)
		}
		if _, err := RetainProviderWorkCut(t.Context(), submission, key, request.DomainHash); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{`UPDATE provider_work_request SET original=original`, `DELETE FROM provider_work_request`, `UPDATE provider_work_cut SET original=original`, `DELETE FROM provider_work_cut`, `TRUNCATE provider_work_request,provider_work_cut`, `TRUNCATE provider_work_cut`} {
			if recovered := server.HandleError(func() {
				server.Tx(t.Context(), func(tx server.PgTx) { server.RaisePgResult(tx.Exec(t.Context(), statement)) })
			}); recovered == nil {
				t.Fatal("immutable original SQL mutation succeeded", statement)
			}
		}
	})
}

// Even a window with only open contracts needs an explicit original inventory;
// no artificial payout row may be created merely to carry that evidence.
func TestProviderWorkEmptyCreditRetainsSeparateSqlWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		start := server.NowUtc().Add(-time.Minute)
		end := start.Add(time.Hour)
		usage, census, window, err := GetStEpochProviderUsageWholeCensus(f.ctx, 17, start, end)
		if err != nil || len(usage) != 0 || census == nil || census.Count != 0 || len(census.Records) != 0 || window == nil || len(window.Records) != 2 {
			t.Fatal("known empty credit lost actual window", census, window, err)
		}
		for _, row := range window.Records {
			if row.Disposition != "open" {
				t.Fatal("open work became credit", row.Disposition)
			}
		}
	})
}
