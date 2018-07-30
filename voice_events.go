package discordgo

import (
	"encoding/json"
	"time"
)

const voiceAPIVersion = "3"

// voiceEvent is the base structure of voice websocket events.
type voiceEvent struct {
	Operation int `json:"op"`
	// Shape of RawData varies by Operation
	// May be a nested struct, an int, or null
	RawData json.RawMessage `json:"d"`
}

// voiceIdentify stores the data of the Opcode 0 IDENTIFY event.
// The IDENTIFY event is sent by the client when the websocket is first opened.
type voiceIdentify struct {
	ServerID  string `json:"server_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
}

// voiceSelectProtocol stores the data of the Opcode 1 SELECT PROTOCOL event.
// The SELECT PROTOCOL event is sent by the client.
type voiceSelectProtocol struct {
	Protocol string       `json:"protocol"` // Always "udp" ?
	Data     voiceUDPData `json:"data"`
}

type voiceUDPData struct {
	Address string `json:"address"` // Public IP of machine running this code
	Port    uint16 `json:"port"`    // UDP Port of machine running this code
	Mode    string `json:"mode"`    // always "xsalsa20_poly1305"
}

// voiceReady stores the data component of the Opcode 2 READY event.
// The READY event is sent by the server when the websocket is first opened.
type voiceReady struct {
	SSRC  uint32   `json:"ssrc"`
	Port  int      `json:"port"`
	Modes []string `json:"modes"`
	// docs specifically say to ignore this field
	// correct value is in HELLO event
	// https://discordapp.com/developers/docs/topics/voice-connections#establishing-a-voice-websocket-connection
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
}

// voiceOPHeartbeat stores the data of the Opcode 3 HEARTBEAT event.
// The HEARTBEAT event is sent by the client.
type voiceHeartbeat int

// voiceSessionDescription stores the data of the Opcode 4 SESSION DESCRIPTION event
// The SESSION DESCRIPTION event is sent by the server.
// This event provides the NaCL SecretBox encryption key.
type voiceSessionDescription struct {
	Mode      string   `json:"mode"`
	SecretKey [32]byte `json:"secret_key"`
}

// VoiceSpeakingUpdate stores the data of the Opcode 5 SPEAKING event.
// The SPEAKING event is sent by the server whenever a user in the voice channel starts or stops speaking.
type VoiceSpeakingUpdate struct {
	UserID   string `json:"user_id"`
	SSRC     int    `json:"ssrc"`
	Speaking bool   `json:"speaking"`
}

// voiceHeartbeatAck stores the data of the Opcode 6 HEARTBEAT ACK event.
// The HEATBEAT ACK event is sent by the server after the client sends a HEARTBEAT event.
type voiceHeartbeatAck int

// voiceResume stores the data of the Opcode 7 RESUME event.
// The RESUME event is sent by the client to restore a disconnected voice session.
type voiceResume struct {
	ServerID  string `json:"server_id"`
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
}

// voiceHello stores the data of the Opcode 8 HELLO event.
// The HELLO event is sent by the server when the websocket is first opened.
type voiceHello struct {
	// docs say to use 0.75 times this value
	// https://discordapp.com/developers/docs/topics/voice-connections#heartbeating
	HeartbeatInterval int `json:"heartbeat_interval"`
}

// voiceResumed stores the data of the Opcode 9 RESUMED event.
// The RESUMED event is sent by the server after the client sends a RESUME event.
type voiceResumed *struct{} // Always null
