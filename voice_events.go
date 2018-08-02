package discordgo

import (
	"encoding/json"
	"strconv"
	"time"
)

const voiceAPIVersion = "3"

type voiceOp int

const (
	voiceOpIdentify voiceOp = iota
	voiceOpSelectProtocol
	voiceOpReady
	voiceOpHeartbeat
	voiceOpSessionDescription
	voiceOpSpeaking
	voiceOpHeartbeatAck
	voiceOpResume
	voiceOpHello
	voiceOpResumed
)

// voiceEvent is the base structure of voice websocket events received from server.
// The type of the data component of the voiceEvent varies by Opcode.
// We use a json.RawMessage to delay parsing the data until we know the Opcode.
type voiceEvent struct {
	Operation voiceOp `json:"op"`
	// Shape of RawData varies by Operation
	// May be a nested struct, an int, or null
	RawData json.RawMessage `json:"d"`
}

// voiceClientEvent is the base structure of voice websocket events sent by us.
// As a client we generally know up front what Operation we are sending, so we use a separate
// base struct to avoid having to do partial json.Marshals on the nested structs.
type voiceClientEvent struct {
	Operation int         `json:"op"`
	Data      interface{} `json:"d"`
}

// voiceIdentify stores the data of the Opcode 0 Identify event.
// The Identify event is sent by the client when the websocket is first opened.
type voiceIdentify struct {
	ServerID  string `json:"server_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
}

// voiceSelectProtocol stores the data of the Opcode 1 Select Protocol event.
// The Select Protocol event is sent by the client to configure the udp connection.
type voiceSelectProtocol struct {
	Protocol string       `json:"protocol"` // Always "udp" ?
	Data     voiceUDPData `json:"data"`
}

type voiceUDPData struct {
	Address string `json:"address"` // Public IP of machine running this code
	Port    uint16 `json:"port"`    // UDP Port of machine running this code
	Mode    string `json:"mode"`    // always "xsalsa20_poly1305"
}

// voiceReady stores the data component of the Opcode 2 Ready event.
// The Ready event is sent by the server in response to the Identify event.
type voiceReady struct {
	SSRC  uint32   `json:"ssrc"`
	Port  int      `json:"port"`
	Modes []string `json:"modes"`
	// docs specifically say to ignore this field
	// correct value is in HELLO event
	// https://discordapp.com/developers/docs/topics/voice-connections#establishing-a-voice-websocket-connection
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
}

// voiceOPHeartbeat stores the data of the Opcode 3 Heartbeat event.
// The Heartbeat event is sent by the client.
type voiceHeartbeat int

// voiceSessionDescription stores the data of the Opcode 4 Session Description event
// The Session Description event is sent by the server to provide the NaCL SecretBox encryption key.
type voiceSessionDescription struct {
	Mode      string   `json:"mode"`
	SecretKey [32]byte `json:"secret_key"`
}

// VoiceSpeakingUpdate stores the data of the server's Opcode 5 Speaking event.
// The Speaking event is sent by the server when a user in the channel starts or stops speaking.
type VoiceSpeakingUpdate struct {
	UserID   string `json:"user_id"`
	SSRC     int    `json:"ssrc"`
	Speaking bool   `json:"speaking"`
}

// VoiceSpeakingUpdateHandler type provides a function definition for the VoiceSpeakingUpdate event.
type VoiceSpeakingUpdateHandler func(vc *VoiceConnection, vs *VoiceSpeakingUpdate)

// VoiceSpeaking stores the data of the client's Opcode 5 Speaking event.
// Clients must send a Speaking event before sending voice data.
type voiceSpeaking struct {
	Speaking bool   `json:"speaking"`
	Delay    int    `json:"delay"`
	SSRC     uint32 `json:"ssrc"`
}

// voiceHeartbeatAck stores the data of the Opcode 6 Heartbeat ACK event.
// The Heartbeat ACK event is sent by the server after the client sends a Heartbeat ACK event.
type voiceHeartbeatAck int

// voiceResume stores the data of the Opcode 7 Resume event.
// The Resume event is sent by the client to restore a disconnected voice session.
type voiceResume struct {
	ServerID  string `json:"server_id"`
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
}

// voiceHello stores the data of the Opcode 8 Hello event.
// The Hello event is sent by the server in response to the Identify event.
type voiceHello struct {
	// docs say to use 0.75 times this value
	// https://discordapp.com/developers/docs/topics/voice-connections#heartbeating
	HeartbeatInterval int `json:"heartbeat_interval"`
}

// voiceResumed stores the data of the Opcode 9 Resumed event.
// The Resumed event is sent by the server after the client sends a Resume event.
type voiceResumed *struct{} // Always null

// Map opcodes to a name for error messages and logging.
var voiceOpName = map[voiceOp]string{
	0: "Identify",
	1: "Select Protocol",
	2: "Ready",
	3: "Heartbeat",
	4: "Session Description",
	5: "Speaking",
	6: "Heartbeat ACK",
	7: "Resume",
	8: "Hello",
	9: "Resumed",
}

func (vo voiceOp) String() string {
	name, ok := voiceOpName[vo]
	if ok {
		return name
	}
	return "Unknown Opcode: " + strconv.Itoa(int(vo))
}
