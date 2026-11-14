package taskworker

// The push sender's composition in taskworker (SDK-M8-10): vault's wake-vendors.yml builds each platform's
// vendor from its keys, refusing a file that names neither or one whose endpoint is not https without naming a
// key; and a sender on a host wakes through its vendor an entry the owning services' enqueuer queued while
// sidecar.yml's wake switch is on, and reads nothing while it is off. Keys are made by the test, and every host,
// team and project is synthetic.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A PEM PKCS #8 key.
func testWakePem(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// wake-vendors.yml builds an APNs and an FCM vendor from their keys and endpoints; a file naming neither, an http
// endpoint and a key of the wrong kind are refused, and no refusal names a key.
func TestWakeVendorsOfBuildsEachPlatform(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	apnsPem := testWakePem(t, ecKey)
	serviceAccount, err := json.Marshal(map[string]string{
		"project_id":   "synthetic-wake-project",
		"private_key":  testWakePem(t, rsaKey),
		"client_email": "wake-sender@synthetic-wake-project.example",
		"token_uri":    "https://token.synthetic.example/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	y := &wakeVendorsYaml{}
	y.Apns = &struct {
		Endpoint   string `yaml:"endpoint"`
		Topic      string `yaml:"topic"`
		TeamId     string `yaml:"team_id"`
		KeyId      string `yaml:"key_id"`
		PrivateKey string `yaml:"private_key"`
	}{Endpoint: "https://apns.synthetic.example", Topic: "example.synthetic.wake", TeamId: "SYNTHTEAM1", KeyId: "SYNTHKEY01", PrivateKey: apnsPem}
	y.Fcm = &struct {
		Endpoint       string `yaml:"endpoint"`
		ServiceAccount string `yaml:"service_account"`
	}{Endpoint: "https://fcm.synthetic.example", ServiceAccount: string(serviceAccount)}
	vendors, err := wakeVendorsOf(y)
	if err != nil || len(vendors) != 2 || vendors[0].Platform() != pushservice.WakePlatformApns || vendors[1].Platform() != pushservice.WakePlatformFcm {
		t.Fatalf("%v, %v", vendors, err)
	}
	if _, err := wakeVendorsOf(&wakeVendorsYaml{}); err == nil {
		t.Error("a file naming no vendor built one")
	}
	y.Apns.Endpoint = "http://apns.synthetic.example"
	if _, err := wakeVendorsOf(y); err == nil || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("an http endpoint: %v", err)
	}
	y.Apns.Endpoint = "https://apns.synthetic.example"
	y.Apns.PrivateKey = testWakePem(t, rsaKey)
	if _, err := wakeVendorsOf(y); err == nil || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("an rsa apns key: %v", err)
	}
}

// A sender on a host takes the host's Redis, gates and clock: an entry the owning services' enqueuer queues for a
// token holder wakes through the vendor while the wake switch is on; switched off, the sender reads nothing.
func TestWakeSenderOnHostFollowsTheSwitch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	redis := sidecartest.NewFakeRedis(ctx, clock, nil, nil, nil)
	defer redis.Close()
	sidecartest.RegisterPushScripts(redis)
	gates := sidecartest.NewFakeServiceGates(nil)
	if err := gates.Set("sidecar:\n  wake: {enabled: true}\n"); err != nil {
		t.Fatal(err)
	}
	host := &sidecar.ServiceHost{Redis: redis, Gates: gates, Clock: clock}
	// the group stands before the first entry, as the first sender's start leaves it in production
	queue := pushservice.DefaultWakeQueueSettings()
	for shard := range queue.ShardCount {
		if err := redis.StreamGroupCreate(ctx, pushservice.WakeQueueKey(shard), pushservice.DefaultWakeSenderSettings().ConsumerGroup); err != nil {
			t.Fatal(err)
		}
	}
	vendor := sidecartest.NewFakeWakeVendor(pushservice.WakePlatformApns, clock)
	var lifetime sync.WaitGroup
	sender, err := startWakeSenderOnHost(ctx, &lifetime, host, []pushservice.WakeVendor{vendor}, "taskworker-test")
	if err != nil {
		t.Fatal(err)
	}
	clientId := connect.Id{0xc1, 0x1e}
	if err := redis.Set(ctx, pushservice.HintTokenKey(clientId), pushservice.WakeTokenValue(pushservice.WakePlatformApns, "synthetic-apns-token"), 0); err != nil {
		t.Fatal(err)
	}
	if err := redis.Set(ctx, pushservice.WakeTokenRevisionKey(clientId), []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	enqueuer, err := pushservice.NewWakeEnqueuer(redis, nil, clock, func() bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(seq int) {
		result := &pushservice.PushHintResult{
			StreamId: strconv.FormatInt(clock.Now().UnixMilli(), 10) + "-" + strconv.Itoa(seq),
			Version:  uint64(seq),
			Token:    []byte("present"),
		}
		if outcome := enqueuer.Enqueue(ctx, clientId, &sidecar.ClientHint{Topic: sidecar.PushTopicMail, Version: uint64(seq)}, result); outcome != pushservice.WakeEnqueueDue {
			t.Fatalf("enqueue %d: %v", seq, outcome)
		}
	}
	enqueue(1)
	select {
	case submission := <-vendor.Submitted():
		if submission.Token != "synthetic-apns-token" || submission.Payload.TopicClass != "mail" || submission.Payload.Version != 1 {
			t.Fatalf("%+v", submission)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no wake while the switch is on")
	}
	if err := gates.Set("sidecar:\n  wake: {enabled: false}\n"); err != nil {
		t.Fatal(err)
	}
	if wakeSenderCurrent.sender != sender {
		t.Error("the sender is not current")
	}
	cancel()
	lifetime.Wait()
	if wakeSenderCurrent.sender != nil {
		t.Error("the sender outlived its runtime")
	}
}
