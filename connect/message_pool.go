package connect

import connectcore "github.com/urnetwork/connect"

const serviceMessagePoolByteCount connectcore.ByteCount = 16 << 30

// ConfigureMessagePools applies the process-wide free-buffer allowance shared
// by Connect and Alt. It leaves borrowed payloads and their owners untouched.
func ConfigureMessagePools() {
	resizeServiceMessagePools(serviceMessagePoolByteCount)
}

func resizeServiceMessagePools(totalByteCount connectcore.ByteCount) {
	// The legacy one-argument form repeats its cap for every large class.
	// Divide one total allowance across packet and large-object classes,
	// matching the Proxy policy and the pool's initial 1:2 byte split.
	packetByteCount := totalByteCount / 3
	connectcore.ResizeMessagePools(packetByteCount, totalByteCount-packetByteCount)
}
