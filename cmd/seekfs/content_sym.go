package main

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
)

// Definition patterns are compiled once; the identifier is regexp-quoted.
var contentSymPatterns = map[string][]string{
	"go":   {`^\s*func\s+(\([^)]*\)\s*)?%s\b`, `^\s*type\s+%s\b`, `^\s*(var|const)\s+%s\b`},
	"c":    {`^\s*(template\s*<[^>]*>\s*)?(class|struct|union|enum(\s+class)?)\s+(\w+_EXPORT\s+)?%s\b[^;]*$`, `^\s*#define\s+%s\b`, `^.*\b%[1]s\s*\([^;]*$`},
	"py":   {`^\s*(async\s+)?def\s+%s\b`, `^\s*class\s+%s\b`},
	"js":   {`\bfunction\s+%s\b`, `^\s*(export\s+)?(default\s+)?(abstract\s+)?class\s+%s\b`, `\b(const|let|var)\s+%s\s*=`, `^\s*(export\s+)?(interface|type|enum)\s+%s\b`},
	"rs":   {`\bfn\s+%s\b`, `\b(struct|enum|trait|type|union|mod)\s+%s\b`, `\bmacro_rules!\s*%s\b`},
	"java": {`\b(class|interface|enum|record|struct)\s+%s\b`, `\b\w[\w<>,\[\]\s]*\s+%s\s*\([^;]*$`},
}

func contentDefinitionLine(path string, line []byte, name string) bool {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	lang := ""
	switch ext {
	case "go":
		lang = "go"
	case "c", "cc", "cpp", "cxx", "h", "hh", "hpp", "mm":
		lang = "c"
	case "py":
		lang = "py"
	case "js", "jsx", "ts", "tsx", "mjs":
		lang = "js"
	case "rs":
		lang = "rs"
	case "java", "cs", "kt":
		lang = "java"
	}
	patterns := contentSymPatterns[lang]
	if patterns == nil {
		patterns = []string{`\b(fn|function|def)\s+%s\b`, `\b(class|struct|interface|enum|type)\s+%s\b`}
	}
	quoted := regexp.QuoteMeta(name)
	for _, p := range patterns {
		p = strings.ReplaceAll(p, "%s", quoted)
		p = strings.ReplaceAll(p, "%[1]s", quoted)
		re, err := regexp.Compile(p)
		if err == nil && re.Match(line) {
			if lang == "c" && (bytes.Contains(line, []byte(";")) && !bytes.Contains(line, []byte("#define"))) {
				continue
			}
			return true
		}
	}
	return false
}

func contentHasDefinition(text []byte, path, name string) bool {
	for _, line := range bytes.Split(text, []byte{'\n'}) {
		if contentDefinitionLine(path, bytes.TrimSuffix(line, []byte{'\r'}), name) {
			return true
		}
	}
	return false
}
