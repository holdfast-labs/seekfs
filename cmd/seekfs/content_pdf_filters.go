package main

// PDF stream filter chain. Every decoder is a bounded transform: its output is
// hard-capped at maxOut and it never allocates from a declared size. A bomb (a
// tiny /FlateDecode input that inflates to gigabytes) is stopped at the cap and
// the caller keeps the bounded prefix; the stream is never fully materialised.
//
// Supported: FlateDecode (+ PNG/TIFF predictors), LZWDecode (PDF early-change),
// ASCIIHexDecode, ASCII85Decode, RunLengthDecode. Image filters (DCTDecode,
// JPXDecode, CCITTFaxDecode, JBIG2Decode) are rejected; a page's text does not
// live in them, so rejecting loses nothing.

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"errors"
	"io"
)

var errContentPDFFilter = errors.New("pdf: unsupported or corrupt stream filter")

// contentPDFDecodeStream applies the filter chain in /Filter order. maxOut is
// the hard output cap (the policy raw cap); truncated reports the cap was hit.
func contentPDFDecodeStream(ctx context.Context, doc *contentPDFDoc, dict map[string]contentPDFValue, raw []byte, maxOut int64) ([]byte, bool, error) {
	if maxOut <= 0 {
		maxOut = contentExtractHardMaxRawBytes
	}
	filters := contentPDFNameList(doc, dict["Filter"])
	parms := contentPDFParmsList(doc, dict["DecodeParms"], len(filters))
	data := raw
	truncated := false
	for i, f := range filters {
		if err := ctx.Err(); err != nil {
			return nil, truncated, err
		}
		var p contentPDFValue
		if i < len(parms) {
			p = parms[i]
		}
		var (
			out []byte
			t   bool
			err error
		)
		switch f {
		case "FlateDecode", "Fl":
			out, t, err = contentPDFFlate(data, maxOut)
			if err == nil {
				var t2 bool
				out, t2 = contentPDFApplyPredictor(out, p, maxOut)
				t = t || t2
			}
		case "LZWDecode", "LZW":
			out, t, err = contentPDFLZW(data, p, maxOut)
			if err == nil {
				var t2 bool
				out, t2 = contentPDFApplyPredictor(out, p, maxOut)
				t = t || t2
			}
		case "ASCIIHexDecode", "AHx":
			out = contentPDFASCIIHex(data, maxOut)
		case "ASCII85Decode", "A85":
			out, t = contentPDFASCII85(data, maxOut)
		case "RunLengthDecode", "RL":
			out, t = contentPDFRunLength(data, maxOut)
		default:
			return nil, truncated, errContentPDFFilter
		}
		if err != nil {
			return nil, truncated, err
		}
		truncated = truncated || t
		data = out
		if len(data) == 0 {
			break
		}
	}
	return data, truncated, nil
}

// contentPDFNameList flattens a /Filter (name or array of names) to names.
func contentPDFNameList(doc *contentPDFDoc, v contentPDFValue) []string {
	if v.kind == contentPDFNull {
		return nil
	}
	v = doc.resolve(v)
	switch v.kind {
	case contentPDFName:
		return []string{v.name}
	case contentPDFArray:
		out := make([]string, 0, len(v.arr))
		for _, e := range v.arr {
			if n, ok := contentPDFNameOf(doc.resolve(e)); ok {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

// contentPDFParmsList aligns /DecodeParms with /Filter, padding with nulls.
func contentPDFParmsList(doc *contentPDFDoc, v contentPDFValue, n int) []contentPDFValue {
	out := make([]contentPDFValue, n)
	if v.kind == contentPDFNull || n == 0 {
		return out
	}
	v = doc.resolve(v)
	if v.kind == contentPDFArray {
		for i := 0; i < n && i < len(v.arr); i++ {
			out[i] = v.arr[i]
		}
		return out
	}
	out[0] = v
	return out
}

func contentPDFReadCapped(r io.Reader, maxOut int64) ([]byte, bool) {
	b, _ := io.ReadAll(io.LimitReader(r, maxOut+1))
	if int64(len(b)) > maxOut {
		return b[:maxOut], true
	}
	return b, false
}

// contentPDFFlate inflates zlib-framed data, falling back to raw deflate for
// writers that omit the zlib header. A partial decode is still returned.
func contentPDFFlate(data []byte, maxOut int64) ([]byte, bool, error) {
	if len(data) == 0 {
		return nil, false, errContentPDFFilter
	}
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		out, t := contentPDFReadCapped(zr, maxOut)
		zr.Close()
		if len(out) > 0 {
			return out, t, nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(data))
	out, t := contentPDFReadCapped(fr, maxOut)
	fr.Close()
	if len(out) == 0 {
		return nil, false, errContentPDFFilter
	}
	return out, t, nil
}

func contentPDFASCIIHex(data []byte, maxOut int64) []byte {
	out := make([]byte, 0, len(data)/2+1)
	var hi byte
	have := false
	for _, c := range data {
		if c == '>' {
			break
		}
		v, ok := contentPDFHexVal(c)
		if !ok {
			continue
		}
		if have {
			out = append(out, hi<<4|v)
			have = false
			if int64(len(out)) >= maxOut {
				return out
			}
		} else {
			hi = v
			have = true
		}
	}
	if have {
		out = append(out, hi<<4)
	}
	return out
}

func contentPDFASCII85(data []byte, maxOut int64) ([]byte, bool) {
	out := make([]byte, 0, len(data)*4/5+4)
	var group [5]byte
	n := 0
	i := 0
	if len(data) >= 2 && data[0] == '<' && data[1] == '~' {
		i = 2
	}
	for ; i < len(data); i++ {
		c := data[i]
		if contentPDFIsWhite(c) {
			continue
		}
		if c == '~' {
			break
		}
		if c == 'z' && n == 0 {
			out = append(out, 0, 0, 0, 0)
			if int64(len(out)) >= maxOut {
				return out, true
			}
			continue
		}
		if c < '!' || c > 'u' {
			continue
		}
		group[n] = c - '!'
		n++
		if n == 5 {
			var acc uint64
			for _, g := range group {
				acc = acc*85 + uint64(g)
			}
			if acc > 0xFFFFFFFF {
				return out, false
			}
			v := uint32(acc)
			out = append(out, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
			n = 0
			if int64(len(out)) >= maxOut {
				return out, true
			}
		}
	}
	if n > 0 {
		for j := n; j < 5; j++ {
			group[j] = 84
		}
		var acc uint64
		for _, g := range group {
			acc = acc*85 + uint64(g)
		}
		if acc > 0xFFFFFFFF {
			return out, false
		}
		v := uint32(acc)
		b := []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
		out = append(out, b[:n-1]...)
	}
	return out, false
}

func contentPDFRunLength(data []byte, maxOut int64) ([]byte, bool) {
	out := make([]byte, 0, len(data)*2)
	for i := 0; i < len(data); {
		n := int(data[i])
		i++
		switch {
		case n == 128:
			return out, false
		case n < 128:
			end := i + n + 1
			if end > len(data) {
				end = len(data)
			}
			if room := maxOut - int64(len(out)); int64(end-i) > room {
				if room > 0 {
					out = append(out, data[i:i+int(room)]...)
				}
				return out, true
			}
			out = append(out, data[i:end]...)
			i = end
		default:
			if i >= len(data) {
				return out, false
			}
			c := data[i]
			i++
			for k := 0; k < 257-n; k++ {
				out = append(out, c)
				if int64(len(out)) >= maxOut {
					return out, true
				}
			}
		}
		if int64(len(out)) >= maxOut {
			return out, true
		}
	}
	return out, false
}

// ----- predictors -----

func contentPDFApplyPredictor(data []byte, parms contentPDFValue, maxOut int64) ([]byte, bool) {
	if parms.kind != contentPDFDict {
		return data, false
	}
	pred := int(contentPDFDictInt(parms.dict, "Predictor", 1))
	if pred <= 1 {
		return data, false
	}
	colors := int(contentPDFDictInt(parms.dict, "Colors", 1))
	bpc := int(contentPDFDictInt(parms.dict, "BitsPerComponent", 8))
	columns := int(contentPDFDictInt(parms.dict, "Columns", 1))
	if colors < 1 || colors > 64 {
		colors = 1
	}
	switch bpc {
	case 1, 2, 4, 8, 16:
	default:
		bpc = 8
	}
	if columns < 1 || columns > 1<<20 {
		columns = 1
	}
	rowLen := (colors*columns*bpc + 7) / 8
	if rowLen <= 0 || rowLen > len(data)+1 {
		return data, false
	}

	if pred == 2 {
		if bpc != 8 {
			return data, false
		}
		for r := 0; r+rowLen <= len(data); r += rowLen {
			row := data[r : r+rowLen]
			for i := colors; i < rowLen; i++ {
				row[i] += row[i-colors]
			}
		}
		return data, false
	}

	// PNG predictors 10..15: every row carries a one-byte filter selector.
	bpp := (colors*bpc + 7) / 8
	if bpp < 1 {
		bpp = 1
	}
	out := make([]byte, 0, len(data))
	prev := make([]byte, rowLen)
	truncated := false
	for pos := 0; pos < len(data); {
		fb := data[pos]
		pos++
		if pos+rowLen > len(data) {
			break
		}
		row := data[pos : pos+rowLen]
		pos += rowLen
		switch fb {
		case 0:
		case 1:
			for i := bpp; i < rowLen; i++ {
				row[i] += row[i-bpp]
			}
		case 2:
			for i := 0; i < rowLen; i++ {
				row[i] += prev[i]
			}
		case 3:
			for i := 0; i < rowLen; i++ {
				row[i] += byte((int(contentPDFPredAt(row, i-bpp)) + int(prev[i])) / 2)
			}
		case 4:
			for i := 0; i < rowLen; i++ {
				row[i] += contentPDFPaeth(contentPDFPredAt(row, i-bpp), prev[i], contentPDFPredAt(prev, i-bpp))
			}
		default:
			return out, truncated
		}
		out = append(out, row...)
		copy(prev, row)
		if int64(len(out)) >= maxOut {
			truncated = true
			break
		}
	}
	return out, truncated
}

func contentPDFPredAt(row []byte, i int) byte {
	if i < 0 || i >= len(row) {
		return 0
	}
	return row[i]
}

func contentPDFPaeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := absInt(p-int(a)), absInt(p-int(b)), absInt(p-int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ----- LZW (PDF variant, MSB-first, /EarlyChange) -----

func contentPDFLZW(data []byte, parms contentPDFValue, maxOut int64) ([]byte, bool, error) {
	early := 1
	if parms.kind == contentPDFDict {
		early = int(contentPDFDictInt(parms.dict, "EarlyChange", 1))
	}
	const (
		clearCode = 256
		eodCode   = 257
	)
	table := make([][]byte, 4096)
	for i := 0; i < 256; i++ {
		table[i] = []byte{byte(i)}
	}
	width := 9
	next := 258
	prev := -1
	out := make([]byte, 0, 256)
	br := contentPDFBitReader{buf: data}
	for {
		code, ok := br.read(width)
		if !ok {
			return out, false, nil
		}
		if code == clearCode {
			width = 9
			next = 258
			prev = -1
			continue
		}
		if code == eodCode {
			return out, false, nil
		}
		var entry []byte
		switch {
		case code < next && table[code] != nil:
			entry = table[code]
		case code == next && prev >= 0:
			entry = append(append([]byte(nil), table[prev]...), table[prev][0])
		default:
			return out, false, errContentPDFFilter
		}
		out = append(out, entry...)
		if prev >= 0 && next < 4096 {
			ne := append(append([]byte(nil), table[prev]...), entry[0])
			table[next] = ne
			next++
		}
		prev = code
		if width < 12 {
			limit := 1 << width
			if early != 0 {
				limit--
			}
			if next >= limit {
				width++
			}
		}
		if int64(len(out)) >= maxOut {
			return out[:maxOut], true, nil
		}
	}
}

type contentPDFBitReader struct {
	buf []byte
	pos int
	bit uint
}

func (b *contentPDFBitReader) read(n int) (int, bool) {
	v := 0
	for i := 0; i < n; i++ {
		if b.pos >= len(b.buf) {
			return 0, false
		}
		v = v<<1 | int((b.buf[b.pos]>>(7-b.bit))&1)
		b.bit++
		if b.bit == 8 {
			b.bit = 0
			b.pos++
		}
	}
	return v, true
}
