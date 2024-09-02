package main_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"bringyour.com/connect"
	"bringyour.com/service/api/apirouter"
	webrtcconn "github.com/bringyour/webrtc-conn"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/sync/errgroup"
)

func setupPostgres(t *testing.T, tempDir string) {

	t.Helper()

	ctx := context.Background()

	dbName := "bringyour"
	dbUser := "bringyour"
	dbPassword := "thisisatest"

	postgresContainer, err := postgres.Run(
		ctx,
		"docker.io/postgres:16-alpine",
		postgres.WithDatabase(dbName),
		postgres.WithUsername(dbUser),
		postgres.WithPassword(dbPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(5*time.Second),
		),
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		postgresContainer.Terminate(ctx)
	})

	ep, err := postgresContainer.Endpoint(ctx, "")
	require.NoError(t, err)

	pgAuth := map[string]string{
		"authority": ep,
		"user":      dbUser,
		"password":  dbPassword,
		"db":        dbName,
	}

	d, err := json.Marshal(pgAuth)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tempDir, "pg.yml"), d, 0644)
	require.NoError(t, err)

	bringyour.ApplyDbMigrations(ctx)

}

func setupRedis(t *testing.T, tempDir string) {
	ctx := context.Background()

	redisContainer, err := redis.Run(ctx, "redis:7.4.0")
	require.NoError(t, err)

	t.Cleanup(func() {
		redisContainer.Terminate(ctx)
	})

	ep, err := redisContainer.Endpoint(ctx, "")
	require.NoError(t, err)

	redisConfig := map[string]any{
		"authority": ep,
		"password":  "",
		"db":        0,
	}

	d, err := json.Marshal(redisConfig)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(tempDir, "redis.yml"), d, 0644)
	require.NoError(t, err)

}

func setupPrivateKey(t *testing.T, tempDir string) {

	t.Helper()

	// Generate RSA private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	pksc8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)

	// Encode the private key to PEM format
	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: pksc8,
	}

	keyDir := filepath.Join(tempDir, "tls", "bringyour.com")

	err = os.MkdirAll(keyDir, 0755)
	require.NoError(t, err)

	// // Create a file to save the PEM encoded private key
	file, err := os.Create(filepath.Join(keyDir, "bringyour.com.key"))
	require.NoError(t, err)
	defer file.Close()

	// Write the PEM encoded private key to the file
	err = pem.Encode(file, privateKeyPEM)
	require.NoError(t, err)

}

func testID(t *testing.T) string {
	t.Helper()
	s := sha1.Sum([]byte(t.Name()))
	return hex.EncodeToString(s[:])
}

func TestEndToEnd(t *testing.T) {

	td := t.TempDir()

	os.Setenv("WARP_VAULT_HOME", td)

	setupPrivateKey(t, td)
	setupRedis(t, td)
	setupPostgres(t, td)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	notJWTSession := session.NewLocalClientSession(ctx, "localhost:1234", nil)

	userName := "test"
	userAuth := "test@bringyour.com"
	userPassword := "aaksdfkasd634"
	networkName := "thisisatest"

	result, err := model.NetworkCreate(model.NetworkCreateArgs{
		UserName:    userName,
		Password:    &userPassword,
		UserAuth:    &userAuth,
		NetworkName: networkName,
		Terms:       true,
	}, notJWTSession)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.NotNil(t, result.VerificationRequired)

	createCodeResult, err := model.AuthVerifyCreateCode(
		model.AuthVerifyCreateCodeArgs{
			UserAuth: result.VerificationRequired.UserAuth,
		},
		notJWTSession,
	)
	require.NoError(t, err)
	require.Nil(t, createCodeResult.Error)

	av, err := model.AuthVerify(
		model.AuthVerifyArgs{
			UserAuth:   result.VerificationRequired.UserAuth,
			VerifyCode: *createCodeResult.VerifyCode,
		},
		notJWTSession,
	)

	require.NoError(t, err)
	require.NotNil(t, av)

	s := httptest.NewServer(apirouter.NewAPIRouter())
	defer s.Close()

	login, err := model.AuthLoginWithPassword(model.AuthLoginWithPasswordArgs{
		UserAuth: userAuth,
		Password: userPassword,
	}, notJWTSession)

	require.NoError(t, err)
	require.NotNil(t, login)

	byJwt, err := jwt.ParseByJwt(*login.Network.ByJwt)
	require.NoError(t, err)

	jwtSession := session.NewLocalClientSession(ctx, "localhost:1234", byJwt)

	cl, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{
		Description: "test",
		DeviceSpec:  "test",
	}, jwtSession)
	require.NoError(t, err)

	require.Nil(t, cl.Error)

	byApiClient := connect.NewBringYourApi(connect.DefaultClientStrategy(ctx), s.URL)
	byApiClient.SetByJwt(*cl.ByClientJwt)

	t.Run("offer sdp", func(t *testing.T) {

		handshakeID := testID(t)

		err := byApiClient.PutPeerToPeerOfferSDPSync(
			ctx,
			handshakeID,
			webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  "",
			},
		)
		require.NoError(t, err)

		sdp, err := byApiClient.PollPeerToPeerOfferSDPSync(
			ctx,
			handshakeID,
		)

		require.NoError(t, err)
		require.Equal(
			t,
			webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  "",
			},
			sdp,
		)
	})

	t.Run("answer sdp", func(t *testing.T) {

		handshakeID := testID(t)

		err := byApiClient.PutPeerToPeerAnswerSDPSync(
			ctx,
			handshakeID,
			webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  "",
			},
		)
		require.NoError(t, err)

		sdp, err := byApiClient.PollPeerToPeerAnswerSDPSync(
			ctx,
			handshakeID,
		)
		require.NoError(t, err)

		require.Equal(
			t,
			webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  "",
			},
			sdp,
		)
	})

	t.Run("offer peer candidates", func(t *testing.T) {

		handshakeID := testID(t)

		require.NoError(t, err)
		err := byApiClient.PostPeerToPeerOfferPeerCandidateSync(
			ctx,
			handshakeID,
			webrtc.ICECandidate{
				Typ:        webrtc.ICECandidateTypeHost,
				Foundation: "test",
				Priority:   1,
				Protocol:   webrtc.ICEProtocolTCP,
				Address:    "test1",
				Port:       1,
			},
		)
		require.NoError(t, err)

		candidates, err := byApiClient.PollPeerToPeerOfferCandidatesSync(
			ctx,
			handshakeID,
			0,
		)
		require.NoError(t, err)
		require.Equal(
			t,
			[]webrtc.ICECandidate{
				{
					Typ:        webrtc.ICECandidateTypeHost,
					Foundation: "test",
					Priority:   1,
					Protocol:   webrtc.ICEProtocolTCP,
					Address:    "test1",
					Port:       1,
				},
			},
			candidates,
		)

		err = byApiClient.PostPeerToPeerOfferPeerCandidateSync(
			ctx,
			handshakeID,
			webrtc.ICECandidate{
				Typ: webrtc.ICECandidateTypeHost,

				Foundation: "test",
				Priority:   2,
				Protocol:   webrtc.ICEProtocolTCP,
				Address:    "test2",
				Port:       2,
			},
		)
		require.NoError(t, err)

		candidates, err = byApiClient.PollPeerToPeerOfferCandidatesSync(
			ctx,
			handshakeID,
			1,
		)
		require.NoError(t, err)
		require.Equal(
			t,
			[]webrtc.ICECandidate{
				{
					Typ: webrtc.ICECandidateTypeHost,

					Foundation: "test",
					Priority:   2,
					Protocol:   webrtc.ICEProtocolTCP,
					Address:    "test2",
					Port:       2,
				},
			},
			candidates,
		)

	})

	t.Run("answer peer candidates", func(t *testing.T) {

		handshakeID := testID(t)

		err := byApiClient.PostPeerToPeerAnswerPeerCandidateSync(
			ctx,
			handshakeID,
			webrtc.ICECandidate{
				Typ:        webrtc.ICECandidateTypeHost,
				Foundation: "test",
				Priority:   1,
				Protocol:   webrtc.ICEProtocolTCP,
				Address:    "test1",
				Port:       1,
			},
		)
		require.NoError(t, err)

		candidates, err := byApiClient.PollPeerToPeerAnswerCandidatesSync(
			ctx,
			handshakeID,
			0,
		)
		require.NoError(t, err)
		require.Equal(
			t,
			[]webrtc.ICECandidate{
				{
					Typ:        webrtc.ICECandidateTypeHost,
					Foundation: "test",
					Priority:   1,
					Protocol:   webrtc.ICEProtocolTCP,
					Address:    "test1",
					Port:       1,
				},
			}, candidates,
		)

		err = byApiClient.PostPeerToPeerAnswerPeerCandidateSync(
			ctx,
			handshakeID,
			webrtc.ICECandidate{
				Typ: webrtc.ICECandidateTypeHost,

				Foundation: "test",
				Priority:   2,
				Protocol:   webrtc.ICEProtocolTCP,
				Address:    "test2",
				Port:       2,
			},
		)
		require.NoError(t, err)

		candidates, err = byApiClient.PollPeerToPeerAnswerCandidatesSync(
			ctx,
			handshakeID,
			1,
		)
		require.NoError(t, err)
		require.Equal(
			t,
			[]webrtc.ICECandidate{
				{
					Typ: webrtc.ICECandidateTypeHost,

					Foundation: "test",
					Priority:   2,
					Protocol:   webrtc.ICEProtocolTCP,
					Address:    "test2",
					Port:       2,
				},
			},
			candidates,
		)

	})

	t.Run("establish WebRTC conn using the backend", func(t *testing.T) {

		handshakeID := testID(t)

		tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		eg, egCtx := errgroup.WithContext(tctx)

		var offerReceived string

		eg.Go(func() error {

			conn, err := webrtcconn.Offer(
				egCtx,
				webrtc.Configuration{},
				connect.NewWebRTCOfferHandshake(byApiClient, handshakeID),
				true,
				3,
				5*time.Second,
			)
			if err != nil {
				return fmt.Errorf("offer: %w", err)
			}
			defer conn.Close()

			_, err = conn.Write([]byte("test offer body"))
			if err != nil {
				return fmt.Errorf("offer write: %w", err)
			}

			buffer := make([]byte, 1024)

			n, err := conn.Read(buffer)

			if err != nil {
				return fmt.Errorf("offer read: %w", err)
			}

			offerReceived = string(buffer[:n])

			return err
		})

		var answerReceived string

		eg.Go(func() error {
			conn, err := webrtcconn.Answer(
				egCtx,
				webrtc.Configuration{},
				connect.NewWebRTCAnswerHandshake(byApiClient, handshakeID),
				5*time.Second,
			)
			if err != nil {
				return fmt.Errorf("answer: %w", err)
			}
			defer conn.Close()

			_, err = conn.Write([]byte("test answer body"))

			if err != nil {
				return fmt.Errorf("answer write: %w", err)
			}

			buffer := make([]byte, 1024)

			n, err := conn.Read(buffer)
			if err != nil {
				return fmt.Errorf("answer read: %w", err)
			}

			answerReceived = string(buffer[:n])

			return nil
		})

		err = eg.Wait()
		require.NoError(t, err)

		require.Equal(t, "test answer body", offerReceived)

		require.Equal(t, "test offer body", answerReceived)

	})

}
