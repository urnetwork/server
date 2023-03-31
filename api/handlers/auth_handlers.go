package handlers

import (
	"context"
	"net/http"
	"encoding/json"

	"bringyour.com/bringyour"
	"github.com/jackc/pgx/v5/pgxpool"
)





func AuthLogin(w http.ResponseWriter, r *http.Request) {
	// userAuth, authJwtType, authJwt, authAllowed=[], error={suggestedUserAuth, message}, network:{byJwt}
	router.WrapWithJson(model.AuthLogin, w, r)

	// var login model.AuthLogin
	// err := json.NewDecoder(req.Body).Decode(&login)
    // if err != nil {
    //     http.Error(w, err.Error(), http.StatusBadRequest)
    //     return
    // }

	// loginResult, err := model.AuthLogin(login)
	// if err != nil {
	// 	http.Error(w, err.Error(), http.StatusInternalServerError)
    //     return
	// }

	// responseJson, err := json.Marshal(loginResult)
    // if err != nil {
    //     http.Error(w, err.Error(), http.StatusInternalServerError)
    //     return
    // }
    // w.Header().Set("Content-Type", "application/json")
    // w.Write(responseJson)
}





func AuthLoginWithPassword(w http.ResponseWriter, r *http.Request) {
	// userAuth, validationRequired={userAuth}, network={byJwt, name}, error={message}
	router.WrapWithJson(controller.AuthLoginWithPassword, w, r)

	// var loginWithPassword model.AuthLoginWithPassword
	// err := json.NewDecoder(req.Body).Decode(&loginWithPassword)
    // if err != nil {
    //     http.Error(w, err.Error(), http.StatusBadRequest)
    //     return
    // }

	// loginWithPasswordResult, err := model.AuthLogin(loginWithPassword)
	// if err != nil {
	// 	http.Error(w, err.Error(), http.StatusInternalServerError)
    //     return
	// }

	// responseJson, err := json.Marshal(loginWithPasswordResult)
    // if err != nil {
    //     http.Error(w, err.Error(), http.StatusInternalServerError)
    //     return
    // }
    // w.Header().Set("Content-Type", "application/json")
    // w.Write(responseJson)
}





func AuthValidate(w http.ResponseWriter, r *http.Request) {
	/*
   userAuth: userAuth,
   validateCode: validateCode
    */
    // network: {byJwt}

    router.WrapWithJson(model.AuthValidate, w, r)
}




func AuthValidateSend(w http.ResponseWriter, r *http.Request) {
	/*
	userAuth: userAuth
	*/

	router.WrapWithJson(controller.AuthValidateSend, w, r)
}


func AuthPasswordReset(w http.ResponseWriter, r *http.Request) {
	/*
	userAuth: userAuth
	*/

	router.WrapWithJson(controller.AuthPasswordReset, w, r)
}




func AuthPasswordSet(w http.ResponseWriter, r *http.Request) {
	/*
	resetCode: resetCode,
    password: password
    */
    router.WrapWithJson(controller.AuthPasswordSet, w, r)
}


func AuthNetworkCheck(w http.ResponseWriter, r *http.Request) {
	/*
	networkName: networkName
	*/
	router.WrapWithJson(model.NetworkCheck, w, r)
}


func AuthNetworkCreate(w http.ResponseWriter, r *http.Request) {
	// // userName, userAuth, authJwt, password, networkName, terms,   network: {byJwt, name}, validationRequired={userAuth},

	// fixme
	router.WrapWithJson(controller.NetworkCreate, w, r)
}

