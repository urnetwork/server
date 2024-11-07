package handlers

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/model"
	"bringyour.com/service/api/myipinfo/landmarks"
	"bringyour.com/service/api/myipinfo/myinfo"
	"github.com/ipinfo/go/v2/ipinfo"
)

func MyIPInfoOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

type response struct {
	Info               myinfo.MyInfo              `json:"info"`
	ExpectedRTTs       []landmarks.LandmarkAndRTT `json:"landmarks"`
	ConnectedToNetwork bool                       `json:"connected_to_network"`
}

func MyIPInfo(w http.ResponseWriter, r *http.Request) {
	remoteIPPort := r.Header.Get("X-Forwarded-For")
	if remoteIPPort == "" {
		remoteIPPort = r.RemoteAddr
	}

	remoteIP, _, err := net.SplitHostPort(remoteIPPort)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	ipInfoRaw, err := controller.GetIPInfo(r.Context(), remoteIP)
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

	isIPV6 := strings.Contains(remoteIP, ":")

	var landmarksAndRTTs []landmarks.LandmarkAndRTT

	if isIPV6 {
		landmarksAndRTTs = landmarks.CurrentLandmarks.V6.ExpectedRTTSVerbose(*myInfo.Location.Coordinates, 5)
	} else {
		landmarksAndRTTs = landmarks.CurrentLandmarks.V4.ExpectedRTTSVerbose(*myInfo.Location.Coordinates, 5)
	}

	respStruct := response{
		Info:               myInfo,
		ExpectedRTTs:       landmarksAndRTTs,
		ConnectedToNetwork: model.IsAddressConnectedToNetwork(r.Context(), remoteIP),
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(respStruct)

}
