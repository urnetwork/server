package model

// Where api keeps §7.9's site-loss uploads until two members agree (QD-724):
// the Redis soft-state family {acl_<networkId>}r, one hash per network in
// {acl_}v's hash tag, so a network's keys share a cluster slot and every api
// instance reads the same uploads. Only api writes it, and only while
// sidecar.yml's reseed is true.
//
//	u:<sidecar_id>  the sidecar's newest upload: its genesis, entries, rows
//	                and pins, verbatim (configReseedUploadBytes)
//	o               the uploading sidecars' ids in the order they last
//	                uploaded, 36 characters each, which bounds the hash:
//	                past MaxUploadCount uploads or MaxNetworkByteCount bytes
//	                the oldest leave
//	w               the adopted tip's position, set once the adopted run is
//	                written to proj/; it only rises
//	resumed         the marker: the position of the first base that landed
//	                past w, the network's resume base. §7.8's re-seed mode
//	                ends for the network here, and its uploads are dropped
//
// The key lives UploadTtl, 24 h, after its last write, and a read that finds
// the marker counts as one, so a network that resumed and still exchanges
// tokens stays out of re-seed mode while the restore runs. Everything here is
// soft state: an upload that expires is one its device sends again, and a lost
// marker returns its network to re-seed mode, the closed direction, until its
// devices upload and resume again.
//
// The marker is set only past w, which only an adopted run written at its own
// positions sets, so a base of a log no upload vouched for, a network founded
// during the restore or a base 1 over an emptied store, ends no re-seed mode.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// {acl_<networkId>}r, in {acl_}v's hash tag.
func configReseedKey(networkId server.Id) string {
	return "{acl_" + networkId.String() + "}r"
}

// The field of a sidecar's upload.
func configReseedUploadField(sidecarId server.Id) string {
	return configReseedFieldUploadPrefix + sidecarId.String()
}

// {acl_}r's fields: a sidecar's upload under its id's text, the order, the
// adopted tip's position and the resumed marker. The scripts below write the
// same names.
const (
	configReseedFieldUploadPrefix = "u:"
	configReseedFieldOrder        = "o"
	configReseedFieldWritten      = "w"
	configReseedFieldResumed      = "resumed"
)

// A stored upload's fields, on protobuf's wire: the genesis, each entry, each
// base's rows and each pin as an AuthPin.
const (
	configReseedUploadFieldGenesis protowire.Number = 1
	configReseedUploadFieldEntry   protowire.Number = 2
	configReseedUploadFieldRows    protowire.Number = 3
	configReseedUploadFieldPin     protowire.Number = 4
)

// An upload as its field holds it. The sidecar is the field's name.
func configReseedUploadBytes(upload *ConfigReseedUpload) ([]byte, error) {
	var b []byte
	if 0 < len(upload.SignedGenesis) {
		b = protowire.AppendTag(b, configReseedUploadFieldGenesis, protowire.BytesType)
		b = protowire.AppendBytes(b, upload.SignedGenesis)
	}
	for _, entryBytes := range upload.Entries {
		b = protowire.AppendTag(b, configReseedUploadFieldEntry, protowire.BytesType)
		b = protowire.AppendBytes(b, entryBytes)
	}
	for _, rowsBytes := range upload.Rows {
		b = protowire.AppendTag(b, configReseedUploadFieldRows, protowire.BytesType)
		b = protowire.AppendBytes(b, rowsBytes)
	}
	for _, pin := range upload.Pins {
		pinBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(pin)
		if err != nil {
			return nil, err
		}
		b = protowire.AppendTag(b, configReseedUploadFieldPin, protowire.BytesType)
		b = protowire.AppendBytes(b, pinBytes)
	}
	return b, nil
}

// The upload a field holds, or an error for bytes configReseedUploadBytes
// never wrote. Its slices alias b.
func parseConfigReseedUpload(sidecarId server.Id, b []byte) (*ConfigReseedUpload, error) {
	upload := &ConfigReseedUpload{SidecarId: sidecarId}
	for 0 < len(b) {
		number, wireType, tagByteCount := protowire.ConsumeTag(b)
		if tagByteCount < 0 {
			return nil, protowire.ParseError(tagByteCount)
		}
		if wireType != protowire.BytesType {
			return nil, fmt.Errorf("field %d is not bytes", number)
		}
		b = b[tagByteCount:]
		value, valueByteCount := protowire.ConsumeBytes(b)
		if valueByteCount < 0 {
			return nil, protowire.ParseError(valueByteCount)
		}
		b = b[valueByteCount:]
		switch number {
		case configReseedUploadFieldGenesis:
			if upload.SignedGenesis != nil {
				return nil, errors.New("two geneses")
			}
			upload.SignedGenesis = value
		case configReseedUploadFieldEntry:
			upload.Entries = append(upload.Entries, value)
		case configReseedUploadFieldRows:
			upload.Rows = append(upload.Rows, value)
		case configReseedUploadFieldPin:
			pin := &bridge.AuthPin{}
			if err := proto.Unmarshal(value, pin); err != nil {
				return nil, err
			}
			upload.Pins = append(upload.Pins, pin)
		default:
			return nil, fmt.Errorf("no field %d", number)
		}
	}
	return upload, nil
}

// KEYS[1] is {acl_}r. ARGV[1] is the sidecar's id, 36 characters, ARGV[2] its
// upload, ARGV[3] the key's ttl in seconds, ARGV[4] the most uploads to keep
// and ARGV[5] the most bytes of them. Returns 0 for a network that resumed,
// which keeps no upload, else 1. The newest upload always stays.
const configReseedPutScriptSource = `
local ttl = tonumber(ARGV[3])
if redis.call('HEXISTS', KEYS[1], 'resumed') == 1 then
    redis.call('EXPIRE', KEYS[1], ttl)
    return 0
end
local order = redis.call('HGET', KEYS[1], 'o') or ''
local kept = {}
for i = 1, #order, 36 do
    local id = string.sub(order, i, i + 35)
    if id ~= ARGV[1] then
        kept[#kept + 1] = id
    end
end
kept[#kept + 1] = ARGV[1]
redis.call('HSET', KEYS[1], 'u:' .. ARGV[1], ARGV[2])
local sizes = {}
local total = 0
for i, id in ipairs(kept) do
    sizes[i] = redis.call('HSTRLEN', KEYS[1], 'u:' .. id)
    total = total + sizes[i]
end
local maxCount = tonumber(ARGV[4])
local maxByteCount = tonumber(ARGV[5])
local first = 1
while first < #kept and (maxCount < #kept - first + 1 or maxByteCount < total) do
    redis.call('HDEL', KEYS[1], 'u:' .. kept[first])
    total = total - sizes[first]
    first = first + 1
end
redis.call('HSET', KEYS[1], 'o', table.concat(kept, '', first))
redis.call('EXPIRE', KEYS[1], ttl)
return 1
`

// KEYS[1] is {acl_}r. Drops every upload and the order, keeping w and the
// marker.
const configReseedClearScriptSource = `
local order = redis.call('HGET', KEYS[1], 'o') or ''
for i = 1, #order, 36 do
    redis.call('HDEL', KEYS[1], 'u:' .. string.sub(order, i, i + 35))
end
redis.call('HDEL', KEYS[1], 'o')
return 1
`

// KEYS[1] is {acl_}r. ARGV[1] is the adopted tip's position and ARGV[2] the
// key's ttl in seconds. w only rises. Positions are log positions, far below
// what a Lua number holds exactly.
const configReseedWrittenScriptSource = `
local w = tonumber(redis.call('HGET', KEYS[1], 'w'))
if w == nil or w < tonumber(ARGV[1]) then
    redis.call('HSET', KEYS[1], 'w', ARGV[1])
end
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
return 1
`

// KEYS[1] is {acl_}r. ARGV[1] is the position a base landed at and ARGV[2] the
// key's ttl in seconds. Sets the marker when the base lies past w, keeping the
// first base's position, and drops the uploads. Returns 1 when the network is
// resumed, 0 for a base that resumes nothing: no adopted run is written, or
// the base is one of that run's.
const configReseedResumedScriptSource = `
local w = tonumber(redis.call('HGET', KEYS[1], 'w'))
if w == nil or tonumber(ARGV[1]) <= w then
    return 0
end
if redis.call('HEXISTS', KEYS[1], 'resumed') == 0 then
    redis.call('HSET', KEYS[1], 'resumed', ARGV[1])
end
local order = redis.call('HGET', KEYS[1], 'o') or ''
for i = 1, #order, 36 do
    redis.call('HDEL', KEYS[1], 'u:' .. string.sub(order, i, i + 35))
end
redis.call('HDEL', KEYS[1], 'o')
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
return 1
`

// KEYS[1] is {acl_}r. ARGV[1] is the key's ttl in seconds. Returns 1 for a
// network that resumed, whose key then lives the ttl from this read, else 0.
const configReseedIsResumedScriptSource = `
if redis.call('HEXISTS', KEYS[1], 'resumed') == 1 then
    redis.call('EXPIRE', KEYS[1], tonumber(ARGV[1]))
    return 1
end
return 0
`

var (
	configReseedPutScript       = redis.NewScript(configReseedPutScriptSource)
	configReseedClearScript     = redis.NewScript(configReseedClearScriptSource)
	configReseedWrittenScript   = redis.NewScript(configReseedWrittenScriptSource)
	configReseedResumedScript   = redis.NewScript(configReseedResumedScriptSource)
	configReseedIsResumedScript = redis.NewScript(configReseedIsResumedScriptSource)
)

// A network's uploads as {acl_}r holds them.
type ConfigReseedHeld struct {
	// each sidecar's newest upload, in the order their sidecars last uploaded
	Uploads []*ConfigReseedUpload
	// the adopted tip's position once its run is written to proj/, else 0
	WrittenTip uint64
	// the network's resume base landed: re-seed mode ended for it
	Resumed bool
}

// Each network's uploads on server's Redis: soft state, which devices upload
// again until their network's log resumes. Holds nothing in this process, so
// every api instance sees the same uploads. Safe for concurrent use.
type ConfigReseedUploads struct {
	settings *ConfigReseedSettings
}

func NewConfigReseedUploads(settings *ConfigReseedSettings) *ConfigReseedUploads {
	if settings == nil {
		settings = DefaultConfigReseedSettings()
	}
	return &ConfigReseedUploads{
		settings: settings,
	}
}

// The key's ttl in whole seconds, at least one.
func (self *ConfigReseedUploads) ttlSeconds() int64 {
	return max(1, int64(self.settings.UploadTtl/time.Second))
}

// One script on the network's key, bounded by timeout.
func (self *ConfigReseedUploads) run(ctx context.Context, timeout time.Duration, script *redis.Script, networkId server.Id, args ...any) (result int64, returnErr error) {
	boundedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(boundedCtx, func(r server.RedisClient) error {
		var err error
		result, err = script.Run(boundedCtx, r, []string{configReseedKey(networkId)}, args...).Int64()
		return err
	})
	return
}

// Keeps upload as its sidecar's newest for the network. An upload whose stored
// form passes MaxUploadByteCount is refused with ErrConfigReseedUploadTooLarge.
// resumed is true for a network whose log resumed, which keeps no upload.
func (self *ConfigReseedUploads) Put(ctx context.Context, networkId server.Id, upload *ConfigReseedUpload) (resumed bool, returnErr error) {
	uploadBytes, err := configReseedUploadBytes(upload)
	if err != nil {
		return false, err
	}
	if self.settings.MaxUploadByteCount < int64(len(uploadBytes)) {
		return false, ErrConfigReseedUploadTooLarge
	}
	kept, err := self.run(
		ctx,
		self.settings.RedisTimeout,
		configReseedPutScript,
		networkId,
		upload.SidecarId.String(),
		uploadBytes,
		self.ttlSeconds(),
		max(1, self.settings.MaxUploadCount),
		self.settings.MaxNetworkByteCount,
	)
	if err != nil {
		return false, err
	}
	return kept == 0, nil
}

// What the network's key holds. A field that holds no upload counts for
// nothing.
func (self *ConfigReseedUploads) List(ctx context.Context, networkId server.Id) (*ConfigReseedHeld, error) {
	var fieldValues map[string]string
	boundedCtx, cancel := context.WithTimeout(ctx, self.settings.RedisTimeout)
	defer cancel()
	err := server.RedisWithDeadline(boundedCtx, func(r server.RedisClient) error {
		var err error
		fieldValues, err = r.HGetAll(boundedCtx, configReseedKey(networkId)).Result()
		return err
	})
	if err != nil {
		return nil, err
	}
	held := &ConfigReseedHeld{}
	if written, found := fieldValues[configReseedFieldWritten]; found {
		held.WrittenTip, _ = strconv.ParseUint(written, 10, 64)
	}
	_, held.Resumed = fieldValues[configReseedFieldResumed]
	order := fieldValues[configReseedFieldOrder]
	idTextLen := len(server.Id{}.String())
	for i := 0; i+idTextLen <= len(order); i += idTextLen {
		idText := order[i : i+idTextLen]
		sidecarId, err := server.ParseId(idText)
		if err != nil {
			continue
		}
		uploadBytes, found := fieldValues[configReseedUploadField(sidecarId)]
		if !found {
			continue
		}
		upload, err := parseConfigReseedUpload(sidecarId, []byte(uploadBytes))
		if err != nil {
			glog.Warningf("[config]%s {acl_}r holds no upload for %s: %v\n", networkId, sidecarId, err)
			continue
		}
		held.Uploads = append(held.Uploads, upload)
	}
	return held, nil
}

// Drops the network's uploads: a run that failed its walk from C_0 is
// discarded.
func (self *ConfigReseedUploads) Clear(ctx context.Context, networkId server.Id) error {
	_, err := self.run(ctx, self.settings.MarkerRedisTimeout, configReseedClearScript, networkId)
	return err
}

// Records that the adopted run through tip is written to proj/, so the base
// that lands past it resumes the network.
func (self *ConfigReseedUploads) MarkWritten(ctx context.Context, networkId server.Id, tip uint64) error {
	_, err := self.run(ctx, self.settings.MarkerRedisTimeout, configReseedWrittenScript, networkId, strconv.FormatUint(tip, 10), self.ttlSeconds())
	return err
}

// A base landed at n: sets the resumed marker and drops the uploads when the
// network's adopted run is written and ends below n. Answers whether the
// network is resumed.
func (self *ConfigReseedUploads) MarkResumed(ctx context.Context, networkId server.Id, n uint64) (bool, error) {
	resumed, err := self.run(ctx, self.settings.MarkerRedisTimeout, configReseedResumedScript, networkId, strconv.FormatUint(n, 10), self.ttlSeconds())
	return resumed == 1, err
}

// Whether the network's resume base landed. A read that finds the marker keeps
// its key another UploadTtl.
func (self *ConfigReseedUploads) Resumed(ctx context.Context, networkId server.Id) (bool, error) {
	resumed, err := self.run(ctx, self.settings.MarkerRedisTimeout, configReseedIsResumedScript, networkId, self.ttlSeconds())
	return resumed == 1, err
}

// api's uploads, with the default settings.
var configReseedUploads = NewConfigReseedUploads(nil)

// Whether §7.8's re-seed mode ended for the network: its resume base landed
// (QD-724). Read only while sidecar.yml's reseed is true, the switch that
// allows re-seed mode at all. An error is Redis's, and decides nothing.
func ConfigReseedResumed(ctx context.Context, networkId server.Id) (bool, error) {
	return configReseedUploads.Resumed(ctx, networkId)
}

// The sequencer's BaseLanded on server's Redis: while sidecar.yml's reseed is
// true, a base that lands past a network's adopted run sets its resumed marker.
// Best effort: a marker Redis did not take leaves the network in re-seed mode,
// the closed direction, until the base's retry or the next base.
func NewConfigReseedBaseLanded(settings *ConfigReseedSettings) func(ctx context.Context, networkId server.Id, pin *bridge.AuthPin) {
	uploads := NewConfigReseedUploads(settings)
	return func(ctx context.Context, networkId server.Id, pin *bridge.AuthPin) {
		if !server.CurrentSidecarConfig().Reseed() {
			return
		}
		resumed, err := uploads.MarkResumed(ctx, networkId, pin.GetVersion())
		if err != nil {
			glog.Warningf("[config]%s {acl_}r misses the base at %d: %v\n", networkId, pin.GetVersion(), err)
			return
		}
		if resumed {
			glog.Infof("[config]%s resumed at %d\n", networkId, pin.GetVersion())
		}
	}
}
