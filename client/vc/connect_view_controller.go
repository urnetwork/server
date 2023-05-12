package vc


import (
	"golang.org/x/mobile/gl"

	"bringyour.com/client"
	"bringyour.com/client/endpoint"
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
	cvcLog("draw")

	g.ClearColor(0, 0, 255, 1.0)
	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
}

func (self *ConnectViewController) drawLoopOpen(endpoints *endpoint.Endpoints) {
	self.frameRate = 24
}

func (self *ConnectViewController) drawLoopClose(endpoints *endpoint.Endpoints) {
}
