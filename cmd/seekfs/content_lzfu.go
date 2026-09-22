package main

// Bounded LZFu (MS-OVBA / [MS-OXRTFCP]) decompression for the compressed RTF
// body of a .msg file (PidTagRtfCompressed, 0x1009).
//
// Adapted from github.com/ffois/mailfmt internal/rtf/rtf.go (MIT License,
// Copyright (c) 2025 ffois): the 207-byte dictionary preload and the
// control-byte/back-reference loop are modelled on that decompressor. This copy
// adds a hard output cap: the caller passes the extraction policy's maxText, the
// loop stops the moment that many bytes exist, and the preallocated capacity is
// clamped to min(rawSize, 17x input, maxText). rawSize is a 4-byte field read
// straight from the stream, so without the clamp a ~20-byte payload declaring
// 0xFFFFFFFF would reserve 4 GiB before decompression starts.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	contentLZFuCompressedMagic   = 0x75465A4C // "LZFu"
	contentLZFuUncompressedMagic = 0x414C454D // "MELA"
	contentLZFuDictSize          = 4096
)

// contentLZFuDictInit is the 207-byte dictionary preload mandated by
// [MS-OXRTFCP]; it must be byte-exact or decompression diverges.
var contentLZFuDictInit = []byte("{\\rtf1\\ansi\\mac\\deff0\\deftab720{\\fonttbl;}" +
	"{\\f0\\fnil \\froman \\fswiss \\fmodern \\fscript \\fdecor MS Sans SerifSymbolArial" +
	"Times New RomanCourier{\\colortbl\\red0\\green0\\blue0" +
	"\r\n" +
	"\\par \\pard\\plain\\f0\\fs20\\b\\i\\u\\tab\\tx")

// contentLZFuDecompress inflates a PidTagRtfCompressed stream into raw RTF
// bytes, emitting at most maxOut bytes. truncated reports that the output cap
// (or the sender's rawSize) cut the result; a stream that simply runs out of
// source bytes is not an error, since the control-byte loop carries no length
// of its own.
func contentLZFuDecompress(ctx context.Context, data []byte, maxOut int64) ([]byte, bool, error) {
	if len(data) < 16 {
		return nil, false, errors.New("lzfu: stream too short")
	}
	compSize := binary.LittleEndian.Uint32(data[0:4])
	rawSize := binary.LittleEndian.Uint32(data[4:8])
	magic := binary.LittleEndian.Uint32(data[8:12])

	if maxOut < 0 {
		maxOut = 0
	}

	switch magic {
	case contentLZFuUncompressedMagic:
		end := int64(16) + int64(rawSize)
		if rawSize == 0 || end > int64(len(data)) {
			end = int64(len(data))
		}
		src := data[16:end]
		truncated := int64(rawSize) > maxOut
		if int64(len(src)) > maxOut {
			src = src[:maxOut]
			truncated = true
		}
		out := make([]byte, len(src))
		copy(out, src)
		return out, truncated, nil
	case contentLZFuCompressedMagic:
	default:
		return nil, false, fmt.Errorf("lzfu: unknown compression type 0x%08X", magic)
	}

	// compSize counts the 12 bytes after the first 4 (rawSize + magic + crc)
	// plus the payload, so the stream ends at compSize+4.
	end := int64(compSize) + 4
	if end > int64(len(data)) || end < 16 {
		end = int64(len(data))
	}
	src := data[16:end]

	// A control byte followed by eight 17-byte back-references is 136 output
	// bytes per 17 input bytes, so 17x input is the physical ceiling; rawSize is
	// the sender's claim and may be absurd. Clamp the reservation to the
	// smallest of the three, and let append grow past it only as real output
	// arrives (bounded by maxOut).
	capHint := int64(rawSize)
	if maxExpansion := int64(len(src)) * 17; capHint > maxExpansion {
		capHint = maxExpansion
	}
	if capHint > maxOut {
		capHint = maxOut
	}
	if capHint < 0 {
		capHint = 0
	}
	out := make([]byte, 0, capHint)
	dict := make([]byte, contentLZFuDictSize)
	wp := copy(dict, contentLZFuDictInit)
	mask := contentLZFuDictSize - 1

	truncated := false
	i := 0
	for i < len(src) {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		control := src[i]
		i++
		for bit := 0; bit < 8; bit++ {
			if int64(len(out)) >= maxOut {
				truncated = true
				return out, truncated, nil
			}
			if control&(1<<uint(bit)) == 0 {
				if i >= len(src) {
					return out, truncated, nil
				}
				c := src[i]
				i++
				out = append(out, c)
				dict[wp] = c
				wp = (wp + 1) & mask
			} else {
				if i+1 >= len(src) {
					return out, truncated, nil
				}
				hi, lo := src[i], src[i+1]
				i += 2
				offset := (int(hi) << 4) | (int(lo) >> 4)
				length := (int(lo) & 0x0F) + 2
				if offset == wp {
					return out, truncated, nil // end-of-stream marker
				}
				for j := 0; j < length; j++ {
					c := dict[(offset+j)&mask]
					out = append(out, c)
					dict[wp] = c
					wp = (wp + 1) & mask
				}
				// A back-reference can carry the output up to 16 bytes past the
				// hard cap; clamp here so every later in-loop return honours it.
				if int64(len(out)) > maxOut {
					out = out[:maxOut]
					truncated = true
				}
			}
			if rawSize > 0 && int64(len(out)) >= int64(rawSize) {
				return out, truncated, nil
			}
		}
	}
	// A back-reference may have overshot maxOut by up to 16 bytes; clamp so the
	// returned length honours the hard cap.
	if int64(len(out)) > maxOut {
		out = out[:maxOut]
		truncated = true
	}
	return out, truncated, nil
}
