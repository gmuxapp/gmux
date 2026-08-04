package main

import (
	"fmt"
	"os"
)

// cmdRead is the explicit/bulk consumption tool. --family includes the root
// and every transitive launch descendant, which is the migration path for
// retained unread child piles created before waits consumed results.
func cmdRead(refs []string, familyRef string) int {
	var targets []cliSession
	if familyRef != "" {
		sessions, err := fetchSessions()
		if err != nil {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return 1
		}
		root, err := matchSession(sessions, familyRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gmux:", err)
			return 1
		}
		targets = sessionFamily(sessions, root)
	} else {
		seen := map[string]bool{}
		for _, ref := range refs {
			sess, err := resolveSession(ref)
			if err != nil {
				fmt.Fprintln(os.Stderr, "gmux:", err)
				return 1
			}
			key := displayID(sess)
			if !seen[key] {
				seen[key] = true
				targets = append(targets, sess)
			}
		}
	}

	failures := 0
	for _, sess := range targets {
		if err := consumeSession(sess); err != nil {
			fmt.Fprintf(os.Stderr, "gmux: read %s: %v\n", displayID(sess), err)
			failures++
		}
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "gmux: marked %d of %d sessions read\n", len(targets)-failures, len(targets))
		return 1
	}
	fmt.Printf("marked %d session(s) read\n", len(targets))
	return 0
}

func sessionFamily(sessions []cliSession, root cliSession) []cliSession {
	included := map[string]bool{displayID(root): true, root.ID: true}
	out := []cliSession{root}
	for changed := true; changed; {
		changed = false
		for _, sess := range sessions {
			key := displayID(sess)
			if included[key] || included[sess.ID] || sess.ParentSessionID == "" || !included[sess.ParentSessionID] {
				continue
			}
			included[key], included[sess.ID] = true, true
			out = append(out, sess)
			changed = true
		}
	}
	return out
}
