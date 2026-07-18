---
name: mv-tab
description: Move the current herdr session's Tab to another Workspace. Use when the user says things like "このセッションを <label> workspace に移動して" or "move this tab to workspace X" — the user is running Claude inside herdr and wants the Tab this conversation lives in to be relocated. Requires the herdr-drover plugin (github.com/4noha/herdr-drover) to be installed.
---

# mv-tab — このセッションの Tab を別 Workspace に移動

## いつ発動するか

ユーザーが「このセッション/この Tab を <workspace 名> に移動して」等と言ったとき。
herdr（AI エージェント用ターミナル）内で走っている Claude セッションが対象。

例:
- 「このセッションを slave に移動して」
- 「この tab を main workspace に移動」
- 「move this to workspace X」

## 実行するコマンド

`herdr-drover mv-tab --self --dst-ws-label <label>` を Bash tool で走らせるだけ。
`<label>` はユーザーが指定した workspace 名を **そのまま** 入れる（推測しない）。

例: 「slave に移動して」→ `herdr-drover mv-tab --self --dst-ws-label slave`

- `--self` = herdr の `pane.current` API で自 pane を exact 特定
- `--dst-ws-label <label>` = workspace.list の label exact 一致で解決
- 成功後は自動的に受入先 WS の新 Tab にフォーカスが移動する（herdr UI で可視）
- Tab の中で走っているプロセス（Claude 自身を含む）は無停止で継続する
  （terminal_id 保存の実測事実）

## エラー時の対応

- **`workspace label <X> が見つからない`**: ユーザーの指定した label が実在しない。
  `herdr workspace list` を Bash tool で叩いて実在 label を提示し、ユーザーに確認する。
- **`workspace label <X> が N 件一致`**: label は herdr 上で重複可（実測仕様）。
  複数一致した workspace_id が列挙されるので、ユーザーに `workspace_id` を確認して
  `herdr-drover mv-tab --self --dst-ws <workspace_id>` で再実行。
- **`src と dst が同一 WS`**: 既にその WS に居る＝移動不要。ユーザーに報告して終了。
- **`herdr dial 失敗`**: herdr サーバが動いていない or drover 未インストール。
  `herdr status` と `~/.herdr-drover/bin/herdr-drover status` を叩いて診断結果を提示。

## やってはいけないこと

- **推測で label を書き換えない**: 「slave」→「slaves」等の類推補正はしない。
  ユーザーの言った通りに `--dst-ws-label` へ渡す（一致しなければ loud エラー）。
- **他 Tab を巻き添えにしない**: 「このセッション」は必ず `--self` を使う。
  `--src-tab <id>` を勝手に埋めるのは NG（同一 cwd の別 claude を動かすリスク）。
- **確認なしに移動しない**: ユーザーが「移動して」と明示的に言った場合のみ実行。
  会話の流れで暗黙に推論して動かさない（`herdr-drover mv-tab` は元に戻すのは可能
  だが、pane_id が変わるため注意）。

## 参考: 手動で対話ピッカを使う場合

ユーザーが自分で細かく指定したいと言った時のみ:

```
herdr-drover mv-tab
```

（対話ピッカで src Tab と dst WS を番号選択・TTY 必須）
