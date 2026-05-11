// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"path/filepath"
	"testing"

	"github.com/larksuite/cli/internal/cmdutil"
	"github.com/larksuite/cli/internal/core"
)

func TestNewCmdSyncParsesFlags(t *testing.T) {
	f, _, _, _ := cmdutil.TestFactory(t, &core.CliConfig{
		AppID:      "app",
		AppSecret:  "secret",
		Brand:      core.BrandFeishu,
		UserOpenId: "ou_self",
	})

	var got *SyncOptions
	cmd := NewCmdSync(f, func(opts *SyncOptions) error {
		got = opts
		return nil
	})
	cmd.SetArgs([]string{
		"--im",
		"--output-dir", "out",
		"--im-active-days", "5",
		"--im-history-days", "9",
		"--docs-folder-token", "fld_a,fld_b",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got == nil {
		t.Fatal("run hook was not called")
	}
	if !got.IM || got.Docs {
		t.Fatalf("IM/Docs flags = %v/%v, want true/false", got.IM, got.Docs)
	}
	if got.IMActiveDays != 5 || got.IMHistoryDays != 9 {
		t.Fatalf("IM days = %d/%d, want 5/9", got.IMActiveDays, got.IMHistoryDays)
	}
	if len(got.DocsFolderTokens) != 2 || got.DocsFolderTokens[0] != "fld_a" || got.DocsFolderTokens[1] != "fld_b" {
		t.Fatalf("DocsFolderTokens = %#v", got.DocsFolderTokens)
	}
	if got.As != core.AsUser {
		t.Fatalf("As = %q, want user", got.As)
	}
}

func TestNewCmdMountParsesFlags(t *testing.T) {
	f, _, _, _ := cmdutil.TestFactory(t, &core.CliConfig{
		AppID:      "app",
		AppSecret:  "secret",
		Brand:      core.BrandFeishu,
		UserOpenId: "ou_self",
	})

	var got *MountOptions
	cmd := NewCmdMount(f, func(opts *MountOptions) error {
		got = opts
		return nil
	})
	cmd.SetArgs([]string{
		"mnt",
		"--cache-dir", "cache",
		"--backend", "webdav",
		"--refresh",
		"--im",
		"--im-active-days", "5",
		"--im-history-days", "9",
		"--docs-prefetch", "metadata",
		"--im-prefetch", "metadata",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got == nil {
		t.Fatal("run hook was not called")
	}
	if got.MountPoint != "mnt" || got.CacheDir != "cache" {
		t.Fatalf("mount/cache = %q/%q, want mnt/cache", got.MountPoint, got.CacheDir)
	}
	if got.Backend != "webdav" {
		t.Fatalf("Backend = %q, want webdav", got.Backend)
	}
	if !got.Refresh || !got.ReadOnly {
		t.Fatalf("Refresh/ReadOnly = %v/%v, want true/true", got.Refresh, got.ReadOnly)
	}
	if !got.IM || got.Docs {
		t.Fatalf("IM/Docs flags = %v/%v, want true/false", got.IM, got.Docs)
	}
	if got.IMActiveDays != 5 || got.IMHistoryDays != 9 {
		t.Fatalf("IM days = %d/%d, want 5/9", got.IMActiveDays, got.IMHistoryDays)
	}
	if got.DocsPrefetch != "metadata" || got.IMPrefetch != "metadata" {
		t.Fatalf("prefetch = %q/%q, want metadata/metadata", got.DocsPrefetch, got.IMPrefetch)
	}
	if got.As != core.AsUser {
		t.Fatalf("As = %q, want user", got.As)
	}
}

func TestResolveOutputDirRelative(t *testing.T) {
	dir := t.TempDir()
	cmdutil.TestChdir(t, dir)

	got, err := resolveOutputDir("out")
	if err != nil {
		t.Fatalf("resolveOutputDir() error = %v", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	want := filepath.Join(realDir, "out")
	if got != want {
		t.Fatalf("resolveOutputDir() = %q, want %q", got, want)
	}
}

func TestValidateMountOptionsRejectsInvalidBackend(t *testing.T) {
	err := validateMountOptions(&MountOptions{
		MountPoint:         "mnt",
		CacheDir:           "cache",
		Backend:            "bad",
		ReadOnly:           true,
		DocsPageLimit:      1,
		IMActiveDays:       3,
		IMHistoryDays:      7,
		IMActivePageLimit:  40,
		IMMessagePageLimit: 1,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateMountOptionsRejectsNativeBackendsUntilVirtualized(t *testing.T) {
	err := validateMountOptions(&MountOptions{
		MountPoint:         "mnt",
		CacheDir:           "cache",
		Backend:            "fskit",
		ReadOnly:           true,
		DocsPrefetch:       prefetchAll,
		DocsPrefetchLimit:  1,
		IMPrefetch:         prefetchAll,
		DocsPageLimit:      1,
		IMActiveDays:       3,
		IMHistoryDays:      7,
		IMActivePageLimit:  40,
		IMMessagePageLimit: 1,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateMountOptionsRejectsWritable(t *testing.T) {
	err := validateMountOptions(&MountOptions{
		MountPoint:         "mnt",
		CacheDir:           "cache",
		Backend:            "auto",
		ReadOnly:           false,
		DocsPageLimit:      1,
		IMActiveDays:       3,
		IMHistoryDays:      7,
		IMActivePageLimit:  40,
		IMMessagePageLimit: 1,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateSyncOptionsRejectsInvalidWindows(t *testing.T) {
	err := validateSyncOptions(&SyncOptions{
		OutputDir:          "out",
		DocsPageLimit:      1,
		IMActiveDays:       0,
		IMHistoryDays:      7,
		IMActivePageLimit:  40,
		IMMessagePageLimit: 1,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}
