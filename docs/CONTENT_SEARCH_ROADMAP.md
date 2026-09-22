# Content Search — Roadmap to Filename-Search Parity

Status: the parity packages (PB1–PB9, M1–M10) are complete; the remaining
optional item is PB8 items 4–7 (global-lane integration, WP7). **WP11 —
additional file types — has landed all planned extractors except legacy OLE**
(HTML, EML, mbox, RTF, MSG, PDF), with one fuzz target per parser. Companion to
`docs/CONTENT_SEARCH.md` (which documents P0–P5, the accepted feature as it
stands today); the full format detail is in the gitignored
`docs/CONTENT_SEARCH_FORMATS_PLAN.md`.

## 0. The standard

Content search must reach **parity with filename search** in both **speed** and
**completeness**. Filename search is the product core: indexed, continuously
maintained by the service, incremental via USN, multi-volume, self-healing, and
millisecond-scale. Content search today is a *bounded, opt-in, offline-built
auxiliary index*: correct within its scope and honest about degradation, but not
a peer. This roadmap closes the gap.

An independent adversarial audit (P0–P5 code) produced the gap register in §1.
The three items originally proposed (PDF quality, email, journal-reset rebuild)
are real but are **not** the top blockers; the audit found architectural parity
blockers — **PB1–PB4**, the service-owned lifecycle and USN/checkpoint gap —
that dominate them.

## 1. Gap register (audit findings)

Severity: **PB** = parity blocker, **M** = major, **A** = acceptable-but-document.

| ID | Gap | Evidence (file) | Impact |
| --- | --- | --- | --- |
| PB1 | Service never builds/maintains the base `.gsx`; only the offline CLI does. Flag on + no hand-built sidecar ⇒ every `content:` query errors. | `content_cli.go:65,70`; `content_service_wiring.go:44-92` | Completeness/UX: feature unusable without an undocumented manual build. |
| PB2 | The USN delta is in-memory only, never folded into the base, and lost on restart; the base stays frozen at the last manual build. | `content_service.go:110-175`; no service call to `contentSaveFile` | Completeness: files edited since the last build are unsearchable after restart until changed again. |
| PB3 | Changes applied during startup WAL/USN replay are never enqueued: `drainEnabled` is set in `attachContentForVolume`, which runs after the replay loops start. | `service_runtime.go:69-87`; `content_service.go:395-397` | Completeness: silent content staleness after every service restart. |
| PB4 | No content rebuild is scheduled after a real journal reset; the volume is correctly marked `stale` but stays unavailable forever. | `content_service_wiring.go:196-205`; `service_server.go:623-624` | Freshness: content dies where filename search self-heals via MFT rebuild. |
| PB5 | Formats silently not indexed: `.eml`/`.mbox`/`.rtf`/`.html` indexed as raw markup/base64; `.msg` and legacy `.doc/.xls/.ppt` (OLE) skipped as binary; scanned PDFs yield nothing. | `content_extract.go:77-102`; `content_extract_text.go:24-26` | Completeness: the flagship "I remember the phrase, not the filename" cases are invisible. |
| PB6 | Only UTF-8/UTF-16 BOM encodings handled; Windows-1252/Latin-1 is repaired to `U+FFFD`. `parseContentEncoding` exists but is unused; no encoding flag. | `content_extract_text.go:41`; `content_encoding.go:42` | Completeness: legacy/Office text and CP1252 notes miss searches for accented terms. |
| PB7 | Hard 32 MiB per-file cap; larger text files are never indexed. | `content_extract.go:34` | Completeness: large logs/PDFs are filename hits but content misses. |
| PB8 | Broad content queries bypass every fast lane, run per-volume, and can verify millions of records; the whole `.gsx` is heap-resident; posting lists are fully decoded before the cap. | `search.go:385-396`; `candidate_scans.go`; `content_index.go:296`; `content_read.go:218-241` | Speed: seconds-to-minutes and hundreds of MB vs milliseconds for filename search. |
| PB9 | Default content result order is candidate/hash order, not the filename name/rank order. | `content_verify.go` (`sort.Ints`); `search_compact.go` order=candidates | Parity: same query returns differently ordered results. |
| M1 | Delta keeps full normalized text of every changed file, unbounded, no eviction/merge. | `content_service.go:110-175` | Memory grows with churn. |
| M2 | No per-document extraction timeout; `processQueue` is serial and can stall the drain. | `content_service.go:515,539-564` | One pathological file stalls all content work. |
| M3 | `case:true` is not honored for content (always case-insensitive; stored text lowercased). | `content_verify.go:92,105`; `content_build.go:285-288` | Semantics diverge from name search. |
| M4 | Pure substring semantics: no word boundaries, no stemming, no phrase adjacency beyond literal substring. | `content_verify.go`; `content_read.go:198` | Precision/recall differ from user expectation. |
| M5 | `content-index -db` with no `-ext`/`-under` indexes the whole volume by default. | `content_cli.go:19-42` | Resource/semantics footgun. |
| M6 | Posting decode not streamed (tens of millions of entries transiently). | `content_read.go:218-241` | Speed/memory transient. |
| M7 | No `.gsx` eviction/size cap/dedup; sidecar grows and is never pruned/compacted. | — | Disk/latency drift. |
| M8 | Multi-volume content is not federated like filename search; one unusable volume degrades/blocks. | `content_verify.go` usable-volume logic | Parity in multi-volume setups. |
| M9 | count/search divergence for `under:`/`Exists` is silent in the response. | `content_verify.go:561-564` | Trust: count can exceed results with no marker. |
| M10 | CLI-built `.gsx` can race a running service's persist/compaction swap. | `content_cli.go:61` | Correctness of offline build vs live index. |
| A | Binary sniff edges (UTF-16 without BOM rejected; NUL-free binary indexed as garbage); renames keyed by FRN are fine but delta paths can go stale; half-written files extracted on the 2 s tick; cloud/reparse permanently excluded; whole `.gsx` in heap; no `contentResolvePath` e2e; runtime-added volumes not attached; no per-volume drain cancellation; remote callers don't see degraded detail; biased search scans to budget. | `docs/CONTENT_SEARCH.md` §7 | Document or fold into WP1/WP7. |

## 2. Work packages

Each package states: method, effort (S ≈ 1–2 eng-days, M ≈ 3–8, L ≈ 8–20),
validation, dependencies, risks. Estimates are for one engineer already familiar
with the code. §3 orders them and §2.1 totals them.

### WP0 — Content USN checkpoint & format versioning (critical path)
`contentIndex` carries only `Version, Origin, BuiltAt, Docs, Sections`
(`content_index.go:74-80`) and has no USN watermark, so a service build cannot
know where incremental work resumes, and no version gate protects a format
change.
- **Method:** add a content USN checkpoint (build watermark) to the `.gsx`; bump
  the format version; define a migration/rebuild policy for existing sidecars —
  an old-version `.gsx` is detected and rebuilt, never silently attached.
- **Role:** enabling prerequisite for WP1a/WP1c/WP1d and on the critical path.
- **Effort:** M (3–6 d).
- **Validation:** restart and crash tests prove the checkpoint is persisted
  atomically; an old-version `.gsx` triggers a rebuild rather than attaching.
- **Risks:** format churn; the version gate must fail closed.

### WP1 — Service-owned content lifecycle & freshness (PB1, PB2, PB3, PB4, PB9)
The parity core: content becomes a first-class, service-maintained index.
- **WP1a (PB3 + small A-row, S–M):** enable the coordinator/drain before the
  replay loops start (or re-enqueue FRNs touched during replay after attach).
  Also close the small lifecycle A-row gaps: a `contentResolvePath` end-to-end
  test (compact index with a real parent chain), content attach for runtime-added
  volumes (`replaceLoadedVolumeLocked`), and per-volume drain cancellation (loops
  currently end only at `s.stop`). Validation: a restart test — edit a file while
  stopped, start, assert `content:` finds it — plus a test for each closed item.
- **WP1b (PB9, S):** order content results with the same name/rank comparator as
  filename results when no explicit sort. Validation: `content:x` and `x` return
  the same relative order on a fixture.
- **WP1c (PB2, M–L):** fold the delta into the persisted `.gsx`. This is a
  **full re-encode, not an incremental merge**: `assembleContentIndex`
  (`content_build.go:150`) re-derives terms/grams from doc text and there is no
  incremental append. A fold loads base + delta text, re-runs the external
  builder over the whole corpus, `contentSaveFile`, then re-loads +
  `openContentReader` + rebuilds the resolver + `setReady` under the same lock
  discipline (`contentLoadFile` holds the file in heap,
  `content_index.go:322-327`). **Checkpoint atomicity:** the WP0 checkpoint must
  be persisted with the same atomicity as the `.gsx`, or a crash loses the delta
  between the last fold and the crash. If incremental folding is later required,
  the alternative is an LSM/segment delta design. M1 (unbounded delta) is owned
  by WP8, not here. May be merged into WP1d; otherwise effort M–L (8–15 d).
  Validation: churn then restart; all edits findable; delta memory bounded; a
  crash during a fold loses no persisted-and-acknowledged content.
- **WP1d (PB1, L–XL):** service-owned background build/rebuild.
  `buildContentIndexForIndex(ctx, idx, opts)` (`content_build.go:223`) walks the
  live `vol.index`; persist/rebuild close the mmap under `s.indexMu.Lock()`
  (`service_runtime.go:1093-1095`, `service_replay.go:180-181`) while queries
  hold `RLock`. The background build therefore needs `s.indexMu.RLock` for its
  whole duration (blocking persist/rebuild) or a record generation/snapshot;
  holding `vol.mu` is not an option. Add: an IO governor so the build does not
  compete with filename search or replay; a default scope/extension allowlist at
  the service level (fixes M5); per-volume build progress/health that does **not**
  collide with `markDrained` (`content_service.go:344-352`, which returns state
  to `ready` unless `stale`). `rebuildVolumeInPlace`
  (`service_replay.go:160-225`) is synchronous and must gain a background
  continuation. Validation: fresh install + flag on ⇒ content becomes searchable
  without any CLI step; build is resumable/observable; filename-search latency is
  unaffected during a build; an old-version sidecar is rebuilt (WP0).
- **WP1e (PB4, depends on WP1d):** chain the background content build **after**
  `rebuildVolumeInPlace` completes (not merely "reuse WP1d"), hooked at the
  journal-id-changed branch in `service_server.go:623-624`. Validation:
  journal-reset test ⇒ content self-heals to `ready` within the rebuild window,
  never permanently `stale`.
- **Dependencies:** WP0 precedes WP1c/WP1d; WP1d unlocks WP1e; WP1a/WP1b
  independent.
- **Risks:** build cost/IO on large volumes; the IO governor and lock choice are
  the crux; persistence must be crash-safe.

### WP2 — PDF extraction quality (PB5-pdf)
- **Method:** replace the linear `stream` scan with a real (bounded) parser:
  xref tables **and xref streams**, object streams (`/ObjStm`), indirect
  `/Length`, the filter chain (Flate, ASCIIHex, ASCII85, LZW, RunLength), and
  font/text decoding with `/ToUnicode` CMaps and Type0/CID. Decide build-vs-add:
  evaluate a maintained pure-Go library (none in `go.mod` today) against writing
  a bounded parser; prefer a library if licensing/quality/size are acceptable.
  Keep caps and the `Skipped`/`Reason` contract; bump `Version()`.
- **Effort:** L–XL (15–30 d) writing it; M–L (6–12 d) if a suitable library is
  adopted and wrapped. Scanned/image-only PDFs stay `Skipped` unless OCR is
  separately scoped (out of scope).
- **Validation:** a corpus of real PDFs (object-stream, CID/CJK, multi-filter,
  encrypted) asserting extracted text substrings and no regressions; fuzz the
  parser for panics/timeouts; bound decompression.
- **Risks:** PDF is a large surface; a bounded, timeout-guarded parser is
  mandatory (see M2). Licensing if a library is added.
- **Note:** independent and parallelizable with WP3 once the extractor
  interface/versioning is frozen (WP0).

### WP3 — Email & text-markup extraction (PB5-email, PB5-markup)
- **Method:** a `contentEmailExtractor` (`contentClassEmail=5` already reserved)
  for `.eml`/`.mbox` using stdlib `net/mail` + `mime/multipart` +
  `mime/quotedprintable` + `encoding/base64`: index decoded headers (Subject,
  From/To) and text/plain + text/html bodies; skip attachments (or index
  attachment text only when a nested extractor claims it, with recursion bounds
  and a size cap). `.mbox` needs a hand-rolled `From_`-line splitter because
  stdlib `net/mail` parses a single message only. Also add to-text extraction for
  `.html` (strip markup/script/style) and `.rtf` (control words/`\u` escapes),
  closing the PB5 raw-markup gap. `.msg` (OLE/CFB) needs a minimal CFB reader —
  scope separately.
- **Effort:** M (4–8 d) for `.eml`/`.mbox` + HTML/RTF; +1–2 d for the mbox
  splitter; +M (3–5 d) for `.msg`.
- **Validation:** fixtures for multipart/alternative, quoted-printable, base64,
  nested attachments, mbox with several messages, HTML with script/style, RTF
  escapes; assert subject/body terms found and base64 noise not indexed.
- **Risks:** HTML body must be text-extracted, not raw markup; nested-attachment
  recursion must be bounded.

### WP4 — Encoding breadth (PB6) — format-affecting
- **Method:** use the existing `parseContentEncoding` + `golang.org/x/text`
  charmaps; detect legacy single-byte encodings (default Windows-1252 fallback
  instead of `U+FFFD` repair) and optionally a per-root/CLI `-encoding`.
- **Effort:** M (3–5 d).
- **Validation:** CP1252/Latin-1 fixtures with accented terms are findable;
  UTF-8/16 unchanged; no false decodes of binary.
- **Risks:** charset detection is heuristic; must not regress UTF-8. Format-
  affecting for the `U+FFFD` histogram/decoded text: run before WP1d's persistent
  build or cover it in WP0's version bump/rebuild policy.

### WP5 — Legacy OLE Office `.doc/.xls/.ppt` (PB5-ole) — candidate to defer
- **Method:** CFB reader + binary Word/Excel/PowerPoint text extraction.
- **Effort:** L (8–15 d), high format-complexity.
- **Validation:** representative old-format fixtures.
- **Recommendation:** defer unless user documents are predominantly legacy;
  position as "not indexed, documented" until then.

### WP6 — Size policy (PB7) — format-affecting
- **Method:** make the per-file cap configurable; stream/section large text
  rather than all-or-nothing skip; keep a hard safety ceiling.
- **Effort:** S–M (1–3 d).
- **Validation:** a large log above the old cap is indexed up to the new policy;
  memory stays bounded.
- **Note:** changing which text lands in the store is format-affecting; run
  before WP1d's persistent build or cover it in WP0's version bump/rebuild
  policy.

### WP7 — Global-lane & count integration, multi-volume federation (PB8, M6, M8)
Content currently bypasses the global planners via early dispatch
(`search.go:386-396`, `count.go:42-63`). Parity means first-class lanes:
integrate content into the global lane chain and count lanes, the
`globalIDIterator`/`globalRecordID` machinery, per-volume `.gsx` lookups, count
parity, and ordering — the content id space is per-volume FRN/docID, not global,
and that join is the hard part. Add **M8** explicitly: a volume without content
must not block a query filename search can answer. This package also owns the
A-row planner items — the remote degraded detail (the sanitizer omits `Content`,
so remote callers see only `Complete=false`) and the biased-search budget
(`RootBias`/`CWDIBias` disables early-stop and scans to budget before the limit).
Streaming postings (M6) and optional mmap of `.gsx` are the easy tails.
- **Effort:** L–XL (15–30 d).
- **Validation:** the §4 latency bars; a mixed multi-volume query with one
  unusable content volume still returns that volume's filename matches; count ==
  search for non-stat shapes; the degraded flag reaches remote callers.
- **Risks:** overlaps WP1d (build) and the engine R1/R5 work (mmap).

### WP8 — Resource hardening (M1, M2, M5, M9, M10)
- **Method:** bounded delta + eviction between folds (M1), per-document
  extraction timeout + bounded concurrency (M2), make whole-volume builds
  explicit opt-in and coordinate the CLI build with the service (lock/version
  check, M5/M10 — the service-level allowlist lands in WP1d), and surface
  count/search divergence for `under:`/`exists:` with a visible flag (M9).
- **Effort:** M (4–7 d) including M9.
- **Validation:** churn memory stays bounded; a slow file cannot stall the drain;
  CLI build against a live service is rejected or serialized; a stat-shaped
  divergence sets the flag.
- **Status:** M2, M5, M10 and M9 are **done** (M10 Windows-only; M9's local
  flag plus the remote projection of a coarse `content` health block). M1 is
  done for the normal and over-cap paths — the fold trigger bounds the delta
  between folds, and the hard ceiling refuses new distinct docs while the
  sidecar is capped. **Residual (documented):** the hard ceiling is only
  enforced while the sidecar is capped, because that is the one case where a
  fold provably cannot advance the persisted checkpoint, so deferring cannot
  lose a change. A *persistent* non-cap fold failure (e.g. a full disk) can
  still grow the delta; a correct fix needs per-FRN USN tracking plus a
  checkpoint clamp so deferred changes replay after a restart, which touches
  the PB2/WP1c checkpoint semantics and is its own package. To make the growth
  visible meanwhile, health now reports the resident `delta_bytes` and a
  `fold_error` (cleared on the next successful fold).

### WP9 — Semantics (M3, M4) — decide, then scope
- **Method:** honor `case:true` for content (store a case-preserving text section
  or index case-folded with a case flag); decide whether to add word-boundary or
  token matching (with stemming) vs keeping literal substring semantics. The
  case-preserving text section is **format-affecting** — run before WP1d's
  persistent build or cover it in WP0's version bump/rebuild policy.
- **Effort:** M–L (5–12 d) depending on the decision.
- **Validation:** `case:true content:Needle` excludes `needle`; a documented
  boundary rule with tests.
- **Note:** this also unlocks case-preserving snippets (a deferred item).

### WP10 — `.gsx` eviction / size cap / dedup (M7)
- **Method:** cap the sidecar size, evict/compact stale documents, and dedup
  where it pays. Distinct from the in-memory delta bound in WP8 (M1).
- **Effort:** M (3–6 d).
- **Validation:** the sidecar stays under the cap across sustained churn and
  query latency does not drift.

### WP11 — Additional file types (next steps)
The next major body of work after the parity packages: real extraction for the
formats PB5 still drops. Full detail — parsing specifics, licensing, security,
test strategy, open questions — lives in the gitignored
`docs/CONTENT_SEARCH_FORMATS_PLAN.md`; this section is the durable summary.
- **Scope & order (value-per-effort, interface frozen first):** HTML → EML
  (+`.emlx`/`.mht`/`.mhtml`) → mbox → RTF → MSG → PDF (separate track) → legacy
  OLE (defer). CSV/JSON/XML/YAML/log are already text; `.pst/.ost/.one/iWork/
  images` are documented not-indexed, not implemented.
- **Method (per format, hand-roll vs library):**
  - **HTML** (S): adopt `golang.org/x/net/html`'s **tokenizer** (not `Parse`) —
    BSD-3, already in the module graph, zero new download; promote
    indirect→direct. Mandatory `SetMaxBuf` + per-token `ctx.Err()` + `maxText`
    truncation; charset via `x/net/html/charset`.
  - **EML** (M): pure stdlib — `net/mail`, `mime`, `mime/multipart`,
    `mime/quotedprintable`, `encoding/base64`; RFC 2047 headers via
    `x/text/encoding/htmlindex`. Body-only by default; attachment recursion
    bounded, disabled unless a nested extractor claims the filename.
  - **mbox** (S): hand-rolled `From_`-line splitter reusing the EML parser.
  - **RTF** (S–M): hand-rolled bounded control-word/group parser (`\uN`, `\'hh`,
    `\par`; skip `\pict`/`\objdata`); shared with MSG's compressed body.
  - **MSG** (M–L): hand-rolled bounded CFB reader + MAPI property streams +
    bounded LZFu — or `github.com/richardlehane/mscfb` (Apache-2.0) if the
    maintainer prefers; reuses the RTF parser.
  - **PDF** (separate track, L–XL write / M–L library): bounded **hand-rolled
    text-layer** parser — the pure-Go libraries are unsatisfying
    (`ledongthuc/pdf` panics/unbounded, `pdfcpu` has no text extraction,
    `unipdf` commercial/cgo rejected). Bounded xref/ObjStm/filters/ToUnicode.
  - **Legacy OLE DOC/XLS/PPT** (L): defer; per-format binary parsers (Word piece
    table, BIFF8 SST, PPT text atoms). Document as not-indexed until demand.
- **Pre-requisites (P1–P4) — land before the first parser:**
  - **P1 (blocking, decision):** `ContentType`/`ExtractorVersion` are persisted
    but never populated (`content_build.go:252-261`;
    `content_service.go:1178`), so a per-format `Version()` bump is inert and the
    only invalidation is a whole-`.gsx` `contentIndexVersion` rebuild. Wire both
    fields and decide per-doc re-extraction vs whole-`.gsx` bump.
  - **P2 (blocking):** the offline walk builder has no panic recovery
    (`content_build.go:151`); route it through `contentBuildDocSafe` (or a local
    `recover`) and count a panic as `Skipped`.
  - **P3:** M2 — enforce the per-document timeout / `ctx` at the coordinator
    boundary (goroutine + `select` on `ctx.Done()` + deadline) and apply it to
    the offline builder too; one ctx-ignoring parser otherwise wedges the serial
    drain.
  - **P4 (design gate):** keep first-party parsers in-process under the existing
    bound patterns plus one fuzz test per parser; route untrusted/third-party
    parsers through a documented out-of-process protocol behind the same
    `contentExtractor` interface.
  - **Module refactor:** move extraction into `internal/contentextract`, one
    subpackage per format; the service allowlist auto-unions each subpackage's
    `Extensions()` (`content_service_build.go:175-186`), so a new extractor
    auto-scopes without a central edit.
- **Dependency decisions to record:** promote `x/net/html` to direct; PDF
  hand-rolled vs `ledongthuc/pdf`; MSG container hand-rolled vs `go-cfb`/`mscfb`;
  P1 per-doc invalidation vs whole-`.gsx` bump.
- **Effort:** ~25–50 d total — HTML S, EML M, mbox S, RTF S–M, MSG M–L, PDF
  L–XL (write) / M–L (library); legacy OLE deferred. Pre-requisites P1/P2 are
  cheap and block item 1; P3/P4 scale with the number of parsers.
- **Validation:** per-format synthetic fixtures (mirroring
  `contentOOXMLTestZip`/`contentPDFTestDoc`) asserting needle-in / boilerplate-out
  (no `<script>`, base64 noise, `\fonttbl`, control words), `Skipped`+`Reason`
  for encrypted/scanned/malformed, `Truncated` over cap, `len(Text) ≤ maxText`;
  one `FuzzContent<Format>Extractor` per parser (no panic, bounded, ctx honored);
  offline differential goldens outside CI. **Status:** landed — synthetic
  fixtures per format, an allowlist assertion per format, and the six fuzz
  targets (HTML/EML/mbox/RTF/MSG/PDF) are in `content_extract_fuzz_test.go`;
  campaigns found and fixed an RTF UTF-8 rune-split and a PDF `/Length`-absent
  `endstream` rescan that could burn seconds on a ≤1 MiB input.
- **Risks:** every new parser is a hostile-input surface (decompression bombs,
  malformed CFB/xref loops); format-affecting changes must go through P1/WP0.
- **Decisions already made (do not relitigate):** PDF **text-layer only, no
  OCR**; encrypted PDF → `Skipped`; no OCR anywhere; scanned/image-only PDFs →
  `Skipped` ("no extractable text").
- **Note:** independent and parallelizable with the WP7 global-lane work once
  the extractor interface/versioning is frozen; it absorbs and concretizes the
  earlier WP2 (PDF), WP3 (email/markup), and WP5 (legacy OLE) sketches.

### 2.1 Effort summary

| WP | Scope | Effort |
| --- | --- | --- |
| WP0 | Content USN checkpoint + format versioning | M (3–6 d) |
| WP1a/WP1b | Early drain + ordering, small A-row lifecycle items | S–M (2–4 d) |
| WP1c | Delta fold (full re-encode); merge into WP1d or | M–L (8–15 d) |
| WP1d | Background service build (locking/snapshot, IO governor) | L–XL (15–30 d) |
| WP1e | Journal-reset rebuild continuation (chained after WP1d) | (within WP1d) |
| WP2 | PDF extraction | L–XL (15–30 d write) / M–L (6–12 d library) |
| WP3 | Email + HTML/RTF to-text | M (4–8 d) + 1–2 d (mbox) |
| WP4 | Encoding breadth | M (3–5 d) |
| WP5 | Legacy OLE Office (candidate to defer) | L (8–15 d) |
| WP6 | Size policy | S–M (1–3 d) |
| WP7 | Global-lane + count integration, multi-volume federation | L–XL (15–30 d) |
| WP8 | Resource hardening (with M9) | **done** except the M1 non-cap fold-failure residual |
| WP9 | Semantics | M–L (5–12 d) |
| WP10 | `.gsx` eviction/size cap/dedup (M7) | M (3–6 d) |
| WP11 | Additional file types (HTML→EML→mbox→RTF→MSG→PDF; legacy OLE deferred) | **done** except legacy OLE |

Corrected program total: **~60–110 engineer-days**, with the content-checkpoint /
format-versioning work (WP0) on the critical path. WP11 is a separate,
independently orderable track adding **~25–50 engineer-days**; legacy OLE stays
deferred.

## 3. Recommended order (each gated by a reviewer, implemented one at a time)

1. **WP0** — content USN checkpoint + format-version/rebuild policy. Design the
   checkpoint before WP1c/WP1d; format-affecting work depends on its version gate.
2. **WP1a/WP1b** — lifecycle drain + result ordering; cheap, immediate parity
   wins (plus the small A-row lifecycle items).
3. **Format-affecting work: WP4 (encoding), WP6 (size policy), WP9 (case
   section)** — must precede WP1d's persistent build, or WP0's format bump +
   staged-rebuild policy must explicitly cover it.
4. **WP1c/WP1d** — full re-encode fold and the background, IO-governed,
   snapshot-locked service build.
5. **WP1e** — journal-reset rebuild, chained after `rebuildVolumeInPlace`.
6. **WP7** — can run alongside WP1d if staffed (both touch the lane/planner
   surface); otherwise after WP1e.
7. **WP2 (PDF) and WP3 (email + markup)** — independent and parallelizable once
   the extractor interface/versioning is frozen.
8. **WP8** — hardening (M1, M2, M5, M9, M10) — **done** except the M1
   non-cap fold-failure residual (see §WP8).
9. **WP10** — `.gsx` eviction/size cap/dedup (M7).
10. **WP5** — legacy OLE (defer unless required).
11. **WP11 — additional file types** — **done** except legacy OLE: HTML → EML →
    mbox → RTF → MSG → PDF, each reviewed and committed, with the P1/P2/P3
    pre-requisites (per-doc extractor refresh, offline panic isolation,
    coordinator deadline) already landed and the six fuzz targets in place.
    Legacy OLE stays deferred (WP5). Full detail in
    `docs/CONTENT_SEARCH_FORMATS_PLAN.md`.

The originally proposed three items map to **WP2 (PDF)**, **WP3 (email)**, and
**WP1e (journal-reset rebuild)**; the roadmap places WP0/WP1 ahead of them
because the audit shows the index-lifecycle and format-versioning gaps are the
dominant parity blockers.

## 4. Parity acceptance criteria (definition of done)

Content search is at parity when, on a service with the flag enabled, the index
is built, persisted, and maintained **by the service** — no manual CLI step —
degradation is always visible, and the following measurable bars hold:

- **Latency.** On the 28M-record `C:` volume: p95 selective `content:term`
  ≤ 50 ms; broad/common `content:term` ≤ 500 ms; and always ≤ 3× the equivalent
  filename query p95. No full-volume scan — a trace shows no `bounded-scan`
  source for common terms.
- **Memory.** Service private bytes after content attach ≤ the filename baseline
  + 250 MB (or a stated absolute cap); the `.gsx` is not wholly heap-resident.
- **Restart (defined).** Service stopped; create/modify/delete files; start.
  Content findability equals filename findability within N seconds of `ready`,
  and the delta survived the restart.
- **Journal reset (defined).** Force a journal-id change/rebuild; content
  self-heals to `ready` within the rebuild window and never remains `stale`.
- **Formats.** Indexed-format list enumerated and tested — text/code/config/
  markdown/JSON/XML/CSV; `.docx/.docm/.xlsx/.xlsm/.pptx/.pptm/.odt/.ods/.odp/
  .epub`; `.pdf`; `.eml/.mbox`; `.html` — plus an explicit, user-visible
  NOT-indexed list (`.doc/.xls/.ppt/.msg`, scanned PDFs, over-cap,
  cloud/reparse); never silently dropped.
- **Count.** count == search for non-stat shapes; divergence (`under:`/`exists:`)
  sets a visible flag.
- **Ordering.** `content:x` and `x` return the same relative order on a fixture.

## 5. Explicit non-goals

OCR of scanned PDFs; semantic/embedding search (separate plan); network/
client-server content search; CJK segmentation beyond what PDF/encoding
extraction yields; full legacy OLE Office unless WP5 is pulled in.
