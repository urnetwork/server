package client

import (
	"context"
	"errors"

	// "fmt"
	"math"
	mathrand "math/rand"
	"sync"
	"time"

	"github.com/golang/glog"

	"bringyour.com/connect"
)

var connectVcLog = logFn("connect_view_controller")

type ConnectionStatus = string

const (
	Disconnected   ConnectionStatus = "DISCONNECTED"
	Connecting     ConnectionStatus = "CONNECTING"
	DestinationSet ConnectionStatus = "DESTINATION_SET"
	Connected      ConnectionStatus = "CONNECTED"
)

type SelectedLocationListener interface {
	SelectedLocationChanged(location *ConnectLocation)
}

type ConnectionStatusListener interface {
	ConnectionStatusChanged()
}

// type ConnectGridListener interface {
// 	ConnectGridPointChanged(index int32)
// }

type GridListener interface {
	GridChanged()
}

type ConnectViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	// this is set when the client is connected
	connected        bool
	connectionStatus ConnectionStatus
	selectedLocation *ConnectLocation
	grid             *ConnectGrid
	// providerGridPointList *ProviderGridPointList

	selectedLocationListeners *connect.CallbackList[SelectedLocationListener]
	connectionStatusListeners *connect.CallbackList[ConnectionStatusListener]
	// connectGridListeners       *connect.CallbackList[ConnectGridListener]
	gridListeners *connect.CallbackList[GridListener]
}

func newConnectViewController(ctx context.Context, device *BringYourDevice) *ConnectViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ConnectViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		connected:        false,
		connectionStatus: Disconnected,
		selectedLocation: nil,
		grid:             nil,

		selectedLocationListeners: connect.NewCallbackList[SelectedLocationListener](),
		connectionStatusListeners: connect.NewCallbackList[ConnectionStatusListener](),
		// connectGridListeners:       connect.NewCallbackList[ConnectGridListener](),
		// windowSizeListeners:   connect.NewCallbackList[WindowSizeListener](),
		gridListeners: connect.NewCallbackList[GridListener](),
	}
	if location := device.GetConnectLocation(); location != nil {
		vc.setConnected(true)
		vc.setSelectedLocation(location)
		vc.setConnectionStatus(DestinationSet)
		vc.setGrid()
	}
	return vc
}

func (self *ConnectViewController) Start() {}

func (self *ConnectViewController) Stop() {}

func (self *ConnectViewController) Close() {
	connectVcLog("close")
	self.cancel()
}

func (self *ConnectViewController) GetConnected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connected
}

func (self *ConnectViewController) GetConnectionStatus() ConnectionStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectionStatus
}

func (self *ConnectViewController) setConnectionStatus(status ConnectionStatus) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.connectionStatus != status {
			self.connectionStatus = status
			changed = true
		}
	}()
	if changed {
		self.connectionStatusChanged()
	}
}

func (self *ConnectViewController) connectionStatusChanged() {
	for _, listener := range self.connectionStatusListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectionStatusChanged()
		})
	}
}

func (self *ConnectViewController) AddConnectionStatusListener(listener ConnectionStatusListener) Sub {
	callbackId := self.connectionStatusListeners.Add(listener)
	return newSub(func() {
		self.connectionStatusListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) AddSelectedLocationListener(listener SelectedLocationListener) Sub {
	callbackId := self.selectedLocationListeners.Add(listener)
	return newSub(func() {
		self.selectedLocationListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) setSelectedLocation(location *ConnectLocation) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.selectedLocation = location
	}()
	self.selectedLocationChanged(location)
}

func (self *ConnectViewController) GetSelectedLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.selectedLocation
}

func (self *ConnectViewController) selectedLocationChanged(location *ConnectLocation) {
	for _, listener := range self.selectedLocationListeners.Get() {
		connect.HandleError(func() {
			listener.SelectedLocationChanged(location)
		})
	}
}

func (self *ConnectViewController) AddGridListener(listener GridListener) Sub {
	callbackId := self.gridListeners.Add(listener)
	return newSub(func() {
		self.gridListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) gridChanged() {
	for _, listener := range self.gridListeners.Get() {
		connect.HandleError(func() {
			listener.GridChanged()
		})
	}
}

// func (self *ConnectViewController) AddConnectGridListener(listener ConnectGridListener) Sub {
// 	callbackId := self.connectGridListeners.Add(listener)
// 	return newSub(func() {
// 		self.connectGridListeners.Remove(callbackId)
// 	})
// }

// func (self *ConnectViewController) connectGridPointChanged(index int32) {
// 	for _, listener := range self.connectGridListeners.Get() {
// 		connect.HandleError(func() {
// 			listener.ConnectGridPointChanged(index)
// 		})
// 	}
// }

// func (self *ConnectViewController) AddWindowSizeListener(listener WindowSizeListener) Sub {
// 	callbackId := self.windowSizeListeners.Add(listener)
// 	return newSub(func() {
// 		self.windowSizeListeners.Remove(callbackId)
// 	})
// }

// func (self *ConnectViewController) windowSizeChanged() {
// 	for _, listener := range self.windowSizeListeners.Get() {
// 		connect.HandleError(func() {
// 			listener.WindowSizeChanged()
// 		})
// 	}
// }

func (self *ConnectViewController) setConnected(connected bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.connected = connected
}

func (self *ConnectViewController) Connect(location *ConnectLocation) {
	self.setConnected(true)

	// enable provider
	provideMode := ProvideModePublic
	self.device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().SetProvideMode(provideMode)
	self.device.setProvideModeNoEvent(provideMode)

	// persist the connection location for automatic reconnect
	self.device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().SetConnectLocation(location)
	self.device.SetConnectLocation(location)

	self.setSelectedLocation(location)

	self.setGrid()
}

func (self *ConnectViewController) ConnectBestAvailable() {
	self.Connect(&ConnectLocation{
		ConnectLocationId: &ConnectLocationId{
			BestAvailable: true,
		},
	})
}

func (self *ConnectViewController) Disconnect() {
	self.setConnected(false)

	if !self.device.GetProvideWhileDisconnected() {
		// disable provider
		provideMode := ProvideModeNone
		self.device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().SetProvideMode(provideMode)
		self.device.setProvideModeNoEvent(provideMode)
	}

	self.device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().SetConnectLocation(nil)
	self.device.SetConnectLocation(nil)

	self.setGrid()
}

func (self *ConnectViewController) setGrid() {
	var grid *ConnectGrid
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.connected {
			if self.grid != nil {
				self.grid.close()
			}
			grid = newConnectGridWithDefaults(self.ctx, self)
			self.grid = grid
			changed = true
		} else if self.grid != nil {
			self.grid.close()
			self.grid = nil
			changed = true
		}
	}()

	if !changed {
		return
	}

	self.gridChanged()

	if grid != nil {
		self.setConnectionStatus(DestinationSet)

		if windowMonitor := self.device.windowMonitor(); windowMonitor != nil {
			grid.listenToWindow(windowMonitor)
		}
	} else {
		self.setConnectionStatus(Disconnected)
	}
}

func (self *ConnectViewController) GetGrid() *ConnectGrid {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.grid
}

type ProviderState = string

const (
	ProviderStateInEvaluation     ProviderState = "InEvaluation"
	ProviderStateEvaluationFailed ProviderState = "EvaluationFailed"
	ProviderStateNotAdded         ProviderState = "NotAdded"
	ProviderStateAdded            ProviderState = "Added"
	ProviderStateRemoved          ProviderState = "Removed"
)

func parseProviderState(state connect.ProviderState) (ProviderState, error) {
	switch state {
	case connect.ProviderStateInEvaluation:
		return ProviderStateInEvaluation, nil
	case connect.ProviderStateEvaluationFailed:
		return ProviderStateEvaluationFailed, nil
	case connect.ProviderStateNotAdded:
		return ProviderStateNotAdded, nil
	case connect.ProviderStateAdded:
		return ProviderStateAdded, nil
	case connect.ProviderStateRemoved:
		return ProviderStateRemoved, nil
	default:
		return "", errors.New("invalid ProviderState")
	}
}

type ProviderGridPoint struct {
	// note gomobile does not support struct composition
	X        int32
	Y        int32
	ClientId *Id
	// EventTime *Time
	State ProviderState
	// the time when this point will be removed
	// the ui can transition out based on this value
	EndTime *Time
	// wether the point is active for routing
	Active bool
}

type gridPointCoord struct {
	X int
	Y int
}
type gridPoint struct {
	gridPointCoord
	Plottable bool
	Occupied  bool
}

func defaultConnectGridSettings() *connectGridSettings {
	return &connectGridSettings{
		MinSideLength:  16,
		ExpandFraction: 1.5,
		RemoveTimeout:  8 * time.Second,
	}
}

type connectGridSettings struct {
	MinSideLength  int
	ExpandFraction float32
	RemoveTimeout  time.Duration
}

type ConnectGrid struct {
	ctx    context.Context
	cancel context.CancelFunc

	connectViewController *ConnectViewController
	settings              *connectGridSettings

	providerGridPointsMonitor *connect.Monitor

	stateLock sync.Mutex

	windowTargetSize  int
	windowCurrentSize int

	windowSub func()

	sideLength         int
	gridPoints         map[gridPointCoord]*gridPoint
	providerGridPoints map[connect.Id]*ProviderGridPoint
}

func newConnectGridWithDefaults(ctx context.Context, connectViewController *ConnectViewController) *ConnectGrid {
	return newConnectGrid(ctx, connectViewController, defaultConnectGridSettings())
}

func newConnectGrid(ctx context.Context, connectViewController *ConnectViewController, settings *connectGridSettings) *ConnectGrid {
	cancelCtx, cancel := context.WithCancel(ctx)
	providerGrid := &ConnectGrid{
		ctx:                       cancelCtx,
		cancel:                    cancel,
		connectViewController:     connectViewController,
		settings:                  settings,
		providerGridPointsMonitor: connect.NewMonitor(),
		windowTargetSize:          0,
		windowCurrentSize:         0,
		windowSub:                 nil,
		sideLength:                0,
		gridPoints:                map[gridPointCoord]*gridPoint{},
		providerGridPoints:        map[connect.Id]*ProviderGridPoint{},
	}
	go providerGrid.run()
	return providerGrid
}

func (self *ConnectGrid) GetWidth() int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return int32(self.sideLength)
}

func (self *ConnectGrid) GetHeight() int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return int32(self.sideLength)
}

func (self *ConnectGrid) GetWindowTargetSize() int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return int32(self.windowTargetSize)
}

func (self *ConnectGrid) GetWindowCurrentSize() int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return int32(self.windowCurrentSize)
}

func (self *ConnectGrid) GetProviderGridPointByClientId(clientId *Id) *ProviderGridPoint {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.providerGridPoints[clientId.toConnectId()]
}

func (self *ConnectGrid) GetProviderGridPointList() *ProviderGridPointList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	providerGridPointList := NewProviderGridPointList()
	for _, providerGridPoint := range self.providerGridPoints {
		// make a copy
		copyProviderGridPoint := *providerGridPoint
		providerGridPointList.Add(&copyProviderGridPoint)
	}
	return providerGridPointList
}

// *important* do not call this while holding the view controller state lock
// because this intialized with the current state, it will call back into the view controller
func (self *ConnectGrid) listenToWindow(windowMonitor *connect.RemoteUserNatMultiClientMonitor) {
	done := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		select {
		case <-self.ctx.Done():
			done = true
			return
		default:
		}

		if self.windowSub != nil {
			self.windowSub()
		}
		self.windowSub = windowMonitor.AddMonitorEventCallback(self.windowMonitorEventCallback)
	}()

	if done {
		return
	}

	// initialize with the current values
	self.windowMonitorEventCallback(windowMonitor.Events())
}

func (self *ConnectGrid) close() {
	self.cancel()

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.windowSub != nil {
			self.windowSub()
			self.windowSub = nil
		}
	}()
}

func (self *ConnectGrid) run() {
	defer self.cancel()

	for {
		providerGridPointChanged := false
		var notify chan struct{}
		var minEndPoint *ProviderGridPoint
		var timeout time.Duration
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			notify = self.providerGridPointsMonitor.NotifyChannel()
			now := time.Now()
			removedCount := 0
			for clientId, point := range self.providerGridPoints {
				if point.EndTime != nil {
					if !now.Before(point.EndTime.toTime()) {
						// remove
						delete(self.providerGridPoints, clientId)
						gridPointCoord := gridPointCoord{X: int(point.X), Y: int(point.Y)}
						self.gridPoints[gridPointCoord].Occupied = false
						providerGridPointChanged = true
						removedCount += 1
					} else if minEndPoint == nil || point.EndTime.toTime().Before(minEndPoint.EndTime.toTime()) {
						minEndPoint = point
					}
				}
			}
			if minEndPoint != nil {
				timeout = minEndPoint.EndTime.toTime().Sub(now)
			}

			if 0 < removedCount {
				glog.Infof(
					"[grid]%d->%d points=%d(-%d)\n",
					self.windowCurrentSize,
					self.windowTargetSize,
					len(self.providerGridPoints),
					removedCount,
				)
			}
		}()

		if providerGridPointChanged {
			self.connectViewController.gridChanged()
		}

		if minEndPoint != nil {
			// there is a next point to be removed after `timeout`
			select {
			case <-self.ctx.Done():
				return
			case <-notify:
			case <-time.After(timeout):
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			case <-notify:
			}
		}
	}
}

// must be called with the state lock
// create a new grid based on targetClientSize sets specific points as plottable or unplottable
func (self *ConnectGrid) resize() {
	targetClientSize := math.Ceil(math.Pow(float64(len(self.providerGridPoints)), float64(self.settings.ExpandFraction)))

	sideLength := max(
		self.settings.MinSideLength,
		// currently the size never contracts
		self.sideLength,
		int(math.Ceil(math.Sqrt(targetClientSize))),
	)

	if self.sideLength == sideLength {
		return
	}

	gridPoints := map[gridPointCoord]*gridPoint{}
	// merge the existing state
	for x := range sideLength {
		for y := range sideLength {
			c := gridPointCoord{X: x, Y: y}
			// shiftC := gridPointCoord{
			// 	X: x - (sideLength - self.sideLength) / 2,
			// 	Y: y - (sideLength - self.sideLength) / 2,
			// }
			point, ok := self.gridPoints[c]
			if ok {
				// reset the plottable state, which will be set below to the new shape
				point.Plottable = true
				// point = &gridPoint{
				// 	gridPointCoord: c,
				// 	Plottable:      true,
				// 	Occupied:       point.Occupied,
				// }
				gridPoints[c] = point
			} else {
				point = &gridPoint{
					gridPointCoord: c,
					Plottable:      true,
					Occupied:       false,
				}
				gridPoints[c] = point
			}
		}
	}

	// in a equal corner triangle, x+y<s
	// cut out 1/3 corner triangles
	for x := range sideLength / 3 {
		for y := range sideLength / 3 {
			// triangle is w*y+h*x<=w*h
			if x+y < sideLength/3 {
				gridPoints[gridPointCoord{X: x, Y: y}].Plottable = false

				// flip x
				gridPoints[gridPointCoord{X: sideLength - 1 - x, Y: y}].Plottable = false

				// flip y
				gridPoints[gridPointCoord{X: x, Y: sideLength - 1 - y}].Plottable = false

				// flip x and y
				gridPoints[gridPointCoord{X: sideLength - 1 - x, Y: sideLength - 1 - y}].Plottable = false
			}
		}
	}

	self.sideLength = sideLength
	self.gridPoints = gridPoints
}

// connect.MonitorEventFunction
func (self *ConnectGrid) windowMonitorEventCallback(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent) {
	done := false
	windowSizeChanged := false
	providerGridPointChanged := false
	var connectionStatus ConnectionStatus
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		select {
		case <-self.ctx.Done():
			done = true
			return
		default:
		}

		// eventTime := time.Now()

		for clientId, providerEvent := range providerEvents {
			providerState, err := parseProviderState(providerEvent.State)
			if err != nil {
				glog.Errorf("[grid]could not parse provider event state: %s", string(providerEvent.State))
				continue
			}

			point, ok := self.providerGridPoints[clientId]

			if ok {
				if point.State != providerState {
					point.State = providerState
					var endTime *Time
					if providerEvent.State.IsTerminal() {
						// schedule the point to be removed
						// note this resets the end time if already set
						endTime = newTime(time.Now().Add(self.settings.RemoveTimeout))
					}
					point.EndTime = endTime
					point.Active = providerEvent.State.IsActive()
					providerGridPointChanged = true
				}
				// point.EventTime = newTime(eventTime)
			} else {
				// insert a new provider point

				findUnuccupiedGridPoint := func() *gridPoint {
					// try random sampling, then use an exhaustive search
					if self.sideLength == 0 {
						return nil
					}

					for range 8 {
						c := gridPointCoord{
							X: mathrand.Intn(self.sideLength),
							Y: mathrand.Intn(self.sideLength),
						}
						if gridPoint, ok := self.gridPoints[c]; ok && gridPoint.Plottable && !gridPoint.Occupied {
							return gridPoint
						}
					}

					unoccupiedGridPoints := []*gridPoint{}
					for _, gridPoint := range self.gridPoints {
						if gridPoint.Plottable && !gridPoint.Occupied {
							unoccupiedGridPoints = append(unoccupiedGridPoints, gridPoint)
						}
					}
					if len(unoccupiedGridPoints) == 0 {
						return nil
					}
					return unoccupiedGridPoints[mathrand.Intn(len(unoccupiedGridPoints))]
				}

				self.resize()
				unoccupiedGridPoint := findUnuccupiedGridPoint()
				if unoccupiedGridPoint == nil {
					self.resize()
					unoccupiedGridPoint = findUnuccupiedGridPoint()
				}
				if unoccupiedGridPoint == nil {
					// resize is broken
					// TODO log
					continue
				}

				unoccupiedGridPoint.Occupied = true
				var endTime *Time
				if providerEvent.State.IsTerminal() {
					// schedule the point to be removed
					endTime = newTime(time.Now().Add(self.settings.RemoveTimeout))
				}
				point = &ProviderGridPoint{
					X:        int32(unoccupiedGridPoint.X),
					Y:        int32(unoccupiedGridPoint.Y),
					ClientId: newId(clientId),
					State:    providerState,
					// EventTime: newTime(eventTime),
					EndTime: endTime,
					Active:  providerEvent.State.IsActive(),
				}
				self.providerGridPoints[clientId] = point
				providerGridPointChanged = true
			}
		}

		// compute the window size here from the number of added providers
		windowCurrentSize := 0
		for _, point := range self.providerGridPoints {
			if point.Active {
				windowCurrentSize += 1
			}
		}

		if self.windowCurrentSize != windowCurrentSize || self.windowTargetSize != windowExpandEvent.TargetSize {
			self.windowCurrentSize = windowCurrentSize
			self.windowTargetSize = windowExpandEvent.TargetSize
			windowSizeChanged = true
		}

		// note the callback is only active while the device is connected
		if windowExpandEvent.MinSatisfied {
			connectionStatus = Connected
		} else {
			connectionStatus = Connecting
		}

		glog.Infof(
			"[grid]%d->%d(%t) points=%d %s (w=%t, p=%t)\n",
			self.windowCurrentSize,
			self.windowTargetSize,
			windowExpandEvent.MinSatisfied,
			len(self.providerGridPoints),
			connectionStatus,
			windowSizeChanged,
			providerGridPointChanged,
		)
	}()

	if done {
		return
	}

	if providerGridPointChanged {
		self.providerGridPointsMonitor.NotifyAll()
	}
	if windowSizeChanged || providerGridPointChanged {
		self.connectViewController.gridChanged()
	}
	self.connectViewController.setConnectionStatus(connectionStatus)
}
