package vc


import (
	"golang.org/x/mobile/gl"

	"bringyour.com/client"
	"bringyour.com/client/endpoint"
)


var pvcLog = client.LogFn("provide_view_controller")


type ProvideViewController struct {
	glViewController
}


func NewProvideViewController() *ProvideViewController {
	vc := &ProvideViewController{
		glViewController: *newGLViewController(),
	}
	vc.drawController = vc
	return vc
}


func (self *ProvideViewController) draw(g gl.Context) {
	pvcLog("draw")

	g.ClearColor(self.bgRed, self.bgGreen, self.bgBlue, 1.0)
	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
}

func (self *ProvideViewController) drawLoopOpen(endpoints *endpoint.Endpoints) {
	self.frameRate = 24
}

func (self *ProvideViewController) drawLoopClose(endpoints *endpoint.Endpoints) {
}

