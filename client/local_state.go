package client

import (
	"context"
	"fmt"
	// "io"
	"os"
	"path/filepath"
	"errors"

	"bringyour.com/connect"
)


const AsyncQueueSize = 32


type LocalState struct {
	ctx context.Context
	cancel context.CancelFunc

	localStorageDir string
}

func newLocalState(ctx context.Context, localStorageDir string) *LocalState {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &LocalState{
		ctx: cancelCtx,
		cancel: cancel,
		localStorageDir: localStorageDir,
	}
}

func (self *LocalState) GetByJwt() (string, error) {
	path := filepath.Join(self.localStorageDir, ".by_jwt")
	if byJwtBytes, err := os.ReadFile(path); err == nil {
		return string(byJwtBytes), nil
	}
	return "", fmt.Errorf("Not found.")
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
		return os.WriteFile(path, []byte(byJwt), 0700)
	}
}

func (self *LocalState) GetByClientJwt() (string, error) {
	path := filepath.Join(self.localStorageDir, ".by_client_jwt")
	if byClientJwtBytes, err := os.ReadFile(path); err == nil {
		return string(byClientJwtBytes), nil
	}
	return "", fmt.Errorf("Not found.")
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
		self.setInstanceId(nil)
		os.Remove(path)
		return nil
	} else {
		instanceId := connect.NewId()
		self.setInstanceId(Id(instanceId.Bytes()))
		return os.WriteFile(path, []byte(byClientJwt), 0700)
	}
}

func (self *LocalState) GetInstanceId() (Id, error) {
	path := filepath.Join(self.localStorageDir, ".instance_id")
	if instanceIdBytes, err := os.ReadFile(path); err == nil {
		instanceId, err := connect.IdFromBytes(instanceIdBytes)
		return Id(instanceId.Bytes()), err
	}
	return nil, fmt.Errorf("Not found.")
}

func (self *LocalState) setInstanceId(instanceId Id) error {
	path := filepath.Join(self.localStorageDir, ".instance_id")
	if instanceId == nil {
		os.Remove(path)
		return nil
	} else {
		instanceId_ := connect.Id(instanceId)
		return os.WriteFile(path, instanceId_.Bytes(), 0700)
	}
}

// clears all auth tokens
func (self *LocalState) Logout() error {
	return errors.Join(
		self.SetByJwt(""),
		self.SetByClientJwt(""),
	)
}

func (self *LocalState) Close() {
	self.cancel()
}


type CommitCallback interface {
	Complete(success bool)
}


type singleResultCallback[R any] interface {
	Present(result R)
	NotPresent()
}


type GetByJwtCallback interface {
	CommitCallback
	singleResultCallback[string]
}


type GetByClientJwtCallback interface {
	CommitCallback
	singleResultCallback[string]
}


type GetInstanceIdCallback interface {
	CommitCallback
	singleResultCallback[Id]
}


type AsyncLocalState struct {
	ctx context.Context
	cancel context.CancelFunc

	localState *LocalState

	jobs chan *job
}

func NewAsyncLocalState(localStorageDir string) *AsyncLocalState {
	cancelCtx, cancel := context.WithCancel(context.Background())

	localState := newLocalState(cancelCtx, localStorageDir)

	asyncLocalState := &AsyncLocalState{
		ctx: cancelCtx,
		cancel: cancel,
		localState: localState,
		jobs: make(chan *job, AsyncQueueSize),
	}
	go asyncLocalState.run()

	return asyncLocalState
}

func (self *AsyncLocalState) run() {
	defer func(){
		self.cancel()

		// drain the jobs
		func() {
			for {
				select {
				case job, ok := <- self.jobs:
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
		case <- self.ctx.Done():
			return
		case job, ok := <- self.jobs:
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

func (self *AsyncLocalState) serialAsync(work func()(error), callbacks ...CommitCallback) {
	job := &job{
		work: work,
		callbacks: callbacks,
	}
	select {
	case <- self.ctx.Done():
		for _, callback := range callbacks {
			callback.Complete(false)
		}
	case self.jobs <- job:
	}
}

// get the sync local state
func  (self *AsyncLocalState) LocalState() *LocalState {
	return self.localState
}

func (self *AsyncLocalState) GetByJwt(callback GetByJwtCallback) {
	self.serialAsync(func()(error) {
		byJwt, err := self.localState.GetByJwt()
		if err == nil {
			callback.Present(byJwt)
		} else {
			callback.NotPresent()
		}
		return nil
	}, callback)
}

// clears the clientjwt and instanceid if differnet
func (self *AsyncLocalState) SetByJwt(byJwt string, callback CommitCallback) {
	self.serialAsync(func()(error) {
		return self.localState.SetByJwt(byJwt)
	}, callback)
}

func (self *AsyncLocalState) GetByClientJwt(callback GetByClientJwtCallback) {
	self.serialAsync(func()(error) {
		byClientJwt, err := self.localState.GetByClientJwt()
		if err == nil {
			callback.Present(byClientJwt)
		} else {
			callback.NotPresent()
		}
		return nil
	}, callback)
}

func (self *AsyncLocalState) SetByClientJwt(byClientJwt string, callback CommitCallback) {
	self.serialAsync(func()(error) {
		return self.localState.SetByClientJwt(byClientJwt)
	}, callback)
}

func (self *AsyncLocalState) GetInstanceId(callback GetInstanceIdCallback) {
	self.serialAsync(func()(error) {
		instanceId, err := self.localState.GetInstanceId()
		if err == nil {
			callback.Present(instanceId)
		} else {
			callback.NotPresent()
		}
		return nil
	}, callback)
}

func (self *AsyncLocalState) Logout(callback CommitCallback) {
	self.serialAsync(func()(error) {
		return self.localState.Logout()
	}, callback)
}

func (self *AsyncLocalState) Close() {
	self.cancel()
	close(self.jobs)
}


type job struct {
	work func()(error)
	callbacks []CommitCallback
}

