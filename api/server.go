package main

import (
	"fmt"
	"log"
	"net/http"
	
	"bringyour.com/api/handlers"
	"bringyour.com/bringyour/router"
)



var routes = []*router.Route{
	router.NewRoute("GET", "/stats/last-90", handlers.StatsLast90),
	router.NewRoute("POST", "/auth/login", handlers.AuthLogin),
	router.NewRoute("POST", "/auth/login-with-password", handlers.AuthLoginWithPassword),
	router.NewRoute("POST", "/auth/validate", handlers.AuthValidate),
	router.NewRoute("POST", "/auth/validate-send", handlers.AuthValidateSend),
	router.NewRoute("POST", "/auth/password-reset", handlers.AuthPasswordReset),
	router.NewRoute("POST", "/auth/password-set", handlers.AuthPasswordSet),
	router.NewRoute("POST", "/auth/network-check", handlers.AuthNetworkCheck),
	router.NewRoute("POST", "/auth/network-create", handlers.AuthNetworkCreate),
	router.NewRoute("POST", "/preferences/set-preferences", handlers.PreferencesSet),
	router.NewRoute("POST", "/feedback/send-feedback", handlers.FeedbackSend),
}


func main() {
	port := 8080
	routerHandler := router.NewRouter(routes)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler))
}

