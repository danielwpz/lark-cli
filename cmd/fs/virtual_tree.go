// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"bytes"
	"context"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

type virtualFileLoader func(context.Context) ([]byte, time.Time, error)

type virtualTree struct {
	mu   sync.RWMutex
	root *virtualNode
}

type virtualNode struct {
	name string
	dir  bool

	mu      sync.Mutex
	data    []byte
	loaded  bool
	loader  virtualFileLoader
	modTime time.Time

	children map[string]*virtualNode
}

func newVirtualTree(now time.Time) *virtualTree {
	return &virtualTree{
		root: &virtualNode{
			name:     "",
			dir:      true,
			modTime:  now,
			children: map[string]*virtualNode{},
		},
	}
}

func (t *virtualTree) AddDir(rel string, modTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensureDirLocked(rel, modTime)
}

func (t *virtualTree) AddStaticFile(rel string, data []byte, modTime time.Time) {
	t.addFile(rel, data, nil, modTime)
}

func (t *virtualTree) AddLazyFile(rel string, loader virtualFileLoader, modTime time.Time) {
	t.addFile(rel, nil, loader, modTime)
}

func (t *virtualTree) SetStaticFile(rel string, data []byte, modTime time.Time) {
	t.addFile(rel, data, nil, modTime)
}

func (t *virtualTree) addFile(rel string, data []byte, loader virtualFileLoader, modTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	rel = cleanVirtualPath(rel)
	parent := t.ensureDirLocked(path.Dir(rel), modTime)
	name := path.Base(rel)
	parent.children[name] = &virtualNode{
		name:    name,
		dir:     false,
		data:    cloneBytes(data),
		loaded:  loader == nil,
		loader:  loader,
		modTime: modTimeOrNow(modTime),
	}
}

func (t *virtualTree) ensureDirLocked(rel string, modTime time.Time) *virtualNode {
	rel = cleanVirtualPath(rel)
	if rel == "" || rel == "." {
		return t.root
	}
	node := t.root
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		child := node.children[part]
		if child == nil || !child.dir {
			child = &virtualNode{
				name:     part,
				dir:      true,
				modTime:  modTimeOrNow(modTime),
				children: map[string]*virtualNode{},
			}
			node.children[part] = child
		}
		node = child
	}
	return node
}

func (t *virtualTree) lookup(rel string) (*virtualNode, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	rel = cleanVirtualPath(rel)
	if rel == "" || rel == "." {
		return t.root, nil
	}
	node := t.root
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		child := node.children[part]
		if child == nil {
			return nil, iofs.ErrNotExist
		}
		node = child
	}
	return node, nil
}

func (n *virtualNode) read(ctx context.Context) ([]byte, time.Time, error) {
	if n.dir {
		return nil, n.modTime, iofs.ErrInvalid
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.loaded && n.loader != nil {
		data, modTime, err := n.loader(ctx)
		if err != nil {
			return nil, n.modTime, err
		}
		n.data = cloneBytes(data)
		n.loaded = true
		if !modTime.IsZero() {
			n.modTime = modTime
		}
	}
	return cloneBytes(n.data), n.modTime, nil
}

func (n *virtualNode) fileInfo() os.FileInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	size := int64(len(n.data))
	if n.dir {
		size = 0
	}
	return virtualFileInfo{
		name:    n.displayName(),
		size:    size,
		mode:    n.mode(),
		modTime: modTimeOrNow(n.modTime),
		dir:     n.dir,
	}
}

func (n *virtualNode) displayName() string {
	if n.name == "" {
		return "/"
	}
	return n.name
}

func (n *virtualNode) mode() os.FileMode {
	if n.dir {
		return os.ModeDir | 0555
	}
	return 0444
}

func (n *virtualNode) dirInfos() []os.FileInfo {
	if !n.dir {
		return nil
	}
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	infos := make([]os.FileInfo, 0, len(names))
	for _, name := range names {
		infos = append(infos, n.children[name].fileInfo())
	}
	return infos
}

type virtualWebDAVFS struct {
	tree *virtualTree
}

func newVirtualWebDAVFS(tree *virtualTree) webdav.FileSystem {
	return virtualWebDAVFS{tree: tree}
}

func (fsys virtualWebDAVFS) Mkdir(context.Context, string, os.FileMode) error {
	return os.ErrPermission
}

func (fsys virtualWebDAVFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 || flag&(os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_EXCL) != 0 {
		return nil, os.ErrPermission
	}
	node, err := fsys.tree.lookup(name)
	if err != nil {
		return nil, err
	}
	if node.dir {
		return &virtualWebDAVFile{info: node.fileInfo(), dirEntries: node.dirInfos()}, nil
	}
	data, modTime, err := node.read(ctx)
	if err != nil {
		return nil, err
	}
	return &virtualWebDAVFile{
		reader: bytes.NewReader(data),
		info: virtualFileInfo{
			name:    node.displayName(),
			size:    int64(len(data)),
			mode:    0444,
			modTime: modTimeOrNow(modTime),
		},
	}, nil
}

func (fsys virtualWebDAVFS) RemoveAll(context.Context, string) error {
	return os.ErrPermission
}

func (fsys virtualWebDAVFS) Rename(context.Context, string, string) error {
	return os.ErrPermission
}

func (fsys virtualWebDAVFS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	node, err := fsys.tree.lookup(name)
	if err != nil {
		return nil, err
	}
	return node.fileInfo(), nil
}

type virtualWebDAVFile struct {
	reader     *bytes.Reader
	info       os.FileInfo
	dirEntries []os.FileInfo
	dirOffset  int
}

func (f *virtualWebDAVFile) Close() error {
	return nil
}

func (f *virtualWebDAVFile) Read(p []byte) (int, error) {
	if f.reader == nil {
		return 0, io.EOF
	}
	return f.reader.Read(p)
}

func (f *virtualWebDAVFile) Write([]byte) (int, error) {
	return 0, os.ErrPermission
}

func (f *virtualWebDAVFile) Seek(offset int64, whence int) (int64, error) {
	if f.reader == nil {
		return 0, errors.New("cannot seek directory")
	}
	return f.reader.Seek(offset, whence)
}

func (f *virtualWebDAVFile) Readdir(count int) ([]os.FileInfo, error) {
	if f.dirEntries == nil {
		return nil, errors.New("not a directory")
	}
	if f.dirOffset >= len(f.dirEntries) && count > 0 {
		return nil, io.EOF
	}
	if count <= 0 {
		out := f.dirEntries[f.dirOffset:]
		f.dirOffset = len(f.dirEntries)
		return out, nil
	}
	end := f.dirOffset + count
	if end > len(f.dirEntries) {
		end = len(f.dirEntries)
	}
	out := f.dirEntries[f.dirOffset:end]
	f.dirOffset = end
	return out, nil
}

func (f *virtualWebDAVFile) Stat() (os.FileInfo, error) {
	return f.info, nil
}

type virtualFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	dir     bool
}

func (i virtualFileInfo) Name() string {
	return i.name
}

func (i virtualFileInfo) Size() int64 {
	return i.size
}

func (i virtualFileInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0555
	}
	if i.mode == 0 {
		return 0444
	}
	return i.mode
}

func (i virtualFileInfo) ModTime() time.Time {
	return modTimeOrNow(i.modTime)
}

func (i virtualFileInfo) IsDir() bool {
	return i.dir
}

func (i virtualFileInfo) Sys() interface{} {
	return nil
}

func cleanVirtualPath(rel string) string {
	rel = strings.TrimSpace(strings.TrimPrefix(rel, "/"))
	if rel == "" {
		return ""
	}
	cleaned := path.Clean(path.Clean("/" + rel))
	return strings.TrimPrefix(cleaned, "/")
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func modTimeOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
