package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestWaitSignalWritesStdoutAndDiesWithShellStatus(t *testing.T) {
	if os.Getenv("GMUX_TEST_WAIT_SIGNAL") == "child" {
		defer noticeInterruptedWait(os.Stderr, "s", os.Stdout, false)()
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(syscall.SIGINT)
		time.Sleep(10 * time.Second)
		os.Exit(4)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestWaitSignalWritesStdoutAndDiesWithShellStatus", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "GMUX_TEST_WAIT_SIGNAL=child")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child survived or failed to run: %v", err)
	}
	status := exitErr.Sys().(syscall.WaitStatus)
	if !status.Signaled() && status.ExitStatus() != 130 {
		t.Fatalf("status=%v", status)
	}
	if stdout.String() != "[Wait interrupted; agent remains active]\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestWaitSignalQuietAndSecondSignalImmediate(t *testing.T) {
	oldDie, oldExit := dieFromSignal, exitImmediately
	first := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	dieFromSignal = func(sig os.Signal) { first <- sig }
	exitImmediately = func(code int) { exited <- code }
	t.Cleanup(func() { dieFromSignal, exitImmediately = oldDie, oldExit })

	var out bytes.Buffer
	stop := noticeInterruptedWait(os.Stderr, "s", &out, true)
	defer stop()
	p, _ := os.FindProcess(os.Getpid())
	_ = p.Signal(syscall.SIGINT)
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first signal did not reach death path")
	}
	if out.Len() != 0 {
		t.Fatalf("quiet signal output=%q", out.String())
	}
	_ = p.Signal(syscall.SIGTERM)
	select {
	case code := <-exited:
		if code != 143 {
			t.Fatalf("second signal exit=%d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal did not exit immediately")
	}
}
