//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

// currentProcessUserSID returns the user SID of the current process, or ""
// when it cannot be resolved. The CLI uses it to pick its per-user signals
// file; the service never uses it (it uses the pipe client's SID instead).
func currentProcessUserSID() string {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return ""
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid == nil {
		return ""
	}
	return user.User.Sid.String()
}

// restrictSignalStoreFile limits a per-user signals file to SYSTEM,
// Administrators, and the owning user SID. The file lives in the shared
// service state dir (%ProgramData%\seekfs), so without this any local user
// could read another user's frecency/query history. Best-effort: failures are
// swallowed (the data is still correct; the next save retries).
//
// SDDL: D:P — protected DACL (no inheritance from the state dir, which is
// otherwise readable), FA (file all) for SY, BA, and the owner SID.
func restrictSignalStoreFile(path, sid string) {
	if path == "" || sid == "" || signalStoreDisableACLForTest {
		return
	}
	sddl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return
	}
	_ = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}
