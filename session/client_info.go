// Advisory metadata shares a parser with connect's header and frame transports.
package session

import (
	"github.com/urnetwork/connect"
	"net/http"
)

const ClientInfoHeader = connect.ClientInfoHeader
const ClientInfoMaxBytes = connect.ClientInfoMaxBytes

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
