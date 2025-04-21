package controller

// import (
// 	// "context"
//     "testing"
//     "fmt"
//     "encoding/json"

//     "github.com/go-playground/assert/v2"

//     "github.com/urnetwork/server/v2025"
// )

/*
func TestGooglePlayPubSub(t *testing.T) { (&server.TestEnv{ApplyDbMigrations: false}).Run(func() {
	// https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions?hl=en#protocol
	jwt := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImJkYzRlMTA5ODE1ZjQ2OTQ2MGU2M2QzNGNkNjg0MjE1MTQ4ZDdiNTkiLCJ0eXAiOiJKV1QifQ.eyJhdWQiOiJodHRwczovL2FwaS5icmluZ3lvdXIuY29tL3BheS9wbGF5IiwiYXpwIjoiMTA4NTQyODI4MTk0ODEzMjUyNjI4IiwiZW1haWwiOiIzMzg2Mzg4NjUzOTAtY29tcHV0ZUBkZXZlbG9wZXIuZ3NlcnZpY2VhY2NvdW50LmNvbSIsImVtYWlsX3ZlcmlmaWVkIjp0cnVlLCJleHAiOjE3MDc3Njc3NzksImlhdCI6MTcwNzc2NDE3OSwiaXNzIjoiaHR0cHM6Ly9hY2NvdW50cy5nb29nbGUuY29tIiwic3ViIjoiMTA4NTQyODI4MTk0ODEzMjUyNjI4In0.qyBEr-SiWZ4OiY1ettW3CC-17cd0pRVGLRl3QCvFoEF9JtY-ivrPQuTdWKMbCe668-2Sejq1jhfF_rXnigIv42SuIN7QwS9cYstRIHbwP4Qekvk5ArSuAQnOnLcEgiAqLS8MC4a0JHuUkJPzOrFKfH_IBM6e7J3BJMoEQ-WuTDsaONqeMB4RENwuwV48R_pUs9P3OY1LM-5S2qtXEnYnbWFuBWETj6ewtw0X2jiq8Feh8ZMeGdSjLaG8CXfFhOMgOAy6Kg-3CxDThCN6ozLVXsP5ICB99KsxErsivpNYfe02TuDRtMPdVnRrnvGwKQs0ak1oKtHAPyFBp-X5LJ5cAA"
	url := fmt.Sprintf("https://oauth2.googleapis.com/tokeninfo?id_token=%s", jwt)

	bodyBytes, err := server.HttpGetRawRequireStatusOk(url, server.NoCustomHeaders)
	assert.Equal(t, err, nil)
	assert.NotEqual(t, bodyBytes, nil)

	// parse the body as a claim map
	var claims map[string]any
	err = json.Unmarshal(bodyBytes, &claims)
	assert.Equal(t, err, nil)

	assert.Equal(t, claims["email"], "338638865390-compute@developer.gserviceaccount.com")
})}
*/
