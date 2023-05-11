package vc


import (
	"runtime"
	"time"
	"sync"
	"context"

	"golang.org/x/mobile/gl"

	"bringyour.com/client/endpoint"
)


type GLViewCallback interface {
	Update()
}

type GLViewController interface {
	SurfaceCreated()
	SurfaceChanged(width int32, height int32)
	DrawFrame()
	Start(endpoints *endpoint.Endpoints, callback GLViewCallback)
	Stop()
}


type glViewController struct {
	glContext gl.Context
	glWorker gl.Worker
	// unix millis
	drawStartTime int64

	width int32
	height int32
	
	frameRate float32
	
	updateEvents chan func()


	loopMutex sync.Mutex
	loopStop *context.CancelFunc

}
// mirror GLSurfaceView.Renderer
func (self *glViewController) SurfaceCreated() {
	self.glContext, self.glWorker = gl.NewContext()
}
func (self *glViewController) SurfaceChanged(width int32, height int32) {
	self.width = width
	self.height = height
}
func (self *glViewController) DrawFrame() {
	self.drawStartTime = time.Now().UnixMilli()
	self.draw()

	workAvailable := self.glWorker.WorkAvailable()
	for {
		select {
		case <-workAvailable:
			self.glWorker.DoWork()
		// use default for non-blocking receive
		default:
			break
		}
	}
}
func (self *glViewController) draw() {
	// composited types should redefine this
}

func (self *glViewController) drawLoopOpen(endpoints *endpoint.Endpoints) {
	self.frameRate = 24
}

func (self *glViewController) drawLoopClose(endpoints *endpoint.Endpoints) {
}

func (self *glViewController) drawLoopUpdate(change func()) {
	self.updateEvents <- change
}


func (self *glViewController) drawLoop(ctx context.Context, endpoints *endpoint.Endpoints, callback GLViewCallback) {
	loop := func() {
		for {
			self.drawStartTime = time.Now().UnixMilli()
			self.draw()
			if callback != nil {
		  		callback.Update()
		  	}

			for {
				drainUpdateEvents := func() {
					for {
						select {
						case change := <-self.updateEvents:
					  		change()
					  	default:
					  		break
						}
					}
				}

				if 0 < self.frameRate {
					now := time.Now().UnixMilli()
					nextDrawTime := self.drawStartTime + int64(1000 / self.frameRate)
					if nextDrawTime < now {
				  		break
				  	}

					timeout := nextDrawTime - now
					select {
					case <-ctx.Done():
				  		return
				  	case change := <-self.updateEvents:
				  		change()
				  		drainUpdateEvents()
				  		// continue
				  	case <-time.After(time.Duration(timeout) * time.Millisecond):
				  		// continue
					}
				} else {
					select {
					case <-ctx.Done():
				  		return
				  	case change := <-self.updateEvents:
				  		change()
				  		drainUpdateEvents()
				  		// continue
					}
				}
			}
		}
	}

	self.drawLoopOpen(endpoints)
	loop()
	self.drawLoopClose(endpoints)
}
func (self *glViewController) Start(endpoints *endpoint.Endpoints, callback GLViewCallback) {
	self.loopMutex.Lock()
	defer self.loopMutex.Unlock()

	if self.loopStop == nil {
		ctx, stop := context.WithCancel(context.Background())
		self.loopStop = &stop

		go func() {
			runtime.LockOSThread()
			self.drawLoop(ctx, endpoints, callback)
		}()
	}
}
func (self *glViewController) Stop() {
	self.loopMutex.Lock()
	defer self.loopMutex.Unlock()

	if self.loopStop != nil {
		(*self.loopStop)()
		self.loopStop = nil
	}
}

