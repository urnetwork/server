package client

import (
	"context"
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

// type FilteredLocationsListener interface {
// 	FilteredLocationsChanged()
// }

type ConnectedProviderCountListener interface {
	ConnectedProviderCountChanged(count int32)
}

type ConnectGridListener interface {
	ConnectGridPointChanged(index int32)
}

type ConnectViewControllerV0 struct {
	ctx    context.Context
	cancel context.CancelFunc
	device *BringYourDevice

	stateLock sync.Mutex

	selectedLocation       *ConnectLocation
	connectionStatus       ConnectionStatus
	connectedProviderCount int32
	grid                   *ConnectGrid
	connectedTargetSize    int32
	connectedCurrentSize   int32

	selectedLocationListeners       *connect.CallbackList[SelectedLocationListener]
	connectionStatusListeners       *connect.CallbackList[ConnectionStatusListener]
	connectedProviderCountListeners *connect.CallbackList[ConnectedProviderCountListener]
	connectGridListeners            *connect.CallbackList[ConnectGridListener]
}

func newConnectViewControllerV0(ctx context.Context, device *BringYourDevice) *ConnectViewControllerV0 {
	cancelCtx, cancel := context.WithCancel(ctx)

	vm := &ConnectViewControllerV0{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		connectionStatus:       Disconnected,
		selectedLocation:       nil,
		connectedProviderCount: 0,
		grid:                   nil,
		connectedTargetSize:    0,
		connectedCurrentSize:   0,

		selectedLocationListeners:       connect.NewCallbackList[SelectedLocationListener](),
		connectionStatusListeners:       connect.NewCallbackList[ConnectionStatusListener](),
		connectedProviderCountListeners: connect.NewCallbackList[ConnectedProviderCountListener](),
		connectGridListeners:            connect.NewCallbackList[ConnectGridListener](),
	}
	return vm
}

func (vc *ConnectViewControllerV0) Start() {
	vc.monitorWindowEvents()
}

func (vc *ConnectViewControllerV0) Stop() {
	// FIXME
}

func (vc *ConnectViewControllerV0) Close() {
	connectVcLog("close")

	vc.cancel()
}

func (vc *ConnectViewControllerV0) GetConnectionStatus() ConnectionStatus {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	return vc.connectionStatus
}

func (vc *ConnectViewControllerV0) SetConnectionStatus(status ConnectionStatus) {
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

func (vc *ConnectViewControllerV0) AddConnectedProviderCountListener(listener ConnectedProviderCountListener) Sub {
	callbackId := vc.connectedProviderCountListeners.Add(listener)
	return newSub(func() {
		vc.connectedProviderCountListeners.Remove(callbackId)
	})
}

func (vc *ConnectViewControllerV0) connectedProviderCountChanged(count int32) {
	for _, listener := range vc.connectedProviderCountListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectedProviderCountChanged(count)
		})
	}
}

func (vc *ConnectViewControllerV0) connectGridChanged(index int32) {
	for _, listener := range vc.connectGridListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectGridPointChanged(index)
		})
	}
}

func (vc *ConnectViewControllerV0) setConnectedProviderCount(count int32) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.connectedProviderCount = count
	}()
	vc.connectedProviderCountChanged(count)
}

func (vc *ConnectViewControllerV0) GetConnectedProviderCount() int32 {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	return vc.connectedProviderCount
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
	// api call to get client ids, device.SETLOCATION
	// call callback

	// TODO store the connected locationId
	// TODO reset clientIds

	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.selectedLocation = location
	}()

	// self.setSelectedLocation(location)
	vc.SetConnectionStatus(Connecting)

	if location.IsDevice() {

		isCanceling := vc.isCanceling()

		if isCanceling {
			vc.SetConnectionStatus(Disconnected)
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
			vc.SetConnectionStatus(DestinationSet)
		}

	} else {
		specs := NewProviderSpecList()
		specs.Add(&ProviderSpec{
			LocationId:      location.ConnectLocationId.LocationId,
			LocationGroupId: location.ConnectLocationId.LocationGroupId,
		})

		isCanceling := vc.isCanceling()

		if isCanceling {
			vc.SetConnectionStatus(Disconnected)
		} else {
			vc.device.SetDestination(specs, ProvideModePublic)
			vc.setSelectedLocation(location)
			vc.setConnectedProviderCount(location.ProviderCount)
			// vc.setConnectionStatus(Connected)
			vc.SetConnectionStatus(DestinationSet)
		}
	}
}

func (vc *ConnectViewControllerV0) ConnectBestAvailable() {

	vc.SetConnectionStatus(Connecting)

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
				vc.SetConnectionStatus(Disconnected)
			} else {

				if err == nil && result.ProviderStats != nil {

					clientIds := []Id{}
					for _, provider := range result.ProviderStats.exportedList.values {
						clientId := provider.ClientId
						clientIds = append(clientIds, *clientId)
					}
					vc.setConnectedProviderCount(int32(len(clientIds)))
					// vc.setConnectionStatus(Connected)
				} else {
					vc.SetConnectionStatus(Disconnected)
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

// func (self *ConnectViewController) Shuffle() {
// 	self.device.Shuffle()
// }

// func (self *ConnectViewController) Broaden() {
// 	// FIXME
// 	var upLocation *ConnectLocation
// 	func() {
// 		self.stateLock.Lock()
// 		defer self.stateLock.Unlock()

// 		if self.selectedLocation != nil {
// 			if !self.selectedLocation.IsGroup() {
// 				// a location
// 				switch self.selectedLocation.LocationType {
// 				case LocationTypeCity:
// 					upLocation = self.selectedLocation.ToRegion()
// 				case LocationTypeRegion:
// 					upLocation = self.selectedLocation.ToCountry()
// 					// TODO Mark each country with an upgroup
// 					// case Country:
// 					// 	upLocation = self.activeLocation.UpGroup()
// 				}
// 			}
// 		}
// 	}()

// 	if upLocation != nil {
// 		self.Connect(upLocation)
// 	}
// }

// func (self *ConnectViewController) Reset() {
// 	// self.device.client().Reset()

// 	// todo how to reset?
// }

func (vc *ConnectViewControllerV0) monitorWindowEvents() {

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-vc.ctx.Done():
			return
		case <-ticker.C:
			if vc.connectionStatus == Connected || vc.connectionStatus == Connecting {

				func() {
					vc.stateLock.Lock()
					defer vc.stateLock.Unlock()
					windowEvents := vc.device.WindowEvents()
					if vc.connectedCurrentSize != int32(windowEvents.CurrentSize()) {
						vc.connectedCurrentSize = int32(windowEvents.CurrentSize())
					}

					if vc.connectedTargetSize != int32(windowEvents.TargetSize()) {
						vc.connectedTargetSize = int32(windowEvents.TargetSize())
					}

					for _, providerEvent := range windowEvents.providerEvents {
						index := vc.grid.FindPointIndexByClientId(providerEvent.ClientId)

						if index < 0 {
							// insert a new item in the grid

							index, err := vc.grid.InsertProviderEventPoint(providerEvent)
							if err != nil {
								connectVcLog("error creating a new provider event point: %s", err.Error())
							}
							vc.connectGridChanged(int32(index))

							continue
						}

						point := vc.grid.Points.Get(index)

						// check if eventTime is more recent than current point latest event time
						if providerEvent.EventTime.After(point.LatestEvent.EventTime) {
							// providerEvent is the most recent, update point
							point.LatestEvent.State = providerEvent.State
							point.LatestEvent.EventTime = providerEvent.EventTime

							vc.connectGridChanged(int32(index))
						}
					}

					// current size equals target size, mark connection status as connected
					if vc.connectionStatus == Connecting && vc.connectedCurrentSize == vc.connectedTargetSize {
						vc.SetConnectionStatus(Connected)
					}

				}()
			}
		}
	}
}

func (vc *ConnectViewControllerV0) Disconnect() {
	vc.device.RemoveDestination()
	vc.connectionStatus = Disconnected
	vc.connectionStatusChanged()
}

func (vc *ConnectViewControllerV0) GetGridPointAtIndex(index int32) *GridPoint {
	return vc.grid.Points.Get(int(index))
}

type GridPoint struct {
	X           int
	Y           int
	Plottable   bool
	LatestEvent *connect.ProviderEvent
}

type ProviderPoint struct {
	ClientId Id
	State    connect.ProviderState
}

type ConnectGrid struct {
	Width  int
	Height int
	Points *GridPointList
}

func (cg *ConnectGrid) FindPointIndexByClientId(id connect.Id) int {

	for i := 0; i < cg.Points.Len()-1; i++ {

		if cg.Points.Get(i).LatestEvent.ClientId == id {
			return i
		}

	}

	return -1
}

func (cg *ConnectGrid) InsertProviderEventPoint(latestEvent *connect.ProviderEvent) (int, error) {

	// filter plottable points
	var plottablePoints []*GridPoint
	for i := 0; i < cg.Points.Len()-1; i++ {

		if cg.Points.Get(i).Plottable {
			plottablePoints = append(plottablePoints, cg.Points.Get(i))
		}

	}

	if len(plottablePoints) == 0 {
		return -1, fmt.Errorf("no plottable points available")
	}

	// randomly select one & set latest event
	randomIndex := rand.Intn(len(plottablePoints))
	point := cg.Points.Get(randomIndex)
	point.LatestEvent = latestEvent

	return randomIndex, nil
}

func buildGridPointList(width int, height int) *GridPointList {
	gridPointsList := NewGridPointList()

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			gridPointsList.Add(
				&GridPoint{X: x, Y: y, Plottable: true, LatestEvent: nil},
			)
		}
	}

	return gridPointsList
}

// create a new grid based on targetClientSize sets specific points as plottable or unplottable
func (vc *ConnectViewControllerV0) initGrid(targetClientSize int32) *ConnectGrid {
	sideLength := int(math.Ceil(math.Sqrt(float64(targetClientSize))))
	if sideLength < 8 {
		sideLength = 8
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
				points.Get(y*width + x).Plottable = false
			}
		}
	}
	for y := height - oneEighth; y < height; y++ {
		for x := 0; x < width; x++ {
			if x < oneFourth || x >= width-oneFourth {
				points.Get(y*width + x).Plottable = false
			}
		}
	}

	// Set the first and last 1/8 columns of the second 1/8 and second to last 1/8 rows as unplottable
	for y := oneEighth; y < 2*oneEighth; y++ {
		for x := 0; x < width; x++ {
			if x < oneEighthWidth || x >= width-oneEighthWidth {
				points.Get(y*width + x).Plottable = false
			}
		}
	}
	for y := height - 2*oneEighth; y < height-oneEighth; y++ {
		for x := 0; x < width; x++ {
			if x < oneEighthWidth || x >= width-oneEighthWidth {
				points.Get(y*width + x).Plottable = false
			}
		}
	}

	return &ConnectGrid{Width: width, Height: height, Points: points}
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
