//go:build linux

// Profile regressions interpret the admitted predicates and exercise the real
// worker HTTP admission with private pinned files and fake daemons. They do not
// load seccomp, crash a process, or qualify native Docker/kernel ABI behavior.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This synthetic default-errno profile has the unconditional permission that
// made an appended denial unsafe. Deployment must pin its actual stock export.
func testCandidateCoreStockProfile() []byte {
	return []byte(`{"defaultAction":"SCMP_ACT_ERRNO","defaultErrnoRet":1,"architectures":["SCMP_ARCH_X86_64","SCMP_ARCH_X86","SCMP_ARCH_X32"],"syscalls":[{"names":["read","write","setrlimit","prlimit64"],"action":"SCMP_ACT_ALLOW"}]}`)
}

// The shared proxy and agent fixtures use the real transformation and loader,
// not an exemption for missing profile authority in unprotected test mode.
func testCandidateCoreProfilePins(t *testing.T, directory string) (Pin, Pin) {
	t.Helper()
	stock := testCandidateCoreStockProfile()
	derived, err := TransformCandidateCoreProfile(stock, DefaultCandidateCoreProfileSettings())
	if err != nil {
		t.Fatal(err)
	}
	stockPin := Pin{Path: filepath.Join(directory, "stock-seccomp.json"), Sha256: digestDockerBytes(stock)}
	candidatePin := Pin{Path: filepath.Join(directory, "candidate-core-profile.json"), Sha256: digestDockerBytes(derived)}
	for _, document := range []struct {
		pin     Pin
		content []byte
	}{{pin: stockPin, content: stock}, {pin: candidatePin, content: derived}} {
		if err := os.WriteFile(document.pin.Path, document.content, 0400); err != nil {
			t.Fatal(err)
		}
	}
	return stockPin, candidatePin
}

// Test mutations pass through the same strict profile reader as production.
func candidateCoreTestProfile(t *testing.T, content []byte) *candidateSeccompProfile {
	t.Helper()
	profile, err := parseCandidateCoreProfile(content, DefaultCandidateCoreProfileSettings())
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

// This intentionally small interpreter covers only the generated allow
// predicate grammar. It does not substitute for libseccomp's native ABI gate.
func candidateCoreTestAllows(t *testing.T, profile *candidateSeccompProfile, name string, arguments [6]uint64) bool {
	t.Helper()
	for _, rule := range profile.Syscalls {
		if rule.Action != "SCMP_ACT_ALLOW" || !emptyCandidateSeccompCondition(rule.Includes) || !emptyCandidateSeccompCondition(rule.Excludes) {
			continue
		}
		found := false
		for _, ruleName := range rule.Names {
			found = found || ruleName == name
		}
		if !found {
			continue
		}
		matched := true
		seen := map[uint]bool{}
		for _, argument := range rule.Args {
			if seen[argument.Index] {
				t.Fatal("generated resource rule relied on repeated-index conjunction")
			}
			seen[argument.Index] = true
			switch argument.Op {
			case "SCMP_CMP_EQ":
				matched = matched && arguments[argument.Index] == argument.Value
			case "SCMP_CMP_MASKED_EQ":
				matched = matched && arguments[argument.Index]&argument.Value == argument.ValueTwo
			default:
				t.Fatalf("unexpected generated comparison: %s", argument.Op)
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// Removing the stock setter names is essential: a default-errno profile with
// its broad allow intact cannot implement the desired write restriction.
func TestCandidateCoreProfileReplacesUnconditionalAllow(t *testing.T) {
	stock := testCandidateCoreStockProfile()
	before := candidateCoreTestProfile(t, stock)
	coreWrite := [6]uint64{0, 4, 4096, 0, 0, 0}
	if !candidateCoreTestAllows(t, before, "prlimit64", coreWrite) {
		t.Fatal("regression fixture did not reproduce unconditional stock permission")
	}
	derived, err := TransformCandidateCoreProfile(stock, DefaultCandidateCoreProfileSettings())
	if err != nil {
		t.Fatal(err)
	}
	after := candidateCoreTestProfile(t, derived)
	if candidateCoreTestAllows(t, after, "prlimit64", coreWrite) || candidateCoreTestAllows(t, after, "setrlimit", [6]uint64{4, 4096}) {
		t.Fatal("stock allow bypassed the core write restriction")
	}
	for _, name := range []string{"read", "write"} {
		if !candidateCoreTestAllows(t, after, name, [6]uint64{}) {
			t.Fatalf("unrelated stock permission was removed: %s", name)
		}
	}
	if !bytes.Equal(stock, testCandidateCoreStockProfile()) {
		t.Fatal("transformation mutated caller-owned stock bytes")
	}
}

// x86-64, compat i386, and x32 resolve the same syscall names through the
// explicit architecture map. Resource conversion is low-word on every path.
func TestCandidateCoreProfileMasksResourceAcrossX86AbiClasses(t *testing.T) {
	derived, err := TransformCandidateCoreProfile(testCandidateCoreStockProfile(), DefaultCandidateCoreProfileSettings())
	if err != nil {
		t.Fatal(err)
	}
	profile := candidateCoreTestProfile(t, derived)
	for _, abi := range []struct {
		architecture  string
		setNumber     uint64
		prlimitNumber uint64
	}{
		{architecture: "SCMP_ARCH_X86_64", setNumber: 160, prlimitNumber: 302},
		{architecture: "SCMP_ARCH_X86", setNumber: 75, prlimitNumber: 340},
		{architecture: "SCMP_ARCH_X32", setNumber: 0x40000000 + 160, prlimitNumber: 0x40000000 + 302},
	} {
		found := false
		for _, architecture := range profile.Architectures {
			found = found || architecture == abi.architecture
		}
		if !found || abi.setNumber == abi.prlimitNumber {
			t.Fatalf("explicit ABI coverage was lost: %+v", abi)
		}
		for _, resource := range []uint64{4, 0x100000004, 0xffffffff00000004} {
			if candidateCoreTestAllows(t, profile, "setrlimit", [6]uint64{resource, 4096}) ||
				candidateCoreTestAllows(t, profile, "prlimit64", [6]uint64{0, resource, 4096}) {
				t.Fatalf("high resource bits bypassed core restriction: abi=%s resource=%x", abi.architecture, resource)
			}
			if !candidateCoreTestAllows(t, profile, "prlimit64", [6]uint64{0, resource, 0, 8192}) {
				t.Fatalf("canonical null-new-limit query was denied: abi=%s resource=%x", abi.architecture, resource)
			}
		}
	}
}

// Neither a null old-limit pointer nor a different target pid converts a
// non-null new-limit pointer into a query. No pointed-to struct is inspected.
func TestCandidateCoreProfileAllowsOnlyNullNewLimitForCore(t *testing.T) {
	derived, err := TransformCandidateCoreProfile(testCandidateCoreStockProfile(), DefaultCandidateCoreProfileSettings())
	if err != nil {
		t.Fatal(err)
	}
	profile := candidateCoreTestProfile(t, derived)
	for _, pid := range []uint64{0, 17, 0xffffffff, 0xffffffffffffffff} {
		for _, oldLimit := range []uint64{0, 4096, 0xffffffffffffffff} {
			for _, newLimit := range []uint64{1, 4096, 0x100000000, 0xffffffffffffffff} {
				if candidateCoreTestAllows(t, profile, "prlimit64", [6]uint64{pid, 4, newLimit, oldLimit}) {
					t.Fatalf("core write acquired permission: pid=%x new=%x old=%x", pid, newLimit, oldLimit)
				}
			}
			if !candidateCoreTestAllows(t, profile, "prlimit64", [6]uint64{pid, 4, 0, oldLimit}) {
				t.Fatal("null-new-limit read query lost its stock permission")
			}
		}
	}
	for resource := uint64(0); resource < candidateResourceEnd; resource++ {
		if resource == candidateCoreResource {
			continue
		}
		if !candidateCoreTestAllows(t, profile, "setrlimit", [6]uint64{resource | 0x100000000, 4096}) ||
			!candidateCoreTestAllows(t, profile, "prlimit64", [6]uint64{0, resource, 4096, 0}) {
			t.Fatalf("another valid resource was unintentionally denied: %d", resource)
		}
	}
	if candidateCoreTestAllows(t, profile, "setrlimit", [6]uint64{16, 4096}) {
		t.Fatal("unreviewed future resource acquired permission")
	}
}

// Conditional stock allows stay conditional, and unrelated errno rules,
// architecture mappings, comments and arguments are retained without widening.
func TestCandidateCoreProfilePreservesStockConditionsAndOtherRules(t *testing.T) {
	profile := candidateCoreTestProfile(t, testCandidateCoreStockProfile())
	profile.Architectures = nil
	profile.ArchMap = []candidateSeccompArchitecture{
		{Architecture: "SCMP_ARCH_X86_64", SubArchitectures: []string{"SCMP_ARCH_X86", "SCMP_ARCH_X32"}},
		{Architecture: "SCMP_ARCH_AARCH64", SubArchitectures: []string{"SCMP_ARCH_ARM"}},
	}
	conditional := candidateSeccompSyscall{Names: []string{"setrlimit", "prlimit64"}, Action: "SCMP_ACT_ALLOW", Comment: "synthetic retained condition",
		Includes: &candidateSeccompCondition{Caps: []string{"CAP_SYS_RESOURCE"}, Arches: []string{"amd64"}, MinKernel: "6.8"},
		Excludes: &candidateSeccompCondition{Arches: []string{"arm64"}}}
	errno := uint32(38)
	unrelated := candidateSeccompSyscall{Names: []string{"clone3"}, Action: "SCMP_ACT_ERRNO", ErrnoRet: &errno, Comment: "synthetic errno fallback",
		Args: []candidateSeccompArgument{{Index: 0, Value: 17, Op: "SCMP_CMP_EQ"}}}
	profile.Syscalls = append(profile.Syscalls, conditional, unrelated)
	stock, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := TransformCandidateCoreProfile(stock, DefaultCandidateCoreProfileSettings())
	if err != nil {
		t.Fatal(err)
	}
	after := candidateCoreTestProfile(t, derived)
	if !reflect.DeepEqual(profile.ArchMap, after.ArchMap) {
		t.Fatal("stock architecture mappings were changed")
	}
	foundCondition, foundUnrelated := 0, false
	for _, rule := range after.Syscalls {
		if rule.Comment == conditional.Comment {
			foundCondition++
			if !reflect.DeepEqual(rule.Includes, conditional.Includes) || !reflect.DeepEqual(rule.Excludes, conditional.Excludes) {
				t.Fatal("generated rule generalized a stock condition")
			}
		}
		foundUnrelated = foundUnrelated || reflect.DeepEqual(rule, unrelated)
	}
	if foundCondition != 31 || !foundUnrelated {
		t.Fatalf("stock condition/rule preservation incomplete: %d %v", foundCondition, foundUnrelated)
	}
}

// Unsupported stock authority is refused, not normalized into a weaker
// profile. In particular, default allow and partial compat maps are not safe.
func TestCandidateCoreProfileRejectsUnreviewedStockClasses(t *testing.T) {
	for _, mutate := range []func(*candidateSeccompProfile){
		func(profile *candidateSeccompProfile) { profile.DefaultAction = "SCMP_ACT_ALLOW" },
		func(profile *candidateSeccompProfile) { errno := uint32(0); profile.DefaultErrnoRet = &errno },
		func(profile *candidateSeccompProfile) {
			profile.Architectures = []string{"SCMP_ARCH_X86_64", "SCMP_ARCH_X86"}
		},
		func(profile *candidateSeccompProfile) { profile.Architectures = []string{"SCMP_ARCH_NATIVE"} },
		func(profile *candidateSeccompProfile) {
			profile.Architectures = append(profile.Architectures, "SCMP_ARCH_X86")
		},
		func(profile *candidateSeccompProfile) {
			profile.ArchMap = []candidateSeccompArchitecture{{Architecture: "SCMP_ARCH_X86_64"}}
		},
		func(profile *candidateSeccompProfile) { profile.Flags = []string{"SECCOMP_FILTER_FLAG_LOG"} },
		func(profile *candidateSeccompProfile) { profile.ListenerPath = "/run/synthetic-notify.sock" },
		func(profile *candidateSeccompProfile) { profile.Syscalls[0].Action = "SCMP_ACT_LOG" },
		func(profile *candidateSeccompProfile) { profile.Syscalls[0].Action = "SCMP_ACT_NOTIFY" },
		func(profile *candidateSeccompProfile) { profile.Syscalls[0].Names = []string{"read", "setrlimit"} },
		func(profile *candidateSeccompProfile) {
			profile.Syscalls[0].Args = []candidateSeccompArgument{{Index: 0, Value: 4, Op: "SCMP_CMP_NE"}}
		},
		func(profile *candidateSeccompProfile) {
			profile.Syscalls[0].Includes = &candidateSeccompCondition{MinKernel: "6.8"}
		},
	} {
		profile := candidateCoreTestProfile(t, testCandidateCoreStockProfile())
		mutate(profile)
		stock, err := json.Marshal(profile)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := TransformCandidateCoreProfile(stock, DefaultCandidateCoreProfileSettings()); err == nil {
			t.Fatalf("unreviewed stock profile was transformed: %s", stock)
		}
	}
}

// Case-folding and duplicate aliases are rejected before the typed decoder.
func TestCandidateCoreProfileRejectsDuplicateCaseAliasedAndTrailingJson(t *testing.T) {
	stock := string(testCandidateCoreStockProfile())
	for _, content := range []string{
		strings.Replace(stock, `"defaultAction":`, `"DefaultAction":`, 1),
		strings.Replace(stock, `"defaultAction":"SCMP_ACT_ERRNO"`, `"defaultAction":"SCMP_ACT_ALLOW","defaultAction":"SCMP_ACT_ERRNO"`, 1),
		strings.Replace(stock, `"action":"SCMP_ACT_ALLOW"`, `"action":"SCMP_ACT_ALLOW","Action":"SCMP_ACT_ERRNO"`, 1),
		strings.Replace(stock, `"defaultErrnoRet":1`, `"defaultErrnoRet":1,"unknownAuthority":false`, 1),
		stock + " {}",
		"null",
	} {
		if _, err := TransformCandidateCoreProfile([]byte(content), DefaultCandidateCoreProfileSettings()); err == nil {
			t.Fatalf("ambiguous profile json was accepted: %s", content)
		}
	}
}

// The derived pin is checked against both its own bytes and the exact stock
// transformation; rehashing a tampered allow list cannot bless the new policy.
func TestCandidateCoreProfilePinsRejectTamperedDerivedAuthority(t *testing.T) {
	stockPin, candidatePin := testCandidateCoreProfilePins(t, t.TempDir())
	settings := DefaultCandidateCoreProfileSettings()
	canonical, err := ValidateCandidateCoreProfiles(stockPin, candidatePin, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	profile := candidateCoreTestProfile(t, canonical)
	profile.Syscalls = append(profile.Syscalls, candidateSeccompSyscall{Names: []string{"setrlimit", "prlimit64"}, Action: "SCMP_ACT_ALLOW"})
	tampered, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(candidatePin.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidatePin.Path, tampered, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCandidateCoreProfiles(stockPin, candidatePin, settings, false); err == nil {
		t.Fatal("changed profile bytes retained an old pin")
	}
	candidatePin.Sha256 = digestDockerBytes(tampered)
	if _, err := ValidateCandidateCoreProfiles(stockPin, candidatePin, settings, false); err == nil {
		t.Fatal("rehashing a derived broad allow bypassed stock transformation")
	}
}

// Test ownership bypasses cannot turn absent or swapped pins into a default
// daemon policy. A valid profile digest alone lacks its stock derivation.
func TestCandidateCoreProfilePinsRequireBothExactAuthorities(t *testing.T) {
	stockPin, candidatePin := testCandidateCoreProfilePins(t, t.TempDir())
	for _, pins := range []struct {
		stock     Pin
		candidate Pin
	}{
		{stock: Pin{}, candidate: candidatePin},
		{stock: stockPin, candidate: Pin{}},
		{stock: candidatePin, candidate: stockPin},
		{stock: stockPin, candidate: stockPin},
	} {
		if _, err := ValidateCandidateCoreProfiles(pins.stock, pins.candidate, DefaultCandidateCoreProfileSettings(), false); err == nil {
			t.Fatalf("missing or substituted profile authority was accepted: %+v", pins)
		}
	}
}

// Even confined relative symlinks are aliases, not pinned file generations.
func TestCandidateCoreProfilePinsRefuseRelativeLeafAndAncestorSymlinks(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	stockPin, candidatePin := testCandidateCoreProfilePins(t, actual)
	settings := DefaultCandidateCoreProfileSettings()
	if err := os.Symlink("actual", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	ancestorPin := candidatePin
	ancestorPin.Path = filepath.Join(root, "alias", filepath.Base(candidatePin.Path))
	if _, err := ValidateCandidateCoreProfiles(stockPin, ancestorPin, settings, false); err == nil {
		t.Fatal("relative ancestor symlink redirected profile authority")
	}
	leafPin := candidatePin
	leafPin.Path = filepath.Join(actual, "leaf-alias.json")
	if err := os.Symlink(filepath.Base(candidatePin.Path), leafPin.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCandidateCoreProfiles(stockPin, leafPin, settings, false); err == nil {
		t.Fatal("relative leaf symlink redirected profile authority")
	}
}

// Each owner-local bound has an observable refusal, including post-transform
// growth. No nonpositive setting means an unbounded profile parser.
func TestCandidateCoreProfileEnforcesOwnedSettings(t *testing.T) {
	for _, mutate := range []func(*CandidateCoreProfileSettings){
		func(settings *CandidateCoreProfileSettings) { settings.DocumentBytes = 0 },
		func(settings *CandidateCoreProfileSettings) { settings.DocumentBytes = 16 },
		func(settings *CandidateCoreProfileSettings) { settings.DocumentDepth = 1 },
		func(settings *CandidateCoreProfileSettings) { settings.RuleCount = 1 },
		func(settings *CandidateCoreProfileSettings) { settings.SyscallNameCount = 3 },
		func(settings *CandidateCoreProfileSettings) { settings.ArgumentCount = 1 },
		func(settings *CandidateCoreProfileSettings) { settings.ArchitectureCount = 2 },
		func(settings *CandidateCoreProfileSettings) { settings.ConditionCount = 0 },
		func(settings *CandidateCoreProfileSettings) { settings.StringBytes = 8 },
	} {
		settings := DefaultCandidateCoreProfileSettings()
		mutate(&settings)
		if _, err := TransformCandidateCoreProfile(testCandidateCoreStockProfile(), settings); err == nil {
			t.Fatalf("insufficient profile bound was ignored: %+v", settings)
		}
	}
}

// Native Docker receives the actual inline profile, not a path interpretation
// or unconfined mode, and every precommitted direct role uses the same class.
func TestDockerCandidateCoreProfileAcceptsCanonicalInlineAndCoreOne(t *testing.T) {
	fixture := newDockerProxyTest(t)
	calls := dockerPolicyTestDaemon(t, fixture)
	canonical, err := ValidateCandidateCoreProfiles(fixture.plan.StockSeccompProfile, fixture.plan.CandidateCoreProfile, fixture.settings.CoreProfile, false)
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, canonical, "", "  "); err != nil {
		t.Fatal(err)
	}
	response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) {
		host := object["HostConfig"].(map[string]any)
		host["SecurityOpt"] = []string{"seccomp=" + pretty.String(), "no-new-privileges=true"}
	})
	if response.Code != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("canonical pinned inline profile did not reach fake daemon: %d %d", response.Code, calls.Load())
	}
}

// Missing, path-selected, broadened or duplicated profile authority is denied
// before upstream issue. A correct core limit alone is never enough.
func TestDockerCandidateCoreProfileRejectsInlineAuthorityBypasses(t *testing.T) {
	for _, mutate := range []func(map[string]any, []byte){
		func(host map[string]any, profile []byte) { host["SecurityOpt"] = []string{"no-new-privileges:true"} },
		func(host map[string]any, profile []byte) {
			host["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=unconfined"}
		},
		func(host map[string]any, profile []byte) {
			host["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=/etc/synthetic/profile.json"}
		},
		func(host map[string]any, profile []byte) {
			host["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=" + string(testCandidateCoreStockProfile())}
		},
		func(host map[string]any, profile []byte) {
			host["SecurityOpt"] = []string{"no-new-privileges:false", "seccomp=" + string(profile)}
		},
		func(host map[string]any, profile []byte) {
			host["SecurityOpt"] = []string{"no-new-privileges:true", "no-new-privileges:true", "seccomp=" + string(profile)}
		},
		func(host map[string]any, profile []byte) {
			host["SecurityOpt"] = []string{"seccomp=" + string(profile), "seccomp=" + string(profile)}
		},
		func(host map[string]any, profile []byte) {
			host["SecurityOpt"] = []string{"no-new-privileges:true", "label=disable"}
		},
		func(host map[string]any, profile []byte) {
			host["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=" + strings.Replace(string(profile), `"value":4294967295`, `"value":18446744073709551615`, 1)}
		},
	} {
		fixture := newDockerProxyTest(t)
		calls := dockerPolicyTestDaemon(t, fixture)
		canonical, err := ValidateCandidateCoreProfiles(fixture.plan.StockSeccompProfile, fixture.plan.CandidateCoreProfile, fixture.settings.CoreProfile, false)
		if err != nil {
			t.Fatal(err)
		}
		response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) { mutate(object["HostConfig"].(map[string]any), canonical) })
		if response.Code != http.StatusForbidden || calls.Load() != 0 {
			t.Fatalf("invalid inline profile reached daemon: %d %d", response.Code, calls.Load())
		}
	}
}

// Core zero re-enables piped helpers on the reviewed kernel path. Neither a
// zero soft limit, an unlimited hard limit, nor an omitted core limit is safe.
func TestDockerCandidateCoreProfileRejectsEveryNonOneLimitPair(t *testing.T) {
	for _, limits := range []any{
		[]map[string]any{},
		[]map[string]any{{"Name": "core", "Soft": 0, "Hard": 0}},
		[]map[string]any{{"Name": "core", "Soft": 0, "Hard": 1}},
		[]map[string]any{{"Name": "core", "Soft": 1, "Hard": -1}},
		[]map[string]any{{"Name": "core", "Soft": 1, "Hard": 2}},
		[]map[string]any{{"Name": "core", "Soft": 2, "Hard": 2}},
		[]map[string]any{{"Name": "core", "Soft": 1, "Hard": 1}, {"Name": "core", "Soft": 1, "Hard": 1}},
	} {
		fixture := newDockerProxyTest(t)
		calls := dockerPolicyTestDaemon(t, fixture)
		response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) { object["HostConfig"].(map[string]any)["Ulimits"] = limits })
		if response.Code != http.StatusForbidden || calls.Load() != 0 {
			t.Fatal("non-one core limit reached fake daemon")
		}
	}
}

// Each CREATE rereads the two immutable authorities. An earlier constructor
// validation is not permission to use a subsequently changed profile file.
func TestDockerCandidateCoreProfileRechecksPinsAtCreate(t *testing.T) {
	fixture := newDockerProxyTest(t)
	calls := dockerPolicyTestDaemon(t, fixture)
	if err := os.Chmod(fixture.plan.StockSeccompProfile.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.plan.StockSeccompProfile.Path, []byte("{}"), 0400); err != nil {
		t.Fatal(err)
	}
	if response := dockerPolicyTestCreate(t, fixture, nil); response.Code != http.StatusForbidden || calls.Load() != 0 {
		t.Fatal("stale constructor proof authorized changed stock profile")
	}
}
