package vpn

//
// OpenVPN control channel
//

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
)

var (
	errBadReset = errors.New("bad reset packet")
)

/* TODO: refactor -------------------------------------------------------------------------------- */
// move global state into the session

var (
	lastAck  uint32
	ackmu    sync.Mutex
	ackQueue = make(chan *packet, 100)
)

// this needs to be moved to muxer state
func isNextPacket(p *packet) bool {
	ackmu.Lock()
	defer ackmu.Unlock()
	if p == nil {
		return false
	}
	return p.id-lastAck == 1
}

var (
	serverPushReply = []byte("PUSH_REPLY")
	serverBadAuth   = []byte("AUTH_FAILED")
)

type session struct {
	RemoteSessionID sessionID
	LocalSessionID  sessionID
	keys            []*dataChannelKey
	keyID           int
	localPacketID   uint32

	mu sync.Mutex
}

/* --- refactor hack end -------------------------------------------------------------------------- */

// newSession initializes a session ready to be used.
// 1. a first session key is initialized
// 2. the LocalSessionID for the first session key id is initialized with
//    random bytes
func newSession() (*session, error) {
	key0 := &dataChannelKey{}
	session := &session{keys: []*dataChannelKey{key0}}

	randomBytes, err := genRandomBytes(8)
	if err != nil {
		return session, err
	}

	// in go 1.17, one could do:
	// localSession := (*sessionID)(lsid)
	var localSession sessionID
	copy(localSession[:], randomBytes[:8])
	session.LocalSessionID = localSession

	log.Printf("Local session ID: %x\n", localSession.Bytes())

	localKey, err := newKeySource()
	if err != nil {
		return session, err
	}

	k, err := session.ActiveKey()
	if err != nil {
		return session, err
	}
	k.local = localKey
	return session, nil
}

// ActiveKey returns the dataChannelKey that is actively being used.
func (s *session) ActiveKey() (*dataChannelKey, error) {
	if len(s.keys) < s.keyID {
		return nil, fmt.Errorf("%w: %s", errDataChannelKey, "no such key id")
	}
	dck := s.keys[s.keyID]
	return dck, nil
}

// localPacketID returns an unique Packet ID. It increments the counter.
// TODO should warn when we're approaching the key end of life.
func (s *session) LocalPacketID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	pid := s.localPacketID
	s.localPacketID++
	return pid
}

/* TODO very little state is left here.
   we can just keep options in the muxer and pass it to the InitTLS method, in this way
   we don't keep any state in the controlHandler implementer.
*/

// control implements the controlHandler interface.
type control struct {
	options *Options
}

func (c *control) Options() *Options {
	return c.options
}

func newControl(opt *Options) *control {
	ctrl := &control{
		options: opt,
	}
	return ctrl
}

//
// write funcs
//

/*
// where was this used?
func sendControlV1(conn net.Conn, s *session, data []byte) (n int, err error) {
	return sendControlPacket(conn, s, pControlV1, 0, data)
}
*/

func (c *control) SendHardReset(conn net.Conn, s *session) {
	sendControlPacket(conn, s, pControlHardResetClientV2, 0, []byte(""))
}

func sendControlPacket(conn net.Conn, s *session, opcode int, ack int, payload []byte) (n int, err error) {
	p := newPacketFromPayload(uint8(opcode), 0, payload)
	p.localSessionID = s.LocalSessionID

	log.Println("session id:", p.localSessionID)

	p.id = s.LocalPacketID()
	out := p.Bytes()

	out = maybeAddSizeFrame(conn, out)
	log.Printf("control write: (%d bytes)\n", len(out))
	fmt.Println(hex.Dump(out))
	return conn.Write(out)
}

func sendACK(conn net.Conn, s *session, pid uint32) error {
	panicIfFalse(len(s.RemoteSessionID) != 0, "tried to ack with null remote")

	ackmu.Lock()
	defer ackmu.Unlock()

	p := newACKPacket(pid, s)
	payload := p.Bytes()
	payload = maybeAddSizeFrame(conn, payload)

	_, err := conn.Write(payload)
	fmt.Println("write ack:", pid)
	fmt.Println(hex.Dump(payload))

	// TODO update lastAck on session -------------------------------
	lastAck = pid
	// --------------------------------------------------------------
	return err
}

//
// read functions
//

func parseHardReset(b []byte) (sessionID, error) {
	p, err := newServerHardReset(b)
	if err != nil {
		return sessionID{}, err
	}
	return parseServerHardResetPacket(p)
}

// sendControlMessage sends a message over the control channel packet
// (this is not a P_CONTROL, but a message over the TLS encrypted channel).
func encodeControlMessage(s *session, opt *Options) ([]byte, error) {
	key, err := s.ActiveKey()
	if err != nil {
		return []byte{}, err
	}
	return encodeClientControlMessageAsBytes(key.local, opt)
}

func isControlMessage(b []byte) bool {
	return bytes.Equal(b[:4], controlMessageHeader)
}

// readControlMessage reads a control message with authentication result data.
// it returns the remote key, remote options and an error if we cannot parse
// the data.

func readControlMessage(d []byte) (*keySource, string, error) {
	cm := newServerControlMessageFromBytes(d)
	return parseServerControlMessage(cm)
}

func maybeAddSizeFrame(conn net.Conn, payload []byte) []byte {
	switch conn.LocalAddr().Network() {
	case protoTCP.String():
		lenght := make([]byte, 2)
		binary.BigEndian.PutUint16(lenght, uint16(len(payload)))
		return append(lenght, payload...)
	default:
		// nothing to do for udp
		return payload
	}
}

func isBadAuthReply(b []byte) bool {
	return bytes.Equal(b[:len(serverBadAuth)], serverBadAuth)
}

func isPushReply(b []byte) bool {
	return bytes.Equal(b[:len(serverPushReply)], serverPushReply)
}
