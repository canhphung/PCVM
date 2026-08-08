package pcvm

import (
	"bufio"
	"net"
	"strconv"
	"testing"
)

func TestSourceRCONSender(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	commands := make(chan string, 1)
	serverErr := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer connection.Close()
		id, packetType, password, err := readRCONPacket(connection)
		if err != nil {
			serverErr <- err
			return
		}
		if id != 1 || packetType != 3 || password != "secret" {
			serverErr <- &controlFixtureError{"invalid auth packet"}
			return
		}
		if err := writeRCONPacket(connection, 1, 2, ""); err != nil {
			serverErr <- err
			return
		}
		id, packetType, command, err := readRCONPacket(connection)
		if err != nil {
			serverErr <- err
			return
		}
		if id != 2 || packetType != 2 {
			serverErr <- &controlFixtureError{"invalid command packet"}
			return
		}
		commands <- command
		serverErr <- writeRCONPacket(connection, 2, 0, "ok")
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := sendSourceRCON("127.0.0.1", port, "secret", "quit"); err != nil {
		t.Fatal(err)
	}
	if command := <-commands; command != "quit" {
		t.Fatalf("command=%q", command)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

type controlFixtureError struct{ message string }

func (e *controlFixtureError) Error() string { return e.message }

func TestTelnetSender(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	lines := make(chan []string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			lines <- []string{"ERROR: " + acceptErr.Error()}
			return
		}
		defer connection.Close()
		scanner := bufio.NewScanner(connection)
		var received []string
		for scanner.Scan() {
			received = append(received, scanner.Text())
			if len(received) == 2 {
				break
			}
		}
		lines <- received
	}()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := telnetSender(ControlSpec{PortVariable: port, Password: "secret"})("shutdown"); err != nil {
		t.Fatal(err)
	}
	received := <-lines
	if len(received) != 2 || received[0] != "secret" || received[1] != "shutdown" {
		t.Fatalf("lines=%v", received)
	}
}
