package main

// Compressed build spool: extracted text staged on disk between selection
// (scope-priority order) and assembly (FRN order).
//
// The spool used to hold raw text (1.0x extracted bytes, kept until the last
// shard saved), which capped coverage by the build's transient peak rather
// than the finished index (~0.33x). It now holds one zstd frame per document
// (~0.26x on code at SpeedFastest, with a frame checksum so corruption fails
// the read instead of indexing wrong text), concatenated in a single file
// with the per-doc table in memory. Assembly reads frames by (offset,
// compressed length) and decodes to the recorded original length; output is
// byte-identical to the raw-spool build for the same input because the
// assembler consumes the same bytes in the same order.
//
// The file layout is temp-only (never shipped): concatenated zstd frames, no
// global header. One writer owns the file position during the serial
// selection loop; readers use ReadAt (pread-safe) with one decoder per
// assembly source, so concurrent shard assembles share the file safely.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// contentSpoolWriter stages per-doc compressed frames at the end of f. It is
// not safe for concurrent use; the selection loop is serial.
type contentSpoolWriter struct {
	f       *os.File
	enc     *zstd.Encoder
	staged  int64
	scratch []byte
}

// newContentSpoolWriter wraps an open, empty file staged for appends.
func newContentSpoolWriter(f *os.File) (*contentSpoolWriter, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	return &contentSpoolWriter{f: f, enc: enc}, nil
}

// Append compresses text as one frame at the current end of file and reports
// the frame's offset and compressed length. The caller records those plus
// len(text) to read the doc back.
func (w *contentSpoolWriter) Append(text []byte) (offset int64, compLen int, err error) {
	offset, err = w.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, err
	}
	w.scratch = w.enc.EncodeAll(text, w.scratch[:0])
	if _, err := w.f.Write(w.scratch); err != nil {
		return 0, 0, err
	}
	w.staged += int64(len(w.scratch))
	return offset, len(w.scratch), nil
}

// StagedBytes reports the exact compressed bytes on disk: the spool's share
// of the build peak, credited back by the reserve allowance.
func (w *contentSpoolWriter) StagedBytes() int64 { return w.staged }

// Close releases the encoder. It does not close the file.
func (w *contentSpoolWriter) Close() error {
	if w.enc != nil {
		err := w.enc.Close()
		w.enc = nil
		return err
	}
	return nil
}

// contentSpoolReader random-access reads frames staged by contentSpoolWriter.
// One reader serves one assembly source (serial use); concurrent shard
// assembles use separate readers over the same file.
type contentSpoolReader struct {
	f   *os.File
	dec *zstd.Decoder
}

// newContentSpoolReader opens a reader over a staged spool file.
func newContentSpoolReader(f *os.File) (*contentSpoolReader, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &contentSpoolReader{f: f, dec: dec}, nil
}

// Read decodes the frame at offset (compLen bytes) and returns the doc's
// original bytes. A short read, a checksum failure, or a length mismatch
// errors: the build fails rather than indexing wrong text.
func (r *contentSpoolReader) Read(offset int64, compLen, origLen int) ([]byte, error) {
	if compLen <= 0 || origLen < 0 {
		return nil, fmt.Errorf("content spool: bad frame (offset %d len %d->%d)", offset, compLen, origLen)
	}
	frame := make([]byte, compLen)
	if _, err := r.f.ReadAt(frame, offset); err != nil {
		return nil, err
	}
	text, err := r.dec.DecodeAll(frame, make([]byte, 0, origLen))
	if err != nil {
		return nil, err
	}
	if len(text) != origLen {
		return nil, fmt.Errorf("content spool: frame at offset %d decoded to %d bytes, want %d", offset, len(text), origLen)
	}
	return text, nil
}

// Close releases the decoder. It does not close the file.
func (r *contentSpoolReader) Close() {
	if r.dec != nil {
		r.dec.Close()
		r.dec = nil
	}
}

// contentSpoolGroup is one shard group's reframed spool: the group's frames
// copied out of the shared spool in group (FRN) order, so its shard assembles
// with sequential spool reads. After the group's shard is saved the file is
// closed and removed, so the assembly peak is one group's spool plus the
// shards saved so far instead of the whole corpus plus the whole output.
type contentSpoolGroup struct {
	File *os.File
	Path string
	// Offsets/Lens parallel the group's doc index list: the reframed (offset,
	// compressed length) of each doc.
	Offsets []int64
	Lens    []int
}

// contentSpoolSplitGroups copies frames for each group (doc index lists over
// a shared offsets/compLens table, e.g. contentShardGroup.docs over the
// FRN-sorted selection) out of src into per-group spool files under dir. The
// caller deletes src afterwards and remaps its table to the returned frames;
// output is unaffected because assembly reads the same bytes in the same
// per-shard order either way. Partial files are removed on error.
func contentSpoolSplitGroups(src *os.File, offsets []int64, compLens []int, groups [][]int, dir string) ([]*contentSpoolGroup, error) {
	out := make([]*contentSpoolGroup, 0, len(groups))
	cleanup := func() {
		for _, g := range out {
			if g.File != nil {
				_ = g.File.Close()
			}
			_ = os.Remove(g.Path)
		}
	}
	frame := make([]byte, 0, 64<<10)
	for gi, idx := range groups {
		path := filepath.Join(dir, fmt.Sprintf("spool-%02d", gi))
		f, err := os.Create(path)
		if err != nil {
			cleanup()
			return nil, err
		}
		fail := func(err error) ([]*contentSpoolGroup, error) {
			_ = f.Close()
			_ = os.Remove(path)
			cleanup()
			return nil, err
		}
		g := &contentSpoolGroup{File: f, Path: path}
		var end int64
		for _, di := range idx {
			need := compLens[di]
			if cap(frame) < need {
				frame = make([]byte, 0, need)
			}
			frame = frame[:need]
			if _, err := src.ReadAt(frame, offsets[di]); err != nil {
				return fail(err)
			}
			if _, err := f.Write(frame); err != nil {
				return fail(err)
			}
			g.Offsets = append(g.Offsets, end)
			g.Lens = append(g.Lens, need)
			end += int64(need)
		}
		out = append(out, g)
	}
	return out, nil
}
