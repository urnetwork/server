package tcplogger

import (
	"encoding/csv"
	"fmt"
	"os"
	"sync"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

type TCPLogger struct {
	w         *csv.Writer
	closeFile func() error
	mu        sync.Mutex
}

func NewLogger(fileName string) (*TCPLogger, error) {
	f, err := os.Create(fileName)
	if err != nil {
		return nil, err
	}

	w := csv.NewWriter(f)
	return &TCPLogger{
		w:         w,
		closeFile: f.Close,
	}, nil

}

func (l *TCPLogger) Log(packet []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	pkt := gopacket.NewPacket(packet, layers.LayerTypeIPv4, gopacket.Default)
	if pkt == nil {
		return nil
	}

	ippacket := pkt.Layer(layers.LayerTypeIPv4)
	if ippacket == nil {
		return nil
	}

	ip := ippacket.(*layers.IPv4)

	tp := pkt.Layer(layers.LayerTypeTCP)
	if tp == nil {
		return nil
	}
	tcp, _ := tp.(*layers.TCP)

	// return l.w.Write([]string{time.Now().Format("15:04:05.000000000"), ip.SrcIP.String(), tcp.SrcPort.String(), ip.DstIP.String(), tcp.DstPort.String(), fmt.Sprintf("%d", tcp.Seq), fmt.Sprintf("%d", tcp.Ack), fmt.Sprintf("%d", len(packet))})

	defer l.w.Flush()
	return l.w.Write([]string{ip.SrcIP.String(), tcp.SrcPort.String(), ip.DstIP.String(), tcp.DstPort.String(), fmt.Sprintf("%d", tcp.Seq), fmt.Sprintf("%d", tcp.Ack), fmt.Sprintf("%d", len(tcp.Payload))})

}

func (l *TCPLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.w.Flush()
	return l.closeFile()
}
