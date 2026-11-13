package model_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// certs' budget on the test Redis and a private database (TLS-M5-16): the
// scripts answer as the fake's Go twins do on fixed vectors, each names one
// key and builds none, and a lost key is rebuilt from the rows before a unit
// is spent from it.

// A CA and a root of this test's own, so no other run's rows or key count.
func certsBudgetTestSettings(budgetCap int) (certsservice.CertsCa, *certsservice.CertsSettings) {
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	ca := certsservice.CertsCa("t" + hex.EncodeToString(nonce))
	settings := certsservice.DefaultCertsSettings()
	settings.RootDomain = "b" + hex.EncodeToString(nonce) + ".example.test"
	settings.Cas = map[certsservice.CertsCa]*certsservice.CertsCaSettings{
		ca: {BudgetWindow: time.Hour, BudgetCap: budgetCap},
	}
	settings.CaOrder = []certsservice.CertsCa{ca}
	settings.EphemeralCaOrder = []certsservice.CertsCa{ca}
	return ca, settings
}

func TestCertsBudgetScriptsNameOneKey(t *testing.T) {
	keyBuilding := regexp.MustCompile(`"\{|\{tcb_|KEYS\[2\]`)
	for _, script := range []*sidecar.RedisScript{certsservice.CertsBudgetSpendScript, certsservice.CertsBudgetSeedScript} {
		if keyBuilding.MatchString(script.Source) {
			t.Errorf("%s builds a key or reads a second", script.Name)
		}
	}
	if key := model.TlsCertBudgetKey("le", "example.test"); key != "{tcb_le_example.test}" || !sidecar.RedisKeysShareTag([]string{key}) {
		t.Fatalf("the budget key is %q", key)
	}
}

func TestCertsBudgetAnswersAsTheFakeOnFixedVectors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		ca, settings := certsBudgetTestSettings(3)
		start := time.Date(2031, time.June, 1, 0, 0, 0, 0, time.UTC)

		fakeRedis := sidecartest.NewFakeRedis(ctx, sidecartest.NewFakeClock(start), nil, nil, nil)
		sidecartest.RegisterCertsBudgetTwins(fakeRedis)
		fake := certsservice.NewCertsBudget(fakeRedis, sidecartest.NewFakeCertsStore(nil), settings)
		real := model.NewCertsBudget(sidecarhost.NewRedis(nil), nil, settings)

		certIds := []sidecar.Id{}
		for range 6 {
			certIds = append(certIds, sidecar.NewId())
		}
		// cap 3 in an hour: three fit, a fourth does not, a retry of one that spent does, and the window slides
		vectors := []struct {
			certId sidecar.Id
			after  time.Duration
			want   bool
		}{
			{certIds[0], 0, true},
			{certIds[1], 10 * time.Minute, true},
			{certIds[0], 11 * time.Minute, true},
			{certIds[2], 20 * time.Minute, true},
			{certIds[3], 30 * time.Minute, false},
			{certIds[1], 31 * time.Minute, true},
			{certIds[3], time.Hour, false},
			{certIds[3], time.Hour + time.Millisecond, true},
			{certIds[4], time.Hour + 2*time.Millisecond, false},
			{certIds[4], time.Hour + 10*time.Minute + time.Millisecond, true},
			{certIds[5], 3 * time.Hour, true},
		}
		for index, vector := range vectors {
			now := start.Add(vector.after)
			fakeFit, fakeErr := fake.Spend(ctx, ca, vector.certId, now)
			realFit, realErr := real.Spend(ctx, ca, vector.certId, now)
			if fakeErr != nil || realErr != nil {
				t.Fatalf("vector %d: fake %v, redis %v", index, fakeErr, realErr)
			}
			if fakeFit != vector.want || realFit != vector.want {
				t.Fatalf("vector %d: fake %v, redis %v, want %v", index, fakeFit, realFit, vector.want)
			}
		}
		// the key expires 7 days after its newest order, and not before
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, model.TlsCertBudgetKey(ca, settings.RootDomain)).Result()
			if err != nil || ttl <= 0 || settings.BudgetTtl < ttl {
				t.Fatalf("the key's ttl is %v, %v", ttl, err)
			}
		})
	})
}

func TestCertsBudgetRebuildsALostKeyFromItsRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		ca, settings := certsBudgetTestSettings(3)
		store := model.NewCertsStore(nil)
		budget := model.NewCertsBudget(sidecarhost.NewRedis(nil), store, settings)
		now := time.Date(2032, time.June, 1, 12, 0, 0, 0, time.UTC)

		// two new sets in the window, one issued and one under way; one issued before it; one that failed
		row := func(orderTime time.Time) *sidecar.TlsCertRecord {
			set := make([]byte, 32)
			if _, err := rand.Read(set); err != nil {
				t.Fatal(err)
			}
			return &sidecar.TlsCertRecord{
				CertId:              sidecartest.CertsIdAt(orderTime),
				NetworkId:           sidecar.NewId(),
				ClientId:            sidecar.NewId(),
				SidecarId:           sidecar.NewId(),
				IdentifierSetSha256: set,
				SpkiSha256:          make([]byte, 32),
				Attestation:         []byte("attestation"),
				Ca:                  string(ca),
				State:               sidecar.TlsCertStateQueued,
			}
		}
		issued := row(now.Add(-30 * time.Minute))
		issued.State, issued.Serial = sidecar.TlsCertStateIssued, "01"
		issued.NotBefore, issued.NotAfter = now.Add(-30*time.Minute), now.Add(45*24*time.Hour)
		underWay := row(now.Add(-10 * time.Minute))
		underWay.State = sidecar.TlsCertStateOrdering
		old := row(now.Add(-2 * time.Hour))
		old.State, old.Serial = sidecar.TlsCertStateIssued, "02"
		old.NotBefore, old.NotAfter = now.Add(-2*time.Hour), now.Add(45*24*time.Hour)
		failed := row(now.Add(-5 * time.Minute))
		failed.State = sidecar.TlsCertStateFailed
		for _, record := range []*sidecar.TlsCertRecord{issued, underWay, old, failed} {
			if err := store.InsertCert(ctx, record); err != nil {
				t.Fatal(err)
			}
		}

		// no key yet: the first spend rebuilds two entries and takes the third unit, and a fourth does not fit
		first := sidecar.NewId()
		if fit, err := budget.Spend(ctx, ca, first, now); err != nil || !fit {
			t.Fatalf("the spend over a rebuilt key: %v, %v", fit, err)
		}
		if fit, err := budget.Spend(ctx, ca, sidecar.NewId(), now); err != nil || fit {
			t.Fatalf("a fourth unit: %v, %v", fit, err)
		}

		// the key is lost: the rows hold two orders, so one more unit fits and the next does not
		key := model.TlsCertBudgetKey(ca, settings.RootDomain)
		server.Redis(ctx, func(r server.RedisClient) {
			if removed, err := r.Del(ctx, key).Result(); err != nil || removed != 1 {
				t.Fatalf("the budget key was not at %s: %d, %v", key, removed, err)
			}
		})
		if fit, err := budget.Spend(ctx, ca, sidecar.NewId(), now); err != nil || !fit {
			t.Fatalf("the spend after the key was lost: %v, %v", fit, err)
		}
		if fit, err := budget.Spend(ctx, ca, sidecar.NewId(), now); err != nil || fit {
			t.Fatal("a lost key read as an empty budget")
		}
	})
}
