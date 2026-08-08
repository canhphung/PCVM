package pcvm

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

type qmpEnvelope struct {
	QMP    json.RawMessage `json:"QMP"`
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
	Event string `json:"event"`
}

func qmpPowerdown(socket string) error {
	connection, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect QMP: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	decoder, encoder := json.NewDecoder(connection), json.NewEncoder(connection)
	var greeting qmpEnvelope
	if err := decoder.Decode(&greeting); err != nil {
		return fmt.Errorf("read QMP greeting: %w", err)
	}
	if len(greeting.QMP) == 0 {
		return fmt.Errorf("invalid QMP greeting")
	}
	if err := qmpExecute(decoder, encoder, "qmp_capabilities"); err != nil {
		return err
	}
	if err := qmpExecute(decoder, encoder, "query-status"); err != nil {
		return err
	}
	return qmpExecute(decoder, encoder, "system_powerdown")
}

func qmpExecute(decoder *json.Decoder, encoder *json.Encoder, command string) error {
	if err := encoder.Encode(map[string]string{"execute": command}); err != nil {
		return fmt.Errorf("send QMP %s: %w", command, err)
	}
	for {
		var response qmpEnvelope
		if err := decoder.Decode(&response); err != nil {
			return fmt.Errorf("read QMP %s: %w", command, err)
		}
		if response.Event != "" {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("QMP %s failed (%s): %s", command, response.Error.Class, response.Error.Desc)
		}
		if response.Return != nil {
			return nil
		}
	}
}
