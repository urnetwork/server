package handlers

import (
	"encoding/json"
	// "net"
	"net/http"
	"regexp"
	"strings"

	"github.com/ipinfo/go/v2/ipinfo"
	// "github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/api/myipinfo/landmarks"
	"github.com/urnetwork/server/v2025/api/myipinfo/myinfo"
	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
)

type response struct {
	Info               myinfo.MyInfo              `json:"info"`
	ExpectedRTTs       []landmarks.LandmarkAndRTT `json:"landmarks"`
	ConnectedToNetwork bool                       `json:"connected_to_network"`
}

// matches the first group to the IPV6 address when the input is <ipv6>:<port>
// example: 2001:5a8:4683:4e00:3a76:dcec:7cb:f180:40894
var malformedIPV6WithPort = regexp.MustCompile(`^(.+):\d+$`)

func MyIPInfo(w http.ResponseWriter, r *http.Request) {

	clientAddress := r.Header.Get("X-UR-Forwarded-For")

	if clientAddress == "" {
		clientAddress = r.Header.Get("X-Forwarded-For")
	}

	if clientAddress == "" {
		clientAddress = r.RemoteAddr
	}

	clientIp, _, err := session.SplitClientAddress(clientAddress)

	ipInfoRaw, err := controller.GetIPInfo(r.Context(), clientIp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	info := &ipinfo.Core{}
	err = json.Unmarshal(ipInfoRaw, info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// enrich the IP info with additional data
	func(v *ipinfo.Core) {
		if v.Country != "" {
			v.CountryName = ipinfo.GetCountryName(v.Country)
			v.IsEU = ipinfo.IsEU(v.Country)
			v.CountryFlag.Emoji = ipinfo.GetCountryFlagEmoji(v.Country)
			v.CountryFlag.Unicode = ipinfo.GetCountryFlagUnicode(v.Country)
			v.CountryFlagURL = ipinfo.GetCountryFlagURL(v.Country)
			v.CountryCurrency.Code = ipinfo.GetCountryCurrencyCode(v.Country)
			v.CountryCurrency.Symbol = ipinfo.GetCountryCurrencySymbol(v.Country)
			v.Continent.Code = ipinfo.GetContinentCode(v.Country)
			v.Continent.Name = ipinfo.GetContinentName(v.Country)
		}
		if v.Abuse != nil && v.Abuse.Country != "" {
			v.Abuse.CountryName = ipinfo.GetCountryName(v.Abuse.Country)
		}
	}(info)

	if info.Bogon {
		http.Error(w, "bogon IP", http.StatusForbidden)
		return
	}

	myInfo, err := myinfo.FromIPInfo(info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	isIPV6 := strings.Contains(clientIp, ":")

	var landmarksAndRTTs []landmarks.LandmarkAndRTT

	if myInfo.Location != nil && myInfo.Location.Coordinates != nil {
		if isIPV6 {
			landmarksAndRTTs = landmarks.CurrentLandmarks.V6.ExpectedRTTSVerbose(*myInfo.Location.Coordinates, 5)
		} else {
			landmarksAndRTTs = landmarks.CurrentLandmarks.V4.ExpectedRTTSVerbose(*myInfo.Location.Coordinates, 5)
		}
	}

	respStruct := response{
		Info:               myInfo,
		ExpectedRTTs:       landmarksAndRTTs,
		ConnectedToNetwork: model.IsIpConnectedToNetwork(r.Context(), clientIp),
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(respStruct)

}
