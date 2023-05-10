package client


import (
	"runtime"
	"time"
	"sync"
	"context"

	"golang.org/x/mobile/gl"
)

/*
type Event struct {
}


type ClientKernel struct {
}

func NewClientKernel() *ClientKernel {
	return nil
}


// returns a delay to process the next event
func (self *ClientKernel) Next(events []Event) (nativeEvents []Event, timeout int64) {


	// go thread that pings a url five times in a loop and sets the data in a channel



	return []Event{}, 0
}

func (self *ClientKernel) SetEventCallback(callback *EventCallback) {
	// todo
}


type EventCallback interface {
	Update(events []Event)
}

*/




// called from https://developer.android.com/reference/android/opengl/GLSurfaceView.Renderer

// GLBind
/*
surface created:
go func() {
	runtime.LockOSThread()
	// ... platform-specific cgo call to bind a C OpenGL context
	// into thread-local storage.

	glctx, worker := gl.NewContext()
	workAvailable := worker.WorkAvailable()
	go userAppCode(glctx)
	for {
		select {
		case <-workAvailable:
			worker.DoWork()
		case <-drawEvent:
			// ... platform-specific cgo call to draw screen
		}
	}
}()


rander:
force a render on glctx


*/


type GLViewCallback interface {
	Update()
}

type GLView interface {
	SurfaceCreated()
	SurfaceChanged(width int, height int)
	DrawFrame()
	Start(callback *GLViewCallback)
	Stop()
}


type glView struct {
	glContext gl.Context
	glWorker gl.Worker
	// unix millis
	drawStartTime int64

	width int
	height int
	
	frameRate float32
	
	updateEvents chan func()


	loopMutex sync.Mutex
	loopStop *context.CancelFunc

}
// mirror GLSurfaceView.Renderer
func (self *glView) SurfaceCreated() {
	self.glContext, self.glWorker = gl.NewContext()
}
func (self *glView) SurfaceChanged(width int, height int) {
	self.width = width
	self.height = height
}
func (self *glView) DrawFrame() {
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
func (self *glView) draw() {
	// composited types should redefine this
}

func (self *glView) drawLoopOpen() {
	self.frameRate = 24
}

func (self *glView) drawLoopClose() {
}

func (self *glView) drawLoopUpdate(change func()) {
	self.updateEvents <- change
}


func (self *glView) drawLoop(ctx context.Context, callback *GLViewCallback) {
	loop := func() {
		for {
			self.drawStartTime = time.Now().UnixMilli()
			self.draw()

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
				  		(*callback).Update()
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
				  		(*callback).Update()
				  		// continue
					}
				}
			}
		}
	}

	self.drawLoopOpen()
	loop()
	self.drawLoopClose()
}
func (self *glView) Start(callback *GLViewCallback) {
	self.loopMutex.Lock()
	defer self.loopMutex.Unlock()

	if self.loopStop == nil {
		ctx, stop := context.WithCancel(context.Background())
		self.loopStop = &stop

		go func() {
			runtime.LockOSThread()
			self.drawLoop(ctx, callback)
		}()
	}
}
func (self *glView) Stop() {
	self.loopMutex.Lock()
	defer self.loopMutex.Unlock()

	if self.loopStop != nil {
		(*self.loopStop)()
		self.loopStop = nil
	}
}




type StatusView struct {
	glView
}





