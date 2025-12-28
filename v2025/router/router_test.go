package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/session"
)

func TestRouterBasic(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		NoAuth := func(w http.ResponseWriter, r *http.Request) {
			impl := func(clientSession *session.ClientSession) (map[string]any, error) {
				if clientSession.ByJwt != nil {
					return nil, errors.New("Contains auth.")
				}
				return map[string]any{}, nil
			}
			WrapNoAuth(impl, w, r)
		}

		Auth := func(w http.ResponseWriter, r *http.Request) {
			impl := func(clientSession *session.ClientSession) (map[string]any, error) {
				if clientSession.ByJwt == nil {
					return nil, errors.New("Missing auth.")
				}

				if clientSession.ByJwt.GuestMode {
					return nil, errors.New("Guest Mode not allowed")
				}

				return map[string]any{}, nil
			}
			WrapRequireAuth(impl, w, r)
		}

		AuthNoGuest := func(w http.ResponseWriter, r *http.Request) {
			impl := func(clientSession *session.ClientSession) (map[string]any, error) {
				if clientSession.ByJwt == nil {
					return nil, errors.New("Missing auth.")
				}

				return map[string]any{}, nil
			}
			WrapRequireAuthNoGuest(impl, w, r)
		}

		Client := func(w http.ResponseWriter, r *http.Request) {
			impl := func(clientSession *session.ClientSession) (map[string]any, error) {
				if clientSession.ByJwt == nil {
					return nil, errors.New("Missing auth.")
				}
				if clientSession.ByJwt.ClientId == nil {
					return nil, errors.New("Missing client.")
				}
				return map[string]any{}, nil
			}
			WrapRequireClient(impl, w, r)
		}

		InputNoAuth := func(w http.ResponseWriter, r *http.Request) {
			impl := func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
				if clientSession.ByJwt != nil {
					return nil, errors.New("Contains auth.")
				}
				return map[string]any{}, nil
			}
			WrapWithInputNoAuth(impl, w, r)
		}

		InputAuth := func(w http.ResponseWriter, r *http.Request) {
			impl := func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
				if clientSession.ByJwt == nil {
					return nil, errors.New("Missing auth.")
				}
				return map[string]any{}, nil
			}
			WrapWithInputRequireAuth(impl, w, r)
		}

		InputAuthNoGuest := func(w http.ResponseWriter, r *http.Request) {
			impl := func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
				if clientSession.ByJwt == nil {
					return nil, errors.New("Missing auth.")
				}
				return map[string]any{}, nil
			}
			WrapWithInputRequireAuthNoGuest(impl, w, r)
		}

		InputClient := func(w http.ResponseWriter, r *http.Request) {
			impl := func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
				if clientSession.ByJwt == nil {
					return nil, errors.New("Missing auth.")
				}
				if clientSession.ByJwt.ClientId == nil {
					return nil, errors.New("Missing client.")
				}
				return map[string]any{}, nil
			}
			WrapWithInputRequireClient(impl, w, r)
		}

		routes := []*Route{
			NewRoute("GET", "/noauth", NoAuth),
			NewRoute("GET", "/auth", Auth),
			NewRoute("GET", "/noguest", AuthNoGuest),
			NewRoute("GET", "/client", Client),
			NewRoute("POST", "/inputnoauth", InputNoAuth),
			NewRoute("POST", "/inputauth", InputAuth),
			NewRoute("POST", "/inputauth-no-guest", InputAuthNoGuest),
			NewRoute("POST", "/inputclient", InputClient),
		}

		port := 8080

		routerHandler := NewRouter(ctx, routes)
		go http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)

		select {
		case <-time.After(time.Second):
		}

		networkId := server.NewId()
		userId := server.NewId()
		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			"test",
			false, // guest mode false
			false, // pro is false
		)
		auth := func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", byJwt.Sign()))
		}

		byJwtGuestMode := jwt.NewByJwt(
			networkId,
			userId,
			"test",
			true,  // guest mode true
			false, // pro is false
		)
		authGuestMode := func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", byJwtGuestMode.Sign()))
		}

		deviceId := server.NewId()
		clientId := server.NewId()
		byClientJwt := byJwt.Client(deviceId, clientId)
		authClient := func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", byClientJwt.Sign()))
		}

		var err error

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
			server.NoCustomHeaders,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
			auth,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
			authGuestMode,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
			authGuestMode,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		// users in guest mode should be restricted to authenticated level routes
		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/auth", port),
			authGuestMode,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/auth", port),
			server.NoCustomHeaders,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/noguest", port),
			authGuestMode,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		// authenticated users should be able to access guest level routes
		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/noguest", port),
			auth,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/noguest", port),
			server.NoCustomHeaders,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/client", port),
			authClient,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpGet(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/client", port),
			auth,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputnoauth", port),
			map[string]any{},
			server.NoCustomHeaders,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputnoauth", port),
			map[string]any{},
			auth,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputauth", port),
			map[string]any{},
			auth,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		// should allow guest requests
		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputauth", port),
			map[string]any{},
			authGuestMode,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputauth", port),
			map[string]any{},
			server.NoCustomHeaders,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputauth-no-guest", port),
			map[string]any{},
			authGuestMode,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		// should deny guest requests
		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputauth-no-guest", port),
			map[string]any{},
			auth,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputauth-no-guest", port),
			map[string]any{},
			server.NoCustomHeaders,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputclient", port),
			map[string]any{},
			authClient,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = server.HttpPost(
			ctx,
			fmt.Sprintf("http://127.0.0.1:%d/inputclient", port),
			map[string]any{},
			auth,
			server.HttpResponseRequireStatusOk(server.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

	})
}
