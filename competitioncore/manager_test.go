// Manager proof tests use scripted private peer sessions and private files.
// They never dial the real systemd bus, execute a program or move a process.
package competitioncore

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This test encoder is independent of the production wire append helpers.
type testManagerWire struct{ data []byte }

func (self *testManagerWire) align(size int) {
	for len(self.data)%size != 0 {
		self.data = append(self.data, 0)
	}
}
func (self *testManagerWire) u32(value uint32) {
	self.align(4)
	var data [4]byte
	binary.LittleEndian.PutUint32(data[:], value)
	self.data = append(self.data, data[:]...)
}
func (self *testManagerWire) text(value string) {
	self.u32(uint32(len(value)))
	self.data = append(self.data, []byte(value)...)
	self.data = append(self.data, 0)
}
func (self *testManagerWire) signature(value string) {
	self.data = append(self.data, byte(len(value)))
	self.data = append(self.data, []byte(value)...)
	self.data = append(self.data, 0)
}
func (self *testManagerWire) field(code byte, kind, text string, number uint32) {
	self.align(8)
	self.data = append(self.data, code)
	self.signature(kind)
	if kind == "u" {
		self.u32(number)
	} else if kind == "g" {
		self.signature(text)
	} else {
		self.text(text)
	}
}

// Each reply has its own nonzero sender serial and the exact call reply serial.
func testManagerFrame(serial uint32, signature string, body, extraFields []byte) []byte {
	fields := &testManagerWire{}
	fields.field(5, "u", "", serial)
	fields.field(7, "s", managerName, 0)
	fields.field(8, "g", signature, 0)
	if len(extraFields) != 0 {
		fields.align(8)
		fields.data = append(fields.data, extraFields...)
	}
	frame := &testManagerWire{data: []byte{'l', 2, 1, 1}}
	frame.u32(uint32(len(body)))
	frame.u32(serial + 100)
	frame.u32(uint32(len(fields.data)))
	frame.data = append(frame.data, fields.data...)
	frame.align(8)
	frame.data = append(frame.data, body...)
	return frame.data
}

// The fixed native bus types are not systemctl's rendered strings.
func testManagerVariant(value managerValue) []byte {
	result := &testManagerWire{}
	result.signature(value.signature)
	switch value.signature {
	case "s":
		result.text(value.text)
	case "t":
		result.align(8)
		var data [8]byte
		binary.LittleEndian.PutUint64(data[:], value.number)
		result.data = append(result.data, data[:]...)
	case "ay":
		result.u32(uint32(len(value.bytes)))
		result.data = append(result.data, value.bytes...)
	case "as":
		array := &testManagerWire{}
		for _, part := range value.strings {
			array.text(part)
		}
		result.u32(uint32(len(array.data)))
		result.data = append(result.data, array.data...)
	}
	return result.data
}

// The little-endian mask retains harmless trailing zero allocation bytes.
func testManagerValues(policy Policy, settings Settings) map[string]managerValue {
	mask := make([]byte, 8)
	mask[2] = (1 << 4) | (1 << 6)
	return map[string]managerValue{
		"Id": {signature: "s", text: HandlerSliceName}, "LoadState": {signature: "s", text: "loaded"}, "ActiveState": {signature: "s", text: "active"},
		"FragmentPath": {signature: "s", text: policy.HandlerUnit.Path}, "DropInPaths": {signature: "as"}, "ControlGroup": {signature: "s", text: HandlerCgroup},
		"MemoryMax": {signature: "t", number: settings.HandlerMemoryBytes}, "MemorySwapMax": {signature: "t", number: 0}, "TasksMax": {signature: "t", number: settings.HandlerTasksMaximum},
		"CPUQuotaPerSecUSec": {signature: "t", number: 2000000}, "CPUQuotaPeriodUSec": {signature: "t", number: 100000}, "AllowedCPUs": {signature: "ay", bytes: mask},
	}
}

// A complete finite scripted conversation contains no real endpoint.
func testManagerTranscript(values map[string]managerValue) []byte {
	result := []byte("OK aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r\n")
	path := &testManagerWire{}
	path.text(managerUnitObject)
	result = append(result, testManagerFrame(1, "o", path.data, nil)...)
	for index, property := range managerProperties() {
		result = append(result, testManagerFrame(uint32(index+2), "v", testManagerVariant(values[property.name]), nil)...)
	}
	return append(result, testManagerFrame(14, "o", path.data, nil)...)
}

// The fake peer is serial but Close may race cancellation, just like UnixConn.
type testManagerStream struct {
	stateLock                           sync.Mutex
	data, written                       []byte
	offset, reads, writes, closes       int
	maximumRead, maximumWrite           int
	peer, finalPeer                     managerPeer
	peerCalls                           int
	readFault, writeFault, pendingFault error
	control                             []byte
	controlAt                           int
	flags                               int
	deadline                            time.Time
	onWrite                             func()
}

func (self *testManagerStream) ReadMsg(content, control []byte) (int, int, int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reads++
	if self.readFault != nil {
		return 0, 0, 0, self.readFault
	}
	if self.offset == len(self.data) {
		return 0, 0, 0, io.EOF
	}
	count := len(content)
	if count > len(self.data)-self.offset {
		count = len(self.data) - self.offset
	}
	if self.maximumRead > 0 && count > self.maximumRead {
		count = self.maximumRead
	}
	copy(content, self.data[self.offset:self.offset+count])
	self.offset += count
	oob := 0
	if len(self.control) > 0 && self.offset >= self.controlAt {
		oob = copy(control, self.control)
		self.control = nil
	}
	return count, oob, self.flags, nil
}

func (self *testManagerStream) Write(content []byte) (int, error) {
	self.stateLock.Lock()
	self.writes++
	if self.writeFault != nil {
		err := self.writeFault
		self.stateLock.Unlock()
		return 0, err
	}
	count := len(content)
	if self.maximumWrite > 0 && count > self.maximumWrite {
		count = self.maximumWrite
	}
	self.written = append(self.written, content[:count]...)
	callback := self.onWrite
	self.onWrite = nil
	self.stateLock.Unlock()
	if callback != nil {
		callback()
	}
	return count, nil
}

func (self *testManagerStream) SetDeadline(value time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.deadline = value
	return nil
}
func (self *testManagerStream) Peer() (managerPeer, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.peerCalls++
	if self.peerCalls > 1 {
		return self.finalPeer, nil
	}
	return self.peer, nil
}
func (self *testManagerStream) Pending(int) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.offset != len(self.data) {
		return errors.New("unexpected trailing fake bus bytes")
	}
	return self.pendingFault
}
func (self *testManagerStream) Close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closes++
	return nil
}

func newTestManager(t *testing.T) (Policy, Settings, *testManagerStream, func(Pin) error) {
	t.Helper()
	_, host, _ := newTestHandler(t, "/system.slice/synthetic-ordinary.service")
	settings := DefaultSettings()
	stream := &testManagerStream{peer: managerPeer{uid: 0, pid: 1}, finalPeer: managerPeer{uid: 0, pid: 1}, maximumRead: 3, maximumWrite: 5}
	stream.data = testManagerTranscript(testManagerValues(host.policy, settings))
	return host.policy, settings, stream, func(Pin) error { return nil }
}

func testVerifyManager(t *testing.T, policy Policy, settings Settings, stream *testManagerStream, pin func(Pin) error) error {
	t.Helper()
	return verifyManager(t.Context(), policy, settings, func(context.Context) (managerStream, error) { return stream, nil }, pin)
}

// Fragment checks, authentication, both peer checks and every fixed request
// must complete; short writes/reads cannot truncate the successful proof.
func TestManagerPrivateProofAuthenticatesAndReadsExactTypedClass(t *testing.T) {
	policy, settings, stream, _ := newTestManager(t)
	pins := 0
	if err := testVerifyManager(t, policy, settings, stream, func(pin Pin) error {
		pins++
		if pin != policy.HandlerUnit {
			return errors.New("wrong pin")
		}
		return nil
	}); err != nil {
		t.Fatal("finite typed manager control failed", err)
	}
	if pins != 2 || stream.peerCalls != 2 || stream.closes != 1 || stream.deadline.IsZero() {
		t.Fatal("manager proof omitted fragment/peer/deadline custody", pins, stream.peerCalls, stream.closes)
	}
	written := string(stream.written)
	if !strings.HasPrefix(written, "\x00AUTH EXTERNAL 30\r\nBEGIN\r\n") || strings.Contains(written, "NEGOTIATE_UNIX_FD") || strings.Contains(written, "GetAll") || strings.Contains(written, "Set") {
		t.Fatal("manager proof used a broader authentication/method vocabulary")
	}
	if strings.Count(written, "GetUnit") != 2 || strings.Count(written, propertyInterface) != 12 {
		t.Fatal("manager proof changed its finite exact request count")
	}
	frames := stream.written[len("\x00AUTH EXTERNAL 30\r\nBEGIN\r\n"):]
	for serial := uint32(1); serial <= 14; serial++ {
		if len(frames) < 16 || frames[0] != 'l' || frames[1] != 1 || frames[2] != 2 || frames[3] != 1 || binary.LittleEndian.Uint32(frames[8:12]) != serial {
			t.Fatal("manager request omitted exact serial/NO_AUTO_START/read-only call framing", serial)
		}
		body, fields := uint64(binary.LittleEndian.Uint32(frames[4:8])), uint64(binary.LittleEndian.Uint32(frames[12:16]))
		size := ((16 + fields + 7) &^ uint64(7)) + body
		if size > uint64(len(frames)) {
			t.Fatal("manager request was truncated")
		}
		frames = frames[int(size):]
	}
	if len(frames) != 0 {
		t.Fatal("manager proof emitted a request outside its closed inventory")
	}
}

// An exact source list prevents fake data from legitimizing a wrong interface,
// omitted limit, broad GetAll or arbitrary peer-selected property.
func TestManagerPropertyInventoryIsClosedAndTyped(t *testing.T) {
	want := []managerProperty{{iface: unitInterface, name: "Id", signature: "s"}, {iface: unitInterface, name: "LoadState", signature: "s"}, {iface: unitInterface, name: "ActiveState", signature: "s"}, {iface: unitInterface, name: "FragmentPath", signature: "s"}, {iface: unitInterface, name: "DropInPaths", signature: "as"}, {iface: sliceInterface, name: "ControlGroup", signature: "s"}, {iface: sliceInterface, name: "MemoryMax", signature: "t"}, {iface: sliceInterface, name: "MemorySwapMax", signature: "t"}, {iface: sliceInterface, name: "TasksMax", signature: "t"}, {iface: sliceInterface, name: "CPUQuotaPerSecUSec", signature: "t"}, {iface: sliceInterface, name: "CPUQuotaPeriodUSec", signature: "t"}, {iface: sliceInterface, name: "AllowedCPUs", signature: "ay"}}
	if !reflect.DeepEqual(managerProperties(), want) {
		t.Fatal("closed manager proof API drifted")
	}
}

// Reusing a peer message cookie is rejected even if a forged frame changes its
// reply-to field to the next outstanding request's otherwise correct serial.
func TestManagerSessionRejectsReusedPeerMessageSerial(t *testing.T) {
	policy, settings, stream, pin := newTestManager(t)
	for offset := len("OK aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r\n"); offset < len(stream.data); {
		prefix := stream.data[offset : offset+16]
		body, header := uint64(binary.LittleEndian.Uint32(prefix[4:8])), uint64(binary.LittleEndian.Uint32(prefix[12:16]))
		binary.LittleEndian.PutUint32(prefix[8:12], 1)
		offset += int(((16 + header + 7) &^ uint64(7)) + body)
	}
	if testVerifyManager(t, policy, settings, stream, pin) == nil {
		t.Fatal("reused manager sender serial became fresh proof authority")
	}
}

func TestManagerProofRejectsNonRootNonPid1AndChangedPeers(t *testing.T) {
	for _, fault := range []string{"uid", "pid", "changed"} {
		policy, settings, stream, pin := newTestManager(t)
		switch fault {
		case "uid":
			stream.peer.uid = 1000
		case "pid":
			stream.peer.pid = 123
		case "changed":
			stream.finalPeer.pid = 123
		}
		if err := testVerifyManager(t, policy, settings, stream, pin); err == nil {
			t.Fatal("untrusted peer became manager proof", fault)
		}
		if fault != "changed" && len(stream.written) != 0 {
			t.Fatal("client authenticated before native peer proof", fault)
		}
	}
}

func TestManagerProofRejectsAuthenticationFallbackAndOversizedLines(t *testing.T) {
	for _, line := range []string{"REJECTED EXTERNAL\r\n", "DATA\r\n", "OK invalid\r\n", "OK aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", strings.Repeat("A", 257)} {
		policy, settings, stream, pin := newTestManager(t)
		stream.data = []byte(line)
		if testVerifyManager(t, policy, settings, stream, pin) == nil {
			t.Fatal("authentication fallback/incomplete grammar passed", line)
		}
		if strings.Contains(string(stream.written), "BEGIN") {
			t.Fatal("invalid authentication entered binary protocol")
		}
	}
}

func TestManagerProofRejectsEveryWrongUnitResourceAndDropin(t *testing.T) {
	for _, fault := range []string{"Id", "LoadState", "ActiveState", "FragmentPath", "DropInPaths", "ControlGroup", "MemoryMax", "MemorySwapMax", "TasksMax", "CPUQuotaPerSecUSec", "CPUQuotaPeriodUSec", "AllowedCPUs"} {
		policy, settings, stream, pin := newTestManager(t)
		values := testManagerValues(policy, settings)
		value := values[fault]
		switch value.signature {
		case "s":
			value.text = "synthetic-wrong"
		case "t":
			value.number = math.MaxUint64
		case "as":
			value.strings = []string{"/synthetic/override.conf"}
		case "ay":
			value.bytes = []byte{1}
		}
		values[fault] = value
		stream.data = testManagerTranscript(values)
		if testVerifyManager(t, policy, settings, stream, pin) == nil {
			t.Fatal("wrong manager value passed exact class proof", fault)
		}
	}
}

func TestManagerProofRejectsWrongPropertyWireTypesAndNestedVariants(t *testing.T) {
	for _, kind := range []string{"s", "v", "a{sv}", "h"} {
		policy, settings, stream, pin := newTestManager(t)
		values := testManagerValues(policy, settings)
		values["MemoryMax"] = managerValue{signature: kind, text: "268435456"}
		stream.data = testManagerTranscript(values)
		if testVerifyManager(t, policy, settings, stream, pin) == nil {
			t.Fatal("wrong/nested property wire type passed", kind)
		}
	}
}

// Message boundaries/serials are verified independently of positive properties.
func TestManagerWireRejectsMalformedOverflowTrailingAndMismatchedFrames(t *testing.T) {
	settings := DefaultManagerSettings()
	body := &testManagerWire{}
	body.text(managerUnitObject)
	valid := testManagerFrame(1, "o", body.data, nil)
	for _, fault := range []string{"serial", "sender-serial", "type", "version", "endian", "flags", "body-overflow", "header-overflow", "truncated", "trailing", "header-type", "padding"} {
		frame := append([]byte(nil), valid...)
		switch fault {
		case "serial":
			binary.LittleEndian.PutUint32(frame[20:24], 2)
		case "sender-serial":
			binary.LittleEndian.PutUint32(frame[8:12], 0)
		case "type":
			frame[1] = 4
		case "version":
			frame[3] = 2
		case "endian":
			frame[0] = 'B'
		case "flags":
			frame[2] = 128
		case "body-overflow":
			binary.LittleEndian.PutUint32(frame[4:8], math.MaxUint32)
		case "header-overflow":
			binary.LittleEndian.PutUint32(frame[12:16], math.MaxUint32)
		case "truncated":
			frame = frame[:len(frame)-1]
		case "trailing":
			frame = append(frame, 0)
		case "header-type":
			frame[18] = 's'
		case "padding":
			padding := 16 + int(binary.LittleEndian.Uint32(frame[12:16]))
			_, start, err := managerFrameSize(frame[:16], settings)
			if err != nil || padding >= start {
				t.Fatal("padding regression fixture has no body alignment byte", err)
			}
			frame[padding] = 1
		}
		if _, err := managerReply(frame, 1, "o", settings); err == nil {
			t.Fatal("malformed reply frame passed", fault)
		}
	}
	for _, code := range []byte{5, 9, 99} {
		extra := &testManagerWire{}
		extra.field(code, "u", "", 1)
		if _, err := managerReply(testManagerFrame(1, "o", body.data, extra.data), 1, "o", settings); err == nil {
			t.Fatal("duplicate/rights/unknown header passed", code)
		}
	}
}

func TestManagerProofRejectsWrongGetUnitObjectAndTrailingBody(t *testing.T) {
	for _, trailing := range []bool{false, true} {
		policy, settings, stream, pin := newTestManager(t)
		body := &testManagerWire{}
		if trailing {
			body.text(managerUnitObject)
			body.data = append(body.data, 0)
		} else {
			body.text("/org/freedesktop/systemd1/unit/synthetic_2eslice")
		}
		stream.data = append([]byte("OK aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\r\n"), testManagerFrame(1, "o", body.data, nil)...)
		if testVerifyManager(t, policy, settings, stream, pin) == nil {
			t.Fatal("peer-selected object or trailing value passed")
		}
	}
}

func TestManagerWireArrayStringAndDepthBoundsRefuseUnknownValues(t *testing.T) {
	settings := DefaultManagerSettings()
	for _, content := range [][]byte{{1, 's', 0, 0, 255, 255, 255, 255}, {2, 'a', 'y', 0, 255, 255, 255, 255}, {1, 'v', 0, 1, 't', 0}, {1, 's', 0, 0, 1, 0, 0, 0, 'x', 'y'}} {
		decoder := &managerDecoder{content: content, limit: settings}
		if _, err := decoder.variant("s"); err == nil {
			t.Fatal("malformed/oversized/nested value passed")
		}
	}
	bytes := testManagerVariant(managerValue{signature: "ay", bytes: make([]byte, settings.ByteArrayBytes+1)})
	if _, err := (&managerDecoder{content: bytes, limit: settings}).variant("ay"); err == nil {
		t.Fatal("CPU array exceeded its bound")
	}
	stringsValue := make([]string, settings.ArrayElements+1)
	stringsData := testManagerVariant(managerValue{signature: "as", strings: stringsValue})
	if _, err := (&managerDecoder{content: stringsData, limit: settings}).variant("as"); err == nil {
		t.Fatal("string array exceeded its element bound")
	}
	settings.MaximumDepth = 1
	if _, err := (&managerDecoder{content: testManagerVariant(managerValue{signature: "t", number: 1}), limit: settings}).variant("t"); err == nil {
		t.Fatal("variant exceeded its depth bound")
	}
}

func TestManagerProofRetainsOriginalDeadlineAndRejectsTimeoutIncompleteAndTrailingData(t *testing.T) {
	for _, fault := range []string{"timeout", "write", "incomplete", "trailing", "calls", "session"} {
		policy, settings, stream, pin := newTestManager(t)
		switch fault {
		case "timeout":
			stream.readFault = os.ErrDeadlineExceeded
		case "write":
			stream.writeFault = io.ErrShortWrite
		case "incomplete":
			stream.data = stream.data[:len(stream.data)-1]
		case "trailing":
			stream.data = append(stream.data, 0)
		case "calls":
			settings.Manager.MaximumCalls = 13
		case "session":
			settings.Manager.MessageBytes = 512
			settings.Manager.HeaderBytes = 256
			settings.Manager.BodyBytes = 128
			settings.Manager.SessionBytes = 512
		}
		if testVerifyManager(t, policy, settings, stream, pin) == nil {
			t.Fatal("incomplete/expired/over-budget proof passed", fault)
		}
	}
	policy, settings, stream, pin := newTestManager(t)
	deadline := time.Now().Add(settings.Manager.Timeout / 2)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	if err := verifyManager(ctx, policy, settings, func(context.Context) (managerStream, error) { return stream, nil }, pin); err != nil || !stream.deadline.Equal(deadline) {
		t.Fatal("manager session reset or ignored its parent deadline", err)
	}
}

func TestManagerProofCanceledOwnerAndPinSwapsCannotGrantAuthority(t *testing.T) {
	policy, settings, stream, _ := newTestManager(t)
	path := filepath.Join(t.TempDir(), "handler.slice")
	original := []byte("[Slice]\nMemoryMax=256M\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	policy.HandlerUnit.Sha256 = digest(original)
	pin := func(Pin) error {
		content, err := os.ReadFile(path)
		if err != nil || digest(content) != policy.HandlerUnit.Sha256 {
			return errors.New("changed synthetic unit pin")
		}
		return nil
	}
	stream.onWrite = func() {
		if err := os.WriteFile(path, []byte("synthetic substituted fragment"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if testVerifyManager(t, policy, settings, stream, pin) == nil {
		t.Fatal("fragment path/bytes changed during proof")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dials := 0
	if verifyManager(ctx, policy, settings, func(context.Context) (managerStream, error) { dials++; return stream, nil }, pin) == nil || dials != 0 {
		t.Fatal("canceled owner performed a new manager proof")
	}
}

// Rights are duplicated from a private file solely to prove rejection closes
// received descriptors; no real bus or host descriptor is touched.
func TestManagerProofRejectsAndClosesAncillaryRightsAndTruncation(t *testing.T) {
	for _, flags := range []int{0, syscall.MSG_CTRUNC, syscall.MSG_TRUNC} {
		policy, settings, stream, pin := newTestManager(t)
		file, err := os.CreateTemp(t.TempDir(), "rights-")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		fd, err := syscall.Dup(int(file.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		stream.control = syscall.UnixRights(fd)
		stream.controlAt = 1
		stream.flags = flags
		if testVerifyManager(t, policy, settings, stream, pin) == nil {
			t.Fatal("unexpected ancillary rights/truncation passed")
		}
		var stat syscall.Stat_t
		if err := syscall.Fstat(fd, &stat); err != syscall.EBADF {
			syscall.Close(fd)
			t.Fatal("rejected ancillary fd leaked", err)
		}
	}
}

func TestManagerProofRejectsTrailingRightsAndUnpinnedUnitBeforeDial(t *testing.T) {
	policy, settings, stream, pin := newTestManager(t)
	stream.pendingFault = errors.New("synthetic trailing rights")
	if testVerifyManager(t, policy, settings, stream, pin) == nil {
		t.Fatal("trailing ancillary custody was ignored")
	}
	for _, fault := range []string{"path", "pin"} {
		policy, settings, stream, pin = newTestManager(t)
		if fault == "path" {
			policy.HandlerUnit.Path = "/synthetic/other.slice"
		} else {
			policy.HandlerUnit.Sha256 = ""
		}
		dials := 0
		if verifyManager(t.Context(), policy, settings, func(context.Context) (managerStream, error) { dials++; return stream, nil }, pin) == nil || dials != 0 {
			t.Fatal("unprotected caller-selected unit reached manager", fault)
		}
	}
}
