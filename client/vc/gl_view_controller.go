package vc


import (
	"runtime"
	"time"
	"sync"
	"context"

	"golang.org/x/mobile/gl"

	// "bringyour.com/client"
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

type GLViewDrawController interface {
	draw(g gl.Context)
	drawLoopOpen(endpoints *endpoint.Endpoints)
	drawLoopClose(endpoints *endpoint.Endpoints)
}


// fixme set the clear color
type glViewController struct {
	glInitialized bool
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

	drawController GLViewDrawController
}

func newGLViewController() *glViewController {
	return &glViewController{
		glInitialized: false,
		width: 0,
		height: 0,
		frameRate: 0,
		updateEvents: make(chan func(), 64),
	}
}

// mirror GLSurfaceView.Renderer
func (self *glViewController) SurfaceCreated() {
	self.glContext, self.glWorker = gl.NewContext()
	// self.glInitialized = true
}
func (self *glViewController) SurfaceChanged(width int32, height int32) {
	self.drawLoopUpdate(func() {
		self.width = width
		self.height = height
	})
}
func (self *glViewController) DrawFrame() {
	// if !self.glInitialized {
	// 	self.glContext, self.glWorker = gl.NewContext()
	// 	self.glInitialized = true
	// }

	self.drawStartTime = time.Now().UnixMilli()
	if self.drawController != nil {
		self.drawController.draw(self.glContext)
	}

	workAvailable := self.glWorker.WorkAvailable()
	drainLoop:
	for {
		select {
		case <-workAvailable:
			self.glWorker.DoWork()
		// use default for non-blocking receive
		default:
			break drainLoop
		}
	}
}

// // fixme figure out how to use the composed struct "override"
// func (self *glViewController) draw(g gl.Context) {
// 	// composited types should redefine this
// 	client.Logger().Printf("draw base\n")

// 	g.ClearColor(255, 0, 0, 1.0)
// 	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
// }

// func (self *glViewController) drawLoopOpen(endpoints *endpoint.Endpoints) {
// 	self.frameRate = 24
// 	// composited types should redefine this
// }

// func (self *glViewController) drawLoopClose(endpoints *endpoint.Endpoints) {
// 	// composited types should redefine this
// }

func (self *glViewController) drawLoopUpdate(change func()) {
	select {
	    case self.updateEvents <- change:
	    	// ok
	    default:
	    	// dropped due to back pressure
	}
}


func (self *glViewController) drawLoop(ctx context.Context, endpoints *endpoint.Endpoints, callback GLViewCallback) {
	loop := func() {
		loop:
		for {
			self.drawStartTime = time.Now().UnixMilli()
			// self.draw()
			if callback != nil {
		  		callback.Update()
		  	}

			for {
				drainUpdateEvents := func() {
					drainLoop:
					for {
						select {
						case change := <-self.updateEvents:
					  		change()
					  	default:
					  		break drainLoop
						}
					}
				}

				drainUpdateEvents()

				if 0 < self.frameRate {
					now := time.Now().UnixMilli()
					nextDrawTime := self.drawStartTime + int64(1000 / self.frameRate)
					if nextDrawTime < now {
				  		break
				  	}

					timeout := nextDrawTime - now
					select {
					case <-ctx.Done():
				  		break loop
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
				  		break loop
				  	case change := <-self.updateEvents:
				  		change()
				  		drainUpdateEvents()
				  		// continue
					}
				}
			}
		}
	}

	if self.drawController != nil {
		self.drawController.drawLoopOpen(endpoints)
		loop()
		self.drawController.drawLoopClose(endpoints)
	}
}
func (self *glViewController) Start(endpoints *endpoint.Endpoints, callback GLViewCallback) {
	self.loopMutex.Lock()
	defer self.loopMutex.Unlock()

	if self.loopStop == nil {
		ctx, stop := context.WithCancel(context.Background())
		self.loopStop = &stop

		go func() {
			// see https://github.com/golang/go/wiki/LockOSThread
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

