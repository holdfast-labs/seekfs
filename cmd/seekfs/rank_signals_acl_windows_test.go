//go:build windows

package main

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func assertSignalStoreACL(t *testing.T, path, sid string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		t.Fatalf("GetNamedSecurityInfo(%q): %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("DACL(%q): %v", path, err)
	}
	if dacl.AceCount != 3 {
		t.Fatalf("per-user DACL ACEs = %d, want 3 (SYSTEM, Administrators, owner)", dacl.AceCount)
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatalf("Control(%q): %v", path, err)
	}
	const seDACLProtected = 0x1000
	if ctrl&seDACLProtected == 0 {
		t.Fatalf("per-user DACL must be protected (no inheritance), control = %#x", uint32(ctrl))
	}
	// SDDL prints the built-in Administrator account (RID 500, the user on
	// some CI runners) as the alias LA instead of its full SID.
	sddl := sd.String()
	ownerNamed := strings.Contains(sddl, sid) ||
		(strings.HasSuffix(sid, "-500") && strings.Contains(sddl, ";;;LA)"))
	if !ownerNamed {
		t.Fatalf("per-user SDDL must name the owner SID %s: %q", sid, sddl)
	}
}
