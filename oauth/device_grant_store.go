package oauth

// Device authorizations on Redis (QD-830; §2.8's api-11): where api keeps RFC 8628's device authorizations between
// their steps (device_grant.go), as model keeps a team IdP sign-in's flow (QD-720). Each authorization is two keys,
// each key the whole of its hash tag, so each is one key in one slot:
//
//	{oauthdev_<device code's SHA-256, hex>}  the authorization's JSON, with a ttl of its remaining life
//	{oauthuser_<user code>}                    the device code's SHA-256 in hex, with the same ttl: the consent page
//	                                           finds the authorization by the code the person typed
//
// Every write is one command on one key and is never replayed, since a write whose reply was lost may have been
// applied:
//
//	put   the authorization by SET NX, then its user code's pointer by SET NX, so a user code names at most one live
//	      authorization; when the pointer is another's, the authorization is removed again while it is still the value
//	      written, and the grant draws another code
//	swap  a script on the authorization's key replaces it only while it holds the value that was read, keeping its
//	      ttl, so the consent page's answer and a poll's bookkeeping never undo each other
//	take  GETDEL, so of any number of polls on any api instance at most one is answered the authorization; its pointer
//	      then goes, while it still names it
//
// A key Redis expired is gone, and the poll answers expired_token for it (device_grant.go), as for an authorization
// past its life; the store's own clock also ends an authorization its key outlived. The state is soft (§19.7): a lost
// Redis loses only the sign-ins in flight, which start again. No error here quotes a stored value, and nothing here
// logs a device code, a user code or a token.
//
// Safe for concurrent use.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
)

// Replaces KEYS[1]'s value with ARGV[2], keeping its ttl, only while the value is ARGV[1]: 1 when it replaced it,
// else 0, a missing key included.
const redisDeviceAuthorizationSwapScript = `if redis.call('GET', KEYS[1]) == ARGV[1] then redis.call('SET', KEYS[1], ARGV[2], 'KEEPTTL') return 1 end return 0`

// The key an authorization waits under (QD-830; §2.8's api-11).
func redisDeviceAuthorizationKey(deviceCodeSha256 [sha256.Size]byte) string {
	return "{oauthdev_" + hex.EncodeToString(deviceCodeSha256[:]) + "}"
}

// The key of a user code's pointer at its authorization. Only a code validStoredDeviceUserCode passes reaches it.
func redisDeviceUserCodeKey(userCode string) string {
	return "{oauthuser_" + userCode + "}"
}

// Whether userCode is a user code as the grant stores one: settings' length of the alphabet's letters, uppercase, no
// hyphen. A user code the consent page sends is the caller's text, and nothing else of it reaches a key.
func validStoredDeviceUserCode(userCode string, settings *DeviceGrantSettings) bool {
	normalized, ok := normalizeDeviceUserCode(userCode, settings)
	return ok && normalized == userCode
}

// An authorization as Redis holds it, §2.8 api-11's row: the device code's SHA-256, the user code, the ur-cli request
// with its DPoP key's thumbprint and command digest, the life and the poll times, and once decided the answer with the
// approving user, network and sign-in time.
type redisDeviceAuthorization struct {
	DeviceCodeSha256     string    `json:"device_code_sha256"`
	UserCode             string    `json:"user_code"`
	ClientId             string    `json:"client_id"`
	Scopes               []string  `json:"scopes"`
	Resource             string    `json:"resource"`
	DpopJkt              string    `json:"dpop_jkt"`
	AuthorizationDetails string    `json:"authorization_details"`
	Cmd                  string    `json:"cmd"`
	ExpireTime           time.Time `json:"expire_time"`
	IntervalMillis       int64     `json:"interval_ms"`
	LastPollTime         time.Time `json:"last_poll_time,omitzero"`
	Approved             bool      `json:"approved,omitempty"`
	Denied               bool      `json:"denied,omitempty"`
	UserId               server.Id `json:"user_id,omitzero"`
	NetworkId            server.Id `json:"network_id,omitzero"`
	AuthTime             time.Time `json:"auth_time,omitzero"`
}

// The stored form of an authorization, its times in UTC so equal authorizations store equal bytes.
func encodeRedisDeviceAuthorization(authorization *DeviceAuthorization) ([]byte, error) {
	utc := func(t time.Time) time.Time {
		if t.IsZero() {
			return time.Time{}
		}
		return t.UTC()
	}
	return json.Marshal(&redisDeviceAuthorization{
		DeviceCodeSha256:     hex.EncodeToString(authorization.DeviceCodeSha256[:]),
		UserCode:             authorization.UserCode,
		ClientId:             authorization.ClientId,
		Scopes:               authorization.Scopes,
		Resource:             authorization.Resource,
		DpopJkt:              authorization.DpopJkt,
		AuthorizationDetails: authorization.AuthorizationDetails,
		Cmd:                  authorization.Cmd,
		ExpireTime:           utc(authorization.ExpireTime),
		IntervalMillis:       authorization.Interval.Milliseconds(),
		LastPollTime:         utc(authorization.LastPollTime),
		Approved:             authorization.Approved,
		Denied:               authorization.Denied,
		UserId:               authorization.UserId,
		NetworkId:            authorization.NetworkId,
		AuthTime:             utc(authorization.AuthTime),
	})
}

// The authorization a stored value holds, which must be the one its key names, with the value as its version. An
// error never quotes the value.
func decodeRedisDeviceAuthorization(value []byte, deviceCodeSha256 [sha256.Size]byte, settings *DeviceGrantSettings) (*DeviceAuthorization, error) {
	stored := &redisDeviceAuthorization{}
	if err := json.Unmarshal(value, stored); err != nil {
		return nil, errors.New("a device authorization that does not parse")
	}
	if stored.DeviceCodeSha256 != hex.EncodeToString(deviceCodeSha256[:]) {
		return nil, errors.New("a device authorization under another device code's key")
	}
	if !validStoredDeviceUserCode(stored.UserCode, settings) {
		return nil, errors.New("a device authorization with no user code")
	}
	return &DeviceAuthorization{
		DeviceCodeSha256:     deviceCodeSha256,
		UserCode:             stored.UserCode,
		ClientId:             stored.ClientId,
		Scopes:               stored.Scopes,
		Resource:             stored.Resource,
		DpopJkt:              stored.DpopJkt,
		AuthorizationDetails: stored.AuthorizationDetails,
		Cmd:                  stored.Cmd,
		ExpireTime:           stored.ExpireTime,
		Interval:             time.Duration(stored.IntervalMillis) * time.Millisecond,
		LastPollTime:         stored.LastPollTime,
		Approved:             stored.Approved,
		Denied:               stored.Denied,
		UserId:               stored.UserId,
		NetworkId:            stored.NetworkId,
		AuthTime:             stored.AuthTime,
		storeVersion:         value,
	}, nil
}

// Runs one write of the store, never replayed: a write whose reply was lost may have been applied, and the caller is
// told so rather than given a second run's answer. A raise is returned as its error.
func deviceAuthorizationRedisWrite(ctx context.Context, callback func(server.RedisClient) error) (returnErr error) {
	server.HandleError(func() {
		server.RedisDoOnce(ctx, func(r server.RedisClient) {
			returnErr = callback(r)
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// The value of key, nil for none, read on the pool that retries a lost connection: a read changes nothing to replay.
func deviceAuthorizationRedisGet(ctx context.Context, key string) (value []byte, returnErr error) {
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			value, err = r.Get(ctx, key).Bytes()
			if errors.Is(err, server.RedisNil) {
				value = nil
				return
			}
			server.Raise(err)
		})
	}, func(err error) {
		value = nil
		returnErr = err
	})
	return
}

// The device authorization store on Redis (the file header). Production's: api installs one at startup
// (InstallDeviceAuthorizationStore).
type RedisDeviceAuthorizationStore struct {
	settings *DeviceGrantSettings
	now      atomic.Pointer[func() time.Time]
}

func NewRedisDeviceAuthorizationStore(settings *DeviceGrantSettings) *RedisDeviceAuthorizationStore {
	return &RedisDeviceAuthorizationStore{settings: settings}
}

// Installs the Redis store for this process's device grant, with the grant's default settings, so POST
// /oauth/device_authorization serves and the issuer's metadata names it (QD-830). Each process that serves api's
// routes calls it at startup, api's and alt's (controller.InstallApiRoutes). Answers what puts the previous store
// back.
func InstallDeviceAuthorizationStore() (restore func()) {
	var store DeviceAuthorizationStore = NewRedisDeviceAuthorizationStore(DefaultDeviceGrantSettings())
	previous := deviceAuthorizationStore.Swap(&store)
	return func() { deviceAuthorizationStore.Store(previous) }
}

// Replaces the store's clock for a test.
func (self *RedisDeviceAuthorizationStore) Testing_SetNow(now func() time.Time) {
	self.now.Store(&now)
}

func (self *RedisDeviceAuthorizationStore) nowUtc() time.Time {
	if now := self.now.Load(); now != nil {
		return (*now)()
	}
	return server.NowUtc()
}

// Removes key while it holds value, answering whether it did; a write, never replayed.
func (self *RedisDeviceAuthorizationStore) removeIfStill(ctx context.Context, key string, value []byte) (bool, error) {
	removed := false
	err := deviceAuthorizationRedisWrite(ctx, func(r server.RedisClient) error {
		count, err := server.RedisRemoveIfEqual(r, ctx, key, value).Int64()
		removed = count == 1
		return err
	})
	return removed, err
}

// Keeps a new authorization under its device code's hash and its user code's pointer, each with a ttl of the
// authorization's remaining life. A user code that is no stored code, a life already over, which Redis would read as
// no expiry, and a device code in use are refused; a user code another live authorization holds is refused with
// ErrDeviceUserCodeInUse, and the authorization written is removed again.
func (self *RedisDeviceAuthorizationStore) Put(ctx context.Context, authorization *DeviceAuthorization) error {
	if authorization == nil || !validStoredDeviceUserCode(authorization.UserCode, self.settings) {
		return errors.New("a device authorization needs a user code")
	}
	ttl := authorization.ExpireTime.Sub(self.nowUtc())
	if ttl <= 0 {
		return errors.New("a device authorization needs a life ahead of it")
	}
	value, err := encodeRedisDeviceAuthorization(authorization)
	if err != nil {
		return err
	}
	key := redisDeviceAuthorizationKey(authorization.DeviceCodeSha256)
	// the authorization first, so a pointer never names an authorization not yet written
	stored := false
	err = deviceAuthorizationRedisWrite(ctx, func(r server.RedisClient) error {
		var err error
		stored, err = r.SetNX(ctx, key, value, ttl).Result()
		return err
	})
	if err != nil {
		return err
	}
	if !stored {
		return errors.New("a device code already in use")
	}
	pointed := false
	err = deviceAuthorizationRedisWrite(ctx, func(r server.RedisClient) error {
		var err error
		pointed, err = r.SetNX(ctx, redisDeviceUserCodeKey(authorization.UserCode), hex.EncodeToString(authorization.DeviceCodeSha256[:]), ttl).Result()
		return err
	})
	if err == nil && pointed {
		return nil
	}
	// nobody is answered this device code, so it goes again; a removal that fails leaves it to its ttl, unreachable
	// by a user code another holds, and a pointer that a lost reply wrote names nothing once it goes
	self.removeIfStill(ctx, key, value)
	if err != nil {
		return err
	}
	return ErrDeviceUserCodeInUse
}

// The authorization of a device code's hash, nil for none: never kept, taken, or past its life by the store's clock.
func (self *RedisDeviceAuthorizationStore) Get(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (*DeviceAuthorization, error) {
	value, err := deviceAuthorizationRedisGet(ctx, redisDeviceAuthorizationKey(deviceCodeSha256))
	if err != nil || value == nil {
		return nil, err
	}
	authorization, err := decodeRedisDeviceAuthorization(value, deviceCodeSha256, self.settings)
	if err != nil {
		return nil, err
	}
	if !self.nowUtc().Before(authorization.ExpireTime) {
		// Redis's ttl ends the key; this ends an authorization the key outlived
		return nil, nil
	}
	return authorization, nil
}

// The authorization a user code's pointer names, nil for none. A code that is no stored code reaches no key, and a
// pointer at another code's authorization is an error, never an authorization.
func (self *RedisDeviceAuthorizationStore) GetByUserCode(ctx context.Context, userCode string) (*DeviceAuthorization, error) {
	if !validStoredDeviceUserCode(userCode, self.settings) {
		return nil, nil
	}
	pointer, err := deviceAuthorizationRedisGet(ctx, redisDeviceUserCodeKey(userCode))
	if err != nil || pointer == nil {
		return nil, err
	}
	deviceCodeSha256 := [sha256.Size]byte{}
	if len(pointer) != hex.EncodedLen(sha256.Size) {
		return nil, errors.New("a user code's pointer that does not read")
	}
	if _, err := hex.Decode(deviceCodeSha256[:], pointer); err != nil {
		return nil, errors.New("a user code's pointer that does not read")
	}
	authorization, err := self.Get(ctx, deviceCodeSha256)
	if err != nil || authorization == nil {
		return nil, err
	}
	if authorization.UserCode != userCode {
		return nil, errors.New("a user code's pointer at another code's authorization")
	}
	return authorization, nil
}

// Replaces read with next, keeping the key's ttl, while the key holds the value read was answered from (the file
// header): false once another write came between, or the key is gone.
func (self *RedisDeviceAuthorizationStore) Swap(ctx context.Context, read *DeviceAuthorization, next *DeviceAuthorization) (bool, error) {
	if read == nil || next == nil || read.storeVersion == nil {
		return false, errors.New("a swap replaces an authorization the store answered")
	}
	if next.DeviceCodeSha256 != read.DeviceCodeSha256 || next.UserCode != read.UserCode || !next.ExpireTime.Equal(read.ExpireTime) {
		return false, errors.New("a swap keeps the authorization's device code, user code and life")
	}
	value, err := encodeRedisDeviceAuthorization(next)
	if err != nil {
		return false, err
	}
	swapped := false
	err = deviceAuthorizationRedisWrite(ctx, func(r server.RedisClient) error {
		result, err := r.Eval(ctx, redisDeviceAuthorizationSwapScript, []string{redisDeviceAuthorizationKey(read.DeviceCodeSha256)}, read.storeVersion, value).Int64()
		swapped = result == 1
		return err
	})
	if err != nil {
		return false, err
	}
	return swapped, nil
}

// Removes the authorization of a device code's hash by GETDEL and answers it, nil for none: of any number of takes
// at most one is answered it. Its user code's pointer goes too, while it still names it. A value that does not read
// as this code's authorization is an error, and is gone.
func (self *RedisDeviceAuthorizationStore) Take(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (*DeviceAuthorization, error) {
	var value []byte
	err := deviceAuthorizationRedisWrite(ctx, func(r server.RedisClient) error {
		var err error
		value, err = r.GetDel(ctx, redisDeviceAuthorizationKey(deviceCodeSha256)).Bytes()
		if errors.Is(err, server.RedisNil) {
			value = nil
			return nil
		}
		return err
	})
	if err != nil || value == nil {
		return nil, err
	}
	authorization, err := decodeRedisDeviceAuthorization(value, deviceCodeSha256, self.settings)
	if err != nil {
		return nil, err
	}
	// a pointer whose removal fails names nothing until its ttl, which is the authorization's
	self.removeIfStill(ctx, redisDeviceUserCodeKey(authorization.UserCode), []byte(hex.EncodeToString(deviceCodeSha256[:])))
	if !self.nowUtc().Before(authorization.ExpireTime) {
		return nil, nil
	}
	return authorization, nil
}
