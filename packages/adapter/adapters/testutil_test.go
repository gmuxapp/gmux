//go:build integration

// Shared test infrastructure for adapter integration tests.
// PTY helpers, event collection, and output parsing utilities.

package adapters

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// testEvent is a timestamped event for logging.
type testEvent struct {
	Time   time.Time
	Source string // "pty", "fs", "proc", "sidecar"
	Kind   string
	Detail string
	Size   int
}

func (e testEvent) String() string {
	if e.Size > 0 {
		return fmt.Sprintf("[%-7s %-12s] (%d bytes) %s", e.Source, e.Kind, e.Size, e.Detail)
	}
	return fmt.Sprintf("[%-7s %-12s] %s", e.Source, e.Kind, e.Detail)
}

// eventCollector collects timestamped events from multiple goroutines.
type eventCollector struct {
	mu     sync.Mutex
	events []testEvent
	start  time.Time
}

func newEventCollector() *eventCollector {
	return &eventCollector{start: time.Now()}
}

func (c *eventCollector) add(source, kind, detail string, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, testEvent{
		Time:   time.Now(),
		Source: source,
		Kind:   kind,
		Detail: detail,
		Size:   size,
	})
}

func (c *eventCollector) snapshot() []testEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]testEvent, len(c.events))
	copy(cp, c.events)
	return cp
}

func (c *eventCollector) dump(t *testing.T) {
	t.Helper()
	events := c.snapshot()
	t.Logf("--- %d events ---", len(events))
	for _, ev := range events {
		rel := ev.Time.Sub(c.start).Truncate(time.Millisecond)
		t.Logf("  [%12s] %s", rel, ev)
	}
}

func (c *eventCollector) eventsOfKind(source, kind string) []testEvent {
	var result []testEvent
	for _, ev := range c.snapshot() {
		if ev.Source == source && ev.Kind == kind {
			result = append(result, ev)
		}
	}
	return result
}

// --- PTY helpers ---

type ptyProcess struct {
	ptmx *os.File
	pid  int
}

func startProcess(t *testing.T, args []string, cwd string) *ptyProcess {
	t.Helper()
	binary, err := exec.LookPath(args[0])
	if err != nil {
		t.Skipf("%s not found in PATH, skipping", args[0])
	}

	ptmx, pts, err := openTestPTY()
	if err != nil {
		t.Fatalf("openpty: %v", err)
	}

	if ws, err := getTestWinSize(os.Stdout.Fd()); err == nil {
		setTestWinSize(ptmx.Fd(), ws)
	} else {
		setTestWinSize(ptmx.Fd(), &testWinSize{Rows: 24, Cols: 80})
	}

	env := os.Environ()
	env = append(env, "TERM=xterm-256color")

	attr := &syscall.ProcAttr{
		Dir:   cwd,
		Env:   env,
		Files: []uintptr{pts.Fd(), pts.Fd(), pts.Fd()},
		Sys: &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    0,
		},
	}

	pid, _, err := syscall.StartProcess(binary, args, attr)
	pts.Close()
	if err != nil {
		ptmx.Close()
		t.Fatalf("start process: %v", err)
	}

	return &ptyProcess{ptmx: ptmx, pid: pid}
}

func (p *ptyProcess) write(data string) {
	p.ptmx.Write([]byte(data))
}

func (p *ptyProcess) signal(sig syscall.Signal) {
	syscall.Kill(-p.pid, sig)
}

// --- output helpers ---

func summarizeOutput(data []byte) string {
	s := stripTestANSI(string(data))
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

func stripTestANSI(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[':
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
				i = j
			case ']':
				j := i + 2
				for j < len(s) {
					if s[j] == 0x07 || (s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
						if s[j] == 0x07 {
							j++
						} else {
							j += 2
						}
						break
					}
					j++
				}
				i = j
			default:
				i += 2
			}
		} else {
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

// --- low-level PTY ---

func openTestPTY() (ptmx *os.File, pts *os.File, err error) {
	p, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	ptmx = os.NewFile(uintptr(p), "/dev/ptmx")

	unlock := 0
	if err := unix.IoctlSetPointerInt(p, unix.TIOCSPTLCK, unlock); err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}

	sn, err := unix.IoctlGetInt(p, unix.TIOCGPTN)
	if err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("get pty number: %w", err)
	}

	sname := fmt.Sprintf("/dev/pts/%d", sn)
	s, err := unix.Open(sname, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("open slave %s: %w", sname, err)
	}
	pts = os.NewFile(uintptr(s), sname)

	return ptmx, pts, nil
}

type testWinSize struct {
	Rows, Cols, XPixel, YPixel uint16
}

func getTestWinSize(fd uintptr) (*testWinSize, error) {
	ws := &testWinSize{}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(ws)))
	if errno != 0 {
		return nil, errno
	}
	return ws, nil
}

func setTestWinSize(fd uintptr, ws *testWinSize) {
	syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(ws)))
}
