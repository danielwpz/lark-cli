# Lark VFS POC 设计

## 背景

`lark-cli` 已经暴露了大量飞书能力，但不同领域的命令、参数和输出结构并不统一。AI agent 在使用时经常需要先探索命令、查 usage、试参数，再把不同命令的结果手动拼起来。

这个 POC 的目标是把飞书里最接近“文本文件”的资源暴露成一个只读虚拟文件系统，让 agent 可以直接用熟悉的文件工具工作：

```bash
rg "关键词" /mnt/lark
jq -c 'select(.sender.name == "Alice")' /mnt/lark/im/chats/group__项目A__a1b2c3d4/.data/2026-05-09.ndjson
cat /mnt/lark/docs/folders/个人文档__f1e2d3c4/项目A/需求说明__d9e8f7a6.md
```

POC 不追求覆盖所有飞书 API。它只覆盖可枚举、可缓存、主要是文本内容的资源集合。

## POC 范围

第一版只做两类资源：

- 飞书文档：能被当前身份通过搜索发现的 Word 类文档，包含新版文档、旧版文档、Wiki 中实际指向文档的节点，统一以 Markdown 落到本地。
- 飞书聊天消息：自动发现最近 N 天内有消息活动的会话，再为这些会话同步最近 M 天消息。

第一版明确不做：

- 写入、编辑、删除、移动。
- 联系人搜索目录。
- 动态 `/search/<query>` 目录。
- 附件、图片、音视频二进制内容。
- 电子表格、多维表格、幻灯片、普通文件和其它非 Word 类文档的内部结构读取。
- 全租户、全历史、全聊天记录扫描。

## 与现有 CLI 能力的映射

### 文档发现

已有能力：

- `lark-cli drive +search`
  - 使用 Search v2：`/open-apis/search/v2/doc_wiki/search`
  - 可按 `--opened-since`、`--edited-since`、`--created-since`、`--folder-tokens`、`--space-ids`、`--doc-types` 过滤。
  - 适合做“当前身份可搜索到的 Word 类文档”的主发现入口。
- `lark-cli drive files list`
  - 使用 Drive folder listing：`/open-apis/drive/v1/files`
  - 可列指定 `folder_token` 下的文件夹内容。
  - repo 内已有 `shortcuts/drive/list_remote.go` 的递归 listing 逻辑，可复用思路。
- `lark-cli wiki spaces get_node`
  - 可把 `/wiki/<token>` 节点解析为真实 `obj_type` 和 `obj_token`。

设计约束：

- `drive +search` 返回的是搜索结果集合，不保证真实父子路径。
- `drive files list` 返回真实文件夹树，但只覆盖用户指定的 folder scope。
- Live probe 已验证：`drive +search --query "" --doc-types doc,docx` 可以返回当前身份可搜索到的 Word 类文档；返回结果中 `entity_type` 可能是 `DOC` 或 `WIKI`，`result_meta.doc_types` 表示真实文档类型。
- 不要把 `wiki` 当成 `--doc-types` 的主过滤值；本地验证中 `--doc-types wiki` 返回 0。Wiki 文档应通过 `entity_type=WIKI` 且 `result_meta.doc_types=DOC/DOCX` 识别。
- Wiki URL 可直接交给 `docs +fetch --api-version v2`；fetch 返回的 `document.document_id` 是真实文档 token。
- 指向 sheet、bitable、slides、file 等类型时跳过。
- 因此 VFS 需要同时支持“发现视图”和“指定文件夹真实树视图”。

### 文档读取

已有能力：

- `lark-cli docs +fetch --api-version v2`
  - 使用 `/open-apis/docs_ai/v1/documents/{token}/fetch`
  - 支持 `--doc-format xml|markdown|text`
  - 支持 `--detail simple|with-ids|full`
  - 支持全文、outline、range、keyword、section 局部读取。

POC 默认：

```bash
lark-cli docs +fetch --api-version v2 --doc-format markdown --detail simple --scope full
```

原因：

- Markdown 更适合 `cat`、`rg` 和人读。
- `simple` 不带 block id 和复杂样式，正文更稳定。
- 需要精确编辑时才应该使用 XML 和 block id；本 POC 不做写入。

### 聊天发现与读取

已有能力：

- `lark-cli im chats list`
  - 可列当前 user/bot 所在群。
  - 主要适合群聊集合发现。
- `lark-cli im +chat-search`
  - 可按关键词或成员搜索可见群聊。
- `lark-cli im +chat-messages-list`
  - 可按 `chat_id` 或 `user_id` 拉某个会话消息。
  - 支持 `--start`、`--end`、`--sort`、`--page-size`、`--page-token`。
  - 内部会把消息内容转为可读文本，解析 sender name，并展开部分 thread replies。
- `lark-cli im +messages-search`
  - 可跨聊天按关键词搜索消息。
  - Live probe 已验证：空 query + time_range 可用来发现最近 N 天内有消息活动的会话，并能返回 group 和 p2p。
  - 搜索结果可提供 `chat_id`、`chat_type`、群聊 `chat_name`、P2P 的 `chat_partner.open_id`。

设计约束：

- POC 的默认目标是自动发现活跃会话，而不是要求用户手工传入每个 `chat_id`。
- 活跃会话发现窗口与消息同步窗口分开：
  - `--im-active-days 3`：最近 3 天内出现过消息的会话会进入同步范围。
  - `--im-history-days 7`：对进入范围的每个会话，同步最近 7 天消息。
- 推荐发现路径：调用 `im +messages-search` 的底层搜索 API，使用空 query + `time_range`，分页收集返回消息里的唯一 `chat_id`。
- `im +messages-search` 只用于发现活跃会话，不用于生成会话正文、不用于判断某个会话的最新消息。正文同步必须对每个 `chat_id` 再调用 `im +chat-messages-list` 或底层 `/open-apis/im/v1/messages`，按 `ByCreateTimeAsc/Desc` 的时间线结果落盘。
- 备选发现路径：使用 `im chats list --params {"sort_type":"ByActiveTimeDesc"}` 获取活跃排序群聊，再逐个用 `im +chat-messages-list` 检查活跃窗口内是否有消息。这个路径对 DM 覆盖较弱，且 active sort 分页存在官方提示的遗漏风险。
- `im +messages-search` 的 shortcut 自动分页上限最大 40 页；实现时应暴露 `--im-active-page-limit`，避免大租户或长窗口下无限扫描。
- P2P 目录命名需要降级策略：优先从消息 sender name 推断对方显示名；其次尝试 `contact +get-user`；如果通讯录可见性导致返回空对象，则使用 `dm__unknown__<stable-id>`。
- Live probe 纠偏：同一群内“最新消息”必须以 messages list 的毫秒级 `create_time` 为准；搜索结果中的格式化分钟时间和返回顺序不足以判断最新消息，尤其是 text/card/system 在同一分钟内连续出现时。

## 目录结构

整体结构：

```text
/mnt/lark/
  .meta/
    capabilities.json
    snapshot.json
    errors.ndjson

  docs/
    index.ndjson
    all/
    folders/
    .data/

  im/
    chats.ndjson
    chats/
```

### 元信息目录

```text
/mnt/lark/.meta/
  capabilities.json
  snapshot.json
  errors.ndjson
```

- `capabilities.json`：记录本次 mount/sync 启用了哪些资源、身份、scope、刷新策略。
- `snapshot.json`：记录当前快照版本、生成时间、参数、缓存目录、最后刷新时间。
- `errors.ndjson`：记录后台刷新、单文件读取、分页等错误。每行一个 JSON object。

示例：

```json
{"time":"2026-05-09T14:20:00+08:00","resource":"docs","op":"fetch","path":"docs/all/2026-05/产品方案__d9e8f7a6.md","error":"permission denied"}
```

### 文档目录

```text
/mnt/lark/docs/
  index.ndjson

  all/
    2026-05/
      产品方案__d9e8f7a6.md
      周会记录__a1b2c3d4.md

  folders/
    个人文档__f1e2d3c4/
      项目A/
        需求说明__b2c3d4e5.md
        会议纪要__c3d4e5f6.md

  .data/
    d9e8f7a6.meta.json
    d9e8f7a6.raw.json
```

路径命名：

- 文件名格式：`<sanitized-title>__<stable-id>.md`
- `stable-id` 默认由 `type + token` hash 得到，避免把完整 token 放进路径。
- 完整 token、URL、类型、更新时间记录在 `index.ndjson` 和 `.data/*.meta.json`。

`docs/index.ndjson` 每行一个文档：

```json
{"id":"d9e8f7a6","token":"doxcnxxx","type":"docx","entity_type":"DOC","title":"产品方案","url":"https://example.feishu.cn/docx/doxcnxxx","canonical_path":"docs/all/2026-05/产品方案__d9e8f7a6.md","views":["all"],"updated_at":"2026-05-09T10:20:00+08:00"}
```

去重规则：

- 同一个 `type + token` 只应该有一个 canonical Markdown 正文文件。
- `all/` 是主视图，来自 `drive +search --query "" --doc-types doc,docx` 的可搜索 Word 文档集合。
- 如果文档同时出现在 `all/` 和 `folders/`：
  - 第一版优先把正文 canonical path 放在 `all/`，因为它不依赖用户传 folder token。
  - `folders/` 可作为可选真实路径视图；如果实现时无法做硬链接/软链接，就先只在 `index.ndjson` 里记录 folder path，避免正文重复命中。
- 如果同一文档出现在多个 folder 路径，第一版选择一个 deterministic canonical path，其他路径先不暴露正文，避免 `rg /mnt/lark/docs` 重复命中。

### 聊天目录

```text
/mnt/lark/im/
  chats.ndjson

  chats/
    group__项目A群__a1b2c3d4/
      meta.json
      2026/
        05/
          2026-05-09.md
          2026-05-08.md
      .data/
        2026-05-09.ndjson
        2026-05-08.ndjson

    dm__张三__e5f6a7b8/
      meta.json
      2026/
        05/
          2026-05-09.md
      .data/
        2026-05-09.ndjson

    unknown__某会话__c1d2e3f4/
      meta.json
      2026/
        05/
          2026-05-09.md
      .data/
        2026-05-09.ndjson
```

目录命名：

- 群聊：`group__<sanitized-chat-name>__<stable-id>/`
- DM/P2P：`dm__<sanitized-display-name>__<stable-id>/`
- 类型无法确认：`unknown__<sanitized-name>__<stable-id>/`
- `stable-id` 由 `chat_id` hash 得到，完整 `chat_id` 记录在 `meta.json` 和 `chats.ndjson`。

`im/chats.ndjson` 每行一个会话：

```json
{"id":"a1b2c3d4","chat_id":"oc_xxx","type":"group","name":"项目A群","path":"im/chats/group__项目A群__a1b2c3d4","source":"im.chats.list","included_since":"2026-05-02T00:00:00+08:00"}
```

`meta.json` 示例：

```json
{"id":"a1b2c3d4","chat_id":"oc_xxx","type":"group","name":"项目A群","external":false,"owner_id":"ou_xxx","last_synced_at":"2026-05-09T14:20:00+08:00"}
```

每日 Markdown 文件示例：

```markdown
# 项目A群 / 2026-05-09

## 10:03 Alice

今天的方案我放到文档里了：https://example.feishu.cn/docx/doxcnxxx

## 10:05 Bob

收到，我下午看。
```

每日 NDJSON 文件示例：

```json
{"message_id":"om_xxx","chat_id":"oc_xxx","chat_type":"group","chat_name":"项目A群","create_time":"2026-05-09T10:03:00+08:00","sender":{"open_id":"ou_xxx","name":"Alice","type":"user"},"msg_type":"text","content":"今天的方案我放到文档里了：https://example.feishu.cn/docx/doxcnxxx","message_app_link":"https://...","deleted":false,"updated":false}
```

为什么同时提供 `.md` 和 `.ndjson`：

- `.md` 适合 `rg`、`cat` 和直接阅读。
- `.ndjson` 适合 `jq`、程序处理、保留 message id / sender / link / type。
- NDJSON 和 JSONL 本质上是同一种“一行一个 JSON 值”的格式；本项目已有 `--format ndjson`，所以扩展名统一使用 `.ndjson`。

## 搜索语义

VFS 不提供动态搜索目录。

不做：

```text
/mnt/lark/docs/search/<query>/
/mnt/lark/im/search/<query>/
```

原因：

- `ls` 的语义不稳定。
- 搜索 API 是动作，不是资源集合。
- agent 已经擅长使用 `rg` 和 `jq`。

推荐搜索方式：

```bash
rg "关键词" /mnt/lark/docs
rg "关键词" /mnt/lark/im/chats/group__项目A群__a1b2c3d4
jq -c 'select(.msg_type == "text" and (.content | contains("关键词")))' /mnt/lark/im/chats/group__项目A群__a1b2c3d4/.data/2026-05-09.ndjson
```

## 缓存与 `rg` 行为

`rg` 的行为大致是：

1. 递归 `readdir`。
2. 对候选文件做 `stat`。
3. 打开文件并读取内容。
4. 在本地进程内做文本匹配。

这意味着：

- 不需要把全部正文常驻内存。
- 但文件列表必须先能枚举出来。
- 如果正文内容没有缓存，顶层 `rg /mnt/lark` 会触发大量按需网络读取，体验会很差。

POC 缓存策略：

- 内存 manifest：路径、资源 id、token/chat_id、mtime、大小估计、cache key。
- 磁盘内容缓存：文档 Markdown、每日消息 Markdown、每日消息 NDJSON。
- 默认对配置范围做后台预热，保证 `rg` 大概率命中本地缓存。
- 读取时如果缓存缺失，可以懒加载；但需要并发限制和超时。
- 缓存写入使用临时文件 + 原子 rename，避免 `rg` 读到半截文件。

建议默认缓存目录：

```text
~/Library/Caches/lark-cli/vfs/<profile>/
```

跨平台实现时再按 OS 调整。

## 更新与一致性

第一版采用 snapshot + polling，不承诺强实时一致性。

文档：

- `drive +search` 和 `drive files list` 可提供更新时间字段。
- 后台定期刷新 manifest。
- 如果远端更新时间晚于本地 cache，重新 `docs +fetch`。
- 默认刷新间隔可以从 5 分钟开始。

聊天：

- 先用活跃窗口发现会话，再对每个活跃会话按历史窗口增量拉取。
- 默认可从 `--im-active-days 3` 和 `--im-history-days 7` 开始。
- 当前日期文件可更频繁刷新，例如 30-60 秒。
- 历史日期文件可视为低频刷新或固定 snapshot。

事件：

- repo 已有 `event consume` 和 IM 事件总线能力。
- IM 新消息可以作为未来优化，用事件驱动追加当天文件。
- 文档内容更新第一版不依赖事件，先用 polling。

一致性原则：

- VFS 是只读的，文件内容代表“最近一次成功同步的快照”。
- `.meta/snapshot.json` 必须能说明这个快照是什么时候生成的。
- 远端 API 失败时保留旧缓存，并在 `.meta/errors.ndjson` 记录错误。

## Mount 启动性能策略

`fs mount` 的启动关键路径必须很短，目标 5 秒内可用，最多 10 秒内必须完成挂载。

因此 mount 采用 stale-while-revalidate：

- 启动时只创建虚拟树基础目录、读取本地 manifest cache、启动本机 mount backend。
- 不在启动关键路径上等待 Drive 递归、Search v2、IM 活跃会话发现、文档正文 fetch 或消息历史拉取。
- 如果有 manifest cache，挂载点立即显示上次成功发现的 `docs/drive`、`docs/all/by-updated-month` 和 `im/chats`。
- 如果没有 manifest cache，挂载点先显示基础目录，后台刷新完成后目录逐步出现。
- 后台刷新成功后写回 manifest cache，下一次 mount 可秒级恢复目录树。

缓存分层：

- manifest cache：`<cache-dir>/manifests/docs.json`、`<cache-dir>/manifests/im.json`，保存可枚举的目录树和资源 metadata。
- object cache：`<cache-dir>/objects/...`，保存文档 Markdown、聊天消息 JSON 等内容对象。
- 内存 manifest：当前 mount session 的 path -> virtual node 映射；`readdir/stat` 必须走内存，不打网络。

并发策略：

- Docs metadata 刷新时，Drive 真实目录递归和 Search v2 补充发现并发执行。
- Docs 正文 prefetch 使用 worker pool，当前 POC 默认 8 并发。
- IM message prefetch 使用 worker pool，当前 POC 默认 4 并发。
- 单文件读取仍然支持 lazy fetch；如果用户先打开未缓存文件，只拉该文件或该 chat 的窗口消息。

启动验证记录：

- 第一次无 manifest cache 启动：`fs mount` 在约 1 秒内输出 mounted，后台继续发现并填充目录。
- 后台 docs refresh 发现 65 个文档，并写入 manifest cache。
- 第二次有 manifest cache 启动：`fs mount` 在约 1 秒内输出 mounted，`docs/drive` 和 `im/chats` 立即可见。

## 建议命令形态

第一阶段先做同步，不做真实 mount：

```bash
lark-cli fs sync \
  --output-dir ~/.cache/larkfs \
  --docs \
  --docs-folder-token fld_xxx \
  --im-active-days 3 \
  --im-history-days 7 \
  --im-active-page-limit 40
```

原因：

- 先验证目录结构、缓存、格式和 `rg` 体验。
- 不需要引入 FUSE 依赖。
- 测试和调试成本低。

第二阶段再做 mount：

```bash
lark-cli fs mount /mnt/lark \
  --backend webdav \
  --docs \
  --docs-prefetch all \
  --im-active-days 3 \
  --im-history-days 3 \
  --im-prefetch all
```

`mount` 是用户主入口。它不要求用户先执行 `fs sync`，也不把一个已有本地目录再挂载一次。命令内部应该：

- 发现远端资源 metadata，构建内存 manifest。
- 按 prefetch 策略后台拉正文/消息。
- 在读取单个文件时 lazy fetch 未缓存内容。
- 使用内部 cache 加速内容读取，但 cache 不是用户要手工操作的 source tree。
- 在 macOS POC 中使用 WebDAV 作为挂载后端，由 `fs mount` 内部启动本机 WebDAV server 并调用 `mount_webdav`。

可选刷新命令：

```bash
lark-cli fs refresh --cache-dir ~/.cache/larkfs
```

## 2026-05-10 mount 探测记录

本轮已经完成 `fs sync` 和 cache-backed mount 的第一版实现探测：

- `lark-cli fs sync` 已能把 docs/im 同步成真实本地文件树。
- 扩大范围 live sync 验证通过：27 个文档、10 个最近活跃聊天、512 条消息、0 个同步错误。
- 本地 `rg`/`jq` 直接扫 sync 输出目录可用。
- 当时探测版本曾尝试 `--backend fskit/kernel`；当前实现已收敛为 `--backend auto|webdav`。
- macFUSE 5.2.0 已安装，`/Library/Filesystems/macfuse.fs` 存在。
- `/Volumes/LarkFS` 已创建，属于当前用户，可作为 FSKit 挂载点。

但真实 mount 尚未跑通：

- `--backend fskit`：`go-fuse` 进程进入等待状态，但系统 `mount` 表中没有出现 LarkFS，`/Volumes/LarkFS` 仍为空目录。
- `--backend kernel`：现象相同，进程等待但未完成系统挂载。
- 未留下残留挂载；探测后确认 `mount` 表中没有 LarkFS，`/Volumes/LarkFS` 仍是普通空目录。

当前判断：

- 问题不在飞书数据链路、不在目录渲染、不在 cache 内容。
- 问题集中在 macOS mount 层：当前 Go 依赖 `github.com/hanwen/go-fuse/v2` 的 Darwin 实现仍调用 `mount_macfuse` helper，并没有接入 macFUSE 5.2 新增的 `MFMount.framework`/FSKit XPC mount API。
- `mount_macfuse --help` 未显示 `backend=fskit`，而 macFUSE 5.2 的新 `macfuse mount` 子命令提示“not meant to be called directly”，需要 FUSE user-space library 传入 mount communication socket。

下一步建议：

1. 先不要继续卡在 `go-fuse` + macFUSE 的 native mount。
2. 优先做 `lark-cli fs webdav`：把同一份 cache 通过本机 WebDAV server 暴露，再用 macOS 自带 `mount_webdav` 挂载。
3. WebDAV 路线不需要 macFUSE、不需要 FSKit、不需要 Reduced Security，足够验证“真实挂载点 + `ls/cat/rg`”体验。
4. 如果 WebDAV 体验不够，再评估是否 patch/fork `go-fuse`，或单独接 macFUSE 5.2 的 `MFMount.framework`。这部分不要混入当前 WebDAV POC 提交。

## 2026-05-11 WebDAV 探测记录与修正

WebDAV backend 探测结果：

- WebDAV 可以作为 macOS POC 的挂载传输层。
- macOS 自带 `mount_webdav -S -o rdonly` 可以把该服务挂载成真实本地卷，不依赖 macFUSE/FSKit。
- fixture cache 验证通过：`find`、`sed`、`rg` 能通过挂载点读取 `.meta/snapshot.json`、文档 Markdown 和聊天 Markdown。
- 只读语义验证通过：HTTP `PUT` 返回 403；挂载点内 `touch` 返回 `Read-only file system`。
- 已确认测试后无残留挂载；WebDAV 服务可正常停止。

重要修正：

- “把 `fs sync` 生成的本地目录通过 WebDAV 再挂载一次”只能证明 WebDAV backend 可用，不能作为 VFS 产品实现。
- 真正的 `fs mount` 必须直接创建 LarkFS session，并让 backend 读取虚拟树。
- cache 只能作为内部内容缓存，不能成为用户手工准备的 mount source。

额外观察：

- `rg` 会递归探测 `.gitignore`、`.ignore`、`.rgignore` 等文件。WebDAV server 不应把这类正常 404 全部打印到 stderr，否则扫描时日志会很吵。
- `rg` 顶层扫描会打开大量文件。目录枚举和 stat 必须来自内存 manifest；正文读取可以 lazy fetch，但应有后台 prefetch 和本地对象缓存支撑。

当前判断：

- 对 POC 来说，WebDAV 已经足够验证核心目标：把飞书文档和聊天记录暴露成可被 `ls`、`cat`、`rg`、`jq` 访问的真实文件树。
- 原生 FUSE/macfuse 仍可作为后续优化，但不再是推进 POC 的阻塞项。

## 2026-05-11 mount 重做记录

已将 `fs mount` 改为 session-driven VFS：

- `fs mount` 不再要求已有 `.meta/snapshot.json`，也不再把 `--cache-dir` 当成要暴露的目录。
- `--cache-dir` 现在是内部对象缓存目录，用于缓存已拉取的文档正文和聊天消息。
- mount 启动时先拉 docs/im metadata，构建内存虚拟树。
- 文档正文文件是 lazy file：第一次读取时通过 Docs AI fetch 拉 Markdown，并写入对象缓存。
- 聊天日文件也是 lazy file：第一次读取某个 chat/day 时拉该 chat 历史窗口内消息，生成 Markdown 和 NDJSON。
- `--docs-prefetch metadata|recent|all` 控制文档正文后台预取。
- `--im-prefetch metadata|all` 控制聊天消息后台预取。
- `--backend auto` 当前解析为 `webdav`；native fskit/kernel backend 只有在能直接读取虚拟树后才应该重新开放。

本地 live 验证：

- 直接执行 `fs mount /private/tmp/larkfs-vfs --docs --docs-page-limit 100 --im --im-active-days 3 --im-history-days 3 --docs-prefetch metadata --im-prefetch metadata`，未先执行 `fs sync`。
- mount session 发现 34 个 Word 类文档 metadata、22 个最近三天活跃聊天，0 个发现错误。
- `mount` 显示 `/private/tmp/larkfs-vfs` 是 read-only WebDAV 卷。
- `sed /private/tmp/larkfs-vfs/.meta/snapshot.json`、`sed docs/index.ndjson` 可直接读取 metadata。
- `sed <doc>.md` 可触发文档正文 lazy fetch 并读出内容。
- `sed <chat-day>.md` 可触发聊天消息 lazy fetch，并正确渲染 card 消息文本。
- 测试后已卸载并停止后台 mount 进程。

## 实现建议

放在当前 repo 内实现，不长期 fork，也不新开项目 shell out 调 CLI。

原因：

- 认证、profile、身份切换、scope 检查、API client 都已经在本 repo。
- VFS 需要直接复用 `internal/cmdutil.Factory`、`internal/client.APIClient` 和 shortcut 里的请求构造逻辑。
- 如果用外部项目不断执行 `lark-cli ...` 子进程，会在 `cat`/`rg`/`stat` 时遇到启动开销、stderr notice、分页、JSON 解析和错误边界问题。

建议包结构：

```text
cmd/fs/
  sync.go
  mount.go
  webdav.go
  session.go
  virtual_tree.go
  refresh.go

internal/fsview/
  model.go
  paths.go
  render_docs.go
  render_im.go

internal/fscache/
  cache.go
  manifest.go
  atomic_write.go
```

FUSE 依赖后续如果重启，应隔离在单独包和 build tag 中：

```go
//go:build darwin || linux
```

第一阶段 `fs sync` 可以不引入 FUSE。

## 文件与路径规则

- 所有路径都必须经过安全清洗，避免 `/`、控制字符、`..`、过长文件名。
- 文件名保留可读标题，但用 hash 后缀保证稳定和去重。
- 完整 token 和 chat_id 不默认暴露在路径中，放入 `.meta` / `.data` / index 文件。
- 缓存文件权限应尽量使用 `0600`，缓存目录使用 `0700`。
- stdout 保持数据输出；进度、刷新提示、缓存命中率等写 stderr。
- 命令错误继续使用 `output.Errorf` / `output.ErrWithHint`，不要返回裸 `fmt.Errorf`。

## 验证标准

POC 做完后，至少要验证：

- `lark-cli fs sync` 能生成真实本地目录。
- `rg` 能搜到文档正文。
- `rg` 能搜到最近活跃群聊/DM 的历史窗口内消息。
- `jq` 能处理每日 `.ndjson`。
- 文档和消息 API 失败时，不破坏已有缓存。
- 重跑 sync 不产生重复文件和重复消息。
- 同一个文档同时出现在 `all/` 和 folder scope 时，不造成正文重复命中。

手工体验命令：

```bash
rg "关键词" ~/.cache/larkfs/docs
rg "关键词" ~/.cache/larkfs/im
jq -c 'select(.sender.name == "Alice")' ~/.cache/larkfs/im/chats/group__项目A群__a1b2c3d4/.data/2026-05-09.ndjson
```

## 后续扩展

可在 POC 稳定后考虑：

- IM 事件驱动刷新当天消息。
- 文档更新事件或更细粒度 polling。
- 附件索引和按需下载目录。
- 联系人只读集合，例如 `/contacts/people.ndjson`，前提是能可靠枚举。
- Sheets/Base 的导出视图，而不是内部编辑视图。
- 可写 VFS，但需要单独设计并发、冲突、远端 revision 和失败回滚。
