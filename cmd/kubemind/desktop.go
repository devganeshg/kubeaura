//go:build desktop

package main

import webview "github.com/webview/webview_go"

// runDesktop opens the UI in a native window (WKWebView on macOS). It must be
// called on the main thread and blocks until the window is closed.
//
// Note: the native webview lacks Chrome's webkitSpeechRecognition, so the 🎙
// mic button is disabled there — voice output (speech synthesis) still works.
// For voice input, open the same URL in Chrome.
func runDesktop(url string) bool {
	w := webview.New(false)
	defer w.Destroy()
	w.SetTitle("KubeMind")
	w.SetSize(1440, 900, webview.HintNone)
	w.Navigate(url)
	w.Run()
	return true
}
