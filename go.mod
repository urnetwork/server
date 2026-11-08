module github.com/urnetwork/sidecar/sidecarctl

go 1.27.3

// Nothing here requires these yet. §4.7 has every module of this repository repeat the four
// sibling replaces, so a require added later resolves as the root's do.

replace github.com/urnetwork/connect => ../../connect

replace github.com/pion/sctp => ../../connect/sctp

replace github.com/urnetwork/glog => ../../glog

replace gvisor.dev/gvisor => ../../gvisor
