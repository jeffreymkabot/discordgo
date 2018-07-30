package discordgo

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/crypto/nacl/secretbox"
)

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

func ipDiscovery(udpConn *net.UDPConn, SSRC uint32) (ip string, port uint16, err error) {
	addr := udpConn.RemoteAddr().String()

	// Create a 70 byte array and put the SSRC code from the Opcode 2 READY event
	// into it.  Then send that over the UDP connection to Discord.
	sb := make([]byte, 70)
	binary.BigEndian.PutUint32(sb, SSRC)
	_, err = udpConn.Write(sb)
	if err != nil {
		err = errors.Wrapf(err, "udp write error to %s", addr)
		return
	}

	// Create a 70 byte array and listen for the initial handshake response
	// from Discord.  Once we get it parse the IP and PORT information out
	// of the response.  This should be our public IP and PORT as Discord
	// saw us.
	rb := make([]byte, 70)
	rlen, _, err := udpConn.ReadFromUDP(rb)
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

func (v *VoiceConnection) udpKeepAlive(interval time.Duration) {
	defer v.waitGroup.Done()
	defer v.udpConn.Close()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

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
			// TODO log error
			return
		}
	}
}

func (v *VoiceConnection) opusSender(rate, size int) {
	defer v.waitGroup.Done()

	var sequence uint16
	var timestamp uint32
	var recvbuf []byte
	var ok bool
	udpHeader := make([]byte, 12)
	var nonce [24]byte

	v.RLock()
	SSRC := v.ready.SSRC
	v.RUnlock()

	// build the parts that don't change in the udpHeader
	udpHeader[0] = 0x80
	udpHeader[1] = 0x78
	binary.BigEndian.PutUint32(udpHeader[8:], SSRC)

	ticker := time.NewTicker(time.Millisecond * time.Duration(size/(rate/1000)))
	defer ticker.Stop()

	for {
		select {
		case <-v.quit:
			return
		case recvbuf, ok = <-v.OpusSend:
			if !ok {
				return
			}

			v.RLock()
			speaking := v.speaking
			secretKey := v.sessionDesc.SecretKey
			v.RUnlock()

			if !speaking {
				err := v.Speaking(true)
				if err != nil {
					// TODO try to send data anyway?
				}
			}

			// Add sequence and timestamp to udpPacket
			binary.BigEndian.PutUint16(udpHeader[2:], sequence)
			binary.BigEndian.PutUint32(udpHeader[4:], timestamp)

			// encrypt the opus data
			copy(nonce[:], udpHeader)
			sendbuf := secretbox.Seal(udpHeader, recvbuf, &nonce, &secretKey)

			// block here until we're exactly at the right time :)
			// Then send rtp audio packet to Discord over UDP
			select {
			case <-v.quit:
				return
			case <-ticker.C:
			}

			_, err := v.udpConn.Write(sendbuf)
			if err != nil {
				// TODO log
				return
			}

			// don't overflow
			if (sequence) == 0xFFFF {
				sequence = 0
			} else {
				sequence++
			}
			if (timestamp + uint32(size)) >= 0xFFFFFFFF {
				timestamp = 0
			} else {
				timestamp += uint32(size)
			}
		}
	}
}

// A Packet contains the headers and content of a received voice packet.
type Packet struct {
	SSRC      uint32
	Sequence  uint16
	Timestamp uint32
	Type      []byte
	Opus      []byte
	PCM       []int16
}

func (v *VoiceConnection) opusReceiver() {
	defer v.waitGroup.Done()

	recvbuf := make([]byte, 1024)
	var nonce [24]byte

	for {
		rlen, err := v.udpConn.Read(recvbuf)
		if err != nil {
			// TODO log error
			return
		}

		select {
		case <-v.quit:
			return
		default:
			// continue loop
		}

		// For now, skip anything except audio.
		if rlen < 12 || (recvbuf[0] != 0x80 && recvbuf[0] != 0x90) {
			continue
		}

		// build a audio packet struct
		p := Packet{}
		p.Type = recvbuf[0:2]
		p.Sequence = binary.BigEndian.Uint16(recvbuf[2:4])
		p.Timestamp = binary.BigEndian.Uint32(recvbuf[4:8])
		p.SSRC = binary.BigEndian.Uint32(recvbuf[8:12])
		// decrypt opus data
		copy(nonce[:], recvbuf[0:12])

		v.RLock()
		secretKey := v.sessionDesc.SecretKey
		v.RUnlock()

		p.Opus, _ = secretbox.Open(nil, recvbuf[12:rlen], &nonce, &secretKey)

		if len(p.Opus) > 8 && recvbuf[0] == 0x90 {
			// Extension bit is set, first 8 bytes is the extended header
			p.Opus = p.Opus[8:]
		}

		select {
		case <-v.quit:
			return
		case v.OpusRecv <- &p:
		}
	}
}
