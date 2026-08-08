package pcvm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func TestSupervisorTCPReadiness(t *testing.T) {
	if os.Getenv("PCVM_TCP_HELPER") == "1" {
		listener, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PCVM_TCP_PORT"))
		if err != nil {
			os.Exit(4)
		}
		defer listener.Close()
		scan := bufio.NewScanner(os.Stdin)
		for scan.Scan() {
			if scan.Text() == "stop" {
				os.Exit(0)
			}
		}
		os.Exit(3)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var output lockedBuffer
	done := make(chan error, 1)
	s := ProcessSupervisor{Log: NewLogger(io.Discard)}
	go func() {
		done <- s.Run(ctx, ProcessSpec{
			Command:     []string{os.Args[0], "-test.run=TestSupervisorTCPReadiness"},
			Environment: []string{"PCVM_TCP_HELPER=1", "PCVM_TCP_PORT=" + strconv.Itoa(port)},
			Readiness:   ReadinessSpec{Mode: "tcp", PortVariable: strconv.Itoa(port)},
			Control:     ControlSpec{Mode: "stdin", StopCommand: "stop"}, StopTimeout: 3 * time.Second,
		}, strings.NewReader(""), &output, &output)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(output.String(), "[PCVM] READY") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "[PCVM] READY") {
		t.Fatalf("not ready: %s", output.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TCP-ready process did not stop")
	}
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func TestSupervisorReadinessAndGracefulStop(t *testing.T) {
	if os.Getenv("PCVM_HELPER") == "1" {
		fmt.Println("helper booted")
		scan := bufio.NewScanner(os.Stdin)
		for scan.Scan() {
			if scan.Text() == "stop" {
				fmt.Println("helper stopped")
				os.Exit(0)
			}
		}
		os.Exit(3)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output lockedBuffer
	done := make(chan error, 1)
	s := ProcessSupervisor{Log: NewLogger(io.Discard)}
	go func() {
		done <- s.Run(ctx, ProcessSpec{Command: []string{os.Args[0], "-test.run=TestSupervisorReadinessAndGracefulStop"}, Environment: []string{"PCVM_HELPER=1"}, ReadyPatterns: []string{"helper booted"}, StopCommand: "stop", StopTimeout: 3 * time.Second}, strings.NewReader(""), &output, &output)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(output.String(), "[PCVM] READY") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "[PCVM] READY") {
		t.Fatalf("not ready: %s", output.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("graceful stop timed out")
	}
	if !strings.Contains(output.String(), "helper stopped") {
		t.Fatalf("stop command not forwarded: %s", output.String())
	}
}

func TestPumpRawOutputForwardsPromptWithoutNewline(t *testing.T) {
	var output bytes.Buffer
	var inspected []string
	pumpRawOutput(strings.NewReader("pcvm@guest:~$ echo ok\r\n[PCVM-GUEST] READY\nnext> "), &output, func(line string) {
		inspected = append(inspected, line)
	})
	if output.String() != "pcvm@guest:~$ echo ok\r\n[PCVM-GUEST] READY\nnext> " {
		t.Fatalf("raw console output changed: %q", output.String())
	}
	if len(inspected) != 3 || inspected[0] != "pcvm@guest:~$ echo ok" || inspected[1] != "[PCVM-GUEST] READY" || inspected[2] != "next> " {
		t.Fatalf("inspected lines=%q", inspected)
	}
}
