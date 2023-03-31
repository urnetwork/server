package router

import (
	"net/http"
	"encoding/json"
)


// wraps an implementation function using json in/out
func WrapWithJson[T any, R any](impl func(T)(*R, error), w http.ResponseWriter, req *http.Request) {
	var input T

	var err error

	err = json.NewDecoder(req.Body).Decode(&input)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    var result *R
	result, err = impl(input)
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

