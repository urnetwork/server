package router

import (
	"net/http"
	"encoding/json"
	// "reflect"
	"strings"
	"fmt"
	"strconv"
	"regexp"
	"io"
	"bytes"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	// "bringyour.com/bringyour/jwt"
)


type ImplFunction[R any] func(*session.ClientSession)(R, error)
type ImplWithInputFunction[T any, R any] func(T, *session.ClientSession)(R, error)
type BodyFormatFunction func(*http.Request)(io.Reader, error)
type FormatFunction[R any] func(result R)(complete bool)


func JsonFormatter[R any](w http.ResponseWriter) FormatFunction[R] {
	formatter := func(result R)(bool) {
		responseJson, err := json.Marshal(result)
	    if err != nil {
	        http.Error(w, err.Error(), http.StatusInternalServerError)
	        return true
	    }

	    bringyour.Logger().Printf("Response (%T): %s\n", result, responseJson)
	    w.Header().Set("Content-Type", "application/json")
	    w.Write(responseJson)
	    return true
	}
	return formatter
}


func wrap[R any](
	impl ImplFunction[R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
    session, err := session.NewClientSessionFromRequest(req)
    if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
        return
	}

	// bringyour.Logger().Printf("Handling %s\n", impl)
    result, err := impl(session)
	if err != nil {
		bringyour.Logger().Printf("Impl error: %s\n", err)
		RaiseHttpError(err, w)
        return
	}

	for _, formatter := range formatters {
		if complete := formatter(result); complete {
			return
		}
	}

	JsonFormatter[R](w)(result)
}


// guarantees NetworkId+UserId
func WrapRequireAuth[R any](
	impl ImplFunction[R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrap(
		func (session *session.ClientSession)(R, error) {
			if err := session.Auth(req); err != nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			return impl(session)
		},
		w,
		req,
		formatters...,
	)
}


// guarantees NetworkId+UserId+ClientId
func WrapRequireClient[R any](
	impl ImplFunction[R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrap(
		func (session *session.ClientSession)(R, error) {
			if err := session.Auth(req); err != nil || session.ByJwt.ClientId == nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			return impl(session)
		},
		w,
		req,
		formatters...,
	)
}


func WrapNoAuth[R any](
	impl ImplFunction[R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrap(
		func (session *session.ClientSession)(R, error) {
			return impl(session)
		},
		w,
		req,
		formatters...,
	)
}


// wraps an implementation function using json in/out
func wrapWithInput[T any, R any](
	bodyFormatter BodyFormatFunction,
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	body, err := bodyFormatter(req)
	if err != nil {
    	bringyour.Logger().Printf("Request body formatter error %s\n", err)
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

	var input T

	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		bringyour.Logger().Printf("Request read error %s\n", err)
        http.Error(w, err.Error(), http.StatusBadRequest)
	}

	bringyour.Logger().Printf("Request (%T): %s\n", input, strings.ReplaceAll(string(bodyBytes), "\n", ""))

	err = json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&input)
    if err != nil {
    	bringyour.Logger().Printf("Request decoding error %s\n", err)
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    session, err := session.NewClientSessionFromRequest(req)
    if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
        return
	}

	// bringyour.Logger().Printf("Handling %s\n", impl)
    result, err := impl(input, session)
	if err != nil {
		bringyour.Logger().Printf("Request impl error: %s\n", err)
		RaiseHttpError(err, w)
        return
	}

	for _, formatter := range formatters {
		if complete := formatter(result); complete {
			return
		}
	}

	JsonFormatter[R](w)(result)
}


// guarantees NetworkId+UserId
func WrapWithInputRequireAuth[T any, R any](
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	WrapWithInputBodyFormatterRequireAuth(
		RequestBodyFormatter,
		impl,
		w,
		req,
		formatters...,
	)
}


func WrapWithInputBodyFormatterRequireAuth[T any, R any](
	bodyFormatter BodyFormatFunction,
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrapWithInput(
		bodyFormatter,
		func (arg T, session *session.ClientSession)(R, error) {
			if err := session.Auth(req); err != nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			return impl(arg, session)
		},
		w,
		req,
		formatters...,
	)
}


func WrapWithInputRequireClient[T any, R any](
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	WrapWithInputBodyFormatterRequireClient(
		RequestBodyFormatter,
		impl,
		w,
		req,
		formatters...,
	)
}


// guarantees NetworkId+UserId+ClientId
func WrapWithInputBodyFormatterRequireClient[T any, R any](
	bodyFormatter BodyFormatFunction,
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrapWithInput(
		bodyFormatter,
		func (arg T, session *session.ClientSession)(R, error) {
			if err := session.Auth(req); err != nil || session.ByJwt.ClientId == nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			return impl(arg, session)
		},
		w,
		req,
		formatters...,
	)
}


func WrapWithInputNoAuth[T any, R any](
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	WrapWithInputBodyFormatterNoAuth(
		RequestBodyFormatter,
		impl,
		w,
		req,
		formatters...,
	)
}


func WrapWithInputBodyFormatterNoAuth[T any, R any](
	bodyFormatter BodyFormatFunction,
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrapWithInput(
		bodyFormatter,
		func (arg T, session *session.ClientSession)(R, error) {
			return impl(arg, session)
		},
		w,
		req,
		formatters...,
	)
}


func RaiseHttpError(err error, w http.ResponseWriter) {
	statusCode := http.StatusInternalServerError
	message := err.Error()

	// error messages that start with <number><space> 
	// have the number peeled off and converted to the status code
	codeRe := regexp.MustCompile("^(\\d+)\\s+(.*)$")
	if groups := codeRe.FindStringSubmatch(message); groups != nil {
		statusCode, _ = strconv.Atoi(groups[1])
		message = groups[2]
	}

	http.Error(w, message, statusCode)
}


func RequestBodyFormatter(req *http.Request) (io.Reader, error) {
	return req.Body, nil
}

