package client


import (
	"context"

	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
)


var svcLog = logFn("status_view_controller")


type StatusViewController struct {
	ctx context.Context
	cancel context.CancelFunc

	client *connect.Client

	// glInitialized bool

	glViewController
}

func newStatusViewController(ctx context.Context, client *connect.Client) *StatusViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &StatusViewController{
		ctx: cancelCtx,
		cancel: cancel,
		client: client,
		// glInitialized: false,
		glViewController: *newGLViewController(),
	}
	vc.drawController = vc
	return vc
}

func (self *StatusViewController) Start() {
	// FIXME
}

func (self *StatusViewController) Stop() {
	// FIXME	
}

func (self *StatusViewController) draw(g gl.Context) {
	// draw something
	// if !self.glInitialized {
	// 	self.frameBuffer := g.CreateFramebuffer()
	// 	self.glInitialized = true
	// }


	// g.BindFramebuffer(gl.DRAW_FRAMEBUFFER, self.frameBuffer)

	// CreateBuffer
	// BindBuffer

	// svcLog("draw")

	g.ClearColor(self.bgRed, self.bgGreen, self.bgBlue, 1.0)
	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)

	// g.glBegin(gl.GL_QUADS);
    //     g.glColor3d(1,0,0);
    //     glVertex3f(-1,-1,-10);
    //     glColor3d(1,1,0);
    //     glVertex3f(1,-1,-10);
    //     glColor3d(1,1,1);
    //     glVertex3f(1,1,-10);
    //     glColor3d(0,1,1);
    //     glVertex3f(-1,1,-10);
    // glEnd();
}

func (self *StatusViewController) drawLoopOpen() {
	self.frameRate = 24
}

func (self *StatusViewController) drawLoopClose() {
}

func (self *StatusViewController) Close() {
	svcLog("close")

	self.cancel()
}
