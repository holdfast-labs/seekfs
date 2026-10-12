// Pure query helpers shared by the UI frontend and its node tests.
//
// Loaded as a classic script in index.html (exposes window.SeekfsQuery) and
// require()-able from node so the helpers can be unit tested without a DOM.
(function (root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = api;
  } else {
    root.SeekfsQuery = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  // hasIncompleteTrailingToken reports whether the user is mid-token, e.g. a
  // trailing "." or a bare "ext:" filter with no value yet.
  function hasIncompleteTrailingToken(query) {
    const trimmed = query.trim();
    if (!trimmed) return false;
    const fields = trimmed.split(/\s+/);
    const last = fields[fields.length - 1] || "";
    return last === "." || /:$/i.test(last);
  }

  // normalizeLiveQuery blanks partial input so live search never queries the
  // service for an unfinished token.
  function normalizeLiveQuery(query) {
    if (!query) return "";
    if (/^[^\w*?]{1}$/.test(query)) return "";
    if (hasIncompleteTrailingToken(query)) return "";
    return query;
  }

  // Tokens are whitespace-separated, except that a double-quoted phrase
  // (alone or after a prefix, as in content:"two words") stays one token.
  function queryTokens(query) {
    return (query || "").match(/(?:[^\s"]+|"[^"]*"?)+/g) || [];
  }

  // A token the contents toggle leaves alone: a filter (ext:, path:,
  // content:...), an operator, or a negation.
  function isStructuralToken(token) {
    return /^[a-z][\w-]*:/i.test(token) || token === "|" || /^(or|and|not)$/i.test(token) || token.startsWith("-") || token.startsWith("!");
  }

  // toContentQuery rewrites the plain words of query as content: terms, so
  // with the contents toggle on "needle haystack ext:go" searches inside
  // files for both words while the filter still applies to names.
  function toContentQuery(query) {
    return queryTokens(query)
      .map((token) => (isStructuralToken(token) ? token : `content:${token}`))
      .join(" ");
  }

  // contentHighlightTerms returns the literal content: terms of query, for
  // highlighting in snippets. Regex terms (content:/.../) are skipped.
  function contentHighlightTerms(query) {
    const terms = [];
    for (const token of queryTokens(query)) {
      const m = /^content:(.+)$/i.exec(token);
      if (!m) continue;
      let value = m[1];
      if (value.startsWith("/")) continue;
      value = value.replace(/^"|"$/g, "");
      if (value) terms.push(value);
    }
    return terms;
  }

  // highlightSegments splits text into [{text, match}] around
  // case-insensitive occurrences of any term, longest term first.
  function highlightSegments(text, terms) {
    const out = [];
    const wanted = terms.filter(Boolean).map((t) => t.toLowerCase()).sort((a, b) => b.length - a.length);
    if (!text || !wanted.length) return text ? [{ text, match: false }] : [];
    const lower = text.toLowerCase();
    let pos = 0;
    let plain = 0;
    while (pos < text.length) {
      const hit = wanted.find((t) => lower.startsWith(t, pos));
      if (hit) {
        if (pos > plain) out.push({ text: text.slice(plain, pos), match: false });
        out.push({ text: text.slice(pos, pos + hit.length), match: true });
        pos += hit.length;
        plain = pos;
      } else {
        pos++;
      }
    }
    if (plain < text.length) out.push({ text: text.slice(plain), match: false });
    return out;
  }

  return { hasIncompleteTrailingToken, normalizeLiveQuery, toContentQuery, contentHighlightTerms, highlightSegments };
});
