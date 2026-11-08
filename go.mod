module github.com/urnetwork/sidecar/sidecarctl

go 1.27.3

// §4.7 has every module of this repository repeat the four sibling replaces, so that a require of
// connect or glog resolves as the root's do. warp is C-23's: certs renew reads services.yml through
// warp's own loader.

replace github.com/urnetwork/connect => ../../connect

replace github.com/pion/sctp => ../../connect/sctp

replace github.com/urnetwork/glog => ../../glog

replace gvisor.dev/gvisor => ../../gvisor

replace github.com/urnetwork/warp => ../../warp

require (
	github.com/coreos/go-semver v0.3.1
	github.com/urnetwork/warp v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/exp v0.0.0-20260727155853-b88d891fe743 // indirect
