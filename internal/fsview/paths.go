// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fsview

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxNameRunes = 80

// StableID returns a short stable identifier for path suffixes.
func StableID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:4])
}

// SanitizeName keeps names readable while making them safe as a single path segment.
func SanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "untitled"
	}

	var b strings.Builder
	lastUnderscore := false
	runeCount := 0
	for _, r := range name {
		if runeCount >= maxNameRunes {
			break
		}
		replace := r == '/' || r == '\\' || r == ':' || r == 0 || unicode.IsControl(r)
		if replace {
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
				runeCount++
			}
			continue
		}
		if r == utf8.RuneError {
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
				runeCount++
			}
			continue
		}
		b.WriteRune(r)
		lastUnderscore = false
		runeCount++
	}

	out := strings.Trim(strings.TrimSpace(b.String()), ".")
	out = strings.Trim(out, "_")
	if out == "" || out == "." || out == ".." {
		return "untitled"
	}
	return out
}

func DocumentFileName(title, id string) string {
	return SanitizeName(title) + "__" + id + ".md"
}

func ChatDirName(kind, name, id string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "unknown"
	}
	return kind + "__" + SanitizeName(name) + "__" + id
}
