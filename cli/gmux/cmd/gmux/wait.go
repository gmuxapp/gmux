package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// Exit codes from cmdWait. Distinct codes let scripts dispatch on the
// reason a wait ended without parsing strings.
//
//   - waitExitOK (0): the session's turn closed (idle wait — including
//     a one-shot command completing or a shell exiting at its prompt)
//     or its output matched --for-text/--for-regex
//   - waitExitDied (2): session crashed / was killed / exited with its
//     turn still open, or exited before its output matched
//   - waitExitTimeout (3): --timeout elapsed
//
// Any other usage / network error returns 1, matching the rest of the
// CLI.
const (
	waitExitOK      = 0
	waitExitDied    = 2
	waitExitTimeout = 3
)

// cmdWait implements `gmux wait <id> [--timeout N] [--for-text S |
// --for-regex P]`.
//
// The wait itself happens server-side: gmuxd already subscribes to
// per-session events for its own bookkeeping, so we just hand it the
// session id and block on the HTTP response. That keeps the CLI free
// of SSE-parsing logic and ensures the idle-detection rules (how turn
// state resolves, what counts as "died") live in one place. Output conditions equally belong server-side: the bytes
// live in the daemon's scrollback tee, and matching there can't miss
// output the way client-side scrollback polling could.
//
// Local sessions only: the daemon's wait handler resolves the session
// against its local store and consults the adapter allowlist; remote
// peer sessions are out of scope until peer subscriptions stream
// Status events back to the hub.
func cmdWait(ref string, timeoutSecs int, forText, forRegex string) int {
	sess, err := resolveSession(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	if sess.Peer != "" {
		// Use the bare shortID here: the message already names the peer
		// separately, so displayID's "shortID@peer" would just repeat it.
		fmt.Fprintf(os.Stderr, "gmux: wait is only supported for local sessions (%s is on peer %q)\n",
			shortID(sess.ID), sess.Peer)
		return 1
	}

	query := url.Values{}
	if timeoutSecs > 0 {
		query.Set("timeout", strconv.Itoa(timeoutSecs))
	}
	if forText != "" {
		query.Set("for_text", forText)
	}
	if forRegex != "" {
		query.Set("for_regex", forRegex)
	}
	endpoint := gmuxdBaseURL() + "/v1/sessions/" + url.PathEscape(sess.ID) + "/wait"
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	client := gmuxdClient()
	// The default 5s client timeout would cut off any wait that
	// outlasts a turn on a slow agent. With no client-side timeout
	// the only deadline is the optional server-side --timeout.
	client.Timeout = 0

	// No request body; pass http.NoBody so we don't advertise a
	// content-type for bytes that don't exist.
	resp, err := client.Post(endpoint, "", http.NoBody)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux:", err)
		return 1
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Body is { ok: true, data: { reason: "idle" | "matched" | "died" } }
		var env struct {
			Data struct {
				Reason string `json:"reason"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			fmt.Fprintln(os.Stderr, "gmux: decode wait response:", err)
			return 1
		}
		switch env.Data.Reason {
		case "idle", "matched":
			return waitExitOK
		case "died":
			if forText != "" || forRegex != "" {
				fmt.Fprintf(os.Stderr, "gmux: session %s exited before its output matched\n", displayID(sess))
			} else {
				fmt.Fprintf(os.Stderr, "gmux: session %s died before becoming idle\n", displayID(sess))
			}
			return waitExitDied
		default:
			fmt.Fprintf(os.Stderr, "gmux: unexpected wait reason %q\n", env.Data.Reason)
			return 1
		}
	case http.StatusRequestTimeout:
		fmt.Fprintf(os.Stderr, "gmux: wait timed out after %ds\n", timeoutSecs)
		return waitExitTimeout
	case http.StatusUnprocessableEntity:
		// Current daemons only send 422 on the send --wait path
		// (input_no_submit); older daemons also rejected sessions
		// without an idle signal here. Surface the daemon's message
		// either way — it explains what to change.
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "gmux: wait not supported for this session: %s\n",
			extractMessage(body))
		return 1
	case http.StatusNotFound:
		// Means the session id is unknown to gmuxd entirely.
		fmt.Fprintf(os.Stderr, "gmux: session %s not found\n", displayID(sess))
		return 1
	default:
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "gmux: wait failed: %s: %s\n", resp.Status, extractMessage(body))
		return 1
	}
}

// extractMessage pulls the .error.message field out of gmuxd's
// standard error envelope, falling back to the raw body if the
// shape doesn't match.
func extractMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return string(body)
}
