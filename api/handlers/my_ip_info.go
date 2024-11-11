package handlers

import (
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/model"
	"bringyour.com/service/api/myipinfo/landmarks"
	"bringyour.com/service/api/myipinfo/myinfo"
	"github.com/ipinfo/go/v2/ipinfo"
)

func MyIPInfoOptions(w http.ResponseWriter, r *http.Request) {
}

type response struct {
	Info               myinfo.MyInfo              `json:"info"`
	ExpectedRTTs       []landmarks.LandmarkAndRTT `json:"landmarks"`
	ConnectedToNetwork bool                       `json:"connected_to_network"`
}

// matches the first group to the IPV6 address when the input is <ipv6>:<port>
// example: 2001:5a8:4683:4e00:3a76:dcec:7cb:f180:40894
var malformedIPV6WithPort = regexp.MustCompile(`^(.+):\d+$`)

func MyIPInfo(w http.ResponseWriter, r *http.Request) {
	remoteAddr := r.Header.Get("X-Forwarded-For")

	if remoteAddr == "" {
		remoteAddr = r.RemoteAddr
	}

	columnCount := strings.Count(remoteAddr, ":")
	bracketCount := strings.Count(remoteAddr, "[")

	var addressOnly string

	// if the address is malformed, extract the address from the address:port string
	if columnCount > 1 && bracketCount == 0 {
		groups := malformedIPV6WithPort.FindStringSubmatch(remoteAddr)

		if len(groups) > 1 {
			addressOnly = groups[1]
		}

	} else {
		var err error
		addressOnly, _, err = net.SplitHostPort(remoteAddr)
		bringyour.Raise(err)
	}

	ipInfoRaw, err := controller.GetIPInfo(r.Context(), addressOnly)
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

	if info.Bogon {
		http.Error(w, "bogon IP", http.StatusForbidden)
		return
	}

	myInfo, err := myinfo.FromIPInfo(info)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	isIPV6 := strings.Contains(addressOnly, ":")

	var landmarksAndRTTs []landmarks.LandmarkAndRTT

	if isIPV6 {
		landmarksAndRTTs = landmarks.CurrentLandmarks.V6.ExpectedRTTSVerbose(*myInfo.Location.Coordinates, 5)
	} else {
		landmarksAndRTTs = landmarks.CurrentLandmarks.V4.ExpectedRTTSVerbose(*myInfo.Location.Coordinates, 5)
	}

	respStruct := response{
		Info:               myInfo,
		ExpectedRTTs:       landmarksAndRTTs,
		ConnectedToNetwork: model.IsAddressConnectedToNetwork(r.Context(), addressOnly),
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(respStruct)

}
