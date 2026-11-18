package server

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

const testSndsKey = "00000000-test-key-not-a-credential"

// The client's settings on a .example origin.
func testSndsSettings() *SndsClientSettings {
	settings := DefaultSndsClientSettings()
	settings.BaseUrl = "https://snds.example"
	return settings
}

// The client reads each listed address's filter result from SNDS's own file, and names the red ones among the
// addresses asked.
func TestSndsClientReadsEachAddressColor(t *testing.T) {
	transport := &testMailMeasuresTransport{answer: func(request *http.Request) (*http.Response, error) {
		return testMailMeasuresResponse(http.StatusOK, testMailMeasuresFixture(t, "snds_data.csv")), nil
	}}
	client := NewSndsClient(&SndsClientArgs{Key: testSndsKey, Transport: transport, Settings: testSndsSettings()})
	addressColors, known := client.Colors(context.Background())
	if !known || len(addressColors) != 4 {
		t.Fatalf("colors %v, known %t", addressColors, known)
	}
	for address, color := range map[string]SndsColor{
		"192.0.2.10":   SndsColorGreen,
		"192.0.2.11":   SndsColorRed,
		"192.0.2.12":   SndsColorYellow,
		"2001:db8::10": SndsColorGreen,
	} {
		if got := addressColors[netip.MustParseAddr(address)]; got != color {
			t.Errorf("%s is %q, want %q", address, got, color)
		}
	}
	// an address with no row has no color, and only red counts
	own := []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("::ffff:192.0.2.11"), netip.MustParseAddr("192.0.2.12"), netip.MustParseAddr("192.0.2.99")}
	red, known := client.RedAddresses(context.Background(), own)
	if !known || !slices.Equal(red, []netip.Addr{netip.MustParseAddr("192.0.2.11")}) {
		t.Errorf("red %v, known %t", red, known)
	}
	request := transport.seen()[0]
	if request.URL.Scheme != "https" || request.URL.Host != "snds.example" || request.URL.Path != "/snds/data.aspx" || request.URL.Query().Get("key") != testSndsKey || len(request.URL.Query()) != 1 {
		t.Errorf("asked %s", request.URL.Path)
	}
}

// No key, an error, and a file that is not SNDS's read unknown, never green, and no error names the key.
func TestSndsClientAnswersUnknownNotGreen(t *testing.T) {
	asked := &testMailMeasuresTransport{answer: func(request *http.Request) (*http.Response, error) {
		return testMailMeasuresResponse(http.StatusOK, testMailMeasuresFixture(t, "snds_data.csv")), nil
	}}
	none := NewSndsClient(&SndsClientArgs{Transport: asked, Settings: testSndsSettings()})
	if colors, known := none.Colors(context.Background()); known || colors != nil {
		t.Error("a client with no key answered")
	}
	if red, known := none.RedAddresses(context.Background(), []netip.Addr{netip.MustParseAddr("192.0.2.11")}); known || red != nil {
		t.Error("a client with no key named red addresses")
	}
	if len(asked.seen()) != 0 {
		t.Error("a client with no key asked SNDS")
	}

	cases := []struct {
		name   string
		answer func(request *http.Request) (*http.Response, error)
	}{
		{"a refused key", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusForbidden, nil), nil
		}},
		{"an unreachable service", func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("dial " + request.URL.String())
		}},
		{"a redirect", func(request *http.Request) (*http.Response, error) {
			response := testMailMeasuresResponse(http.StatusFound, nil)
			response.Header.Set("Location", "https://elsewhere.example/")
			return response, nil
		}},
		{"a row with no color", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, testMailMeasuresFixture(t, "snds_data_bad_row.csv")), nil
		}},
		{"a sign-in page", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, []byte("<html><body>Sign in</body></html>\n")), nil
		}},
		{"a short row", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, []byte("192.0.2.10,GREEN\n")), nil
		}},
		{"a file past its bound", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, []byte(strings.Repeat("\n", 17*1024*1024))), nil
		}},
	}
	for _, c := range cases {
		client := NewSndsClient(&SndsClientArgs{Key: testSndsKey, Transport: &testMailMeasuresTransport{answer: c.answer}, Settings: testSndsSettings()})
		if colors, known := client.Colors(context.Background()); known || colors != nil {
			t.Errorf("%s: %v, known %t", c.name, colors, known)
		}
		_, err := client.colors(context.Background())
		if !errors.Is(err, ErrSndsSource) {
			t.Errorf("%s: %v", c.name, err)
		} else if strings.Contains(err.Error(), testSndsKey) || strings.Contains(err.Error(), "192.0.2.") || strings.Contains(err.Error(), "snds.example") {
			t.Errorf("%s: the error names the key, the url or an address: %v", c.name, err)
		}
	}
	// a file that read with no rows is an answer: no address has a color
	empty := NewSndsClient(&SndsClientArgs{Key: testSndsKey, Transport: &testMailMeasuresTransport{answer: func(request *http.Request) (*http.Response, error) {
		return testMailMeasuresResponse(http.StatusOK, nil), nil
	}}, Settings: testSndsSettings()})
	if colors, known := empty.Colors(context.Background()); !known || len(colors) != 0 {
		t.Errorf("an empty file: %v, known %t", colors, known)
	}
}

// An address listed twice keeps its worst color.
func TestParseSndsDataKeepsTheWorstColor(t *testing.T) {
	row := func(color string) string {
		return "192.0.2.10,11/1/2026 12:00 AM,11/2/2026 12:00 AM,10,10,10," + color + ",0%,,,0,o1.out.report.example,bounce@o1.out.report.example,\n"
	}
	colors, err := parseSndsData(row("YELLOW")+row("RED")+row("GREEN"), DefaultSndsClientSettings())
	if err != nil || colors[netip.MustParseAddr("192.0.2.10")] != SndsColorRed {
		t.Errorf("%v, %v", colors, err)
	}
}

// abuse-snds.yml reads as its key, and a file with none or not YAML is refused with no value in the error.
func TestParseSndsCredential(t *testing.T) {
	if key, err := ParseSndsCredential([]byte("key: \" " + testSndsKey + " \"\n")); err != nil || key != testSndsKey {
		t.Fatalf("%q, %v", key, err)
	}
	for _, data := range []string{"", "key: \"\"\n", "other: x\n", "key: [" + testSndsKey + "\n"} {
		key, err := ParseSndsCredential([]byte(data))
		if key != "" || !errors.Is(err, ErrSndsCredential) || strings.Contains(err.Error(), testSndsKey) {
			t.Errorf("%q: %q, %v", data, key, err)
		}
	}
}
