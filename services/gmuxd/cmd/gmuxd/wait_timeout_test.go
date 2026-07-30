package main

import (
	"math"
	"strconv"
	"testing"
	"time"
)

func TestParseTimeoutDurationBounds(t *testing.T) {
	tests := []struct {
		name string
		unit time.Duration
	}{
		{name: "milliseconds", unit: time.Millisecond},
		{name: "seconds", unit: time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			max := int64(math.MaxInt64) / int64(tt.unit)
			got, err := parseTimeoutDuration(strconv.FormatInt(max, 10), "invalid timeout", tt.unit)
			if err != nil {
				t.Fatalf("exact safe maximum rejected: %v", err)
			}
			if want := time.Duration(max) * tt.unit; got != want {
				t.Fatalf("duration = %v, want %v", got, want)
			}

			if _, err := parseTimeoutDuration(strconv.FormatInt(max+1, 10), "invalid timeout", tt.unit); err == nil {
				t.Fatal("maximum plus one was accepted")
			}
			if _, err := parseTimeoutDuration("9223372036854775808", "invalid timeout", tt.unit); err == nil {
				t.Fatal("signed 64-bit parse overflow was accepted")
			}
		})
	}
}
