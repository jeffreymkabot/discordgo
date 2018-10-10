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
	voiceWSWriteWait = 2 * time.Second
	// Maximum time allowed for the voice connection handshake
	voiceHandshakeTimeout = 10 * time.Second
	// How frequently to send a udp "keep alive" packet
	udpKeepAliveInterval = 5 * time.Second
	udpWriteTimeout      = 2 * time.Second
)

// A VoiceConnection struct holds all the data and functions related to a Discord Voice Connection.
type VoiceConnection struct {
	// Mutex synchronizes fields set in ChannelVoiceJoin and by events on main gateway
	// Prevents concurrent calls to v.open() and v.Close()
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

	// prevent concurrent writes on websocket
	wsMutex sync.Mutex
	wsConn  *websocket.Conn

	voiceSpeakingUpdateHandlers []VoiceSpeakingUpdateHandler

	ssrc uint32
	// TODO protect with mutex / atomics
	speaking bool

	quit chan<- struct{}
}

// context passed to open allows cancellation of handshake
func (v *VoiceConnection) open(ctx context.Context) error {
	v.Lock()
	defer v.Unlock()

	h, err := v.handshake(ctx)
	if err != nil {
		return err
	}

	quit := make(chan struct{})
	v.quit = quit
	heartbeatInterval := time.Duration(0.75*float64(h.hello.HeartbeatInterval)) * time.Millisecond

	go v.wsListener(quit, h.wsConn)
	go v.voiceWsHeartbeat(quit, h.wsConn, heartbeatInterval)
	go udpKeepAlive(quit, h.udpConn, udpKeepAliveInterval)

	// must reuse OpusSend and OpusRecv if already exists so clients of the library do not need to handle reconnect
	if v.OpusSend == nil {
		v.OpusSend = make(chan []byte, 2)
	}
	if v.OpusRecv == nil {
		v.OpusRecv = make(chan *Packet)
	}

	go opusSender(quit, h.udpConn, v.OpusSend, h.ready.SSRC, h.sessionDesc.SecretKey, 48000, 960)
	if !v.deaf {
		go opusReceiver(quit, h.udpConn, v.OpusRecv, h.sessionDesc.SecretKey)
	}

	go func() {
		<-quit
		// TODO attempt reconnect if allowed
	}()

	v.wsConn = h.wsConn
	v.Ready = true
	return nil
}

type handshake struct {
	wsConn  *websocket.Conn
	udpConn *net.UDPConn

	ip   string
	port uint16

	hello       voiceHello
	ready       voiceReady
	sessionDesc voiceSessionDescription
}

func (v *VoiceConnection) handshake(ctx context.Context) (h handshake, err error) {
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
	if _, msg2, err = wsConn.ReadMessage(); err != nil {
		return
	}
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
	if evt3, err = parseVoiceEvent(msg3); err != nil {
		return
	}

	sessionDesc, ok := evt3.(voiceSessionDescription)
	if !ok {
		err = errors.New("failed to parse session description event")
		return
	}

	h = handshake{
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

// Close closes the voice ws and udp connections and stops any attempts to automatically reconnect.
func (v *VoiceConnection) Close() error {
	v.log(LogDebug, "called")
	// TODO close and prevent reconnects
	return nil
}

// Disconnect tells discord we have left the voice channel, closes the voice websocket
// and udp connections, and stops any attempts to automatically r econnect.
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

func (v *VoiceConnection) reconnect() (err error) {
	// TODO
	return nil
}

func (v *VoiceConnection) wsListener(quit <-chan struct{}, wsConn *websocket.Conn) {
	for {
		_, msg, err := wsConn.ReadMessage()
		if err != nil {
			// TODO signal voice connection error, possibly reconnect
			return
		}

		select {
		case <-quit:
		default:
		}

		data, err := parseVoiceEvent(msg)
		if err != nil {
			// warning
			continue
		}

		switch evt := data.(type) {
		case VoiceSpeakingUpdate:
			if evt.UserID == v.UserID {
				// We can get a speaking update about ourself if another user changes our channel
				// If for whatever reason our speaking status gets set to false,
				// opusSender needs to send a new Speaking frame for users to continue to receive our audio
				// TODO
			}
			for _, h := range v.voiceSpeakingUpdateHandlers {
				h(v, &evt)
			}
		case voiceHeartbeatAck:
			// TODO
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

func (v *VoiceConnection) voiceWsHeartbeat(quit chan struct{}, wsConn *websocket.Conn, heartbeatInterval time.Duration) {
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	var lastHeartbeat time.Time
	// TODO check missed acks for error
	var lastHeartbeatAck time.Time
	for {
		select {
		case <-quit:
			return
		case <-heartbeatTicker.C:
			nonce := int(time.Now().Unix())

			v.wsMutex.Lock()
			v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
			err := v.wsConn.WriteJSON(v.heartbeatEvent(nonce))
			v.wsConn.SetWriteDeadline(time.Time{})
			v.wsMutex.Unlock()

			if err != nil {
				// signal voice connection error, possibly reconnect
				return
			}

			lastHeartbeat = time.Now()
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
	v.wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
	err := v.wsConn.WriteJSON(v.speakingEvent(b))
	v.wsConn.SetWriteDeadline(time.Time{})
	v.wsMutex.Unlock()

	if err != nil {
		return errors.Wrap(err, "failed to send Speaking event to voice gateway")
	}

	v.speaking = b
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
		Operation: voiceOpHello,
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
