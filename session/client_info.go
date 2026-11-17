// Advisory metadata shares a parser with connect's header and frame transports.
package session

import (
	"github.com/urnetwork/connect"
	"net/http"
)

const ClientInfoHeader = connect.ClientInfoHeader
const ClientInfoMaxBytes = connect.ClientInfoMaxBytes

// The distributions an app's build reports in ClientInfo.Distribution
// (connect.ClientDistribution*, which the apps' sdk exports too). The parser
// keeps a value only with the device type it pairs with; "" is unknown.
const (
	ClientDistributionAppStore         = connect.ClientDistributionAppStore
	ClientDistributionMacDirect        = connect.ClientDistributionMacDirect
	ClientDistributionPlay             = connect.ClientDistributionPlay
	ClientDistributionAndroidDirect    = connect.ClientDistributionAndroidDirect
	ClientDistributionSolanaDapp       = connect.ClientDistributionSolanaDapp
	ClientDistributionEthosDapp        = connect.ClientDistributionEthosDapp
	ClientDistributionWindows          = connect.ClientDistributionWindows
	ClientDistributionLinux            = connect.ClientDistributionLinux
	ClientDistributionLinuxAppimage    = connect.ClientDistributionLinuxAppimage
	ClientDistributionLinuxFlatpak     = connect.ClientDistributionLinuxFlatpak
	ClientDistributionWeb              = connect.ClientDistributionWeb
	ClientDistributionExtensionChrome  = connect.ClientDistributionExtensionChrome
	ClientDistributionExtensionFirefox = connect.ClientDistributionExtensionFirefox
)

type ClientInfo = connect.ClientInfo
type SessionLastUsed struct {
	UnixTime    int64  `json:"unix_time"`
	City        string `json:"city"`
	Region      string `json:"region"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	DeviceType  string `json:"device_type"`
	AppVersion  string `json:"app_version"`
}

func UnknownClientInfo() ClientInfo { return connect.UnknownClientInfo() }
func ParseClientInfo(encoded, legacy string) ClientInfo {
	return connect.ParseClientInfo(encoded, legacy)
}
func ClientInfoFromHeader(header http.Header) ClientInfo { return connect.ClientInfoFromHeader(header) }
