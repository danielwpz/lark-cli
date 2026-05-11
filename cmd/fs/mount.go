// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/larksuite/cli/internal/cmdutil"
	"github.com/larksuite/cli/internal/core"
	"github.com/larksuite/cli/internal/output"
	"github.com/larksuite/cli/internal/validate"
	"github.com/larksuite/cli/internal/vfs"
	"github.com/spf13/cobra"
)

type MountOptions struct {
	Factory *cmdutil.Factory
	Cmd     *cobra.Command
	Ctx     context.Context

	MountPoint string
	CacheDir   string
	Backend    string
	WebDAVAddr string
	Refresh    bool
	ReadOnly   bool

	Docs               bool
	IM                 bool
	DocsPrefetch       string
	DocsPrefetchLimit  int
	IMPrefetch         string
	DocsPageLimit      int
	DocsFolderTokens   []string
	IMActiveDays       int
	IMHistoryDays      int
	IMActivePageLimit  int
	IMMessagePageLimit int
	As                 core.Identity
}

func NewCmdMount(f *cmdutil.Factory, runF func(*MountOptions) error) *cobra.Command {
	return NewCmdMountWithContext(context.Background(), f, runF)
}

func NewCmdMountWithContext(ctx context.Context, f *cmdutil.Factory, runF func(*MountOptions) error) *cobra.Command {
	opts := &MountOptions{Factory: f, Backend: "auto", WebDAVAddr: "127.0.0.1:0", ReadOnly: true}

	cmd := &cobra.Command{
		Use:   "mount MOUNTPOINT",
		Short: "Mount a read-only Lark filesystem view",
		Example: `  lark-cli fs mount /private/tmp/larkfs --docs --im --im-active-days 3 --im-history-days 3
  lark-cli fs mount /private/tmp/larkfs --backend webdav --docs-prefetch all
  lark-cli fs mount /private/tmp/larkfs --docs-prefetch metadata --im-prefetch metadata`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Cmd = cmd
			opts.Ctx = cmd.Context()
			opts.MountPoint = args[0]
			asStr, _ := cmd.Flags().GetString("as")
			opts.As = resolveSyncIdentity(f, cmd, core.Identity(asStr))
			if runF != nil {
				return runF(opts)
			}
			return runMount(opts)
		},
	}

	cmd.Flags().StringVar(&opts.CacheDir, "cache-dir", defaultOutputDir, "internal cache directory for fetched content")
	cmd.Flags().StringVar(&opts.Backend, "backend", "auto", "mount backend: auto or webdav")
	cmd.Flags().StringVar(&opts.WebDAVAddr, "webdav-addr", "127.0.0.1:0", "local WebDAV listen address used by --backend=webdav")
	cmd.Flags().BoolVar(&opts.Refresh, "refresh", false, "deprecated; mount always builds a fresh virtual resource index")
	cmd.Flags().BoolVar(&opts.ReadOnly, "read-only", true, "mount read-only; false is not supported in this POC")
	cmd.Flags().BoolVar(&opts.Docs, "docs", false, "include Word-like docs (doc/docx) as Markdown")
	cmd.Flags().BoolVar(&opts.IM, "im", false, "include recently active chats as Markdown and NDJSON")
	cmd.Flags().StringVar(&opts.DocsPrefetch, "docs-prefetch", prefetchAll, "docs content prefetch mode: metadata, recent, or all")
	cmd.Flags().IntVar(&opts.DocsPrefetchLimit, "docs-prefetch-limit", 50, "max docs to prefetch when --docs-prefetch=recent")
	cmd.Flags().StringVar(&opts.IMPrefetch, "im-prefetch", prefetchAll, "IM message prefetch mode: metadata or all")
	cmd.Flags().IntVar(&opts.DocsPageLimit, "docs-page-limit", defaultDocsPageLimit, "max Search v2 pages to scan for docs")
	cmd.Flags().StringSliceVar(&opts.DocsFolderTokens, "docs-folder-token", nil, "limit doc discovery to folder token(s)")
	cmd.Flags().IntVar(&opts.IMActiveDays, "im-active-days", defaultIMActiveDays, "include chats active within this many days")
	cmd.Flags().IntVar(&opts.IMHistoryDays, "im-history-days", defaultIMHistoryDays, "sync this many days of messages for each active chat")
	cmd.Flags().IntVar(&opts.IMActivePageLimit, "im-active-page-limit", defaultIMActivePageLimit, "max message-search pages used to discover active chats")
	cmd.Flags().IntVar(&opts.IMMessagePageLimit, "im-message-page-limit", defaultIMMessagePageLimit, "max message-list pages per active chat")
	cmdutil.AddShortcutIdentityFlag(ctx, cmd, f, []string{"user"})

	return cmd
}

func runMount(opts *MountOptions) error {
	if opts.Factory == nil {
		return output.Errorf(output.ExitInternal, "internal_error", "missing command factory")
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if err := validateMountOptions(opts); err != nil {
		return err
	}
	if err := opts.Factory.CheckIdentity(opts.As, []string{string(core.AsUser)}); err != nil {
		return output.ErrValidation("%s", err)
	}
	if err := opts.Factory.CheckStrictMode(opts.Ctx, opts.As); err != nil {
		return err
	}

	cacheDir, err := resolveCacheDir(opts.CacheDir)
	if err != nil {
		return err
	}
	opts.Backend = resolveMountBackend(opts.Backend)
	mountPoint, err := resolveMountPoint(opts.MountPoint)
	if err != nil {
		return err
	}
	if err := createMountPoint(mountPoint); err != nil {
		return err
	}
	if err := ensureMountPointEmpty(mountPoint); err != nil {
		return err
	}

	session, err := newLarkFSSession(opts.Ctx, opts, cacheDir)
	if err != nil {
		return err
	}

	switch opts.Backend {
	case "webdav":
		return runMountWebDAV(opts, session, mountPoint)
	default:
		return output.ErrWithHint(
			output.ExitValidation,
			"backend_unsupported",
			fmt.Sprintf("--backend=%s is not wired to the virtual resource tree yet", opts.Backend),
			"use --backend webdav for this POC",
		)
	}
}

func validateMountOptions(opts *MountOptions) error {
	if strings.TrimSpace(opts.MountPoint) == "" {
		return output.ErrValidation("MOUNTPOINT cannot be empty")
	}
	if strings.TrimSpace(opts.CacheDir) == "" {
		return output.ErrValidation("--cache-dir cannot be empty")
	}
	switch opts.Backend {
	case "auto", "webdav":
	default:
		return output.ErrValidation("--backend must be one of: auto, webdav")
	}
	if strings.TrimSpace(opts.WebDAVAddr) == "" {
		opts.WebDAVAddr = "127.0.0.1:0"
	}
	if err := validateLoopbackListenAddr("--webdav-addr", opts.WebDAVAddr); err != nil {
		return err
	}
	if !opts.ReadOnly {
		return output.ErrValidation("--read-only=false is not supported in this POC")
	}
	if err := validatePrefetchMode("--docs-prefetch", opts.DocsPrefetch, []string{prefetchMetadata, prefetchRecent, prefetchAll}); err != nil {
		return err
	}
	if err := validatePrefetchMode("--im-prefetch", opts.IMPrefetch, []string{prefetchMetadata, prefetchAll}); err != nil {
		return err
	}
	if opts.DocsPrefetchLimit < 1 {
		return output.ErrValidation("--docs-prefetch-limit must be at least 1")
	}
	return validateSyncOptions(&SyncOptions{
		OutputDir:          opts.CacheDir,
		DocsPageLimit:      opts.DocsPageLimit,
		IMActiveDays:       opts.IMActiveDays,
		IMHistoryDays:      opts.IMHistoryDays,
		IMActivePageLimit:  opts.IMActivePageLimit,
		IMMessagePageLimit: opts.IMMessagePageLimit,
	})
}

func validatePrefetchMode(flag, value string, allowed []string) error {
	for _, item := range allowed {
		if value == item {
			return nil
		}
	}
	return output.ErrValidation("%s must be one of: %s", flag, strings.Join(allowed, ", "))
}

func validateLoopbackListenAddr(flag, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return output.ErrValidation("%s must be host:port, got %q", flag, addr)
	}
	if port == "" {
		return output.ErrValidation("%s must include a port", flag)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return output.ErrWithHint(
			output.ExitValidation,
			"validation",
			fmt.Sprintf("%s must bind to a loopback address for this POC", flag),
			"use --webdav-addr 127.0.0.1:0",
		)
	}
	return nil
}

func resolveMountBackend(backend string) string {
	if backend == "auto" {
		return "webdav"
	}
	return backend
}

func runMountWebDAV(opts *MountOptions, session *larkFSSession, mountPoint string) error {
	listener, err := net.Listen("tcp", opts.WebDAVAddr)
	if err != nil {
		return output.Errorf(output.ExitNetwork, "network", "listen webdav on %s: %s", opts.WebDAVAddr, err)
	}
	addr := listener.Addr().String()
	server := &http.Server{
		Handler:           newReadOnlyWebDAVHandler(newVirtualWebDAVFS(session.tree)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	if runtime.GOOS != "darwin" {
		_ = server.Shutdown(context.Background())
		return output.ErrWithHint(
			output.ExitValidation,
			"backend_unsupported",
			"--backend=webdav automatic mounting is currently implemented for macOS",
			fmt.Sprintf("debug WebDAV URL: http://%s", addr),
		)
	}

	url := fmt.Sprintf("http://%s", addr)
	mountCmd := exec.CommandContext(opts.Ctx, "mount_webdav", "-S", "-o", "rdonly", "-v", "LarkFS", url, mountPoint)
	if out, err := mountCmd.CombinedOutput(); err != nil {
		_ = server.Shutdown(context.Background())
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return output.ErrWithHint(
			output.ExitInternal,
			"mount_error",
			fmt.Sprintf("mount WebDAV LarkFS at %s: %s", mountPoint, message),
			"make sure MOUNTPOINT is an empty local directory and retry",
		)
	}

	fmt.Fprintf(opts.Factory.IOStreams.ErrOut, "larkfs mounted: %s\n", mountPoint)
	fmt.Fprintf(opts.Factory.IOStreams.ErrOut, "backend: webdav %s\n", url)
	fmt.Fprintf(opts.Factory.IOStreams.ErrOut, "cache: %s\n", session.cacheDir)
	fmt.Fprintf(opts.Factory.IOStreams.ErrOut, "unmount with: umount %s\n", mountPoint)
	session.startBackgroundWork(opts.Ctx, opts.Factory.IOStreams.ErrOut)

	select {
	case <-opts.Ctx.Done():
		_ = exec.Command("umount", mountPoint).Run()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return output.Errorf(output.ExitNetwork, "network", "serve webdav backend: %s", err)
	}
}

func resolveCacheDir(raw string) (string, error) {
	resolved, err := resolveLocalDirFlag(raw, "--cache-dir")
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func resolveMountPoint(raw string) (string, error) {
	resolved, err := resolveLocalDirFlag(raw, "MOUNTPOINT")
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func resolveLocalDirFlag(raw, flagName string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", output.ErrValidation("%s cannot be empty", flagName)
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := vfs.UserHomeDir()
		if err != nil {
			return "", output.Errorf(output.ExitInternal, "file_error", "resolve home directory: %s", err)
		}
		if raw == "~" {
			raw = home
		} else {
			raw = filepath.Join(home, strings.TrimPrefix(raw, "~/"))
		}
	}
	if filepath.IsAbs(raw) {
		resolved, err := validate.SafeEnvDirPath(raw, flagName)
		if err != nil {
			return "", output.ErrValidation("%s", err)
		}
		return resolved, nil
	}
	resolved, err := validate.SafeOutputPath(raw)
	if err != nil {
		return "", output.ErrValidation("unsafe %s %q: %s", flagName, raw, err)
	}
	return resolved, nil
}

func createMountPoint(mountPoint string) error {
	if err := vfs.MkdirAll(mountPoint, 0700); err != nil {
		return output.Errorf(output.ExitInternal, "file_error", "create mount point: %s", err)
	}
	return nil
}

func ensureCacheReady(cacheDir string) error {
	info, err := vfs.Stat(cacheDir)
	if err != nil {
		return output.ErrWithHint(
			output.ExitValidation,
			"cache_missing",
			fmt.Sprintf("cache directory is not ready: %s", err),
			"run: lark-cli fs sync --output-dir <cache-dir>",
		)
	}
	if !info.IsDir() {
		return output.ErrValidation("--cache-dir must be a directory")
	}
	if _, err := vfs.Stat(filepath.Join(cacheDir, ".meta", "snapshot.json")); err != nil {
		return output.ErrWithHint(
			output.ExitValidation,
			"cache_missing",
			"cache snapshot metadata is missing",
			"run: lark-cli fs sync --output-dir <cache-dir>",
		)
	}
	return nil
}

func ensureMountPointEmpty(mountPoint string) error {
	entries, err := vfs.ReadDir(mountPoint)
	if err != nil {
		return output.Errorf(output.ExitInternal, "file_error", "read mount point: %s", err)
	}
	if len(entries) > 0 {
		return output.ErrWithHint(
			output.ExitValidation,
			"mountpoint_not_empty",
			"MOUNTPOINT must be an empty directory",
			"choose an empty directory, for example: mkdir -p ~/mnt/lark",
		)
	}
	return nil
}
