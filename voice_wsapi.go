package discordgo

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
)

const voiceWSWriteWait = 10 * time.Second

type VoiceConnection struct {
	sync.RWMutex

	LogLevel int
}

func voiceWSListener(wsConn *websocket.Conn) {
	defer func() {
		wsConn.Close()
	}()
	for {
		_, msg, err := wsConn.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				//
				return
			}
			//
		}

		err = onVoiceWSEvent(msg)
		if err != nil {
			//
		}
	}
}

func onVoiceWSEvent(msg []byte) error {
	var evt voiceEvent
	if err := json.Unmarshal(msg, &evt); err != nil {
		return errors.Wrap(err, "failed to unmarshal json")
	}

	switch evt.Operation {
	case 2: // READY
	case 4: // SESSION DESCRIPTION
	case 5: // SPEAKING
	case 6: // HEARTBEAT ACK
	case 8: // HELLO
	case 9: // RESUMED
	default:
	}

	return nil
}

func voiceWSEmitter(wsConn *websocket.Conn, heartbeat time.Duration) {
	ticker := time.NewTicker(heartbeat)
	defer func() {
		wsConn.Close()
		ticker.Stop()
	}()
	for {
		select {
		case <-quit:
			return
		case <-identifyC:
		case <-selectProtocolC:
		case <-speakingC:
		case <-ticker.C:
			sendHeartbeat(wsConn)
		}
	}
}

func sendHeartbeat(wsConn *websocket.Conn) error {
	nonce := int(time.Now().Unix())
	nonceJSON := []byte(strconv.Itoa(nonce))
	evt := voiceEvent{
		Operation: 3,
		RawData:   nonceJSON,
	}

	err := wsConn.SetWriteDeadline(time.Now().Add(voiceWSWriteWait))
	if err != nil {
		return errors.Wrap(err, "failed to set write deadline")
	}
	err = wsConn.WriteJSON(evt)
	if err != nil {
		return errors.Wrap(err, "failed to send heartbeat")
	}
	return nil
}
