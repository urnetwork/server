package alt

import (
	"context"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/api"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/router"
)

// The alt package's own import path, which nothing behind the lb may reach.
const altPackagePath = "github.com/urnetwork/server/alt"

func defaultRunOptions() RunOptions {
	return RunOptions{
		Port:    DefaultStatusPort,
		H3Port:  DefaultH3Port,
		DnsPort: DefaultDnsPort,
	}
}

func TestRunRejectsInvalidInputsBeforeEnvironmentAccess(t *testing.T) {
	if err := Run(nil, defaultRunOptions()); err == nil {
		t.Fatal("nil context was accepted")
	}
	cases := []RunOptions{
		{Port: DefaultStatusPort, H3Port: 0, DnsPort: DefaultDnsPort},
		{Port: DefaultStatusPort, H3Port: 65_536, DnsPort: DefaultDnsPort},
		{Port: DefaultStatusPort, H3Port: DefaultH3Port, DnsPort: 0},
		{Port: DefaultStatusPort, H3Port: DefaultH3Port, DnsPort: 65_536},
		{Port: DefaultStatusPort, H3Port: DefaultH3Port, DnsPort: DefaultH3Port},
		{Port: 0, H3Port: DefaultH3Port, DnsPort: DefaultDnsPort},
		{Port: 65_536, H3Port: DefaultH3Port, DnsPort: DefaultDnsPort},
	}
	for _, options := range cases {
		if err := options.Validate(); err == nil {
			t.Errorf("%+v was accepted", options)
		}
	}
	if err := defaultRunOptions().Validate(); err != nil {
		t.Fatal(err)
	}
}

// Alt's connect handler owns no listener and unwraps no proxy protocol
// header: alt binds the public udp sockets itself and there is no nginx in
// front of it (L1).
func TestDefaultSettingsGiveTheConnectHandlerNoListenerAndNoProxyProtocol(t *testing.T) {
	settings := DefaultSettings()
	handlerSettings := settings.ExchangeSettings.ConnectHandlerSettings
	if handlerSettings.ListenH3Port != 0 || handlerSettings.ListenDnsPort != 0 {
		t.Fatalf("alt connect handler listens on h3=%d dns=%d", handlerSettings.ListenH3Port, handlerSettings.ListenDnsPort)
	}
	if len(handlerSettings.ListenDnsCompatibilityPorts) != 0 {
		t.Fatalf("alt connect handler compatibility ports = %v", handlerSettings.ListenDnsCompatibilityPorts)
	}
	if handlerSettings.EnableProxyProtocol {
		t.Fatal("alt connect handler still expects a proxy protocol header")
	}
	if len(settings.DnsTlds) != 1 || settings.DnsTlds[0] != DefaultDnsTld {
		t.Fatalf("alt whodis tlds = %v", settings.DnsTlds)
	}
}

// The lb-fronted connect service keeps its own listeners and the proxy
// protocol nginx prepends, so alt's settings cannot have leaked into it.
func TestLbFrontedConnectKeepsItsListenersAndProxyProtocol(t *testing.T) {
	handlerSettings := connectserver.DefaultConnectHandlerSettings()
	if handlerSettings.ListenH3Port != 443 || handlerSettings.ListenDnsPort != 4053 {
		t.Fatalf("connect listens on h3=%d dns=%d", handlerSettings.ListenH3Port, handlerSettings.ListenDnsPort)
	}
	if !handlerSettings.EnableProxyProtocol {
		t.Fatal("connect lost its proxy protocol header")
	}
}

// Behind the lb nginx enforces the limits and the go services enforce none
// (L5). The limiter exists only in alt, so neither lb-fronted package may
// reach it, in production or in its own tests.
func TestLbFrontedServicesConstructNoLimiter(t *testing.T) {
	for _, dir := range []string{"../api", "../api/handlers", "../connect"} {
		fileSet := token.NewFileSet()
		dirPackages, err := parser.ParseDir(fileSet, dir, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, dirPackage := range dirPackages {
			for path, file := range dirPackage.Files {
				for _, packageImport := range file.Imports {
					if strings.Trim(packageImport.Path.Value, `"`) == altPackagePath {
						t.Errorf("%s imports the alt limiter", path)
					}
				}
			}
		}
	}
}

// The same route table behind the lb carries no limit of its own: only the
// alt front wraps it.
func TestLbFrontedApiRoutesAnswerAboveTheAltBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, api.Routes())

	limitsSettings := testLimitsSettings(t)
	requestCount := 4 * (limitsSettings.Burst + 1)
	for i := range requestCount {
		r := httptest.NewRequest(http.MethodGet, "/status", nil)
		r.RemoteAddr = "198.51.100.13:4000"
		w := httptest.NewRecorder()
		apiRouter.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("the lb-fronted api router refused request %d of %d", i, requestCount)
		}
	}
}
