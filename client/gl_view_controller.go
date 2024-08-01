package client

import (
	"context"
	"runtime"
	"sync"
	"time"

	"golang.org/x/mobile/gl"
)

var glvcLog = logFn("gl_view_controller")

type GLViewCallback interface {
	Update()
}

type GLViewController interface {
	// assumes an opaque surface
	SetBackgroundColor(r, g, b float32)
	SurfaceCreated()
	SurfaceChanged(width, height int32)
	DrawFrame()
	StartGl(callback GLViewCallback)
	StopGl()
}

type glViewDrawController interface {
	draw(g gl.Context)
	drawLoopOpen()
	drawLoopClose()
}

// fixme set the clear color
type glViewController struct {
	glContext gl.Context
	glWorker  gl.Worker

	drawMutex sync.Mutex

	loopStop       *context.CancelFunc
	drawController glViewDrawController

	// unix millis
	drawStartTime int64

	updateNotice chan bool

	// draw state. Change this using `drawUpdate`
	bgRed     float32
	bgBlue    float32
	bgGreen   float32
	width     int32
	height    int32
	frameRate float32
}

func newGLViewController() *glViewController {
	return &glViewController{
		width:        0,
		height:       0,
		frameRate:    0,
		updateNotice: make(chan bool, 1),
	}
}

func (self *glViewController) SetBackgroundColor(r, g, b float32) {
	glvcLog("set background color (%.1f, %.1f, %.1f)", r, g, b)

	self.drawUpdate(func() {
		self.bgRed = r
		self.bgGreen = g
		self.bgBlue = b
	})
}

// mirror GLSurfaceView.Renderer
func (self *glViewController) SurfaceCreated() {
	self.glContext, self.glWorker = gl.NewContext()
	// self.glInitialized = true
}
func (self *glViewController) SurfaceChanged(width int32, height int32) {
	self.drawUpdate(func() {
		self.width = width
		self.height = height
	})
}
func (self *glViewController) DrawFrame() {
	// if !self.glInitialized {
	// 	self.glContext, self.glWorker = gl.NewContext()
	// 	self.glInitialized = true
	// }

	self.drawMutex.Lock()
	defer self.drawMutex.Unlock()

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

func (self *glViewController) drawUpdate(change func()) {
	self.drawMutex.Lock()
	defer self.drawMutex.Unlock()

	change()

	select {
	case self.updateNotice <- true:
	default:
		// already notified
	}
}

func (self *glViewController) drawLoop(ctx context.Context, callback GLViewCallback) {
	loop := func() {
	loop:
		for {
			self.drawStartTime = time.Now().UnixMilli()
			if callback != nil {
				callback.Update()
			}

		wait:
			for {
				if 0 < self.frameRate {
					now := time.Now().UnixMilli()
					nextDrawTime := self.drawStartTime + int64(1000/self.frameRate)
					if nextDrawTime < now {
						break
					}

					timeout := nextDrawTime - now
					select {
					case <-ctx.Done():
						break loop
					case <-self.updateNotice:
						continue wait
					case <-time.After(time.Duration(timeout) * time.Millisecond):
						continue wait
					}
				} else {
					select {
					case <-ctx.Done():
						break loop
					case <-self.updateNotice:
						continue wait
					}
				}
			}
		}
	}

	if self.drawController != nil {
		self.drawController.drawLoopOpen()
		loop()
		self.drawController.drawLoopClose()
	}
}
func (self *glViewController) StartGl(callback GLViewCallback) {
	self.drawMutex.Lock()
	defer self.drawMutex.Unlock()

	if self.loopStop == nil {
		ctx, stop := context.WithCancel(context.Background())
		self.loopStop = &stop

		go func() {
			// see https://github.com/golang/go/wiki/LockOSThread
			runtime.LockOSThread()
			self.drawLoop(ctx, callback)
		}()
	}
}
func (self *glViewController) StopGl() {
	self.drawMutex.Lock()
	defer self.drawMutex.Unlock()

	if self.loopStop != nil {
		(*self.loopStop)()
		self.loopStop = nil
	}
}
