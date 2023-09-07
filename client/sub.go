package client


type Sub struct {
	unsubFn func()
}

func (self *Sub) Close() {
	self.unsubFn()
}
