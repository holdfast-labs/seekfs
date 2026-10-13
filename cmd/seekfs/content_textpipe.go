package main

// Ordered text-store pipeline for index assembly.
//
// The streaming assembler's serial emit loop feeds postings (term/gram Adds,
// spill sorts) and the zstd text store from one thread; the text encode is
// single-threaded and overlaps nothing. The pipe moves text encoding to its
// own thread: emit sends stored slices (ownership transferred) and keeps
// doing posting work, while the pipe thread Appends them in order. Frames,
// block cuts, the dictionary switch and the section file are byte-identical
// to serial assembly: messages are processed strictly in send order, and
// offsets are assigned by the emit thread from a running sum that mirrors
// the writer's logical size exactly.
//
// Backpressure is by channel capacity (contentTextPipeDepth documents); the
// emit thread blocks on send when the encoder falls behind, so outstanding
// text stays bounded by in-flight batches as before. The stream ends with
// exactly one terminal action: finish (writes the section file and reports
// its error) or abort (discards everything and exits).

const contentTextPipeDepth = 64

type contentTextMsgKind int

const (
	contentTextMsgAppend contentTextMsgKind = iota
	contentTextMsgSetDict
	contentTextMsgFinish
)

// contentTextMsg is one ordered pipe request. appendMsg carries a stored
// document (ownership transferred); setDictMsg installs the shard's trained
// dictionary from this point on; finishMsg ends the stream.
type contentTextMsg struct {
	kind   contentTextMsgKind
	stored []byte
	dictID uint32
	dict   []byte
	path   string
}

// contentTextPipe owns a text block writer on its own goroutine.
type contentTextPipe struct {
	writer *contentTextBlockWriter
	ch     chan contentTextMsg
	fin    chan error
}

// startContentTextPipe launches the pipe thread for an owned writer. The
// caller must end the stream exactly once (finish or abort) and must not
// touch the writer afterwards.
func startContentTextPipe(writer *contentTextBlockWriter) *contentTextPipe {
	p := &contentTextPipe{
		writer: writer,
		ch:     make(chan contentTextMsg, contentTextPipeDepth),
		fin:    make(chan error, 1),
	}
	go p.run()
	return p
}

func (p *contentTextPipe) run() {
	w := p.writer
	var firstErr error
	for m := range p.ch {
		switch m.kind {
		case contentTextMsgAppend:
			if firstErr == nil {
				if err := w.Append(m.stored); err != nil {
					firstErr = err
					w.abort()
				}
			}
		case contentTextMsgSetDict:
			if firstErr == nil {
				if err := w.setDict(m.dictID, m.dict); err != nil {
					firstErr = err
					w.abort()
				}
			}
		case contentTextMsgFinish:
			if firstErr != nil {
				p.fin <- firstErr
				return
			}
			p.fin <- w.FinishFile(m.path)
			return
		}
	}
	// Aborted: release the writer's temp files and exit. fin is never read
	// on this path (abort and finish are mutually exclusive).
	w.abort()
}

// sendAppend hands one stored document to the pipe thread (ownership
// transferred: the caller must not touch the slice afterwards). It blocks
// when the pipe is a full channel behind (backpressure).
func (p *contentTextPipe) sendAppend(stored []byte) {
	p.ch <- contentTextMsg{kind: contentTextMsgAppend, stored: stored}
}

// sendSetDict installs the shard dictionary from this stream point on.
func (p *contentTextPipe) sendSetDict(id uint32, dict []byte) {
	p.ch <- contentTextMsg{kind: contentTextMsgSetDict, dictID: id, dict: dict}
}

// finish ends the stream, writing the section file at path on the pipe
// thread, and reports the first error (or nil).
func (p *contentTextPipe) finish(path string) error {
	p.ch <- contentTextMsg{kind: contentTextMsgFinish, path: path}
	return <-p.fin
}

// abort discards the stream (emit failed elsewhere) and releases the
// writer's temp files. The caller must send nothing afterwards.
func (p *contentTextPipe) abort() {
	close(p.ch)
}
