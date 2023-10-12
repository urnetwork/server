package router

import (
	"net/http"
	"encoding/json"
	"reflect"
	"strings"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/jwt"
)




// FIXME
// func WrapWithJsonNoArgs() {
	
// }


// func Partial(
// 	ctx context.Context,
// 	impl func(context.Context, http.ResponseWriter, *http.Request),
// ) func(http.ResponseWriter, *http.Request) {
// 	return func(w http.ResponseWriter, req *http.Request) {
// 		impl(ctx, w, req)
// 	}
// }


// wraps an implementation function using json in/out
func WrapWithJson[T any, R any](
	impl func(T, *session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
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
	    // var debugInputJson []byte
	    // debugInputJson, err = json.Marshal(input)
	    // if err == nil {
	    //     bringyour.Logger().Printf("Decoded input %s\n", string(debugInputJson))
	    // }
	// }}


    session := session.NewClientSessionFromRequest(req)

    // (legacy) look for AuthArgs
    // TODO deprecate when all clients migrate to `Authorization: Bearer`
    if session.ByJwt == nil {
	    inputMeta := reflect.ValueOf(&input).Elem()
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


func WrapWithJsonIgnoreSession[T any, R any](
	impl func(T)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
	WrapWithJson(
		func (arg T, _ *session.ClientSession)(R, error) {
			return impl(arg)
		},
		w,
		req,
	)
}

