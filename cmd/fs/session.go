// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/larksuite/cli/internal/fsview"
	"github.com/larksuite/cli/internal/output"
	"github.com/larksuite/cli/internal/vfs"
	"github.com/larksuite/cli/internal/vfs/localfileio"
)

const (
	prefetchMetadata = "metadata"
	prefetchRecent   = "recent"
	prefetchAll      = "all"
)

type larkFSSession struct {
	opts     *MountOptions
	tree     *virtualTree
	syncer   *syncer
	cacheDir string
	now      time.Time

	docsEnabled bool
	imEnabled   bool
	docs        []docItem
	docFolders  []string
	chats       []chatItem
	chatDates   []string
	errors      []syncError

	chatCaches map[string]*chatMessageCache
	mu         sync.Mutex
}

type chatMessageCache struct {
	session *larkFSSession
	chat    chatItem

	mu     sync.Mutex
	loaded bool
	days   map[string]chatDayContent
	err    error
}

type chatDayContent struct {
	markdown []byte
	ndjson   []byte
}

func newLarkFSSession(ctx context.Context, opts *MountOptions, cacheDir string) (*larkFSSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := opts.Factory.Config()
	if err != nil {
		return nil, err
	}
	apiClient, err := opts.Factory.NewAPIClientWithConfig(cfg)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	enableDocs, enableIM := opts.Docs, opts.IM
	if !enableDocs && !enableIM {
		enableDocs, enableIM = true, true
	}
	syncOpts := &SyncOptions{
		Factory:            opts.Factory,
		Cmd:                opts.Cmd,
		Ctx:                ctx,
		OutputDir:          cacheDir,
		Docs:               enableDocs,
		IM:                 enableIM,
		DocsPageLimit:      opts.DocsPageLimit,
		DocsFolderTokens:   opts.DocsFolderTokens,
		IMActiveDays:       opts.IMActiveDays,
		IMHistoryDays:      opts.IMHistoryDays,
		IMActivePageLimit:  opts.IMActivePageLimit,
		IMMessagePageLimit: opts.IMMessagePageLimit,
		As:                 opts.As,
	}
	session := &larkFSSession{
		opts:        opts,
		tree:        newVirtualTree(now),
		cacheDir:    cacheDir,
		now:         now,
		docsEnabled: enableDocs,
		imEnabled:   enableIM,
		chatCaches:  map[string]*chatMessageCache{},
		syncer: &syncer{
			opts:      syncOpts,
			api:       apiClient,
			config:    cfg,
			outputDir: cacheDir,
			now:       now,
		},
	}
	if err := vfs.MkdirAll(session.cacheObjectsDir(), 0700); err != nil {
		return nil, output.Errorf(output.ExitInternal, "file_error", "create internal cache: %s", err)
	}
	if err := vfs.MkdirAll(session.cacheManifestsDir(), 0700); err != nil {
		return nil, output.Errorf(output.ExitInternal, "file_error", "create internal manifest cache: %s", err)
	}
	session.addBaseDirs()
	session.loadCachedManifests()
	session.writeMetaFiles()
	return session, nil
}

func (s *larkFSSession) startBackgroundWork(ctx context.Context, errOut io.Writer) {
	go func() {
		var wg sync.WaitGroup
		if s.docsEnabled {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if errOut != nil {
					fmt.Fprintln(errOut, "larkfs docs refresh started")
				}
				s.buildDocs()
				if errOut != nil {
					fmt.Fprintf(errOut, "larkfs docs refresh complete: %d document(s)\n", len(s.docs))
				}
			}()
		}
		if s.imEnabled {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if errOut != nil {
					fmt.Fprintln(errOut, "larkfs im refresh started")
				}
				s.buildIM()
				if errOut != nil {
					fmt.Fprintf(errOut, "larkfs im refresh complete: %d chat(s)\n", len(s.chats))
				}
			}()
		}
		wg.Wait()
		s.writeMetaFiles()
		s.startPrefetch(ctx, errOut)
	}()
}

func (s *larkFSSession) addBaseDirs() {
	for _, rel := range []string{".meta", "docs", "docs/drive", "docs/all", "docs/all/by-updated-month", "docs/.data", "im", "im/chats"} {
		s.tree.AddDir(rel, s.now)
	}
}

func (s *larkFSSession) buildDocs() {
	var docs []docItem
	seen := map[string]int{}

	var driveDocs []docItem
	var folders []string
	var driveErr error
	var searchDocs []docItem
	var searchErr error
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		driveDocs, folders, driveErr = s.discoverDriveDocs()
	}()
	go func() {
		defer wg.Done()
		searchDocs, searchErr = s.syncer.discoverDocs()
	}()
	wg.Wait()

	if driveErr != nil {
		s.recordError("docs", "discover_drive", "", driveErr)
	} else {
		for _, doc := range driveDocs {
			key := docKey(doc)
			doc.Views = appendUnique(doc.Views, "drive")
			doc.Canonical = doc.DrivePath
			modTime := s.now
			if t, ok := parseRFC3339(doc.UpdatedAt); ok {
				modTime = t
			}
			s.tree.AddLazyFile(doc.Canonical, s.docLoader(doc), modTime)
			s.addDocMetadataFiles(doc, modTime)
			seen[key] = len(docs)
			docs = append(docs, doc)
		}
	}

	if searchErr != nil {
		s.recordError("docs", "discover_search", "", searchErr)
	} else {
		for _, doc := range searchDocs {
			key := docKey(doc)
			if idx, ok := seen[key]; ok {
				docs[idx].Views = appendUnique(docs[idx].Views, "search")
				continue
			}
			doc.Views = appendUnique(doc.Views, "search")
			doc.Canonical = byUpdatedMonthPath(doc, s.now)
			modTime := s.now
			if t, ok := parseRFC3339(doc.UpdatedAt); ok {
				modTime = t
			}
			s.tree.AddLazyFile(doc.Canonical, s.docLoader(doc), modTime)
			s.addDocMetadataFiles(doc, modTime)
			seen[key] = len(docs)
			docs = append(docs, doc)
		}
	}

	sort.Slice(docs, func(i, j int) bool {
		if docs[i].DrivePath != "" && docs[j].DrivePath != "" {
			return docs[i].DrivePath < docs[j].DrivePath
		}
		if docs[i].DrivePath != "" {
			return true
		}
		if docs[j].DrivePath != "" {
			return false
		}
		return docs[i].Canonical < docs[j].Canonical
	})
	s.applyDocsManifest(docs, folders)
	s.writeDocsManifest(docs, folders)
}

func (s *larkFSSession) applyDocsManifest(docs []docItem, folders []string) {
	indexRows := make([]interface{}, 0, len(docs))
	for _, folder := range folders {
		s.tree.AddDir(filepath.ToSlash(filepath.Join("docs", "drive", folder)), s.now)
	}
	for _, doc := range docs {
		modTime := s.now
		if t, ok := parseRFC3339(doc.UpdatedAt); ok {
			modTime = t
		}
		if doc.Canonical != "" {
			s.tree.AddLazyFile(doc.Canonical, s.docLoader(doc), modTime)
		}
		s.addDocMetadataFiles(doc, modTime)
		indexRows = append(indexRows, doc)
	}
	s.docs = docs
	s.docFolders = folders
	s.tree.SetStaticFile("docs/index.ndjson", mustNDJSON(indexRows), s.now)
}

func (s *larkFSSession) addDocMetadataFiles(doc docItem, modTime time.Time) {
	s.tree.AddStaticFile(filepath.ToSlash(filepath.Join("docs", ".data", doc.ID+".meta.json")), mustJSON(doc), modTime)
	s.tree.AddStaticFile(filepath.ToSlash(filepath.Join("docs", ".data", doc.ID+".raw.json")), mustJSON(doc.Raw), modTime)
}

func byUpdatedMonthPath(doc docItem, now time.Time) string {
	month := now.Format("2006-01")
	if t, ok := parseRFC3339(doc.UpdatedAt); ok {
		month = t.Format("2006-01")
	}
	return filepath.ToSlash(filepath.Join("docs", "all", "by-updated-month", month, fsview.DocumentFileName(doc.Title, doc.ID)))
}

func docKey(doc docItem) string {
	return doc.Type + ":" + doc.Token
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

type driveDocDiscovery struct {
	docs    []docItem
	folders []string
}

func (s *larkFSSession) discoverDriveDocs() ([]docItem, []string, error) {
	roots := s.opts.DocsFolderTokens
	if len(roots) == 0 {
		roots = []string{""}
	}
	var out driveDocDiscovery
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if err := s.listDriveFolder(root, "", 0, &out); err != nil {
			return out.docs, out.folders, err
		}
	}
	return out.docs, out.folders, nil
}

func (s *larkFSSession) listDriveFolder(folderToken, relBase string, depth int, out *driveDocDiscovery) error {
	if depth > 20 {
		return nil
	}
	pageToken := ""
	for {
		params := map[string]interface{}{
			"folder_token": folderToken,
			"page_size":    "200",
		}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		data, err := s.syncer.callAPI("GET", "/open-apis/drive/v1/files", params, nil)
		if err != nil {
			return err
		}
		files, _ := data["files"].([]interface{})
		for _, raw := range files {
			m, _ := raw.(map[string]interface{})
			name := getString(m, "name")
			token := getString(m, "token")
			typ := strings.ToLower(getString(m, "type"))
			if name == "" || token == "" {
				continue
			}
			if typ == "folder" {
				folderRel := joinVirtualRel(relBase, fsview.SanitizeName(name))
				out.folders = append(out.folders, folderRel)
				if err := s.listDriveFolder(token, folderRel, depth+1, out); err != nil {
					return err
				}
				continue
			}
			doc, ok := buildDriveDocItem(m, relBase)
			if !ok {
				continue
			}
			out.docs = append(out.docs, doc)
		}
		hasMore, _ := data["has_more"].(bool)
		next := firstNonEmpty(getString(data, "page_token"), getString(data, "next_page_token"))
		if !hasMore || next == "" {
			break
		}
		pageToken = next
	}
	return nil
}

func buildDriveDocItem(raw map[string]interface{}, relBase string) (docItem, bool) {
	docType := normalizeDocType(raw["type"])
	if docType != "DOC" && docType != "DOCX" {
		return docItem{}, false
	}
	token := getString(raw, "token")
	if token == "" {
		return docItem{}, false
	}
	title := firstNonEmpty(getString(raw, "name"), "untitled")
	updatedAt := isoFromAny(raw["modified_time"])
	id := fsview.StableID(strings.ToLower(docType), token)
	driveRel := filepath.ToSlash(filepath.Join("docs", "drive", relBase, fsview.DocumentFileName(title, id)))
	return docItem{
		ID:         id,
		Token:      token,
		Type:       strings.ToLower(docType),
		EntityType: "DRIVE",
		Title:      title,
		URL:        getString(raw, "url"),
		UpdatedAt:  updatedAt,
		Canonical:  driveRel,
		DrivePath:  driveRel,
		Views:      []string{"drive"},
		Raw:        raw,
	}, true
}

func joinVirtualRel(base, name string) string {
	if base == "" {
		return name
	}
	return filepath.ToSlash(filepath.Join(base, name))
}

func (s *larkFSSession) buildIM() {
	activeSince := s.now.AddDate(0, 0, -s.opts.IMActiveDays)
	historySince := s.now.AddDate(0, 0, -s.opts.IMHistoryDays)
	chats, err := s.syncer.discoverActiveChats(activeSince, s.now)
	if err != nil {
		s.recordError("im", "discover", "", err)
		return
	}
	s.syncer.enrichChats(chats)
	sort.Slice(chats, func(i, j int) bool { return chats[i].lastActive.After(chats[j].lastActive) })
	s.applyIMManifest(chats, dateRange(historySince, s.now))
	s.writeIMManifest(chats, s.chatDates)
}

func (s *larkFSSession) applyIMManifest(chats []chatItem, dates []string) {
	chatRows := make([]interface{}, 0, len(chats))
	for i := range chats {
		if chats[i].Name == "" {
			chats[i].Name = "unknown"
		}
		chats[i].Path = filepath.ToSlash(filepath.Join("im", "chats", fsview.ChatDirName(chats[i].Type, chats[i].Name, chats[i].ID)))
		cache := &chatMessageCache{session: s, chat: chats[i], days: map[string]chatDayContent{}}
		s.chatCaches[chats[i].ID] = cache

		s.tree.AddStaticFile(filepath.ToSlash(filepath.Join(chats[i].Path, "meta.json")), mustJSON(chats[i]), s.now)
		for _, date := range dates {
			parts := strings.Split(date, "-")
			dayDir := filepath.Join(chats[i].Path, parts[0], parts[1])
			s.tree.AddLazyFile(filepath.ToSlash(filepath.Join(dayDir, date+".md")), cache.markdownLoader(date), s.now)
			s.tree.AddLazyFile(filepath.ToSlash(filepath.Join(chats[i].Path, ".data", date+".ndjson")), cache.ndjsonLoader(date), s.now)
		}
		chatRows = append(chatRows, chats[i])
	}
	s.chats = chats
	s.chatDates = dates
	s.tree.SetStaticFile("im/chats.ndjson", mustNDJSON(chatRows), s.now)
}

func (s *larkFSSession) writeMetaFiles() {
	capabilities := map[string]interface{}{
		"resources": map[string]bool{
			"docs": s.docsEnabled,
			"im":   s.imEnabled,
		},
		"mode":          "mount",
		"identity":      string(s.opts.As),
		"readonly":      true,
		"backend":       s.opts.Backend,
		"docs_prefetch": s.opts.DocsPrefetch,
		"im_prefetch":   s.opts.IMPrefetch,
	}
	s.tree.SetStaticFile(".meta/capabilities.json", mustJSON(capabilities), s.now)
	s.refreshErrorsFile()
	s.refreshSnapshotFile()
}

func (s *larkFSSession) refreshSnapshotFile() {
	snapshot := map[string]interface{}{
		"version":      1,
		"generated_at": s.now.Format(time.RFC3339),
		"parameters": map[string]interface{}{
			"docs":                  s.docsEnabled,
			"im":                    s.imEnabled,
			"docs_page_limit":       s.opts.DocsPageLimit,
			"docs_folder_tokens":    s.opts.DocsFolderTokens,
			"docs_prefetch":         s.opts.DocsPrefetch,
			"im_active_days":        s.opts.IMActiveDays,
			"im_history_days":       s.opts.IMHistoryDays,
			"im_active_page_limit":  s.opts.IMActivePageLimit,
			"im_message_page_limit": s.opts.IMMessagePageLimit,
			"im_prefetch":           s.opts.IMPrefetch,
		},
		"summary": map[string]interface{}{
			"docs": map[string]interface{}{
				"enabled": s.docsEnabled,
				"found":   len(s.docs),
			},
			"im": map[string]interface{}{
				"enabled": s.imEnabled,
				"chats":   len(s.chats),
			},
			"errors": len(s.errors),
		},
	}
	s.tree.SetStaticFile(".meta/snapshot.json", mustJSON(snapshot), s.now)
}

func (s *larkFSSession) refreshErrorsFile() {
	s.tree.SetStaticFile(".meta/errors.ndjson", mustNDJSON(syncErrorsToAny(s.errors)), time.Now())
}

func (s *larkFSSession) recordError(resource, op, id string, err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, syncError{
		Time:     time.Now().Format(time.RFC3339),
		Resource: resource,
		Op:       op,
		ID:       id,
		Error:    err.Error(),
	})
	s.refreshErrorsFile()
}

func (s *larkFSSession) startPrefetch(ctx context.Context, errOut io.Writer) {
	if s.docsEnabled && s.opts.DocsPrefetch != prefetchMetadata {
		go s.prefetchDocs(ctx, errOut)
	}
	if s.imEnabled && s.opts.IMPrefetch != prefetchMetadata {
		go s.prefetchIM(ctx, errOut)
	}
}

func (s *larkFSSession) prefetchDocs(ctx context.Context, errOut io.Writer) {
	docs := s.docs
	if s.opts.DocsPrefetch == prefetchRecent && len(docs) > s.opts.DocsPrefetchLimit {
		docs = docs[:s.opts.DocsPrefetchLimit]
	}
	const workers = 8
	jobs := make(chan docItem)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for doc := range jobs {
				if ctx.Err() != nil {
					return
				}
				if _, _, err := s.docLoader(doc)(ctx); err != nil {
					s.recordError("docs", "prefetch", doc.ID, err)
				}
			}
		}()
	}
	for _, doc := range docs {
		if ctx.Err() != nil {
			close(jobs)
			wg.Wait()
			return
		}
		jobs <- doc
	}
	close(jobs)
	wg.Wait()
	if errOut != nil {
		fmt.Fprintf(errOut, "larkfs docs prefetch complete: %d document(s)\n", len(docs))
	}
}

func (s *larkFSSession) prefetchIM(ctx context.Context, errOut io.Writer) {
	const workers = 4
	jobs := make(chan chatItem)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chat := range jobs {
				if ctx.Err() != nil {
					return
				}
				cache := s.chatCaches[chat.ID]
				if cache == nil {
					continue
				}
				if err := cache.load(ctx); err != nil {
					s.recordError("im", "prefetch_chat", chat.ID, err)
				}
			}
		}()
	}
	for _, chat := range s.chats {
		if ctx.Err() != nil {
			close(jobs)
			wg.Wait()
			return
		}
		jobs <- chat
	}
	close(jobs)
	wg.Wait()
	if errOut != nil {
		fmt.Fprintf(errOut, "larkfs im prefetch complete: %d chat(s)\n", len(s.chats))
	}
}

func (s *larkFSSession) docLoader(doc docItem) virtualFileLoader {
	return func(ctx context.Context) ([]byte, time.Time, error) {
		modTime := s.now
		if t, ok := parseRFC3339(doc.UpdatedAt); ok {
			modTime = t
		}
		cacheRel := filepath.Join("docs", doc.ID, fsview.StableID(doc.UpdatedAt, doc.Token)+".md")
		if data, ok := s.readCache(cacheRel); ok {
			return data, modTime, nil
		}
		content, fetchedTitle, err := s.syncer.fetchDocMarkdown(doc.Token)
		if err != nil {
			s.recordError("docs", "fetch", doc.ID, err)
			return nil, modTime, err
		}
		if fetchedTitle != "" && !strings.HasPrefix(content, "# ") {
			content = "# " + fetchedTitle + "\n\n" + content
		}
		data := []byte(content)
		if err := s.writeCache(cacheRel, data); err != nil {
			s.recordError("docs", "cache_write", doc.ID, err)
		}
		return data, modTime, nil
	}
}

func (c *chatMessageCache) markdownLoader(date string) virtualFileLoader {
	return func(ctx context.Context) ([]byte, time.Time, error) {
		if err := c.load(ctx); err != nil {
			return nil, time.Now(), err
		}
		return c.days[date].markdown, time.Now(), nil
	}
}

func (c *chatMessageCache) ndjsonLoader(date string) virtualFileLoader {
	return func(ctx context.Context) ([]byte, time.Time, error) {
		if err := c.load(ctx); err != nil {
			return nil, time.Now(), err
		}
		return c.days[date].ndjson, time.Now(), nil
	}
}

func (c *chatMessageCache) load(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.err
	}
	s := c.session
	messagesRel := filepath.Join("im", c.chat.ID, fsview.StableID(s.now.Format("2006-01-02"), fmt.Sprintf("%d", s.opts.IMHistoryDays))+".messages.json")
	if data, ok := s.readCache(messagesRel); ok {
		var messages []map[string]interface{}
		if err := json.Unmarshal(data, &messages); err == nil {
			c.days = renderChatDays(c.chat, messages, dateRange(s.now.AddDate(0, 0, -s.opts.IMHistoryDays), s.now))
			c.loaded = true
			return nil
		}
	}
	historySince := s.now.AddDate(0, 0, -s.opts.IMHistoryDays)
	messages, err := s.syncer.fetchChatMessages(c.chat.ChatID, historySince, s.now)
	if err != nil {
		c.err = err
		s.recordError("im", "messages_list", c.chat.ID, err)
		c.loaded = true
		return err
	}
	s.syncer.resolveSenderNames(messages)
	if c.chat.Type == "dm" && (c.chat.Name == "" || c.chat.Name == "unknown") {
		c.chat.Name = inferDMName(messages, s.syncer.config.UserOpenId)
	}
	if c.chat.Name == "" {
		c.chat.Name = "unknown"
	}
	for _, msg := range messages {
		msg["chat_type"] = c.chat.Type
		msg["chat_name"] = c.chat.Name
	}
	if data, err := json.Marshal(messages); err == nil {
		if cacheErr := s.writeCache(messagesRel, data); cacheErr != nil {
			s.recordError("im", "cache_write", c.chat.ID, cacheErr)
		}
	}
	c.days = renderChatDays(c.chat, messages, dateRange(historySince, s.now))
	c.loaded = true
	return nil
}

func renderChatDays(chat chatItem, messages []map[string]interface{}, dates []string) map[string]chatDayContent {
	byDate := map[string][]map[string]interface{}{}
	for _, date := range dates {
		byDate[date] = nil
	}
	for _, msg := range messages {
		t, ok := parseRFC3339(getString(msg, "create_time"))
		if !ok {
			continue
		}
		date := t.Local().Format("2006-01-02")
		if _, ok := byDate[date]; ok {
			byDate[date] = append(byDate[date], msg)
		}
	}
	out := map[string]chatDayContent{}
	for _, date := range dates {
		msgs := byDate[date]
		out[date] = chatDayContent{
			markdown: []byte(fsview.RenderChatDayMarkdown(chat.Name, date, toRenderMessages(msgs))),
			ndjson:   mustNDJSON(mapsToAny(msgs)),
		}
	}
	return out
}

func (s *larkFSSession) cacheObjectsDir() string {
	return filepath.Join(s.cacheDir, "objects")
}

func (s *larkFSSession) cacheManifestsDir() string {
	return filepath.Join(s.cacheDir, "manifests")
}

type docsManifestCache struct {
	GeneratedAt string    `json:"generated_at"`
	Docs        []docItem `json:"docs"`
	Folders     []string  `json:"folders"`
}

type imManifestCache struct {
	GeneratedAt string     `json:"generated_at"`
	Chats       []chatItem `json:"chats"`
	Dates       []string   `json:"dates"`
}

func (s *larkFSSession) loadCachedManifests() {
	if s.docsEnabled {
		var docsCache docsManifestCache
		if s.readManifest("docs.json", &docsCache) == nil && len(docsCache.Docs) > 0 {
			s.applyDocsManifest(docsCache.Docs, docsCache.Folders)
		}
	}
	if s.imEnabled {
		var imCache imManifestCache
		if s.readManifest("im.json", &imCache) == nil && len(imCache.Chats) > 0 {
			dates := imCache.Dates
			if len(dates) == 0 {
				dates = dateRange(s.now.AddDate(0, 0, -s.opts.IMHistoryDays), s.now)
			}
			s.applyIMManifest(imCache.Chats, dates)
		}
	}
}

func (s *larkFSSession) readManifest(name string, out interface{}) error {
	data, err := vfs.ReadFile(filepath.Join(s.cacheManifestsDir(), name))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (s *larkFSSession) writeDocsManifest(docs []docItem, folders []string) {
	_ = s.writeManifest("docs.json", docsManifestCache{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Docs:        docs,
		Folders:     folders,
	})
}

func (s *larkFSSession) writeIMManifest(chats []chatItem, dates []string) {
	_ = s.writeManifest("im.json", imManifestCache{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Chats:       chats,
		Dates:       dates,
	})
}

func (s *larkFSSession) writeManifest(name string, value interface{}) error {
	return localfileio.AtomicWrite(filepath.Join(s.cacheManifestsDir(), name), mustJSON(value), 0600)
}

func (s *larkFSSession) readCache(rel string) ([]byte, bool) {
	path := filepath.Join(s.cacheObjectsDir(), filepath.FromSlash(filepath.ToSlash(rel)))
	data, err := vfs.ReadFile(path)
	return data, err == nil
}

func (s *larkFSSession) writeCache(rel string, data []byte) error {
	path := filepath.Join(s.cacheObjectsDir(), filepath.FromSlash(filepath.ToSlash(rel)))
	if err := vfs.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return localfileio.AtomicWrite(path, data, 0600)
}

func dateRange(start, end time.Time) []string {
	startDate := time.Date(start.Local().Year(), start.Local().Month(), start.Local().Day(), 0, 0, 0, 0, start.Local().Location())
	endDate := time.Date(end.Local().Year(), end.Local().Month(), end.Local().Day(), 0, 0, 0, 0, end.Local().Location())
	var dates []string
	for d := startDate; !d.After(endDate); d = d.AddDate(0, 0, 1) {
		dates = append(dates, d.Format("2006-01-02"))
	}
	return dates
}

func mustJSON(value interface{}) []byte {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return []byte(fmt.Sprintf("{\"error\":%q}\n", err.Error()))
	}
	return append(b, '\n')
}

func mustNDJSON(rows []interface{}) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return []byte(fmt.Sprintf("{\"error\":%q}\n", err.Error()))
		}
	}
	return b.Bytes()
}
