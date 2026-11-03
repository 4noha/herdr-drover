package main

import "strings"

// clipimage_ps.go — Windows のクリップボード操作に使う PowerShell スクリプト（OS 非依存の純関数＝
// 単体テストで引用符の扱いを固定する）。実行は clipimage_windows.go / bridge の imagepaste。

// psQuote は PowerShell の単一引用符文字列にする（' は ” に）。
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// winClipboardGetScript はクリップボードの画像を out へ PNG で保存し "ok"、無ければ "none" を出す。
func winClipboardGetScript(out string) string {
	return "Add-Type -AssemblyName System.Windows.Forms,System.Drawing; " +
		"if ([System.Windows.Forms.Clipboard]::ContainsImage()) { " +
		"$i = [System.Windows.Forms.Clipboard]::GetImage(); " +
		"$i.Save(" + psQuote(out) + ", [System.Drawing.Imaging.ImageFormat]::Png); $i.Dispose(); 'ok' } else { 'none' }"
}
