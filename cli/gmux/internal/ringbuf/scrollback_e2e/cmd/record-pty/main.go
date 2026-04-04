// record-pty records raw PTY output from a TUI program into a binary file.
//
// It creates a real pseudo-terminal, launches the given command inside it,
// optionally types a prompt, waits for the response, then exits. The
// result is a byte-exact capture of everything the PTY master produced,
// including all ANSI escape sequences, cursor movement, and screen clears.
//
// These recordings serve as fixtures for the scrollback e2e tests. See
// the README.md in the scrollback_e2e directory for usage examples.
//
// Usage:
//
//	record-pty [flags] <output-file> <command> [args...]
//
// Flags:
//
//	-prompt string    Text to type into the TUI after startup (sent char-by-char).
//	                  A trailing \r is appended automatically to submit it.
//	-wait duration    How long to wait for a response after the prompt (default 90s).
//	-startup duration How long to wait for the TUI to initialize (default 3s).
//	-rows int         PTY row count (default 40).
//	-cols int         PTY column count (default 120).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
)

func main() {
	prompt := flag.String("prompt", "", "text to type into the TUI after startup")
	wait := flag.Duration("wait", 90*time.Second, "max time to wait for response after prompt")
	startup := flag.Duration("startup", 3*time.Second, "time to wait for TUI initialization")
	rows := flag.Uint("rows", 40, "PTY row count")
	cols := flag.Uint("cols", 120, "PTY column count")
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: record-pty [flags] <output-file> <command> [args...]\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	outPath := args[0]
	cmd := exec.Command(args[1], args[2:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(*rows),
		Cols: uint16(*cols),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start pty: %v\n", err)
		os.Exit(1)
	}
	defer ptmx.Close()

	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create output: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	// Read loop: write to file and track screen clears.
	var totalBytes atomic.Int64
	var clearCount atomic.Int32
	secondClear := make(chan struct{}, 1)
	ioDone := make(chan struct{})

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				f.Write(buf[:n])
				totalBytes.Add(int64(n))
				// Count ESC[2J screen clears.
				for i := 0; i <= n-4; i++ {
					if buf[i] == 0x1b && buf[i+1] == '[' && buf[i+2] == '2' && buf[i+3] == 'J' {
						c := clearCount.Add(1)
						fmt.Fprintf(os.Stderr, "  clear #%d at byte %d\n", c, totalBytes.Load())
						if c >= 2 {
							select {
							case secondClear <- struct{}{}:
							default:
							}
						}
					}
				}
			}
			if err != nil {
				break
			}
		}
		close(ioDone)
	}()

	// Wait for TUI to initialize.
	time.Sleep(*startup)
	fmt.Fprintf(os.Stderr, "TUI ready (%d bytes)\n", totalBytes.Load())

	// Type the prompt if provided.
	if *prompt != "" {
		for _, c := range []byte(*prompt) {
			ptmx.Write([]byte{c})
			time.Sleep(20 * time.Millisecond)
		}
		ptmx.Write([]byte{'\r'})
		fmt.Fprintf(os.Stderr, "sent prompt: %q\n", *prompt)

		// Wait for the second screen clear (pi redraws after the response
		// is complete) or timeout.
		select {
		case <-secondClear:
			fmt.Fprintf(os.Stderr, "second clear detected, capturing final frame...\n")
			time.Sleep(4 * time.Second)
		case <-time.After(*wait):
			fmt.Fprintf(os.Stderr, "timeout after %s\n", *wait)
		}
	}

	// Send Ctrl-C to exit.
	ptmx.Write([]byte{0x03})
	time.Sleep(1 * time.Second)

	// Wait for process.
	cmdDone := make(chan error, 1)
	go func() { cmdDone <- cmd.Wait() }()
	select {
	case <-cmdDone:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
	}

	select {
	case <-ioDone:
	case <-time.After(3 * time.Second):
	}

	stat, _ := f.Stat()
	fmt.Fprintf(os.Stderr, "recorded %d bytes (%d clears) to %s\n",
		stat.Size(), clearCount.Load(), outPath)
}
