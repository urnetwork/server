package sidecart3

// D-TEST-2 on T3: every store suite sidecartest ships runs on server's real store, and §22.4's runtime
// CROSSSLOT lint runs every script and multi-key call an engine made against the 3-master cluster.
//
// RunSuites is the check that nothing falls between: each of sidecartest.StoreSuites has one T3 entry
// here, which runs it on the real store itself, names the test of server's that does (sidecar-t3.sh's
// suites cadence runs each), or names the task whose real store has not landed, which a gate reports
// missing and never passed. A store fake with no suite fails it, and so does an entry naming a test that
// does not exist or does not call its suite.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// One store suite on T3.
type Suite struct {
	// sidecartest.StoreSuite's Name: "redissuite.Run".
	Name string
	// Runs it on the real store here. nil when Test or WaitsOn says where it runs.
	Run func(t *testing.T)
	// The directory of server, from its root, holding the test that runs it on the real store: "model",
	// or "." for package server.
	Package string
	// That test's name.
	Test string
	// The task whose real store the suite waits on: reported missing, which is never a pass.
	WaitsOn string
}

func (self *Suite) String() string {
	switch {
	case self.Run != nil:
		return self.Name + " here"
	case self.Test != "":
		return fmt.Sprintf("%s by %s's %s", self.Name, self.Package, self.Test)
	}
	return fmt.Sprintf("%s, missing until %s lands its store", self.Name, self.WaitsOn)
}

// Where each of sidecartest.StoreSuites runs on server's real store today. A suite whose store lands
// moves from WaitsOn to the test that runs it, and sidecar-t3.sh's suites cadence names that test too.
func Suites() []*Suite {
	return []*Suite{
		{Name: "blobsuite.Run", Package: ".", Test: "TestFileClustersConformOnTheLocalStack"},
		{Name: "redissuite.Run", Package: "sidecarhost", Test: "TestRedisConformsOnTheTestStore"},
		{Name: "quotasuite.Run", WaitsOn: "QUOTA-M2-11"},
		{Name: "RunPushStoreSuite", Package: "model", Test: "TestSidecarPushStorePassesItsSuite"},
		{Name: "RunSocketStoreSuite", Package: "model", Test: "TestSocketStorePassesItsSuite"},
		{Name: "RunDnsStoreSuite", Package: "dnshost", Test: "TestStoreRunsTheDnsStoreSuite"},
		{Name: "RunDnsRestoreStoreSuite", Package: "dnshost", Test: "TestRestoreStoreRunsItsSuite"},
		{Name: "RunCertsStoreSuite", WaitsOn: "TLS-M5-16"},
		{Name: "RunMxEdgeStoreSuite", Package: "model", Test: "TestMxEdgeStorePassesTheSuite"},
		{Name: "RunSmtpEdgeStoreSuite", WaitsOn: "SMTP-M9-18"},
		{Name: "RunConfigLogStoreSuite", Package: "model", Test: "TestConfigLogSequencerPassesTheStoreSuite"},
	}
}

// What leaves a store fake unheld on T3: every problem sidecartest.StoreSuiteProblems finds, a store
// suite with no T3 entry or with two, an entry for a suite sidecartest does not list, an entry that says
// none or more than one of where it runs, and a named test that serverRoot's source does not hold or
// whose body does not call the suite. Empty when every store fake is held to its store.
func SuiteProblems(storeSuites []*sidecartest.StoreSuite, storeFakes []string, suites []*Suite, serverRoot string) []string {
	problems := sidecartest.StoreSuiteProblems(storeSuites, storeFakes)
	for _, storeSuite := range storeSuites {
		count := 0
		for _, suite := range suites {
			if suite.Name == storeSuite.Name {
				count += 1
			}
		}
		switch count {
		case 0:
			problems = append(problems, storeSuite.Name+" runs on no real store at T3 and waits on no task (D-TEST-2)")
		case 1:
		default:
			problems = append(problems, fmt.Sprintf("%s has %d T3 entries", storeSuite.Name, count))
		}
	}
	for _, suite := range suites {
		if !slices.ContainsFunc(storeSuites, func(storeSuite *sidecartest.StoreSuite) bool { return storeSuite.Name == suite.Name }) {
			problems = append(problems, suite.Name+" is no suite sidecartest lists")
		}
		ways := 0
		if suite.Run != nil {
			ways += 1
		}
		if suite.Test != "" {
			ways += 1
		}
		if suite.WaitsOn != "" {
			ways += 1
		}
		if ways != 1 {
			problems = append(problems, fmt.Sprintf("%s says %d ways it runs, not one", suite.Name, ways))
			continue
		}
		if suite.Test != "" {
			if err := testCallsSuite(filepath.Join(serverRoot, suite.Package), suite.Test, suite.Name); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", suite, err))
			}
		}
	}
	return problems
}

// Whether directory's Go files declare test and its body calls suite: "blobsuite.Run" as that selector,
// "RunSocketStoreSuite" as sidecartest's.
func testCallsSuite(directory string, test string, suite string) error {
	qualifier, name := "sidecartest", suite
	if before, after, found := strings.Cut(suite, "."); found {
		qualifier, name = before, after
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Name.Name != test || function.Body == nil {
				continue
			}
			calls := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == name {
					if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == qualifier {
						calls = true
					}
				}
				return !calls
			})
			if !calls {
				return fmt.Errorf("%s does not call %s.%s", test, qualifier, name)
			}
			return nil
		}
	}
	return fmt.Errorf("no test %s in %s", test, directory)
}

// Checks suites against sidecartest's store suites and fakes, failing t for each problem, runs each
// suite whose Run is here, and logs each that waits, as missing. serverRoot is server's root directory,
// where each named test is looked for: ".." from this package's own tests.
func RunSuites(t *testing.T, suites []*Suite, serverRoot string) {
	t.Helper()
	for _, problem := range SuiteProblems(sidecartest.StoreSuites(), sidecartest.StoreFakes(), suites, serverRoot) {
		t.Error(problem)
	}
	for _, suite := range suites {
		switch {
		case suite.Run != nil:
			t.Logf("T3 runs %s", suite)
			suite.Run(t)
		case suite.Test != "":
			t.Logf("T3 runs %s, in sidecar-t3.sh's suites cadence", suite)
		default:
			t.Logf("missing, which is never a pass: %s", suite)
		}
	}
}

// One Redis call shape an engine made on T3: what RunCrossslot sends again to the 3-master cluster.
type CrossslotCall struct {
	// "mget", "del" or "script:<name>", as CountingRedis names them.
	Op string
	// The script, for a script.
	Script *sidecar.RedisScript
	// As the engine named them.
	Keys []string
	Args []string
}

// Every call shape T3's engines made through a Net's Redis: each script by name, and each mget and del
// of two or more keys by its key count, the first call of each kept. Safe for concurrent use.
type CrossslotCalls struct {
	stateLock sync.Mutex
	calls     []*CrossslotCall
	shapes    map[string]bool
}

func NewCrossslotCalls() *CrossslotCalls {
	return &CrossslotCalls{shapes: map[string]bool{}}
}

func (self *CrossslotCalls) add(shape string, call *CrossslotCall) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.shapes[shape] {
		return
	}
	self.shapes[shape] = true
	self.calls = append(self.calls, call)
}

func (self *CrossslotCalls) addKeys(op string, keys []string) {
	if len(keys) < 2 {
		return
	}
	self.add(fmt.Sprintf("%s/%d", op, len(keys)), &CrossslotCall{Op: op, Keys: slices.Clone(keys)})
}

func (self *CrossslotCalls) addScript(script *sidecar.RedisScript, keys []string, args []string) {
	if script == nil {
		return
	}
	self.add("script:"+script.Name, &CrossslotCall{Op: "script:" + script.Name, Script: script, Keys: slices.Clone(keys), Args: slices.Clone(args)})
}

// The shapes so far, in the order first made.
func (self *CrossslotCalls) Calls() []*CrossslotCall {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

// The lint's key spaces: the master each slot range belongs to.
type CrossslotMaster struct {
	// Inclusive slot ranges, as CLUSTER SHARDS gives them.
	SlotRanges [][2]int
}

func (self *CrossslotMaster) holds(slot int) bool {
	for _, slotRange := range self.SlotRanges {
		if slotRange[0] <= slot && slot <= slotRange[1] {
			return true
		}
	}
	return false
}

// The masters of the Redis server's pools reach, or why it is no cluster of 3 masters: what the runtime
// lint needs and test-env.sh attests only once QD-028's change lands.
func readCrossslotMasters(ctx context.Context) (masters []*CrossslotMaster, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("%v", recovered)
		}
	}()
	server.Redis(ctx, func(client server.RedisClient) {
		var shards []redis.ClusterShard
		shards, returnErr = client.ClusterShards(ctx).Result()
		if returnErr != nil {
			return
		}
		for _, shard := range shards {
			if !slices.ContainsFunc(shard.Nodes, func(node redis.Node) bool { return node.Role == "master" }) {
				continue
			}
			master := &CrossslotMaster{}
			for _, slotRange := range shard.Slots {
				master.SlotRanges = append(master.SlotRanges, [2]int{int(slotRange.Start), int(slotRange.End)})
			}
			masters = append(masters, master)
		}
	})
	return masters, returnErr
}

// The run's keys for one call on one master: each tag the call names becomes a tag of the run's own on
// master, keys sharing a tag still share one, and a key with no tag gets a prefix and stays untagged, so
// the cluster refuses whatever the engine's keys would make it refuse. run keeps the run's keys apart
// from any other's.
func CrossslotKeys(keys []string, master *CrossslotMaster, run string) []string {
	tagTags := map[string]string{}
	spread := make([]string, 0, len(keys))
	for _, key := range keys {
		start := strings.IndexByte(key, '{')
		length := -1
		if 0 <= start {
			length = strings.IndexByte(key[start+1:], '}')
		}
		if start < 0 || length <= 0 {
			spread = append(spread, run+":"+key)
			continue
		}
		tag := key[start+1 : start+1+length]
		newTag, ok := tagTags[tag]
		if !ok {
			for index := 0; ; index += 1 {
				newTag = fmt.Sprintf("%s-%d-%s", run, index, tag)
				if master.holds(sidecartest.RedisClusterSlot("{" + newTag + "}")) {
					break
				}
			}
			tagTags[tag] = newTag
		}
		spread = append(spread, key[:start+1]+newTag+key[start+1+length:])
	}
	return spread
}

// What a cluster answers a call whose keys are not all on the node it reached.
func crossslotRefused(err error) bool {
	text := err.Error()
	return strings.Contains(text, "CROSSSLOT") ||
		strings.Contains(text, "non local key") ||
		strings.Contains(text, "Script attempted to access")
}

// §22.4's runtime lint: each call in calls runs once on each master of the Redis server's pools reach,
// its keys moved onto that master by CrossslotKeys, and a call the cluster refuses as CROSSSLOT, or that
// cannot run at all, fails t. The lint needs T3's 3-master Redis Cluster; test-env.sh attests one Redis
// node until QD-028's change lands, so on that node the lint fails, which is never a pass.
func RunCrossslot(t testing.TB, calls *CrossslotCalls, run string) {
	t.Helper()
	ctx := t.Context()
	masters, err := readCrossslotMasters(ctx)
	if err != nil || len(masters) < 3 {
		t.Fatalf("§22.4's runtime CROSSSLOT lint needs T3's 3-master Redis Cluster, and the attested Redis has %d masters (%v): test-env.sh attests the cluster only once QD-028's change lands, so the lint did not run, which is no pass", len(masters), err)
	}
	store := sidecarhost.NewRedis(nil)
	for _, call := range calls.Calls() {
		for index, master := range masters {
			keys := CrossslotKeys(call.Keys, master, fmt.Sprintf("%s-%d", run, index))
			var err error
			switch {
			case call.Script != nil:
				_, err = store.RunScript(ctx, call.Script, keys, call.Args)
			case call.Op == "mget":
				_, err = store.GetMany(ctx, keys)
			case call.Op == "del":
				err = store.Delete(ctx, keys)
			default:
				err = errors.New("no way to send it")
			}
			if err != nil && crossslotRefused(err) {
				t.Errorf("%s on master %d: CROSSSLOT: %v", call.Op, index, err)
			} else if err != nil {
				t.Errorf("%s on master %d could not run, so it is not linted: %v", call.Op, index, err)
			}
			for _, key := range keys {
				store.Delete(ctx, []string{key})
			}
		}
	}
}
