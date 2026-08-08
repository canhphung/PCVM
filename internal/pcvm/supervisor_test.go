package pcvm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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
