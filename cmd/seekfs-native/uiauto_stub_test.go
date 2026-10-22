//go:build !uiautomation

package main

import "testing"

func TestAutomationDisabledInNormalBuild(t *testing.T) {
	if err := startUIAutomation(nil); err != nil {
		t.Fatalf("normal build startUIAutomation = %v", err)
	}
	uiautoMark("settings-apply-start")
}
