package discordgo

import (
	"encoding/json"
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
	udpKeepAliveInterval  = 5 * time.Second
)

type VoiceConnection struct {
	sync.RWMutex

	LogLevel int

	// Fields in next block should all be set set by ChannelVoiceJoin before calling v.open()

	UserID    string
	GuildID   string
	ChannelID string
	mute      bool
	deaf      bool
	endpoint  string
	token     string
	sessionID string
	session   *Session

	ShouldReconnectOnError bool

	OpusSend chan []byte
	OpusRecv chan *Packet

	Ready bool

	speaking bool

	quit      chan struct{}
	waitGroup sync.WaitGroup

	wsConn        *websocket.Conn
	messageC      chan voiceWSMessage
	heartbeatAckC chan voiceHeartbeatAck

	// take data from these events to complete voice handshake
	// wsListener goroutine will continue setting them if the events are repeated
	hello       voiceHello
	ready       voiceReady
	sessionDesc voiceSessionDescription
	// each of these are closed the first time we get the corresponding event
	receivedHello       chan struct{}
	receivedReady       chan struct{}
	receivedSessionDesc chan struct{}

	udpConn *net.UDPConn
}

func (v *VoiceConnection) open() error {
	endpoint := strings.TrimSuffix(v.endpoint, ":80")
	gateway := "wss://" + endpoint + "?v=" + voiceAPIVersion

	// Open websocket
	wsConn, _, err := websocket.DefaultDialer.Dial(gateway, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to connect to voice gateway %s", gateway)
	}

	v.LogLevel = LogDebug

	v.wsConn = wsConn
	v.heartbeatAckC = make(chan voiceHeartbeatAck)
	v.receivedHello = make(chan struct{})
	v.receivedReady = make(chan struct{})
	v.receivedSessionDesc = make(chan struct{})
	v.quit = make(chan struct{})
	v.waitGroup = sync.WaitGroup{} // reset the waitgroup just to be safe
	v.waitGroup.Add(1)
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

	v.log(LogDebug, "heartbeat interval %v", hello.HeartbeatInterval)
	// docs specifically say to use 0.75 * heartbeat interval from HELLO
	// https://discordapp.com/developers/docs/topics/voice-connections#heartbeating
	heartbeatInterval := time.Duration(0.75*float64(hello.HeartbeatInterval)) * time.Millisecond

	// once this goroutine starts all websocket writes should go through v.messageC via v.emit
	v.messageC = make(chan voiceWSMessage)
	v.waitGroup.Add(1)
	go v.wsEmitter(heartbeatInterval)

	// Open UDP connection
	udpConn, err := udpOpen(v.endpoint, ready.Port)
	if err != nil {
		return errors.Wrap(err, "failed to open udp connection")
	}
	v.udpConn = udpConn
	v.waitGroup.Add(1)
	go v.udpKeepAlive(udpKeepAliveInterval)

	// Perform IP discovery
	ip, port, err := ipDiscovery(udpConn, ready.SSRC)
	if err != nil {
		return errors.Wrap(err, "failed to perform IP Discovery on udp connection")
	}

	// Send Op 1 SELECT PROTOCOL
	evt := voiceClientEvent{
		Operation: 1,
		Data: voiceSelectProtocol{
			Protocol: "udp",
			Data: voiceUDPData{
				Mode:    "xsalsa20_poly1305",
				Address: ip,
				Port:    port,
			},
		},
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
	select {
	case <-timeout:
		return errors.New("timeout waiting for SESSION DESCRIPTION event")
	case <-v.receivedSessionDesc:
	}

	// SESSION DESCRIPTION lets us encrypt and decrypt voice packets
	v.OpusSend = make(chan []byte, 2)
	v.waitGroup.Add(1)
	go v.opusSender(48000, 960)

	if !v.deaf {
		v.OpusRecv = make(chan *Packet)
		v.waitGroup.Add(1)
		go v.opusReceiver()
	}

	return nil
}

func (v *VoiceConnection) Close() error {
	v.Lock()
	v.Ready = false
	defer v.Unlock()
	select {
	case <-v.quit:
		return errors.New("already closed")
	default:
	}
	close(v.quit)
	// wait for
	// - wsListener (returns on Read error)
	// - wsEmitter (returns on <-v.quit, closes ws conn which will cause read error in wsListener)
	// - udpKeepAlive (returns on <-v.quit, closes UDP conn which will cause read/write errors)
	// - opusSender (returns on <-v.quit or UDP write error)
	// - opusReceiver (returns on Read error or <-v.quit)
	v.waitGroup.Wait()
	return nil
}

func (v *VoiceConnection) wsListener() {
	defer v.waitGroup.Done()
	defer v.wsConn.Close()
	for {
		// TODO read deadline for heartbeat acks? (also so this does not block forever)
		_, msg, err := v.wsConn.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				// TODO
				_ = closeErr
			}
			return
		}

		err = v.onVoiceEvent(msg)
		if err != nil {
			v.log(LogWarning, "error in processing voice event %s", err)
		}
	}
}

func (v *VoiceConnection) onVoiceEvent(msg []byte) error {
	var evt voiceEvent
	if err := json.Unmarshal(msg, &evt); err != nil {
		return errors.Wrap(err, "failed to unmarshal event as json")
	}

	v.log(LogDebug, "receieved %s", msg)

	switch evt.Operation {
	case 2: // READY
		var ready voiceReady
		if err := json.Unmarshal(evt.RawData, &ready); err != nil {
			return errors.Wrap(err, "failed to unmarshal ready")
		}
		v.Lock()
		v.ready = ready
		select {
		case <-v.receivedReady:
		default:
			close(v.receivedReady)
		}
		v.Unlock()
	case 4: // SESSION DESCRIPTION
		var sessionDesc voiceSessionDescription
		if err := json.Unmarshal(evt.RawData, &sessionDesc); err != nil {
			return errors.Wrap(err, "failed to unmarshal sessionDesc")
		}
		v.Lock()
		v.sessionDesc = sessionDesc
		select {
		case <-v.receivedSessionDesc:
		default:
			close(v.receivedSessionDesc)
		}
		v.Unlock()
	case 5: // SPEAKING
	case 6: // HEARTBEAT ACK
		var heartbeatAck voiceHeartbeatAck
		if err := json.Unmarshal(evt.RawData, &heartbeatAck); err != nil {
			return errors.Wrap(err, "failed to unmarshal heartbeat ack")
		}
		select {
		case <-v.quit:
		case v.heartbeatAckC <- heartbeatAck:
			// could use a buffered channel with default to break out
		}
	case 8: // HELLO
		var hello voiceHello
		if err := json.Unmarshal(evt.RawData, &hello); err != nil {
			return errors.Wrap(err, "failed to unmarshal hello")
		}
		v.Lock()
		v.hello = hello
		select {
		case <-v.receivedHello:
		default:
			close(v.receivedHello)
		}
		v.Unlock()
	case 9: // RESUMED
	default:
		v.log(LogInformational, "unknown voice operation %d, %v", evt.Operation, string(evt.RawData))
	}

	return nil
}

type voiceWSMessage struct {
	message []byte
	errC    chan error
}

func (v *VoiceConnection) wsEmitter(heartbeatInterval time.Duration) {
	defer v.waitGroup.Done()
	defer v.wsConn.Close()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	var err error
	defer func() {
		if err != nil {
			// TODO log error (reason why loop stopped)
			v.log(LogError, "error in wsEmitter %s", err)
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
				// TODO attempt reconnect ? teardown ?
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
		Data: voiceIdentify{
			UserID:    v.UserID,
			ServerID:  v.GuildID,
			SessionID: v.sessionID,
			Token:     v.token,
		},
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
	case <-v.receivedHello:
	}
	select {
	case <-timeout:
		return errors.New("timeout waiting for READY event")
	case <-v.receivedReady:
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

func (v *VoiceConnection) reconnect() error {
	return nil
}
