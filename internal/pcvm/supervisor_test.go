package pcvm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
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

type blockingConsoleReader struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockingConsoleReader() *blockingConsoleReader {
	return &blockingConsoleReader{started: make(chan struct{}), closed: make(chan struct{})}
}

func (r *blockingConsoleReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.closed
	return 0, os.ErrClosed
}

func (r *blockingConsoleReader) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}

type bufferWriteCloser struct{ bytes.Buffer }

func (w *bufferWriteCloser) Close() error { return nil }

type blockingWriteCloser struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.closed
	return 0, os.ErrClosed
}

func (w *blockingWriteCloser) Close() error {
	select {
	case <-w.closed:
	default:
		close(w.closed)
	}
	return nil
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
		done <- s.Run(ctx, ProcessSpec{Command: []string{os.Args[0], "-test.run=TestSupervisorReadinessAndGracefulStop"}, Environment: []string{"PCVM_HELPER=1"}, Readiness: ReadinessSpec{Mode: "regex", Patterns: []string{"helper booted"}}, Control: ControlSpec{Mode: "stdin", StopCommand: "stop"}, StopTimeout: 3 * time.Second}, strings.NewReader(""), &output, &output)
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

func TestConsoleRelayPreservesBlankLinesWhitespaceAndRawSerial(t *testing.T) {
	var commands []string
	relayConsoleInput(context.Background(), strings.NewReader("\n  status  \r\n\n"), func(value string) error {
		commands = append(commands, value)
		return nil
	}, io.Discard, false)
	want := []string{"", "  status  ", ""}
	if fmt.Sprint(commands) != fmt.Sprint(want) {
		t.Fatalf("commands=%q want=%q", commands, want)
	}

	rawInput := "\n  sudo -i  \r\n\n"
	var raw strings.Builder
	relayConsoleInput(context.Background(), strings.NewReader(rawInput), func(value string) error {
		_, _ = raw.WriteString(value)
		return nil
	}, io.Discard, true)
	if raw.String() != rawInput {
		t.Fatalf("raw serial input changed: got %q want %q", raw.String(), rawInput)
	}
}

func TestConsoleRelayStopCancelsAndJoinsBlockedReader(t *testing.T) {
	reader := newBlockingConsoleReader()
	stop := startConsoleRelay(context.Background(), reader, func(string) error { return nil }, io.Discard, true, NewLogger(io.Discard))
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("console relay did not start")
	}
	started := time.Now()
	stop()
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("console relay did not join promptly after cancellation")
	}
}

func TestSynchronizedProcessStdinKeepsWritesAtomic(t *testing.T) {
	output := &bufferWriteCloser{}
	stdin := &synchronizedProcessStdin{writer: output}
	var wg sync.WaitGroup
	for index := range 100 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if err := stdin.writeCommand(fmt.Sprintf("command-%03d", index)); err != nil {
				t.Errorf("write command: %v", err)
			}
		}(index)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 100 {
		t.Fatalf("got %d complete commands", len(lines))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "command-") || len(line) != len("command-000") {
			t.Fatalf("interleaved command write %q", line)
		}
	}
}

func TestSynchronizedProcessStdinCloseInterruptsBlockedWrite(t *testing.T) {
	pipe := newBlockingWriteCloser()
	stdin := &synchronizedProcessStdin{writer: pipe}
	writeDone := make(chan error, 1)
	go func() { writeDone <- stdin.writeRaw(strings.Repeat("x", 2<<20)) }()
	select {
	case <-pipe.started:
	case <-time.After(time.Second):
		t.Fatal("stdin write did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- stdin.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close blocked writer: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close waited on the blocked pipe write")
	}
	select {
	case err := <-writeDone:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("blocked write error = %v, want os.ErrClosed", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("closing stdin did not interrupt the blocked write")
	}
}

func TestPumpOutputDrainsAndInspectsOversizedLine(t *testing.T) {
	input := strings.Repeat("a", 3<<20) + "[PCVM-LONG-LINE-READY]" + strings.Repeat("b", 3<<20) + "\n"
	var output bytes.Buffer
	found := false
	pumpRawOutput(strings.NewReader(input), &output, func(window string) {
		if strings.Contains(window, "[PCVM-LONG-LINE-READY]") {
			found = true
		}
	})
	if output.String() != input {
		t.Fatalf("forwarded %d bytes; want %d", output.Len(), len(input))
	}
	if !found {
		t.Fatal("readiness text in an oversized line was never inspected")
	}
}

func TestSupervisorBeforeStartCleanupRunsOnStartFailure(t *testing.T) {
	prepared, cleaned := false, false
	spec := ProcessSpec{
		Command: []string{filepath.Join(t.TempDir(), "missing-command")},
		BeforeStart: func(context.Context) (func() error, error) {
			prepared = true
			return func() error { cleaned = true; return nil }, nil
		},
	}
	err := (ProcessSupervisor{Log: NewLogger(io.Discard)}).Run(context.Background(), spec, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("missing command unexpectedly started")
	}
	if !prepared || !cleaned {
		t.Fatalf("prepared=%v cleaned=%v", prepared, cleaned)
	}
}
