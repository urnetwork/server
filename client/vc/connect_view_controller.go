package vc


import (
	"golang.org/x/mobile/gl"

	"bringyour.com/client"
)


var cvcLog = client.LogFn("connect_view_controller")


type ConnectViewController struct {
	glViewController
}


func NewConnectViewController() *ConnectViewController {
	vc := &ConnectViewController{
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
	// FIXME
	cvcLog("close")
}
