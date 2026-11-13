package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Virtual clocks exercise the actual bounded state owner; no five-minute sleep.
func TestProviderConnectionsNativeWindowRequiresFiveActualMinutes(t *testing.T) {
	for _, mode := range []string{"complete", "five", "duplicate", "short-span", "gap", "reversal", "scope-change", "failed-middle", "slow-first", "seventh-with-duration", "interval-overbound", "interval-reversal", "overlap", "long-span"} {
		probe := &providerConnectionsProbe{}
		complete := false
		count := 6
		if mode == "seventh-with-duration" || mode == "long-span" {
			count = 7
		}
		for i := 0; i < count; i++ {
			at := providerConnectionsTestNow.Add(time.Duration(i) * time.Minute)
			duration := time.Duration(0)
			scope := "scope-a"
			if mode == "five" && i == 5 {
				break
			}
			if mode == "duplicate" && i == 3 {
				at = at.Add(-time.Minute)
			}
			if mode == "short-span" {
				at = providerConnectionsTestNow.Add(time.Duration(i) * 50 * time.Second)
			}
			if mode == "gap" && i >= 3 {
				at = at.Add(time.Minute)
			}
			if mode == "reversal" && i == 3 {
				at = at.Add(-3 * time.Minute)
			}
			if mode == "scope-change" && i >= 3 {
				scope = "scope-b"
			}
			if mode == "failed-middle" && i == 3 {
				probe.resetNative()
			}
			if mode == "slow-first" && i == 0 {
				duration = 20 * time.Second
			}
			if mode == "seventh-with-duration" {
				duration = 5 * time.Second
			}
			if mode == "interval-overbound" && i == 3 {
				duration = 23 * time.Second
			}
			if mode == "interval-reversal" && i == 3 {
				duration = -time.Second
			}
			if mode == "overlap" {
				duration = 20 * time.Second
				if i == 3 {
					at = at.Add(-50 * time.Second)
				}
			}
			if mode == "long-span" {
				at = providerConnectionsTestNow.Add(time.Duration(i) * 75 * time.Second)
				duration = 22 * time.Second
			}
			best, ok := probe.addNative(scope, at, at.Add(duration), float64(100+i))
			complete = ok
			if ok && best != float64(100+i) {
				t.Fatalf("%s lost an admitted observation", mode)
			}
		}
		want := mode == "complete" || mode == "seventh-with-duration"
		if complete != want {
			t.Fatalf("%s accepted an incomplete or fabricated interval sequence", mode)
		}
		if len(probe.window.points) > 7 {
			t.Fatal("native history exceeded its source bound")
		}
	}
}

func TestProviderConnectionsNativeFrameRejectsPartialAndAmbiguousRows(t *testing.T) {
	now := providerConnectionsTestNow
	base := providerConnectionsTestNative(t, now, providerConnectionsTestHosts[0], providerConnectionsTestProcesses(1))
	for _, mode := range []string{"valid", "missing_rows", "null_rows", "duplicate_start", "future", "old", "wrong_host", "incomplete", "trailing", "duplicate_json"} {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(base), &decoded); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "missing_rows":
			delete(decoded, "rows")
		case "null_rows":
			decoded["rows"] = nil
		case "duplicate_start":
			rows := decoded["rows"].([]any)
			decoded["rows"] = append(rows, rows[0])
		case "future":
			decoded["completed"] = float64(now.Add(time.Hour).Unix())
		case "old":
			decoded["started"] = float64(now.Add(-time.Hour).Unix())
		case "wrong_host":
			decoded["host"] = providerConnectionsPrivate
		case "incomplete":
			decoded["complete"] = false
		}
		encoded, _ := json.Marshal(decoded)
		raw := string(encoded)
		if mode == "trailing" {
			raw += " private invalid tail"
		}
		if mode == "duplicate_json" {
			raw = strings.Replace(raw, `"schema":1`, `"schema":1,"schema":1`, 1)
		}
		_, ok := parseProviderConnectionsNative(raw, "synthetic", providerConnectionsTestHosts[0], now, []string{providerConnectionsTestBlock})
		if ok != (mode == "valid") {
			t.Fatal("native shape/clock/cohort guard disagreed")
		}
	}
}

// Every independently live generation contributes; an equal-start nonce cannot
// replace or double count one native process.
func TestProviderConnectionsNativeJoinRetainsDrainsAndRejectsNonceAmbiguity(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		now := providerConnectionsTestNow
		processes := providerConnectionsTestProcesses(1000)
		old := providerConnectionsTestProcess{host: providerConnectionsTestHosts[0], instance: "drain001", start: providerConnectionsTestStart - 3600, transports: 40000}
		processes = append(processes, old)
		native := append([]providerConnectionsTestProcess(nil), processes...)
		if ambiguous {
			processes = append(processes, providerConnectionsTestProcess{host: providerConnectionsTestHosts[0], instance: "dupenonc", start: providerConnectionsTestStart, transports: 1000})
		}
		source := &syntheticSource{hostFn: func(h HostSettings, command string) (string, error) {
			return providerConnectionsTestNative(t, now, h.Name, native), nil
		}}
		settings := providerConnectionsTestSettings(source, func() time.Time { return now })
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		hosts, blocks, reason := providerConnectionsScope(env)
		if reason != "" {
			t.Fatal(reason)
		}
		frame, reason := parseProviderConnectionsFrame(providerConnectionsTestJson(t, providerConnectionsTestRows(now, processes, 42000, 80000), nil), "synthetic", now, hosts, blocks)
		if reason != "" {
			t.Fatal(reason)
		}
		observation := (&providerConnectionsProbe{}).nativeSample(context.Background(), env, now, hosts, blocks, frame)
		if ambiguous {
			if observation.reason != "native_process_generation_unmatched" {
				t.Fatal("equal-start nonces were silently picked")
			}
		} else if observation.reason != "" || observation.current != 42000 {
			t.Fatal("live draining transport supply was dropped")
		}
	}
}

func TestProviderConnectionsScopeNeverContactsExcludedOrOverboundHosts(t *testing.T) {
	for _, mode := range []string{"excluded", "missing_inventory", "overbound"} {
		var calls atomic.Int32
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { calls.Add(1); return "", nil }}
		settings := providerConnectionsTestSettings(source, func() time.Time { return providerConnectionsTestNow })
		switch mode {
		case "excluded":
			settings.ExcludedHosts = []string{providerConnectionsTestHosts[1]}
		case "missing_inventory":
			settings.LogServiceHosts = nil
		case "overbound":
			for i := 0; i < 3; i++ {
				name := fmt.Sprintf("extra-%d", i)
				settings.Hosts = append(settings.Hosts, HostSettings{Name: name, Roles: []string{"services"}})
				settings.LogServiceHosts["connect"] = append(settings.LogServiceHosts["connect"], name)
			}
		}
		alerts, err := NewProviderConnectionsSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "provider-connections-unobservable")
		if calls.Load() != 0 {
			t.Fatal("unknown/excluded desired scope contacted a host")
		}
	}
}

// Executes the exact embedded collector with synthetic Docker/proc readers.
// Native Docker, /proc, service endpoints and production are never contacted.
// The actual command reader also consumes bounded synthetic pipe/process seams.
func TestProviderConnectionsNativeCollectorProjection(t *testing.T) {
	wrapper := `import contextlib,io,json,sys,types
script=sys.stdin.read()
head,tail=script.split("result = {'schema':1",1)
sys.argv=['fixture','synthetic','private-provider-fixture-a','["private-provider-fixture-blue"]']
out=[]
for mode in ['empty','live','two-live','changed-set','changed-pid','unknown-block','wrong-name','time-namespace','wrong-executable','malformed-list']:
    ns={}
    exec(head,ns)
    calls={'ps':0,'inspect':0}
    cid='a'*64
    def listed(second=False):
        rows=[] if mode=='empty' or (mode=='changed-set' and second) else [{'id':cid,'name':'synthetic-connect-private-provider-fixture-blue-one','env':'synthetic','job':'connect','block':'private-provider-fixture-blue'}]
        if mode=='two-live':rows.append(dict(rows[0],id='b'*64,name='synthetic-connect-private-provider-fixture-blue-two'))
        if rows and mode=='unknown-block':rows[0]['block']='unexpected'
        if rows and mode=='wrong-name':rows[0]['name']='synthetic-connect-wrong-one'
        return rows
    def command(args):
        if args[0]=='ps':
            calls['ps']+=1
            if mode=='malformed-list':return b'{"id":'
            return b'\n'.join(json.dumps(r).encode() for r in listed(calls['ps']>1))
        calls['inspect']+=1
        rows=[]
        for i,row in enumerate(listed(False)):
            rows.append(dict(row,name='/'+row['name'],pid=123+i+(10 if mode=='changed-pid' and calls['inspect']>1 else 0),running=True,started='2026-10-19T10:00:00Z',path='/usr/local/sbin/bringyour-connect'))
        return b'\n'.join(json.dumps(r).encode() for r in rows)
    def read(path,cap):
        if path=='/proc/sys/kernel/random/boot_id':return 'a'*8+'-'+'a'*4+'-'+'a'*4+'-'+'a'*4+'-'+'a'*12
        if path=='/proc/stat':return 'btime 1700000000\n'
        if path.endswith('/timens_offsets'):return 'monotonic 0 0\nboottime '+('1' if mode=='time-namespace' else '0')+' 0\n'
        if path.endswith('/stat'):
            pid=int(path.split('/')[2]);fields=['S']+['0']*21;fields[19]=str(10000+pid)
            return str(pid)+' (bringyour-connect) '+' '.join(fields)
        raise ValueError('unexpected fixture read')
    def link(path):return 'time:[2]' if mode=='time-namespace' and not path.startswith('/proc/self/') else 'time:[1]'
    def stat(path):return types.SimpleNamespace(st_dev=1,st_ino=2 if mode=='wrong-executable' and path.endswith('/exe') else 1)
    ns['command']=command;ns['read']=read
    ns['os']=types.SimpleNamespace(uname=lambda:types.SimpleNamespace(nodename='private-provider-fixture-a'),readlink=link,stat=stat)
    capture=io.StringIO()
    with contextlib.redirect_stdout(capture):exec("result = {'schema':1"+tail,ns)
    frame=json.loads(capture.getvalue())
    expected=mode in ('empty','live','two-live')
    assert frame['complete']==expected,mode
    if expected:assert len(frame['rows'])=={'empty':0,'live':1,'two-live':2}[mode],mode
    if mode=='live':assert frame['rows'][0]['start']==1700000000+(10000+123)/100,mode
    out.append(mode)
for mode in ['command-complete','command-overflow','command-timeout','command-exit']:
    ns={}
    exec(head,ns)
    source_command=ns['command']
    state={'left':131073 if mode=='command-overflow' else 100,'killed':0,'closed':0}
    class Pipe:
        def fileno(self):return 42
        def close(self):state['closed']+=1
    class Process:
        pid=123
        stdout=Pipe()
        returncode=None
        def poll(self):return self.returncode
        def wait(self,timeout):
            self.returncode=5 if mode=='command-exit' else 0
            return self.returncode
    process=Process()
    def popen(argv,**kwargs):
        assert argv[:3]==['/usr/bin/docker','-H','unix:///var/run/docker.sock']
        assert kwargs['env']=={'PATH':'/usr/bin:/bin','LANG':'C'}
        return process
    class Selector:
        def __enter__(self):return self
        def __exit__(self,*args):pass
        def register(self,*args):pass
        def select(self,left):return [] if mode=='command-timeout' else [True]
    def receive(fd,size):
        take=min(size,state['left']);state['left']-=take
        return b'x'*take
    def kill(pid,sig):state['killed']+=1
    ns['os']=types.SimpleNamespace(read=receive,killpg=kill)
    ns['subprocess']=types.SimpleNamespace(Popen=popen,DEVNULL=-3,PIPE=-1)
    ns['selectors']=types.SimpleNamespace(DefaultSelector=Selector,EVENT_READ=1)
    ns['time']=types.SimpleNamespace(monotonic=lambda:ns['BEGIN'])
    success=False
    try:
        result=source_command(['ps'])
        success=True
        assert len(result)==100
    except ValueError:pass
    assert success==(mode=='command-complete'),mode
    assert state['closed']==1,mode
    assert state['killed']==(1 if mode in ('command-overflow','command-timeout') else 0),mode
    out.append(mode)
print(json.dumps(out))
`
	command := exec.Command("python3", "-c", wrapper)
	command.Stdin = strings.NewReader(providerConnectionsNativeScript)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic native collector projection failed: %v\n%s", err, output)
	}
	var names []string
	if json.Unmarshal(output, &names) != nil || len(names) != 14 {
		t.Fatal("collector fixture did not execute every mechanism case")
	}
}

// Cancellation never turns into a healthy observation or consumes a minute.
func TestProviderConnectionsCancelledOwnerDoesNotObserve(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { calls.Add(1); return "", nil }}
	alerts, err := NewProviderConnectionsSignal().Run(ctx, providerConnectionsTestSettings(source, func() time.Time { return providerConnectionsTestNow }))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls.Load() != 0 {
		t.Fatal("cancelled owner lost its cancellation cause, reached a source or returned findings")
	}
}

// The source keeps context ownership visible at the same public adapter seam.
type providerConnectionsContextSource struct {
	*syntheticSource
	read func(context.Context, HostSettings, string) (string, error)
}

func (self *providerConnectionsContextSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.read(ctx, host, command)
}

// Every launched native child joins cancellation before the signal returns;
// an independent signal owner can still measure a healthy cohort in parallel.
func TestProviderConnectionsNativeCancellationJoinsChildrenAndKeepsSiblingLive(t *testing.T) {
	now := providerConnectionsTestNow
	entered := make(chan struct{}, 2)
	var joined atomic.Int32
	source := &providerConnectionsContextSource{syntheticSource: &syntheticSource{}, read: func(ctx context.Context, _ HostSettings, command string) (string, error) {
		if strings.Contains(command, "monitor-provider-connections-native") {
			entered <- struct{}{}
			<-ctx.Done()
			joined.Add(1)
			return "", ctx.Err()
		}
		return providerConnectionsTestJson(t, providerConnectionsTestRows(now, providerConnectionsTestProcesses(1000), 2000, 76000), nil), nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := NewProviderConnectionsSignal().Run(ctx, providerConnectionsTestSettings(source, func() time.Time { return now }))
		finished <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-finished:
			t.Fatal("signal returned before both native children reached the explicit barrier")
		case <-time.After(30 * time.Second):
			t.Fatal("native child did not reach the explicit barrier")
		}
	}
	healthy := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return providerConnectionsTestJson(t, providerConnectionsTestRows(now, providerConnectionsTestProcesses(40000), 80000, 80000), nil), nil
	}}
	alerts, err := NewProviderConnectionsSignal().Run(context.Background(), providerConnectionsTestSettings(healthy, func() time.Time { return now }))
	if err != nil || len(alerts) != 0 {
		t.Fatal("blocked native owner prevented independent healthy measurement")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || joined.Load() != 2 {
			t.Fatal("cancelled signal returned before its native children joined")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled signal did not finish")
	}
}

// The real public owner uses reader intervals rather than invocation clocks.
// The first census takes 20 virtual seconds; the sixth cannot claim five full
// minutes. A seventh admitted point proves the conservative span.
func TestProviderConnectionsSlowFirstCensusNeedsSeventhObservation(t *testing.T) {
	var minute atomic.Int32
	var elapsed atomic.Int64
	now := func() time.Time {
		return providerConnectionsTestNow.Add(time.Duration(minute.Load())*time.Minute + time.Duration(elapsed.Load())*time.Second)
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		at := providerConnectionsTestNow.Add(time.Duration(minute.Load()) * time.Minute)
		processes := providerConnectionsTestProcesses(1000)
		if strings.Contains(command, "monitor-provider-connections-native") {
			if minute.Load() == 0 {
				elapsed.Store(20)
			}
			return providerConnectionsTestNative(t, at, host.Name, processes), nil
		}
		return providerConnectionsTestJson(t, providerConnectionsTestRows(at, processes, 2000, 76000), nil), nil
	}}
	signal := NewProviderConnectionsSignal()
	settings := providerConnectionsTestSettings(source, now)
	for i := 0; i < 7; i++ {
		minute.Store(int32(i))
		elapsed.Store(0)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if (requireAlertClassCount(alerts, "provider-connections-collapse") == 1) != (i == 6) {
			t.Fatal("invocation timestamps fabricated native five-minute coverage")
		}
	}
}

// Parent termination retains every observed original source cause while public
// error text remains fixed. No later source begins after a terminated Mimir read.
func TestProviderConnectionsEndedParentRetainsSourceCauses(t *testing.T) {
	for _, native := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		causeA, causeB := errors.New(providerConnectionsPrivate+"-cause-a"), errors.New(providerConnectionsPrivate+"-cause-b")
		entered := make(chan struct{}, 2)
		var calls atomic.Int32
		source := &providerConnectionsContextSource{syntheticSource: &syntheticSource{}, read: func(readCtx context.Context, host HostSettings, command string) (string, error) {
			calls.Add(1)
			if strings.Contains(command, "monitor-provider-connections-native") {
				entered <- struct{}{}
				<-readCtx.Done()
				if host.Name == providerConnectionsTestHosts[0] {
					return "", causeA
				}
				return "", causeB
			}
			if !native {
				cancel()
				return "", causeA
			}
			return providerConnectionsTestJson(t, providerConnectionsTestRows(providerConnectionsTestNow, providerConnectionsTestProcesses(1000), 2000, 76000), nil), nil
		}}
		finished := make(chan error, 1)
		go func() {
			_, err := NewProviderConnectionsSignal().Run(ctx, providerConnectionsTestSettings(source, func() time.Time { return providerConnectionsTestNow }))
			finished <- err
		}()
		if native {
			for i := 0; i < 2; i++ {
				select {
				case <-entered:
				case <-finished:
					cancel()
					t.Fatal("signal returned before both source causes reached the cancellation barrier")
				case <-time.After(30 * time.Second):
					cancel()
					t.Fatal("native source did not reach cancellation barrier")
				}
			}
			cancel()
		}
		select {
		case err := <-finished:
			cancel()
			if !errors.Is(err, context.Canceled) || !errors.Is(err, causeA) || (native && !errors.Is(err, causeB)) {
				t.Fatal("ended parent erased an observed source cause")
			}
			if strings.Contains(err.Error(), providerConnectionsPrivate) {
				t.Fatal("private source cause escaped rendered error")
			}
			if (!native && calls.Load() != 1) || (native && calls.Load() != 3) {
				t.Fatal("ended source started an unexpected followup read")
			}
		case <-time.After(30 * time.Second):
			cancel()
			t.Fatal("source children failed to join")
		}
	}
}

// This deterministic context ends at the reducer's final owner boundary after
// the source returned successfully; no scheduler timing or real sleep is proof.
type providerConnectionsFinalBoundaryContext struct {
	context.Context
	armed  atomic.Bool
	checks atomic.Int32
}

func (self *providerConnectionsFinalBoundaryContext) Err() error {
	if self.armed.Load() && self.checks.Add(1) >= 2 {
		return context.DeadlineExceeded
	}
	return nil
}

func TestProviderConnectionsFinalOwnerBoundaryCannotReturnHealthy(t *testing.T) {
	ctx := &providerConnectionsFinalBoundaryContext{Context: context.Background()}
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		payload := providerConnectionsTestJson(t, providerConnectionsTestRows(providerConnectionsTestNow, providerConnectionsTestProcesses(40000), 80000, 80000), nil)
		ctx.armed.Store(true)
		return payload, nil
	}}
	env, err := newProbeEnv(providerConnectionsTestSettings(source, func() time.Time { return providerConnectionsTestNow }).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	probe := &providerConnectionsProbe{}
	hosts, blocks, reason := providerConnectionsScope(env)
	if reason != "" {
		t.Fatal(reason)
	}
	probe.bindNativeScope(env.cfg.env, hosts, blocks)
	prior := finding{probeId: probe.id(), tier: tierPage, class: "provider-connections-collapse", target: "synthetic/connect-fleet"}
	probe.lastFailure = &prior
	probe.lastFailureAt = providerConnectionsTestNow.Add(-time.Minute)
	findings, err := probe.check(ctx, env)
	if !errors.Is(err, context.DeadlineExceeded) || len(findings) != 0 {
		t.Fatal("ended owner emitted healthy or product findings")
	}
	if probe.lastFailure != &prior {
		t.Fatal("ended healthy reduction erased the independently proved prior witness")
	}
}

// Both old and new scopes flatten to env,a,b,c without count/length framing.
// The exact public source topology change must invalidate points and witnesses.
func TestProviderConnectionsInventoryPartitionChangeRestartsWindow(t *testing.T) {
	for _, sourceGap := range []bool{false, true} {
		now := providerConnectionsTestNow
		changed := false
		gap := false
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			if gap {
				return "", errors.New(providerConnectionsPrivate)
			}
			processes := providerConnectionsTestProcesses(1000)
			if strings.Contains(command, "monitor-provider-connections-native") {
				if !changed {
					return providerConnectionsTestNative(t, now, host.Name, processes), nil
				}
				rows := []map[string]any{{"block": providerConnectionsTestBlock, "start": providerConnectionsTestStart}, {"block": providerConnectionsTestHosts[1], "start": providerConnectionsTestStart}}
				encoded, _ := json.Marshal(map[string]any{"schema": 1, "host": host.Name, "environment": "synthetic", "started": now.Unix(), "completed": now.Unix(), "complete": true, "rows": rows})
				return string(encoded), nil
			}
			rows := providerConnectionsTestRows(now, processes, 2000, 76000)
			if changed {
				for _, row := range rows {
					labels := row["metric"].(map[string]string)
					if labels["host"] == providerConnectionsTestHosts[1] {
						labels["host"] = providerConnectionsTestHosts[0]
						labels["block"] = providerConnectionsTestHosts[1]
					}
				}
			}
			return providerConnectionsTestJson(t, rows, nil), nil
		}}
		signal := NewProviderConnectionsSignal()
		settings := providerConnectionsTestSettings(source, func() time.Time { return now })
		last := 11
		if sourceGap {
			last = 12
		}
		for i := 0; i <= last; i++ {
			now = providerConnectionsTestNow.Add(time.Duration(i) * time.Minute)
			if i == 6 {
				changed = true
				settings.LogServiceHosts["connect"] = []string{providerConnectionsTestHosts[0]}
				settings.LogServiceBlocks["connect"] = []string{providerConnectionsTestHosts[1], providerConnectionsTestBlock}
			}
			gap = sourceGap && i == 6
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			wantPage := i == 5 || i == last
			if (requireAlertClassCount(alerts, "provider-connections-collapse") == 1) != wantPage {
				t.Fatal("different host/block partitions inherited a prior native window or failure")
			}
		}
	}
}
