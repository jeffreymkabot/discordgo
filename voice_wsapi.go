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

	voiceSpeakingUpdateHandlers []VoiceSpeakingUpdateHandler

	quit      chan struct{}
	waitGroup sync.WaitGroup

	wsMutex       sync.Mutex
	wsConn        *websocket.Conn
	heartbeatAckC chan voiceHeartbeatAck

	// eventMu synchronizes values that may change asynchronously on received (or emitted) events
	eventMu  sync.RWMutex
	speaking bool
	// take data from the following events to complete voice setup
	// wsListener goroutine sets these values with the latest event
	hello       voiceHello
	ready       voiceReady
	sessionDesc voiceSessionDescription
	//
	receivedEvent map[voiceOp]chan struct{}

	udpConn *net.UDPConn
}

func (v *VoiceConnection) open() (err error) {
	v.log(LogDebug, "called")

	v.Lock()
	defer func() {
		v.Unlock()
		if err != nil {
			v.log(LogError, err.Error())
			// close any opened connections or goroutines
			v.Close()
		}
	}()

	v.LogLevel = LogDebug
	v.quit = make(chan struct{})

	endpoint := strings.TrimSuffix(v.endpoint, ":80")
	gateway := "wss://" + endpoint + "?v=" + voiceAPIVersion

	wsConn, _, err := websocket.DefaultDialer.Dial(gateway, nil)
	if err != nil {
		err = errors.Wrapf(err, "failed to connect to voice gateway %s", gateway)
		return
	}

	v.wsConn = wsConn
	// new websocket connection, so reset all event channels
	v.heartbeatAckC = make(chan voiceHeartbeatAck)
	v.receivedEvent = make(map[voiceOp]chan struct{})
	v.waitGroup.Add(1)
	go v.wsListener()

	timeoutC := make(chan struct{})
	time.AfterFunc(voiceHandshakeTimeout, func() {
		close(timeoutC)
	})

	// First two events on gateway after sending Identify should be Op 8 Hello and Op 2 Ready
	err = v.identify()
	if err != nil {
		return
	}
	err = v.awaitEvent(voiceOpHello, timeoutC)
	if err != nil {
		return
	}
	err = v.awaitEvent(voiceOpReady, timeoutC)
	if err != nil {
		return
	}

	v.eventMu.RLock()
	hello := v.hello
	ready := v.ready
	v.eventMu.RUnlock()

	// docs specifically say to use 0.75 * heartbeat interval from HELLO
	// https://discordapp.com/developers/docs/topics/voice-connections#heartbeating
	heartbeatInterval := time.Duration(0.75*float64(hello.HeartbeatInterval)) * time.Millisecond
	v.waitGroup.Add(1)
	go v.heartbeat(heartbeatInterval)

	udpConn, err := udpOpen(v.endpoint, ready.Port)
	if err != nil {
		err = errors.Wrap(err, "failed to open udp connection")
		return
	}
	v.udpConn = udpConn

	ip, port, err := ipDiscovery(udpConn, ready.SSRC)
	if err != nil {
		err = errors.Wrap(err, "failed to perform IP Discovery on udp connection")
		return
	}

	v.waitGroup.Add(1)
	go v.udpKeepAlive(udpKeepAliveInterval)

	// Gateway should send Op 4 Session Description after we sent Select Protocol
	// Session Description lets us encrypt and decrypt voice packets
	err = v.selectProtocol(ip, port)
	if err != nil {
		return
	}
	err = v.awaitEvent(voiceOpSessionDescription, timeoutC)
	if err != nil {
		return
	}

	// reuse exported channels if this is a reconnect, client code could be using it
	if v.OpusSend == nil {
		v.OpusSend = make(chan []byte, 2)
	}
	v.waitGroup.Add(1)
	go v.opusSender(v.OpusSend, 48000, 960)

	if !v.deaf {
		if v.OpusRecv == nil {
			v.OpusRecv = make(chan *Packet)
		}
		v.waitGroup.Add(1)
		go v.opusReceiver(v.OpusRecv)
	}

	return nil
}

func (v *VoiceConnection) Close() error {
	v.log(LogDebug, "called")

	v.Lock()
	defer v.Unlock()
	select {
	case <-v.quit:
		return errors.New("already closed")
	default:
	}

	// wsConn and udpConn can be nil if v.open defers v.Close
	if v.wsConn != nil {
		v.log(LogInformational, "sending close message on gateway")
		v.wsMutex.Lock()
		v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
		v.wsConn.WriteMessage(websocket.CloseMessage, []byte{})
		v.wsMutex.Unlock()
		v.wsConn.Close()
	}

	if v.udpConn != nil {
		v.udpConn.Close()
	}

	// terminate goroutines
	close(v.quit)
	// wait for
	// - wsListener (returns on Read error after closing websocket above)
	// - heartbeat (returns on <-v.quit, closes ws conn which will cause read error in wsListener)
	// - udpKeepAlive (returns on <-v.quit, closes UDP conn which will cause read/write errors)
	// - opusSender (returns on <-v.quit or UDP write error)
	// - opusReceiver (returns on UDP Read error or <-v.quit)
	v.waitGroup.Wait()
	v.Ready = false
	// TODO what other data should be reset for v.open() during a reconnect
	v.speaking = false
	return nil
}

func (v *VoiceConnection) Disconnect() (err error) {
	v.log(LogDebug, "called")

	v.Close()

	data := voiceChannelJoinOp{4, voiceChannelJoinData{&v.GuildID, nil, true, true}}
	v.session.wsMutex.Lock()
	err = v.session.wsConn.WriteJSON(data)
	v.session.wsMutex.Unlock()

	v.log(LogInformational, "deleting VoiceConnection %s", v.GuildID)

	v.session.Lock()
	delete(v.session.VoiceConnections, v.GuildID)
	v.session.Unlock()

	return nil
}

func (v *VoiceConnection) wsListener() {
	v.log(LogDebug, "called")

	defer v.waitGroup.Done()
	defer v.wsConn.Close()
	for {
		// TODO read deadline for heartbeat acks? (also so this does not block forever)
		_, msg, err := v.wsConn.ReadMessage()
		if err != nil {
			select {
			// v.Close() was called so error was expected
			case <-v.quit:
				return
			default:
			}
			if closeErr, ok := err.(*websocket.CloseError); ok {
				v.log(LogError, "voice gateway closed unexpecedly, %s", closeErr)
				return
			}
			v.log(LogError, "failed to read message from voice gateway %v", err)
			return
		}

		err = v.onVoiceEvent(msg)
		if err != nil {
			v.log(LogWarning, "error in processing voice event %s", err)
		}
	}
}

// onVoiceEvent processes voice websocket events and broadcasts that the event type was received
// to any goroutines waiting for that event type
func (v *VoiceConnection) onVoiceEvent(msg []byte) error {
	v.log(LogDebug, "received: %s", msg)

	var evt voiceEvent
	if err := json.Unmarshal(msg, &evt); err != nil {
		return errors.Wrap(err, "failed to unmarshal event as json")
	}

	switch evt.Operation {
	case voiceOpReady:
		var ready voiceReady
		if err := json.Unmarshal(evt.RawData, &ready); err != nil {
			return errors.Wrap(err, "failed to unmarshal ready")
		}

		v.eventMu.Lock()
		v.ready = ready
		v.eventMu.Unlock()
	case voiceOpSessionDescription: // SESSION DESCRIPTION
		var sessionDesc voiceSessionDescription
		if err := json.Unmarshal(evt.RawData, &sessionDesc); err != nil {
			return errors.Wrap(err, "failed to unmarshal sessionDesc")
		}

		v.eventMu.Lock()
		v.sessionDesc = sessionDesc
		v.eventMu.Unlock()
	case voiceOpSpeaking:
		var speakingUpdate VoiceSpeakingUpdate
		if err := json.Unmarshal(evt.RawData, &speakingUpdate); err != nil {
			return errors.Wrap(err, "failed to unmarshal speakingUpdate")
		}

		// We can get a speaking update about ourself if another user changes our channel
		// If for whatever reason our speaking status gets set to false,
		// opusSender needs to send a new Speaking frame for users to continue to receive our audio
		if speakingUpdate.UserID == v.session.State.User.ID {
			v.eventMu.Lock()
			v.speaking = speakingUpdate.Speaking
			v.eventMu.Unlock()
		}

		for _, h := range v.voiceSpeakingUpdateHandlers {
			h(v, &speakingUpdate)
		}
	case voiceOpHeartbeatAck:
		var heartbeatAck voiceHeartbeatAck
		if err := json.Unmarshal(evt.RawData, &heartbeatAck); err != nil {
			return errors.Wrap(err, "failed to unmarshal heartbeat ack")
		}

		select {
		case <-v.quit:
		case v.heartbeatAckC <- heartbeatAck:
			// TODO could use a buffered channel with default to break out
		}
	case voiceOpHello:
		var hello voiceHello
		if err := json.Unmarshal(evt.RawData, &hello); err != nil {
			return errors.Wrap(err, "failed to unmarshal hello")
		}

		v.eventMu.Lock()
		v.hello = hello
		v.eventMu.Unlock()
	case voiceOpResumed:
	default:
		v.log(LogInformational, "unknown voice operation %d, %v", evt.Operation, string(evt.RawData))
	}

	// Broadcast to any goroutines waiting for this event type (e.g. via awaitEvent).
	// Replace the channel for the next time the event type is received.
	v.eventMu.Lock()
	c, ok := v.receivedEvent[evt.Operation]
	if ok {
		close(c)
		c = make(chan struct{})
	}
	v.eventMu.Unlock()

	return nil
}

func (v *VoiceConnection) heartbeat(interval time.Duration) {
	v.log(LogDebug, "called")

	defer v.waitGroup.Done()
	defer v.wsConn.Close()
	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()

	// Make sure we get a heartbeat ack before sending the next heartbeat
	// TODO relate nonces in heartbeat ack and heartbeat to match them up
	// TODO allow one or more acks to be missed?
	heartbeatAcked := true

	var err error
	defer func() {
		if err != nil {
			v.log(LogError, "error in wsEmitter %s", err)
			// TODO failure here should probably teardown everything
		}
	}()

	for {
		select {
		case <-v.quit:
			return
		case <-v.heartbeatAckC:
			heartbeatAcked = true
		case <-heartbeat.C:
			if !heartbeatAcked {
				err = errors.New("failed to receive Heartbeat ACK in time")
				// TODO attempt reconnect ? teardown ?
				return
			}

			v.log(LogDebug, "sending heartbeat")
			nonce := int(time.Now().Unix())
			evt := voiceClientEvent{
				Operation: 3,
				Data:      nonce,
			}
			msg, err := json.Marshal(evt)
			if err != nil {
				v.log(LogWarning, "failed to marshal Heartbeat as json, %s", err)
				continue
			}

			v.wsMutex.Lock()
			v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
			err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
			v.wsMutex.Unlock()
			if err != nil {
				err = errors.Wrapf(err, "failed to send Heartbeat to voice gateway %s", v.endpoint)
				return
			}

			heartbeatAcked = false
		}
	}
}

func (v *VoiceConnection) reconnect() error {
	return nil
}

func (v *VoiceConnection) Speaking(b bool) error {
	v.log(LogDebug, "called")

	v.eventMu.RLock()
	SSRC := v.ready.SSRC
	v.eventMu.RUnlock()

	evt := voiceClientEvent{
		Operation: 5,
		Data: voiceSpeaking{
			SSRC:     SSRC,
			Speaking: b,
		},
	}
	msg, err := json.Marshal(evt)
	if err != nil {
		return errors.Wrap(err, "failed to marshal Speaking event as json")
	}

	v.wsMutex.Lock()
	v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
	err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
	v.wsMutex.Unlock()
	if err != nil {
		return errors.Wrap(err, "failed to send Speaking event to voice gateway")
	}

	v.eventMu.Lock()
	v.speaking = b
	v.eventMu.Unlock()
	return nil
}

func (v *VoiceConnection) identify() error {
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
		return errors.Wrap(err, "failed to marshal Identify event as json")
	}

	v.wsMutex.Lock()
	v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
	err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
	v.wsMutex.Unlock()
	if err != nil {
		return errors.Wrap(err, "failed to send Identify event to voice gateway")
	}

	return nil
}

func (v *VoiceConnection) selectProtocol(ip string, port uint16) error {
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
		return errors.Wrap(err, "failed to marshal Select Protocol event as json")
	}

	v.wsMutex.Lock()
	v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
	err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
	v.wsMutex.Unlock()
	if err != nil {
		return errors.Wrap(err, "failed to send Select Protocol event to voice gateway")
	}

	return nil
}

// awaitEvent is similar in behavior to sync.Cond.Wait
// Wait until we have processed a websocket event with a particular opcode,
// or until timeout or the connection is closed.
func (v *VoiceConnection) awaitEvent(op voiceOp, timeoutC <-chan struct{}) error {
	v.eventMu.Lock()
	c, ok := v.receivedEvent[op]
	if !ok {
		c = make(chan struct{})
		v.receivedEvent[op] = c
	}
	v.eventMu.Unlock()
	select {
	case <-v.quit:
		return errors.New("connection closed")
	case <-timeoutC:
		return errors.New("timeout waiting for " + op.String() + " event")
	case <-c:
	}
	return nil
}
