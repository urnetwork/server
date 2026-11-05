// A fixed private Unix peer session authenticates only root PID1 and performs
// the closed manager proof. It never connects TCP, forks or negotiates fd IPC.
package competitioncore

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"
)

// Every protocol allocation, call count, session byte and timeout has an owner.
// Nonpositive settings refuse proof, never permit an unbounded fallback.
type ManagerSettings struct {
	Timeout        time.Duration
	AuthBytes      int
	HeaderBytes    int
	BodyBytes      int
	MessageBytes   int
	SessionBytes   int
	StringBytes    int
	ByteArrayBytes int
	ArrayElements  int
	MaximumCalls   int
	MaximumDepth   int
	ControlBytes   int
}

// One proof is GetUnit, twelve fixed Properties.Get calls, then GetUnit again.
// This finite PID1 RPC cost still needs measured crash-storm native capacity.
func DefaultManagerSettings() ManagerSettings {
	return ManagerSettings{Timeout: time.Second, AuthBytes: 256, HeaderBytes: 4096, BodyBytes: 32 * 1024,
		MessageBytes: 64 * 1024, SessionBytes: 128 * 1024, StringBytes: 4096, ByteArrayBytes: 512,
		ArrayElements: 16, MaximumCalls: 14, MaximumDepth: 4, ControlBytes: 256}
}

// Closed types need no recursion; depth less than two cannot contain a variant.
func validateManagerSettings(settings ManagerSettings) error {
	if settings.Timeout <= 0 || settings.AuthBytes <= 0 || settings.HeaderBytes <= 0 || settings.BodyBytes <= 0 || settings.MessageBytes < 16 ||
		settings.SessionBytes < settings.MessageBytes || settings.StringBytes <= 0 || settings.ByteArrayBytes <= 0 || settings.ArrayElements <= 0 ||
		settings.MaximumCalls <= 0 || settings.MaximumDepth < 2 || settings.ControlBytes < syscall.CmsgSpace(4) || settings.HeaderBytes > settings.MessageBytes || settings.BodyBytes > settings.MessageBytes {
		return errors.New("manager proof requires positive finite protocol bounds")
	}
	return nil
}

// Only private package tests replace transport/credentials, never caller input.
type managerPeer struct {
	uid uint32
	pid int32
}

type managerStream interface {
	ReadMsg([]byte, []byte) (int, int, int, error)
	Write([]byte) (int, error)
	SetDeadline(time.Time) error
	Peer() (managerPeer, error)
	Pending(int) error
	Close() error
}

type nativeManagerStream struct{ connection *net.UnixConn }

// No environment, address discovery, alternative socket or caller path exists.
func connectNativeManager(ctx context.Context) (managerStream, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", managerSocketPath)
	if err != nil {
		return nil, err
	}
	unix, ok := connection.(*net.UnixConn)
	if !ok {
		connection.Close()
		return nil, errors.New("private manager is not a Unix connection")
	}
	return &nativeManagerStream{connection: unix}, nil
}

func (self *nativeManagerStream) ReadMsg(content, control []byte) (int, int, int, error) {
	n, oob, flags, _, err := self.connection.ReadMsgUnix(content, control)
	return n, oob, flags, err
}

func (self *nativeManagerStream) Write(content []byte) (int, error) {
	return self.connection.Write(content)
}
func (self *nativeManagerStream) SetDeadline(deadline time.Time) error {
	return self.connection.SetDeadline(deadline)
}
func (self *nativeManagerStream) Close() error { return self.connection.Close() }

// Peer identity is obtained from the kernel, not from D-Bus sender strings.
func (self *nativeManagerStream) Peer() (managerPeer, error) {
	raw, err := self.connection.SyscallConn()
	if err != nil {
		return managerPeer{}, err
	}
	var credential *syscall.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credential, controlErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || controlErr != nil || credential == nil {
		return managerPeer{}, errors.New("private manager has no kernel peer credentials")
	}
	return managerPeer{uid: credential.Uid, pid: credential.Pid}, nil
}

// Any received rights are immediately closed even when malformed/truncated
// framing is rejected. No reply to this protocol is allowed to transfer an fd.
func rejectManagerControl(control []byte, flags int) error {
	messages, parseErr := syscall.ParseSocketControlMessage(control)
	seen := map[int]bool{}
	for _, message := range messages {
		if message.Header.Level == syscall.SOL_SOCKET && message.Header.Type == syscall.SCM_RIGHTS {
			rights, _ := syscall.ParseUnixRights(&message)
			for _, fd := range rights {
				if !seen[fd] {
					syscall.Close(fd)
					seen[fd] = true
				}
			}
		}
	}
	if len(control) != 0 || parseErr != nil || flags&(syscall.MSG_CTRUNC|syscall.MSG_TRUNC) != 0 {
		return errors.New("manager supplied unexpected ancillary data or truncation")
	}
	return nil
}

// No subscribed signals are expected. Peek once without waiting to reject
// already queued trailing bytes/rights; closing also drops any unreceived rights.
func (self *nativeManagerStream) Pending(controlBytes int) error {
	raw, err := self.connection.SyscallConn()
	if err != nil {
		return err
	}
	var pendingErr error
	if err := raw.Control(func(fd uintptr) {
		data, control := make([]byte, 1), make([]byte, controlBytes)
		n, oob, flags, _, err := syscall.Recvmsg(int(fd), data, control, syscall.MSG_DONTWAIT|syscall.MSG_PEEK|syscall.MSG_CMSG_CLOEXEC)
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			return
		}
		if err != nil {
			pendingErr = err
			return
		}
		if controlErr := rejectManagerControl(control[:oob], flags); controlErr != nil {
			pendingErr = controlErr
			return
		}
		if n != 0 {
			pendingErr = errors.New("manager queued unexpected trailing protocol data")
		}
	}); err != nil {
		return err
	}
	return pendingErr
}

// One session has no general-purpose send/read API exposed to callers.
type managerBus struct {
	stream       managerStream
	settings     ManagerSettings
	ctx          context.Context
	used, calls  int
	replySerials map[uint32]bool
}

// Budget is charged before each write and after every bounded recvmsg segment.
func (self *managerBus) charge(size int) error {
	if size < 0 || size > self.settings.SessionBytes-self.used || self.ctx.Err() != nil {
		return errors.New("manager proof session exceeded its original budget")
	}
	self.used += size
	return nil
}

func (self *managerBus) write(content []byte) error {
	if err := self.charge(len(content)); err != nil {
		return err
	}
	for len(content) != 0 {
		n, err := self.stream.Write(content)
		if err != nil || n <= 0 || n > len(content) {
			return errors.New("incomplete manager request write")
		}
		content = content[n:]
	}
	return self.ctx.Err()
}

// Every read uses recvmsg, including authentication, so rights cannot sneak
// through a buffered Read call. No FD negotiation command is ever transmitted.
func (self *managerBus) read(content []byte) error {
	control := make([]byte, self.settings.ControlBytes)
	for len(content) != 0 {
		if self.ctx.Err() != nil {
			return self.ctx.Err()
		}
		n, oob, flags, err := self.stream.ReadMsg(content, control)
		if n < 0 || n > len(content) || oob < 0 || oob > len(control) {
			return errors.New("manager read exceeded its declared buffer")
		}
		controlErr := rejectManagerControl(control[:oob], flags)
		if self.charge(n+oob) != nil || controlErr != nil || err != nil || n == 0 {
			return errors.New("incomplete, ancillary-bearing or expired manager response")
		}
		content = content[n:]
	}
	return nil
}

// The fixed EXTERNAL identity is the ASCII uid '0' represented as hex 30.
// REJECTED/DATA/FD-negotiation/fallback mechanisms all fail closed.
func (self *managerBus) authenticate() error {
	peer, err := self.stream.Peer()
	if err != nil || peer.uid != 0 || peer.pid != 1 {
		return errors.New("private manager peer is not root PID1")
	}
	if err := self.write([]byte("\x00AUTH EXTERNAL 30\r\n")); err != nil {
		return err
	}
	line := make([]byte, 0, self.settings.AuthBytes)
	for len(line) < self.settings.AuthBytes {
		value := make([]byte, 1)
		if err := self.read(value); err != nil {
			return err
		}
		line = append(line, value[0])
		if len(line) >= 2 && line[len(line)-2] == '\r' && line[len(line)-1] == '\n' {
			break
		}
	}
	value := string(line)
	if !strings.HasPrefix(value, "OK ") || !strings.HasSuffix(value, "\r\n") || !validHex(strings.TrimSuffix(strings.TrimPrefix(value, "OK "), "\r\n"), 32) {
		return errors.New("manager EXTERNAL authentication did not complete exactly")
	}
	return self.write([]byte("BEGIN\r\n"))
}

// Only exact returns are consumed; unsolicited signals/errors cannot advance
// a serial or be skipped in an unbounded search for a desired response.
func (self *managerBus) call(object, iface, member, signature string, body []byte, expected string) ([]byte, error) {
	if self.calls >= self.settings.MaximumCalls {
		return nil, errors.New("manager proof exceeded its closed call count")
	}
	self.calls++
	serial := uint32(self.calls)
	request, err := managerCall(serial, object, iface, member, signature, body, self.settings)
	if err != nil || self.write(request) != nil {
		return nil, errors.New("manager proof request was not sent completely")
	}
	prefix := make([]byte, 16)
	if err := self.read(prefix); err != nil {
		return nil, err
	}
	size, _, err := managerFrameSize(prefix, self.settings)
	if err != nil {
		return nil, err
	}
	senderSerial := binary.LittleEndian.Uint32(prefix[8:12])
	if self.replySerials == nil {
		self.replySerials = make(map[uint32]bool)
	}
	if self.replySerials[senderSerial] {
		return nil, errors.New("manager reused a peer message serial within this proof")
	}
	self.replySerials[senderSerial] = true
	frame := make([]byte, size)
	copy(frame, prefix)
	if err := self.read(frame[16:]); err != nil {
		return nil, err
	}
	return managerReply(frame, serial, expected, self.settings)
}

// Object identity is never taken from an arbitrary peer-returned path.
func (self *managerBus) unit() error {
	body, err := self.call(managerObjectPath, managerInterface, "GetUnit", "s", managerText(nil, HandlerSliceName), "o")
	if err != nil {
		return err
	}
	decoder := &managerDecoder{content: body, limit: self.settings}
	object, err := decoder.text()
	if err != nil || decoder.offset != len(body) || object != managerUnitObject {
		return errors.New("manager GetUnit did not return the exact escaped handler object")
	}
	return nil
}
