package router

import (
	"encoding/json"
	"net/http"

	"bytes"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/urnetwork/glog/v2025"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/session"
	// "github.com/urnetwork/server/v2025/jwt"
)

// const BanMessage = "This client has been temporarily banned by bandit. support@ur.io"

type ImplFunction[R any] func(*session.ClientSession) (R, error)
type ImplWithInputFunction[T any, R any] func(T, *session.ClientSession) (R, error)
type BodyFormatFunction func(*http.Request) (io.Reader, error)
type FormatFunction[R any] func(result R) (complete bool)

func JsonFormatter[R any](w http.ResponseWriter) FormatFunction[R] {
	formatter := func(result R) bool {
		responseJson, err := json.Marshal(result)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return true
		}

		glog.V(2).Infof("[h]response (%T): %s\n", result, responseJson)
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

	// if server.Banned(session.ClientIpPort()) {
	// 	http.Error(w, BanMessage, http.StatusForbidden)
	// 	return
	// }

	// server.Logger().Printf("Handling %s\n", impl)
	result, err := impl(session)
	if err != nil {
		if !RaiseHttpError(err, w) {
			glog.Infof("[h]impl error: %s\n", err)
		}
		return
	}

	for _, formatter := range formatters {
		if complete := formatter(result); complete {
			return
		}
	}

	JsonFormatter[R](w)(result)
}

func implName[R any](impl ImplFunction[R]) string {
	name := runtime.FuncForPC(reflect.ValueOf(impl).Pointer()).Name()
	// remove all /vXXXX paths in the canonical module
	name = regexp.MustCompile("/v\\d+").ReplaceAllString(name, "")
	return name
}

// guarantees NetworkId+UserId
// denies guest mode requests
func WrapRequireAuthNoGuest[R any](
	impl ImplFunction[R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrap(
		func(session *session.ClientSession) (R, error) {
			if err := session.Auth(req); err != nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			if session.ByJwt.GuestMode {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			r, err := impl(session)
			if err != nil {
				// wrap the error to tag the impl
				err = fmt.Errorf("[%s]%s", implName(impl), err)
			}
			return r, err
		},
		w,
		req,
		formatters...,
	)
}

// allow guest mode, or authenticated requests
func WrapRequireAuth[R any](
	impl ImplFunction[R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrap(
		func(session *session.ClientSession) (R, error) {
			if err := session.Auth(req); err != nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			r, err := impl(session)
			if err != nil {
				// wrap the error to tag the impl
				err = fmt.Errorf("[%s]%s", implName(impl), err)
			}
			return r, err
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
		func(session *session.ClientSession) (R, error) {
			if err := session.Auth(req); err != nil || session.ByJwt.ClientId == nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}
			r, err := impl(session)
			if err != nil {
				// wrap the error to tag the impl
				err = fmt.Errorf("[%s]%s", implName(impl), err)
			}
			return r, err
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
		func(session *session.ClientSession) (R, error) {
			r, err := impl(session)
			if err != nil {
				// wrap the error to tag the impl
				err = fmt.Errorf("[%s]%s", implName(impl), err)
			}
			return r, err
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
	session, err := session.NewClientSessionFromRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// if server.Banned(session.ClientIpPort()) {
	// 	http.Error(w, BanMessage, http.StatusForbidden)
	// 	return
	// }

	body, err := bodyFormatter(req)
	if err != nil {
		glog.Infof("[h]request body formatter error %s\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var input T

	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		glog.Infof("[h]request read error %s\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
	}

	glog.V(2).Infof("[h]request (%T): %s\n", input, strings.ReplaceAll(string(bodyBytes), "\n", ""))

	err = json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&input)
	if err != nil {
		glog.Infof("[h]request decoding error %s\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// server.Logger().Printf("Handling %s\n", impl)
	result, err := impl(input, session)
	if err != nil {
		custom := map[string]any{
			"headers": req.Header,
		}
		if !RaiseHttpError(err, w) {
			glog.Infof("[h]request impl error (%T): %s\n", input, server.ErrorJsonWithCustomNoStack(err, custom))
		}
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
// denies guest mode requests
func WrapWithInputRequireAuthNoGuest[T any, R any](
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	WrapWithInputBodyFormatterRequireAuthNoGuest(
		RequestBodyFormatter,
		impl,
		w,
		req,
		formatters...,
	)
}

func WrapWithInputBodyFormatterRequireAuthNoGuest[T any, R any](
	bodyFormatter BodyFormatFunction,
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrapWithInput(
		bodyFormatter,
		func(arg T, session *session.ClientSession) (R, error) {
			if err := session.Auth(req); err != nil {
				var empty R
				return empty, fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
			}

			if session.ByJwt.GuestMode {
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
		func(arg T, session *session.ClientSession) (R, error) {
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
// denies requests from guest mode
func WrapWithInputBodyFormatterRequireClient[T any, R any](
	bodyFormatter BodyFormatFunction,
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrapWithInput(
		bodyFormatter,
		func(arg T, session *session.ClientSession) (R, error) {
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
		func(arg T, session *session.ClientSession) (R, error) {
			return impl(arg, session)
		},
		w,
		req,
		formatters...,
	)
}

func RaiseHttpError(err error, w http.ResponseWriter) (statusError bool) {
	statusCode := http.StatusInternalServerError
	message := err.Error()

	// error messages that start with <number><space>
	// have the number peeled off and converted to the status code
	codeRe := regexp.MustCompile("^(\\d+)\\s+(.*)$")
	if groups := codeRe.FindStringSubmatch(message); groups != nil {
		statusCode, _ = strconv.Atoi(groups[1])
		message = groups[2]
		statusError = true
	}

	http.Error(w, message, statusCode)
	return
}

func RequestBodyFormatter(req *http.Request) (io.Reader, error) {
	return req.Body, nil
}
