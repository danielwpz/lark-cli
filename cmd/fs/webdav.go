// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/larksuite/cli/internal/cmdutil"
	"github.com/larksuite/cli/internal/output"
	"github.com/spf13/cobra"
	"golang.org/x/net/webdav"
)

const defaultWebDAVAddr = "127.0.0.1:8765"

type WebDAVOptions struct {
	Factory *cmdutil.Factory
	Cmd     *cobra.Command
	Ctx     context.Context

	CacheDir string
	Addr     string
}

func NewCmdWebDAV(f *cmdutil.Factory, runF func(*WebDAVOptions) error) *cobra.Command {
	return NewCmdWebDAVWithContext(context.Background(), f, runF)
}

func NewCmdWebDAVWithContext(ctx context.Context, f *cmdutil.Factory, runF func(*WebDAVOptions) error) *cobra.Command {
	opts := &WebDAVOptions{Factory: f, CacheDir: defaultOutputDir, Addr: defaultWebDAVAddr}

	cmd := &cobra.Command{
		Use:   "webdav",
		Short: "Serve a local cache over read-only WebDAV for debugging",
		Example: `  lark-cli fs sync --output-dir ~/.cache/larkfs
  lark-cli fs webdav --cache-dir ~/.cache/larkfs --addr 127.0.0.1:8765
  mkdir -p /private/tmp/larkfs-webdav
  mount_webdav -S -o rdonly -v LarkFS http://127.0.0.1:8765 /private/tmp/larkfs-webdav`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Cmd = cmd
			opts.Ctx = cmd.Context()
			if runF != nil {
				return runF(opts)
			}
			return runWebDAV(opts)
		},
	}

	cmd.Flags().StringVar(&opts.CacheDir, "cache-dir", defaultOutputDir, "local sync cache directory to expose")
	cmd.Flags().StringVar(&opts.Addr, "addr", defaultWebDAVAddr, "listen address for the local WebDAV server")
	_ = ctx
	return cmd
}

func runWebDAV(opts *WebDAVOptions) error {
	if opts.Factory == nil {
		return output.Errorf(output.ExitInternal, "internal_error", "missing command factory")
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if err := validateWebDAVOptions(opts); err != nil {
		return err
	}
	cacheDir, err := resolveCacheDir(opts.CacheDir)
	if err != nil {
		return err
	}
	if err := ensureCacheReady(cacheDir); err != nil {
		return err
	}

	server := &http.Server{
		Addr:              opts.Addr,
		Handler:           newReadOnlyWebDAVHandler(readOnlyWebDAVFS{FileSystem: webdav.Dir(cacheDir)}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	fmt.Fprintf(opts.Factory.IOStreams.ErrOut, "serve larkfs webdav: cache=%s addr=http://%s\n", cacheDir, opts.Addr)
	printWebDAVMountHint(opts.Factory.IOStreams.ErrOut, opts.Addr)

	select {
	case <-opts.Ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return output.Errorf(output.ExitInternal, "network", "shutdown webdav server: %s", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return output.Errorf(output.ExitNetwork, "network", "serve webdav on %s: %s", opts.Addr, err)
	}
}

func validateWebDAVOptions(opts *WebDAVOptions) error {
	if strings.TrimSpace(opts.CacheDir) == "" {
		return output.ErrValidation("--cache-dir cannot be empty")
	}
	if strings.TrimSpace(opts.Addr) == "" {
		return output.ErrValidation("--addr cannot be empty")
	}
	host, port, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return output.ErrValidation("--addr must be host:port, got %q", opts.Addr)
	}
	if port == "" {
		return output.ErrValidation("--addr must include a port")
	}
	if host == "" {
		return output.ErrValidation("--addr host cannot be empty; use 127.0.0.1 for local-only serving")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return output.ErrWithHint(
			output.ExitValidation,
			"validation",
			"--addr must bind to a loopback address for this POC",
			"use --addr 127.0.0.1:8765",
		)
	}
	return nil
}

func newReadOnlyWebDAVHandler(fsys webdav.FileSystem) http.Handler {
	handler := &webdav.Handler{
		Prefix:     "/",
		FileSystem: fsys,
		LockSystem: webdav.NewMemLS(),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebDAVWriteMethod(r.Method) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("read-only WebDAV view\n"))
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func isWebDAVWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodDelete, http.MethodPatch, "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", "PROPPATCH":
		return true
	default:
		return false
	}
}

func printWebDAVMountHint(errOut io.Writer, addr string) {
	if errOut == nil {
		return
	}
	if runtime.GOOS == "darwin" {
		fmt.Fprintf(errOut, "mount on macOS: mkdir -p /private/tmp/larkfs-webdav && mount_webdav -S -o rdonly -v LarkFS http://%s /private/tmp/larkfs-webdav\n", addr)
		fmt.Fprintln(errOut, "unmount on macOS: umount /private/tmp/larkfs-webdav")
		return
	}
	fmt.Fprintf(errOut, "webdav url: http://%s\n", addr)
}

type readOnlyWebDAVFS struct {
	webdav.FileSystem
}

func (fs readOnlyWebDAVFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return os.ErrPermission
}

func (fs readOnlyWebDAVFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if flag&syscall.O_ACCMODE != os.O_RDONLY || flag&(os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_EXCL) != 0 {
		return nil, os.ErrPermission
	}
	return fs.FileSystem.OpenFile(ctx, name, flag, perm)
}

func (fs readOnlyWebDAVFS) RemoveAll(ctx context.Context, name string) error {
	return os.ErrPermission
}

func (fs readOnlyWebDAVFS) Rename(ctx context.Context, oldName, newName string) error {
	return os.ErrPermission
}
