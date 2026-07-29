package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// fetchScrollbackQuiet feeds the best-effort terminal excerpt attached to an
// admission failure. The primary error is already known, so this helper emits
// no secondary diagnostics.
func fetchScrollbackQuiet(sess cliSession, lines int) []byte {
	client := gmuxdClient()
	resp, err := client.Get(fmt.Sprintf("%s/v1/sessions/%s/scrollback?tail=%d", gmuxdBaseURL(), sess.ID, lines))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	return data
}

func terminalTailExcerpt(sess cliSession, lines int) []string {
	data := fetchScrollbackQuiet(sess, lines)
	if len(data) == 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(stripANSI(data)), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, strings.TrimRight(line, " "))
	}
	if len(out) > lines {
		out = out[len(out)-lines:]
	}
	return out
}
