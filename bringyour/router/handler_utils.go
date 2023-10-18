package router

import (
	"net/http"
	"encoding/json"
	// "reflect"
	// "strings"
	"fmt"
	"strconv"
	"regexp"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	// "bringyour.com/bringyour/jwt"
)


func wrap[R any](
	impl func(*session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
    session, err := session.NewClientSessionFromRequest(req)
    if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
        return
	}

	bringyour.Logger().Printf("Handling %s\n", impl)
    result, err := impl(session)
	if err != nil {
		raiseHttpError(err, w)
        return
	}

	responseJson, err := json.Marshal(result)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    bringyour.Logger().Printf("Response %s\n", responseJson)
    w.Header().Set("Content-Type", "application/json")
    w.Write(responseJson)
}


// guarantees NetworkId+UserId
func WrapRequireAuth[R any](
	impl func(*session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
	wrap(
		func (session *session.ClientSession)(R, error) {
			if session.ByJwt == nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			return impl(session)
		},
		w,
		req,
	)
}


// guarantees NetworkId+UserId+ClientId
func WrapRequireClient[R any](
	impl func(*session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
	wrap(
		func (session *session.ClientSession)(R, error) {
			if session.ByJwt == nil || session.ByJwt.ClientId == nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			return impl(session)
		},
		w,
		req,
	)
}


func WrapNoAuth[R any](
	impl func(*session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
	wrap(
		func (session *session.ClientSession)(R, error) {
			// even if there was an auth value, remove it
			session.ByJwt = nil
			return impl(session)
		},
		w,
		req,
	)
}


// wraps an implementation function using json in/out
func wrapWithInput[T any, R any](
	impl func(T, *session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
	var input T

	bringyour.Logger().Printf("Decoding request\n")
	err := json.NewDecoder(req.Body).Decode(&input)
    if err != nil {
    	bringyour.Logger().Printf("Decoding error %s\n", err)
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    session, err := session.NewClientSessionFromRequest(req)
    if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
        return
	}

	bringyour.Logger().Printf("Handling %s\n", impl)
    result, err := impl(input, session)
	if err != nil {
		raiseHttpError(err, w)
        return
	}

	responseJson, err := json.Marshal(result)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    bringyour.Logger().Printf("Response %s\n", responseJson)
    w.Header().Set("Content-Type", "application/json")
    w.Write(responseJson)
}


// guarantees NetworkId+UserId
func WrapWithInputRequireAuth[T any, R any](
	impl func(T, *session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
	wrapWithInput(
		func (arg T, session *session.ClientSession)(R, error) {
			if session.ByJwt == nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			return impl(arg, session)
		},
		w,
		req,
	)
}


// guarantees NetworkId+UserId+ClientId
func WrapWithInputRequireClient[T any, R any](
	impl func(T, *session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
	wrapWithInput(
		func (arg T, session *session.ClientSession)(R, error) {
			if session.ByJwt == nil || session.ByJwt.ClientId == nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			return impl(arg, session)
		},
		w,
		req,
	)
}


func WrapWithInputNoAuth[T any, R any](
	impl func(T, *session.ClientSession)(R, error),
	w http.ResponseWriter,
	req *http.Request,
) {
	wrapWithInput(
		func (arg T, session *session.ClientSession)(R, error) {
			// even if there was an auth value, remove it
			session.ByJwt = nil
			return impl(arg, session)
		},
		w,
		req,
	)
}


func raiseHttpError(err error, w http.ResponseWriter) {
	statusCode := http.StatusInternalServerError
	message := err.Error()

	codeRe := regexp.MustCompile("^(\\d+)\\s+(.*)$")
	if groups := codeRe.FindStringSubmatch(message); groups != nil {
		statusCode, _ = strconv.Atoi(groups[1])
		message = groups[2]
	}

	http.Error(w, message, statusCode)
}

