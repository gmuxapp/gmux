package adapter

import "testing"

func TestRenderExchangeReportGoldens(t *testing.T) {
	tests := []struct {
		name   string
		report ExchangeReport
		want   string
	}{
		{"empty", ExchangeReport{}, "[No exchanges yet]\n"},
		{"singular previous and iterations", ExchangeReport{PreviousKnown: true, Previous: 1, Outcome: ExchangeCompleted, Exchanges: []Exchange{{User: "hello\n  world", Iterations: 1, Terminal: "done\nexactly"}}}, "[1 previous exchange]\n\n[USER]: hello\n  world\n\n[Agent worked for 1 iteration]\n\n[AGENT]: done\nexactly\n"},
		{"multi exchange zero marker omitted", ExchangeReport{Outcome: ExchangeCompleted, Exchanges: []Exchange{{User: "first"}, {User: "second", Iterations: 2, Terminal: "answer"}}}, "[USER]: first\n\n[USER]: second\n\n[Agent worked for 2 iterations]\n\n[AGENT]: answer\n"},
		{"tool only", ExchangeReport{Outcome: ExchangeCompleted, Exchanges: []Exchange{{User: "act", Iterations: 1}}}, "[USER]: act\n\n[Agent worked for 1 iteration]\n\n[Agent completed without a final response]\n"},
		{"active zero", ExchangeReport{Outcome: ExchangeActive, Exchanges: []Exchange{{User: "wait"}}}, "[USER]: wait\n\n[Agent active, no completed iterations yet...]\n"},
		{"partial failure", ExchangeReport{Outcome: ExchangeFailed, Diagnostic: "provider failed", TerminalPartial: true, Exchanges: []Exchange{{User: "work", Iterations: 3, Terminal: "half"}}}, "[USER]: work\n\n[Agent worked for 3 iterations]\n\n[AGENT, partial]: half\n\n[Agent failed: provider failed]\n"},
		{"timeout", ExchangeReport{Outcome: ExchangeTimeout, TimeoutSeconds: 7, Exchanges: []Exchange{{User: "work", Iterations: 2}}}, "[USER]: work\n\n[Wait timed out after 7s; agent active, 2 iterations so far...]\n"},
		{"generic signal has no invented exchange claim", ExchangeReport{Outcome: ExchangeWaitSignal}, "[Wait interrupted; agent remains active]\n"},
		{"signal with known frame count", ExchangeReport{Outcome: ExchangeWaitSignal, Exchanges: []Exchange{{User: "work", Iterations: 2}}}, "[USER]: work\n\n[Wait interrupted; agent remains active, 2 iterations so far...]\n"},
		{"old frame output", ExchangeReport{Outcome: ExchangeCompleted, UnscopedTerminal: "answer", TerminalTruncated: true}, "[AGENT, compatibility, truncated]: answer\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(RenderExchangeReport(tt.report)); got != tt.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}
