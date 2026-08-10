package pcvm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ProcessSupervisor struct{ Log *Logger }

type synchronizedProcessStdin struct {
	state   sync.Mutex
	writeMu sync.Mutex
	writer  io.WriteCloser
	closed  bool
}

func (w *synchronizedProcessStdin) writeCommand(command string) error {
	return w.write(strings.TrimRight(command, "\r\n") + "\n")
}

func (w *synchronizedProcessStdin) writeRaw(data string) error {
	return w.write(data)
}

func (w *synchronizedProcessStdin) write(data string) error {
	// Serialize complete commands, but never hold the state lock across a pipe
	// write. A child that stops reading stdin can fill the pipe indefinitely;
	// Close must still be able to close the descriptor and unblock that write so
	// supervisor escalation is never held hostage by console input.
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.state.Lock()
	if w.closed {
		w.state.Unlock()
		return os.ErrClosed
	}
	writer := w.writer
	w.state.Unlock()
	_, err := io.WriteString(writer, data)
	return err
}

func (w *synchronizedProcessStdin) Close() error {
	w.state.Lock()
	if w.closed {
		w.state.Unlock()
		return nil
	}
	w.closed = true
	writer := w.writer
	w.state.Unlock()
	return writer.Close()
}

func compileReadyPatterns(rawPatterns []string) ([]*regexp.Regexp, error) {
	patterns := make([]*regexp.Regexp, 0, len(rawPatterns))
	for _, raw := range rawPatterns {
		re, err := regexp.Compile(raw)
		if err != nil {
			return nil, fmt.Errorf("ready pattern %q: %w", raw, err)
		}
		patterns = append(patterns, re)
	}
	return patterns, nil
}

func (s ProcessSupervisor) Run(ctx context.Context, spec ProcessSpec, input io.Reader, stdout, stderr io.Writer) error {
	if len(spec.Command) == 0 {
		return fmt.Errorf("empty process command")
	}
	if spec.StopTimeout == 0 {
		spec.StopTimeout = 30 * time.Second
	}
	if spec.ReadyAfter == 0 {
		spec.ReadyAfter = 5 * time.Second
	}
	if spec.ReadyTimeout == 0 {
		spec.ReadyTimeout = 2 * time.Minute
	}
	readiness := spec.Readiness
	patternsRaw := readiness.Patterns
	if readiness.Mode == "" {
		if len(patternsRaw) > 0 {
			readiness.Mode = "regex"
		} else {
			readiness.Mode = "delay"
		}
	}
	patterns, err := compileReadyPatterns(patternsRaw)
	if err != nil {
		return err
	}
	if readiness.Mode != "delay" && readiness.Mode != "tcp" && readiness.Mode != "regex" {
		return fmt.Errorf("unsupported readiness mode %q", readiness.Mode)
	}
	control := spec.Control
	if control.Mode == "" {
		control = ControlSpec{Mode: "signal"}
	}
	if control.Mode != "stdin" && control.Mode != "source-rcon" && control.Mode != "telnet" && control.Mode != "signal" && control.Mode != "qmp" {
		return fmt.Errorf("unsupported control mode %q", control.Mode)
	}
	if control.Mode == "qmp" && control.SocketPath == "" {
		return fmt.Errorf("QMP control requires a socket path")
	}
	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if spec.BeforeStart != nil {
		cleanup, hookErr := spec.BeforeStart(runContext)
		if hookErr != nil {
			return fmt.Errorf("prepare process: %w", hookErr)
		}
		if cleanup != nil {
			defer func() {
				if cleanupErr := cleanup(); cleanupErr != nil && s.Log != nil {
					s.Log.Printf("WARNING: process cleanup failed: %v", cleanupErr)
				}
			}()
		}
	}

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Directory
	cmd.Env = append(os.Environ(), spec.Environment...)
	configureProcessGroup(cmd)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdin := &synchronizedProcessStdin{writer: stdinPipe}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var killOnce sync.Once
	killGroup := func() { killOnce.Do(func() { _ = killProcessGroup(cmd) }) }
	defer killGroup()
	defer stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ready := make(chan struct{}, 1)
	var readyOnce sync.Once
	markReady := func() {
		first := false
		readyOnce.Do(func() {
			first = true
			fmt.Fprintln(stdout, "[PCVM] READY")
			ready <- struct{}{}
		})
		if !first && spec.RepeatReadiness {
			fmt.Fprintln(stdout, "[PCVM] READY")
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(src io.Reader, dst io.Writer) {
		defer wg.Done()
		matchReady := func(line string) {
			if readiness.Mode == "regex" {
				for _, re := range patterns {
					if re.MatchString(line) {
						markReady()
						break
					}
				}
			}
		}
		pumpProcessOutput(src, dst, matchReady)
	}
	go pump(outPipe, stdout)
	go pump(errPipe, stderr)
	pumpsDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(pumpsDone)
	}()
	drainPumps := func() {
		select {
		case <-pumpsDone:
			return
		case <-time.After(2 * time.Second):
			_ = outPipe.Close()
			_ = errPipe.Close()
		}
		select {
		case <-pumpsDone:
		case <-time.After(500 * time.Millisecond):
			if s.Log != nil {
				s.Log.Printf("WARNING: process output pumps did not stop promptly")
			}
		}
	}
	switch readiness.Mode {
	case "delay":
		go func() {
			timer := time.NewTimer(spec.ReadyAfter)
			defer timer.Stop()
			select {
			case <-timer.C:
				markReady()
			case <-runContext.Done():
			}
		}()
	case "tcp":
		go waitTCPReady(runContext, readiness.PortVariable, markReady)
	case "regex":
		if len(patterns) == 0 {
			go func() {
				timer := time.NewTimer(spec.ReadyAfter)
				defer timer.Stop()
				select {
				case <-timer.C:
					markReady()
				case <-runContext.Done():
				}
			}()
		}
	}

	consoleSender := commandSender(stdin.writeCommand)
	stopSender := consoleSender
	switch control.Mode {
	case "stdin", "":
	case "source-rcon":
		consoleSender = sourceRCONSender(control)
		stopSender = consoleSender
	case "telnet":
		consoleSender = telnetSender(control)
		stopSender = consoleSender
	case "signal":
		consoleSender = func(string) error { return fmt.Errorf("console commands are not supported by this provider") }
		stopSender = nil
	case "qmp":
		// QEMU's serial console is a byte stream, not a line-based command
		// protocol. Preserve whitespace and blank lines exactly as received.
		consoleSender = stdin.writeRaw
		stopSender = func(string) error { return qmpPowerdown(control.SocketPath) }
	}
	stopConsoleRelay := startConsoleRelay(runContext, input, consoleSender, stderr, control.Mode == "qmp", s.Log)
	defer stopConsoleRelay()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	var shutdownErr error
	readyTimer := time.NewTimer(spec.ReadyTimeout)
	defer readyTimer.Stop()
	select {
	case <-ready:
		select {
		case err := <-done:
			killGroup()
			drainPumps()
			if err != nil {
				return fmt.Errorf("process exited: %w", err)
			}
			return nil
		case <-ctx.Done():
		case <-sigCh:
		}
	case err := <-done:
		killGroup()
		drainPumps()
		if err != nil {
			return fmt.Errorf("process exited before readiness: %w", err)
		}
		return fmt.Errorf("process exited before readiness")
	case <-ctx.Done():
	case <-sigCh:
	case <-readyTimer.C:
		shutdownErr = fmt.Errorf("process did not become ready within %s", spec.ReadyTimeout)
	}
	// Stop and join the console reader before initiating graceful shutdown so a
	// user command cannot race the provider-specific stop command on stdin.
	consoleStopped := stopConsoleRelay()
	if !consoleStopped {
		// Closing the descriptor is the only portable way to interrupt a console
		// sender already blocked in a full child pipe. Graceful stdin control is
		// no longer possible, so the provider falls through to signal escalation.
		_ = stdin.Close()
		if control.Mode == "stdin" || control.Mode == "" {
			stopSender = nil
		}
	}

	stopCommand := control.StopCommand
	if control.Mode == "qmp" {
		if err := sendWithRetry(stopSender, "system_powerdown"); err != nil && s.Log != nil {
			s.Log.Printf("WARNING: graceful VM powerdown failed: %v", err)
		}
	} else if control.Mode == "signal" {
		_ = signalProcessGroup(cmd, os.Interrupt)
	} else if stopCommand != "" && stopSender != nil {
		if err := sendWithRetry(stopSender, stopCommand); err != nil && s.Log != nil {
			s.Log.Printf("WARNING: graceful stop command failed: %v", err)
		}
	} else {
		_ = signalProcessGroup(cmd, os.Interrupt)
	}
	_ = stdin.Close()
	timer := time.NewTimer(spec.StopTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		killGroup()
		drainPumps()
		if shutdownErr != nil {
			return shutdownErr
		}
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	case <-timer.C:
	}
	_ = signalProcessGroup(cmd, syscall.SIGTERM)
	select {
	case err := <-done:
		killGroup()
		drainPumps()
		if shutdownErr != nil {
			return shutdownErr
		}
		return err
	case <-time.After(5 * time.Second):
		killGroup()
		select {
		case err := <-done:
			drainPumps()
			if shutdownErr != nil {
				return shutdownErr
			}
			return err
		case <-time.After(5 * time.Second):
			drainPumps()
			if shutdownErr != nil {
				return shutdownErr
			}
			return fmt.Errorf("process could not be reaped after SIGKILL")
		}
	}
}

func pumpRawOutput(src io.Reader, dst io.Writer, inspect func(string)) {
	pumpProcessOutput(src, dst, inspect)
}

// pumpProcessOutput never stops draining merely because an upstream emits an
// abnormally long line. Inspection stays bounded while bytes are forwarded
// unchanged, preventing a child from deadlocking on a full stdout/stderr pipe.
func pumpProcessOutput(src io.Reader, dst io.Writer, inspect func(string)) {
	const (
		inspectLimit   = 2 << 20
		inspectOverlap = 64 << 10
	)
	buffer := make([]byte, 32*1024)
	pending := make([]byte, 0, 64*1024)
	for {
		count, err := src.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			_, _ = dst.Write(chunk)
			for len(chunk) > 0 {
				index := bytesIndexByte(chunk, '\n')
				if index < 0 {
					pending = append(pending, chunk...)
					break
				}
				pending = append(pending, chunk[:index]...)
				inspect(strings.TrimSuffix(string(pending), "\r"))
				pending = pending[:0]
				chunk = chunk[index+1:]
			}
			if len(pending) >= inspectLimit {
				inspect(string(pending))
				copy(pending[:inspectOverlap], pending[len(pending)-inspectOverlap:])
				pending = pending[:inspectOverlap]
			}
		}
		if err != nil {
			if len(pending) > 0 {
				inspect(string(pending))
			}
			return
		}
	}
}

func bytesIndexByte(value []byte, target byte) int {
	for index, candidate := range value {
		if candidate == target {
			return index
		}
	}
	return -1
}

func waitTCPReady(ctx context.Context, rawPort string, markReady func()) {
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
		if err == nil {
			connection.Close()
			markReady()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func startConsoleRelay(parent context.Context, input io.Reader, sender commandSender, stderr io.Writer, raw bool, log *Logger) func() bool {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		relayConsoleInput(ctx, input, sender, stderr, raw)
	}()
	var once sync.Once
	stopped := false
	return func() bool {
		once.Do(func() {
			cancel()
			select {
			case <-done:
				stopped = true
				return
			default:
			}
			// Production console input is os.Stdin and therefore closable. Closing
			// it is the only portable way to interrupt a blocked Reader.
			if closer, ok := input.(io.ReadCloser); ok {
				_ = closer.Close()
			}
			select {
			case <-done:
				stopped = true
			case <-time.After(time.Second):
				if log != nil {
					log.Printf("WARNING: console input relay did not stop promptly")
				}
			}
		})
		return stopped
	}
}

func relayConsoleInput(ctx context.Context, input io.Reader, sender commandSender, stderr io.Writer, raw bool) {
	if raw {
		buffer := make([]byte, 32*1024)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			count, err := input.Read(buffer)
			if count > 0 {
				if sendErr := sender(string(buffer[:count])); sendErr != nil && !errors.Is(sendErr, os.ErrClosed) {
					fmt.Fprintf(stderr, "[PCVM] console input: %v\n", sendErr)
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					fmt.Fprintf(stderr, "[PCVM] console input read: %v\n", err)
				}
				return
			}
		}
	}
	reader := bufio.NewScanner(input)
	reader.Buffer(make([]byte, 64*1024), 2<<20)
	for reader.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := strings.TrimSuffix(reader.Text(), "\r")
		if sendErr := sender(line); sendErr != nil && !errors.Is(sendErr, os.ErrClosed) {
			fmt.Fprintf(stderr, "[PCVM] console input: %v\n", sendErr)
		}
	}
	if err := reader.Err(); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "[PCVM] console input read: %v\n", err)
	}
}

func sendWithRetry(sender commandSender, command string) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := sender(command); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return last
}
