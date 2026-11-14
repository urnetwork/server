package taskworker

// §19.4's push sender in taskworker (SDK-M8-10; §18.7's step 4, D-SDK-16): a continuous loop beside the task
// workers, not a job, that wakes phones through APNs and FCM for the entries the owning services queue on
// {wakeq_<shard>}. It runs on a ServiceHost of its own on server's Redis, metrics and sidecar.yml's gates, and
// sends only while the wake switch is on; while it is off the sender reads nothing and the entries age out.
//
// The vendors' keys come from vault's wake-vendors.yml, which OPS-M8-01's taskworker fragment mounts:
//
//	apns:  endpoint, topic, team_id, key_id, private_key   the .p8 in PEM
//	fcm:   endpoint, service_account                         the project's JSON key
//
// either may be absent, and a process with neither file holds no sender, today's production among them. Each
// platform's endpoint is the file's, so no vendor host sits in code. A vendor's rejection is tombstoned through
// model.TombstonePushTokenIfRevision, which tombstones only at the revision the sender read with the rejected
// token and then applies the tombstone (§18.7's step 0). Nothing here logs a token or a key (I-13).

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// vault's file of the vendors' keys, which OPS-M8-01's fragment mounts.
const wakeVendorsResourceName = "wake-vendors.yml"

// wake-vendors.yml's form.
type wakeVendorsYaml struct {
	Apns *struct {
		Endpoint   string `yaml:"endpoint"`
		Topic      string `yaml:"topic"`
		TeamId     string `yaml:"team_id"`
		KeyId      string `yaml:"key_id"`
		PrivateKey string `yaml:"private_key"`
	} `yaml:"apns"`
	Fcm *struct {
		Endpoint       string `yaml:"endpoint"`
		ServiceAccount string `yaml:"service_account"`
	} `yaml:"fcm"`
}

// What startWakeSender runs: a test replaces it.
var startWakeSender = startWakeSenderFromVault

// The sender the newest runtime started, nil while none runs, for a status or a test to read.
var wakeSenderCurrent struct {
	stateLock sync.Mutex
	sender    *pushservice.WakeSender
}

// Starts the push sender from vault's keys on a host of its own, ending with ctx. A vault without the file, or a
// file that names no vendor, starts nothing and says why.
func startWakeSenderFromVault(ctx context.Context, lifetime *sync.WaitGroup) {
	resource, err := server.Vault.SimpleResource(wakeVendorsResourceName)
	if err != nil {
		glog.Infof("[wake]no %s, so the push sender waits for its keys\n", wakeVendorsResourceName)
		return
	}
	y := &wakeVendorsYaml{}
	if err := resource.UnmarshalYamlE(y); err != nil {
		glog.Errorf("[wake]%s does not parse, so the push sender waits\n", wakeVendorsResourceName)
		return
	}
	vendors, err := wakeVendorsOf(y)
	if err != nil {
		// the error names a setting, never a key
		glog.Errorf("[wake]%s: %v\n", wakeVendorsResourceName, err)
		return
	}
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{})
	consumer := "taskworker"
	if hostName, err := server.Host(); err == nil {
		consumer = "taskworker-" + hostName
	}
	if _, err := startWakeSenderOnHost(ctx, lifetime, host, vendors, consumer); err != nil {
		glog.Errorf("[wake]the push sender did not start: %v\n", err)
	}
}

// The vendors y names, each built from its keys: none when it names none.
func wakeVendorsOf(y *wakeVendorsYaml) ([]pushservice.WakeVendor, error) {
	vendors := []pushservice.WakeVendor{}
	if y.Apns != nil {
		settings := pushservice.DefaultApnsVendorSettings()
		settings.Endpoint = y.Apns.Endpoint
		settings.Topic = y.Apns.Topic
		vendor, err := pushservice.NewApnsVendor(&pushservice.ApnsVendorArgs{
			TeamId:        y.Apns.TeamId,
			KeyId:         y.Apns.KeyId,
			PrivateKeyPem: []byte(y.Apns.PrivateKey),
			Settings:      settings,
		})
		if err != nil {
			return nil, fmt.Errorf("apns: %w", err)
		}
		vendors = append(vendors, vendor)
	}
	if y.Fcm != nil {
		account, err := pushservice.ParseFcmServiceAccount([]byte(y.Fcm.ServiceAccount))
		if err != nil {
			return nil, fmt.Errorf("fcm: %w", err)
		}
		settings := pushservice.DefaultFcmVendorSettings()
		settings.Endpoint = y.Fcm.Endpoint
		vendor, err := pushservice.NewFcmVendor(&pushservice.FcmVendorArgs{Account: account, Settings: settings})
		if err != nil {
			return nil, fmt.Errorf("fcm: %w", err)
		}
		vendors = append(vendors, vendor)
	}
	if len(vendors) == 0 {
		return nil, errors.New("names neither apns nor fcm")
	}
	return vendors, nil
}

// Starts a sender on host with vendors as consumer, current until ctx ends, then closed.
func startWakeSenderOnHost(ctx context.Context, lifetime *sync.WaitGroup, host *sidecar.ServiceHost, vendors []pushservice.WakeVendor, consumer string) (*pushservice.WakeSender, error) {
	if host == nil || host.Redis == nil {
		return nil, errors.New("the push sender needs a host with Redis")
	}
	sender, err := pushservice.NewWakeSender(ctx, &pushservice.WakeSenderArgs{
		Redis:    host.Redis,
		Tokens:   wakeTokenStore{},
		Vendors:  vendors,
		Consumer: consumer,
		Enabled: func() bool {
			return host.GatesOrClosed().Switch(sidecar.GateSwitchWake)
		},
		Clock:   host.ClockOrReal(),
		Metrics: host.Metrics,
		// glog, as the budget loops' own adapter writes it
		Log: evsBudgetLogger{},
	})
	if err != nil {
		return nil, err
	}
	func() {
		wakeSenderCurrent.stateLock.Lock()
		defer wakeSenderCurrent.stateLock.Unlock()
		wakeSenderCurrent.sender = sender
	}()
	lifetime.Go(func() {
		<-ctx.Done()
		sender.Close()
		wakeSenderCurrent.stateLock.Lock()
		defer wakeSenderCurrent.stateLock.Unlock()
		if wakeSenderCurrent.sender == sender {
			wakeSenderCurrent.sender = nil
		}
	})
	return sender, nil
}

// pushservice.WakeTokenStore over model: a vendor's rejection tombstones the row only at the revision read with
// the rejected token, then applies the tombstone.
type wakeTokenStore struct{}

// See pushservice.WakeTokenStore.
func (self wakeTokenStore) TombstoneIfRevision(ctx context.Context, clientId connect.Id, revision int64) (bool, error) {
	return model.TombstonePushTokenIfRevision(ctx, server.Id(clientId), revision, nil)
}
