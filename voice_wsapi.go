package discordgo

import (
	"context"
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
	voiceWSWriteTimeout = 2 * time.Second
	// Maximum time allowed for the voice connection handshake
	voiceHandshakeTimeout = 10 * time.Second
	// Number of voice connection heartbeat intervals to tolerate before restarting connection.
	voiceFailedHeartbeatAcks = 5
	// How frequently to send a udp "keep alive" packet
	udpKeepAliveInterval = 5 * time.Second
	// Timeout for writes on the udp connection
	udpWriteTimeout = 2 * time.Second
)

// A VoiceConnection struct holds all the data and functions related to a Discord Voice Connection.
type VoiceConnection struct {
	// Mutex synchronizes fields set in ChannelVoiceJoin and by events on main gateway
	// Serialize concurrent calls to v.open() and v.Close()
	sync.RWMutex

	// following block should be set before calling open

	UserID    string
	GuildID   string
	ChannelID string
	mute      bool
	deaf      bool
	endpoint  string
	token     string
	sessionID string
	session   *Session

	Debug    bool // If true, print extra logging -- DEPRECATED, use LogLevel instead
	LogLevel int
	Ready    bool // If true, voice is ready to send/receive audio

	OpusSend chan []byte  // Chan for sending opus audio
	OpusRecv chan *Packet // Chan for receiving opus audio

	// serialize concurrent writes on websocket
	wsMutex sync.Mutex
	wsConn  *websocket.Conn

	udpConn *net.UDPConn

	voiceSpeakingUpdateHandlers []VoiceSpeakingUpdateHandler

	ssrc uint32

	speakingMutex sync.RWMutex
	speaking      bool

	// lifecycle
	quit    func()         // signal goroutines to return
	wg      sync.WaitGroup // wait for goroutines to return
	dispose func()         // cancel reconnects
}

// context passed to open allows cancellation of handshake
func (v *VoiceConnection) open(openCtx context.Context) error {
	// serialize concurrent calls with v.Close()
	v.Lock()
	defer v.Unlock()
	v.log(LogDebug, "called")

	h, err := v.handshake(openCtx)
	if err != nil {
		return err
	}

	v.wsConn, v.udpConn, v.ssrc = h.wsConn, h.udpConn, h.ready.SSRC

	// must reuse OpusSend and OpusRecv if already exists so clients of the library do not need to handle reconnect
	if v.OpusSend == nil {
		v.OpusSend = make(chan []byte, 2)
	}
	if !v.deaf && v.OpusRecv == nil {
		v.OpusRecv = make(chan *Packet)
	}

	quit := make(chan struct{})
	var quitOnce sync.Once
	v.quit = func() {
		quitOnce.Do(func() {
			close(quit)
		})
	}

	// first goroutine to experience an error sends it on errC to reconnectOnError
	errC := make(chan error)
	reconnectCtx, reconnectCancel := context.WithCancel(context.Background())
	v.dispose = reconnectCancel
	go v.reconnectOnError(reconnectCtx, errC)

	onError := func(err error) {
		err = errors.WithStack(err)
		select {
		case errC <- err:
		default:
		}
	}

	heartbeatInterval := time.Duration(0.75*float64(h.hello.HeartbeatInterval)) * time.Millisecond
	heartbeatAckC := make(chan time.Time, 1)
	v.wg.Add(4)
	go v.wsListener(quit, onError, heartbeatAckC)
	go v.wsHeartbeat(quit, onError, heartbeatInterval, heartbeatAckC)
	go v.udpKeepAlive(quit, onError, udpKeepAliveInterval)
	go v.opusSender(quit, onError, v.OpusSend, h.ready.SSRC, h.sessionDesc.SecretKey, 48000, 960)
	if !v.deaf {
		v.wg.Add(1)
		go v.opusReceiver(quit, onError, v.OpusRecv, h.sessionDesc.SecretKey)
	}

	v.Ready = true
	return nil
}

// Close closes the voice websocket and udp connections
// and stops any attempts to automatically reconnect the voice connection.
func (v *VoiceConnection) Close() error {
	// serialize concurrent calls with v.open() or other calls to v.Close()
	v.Lock()
	defer v.Unlock()
	v.log(LogDebug, "called")

	v.dispose() // TODO prevent call nil

	v.closeConnections(context.Background())
	return nil
}

func (v *VoiceConnection) closeConnections(ctx context.Context) {
	v.log(LogDebug, "called")
	v.quit() // TODO prevent call nil
	// wsConn and udpConn are not nil if v was ever successfully opened, but belt and braces
	if v.wsConn != nil {
		// "Close and WriteControl methods can be called concurrently with all other methods."
		v.wsConn.WriteControl(websocket.CloseMessage, []byte{}, time.Now().Add(voiceWSWriteTimeout))
		v.wsConn.Close()
	}
	if v.udpConn != nil {
		v.udpConn.Close()
	}
	// wait for goroutines to terminate
	v.wg.Wait()
}

// Disconnect tells discord we have left the voice channel, closes the voice websocket
// and udp connections, and stops any attempts to automatically reconnect the voice connection.
func (v *VoiceConnection) Disconnect() (err error) {
	v.log(LogDebug, "called")

	// Close all resources and stop all reconnect attempts.
	v.Close()

	// TODO before or after v.Close() ?
	// Send a OP4 with a nil channel to disconnect
	data := voiceChannelJoinOp{4, voiceChannelJoinData{&v.GuildID, nil, true, true}}
	v.session.wsMutex.Lock()
	err = v.session.wsConn.WriteJSON(data)
	v.session.wsMutex.Unlock()

	v.log(LogInformational, "deleting VoiceConnection %s", v.GuildID)

	v.session.Lock()
	delete(v.session.VoiceConnections, v.GuildID)
	v.session.Unlock()

	return
}

// reconnect if receive an error on errC
// accepts a context so that the reconnect attempts can be cancelled
func (v *VoiceConnection) reconnectOnError(reconnectCtx context.Context, errC <-chan error) {
	v.log(LogDebug, "called")

	var err error
	select {
	case <-reconnectCtx.Done(): // cancelled by v.Close()
		return
	case err = <-errC: // a voice connection goroutine had an error
	}

	v.log(LogError, "voice connection error: %+v", err)

	v.closeConnections(reconnectCtx)

	wait := 1 * time.Second
	select {
	case <-reconnectCtx.Done(): // cancelled by v.Close()
		return
	case <-time.After(wait):
		v.log(LogInformational, "trying to reconnect voice connection")
		// cancelled after timeout or if reconnectCtx's cancel is called
		openCtx, openCancel := context.WithTimeout(reconnectCtx, voiceHandshakeTimeout)
		err := v.open(openCtx)
		openCancel()
		if err == nil {
			return
		}
		v.log(LogError, "error reconnecting voice connection %v", err)

		wait *= 2
		if wait > 600*time.Second {
			wait = 600 * time.Second
		}
	}
}

type handshakeResult struct {
	wsConn  *websocket.Conn
	udpConn *net.UDPConn

	ip   string
	port uint16

	hello       voiceHello
	ready       voiceReady
	sessionDesc voiceSessionDescription
}

func (v *VoiceConnection) handshake(ctx context.Context) (h handshakeResult, err error) {
	endpoint := strings.TrimSuffix(v.endpoint, ":80")
	wsGateway := "wss://" + endpoint + "?v=" + voiceAPIVersion
	wsConn, _, err := websocket.DefaultDialer.DialContext(ctx, wsGateway, nil)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			wsConn.Close()
		}
	}()

	deadline, ok := ctx.Deadline()
	if ok {
		wsConn.SetReadDeadline(deadline)
		wsConn.SetWriteDeadline(deadline)
		defer wsConn.SetReadDeadline(time.Time{})
		defer wsConn.SetWriteDeadline(time.Time{})
	}

	v.log(LogDebug, "identifying %#v", v.identifyEvent())
	err = wsConn.WriteJSON(v.identifyEvent())
	if err != nil {
		return
	}

	var hello voiceHello
	var ready voiceReady
	var msg1, msg2, msg3 []byte
	var evt1, evt2, evt3 interface{}
	if _, msg1, err = wsConn.ReadMessage(); err != nil {
		return
	}
	v.log(LogDebug, "received: %s", msg1)
	if _, msg2, err = wsConn.ReadMessage(); err != nil {
		return
	}
	v.log(LogDebug, "received: %s", msg2)
	if evt1, err = parseVoiceEvent(msg1); err != nil {
		return
	}
	if evt2, err = parseVoiceEvent(msg2); err != nil {
		return
	}
	switch evt := evt1.(type) {
	case voiceHello:
		hello = evt
	case voiceReady:
		ready = evt
	}
	switch evt := evt2.(type) {
	case voiceHello:
		hello = evt
	case voiceReady:
		ready = evt
	}

	udpConn, err := udpOpen(endpoint, ready.Port)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			udpConn.Close()
		}
	}()

	ip, port, err := ipDiscovery(ctx, udpConn, ready.SSRC)
	if err != nil {
		return
	}

	err = wsConn.WriteJSON(v.selectProtocolEvent(ip, port))
	if err != nil {
		return
	}

	if _, msg3, err = wsConn.ReadMessage(); err != nil {
		return
	}
	v.log(LogDebug, "received: %s", msg3)
	if evt3, err = parseVoiceEvent(msg3); err != nil {
		return
	}

	sessionDesc, ok := evt3.(voiceSessionDescription)
	if !ok {
		err = errors.New("failed to parse session description event")
		return
	}

	h = handshakeResult{
		wsConn:      wsConn,
		udpConn:     udpConn,
		ip:          ip,
		port:        port,
		hello:       hello,
		ready:       ready,
		sessionDesc: sessionDesc,
	}

	return
}

func (v *VoiceConnection) wsListener(quit <-chan struct{}, onError func(error), heartbeatAckC chan<- time.Time) {
	v.log(LogDebug, "called")
	defer v.wg.Done()

	for {
		_, msg, err := v.wsConn.ReadMessage()
		if err != nil {
			onError(err)
			return
		}

		v.log(LogDebug, "received: %s", msg)

		select {
		case <-quit:
		default:
		}

		data, err := parseVoiceEvent(msg)
		if err != nil {
			v.log(LogWarning, "failed to parse voice event %v", err)
			continue
		}

		switch evt := data.(type) {
		case VoiceSpeakingUpdate:
			if evt.UserID == v.UserID {
				// We can get a speaking update about ourself if another user changes our channel
				// If for whatever reason our speaking status gets set to false,
				// opusSender needs to send a new Speaking frame for users to continue to receive our audio
				v.speakingMutex.Lock()
				v.speaking = evt.Speaking
				v.speakingMutex.Unlock()
			}
			// TODO VoiceConnection.AddHandler
			for _, h := range v.voiceSpeakingUpdateHandlers {
				h(v, &evt)
			}
		case voiceHeartbeatAck:
			select {
			case <-quit:
				return
			case heartbeatAckC <- time.Now():
			}
		default:
			// TODO what if we receive unexpected handshake events (after handshake)?
		}
	}
}

func parseVoiceEvent(msg []byte) (data interface{}, err error) {
	var evt voiceEvent
	if err = json.Unmarshal(msg, &evt); err != nil {
		return
	}

	switch evt.Operation {
	case voiceOpReady:
		var ready voiceReady
		if err = json.Unmarshal(evt.RawData, &ready); err != nil {
			return
		}
		data = ready
	case voiceOpSessionDescription:
		var sessionDesc voiceSessionDescription
		if err = json.Unmarshal(evt.RawData, &sessionDesc); err != nil {
			return
		}
		data = sessionDesc
	case voiceOpSpeaking:
		var speaking voiceSpeaking
		if err = json.Unmarshal(evt.RawData, &speaking); err != nil {
			return
		}
		data = speaking
	case voiceOpHeartbeatAck:
		var heartbeatAck voiceHeartbeatAck
		if err = json.Unmarshal(evt.RawData, &heartbeatAck); err != nil {
			return
		}
		data = heartbeatAck
	case voiceOpHello:
		var hello voiceHello
		if err = json.Unmarshal(evt.RawData, &hello); err != nil {
			return
		}
		data = hello
	case voiceOpResumed:
		var resumed voiceResumed
		if err = json.Unmarshal(evt.RawData, &resumed); err != nil {
			return
		}
		data = resumed
	}
	return
}

func (v *VoiceConnection) wsHeartbeat(quit <-chan struct{}, onError func(error), heartbeatInterval time.Duration, heartbeatAckC <-chan time.Time) {
	v.log(LogDebug, "called")
	defer v.wg.Done()

	lastAck := time.Now()
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-quit:
			return
		case lastAck = <-heartbeatAckC:
		case <-heartbeatTicker.C:
			sinceLastAck := time.Now().Sub(lastAck)
			if sinceLastAck > voiceFailedHeartbeatAcks*heartbeatInterval {
				err := errors.Errorf("have not received heartbeat ACK in %v", sinceLastAck)
				onError(err)
				return
			}

			nonce := int(time.Now().Unix())

			v.log(LogDebug, "sending voice connection heartbeat")
			v.wsMutex.Lock()
			v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteTimeout))
			err := v.wsConn.WriteJSON(v.heartbeatEvent(nonce))
			v.wsConn.SetWriteDeadline(time.Time{})
			v.wsMutex.Unlock()

			if err != nil {
				err = errors.Wrapf(err, "failed to send heartbeat to voice gateway %v", v.endpoint)
				onError(err)
				return
			}
		}
	}
}

// Speaking sends a speaking notification to Discord over the voice websocket.
// This must be sent as true prior to sending audio and should be set to false
// once finished sending audio.
//  b  : Send true if speaking, false if not.
func (v *VoiceConnection) Speaking(b bool) error {
	v.log(LogDebug, "called (%t)", b)

	v.wsMutex.Lock()
	v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteTimeout))
	err := v.wsConn.WriteJSON(v.speakingEvent(b))
	v.wsConn.SetWriteDeadline(time.Time{})
	v.wsMutex.Unlock()

	if err != nil {
		return errors.Wrap(err, "failed to send Speaking event to voice gateway")
	}

	v.speakingMutex.Lock()
	v.speaking = b
	v.speakingMutex.Unlock()

	return nil
}

func (v *VoiceConnection) speakingEvent(b bool) voiceClientEvent {
	return voiceClientEvent{
		Operation: voiceOpSpeaking,
		Data: voiceSpeaking{
			SSRC:     v.ssrc,
			Speaking: b,
		},
	}
}

func (v *VoiceConnection) identifyEvent() voiceClientEvent {
	return voiceClientEvent{
		Operation: voiceOpIdentify,
		Data: voiceIdentify{
			UserID:    v.UserID,
			ServerID:  v.GuildID,
			SessionID: v.sessionID,
			Token:     v.token,
		},
	}
}

func (v *VoiceConnection) heartbeatEvent(nonce int) voiceClientEvent {
	return voiceClientEvent{
		Operation: voiceOpHeartbeat,
		Data:      nonce,
	}
}

func (v *VoiceConnection) selectProtocolEvent(ip string, port uint16) voiceClientEvent {
	return voiceClientEvent{
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
}

func (v *VoiceConnection) resumeEvent(deadline time.Time) voiceClientEvent {
	return voiceClientEvent{
		Operation: voiceOpResume,
		Data: voiceResume{
			ServerID:  v.GuildID,
			SessionID: v.sessionID,
			Token:     v.token,
		},
	}
}

// ChangeChannel sends Discord a request to change channels within a Guild
// !!! NOTE !!! This function may be removed in favour of just using ChannelVoiceJoin
func (v *VoiceConnection) ChangeChannel(channelID string, mute, deaf bool) (err error) {
	v.log(LogInformational, "called")

	v.RLock()
	session, guildID := v.session, v.GuildID
	v.RUnlock()

	_, err = session.ChannelVoiceJoin(guildID, channelID, mute, deaf)
	return
}
