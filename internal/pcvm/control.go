package pcvm

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type commandSender func(string) error

func sourceRCONSender(control ControlSpec) commandSender {
	return func(command string) error {
		return sendSourceRCON("127.0.0.1", control.PortVariable, control.Password, command)
	}
}

func sendSourceRCON(host, port, password, command string) error {
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("invalid RCON port")
	}
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writeRCONPacket(connection, 1, 3, password); err != nil {
		return err
	}
	authenticated := false
	for i := 0; i < 2; i++ {
		id, _, _, err := readRCONPacket(connection)
		if err != nil {
			return err
		}
		if id == -1 {
			return fmt.Errorf("RCON authentication failed")
		}
		if id == 1 {
			authenticated = true
			break
		}
	}
	if !authenticated {
		return fmt.Errorf("RCON authentication response missing")
	}
	if err := writeRCONPacket(connection, 2, 2, command); err != nil {
		return err
	}
	_, _, _, err = readRCONPacket(connection)
	return err
}

func writeRCONPacket(writer io.Writer, id, packetType int32, body string) error {
	payload := make([]byte, 4+4+len(body)+2)
	binary.LittleEndian.PutUint32(payload[0:4], uint32(id))
	binary.LittleEndian.PutUint32(payload[4:8], uint32(packetType))
	copy(payload[8:], body)
	packet := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(packet[:4], uint32(len(payload)))
	copy(packet[4:], payload)
	_, err := writer.Write(packet)
	return err
}

func readRCONPacket(reader io.Reader) (int32, int32, string, error) {
	var length int32
	if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
		return 0, 0, "", err
	}
	if length < 10 || length > 4<<20 {
		return 0, 0, "", fmt.Errorf("invalid RCON packet length %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, 0, "", err
	}
	id := int32(binary.LittleEndian.Uint32(payload[0:4]))
	packetType := int32(binary.LittleEndian.Uint32(payload[4:8]))
	body := strings.TrimRight(string(payload[8:]), "\x00")
	return id, packetType, body, nil
}

func telnetSender(control ControlSpec) commandSender {
	return func(command string) error {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", control.PortVariable), 3*time.Second)
		if err != nil {
			return err
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		if control.Password != "" {
			if _, err := io.WriteString(connection, control.Password+"\n"); err != nil {
				return err
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, err = io.WriteString(connection, strings.TrimSpace(command)+"\n")
		return err
	}
}
