package jwt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v3"
	"github.com/pquerna/cachecontrol"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
)

const JwkRefreshTimeout = 30 * time.Minute
const JwkParseRetryTimeout = 5 * time.Second

// actively refreshed based on the `Cache-Control` of the JWK response
type JwkValidator struct {
	ctx    context.Context
	cancel context.CancelFunc
	name   string
	jwkUrl string

	stateLock  *sync.Mutex
	keySetCond *sync.Cond
	keySet     *jose.JSONWebKeySet
}

func NewJwkValidator(ctx context.Context, name string, jwkUrl string) *JwkValidator {
	cancelCtx, cancel := context.WithCancel(ctx)

	stateLock := &sync.Mutex{}
	keySetCond := sync.NewCond(stateLock)
	validator := &JwkValidator{
		ctx:        cancelCtx,
		cancel:     cancel,
		name:       name,
		jwkUrl:     jwkUrl,
		stateLock:  stateLock,
		keySetCond: keySetCond,
		keySet:     nil,
	}
	go validator.refresh()
	return validator
}

// type https://pkg.go.dev/crypto/rsa#PrivateKey.Public
func (self *JwkValidator) Keys() []any {
	// RsaPublicKeys iterate Keys in JSONWebKeySet
	// key.Key

	select {
	case <-self.ctx.Done():
		return []any{}
	default:
	}

	keySet := func() *jose.JSONWebKeySet {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		for self.keySet == nil {
			self.keySetCond.Wait()
		}
		return self.keySet
	}()

	var keys []any
	for _, key := range keySet.Keys {
		keys = append(keys, key.Key)
	}
	return keys
}

func (self *JwkValidator) refresh() {
	defer self.cancel()

	parseKeySet := func() (returnKeySet *jose.JSONWebKeySet, returnExpireTime time.Time, returnErr error) {
		req, err := http.NewRequest(
			"GET",
			self.jwkUrl,
			nil,
		)
		if err != nil {
			returnErr = err
			return
		}

		header := req.Header
		header.Add("Content-Type", "application/json")

		client := server.DefaultHttpClient()

		res, err := client.Do(req)
		if err != nil {
			returnErr = err
			return
		}

		responseBodyBytes, err := io.ReadAll(res.Body)
		if err != nil {
			returnErr = err
			return
		}

		var keySet jose.JSONWebKeySet
		err = json.Unmarshal(responseBodyBytes, &keySet)
		if err != nil {
			returnErr = err
			return
		}
		returnKeySet = &keySet

		// look at the `Cache-Control` header to find the expiration time
		reasons, expireTime, _ := cachecontrol.CachableResponse(req, res, cachecontrol.Options{})
		if 0 < len(reasons) {
			returnExpireTime = time.Now().Add(JwkRefreshTimeout)
		} else {
			returnExpireTime = expireTime
		}

		return
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		keySet, expireTime, err := parseKeySet()

		var timeout time.Duration
		if err != nil {
			// retry
			timeout = JwkParseRetryTimeout
		} else {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()

				self.keySet = keySet
				self.keySetCond.Broadcast()
			}()
			glog.Infof("[jwk]%s updated\n", self.name)

			timeout = expireTime.Sub(time.Now())
			if timeout <= 0 {
				timeout = JwkRefreshTimeout
			}
		}
		glog.Infof("[jwk]%s refresh in %.2fh\n", self.name, (float64(timeout) / float64(time.Hour)))
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(timeout):
		}
	}
}
