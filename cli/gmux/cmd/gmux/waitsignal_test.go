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

type blockingSignalWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w blockingSignalWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}

func TestWaitSignalWritesStdoutAndDiesWithShellStatus(t *testing.T) {
	if os.Getenv("GMUX_TEST_WAIT_SIGNAL") == "child" {
		defer noticeInterruptedWait(os.Stdout, false)()
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

func TestCommandWaitSignalUsesNeutralLanguage(t *testing.T) {
	oldDie := dieFromSignal
	dieFromSignal = func(os.Signal) {}
	t.Cleanup(func() { dieFromSignal = oldDie })

	var out bytes.Buffer
	stop, observed := observeInterruptedWait(&out, false, false)
	p, _ := os.FindProcess(os.Getpid())
	_ = p.Signal(syscall.SIGINT)
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("signal was not observed")
	}
	stop()
	if got := out.String(); got != "[Wait interrupted; session activity continues]\n" {
		t.Fatalf("notice=%q", got)
	}
}

func TestWaitSignalStopJoinsNoticeWriteBeforePublishing(t *testing.T) {
	oldDie := dieFromSignal
	dieFromSignal = func(os.Signal) {}
	t.Cleanup(func() { dieFromSignal = oldDie })

	writer := blockingSignalWriter{started: make(chan struct{}), release: make(chan struct{})}
	stop, observed := observeInterruptedWait(writer, false, true)
	p, _ := os.FindProcess(os.Getpid())
	_ = p.Signal(syscall.SIGINT)
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("notice write did not start")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while notice write was blocked")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-observed:
		t.Fatal("signal published before notice write completed")
	default:
	}
	close(writer.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not join observer")
	}
	select {
	case sig := <-observed:
		if sig != syscall.SIGINT {
			t.Fatalf("observed %v", sig)
		}
	default:
		t.Fatal("completed notice was not published")
	}
}

func TestWaitSignalSecondSignalBypassesBlockedNotice(t *testing.T) {
	oldDie, oldExit := dieFromSignal, exitImmediately
	dieFromSignal = func(os.Signal) {}
	exited := make(chan int, 1)
	exitImmediately = func(code int) { exited <- code }
	t.Cleanup(func() { dieFromSignal, exitImmediately = oldDie, oldExit })

	writer := blockingSignalWriter{started: make(chan struct{}), release: make(chan struct{})}
	stop, _ := observeInterruptedWait(writer, false, true)
	p, _ := os.FindProcess(os.Getpid())
	_ = p.Signal(syscall.SIGINT)
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("notice write did not block")
	}
	_ = p.Signal(syscall.SIGTERM)
	select {
	case code := <-exited:
		if code != 143 {
			t.Fatalf("second signal exit=%d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal waited for blocked stdout")
	}
	close(writer.release)
	stop()
}

func TestWaitSignalQuietAndSecondSignalImmediate(t *testing.T) {
	oldDie, oldExit := dieFromSignal, exitImmediately
	first := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	dieFromSignal = func(sig os.Signal) { first <- sig }
	exitImmediately = func(code int) { exited <- code }
	t.Cleanup(func() { dieFromSignal, exitImmediately = oldDie, oldExit })

	var out bytes.Buffer
	stop := noticeInterruptedWait(&out, true)
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
