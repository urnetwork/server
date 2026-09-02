package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestEmailAssetsCatalogMatchesServerTemplates(t *testing.T) {
	templatePaths, err := filepath.Glob("../controller/email_templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(templatePaths) == 0 {
		t.Fatal("server email-template inventory is empty")
	}
	pattern := regexp.MustCompile(`https://ur\.io(/images/emails/[A-Za-z0-9._-]+)`)
	fromTemplates := map[string]struct{}{}
	for _, templatePath := range templatePaths {
		contents, err := os.ReadFile(templatePath)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllSubmatch(contents, -1) {
			fromTemplates[string(match[1])] = struct{}{}
		}
	}
	fromProbe := map[string]struct{}{}
	for _, asset := range emailAssets {
		fromProbe[asset.path] = struct{}{}
	}
	if got, want := sortedEmailAssetPaths(fromProbe), sortedEmailAssetPaths(fromTemplates); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("probe/template email assets differ\nprobe:\n%s\ntemplates:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestEmailAssetsSignalSyntheticHealthyPublicAndEdges(t *testing.T) {
	address := "2001:db8:19::2"
	var lock sync.Mutex
	calls := map[string]int{}
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		joined := strings.Join(args, " ")
		asset := requestedEmailAsset(joined)
		if asset == "" {
			return "", fmt.Errorf("request omitted a tracked email asset: %s", joined)
		}
		scope := "public"
		if strings.Contains(joined, "--resolve") {
			scope = "edge"
			// the edge check keeps the site's own Host, pinned to the exact address
			if !strings.Contains(joined, "ur.io:443:["+address+"]") ||
				!strings.Contains(joined, "https://ur.io"+asset) {
				return "", fmt.Errorf("edge request did not retain exact address and Host: %s", joined)
			}
		} else if !strings.Contains(joined, "https://ur.io"+asset) {
			return "", fmt.Errorf("public request used the wrong URL: %s", joined)
		}
		lock.Lock()
		calls[scope+":"+asset]++
		lock.Unlock()
		return emailAssetHTTPFixture("200", "0", "image/png", "5038", address), nil
	}}

	alerts, err := NewEmailAssetsSignal().Run(
		context.Background(),
		emailAssetSyntheticSettings(source, address),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy email assets alerted: %+v", alerts)
	}
	lock.Lock()
	defer lock.Unlock()
	if len(calls) != 2*len(emailAssets) {
		t.Fatalf("distinct public/edge requests = %d, want %d: %v", len(calls), 2*len(emailAssets), calls)
	}
	for _, asset := range emailAssets {
		for _, scope := range []string{"public", "edge"} {
			if calls[scope+":"+asset.path] != 1 {
				t.Errorf("%s %s calls = %d, want one", scope, asset.path, calls[scope+":"+asset.path])
			}
		}
	}
}

func TestEmailAssetsSignalDisarmedWithoutWebsiteDomain(t *testing.T) {
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		return "", errors.New("no request expected without a website domain")
	}}
	settings := emailAssetSyntheticSettings(source, "2001:db8:19::9")
	settings.WebsiteDomain = ""

	alerts, err := NewEmailAssetsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("probe ran without a website domain: %+v", alerts)
	}
}

func TestEmailAssetsSignalSyntheticStaleSiteBundleRootCause(t *testing.T) {
	address := "2001:db8:19::404"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" || requestedEmailAsset(strings.Join(args, " ")) == "" {
			return "", errors.New("unexpected email-asset request")
		}
		return emailAssetHTTPFixture("404", "0", "text/html", "146", address), nil
	}}

	alerts, err := NewEmailAssetsSignal().Run(
		context.Background(),
		emailAssetSyntheticSettings(source, address),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one aggregated email-asset alert: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "web-email-assets")
	if alert.SignalNumber != "19.2" || alert.SignalKey != "email-assets" || alert.Target != "ur.io" {
		t.Fatalf("wrong email-asset identity: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"public_failed=2",
		"edge_failed=2",
		"missing_404=4",
		"ur.io",
		address,
		"/images/emails/ur-wordmark-white-320.png",
		"controller/email_templates",
		"sync-public",
		"Mmm descendant of b4b229c5c",
		"clean, attributable SDK checkout",
		"Web service",
		"release-order blocker",
		"API and Taskworker",
		"server commit ec6e3b92",
		"deployed build predates the assets",
		"functional product regression",
		"SIGNALS.md §19.2",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("email-asset alert missing %q: %s", want, markdown)
		}
	}
	for _, stale := range []string{"CloudFront", "main-web", "/res/emails/", "7c852d56"} {
		if strings.Contains(markdown, stale) {
			t.Fatalf("email-asset alert retained the retired hosting diagnosis %q: %s", stale, markdown)
		}
	}
}

func TestEmailAssetsSignalRejectsHTMLAndEmptyImages(t *testing.T) {
	address := "2001:db8:19::200"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		// Keep the public contract healthy and synthesize two different exact-edge
		// semantic failures behind it.
		if !strings.Contains(joined, "--resolve") {
			return emailAssetHTTPFixture("200", "0", "image/png", "1024", address), nil
		}
		switch requestedEmailAsset(joined) {
		case "/images/emails/ur-wordmark-black-bg-320.png":
			return emailAssetHTTPFixture("200", "0", "text/html", "1024", address), nil
		case "/images/emails/ur-wordmark-white-320.png":
			return emailAssetHTTPFixture("200", "0", "image/png", "0", address), nil
		default:
			return emailAssetHTTPFixture("200", "0", "image/png", "4096", address), nil
		}
	}}

	alerts, err := NewEmailAssetsSignal().Run(
		context.Background(),
		emailAssetSyntheticSettings(source, address),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one semantic email-asset alert: %+v", len(alerts), alerts)
	}
	markdown := alerts[0].Markdown()
	for _, want := range []string{"non_image=1", "empty_body=1", `content type "text/html" is not an image`, `body size "0" is not positive`} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("semantic alert missing %q: %s", want, markdown)
		}
	}
}

func TestEmailAssetsSignalLeavesExactTransportFailureToEdgeIPv6(t *testing.T) {
	address := "2001:db8:19::28"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		if strings.Contains(strings.Join(args, " "), "--resolve") {
			return emailAssetHTTPFixture("000", "28", "", "0", ""), errors.New("exit status 28")
		}
		return emailAssetHTTPFixture("200", "0", "image/png", "1024", "203.0.113.19"), nil
	}}

	alerts, err := NewEmailAssetsSignal().Run(
		context.Background(),
		emailAssetSyntheticSettings(source, address),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("exact transport-only failure duplicated edge-ipv6: %+v", alerts)
	}
}

func TestEmailAssetsSignalClassifiesTransientPublicTransport(t *testing.T) {
	address := "2001:db8:19::35"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		joined := strings.Join(args, " ")
		asset := requestedEmailAsset(joined)
		if asset == "" {
			return "", fmt.Errorf("request omitted a tracked email asset: %s", joined)
		}
		if !strings.Contains(joined, "--resolve") && asset == "/images/emails/ur-wordmark-black-bg-320.png" {
			return emailAssetHTTPFixture("000", "35", "", "0", "2001:db8:ffff::35"),
				errors.New("curl --http1.1 https://ur.io/images/emails/ur-wordmark-black-bg-320.png: exit status 35")
		}
		return emailAssetHTTPFixture("200", "0", "image/png", "4096", address), nil
	}}

	alerts, err := NewEmailAssetsSignal().Run(
		context.Background(),
		emailAssetSyntheticSettings(source, address),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one transport diagnostic: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "web-email-assets-transport")
	if alert.Sustain != 2 {
		t.Fatalf("transport alert sustain = %d, want two consecutive cadences", alert.Sustain)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"transport_failures=1",
		"other_response_failures=0",
		"public_failed=1",
		"edge_failed=0",
		"problem=curl exit 35",
		"before receiving any HTTP response",
		"two consecutive five-minute cadences",
		"separately over IPv4 and IPv6",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("transport alert missing %q: %s", want, markdown)
		}
	}
	for _, stale := range []string{"cached negative/error response", "invalidate only the failed", "curl --http1.1"} {
		if strings.Contains(markdown, stale) {
			t.Fatalf("transport alert retained stale diagnosis %q: %s", stale, markdown)
		}
	}
}

func emailAssetSyntheticSettings(source SignalSource, addresses ...string) SignalSettings {
	settings := syntheticSettings(source)
	settings.PublicDomain = "example.com"
	settings.WebsiteDomain = "ur.io"
	settings.Hosts = []HostSettings{{Name: "by-us-test-edge-1"}}
	for index, address := range addresses {
		settings.Hosts[0].EdgeIPv6 = append(settings.Hosts[0].EdgeIPv6, EdgeIPv6InterfaceSettings{
			Interface: fmt.Sprintf("eno%d", index+1),
			Address:   address,
		})
	}
	return settings
}

func requestedEmailAsset(joined string) string {
	for _, asset := range emailAssets {
		if strings.Contains(joined, asset.path) {
			return asset.path
		}
	}
	return ""
}

func emailAssetHTTPFixture(code, exitCode, contentType, size, remoteIP string) string {
	return "\nmonitor_http_code=" + code + "\n" +
		"monitor_exitcode=" + exitCode + "\n" +
		"monitor_remote_ip=" + remoteIP + "\n" +
		"monitor_content_type=" + contentType + "\n" +
		"monitor_size_download=" + size + "\n" +
		"monitor_time_total=0.080\n"
}

func sortedEmailAssetPaths(paths map[string]struct{}) []string {
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return ordered
}
