package requestid

import (
	"context"
	"strings"
	"testing"
)

func TestNewIsValidAndUnique(t *testing.T) {
	a, b := New(), New()
	if !Valid(a) || !Valid(b) {
		t.Fatalf("generated IDs must be valid: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("generated IDs must be unique, got %q twice", a)
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"0192f0c2-7b8e-7c1a-9d2e-1234567890ab", true},
		{"abc123", true},
		{"cf-ray:8a1b.2_c", true},
		{"", false},
		{strings.Repeat("a", 129), false},
		{"has space", false},
		{"inject\nnewline", false},
		{"quote\"", false},
		{"<script>", false},
		{"ünicode", false},
	}
	for _, tt := range tests {
		if got := Valid(tt.id); got != tt.want {
			t.Errorf("Valid(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	if got := From(context.Background()); got != "" {
		t.Fatalf("empty context should yield empty ID, got %q", got)
	}
	ctx := With(context.Background(), "req-1")
	if got := From(ctx); got != "req-1" {
		t.Fatalf("From() = %q, want req-1", got)
	}
}
