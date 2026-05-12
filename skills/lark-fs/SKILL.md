---
name: lark-fs
version: 1.0.0
description: "飞书 LarkFS 只读虚拟文件系统：把用户可访问的飞书云文档和最近聊天记录挂载成本地文件树，方便 AI Agent 用 ls/find/rg/cat/sed/jq 搜索和读取。用户需要开启、mount、卸载 LarkFS，跨多个飞书文档或聊天做全文搜索，按真实云空间目录浏览文档，或把最近聊天记录当文件读取时使用；需要创建/编辑文档、发送消息、下载聊天附件、管理权限或执行精确 API 操作时不要使用本 skill，应改用 lark-doc、lark-im 或 lark-drive。"
metadata:
  requires:
    bins: ["lark-cli"]
  cliHelp: "lark-cli fs --help; lark-cli fs mount --help; lark-cli fs sync --help"
---

# LarkFS

LarkFS 是一个只读 POC，把飞书文档和最近聊天记录暴露为本地文件树。它的价值不是替代所有飞书 API，而是让 Agent 在“需要搜索很多材料”时直接使用文件系统工具。

**CRITICAL — 开始前 MUST 先用 Read 工具读取 [`../lark-shared/SKILL.md`](../lark-shared/SKILL.md)，其中包含认证、身份和权限处理。**

## 适用边界

优先使用 LarkFS：

- 用户要求“在我的飞书文档/聊天记录里搜一下”“找相关材料”“用 rg/grep 搜飞书内容”。
- 需要同时搜索文档和聊天，或需要跨多个文档/多个会话做关键词检索。
- 需要按真实云空间目录浏览 Word 类文档，并把文档作为 Markdown 读取。
- 需要读取最近几天活跃聊天的历史消息，并用 Markdown/NDJSON 分析。

不要使用 LarkFS：

- 只读取一个已知文档 URL/token：用 `lark-doc` 的 `docs +fetch`。
- 创建或编辑文档：用 `lark-doc`。
- 发送、回复、转发消息，下载聊天里的图片/文件：用 `lark-im`。
- 管理云空间文件、权限、评论：用 `lark-drive`。
- 用户明确要一个真实本地副本/snapshot，而不是 mount：用 `lark-cli fs sync`。

## 前置条件

- 当前 POC 的自动 mount backend 使用 macOS 自带 WebDAV：优先在 macOS 上使用 `fs mount`。
- 必须使用用户身份访问个人云空间和聊天记录。命令默认会以 `--as user` 运行；遇到权限错误时按 `lark-shared` 的 user 授权流程处理。
- `fs mount` 是长运行命令：进程必须保持运行，挂载点才可继续访问。
- mount 是只读的。不要尝试在挂载点里创建、修改或删除文件。

## 推荐启动方式

先检查是否已经挂载，避免重复启动：

```bash
mount | rg '/private/tmp/larkfs|LarkFS'
```

如果没有现成挂载，创建挂载点并启动：

```bash
mkdir -p /private/tmp/larkfs
lark-cli fs mount /private/tmp/larkfs \
  --docs \
  --docs-page-limit 100 \
  --docs-prefetch all \
  --im \
  --im-active-days 3 \
  --im-history-days 3 \
  --im-prefetch all
```

在支持后台任务的 Agent 环境里，把上面的 `lark-cli fs mount ...` 作为长运行/background 进程启动。看到 stderr 出现 `larkfs mounted: /private/tmp/larkfs` 后，即可从另一个命令读取挂载点。

### 参数选择

- `--docs`：包含 Word 类文档，正文以 Markdown 暴露。
- `--docs-folder-token <folder>`：只扫描指定云空间文件夹；可重复。默认会尝试构建个人云空间真实目录树。
- `--docs-page-limit 100`：搜索发现文档的最大页数；材料较多时可调大。
- `--docs-prefetch metadata|recent|all`：文档正文预取策略。要用 `rg` 全文搜时优先 `all`；只想快速看到目录时用 `metadata`。
- `--im`：包含最近活跃聊天。
- `--im-active-days 3`：纳入过去 3 天内有过消息的会话。
- `--im-history-days 3`：对每个纳入的会话读取过去 3 天消息。它和 `--im-active-days` 是两个独立窗口。
- `--im-prefetch metadata|all`：聊天消息预取策略。要全文搜聊天内容时用 `all`。

## 目录结构

典型挂载点：

```text
/private/tmp/larkfs/
  .meta/
    capabilities.json
    snapshot.json
    errors.ndjson
  docs/
    index.ndjson
    drive/
      <真实云空间目录>/<文档标题>__<stable-id>.md
    all/
      by-updated-month/YYYY-MM/<文档标题>__<stable-id>.md
    .data/
      <stable-id>.meta/raw.json
  im/
    chats.ndjson
    chats/
      group__<群名>__<stable-id>/
        meta.json
        YYYY-MM-DD.md
        YYYY-MM-DD.ndjson
      dm__<对方名或unknown>__<stable-id>/
        meta.json
        YYYY-MM-DD.md
        YYYY-MM-DD.ndjson
```

说明：

- `docs/drive/` 是默认文档入口，尽量还原真实云空间目录结构。
- `docs/all/by-updated-month/` 是补充视图，放搜索发现但不一定能定位到真实目录的文档。
- `im/chats/*/*.md` 适合给人或模型阅读。
- `im/chats/*/*.ndjson` 适合用 `jq` 做结构化分析。
- `.meta/errors.ndjson` 记录后台刷新、预取或单文件读取失败；排查异常时先看它。

## 常用操作

列顶层目录：

```bash
ls /private/tmp/larkfs
find /private/tmp/larkfs/docs/drive -maxdepth 3 -type d | sort
```

跨文档和聊天全文搜索：

```bash
rg -n "关键词" /private/tmp/larkfs/docs /private/tmp/larkfs/im
```

只搜文档：

```bash
rg -n "预算|排期|方案" /private/tmp/larkfs/docs
```

只搜聊天：

```bash
rg -n "上线|评审|排期" /private/tmp/larkfs/im/chats
```

读取命中文档：

```bash
sed -n '1,160p' '/private/tmp/larkfs/docs/drive/.../文档标题__abcd1234.md'
```

查看聊天索引：

```bash
sed -n '1,20p' /private/tmp/larkfs/im/chats.ndjson
jq -r '.type + " " + .name + " " + .path' /private/tmp/larkfs/im/chats.ndjson
```

分析某个聊天日文件：

```bash
jq -r '[.create_time, .sender.name, .msg_type, .content] | @tsv' \
  '/private/tmp/larkfs/im/chats/group__示例群聊__abcd1234/2026-05-11.ndjson'
```

## 性能和一致性

- `fs mount` 会尽快完成系统挂载，然后在后台刷新 docs/im manifest 并预取正文。
- 如果没有缓存，刚挂载时目录可能只有基础结构；等待 `larkfs docs refresh complete` / `larkfs im refresh complete` 后再做大范围 `rg` 更稳定。
- `rg` 只能搜索当前目录树中已经出现的文件；如果 metadata 还没刷新完成，文件还不会被扫描到。
- `--docs-prefetch all` / `--im-prefetch all` 会在后台尽量提前拉正文，适合后续大范围搜索。
- 第一次读取 lazy 文件可能触发网络请求；第二次读取通常走本地对象缓存。
- 如果只需要快速浏览目录，把 prefetch 调成 `metadata`；如果目标是全文搜索，把 prefetch 调成 `all`。

## 卸载和清理

完成后卸载：

```bash
umount /private/tmp/larkfs
```

如果 mount 进程没有自动退出，停止对应长运行进程。只有在确认该命令就是本次 LarkFS mount 后，才使用：

```bash
pkill -f "lark-cli fs mount /private/tmp/larkfs"
```

如果 `umount` 提示 busy，先退出正在访问挂载点的 shell、编辑器、`rg` 或 `jq` 进程，再重试。

## `fs sync` 的定位

`lark-cli fs sync` 会把当前可访问材料同步成真实本地文件树。它不是 mount，也不提供热更新或虚拟读取语义。

只有在这些场景使用 `fs sync`：

- 用户明确想要本地 snapshot。
- 当前系统不能使用 mount backend。
- 需要把同步结果交给不支持访问 WebDAV mount 的工具。

不要为了 mount 先跑 `fs sync`；`fs mount` 会自己建立 LarkFS session，并使用内部缓存。
