package server

// Microsoft SNDS's per-address filter result, one of the measures §21.5's breakers read (ABUSE-M9-03; D-ABUSE-2,
// R-ABUSE-4, I-13): the automated data access file, a CSV with one row for each of the account's addresses that
// sent enough mail in the period, whose seventh column is GREEN, YELLOW or RED.
//
// The answer is each address's color or unknown, never green for want of an answer. Unknown is every case SNDS gave
// no usable file: no key, an unreachable service, any status but 200, a file past its bound, and a row that does not
// read as SNDS's, since a file half read could hide a red row. An address with no row in a file that did read has no
// color: SNDS lists only addresses with enough mail in the period, so no row is no verdict against it, and the
// caller counts only red.
//
// SNDS takes the key in the url's query, so the url is never logged and no error carries it, the key or a row
// (I-13). Only the account's own addresses are in the file, and the client asks about no address: it reads what SNDS
// lists for the account. The credential is external: with no key the client answers unknown. Safe for concurrent
// use.

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A filter result, as SNDS words it.
type SndsColor string

const (
	SndsColorGreen  SndsColor = "GREEN"
	SndsColorYellow SndsColor = "YELLOW"
	SndsColorRed    SndsColor = "RED"
)

// SNDS's names: the data file's path and the key's query field.
const (
	sndsDataPath      = "/snds/data.aspx"
	sndsKeyQueryField = "key"
)

// SNDS gave no usable file: see the file header.
var ErrSndsSource = errors.New("snds: the source gave no usable answer")

// The vault file holding the account's automated data access key (QD-1069).
const SndsCredentialFileName = "abuse-snds.yml"

// abuse-snds.yml: the key, a credential.
type sndsCredentialYaml struct {
	Key string `yaml:"key"`
}

// The file holds no usable key. The text never holds the key.
var ErrSndsCredential = errors.New("snds: the credential is not usable")

// Reads abuse-snds.yml's key. A file that does not parse or holds none is refused.
func ParseSndsCredential(data []byte) (key string, err error) {
	credential := &sndsCredentialYaml{}
	// a decode error can quote the line it failed on, so only the verdict is kept
	if err := yaml.Unmarshal(data, credential); err != nil {
		return "", fmt.Errorf("%w: the file does not parse", ErrSndsCredential)
	}
	if key = strings.TrimSpace(credential.Key); key == "" {
		return "", fmt.Errorf("%w: no key", ErrSndsCredential)
	}
	return key, nil
}

// Tunables of the client. The columns are the file's format, so a deployment changes them only with SNDS.
type SndsClientSettings struct {
	// SNDS's origin: https, with no path.
	BaseUrl string
	// One request's deadline.
	RequestTimeout time.Duration
	// The most bytes one file is read to.
	ResponseMaxByteCount int64
	// The file's columns, from zero: the address, and the filter result.
	AddressColumn int
	ColorColumn   int
	// The fewest columns a row has.
	ColumnMinCount int
}

func DefaultSndsClientSettings() *SndsClientSettings {
	return &SndsClientSettings{
		BaseUrl:              "https://sendersupport.olc.protection.outlook.com",
		RequestTimeout:       60 * time.Second,
		ResponseMaxByteCount: 16 * 1024 * 1024,
		AddressColumn:        0,
		ColorColumn:          6,
		ColumnMinCount:       7,
	}
}

// What NewSndsClient takes.
type SndsClientArgs struct {
	// The account's automated data access key. Empty answers unknown.
	Key string
	// How the client reaches SNDS. nil dials directly, with a transport of the client's own.
	Transport http.RoundTripper
	// nil takes the defaults.
	Settings *SndsClientSettings
}

// See the file header.
type SndsClient struct {
	key        string
	httpClient *http.Client
	settings   *SndsClientSettings
}

func NewSndsClient(args *SndsClientArgs) *SndsClient {
	settings := args.Settings
	if settings == nil {
		settings = DefaultSndsClientSettings()
	}
	transport := args.Transport
	if transport == nil {
		transport = &http.Transport{Proxy: nil}
	}
	return &SndsClient{
		key: args.Key,
		httpClient: &http.Client{
			Transport: transport,
			// a redirect would carry the key's url elsewhere
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		settings: settings,
	}
}

// The color of each address SNDS lists for the account in its latest period, and whether the file was read (see the
// file header). An address the file does not list is not in the map.
func (self *SndsClient) Colors(ctx context.Context) (addressColors map[netip.Addr]SndsColor, known bool) {
	addressColors, err := self.colors(ctx)
	if err != nil {
		return nil, false
	}
	return addressColors, true
}

// Of addresses, those SNDS colors red, and whether the file was read.
func (self *SndsClient) RedAddresses(ctx context.Context, addresses []netip.Addr) (red []netip.Addr, known bool) {
	addressColors, known := self.Colors(ctx)
	if !known {
		return nil, false
	}
	red = []netip.Addr{}
	for _, address := range addresses {
		if addressColors[address.Unmap()] == SndsColorRed {
			red = append(red, address.Unmap())
		}
	}
	return red, true
}

func (self *SndsClient) colors(ctx context.Context) (map[netip.Addr]SndsColor, error) {
	if self.key == "" {
		return nil, fmt.Errorf("%w: no credential", ErrSndsSource)
	}
	base, err := url.Parse(self.settings.BaseUrl)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("%w: no usable origin", ErrSndsSource)
	}
	query := url.Values{}
	query.Set(sndsKeyQueryField, self.key)
	requestUrl := base.Scheme + "://" + base.Host + sndsDataPath + "?" + query.Encode()

	requestCtx, cancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestUrl, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: no request", ErrSndsSource)
	}
	response, err := self.httpClient.Do(request)
	if err != nil {
		// the transport's error names the url, and the url holds the key
		return nil, fmt.Errorf("%w: unreachable", ErrSndsSource)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrSndsSource, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, self.settings.ResponseMaxByteCount+1))
	if err != nil || self.settings.ResponseMaxByteCount < int64(len(body)) {
		return nil, fmt.Errorf("%w: a file past its bound", ErrSndsSource)
	}
	return parseSndsData(string(body), self.settings)
}

// The file's rows as colors. A row that is not SNDS's fails the whole file; an address listed twice keeps its worst
// color.
func parseSndsData(data string, settings *SndsClientSettings) (map[netip.Addr]SndsColor, error) {
	if settings.AddressColumn < 0 || settings.ColorColumn < 0 ||
		settings.ColumnMinCount <= settings.AddressColumn || settings.ColumnMinCount <= settings.ColorColumn {
		return nil, fmt.Errorf("%w: unusable columns", ErrSndsSource)
	}
	reader := csv.NewReader(strings.NewReader(data))
	reader.FieldsPerRecord = -1
	addressColors := map[netip.Addr]SndsColor{}
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: a file that is not csv", ErrSndsSource)
		}
		if len(record) == 1 && strings.TrimSpace(record[0]) == "" {
			continue
		}
		if len(record) < settings.ColumnMinCount {
			return nil, fmt.Errorf("%w: a row of %d columns", ErrSndsSource, len(record))
		}
		address, err := netip.ParseAddr(strings.TrimSpace(record[settings.AddressColumn]))
		if err != nil {
			return nil, fmt.Errorf("%w: a row with no address", ErrSndsSource)
		}
		address = address.Unmap()
		color := SndsColor(strings.ToUpper(strings.TrimSpace(record[settings.ColorColumn])))
		switch color {
		case SndsColorGreen, SndsColorYellow, SndsColorRed:
		default:
			return nil, fmt.Errorf("%w: a row with no filter result", ErrSndsSource)
		}
		if sndsColorRank(addressColors[address]) < sndsColorRank(color) {
			addressColors[address] = color
		}
	}
	return addressColors, nil
}

// Worse is higher; no color is lowest.
func sndsColorRank(color SndsColor) int {
	switch color {
	case SndsColorGreen:
		return 1
	case SndsColorYellow:
		return 2
	case SndsColorRed:
		return 3
	}
	return 0
}
