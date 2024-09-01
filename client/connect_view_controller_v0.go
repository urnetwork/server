package client

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"

	"sync"

	"bringyour.com/connect"
)

var connectVcLog = logFn("connect_view_controller_v0")

type ConnectionStatus = string

const (
	Disconnected   ConnectionStatus = "DISCONNECTED"
	Connecting     ConnectionStatus = "CONNECTING"
	DestinationSet ConnectionStatus = "DESTINATION_SET"
	Connected      ConnectionStatus = "CONNECTED"
	Canceling      ConnectionStatus = "CANCELING"
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

type ConnectViewControllerV0 struct {
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

func newConnectViewControllerV0(ctx context.Context, device *BringYourDevice) *ConnectViewControllerV0 {
	cancelCtx, cancel := context.WithCancel(ctx)

	vm := &ConnectViewControllerV0{
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
	return vm
}

func (vc *ConnectViewControllerV0) Start() {
	vc.monitorWindowEvents()
}

func (vc *ConnectViewControllerV0) Stop() {}

func (vc *ConnectViewControllerV0) Close() {
	connectVcLog("close")

	vc.cancel()
}

func (vc *ConnectViewControllerV0) GetConnectionStatus() ConnectionStatus {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	return vc.connectionStatus
}

func (vc *ConnectViewControllerV0) setConnectionStatus(status ConnectionStatus) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.connectionStatus = status
	}()
	vc.connectionStatusChanged()
}

func (vc *ConnectViewControllerV0) connectionStatusChanged() {
	for _, listener := range vc.connectionStatusListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectionStatusChanged()
		})
	}
}

func (vc *ConnectViewControllerV0) AddConnectionStatusListener(listener ConnectionStatusListener) Sub {
	callbackId := vc.connectionStatusListeners.Add(listener)
	return newSub(func() {
		vc.connectionStatusListeners.Remove(callbackId)
	})
}

func (vc *ConnectViewControllerV0) AddSelectedLocationListener(listener SelectedLocationListener) Sub {
	callbackId := vc.selectedLocationListeners.Add(listener)
	return newSub(func() {
		vc.selectedLocationListeners.Remove(callbackId)
	})
}

func (vc *ConnectViewControllerV0) setSelectedLocation(location *ConnectLocation) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.selectedLocation = location
	}()
	vc.selectedLocationChanged(location)
}

func (vc *ConnectViewControllerV0) GetSelectedLocation() *ConnectLocation {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	return vc.selectedLocation
}

func (vc *ConnectViewControllerV0) selectedLocationChanged(location *ConnectLocation) {
	for _, listener := range vc.selectedLocationListeners.Get() {
		connect.HandleError(func() {
			listener.SelectedLocationChanged(location)
		})
	}
}

func (vc *ConnectViewControllerV0) AddProviderGridPointListener(listener ProviderGridPointsUpdated) Sub {
	callbackId := vc.providerGridPointListeners.Add(listener)
	return newSub(func() {
		vc.providerGridPointListeners.Remove(callbackId)
	})
}

func (vc *ConnectViewControllerV0) providerGridPointChanged() {
	for _, listener := range vc.providerGridPointListeners.Get() {
		connect.HandleError(func() {
			listener.ProviderGridPointsUpdated()
		})
	}
}

func (vc *ConnectViewControllerV0) AddConnectGridListener(listener ConnectGridListener) Sub {
	callbackId := vc.connectGridListeners.Add(listener)
	return newSub(func() {
		vc.connectGridListeners.Remove(callbackId)
	})
}

func (vc *ConnectViewControllerV0) connectGridPointChanged(index int32) {
	for _, listener := range vc.connectGridListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectGridPointChanged(index)
		})
	}
}

func (vc *ConnectViewControllerV0) GetWindowTargetSize() int32 {
	return vc.windowTargetSize
}

func (vc *ConnectViewControllerV0) setWindowTargetSize(size int32) {

	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.windowTargetSize = size
	}()

	vc.windowEventSizeChanged()
}

func (vc *ConnectViewControllerV0) GetWindowCurrentSize() int32 {
	return vc.windowCurrentSize
}

func (vc *ConnectViewControllerV0) setWindowCurrentSize(size int32) {

	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()

	vc.windowCurrentSize = size

	vc.windowEventSizeChanged()
}

func (vc *ConnectViewControllerV0) AddWindowEventSizeListener(listener WindowEventSizeListener) Sub {
	callbackId := vc.windowEventSizeListeners.Add(listener)
	return newSub(func() {
		vc.windowEventSizeListeners.Remove(callbackId)
	})
}

func (vc *ConnectViewControllerV0) windowEventSizeChanged() {
	for _, listener := range vc.windowEventSizeListeners.Get() {
		connect.HandleError(func() {
			listener.WindowEventSizeChanged()
		})
	}
}

func (vc *ConnectViewControllerV0) isCanceling() bool {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	isCanceling := false
	if vc.connectionStatus == Canceling {
		isCanceling = true
	}
	return isCanceling
}

func (vc *ConnectViewControllerV0) Connect(location *ConnectLocation) {

	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.selectedLocation = location
	}()

	vc.clearProviderPoints()

	vc.setConnectionStatus(Connecting)

	if location.IsDevice() {

		isCanceling := vc.isCanceling()

		if isCanceling {
			vc.setConnectionStatus(Disconnected)
		} else {
			destinationIds := []Id{
				*location.ConnectLocationId.ClientId,
			}

			specs := NewProviderSpecList()
			for _, destinationId := range destinationIds {
				specs.Add(&ProviderSpec{
					ClientId: &destinationId,
				})
			}
			vc.device.SetDestination(specs, ProvideModePublic)
			vc.setSelectedLocation(location)
			vc.setConnectionStatus(DestinationSet)
		}

	} else {
		specs := NewProviderSpecList()
		specs.Add(&ProviderSpec{
			LocationId:      location.ConnectLocationId.LocationId,
			LocationGroupId: location.ConnectLocationId.LocationGroupId,
		})

		isCanceling := vc.isCanceling()

		if isCanceling {
			vc.setConnectionStatus(Disconnected)
		} else {
			vc.device.SetDestination(specs, ProvideModePublic)
			vc.setSelectedLocation(location)
			vc.setConnectionStatus(DestinationSet)
		}
	}
}

func (vc *ConnectViewControllerV0) ConnectBestAvailable() {

	vc.setConnectionStatus(Connecting)

	specs := &ProviderSpecList{}
	specs.Add(&ProviderSpec{
		BestAvailable: true,
	})

	args := &FindProviders2Args{
		Specs: specs,
		Count: 1024,
	}

	vc.device.Api().FindProviders2(args, FindProviders2Callback(newApiCallback[*FindProviders2Result](
		func(result *FindProviders2Result, err error) {

			isCanceling := vc.isCanceling()
			if isCanceling {
				vc.setConnectionStatus(Disconnected)
			} else {

				if err == nil && result.ProviderStats != nil {

					clientIds := []Id{}
					for _, provider := range result.ProviderStats.exportedList.values {
						clientId := provider.ClientId
						clientIds = append(clientIds, *clientId)
					}
					vc.setConnectionStatus(DestinationSet)
				} else {
					vc.setConnectionStatus(Disconnected)
				}
			}
		},
	)))
}

func (vc *ConnectViewControllerV0) CancelConnection() {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	status := Canceling
	vc.connectionStatus = status
	vc.connectionStatusChanged()
}

func (vc *ConnectViewControllerV0) monitorWindowEvents() {

	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		defer func() {
			if r := recover(); r != nil {
				connectVcLog("monitorWindowEvents: recovered from panic: %v", r)
			}
		}()

		for {
			select {
			case <-vc.ctx.Done():
				return
			case <-ticker.C:

				if vc.connectionStatus == Connected || vc.connectionStatus == Connecting || vc.connectionStatus == DestinationSet {

					func() {
						windowEvents := vc.device.WindowEvents()

						if vc.windowCurrentSize != int32(windowEvents.CurrentSize()) {
							vc.setWindowCurrentSize(int32(windowEvents.CurrentSize()))
						}

						if vc.windowTargetSize != int32(windowEvents.TargetSize()) {
							vc.setWindowTargetSize(int32(windowEvents.TargetSize()))

							if vc.grid == nil {
								vc.initGrid(int32(windowEvents.TargetSize()))
							}
						}

						for _, providerEvent := range windowEvents.providerEvents {

							vc.stateLock.Lock()
							point := vc.GetProviderGridPointByClientId(newId(providerEvent.ClientId))
							vc.stateLock.Unlock()

							providerEventState, err := vc.parseConnectProviderState(string(providerEvent.State))
							if err != nil {
								connectVcLog("Error prasing connect provider state: %s", string(providerEvent.State))
								continue
							}

							if point == nil {
								// insert a new item in the grid

								vc.addProviderGridPoint(
									newId(providerEvent.ClientId),
									providerEventState,
									newTime(providerEvent.EventTime),
								)

								continue
							}

							// check if eventTime is more recent than current point latest event time
							if providerEvent.EventTime.UnixMilli() > point.EventTime.UnixMilli() {
								// update the point
								vc.updateProviderGridPoint(
									point.ClientId,
									newTime(providerEvent.EventTime),
									providerEventState,
								)

							}
						}

						// current size equals target size, mark connection status as connected
						if (vc.connectionStatus == Connecting || vc.connectionStatus == DestinationSet) && vc.windowCurrentSize >= vc.windowTargetSize {
							vc.setConnectionStatus(Connected)
						}

						if vc.connectionStatus == Connected && (vc.windowCurrentSize < vc.windowTargetSize && vc.windowCurrentSize < 2) {
							vc.setConnectionStatus(Connecting)
						}
					}()
				}
			}
		}
	}()

}

type ProviderState = string

const (
	ProviderStateInEvaluation     ProviderState = "InEvaluation"
	ProviderStateEvaluationFailed ProviderState = "EvaluationFailed"
	ProviderStateNotAdded         ProviderState = "NotAdded"
	ProviderStateAdded            ProviderState = "Added"
	ProviderStateRemoved          ProviderState = "Removed"
)

func (vc *ConnectViewControllerV0) parseConnectProviderState(state string) (ProviderState, error) {
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

func (vc *ConnectViewControllerV0) Disconnect() {

	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.device.RemoveDestination()
		vc.connectionStatus = Disconnected
	}()

	vc.clearProviderPoints()

	vc.connectionStatusChanged()
}

type ProviderGridPoint struct {
	X         int32
	Y         int32
	ClientId  *Id
	EventTime *Time
	State     ProviderState
}

func (vc *ConnectViewControllerV0) GetProviderGridPointByClientId(clientId *Id) *ProviderGridPoint {

	if vc.providerGridPointList == nil {
		return nil
	}

	for i := 0; i < vc.providerGridPointList.Len(); i++ {

		point := vc.providerGridPointList.Get(i)

		if point.ClientId.Cmp(clientId) == 0 {
			return point
		}

	}

	return nil

}

func (vc *ConnectViewControllerV0) updateProviderGridPoint(
	clientId *Id,
	eventTime *Time,
	state ProviderState,
) {

	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()

		if vc.providerGridPointList == nil || vc.providerGridPointList.Len() <= 0 {
			return
		}

		for i := 0; i < vc.providerGridPointList.Len(); i++ {

			point := vc.providerGridPointList.Get(i)

			if point.ClientId == clientId {

				point.State = state
				point.EventTime = eventTime

				vc.providerGridPointChanged()

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

func (vc *ConnectViewControllerV0) addProviderGridPoint(
	clientId *Id,
	state ProviderState,
	eventTime *Time,
) {

	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()

	availableGridPoint, err := vc.getAvailableGridPoint()
	if err != nil {
		return
	}

	vc.providerGridPointList.Add(&ProviderGridPoint{
		X:         availableGridPoint.X,
		Y:         availableGridPoint.Y,
		ClientId:  clientId,
		State:     state,
		EventTime: eventTime,
	})

	availableGridPoint.Occupied = true

	vc.providerGridPointChanged()

}

func (vc *ConnectViewControllerV0) GetProviderGridPointList() *ProviderGridPointList {
	return vc.providerGridPointList
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

func (vc *ConnectViewControllerV0) getAvailableGridPoint() (*GridPoint, error) {

	if vc.grid == nil {
		return nil, fmt.Errorf("grid uninitialized")
	}

	var availablePoints []*GridPoint

	for i := 0; i < vc.grid.Points.Len(); i++ {
		point := vc.grid.Points.Get(i)
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

func (vc *ConnectViewControllerV0) clearProviderPoints() {

	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.providerGridPointList = NewProviderGridPointList()
	}()

	vc.providerGridPointChanged()
}

// create a new grid based on targetClientSize sets specific points as plottable or unplottable
func (vc *ConnectViewControllerV0) initGrid(targetClientSize int32) {

	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()

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

	vc.grid = &ConnectGrid{Width: int32(width), Height: int32(height), Points: pointsList}
}

func (vc *ConnectViewControllerV0) GetGrid() *ConnectGrid {
	return vc.grid
}

// merged location and location group
type ConnectLocation struct {
	ConnectLocationId *ConnectLocationId
	Name              string

	ProviderCount int32
	Promoted      bool
	MatchDistance int32

	LocationType LocationType

	City        string
	Region      string
	Country     string
	CountryCode string

	CityLocationId    *Id
	RegionLocationId  *Id
	CountryLocationId *Id
}

func (self *ConnectLocation) IsGroup() bool {
	return self.ConnectLocationId.IsGroup()
}

func (self *ConnectLocation) IsDevice() bool {
	return self.ConnectLocationId.IsDevice()
}

func (self *ConnectLocation) ToRegion() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name:              self.Region,

		ProviderCount: self.ProviderCount,
		Promoted:      false,
		MatchDistance: 0,

		LocationType: LocationTypeRegion,

		City:        "",
		Region:      self.Region,
		Country:     self.Country,
		CountryCode: self.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  self.RegionLocationId,
		CountryLocationId: self.CountryLocationId,
	}
}

func (self *ConnectLocation) ToCountry() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name:              self.Country,

		ProviderCount: self.ProviderCount,
		Promoted:      false,
		MatchDistance: 0,

		LocationType: LocationTypeCountry,

		City:        "",
		Region:      "",
		Country:     self.Country,
		CountryCode: self.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  nil,
		CountryLocationId: self.CountryLocationId,
	}
}

// merged location and location group
type ConnectLocationId struct {
	// if set, the location is a direct connection to another device
	ClientId        *Id
	LocationId      *Id
	LocationGroupId *Id
}

func (self *ConnectLocationId) IsGroup() bool {
	return self.LocationGroupId != nil
}

func (self *ConnectLocationId) IsDevice() bool {
	return self.ClientId != nil
}

func (self *ConnectLocationId) Cmp(b *ConnectLocationId) int {
	// - direct
	// - group
	if self.ClientId != nil && b.ClientId != nil {
		if c := self.ClientId.Cmp(b.ClientId); c != 0 {
			return c
		}
	} else if self.ClientId != nil {
		return -1
	} else if b.ClientId != nil {
		return 1
	}

	if self.LocationGroupId != nil && b.LocationGroupId != nil {
		if c := self.LocationGroupId.Cmp(b.LocationGroupId); c != 0 {
			return c
		}
	} else if self.LocationGroupId != nil {
		return -1
	} else if b.LocationGroupId != nil {
		return 1
	}

	if self.LocationId != nil && b.LocationId != nil {
		if c := self.LocationId.Cmp(b.LocationId); c != 0 {
			return c
		}
	} else if self.LocationId != nil {
		return -1
	} else if b.LocationId != nil {
		return 1
	}

	return 0
}
