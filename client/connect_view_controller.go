package client

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"

	"sync"

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

type ConnectGridListener interface {
	ConnectGridPointChanged(index int32)
}

type WindowEventSizeListener interface {
	WindowEventSizeChanged()
}

type ProviderGridPointsUpdated interface {
	ProviderGridPointsUpdated()
}

type ConnectViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	selectedLocation      *ConnectLocation
	connectionStatus      ConnectionStatus
	grid                  *ConnectGrid
	providerGridPointList *ProviderGridPointList

	windowTargetSize  int32
	windowCurrentSize int32

	selectedLocationListeners  *connect.CallbackList[SelectedLocationListener]
	connectionStatusListeners  *connect.CallbackList[ConnectionStatusListener]
	connectGridListeners       *connect.CallbackList[ConnectGridListener]
	windowEventSizeListeners   *connect.CallbackList[WindowEventSizeListener]
	providerGridPointListeners *connect.CallbackList[ProviderGridPointsUpdated]
}

func newConnectViewController(ctx context.Context, device *BringYourDevice) *ConnectViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ConnectViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		connectionStatus:      Disconnected,
		selectedLocation:      nil,
		grid:                  nil,
		windowTargetSize:      0,
		windowCurrentSize:     0,
		providerGridPointList: NewProviderGridPointList(),

		selectedLocationListeners:  connect.NewCallbackList[SelectedLocationListener](),
		connectionStatusListeners:  connect.NewCallbackList[ConnectionStatusListener](),
		connectGridListeners:       connect.NewCallbackList[ConnectGridListener](),
		windowEventSizeListeners:   connect.NewCallbackList[WindowEventSizeListener](),
		providerGridPointListeners: connect.NewCallbackList[ProviderGridPointsUpdated](),
	}
	if location := device.GetConnectLocation(); location != nil {
		vc.setSelectedLocation(location)
		vc.setConnectionStatus(DestinationSet)
		vc.addWindowEventMonitor()
	}
	return vc
}

func (self *ConnectViewController) Start() {}

func (self *ConnectViewController) Stop() {}

func (self *ConnectViewController) Close() {
	connectVcLog("close")

	self.cancel()
}

func (self *ConnectViewController) GetConnectionStatus() ConnectionStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectionStatus
}

func (self *ConnectViewController) setConnectionStatus(status ConnectionStatus) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.connectionStatus = status
	}()
	self.connectionStatusChanged()
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

func (self *ConnectViewController) AddProviderGridPointListener(listener ProviderGridPointsUpdated) Sub {
	callbackId := self.providerGridPointListeners.Add(listener)
	return newSub(func() {
		self.providerGridPointListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) providerGridPointChanged() {
	for _, listener := range self.providerGridPointListeners.Get() {
		connect.HandleError(func() {
			listener.ProviderGridPointsUpdated()
		})
	}
}

func (self *ConnectViewController) AddConnectGridListener(listener ConnectGridListener) Sub {
	callbackId := self.connectGridListeners.Add(listener)
	return newSub(func() {
		self.connectGridListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) connectGridPointChanged(index int32) {
	for _, listener := range self.connectGridListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectGridPointChanged(index)
		})
	}
}

func (self *ConnectViewController) GetWindowTargetSize() int32 {
	return self.windowTargetSize
}

func (self *ConnectViewController) setWindowTargetSize(size int32) {

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.windowTargetSize = size
	}()

	self.windowEventSizeChanged()
}

func (self *ConnectViewController) GetWindowCurrentSize() int32 {
	return self.windowCurrentSize
}

func (self *ConnectViewController) setWindowCurrentSize(size int32) {

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.windowCurrentSize = size

	self.windowEventSizeChanged()
}

func (self *ConnectViewController) AddWindowEventSizeListener(listener WindowEventSizeListener) Sub {
	callbackId := self.windowEventSizeListeners.Add(listener)
	return newSub(func() {
		self.windowEventSizeListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) windowEventSizeChanged() {
	for _, listener := range self.windowEventSizeListeners.Get() {
		connect.HandleError(func() {
			listener.WindowEventSizeChanged()
		})
	}
}

func (self *ConnectViewController) Connect(location *ConnectLocation) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.selectedLocation = location
	}()

	self.clearProviderPoints()

	self.setConnectionStatus(Connecting)

	// enable provider
	provideMode := ProvideModePublic
	self.device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().SetProvideMode(provideMode)
	self.device.setProvideModeNoEvent(provideMode)

	// persist the connection location for automatic reconnect
	self.device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().SetConnectLocation(location)
	self.device.SetConnectLocation(location)
	self.setSelectedLocation(location)
	self.setConnectionStatus(DestinationSet)
	self.addWindowEventMonitor()
}

func (self *ConnectViewController) ConnectBestAvailable() {
	self.Connect(&ConnectLocation{
		ConnectLocationId: &ConnectLocationId{
			BestAvailable: true,
		},
	})
}

func (self *ConnectViewController) Disconnect() {
	self.clearProviderPoints()

	provideMode := ProvideModeNone
	self.device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().SetProvideMode(provideMode)
	self.device.setProvideModeNoEvent(provideMode)

	self.device.GetNetworkSpace().GetAsyncLocalState().GetLocalState().SetConnectLocation(nil)
	self.device.SetConnectLocation(nil)
	self.setConnectionStatus(Disconnected)
}

func (self *ConnectViewController) monitorEventCallback(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent) {

	if self.windowCurrentSize != int32(windowExpandEvent.CurrentSize) {
		self.setWindowCurrentSize(int32(windowExpandEvent.CurrentSize))
	}

	if self.windowTargetSize != int32(windowExpandEvent.TargetSize) {
		self.setWindowTargetSize(int32(windowExpandEvent.TargetSize))

		if self.grid == nil {
			self.initGrid(int32(windowExpandEvent.TargetSize))
		}
	}

	for id, providerEvent := range providerEvents {

		self.stateLock.Lock()
		point := self.GetProviderGridPointByClientId(newId(id))
		self.stateLock.Unlock()

		providerEventState, err := self.parseConnectProviderState(string(providerEvent.State))
		if err != nil {
			connectVcLog("Error prasing connect provider state: %s", string(providerEvent.State))
			continue
		}

		if point == nil {
			// insert a new item in the grid

			self.addProviderGridPoint(
				newId(providerEvent.ClientId),
				providerEventState,
				newTime(providerEvent.EventTime),
			)

			continue
		}

		// check if eventTime is more recent than current point latest event time
		if providerEvent.EventTime.UnixMilli() > point.EventTime.UnixMilli() {
			// update the point
			self.updateProviderGridPoint(
				point.ClientId,
				newTime(providerEvent.EventTime),
				providerEventState,
			)

		}
	}

	// current size equals target size, mark connection status as connected
	if (self.connectionStatus == Connecting || self.connectionStatus == DestinationSet) && self.windowCurrentSize >= self.windowTargetSize {
		self.setConnectionStatus(Connected)
	}

	if self.connectionStatus == Connected && (self.windowCurrentSize < self.windowTargetSize && self.windowCurrentSize < 2) {
		self.setConnectionStatus(Connecting)
	}

}

func (self *ConnectViewController) addWindowEventMonitor() {
	var monitorEventFunc connect.MonitorEventFunction = self.monitorEventCallback
	self.device.addMonitorEventCallback(monitorEventFunc)
}

type ProviderState = string

const (
	ProviderStateInEvaluation     ProviderState = "InEvaluation"
	ProviderStateEvaluationFailed ProviderState = "EvaluationFailed"
	ProviderStateNotAdded         ProviderState = "NotAdded"
	ProviderStateAdded            ProviderState = "Added"
	ProviderStateRemoved          ProviderState = "Removed"
)

func (self *ConnectViewController) parseConnectProviderState(state string) (ProviderState, error) {
	switch state {
	case string(ProviderStateInEvaluation):
		return ProviderStateInEvaluation, nil
	case string(ProviderStateEvaluationFailed):
		return ProviderStateEvaluationFailed, nil
	case string(ProviderStateNotAdded):
		return ProviderStateNotAdded, nil
	case string(ProviderStateAdded):
		return ProviderStateAdded, nil
	case string(ProviderStateRemoved):
		return ProviderStateRemoved, nil
	default:
		return "", errors.New("invalid ProviderState")
	}
}

type ProviderGridPoint struct {
	X         int32
	Y         int32
	ClientId  *Id
	EventTime *Time
	State     ProviderState
}

func (self *ConnectViewController) GetProviderGridPointByClientId(clientId *Id) *ProviderGridPoint {

	if self.providerGridPointList == nil {
		return nil
	}

	for i := 0; i < self.providerGridPointList.Len(); i++ {

		point := self.providerGridPointList.Get(i)

		if point.ClientId.Cmp(clientId) == 0 {
			return point
		}

	}

	return nil

}

func (self *ConnectViewController) updateProviderGridPoint(
	clientId *Id,
	eventTime *Time,
	state ProviderState,
) {

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.providerGridPointList == nil || self.providerGridPointList.Len() <= 0 {
			return
		}

		for i := 0; i < self.providerGridPointList.Len(); i++ {

			point := self.providerGridPointList.Get(i)

			if point.ClientId == clientId {

				point.State = state
				point.EventTime = eventTime

				self.providerGridPointChanged()

			}

		}

	}()

}

type GridPoint struct {
	X         int32
	Y         int32
	Plottable bool
	Occupied  bool
}

type ConnectGrid struct {
	Width  int32
	Height int32
	Points *GridPointList
}

func (self *ConnectViewController) addProviderGridPoint(
	clientId *Id,
	state ProviderState,
	eventTime *Time,
) {

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	availableGridPoint, err := self.getAvailableGridPoint()
	if err != nil {
		return
	}

	self.providerGridPointList.Add(&ProviderGridPoint{
		X:         availableGridPoint.X,
		Y:         availableGridPoint.Y,
		ClientId:  clientId,
		State:     state,
		EventTime: eventTime,
	})

	availableGridPoint.Occupied = true

	self.providerGridPointChanged()

}

func (self *ConnectViewController) GetProviderGridPointList() *ProviderGridPointList {
	return self.providerGridPointList
}

func buildGridPointList(width int, height int) []*GridPoint {

	gridPoints := []*GridPoint{}

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {

			gridPoints = append(gridPoints, &GridPoint{X: int32(x), Y: int32(y), Plottable: true, Occupied: false})

		}
	}

	return gridPoints
}

func (self *ConnectViewController) getAvailableGridPoint() (*GridPoint, error) {

	if self.grid == nil {
		return nil, fmt.Errorf("grid uninitialized")
	}

	var availablePoints []*GridPoint

	for i := 0; i < self.grid.Points.Len(); i++ {
		point := self.grid.Points.Get(i)
		if point.Plottable && !point.Occupied {

			availablePoints = append(availablePoints, point)

		}

	}

	if len(availablePoints) == 0 {
		return nil, fmt.Errorf("no plottable points available")
	}

	// randomly select one & set latest event
	randomIndex := rand.Intn(len(availablePoints))
	return availablePoints[randomIndex], nil
}

func (self *ConnectViewController) clearProviderPoints() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.providerGridPointList = NewProviderGridPointList()
	}()

	self.providerGridPointChanged()
}

// create a new grid based on targetClientSize sets specific points as plottable or unplottable
func (self *ConnectViewController) initGrid(targetClientSize int32) {

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	sideLength := int(math.Ceil(math.Sqrt(float64(targetClientSize))))
	if sideLength < 16 {
		sideLength = 16
	}
	width := sideLength
	height := sideLength

	points := buildGridPointList(width, height)

	oneEighth := height / 8
	oneFourth := width / 4
	oneEighthWidth := width / 8

	// Set the first and last 1/4 columns of the first and last 1/8 rows as unplottable
	for y := 0; y < oneEighth; y++ {
		for x := 0; x < width; x++ {
			if x < oneFourth || x >= width-oneFourth {
				points[y*width+x].Plottable = false
			}
		}
	}
	for y := height - oneEighth; y < height; y++ {
		for x := 0; x < width; x++ {
			if x < oneFourth || x >= width-oneFourth {
				points[y*width+x].Plottable = false
			}
		}
	}

	// Set the first and last 1/8 columns of the second 1/8 and second to last 1/8 rows as unplottable
	for y := oneEighth; y < 2*oneEighth; y++ {
		for x := 0; x < width; x++ {
			if x < oneEighthWidth || x >= width-oneEighthWidth {
				points[y*width+x].Plottable = false
			}
		}
	}
	for y := height - 2*oneEighth; y < height-oneEighth; y++ {
		for x := 0; x < width; x++ {
			if x < oneEighthWidth || x >= width-oneEighthWidth {
				points[y*width+x].Plottable = false
			}
		}
	}

	pointsList := NewGridPointList()
	pointsList.addAll(points...)

	self.grid = &ConnectGrid{Width: int32(width), Height: int32(height), Points: pointsList}
}

func (self *ConnectViewController) GetGrid() *ConnectGrid {
	return self.grid
}
