// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fsview

import (
	"strings"
	"testing"
	"time"
)

func TestRenderChatDayMarkdown(t *testing.T) {
	ts := time.Date(2026, 5, 9, 10, 3, 0, 0, time.Local)
	got := RenderChatDayMarkdown("Project", "2026-05-09", []ChatMessage{{
		CreateTime: ts,
		SenderName: "Alice",
		MsgType:    "text",
		Content:    "hello",
	}})
	for _, want := range []string{"# Project / 2026-05-09", "## 10:03 Alice", "hello"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered markdown missing %q:\n%s", want, got)
		}
	}
}
