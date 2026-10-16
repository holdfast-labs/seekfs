package main

// sym: definition search (G9). A sym:Name leaf matches a file with a line that
// DEFINES Name, decided at query time by per-language anchored patterns keyed
// by the file extension; candidates come from the normal whole-word postings,
// so no index format change is needed. The same check gives plain content:Name
// queries a definition-line bonus under sort:relevance.

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
)

// contentSymPatterns are the definition shapes per language; %s is the
// regexp-quoted identifier. Lines are tested only when they contain the name,
// and lineDefines applies the shared post-checks (declarations ending in ';',
// control-flow keywords, comments).
var contentSymPatterns = map[string][]string{
	"go": {
		`^\s*func\s+(\([^)]*\)\s*)?%s\b`,
		`^\s*type\s+%s\b`,
		`^\s*(var|const)\s+%s\b`,
	},
	"c": {
		`^\s*(template\s*<[^>]*>\s*)?(typedef\s+)?(class|struct|union|enum(\s+class)?)\s+(\w+\s+)?%s\b`,
		`^\s*#\s*define\s+%s\b`,
		// Out-of-line member definition at column 0: [Type ]Class::Name( /
		// Class::~Name( (constructors and destructors have no type).
		`^([\w:<>,\*&~]+[\s\*&]+)*(\w+::)+~?%s\s*\(`,
		// Function definition with a return type: Type Name( / Type* Name(.
		`^\s*([\w:<>,~]+[\s\*&]+)+[\*&]*%s\s*\(`,
	},
	"py": {
		`^\s*(async\s+)?def\s+%s\b`,
		`^\s*class\s+%s\b`,
		`^%s\s*(:[^=]*)?=[^=]`,
	},
	"js": {
		`\bfunction\s*\*?\s*%s\b`,
		`^\s*(export\s+)?(default\s+)?(abstract\s+)?class\s+%s\b`,
		`^\s*(export\s+)?(const|let|var)\s+%s\s*[=:]`,
		`^\s*(export\s+)?(declare\s+)?(interface|type|enum|namespace)\s+%s\b`,
		// Class method: [modifiers] name(...) {  (calls end in ';').
		`^\s*((public|private|protected|static|async|readonly|override|get|set)\s+)*%s\s*(<[^>]*>)?\([^;]*\)\s*(:\s*[^={;]+)?\{\s*$`,
	},
	"rs": {
		`\bfn\s+%s\b`,
		`\b(struct|enum|trait|type|union|mod|const|static)\s+%s\b`,
		`\bmacro_rules!\s*%s\b`,
	},
	"java": {
		`\b(class|interface|enum|record|struct|object)\s+%s\b`,
		`\bfun\s+(<[^>]*>\s*)?([\w.]+\.)?%s\b`,
		`^\s*([\w<>\[\],?@.]+\s+)+%s\s*\(`,
	},
	"": {
		`\b(fn|func|function|def|fun|sub|proc)\s+%s\b`,
		`\b(class|struct|interface|enum|trait|type|module)\s+%s\b`,
	},
}

// contentSymSemicolonChecked lists, per language, the pattern indexes where a
// line ending in ';' without a body is a declaration or call, not a
// definition (#define and the non C-family shapes are exempt).
var contentSymSemicolonChecked = map[string]map[int]bool{
	"c":    {0: true, 2: true, 3: true},
	"java": {2: true},
}

// contentSymLangOf maps a path to its pattern set ("" = generic).
func contentSymLangOf(path string) string {
	switch strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".") {
	case "go":
		return "go"
	case "c", "cc", "cpp", "cxx", "h", "hh", "hpp", "hxx", "inl", "ipp", "m", "mm":
		return "c"
	case "py", "pyi":
		return "py"
	case "js", "jsx", "ts", "tsx", "mjs", "cjs", "mts", "cts":
		return "js"
	case "rs":
		return "rs"
	case "java", "cs", "kt", "kts", "scala", "swift":
		return "java"
	}
	return ""
}

// contentSymControlWords start statements that are never definitions (calls
// and expressions inside bodies).
var contentSymControlWords = map[string]bool{
	"return": true, "if": true, "else": true, "while": true, "for": true,
	"switch": true, "case": true, "do": true, "new": true, "delete": true,
	"throw": true, "goto": true, "sizeof": true, "await": true, "yield": true,
	"not": true, "and": true, "or": true, "in": true, "assert": true,
	"print": true, "co_return": true, "co_await": true, "using": true,
	"friend": true,
}

// contentSymMatcher holds one identifier's compiled definition patterns,
// built once per query leaf.
type contentSymMatcher struct {
	name   []byte
	byLang map[string][]*regexp.Regexp
}

func newContentSymMatcher(name string) *contentSymMatcher {
	s := &contentSymMatcher{name: []byte(name), byLang: make(map[string][]*regexp.Regexp, len(contentSymPatterns))}
	quoted := regexp.QuoteMeta(name)
	for lang, pats := range contentSymPatterns {
		for _, p := range pats {
			// Keep pattern indexes stable for contentSymSemicolonChecked.
			s.byLang[lang] = append(s.byLang[lang], regexp.MustCompile(strings.ReplaceAll(p, "%s", quoted)))
		}
	}
	return s
}

// lineDefines reports whether one line (no newline) defines the name.
func (s *contentSymMatcher) lineDefines(lang string, line []byte) bool {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("//")) || bytes.HasPrefix(trimmed, []byte("*")) || bytes.HasPrefix(trimmed, []byte("/*")) {
		return false
	}
	if bytes.HasPrefix(trimmed, []byte("#")) && lang != "c" {
		return false // a comment outside the C preprocessor
	}
	if contentSymControlWords[contentSymFirstWord(trimmed)] {
		return false
	}
	declaration := bytes.HasSuffix(trimmed, []byte(";")) && !bytes.Contains(trimmed, []byte("{"))
	for i, re := range s.byLang[lang] {
		if !re.Match(line) {
			continue
		}
		if declaration && contentSymSemicolonChecked[lang][i] {
			continue
		}
		return true
	}
	return false
}

// definitionLine returns the byte span of the first line of text that defines
// the name, testing only lines that contain it.
func (s *contentSymMatcher) definitionLine(path string, text []byte) (start, end int, ok bool) {
	if s == nil || len(s.name) == 0 {
		return 0, 0, false
	}
	lang := contentSymLangOf(path)
	for off := 0; off < len(text); {
		i := bytes.Index(text[off:], s.name)
		if i < 0 {
			return 0, 0, false
		}
		i += off
		ls := bytes.LastIndexByte(text[:i], '\n') + 1
		le := bytes.IndexByte(text[i:], '\n')
		if le < 0 {
			le = len(text)
		} else {
			le += i
		}
		if s.lineDefines(lang, text[ls:le]) {
			return ls, le, true
		}
		off = le + 1
	}
	return 0, 0, false
}

func (s *contentSymMatcher) defines(path string, text []byte) bool {
	_, _, ok := s.definitionLine(path, text)
	return ok
}

func contentSymFirstWord(b []byte) string {
	end := 0
	for end < len(b) && (b[end] == '_' || b[end] >= 'a' && b[end] <= 'z' || b[end] >= 'A' && b[end] <= 'Z' || b[end] >= '0' && b[end] <= '9') {
		end++
	}
	return string(b[:end])
}

// contentLooksLikeIdentifier reports whether a single-word content leaf could
// name a symbol (for the definition-line ranking bonus).
func contentLooksLikeIdentifier(s string) bool {
	if s == "" || s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
