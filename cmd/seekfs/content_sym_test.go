package main

import (
	"sort"
	"strings"
	"testing"
)

func TestContentSymLineDefines(t *testing.T) {
	cases := []struct {
		path, line string
		want       bool
	}{
		{"a.go", "func Foo() int {", true},
		{"a.go", "func (s *T) Foo(x int) {", true},
		{"a.go", "type Foo struct {", true},
		{"a.go", "\tFoo()", false},
		{"a.go", "// func Foo() is old", false},
		{"c.h", "class Foo : public Base {", true},
		{"c.h", "class CORE_EXPORT Foo final : public Base {", true},
		{"c.h", "class Foo;", false},
		{"c.h", "struct Foo bar;", false},
		{"c.h", "enum class Foo : int {", true},
		{"c.h", "#define Foo(x) ((x) + 1)", true},
		{"c.cc", "void Bar::Foo(int x) {", true},
		{"c.cc", "Bar::~Foo() {", true},
		{"c.cc", "static int Foo(const char* s) {", true},
		{"c.cc", "  int Foo(int a);", false},
		{"c.cc", "  if (Foo(x)) {", false},
		{"c.cc", "  return Foo(x);", false},
		{"c.cc", "  Foo(x);", false},
		{"c.cc", "  x = Foo(y);", false},
		{"f.py", "def Foo(self):", true},
		{"f.py", "async def Foo():", true},
		{"f.py", "class Foo(Base):", true},
		{"f.py", "Foo = 3", true},
		{"f.py", "Foo()", false},
		{"f.py", "# def Foo()", false},
		{"h.ts", "export class Foo {", true},
		{"h.ts", "export const Foo = () => 1;", true},
		{"h.ts", "export interface Foo {", true},
		{"h.ts", "function Foo(a: number) {", true},
		{"h.ts", "  Foo(a: number): void {", true},
		{"h.ts", "  Foo(a);", false},
		{"h.ts", "const x = new Foo();", false},
		{"j.rs", "pub fn Foo() {}", true},
		{"j.rs", "pub struct Foo {", true},
		{"j.rs", "let y = Foo();", false},
		{"k.java", "public class Foo extends Bar {", true},
		{"k.java", "  public static int Foo(int x) {", true},
		{"k.java", "    return Foo(x);", false},
		{"k.java", "    int y = Foo(x);", false},
		{"k.kt", "fun Foo(x: Int): Int {", true},
		{"x.unknown", "function Foo() {", true},
		{"x.unknown", "call Foo()", false},
	}
	s := newContentSymMatcher("Foo")
	for _, tc := range cases {
		if got := s.lineDefines(contentSymLangOf(tc.path), []byte(tc.line)); got != tc.want {
			t.Errorf("%s %q: lineDefines = %v; want %v", tc.path, tc.line, got, tc.want)
		}
	}
}

func symFixture(t *testing.T) *serviceVolumeIndex {
	t.Helper()
	return newContentQueryVolume(t, []contentFixtureFile{
		{2, "a.go", "package a\n\nfunc Foo() int {\n\treturn 1\n}\n"},
		{3, "b.go", "package b\n\nfunc g() {\n\tFoo()\n}\n"},
		{4, "c.h", "class Foo : public Base {\n public:\n  void Run();\n};\n"},
		{5, "d.h", "class Foo;\nvoid Use(Foo* f);\n"},
		{6, "e.cc", "#include \"c.h\"\nvoid Foo::Run() {\n  if (Foo(x)) {\n  }\n}\n"},
		{7, "f.py", "def Foo():\n    pass\n"},
		{8, "g.py", "Foo()\n"},
		{9, "h.ts", "export class Foo {\n}\n"},
		{10, "i.ts", "const x = new Foo();\n"},
		{11, "j.rs", "pub fn Foo() {}\n"},
		{12, "k.rs", "let y = Foo();\n"},
	})
}

func sortedNames(entries []Entry) string {
	names := namesOf(entries)
	sort.Strings(names)
	return strings.Join(names, ",")
}

func TestContentSymDefinitionSearch(t *testing.T) {
	vol := symFixture(t)
	for q, want := range map[string]string{
		"sym:Foo":        "a.go,c.h,f.py,h.ts,j.rs",
		"sym:Run":        "e.cc",
		"sym:Foo ext:py": "f.py",
		"sym:Nope":       "",
	} {
		got, err := contentServiceSearch(t, vol, q, false)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if s := sortedNames(got); s != want {
			t.Errorf("%s = %s; want %s", q, s, want)
		}
		if n := contentServiceCount(t, []*serviceVolumeIndex{vol}, q); n != len(got) {
			t.Errorf("%s: count %d != search %d", q, n, len(got))
		}
	}
	if _, err := parseQuery(queryOptions{Query: "sym:two.words"}); err == nil {
		t.Error("sym: with a non-identifier must not parse")
	}
}

func TestContentSymRelevanceBonusAndLines(t *testing.T) {
	vol := symFixture(t)
	got, err := contentServiceSearch(t, vol, "content:Foo sort:relevance", false)
	if err != nil {
		t.Fatal(err)
	}
	defining := map[string]bool{"a.go": true, "c.h": true, "f.py": true, "h.ts": true, "j.rs": true}
	names := entryNamesInOrder(got)
	if len(names) < len(defining)+1 {
		t.Fatalf("content:Foo = %v; want all mentioning files", names)
	}
	for i, n := range names {
		if i < len(defining) && !defining[n] {
			t.Fatalf("content:Foo sort:relevance = %v; defining files must rank first", names)
		}
	}
	selected := true
	servicePluginContentSelection.Store(&selected)
	t.Cleanup(func() { servicePluginContentSelection.Store(nil) })
	pq, err := parseQuery(queryOptions{Query: "sym:Run"})
	if err != nil {
		t.Fatal(err)
	}
	text := []byte("#include \"c.h\"\nvoid Foo::Run() {\n  Run();\n}\n")
	lines := contentMatchingLinesAt(`C:\e.cc`, text, newContentLeafMatcher(pq), contentPositiveLeaves(pq), 10, 0, 0)
	if len(lines) != 1 || lines[0].Line != 2 {
		t.Fatalf("sym:Run lines = %+v; want only line 2", lines)
	}
}
