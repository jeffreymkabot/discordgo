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
	// Timeout for writes on the voice websocket
	voiceWSWriteWait = 10 * time.Second
	// Maximum time allowed for the voice connection handshake
	voiceHandshakeTimeout = 10 * time.Second
	// How frequently to send a udp "keep alive" packet
	udpKeepAliveInterval = 5 * time.Second
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
	// used to signal during the handshake that voice events have been received
	receivedEvent map[voiceOp]chan struct{}

	udpConn *net.UDPConn
}

// TODO open should error if called on a voiceConnection that is already open
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
	v.heartbeatAckC = make(chan voiceHeartbeatAck, 1)
	v.receivedEvent = make(map[voiceOp]chan struct{})
	v.waitGroup.Add(1)
	go v.wsListener()

	// TODO does it make sense to use a context.Context?
	timeoutC := make(chan struct{})
	deadline := time.Now().Add(voiceHandshakeTimeout)
	time.AfterFunc(voiceHandshakeTimeout, func() {
		close(timeoutC)
	})

	// TODO
	tryResume := false
	haveReady := false
	if tryResume && haveReady {
		// First two events on gateway after sending Resume should be Op 8 Hello and Op 9 Resumed
		err = v.resume(deadline)
		if err != nil {
			return
		}
		err = v.awaitEvent(timeoutC, voiceOpHello, voiceOpResumed)
		if err != nil {
			return
		}
	} else {
		// First two events on gateway after sending Identify should be Op 8 Hello and Op 2 Ready
		err = v.identify(deadline)
		if err != nil {
			return
		}
		err = v.awaitEvent(timeoutC, voiceOpHello, voiceOpReady)
		if err != nil {
			return
		}
	}

	v.eventMu.RLock()
	hello := v.hello
	ready := v.ready
	v.eventMu.RUnlock()

	// docs specifically say to use 0.75 * heartbeat interval from HELLO
	// https://discordapp.com/developers/docs/topics/voice-connections#heartbeating
	heartbeatInterval := time.Duration(0.75*float64(hello.HeartbeatInterval)) * time.Millisecond
	// time.NewTicker will panic if interval is non-positive
	if hello.HeartbeatInterval <= 0 {
		err = errors.New("received invalid heartbeat interval " + heartbeatInterval.String())
		return
	}
	v.waitGroup.Add(1)
	go v.heartbeat(heartbeatInterval)

	udpConn, err := udpOpen(v.endpoint, ready.Port)
	if err != nil {
		err = errors.Wrap(err, "failed to open udp connection")
		return
	}
	v.udpConn = udpConn

	ip, port, err := ipDiscovery(udpConn, deadline, ready.SSRC)
	if err != nil {
		err = errors.Wrap(err, "failed to perform IP Discovery on udp connection")
		return
	}

	v.waitGroup.Add(1)
	go v.udpKeepAlive(udpKeepAliveInterval)

	// Gateway should send Op 4 Session Description after we sent Select Protocol
	// Session Description lets us encrypt and decrypt voice packets
	err = v.selectProtocol(deadline, ip, port)
	if err != nil {
		return
	}
	err = v.awaitEvent(timeoutC, voiceOpSessionDescription)
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
	if v.quit == nil {
		return errors.New("never opened")
	}
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

func (v *VoiceConnection) reconnect() (err error) {
	// TODO shouldReconnectOnError
	v.Close()
	// tryResume := true
	wait := 1 * time.Second
	for {
		select {
		// TODO separate channel for canceling a reconnects
		case <-v.quit:
			err = errors.New("reconnect canceled")
			return
		case <-time.After(wait):
		}

		err = v.open()
		if err == nil {
			return
		}
		// tryResume = false
		wait *= 2
		if wait > 600*time.Second {
			wait = 600 * time.Second
		}
	}
}

func (v *VoiceConnection) wsListener() {
	v.log(LogDebug, "called")

	defer func() {
		v.wsConn.Close()
		v.waitGroup.Done()
	}()
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
	case voiceOpSessionDescription:
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
			// heartbeatAckC should be buffered
		case v.heartbeatAckC <- heartbeatAck:
		default:
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
	// Remove the channel so that the next time the event type is received
	// there will only be a channel to close if new calls to awaitEvent have been made.
	v.eventMu.Lock()
	c, ok := v.receivedEvent[evt.Operation]
	if ok {
		close(c)
		delete(v.receivedEvent, evt.Operation)
	}
	v.eventMu.Unlock()

	return nil
}

func (v *VoiceConnection) heartbeat(interval time.Duration) {
	v.log(LogDebug, "called")

	var err error
	heartbeat := time.NewTicker(interval)
	defer func() {
		heartbeat.Stop()
		v.wsConn.Close()
		if err != nil {
			v.log(LogError, "error in wsEmitter %s", err)
			// TODO if err isnt because v.quit is closed (and the wsConn was closed, then reconnect)
		}
		v.waitGroup.Done()
	}()

	// Make sure we get a heartbeat ack before sending the next heartbeat
	// TODO relate nonces in heartbeat ack and heartbeat to match them up?
	// TODO allow one or more acks to be missed? looks like session.heartbeat allows 5 missed heartbeats
	heartbeatAcked := true

	for {
		select {
		case <-v.quit:
			return
		case <-v.heartbeatAckC:
			heartbeatAcked = true
		case <-heartbeat.C:
			if !heartbeatAcked {
				err = errors.New("failed to receive Heartbeat ACK in time")
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

func (v *VoiceConnection) Speaking(b bool) error {
	v.log(LogDebug, "called")

	v.eventMu.RLock()
	SSRC := v.ready.SSRC
	v.eventMu.RUnlock()

	evt := voiceClientEvent{
		Operation: voiceOpSpeaking,
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

func (v *VoiceConnection) identify(deadline time.Time) error {
	evt := voiceClientEvent{
		Operation: voiceOpIdentify,
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
	v.wsConn.SetWriteDeadline(deadline)
	err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
	v.wsMutex.Unlock()
	if err != nil {
		return errors.Wrap(err, "failed to send Identify event to voice gateway")
	}

	return nil
}

func (v *VoiceConnection) selectProtocol(deadline time.Time, ip string, port uint16) error {
	evt := voiceClientEvent{
		Operation: voiceOpSelectProtocol,
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
	v.wsConn.SetWriteDeadline(deadline)
	err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
	v.wsMutex.Unlock()
	if err != nil {
		return errors.Wrap(err, "failed to send Select Protocol event to voice gateway")
	}

	return nil
}

func (v *VoiceConnection) resume(deadline time.Time) error {
	evt := voiceClientEvent{
		Operation: voiceOpResume,
		Data: voiceResume{
			ServerID:  v.GuildID,
			SessionID: v.sessionID,
			Token:     v.token,
		},
	}
	msg, err := json.Marshal(evt)
	if err != nil {
		return errors.Wrap(err, "failed to marshal Resume event as json")
	}

	v.wsMutex.Lock()
	v.wsConn.SetWriteDeadline(deadline)
	err = v.wsConn.WriteMessage(websocket.TextMessage, msg)
	v.wsMutex.Unlock()
	if err != nil {
		return errors.Wrap(err, "failed to send Resume event to voice gateway")
	}

	return nil
}

// awaitEvent is similar in behavior to sync.Cond.Wait
// Wait until we have processed all websocket event with given opcodes (in any order),
// or until timeout or the connection is closed.
func (v *VoiceConnection) awaitEvent(timeoutC <-chan struct{}, ops ...voiceOp) error {
	var events []chan struct{}

	v.eventMu.Lock()
	for _, op := range ops {
		c, ok := v.receivedEvent[op]
		if !ok {
			c = make(chan struct{})
			v.receivedEvent[op] = c
		}
		events = append(events, c)
	}
	v.eventMu.Unlock()

	// wait for events in order
	// each channel will have been closed even if events are received out of order
	for i, c := range events {
		select {
		case <-v.quit:
			return errors.New("connection closed")
		case <-timeoutC:
			return errors.New("timeout waiting for " + ops[i].String() + " event")
		case <-c:
		}
	}
	return nil
}
