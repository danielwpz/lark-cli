// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fsview

import "testing"

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"  Project / Plan: v1  ", "Project _ Plan_ v1"},
		{"..", "untitled"},
		{"", "untitled"},
		{"a\x00b", "a_b"},
	}
	for _, tt := range tests {
		if got := SanitizeName(tt.in); got != tt.want {
			t.Fatalf("SanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStableID(t *testing.T) {
	a := StableID("docx", "token")
	b := StableID("docx", "token")
	c := StableID("doc", "token")
	if a != b {
		t.Fatalf("StableID not stable: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("StableID should include all parts")
	}
	if len(a) != 8 {
		t.Fatalf("StableID length = %d, want 8", len(a))
	}
}
