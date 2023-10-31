package client


import (
	"context"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)


var pvcLog = logFn("provide_view_controller")


type ProvideViewController struct {
	ctx context.Context
	cancel context.CancelFunc
	
	client *connect.Client

	router Router

	glViewController
}

func newProvideViewController(ctx context.Context, client *connect.Client, router Router) *ProvideViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ProvideViewController{
		ctx: cancelCtx,
		cancel: cancel,
		client: client,
		router: router,
		glViewController: *newGLViewController(),
	}
	vc.drawController = vc
	return vc
}

func (self *ProvideViewController) draw(g gl.Context) {
	// pvcLog("draw")

	g.ClearColor(self.bgRed, self.bgGreen, self.bgBlue, 1.0)
	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
}

func (self *ProvideViewController) drawLoopOpen() {
	self.frameRate = 24
}

func (self *ProvideViewController) drawLoopClose() {
}

func (self *ProvideViewController) Close() {
	pvcLog("close")

	self.cancel()
}