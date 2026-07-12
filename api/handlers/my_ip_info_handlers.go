package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/urnetwork/server/api/myipinfo/myinfo"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"

	"github.com/urnetwork/server"
)

type response struct {
	Info               myinfo.MyInfo `json:"info"`
	ConnectedToNetwork bool          `json:"connected_to_network"`
}

func MyIPInfo(w http.ResponseWriter, r *http.Request) {

	clientAddress := r.Header.Get("X-UR-Forwarded-For")

	if clientAddress == "" {
		clientAddress = r.Header.Get("X-Forwarded-For")
	}

	if clientAddress == "" {
		clientAddress = r.RemoteAddr
	}

	clientIp, _, err := server.SplitClientAddress(clientAddress)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	location, connectionLocationScores, err := controller.GetLocationForIp(r.Context(), clientIp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	myInfo, err := myinfo.NewMyInfo(clientIp, location, connectionLocationScores)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respStruct := response{
		Info:               myInfo,
		ConnectedToNetwork: model.IsIpConnectedToNetwork(r.Context(), clientIp),
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(respStruct)

}
