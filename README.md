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

### キー

| キー | 動作 |
| --- | --- |
| `tab` | ツリー / diff のフォーカス切替 |
| `j` / `k` | 移動 |
| `enter` (ツリー) | ディレクトリ開閉 / ファイルへジャンプ |
| `ctrl+d` / `ctrl+u` | 半ページ移動 |
| `g` / `G` | 先頭 / 末尾 |
| `n` / `p` | 次 / 前のファイル |
| `enter` / `space` (ファイルヘッダ上) , `x` | ファイルの開閉 |
| `c` (コード行上) | コメント (`ctrl+s` 送信, `esc` 取消) |
| `r` / `R` (スレッド上) | 返信 / resolve 切替 |
| `u` | Claude の返信が来た未読スレッドへジャンプ |
| マウス | ツリーのクリックでファイルへジャンプ / ディレクトリ開閉、diff のクリックでカーソル移動、ホイールでスクロール |
| `q` / `ctrl+c` | 終了 |

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

di でコメントを送ると Claude に channel 通知として届き、Claude は `reply_comment` で返信 / resolve します。
Claude に「生成コードは畳んで」などと頼むと `get_collapsed` / `set_collapsed` で config の collapsed を更新し、起動中の di に即反映されます
(config は書き直されるため YAML のコメントは消えます。origin remote が無いリポジトリでは設定できません)。

channels を使わない場合は Claude に「di のコメントに対応して」と頼めば `list_comments` で取得します。
