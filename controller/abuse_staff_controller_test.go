package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The abuse role (abuse_staff_controller.go; ABUSE-M5-12): the staff credential's table, the verbs' refusals, the
// read rule TestEveryAbuseReadLogs holds every abuse route and the staff path to, and the verbs on MG5's tables,
// which run in a build with sidecar_pending.

// A verify file of entries, each a bearer with its reviewer, roles and expiry.
func testAbuseStaffFile(entries ...string) []byte {
	return []byte("staff:\n" + strings.Join(entries, ""))
}

// One entry of a verify file for bearer.
func testAbuseStaffEntry(reviewerId server.Id, bearer string, roles string, expires time.Time) string {
	digest := sha256.Sum256([]byte(bearer))
	return fmt.Sprintf(
		"  - reviewer_id: %s\n    sha256: %s\n    roles: [%s]\n    expires: %s\n",
		reviewerId, hex.EncodeToString(digest[:]), roles, expires.UTC().Format(time.RFC3339),
	)
}

// The credential's table: the right bearer names its member and roles; a wrong, empty, expired or too long-lived one
// names no one.
func TestAbuseStaffVerifierCredentialTable(t *testing.T) {
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	settings := DefaultAbuseStaffSettings()
	reviewerId := server.NewId()
	legalId := server.NewId()
	verifier, err := NewAbuseStaffVerifier(testAbuseStaffFile(
		testAbuseStaffEntry(reviewerId, "reviewer-bearer", "reviewer", now.Add(30*24*time.Hour)),
		testAbuseStaffEntry(legalId, "legal-bearer", "legal, engineering", now.Add(90*24*time.Hour)),
		testAbuseStaffEntry(server.NewId(), "expired-bearer", "reviewer", now.Add(-time.Second)),
		testAbuseStaffEntry(server.NewId(), "long-bearer", "reviewer", now.Add(90*24*time.Hour+time.Second)),
	))
	if err != nil {
		t.Fatal(err)
	}

	staff, ok := verifier.Authenticate("reviewer-bearer", now, settings)
	if !ok || staff.ReviewerId != reviewerId || !slices.Equal(staff.Roles, []AbuseStaffRole{AbuseStaffRoleReviewer}) {
		t.Fatalf("the reviewer: %+v, %t", staff, ok)
	}
	staff, ok = verifier.Authenticate("legal-bearer", now, settings)
	if !ok || staff.ReviewerId != legalId || !staff.HoldsAny([]AbuseStaffRole{AbuseStaffRoleEngineering}) || staff.HoldsAny([]AbuseStaffRole{AbuseStaffRoleReviewer}) {
		t.Fatalf("legal at the lifetime's bound: %+v, %t", staff, ok)
	}
	for _, bearer := range []string{"", "wrong-bearer", "reviewer-bearer ", "expired-bearer", "long-bearer"} {
		if staff, ok := verifier.Authenticate(bearer, now, settings); ok || staff != nil {
			t.Errorf("%q named %+v", bearer, staff)
		}
	}
	// a credential expires at its expiry, not after
	if _, ok := verifier.Authenticate("reviewer-bearer", now.Add(30*24*time.Hour), settings); ok {
		t.Fatal("a credential at its expiry passed")
	}
	settings.CredentialMaxLifetime = 0
	if _, ok := verifier.Authenticate("reviewer-bearer", now, settings); ok {
		t.Fatal("a zero lifetime passed a credential")
	}
}

// A verify file with any malformed entry, an unknown field or no staff is refused whole.
func TestAbuseStaffVerifierRefusesABrokenFile(t *testing.T) {
	expires := time.Now().Add(24 * time.Hour)
	good := testAbuseStaffEntry(server.NewId(), "bearer", "reviewer", expires)
	for name, data := range map[string][]byte{
		"empty":         []byte(""),
		"no staff":      []byte("staff: []\n"),
		"unknown field": []byte(strings.Replace(string(testAbuseStaffFile(good)), "roles:", "role:", 1)),
		"bad id":        testAbuseStaffFile(strings.Replace(good, "reviewer_id: ", "reviewer_id: x", 1)),
		"bad digest":    testAbuseStaffFile(strings.Replace(good, "sha256: ", "sha256: zz", 1)),
		"unknown role":  testAbuseStaffFile(testAbuseStaffEntry(server.NewId(), "bearer", "ops", expires)),
		"no role":       testAbuseStaffFile(testAbuseStaffEntry(server.NewId(), "bearer", "", expires)),
		"bad expiry":    testAbuseStaffFile(strings.Replace(good, "expires: ", "expires: soon", 1)),
		"one bad entry": testAbuseStaffFile(good, testAbuseStaffEntry(server.NewId(), "other", "nobody", expires)),
	} {
		if verifier, err := NewAbuseStaffVerifier(data); !errors.Is(err, ErrAbuseStaffFileRefused) || verifier != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := NewAbuseStaffVerifier(testAbuseStaffFile(good)); err != nil {
		t.Fatalf("a good file: %v", err)
	}
}

// A request is refused before anything runs: an unknown kind, a kind the member's roles do not take, a body that is
// not a JSON object, and a field the kind does not take.
func TestAbuseStaffActionRefusesBeforeRunning(t *testing.T) {
	ctx := context.Background()
	reviewer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
	engineer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleEngineering}}
	if _, err := AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"everything"}`)); !errors.Is(err, ErrAbuseStaffKindUnknown) {
		t.Errorf("an unknown kind: %v", err)
	}
	for _, kind := range []string{"lookup_case", "lookup_join", "action", "lift", "review", "decide_appeal", "clear_restore_flag"} {
		if _, err := AbuseStaffAction(ctx, engineer, []byte(`{"kind":"`+kind+`"}`)); !errors.Is(err, ErrAbuseStaffRoleRefused) {
			t.Errorf("%s by an engineer: %v", kind, err)
		}
	}
	legal := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleLegal}}
	if _, err := AbuseStaffAction(ctx, legal, []byte(`{"kind":"decide_appeal"}`)); !errors.Is(err, ErrAbuseStaffRoleRefused) {
		t.Errorf("an appeal decided by legal: %v", err)
	}
	if _, err := AbuseStaffAction(ctx, reviewer, []byte(`[1, 2]`)); !errors.Is(err, ErrAbuseStaffRequestInvalid) {
		t.Errorf("a body that is no object: %v", err)
	}
	if _, err := AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"lookup_case","case_id":"`+server.NewId().String()+`","network_id":"x"}`)); !errors.Is(err, ErrAbuseStaffRequestInvalid) {
		t.Errorf("a field the kind does not take: %v", err)
	}
	if _, err := AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"review","case_id":"`+server.NewId().String()+`","decision":"uphold"}`)); !errors.Is(err, ErrAbuseStaffRequestInvalid) {
		t.Errorf("a review with no reasons: %v", err)
	}
	if names := AbuseStaffKindNames(); !slices.Equal(names, []string{"action", "clear_restore_flag", "close_legal_request", "decide_appeal", "file_legal_request", "lift", "limit_tighten", "lookup_case", "lookup_join", "place_hold", "produce", "renew_hold", "report_csam", "review", "sign_production", "transparency_counts"}) {
		t.Errorf("the table holds %v", names)
	}
}

// The functions of the Go files in dir, by name ("Recv.Name" for a method), with the file each is in.
type testAbuseFuncs struct {
	files     map[string]*ast.File
	funcFiles map[string]string
	funcDecls map[string]*ast.FuncDecl
}

func parseTestAbuseFuncs(t *testing.T, dir string) *testAbuseFuncs {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no Go files in %s: %v", dir, err)
	}
	funcs := &testAbuseFuncs{files: map[string]*ast.File{}, funcFiles: map[string]string{}, funcDecls: map[string]*ast.FuncDecl{}}
	fileSet := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		funcs.files[filepath.Base(path)] = file
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Body == nil {
				continue
			}
			name := testAbuseFuncName(funcDecl)
			funcs.funcFiles[name] = filepath.Base(path)
			funcs.funcDecls[name] = funcDecl
		}
	}
	return funcs
}

// A function's name, "Recv.Name" for a method.
func testAbuseFuncName(funcDecl *ast.FuncDecl) string {
	if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
		return funcDecl.Name.Name
	}
	recvType := funcDecl.Recv.List[0].Type
	if star, ok := recvType.(*ast.StarExpr); ok {
		recvType = star.X
	}
	if ident, ok := recvType.(*ast.Ident); ok {
		return ident.Name + "." + funcDecl.Name.Name
	}
	return funcDecl.Name.Name
}

// SQL naming one of the three tables a role read reaches, after FROM or JOIN.
var testAbuseReadSql = regexp.MustCompile(`(?is)\b(from|join)\s+abuse_(case|action|appeal)\b`)

// The package's functions that read abuse_case, abuse_action or abuse_appeal: those whose body holds such SQL, as a
// literal or through a package-level string it names, and every function of the package that calls one,
// transitively. A method call is matched by its name to any method of that name, which errs toward more readers.
func (self *testAbuseFuncs) readers() map[string]bool {
	readSqlNames := map[string]bool{}
	for _, file := range self.files {
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range genDecl.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, value := range valueSpec.Values {
					ast.Inspect(value, func(node ast.Node) bool {
						if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING && testAbuseReadSql.MatchString(lit.Value) {
							readSqlNames[valueSpec.Names[index].Name] = true
						}
						return true
					})
				}
			}
		}
	}
	methodNames := map[string][]string{}
	for name := range self.funcDecls {
		if _, method, ok := strings.Cut(name, "."); ok {
			methodNames[method] = append(methodNames[method], name)
		}
	}
	direct := map[string]bool{}
	calls := map[string][]string{}
	for name, funcDecl := range self.funcDecls {
		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.BasicLit:
				if node.Kind == token.STRING && testAbuseReadSql.MatchString(node.Value) {
					direct[name] = true
				}
			case *ast.Ident:
				if readSqlNames[node.Name] {
					direct[name] = true
				}
			case *ast.CallExpr:
				switch fun := node.Fun.(type) {
				case *ast.Ident:
					calls[name] = append(calls[name], fun.Name)
				case *ast.SelectorExpr:
					calls[name] = append(calls[name], methodNames[fun.Sel.Name]...)
				}
			}
			return true
		})
	}
	readers := direct
	for changed := true; changed; {
		changed = false
		for name, callees := range calls {
			if readers[name] {
				continue
			}
			for _, callee := range callees {
				if readers[callee] {
					readers[name] = true
					changed = true
					break
				}
			}
		}
	}
	return readers
}

// The calls in file to package model's readers that allowed does not hold, and to this package's readers (same
// package calls by name) that localAllowed does not hold, each as "<file>: <call>".
func testAbuseReadViolations(fileName string, file *ast.File, modelReaders map[string]bool, allowed map[string]bool, localReaders map[string]bool, localAllowed map[string]bool) []string {
	modelAlias := ""
	for _, importSpec := range file.Imports {
		if path, _ := strconv.Unquote(importSpec.Path.Value); path == "github.com/urnetwork/server/model" {
			modelAlias = "model"
			if importSpec.Name != nil {
				modelAlias = importSpec.Name.Name
			}
		}
	}
	violations := []string{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := fun.X.(*ast.Ident); ok && modelAlias != "" && pkg.Name == modelAlias {
				if modelReaders[fun.Sel.Name] && !allowed[fun.Sel.Name] {
					violations = append(violations, fileName+": model."+fun.Sel.Name)
				}
			}
		case *ast.Ident:
			if localReaders[fun.Name] && !localAllowed[fun.Name] {
				violations = append(violations, fileName+": "+fun.Name)
			}
		}
		return true
	})
	return violations
}

// §21.10's rule, held by the code's shape: no abuse route and nothing on the staff path reads abuse_case,
// abuse_action or abuse_appeal but through model.AbuseReader, which logs each read first, or through the writes a
// review, decision or flag runs on, each answering only what it wrote, or, on the customer routes alone, the caller's
// own network's reads. A new read path that skips the reader fails here, not in a review; the scanner's own fixtures
// show it catches one.
func TestEveryAbuseReadLogs(t *testing.T) {
	modelFuncs := parseTestAbuseFuncs(t, filepath.Join("..", "model"))
	modelReaders := modelFuncs.readers()
	for _, reader := range []string{"GetAbuseCase", "GetAbuseCaseActions", "GetNetworkAbuseHistory", "ListOwedAbuseActions", "AbuseReader.LookupAbuseCase", "ListNetworkAbuseCases"} {
		if !modelReaders[reader] {
			t.Fatalf("the scan does not see %s read the tables", reader)
		}
	}
	controllerFuncs := parseTestAbuseFuncs(t, ".")
	controllerReaders := map[string]bool{}
	for name, funcDecl := range controllerFuncs.funcDecls {
		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "model" && modelReaders[selector.Sel.Name] {
						controllerReaders[name] = true
					}
				}
			}
			return true
		})
	}
	// the staff path's writes: the role's reads go through the reader, its writes through these
	staffAllowed := map[string]bool{
		"NewAbuseReader":      true,
		"RecordAbuseReview":   true,
		"DecideAbuseAppeal":   true,
		"ClearDnsRestoreFlag": true,
	}
	staffLocalAllowed := map[string]bool{
		"ApplyAbuseAction": true,
		"LiftAbuseAction":  true,
	}
	violations := testAbuseReadViolations("abuse_staff_controller.go", controllerFuncs.files["abuse_staff_controller.go"], modelReaders, staffAllowed, controllerReaders, staffLocalAllowed)

	handlerDir := filepath.Join("..", "api", "handlers")
	handlerPaths, err := filepath.Glob(filepath.Join(handlerDir, "abuse_*.go"))
	if err != nil {
		t.Fatal(err)
	}
	customerAllowed := map[string]bool{
		"ListNetworkAbuseCases": true,
		"GetNetworkAbuseStatus": true,
		"CreateAbuseAppeal":     true,
		// a count of a trusted flagger's own machine kills, which the report route reads to cap the flagger and
		// never shows: its answer is one body either way (ABUSE-M8-01)
		"AbuseFlaggerReportClass": true,
	}
	scanned := 0
	for _, path := range handlerPaths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{}
		if filepath.Base(path) == "abuse_handlers.go" {
			allowed = customerAllowed
		}
		violations = append(violations, testAbuseReadViolations(filepath.Base(path), file, modelReaders, allowed, nil, nil)...)
		scanned += 1
	}
	if scanned < 2 {
		t.Fatalf("scanned %d abuse route files", scanned)
	}
	if 0 < len(violations) {
		t.Fatalf("reads that skip model.AbuseReader:\n%s", strings.Join(violations, "\n"))
	}

	// the scanner's own fixtures: a direct read is caught, the reader's and a write are not
	fixture, err := parser.ParseFile(token.NewFileSet(), "fixture.go", `package handlers
import "github.com/urnetwork/server/model"
func read(reader *model.AbuseReader) {
	model.GetAbuseCase(nil, model.NewId())
	model.GetNetworkAbuseHistory(nil, model.NewId())
	reader.LookupAbuseCase(nil, model.NewId())
	model.RecordAbuseReview(nil, nil, model.Now(), nil)
}
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	caught := testAbuseReadViolations("fixture.go", fixture, modelReaders, staffAllowed, nil, nil)
	if !slices.Equal(caught, []string{"fixture.go: model.GetAbuseCase", "fixture.go: model.GetNetworkAbuseHistory"}) {
		t.Fatalf("the scanner caught %v", caught)
	}
}

// The staff path's verbs on MG5's tables: a review's lift ends the action through §21's one path, with its
// reviewer, its effect and its record; a grant lifts every standing action as wrong and closes the case.
func TestAbuseStaffReviewLiftEndsTheAction(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		_, _, caseId := newTestAbuseCase(t, ctx, false)
		kill, err := ApplyAbuseAction(ctx, testAbuseKillArgs(caseId))
		if err != nil {
			t.Fatal(err)
		}
		reviewer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
		body, _ := json.Marshal(map[string]any{
			"kind":      "review",
			"case_id":   caseId,
			"action_id": kill.Action.ActionId,
			"decision":  "lift_wrong",
			"reason":    "another network's page",
		})
		result, err := AbuseStaffAction(ctx, reviewer, body)
		if err != nil {
			t.Fatal(err)
		}
		review := result.Result.(*AbuseStaffReviewResult)
		if result.Kind != "review" || review.Lift == nil || !review.Lift.Inserted || review.Lift.Action.Kind != string(model.AbuseActionKindLift) {
			t.Fatalf("the review's lift: %+v", review)
		}
		lifted := model.GetAbuseAction(ctx, kill.Action.ActionId)
		if lifted.ReviewState != model.AbuseReviewStateLiftedWrong || lifted.ReviewerId == nil || *lifted.ReviewerId != reviewer.ReviewerId || lifted.ReviewReason != "another network's page" {
			t.Fatalf("the lifted action: %+v", lifted)
		}
		applied := effects.applied()
		if len(applied) != 2 || applied[1].Lifted == nil || applied[1].Lifted.ActionId != kill.Action.ActionId {
			t.Fatalf("the lift ran no effect: %d effects", len(applied))
		}
		if len(mailer.mailed()) != 2 {
			t.Fatalf("%d records mailed, want the kill's and the lift's", len(mailer.mailed()))
		}
		if abuseCase := model.GetAbuseCase(ctx, caseId); abuseCase.State != model.AbuseCaseStateClosed {
			t.Fatalf("the case after its last lift: %s", abuseCase.State)
		}
	})
}

// A granted appeal lifts each standing action of the case as wrong, by the second reviewer, and the case closes;
// deciding it again lifts nothing more.
func TestAbuseStaffDecideAppealGrantLiftsAsWrong(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseDeps(t)
		networkId, _, caseId := newTestAbuseCase(t, ctx, false)
		first := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
		second := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
		defer server.Testing_PushAbuseConfig("protected_names: {roots: [root.example], platform: [platform.example]}\n")()

		run := func(staff *AbuseStaff, request map[string]any) (*AbuseStaffResult, error) {
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			return AbuseStaffAction(ctx, staff, body)
		}
		actionResult, err := run(first, map[string]any{"kind": "action", "case_id": caseId, "action_kind": "host_kill", "target": "shop.root.example", "reason": "phishing confirmed"})
		if err != nil {
			t.Fatal(err)
		}
		action := actionResult.Result.(*AbuseStaffActionResult).Action
		if action.ReviewState != string(model.AbuseReviewStateUpheld) || action.ByMachine || *action.ReviewerId != first.ReviewerId {
			t.Fatalf("a person's action: %+v", action)
		}
		if _, err := run(first, map[string]any{"kind": "action", "case_id": caseId, "action_kind": "label_kill", "target": "root.example", "reason": "the root"}); !errors.Is(err, model.ErrAbuseTargetRefused) {
			t.Fatalf("an action on the root: %v", err)
		}

		appeal, _, err := model.CreateAbuseAppeal(ctx, networkId, server.NewId(), &model.AbuseAppealArgs{CaseId: caseId, Reason: "the page was ours, and legitimate"}, server.NowUtc(), model.DefaultAbuseCustomerSettings())
		if err != nil {
			t.Fatal(err)
		}
		decide := map[string]any{"kind": "decide_appeal", "appeal_id": appeal.AppealId, "decision": "granted", "reason": "the login page is the network's own"}
		if _, err := run(first, decide); !errors.Is(err, model.ErrAbuseAppealSameReviewer) {
			t.Fatalf("the action's taker decided: %v", err)
		}
		decided, err := run(second, decide)
		if err != nil {
			t.Fatal(err)
		}
		decision := decided.Result.(*AbuseStaffDecideAppealResult)
		if decision.Appeal.State != string(model.AbuseAppealStateGranted) || len(decision.Lifts) != 1 || decision.Lifts[0].Action.Target != action.ActionId.String() {
			t.Fatalf("the grant: %+v", decision)
		}
		lifted := model.GetAbuseAction(ctx, action.ActionId)
		if lifted.ReviewState != model.AbuseReviewStateLiftedWrong || *lifted.ReviewerId != second.ReviewerId {
			t.Fatalf("the granted action: %+v", lifted)
		}
		if abuseCase := model.GetAbuseCase(ctx, caseId); abuseCase.State != model.AbuseCaseStateClosed {
			t.Fatalf("the case after a grant: %s", abuseCase.State)
		}
		again, err := run(second, decide)
		if err != nil || len(again.Result.(*AbuseStaffDecideAppealResult).Lifts) != 0 {
			t.Fatalf("the grant again: %+v, %v", again, err)
		}
	})
}

// The lookups answer through the reader, so each writes its access log row with the member's id.
func TestAbuseStaffLookupsLogTheirReads(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, caseId := newTestAbuseCase(t, ctx, true)
		legal := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleLegal}}
		body, _ := json.Marshal(map[string]any{"kind": "lookup_case", "case_id": caseId})
		result, err := AbuseStaffAction(ctx, legal, body)
		if err != nil {
			t.Fatal(err)
		}
		caseFile := result.Result.(*AbuseStaffCaseFileResult)
		if caseFile.Case.CaseId != caseId || !caseFile.Case.Barred || caseFile.Case.JoinKey != "shop.root.example" {
			t.Fatalf("the case read: %+v", caseFile.Case)
		}
		body, _ = json.Marshal(map[string]any{"kind": "lookup_join", "join_kind": "host", "join_key": "SHOP.root.example"})
		if result, err := AbuseStaffAction(ctx, legal, body); err != nil || len(result.Result.(*AbuseStaffJoinResult).Cases) != 1 {
			t.Fatalf("the join read: %+v, %v", result, err)
		}
		body, _ = json.Marshal(map[string]any{"kind": "lookup_join", "join_kind": "host", "join_key": ""})
		if _, err := AbuseStaffAction(ctx, legal, body); !errors.Is(err, model.ErrAbuseLookupInvalid) {
			t.Fatalf("a join read with no key: %v", err)
		}
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_access_log WHERE reviewer_id = $1`, legal.ReviewerId).Scan(&count))
		})
		if count != 2 {
			t.Fatalf("%d access log rows, want the two reads", count)
		}
	})
}

// The controller's tests run from its own directory, which TestEveryAbuseReadLogs reads its siblings from.
func TestAbuseStaffTestsRunFromTheControllerDirectory(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "model", "abuse_access_model.go")); err != nil {
		t.Fatalf("the model's sources are not beside the controller's: %v", err)
	}
}
