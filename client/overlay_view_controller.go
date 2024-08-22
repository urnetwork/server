package client

import (
	"context"
	"sync"

	"bringyour.com/connect"
)

type OverlayModeListener interface {
	OverlayModeChanged(mode string)
}

type OverlayViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	overlayModeListeners *connect.CallbackList[OverlayModeListener]
}

func newOverlayViewController(ctx context.Context, device *BringYourDevice) *OverlayViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &OverlayViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		overlayModeListeners: connect.NewCallbackList[OverlayModeListener](),
	}
	return vc
}

func (vc *OverlayViewController) Start() {
}

func (vc *OverlayViewController) Stop() {
	// FIXME
}

func (vc *OverlayViewController) Close() {
	cvcLog("close")

	vc.cancel()
}

func (vc *OverlayViewController) AddOverlayModeListener(listener OverlayModeListener) Sub {
	callbackId := vc.overlayModeListeners.Add(listener)
	return newSub(func() {
		vc.overlayModeListeners.Remove(callbackId)
	})
}

func (vc *OverlayViewController) overlayModeChanged(mode string) {
	for _, listener := range vc.overlayModeListeners.Get() {
		connect.HandleError(func() {
			listener.OverlayModeChanged(mode)
		})
	}
}

func (vc *OverlayViewController) OpenOverlay(mode string) {
	vc.overlayModeChanged(mode)
}
