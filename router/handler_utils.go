package router

import (
	"encoding/json"
	"errors"
	"net/http"

	"fmt"
	"io"
	"reflect"
	"regexp"
	"runtime"
	"strconv"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	// "github.com/urnetwork/server/jwt"
)

// compiled once at package load; these run on the error path of every request,
// so per-call regexp.MustCompile would be pure waste.
var (
	// strips /vXXXX version path segments from an impl's canonical name
	implNameVersionRegex = regexp.MustCompile("/v\\d+")
	// peels a leading "<code> " off an error message into an http status code.
	// The auth wrappers tag impl errors with one or more "[implName]" prefixes
	// (see WrapRequireAuth), so the code may follow bracketed tags; without
	// peeling them every tagged "%d message" error would surface as a 500.
	httpErrorCodeRegex = regexp.MustCompile("^((?:\\[[^\\]]*\\])*)\\s*(\\d+)\\s+(.*)$")
)

// This matches the existing public load-balancer cap. Enforcing it again at
// the application boundary prevents a direct-backend request from bypassing
// the public limit. Routes with smaller payloads should add a tighter cap.
const MaxJsonRequestBytes int64 = 16 * 1024 * 1024

// const BanMessage = "This client has been temporarily banned by bandit. support@ur.io"

type ImplFunction[R any] func(*session.ClientSession) (R, error)
type ImplWithInputFunction[T any, R any] func(T, *session.ClientSession) (R, error)
type BodyFormatFunction func(*http.Request) (io.Reader, error)
type FormatFunction[R any] func(result R) (complete bool)

// writeJsonResponse marshals result as JSON and writes it. This is the default
// response path; calling it directly avoids building a closure per response
// (unlike going through JsonFormatter). The verbose log is guarded so the `%T`
// boxing and formatting do not run when V(2) is off.
func writeJsonResponse[R any](w http.ResponseWriter, result R) {
	responseJson, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if v := glog.V(2); v {
		v.Infof("[h]response (%T): %d bytes\n", result, len(responseJson))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJson)
}

// JsonFormatter returns a FormatFunction that writes result as JSON. The default
// path uses writeJsonResponse directly; this remains for explicit formatter lists.
func JsonFormatter[R any](w http.ResponseWriter) FormatFunction[R] {
	return func(result R) bool {
		writeJsonResponse(w, result)
		return true
	}
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

	writeJsonResponse(w, result)
}

// tagImplError prefixes an impl error with the impl name, the "[tag]" form
// RaiseHttpError's httpErrorCodeRegex peels back off before the message reaches
// the client.
//
// %w, not %s. RaiseHttpError reads the retry hint off a rate limit with
// errors.As, and %s flattens the error to text, which breaks that chain. The
// client-facing message is byte-identical either way, so the only visible
// effect of %s was a 429 that silently lost its Retry-After -- a defect with no
// symptom at the status code. This is one function rather than three copies so
// that a single test can pin all three wrappers.
func tagImplError[R any](impl ImplFunction[R], err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("[%s]%w", implName(impl), err)
}

func implName[R any](impl ImplFunction[R]) string {
	name := runtime.FuncForPC(reflect.ValueOf(impl).Pointer()).Name()
	// remove all /vXXXX paths in the canonical module
	name = implNameVersionRegex.ReplaceAllString(name, "")
	return name
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
			return r, tagImplError(impl, err)
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
			return r, tagImplError(impl, err)
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
			return r, tagImplError(impl, err)
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

	if req.Body != nil {
		if req.ContentLength > MaxJsonRequestBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, MaxJsonRequestBytes)
	}

	body, err := bodyFormatter(req)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		glog.Infof("[h]request body formatter error %s\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var input T

	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// a truncated or aborted body is client-driven, same as a malformed
		// one below
		if glog.V(1) {
			glog.Infof("[h]request read error %s\n", err)
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if v := glog.V(2); v {
		v.Infof("[h]request (%T): %d bytes\n", input, len(bodyBytes))
	}

	err = json.Unmarshal(bodyBytes, &input)
	if err != nil {
		// a malformed body is client-supplied input: logging it at the
		// default level lets any caller write to the logs at will
		if glog.V(1) {
			glog.Infof("[h]request decoding error %s\n", err)
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// server.Logger().Printf("Handling %s\n", impl)
	result, err := impl(input, session)
	if err != nil {
		custom := map[string]any{
			"headers": server.SafeHttpHeadersForLog(req.Header),
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

	writeJsonResponse(w, result)
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

	// error messages that start with <number><space>, optionally preceded by
	// "[tag]" prefixes, have the number peeled off and converted to the
	// status code. The tags are dropped from the client-facing message.
	if groups := httpErrorCodeRegex.FindStringSubmatch(message); groups != nil {
		statusCode, _ = strconv.Atoi(groups[2])
		message = groups[3]
		statusError = true
	}

	// A rate limit knows when the caller may try again; without the header the
	// client can only guess, and guessing early costs it more budget. Read
	// through a one-method interface so the direction of the import stays as it
	// is: model does not import the router.
	var retryAfter interface{ RetryAfterSeconds() int }
	if errors.As(err, &retryAfter) {
		if seconds := retryAfter.RetryAfterSeconds(); 0 < seconds {
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
	}

	http.Error(w, message, statusCode)
	return
}

func RequestBodyFormatter(req *http.Request) (io.Reader, error) {
	return req.Body, nil
}
