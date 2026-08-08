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
	patternsRaw := spec.ReadyPatterns
	if len(readiness.Patterns) > 0 {
		patternsRaw = readiness.Patterns
	}
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

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Directory
	cmd.Env = append(os.Environ(), spec.Environment...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
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
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ready := make(chan struct{}, 1)
	var readyOnce sync.Once
	markReady := func() { readyOnce.Do(func() { fmt.Fprintln(stdout, "[PCVM] READY"); ready <- struct{}{} }) }
	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(src io.Reader, dst io.Writer) {
		defer wg.Done()
		scan := bufio.NewScanner(src)
		scan.Buffer(make([]byte, 64*1024), 2<<20)
		for scan.Scan() {
			line := scan.Text()
			fmt.Fprintln(dst, line)
			if readiness.Mode == "regex" {
				for _, re := range patterns {
					if re.MatchString(line) {
						markReady()
						break
					}
				}
			}
		}
	}
	go pump(outPipe, stdout)
	go pump(errPipe, stderr)
	switch readiness.Mode {
	case "delay":
		go func() {
			timer := time.NewTimer(spec.ReadyAfter)
			defer timer.Stop()
			select {
			case <-timer.C:
				markReady()
			case <-ctx.Done():
			}
		}()
	case "tcp":
		go waitTCPReady(ctx, readiness.PortVariable, markReady)
	case "regex":
		if len(patterns) == 0 {
			go func() { time.Sleep(spec.ReadyAfter); markReady() }()
		}
	default:
		_ = cmd.Process.Kill()
		return fmt.Errorf("unsupported readiness mode %q", readiness.Mode)
	}

	control := spec.Control
	if control.Mode == "" {
		control = ControlSpec{Mode: "stdin", StopCommand: spec.StopCommand}
	}
	sender := commandSender(func(command string) error {
		_, err := io.WriteString(stdin, strings.TrimSuffix(command, "\n")+"\n")
		return err
	})
	switch control.Mode {
	case "stdin", "":
	case "source-rcon":
		sender = sourceRCONSender(control)
	case "telnet":
		sender = telnetSender(control)
	case "signal":
		sender = func(string) error { return fmt.Errorf("console commands are not supported by this provider") }
	default:
		_ = cmd.Process.Kill()
		return fmt.Errorf("unsupported control mode %q", control.Mode)
	}
	go relayConsoleInput(input, sender, stderr)

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
			wg.Wait()
			if err != nil {
				return fmt.Errorf("process exited: %w", err)
			}
			return nil
		case <-ctx.Done():
		case <-sigCh:
		}
	case err := <-done:
		wg.Wait()
		if err != nil {
			return fmt.Errorf("process exited before readiness: %w", err)
		}
		return fmt.Errorf("process exited before readiness")
	case <-ctx.Done():
	case <-sigCh:
	case <-readyTimer.C:
		shutdownErr = fmt.Errorf("process did not become ready within %s", spec.ReadyTimeout)
	}

	stopCommand := control.StopCommand
	if stopCommand == "" {
		stopCommand = spec.StopCommand
	}
	if stopCommand != "" {
		if err := sendWithRetry(sender, stopCommand); err != nil && s.Log != nil {
			s.Log.Printf("WARNING: graceful stop command failed: %v", err)
		}
	} else {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	_ = stdin.Close()
	timer := time.NewTimer(spec.StopTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		wg.Wait()
		if shutdownErr != nil {
			return shutdownErr
		}
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	case <-timer.C:
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		wg.Wait()
		if shutdownErr != nil {
			return shutdownErr
		}
		return err
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		err := <-done
		wg.Wait()
		if shutdownErr != nil {
			return shutdownErr
		}
		return err
	}
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

func relayConsoleInput(input io.Reader, sender commandSender, stderr io.Writer) {
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := sender(line); err != nil {
			fmt.Fprintf(stderr, "[PCVM] console input: %v\n", err)
		}
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
