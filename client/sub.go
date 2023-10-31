package client


type Sub interface {
	Close()
}


type simpleSub struct {
	unsubFn func()
}

func newSub(unsubFn func()) Sub {
	return &simpleSub{
		unsubFn: unsubFn,
	}
}

func (self *simpleSub) Close() {
	self.unsubFn()
}
