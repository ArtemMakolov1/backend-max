package api

import "testing"

func TestParseMediaRange(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		raw        string
		size       int64
		start, end int64
	}{
		"closed":  {raw: "bytes=2-5", size: 10, start: 2, end: 5},
		"open":    {raw: "bytes=7-", size: 10, start: 7, end: 9},
		"suffix":  {raw: "bytes=-3", size: 10, start: 7, end: 9},
		"clamped": {raw: "bytes=8-99", size: 10, start: 8, end: 9},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseMediaRange(test.raw, test.size)
			if err != nil {
				t.Fatal(err)
			}
			if got.start != test.start || got.end != test.end {
				t.Fatalf("range = %+v, want %d-%d", got, test.start, test.end)
			}
		})
	}
}

func TestParseMediaRangeRejectsUnsupportedRanges(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"items=0-1", "bytes=", "bytes=0-1,3-4", "bytes=10-", "bytes=5-2", "bytes=-0"} {
		if _, err := parseMediaRange(raw, 10); err == nil {
			t.Fatalf("invalid range %q was accepted", raw)
		}
	}
}
