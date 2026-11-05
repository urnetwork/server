// A closed D-Bus v1 codec handles only the fixed manager proof exchange. It is
// not a generic D-Bus decoder, introspector, property or object-path selector.
package competitioncore

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"unicode/utf8"
)

// Wire constants come from the D-Bus specification and systemd's escaped unit
// object-path ABI. Only little-endian Linux/amd64 is supported by this package.
const (
	managerSocketPath = "/run/systemd/private"
	managerObjectPath = "/org/freedesktop/systemd1"
	managerUnitObject = "/org/freedesktop/systemd1/unit/urnetwork_2dcore_2dhandler_2eslice"
	managerUnitPath   = "/etc/systemd/system/urnetwork-core-handler.slice"
	managerName       = "org.freedesktop.systemd1"
	managerInterface  = "org.freedesktop.systemd1.Manager"
	unitInterface     = "org.freedesktop.systemd1.Unit"
	sliceInterface    = "org.freedesktop.systemd1.Slice"
	propertyInterface = "org.freedesktop.DBus.Properties"
)

// Alignment bytes are validated as zero, not skipped as unauthenticated data.
type managerDecoder struct {
	content []byte
	offset  int
	limit   ManagerSettings
}

// Only protocol alignments from the finite supported type vocabulary are used.
func (self *managerDecoder) align(size int) error {
	padding := (size - self.offset%size) % size
	if self.offset > len(self.content)-padding {
		return errors.New("incomplete manager alignment")
	}
	for _, value := range self.content[self.offset : self.offset+padding] {
		if value != 0 {
			return errors.New("nonzero manager alignment padding")
		}
	}
	self.offset += padding
	return nil
}

// Subtraction precedes arithmetic, so a peer length cannot wrap an allocation.
func (self *managerDecoder) take(size uint64) ([]byte, error) {
	if self.offset > len(self.content) || size > uint64(len(self.content)-self.offset) {
		return nil, errors.New("incomplete bounded manager value")
	}
	value := self.content[self.offset : self.offset+int(size)]
	self.offset += int(size)
	return value, nil
}

func (self *managerDecoder) uint32() (uint32, error) {
	if err := self.align(4); err != nil {
		return 0, err
	}
	value, err := self.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(value), nil
}

func (self *managerDecoder) uint64() (uint64, error) {
	if err := self.align(8); err != nil {
		return 0, err
	}
	value, err := self.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(value), nil
}

// Signatures are exact single closed values; nested variants/dictionaries and
// other type grammars never become a recursive generic parser.
func (self *managerDecoder) signature() (string, error) {
	length, err := self.take(1)
	if err != nil {
		return "", err
	}
	value, err := self.take(uint64(length[0]) + 1)
	if err != nil || value[len(value)-1] != 0 {
		return "", errors.New("incomplete manager signature")
	}
	signature := string(value[:len(value)-1])
	switch signature {
	case "s", "o", "u", "t", "g", "v", "ay", "as", "ss":
		return signature, nil
	default:
		return "", errors.New("manager signature is outside the closed scalar/array vocabulary")
	}
}

// Strings are UTF-8 without embedded NUL and have an independently bounded
// length even when the full frame would otherwise fit its maximum.
func (self *managerDecoder) text() (string, error) {
	length, err := self.uint32()
	if err != nil || uint64(length) > uint64(self.limit.StringBytes) {
		return "", errors.New("manager string exceeds its finite bound")
	}
	value, err := self.take(uint64(length) + 1)
	if err != nil || value[len(value)-1] != 0 || !utf8.Valid(value[:len(value)-1]) || strings.ContainsRune(string(value[:len(value)-1]), 0) {
		return "", errors.New("invalid manager string or terminator")
	}
	return string(value[:len(value)-1]), nil
}

// The only variants consumed are exact string, uint64, byte-array or string-
// array properties, never a peer-selected object/method/descriptor type.
type managerValue struct {
	signature string
	text      string
	number    uint64
	bytes     []byte
	strings   []string
}

func (self *managerDecoder) variant(expected string) (managerValue, error) {
	value := managerValue{}
	if self.limit.MaximumDepth < 2 {
		return value, errors.New("manager variant depth bound is unavailable")
	}
	signature, err := self.signature()
	if err != nil || signature != expected {
		return value, errors.New("manager property changed its exact wire type")
	}
	value.signature = signature
	switch signature {
	case "s":
		value.text, err = self.text()
	case "t":
		value.number, err = self.uint64()
	case "ay", "as":
		var length uint32
		length, err = self.uint32()
		if err != nil || uint64(length) > uint64(self.limit.BodyBytes) || uint64(length) > uint64(len(self.content)-self.offset) {
			return value, errors.New("manager array exceeds its finite body bound")
		}
		end := self.offset + int(length)
		if signature == "ay" {
			if int(length) > self.limit.ByteArrayBytes {
				return value, errors.New("manager CPU byte array exceeds its bound")
			}
			value.bytes = append([]byte(nil), self.content[self.offset:end]...)
			self.offset = end
		} else {
			for self.offset < end {
				if len(value.strings) >= self.limit.ArrayElements {
					return value, errors.New("manager string array exceeds its count bound")
				}
				var part string
				part, err = self.text()
				if err != nil || self.offset > end {
					return value, errors.New("manager string array has an incomplete element")
				}
				value.strings = append(value.strings, part)
			}
		}
	default:
		return value, errors.New("unsupported manager property variant")
	}
	return value, err
}

// Small encoders append only static method vocabulary and root-fixed unit name.
func managerAlign(content []byte, size int) []byte {
	return append(content, make([]byte, (size-len(content)%size)%size)...)
}

func managerUint32(content []byte, value uint32) []byte {
	content = managerAlign(content, 4)
	return binary.LittleEndian.AppendUint32(content, value)
}

func managerText(content []byte, value string) []byte {
	content = managerUint32(content, uint32(len(value)))
	return append(append(content, value...), 0)
}

func managerSignature(content []byte, value string) []byte {
	return append(append(append(content, byte(len(value))), value...), 0)
}

func managerHeader(content []byte, code byte, signature, text string, number uint32) []byte {
	content = append(managerAlign(content, 8), code)
	content = managerSignature(content, signature)
	if signature == "u" {
		return managerUint32(content, number)
	}
	if signature == "g" {
		return managerSignature(content, text)
	}
	return managerText(content, text)
}

// No public API accepts method/path/property strings. These private builders
// serve only GetUnit and the fixed proof call list below.
func managerCall(serial uint32, object, iface, member, signature string, body []byte, settings ManagerSettings) ([]byte, error) {
	if serial == 0 || len(body) > settings.BodyBytes {
		return nil, errors.New("invalid bounded manager call")
	}
	fields := []byte{}
	fields = managerHeader(fields, 1, "o", object, 0)
	fields = managerHeader(fields, 2, "s", iface, 0)
	fields = managerHeader(fields, 3, "s", member, 0)
	fields = managerHeader(fields, 6, "s", managerName, 0)
	fields = managerHeader(fields, 8, "g", signature, 0)
	if len(fields) > settings.HeaderBytes || uint64(len(fields))+uint64(len(body))+24 > uint64(settings.MessageBytes) {
		return nil, errors.New("manager call exceeds its fixed header/body bound")
	}
	// NO_AUTO_START prevents even a read-only request from activating a unit.
	frame := []byte{'l', 1, 2, 1}
	frame = binary.LittleEndian.AppendUint32(frame, uint32(len(body)))
	frame = binary.LittleEndian.AppendUint32(frame, serial)
	frame = binary.LittleEndian.AppendUint32(frame, uint32(len(fields)))
	frame = append(frame, fields...)
	return append(managerAlign(frame, 8), body...), nil
}

// The fixed prefix is validated before allocating peer-declared lengths.
func managerFrameSize(prefix []byte, settings ManagerSettings) (int, int, error) {
	if len(prefix) != 16 || prefix[0] != 'l' || prefix[1] != 2 || prefix[2]&^byte(7) != 0 || prefix[3] != 1 || binary.LittleEndian.Uint32(prefix[8:12]) == 0 {
		return 0, 0, errors.New("unexpected manager message type/version/serial/endianness")
	}
	body := uint64(binary.LittleEndian.Uint32(prefix[4:8]))
	header := uint64(binary.LittleEndian.Uint32(prefix[12:16]))
	start := (16 + header + 7) &^ uint64(7)
	if header > uint64(settings.HeaderBytes) || body > uint64(settings.BodyBytes) || start > uint64(settings.MessageBytes) || body > uint64(settings.MessageBytes)-start || start+body > math.MaxInt {
		return 0, 0, errors.New("manager frame length exceeds its finite bound")
	}
	return int(start + body), int(start), nil
}

// Replies must bind the exact outstanding serial and body signature. Unknown,
// duplicate, descriptor-bearing or wrongly typed header fields fail closed.
func managerReply(frame []byte, serial uint32, expected string, settings ManagerSettings) ([]byte, error) {
	if len(frame) < 16 {
		return nil, errors.New("incomplete manager frame")
	}
	total, start, err := managerFrameSize(frame[:16], settings)
	if err != nil || total != len(frame) {
		return nil, errors.New("incomplete or trailing manager frame")
	}
	length := int(binary.LittleEndian.Uint32(frame[12:16]))
	header := &managerDecoder{content: frame[16 : 16+length], limit: settings}
	seen := map[byte]bool{}
	replySerial, signature := uint32(0), ""
	for header.offset < len(header.content) {
		if err := header.align(8); err != nil {
			return nil, err
		}
		code, err := header.take(1)
		if err != nil || seen[code[0]] {
			return nil, errors.New("duplicate or incomplete manager header field")
		}
		seen[code[0]] = true
		kind, err := header.signature()
		if err != nil {
			return nil, err
		}
		switch code[0] {
		case 5, 9:
			if kind != "u" {
				return nil, errors.New("manager numeric header has the wrong type")
			}
			value, err := header.uint32()
			if err != nil || code[0] == 9 && value != 0 {
				return nil, errors.New("manager reply contains unexpected Unix descriptors")
			}
			if code[0] == 5 {
				replySerial = value
			}
		case 6, 7:
			if kind != "s" {
				return nil, errors.New("manager sender/destination has the wrong type")
			}
			value, err := header.text()
			if err != nil || code[0] == 7 && value != managerName {
				return nil, errors.New("manager reply has an unexpected sender")
			}
		case 8:
			if kind != "g" {
				return nil, errors.New("manager body signature has the wrong type")
			}
			signature, err = header.signature()
			if err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("manager reply contains an unsupported header field")
		}
	}
	if replySerial != serial || signature != expected {
		return nil, errors.New("manager reply mismatched its outstanding call")
	}
	for _, value := range frame[16+length : start] {
		if value != 0 {
			return nil, errors.New("manager frame has nonzero body padding")
		}
	}
	return frame[start:], nil
}
