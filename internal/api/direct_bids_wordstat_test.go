package api

import (
	"testing"
)

func TestParseNullableDirectBidCeilingDistinguishesSetRemoveAndMissing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		raw       string
		want      int64
		wantNil   bool
		wantError bool
	}{
		{name: "set", raw: "1200", want: 1200},
		{name: "remove", raw: "null", wantNil: true},
		{name: "missing", wantError: true},
		{name: "zero", raw: "0", wantError: true},
		{name: "negative", raw: "-1", wantError: true},
		{name: "fraction", raw: "1.5", wantError: true},
		{name: "string", raw: `"1200"`, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value, err := parseNullableDirectBidCeiling([]byte(tt.raw))
			if tt.wantError {
				if err == nil {
					t.Fatalf("value = %v, want validation error", value)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantNil {
				if value != nil {
					t.Fatalf("value = %v, want nil", *value)
				}
				return
			}
			if value == nil || *value != tt.want {
				t.Fatalf("value = %v, want %d", value, tt.want)
			}
		})
	}
}
