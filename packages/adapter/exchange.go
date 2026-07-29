package adapter

import (
	"bytes"
	"fmt"
	"strings"
)

// Exchange is the adapter-neutral unit used by semantic conversation reads.
// A user message opens an exchange; every completed assistant response before
// the next user message is an iteration. Terminal is prose from the final
// assistant response only (never tool traffic or an earlier response).
type Exchange struct {
	// Ordinal is monotonic within the observed activity. Historical
	// projections use the conversation-branch ordinal.
	Ordinal    uint64 `json:"ordinal,omitempty"`
	User       string `json:"user"`
	Iterations int    `json:"iterations,omitempty"`
	Terminal   string `json:"terminal,omitempty"`
}

// ConversationExchangeRenderer reconstructs the active native conversation
// branch as visible exchanges. The returned slice is oldest first.
type ConversationExchangeRenderer interface {
	RenderConversationExchanges(ref string) ([]Exchange, error)
}

// ExchangeOutcome controls the terminal marker of an exchange report.
type ExchangeOutcome string

const (
	ExchangeSnapshot    ExchangeOutcome = "snapshot"
	ExchangeActive      ExchangeOutcome = "active"
	ExchangeCompleted   ExchangeOutcome = "completed"
	ExchangeInterrupted ExchangeOutcome = "interrupted"
	ExchangeFailed      ExchangeOutcome = "failed"
	ExchangeTimeout     ExchangeOutcome = "timeout"
	ExchangeWaitSignal  ExchangeOutcome = "wait_interrupted"
)

// ExchangeReport is one renderer input. Previous is omitted when unknown by
// setting PreviousKnown false. TerminalPartial labels Terminal honestly for a
// non-completed final exchange.
type ExchangeReport struct {
	Exchanges     []Exchange
	Previous      int
	PreviousKnown bool
	Outcome       ExchangeOutcome
	Diagnostic    string
	// UnscopedTerminal is compatibility prose from an older source frame that
	// carried no user-bounded exchanges. It is rendered explicitly, never lost
	// or attributed to conversation history.
	UnscopedTerminal  string
	TerminalPartial   bool
	TerminalTruncated bool
	TimeoutSeconds    int
	OmittedExchanges  int
	OmittedBytes      int
}

// RenderExchangeReport renders the single human document shared by logs, wait,
// and synchronous prompt. It deliberately appends one newline so it can be
// written directly to stdout.
func RenderExchangeReport(report ExchangeReport) []byte {
	if report.Outcome == ExchangeWaitSignal && len(report.Exchanges) == 0 {
		return []byte("[Wait interrupted; agent remains active]\n")
	}
	var parts []string
	if report.PreviousKnown && report.Previous > 0 {
		word := "exchanges"
		if report.Previous == 1 {
			word = "exchange"
		}
		parts = append(parts, fmt.Sprintf("[%d previous %s]", report.Previous, word))
	}
	if report.OmittedExchanges > 0 || report.OmittedBytes > 0 {
		parts = append(parts, fmt.Sprintf("[%d exchange(s) and %d bytes omitted from live report]", report.OmittedExchanges, report.OmittedBytes))
	}
	for i, ex := range report.Exchanges {
		parts = append(parts, "[USER]: "+ex.User)
		last := i == len(report.Exchanges)-1
		if !last {
			if ex.Iterations > 0 {
				parts = append(parts, workMarker(ex.Iterations))
			}
			continue
		}
		if ex.Iterations > 0 && (report.Outcome == ExchangeActive || report.Outcome == ExchangeTimeout || report.Outcome == ExchangeWaitSignal) {
			// Active terminal markers include the count, so do not repeat it.
		} else if ex.Iterations > 0 {
			parts = append(parts, workMarker(ex.Iterations))
		}
		if ex.Terminal != "" {
			parts = append(parts, agentLabel("", report)+ex.Terminal)
		}
		parts = appendTerminal(parts, report, ex)
	}
	if len(report.Exchanges) == 0 {
		if report.UnscopedTerminal != "" {
			parts = append(parts, agentLabel("compatibility", report)+report.UnscopedTerminal)
			parts = appendTerminal(parts, report, Exchange{Terminal: report.UnscopedTerminal})
		} else {
			parts = append(parts, "[No exchanges yet]")
			switch report.Outcome {
			case ExchangeActive, ExchangeInterrupted, ExchangeFailed, ExchangeTimeout, ExchangeWaitSignal:
				parts = appendTerminal(parts, report, Exchange{})
			}
		}
	}
	var b bytes.Buffer
	b.WriteString(strings.Join(parts, "\n\n"))
	b.WriteByte('\n')
	return b.Bytes()
}

func agentLabel(extra string, report ExchangeReport) string {
	parts := []string{"AGENT"}
	if extra != "" {
		parts = append(parts, extra)
	}
	if report.TerminalPartial {
		parts = append(parts, "partial")
	}
	if report.TerminalTruncated {
		parts = append(parts, "truncated")
	}
	return "[" + strings.Join(parts, ", ") + "]: "
}

func workMarker(n int) string {
	if n == 1 {
		return "[Agent worked for 1 iteration]"
	}
	return fmt.Sprintf("[Agent worked for %d iterations]", n)
}

func progress(n int) string {
	if n == 0 {
		return "no completed iterations yet..."
	}
	if n == 1 {
		return "1 iteration so far..."
	}
	return fmt.Sprintf("%d iterations so far...", n)
}

func appendTerminal(parts []string, report ExchangeReport, ex Exchange) []string {
	switch report.Outcome {
	case ExchangeActive:
		return append(parts, "[Agent active, "+progress(ex.Iterations)+"]")
	case ExchangeCompleted:
		if ex.Terminal == "" {
			return append(parts, "[Agent completed without a final response]")
		}
	case ExchangeInterrupted:
		return append(parts, "[Agent interrupted]")
	case ExchangeFailed:
		reason := report.Diagnostic
		if reason == "" {
			reason = "activity could not be completed"
		}
		return append(parts, "[Agent failed: "+reason+"]")
	case ExchangeTimeout:
		return append(parts, fmt.Sprintf("[Wait timed out after %ds; agent active, %s]", report.TimeoutSeconds, progress(ex.Iterations)))
	case ExchangeWaitSignal:
		if len(report.Exchanges) == 0 {
			return append(parts, "[Wait interrupted; agent remains active]")
		}
		return append(parts, "[Wait interrupted; agent remains active, "+progress(ex.Iterations)+"]")
	}
	return parts
}
