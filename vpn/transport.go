package vpn

//
// TLS transports for OpenVPN over TCP and over UDP.
//

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"time"
)

var (
	// ErrBadConnNetwork indicates that the conn's network is neither TCP nor UDP.
	ErrBadConnNetwork = errors.New("bad conn.Network value")

	// ErrPacketTooShort indicates that a packet is too short.
	ErrPacketTooShort = errors.New("packet too short")
)

// TLSModeTransport is a transport for OpenVPN in TLS mode.
//
// See https://openvpn.net/community-resources/openvpn-protocol/ for documentation
// on the protocol used by OpenVPN on the wire.
type TLSModeTransport interface {
	// ReadPacket reads an OpenVPN packet from the wire.
	ReadPacket() (p *packet, err error)

	// WritePacket writes an OpenVPN packet to the wire.
	WritePacket(opcodeKeyID uint8, data []byte) error

	// SetDeadline sets the underlying conn's deadline.
	SetDeadline(deadline time.Time) error

	// SetReadDeadline sets the underlying conn's read deadline.
	SetReadDeadline(deadline time.Time) error

	// SetWriteDeadline sets the underlying conn's write deadline.
	SetWriteDeadline(deadline time.Time) error

	// Close closes the underlying conn.
	Close() error

	// LocalAddr returns the underlying conn's local addr.
	LocalAddr() net.Addr

	// RemoteAddr returns the underlying conn's remote addr.
	RemoteAddr() net.Addr
}

// NewTLSModeTransport creates a new TLSModeTransport using the given net.Conn.
// TODO refactor --------------------------------------------
// we currently need the session because:
// 1. we get a hold on the localPacketID during write.
// 2. we need to send (queue?) acks during reads.
// TODO I think there's no need to split udp/tcp.
func NewTLSModeTransport(conn net.Conn, s *session) (TLSModeTransport, error) {
	switch network := conn.LocalAddr().Network(); network {
	case "tcp", "tcp4", "tcp6":
		return &tlsModeTransportTCP{Conn: conn, session: s}, nil
	case "udp", "udp4", "udp6":
		return &tlsModeTransportUDP{Conn: conn, session: s}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrBadConnNetwork, network)
	}
}

// tlsModeTransportUDP implements TLSModeTransport for UDP.
type tlsModeTransportUDP struct {
	net.Conn
	session *session
}

func (txp *tlsModeTransportUDP) ReadPacket() (*packet, error) {
	const enough = 1 << 17
	buff := make([]byte, enough)
	count, err := txp.Conn.Read(buff)
	if err != nil {
		return nil, err
	}
	buff = buff[:count]

	p := newPacketFromBytes(buff)
	return p, nil
}

func (txp *tlsModeTransportUDP) WritePacket(opcodeKeyID uint8, data []byte) error {
	log.Println("write packet udp")

	var out bytes.Buffer
	out.WriteByte(opcodeKeyID)
	out.Write(data)
	_, err := txp.Conn.Write(out.Bytes())
	return err
}

// tlsModeTransportTCP implements TLSModeTransport for TCP.
type tlsModeTransportTCP struct {
	net.Conn
	session *session
}

// refactor with a util function
func (txp *tlsModeTransportTCP) ReadPacket() (*packet, error) {
	buf, err := readPacketFromTCP(txp.Conn)
	if err != nil {
		return nil, err
	}

	p := newPacketFromBytes(buf)
	if p.isACK() {
		log.Println("ACK, skip...")
		return &packet{}, nil
	}
	return p, nil
}

func (txp *tlsModeTransportTCP) WritePacket(opcodeKeyID uint8, data []byte) error {
	p := newPacketFromPayload(opcodeKeyID, 0, data)
	p.id = txp.session.LocalPacketID()
	p.localSessionID = txp.session.LocalSessionID
	payload := p.Bytes()

	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(payload)))
	var out bytes.Buffer
	out.Write(length)
	out.Write(payload)

	log.Println("tls write:", len(out.Bytes()))
	fmt.Println(hex.Dump(out.Bytes()))

	_, err := txp.Conn.Write(out.Bytes())
	return err
}

// TLSConn implements net.Conn, and is passed to the tls.Client to perform a
// TLS Handshake over OpenVPN control packets.
type TLSConn struct {
	tlsTr   TLSModeTransport
	conn    net.Conn
	session *session
	// we need a buffer because the tls records request less than the
	// payload we receive.
	bufReader *bytes.Buffer
}

// TODO refactor this algorithm into the control channel / muxer that is in
// control of the reads on the net.Conn
func (t *TLSConn) Read(b []byte) (int, error) {
	// XXX this is basically the reliability layer. retry until next packet is received.
	pa := &packet{}
	for {
		if len(ackQueue) != 0 {
			log.Printf("queued: %d packets", len(ackQueue))
			for p := range ackQueue {
				if p != nil && isNextPacket(p) {
					pa = p
					break
				} else {
					if p != nil {
						ackQueue <- p
						goto read
					}
				}
			}
		}
	read:
		if p, _ := t.tlsTr.ReadPacket(); p != nil && isNextPacket(p) {
			pa = p
			break
		} else {
			if p != nil {
				ackQueue <- p
			}
		}

	}

	sendACK(t.conn, t.session, pa.id)
	t.bufReader.Write(pa.payload)
	return t.bufReader.Read(b)
}

func (t *TLSConn) Write(b []byte) (int, error) {
	err := t.tlsTr.WritePacket(uint8(pControlV1), b)
	if err != nil {
		log.Println("ERROR write:", err.Error())
	}
	return len(b), err
}

func (t *TLSConn) Close() error {
	return t.conn.Close()
}

func (t *TLSConn) LocalAddr() net.Addr {
	return t.conn.LocalAddr()
}

func (t *TLSConn) RemoteAddr() net.Addr {
	return t.conn.RemoteAddr()
}

func (t *TLSConn) SetDeadline(tt time.Time) error {
	return t.conn.SetDeadline(tt)
}

func (t *TLSConn) SetReadDeadline(tt time.Time) error {
	return t.conn.SetReadDeadline(tt)
}

func (t *TLSConn) SetWriteDeadline(tt time.Time) error {
	return t.conn.SetWriteDeadline(tt)
}

func NewTLSConn(conn net.Conn, s *session) (*TLSConn, error) {
	tlsTr, err := NewTLSModeTransport(conn, s)
	bufReader := bytes.NewBuffer(nil)
	tlsConn := &TLSConn{tlsTr, conn, s, bufReader}
	return tlsConn, err

}
