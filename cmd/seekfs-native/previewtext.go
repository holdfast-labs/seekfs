package main

import (
	"os"
	"strings"
	"syscall"
)

// previewtext.go: portable text-preview helpers for the preview pane —
// encoding-sniffed reads of text files plus a metadata card fallback.

const maxTextPreviewBytes = 256 * 1024

// textPreviewableExt reports whether the extension is plausibly plain text.
func textPreviewableExt(path string) bool {
	switch strings.ToLower(dotExt(path)) {
	case ".txt", ".log", ".md", ".markdown", ".go", ".c", ".h", ".cpp", ".hpp",
		".cs", ".java", ".js", ".ts", ".tsx", ".jsx", ".py", ".rb", ".rs",
		".toml", ".yaml", ".yml", ".json", ".xml", ".html", ".htm", ".css",
		".sql", ".sh", ".ps1", ".bat", ".cmd", ".ini", ".cfg", ".csv", ".tsv",
		".tex", ".rst", ".diff", ".patch", ".gitignore", ".env", "":
		return true
	}
	return false
}

func dotExt(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i:]
		}
		if path[i] == '\\' || path[i] == '/' || path[i] == ':' {
			break
		}
	}
	return ""
}

// readTextPreview reads up to maxTextPreviewBytes and decodes UTF-8/UTF-16.
// The second return is false when the file looks binary.
func readTextPreview(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, maxTextPreviewBytes+1)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", false
	}
	buf = buf[:n]
	if len(buf) == 0 {
		return "", true
	}
	if len(buf) >= 3 && buf[0] == 0xEF && buf[1] == 0xBB && buf[2] == 0xBF {
		return string(buf[3:]), true
	}
	if len(buf) >= 2 && buf[0] == 0xFF && buf[1] == 0xFE {
		return utf16BytesToString(buf[2:], true), true
	}
	if len(buf) >= 2 && buf[0] == 0xFE && buf[1] == 0xFF {
		return utf16BytesToString(buf[2:], false), true
	}
	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	for _, b := range probe {
		if b == 0 {
			return "", false
		}
	}
	return string(buf), true
}

func utf16BytesToString(b []byte, littleEndian bool) string {
	n := len(b) / 2
	u := make([]uint16, n)
	for i := 0; i < n; i++ {
		if littleEndian {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		} else {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		}
	}
	return syscall.UTF16ToString(u)
}

// previewMetadata renders name/path/size/date/type/snippet lines for files
// without a text or image preview.
func previewMetadata(r ResultRow, typeLabel string) string {
	out := r.Name + "\n" + r.Path + "\n\n"
	if !r.IsDir {
		out += typeLabel + "\n"
		if r.HasSize {
			out += formatSizeShort(r.Size) + "\n"
		}
		if r.HasMod {
			out += formatExplorerTime(r.Modified) + "\n"
		}
	}
	if r.Snippet != "" {
		out += "\nMatch:\n" + r.Snippet
	}
	return out
}

func formatSizeShort(n int64) string {
	const unit = 1024
	if n < unit {
		return itoa(int(n)) + " bytes"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return itoa(int(n/div)) + " " + string("KMGTPE"[exp]) + "B"
}
