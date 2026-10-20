// The native process census controls run only an in-memory Python harness.
// Docker, /proc, executable identity and time are replaced before reader code.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A stable independently observed running process joins the metric generation;
// wrong host, missing metadata and changing process sets stay unknown.
func TestJwtRejectionsCensusParser(t *testing.T) {
	now := syntheticSettings(nil).Now()
	scope := jwtRejectionsScope{slots: map[jwtRejectionsSlot]bool{{service: "connect", host: "jwt-source", block: "blue"}: true}}
	for _, mode := range []string{"healthy", "wrong host", "future", "empty", "duplicate", "unexpected slot", "extra field"} {
		row := map[string]any{"service": "connect", "block": "blue", "start": float64(now.Add(-time.Hour).Unix())}
		source := map[string]any{"schema": 1, "host": "jwt-source", "at": now.Unix(), "processes": []any{row}}
		switch mode {
		case "wrong host":
			source["host"] = "private-other-host"
		case "future":
			source["at"] = now.Add(time.Minute).Unix()
		case "empty":
			source["processes"] = []any{}
		case "duplicate":
			source["processes"] = []any{row, row}
		case "unexpected slot":
			row["block"] = "private-other-block"
		case "extra field":
			source["private-token"] = "private-canary"
		}
		raw, _ := json.Marshal(source)
		starts, valid := parseJwtRejectionsCensus(string(raw), "jwt-source", now, scope)
		if mode == "healthy" {
			if !valid || len(starts) != 1 {
				t.Fatal("healthy census failed its exact join")
			}
		} else if mode == "empty" {
			if !valid || len(starts) != 0 {
				t.Fatal("empty actual census fabricated a process")
			}
		} else if valid {
			t.Fatal("invalid native census was accepted")
		}
	}
}

// Test doubles refuse every unlisted command and file. The same program
// shipped to SSH is exercised without reading local /proc or running Docker.
// Target metadata controls add a second current container beside the complete
// old process. Ignoring that new container would create the HJ002 false clear.
func jwtRejectionsNativeFixture(t *testing.T, mode string) (string, error) {
	t.Helper()
	now := syntheticSettings(nil).Now()
	modeJson, _ := json.Marshal(mode)
	prelude := fmt.Sprintf(`import builtins,io,json,os,socket,subprocess,time
mode=%s
now=%d
calls={'ps':0,'stat':0}
identity='a'*64
second='c'*64
metadata_modes={'absent service','null service','empty service','wrong service','wrong target service','missing block','wrong block','wrong name','unrelated container'}
containers=[identity,second] if mode in metadata_modes else [identity]
def fake_open(path,mode_arg):
 if mode_arg!='rb': raise AssertionError('unowned mode')
 if path=='/proc/sys/kernel/random/boot_id': text='00000000-0000-0000-0000-000000000001\n'
 elif path=='/proc/stat': text='btime '+str(now-3610)+'\n'
 elif path=='/proc/123/stat':
  calls['stat']+=1
  ticks=1001 if mode=='pid changed' and calls['stat']>1 else 1000
  text='123 (bringyour-connect) '+' '.join(['S']+['0']*18+[str(ticks)]+['0']*10)+'\n'
 else: raise AssertionError('unowned path')
 return io.BytesIO(text.encode())
def fake_readlink(path):
 if path!='/proc/123/exe': raise AssertionError('unowned executable path')
 return '/private/unowned' if mode=='wrong executable' else '/usr/local/sbin/bringyour-connect'
class FakeChild:
 def __init__(self,argv,**kwargs):
  if argv==['docker','ps','--no-trunc','--format','{{.ID}}']:
   calls['ps']+=1
   values=['b'*64] if mode=='turnover' and calls['ps']>1 else containers
   text='\n'.join(values)+'\n'
   if mode=='duplicate ids': text+=text
   if mode=='command oversize': text='a'*32770
  elif len(argv)==4+len(containers) and argv[:3]==['docker','inspect','--format'] and argv[4:]==containers:
   if '.Config.Env' in argv[3] or '.Args' in argv[3] or '{{json .}}' in argv[3]: raise AssertionError('private fields')
   rows=[{'id':identity,'name':'/synthetic-connect-blue-v1-1','pid':123,'running':True,'env':None if mode=='missing labels' else 'synthetic','service':'connect','block':'blue'}]
   if second in containers:
    row={'id':second,'name':'/synthetic-connect-blue-v2-2','pid':124,'running':True,'env':'synthetic','service':'connect','block':'blue'}
    if mode=='absent service': del row['service']
    elif mode=='null service': row['service']=None
    elif mode=='empty service': row['service']=''
    elif mode=='wrong service': row['service']='redis'
    elif mode=='wrong target service': row['service']='proxy'
    elif mode=='missing block': del row['block']
    elif mode=='wrong block': row['block']='private-block-canary'
    elif mode=='wrong name': row['name']='/synthetic-proxy-blue-v2-2'
    elif mode=='unrelated container': row.update({'name':'/synthetic-redis-cluster-v2-2','service':'redis','block':'cluster'})
    rows.append(row)
   text='\n'.join(json.dumps(row) for row in rows)+'\n'
  else: raise AssertionError('unowned command')
  if mode=='private failure': raise ValueError('private-token-key-canary')
  self.stdout=io.BytesIO(text.encode())
 def wait(self,timeout=None): return 0
 def poll(self): return 0
 def kill(self): raise AssertionError('fake child already complete')
builtins.open=fake_open
subprocess.Popen=FakeChild
os.readlink=fake_readlink
os.sysconf=lambda name: 100
socket.gethostname=lambda: 'jwt-source'
time.time=lambda: float(now)
`, modeJson, now.Unix())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-c", prelude+"\n"+jwtRejectionsCensusProgram("synthetic", []string{"connect"}))
	out, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("mocked native census exceeded its local harness deadline")
	}
	if strings.Contains(string(out), "private-token-key-canary") || strings.Contains(string(out), "private-block-canary") {
		t.Fatal("native census exposed private source fields")
	}
	return string(out), err
}

// Valid unrelated identity remains excluded; missing or contradictory target
// metadata cannot turn an unknown current generation into an empty census.
func TestJwtRejectionsCensusNativeProgramControls(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, mode := range []string{"healthy", "unrelated container", "missing labels", "absent service", "null service", "empty service", "wrong service", "wrong target service", "missing block", "wrong block", "wrong name", "wrong executable", "turnover", "pid changed", "duplicate ids", "command oversize", "private failure"} {
		t.Logf("case: %s", mode)
		out, err := jwtRejectionsNativeFixture(t, mode)
		if mode == "healthy" || mode == "unrelated container" {
			if err != nil {
				t.Fatalf("%s: complete mocked native census failed", mode)
			}
			slot := jwtRejectionsSlot{service: "connect", host: "jwt-source", block: "blue"}
			scope := jwtRejectionsScope{slots: map[jwtRejectionsSlot]bool{slot: true}}
			starts, valid := parseJwtRejectionsCensus(out, "jwt-source", now, scope)
			if !valid || len(starts) != 1 || len(starts[slot]) != 1 || starts[slot][0] != float64(now.Add(-time.Hour).Unix()) {
				t.Fatalf("%s: complete native receipt did not preserve the exact independent process start", mode)
			}
		} else if err == nil || len(out) != 0 {
			t.Fatalf("%s: incomplete or contradictory target metadata fabricated census completeness", mode)
		}
	}
}

// Feed the generated program's real output/exit to the public source seam.
// The metric witness stays independently qualified even when census fails.
func jwtRejectionsNativeSettings(t *testing.T, rate float64, mode string) SignalSettings {
	t.Helper()
	out, sourceErr := jwtRejectionsNativeFixture(t, mode)
	f := newJwtRejectionsTestFixture(jwtRejectionsTestProcess{service: "connect", rate: rate})
	settings := f.settings(t)
	source := settings.Source.(*providerHttpsTestSource)
	source.syntheticSource.hostTimeoutFn = func(host HostSettings, command string, timeout time.Duration) (string, error) {
		want := "timeout 14s python3 -c " + shellSingleQuote(jwtRejectionsCensusProgram("synthetic", []string{"connect"}))
		if host.Name != "jwt-source" || command != want || timeout != jwtRejectionsCensusTimeout {
			t.Fatal("public JWT reader did not use the mocked owning native program")
		}
		return out, sourceErr
	}
	return settings
}

// A quiet complete older process cannot certify health beside an invisible
// current target. An independently observed rate breach still survives.
func TestJwtRejectionsCensusTargetMetadataPublicControls(t *testing.T) {
	for _, mode := range []string{"absent service", "null service", "empty service", "wrong service", "wrong target service", "missing block", "wrong name"} {
		t.Logf("case: %s", mode)
		for _, rate := range []float64{0, 2} {
			settings := jwtRejectionsNativeSettings(t, rate, mode)
			alerts, err := NewJwtRejectionsSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatalf("%s: bounded native census failure escaped the public reducer", mode)
			}
			unknown := requireAlertClass(t, alerts, "jwt-rejections-unobservable")
			if !unknown.unobservable {
				t.Fatalf("%s: invisible current target could certify recovery", mode)
			}
			if rate == 0 {
				if len(alerts) != 1 {
					t.Fatalf("%s: quiet process fabricated an incident beside incomplete census", mode)
				}
			} else {
				requireAlertClass(t, alerts, "jwt-signature-rejection-rate")
				if len(alerts) != 2 {
					t.Fatalf("%s: independent positive did not survive census uncertainty", mode)
				}
			}
			for _, alert := range alerts {
				requireAlertOmits(t, alert, "private-token-key-canary", "private-block-canary", "jwt-source", "00000001")
			}
		}
	}
}
