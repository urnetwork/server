package api

import (
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/server/session"
)

// The ClientInfo schema of connect/api/bringyour.yml names exactly the
// distributions the parser keeps (connect client_info.go), each with a device
// type it pairs with: the spec is what app and sdk authors read to stamp a
// build, and a value missing from either side is a build whose distribution
// the server reads as unknown.
func TestSpecClientInfoDistributionsMatchTheParser(t *testing.T) {
	spec := loadSpec(t)
	properties := asMap(asMap(spec.schemas["ClientInfo"])["properties"])
	enum, ok := asMap(properties["distribution"])["enum"].([]any)
	if !ok {
		t.Fatal("the ClientInfo schema has no distribution enum")
	}
	specValues := []string{}
	for _, value := range enum {
		specValues = append(specValues, fmt.Sprint(value))
	}
	parserValues := []string{
		session.ClientDistributionAppStore,
		session.ClientDistributionMacDirect,
		session.ClientDistributionPlay,
		session.ClientDistributionAndroidDirect,
		session.ClientDistributionSolanaDapp,
		session.ClientDistributionEthosDapp,
		session.ClientDistributionWindows,
		session.ClientDistributionLinux,
		session.ClientDistributionLinuxAppimage,
		session.ClientDistributionLinuxFlatpak,
		session.ClientDistributionWeb,
		session.ClientDistributionExtensionChrome,
		session.ClientDistributionExtensionFirefox,
	}
	slices.Sort(specValues)
	slices.Sort(parserValues)
	if !slices.Equal(specValues, parserValues) {
		t.Fatalf("spec %v, parser %v", specValues, parserValues)
	}
	for _, value := range specValues {
		kept := false
		for _, deviceType := range []string{"android", "ios", "macos", "windows", "linux", "web"} {
			info := session.ParseClientInfo(fmt.Sprintf(`{"v":1,"device_type":%q,"app_version":"1","distribution":%q}`, deviceType, value), "")
			kept = kept || info.Distribution == value
		}
		if !kept {
			t.Errorf("the parser keeps %q with no device type", value)
		}
	}
}
