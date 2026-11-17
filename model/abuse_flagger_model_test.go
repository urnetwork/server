package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.8's trusted flaggers (abuse_flagger_model.go; ABUSE-M8-01): the vault file under abuse.yml's list, the
// constant-time compare by a source check, the reloads, the case's class and the day's kill cap.

// A vault file holding each id's key's digest.
func testAbuseFlaggerFile(idKeys map[string]string) []byte {
	text := "flaggers:\n"
	for id, key := range idKeys {
		digest := sha256.Sum256([]byte(key))
		text += "  - id: " + id + "\n    sha256: " + hex.EncodeToString(digest[:]) + "\n"
	}
	return []byte(text)
}

// A listed flagger's key matches it; a wrong key, an empty one and a key whose id abuse.yml no longer lists match
// none; a malformed file is refused whole.
func TestAbuseFlaggerVerifierAuthenticates(t *testing.T) {
	file := testAbuseFlaggerFile(map[string]string{"flagger-a": "synthetic-key-a", "flagger-b": "synthetic-key-b"})
	verifier, err := NewAbuseFlaggerVerifier(file, []string{"flagger-a"})
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := verifier.Authenticate("synthetic-key-a"); !ok || id != "flagger-a" {
		t.Fatalf("the listed key: %q %t", id, ok)
	}
	for _, key := range []string{"synthetic-key-b", "synthetic-key-c", ""} {
		if id, ok := verifier.Authenticate(key); ok {
			t.Errorf("%q matched %s", key, id)
		}
	}
	for name, bad := range map[string]string{
		"a digest that is no hex": "flaggers:\n  - id: flagger-a\n    sha256: zz\n",
		"an id with capitals":     "flaggers:\n  - id: Flagger\n    sha256: " + hex.EncodeToString(make([]byte, 32)) + "\n",
		"an id twice":             string(testAbuseFlaggerFile(map[string]string{"flagger-a": "k"})) + "  - id: flagger-a\n    sha256: " + hex.EncodeToString(make([]byte, 32)) + "\n",
		"an unknown field":        "flaggers:\n  - id: flagger-a\n    key: plaintext\n",
	} {
		if _, err := NewAbuseFlaggerVerifier([]byte(bad), []string{"flagger-a"}); !errors.Is(err, ErrAbuseFlaggerFileRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var none *AbuseFlaggerVerifier
	if _, ok := none.Authenticate("synthetic-key-a"); ok {
		t.Fatal("no verifier matched a key")
	}
}

// The compare is constant time, by the source: Authenticate compares digests with subtle.ConstantTimeCompare over
// every entry and returns nowhere inside its loop, so no early exit says which entry matched.
func TestAbuseFlaggerComparesInConstantTime(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "abuse_flagger_model.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range file.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Name.Name != "Authenticate" {
			continue
		}
		found = true
		compares := 0
		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				if selector, ok := node.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "bytes" && selector.Sel.Name == "Equal" {
						t.Error("Authenticate compares with bytes.Equal")
					}
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "subtle" && selector.Sel.Name == "ConstantTimeCompare" {
						compares += 1
					}
				}
			case *ast.RangeStmt:
				ast.Inspect(node.Body, func(inner ast.Node) bool {
					switch inner.(type) {
					case *ast.ReturnStmt, *ast.BranchStmt:
						t.Error("Authenticate leaves its loop early")
					}
					return true
				})
			case *ast.BinaryExpr:
				if node.Op == token.EQL {
					if _, ok := node.X.(*ast.IndexExpr); ok {
						t.Error("Authenticate compares digests with ==")
					}
				}
			}
			return true
		})
		if compares != 1 {
			t.Errorf("Authenticate calls subtle.ConstantTimeCompare %d times", compares)
		}
	}
	if !found {
		t.Fatal("no Authenticate in the file")
	}
}

// A flagger's case keeps its id as its class, which reads back; another class names no flagger.
func TestAbuseFlaggerReporterClassRoundTrips(t *testing.T) {
	class := AbuseFlaggerReporterClass("flagger-a")
	if class != "trusted_flagger:flagger-a" {
		t.Fatalf("the class %q", class)
	}
	if id, ok := AbuseFlaggerIdOf(class); !ok || id != "flagger-a" {
		t.Fatalf("read back %q %t", id, ok)
	}
	for _, other := range []AbuseReporterClass{AbuseReporterClassPublic, AbuseReporterClassTrustedFlagger, "trusted_flagger:", "trusted_flagger:Not An Id"} {
		if _, ok := AbuseFlaggerIdOf(other); ok {
			t.Errorf("%q names a flagger", other)
		}
	}
	report := &AbuseReport{Category: AbuseCategoryPhishing, ReporterClass: AbuseReporterClassTrustedFlagger, FlaggerId: "flagger-a"}
	if err := report.Validate(); err != nil || report.caseReporterClass() != class {
		t.Fatalf("a flagger's report: %v, %q", err, report.caseReporterClass())
	}
	for _, bad := range []*AbuseReport{
		{Category: AbuseCategoryPhishing, ReporterClass: AbuseReporterClassPublic, FlaggerId: "flagger-a"},
		{Category: AbuseCategoryPhishing, ReporterClass: AbuseReporterClassTrustedFlagger, FlaggerId: "Not An Id"},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrAbuseReportInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// The source reads once a ReloadInterval, and a read that fails answers no flagger.
func TestAbuseFlaggerSourceReloads(t *testing.T) {
	loads := 0
	fail := false
	file := testAbuseFlaggerFile(map[string]string{"flagger-a": "synthetic-key-a"})
	source := NewAbuseFlaggerSource(func() (*AbuseFlaggerVerifier, error) {
		loads += 1
		if fail {
			return nil, ErrAbuseFlaggerFileRefused
		}
		return NewAbuseFlaggerVerifier(file, []string{"flagger-a"})
	}, nil)
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	if _, ok := source.Verifier(now).Authenticate("synthetic-key-a"); !ok || loads != 1 {
		t.Fatalf("the first read: %d loads", loads)
	}
	if _, ok := source.Verifier(now.Add(59 * time.Second)).Authenticate("synthetic-key-a"); !ok || loads != 1 {
		t.Fatalf("inside the interval: %d loads", loads)
	}
	fail = true
	if _, ok := source.Verifier(now.Add(61 * time.Second)).Authenticate("synthetic-key-a"); ok || loads != 2 {
		t.Fatalf("a failed read: %d loads", loads)
	}
}

// A flagger's reports take its class until its machine kills in the day reach the cap, then §21.8's report row; a
// cap of 0 lets none kill.
func TestAbuseFlaggerKillCap(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseFlaggerSettings()
		settings.KillsPerDay = 2
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "flagger-cap-network", server.NewId())
		now := server.NowUtc()
		kill := func(at time.Time, host string) {
			server.Tx(ctx, func(tx server.PgTx) {
				caseId, _, err := OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
					NetworkId:     &networkId,
					Category:      AbuseCategoryPhishing,
					Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: host},
					ReporterClass: AbuseFlaggerReporterClass("flagger-a"),
					JoinKind:      AbuseJoinKindHost,
					JoinKey:       host,
				}, at, DefaultAbuseSettings())
				if err != nil {
					t.Fatal(err)
				}
				if _, _, _, err := RecordAbuseActionInTx(ctx, tx, &AbuseActionArgs{ActionId: server.NewId(), CaseId: caseId, Kind: AbuseActionKindHostKill, Target: host}, at, DefaultAbuseSettings()); err != nil {
					t.Fatal(err)
				}
			})
		}
		classOf := func() AbuseReporterClass {
			class, _ := AbuseFlaggerReportClass(ctx, "flagger-a", now, settings)
			return class
		}
		kill(now.Add(-30*time.Hour), "old.flagger.example")
		kill(now.Add(-time.Hour), "one.flagger.example")
		if class := classOf(); class != AbuseReporterClassTrustedFlagger {
			t.Fatalf("under the cap: %s", class)
		}
		kill(now.Add(-time.Minute), "two.flagger.example")
		if class, flaggerId := AbuseFlaggerReportClass(ctx, "flagger-a", now, settings); class != AbuseReporterClassPublic || flaggerId != "" {
			t.Fatalf("at the cap: %s %q", class, flaggerId)
		}
		if class, _ := AbuseFlaggerReportClass(ctx, "flagger-b", now, settings); class != AbuseReporterClassTrustedFlagger {
			t.Fatalf("another flagger: %s", class)
		}
		settings.KillsPerDay = 0
		if class, _ := AbuseFlaggerReportClass(ctx, "flagger-b", now, settings); class != AbuseReporterClassPublic {
			t.Fatalf("a cap of none: %s", class)
		}
		defer server.Testing_PushAbuseConfig("flagger_kills_per_day: 5\ntrusted_flaggers: [{id: flagger-a}, {id: Bad Id}]\n")()
		if killsPerDay := AbuseFlaggerSettingsOf(server.GetAbuseConfig()).KillsPerDay; killsPerDay != 5 {
			t.Fatalf("abuse.yml's cap read %d", killsPerDay)
		}
		if ids := server.GetAbuseConfig().TrustedFlaggerIds(); len(ids) != 1 || ids[0] != "flagger-a" {
			t.Fatalf("abuse.yml's flaggers read %v", ids)
		}
	})
}
