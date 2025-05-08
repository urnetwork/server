package main

import (
	"context"
	"net"
	// "net/netip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"testing"

	"github.com/go-playground/assert/v2"
)

func TestPpNginx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testDir, err := os.MkdirTemp("", "")
	assert.Equal(t, err, nil)

	nginxConfig := `
worker_processes auto;
events {
    worker_connections 8192;
    multi_accept on;
}

daemon off;

stream {
    proxy_protocol on;
    proxy_timeout 30s;

    upstream test {
        server 127.0.0.1:5556;

        random two least_conn;
    }

    server {
        listen 127.0.0.1:5555 udp;

        proxy_pass test;
    }
}`
	configPath := filepath.Join(testDir, "nginx.conf")
	os.WriteFile(configPath, []byte(nginxConfig), 0700)
	fmt.Printf("config %s\n", configPath)

	nginxCmd := exec.Command("nginx", "-c", configPath)
	nginxCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	nginxCmd.Start()
	// defer nginxCmd.Process.Kill()
	defer syscall.Kill(-nginxCmd.Process.Pid, syscall.SIGKILL)

	go func() {
		defer cancel()
		nginxCmd.Wait()
		fmt.Printf("NGINX done\n")
	}()

	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	packetSize := 1500

	sendPattern := make([]byte, packetSize)
	for i := range len(sendPattern) {
		sendPattern[i] = byte(i)
	}

	var serverReadCount atomic.Uint64
	var clientReadCount atomic.Uint64

	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 5555,
	})
	assert.Equal(t, err, nil)
	defer conn.Close()
	realAddr := conn.LocalAddr().(*net.UDPAddr)

	listener_, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 5556,
	})
	assert.Equal(t, err, nil)

	listener := NewPpPacketConn(listener_, DefaultWarpQuicPpSettings())
	defer listener.Close()
	go func() {
		defer cancel()

		buffer := make([]byte, packetSize)

		n, addr, err := listener.ReadFrom(buffer)
		assert.Equal(t, err, nil)
		assert.Equal(t, n, packetSize)
		assert.Equal(t, buffer[0:n], sendPattern[0:n])

		addrPort := addr.(*net.UDPAddr).AddrPort()
		if addrPort.Addr().Is4() {
			assert.Equal(t,
				addrPort,
				(&net.UDPAddr{
					IP:   realAddr.IP.To4(),
					Port: realAddr.Port,
					Zone: realAddr.Zone,
				}).AddrPort(),
			)
		} else {
			assert.Equal(t,
				addrPort,
				(&net.UDPAddr{
					IP:   realAddr.IP.To16(),
					Port: realAddr.Port,
					Zone: realAddr.Zone,
				}).AddrPort(),
			)
		}

		go func() {
			defer cancel()

			packet := sendPattern[:packetSize]
			for {
				_, err := listener.WriteTo(packet, addr)
				select {
				case <-ctx.Done():
					return
				default:
				}
				assert.Equal(t, err, nil)
			}
		}()

		for {
			n, readAddr, err := listener.ReadFrom(buffer)
			select {
			case <-ctx.Done():
				return
			default:
			}
			assert.Equal(t, err, nil)
			assert.Equal(t, readAddr.(*net.UDPAddr).AddrPort(), addr.(*net.UDPAddr).AddrPort())
			assert.Equal(t, n, packetSize)
			assert.Equal(t, buffer[0:n], sendPattern[0:n])
			serverReadCount.Add(1)
		}
	}()

	go func() {
		defer cancel()

		buffer := make([]byte, packetSize)
		for {
			n, err := conn.Read(buffer)
			select {
			case <-ctx.Done():
				return
			default:
			}
			assert.Equal(t, err, nil)
			assert.Equal(t, n, packetSize)
			assert.Equal(t, buffer[0:n], sendPattern[0:n])
			clientReadCount.Add(1)
		}
	}()
	go func() {
		defer cancel()

		packet := sendPattern[:packetSize]

		for {
			_, err := conn.Write(packet)
			select {
			case <-ctx.Done():
				return
			default:
			}
			assert.Equal(t, err, nil)
		}
	}()

	startTime := time.Now()
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}
	endTime := time.Now()

	seconds := float64(endTime.Sub(startTime)/time.Millisecond) / 1000.0

	// fmt.Printf("%d %d %.2fs\n", serverReadCount.Load(), clientReadCount.Load(), seconds)
	fmt.Printf(
		"to=%.2fMiB/s from=%.2fMiB/s\n",
		float64(serverReadCount.Load()*uint64(packetSize))/(1024*1024*seconds),
		float64(clientReadCount.Load()*uint64(packetSize))/(1024*1204*seconds),
	)

	cancel()

	// start nginx
	// start client
	// start server

	// send as many packets as possible in both directions
	// for 1 minute
	// count packets, compute throughput
	// make sure that listener sees the source address as the local source address always

}
