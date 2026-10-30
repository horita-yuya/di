# di — design

GitHub の "Files changed" をターミナルで。作業ブランチと target branch の差分をレビューし、
コメントを MCP (Claude Code channels) 経由で Claude に送ってそのまま対応させる。

## Commands

```
di [--base <ref>]   # TUI を起動
di mcp              # stdio MCP サーバー (Claude Code から起動される)
di upgrade          # 最新リリースでバイナリを置き換える (git repo 外でも動く)
```

## Dependencies (これ以外は足さない)

- charm.land/bubbletea/v2, bubbles/v2 (textarea), lipgloss/v2 — TUI
- github.com/bluekeyes/go-gitdiff — unified diff のパース (自前パーサを書かない)
- github.com/bmatcuk/doublestar/v4 — glob (`**`)
- github.com/modelcontextprotocol/go-sdk — MCP
- gopkg.in/yaml.v3 — config

## Packages

```
main.go                 サブコマンド分岐 (stdlib flag のみ, cobra 不要)
internal/git            git コマンド呼び出し
internal/config         global config
internal/thread         スレッド型 + TUI⇄mcp の socket パス
internal/tui            bubbletea model
internal/mcp            MCP サーバー
internal/upgrade        `di upgrade` (stdlib のみ)
```

## internal/git

- `Open(dir)` → `Repo{Root, GitDir}` (`git rev-parse --show-toplevel`, `--absolute-git-dir`)
- `Slug()` → origin remote URL から `owner/name` (ssh/https 両対応, `.git` 除去)。remote 無しなら ""
- `Branch()` → 現在のブランチ名
- `DefaultBase()` → `git symbolic-ref --short refs/remotes/origin/HEAD` (例 `origin/main`)、失敗時 `main` → `master` の順で存在するもの
- `Diff(base)` → `[]*gitdiff.File`
  - `git diff --merge-base <base> --no-color --no-ext-diff -M` (= base との merge-base から **working tree** までの差分。未コミット変更も含むので hot reload が効く)
  - untracked: `git ls-files --others --exclude-standard -z` の各ファイルに `git diff --no-index --no-color -- /dev/null <f>` (exit 1 は正常) を連結
  - 戻り値と一緒に raw 文字列も返す (変化検知用)

## internal/config

Path: `$XDG_CONFIG_HOME/di/config.yaml`、未設定なら `~/.config/di/config.yaml`。無ければ空 config。

```yaml
repos:
  - repo: owner/name        # origin remote の owner/name と一致
    base: origin/develop    # optional, --base > config > DefaultBase
    collapsed:              # doublestar パターン, repo root からの相対パス
      - "**/*.pb.go"
      - "pnpm-lock.yaml"
      - "vendor/**"
```

- `Load() (*Config, error)`, `(*Config).Repo(slug) RepoConfig` (無ければ zero value)
- `(RepoConfig).Collapsed(path) bool` — いずれかのパターンに match

## internal/thread

コメントはスレッド (ユーザー ⇄ Claude の往復)。**永続化しない。TUI プロセスのメモリ上にのみ持つ** (di を閉じたら消える)。

```go
type Message struct {
    Author string // "user" | "claude"
    Body   string
}
type Thread struct {
    ID       string    // 連番で十分
    File     string    // new path (削除ファイルなら old path)
    Side     string    // "new" | "old"
    Line     int       // その side での行番号
    LineText string    // コメント時点の行内容 (outdated 判定と Claude へのコンテキスト)
    Context  string    // コメント時点の hunk 抜粋 (対象行の前後 最大 8 行, " "/"+"/"-" prefix 付き)
    Messages []Message
}
```
状態は派生: 最後の Author が user → "waiting for Claude"、claude → "Claude replied"。

### TUI ⇄ `di mcp` の通信

`di mcp` は Claude Code が spawn する別プロセス (channels は stdio 必須) なので、TUI と unix socket で繋ぐ。
`di mcp` は状態を持たない中継役 (list_comments 用に受け取ったスレッドのキャッシュのみ)。

- Socket: `filepath.Join(os.TempDir(), "di-"+hex(sha256(GitDir))[:12]+".sock")`
  (GitDir = `git rev-parse --absolute-git-dir` は worktree ごとに別 ⇒ **worktree ごとに独立**。
  sun_path 長制限 (macOS 104B) を避けるため GitDir 配下には置かない)
- TUI が listen。起動時に dial して繋がれば「この worktree で di は起動済み」でエラー終了、繋がらなければ stale socket を消して listen。終了時に削除。
- プロトコル: 双方向 newline-delimited JSON (`json.Encoder`/`json.Decoder`)
  - TUI → mcp: `Thread` 全体。ユーザーが新規コメント / 返信した時に接続中の全 mcp へ broadcast。
    mcp が接続した時は「最後が user」のスレッドを全部送る。
  - mcp → TUI: `{"id", "body"}` (Claude の返信)。TUI はメッセージ追加して再描画 (`p.Send`)。
- `di mcp` は TUI 未起動なら 1 秒間隔で再接続を試み続ける (Claude を先に起動しても良い)。

## Hot reload

fsnotify は使わない (再帰 watch / ignore 処理が不要になる)。TUI は 1 秒ごとに `tea.Tick` で
`Diff()` を実行し、raw diff が前回と違えばモデルを再構築する (スレッドはメモリ上なので socket 受信時に `p.Send` で即反映)。
再構築時に UI 状態 (ファイル毎の collapse 状態, カーソル位置のファイル+行, ツリーの開閉) を path キーで保持する。
git 実行は tea.Cmd 内 (goroutine) で行い UI をブロックしない。

## TUI

左: ファイルツリー (幅 ~30%)、右: 全ファイルの diff を縦に連結 (GitHub と同じ)。`tab` でフォーカス切替。

### Tree
- ディレクトリ階層。ファイル行は `+N -M` を表示。
- 折りたたまれたファイルは dim 表示。**配下のファイルが全て collapsed なディレクトリはデフォルトで閉じる**。
- `j/k` 移動, `enter`: dir → 開閉 / file → diff 側をそのファイルへジャンプ (collapsed なら展開)。
- ファイル行にも `▾/▸` を表示し、クリックで fold 状態をトグル (行の他の部分はジャンプ)。
- diff 側カーソルのファイルをツリーでハイライト (同期)。
- Claude の返信に未回答のスレッドがあるファイル (と、それを含む閉じたディレクトリ) をスレッドと同じ黄色でハイライト。

### Diff
- 平坦な row 配列 (file header / hunk header / code line / comment) を自前でスクロール描画する (viewport 不要)。
- file header: `▼ path  +N -M` / collapsed は `▶ path` のみ。rename は `old → new`、binary は "Binary file"。
- code line: `old行番号 new行番号 ±内容`、追加=緑, 削除=赤。横はみ出しは切り詰め。
- config で collapsed なファイルはデフォルトで `▶`。ユーザーは開ける。
- 操作はマウスのみ (キーは `ctrl+c` 終了だけ)。ホイールでポインタ下のペインをスクロール、ファイルヘッダのクリックで開閉、
  コード行のクリックでコメント入力 (ホバー行の gutter に `+` を表示、入力中のクリックは書きかけのまま対象を切替) (画面下部の bubbles/textarea + `Comment` / `Cancel` ボタン)。
- スレッドは対象行の直下に表示: `💬 [status]` + メッセージを `you: ...` / `claude: ...` で時系列表示。
- スレッド末尾の `Comment` ボタンでスレッドにコメントを追加 (送信で Claude に push)。
  対象 (Side, Line) の行内容が LineText と一致しなければ outdated 扱いで file header 直下に表示。
- footer: `<branch> ← <base>  N files  +A -D  │ keys hint`

## MCP (`di mcp`)

go-sdk の stdio サーバー。cwd の repo を対象とする。

- Capabilities: `Experimental: {"claude/channel": {}}`, tools
- Instructions: 「`<channel source="di">` で届くのはユーザーが di でつけたレビューコメントのスレッド。
  対応 (コード修正 / 質問への回答) したら必ず `reply_comment` で返信する。ユーザーへの質問・確認も `reply_comment` で行う
  (ユーザーは di 上で読んで返信する)」
- Channel push: go-sdk は未知の method を送れないため、`mcp.Transport` をラップして `Connection` を捕まえ、
  `conn.Write(ctx, &jsonrpc.Request{Method: "notifications/claude/channel", Params: ...})` (ID 無し = notification) で直接送る。
  initialized 後にのみ送る。
  - params: `{"content": <整形済みテキスト>, "meta": {"thread_id": id, "file": file, "line": "<n>"}}` (meta は string 値, key は英数と _ のみ)
  - content: file:line (side)、Context (```diff ブロック)、スレッド全履歴、最新のユーザーメッセージを明示
- Tools:
  - `list_comments` — キャッシュ中のスレッドを content と同形式で返す。channels を使わない場合のフォールバック
    (「di のコメントに対応して」と頼めば動く)。
  - `reply_comment {id, body}` — socket 経由で TUI に返信を送る。TUI 未接続ならエラーを返す。

### Claude Code 側のセットアップ (README に記載)
```
claude mcp add --scope user di -- di mcp
claude --dangerously-load-development-channels server:di
```
(2026-07-28 revision では channel が登録されないため、`di mcp` は SupportedProtocolVersions でそれより前に限定する。
relay は initialized を待たず stdio 接続時に開始する)

## Release / upgrade

- main への push ごとに release workflow が最新 `v*` tag の patch+1 を tag して push し、goreleaser でリリースする
  (GITHUB_TOKEN で push した tag は workflow を起動しないのでループしない)。version は ldflags で `main.version` に入る (先頭の `v` 無し)。
- asset 名は version 無し: `di_<GOOS>_<GOARCH>.tar.gz` (windows は zip) と `checksums.txt`。
- `di upgrade`: API を使わず `releases/latest` の redirect 先 `.../tag/<tag>` から最新 tag を得る。`"v"+version` と同じなら何もしない。
  archive と checksums.txt を取得して SHA-256 を照合し、tar.gz から `di` を取り出して実行中バイナリ (symlink 解決後) と同じディレクトリの
  一時ファイルに書き、`os.Rename` で置き換える。Windows は非対応 (Releases から手動で入れ替える)。
