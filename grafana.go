package server

// stats publishing to the warp grafana service.
// every service pushes the default prometheus registry to the stable local
// publish port on its host (`local_port` in config grafana.yml), keyed by
// {env, service, block, host, instance}. the grafana go front stamps the
// receive time and forwards to mimir, so series go stale when a service
// stops pushing.
//
// instance is a random per-process nonce, and it is load bearing: during a
// warp redeploy the draining old container and the new container run — and
// push — CONCURRENTLY. without instance they write the SAME series, and
// the interleaved counter values (old high, new near zero) read as a
// counter reset on every interleave, so increase() re-adds the old
// counter's total each time and windowed sums inflate by terabytes. with
// instance each process owns its series: the old one goes stale at drain
// end, the new one starts near zero, and increase() sums both correctly.
//
// register service metrics with the default prometheus registry
// (see connect/transport.go for an example) and they are pushed
// automatically.
// see warp/grafana in the warp repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/glog"
)

const statsPushInterval = 15 * time.Second

const defaultGrafanaLocalPort = 3100

// Narrow service-user view shared by ordinary identities/roles and vault-owned
// passwords.
type grafanaPushConfig struct {
	Users []*grafanaPushUser `yaml:"users,omitempty"`
}

// One role-scoped HTTP Basic Auth identity.
type grafanaPushUser struct {
	Name     string   `yaml:"name,omitempty"`
	Password string   `yaml:"password,omitempty"`
	Roles    []string `yaml:"roles,omitempty"`
}

// A service's own push-only vault file (plan-OPS-small-readings #5): the one
// credential a sidecar block mounts in its secret_files, holding one user,
// which config grafana.yml gives the push role. A new block never mounts
// vault's grafana.yml, so one hostile-facing block's credential is revoked and
// rotated alone.
func grafanaPushResourceName(service string) string {
	return fmt.Sprintf("grafana-push-%s.yml", service)
}

// A warp service name, as a vault file name may carry it.
var grafanaServiceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// The push credential in a service's own push file: exactly one user, with a
// password and no roles, whom config grafana.yml gives the push role, so the
// file cannot add an identity to the ingestion policy any more than vault's
// grafana.yml can.
func grafanaScopedPushCredential(resourceName string, resource *SimpleResource, ordinaryUsers map[string]*grafanaPushUser) (username string, password string, err error) {
	var pushConfig grafanaPushConfig
	if err := resource.UnmarshalYamlE(&pushConfig); err != nil {
		return "", "", fmt.Errorf("vault %s: %w", resourceName, err)
	}
	if len(pushConfig.Users) != 1 || pushConfig.Users[0] == nil {
		return "", "", fmt.Errorf("vault %s holds %d users, not one", resourceName, len(pushConfig.Users))
	}
	pushUser := pushConfig.Users[0]
	if pushUser.Name == "" || pushUser.Password == "" {
		return "", "", fmt.Errorf("vault %s has a user with no name or password", resourceName)
	}
	if 0 < len(pushUser.Roles) {
		return "", "", fmt.Errorf("vault %s contains roles for %q", resourceName, pushUser.Name)
	}
	ordinaryUser := ordinaryUsers[pushUser.Name]
	if ordinaryUser == nil || !slices.Contains(ordinaryUser.Roles, "push") {
		return "", "", fmt.Errorf("vault %s user %q has no push role in config grafana.yml", resourceName, pushUser.Name)
	}
	return pushUser.Name, pushUser.Password, nil
}

// Resolves ordinary endpoint configuration separately from the scoped push
// credential. A service's own push file, grafana-push-<service>.yml, comes
// first; a service without one reads vault's grafana.yml as existing services
// do. A push file that is there but unreadable, or that does not hold one push
// user, is an error, never a fall back to another credential.
func grafanaPushSettings() (localPort int, username string, password string, err error) {
	localPort = defaultGrafanaLocalPort
	grafanaResource, err := Config.SimpleResource("grafana.yml")
	if err != nil {
		return 0, "", "", err
	}
	if localPorts := grafanaResource.Int("local_port"); 0 < len(localPorts) {
		localPort = localPorts[0]
	}

	var ordinaryConfig grafanaPushConfig
	grafanaResource.UnmarshalYaml(&ordinaryConfig)
	ordinaryUsers := map[string]*grafanaPushUser{}
	for _, ordinaryUser := range ordinaryConfig.Users {
		if ordinaryUser == nil || ordinaryUser.Name == "" {
			return 0, "", "", errors.New("config grafana.yml has an unnamed user")
		}
		if ordinaryUser.Password != "" {
			return 0, "", "", fmt.Errorf("config grafana.yml contains password for %q", ordinaryUser.Name)
		}
		if ordinaryUsers[ordinaryUser.Name] != nil {
			return 0, "", "", fmt.Errorf("config grafana.yml repeats user %q", ordinaryUser.Name)
		}
		ordinaryUsers[ordinaryUser.Name] = ordinaryUser
	}

	if service, serviceErr := Service(); serviceErr == nil {
		if !grafanaServiceNamePattern.MatchString(service) {
			return 0, "", "", fmt.Errorf("service %q names no push file", service)
		}
		pushResourceName := grafanaPushResourceName(service)
		pushResource, err := Vault.SimpleResource(pushResourceName)
		if err == nil {
			username, password, err := grafanaScopedPushCredential(pushResourceName, pushResource, ordinaryUsers)
			if err != nil {
				return 0, "", "", err
			}
			return localPort, username, password, nil
		}
		if !errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrResourceUnavailable) {
			return 0, "", "", err
		}
	}

	secretResource, err := Vault.SimpleResource("grafana.yml")
	if err != nil {
		return 0, "", "", err
	}
	var secretConfig grafanaPushConfig
	secretResource.UnmarshalYaml(&secretConfig)
	secretPasswords := map[string]string{}
	for _, secretUser := range secretConfig.Users {
		if secretUser == nil || secretUser.Name == "" {
			return 0, "", "", errors.New("grafana.yml has an unnamed secret user")
		}
		if 0 < len(secretUser.Roles) {
			return 0, "", "", fmt.Errorf("vault grafana.yml contains roles for %q", secretUser.Name)
		}
		if _, ok := secretPasswords[secretUser.Name]; ok {
			return 0, "", "", fmt.Errorf("grafana.yml repeats secret user %q", secretUser.Name)
		}
		if ordinaryUsers[secretUser.Name] == nil {
			return 0, "", "", fmt.Errorf("vault grafana.yml user %q is absent from config", secretUser.Name)
		}
		secretPasswords[secretUser.Name] = secretUser.Password
	}
	for _, ordinaryUser := range ordinaryConfig.Users {
		if ordinaryUser != nil && ordinaryUser.Name != "" && slices.Contains(ordinaryUser.Roles, "push") {
			if password := secretPasswords[ordinaryUser.Name]; password != "" {
				return localPort, ordinaryUser.Name, password, nil
			}
		}
	}
	return 0, "", "", errors.New("grafana.yml has no credential with the push role")
}

// Centralizes the authenticated client boundary so local and regression-test
// callers cannot accidentally omit the push credential.
func newStatsPusher(pushUrl string, service string, username string, password string) *push.Pusher {
	return push.New(pushUrl, service).
		Gatherer(prometheus.DefaultGatherer).
		BasicAuth(username, password)
}

var buildInfoGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Name:      "build_info",
		Help:      "Build info of the running service",
	},
	[]string{"version"},
)

var sourceInfoGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Name:      "source_info",
		Help:      "Go VCS source and immutable image identity of the running service",
	},
	[]string{"revision", "modified", "image_digest"},
)

type sourceBuildInfo struct {
	revision string
	modified bool
}

func sourceBuildInfoFromSettings(settings []debug.BuildSetting) (sourceBuildInfo, bool) {
	info := sourceBuildInfo{}
	hasModified := false
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			info.revision = setting.Value
		case "vcs.modified":
			modified, err := strconv.ParseBool(setting.Value)
			if err != nil {
				return sourceBuildInfo{}, false
			}
			info.modified = modified
			hasModified = true
		}
	}
	return info, info.revision != "" && hasModified
}

func currentSourceBuildInfo() (sourceBuildInfo, bool) {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return sourceBuildInfo{}, false
	}
	return sourceBuildInfoFromSettings(buildInfo.Settings)
}

func sourceInfoLabelValues(source sourceBuildInfo, imageDigest string) []string {
	return []string{
		source.revision,
		strconv.FormatBool(source.modified),
		strings.TrimSpace(imageDigest),
	}
}

func init() {
	prometheus.MustRegister(buildInfoGauge)
	prometheus.MustRegister(sourceInfoGauge)
}

// Why StartStatsPusher would publish nothing in this process, nil when it
// would: no warp identity, or no push credential. StartStatsPusher says so only
// at Info, so a sidecar block's host asks here and logs it loudly, since a block
// that pushes nothing runs unseen (plan-OPS-small-readings #5).
func StatsPushUnavailable() error {
	if _, err := Env(); err != nil {
		return err
	}
	if _, err := Service(); err != nil {
		return err
	}
	if _, err := Block(); err != nil {
		return err
	}
	if _, err := Host(); err != nil {
		return err
	}
	_, _, _, err := grafanaPushSettings()
	return err
}

// StartStatsPusher pushes the default prometheus registry
// (go runtime, process, and service metrics) to the local grafana
// publish port every interval, until the context is done.
// a no-op when not running under warp
// or when ordinary configuration or a push credential is unavailable.
// The returned flush pushes the current registry state immediately: call it
// once at drain end so the final drain gauges land before the process exits
// (series go stale as soon as the pusher stops).
func StartStatsPusher(ctx context.Context) (flush func()) {
	flush = func() {}

	env, err := Env()
	if err != nil {
		glog.Infof("[stats]not running under warp (%s). Not publishing stats.\n", err)
		return
	}
	service, err := Service()
	if err != nil {
		glog.Infof("[stats]not running under warp (%s). Not publishing stats.\n", err)
		return
	}
	block, err := Block()
	if err != nil {
		glog.Infof("[stats]not running under warp (%s). Not publishing stats.\n", err)
		return
	}
	host, err := Host()
	if err != nil {
		glog.Infof("[stats]not running under warp (%s). Not publishing stats.\n", err)
		return
	}

	localPort, username, password, err := grafanaPushSettings()
	if err != nil {
		glog.Infof("[stats]grafana push is not configured (%s). Not publishing stats.\n", err)
		return
	}

	if version, err := Version(); err == nil {
		buildInfoGauge.WithLabelValues(version).Set(1)
	}
	if source, ok := currentSourceBuildInfo(); ok {
		sourceInfoGauge.WithLabelValues(sourceInfoLabelValues(
			source,
			os.Getenv("WARP_IMAGE_DIGEST"),
		)...).Set(1)
	}

	// a random per-process nonce so concurrent containers never write the
	// same series (see the package comment)
	instanceBytes := make([]byte, 4)
	if _, err := rand.Read(instanceBytes); err != nil {
		panic(err)
	}
	instance := hex.EncodeToString(instanceBytes)

	pusher := newStatsPusher(fmt.Sprintf("http://127.0.0.1:%d", localPort), service, username, password).
		Client(NewHttpClient(10*time.Second)).
		Grouping("env", env).
		Grouping("service", service).
		Grouping("block", block).
		Grouping("host", host).
		Grouping("instance", instance)

	glog.Infof("[stats]publishing to http://127.0.0.1:%d\n", localPort)

	// the pusher is shared between the periodic loop and the final flush
	var pushLock sync.Mutex
	push := func(pushCtx context.Context) error {
		pushLock.Lock()
		defer pushLock.Unlock()
		return pusher.PushContext(pushCtx)
	}

	go HandleError(func() {
		pushOk := true
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(statsPushInterval):
			}

			if err := push(ctx); err != nil {
				// log on state change only, e.g. during a grafana redeploy
				if pushOk {
					glog.Infof("[stats]push error (%s)\n", err)
				}
				pushOk = false
			} else {
				if !pushOk {
					glog.Infof("[stats]push ok\n")
				}
				pushOk = true
			}
		}
	})

	flush = func() {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer flushCancel()
		if err := push(flushCtx); err != nil {
			glog.Infof("[stats]final push error (%s)\n", err)
		}
	}
	return
}
