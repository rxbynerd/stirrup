package egressproxy

import (
	"encoding/binary"
	"errors"
	"io"
	"strings"
)

// errSNINotPresent is returned when a ClientHello parses successfully but
// carries no SNI extension. The caller may treat this as a tampering signal
// (every modern client sends SNI for HTTPS) and drop the connection.
var errSNINotPresent = errors.New("egressproxy: ClientHello carries no SNI extension")

// peekTLSClientHello reads the very first TLS record from r — which for a
// well-formed TLS handshake is always a ClientHello — and returns the bytes
// of the record (header + body) along with a parsed SNI hostname. The
// returned bytes must be replayed to the upstream connection unchanged so
// the upstream sees the original handshake.
//
// The caller is responsible for setting a read deadline on r before invoking
// this function; we do no blocking here other than what r imposes.
func peekTLSClientHello(r io.Reader) (raw []byte, sni string, err error) {
	// TLS record header: type(1) | version(2) | length(2)
	header := make([]byte, 5)
	if _, err := readFull(r, header); err != nil {
		return nil, "", err
	}
	if header[0] != 0x16 { // 22 = handshake
		return nil, "", errors.New("egressproxy: first TLS record is not a handshake")
	}
	length := int(binary.BigEndian.Uint16(header[3:5]))
	// RFC 8446: TLS records are at most 2^14 + 256 bytes. A ClientHello
	// rarely exceeds 2 KB; cap the read to defend against a hostile client
	// that announces an enormous record to exhaust memory.
	const maxClientHello = 16384 + 256
	if length <= 0 || length > maxClientHello {
		return nil, "", errors.New("egressproxy: ClientHello length out of range")
	}

	body := make([]byte, length)
	if _, err := readFull(r, body); err != nil {
		return nil, "", err
	}

	sni, err = parseSNIFromHandshake(body)
	raw = append(header, body...)
	return raw, sni, err
}

// parseSNIFromHandshake walks a TLS handshake message body looking for the
// server_name extension. Returns errSNINotPresent if the handshake is
// well-formed but carries no SNI.
//
// Wire format reference (RFC 5246 §7.4.1.2 + RFC 6066 §3):
//
//	struct {
//	  HandshakeType msg_type; // 1 byte; 0x01 = client_hello
//	  uint24 length;
//	  ProtocolVersion client_version; // 2 bytes
//	  Random random;                  // 32 bytes
//	  SessionID session_id;           // 1-byte length + bytes
//	  CipherSuite cipher_suites<2..2^16-2>; // 2-byte length + bytes
//	  CompressionMethod compression_methods<1..2^8-1>; // 1-byte length + bytes
//	  Extension extensions<0..2^16-1>; // optional, 2-byte length + bytes
//	} ClientHello;
func parseSNIFromHandshake(body []byte) (string, error) {
	if len(body) < 4 {
		return "", errors.New("egressproxy: ClientHello body too short")
	}
	if body[0] != 0x01 {
		return "", errors.New("egressproxy: handshake message is not a ClientHello")
	}
	// Skip msg_type (1) + length (3) + client_version (2) + random (32) = 38
	const headerLen = 4 + 2 + 32
	if len(body) < headerLen {
		return "", errors.New("egressproxy: ClientHello header truncated")
	}
	off := headerLen

	// session_id
	if len(body) < off+1 {
		return "", errors.New("egressproxy: ClientHello session_id truncated")
	}
	sidLen := int(body[off])
	off += 1 + sidLen
	if len(body) < off {
		return "", errors.New("egressproxy: ClientHello session_id overrun")
	}

	// cipher_suites
	if len(body) < off+2 {
		return "", errors.New("egressproxy: ClientHello cipher_suites truncated")
	}
	csLen := int(binary.BigEndian.Uint16(body[off : off+2]))
	off += 2 + csLen
	if len(body) < off {
		return "", errors.New("egressproxy: ClientHello cipher_suites overrun")
	}

	// compression_methods
	if len(body) < off+1 {
		return "", errors.New("egressproxy: ClientHello compression_methods truncated")
	}
	cmLen := int(body[off])
	off += 1 + cmLen
	if len(body) < off {
		return "", errors.New("egressproxy: ClientHello compression_methods overrun")
	}

	// extensions are optional in TLS 1.0/1.1 but mandatory in 1.3 (and
	// every modern client emits them for 1.2 anyway).
	if len(body) < off+2 {
		return "", errSNINotPresent
	}
	extTotal := int(binary.BigEndian.Uint16(body[off : off+2]))
	off += 2
	if len(body) < off+extTotal {
		return "", errors.New("egressproxy: ClientHello extensions truncated")
	}
	end := off + extTotal

	for off+4 <= end {
		extType := binary.BigEndian.Uint16(body[off : off+2])
		extLen := int(binary.BigEndian.Uint16(body[off+2 : off+4]))
		off += 4
		if off+extLen > end {
			return "", errors.New("egressproxy: extension length overrun")
		}
		if extType == 0x0000 { // server_name extension
			name, err := parseServerNameExtension(body[off : off+extLen])
			if err != nil {
				return "", err
			}
			return name, nil
		}
		off += extLen
	}
	return "", errSNINotPresent
}

// parseServerNameExtension parses RFC 6066 §3 ServerNameList:
//
//	struct {
//	  ServerName server_name_list<1..2^16-1>;
//	} ServerNameList;
//	struct {
//	  NameType name_type; // 1 byte; 0 = host_name
//	  select (name_type) { case host_name: HostName; } name;
//	} ServerName;
//	opaque HostName<1..2^16-1>;
//
// Only NameType=0 (host_name) is defined, so we extract the first one we see.
func parseServerNameExtension(buf []byte) (string, error) {
	if len(buf) < 2 {
		return "", errors.New("egressproxy: server_name extension truncated")
	}
	listLen := int(binary.BigEndian.Uint16(buf[0:2]))
	if listLen+2 > len(buf) {
		return "", errors.New("egressproxy: server_name list overrun")
	}
	list := buf[2 : 2+listLen]
	off := 0
	for off+3 <= len(list) {
		nameType := list[off]
		nameLen := int(binary.BigEndian.Uint16(list[off+1 : off+3]))
		off += 3
		if off+nameLen > len(list) {
			return "", errors.New("egressproxy: server_name entry overrun")
		}
		if nameType == 0x00 { // host_name
			return strings.ToLower(strings.TrimSuffix(string(list[off:off+nameLen]), ".")), nil
		}
		off += nameLen
	}
	return "", errSNINotPresent
}

// readFull wraps io.ReadFull so the SNI parser can stay independent of
// changes to the proxy's I/O strategy.
func readFull(r io.Reader, buf []byte) (int, error) {
	return io.ReadFull(r, buf)
}
