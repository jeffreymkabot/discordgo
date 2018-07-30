package discordgo

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
)

const (
	voiceWSWriteWait      = 10 * time.Second
	voiceHandshakeTimeout = 10 * time.Second
)

type VoiceConnection struct {
	sync.RWMutex

	LogLevel int

	UserID   string
	GuildID  string
	endpoint string
	mute     bool
	deaf     bool

	speaking bool

	OpusSend chan []byte
	OpusRecv chan Packet

	quit chan struct{}

	wsConn        *websocket.Conn
	messageC      chan voiceWSMessage
	heartbeatAckC chan voiceHeartbeatAck

	// need to take data from these events to complete voice handshake
	// wsListener goroutine will continue setting them if the events are repeated
	hello       voiceHello
	ready       voiceReady
	sessionDesc voiceSessionDescription
	// each of these are closed once we have gotten the event at least once
	helloC       chan struct{}
	readyC       chan struct{}
	sessionDescC chan struct{}

	udpConn *net.UDPConn
}

type Packet struct {
}

func (v *VoiceConnection) open() error {
	endpoint := strings.TrimSuffix(v.endpoint, ":80")
	gateway := "wss://" + endpoint + "?v=" + voiceAPIVersion

	// Open websocket
	wsConn, _, err := websocket.DefaultDialer.Dial(gateway, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to voice gateway %s", gateway)
	}

	v.wsConn = wsConn
	v.heartbeatAckC = make(chan voiceHeartbeatAck)
	v.helloC = make(chan struct{})
	v.readyC = make(chan struct{})
	v.sessionDescC = make(chan struct{})
	go v.wsListener()

	timeout := time.After(voiceHandshakeTimeout)

	err = v.wsHandshake(timeout)
	if err != nil {
		return err
	}

	v.RLock()
	hello := v.hello
	ready := v.ready
	v.RUnlock()

	// docs specifically say to use 0.75 * heartbeat interval from HELLO
	// https://discordapp.com/developers/docs/topics/voice-connections#heartbeating
	heartbeatInterval := time.Duration(0.75*float64(hello.HeartbeatInterval)) * time.Millisecond

	// once this goroutine starts all websocket writes should go through v.messageC via v.emit
	v.messageC = make(chan voiceWSMessage)
	go v.wsEmitter(heartbeatInterval)

	// Open UDP connection
	udpConn, err := udpOpen(v.endpoint, ready.Port)
	if err != nil {
		return errors.Wrap(err, "failed to open udp connection")
	}
	v.udpConn = udpConn
	go v.udpKeepAlive(5 * time.Second)

	// Perform IP discovery
	ip, port, err := v.discoverIP()
	if err != nil {
		return errors.Wrap(err, "failed to perform IP Discovery on udp connection")
	}

	// Send Op 1 SELECT PROTOCOL
	evt := voiceClientEvent{
		Operation: 1,
		Data:      voiceSelectProtocol{}, // TODO
	}
	msg, err := json.Marshal(evt)
	if err != nil {
		return errors.Wrap(err, "failed to marshal SELECT PROTOCOL message as json")
	}
	err = v.emit(msg)
	if err != nil {
		return errors.Wrap(err, "failed to send SELECT PROTOCOL message")
	}

	// Gateway should send Op 4 SESSION DESCRIPTION after we sent SELECT PROTOCOL
	var sessionDesc voiceSessionDescription
	select {
	case <-timeout:
		return errors.New("timeout waiting for SESSION DESCRIPTION event")
	case <-v.sessionDescC:
	}

	// SESSION DESCRIPTION lets us encrypt and decrypt voice packets
	v.OpusSend = make(chan []byte, 2)
	if !v.deaf {
		v.OpusRecv = make(chan Packet)
	}

	return nil
}

func (v *VoiceConnection) Close() error {
	v.Lock()
	defer v.Unlock()
	select {
	case <-v.quit:
		return errors.New("already closed")
	default:
	}
	close(v.quit)
	return nil
}

func (v *VoiceConnection) wsListener() {
	defer v.wsConn.Close()
	for {
		// TODO read deadline for heartbeat acks? (also so this does not block forever)
		_, msg, err := v.wsConn.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				//
				return
			}
			//
		}

		err = v.onVoiceWSEvent(msg)
		if err != nil {
			//
		}
	}
}

func (v *VoiceConnection) onVoiceWSEvent(msg []byte) error {
	var evt voiceEvent
	if err := json.Unmarshal(msg, &evt); err != nil {
		return errors.Wrap(err, "failed to unmarshal event as json")
	}

	switch evt.Operation {
	case 2: // READY
		var ready voiceReady
		if err := json.Unmarshal(msg, &ready); err != nil {
			return errors.Wrap(err, "failed to unmarshal ready")
		}
		v.Lock()
		v.ready = ready
		select {
		case <-v.readyC:
		default:
			close(v.readyC)
		}
		v.Unlock()
	case 4: // SESSION DESCRIPTION
		var sessionDesc voiceSessionDescription
		if err := json.Unmarshal(msg, &sessionDesc); err != nil {
			return errors.Wrap(err, "failed to unmarshal sessionDesc")
		}
		v.Lock()
		v.sessionDesc = sessionDesc
		select {
		case <-v.sessionDescC:
		default:
			close(v.sessionDescC)
		}
		v.Unlock()
	case 5: // SPEAKING
	case 6: // HEARTBEAT ACK
		var heartbeatAck voiceHeartbeatAck
		if err := json.Unmarshal(msg, &heartbeatAck); err != nil {
			return errors.Wrap(err, "failed to unmarshal heartbeat ack")
		}
		select {
		case <-v.quit:
		case v.heartbeatAckC <- heartbeatAck:
			// could use a buffered channel with default to break out
		}
	case 8: // HELLO
		var hello voiceHello
		if err := json.Unmarshal(msg, &hello); err != nil {
			return errors.Wrap(err, "failed to unmarshal hello")
		}
		v.Lock()
		v.hello = hello
		select {
		case <-v.helloC:
		default:
			close(v.helloC)
		}
		v.Unlock()
	case 9: // RESUMED
	default:
		// unexpected event
	}

	return nil
}

type voiceWSMessage struct {
	message []byte
	errC    chan error
}

func (v *VoiceConnection) wsEmitter(heartbeatInterval time.Duration) {
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	defer v.wsConn.Close()

	var err error
	defer func() {
		if err != nil {
			// TODO log error (reason why loop stopped)
		}
	}()

	// Make sure we get a heartbeat ack before sending the next heartbeat
	// TODO relate nonces in heartbeat ack and heartbeat to match them up
	// TODO allow one or more acks to be missed?
	heartbeatAcked := true

	for {
		select {
		case <-v.quit:
			v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
			v.wsConn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		case msg := <-v.messageC:
			v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
			err = v.wsConn.WriteMessage(websocket.TextMessage, msg.message)
			// errC should be buffered
			msg.errC <- err
			close(msg.errC)
			if err != nil {
				return
			}
		// handle heartbeats in separate channel so they can "skip the line"
		case <-heartbeat.C:
			if !heartbeatAcked {
				err = errors.New("failed to receive heartbeat ack in time")
				// TODO attempt reconnect
				return
			}

			nonce := int(time.Now().Unix())
			evt := voiceClientEvent{
				Operation: 3,
				Data:      nonce,
			}
			msg, err := json.Marshal(evt)
			if err != nil {
				err = errors.Wrap(err, "failed to marshal heartbeat as json")
				// TODO can we just continue here instead of quitting?
				return
			}

			v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
			err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				err = errors.Wrap(err, "failed to send heartbeat")
				return
			}
			heartbeatAcked = false
		case <-v.heartbeatAckC:
			heartbeatAcked = true
		}
	}
}

func (v *VoiceConnection) emit(message []byte) error {
	msg := voiceWSMessage{
		message: message,
		// buffer errC so the wsEmitter goroutine doesn't block on sending back error
		errC: make(chan error, 1),
	}
	select {
	case <-v.quit:
		return errors.New("voice connection closed")
	// TODO wsEmitter can close a channel on return instead of using this timeout
	case <-time.After(voiceWSWriteWait):
		return errors.New("timeout waiting to pass event")
	case v.messageC <- msg:
		return <-msg.errC
	}
}

func (v *VoiceConnection) wsHandshake(timeout <-chan time.Time) error {
	// Send Op 1 IDENTIFY
	evt := voiceClientEvent{
		Operation: 0,
		Data:      voiceIdentify{}, // TODO
	}
	msg, err := json.Marshal(evt)
	if err != nil {
		return errors.Wrap(err, "failed to marshal IDENTIFY message as json")
	}

	v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
	err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
	if err != nil {
		return errors.Wrap(err, "failed to send IDENTIFY message")
	}

	// First two events on gateway after sending IDENITFY should be Op 8 HELLO and Op 2 READY
	select {
	case <-timeout:
		return errors.New("timeout waiting for HELLO event")
	case <-v.helloC:
	}
	select {
	case <-timeout:
		return errors.New("timeout waiting for READY event")
	case <-v.readyC:
	}

	return nil
}

func (v *VoiceConnection) Speaking(b bool) error {
	v.RLock()
	SSRC := v.ready.SSRC
	v.RUnlock()

	evt := voiceClientEvent{
		Operation: 5,
		Data: voiceSpeaking{
			SSRC:     SSRC,
			Speaking: b,
		},
	}
	evtJSON, err := json.Marshal(evt)
	if err != nil {
		return errors.Wrap(err, "failed to marshal SPEAKING message as json")
	}

	v.Lock()
	v.speaking = b
	v.Unlock()

	return v.emit(evtJSON)
}

func udpOpen(endpoint string, port int) (*net.UDPConn, error) {
	endpoint = strings.TrimSuffix(endpoint, ":80")
	host := fmt.Sprintf("%s:%d", endpoint, port)
	addr, err := net.ResolveUDPAddr("udp", host)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve udp host %v", host)
	}

	udpConn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to connect to udp address %v", addr.String())
	}
	return udpConn, nil
}

func (v *VoiceConnection) udpKeepAlive(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer v.udpConn.Close()

	var err error
	var sequence uint64

	packet := make([]byte, 8)

	for {
		select {
		case <-v.quit:
			return
		case <-ticker.C:
		}

		binary.LittleEndian.PutUint64(packet, sequence)
		sequence++

		_, err = v.udpConn.Write(packet)
		if err != nil {
			return
		}
	}
}

func (v *VoiceConnection) discoverIP() (ip string, port uint16, err error) {
	v.RLock()
	SSRC := v.ready.SSRC
	v.RUnlock()

	addr := v.udpConn.RemoteAddr().String()

	// Create a 70 byte array and put the SSRC code from the Opcode 2 READY event
	// into it.  Then send that over the UDP connection to Discord.
	sb := make([]byte, 70)
	binary.BigEndian.PutUint32(sb, SSRC)
	_, err = v.udpConn.Write(sb)
	if err != nil {
		err = errors.Wrapf(err, "udp write error to %s", addr)
		return
	}

	// Create a 70 byte array and listen for the initial handshake response
	// from Discord.  Once we get it parse the IP and PORT information out
	// of the response.  This should be our public IP and PORT as Discord
	// saw us.
	rb := make([]byte, 70)
	rlen, _, err := v.udpConn.ReadFromUDP(rb)
	if err != nil {
		err = errors.Wrapf(err, "udp read error from %s", addr)
		return
	}

	if rlen < 70 {
		err = errors.New("received udp packet too small")
		return
	}

	// Loop over position 4 through 20 to grab the IP address
	// Should never be beyond position 20.
	for i := 4; i < 20; i++ {
		if rb[i] == 0 {
			break
		}
		ip += string(rb[i])
	}
	// Grab port from position 68 and 69
	port = binary.LittleEndian.Uint16(rb[68:70])
	return
}
