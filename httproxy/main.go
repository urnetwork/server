package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/urfave/cli/v2"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server/httproxy/connprovider"
	"github.com/urnetwork/server/httproxy/ratelimiter"
)

type ConnectionConfig struct {
	ApiURL      string
	PlatformURL string
	JWT         string
}

type ConnectRequest struct {
	AuthCode string `json:"auth_code"`
}

type ClientProxyResponse struct {
	Host string `json:"host"`
	Port uint64 `json:"port"`
}

func main() {
	cfg := struct {
		addr        string
		proxyAddr   string
		apiURL      string
		platformURL string
		certFile    string
		keyFile     string
		redis       struct {
			addr     string
			password string
			db       int
		}
		rateLimits struct {
			networkPerMinute int
		}
	}{}
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "addr",
				Usage:       "Httpproxy server address",
				EnvVars:     []string{"ADDR"},
				Destination: &cfg.addr,
				Value:       ":30001",
			},
			&cli.StringFlag{
				Name:        "api-url",
				Usage:       "API URL",
				EnvVars:     []string{"API_URL"},
				Destination: &cfg.apiURL,
				Value:       "https://api.bringyour.com",
			},
			&cli.StringFlag{
				Name:        "platform-url",
				Usage:       "Platform URL",
				EnvVars:     []string{"PLATFORM_URL"},
				Destination: &cfg.platformURL,
				Value:       "wss://connect.bringyour.com",
			},
			&cli.StringFlag{
				Name:        "proxy-addr",
				Usage:       "Proxy address",
				EnvVars:     []string{"PROXY_ADDR"},
				Destination: &cfg.proxyAddr,
				Value:       ":10000",
			},
			&cli.StringFlag{
				Name:        "cert-file",
				Usage:       "Path to TLS certificate file",
				EnvVars:     []string{"CERT_FILE"},
				Destination: &cfg.certFile,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "key-file",
				Usage:       "Path to TLS private key file",
				EnvVars:     []string{"KEY_FILE"},
				Destination: &cfg.keyFile,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "redis-addr",
				Usage:       "Redis address",
				EnvVars:     []string{"REDIS_ADDR"},
				Destination: &cfg.redis.addr,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "redis-password",
				Usage:       "Redis password",
				EnvVars:     []string{"REDIS_PASSWORD"},
				Destination: &cfg.redis.password,
			},
			&cli.IntFlag{
				Name:        "redis-db",
				Usage:       "Redis database",
				EnvVars:     []string{"REDIS_DB"},
				Destination: &cfg.redis.db,
				Value:       0,
			},
			&cli.IntFlag{
				Name:        "rate-limit-network-per-minute",
				Usage:       "Rate limit for network connections per minute",
				EnvVars:     []string{"RATE_LIMIT_NETWORK_PER_MINUTE"},
				Destination: &cfg.rateLimits.networkPerMinute,
				Value:       5,
			},
		},
		Name: "httproxy",
		Action: func(c *cli.Context) error {

			log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

			ctx := c.Context

			mux := http.NewServeMux()

			mu := &sync.Mutex{}

			proxies := make(map[string]*ClientProxy)

			rdb := redis.NewClient(&redis.Options{
				Addr:     cfg.redis.addr,
				Password: cfg.redis.password,
				DB:       cfg.redis.db,
			})

			rl := ratelimiter.New(rdb)

			proxyFunc := func(w http.ResponseWriter, r *http.Request) {
				log := log.With("path", r.URL.Path, "method", r.Method)
				hostname := r.TLS.ServerName

				auth, _, ok := strings.Cut(hostname, ".")
				if !ok {
					log.Error("invalid hostname", "hostname", hostname)
					http.Error(w, "invalid hostname", http.StatusBadRequest)
					return
				}

				mu.Lock()
				proxy, ok := proxies[auth]
				mu.Unlock()

				if !ok {
					log.Error("proxy not found, fetching auth jwt from redis", "auth", auth)

					val := rdb.Get(ctx, fmt.Sprintf("proxy-auth-jwt-%s", auth))

					err := val.Err()
					if err != nil {
						log.Error("failed to get auth code", "error", err)
						http.Error(w, "proxy not found", http.StatusNotFound)
						return
					}

					cc := ConnectionConfig{
						ApiURL:      cfg.apiURL,
						PlatformURL: cfg.platformURL,
						JWT:         val.Val(),
					}

					proxy, err = NewClientProxy(context.Background(), cc)
					if err != nil {
						log.Error("failed to create proxy", "error", err)
						http.Error(w, "failed to create proxy", http.StatusInternalServerError)
						return
					}

					mu.Lock()
					proxies[auth] = proxy
					mu.Unlock()
				}

				proxy.ServeHTTP(w, r)

			}

			mux.HandleFunc("OPTIONS /add-client", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "POST")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
				w.Header().Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				log.Info("added client options", "path", r.URL.Path, "method", r.Method)
			})

			type StatusResponse struct {
				Version       string `json:"version"`
				ConfigVersion string `json:"config_version"`
				Status        string `json:"status"`
				ClientAddress string `json:"client_address"`
				Host          string `json:"host"`
			}

			hostname, err := os.Hostname()
			if err != nil {
				return fmt.Errorf("failed to get hostname: %w", err)
			}

			mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {

				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(
					StatusResponse{
						Version:       "0.0.0",
						ConfigVersion: "0.0.0",
						Status:        "ok",
						ClientAddress: r.RemoteAddr,
						Host:          hostname,
					},
				)

			})

			mux.HandleFunc("POST /add-client", func(w http.ResponseWriter, r *http.Request) {

				log := log.With("path", r.URL.Path, "method", r.Method)
				w.Header().Set("Access-Control-Allow-Origin", "*")

				a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
				if !ok {
					http.Error(w, "Failed to get local address", http.StatusInternalServerError)
					return
				}

				// Assert the address to *net.TCPAddr
				ta, ok := a.(*net.TCPAddr)
				if !ok {
					http.Error(w, "Unexpected address type", http.StatusInternalServerError)
					return
				}

				// Get the port
				port := ta.Port

				cr := ConnectRequest{}

				err := json.NewDecoder(r.Body).Decode(&cr)

				if err != nil {
					log.Error("failed to read body", "error", err)
					http.Error(w, fmt.Sprintf("failed to read body: %v", err), http.StatusBadRequest)
					return
				}

				id, err := uuid.NewRandom()
				if err != nil {
					log.Error("failed to generate id", "error", err)
					http.Error(w, fmt.Sprintf("failed to generate id: %v", err), http.StatusInternalServerError)
					return
				}
				ctx := r.Context()

				jwt, err := connprovider.ConvertAuthCodeToJWT(ctx, cfg.apiURL, cr.AuthCode)
				if err != nil {
					log.Error("failed to convert auth code to jwt", "error", err)
					http.Error(w, "failed to convert auth code to jwt", http.StatusUnauthorized)
					return
				}

				byjwt, err := connect.ParseByJwtUnverified(jwt)
				if err != nil {
					log.Error("failed to parse byJwt", "error", err)
					http.Error(w, "failed to parse jwt", http.StatusUnauthorized)
					return
				}

				err = rl.AllowPerMinute(ctx, fmt.Sprintf("clientid-%s", byjwt.ClientId.String()), cfg.rateLimits.networkPerMinute)
				if err == ratelimiter.ErrRateLimitExceeded {
					log.Error("rate limit exceeded", "error", err)
					http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
					return
				}

				cc := ConnectionConfig{
					ApiURL:      cfg.apiURL,
					PlatformURL: cfg.platformURL,
					JWT:         jwt,
				}

				proxy, err := NewClientProxy(context.Background(), cc)
				if err != nil {
					log.Error("failed to create proxy", "error", err)
					http.Error(w, fmt.Sprintf("failed to create proxy: %v", err), http.StatusInternalServerError)
					return
				}

				sc := rdb.Set(ctx, fmt.Sprintf("proxy-auth-jwt-%s", id.String()), jwt, time.Hour*24)
				err = sc.Err()
				if err != nil {
					log.Error("failed to set auth code", "error", err)
					http.Error(w, fmt.Sprintf("failed to set auth code: %v", err), http.StatusInternalServerError)
					return
				}

				mu.Lock()
				proxies[id.String()] = proxy
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")

				serverName := r.TLS.ServerName
				parts := strings.Split(serverName, ".")
				parts[0] = id.String()

				json.NewEncoder(w).Encode(
					ClientProxyResponse{
						Host: strings.Join(parts, "."),
						Port: uint64(port),
					},
				)
				log.Info("added client", "id", id, "host", strings.Join(parts, "."), "port", port)
			})

			server := &http.Server{
				Addr: cfg.addr,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					serverName := r.TLS.ServerName
					if strings.HasPrefix(serverName, "api.") {
						mux.ServeHTTP(w, r)
						return
					}
					proxyFunc(w, r)
				}),
				TLSConfig: &tls.Config{
					NextProtos: []string{"http/1.1"}, // Disable HTTP/2
				},
			}

			log.Info("serving requests on", "addr", cfg.addr)
			return server.ListenAndServeTLS(cfg.certFile, cfg.keyFile)

		},
	}
	app.RunAndExitOnError()

}
