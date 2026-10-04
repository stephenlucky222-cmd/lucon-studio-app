package main

// PTZ camera control with VISCA.
//  sony : VISCA over IP (Sony standard) – UDP port 52381, 8-byte header in front of each command
//  udp  : plain VISCA over UDP (PTZOptics and many others) – usually port 1259
//  tcp  : plain VISCA over TCP – usually port 5678

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type PTZCam struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	IP      string            `json:"ip"`
	Proto   string            `json:"proto"` // sony | udp | tcp
	Port    int               `json:"port"`
	Presets map[string]string `json:"presets,omitempty"` // "1" -> "Podium"
}

var ptzSeq = map[string]uint32{}
var ptzMu sync.Mutex

func ptzDefaultPort(proto string) int {
	switch proto {
	case "udp":
		return 1259
	case "tcp":
		return 5678
	}
	return 52381
}

func speeds(s string) (pan, tilt, zoom byte) {
	switch s {
	case "slow":
		return 0x04, 0x04, 2
	case "fast":
		return 0x14, 0x12, 7
	}
	return 0x0A, 0x0A, 4
}

// ptzCommand builds the VISCA bytes for one action.
func ptzCommand(cmd, dir, speed string, n int) ([]byte, error) {
	ps, ts, zs := speeds(speed)
	switch cmd {
	case "move":
		x, y := byte(3), byte(3)
		switch dir {
		case "up":
			y = 1
		case "down":
			y = 2
		case "left":
			x = 1
		case "right":
			x = 2
		case "upleft":
			x, y = 1, 1
		case "upright":
			x, y = 2, 1
		case "downleft":
			x, y = 1, 2
		case "downright":
			x, y = 2, 2
		default:
			return nil, errors.New("unknown direction")
		}
		return []byte{0x81, 0x01, 0x06, 0x01, ps, ts, x, y, 0xFF}, nil
	case "stop":
		return []byte{0x81, 0x01, 0x06, 0x01, ps, ts, 0x03, 0x03, 0xFF}, nil
	case "home":
		return []byte{0x81, 0x01, 0x06, 0x04, 0xFF}, nil
	case "zoomin":
		return []byte{0x81, 0x01, 0x04, 0x07, 0x20 | zs, 0xFF}, nil
	case "zoomout":
		return []byte{0x81, 0x01, 0x04, 0x07, 0x30 | zs, 0xFF}, nil
	case "zoomstop":
		return []byte{0x81, 0x01, 0x04, 0x07, 0x00, 0xFF}, nil
	case "preset", "save":
		if n < 1 || n > 100 {
			return nil, errors.New("preset must be 1 to 100")
		}
		op := byte(0x02)
		if cmd == "save" {
			op = 0x01
		}
		// Preset numbers on the camera start at 0; button 1 = preset 0.
		return []byte{0x81, 0x01, 0x04, 0x3F, op, byte(n - 1), 0xFF}, nil
	}
	return nil, errors.New("unknown command")
}

func ptzSend(cam PTZCam, visca []byte) error {
	if net.ParseIP(cam.IP) == nil {
		return fmt.Errorf("camera address %q is not an IP address", cam.IP)
	}
	port := cam.Port
	if port == 0 {
		port = ptzDefaultPort(cam.Proto)
	}
	addr := net.JoinHostPort(cam.IP, fmt.Sprint(port))
	switch cam.Proto {
	case "tcp":
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		_, err = c.Write(visca)
		return err
	case "udp":
		c, err := net.Dial("udp", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Write(visca)
		return err
	default: // sony VISCA over IP
		c, err := net.Dial("udp", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		ptzMu.Lock()
		seq, seen := ptzSeq[cam.ID]
		if !seen {
			// reset the camera's sequence counter first
			reset := []byte{0x02, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0x01}
			c.Write(reset)
			seq = 0
		}
		seq++
		ptzSeq[cam.ID] = seq
		ptzMu.Unlock()
		pkt := make([]byte, 8+len(visca))
		pkt[0], pkt[1] = 0x01, 0x00
		binary.BigEndian.PutUint16(pkt[2:], uint16(len(visca)))
		binary.BigEndian.PutUint32(pkt[4:], seq)
		copy(pkt[8:], visca)
		_, err = c.Write(pkt)
		return err
	}
}
