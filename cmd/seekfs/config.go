package main

import (
	"encoding/binary"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

func findDefaultConfig() string {
	candidates := []string{"seekfs.toml"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "seekfs.toml"))
	}
	candidates = append(candidates, defaultConfigPath())
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "seekfs.toml"))
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func defaultConfigPath() string {
	return filepath.Join(defaultSeekFSDir(), "seekfs.toml")
}

func parseTOMLString(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(value, ","))
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}

func parseTOMLStringArray(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if s := parseTOMLString(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseTOMLBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

func loadIndexes(paths []string) ([]*Index, error) {
	indexes := make([]*Index, 0, len(paths))
	for _, path := range paths {
		idx, err := loadIndex(path)
		if err != nil {
			return nil, err
		}
		idx.DBPath = path
		indexes = append(indexes, idx)
	}
	return indexes, nil
}

func defaultDB() string {
	if v := os.Getenv("SEEKFS_DB"); v != "" {
		return v
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "seekfs.db"
	}
	return filepath.Join(dir, "seekfs", "index.gsi")
}

func defaultSeekFSDir() string {
	if v := os.Getenv("SEEKFS_DIR"); v != "" {
		return filepath.Clean(v)
	}
	if base := os.Getenv("ProgramData"); base != "" {
		return filepath.Join(base, "seekfs")
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "seekfs")
	}
	return "seekfs"
}

// seekFSExclusionDirsUnder returns the canonical folders that must never be
// indexed: the configured seekfs_dir and the default seekfs dir (SEEKFS_DIR
// env, else %ProgramData%\seekfs). Only paths covered by sourceRoot are
// returned; a seekfs dir on a different volume is irrelevant to that build.
func seekFSExclusionDirsUnder(sourceRoot string) []string {
	candidates := []string{}
	if cfg, err := loadConfig(""); err == nil && cfg.SeekFSDir != "" {
		candidates = append(candidates, cfg.SeekFSDir)
	}
	candidates = append(candidates, defaultSeekFSDir())
	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		canonical, err := directCanonicalPath(candidate)
		if err != nil {
			continue
		}
		if !directPathUnderAny(canonical, []string{sourceRoot}) {
			continue
		}
		key := strings.ToLower(canonical)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, canonical)
	}
	return out
}

func defaultIndexDir() string {
	return filepath.Join(defaultSeekFSDir(), "indexes")
}

// ntfsFileReference returns the NTFS file reference for an existing file or
// directory, in the same form the USN journal reports as FRN/ParentFRN: the
// 48-bit MFT record number with the 16-bit sequence stripped (see
// fileReferenceRecordNumber).  Opening with no requested access plus
// FILE_FLAG_BACKUP_SEMANTICS works for directories and needs no privilege; the
// id comes from FileIdInfo, whose FILE_ID_128 carries the 64-bit MFT reference
// in its first 8 bytes on NTFS.  FILE_ID_INFO is not exported by x/sys, so it
// is declared locally.
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// fileReferenceFromFileID converts a FileIdInfo FILE_ID_128 to the journal's
// FRN form.  The sequence number MUST be stripped: the journal parser applies
// fileReferenceRecordNumber to every record it reads, so an unmasked reference
// would never equal a ParentFRN and the owned-dir filter would silently match
// nothing (verified against a real index: C:\ProgramData is FRN 31305).
func fileReferenceFromFileID(fileID [16]byte) uint64 {
	return fileReferenceRecordNumber(binary.LittleEndian.Uint64(fileID[:8]))
}

func ntfsFileReference(path string) (uint64, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(pathPtr, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var info fileIDInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, err
	}
	return fileReferenceFromFileID(info.FileID), nil
}

const (
	ownedDirWalkMaxDepth = 6
	ownedDirWalkMaxDirs  = 4096
)

// ownedReplayDirFRNs collects the NTFS directory references that hold seekfs's
// own artifacts for an indexed volume: the configured and default seekfs dirs
// (which the offline walk already refuses to index) plus the name-gram spool
// dir, each including their descendant directories.  Only directories are
// collected, and USN records carry ParentFRN, so a single set check filters
// every artifact change without path reconstruction.  Called once when the
// volume is loaded; a directory that appears later under an owned directory
// (the builder's random scratch dirs) is picked up by the dynamic set instead.
// A reference is resolved from its live path, so a directory that no longer
// exists contributes nothing.
func ownedReplayDirFRNs(volume string) map[uint64]struct{} {
	vol := normalizeVolume(volume)
	if len(vol) != 2 || vol[1] != ':' {
		return nil
	}
	root := vol + "\\"
	canonicalRoot, err := directCanonicalPath(root)
	if err != nil {
		return nil
	}
	dirs := seekFSExclusionDirsUnder(canonicalRoot)
	spool := nameGramSpoolDir()
	if canonical, spoolErr := directCanonicalPath(spool); spoolErr == nil &&
		directPathUnderAny(canonical, []string{canonicalRoot}) {
		dirs = append(dirs, canonical)
	}
	out := make(map[uint64]struct{})
	for _, dir := range dirs {
		collectOwnedDirFRNs(dir, out, 0)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func collectOwnedDirFRNs(dir string, out map[uint64]struct{}, depth int) {
	if depth > ownedDirWalkMaxDepth || len(out) >= ownedDirWalkMaxDirs {
		return
	}
	frn, err := ntfsFileReference(dir)
	if err != nil {
		return
	}
	out[frn] = struct{}{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if len(out) >= ownedDirWalkMaxDirs {
			return
		}
		collectOwnedDirFRNs(filepath.Join(dir, entry.Name()), out, depth+1)
	}
}

func defaultVolumeDB(indexDir, volume string) string {
	letter := strings.ToLower(strings.TrimSuffix(strings.TrimRight(volume, `\`), ":"))
	if letter == "" {
		letter = "volume"
	}
	return filepath.Join(indexDir, "seekfs_"+letter+".gsi")
}

func defaultIndexVolumes() []string {
	var volumes []string
	for letter := 'C'; letter <= 'Z'; letter++ {
		root := fmt.Sprintf("%c:\\", letter)
		driveType := windows.GetDriveType(windows.StringToUTF16Ptr(root))
		if driveType == windows.DRIVE_FIXED {
			volumes = append(volumes, fmt.Sprintf("%c:", letter))
		}
	}
	if len(volumes) == 0 {
		return []string{"C:"}
	}
	return volumes
}
