package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// directWriteLOWRSectionFromSpool writes the existing LOWR wire format
// directly from the canonical record spool: header (token count, blob
// length), then the whole offset table, then the whole length table, then
// the blob — the split layout decodeLowerSection reads. It reads the spool
// once, streaming offsets, lengths, and folded names to three bounded temp
// files; the section header is emitted from the final blob size and all
// three temps are streamed to the output in order.
//
// Offsets and lengths are per name token. Direct builds use one token per
// record in spool order, so per-record iteration already yields token order.
// An interleaved (offset,length) per-record table would decode as garbage
// under the split layout and silently underfill every token-addressed lower
// name read (see TestDirectLOWRSectionIsCanonical).
//
// The caller owns alignment and section-table insertion. The returned entry
// starts at the writer's current offset.
func directWriteLOWRSectionFromSpool(ctx context.Context, cw *countingWriter, finalPath string, recordCount int) (indexSectionTableEntry, error) {
	if cw == nil {
		return indexSectionTableEntry{}, errors.New("direct LOWR writer is nil")
	}
	if recordCount < 0 || uint64(recordCount) > uint64(^uint32(0)) {
		return indexSectionTableEntry{}, errors.New("direct LOWR record count out of range")
	}
	if recordCount == 0 {
		return indexSectionTableEntry{}, nil
	}

	blobPath := filepath.Join(filepath.Dir(finalPath), "direct-lowr-blob.tmp")
	offsPath := filepath.Join(filepath.Dir(finalPath), "direct-lowr-offs.tmp")
	lensPath := filepath.Join(filepath.Dir(finalPath), "direct-lowr-lens.tmp")
	defer os.Remove(blobPath)
	defer os.Remove(offsPath)
	defer os.Remove(lensPath)
	blob, err := os.Create(blobPath)
	if err != nil {
		return indexSectionTableEntry{}, err
	}
	blobWriter := bufio.NewWriterSize(blob, 256*1024)
	offs, err := os.Create(offsPath)
	if err != nil {
		_ = blob.Close()
		return indexSectionTableEntry{}, err
	}
	offsWriter := bufio.NewWriterSize(offs, 256*1024)
	lens, err := os.Create(lensPath)
	if err != nil {
		_ = blob.Close()
		_ = offs.Close()
		return indexSectionTableEntry{}, err
	}
	lensWriter := bufio.NewWriterSize(lens, 256*1024)
	closeAll := func() {
		_ = blob.Close()
		_ = offs.Close()
		_ = lens.Close()
	}

	var blobBytes uint64
	count, err := directForEachSpoolName(ctx, finalPath, recordCount, func(_ int, name string) error {
		lower := directLOWRName(name)
		var off uint32 = packedLowerSameAsName
		if lower != name {
			if blobBytes > uint64(^uint32(0)) {
				return errors.New("direct LOWR offset exceeds on-disk limits")
			}
			off = uint32(blobBytes)
			blobBytes += uint64(len(lower))
			if blobBytes > uint64(^uint32(0)) {
				return errors.New("direct LOWR blob exceeds on-disk limits")
			}
			if _, err := blobWriter.WriteString(lower); err != nil {
				return err
			}
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], off)
		if _, err := offsWriter.Write(b[:]); err != nil {
			return err
		}
		var lens [2]byte
		binary.LittleEndian.PutUint16(lens[:], uint16(len(lower)))
		if _, err := lensWriter.Write(lens[:]); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		closeAll()
		return indexSectionTableEntry{}, err
	}
	if count != recordCount {
		closeAll()
		return indexSectionTableEntry{}, errors.New("direct LOWR record count mismatch")
	}
	if err := blobWriter.Flush(); err != nil {
		closeAll()
		return indexSectionTableEntry{}, err
	}
	if err := offsWriter.Flush(); err != nil {
		closeAll()
		return indexSectionTableEntry{}, err
	}
	if err := lensWriter.Flush(); err != nil {
		closeAll()
		return indexSectionTableEntry{}, err
	}
	closeAll()
	blobInfo, err := os.Stat(blobPath)
	if err != nil {
		return indexSectionTableEntry{}, err
	}
	if uint64(blobInfo.Size()) != blobBytes {
		return indexSectionTableEntry{}, errors.New("direct LOWR blob length mismatch")
	}

	offset := uint64(cw.n)
	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(recordCount))
	binary.LittleEndian.PutUint32(header[4:8], uint32(blobBytes))
	if err := directWriteBytes(cw, header[:]); err != nil {
		return indexSectionTableEntry{}, err
	}
	if err := copyFileToWriter(cw, offsPath); err != nil {
		return indexSectionTableEntry{}, err
	}
	if err := copyFileToWriter(cw, lensPath); err != nil {
		return indexSectionTableEntry{}, err
	}
	if err := copyFileToWriter(cw, blobPath); err != nil {
		return indexSectionTableEntry{}, err
	}
	return indexSectionTableEntry{tag: indexSectionLOWR, offset: offset, length: uint64(cw.n) - offset}, nil
}

func copyFileToWriter(cw *countingWriter, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(cw, f)
	return err
}

func directLOWRName(name string) string {
	lower := strings.ToLower(name)
	if len(lower) > int(^uint16(0)) {
		lower = lower[:int(^uint16(0))]
	}
	return lower
}

func directForEachSpoolName(ctx context.Context, finalPath string, expected int, fn func(int, string) error) (int, error) {
	f, err := os.Open(finalPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 256*1024)
	count := 0
	for {
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		default:
		}
		rec, readErr := readDirectSpoolRecord(r)
		if errors.Is(readErr, io.EOF) {
			if count != expected {
				return count, errors.New("direct spool record count mismatch")
			}
			return count, nil
		}
		if readErr != nil {
			return count, readErr
		}
		if count >= expected {
			return count, errors.New("direct spool contains extra records")
		}
		if err := fn(count, rec.Name); err != nil {
			return count, err
		}
		count++
	}
}

func directWriteBytes(cw *countingWriter, p []byte) error {
	n, err := cw.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// These bounded-fixture encoders delegate to the canonical v9 encoders. They
// deliberately keep the existing sorting, deduplication, block metadata, and
// decode contracts in one place until the direct builder has its streaming
// posting pipeline.
func directEncodePATRSection(attrBits map[uint32][]uint32) []byte {
	return encodeAttrPostingSection(attrBits)
}

func directEncodePEXTSection(postings map[string][]uint32, ranks []uint32) []byte {
	return encodeStringPostingSection(postings, ranks)
}

func directEncodePCMPSection(postings map[string][]uint32, ranks []uint32) []byte {
	return encodeStringPostingSection(postings, ranks)
}
