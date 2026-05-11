// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVirtualWebDAVFSServesLazyFiles(t *testing.T) {
	tree := newVirtualTree(time.Now())
	loads := 0
	tree.AddLazyFile("docs/all/hello.md", func(context.Context) ([]byte, time.Time, error) {
		loads++
		return []byte("# Hello\n"), time.Now(), nil
	}, time.Now())

	handler := newReadOnlyWebDAVHandler(newVirtualWebDAVFS(tree))
	req := httptest.NewRequest(http.MethodGet, "/docs/all/hello.md", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%q", resp.Code, resp.Body.String())
	}
	if resp.Body.String() != "# Hello\n" {
		t.Fatalf("GET body = %q", resp.Body.String())
	}
	if loads != 1 {
		t.Fatalf("loader calls = %d, want 1", loads)
	}

	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("second GET status = %d, want 200", resp.Code)
	}
	if loads != 1 {
		t.Fatalf("loader calls after second GET = %d, want 1", loads)
	}
}

func TestVirtualWebDAVFSRejectsWrites(t *testing.T) {
	tree := newVirtualTree(time.Now())
	tree.AddStaticFile("docs/all/hello.md", []byte("# Hello\n"), time.Now())
	handler := newReadOnlyWebDAVHandler(newVirtualWebDAVFS(tree))

	req := httptest.NewRequest(http.MethodPut, "/docs/all/new.md", strings.NewReader("no"))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT status = %d, want 403; body=%q", resp.Code, string(body))
	}
}
