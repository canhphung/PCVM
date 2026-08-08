package multiegg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ProcessSupervisor struct{ Log *Logger }

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
	markReady := func() { readyOnce.Do(func() { fmt.Fprintln(stdout, "[MULTIEGG] READY"); ready <- struct{}{} }) }
	patterns := make([]*regexp.Regexp, 0, len(spec.ReadyPatterns))
	for _, raw := range spec.ReadyPatterns {
		re, e := regexp.Compile(raw)
		if e != nil {
			_ = cmd.Process.Kill()
			return fmt.Errorf("ready pattern %q: %w", raw, e)
		}
		patterns = append(patterns, re)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(src io.Reader, dst io.Writer) {
		defer wg.Done()
		scan := bufio.NewScanner(src)
		scan.Buffer(make([]byte, 64*1024), 2<<20)
		for scan.Scan() {
			line := scan.Text()
			fmt.Fprintln(dst, line)
			for _, re := range patterns {
				if re.MatchString(line) {
					markReady()
					break
				}
			}
		}
	}
	go pump(outPipe, stdout)
	go pump(errPipe, stderr)
	if len(patterns) == 0 {
		go func() { <-time.After(spec.ReadyAfter); markReady() }()
	}
	go func() { _, _ = io.Copy(stdin, input) }()
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	var shutdownErr error
	readyTimeout := spec.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = 2 * time.Minute
	}
	readyTimer := time.NewTimer(readyTimeout)
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
		shutdownErr = fmt.Errorf("process did not become ready within %s", readyTimeout)
	}
	if spec.StopCommand != "" {
		_, _ = io.WriteString(stdin, strings.TrimSuffix(spec.StopCommand, "\n")+"\n")
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
