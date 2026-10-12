const test = require("node:test");
const assert = require("node:assert");
const query = require("../cmd/seekfs/ui_frontend/query.js");

test("normalizeLiveQuery blanks empty and incomplete input", () => {
  assert.equal(query.normalizeLiveQuery(""), "");
  assert.equal(query.normalizeLiveQuery("."), "");
  assert.equal(query.normalizeLiveQuery("ext:"), "");
  assert.equal(query.normalizeLiveQuery("main.go ."), "");
  assert.equal(query.normalizeLiveQuery("?"), "?");
  assert.equal(query.normalizeLiveQuery("main.go"), "main.go");
  assert.equal(query.normalizeLiveQuery("dir:cmd main"), "dir:cmd main");
});

test("hasIncompleteTrailingToken flags mid-token input", () => {
  assert.equal(query.hasIncompleteTrailingToken("main.go"), false);
  assert.equal(query.hasIncompleteTrailingToken("dir:src"), false);
  assert.equal(query.hasIncompleteTrailingToken("dir:"), true);
  assert.equal(query.hasIncompleteTrailingToken("a b ."), true);
  assert.equal(query.hasIncompleteTrailingToken(""), false);
});

test("toContentQuery turns plain words into content terms and keeps filters", () => {
  assert.equal(query.toContentQuery("needle"), "content:needle");
  assert.equal(query.toContentQuery('needle "two words" ext:go'), 'content:needle content:"two words" ext:go');
  assert.equal(query.toContentQuery("a | b -c content:d"), "content:a | content:b -c content:d");
  assert.equal(query.toContentQuery(""), "");
});

test("contentHighlightTerms extracts literal content terms", () => {
  assert.deepEqual(query.contentHighlightTerms('content:needle content:"two words" content:/re.x/ ext:go'), ["needle", "two words"]);
});

test("highlightSegments marks case-insensitive matches", () => {
  assert.deepEqual(query.highlightSegments("Find the Needle here", ["needle"]), [
    { text: "Find the ", match: false },
    { text: "Needle", match: true },
    { text: " here", match: false },
  ]);
  assert.deepEqual(query.highlightSegments("abc", []), [{ text: "abc", match: false }]);
  assert.deepEqual(query.highlightSegments("aaa", ["a", "aa"]), [
    { text: "aa", match: true },
    { text: "a", match: true },
  ]);
});
