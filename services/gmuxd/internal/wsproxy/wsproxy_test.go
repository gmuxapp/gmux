package wsproxy

import (
	"net/url"
	"testing"
)

func TestBrowserAttachQueryAllowlist(t *testing.T) {
	for name, query := range map[string]string{
		"browser":         "client=browser",
		"missing":         "",
		"other":           "client=browser&route=secret",
		"wrong value":     "client=1",
		"duplicate":       "client=browser&client=browser",
		"empty duplicate": "client=browser&client=",
	} {
		t.Run(name, func(t *testing.T) {
			if got := browserAttachQuery(mustParseQuery(t, query)); got != (name == "browser") {
				t.Fatalf("browserAttachQuery(%q) = %v", query, got)
			}
		})
	}
}

func mustParseQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return q
}
