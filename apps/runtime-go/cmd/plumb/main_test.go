package main

import (
	"io"
	"slices"
	"testing"
)

func TestPermute(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			// The form the usage text documents, which Go's flag package would
			// otherwise treat as two positional arguments.
			name: "flag after positional",
			in:   []string{"/repo", "--json"},
			want: []string{"--json", "/repo"},
		},
		{
			name: "value flag after positional keeps its value",
			in:   []string{"/repo", "--db", "out.db"},
			want: []string{"--db", "out.db", "/repo"},
		},
		{
			name: "equals form needs no lookahead",
			in:   []string{"/repo", "--db=out.db"},
			want: []string{"--db=out.db", "/repo"},
		},
		{
			name: "boolean flag never consumes the next argument",
			in:   []string{"--json", "/repo"},
			want: []string{"--json", "/repo"},
		},
		{
			name: "already ordered is unchanged",
			in:   []string{"--commits", "5", "/repo"},
			want: []string{"--commits", "5", "/repo"},
		},
		{
			name: "mixed",
			in:   []string{"/repo", "--commits", "5", "--json"},
			want: []string{"--commits", "5", "--json", "/repo"},
		},
		{
			name: "no arguments",
			in:   nil,
			want: nil,
		},
		{
			// A value flag in final position must not read past the end.
			name: "dangling value flag",
			in:   []string{"/repo", "--db"},
			want: []string{"--db", "/repo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := permute(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("permute(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTakesValue(t *testing.T) {
	tests := []struct {
		flag string
		want bool
	}{
		{"--db", true},
		{"-db", true},
		{"--commits", true},
		{"--json", false},
		{"--unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			if got := takesValue(tt.flag); got != tt.want {
				t.Errorf("takesValue(%q) = %v, want %v", tt.flag, got, tt.want)
			}
		})
	}
}

func TestRunRejectsBadInvocations(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"frobnicate", "/tmp"}},
		{"two positionals", []string{"verify", "/tmp", "/tmp"}},
		{"no repository", []string{"verify"}},
		{"index without db", []string{"index", "/tmp"}},
		{"missing directory", []string{"verify", "/nonexistent-plumb-fixture"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _ := run(tt.args, io.Discard)
			if code != exitError {
				t.Errorf("run(%v) = %d, want %d", tt.args, code, exitError)
			}
		})
	}
}

func TestIndexRejectsJSON(t *testing.T) {
	// Accepting a flag and ignoring it is worse than refusing it: the caller
	// asked for JSON and would have received a human report with exit 0.
	code, err := run([]string{"index", "/tmp", "--db", "/tmp/x.db", "--json"}, io.Discard)
	if code != exitError {
		t.Errorf("exit = %d, want %d (err=%v)", code, exitError, err)
	}
}
