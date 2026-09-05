package mounter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type CommandRunner struct{}

func (CommandRunner) Start(_ context.Context, argv, environment []string) (Process, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty process argv")
	}
	command := exec.Command(argv[0], argv[1:]...)
	commandEnvironment, err := safeCommandEnvironment(environment)
	if err != nil {
		return nil, err
	}
	command.Env = commandEnvironment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open child null device: %w", err)
	}
	defer null.Close()
	command.Stdin, command.Stdout, command.Stderr = null, null, null
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &commandProcess{command: command}, nil
}

func (CommandRunner) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty process argv")
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=/nonexistent"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open command null device: %w", err)
	}
	defer null.Close()
	command.Stdin, command.Stdout, command.Stderr = null, null, null
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		reapOrphanedProcessGroup(command.Process.Pid)
		return ctx.Err()
	}
}

func safeCommandEnvironment(overrides []string) ([]string, error) {
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=/nonexistent"}
	if len(overrides) == 0 {
		return environment, nil
	}
	if len(overrides) != 1 || !strings.HasPrefix(overrides[0], "CURL_CA_BUNDLE=/") || strings.ContainsAny(overrides[0], "\x00\r\n") {
		return nil, fmt.Errorf("untrusted child environment override")
	}
	return append(environment, overrides[0]), nil
}

type commandProcess struct{ command *exec.Cmd }

func (p *commandProcess) Wait() error { return p.command.Wait() }

func (p *commandProcess) Signal(signal os.Signal) error {
	systemSignal, ok := signal.(syscall.Signal)
	if !ok || p.command.Process == nil {
		return fmt.Errorf("unsupported process signal")
	}
	return syscall.Kill(-p.command.Process.Pid, systemSignal)
}
