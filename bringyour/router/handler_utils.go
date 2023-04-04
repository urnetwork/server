package router

import (
	"net/http"
	"encoding/json"

	"bringyour.com/bringyour"
)


// wraps an implementation function using json in/out
func WrapWithJson[T any, R any](impl func(T, *bringyour.ClientSession)(*R, error), w http.ResponseWriter, req *http.Request) {
	var input T

	var err error

	err = json.NewDecoder(req.Body).Decode(&input)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    session := bringyour.NewClientSessionFromRequest(req)

    var result *R
	result, err = impl(input, session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
        return
	}

	responseJson, err := json.Marshal(result)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    w.Write(responseJson)
}


func WrapWithJsonIgnoreSession[T any, R any](impl func(T)(*R, error), w http.ResponseWriter, req *http.Request) {
	WrapWithJson(func (arg T, _ *bringyour.ClientSession)(*R, error) {
		return impl(arg)
	}, w, req)
}
