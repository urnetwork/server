package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestActiveWebsiteDomainUsesManagedProductDomain(t *testing.T) {
	if got := activeWebsiteDomainFromServices(servicesYaml{
		Domains: map[string]string{"ur.io": "cloudflare", "bringyour.com": "route53"},
	}); got != "ur.io" {
		t.Fatalf("website domain = %q, want ur.io", got)
	}
	if got := activeWebsiteDomainFromServices(servicesYaml{
		Domains: map[string]string{"local-test.bringyour.com": "route53"},
	}); got != "" {
		t.Fatalf("alternate environment armed production website probe with %q", got)
	}
}

func TestAssociationFilesSignalSyntheticHealthyContracts(t *testing.T) {
	address := "2001:db8:19::1"
	var lock sync.Mutex
	calls := []string{}
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		joined := strings.Join(args, " ")
		lock.Lock()
		calls = append(calls, joined)
		lock.Unlock()
		if !strings.Contains(joined, "ur.io:443:["+address+"]") {
			return "", fmt.Errorf("probe did not pin ur.io to the exact edge: %s", joined)
		}
		switch {
		case strings.Contains(joined, "/.well-known/assetlinks.json"):
			return associationHTTPFixture(validAssetlinksFixture(), "200", "0", "application/json", address), nil
		case strings.Contains(joined, "/.well-known/apple-app-site-association"):
			return associationHTTPFixture(validAppleAssociationFixture(), "200", "0", "application/json; charset=utf-8", address), nil
		default:
			return "", errors.New("unexpected association path")
		}
	}}
	settings := associationSyntheticSettings(source, address)

	alerts, err := NewAssociationFilesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy association contracts alerted: %+v", alerts)
	}
	lock.Lock()
	defer lock.Unlock()
	if len(calls) != 2 {
		t.Fatalf("association requests = %d, want both platform files: %v", len(calls), calls)
	}
}

func TestAssociationFilesSignalSyntheticMissingAndInvalid(t *testing.T) {
	missingAddress := "2001:db8:19::404"
	invalidAddress := "2001:db8:19::200"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "["+missingAddress+"]"):
			return associationHTTPFixture("<html>branded not found</html>", "404", "0", "text/html", missingAddress), nil
		case strings.Contains(joined, "["+invalidAddress+"]") && strings.Contains(joined, "/assetlinks.json"):
			return associationHTTPFixture(`[{"relation":[],"target":{"namespace":"android_app"}}]`, "200", "0", "application/json", invalidAddress), nil
		case strings.Contains(joined, "["+invalidAddress+"]") && strings.Contains(joined, "/apple-app-site-association"):
			return associationHTTPFixture(validAppleAssociationFixture(), "200", "0", "application/json", invalidAddress), nil
		default:
			return "", errors.New("unexpected edge/path request")
		}
	}}
	settings := associationSyntheticSettings(source, missingAddress, invalidAddress)

	alerts, err := NewAssociationFilesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one aggregated site-contract alert: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "web-association-files")
	if alert.SignalNumber != "19.1" || alert.SignalKey != "association-files" || alert.Target != "ur.io" {
		t.Fatalf("wrong association signal identity: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"missing_404=2",
		"invalid_contract=1",
		missingAddress,
		invalidAddress,
		"/.well-known/assetlinks.json",
		"/.well-known/apple-app-site-association",
		"mv dist/*",
		"72190198",
		"not optional crawler assets",
		"SIGNALS.md §19.1",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("association alert missing %q: %s", want, markdown)
		}
	}
}

func TestAssociationFilesSignalLeavesTransportFailureToEdgeIPv6(t *testing.T) {
	address := "2001:db8:19::28"
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "curl" {
			return "", errors.New("unexpected local command")
		}
		return associationHTTPFixture("", "000", "28", "", ""), errors.New("exit status 28")
	}}

	alerts, err := NewAssociationFilesSignal().Run(
		context.Background(),
		associationSyntheticSettings(source, address),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("transport-only failure duplicated the edge-ipv6 alert: %+v", alerts)
	}
}

func associationSyntheticSettings(source SignalSource, addresses ...string) SignalSettings {
	settings := syntheticSettings(source)
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

func associationHTTPFixture(body, code, exitCode, contentType, remoteIP string) string {
	return body + "\n" + associationOutputMarker + "\n" +
		"monitor_http_code=" + code + "\n" +
		"monitor_exitcode=" + exitCode + "\n" +
		"monitor_remote_ip=" + remoteIP + "\n" +
		"monitor_content_type=" + contentType + "\n" +
		"monitor_time_total=0.080\n"
}

func validAssetlinksFixture() string {
	fingerprint := strings.Repeat("AA:", 31) + "AA"
	return fmt.Sprintf(`[{"relation":["delegate_permission/common.handle_all_urls"],"target":{"namespace":"android_app","package_name":"%s","sha256_cert_fingerprints":["%s"]}}]`, associationAndroidPackage, fingerprint)
}

func validAppleAssociationFixture() string {
	return fmt.Sprintf(`{"applinks":{"details":[{"appIDs":["%s"],"components":[]}]},"webcredentials":{"apps":["%s"]}}`, associationAppleAppID, associationAppleAppID)
}
