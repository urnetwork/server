package client


import (
	"context"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)


var cvcLog = logFn("connect_view_controller")


type ConnectViewController struct {
	ctx context.Context
	cancel context.CancelFunc
	client *connect.Client

	glViewController
}


func newConnectViewController(ctx context.Context, client *connect.Client) *ConnectViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ConnectViewController{
		ctx: cancelCtx,
		cancel: cancel,
		client: client,
		glViewController: *newGLViewController(),
	}
	vc.drawController = vc
	return vc
}


func (self *ConnectViewController) draw(g gl.Context) {
	// cvcLog("draw")

	g.ClearColor(self.bgRed, self.bgGreen, self.bgBlue, 1.0)
	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
}

func (self *ConnectViewController) drawLoopOpen() {
	self.frameRate = 24
}

func (self *ConnectViewController) drawLoopClose() {
}

func (self *ConnectViewController) Close() {
	cvcLog("close")

	self.cancel()
}
