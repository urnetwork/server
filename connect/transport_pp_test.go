//go:build unix

package connect

import (
	"bytes"
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

	"github.com/urnetwork/connect"
)

// FIXME add counting quic stream through nginx

// nginxUdpProxyV2TestBinary locates the explicitly selected binary first and
// then the build owned by warp/lb. The temporary candidate keeps existing
// developer builds usable while they migrate to the repository-local path.
func nginxUdpProxyV2TestBinary(t *testing.T) string {
	t.Helper()
	if configuredBinary := os.Getenv("NGINX_UDP_PROXY_V2_BINARY"); configuredBinary != "" {
		resolvedBinary, err := exec.LookPath(configuredBinary)
		if err != nil {
			t.Fatalf("NGINX_UDP_PROXY_V2_BINARY=%q is not executable: %v", configuredBinary, err)
		}
		return resolvedBinary
	}

	candidates := []string{
		filepath.Join("..", "..", "warp", "lb", "build", "nginx-local", "sbin", "nginx"),
		"/tmp/urnetwork-nginx-udp-v2-full/sbin/nginx",
	}
	for _, candidate := range candidates {
		resolvedBinary, err := exec.LookPath(candidate)
		if err == nil {
			return resolvedBinary
		}
	}

	t.Skip("pinned NGINX 1.31.4 build not found; run `make nginx_local` in warp/lb")
	return ""
}

// NGINX may keep a UDP pseudo-session for 30 seconds. Preserve the PP source
// mapping beyond that lifetime so every reply in the session remains routable.
func TestDefaultWarpPpTimeoutOutlivesNginxUdpSession(t *testing.T) {
	settings := DefaultWarpPpSettings()
	if settings.ProxyTimeout <= 30*time.Second {
		t.Fatalf("PP source mapping timeout=%s must exceed nginx UDP session timeout=30s", settings.ProxyTimeout)
	}
}

// The pinned NGINX capability preserves client identity and payloads for raw
// UDP and both DNS envelope modes in both directions.
func TestPpNginxUdpV2(t *testing.T) {
	nginxBinary := nginxUdpProxyV2TestBinary(t)
	versionOutput, err := exec.Command(nginxBinary, "-V").CombinedOutput()
	if err != nil {
		t.Fatalf("read NGINX build configuration: %v: %s", err, versionOutput)
	}
	if !bytes.Contains(versionOutput, []byte("--with-stream")) {
		t.Fatalf("NGINX binary omits the stream module: %s", versionOutput)
	}

	upstreamConn, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP: net.ParseIP("127.0.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = upstreamConn.Close()
	})
	upstreamPort := upstreamConn.LocalAddr().(*net.UDPAddr).Port

	frontReservation, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP: net.ParseIP("127.0.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	frontPort := frontReservation.LocalAddr().(*net.UDPAddr).Port
	if err := frontReservation.Close(); err != nil {
		t.Fatal(err)
	}

	tempDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(tempDir, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	nginxConfig := fmt.Sprintf(`
worker_processes 2;
pid nginx.pid;
error_log stderr info;
events {
    worker_connections 128;
}

daemon off;

stream {
    proxy_timeout 30s;

    upstream test {
        server 127.0.0.1:%d;
    }

    server {
        listen 127.0.0.1:%d udp reuseport;
        proxy_protocol v2;
        proxy_pass test;
    }
}`, upstreamPort, frontPort)
	configPath := filepath.Join(tempDir, "nginx.conf")
	if err := os.WriteFile(configPath, []byte(nginxConfig), 0600); err != nil {
		t.Fatal(err)
	}

	var nginxOutput bytes.Buffer
	nginxCmd := exec.Command(nginxBinary, "-p", tempDir+string(os.PathSeparator), "-c", configPath)
	nginxCmd.Stdout = &nginxOutput
	nginxCmd.Stderr = &nginxOutput
	nginxCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := nginxCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-nginxCmd.Process.Pid, syscall.SIGQUIT)
		waitDone := make(chan error, 1)
		go func() {
			waitDone <- nginxCmd.Wait()
		}()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-nginxCmd.Process.Pid, syscall.SIGKILL)
			<-waitDone
		}
		if t.Failed() {
			t.Logf("nginx output:\n%s", nginxOutput.String())
		}
	})
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(tempDir, "nginx.pid")); err == nil {
			break
		}
		if readyDeadline.Before(time.Now()) {
			t.Fatalf("nginx did not become ready:\n%s", nginxOutput.String())
		}
		select {
		case <-time.After(5 * time.Millisecond):
		}
	}

	listener := NewPpPacketConn(upstreamConn, DefaultWarpPpSettings())
	t.Cleanup(func() {
		_ = listener.Close()
	})

	clients := make([]*net.UDPConn, 2)
	for i := range clients {
		client, err := net.DialUDP("udp4", nil, &net.UDPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: frontPort,
		})
		if err != nil {
			t.Fatal(err)
		}
		clients[i] = client
		t.Cleanup(func() {
			_ = client.Close()
		})
	}

	serverBuffer := make([]byte, 1500)
	clientBuffer := make([]byte, 1500)
	for sequence := range 64 {
		clientIndex := sequence % len(clients)
		client := clients[clientIndex]
		request := fmt.Appendf(nil, "client=%d sequence=%d ", clientIndex, sequence)
		for len(request) < 1400 {
			request = append(request, byte(sequence))
		}

		if _, err := client.Write(request); err != nil {
			t.Fatal(err)
		}
		if err := listener.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, realAddr, err := listener.ReadFrom(serverBuffer)
		if err != nil {
			t.Fatalf("read request %d through nginx: %v", sequence, err)
		}

		if !bytes.Equal(serverBuffer[:n], request) {
			t.Fatalf("request %d changed in transit", sequence)
		}
		wantAddr := client.LocalAddr().(*net.UDPAddr).AddrPort()
		gotAddr := realAddr.(*net.UDPAddr).AddrPort()
		if gotAddr.Port() != wantAddr.Port() || gotAddr.Addr().Unmap() != wantAddr.Addr().Unmap() {
			t.Fatalf("request %d source = %s, want original client %s", sequence, gotAddr, wantAddr)
		}

		reply := fmt.Appendf(nil, "reply client=%d sequence=%d", clientIndex, sequence)
		if _, err := listener.WriteTo(reply, realAddr); err != nil {
			t.Fatalf("write reply %d through nginx: %v", sequence, err)
		}
		if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, err = client.Read(clientBuffer)
		if err != nil {
			t.Fatalf("read reply %d through nginx: %v", sequence, err)
		}
		if !bytes.Equal(clientBuffer[:n], reply) {
			t.Fatalf("reply %d changed in transit", sequence)
		}
	}

	// Prove the production transform order too: NGINX adds PPv2, the server
	// removes it, and only then may the DNS envelope be decoded. Exercise both
	// client DNS modes in both directions through that stack.
	if err := listener.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	translationCtx, translationCancel := context.WithCancel(context.Background())
	t.Cleanup(translationCancel)
	serverTranslationSettings := connect.DefaultPacketTranslationSettings()
	serverTranslationSettings.DnsTlds = [][]byte{[]byte("ur.xyz.")}
	translatedServer, err := connect.NewPacketTranslation(
		translationCtx,
		connect.PacketTranslationModeDecode53,
		listener,
		serverTranslationSettings,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = translatedServer.Close()
	})

	for _, clientMode := range []connect.PacketTranslationMode{
		connect.PacketTranslationModeDns,
		connect.PacketTranslationModeDnsPump,
	} {
		func() {
			clientConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
			if err != nil {
				t.Fatal(err)
			}
			clientTranslationSettings := connect.DefaultPacketTranslationSettings()
			clientTranslationSettings.DnsTlds = [][]byte{[]byte("ur.xyz.")}
			translatedClient, err := connect.NewPacketTranslation(
				translationCtx,
				clientMode,
				clientConn,
				clientTranslationSettings,
			)
			if err != nil {
				_ = clientConn.Close()
				t.Fatal(err)
			}
			defer translatedClient.Close()

			deadline := time.Now().Add(5 * time.Second)
			if err := translatedClient.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if err := translatedServer.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			request := bytes.Repeat([]byte{byte(len(clientMode))}, 1400)
			frontAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: frontPort}
			if n, err := translatedClient.WriteTo(request, frontAddr); err != nil || n != len(request) {
				t.Fatalf("write %s request through DNS and PPv2: n=%d err=%v", clientMode, n, err)
			}
			n, realAddr, err := translatedServer.ReadFrom(serverBuffer)
			if err != nil {
				t.Fatalf("read %s request through DNS and PPv2: %v", clientMode, err)
			}
			if !bytes.Equal(serverBuffer[:n], request) {
				t.Fatalf("%s request changed across DNS and PPv2 transforms", clientMode)
			}
			wantAddr := clientConn.LocalAddr().(*net.UDPAddr).AddrPort()
			gotAddr := realAddr.(*net.UDPAddr).AddrPort()
			if gotAddr.Port() != wantAddr.Port() || gotAddr.Addr().Unmap() != wantAddr.Addr().Unmap() {
				t.Fatalf("%s source = %s, want original client %s", clientMode, gotAddr, wantAddr)
			}

			reply := append([]byte("reply "), request[:1300]...)
			if n, err := translatedServer.WriteTo(reply, realAddr); err != nil || n != len(reply) {
				t.Fatalf("write %s reply through DNS and PPv2: n=%d err=%v", clientMode, n, err)
			}
			n, _, err = translatedClient.ReadFrom(clientBuffer)
			if err != nil {
				t.Fatalf("read %s reply through DNS and PPv2: %v", clientMode, err)
			}
			if !bytes.Equal(clientBuffer[:n], reply) {
				t.Fatalf("%s reply changed across DNS and PPv2 transforms", clientMode)
			}
		}()
	}
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
