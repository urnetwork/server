package router

import (
	"net/http"
	"encoding/json"
	"reflect"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/jwt"
)


// wraps an implementation function using json in/out
func WrapWithJson[T any, R any](impl func(T, *session.ClientSession)(R, error), w http.ResponseWriter, req *http.Request) {
	var input T

	var err error

	bringyour.Logger().Printf("Decoding request\n")
	err = json.NewDecoder(req.Body).Decode(&input)
    if err != nil {
    	bringyour.Logger().Printf("Decoding error %s\n", err)
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }


    // debugging
    // {{ fixme use LogJson
	    var debugInputJson []byte
	    debugInputJson, err = json.Marshal(input)
	    if err == nil {
	        bringyour.Logger().Printf("Decoded input %s\n", string(debugInputJson))
	    }
	// }}


    session := session.NewClientSessionFromRequest(req)

    // look for AuthArgs
    inputMeta := reflect.ValueOf(&input).Elem()
    // FIXME should this be passed as BEARER auth?
    // FIXME allow ByJwt as a legacy
    byJwtField := reflect.Indirect(inputMeta).FieldByName("ByJwt")
	if byJwtField != (reflect.Value{}) {
		byJwt, err := jwt.ParseByJwt(byJwtField.String())
    	if err != nil {
    		http.Error(w, err.Error(), http.StatusInternalServerError)
        	return
    	}
    	bringyour.Logger().Printf("Authed as %s (%s %s)\n", byJwt.UserId, byJwt.NetworkName, byJwt.NetworkId)
    	session.ByJwt = byJwt
	}

	bringyour.Logger().Printf("Handling %s\n", impl)
    var result R
	result, err = impl(input, session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
        return
	}

	var responseJson []byte
	responseJson, err = json.Marshal(result)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    bringyour.Logger().Printf("Response %s\n", responseJson)
    w.Header().Set("Content-Type", "application/json")
    w.Write(responseJson)
}


func WrapWithJsonIgnoreSession[T any, R any](impl func(T)(R, error), w http.ResponseWriter, req *http.Request) {
	WrapWithJson(func (arg T, _ *session.ClientSession)(R, error) {
		return impl(arg)
	}, w, req)
}

