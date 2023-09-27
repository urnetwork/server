package handlers

import (
	"net/http"

    "bringyour.com/bringyour/model"
	"bringyour.com/bringyour/router"
)



func AuthNetworkClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(model.AuthNetworkClient, w, r)
}


func RemoveNetworkClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJson(model.RemoveNetworkClient, w, r)
}


func NetworkClients(w http.ResponseWriter, r *http.Request) {
	router.WrapWithJsonNoArgs(model.GetNetworkClients, w, r)
}

