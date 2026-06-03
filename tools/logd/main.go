package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const listenAddr = "127.0.0.1:9876"

func main() {
	logfile := flag.String("logfile", "", "path to log file (required)")
	flag.Parse()

	if *logfile == "" {
		fmt.Fprintln(os.Stderr, "logd: -logfile is required")
		os.Exit(1)
	}

	f, err := os.OpenFile(*logfile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logd: open logfile: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	out := io.MultiWriter(os.Stderr, f)

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logd: listen %s: %v\n", listenAddr, err)
		os.Exit(1)
	}

	writeEntry(out, "logd", fmt.Sprintf("listening on %s  log → %s\n", listenAddr, *logfile))

	mux := http.NewServeMux()
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		source := r.Header.Get("X-Log-Source")
		if source == "" {
			source = "backend"
		}
		writeEntry(out, source, string(body))
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Handler: mux}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		writeEntry(out, "logd", fmt.Sprintf("serve error: %v\n", err))
		os.Exit(1)
	}
}

// writeEntry writes a formatted log entry: "HH:MM:SS.mmm [source  ] INFO   msg"
// where source is padded to 8 characters. msg is written as-is; a trailing
// newline is added if msg does not already end with one.
func writeEntry(w io.Writer, source, msg string) {
	ts := time.Now().Format("15:04:05.000")
	padded := fmt.Sprintf("%-8s", source)
	msg = strings.TrimRight(msg, "\n")
	fmt.Fprintf(w, "%s [%s] INFO   %s\n", ts, padded, msg)
}
