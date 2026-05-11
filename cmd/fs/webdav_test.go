// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/larksuite/cli/internal/cmdutil"
	"github.com/larksuite/cli/internal/core"
	"github.com/larksuite/cli/internal/vfs"
	"golang.org/x/net/webdav"
)

func TestNewCmdWebDAVParsesFlags(t *testing.T) {
	f, _, _, _ := cmdutil.TestFactory(t, &core.CliConfig{
		AppID:     "app",
		AppSecret: "secret",
		Brand:     core.BrandFeishu,
	})

	var got *WebDAVOptions
	cmd := NewCmdWebDAV(f, func(opts *WebDAVOptions) error {
		got = opts
		return nil
	})
	cmd.SetArgs([]string{"--cache-dir", "cache", "--addr", "127.0.0.1:9876"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got == nil {
		t.Fatal("run hook was not called")
	}
	if got.CacheDir != "cache" || got.Addr != "127.0.0.1:9876" {
		t.Fatalf("CacheDir/Addr = %q/%q, want cache/127.0.0.1:9876", got.CacheDir, got.Addr)
	}
}

func TestValidateWebDAVOptionsRejectsNonLoopback(t *testing.T) {
	err := validateWebDAVOptions(&WebDAVOptions{CacheDir: "cache", Addr: "0.0.0.0:8765"})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestReadOnlyWebDAVHandlerServesReadsAndRejectsWrites(t *testing.T) {
	root := t.TempDir()
	if err := vfs.MkdirAll(filepath.Join(root, ".meta"), 0700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := vfs.WriteFile(filepath.Join(root, ".meta", "snapshot.json"), []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	handler := newReadOnlyWebDAVHandler(readOnlyWebDAVFS{FileSystem: webdav.Dir(root)})

	getReq := httptest.NewRequest(http.MethodGet, "/.meta/snapshot.json", nil)
	getResp := httptest.NewRecorder()
	handler.ServeHTTP(getResp, getReq)
	if getResp.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%q", getResp.Code, getResp.Body.String())
	}
	if strings.TrimSpace(getResp.Body.String()) != `{"ok":true}` {
		t.Fatalf("GET body = %q", getResp.Body.String())
	}

	propfindReq := httptest.NewRequest("PROPFIND", "/", nil)
	propfindReq.Header.Set("Depth", "1")
	propfindResp := httptest.NewRecorder()
	handler.ServeHTTP(propfindResp, propfindReq)
	if propfindResp.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND status = %d, want 207; body=%q", propfindResp.Code, propfindResp.Body.String())
	}

	putReq := httptest.NewRequest(http.MethodPut, "/new.txt", bytes.NewBufferString("nope"))
	putResp := httptest.NewRecorder()
	handler.ServeHTTP(putResp, putReq)
	if putResp.Code != http.StatusForbidden {
		t.Fatalf("PUT status = %d, want 403; body=%q", putResp.Code, putResp.Body.String())
	}
}
