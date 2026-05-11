// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fsview

import (
	"fmt"
	"strings"
	"time"
)

type ChatMessage struct {
	CreateTime time.Time
	SenderName string
	SenderID   string
	MsgType    string
	Content    string
}

func RenderChatDayMarkdown(chatName, date string, messages []ChatMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s / %s\n\n", strings.TrimSpace(chatName), date)
	for _, msg := range messages {
		sender := strings.TrimSpace(msg.SenderName)
		if sender == "" {
			sender = strings.TrimSpace(msg.SenderID)
		}
		if sender == "" {
			sender = "unknown"
		}
		t := msg.CreateTime
		timeLabel := "unknown"
		if !t.IsZero() {
			timeLabel = t.Local().Format("15:04")
		}
		fmt.Fprintf(&b, "## %s %s\n\n", timeLabel, sender)
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			content = fmt.Sprintf("[%s]", emptyDefault(msg.MsgType, "message"))
		}
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	return b.String()
}

func emptyDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
