package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/jwt"
	"github.com/urnetwork/server/bringyour/session"
)

func TestRouterBasic(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		cancelCtx, cancel := context.WithCancel(context.Background())
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

		routerHandler := NewRouter(cancelCtx, routes)
		go http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)

		select {
		case <-time.After(time.Second):
		}

		networkId := bringyour.NewId()
		userId := bringyour.NewId()
		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			"test",
			false, // guest mode false
		)
		auth := func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", byJwt.Sign()))
		}

		byJwtGuestMode := jwt.NewByJwt(
			networkId,
			userId,
			"test",
			true, // guest mode true
		)
		authGuestMode := func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", byJwtGuestMode.Sign()))
		}

		deviceId := bringyour.NewId()
		clientId := bringyour.NewId()
		byClientJwt := byJwt.Client(deviceId, clientId)
		authClient := func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", byClientJwt.Sign()))
		}

		var err error

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
			bringyour.NoCustomHeaders,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
			auth,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
			authGuestMode,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
			authGuestMode,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		// users in guest mode should be restricted to authenticated level routes
		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/auth", port),
			authGuestMode,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/auth", port),
			bringyour.NoCustomHeaders,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/noguest", port),
			authGuestMode,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		// authenticated users should be able to access guest level routes
		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/noguest", port),
			auth,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/noguest", port),
			bringyour.NoCustomHeaders,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/client", port),
			authClient,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpGet(
			fmt.Sprintf("http://127.0.0.1:%d/client", port),
			auth,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputnoauth", port),
			map[string]any{},
			bringyour.NoCustomHeaders,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputnoauth", port),
			map[string]any{},
			auth,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputauth", port),
			map[string]any{},
			auth,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		// should allow guest requests
		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputauth", port),
			map[string]any{},
			authGuestMode,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputauth", port),
			map[string]any{},
			bringyour.NoCustomHeaders,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputauth-no-guest", port),
			map[string]any{},
			authGuestMode,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		// should deny guest requests
		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputauth-no-guest", port),
			map[string]any{},
			auth,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputauth-no-guest", port),
			map[string]any{},
			bringyour.NoCustomHeaders,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputclient", port),
			map[string]any{},
			authClient,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.Equal(t, err, nil)

		_, err = bringyour.HttpPost(
			fmt.Sprintf("http://127.0.0.1:%d/inputclient", port),
			map[string]any{},
			auth,
			bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject[map[string]any]),
		)
		assert.NotEqual(t, err, nil)

	})
}
