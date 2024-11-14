package client

import (
	"context"
	"fmt"

	// "io"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	gojwt "github.com/golang-jwt/jwt/v5"

	"bringyour.com/connect"
)

var lsLog = logFn("local_state")

const AsyncQueueSize = 32

const LocalStorageFilePermissions = 0700

type ByJwt struct {
	UserId      *Id
	NetworkName string
	NetworkId   *Id
	GuestMode   bool
}

type LocalState struct {
	ctx    context.Context
	cancel context.CancelFunc

	localStorageDir string
}

func newLocalState(ctx context.Context, localStorageHome string) *LocalState {
	// FIXME local storage dir is always a sub dir of the passed dir
	// localStorageHome/.by
	localStorageDir := filepath.Join(localStorageHome, ".by")
	err := os.MkdirAll(localStorageDir, LocalStorageFilePermissions)
	if err != nil {
		panic(err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)

	return &LocalState{
		ctx:             cancelCtx,
		cancel:          cancel,
		localStorageDir: localStorageDir,
	}
}

func (self *LocalState) GetByJwt() string {
	path := filepath.Join(self.localStorageDir, ".by_jwt")
	if byJwtBytes, err := os.ReadFile(path); err == nil {
		return string(byJwtBytes)
	}
	return ""
}

func (self *LocalState) ParseByJwt() (*ByJwt, error) {
	byJwtStr := self.GetByJwt()
	if byJwtStr == "" {
		return nil, errors.New("Not found.")
	}

	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byJwtStr, gojwt.MapClaims{})
	if err != nil {
		return nil, err
	}

	claims := token.Claims.(gojwt.MapClaims)

	byJwt := &ByJwt{}

	if userIdStr, ok := claims["user_id"]; ok {
		if userId, err := ParseId(userIdStr.(string)); err == nil {
			byJwt.UserId = userId
		}
	}
	if networkName, ok := claims["network_name"]; ok {
		byJwt.NetworkName = networkName.(string)
	}
	if networkIdStr, ok := claims["network_name"]; ok {
		if networkId, err := ParseId(networkIdStr.(string)); err == nil {
			byJwt.NetworkId = networkId
		}
	}
	if guestMode, ok := claims["guest_mode"]; ok {
		byJwt.GuestMode = guestMode.(bool)
	}

	return byJwt, nil
}

// clears `byClientJwt` and `instanceId`
func (self *LocalState) SetByJwt(byJwt string) error {
	path := filepath.Join(self.localStorageDir, ".by_jwt")

	if existingByJwtBytes, err := os.ReadFile(path); err == nil {
		if string(existingByJwtBytes) == byJwt {
			// already set, no need to clear state
			return nil
		}
	}

	self.SetByClientJwt("")

	if byJwt == "" {
		os.Remove(path)
		return nil
	} else {
		return os.WriteFile(path, []byte(byJwt), LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetByClientJwt() string {
	path := filepath.Join(self.localStorageDir, ".by_client_jwt")
	if byClientJwtBytes, err := os.ReadFile(path); err == nil {
		return string(byClientJwtBytes)
	}
	return ""
}

// if `byClientJwt` is set, sets a new `instanceId`; othewwise, clears `instanceId`
func (self *LocalState) SetByClientJwt(byClientJwt string) error {
	path := filepath.Join(self.localStorageDir, ".by_client_jwt")

	if existingByClientJwtBytes, err := os.ReadFile(path); err == nil {
		if string(existingByClientJwtBytes) == byClientJwt {
			// already set, no need to clear state
			return nil
		}
	}

	if byClientJwt == "" {
		self.SetInstanceId(nil)
		os.Remove(path)
		return nil
	} else {
		instanceId := connect.NewId()
		self.SetInstanceId(newId(instanceId))
		return os.WriteFile(path, []byte(byClientJwt), LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetInstanceId() *Id {
	path := filepath.Join(self.localStorageDir, ".instance_id")
	if instanceIdBytes, err := os.ReadFile(path); err == nil {
		if instanceId, err := connect.IdFromBytes(instanceIdBytes); err == nil {
			return newId(instanceId)
		}
	}
	return nil
}

func (self *LocalState) SetInstanceId(instanceId *Id) error {
	path := filepath.Join(self.localStorageDir, ".instance_id")
	if instanceId == nil {
		os.Remove(path)
		return nil
	} else {
		return os.WriteFile(path, instanceId.Bytes(), LocalStorageFilePermissions)
	}
}

func (self *LocalState) SetProvideMode(provideMode ProvideMode) error {
	path := filepath.Join(self.localStorageDir, ".provide_mode")
	provideModeBytes := []byte(fmt.Sprintf("%d", provideMode))
	return os.WriteFile(path, provideModeBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetProvideMode() ProvideMode {
	path := filepath.Join(self.localStorageDir, ".provide_mode")
	if provideModeBytes, err := os.ReadFile(path); err == nil {
		var provideMode ProvideMode
		if _, err := fmt.Sscanf(string(provideModeBytes), "%d", &provideMode); err == nil {
			return provideMode
		}
	}
	return ProvideModeNone
}

func (self *LocalState) SetRouteLocal(routeLocal bool) error {
	path := filepath.Join(self.localStorageDir, ".route_local")
	routeLocalBytes := []byte(fmt.Sprintf("%t", routeLocal))
	return os.WriteFile(path, routeLocalBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetRouteLocal() bool {
	path := filepath.Join(self.localStorageDir, ".route_local")
	if routeLocalBytes, err := os.ReadFile(path); err == nil {
		var routeLocal bool
		if _, err := fmt.Sscanf(string(routeLocalBytes), "%t", &routeLocal); err == nil {
			return routeLocal
		}
	}
	return true
}

func (self *LocalState) SetConnectLocation(connectLocation *ConnectLocation) error {
	path := filepath.Join(self.localStorageDir, ".connect_location")
	if connectLocation == nil {
		os.Remove(path)
		return nil
	} else {
		connectLocationBytes, err := json.Marshal(connectLocation)
		if err != nil {
			return err
		}
		return os.WriteFile(path, connectLocationBytes, LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetConnectLocation() *ConnectLocation {
	path := filepath.Join(self.localStorageDir, ".connect_location")
	if connectLocationBytes, err := os.ReadFile(path); err == nil {
		var connectLocation ConnectLocation
		if err := json.Unmarshal(connectLocationBytes, &connectLocation); err == nil {
			return &connectLocation
		}
	}
	return nil
}

func (self *LocalState) SetProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) error {
	path := filepath.Join(self.localStorageDir, ".provide_secret_keys")
	if provideSecretKeyList == nil {
		os.Remove(path)
		return nil
	} else {
		provideSecretKeysBytes, err := json.Marshal(provideSecretKeyList)
		if err != nil {
			return err
		}
		return os.WriteFile(path, provideSecretKeysBytes, LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetProvideSecretKeys() *ProvideSecretKeyList {
	path := filepath.Join(self.localStorageDir, ".provide_secret_keys")
	if connectLocationBytes, err := os.ReadFile(path); err == nil {
		var provideSecretKeys ProvideSecretKeyList
		if err := json.Unmarshal(connectLocationBytes, &provideSecretKeys); err == nil {
			return &provideSecretKeys
		}
	}
	return nil
}

func (self *LocalState) SetCanShowRatingDialog(canShowRatingDialog bool) error {
	path := filepath.Join(self.localStorageDir, ".can_show_rating_dialog")
	canShowRatingDialogBytes, err := json.Marshal(canShowRatingDialog)
	if err != nil {
		return err
	}
	return os.WriteFile(path, canShowRatingDialogBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetCanShowRatingDialog() bool {
	path := filepath.Join(self.localStorageDir, ".can_show_rating_dialog")
	if canShowRatingDialogBytes, err := os.ReadFile(path); err == nil {
		var canShowRatingDialog bool
		if err := json.Unmarshal(canShowRatingDialogBytes, &canShowRatingDialog); err == nil {
			return canShowRatingDialog
		}
	}
	return true
}

func (self *LocalState) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) error {
	path := filepath.Join(self.localStorageDir, ".vpn_interface_while_offline")
	vpnInterfaceWhileOfflineBytes, err := json.Marshal(vpnInterfaceWhileOffline)
	if err != nil {
		return err
	}
	return os.WriteFile(path, vpnInterfaceWhileOfflineBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetVpnInterfaceWhileOffline() bool {
	path := filepath.Join(self.localStorageDir, ".vpn_interface_while_offline")
	if vpnInterfaceWhileOfflineBytes, err := os.ReadFile(path); err == nil {
		var vpnInterfaceWhileOffline bool
		if err := json.Unmarshal(vpnInterfaceWhileOfflineBytes, &vpnInterfaceWhileOffline); err == nil {
			return vpnInterfaceWhileOffline
		}
	}
	return false
}

func (self *LocalState) SetProvideWhileDisconnected(provideWhileDisconnected bool) error {
	path := filepath.Join(self.localStorageDir, ".provide_while_disconnected")
	provideWhileDisconnectedBytes, err := json.Marshal(provideWhileDisconnected)
	if err != nil {
		return err
	}
	return os.WriteFile(path, provideWhileDisconnectedBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetProvideWhileDisconnected() bool {
	path := filepath.Join(self.localStorageDir, ".provide_while_disconnected")
	if provideWhileDisconnectedBytes, err := os.ReadFile(path); err == nil {
		var provideWhileDisconnected bool
		if err := json.Unmarshal(provideWhileDisconnectedBytes, &provideWhileDisconnected); err == nil {
			return provideWhileDisconnected
		}
	}
	return false
}

func (self *LocalState) SetCanRefer(canRefer bool) error {
	path := filepath.Join(self.localStorageDir, ".can_refer")
	canReferBytes, err := json.Marshal(canRefer)
	if err != nil {
		return err
	}
	return os.WriteFile(path, canReferBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetCanRefer() bool {
	path := filepath.Join(self.localStorageDir, ".can_refer")
	if canReferBytes, err := os.ReadFile(path); err == nil {
		var canRefer bool
		if err := json.Unmarshal(canReferBytes, &canRefer); err == nil {
			return canRefer
		}
	}
	return false
}

// clears all auth tokens
func (self *LocalState) Logout() error {
	return errors.Join(
		os.RemoveAll(self.localStorageDir),
		os.MkdirAll(self.localStorageDir, LocalStorageFilePermissions),
	)
}

func (self *LocalState) Close() {
	self.cancel()
}

type CommitCallback interface {
	Complete(success bool)
}

type singleResultCallback[R any] interface {
	Result(result R, ok bool)
}

type GetByJwtCallback interface {
	singleResultCallback[string]
}

type ParseByJwtCallback interface {
	singleResultCallback[*ByJwt]
}

type GetByClientJwtCallback interface {
	singleResultCallback[string]
}

type GetInstanceIdCallback interface {
	singleResultCallback[*Id]
}

type AsyncLocalState struct {
	ctx    context.Context
	cancel context.CancelFunc

	localState *LocalState

	jobs chan *job
}

func NewAsyncLocalState(localStorageHome string) *AsyncLocalState {
	cancelCtx, cancel := context.WithCancel(context.Background())

	localState := newLocalState(cancelCtx, localStorageHome)

	asyncLocalState := &AsyncLocalState{
		ctx:        cancelCtx,
		cancel:     cancel,
		localState: localState,
		jobs:       make(chan *job, AsyncQueueSize),
	}
	go connect.HandleError(asyncLocalState.run)

	return asyncLocalState
}

func (self *AsyncLocalState) run() {
	defer func() {
		self.cancel()

		// drain the jobs
		func() {
			for {
				select {
				case job, ok := <-self.jobs:
					if !ok {
						return
					}
					for _, callback := range job.callbacks {
						callback.Complete(false)
					}
				}
			}
		}()
	}()
	for {
		select {
		case <-self.ctx.Done():
			return
		case job, ok := <-self.jobs:
			if !ok {
				return
			}
			func() {
				defer func() {
					if err := recover(); err != nil {
						for _, callback := range job.callbacks {
							callback.Complete(false)
						}
					}
				}()
				err := job.work()
				for _, callback := range job.callbacks {
					success := err == nil
					callback.Complete(success)
				}
			}()
		}
	}
}

func (self *AsyncLocalState) serialAsync(work func() error, callbacks ...CommitCallback) {
	job := &job{
		work:      work,
		callbacks: callbacks,
	}
	select {
	case <-self.ctx.Done():
		for _, callback := range callbacks {
			callback.Complete(false)
		}
	case self.jobs <- job:
	}
}

// get the sync local state
func (self *AsyncLocalState) GetLocalState() *LocalState {
	return self.localState
}

func (self *AsyncLocalState) GetByJwt(callback GetByJwtCallback) {
	self.serialAsync(func() error {
		byJwt := self.localState.GetByJwt()
		callback.Result(byJwt, byJwt != "")
		return nil
	})
}

func (self *AsyncLocalState) ParseByJwt(callback ParseByJwtCallback) {
	self.serialAsync(func() error {
		byJwt, err := self.localState.ParseByJwt()
		if err == nil {
			callback.Result(byJwt, true)
		} else {
			callback.Result(nil, false)
		}
		return nil
	})
}

// clears the clientjwt and instanceid if differnet
func (self *AsyncLocalState) SetByJwt(byJwt string, callback CommitCallback) {
	self.serialAsync(func() error {
		return self.localState.SetByJwt(byJwt)
	}, callback)
}

func (self *AsyncLocalState) GetByClientJwt(callback GetByClientJwtCallback) {
	self.serialAsync(func() error {
		byClientJwt := self.localState.GetByClientJwt()
		callback.Result(byClientJwt, byClientJwt != "")
		return nil
	})
}

func (self *AsyncLocalState) SetByClientJwt(byClientJwt string, callback CommitCallback) {
	self.serialAsync(func() error {
		return self.localState.SetByClientJwt(byClientJwt)
	}, callback)
}

func (self *AsyncLocalState) GetInstanceId(callback GetInstanceIdCallback) {
	self.serialAsync(func() error {
		instanceId := self.localState.GetInstanceId()
		callback.Result(instanceId, instanceId != nil)
		return nil
	})
}

func (self *AsyncLocalState) Logout(callback CommitCallback) {
	self.serialAsync(func() error {
		return self.localState.Logout()
	}, callback)
}

func (self *AsyncLocalState) Close() {
	self.cancel()
	close(self.jobs)
}

type job struct {
	work      func() error
	callbacks []CommitCallback
}
