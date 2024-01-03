package router

import (
    "context"
    "testing"
    "net/http"
    "errors"
    "fmt"
    "time"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour/session"
    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour"
)


func TestRouterBasic(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
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
            return map[string]any{}, nil
        }
        WrapRequireAuth(impl, w, r)
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
        NewRoute("GET", "/client", Client),
        NewRoute("POST", "/inputnoauth", InputNoAuth),
        NewRoute("POST", "/inputauth", InputAuth),
        NewRoute("POST", "/inputclient", InputClient),
    }

    port := 8080

    routerHandler := NewRouter(cancelCtx, routes)
    go http.ListenAndServe(fmt.Sprintf(":%d", port), routerHandler)


    select {
    case <- time.After(time.Second):
    }

    networkId := bringyour.NewId()
    userId := bringyour.NewId()
    byJwt := jwt.NewByJwt(
        networkId,
        userId,
        "test",
    )
    auth := func(header http.Header) {
        header.Add("Authorization", fmt.Sprintf("Bearer %s", byJwt.Sign()))
    }

    clientId := bringyour.NewId()
    byClientJwt := byJwt.WithClientId(&clientId)
    authClient := func(header http.Header) {
        header.Add("Authorization", fmt.Sprintf("Bearer %s", byClientJwt.Sign()))
    }


    var err error

    _, err = bringyour.HttpGet(
        fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
        bringyour.NoCustomHeaders,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.Equal(t, err, nil)

    _, err = bringyour.HttpGet(
        fmt.Sprintf("http://127.0.0.1:%d/noauth", port),
        auth,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.Equal(t, err, nil)

    _, err = bringyour.HttpGet(
        fmt.Sprintf("http://127.0.0.1:%d/auth", port),
        auth,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.Equal(t, err, nil)

    _, err = bringyour.HttpGet(
        fmt.Sprintf("http://127.0.0.1:%d/auth", port),
        bringyour.NoCustomHeaders,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.NotEqual(t, err, nil)

    _, err = bringyour.HttpGet(
        fmt.Sprintf("http://127.0.0.1:%d/client", port),
        authClient,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.Equal(t, err, nil)

    _, err = bringyour.HttpGet(
        fmt.Sprintf("http://127.0.0.1:%d/client", port),
        auth,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.NotEqual(t, err, nil)


    _, err = bringyour.HttpPost(
        fmt.Sprintf("http://127.0.0.1:%d/inputnoauth", port),
        map[string]any{},
        bringyour.NoCustomHeaders,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.Equal(t, err, nil)

    _, err = bringyour.HttpPost(
        fmt.Sprintf("http://127.0.0.1:%d/inputnoauth", port),
        map[string]any{},
        auth,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.Equal(t, err, nil)

    _, err = bringyour.HttpPost(
        fmt.Sprintf("http://127.0.0.1:%d/inputauth", port),
        map[string]any{},
        auth,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.Equal(t, err, nil)

    _, err = bringyour.HttpPost(
        fmt.Sprintf("http://127.0.0.1:%d/inputauth", port),
        map[string]any{},
        bringyour.NoCustomHeaders,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.NotEqual(t, err, nil)

    _, err = bringyour.HttpPost(
        fmt.Sprintf("http://127.0.0.1:%d/inputclient", port),
        map[string]any{},
        authClient,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.Equal(t, err, nil)

    _, err = bringyour.HttpPost(
        fmt.Sprintf("http://127.0.0.1:%d/inputclient", port),
        map[string]any{},
        auth,
        bringyour.HttpResponseRequireStatusOk(bringyour.ResponseJsonObject),
    )
    assert.NotEqual(t, err, nil)

})}

