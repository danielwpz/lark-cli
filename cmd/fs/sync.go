// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/larksuite/cli/internal/client"
	"github.com/larksuite/cli/internal/cmdutil"
	"github.com/larksuite/cli/internal/core"
	"github.com/larksuite/cli/internal/fsview"
	"github.com/larksuite/cli/internal/output"
	"github.com/larksuite/cli/internal/validate"
	"github.com/larksuite/cli/internal/vfs"
	"github.com/larksuite/cli/internal/vfs/localfileio"
	convertlib "github.com/larksuite/cli/shortcuts/im/convert_lib"
	"github.com/spf13/cobra"
)

const (
	defaultOutputDir             = "~/.cache/larkfs"
	defaultDocsPageLimit         = 20
	defaultIMActiveDays          = 3
	defaultIMHistoryDays         = 7
	defaultIMActivePageLimit     = 40
	defaultIMMessagePageLimit    = 200
	defaultSearchPageSize        = 20
	defaultMessageSearchPageSize = 50
	defaultMessageListPageSize   = 50
)

type SyncOptions struct {
	Factory *cmdutil.Factory
	Cmd     *cobra.Command
	Ctx     context.Context

	OutputDir          string
	Docs               bool
	IM                 bool
	DocsPageLimit      int
	DocsFolderTokens   []string
	IMActiveDays       int
	IMHistoryDays      int
	IMActivePageLimit  int
	IMMessagePageLimit int
	As                 core.Identity
}

type SyncResult struct {
	OutputDir string         `json:"output_dir"`
	Docs      DocsSyncResult `json:"docs,omitempty"`
	IM        IMSyncResult   `json:"im,omitempty"`
	Errors    int            `json:"errors"`
	Snapshot  string         `json:"snapshot"`
}

type DocsSyncResult struct {
	Enabled bool `json:"enabled"`
	Found   int  `json:"found"`
	Fetched int  `json:"fetched"`
	Skipped int  `json:"skipped"`
}

type IMSyncResult struct {
	Enabled  bool `json:"enabled"`
	Chats    int  `json:"chats"`
	Messages int  `json:"messages"`
}

type syncError struct {
	Time     string `json:"time"`
	Resource string `json:"resource"`
	Op       string `json:"op"`
	ID       string `json:"id,omitempty"`
	Error    string `json:"error"`
}

func NewCmdSync(f *cmdutil.Factory, runF func(*SyncOptions) error) *cobra.Command {
	return NewCmdSyncWithContext(context.Background(), f, runF)
}

func NewCmdSyncWithContext(ctx context.Context, f *cmdutil.Factory, runF func(*SyncOptions) error) *cobra.Command {
	opts := &SyncOptions{Factory: f}

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync docs and recent chat messages into local Markdown/NDJSON files",
		Example: `  lark-cli fs sync
  lark-cli fs sync --output-dir ~/.cache/larkfs --docs
  lark-cli fs sync --im --im-active-days 3 --im-history-days 7`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Cmd = cmd
			opts.Ctx = cmd.Context()
			asStr, _ := cmd.Flags().GetString("as")
			opts.As = resolveSyncIdentity(f, cmd, core.Identity(asStr))
			if runF != nil {
				return runF(opts)
			}
			return runSync(opts)
		},
	}

	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", defaultOutputDir, "local directory to write the synced file tree")
	cmd.Flags().BoolVar(&opts.Docs, "docs", false, "sync Word-like docs (doc/docx) as Markdown")
	cmd.Flags().BoolVar(&opts.IM, "im", false, "sync recently active chats as Markdown and NDJSON")
	cmd.Flags().IntVar(&opts.DocsPageLimit, "docs-page-limit", defaultDocsPageLimit, "max Search v2 pages to scan for docs")
	cmd.Flags().StringSliceVar(&opts.DocsFolderTokens, "docs-folder-token", nil, "limit doc discovery to folder token(s); may be repeated or comma-separated")
	cmd.Flags().IntVar(&opts.IMActiveDays, "im-active-days", defaultIMActiveDays, "include chats active within this many days")
	cmd.Flags().IntVar(&opts.IMHistoryDays, "im-history-days", defaultIMHistoryDays, "sync this many days of messages for each active chat")
	cmd.Flags().IntVar(&opts.IMActivePageLimit, "im-active-page-limit", defaultIMActivePageLimit, "max message-search pages used to discover active chats")
	cmd.Flags().IntVar(&opts.IMMessagePageLimit, "im-message-page-limit", defaultIMMessagePageLimit, "max message-list pages per active chat")
	cmdutil.AddShortcutIdentityFlag(ctx, cmd, f, []string{"user"})

	return cmd
}

func resolveSyncIdentity(f *cmdutil.Factory, cmd *cobra.Command, flagAs core.Identity) core.Identity {
	if cmd != nil && cmd.Flags().Changed("as") {
		return f.ResolveAs(cmd.Context(), cmd, flagAs)
	}
	if forced := f.ResolveStrictMode(cmd.Context()).ForcedIdentity(); forced != "" {
		f.ResolvedIdentity = forced
		return forced
	}
	f.ResolvedIdentity = core.AsUser
	return core.AsUser
}

func runSync(opts *SyncOptions) error {
	if opts.Factory == nil {
		return output.Errorf(output.ExitInternal, "internal_error", "missing command factory")
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if err := opts.Factory.CheckIdentity(opts.As, []string{string(core.AsUser)}); err != nil {
		return output.ErrValidation("%s", err)
	}
	if err := opts.Factory.CheckStrictMode(opts.Ctx, opts.As); err != nil {
		return err
	}
	if err := validateSyncOptions(opts); err != nil {
		return err
	}

	outputDir, err := resolveOutputDir(opts.OutputDir)
	if err != nil {
		return err
	}
	if err := vfs.MkdirAll(outputDir, 0700); err != nil {
		return output.Errorf(output.ExitInternal, "file_error", "create --output-dir: %s", err)
	}

	cfg, err := opts.Factory.Config()
	if err != nil {
		return err
	}
	ac, err := opts.Factory.NewAPIClientWithConfig(cfg)
	if err != nil {
		return err
	}

	enableDocs, enableIM := opts.Docs, opts.IM
	if !enableDocs && !enableIM {
		enableDocs, enableIM = true, true
	}

	s := &syncer{
		opts:      opts,
		api:       ac,
		config:    cfg,
		outputDir: outputDir,
		now:       time.Now(),
	}

	if err := s.ensureBaseDirs(); err != nil {
		return err
	}

	result := SyncResult{
		OutputDir: outputDir,
		Snapshot:  s.now.Format(time.RFC3339),
	}
	if enableDocs {
		fmt.Fprintln(opts.Factory.IOStreams.ErrOut, "sync docs: discovering Word-like docs")
		result.Docs = s.syncDocs()
	}
	if enableIM {
		fmt.Fprintln(opts.Factory.IOStreams.ErrOut, "sync im: discovering active chats")
		result.IM = s.syncIM()
	}
	result.Errors = len(s.errors)

	if err := s.writeMeta(result, enableDocs, enableIM); err != nil {
		return err
	}
	return writeSuccess(opts, result)
}

func validateSyncOptions(opts *SyncOptions) error {
	if strings.TrimSpace(opts.OutputDir) == "" {
		return output.ErrValidation("--output-dir cannot be empty")
	}
	if opts.DocsPageLimit < 1 {
		return output.ErrValidation("--docs-page-limit must be at least 1")
	}
	if opts.IMActiveDays < 1 {
		return output.ErrValidation("--im-active-days must be at least 1")
	}
	if opts.IMHistoryDays < 1 {
		return output.ErrValidation("--im-history-days must be at least 1")
	}
	if opts.IMActivePageLimit < 1 || opts.IMActivePageLimit > 40 {
		return output.ErrValidation("--im-active-page-limit must be between 1 and 40")
	}
	if opts.IMMessagePageLimit < 1 {
		return output.ErrValidation("--im-message-page-limit must be at least 1")
	}
	return nil
}

func writeSuccess(opts *SyncOptions, result SyncResult) error {
	env := output.Envelope{
		OK:       true,
		Identity: string(opts.As),
		Data:     result,
		Notice:   output.GetNotice(),
	}
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return output.Errorf(output.ExitInternal, "internal_error", "marshal sync result: %s", err)
	}
	fmt.Fprintln(opts.Factory.IOStreams.Out, string(b))
	return nil
}

func resolveOutputDir(raw string) (string, error) {
	return resolveLocalDirFlag(raw, "--output-dir")
}

type syncer struct {
	opts      *SyncOptions
	api       *client.APIClient
	config    *core.CliConfig
	outputDir string
	now       time.Time
	errors    []syncError
}

func (s *syncer) ensureBaseDirs() error {
	for _, rel := range []string{".meta", "docs", "docs/all", "docs/.data", "im", "im/chats"} {
		if err := vfs.MkdirAll(filepath.Join(s.outputDir, rel), 0700); err != nil {
			return output.Errorf(output.ExitInternal, "file_error", "create %s: %s", rel, err)
		}
	}
	return nil
}

func (s *syncer) callAPI(method, apiPath string, params map[string]interface{}, body interface{}) (map[string]interface{}, error) {
	result, err := s.api.CallAPI(s.opts.Ctx, client.RawApiRequest{
		Method: method,
		URL:    apiPath,
		Params: params,
		Data:   body,
		As:     s.opts.As,
	})
	if err != nil {
		return nil, err
	}
	if err := client.CheckLarkResponse(result); err != nil {
		return nil, err
	}
	resultMap, _ := result.(map[string]interface{})
	data, _ := resultMap["data"].(map[string]interface{})
	if data == nil {
		data = map[string]interface{}{}
	}
	return data, nil
}

func (s *syncer) recordError(resource, op, id string, err error) {
	if err == nil {
		return
	}
	s.errors = append(s.errors, syncError{
		Time:     time.Now().Format(time.RFC3339),
		Resource: resource,
		Op:       op,
		ID:       id,
		Error:    err.Error(),
	})
}

func (s *syncer) writeMeta(result SyncResult, docsEnabled, imEnabled bool) error {
	capabilities := map[string]interface{}{
		"resources": map[string]bool{
			"docs": docsEnabled,
			"im":   imEnabled,
		},
		"mode":     "sync",
		"identity": string(s.opts.As),
		"readonly": true,
	}
	if err := s.writeJSON(".meta/capabilities.json", capabilities); err != nil {
		return err
	}
	snapshot := map[string]interface{}{
		"version":      1,
		"generated_at": s.now.Format(time.RFC3339),
		"output_dir":   s.outputDir,
		"parameters": map[string]interface{}{
			"docs":                  docsEnabled,
			"im":                    imEnabled,
			"docs_page_limit":       s.opts.DocsPageLimit,
			"docs_folder_tokens":    s.opts.DocsFolderTokens,
			"im_active_days":        s.opts.IMActiveDays,
			"im_history_days":       s.opts.IMHistoryDays,
			"im_active_page_limit":  s.opts.IMActivePageLimit,
			"im_message_page_limit": s.opts.IMMessagePageLimit,
		},
		"summary": result,
	}
	if err := s.writeJSON(".meta/snapshot.json", snapshot); err != nil {
		return err
	}
	return s.writeNDJSON(".meta/errors.ndjson", syncErrorsToAny(s.errors))
}

func syncErrorsToAny(items []syncError) []interface{} {
	out := make([]interface{}, len(items))
	for i := range items {
		out[i] = items[i]
	}
	return out
}

type docItem struct {
	ID          string                 `json:"id"`
	Token       string                 `json:"token"`
	Type        string                 `json:"type"`
	EntityType  string                 `json:"entity_type"`
	Title       string                 `json:"title"`
	URL         string                 `json:"url,omitempty"`
	UpdatedAt   string                 `json:"updated_at,omitempty"`
	Canonical   string                 `json:"canonical_path"`
	DrivePath   string                 `json:"drive_path,omitempty"`
	Views       []string               `json:"views"`
	Raw         map[string]interface{} `json:"-"`
	ContentSize int                    `json:"content_size,omitempty"`
}

func (s *syncer) syncDocs() DocsSyncResult {
	result := DocsSyncResult{Enabled: true}
	docs, err := s.discoverDocs()
	if err != nil {
		s.recordError("docs", "discover", "", err)
		_ = s.writeNDJSON("docs/index.ndjson", nil)
		return result
	}
	result.Found = len(docs)

	indexRows := make([]interface{}, 0, len(docs))
	for _, doc := range docs {
		content, fetchedTitle, err := s.fetchDocMarkdown(doc.Token)
		if err != nil {
			s.recordError("docs", "fetch", doc.ID, err)
			result.Skipped++
			continue
		}
		if fetchedTitle != "" {
			doc.Title = fetchedTitle
		}
		month := "unknown"
		if t, ok := parseRFC3339(doc.UpdatedAt); ok {
			month = t.Format("2006-01")
		} else {
			month = s.now.Format("2006-01")
		}
		doc.Canonical = filepath.ToSlash(filepath.Join("docs", "all", month, fsview.DocumentFileName(doc.Title, doc.ID)))
		doc.Views = []string{"all"}
		doc.ContentSize = len(content)

		if err := s.writeFile(doc.Canonical, []byte(content)); err != nil {
			s.recordError("docs", "write", doc.ID, err)
			result.Skipped++
			continue
		}
		if err := s.writeJSON(filepath.ToSlash(filepath.Join("docs", ".data", doc.ID+".meta.json")), doc); err != nil {
			s.recordError("docs", "write_meta", doc.ID, err)
		}
		if err := s.writeJSON(filepath.ToSlash(filepath.Join("docs", ".data", doc.ID+".raw.json")), doc.Raw); err != nil {
			s.recordError("docs", "write_raw", doc.ID, err)
		}
		indexRows = append(indexRows, doc)
		result.Fetched++
	}
	if err := s.writeNDJSON("docs/index.ndjson", indexRows); err != nil {
		s.recordError("docs", "write_index", "", err)
	}
	return result
}

func (s *syncer) discoverDocs() ([]docItem, error) {
	seen := map[string]bool{}
	var docs []docItem
	pageToken := ""
	for page := 0; page < s.opts.DocsPageLimit; page++ {
		body := map[string]interface{}{
			"query":     "",
			"page_size": defaultSearchPageSize,
		}
		filter := map[string]interface{}{"doc_types": []string{"DOC", "DOCX"}}
		if len(s.opts.DocsFolderTokens) > 0 {
			docFilter := cloneMap(filter)
			docFilter["folder_tokens"] = s.opts.DocsFolderTokens
			body["doc_filter"] = docFilter
		} else {
			body["doc_filter"] = cloneMap(filter)
			body["wiki_filter"] = cloneMap(filter)
		}
		if pageToken != "" {
			body["page_token"] = pageToken
		}

		data, err := s.callAPI(http.MethodPost, "/open-apis/search/v2/doc_wiki/search", nil, body)
		if err != nil {
			return docs, err
		}
		items, _ := data["res_units"].([]interface{})
		for _, raw := range items {
			m, _ := raw.(map[string]interface{})
			item, ok := buildDocItem(m)
			if !ok {
				continue
			}
			key := item.Type + ":" + item.Token
			if seen[key] {
				continue
			}
			seen[key] = true
			docs = append(docs, item)
		}
		hasMore, _ := data["has_more"].(bool)
		next, _ := data["page_token"].(string)
		if !hasMore || next == "" {
			break
		}
		pageToken = next
	}
	sort.Slice(docs, func(i, j int) bool {
		if docs[i].UpdatedAt == docs[j].UpdatedAt {
			return docs[i].Title < docs[j].Title
		}
		return docs[i].UpdatedAt > docs[j].UpdatedAt
	})
	return docs, nil
}

func buildDocItem(raw map[string]interface{}) (docItem, bool) {
	meta := getMap(raw, "result_meta")
	entityType := strings.ToUpper(getString(raw, "entity_type"))
	docType := normalizeDocType(meta["doc_types"])
	if docType == "" {
		docType = normalizeDocType(raw["doc_type"])
	}
	urlValue := getString(meta, "url")
	if docType == "" {
		docType = docTypeFromURL(urlValue)
	}
	if docType != "DOC" && docType != "DOCX" {
		return docItem{}, false
	}
	token := firstNonEmpty(
		getString(meta, "token"),
		getString(meta, "doc_token"),
		getString(meta, "obj_token"),
		extractDocToken(urlValue),
	)
	if token == "" {
		return docItem{}, false
	}
	title := firstNonEmpty(
		stripHighlightTags(getString(raw, "title")),
		stripHighlightTags(getString(raw, "title_highlighted")),
		stripHighlightTags(getString(meta, "title")),
		"untitled",
	)
	updatedAt := firstNonEmpty(
		isoFromAny(meta["update_time"]),
		isoFromAny(meta["edit_time"]),
		isoFromAny(meta["latest_modify_time"]),
		isoFromAny(raw["update_time"]),
	)
	id := fsview.StableID(strings.ToLower(docType), token)
	return docItem{
		ID:         id,
		Token:      token,
		Type:       strings.ToLower(docType),
		EntityType: entityType,
		Title:      title,
		URL:        urlValue,
		UpdatedAt:  updatedAt,
		Raw:        raw,
	}, true
}

func (s *syncer) fetchDocMarkdown(token string) (string, string, error) {
	body := map[string]interface{}{
		"format": "markdown",
		"export_option": map[string]interface{}{
			"export_block_id":        false,
			"export_style_attrs":     false,
			"export_cite_extra_data": false,
		},
	}
	apiPath := fmt.Sprintf("/open-apis/docs_ai/v1/documents/%s/fetch", validate.EncodePathSegment(token))
	data, err := s.callAPI(http.MethodPost, apiPath, nil, body)
	if err != nil {
		return "", "", err
	}
	doc := getMap(data, "document")
	content := getString(doc, "content")
	title := getString(doc, "title")
	return content, title, nil
}

type chatItem struct {
	ID            string    `json:"id"`
	ChatID        string    `json:"chat_id"`
	Type          string    `json:"type"`
	Name          string    `json:"name"`
	Path          string    `json:"path"`
	Source        string    `json:"source"`
	IncludedSince string    `json:"included_since"`
	LastActiveAt  string    `json:"last_active_at,omitempty"`
	lastActive    time.Time `json:"-"`
}

func (s *syncer) syncIM() IMSyncResult {
	result := IMSyncResult{Enabled: true}
	activeSince := s.now.AddDate(0, 0, -s.opts.IMActiveDays)
	historySince := s.now.AddDate(0, 0, -s.opts.IMHistoryDays)

	chats, err := s.discoverActiveChats(activeSince, s.now)
	if err != nil {
		s.recordError("im", "discover", "", err)
		_ = s.writeNDJSON("im/chats.ndjson", nil)
		return result
	}
	s.enrichChats(chats)
	sort.Slice(chats, func(i, j int) bool { return chats[i].lastActive.After(chats[j].lastActive) })

	chatRows := make([]interface{}, 0, len(chats))
	for i := range chats {
		messages, err := s.fetchChatMessages(chats[i].ChatID, historySince, s.now)
		if err != nil {
			s.recordError("im", "messages_list", chats[i].ID, err)
			continue
		}
		s.resolveSenderNames(messages)
		if chats[i].Type == "dm" && (chats[i].Name == "" || chats[i].Name == "unknown") {
			chats[i].Name = inferDMName(messages, s.config.UserOpenId)
		}
		if chats[i].Name == "" {
			chats[i].Name = "unknown"
		}
		chats[i].Path = filepath.ToSlash(filepath.Join("im", "chats", fsview.ChatDirName(chats[i].Type, chats[i].Name, chats[i].ID)))
		if err := s.writeChat(chats[i], messages); err != nil {
			s.recordError("im", "write_chat", chats[i].ID, err)
			continue
		}
		result.Messages += len(messages)
		chatRows = append(chatRows, chats[i])
	}
	result.Chats = len(chatRows)
	if err := s.writeNDJSON("im/chats.ndjson", chatRows); err != nil {
		s.recordError("im", "write_index", "", err)
	}
	return result
}

func (s *syncer) discoverActiveChats(start, end time.Time) ([]chatItem, error) {
	seenChats := map[string]*chatItem{}
	pageToken := ""
	for page := 0; page < s.opts.IMActivePageLimit; page++ {
		body := map[string]interface{}{
			"query": "",
			"filter": map[string]interface{}{
				"time_range": map[string]interface{}{
					"start_time": start.Format(time.RFC3339),
					"end_time":   end.Format(time.RFC3339),
				},
			},
		}
		params := map[string]interface{}{"page_size": defaultMessageSearchPageSize}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		data, err := s.callAPI(http.MethodPost, "/open-apis/im/v1/messages/search", params, body)
		if err != nil {
			return nil, err
		}
		items, _ := data["items"].([]interface{})
		var messageIDs []string
		for _, raw := range items {
			m, _ := raw.(map[string]interface{})
			if chatID := firstNonEmpty(getString(m, "chat_id"), getString(getMap(m, "meta_data"), "chat_id")); chatID != "" {
				rememberChat(seenChats, chatID, start, time.Time{})
			}
			if msgID := getString(getMap(m, "meta_data"), "message_id"); msgID != "" {
				messageIDs = append(messageIDs, msgID)
			}
		}
		if len(messageIDs) > 0 {
			rawMessages, err := s.mgetMessages(messageIDs)
			if err != nil {
				s.recordError("im", "messages_mget", "", err)
			}
			for _, msg := range rawMessages {
				chatID := getString(msg, "chat_id")
				if chatID == "" {
					continue
				}
				rememberChat(seenChats, chatID, start, parseMessageTime(getString(msg, "create_time")))
			}
		}

		hasMore, _ := data["has_more"].(bool)
		next, _ := data["page_token"].(string)
		if !hasMore || next == "" {
			break
		}
		pageToken = next
	}
	chats := make([]chatItem, 0, len(seenChats))
	for _, chat := range seenChats {
		if !chat.lastActive.IsZero() {
			chat.LastActiveAt = chat.lastActive.Format(time.RFC3339)
		}
		chats = append(chats, *chat)
	}
	return chats, nil
}

func rememberChat(chats map[string]*chatItem, chatID string, includedSince time.Time, activeAt time.Time) {
	item := chats[chatID]
	if item == nil {
		item = &chatItem{
			ID:            fsview.StableID(chatID),
			ChatID:        chatID,
			Type:          "unknown",
			Name:          "unknown",
			Source:        "im.messages.search",
			IncludedSince: includedSince.Format(time.RFC3339),
		}
		chats[chatID] = item
	}
	if activeAt.After(item.lastActive) {
		item.lastActive = activeAt
		item.LastActiveAt = activeAt.Format(time.RFC3339)
	}
}

func (s *syncer) mgetMessages(ids []string) ([]map[string]interface{}, error) {
	var out []map[string]interface{}
	for _, batch := range chunkStrings(ids, 50) {
		data, err := s.callAPI(http.MethodGet, buildMGetURL(batch), nil, nil)
		if err != nil {
			return out, err
		}
		items, _ := data["items"].([]interface{})
		for _, raw := range items {
			if m, ok := raw.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func (s *syncer) enrichChats(chats []chatItem) {
	index := make(map[string]*chatItem, len(chats))
	var ids []string
	for i := range chats {
		index[chats[i].ChatID] = &chats[i]
		ids = append(ids, chats[i].ChatID)
	}
	for _, batch := range chunkStrings(ids, 50) {
		data, err := s.callAPI(
			http.MethodPost,
			"/open-apis/im/v1/chats/batch_query",
			map[string]interface{}{"user_id_type": "open_id"},
			map[string]interface{}{"chat_ids": batch},
		)
		if err != nil {
			s.recordError("im", "chats_batch_query", "", err)
			continue
		}
		items, _ := data["items"].([]interface{})
		for _, raw := range items {
			m, _ := raw.(map[string]interface{})
			chatID := getString(m, "chat_id")
			if chatID == "" || index[chatID] == nil {
				continue
			}
			mode := strings.ToLower(getString(m, "chat_mode"))
			switch mode {
			case "p2p":
				index[chatID].Type = "dm"
			case "group":
				index[chatID].Type = "group"
			default:
				if mode != "" {
					index[chatID].Type = mode
				}
			}
			if name := getString(m, "name"); name != "" {
				index[chatID].Name = name
			}
		}
	}
}

func (s *syncer) fetchChatMessages(chatID string, start, end time.Time) ([]map[string]interface{}, error) {
	var messages []map[string]interface{}
	pageToken := ""
	nameCache := map[string]string{}
	for page := 0; page < s.opts.IMMessagePageLimit; page++ {
		params := map[string]interface{}{
			"container_id_type":         "chat",
			"container_id":              chatID,
			"sort_type":                 "ByCreateTimeAsc",
			"page_size":                 defaultMessageListPageSize,
			"card_msg_content_type":     "raw_card_content",
			"only_thread_root_messages": "true",
			"start_time":                strconv.FormatInt(start.Unix(), 10),
			"end_time":                  strconv.FormatInt(end.Unix(), 10),
		}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		data, err := s.callAPI(http.MethodGet, "/open-apis/im/v1/messages", params, nil)
		if err != nil {
			return messages, err
		}
		items, _ := data["items"].([]interface{})
		for _, raw := range items {
			m, _ := raw.(map[string]interface{})
			msg := convertlib.FormatMessageItem(m, nil, nameCache)
			msg["chat_id"] = chatID
			if t := parseMessageTime(getString(m, "create_time")); !t.IsZero() {
				msg["create_time"] = t.Format(time.RFC3339)
			}
			messages = append(messages, msg)
		}
		hasMore, _ := data["has_more"].(bool)
		next, _ := data["page_token"].(string)
		if !hasMore || next == "" {
			break
		}
		pageToken = next
	}
	sort.Slice(messages, func(i, j int) bool {
		ti, _ := parseRFC3339(getString(messages[i], "create_time"))
		tj, _ := parseRFC3339(getString(messages[j], "create_time"))
		return ti.Before(tj)
	})
	return messages, nil
}

func (s *syncer) resolveSenderNames(messages []map[string]interface{}) {
	seen := map[string]bool{}
	var missing []string
	for _, msg := range messages {
		sender := getMap(msg, "sender")
		if strings.ToLower(getString(sender, "sender_type")) != "user" {
			continue
		}
		id := getString(sender, "id")
		if id == "" || !strings.HasPrefix(id, "ou_") || getString(sender, "name") != "" || seen[id] {
			continue
		}
		seen[id] = true
		missing = append(missing, id)
	}
	nameMap := map[string]string{}
	for _, batch := range chunkStrings(missing, 10) {
		data, err := s.callAPI(
			http.MethodPost,
			"/open-apis/contact/v3/users/basic_batch",
			map[string]interface{}{"user_id_type": "open_id"},
			map[string]interface{}{"user_ids": batch},
		)
		if err != nil {
			s.recordError("im", "resolve_sender_names", "", err)
			break
		}
		users, _ := data["users"].([]interface{})
		for _, raw := range users {
			u, _ := raw.(map[string]interface{})
			id := getString(u, "user_id")
			name := getString(u, "name")
			if id != "" && name != "" {
				nameMap[id] = name
			}
		}
	}
	for _, msg := range messages {
		sender := getMap(msg, "sender")
		id := getString(sender, "id")
		if name := nameMap[id]; name != "" {
			sender["name"] = name
		}
	}
}

func inferDMName(messages []map[string]interface{}, selfOpenID string) string {
	for _, msg := range messages {
		sender := getMap(msg, "sender")
		id := getString(sender, "id")
		name := getString(sender, "name")
		if id != "" && id != selfOpenID && name != "" {
			return name
		}
	}
	return "unknown"
}

func (s *syncer) writeChat(chat chatItem, messages []map[string]interface{}) error {
	if err := s.writeJSON(filepath.ToSlash(filepath.Join(chat.Path, "meta.json")), chat); err != nil {
		return err
	}
	byDate := map[string][]map[string]interface{}{}
	for _, msg := range messages {
		t, ok := parseRFC3339(getString(msg, "create_time"))
		if !ok {
			continue
		}
		msg["chat_type"] = chat.Type
		msg["chat_name"] = chat.Name
		byDate[t.Local().Format("2006-01-02")] = append(byDate[t.Local().Format("2006-01-02")], msg)
	}
	var dates []string
	for date := range byDate {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	for _, date := range dates {
		parts := strings.Split(date, "-")
		dayDir := filepath.Join(chat.Path, parts[0], parts[1])
		ndjsonPath := filepath.ToSlash(filepath.Join(chat.Path, ".data", date+".ndjson"))
		if err := s.writeNDJSON(ndjsonPath, mapsToAny(byDate[date])); err != nil {
			return err
		}
		md := fsview.RenderChatDayMarkdown(chat.Name, date, toRenderMessages(byDate[date]))
		if err := s.writeFile(filepath.ToSlash(filepath.Join(dayDir, date+".md")), []byte(md)); err != nil {
			return err
		}
	}
	return nil
}

func toRenderMessages(messages []map[string]interface{}) []fsview.ChatMessage {
	out := make([]fsview.ChatMessage, 0, len(messages))
	for _, msg := range messages {
		t, _ := parseRFC3339(getString(msg, "create_time"))
		sender := getMap(msg, "sender")
		out = append(out, fsview.ChatMessage{
			CreateTime: t,
			SenderName: getString(sender, "name"),
			SenderID:   getString(sender, "id"),
			MsgType:    getString(msg, "msg_type"),
			Content:    getString(msg, "content"),
		})
	}
	return out
}

func (s *syncer) writeJSON(rel string, value interface{}) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return output.Errorf(output.ExitInternal, "internal_error", "marshal %s: %s", rel, err)
	}
	b = append(b, '\n')
	return s.writeFile(rel, b)
}

func (s *syncer) writeNDJSON(rel string, rows []interface{}) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return output.Errorf(output.ExitInternal, "internal_error", "marshal %s: %s", rel, err)
		}
	}
	return s.writeFile(rel, b.Bytes())
}

func (s *syncer) writeFile(rel string, data []byte) error {
	target := filepath.Join(s.outputDir, filepath.FromSlash(rel))
	if err := vfs.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return output.Errorf(output.ExitInternal, "file_error", "create parent for %s: %s", rel, err)
	}
	if err := localfileio.AtomicWrite(target, data, 0600); err != nil {
		return output.Errorf(output.ExitInternal, "file_error", "write %s: %s", rel, err)
	}
	return nil
}

func buildMGetURL(ids []string) string {
	parts := make([]string, 0, len(ids)+1)
	parts = append(parts, "card_msg_content_type=raw_card_content")
	for _, id := range ids {
		parts = append(parts, "message_ids="+url.QueryEscape(id))
	}
	return "/open-apis/im/v1/messages/mget?" + strings.Join(parts, "&")
}

func mapsToAny(items []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(items))
	for i := range items {
		out[i] = items[i]
	}
	return out
}

func chunkStrings(items []string, size int) [][]string {
	if len(items) == 0 || size <= 0 {
		return nil
	}
	chunks := make([][]string, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		chunks = append(chunks, items[start:end])
	}
	return chunks
}

func cloneMap(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if m == nil {
		return nil
	}
	child, _ := m[key].(map[string]interface{})
	return child
}

func getString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		if v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" && strings.TrimSpace(v) != "<nil>" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func normalizeDocType(v interface{}) string {
	switch typed := v.(type) {
	case string:
		return strings.ToUpper(strings.TrimSpace(typed))
	case []interface{}:
		for _, item := range typed {
			if s := normalizeDocType(item); s == "DOC" || s == "DOCX" {
				return s
			}
		}
	case []string:
		for _, item := range typed {
			if s := normalizeDocType(item); s == "DOC" || s == "DOCX" {
				return s
			}
		}
	}
	return ""
}

func docTypeFromURL(raw string) string {
	if strings.Contains(raw, "/docx/") {
		return "DOCX"
	}
	if strings.Contains(raw, "/doc/") {
		return "DOC"
	}
	return ""
}

func extractDocToken(raw string) string {
	for _, marker := range []string{"/docx/", "/doc/", "/wiki/"} {
		if idx := strings.Index(raw, marker); idx >= 0 {
			token := raw[idx+len(marker):]
			if end := strings.IndexAny(token, "/?#"); end >= 0 {
				token = token[:end]
			}
			return strings.TrimSpace(token)
		}
	}
	return ""
}

var highlightTagRe = regexp.MustCompile(`</?h>`)

func stripHighlightTags(s string) string {
	return strings.TrimSpace(html.UnescapeString(highlightTagRe.ReplaceAllString(s, "")))
}

func isoFromAny(v interface{}) string {
	var n int64
	switch typed := v.(type) {
	case nil:
		return ""
	case string:
		typed = strings.TrimSpace(typed)
		if typed == "" {
			return ""
		}
		if t, err := time.Parse(time.RFC3339, typed); err == nil {
			return t.Format(time.RFC3339)
		}
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return ""
		}
		n = parsed
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return ""
		}
		n = parsed
	case float64:
		n = int64(typed)
	case int64:
		n = typed
	case int:
		n = int64(typed)
	default:
		return ""
	}
	if n <= 0 {
		return ""
	}
	if n >= 1e12 {
		n /= 1000
	}
	return time.Unix(n, 0).Local().Format(time.RFC3339)
}

func parseMessageTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	if n >= 1e12 {
		return time.UnixMilli(n).Local()
	}
	return time.Unix(n, 0).Local()
}

func parseRFC3339(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
