package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// directComputeDirBytes computes the recursive file-byte total for every
// record (0 for plain files) in one read-only pass over the record spool.  The
// size rank is emitted before the child and subtree sections, so it cannot use
// their in-memory graph; this pre-pass gives SRNK the directory totals it needs
// to order directories by subtree size instead of their stored 0.
func directComputeDirBytes(ctx context.Context, finalPath, frnPath string, recordCount int) ([]uint64, error) {
	if recordCount <= 0 {
		return nil, nil
	}
	frnMap, frns, err := directMapFRNs(frnPath, recordCount)
	if err != nil {
		return nil, err
	}
	if frnMap != nil {
		defer frnMap.close()
	}
	parents := make([]int32, recordCount)
	sizes := make([]uint64, recordCount)
	f, err := os.Open(finalPath)
	if err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 256*1024)
	for id := 0; id < recordCount; id++ {
		if id&0xfffff == 0 {
			select {
			case <-ctx.Done():
				_ = f.Close()
				return nil, ctx.Err()
			default:
			}
		}
		var header [directSpoolHeaderBytes]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			_ = f.Close()
			return nil, err
		}
		mode := binary.LittleEndian.Uint32(header[16:20])
		size := int64(binary.LittleEndian.Uint64(header[20:28]))
		parentFRN := binary.LittleEndian.Uint64(header[8:16])
		nameLen := int(binary.LittleEndian.Uint32(header[36:40]))
		pathLen := int(binary.LittleEndian.Uint32(header[40:44]))
		if nameLen < 0 || pathLen < 0 {
			_ = f.Close()
			return nil, errors.New("direct spool length overflow")
		}
		if _, err := r.Discard(nameLen + pathLen); err != nil {
			_ = f.Close()
			return nil, err
		}
		if mode&uint32(os.ModeDir) == 0 && size > 0 {
			sizes[id] = uint64(size)
		}
		parentID := int32(-1)
		if parentFRN != 0 {
			parentID = directLookupIDMapped(frns, parentFRN)
		}
		if parentID == int32(id) {
			parentID = -1
		}
		parents[id] = parentID
	}
	_ = f.Close()

	counts := make([]uint32, recordCount)
	roots := make([]uint32, 0, 16)
	for id, parent := range parents {
		if parent < 0 {
			roots = append(roots, uint32(id))
		} else {
			counts[parent]++
		}
	}
	offsets := make([]uint32, recordCount+1)
	for i := 0; i < recordCount; i++ {
		offsets[i+1] = offsets[i] + counts[i]
	}
	children := make([]uint32, offsets[recordCount])
	next := append([]uint32(nil), offsets[:recordCount]...)
	for id, parent := range parents {
		if parent >= 0 {
			children[next[parent]] = uint32(id)
			next[parent]++
		}
	}
	order := make([]uint32, 0, recordCount)
	seen := make([]bool, recordCount)
	type frame struct {
		id   uint32
		next uint32
	}
	visit := func(root uint32) {
		if int(root) >= recordCount || seen[root] {
			return
		}
		seen[root] = true
		order = append(order, root)
		stack := []frame{{id: root}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			childStart, childEnd := offsets[top.id], offsets[top.id+1]
			if childStart+top.next < childEnd {
				child := children[childStart+top.next]
				top.next++
				if !seen[child] {
					seen[child] = true
					order = append(order, child)
					stack = append(stack, frame{id: child})
				}
				continue
			}
			stack = stack[:len(stack)-1]
		}
	}
	for _, root := range roots {
		visit(root)
	}
	for id := 0; id < recordCount; id++ {
		if !seen[id] {
			visit(uint32(id))
		}
	}
	if len(order) != recordCount {
		return nil, errors.New("direct dir-bytes traversal did not cover all records")
	}
	bytes := make([]uint64, recordCount)
	for pos := len(order) - 1; pos >= 0; pos-- {
		id := order[pos]
		sum := sizes[id]
		for childPos := offsets[id]; childPos < offsets[id+1]; childPos++ {
			sum += bytes[children[childPos]]
		}
		bytes[id] = sum
	}
	return bytes, nil
}

func directWriteSubtreeSection(ctx context.Context, cw *countingWriter, parentPath, sizesPath string, recordCount int, rankPaths []string, owned *[]string, scratchHigh *int64) ([]indexSectionTableEntry, []directSectionReport, []string, error) {
	parents := make([]int32, recordCount)
	f, err := os.Open(parentPath)
	if err != nil {
		return nil, nil, nil, err
	}
	parentReader := bufio.NewReaderSize(f, 256*1024)
	for id := range parents {
		var b [4]byte
		if _, err := io.ReadFull(parentReader, b[:]); err != nil {
			_ = f.Close()
			return nil, nil, nil, err
		}
		value := binary.LittleEndian.Uint32(b[:])
		if value == ^uint32(0) {
			parents[id] = -1
		} else if value >= uint32(recordCount) {
			_ = f.Close()
			return nil, nil, nil, errors.New("direct subtree parent out of range")
		} else {
			parents[id] = int32(value)
		}
	}
	_ = f.Close()
	sizes := make([]uint64, recordCount)
	sf, err := os.Open(sizesPath)
	if err != nil {
		return nil, nil, nil, err
	}
	sizeReader := bufio.NewReaderSize(sf, 256*1024)
	for id := range sizes {
		var b [8]byte
		if _, err := io.ReadFull(sizeReader, b[:]); err != nil {
			_ = sf.Close()
			return nil, nil, nil, err
		}
		sizes[id] = binary.LittleEndian.Uint64(b[:])
	}
	_ = sf.Close()
	counts := make([]uint32, recordCount)
	roots := make([]uint32, 0, 16)
	for id, parent := range parents {
		if parent < 0 {
			roots = append(roots, uint32(id))
		} else {
			counts[parent]++
		}
	}
	offsets := make([]uint32, recordCount+1)
	for i := 0; i < recordCount; i++ {
		offsets[i+1] = offsets[i] + counts[i]
	}
	children := make([]uint32, offsets[recordCount])
	next := append([]uint32(nil), offsets[:recordCount]...)
	for id, parent := range parents {
		if parent >= 0 {
			children[next[parent]] = uint32(id)
			next[parent]++
		}
	}
	start := make([]uint32, recordCount)
	end := make([]uint32, recordCount)
	for i := range start {
		start[i] = ^uint32(0)
		end[i] = ^uint32(0)
	}
	order := make([]uint32, 0, recordCount)
	type frame struct {
		id   uint32
		next uint32
	}
	visit := func(root uint32) {
		if int(root) >= recordCount || start[root] != ^uint32(0) {
			return
		}
		start[root] = uint32(len(order))
		order = append(order, root)
		stack := []frame{{id: root}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			childStart, childEnd := offsets[top.id], offsets[top.id+1]
			if childStart+top.next < childEnd {
				child := children[childStart+top.next]
				top.next++
				if start[child] == ^uint32(0) {
					start[child] = uint32(len(order))
					order = append(order, child)
					stack = append(stack, frame{id: child})
				}
				continue
			}
			end[top.id] = uint32(len(order))
			stack = stack[:len(stack)-1]
		}
	}
	for _, root := range roots {
		visit(root)
	}
	for id := 0; id < recordCount; id++ {
		visit(uint32(id))
	}
	if len(order) != recordCount {
		return nil, nil, nil, errors.New("direct subtree traversal did not cover all records")
	}
	// Aggregate each record's subtree file bytes.  Children precede their
	// parent in the pre-order, so a reverse pass sees every child first.
	subtreeBytes := make([]uint64, recordCount)
	for pos := len(order) - 1; pos >= 0; pos-- {
		id := order[pos]
		sum := sizes[id]
		for childPos := offsets[id]; childPos < offsets[id+1]; childPos++ {
			sum += subtreeBytes[children[childPos]]
		}
		subtreeBytes[id] = sum
	}
	if err := writeAlignment(cw, 8); err != nil {
		return nil, nil, nil, err
	}
	offset := uint64(cw.n)
	writePart := func(values []uint32) error {
		if err := binary.Write(cw, binary.LittleEndian, uint32(len(values))); err != nil {
			return err
		}
		if len(values) == 0 {
			return nil
		}
		_, err := cw.Write(uint32SliceBytes(values))
		return err
	}
	// The start array is the exact inverse of the order and is rebuilt at
	// load (see decodeDerivedSection); only end and order persist. start
	// stays live as the traversal's visited set above.
	if err := writePart(end); err != nil {
		return nil, nil, nil, err
	}
	if err := writePart(order); err != nil {
		return nil, nil, nil, err
	}
	subtreeRankPaths := make([]string, 0, len(rankPaths))
	for rankIndex, rankPath := range rankPaths {
		rankFile, openErr := os.Open(rankPath)
		if openErr != nil {
			return nil, nil, nil, openErr
		}
		ranks := make([]uint32, recordCount)
		best := make([]uint32, recordCount)
		for id := range best {
			best[id] = ^uint32(0)
		}
		var buf [4]byte
		rankReader := bufio.NewReaderSize(rankFile, 256*1024)
		for id := range ranks {
			if _, err := io.ReadFull(rankReader, buf[:]); err != nil {
				_ = rankFile.Close()
				return nil, nil, nil, err
			}
			ranks[id] = binary.LittleEndian.Uint32(buf[:])
		}
		_ = rankFile.Close()
		for pos := len(order) - 1; pos >= 0; pos-- {
			id := order[pos]
			best[id] = ranks[id]
			for childPos := offsets[id]; childPos < offsets[id+1]; childPos++ {
				if best[children[childPos]] < best[id] {
					best[id] = best[children[childPos]]
				}
			}
		}
		bestPath := filepath.Join(filepath.Dir(parentPath), fmt.Sprintf("direct-subtree-rank-%d.tmp", rankIndex))
		if err := os.WriteFile(bestPath, uint32SliceBytes(best), 0o600); err != nil {
			return nil, nil, nil, err
		}
		*owned = append(*owned, bestPath)
		subtreeRankPaths = append(subtreeRankPaths, bestPath)
		// Canonical SUBT stores no name-rank part and no start part (the
		// exact inverse of the order, rebuilt at load): end/order are
		// followed by size, modified, extension, type, and path subtree
		// minima.
		if rankIndex > 0 {
			if err := writePart(best); err != nil {
				return nil, nil, nil, err
			}
		}
	}
	entry := indexSectionTableEntry{tag: indexSectionSUBT, offset: offset, length: uint64(cw.n) - offset}
	scratch := int64(recordCount) * 4 * 7
	if info, statErr := os.Stat(parentPath); statErr == nil {
		scratch += info.Size()
	}
	if scratch > *scratchHigh {
		*scratchHigh = scratch
	}
	// Directory aggregate sizes: one uint64 per record (0 for files).
	if err := writeAlignment(cw, 8); err != nil {
		return nil, nil, nil, err
	}
	subsOffset := uint64(cw.n)
	if len(subtreeBytes) > 0 {
		if _, err := cw.Write(uint64SliceBytes(subtreeBytes)); err != nil {
			return nil, nil, nil, err
		}
	}
	subsEntry := indexSectionTableEntry{tag: indexSectionSUBS, offset: subsOffset, length: uint64(cw.n) - subsOffset}
	subsScratch := int64(recordCount) * 8
	if subsScratch > *scratchHigh {
		*scratchHigh = subsScratch
	}
	entries := []indexSectionTableEntry{entry, subsEntry}
	reports := []directSectionReport{
		{Name: "SUBT", Tag: indexSectionSUBT, Runs: 0, Bytes: int64(entry.length), ScratchBytes: scratch},
		{Name: "SUBS", Tag: indexSectionSUBS, Runs: 0, Bytes: int64(subsEntry.length), ScratchBytes: subsScratch},
	}
	return entries, reports, subtreeRankPaths, nil
}

func uint32SliceBytes(values []uint32) []byte {
	if len(values) == 0 {
		return nil
	}
	bytes := make([]byte, len(values)*4)
	for i, value := range values {
		binary.LittleEndian.PutUint32(bytes[i*4:], value)
	}
	return bytes
}

func uint64SliceBytes(values []uint64) []byte {
	if len(values) == 0 {
		return nil
	}
	bytes := make([]byte, len(values)*8)
	for i, value := range values {
		binary.LittleEndian.PutUint64(bytes[i*8:], value)
	}
	return bytes
}
