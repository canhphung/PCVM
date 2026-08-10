//go:build !windows

package pcvm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSupervisorKillsDescendantProcessGroup(t *testing.T) {
	if os.Getenv("PCVM_DESCENDANT_HELPER") == "1" {
		signalIgnoreInterruptAndTerminate()
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("PCVM_GROUP_HELPER") == "1" {
		child := exec.Command(os.Args[0], "-test.run=TestSupervisorKillsDescendantProcessGroup")
		child.Env = append(os.Environ(), "PCVM_DESCENDANT_HELPER=1")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(7)
		}
		if err := os.WriteFile(os.Getenv("PCVM_DESCENDANT_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(8)
		}
		fmt.Println("group helper ready")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if scanner.Text() == "stop" {
				os.Exit(0)
			}
		}
		os.Exit(9)
	}

	pidFile := t.TempDir() + "/descendant.pid"
	ctx, cancel := context.WithCancel(context.Background())
	var output lockedBuffer
	done := make(chan error, 1)
	go func() {
		done <- (ProcessSupervisor{Log: NewLogger(io.Discard)}).Run(ctx, ProcessSpec{
			Command:     []string{os.Args[0], "-test.run=TestSupervisorKillsDescendantProcessGroup"},
			Environment: []string{"PCVM_GROUP_HELPER=1", "PCVM_DESCENDANT_PID_FILE=" + pidFile},
			Readiness:   ReadinessSpec{Mode: "regex", Patterns: []string{"group helper ready"}}, Control: ControlSpec{Mode: "stdin", StopCommand: "stop"}, StopTimeout: 2 * time.Second,
		}, strings.NewReader(""), &output, &output)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(output.String(), "[PCVM] READY") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(output.String(), "[PCVM] READY") {
		cancel()
		t.Fatalf("not ready: %s", output.String())
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(rawPID))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("supervisor did not return")
	}
	deadline = time.Now().Add(3 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived supervisor cleanup", pid)
	}
}
