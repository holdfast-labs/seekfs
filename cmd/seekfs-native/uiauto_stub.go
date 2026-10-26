//go:build !uiautomation

package main

import "gioui.org/app"

// uiauto_stub.go: normal builds keep the automation endpoint and its frame
// instrumentation out of the binary entirely.

func uiautoFrameBegin(u *UI, e app.FrameEvent) {}
func uiautoFrameEnd(u *UI)                     {}
func uiautoMark(name string)                   {}

func startUIAutomation(u *UI) error { return nil }
func stopUIAutomation()             {}
