package vc


import (
	"golang.org/x/mobile/gl"

	"bringyour.com/client"
	"bringyour.com/client/endpoint"
)


var svLog = client.LogFn("status_view_controller")


type StatusViewController struct {

	// glInitialized bool

	glViewController
}


func NewStatusViewController() *StatusViewController {
	vc := &StatusViewController{
		// glInitialized: false,
		glViewController: *newGLViewController(),
	}
	vc.drawController = vc
	return vc
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

	svLog("draw")

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

func (self *StatusViewController) drawLoopOpen(endpoints *endpoint.Endpoints) {
	self.frameRate = 24
}

func (self *StatusViewController) drawLoopClose(endpoints *endpoint.Endpoints) {
}
