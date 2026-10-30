# di

GitHub の "Files changed" をターミナルで。作業ブランチと base ブランチの差分をレビューし、
行コメントを MCP (Claude Code channels) 経由で Claude に送ってそのまま対応させます。

## インストール

```sh
GOBIN=~/.local/bin go install .   # PATH 上の任意のディレクトリへ
```

## 使い方

```sh
di [--base <ref>]   # TUI を起動 (base: --base > config > origin/HEAD > main > master)
di mcp              # stdio MCP サーバー (Claude Code から起動される)
```

差分は base との merge-base から working tree まで (未コミット・untracked を含む) で、1 秒ごとに自動更新されます。
コメントスレッドは di のプロセス内にだけ保持され、di を終了すると消えます。di は worktree ごとに 1 つ起動できます。

ライトテーマ専用です。起動中はターミナルの背景を白に切り替え (OSC 10/11)、終了時に元に戻します (OSC 110/111)。

### 操作

マウスで操作します (キーボードは `↑` / `↓` と `ctrl+c` で終了のみ)。

| 操作 | 動作 |
| --- | --- |
| ホイール | ポインタ下のペイン (ツリー / diff) をスクロール |
| ツリーのクリック | ディレクトリ開閉 / ファイルへジャンプ |
| `↑` / `↓` | ツリー上の前 / 次のファイルへジャンプ |
| ファイルヘッダのクリック | ファイルの開閉 |
| コード行のクリック | その行にコメント (入力中なら書きかけのまま対象行を切替) |
| スレッド末尾の `Comment` | スレッドにコメントを追加 |
| 入力欄の `Comment` / `Cancel` | 送信 / 取消 (`enter` でも送信、改行は `alt+enter` か `ctrl+j`) |

## 設定

`$XDG_CONFIG_HOME/di/config.yaml` (未設定なら `~/.config/di/config.yaml`)

```yaml
repos:
  - repo: owner/name        # origin remote の owner/name
    base: origin/develop    # 省略可
    collapsed:              # デフォルトで畳むファイル (doublestar パターン, repo root からの相対パス)
      - "**/*.pb.go"
      - "pnpm-lock.yaml"
      - "vendor/**"
```

## Claude Code のセットアップ

```sh
claude mcp add --scope user di -- ~/.local/bin/di mcp
claude --dangerously-load-development-channels server:di
```

2026-07-28 revision では channel が登録されないため、`di mcp` はそれより前の revision だけを受け付けます。

di でコメントを送ると Claude に channel 通知として届き、Claude は `reply_comment` で返信します。
Claude に「生成コードは畳んで」などと頼むと `get_collapsed` / `set_collapsed` で config の collapsed を更新し、起動中の di に即反映されます
(config は書き直されるため YAML のコメントは消えます。origin remote が無いリポジトリでは設定できません)。

channels を使わない場合は Claude に「di のコメントに対応して」と頼めば `list_comments` で取得します。
