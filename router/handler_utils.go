package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"fmt"
	"io"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	//
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
	// Only the first line can select a canonical error status. Joined causes
	// belong to the message tail, not to the tag/status parsing grammar.
	httpErrorCodeRegex = regexp.MustCompile(`^((?:\[[^\]\r\n]*\])*)[ \t]*([45][0-9]{2})[ \t]+(?s:(.*))$`)
)

// This matches the existing public load-balancer cap. Enforcing it again at
// the application boundary prevents a direct-backend request from bypassing
// the public limit. Routes with smaller payloads should add a tighter cap.
const MaxJsonRequestBytes int64 = 16 * 1024 * 1024

// const BanMessage = "This client has been temporarily banned by bandit. support@ur.io"

// The fixed answer to a failure the caller cannot act on, as a panic gets.
const UnexpectedErrorMessage = "Error. Please email support@ur.io for help."

// The fixed answers to a request body that does not decode, and to one that
// cannot be read. The decoder's and the reader's own errors stay in the log.
const (
	InvalidRequestBodyMessage    = "Invalid request body."
	UnreadableRequestBodyMessage = "The request body could not be read."
)

type ImplFunction[R any] func(*session.ClientSession) (R, error)
type ImplWithInputFunction[T any, R any] func(T, *session.ClientSession) (R, error)
type BodyFormatFunction func(*session.ClientSession, *http.Request) (io.Reader, error)
type FormatFunction[R any] func(*session.ClientSession, R) (complete bool)

// writeJsonResponse marshals result as JSON and writes it. This is the default
// response path; calling it directly avoids building a closure per response
// (unlike going through JsonFormatter). The verbose log is guarded so the `%T`
// boxing and formatting do not run when V(2) is off.
func writeJsonResponse[R any](w http.ResponseWriter, result R) {
	responseJson, err := json.Marshal(result)
	if err != nil {
		glog.Infof("[h]response encoding error (%T): %s\n", result, err)
		http.Error(w, UnexpectedErrorMessage, http.StatusInternalServerError)
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
	return func(_ *session.ClientSession, result R) bool {
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
	clientSession, err := session.NewClientSessionFromRequest(req)
	if err != nil {
		glog.Infof("[h]client session error: %s\n", err)
		http.Error(w, UnexpectedErrorMessage, http.StatusInternalServerError)
		return
	}

	// if server.Banned(session.ClientIpPort()) {
	// 	http.Error(w, BanMessage, http.StatusForbidden)
	// 	return
	// }

	// server.Logger().Printf("Handling %s\n", impl)
	result, err := impl(clientSession)
	if err != nil {
		if !RaiseHttpError(err, w) {
			glog.Infof("[h]impl error: %s\n", err)
		}
		return
	}

	for _, formatter := range formatters {
		if complete := formatter(clientSession, result); complete {
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
	// A generic impl's name carries "[...]", whose "]" would end the tag early:
	// no status after it could be read, and the tagged text would be sent.
	name = strings.ReplaceAll(name, "[...]", "")
	return name
}

// Dependency failure is not a credential rejection. Keep the sentinel through
// the wrapper so RaiseHttpError can emit a fixed JSON response without causes.
func authenticationHttpError(err error) error {
	if errors.Is(err, session.ErrSessionRevoked) {
		return &session.SessionError{Code: "session_revoked", Status: 401}
	}
	if errors.Is(err, session.ErrAuthUnavailable) {
		return err
	}
	return fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)
}

// allow guest mode, or authenticated requests
func WrapRequireAuth[R any](
	impl ImplFunction[R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrap(
		func(clientSession *session.ClientSession) (R, error) {
			if err := clientSession.Auth(req); err != nil {
				var empty R
				return empty, authenticationHttpError(err)
			}
			r, err := impl(clientSession)
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
		func(clientSession *session.ClientSession) (R, error) {
			if err := clientSession.Auth(req); err != nil || clientSession.ByJwt.ClientId == nil {
				var empty R
				return empty, authenticationHttpError(err)
			}
			r, err := impl(clientSession)
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
		func(clientSession *session.ClientSession) (R, error) {
			r, err := impl(clientSession)
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
	clientSession, err := session.NewClientSessionFromRequest(req)
	if err != nil {
		glog.Infof("[h]client session error: %s\n", err)
		http.Error(w, UnexpectedErrorMessage, http.StatusInternalServerError)
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

	body, err := bodyFormatter(clientSession, req)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		glog.Infof("[h]request body formatter error %s\n", err)
		http.Error(w, InvalidRequestBodyMessage, http.StatusBadRequest)
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
		http.Error(w, UnreadableRequestBodyMessage, http.StatusBadRequest)
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
		http.Error(w, InvalidRequestBodyMessage, http.StatusBadRequest)
		return
	}

	// json.Unmarshal accepts the JSON literal null for pointer, map, slice, and
	// interface inputs. Passing that typed nil to an implementation makes an
	// otherwise client-controlled malformed request look like an application
	// nil-pointer panic. No input wrapper has a meaningful null-body contract:
	// require a concrete JSON value before dispatching any implementation.
	if bytes.Equal(bytes.TrimSpace(bodyBytes), []byte("null")) {
		http.Error(w, "request body must not be null", http.StatusBadRequest)
		return
	}

	// server.Logger().Printf("Handling %s\n", impl)
	result, err := impl(input, clientSession)
	if err != nil {
		custom := map[string]any{
			"headers": server.SafeHttpHeadersForLog(req.Header),
		}
		if !RaiseHttpError(err, w) {
			glog.Infof("[h]request impl error (%T): %s\n", input, server.ErrorJsonWithCustomNoStack(err, custom))
		}
		return
	}

	advanceControlHttpPhase(req, controlHttpResponse)
	for _, formatter := range formatters {
		if complete := formatter(clientSession, result); complete {
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
		func(arg T, clientSession *session.ClientSession) (R, error) {
			if err := clientSession.Auth(req); err != nil {
				var empty R
				return empty, authenticationHttpError(err)
			}
			return impl(arg, clientSession)
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
		func(arg T, clientSession *session.ClientSession) (R, error) {
			advanceControlHttpPhase(req, controlHttpAuthenticate)
			if err := clientSession.Auth(req); err != nil || clientSession.ByJwt.ClientId == nil {
				var empty R
				return empty, authenticationHttpError(err)
			}
			advanceControlHttpPhase(req, controlHttpController)
			return impl(arg, clientSession)
		},
		w,
		req,
		formatters...,
	)
}

// WrapWithInputRequireClient, except that a client credential's request may be
// answered after the checks that need no client state query and before that
// query (session.ClientSession.AuthUnlessAnswered). prepare decides from the
// credential's verified client id and the input whether to answer, and reads
// nothing; reply then produces the answer. Both must grant nothing: only a
// refusal already recorded for that client may be reported again. Every other
// request authenticates and runs impl exactly as WrapWithInputRequireClient.
func WrapWithInputRequireClientUnlessAnswered[T any, A any, R any](
	prepare func(context.Context, server.Id, T) (A, bool),
	reply func(context.Context, A) (R, error),
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrapWithInput(
		RequestBodyFormatter,
		func(arg T, clientSession *session.ClientSession) (R, error) {
			advanceControlHttpPhase(req, controlHttpAuthenticate)
			var prepared A
			answered, err := clientSession.AuthUnlessAnswered(req, func(ctx context.Context, clientId server.Id) bool {
				var ok bool
				prepared, ok = prepare(ctx, clientId, arg)
				return ok
			})
			if err != nil || !answered && clientSession.ByJwt.ClientId == nil {
				var empty R
				return empty, authenticationHttpError(err)
			}
			advanceControlHttpPhase(req, controlHttpController)
			if answered {
				return reply(clientSession.Ctx, prepared)
			}
			return impl(arg, clientSession)
		},
		w,
		req,
		formatters...,
	)
}

// WrapWithInputOptionalAuth serves signed-out callers and, when the request
// carries an Authorization header, authenticates it exactly as RequireAuth
// would (a bad token is a 401, not a silently signed-out request). The impl
// checks session.ByJwt for the optional caller identity.
func WrapWithInputOptionalAuth[T any, R any](
	impl ImplWithInputFunction[T, R],
	w http.ResponseWriter,
	req *http.Request,
	formatters ...FormatFunction[R],
) {
	wrapWithInput(
		RequestBodyFormatter,
		func(arg T, clientSession *session.ClientSession) (R, error) {
			if req.Header.Get("Authorization") != "" {
				if err := clientSession.Auth(req); err != nil {
					var empty R
					return empty, authenticationHttpError(err)
				}
			}
			return impl(arg, clientSession)
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
		func(arg T, clientSession *session.ClientSession) (R, error) {
			return impl(arg, clientSession)
		},
		w,
		req,
		formatters...,
	)
}

// Answers err with the status and message it declares by the "<code> message"
// convention. An error that declares none is an internal failure, such as
// storage, transport or a dependency's url: it is answered with a fixed 500
// and its text stays in the caller's log.
func RaiseHttpError(err error, w http.ResponseWriter) (statusError bool) {
	if errors.Is(err, session.ErrAuthUnavailable) {
		// Only a separately declared safe unavailable body can override the
		// fixed dependency response. Arbitrary error causes remain private.
		var unavailableBody interface{ HttpUnavailableResultBody() any }
		if errors.As(err, &unavailableBody) {
			if body, marshalErr := json.Marshal(unavailableBody.HttpUnavailableResultBody()); marshalErr == nil {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(append(body, '\n'))
				return true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("{\"error\":\"Authentication temporarily unavailable.\"}\n"))
		return true
	}

	statusCode := http.StatusInternalServerError
	message := UnexpectedErrorMessage

	// Error messages that start with <400..599><horizontal space>, optionally
	// preceded by "[tag]" prefixes, have the number converted to the
	// status code. The tags are dropped from the client-facing message.
	if groups := httpErrorCodeRegex.FindStringSubmatch(err.Error()); groups != nil {
		statusCode, _ = strconv.Atoi(groups[2])
		message = groups[3]
		statusError = true
	}

	// A rate limit knows when the caller may try again; without the header the
	// client can only guess, and guessing early costs it more budget. Read
	// through a one-method interface so the direction of the import stays as it
	// is: model does not import the router.
	var retryAfter interface{ RetryAfterSeconds() int }
	if statusError && errors.As(err, &retryAfter) {
		if seconds := retryAfter.RetryAfterSeconds(); 0 < seconds {
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
	}

	// A refusal that carries its result (network create's coded refusal for a
	// client that asked for `result_errors`) keeps its 4xx status, so route
	// metrics still count it as refused, and answers with the result as its
	// JSON body, where that client reads the code. Read through a one-method
	// interface like the retry hint above; a body without a 4xx status adds
	// nothing.
	var resultBody interface{ HttpErrorResultBody() any }
	if statusError && statusCode/100 == 4 && errors.As(err, &resultBody) {
		if body, marshalErr := json.Marshal(resultBody.HttpErrorResultBody()); marshalErr == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(statusCode)
			_, _ = w.Write(append(body, '\n'))
			return
		}
	}

	http.Error(w, message, statusCode)
	return
}

// The answer to an impl error that declares no status: the router's answer to
// a done error that escapes a route.
var errUndeclaredImpl = fmt.Errorf("%d Service temporarily unavailable.", http.StatusServiceUnavailable)

// Reports whether RaiseHttpError answers err with a status err chose, by the
// "<code> message" convention or as an authentication outage.
func DeclaresHttpStatus(err error) bool {
	return errors.Is(err, session.ErrAuthUnavailable) || httpErrorCodeRegex.MatchString(err.Error())
}

// An impl error that declares no status is an unexpected internal failure,
// such as storage, signing or a canceled dependency, and RaiseHttpError would
// answer it with a 500. Here the text stays in the server log and the caller
// is answered 503 with a fixed message, as the provider work routes answer
// theirs. A declared status passes unchanged, with its message, retry
// hint and result body.
//
// A raise inside impl is returned the same way (server.HandleError logs it), so
// a storage failure raised from a model call is a 503 and a refusal declared
// and raised there, such as a 409, keeps its own answer.
func PrivateErrors[R any](impl ImplFunction[R]) ImplFunction[R] {
	return func(clientSession *session.ClientSession) (result R, err error) {
		server.HandleError(func() {
			result, err = impl(clientSession)
		}, func(cause error) { err = cause })
		if err == nil || DeclaresHttpStatus(err) {
			return result, err
		}
		glog.Infof("[h]impl error: [%s]%s\n", implName(impl), err)
		var empty R
		return empty, errUndeclaredImpl
	}
}

// The same, for an impl that takes the decoded request body.
func PrivateErrorsWithInput[T any, R any](impl ImplWithInputFunction[T, R]) ImplWithInputFunction[T, R] {
	return func(input T, clientSession *session.ClientSession) (result R, err error) {
		server.HandleError(func() {
			result, err = impl(input, clientSession)
		}, func(cause error) { err = cause })
		if err == nil || DeclaresHttpStatus(err) {
			return result, err
		}
		glog.Infof("[h]request impl error (%T): %s\n", input, err)
		var empty R
		return empty, errUndeclaredImpl
	}
}

// A declared answer whose cause stays private. RaiseHttpError sends only the
// status and message; the cause, which can name a socket, a path, a store or
// chain endpoint, goes to the server log here. errors.Is and errors.As still
// reach the cause.
func PrivateCause(status int, message string, cause error) error {
	if cause != nil {
		glog.Infof("[h]%d %s: %s\n", status, message, cause)
	}
	return &privateCauseError{message: fmt.Sprintf("%d %s", status, message), cause: cause}
}

// The declared "<code> message" and the cause it does not print.
type privateCauseError struct {
	message string
	cause   error
}

func (self *privateCauseError) Error() string {
	return self.message
}

func (self *privateCauseError) Unwrap() error {
	return self.cause
}

func RequestBodyFormatter(_ *session.ClientSession, req *http.Request) (io.Reader, error) {
	return req.Body, nil
}
