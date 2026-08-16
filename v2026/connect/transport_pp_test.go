//go:build unix

package connect

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"testing"

	"github.com/urnetwork/connect/v2026"
)

// FIXME add counting quic stream through nginx

// note: nginx appears to not officially support UDP PP at this time
// see: https://github.com/nginx/nginx/issues/1061
func DISABLE_TestPpNginxUdp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstreamConn, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP: net.ParseIP("127.0.0.1"),
	})
	connect.AssertEqual(t, err, nil)
	upstreamPort := upstreamConn.LocalAddr().(*net.UDPAddr).Port

	frontReservation, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP: net.ParseIP("127.0.0.1"),
	})
	connect.AssertEqual(t, err, nil)
	frontPort := frontReservation.LocalAddr().(*net.UDPAddr).Port
	connect.AssertEqual(t, frontReservation.Close(), nil)

	nginxConfig := fmt.Sprintf(`
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
        server 127.0.0.1:%d;

        random two least_conn;
    }

    server {
        listen 127.0.0.1:%d udp;

        proxy_pass test;
    }
}`, upstreamPort, frontPort)
	configPath := filepath.Join(t.TempDir(), "nginx.conf")
	connect.AssertEqual(t, os.WriteFile(configPath, []byte(nginxConfig), 0700), nil)
	fmt.Printf("config %s\n", configPath)

	nginxCmd := exec.Command("nginx", "-c", configPath)
	nginxCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = nginxCmd.Start()
	connect.AssertEqual(t, err, nil)
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
		Port: frontPort,
	})
	connect.AssertEqual(t, err, nil)
	defer conn.Close()
	realAddr := conn.LocalAddr().(*net.UDPAddr)

	listener := NewPpPacketConn(upstreamConn, DefaultWarpPpSettings())
	defer listener.Close()
	go func() {
		defer cancel()

		buffer := make([]byte, packetSize)

		n, addr, err := listener.ReadFrom(buffer)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, n, packetSize)
		connect.AssertEqual(t, buffer[0:n], sendPattern[0:n])

		addrPort := addr.(*net.UDPAddr).AddrPort()
		if addrPort.Addr().Is4() {
			connect.AssertEqual(t,
				addrPort,
				(&net.UDPAddr{
					IP:   realAddr.IP.To4(),
					Port: realAddr.Port,
					Zone: realAddr.Zone,
				}).AddrPort(),
			)
		} else {
			connect.AssertEqual(t,
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
				connect.AssertEqual(t, err, nil)
			}
		}()

		for {
			n, readAddr, err := listener.ReadFrom(buffer)
			select {
			case <-ctx.Done():
				return
			default:
			}
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, readAddr.(*net.UDPAddr).AddrPort(), addr.(*net.UDPAddr).AddrPort())
			connect.AssertEqual(t, n, packetSize)
			connect.AssertEqual(t, buffer[0:n], sendPattern[0:n])
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
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, n, packetSize)
			connect.AssertEqual(t, buffer[0:n], sendPattern[0:n])
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
			connect.AssertEqual(t, err, nil)
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

func TestPpNginxTcp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstreamListener, err := net.ListenTCP("tcp4", &net.TCPAddr{
		IP: net.ParseIP("127.0.0.1"),
	})
	connect.AssertEqual(t, err, nil)
	upstreamPort := upstreamListener.Addr().(*net.TCPAddr).Port

	frontReservation, err := net.ListenTCP("tcp4", &net.TCPAddr{
		IP: net.ParseIP("127.0.0.1"),
	})
	connect.AssertEqual(t, err, nil)
	frontPort := frontReservation.Addr().(*net.TCPAddr).Port
	connect.AssertEqual(t, frontReservation.Close(), nil)

	nginxConfig := fmt.Sprintf(`
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
        server 127.0.0.1:%d;

        random two least_conn;
    }

    server {
        listen 127.0.0.1:%d;

        proxy_pass test;
    }
}`, upstreamPort, frontPort)
	configPath := filepath.Join(t.TempDir(), "nginx.conf")
	connect.AssertEqual(t, os.WriteFile(configPath, []byte(nginxConfig), 0700), nil)
	fmt.Printf("config %s\n", configPath)

	nginxCmd := exec.Command("nginx", "-c", configPath)
	nginxCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = nginxCmd.Start()
	connect.AssertEqual(t, err, nil)
	// defer nginxCmd.Process.Kill()
	defer syscall.Kill(-nginxCmd.Process.Pid, syscall.SIGKILL)

	go func() {
		defer cancel()
		nginxCmd.Wait()
		fmt.Printf("NGINX done\n")
	}()

	packetSize := 1500

	sendPattern := make([]byte, packetSize)
	for i := range len(sendPattern) {
		sendPattern[i] = byte(i)
	}

	var serverReadCount atomic.Uint64
	var clientReadCount atomic.Uint64

	var conn *net.TCPConn
	dialDeadline := time.Now().Add(10 * time.Second)
	for conn == nil && time.Now().Before(dialDeadline) {
		conn, err = net.DialTCP("tcp4", nil, &net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: frontPort,
		})
		if err != nil {
			select {
			case <-ctx.Done():
				t.Fatalf("nginx exited before accepting connections: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	if conn == nil {
		t.Fatalf("nginx did not accept connections before deadline: %v", err)
	}
	defer conn.Close()
	realAddr := conn.LocalAddr().(*net.TCPAddr)

	listener := NewPpServerConn(upstreamListener, DefaultWarpPpSettings())
	defer listener.Close()
	go func() {
		defer cancel()

		buffer := make([]byte, packetSize)

		conn, err := listener.Accept()
		connect.AssertEqual(t, err, nil)
		defer conn.Close()

		addr := conn.RemoteAddr()

		addrPort := addr.(*net.TCPAddr).AddrPort()
		if addrPort.Addr().Is4() {
			connect.AssertEqual(t,
				addrPort,
				(&net.TCPAddr{
					IP:   realAddr.IP.To4(),
					Port: realAddr.Port,
					Zone: realAddr.Zone,
				}).AddrPort(),
			)
		} else {
			connect.AssertEqual(t,
				addrPort,
				(&net.TCPAddr{
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
				_, err := conn.Write(packet)
				select {
				case <-ctx.Done():
					return
				default:
				}
				connect.AssertEqual(t, err, nil)
			}
		}()

		for {
			n, err := io.ReadFull(conn, buffer)
			select {
			case <-ctx.Done():
				return
			default:
			}
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, n, packetSize)
			connect.AssertEqual(t, buffer[0:n], sendPattern[0:n])
			serverReadCount.Add(1)
		}
	}()

	go func() {
		defer cancel()

		buffer := make([]byte, packetSize)
		for {
			n, err := io.ReadFull(conn, buffer)
			select {
			case <-ctx.Done():
				return
			default:
			}
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, n, packetSize)
			connect.AssertEqual(t, buffer[0:n], sendPattern[0:n])
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
			connect.AssertEqual(t, err, nil)
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
