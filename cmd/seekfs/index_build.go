package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/sys/windows"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"
)

func cmdIndexUSN(args []string) error {
	fs := flag.NewFlagSet("index-usn", flag.ContinueOnError)
	db := fs.String("db", defaultDB(), "index database path")
	volume := fs.String("volume", "C:", "NTFS volume, for example C:")
	if err := fs.Parse(args); err != nil {
		return err
	}
	start := time.Now()
	idx, err := indexUSNVolume(*volume)
	if err != nil {
		return err
	}
	buildOrders(idx)
	if err := saveIndex(*db, idx); err != nil {
		return err
	}
	fmt.Printf("indexed %d entries from %s via USN in %s\n", len(idx.Entries), *volume, time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdUpgradeIndex(args []string) error {
	fs := flag.NewFlagSet("upgrade-index", flag.ContinueOnError)
	db := fs.String("db", "", "index database path to upgrade")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return errors.New("upgrade-index requires -db")
	}
	idx, err := loadIndex(*db)
	if err != nil {
		return err
	}
	ensureCompactIndexForService(idx)
	if !idx.Compact {
		return errors.New("upgrade-index requires a compact-capable index")
	}
	idx.Version = indexVersion
	if err := saveIndex(*db, idx); err != nil {
		return err
	}
	fmt.Printf("upgraded %s to v9 derived-section format\n", *db)
	return nil
}

func cmdCompactIndex(args []string) error {
	fs := flag.NewFlagSet("compact-index", flag.ContinueOnError)
	db := fs.String("db", "", "index database path whose WAL overlay should be compacted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return errors.New("compact-index requires -db")
	}
	idx, err := loadIndexForService(*db)
	if err != nil {
		return err
	}
	// Compaction only needs FRN and child topology to replay the WAL.  Building
	// the full resident query index here duplicates the expensive writer view
	// and was the dominant peak-memory source for large indexes.
	vol := newCompactionVolumeIndex(*db, idx)
	if err := vol.replayWAL(); err != nil {
		return err
	}
	if err := compactOverlayToDisk(vol); err != nil {
		return err
	}
	if err := removeWAL(*db); err != nil {
		return err
	}
	fmt.Printf("compacted WAL overlay into %s\n", *db)
	return nil
}

// newCompactionVolumeIndex constructs only the state required to apply a WAL
// and produce a compacted index.  It deliberately does not build resident
// postings, path caches, or packed records; the v9 writer builds derived data
// once, from the compacted output.
func newCompactionVolumeIndex(dbPath string, idx *Index) *serviceVolumeIndex {
	vol := &serviceVolumeIndex{
		dbPath:         dbPath,
		index:          idx,
		volume:         idx.Volume,
		journalID:      idx.JournalID,
		checkpoint:     idx.Checkpoint,
		baseCheckpoint: idx.Checkpoint,
		state:          "ready",
		pathCache:      make(map[int]string),
		lastPersist:    time.Now(),
	}
	if idx.Compact && idx.Source == "usn" {
		vol.ownedDirFRNs = ownedReplayDirFRNs(idx.Volume)
		recordCount := idx.compactRecordCount()
		if len(idx.Derived.FRNs) == recordCount && len(idx.Derived.FRNRecordIDs) == recordCount {
			vol.frns = idx.Derived.FRNs
			vol.frnRecordIDs = idx.Derived.FRNRecordIDs
		} else {
			vol.frns = make([]uint64, 0, recordCount)
			vol.frnRecordIDs = make([]uint32, 0, recordCount)
			for id := 0; id < recordCount; id++ {
				rec := idx.compactRecord(id)
				if rec.FRN != 0 {
					vol.frns = append(vol.frns, rec.FRN)
					vol.frnRecordIDs = append(vol.frnRecordIDs, uint32(id))
				}
			}
			sortFRNIndexEntries(vol.frns, vol.frnRecordIDs)
		}
		vol.childOffsets = idx.Derived.ChildOffsets
		vol.childIDs = idx.Derived.ChildIDs
		vol.rootIDs = idx.Derived.RootIDs
		vol.subtreeOrder = idx.Derived.SubtreeOrder
		vol.subtreeStart = idx.Derived.SubtreeStart
		vol.subtreeEnd = idx.Derived.SubtreeEnd
		vol.subtreeBytes = idx.Derived.SubtreeBytes
		if len(vol.childOffsets) == 0 || len(vol.childIDs) == 0 {
			// The fallback subtree walk only needs the packed child graph.  Do
			// not build the optional DFS interval arrays during WAL replay.
			old := os.Getenv("SEEKFS_SUBTREE_INTERVALS")
			_ = os.Setenv("SEEKFS_SUBTREE_INTERVALS", "0")
			vol.buildCompactChildren()
			if old == "" {
				_ = os.Unsetenv("SEEKFS_SUBTREE_INTERVALS")
			} else {
				_ = os.Setenv("SEEKFS_SUBTREE_INTERVALS", old)
			}
		}
	}
	vol.overlay = newOverlaySegment()
	vol.publishSnapshot()
	return vol
}

func cmdIndexVolumes(args []string) error {
	var volumes stringList
	fs := flag.NewFlagSet("index-volumes", flag.ContinueOnError)
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	indexDir := fs.String("index-dir", defaultIndexDir(), "directory for generated .gsi files")
	launch := fs.Bool("launch", false, "launch resident service with the built indexes")
	dryRun := fs.Bool("dry-run", false, "show planned index paths without indexing")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	fs.Var(&volumes, "volume", "NTFS volume to index; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if len(volumes) == 0 && len(cfg.Volumes) > 0 {
		volumes = append(volumes, cfg.Volumes...)
	}
	if len(volumes) == 0 {
		volumes = defaultIndexVolumes()
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	if !*dryRun {
		if _, err := callService(*pipeName, serviceRequest{Command: "status"}); err != nil {
			if setupErr := cmdSetupService([]string{"-pipe", *pipeName, "-no-start"}); setupErr != nil {
				return fmt.Errorf("service unavailable and setup failed: %w", setupErr)
			}
			if startErr := startWindowsService(); startErr != nil {
				return fmt.Errorf("service unavailable and start failed: %w", startErr)
			}
		}
	}
	type result struct {
		Volume  string `json:"volume"`
		DB      string `json:"db"`
		Entries int    `json:"entries"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(volumes))
	dbs := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		vol := normalizeVolume(volume)
		db := defaultVolumeDB(*indexDir, vol)
		if *dryRun {
			results = append(results, result{Volume: vol, DB: db})
			dbs = append(dbs, db)
			continue
		}
		if err := os.MkdirAll(*indexDir, 0o755); err != nil {
			return err
		}
		resp, err := callService(*pipeName, serviceRequest{Command: "index-usn", Volume: vol, DB: db})
		r := result{Volume: vol, DB: db}
		if err != nil {
			r.Error = err.Error()
			results = append(results, r)
			continue
		}
		if !resp.OK {
			r.Error = resp.Message
			results = append(results, r)
			continue
		}
		r.Entries = resp.Entries
		results = append(results, r)
		dbs = append(dbs, db)
	}
	if *launch && len(dbs) > 0 && !*dryRun {
		launchArgs := []string{"-pipe", *pipeName}
		for _, db := range dbs {
			launchArgs = append(launchArgs, "-db", db)
		}
		if err := cmdLaunch(launchArgs); err != nil {
			return err
		}
	}
	if *jsonOut {
		return writeJSON(os.Stdout, struct {
			OK      bool     `json:"ok"`
			Results []result `json:"results"`
		}{OK: len(dbs) == len(volumes), Results: results})
	}
	for _, r := range results {
		if r.Error != "" {
			fmt.Printf("%s -> %s error=%s\n", r.Volume, r.DB, r.Error)
		} else {
			fmt.Printf("%s -> %s entries=%d\n", r.Volume, r.DB, r.Entries)
		}
	}
	if len(dbs) != len(volumes) {
		return errors.New("one or more volumes failed to index")
	}
	return nil
}

func indexUSNVolume(volume string) (*Index, error) {
	vol := normalizeVolume(volume)
	handle, err := openVolume(vol)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle)

	journal, err := queryUSNJournal(handle)
	if err != nil {
		return nil, fmt.Errorf("query USN journal for %s: %w; run elevated or use a service helper for raw volume access", vol, err)
	}

	idx := &Index{
		Version:    indexVersion,
		Roots:      []string{vol + `\`},
		BuiltAt:    time.Now(),
		Source:     "usn",
		Volume:     vol,
		JournalID:  journal.UsnJournalID,
		Checkpoint: journal.NextUsn,
	}
	idx.Compact = true
	idx.CompactAttrs = true

	// Prefer the MFT for the initial build: it carries name, parent, size, and
	// modification time in a single bulk read (the same source Everything uses).
	// Fall back to FSCTL_ENUM_USN_DATA (names only) if the raw MFT read fails.
	if entries, err := enumMFT(handle); err == nil && len(entries) > 0 {
		serviceLog("enum-mft complete volume=%s entries=%d", vol, len(entries))
		if nodes, usnErr := enumUSN(handle, journal.NextUsn); usnErr == nil {
			if added := mergeUSNNodesIntoMFT(entries, nodes); added > 0 {
				serviceLog("enum-usn merge complete volume=%s added=%d total=%d", vol, added, len(entries))
			}
		} else {
			serviceLog("enum-usn merge skipped volume=%s err=%v", vol, usnErr)
		}
		buildRecordsFromMFT(idx, entries)
		serviceLog("compact records complete volume=%s entries=%d source=mft", vol, len(idx.Records))
		return idx, nil
	} else if err != nil {
		serviceLog("enum-mft failed volume=%s err=%v; falling back to USN enum", vol, err)
	}

	nodes, err := enumUSN(handle, journal.NextUsn)
	if err != nil {
		return nil, err
	}
	serviceLog("enum-usn complete volume=%s nodes=%d next_usn=%d", vol, len(nodes), journal.NextUsn)

	frns := make([]uint64, 0, len(nodes))
	for frn := range nodes {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	frnToIndex := make(map[uint64]int, len(frns))
	for i, frn := range frns {
		frnToIndex[frn] = i
	}
	idx.Records = make([]CompactRecord, 0, len(frns))
	for _, frn := range frns {
		node := nodes[frn]
		parent := int32(-1)
		if p, ok := frnToIndex[node.parentFRN]; ok && p != frnToIndex[frn] {
			parent = int32(p)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: node.parentFRN,
			Parent:    parent,
			Name:      node.name,
			Mode:      modeFromAttrs(node.attr),
		})
	}
	serviceLog("compact records complete volume=%s entries=%d source=usn", vol, len(idx.Records))
	return idx, nil
}

func mergeUSNNodesIntoMFT(entries map[uint64]mftEntry, nodes map[uint64]usnNode) int {
	if len(entries) == 0 || len(nodes) == 0 {
		return 0
	}
	added := 0
	for frn, node := range nodes {
		if frn == 0 || node.name == "" {
			continue
		}
		if _, ok := entries[frn]; ok {
			continue
		}
		entries[frn] = mftEntry{
			frn:       frn,
			parentFRN: node.parentFRN,
			name:      node.name,
			attr:      node.attr,
			isDir:     node.attr&fileAttributeDir != 0,
			inUse:     true,
		}
		added++
	}
	return added
}

// buildRecordsFromMFT converts MFT entries into compact records with stable
// parent indexes, populating size and modification time.
func buildRecordsFromMFT(idx *Index, entries map[uint64]mftEntry) {
	idx.CompactAttrs = true
	frns := make([]uint64, 0, len(entries))
	for frn := range entries {
		frns = append(frns, frn)
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })
	frnToIndex := make(map[uint64]int, len(frns))
	for i, frn := range frns {
		frnToIndex[frn] = i
	}
	idx.Records = make([]CompactRecord, 0, len(frns))
	for _, frn := range frns {
		e := entries[frn]
		parent := int32(-1)
		if p, ok := frnToIndex[e.parentFRN]; ok && p != frnToIndex[frn] {
			parent = int32(p)
		}
		idx.Records = append(idx.Records, CompactRecord{
			FRN:       frn,
			ParentFRN: e.parentFRN,
			Parent:    parent,
			Name:      e.name,
			Mode:      modeFromAttrs(e.attr),
			Size:      e.size,
			ModUnix:   e.modUnix,
		})
	}
}

func normalizeVolume(volume string) string {
	volume = strings.TrimSpace(volume)
	volume = strings.TrimRight(volume, `\`)
	if len(volume) == 1 && ((volume[0] >= 'A' && volume[0] <= 'Z') || (volume[0] >= 'a' && volume[0] <= 'z')) {
		volume += ":"
	}
	return strings.ToUpper(volume[:1]) + volume[1:]
}

func openVolume(volume string) (windows.Handle, error) {
	return openVolumeWithFlags(volume, windows.FILE_ATTRIBUTE_NORMAL)
}

// openVolumeOverlapped opens the volume for asynchronous I/O so a blocking
// change-journal read can be aborted with CancelIoEx (see usnOverlappedReader).
func openVolumeOverlapped(volume string) (windows.Handle, error) {
	return openVolumeWithFlags(volume, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED)
}

func openVolumeWithFlags(volume string, flags uint32) (windows.Handle, error) {
	path := `\\.\` + strings.TrimRight(volume, `\`)
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		ptr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
}

func enumUSN(handle windows.Handle, highUSN int64) (map[uint64]usnNode, error) {
	nodes := make(map[uint64]usnNode, 1<<20)
	enumData := mftEnumDataV0{LowUsn: 0, HighUsn: highUSN}
	inSize := uint32(unsafe.Sizeof(enumData))
	buffer := make([]byte, 4*1024*1024)
	for {
		var bytesReturned uint32
		err := windows.DeviceIoControl(
			handle,
			fsctlEnumUSNData,
			(*byte)(unsafe.Pointer(&enumData)),
			inSize,
			&buffer[0],
			uint32(len(buffer)),
			&bytesReturned,
			nil,
		)
		if err != nil {
			if err == windows.ERROR_HANDLE_EOF {
				break
			}
			return nil, fmt.Errorf("enumerate USN data: %w", err)
		}
		if bytesReturned <= 8 {
			break
		}
		enumData.StartFileReferenceNumber = binary.LittleEndian.Uint64(buffer[:8])
		pos := uint32(8)
		for pos+60 <= bytesReturned {
			record := buffer[pos:bytesReturned]
			recordLen := binary.LittleEndian.Uint32(record[0:4])
			if recordLen < 60 || pos+recordLen > bytesReturned {
				break
			}
			major := binary.LittleEndian.Uint16(record[4:6])
			if major == 2 || major == 3 {
				frn, parent, attr, nameLen, nameOff, ok := parseUSNRecordFields(record, major)
				if !ok {
					pos += recordLen
					continue
				}
				if uint32(nameOff)+uint32(nameLen) <= recordLen {
					nameBytes := record[nameOff : uint32(nameOff)+uint32(nameLen)]
					name := windows.UTF16ToString(bytesToUTF16(nameBytes))
					if name != "" {
						nodes[frn] = usnNode{frn: frn, parentFRN: parent, name: name, attr: attr}
					}
				}
			}
			pos += recordLen
		}
	}
	return nodes, nil
}

func readUSNChanges(handle windows.Handle, journalID uint64, startUSN int64, buffer []byte) (int64, []usnChange, error) {
	return readUSNChangesWait(handle, journalID, startUSN, buffer, 0, 0)
}

func readUSNChangesWait(handle windows.Handle, journalID uint64, startUSN int64, buffer []byte, timeout time.Duration, bytesToWaitFor uint64) (int64, []usnChange, error) {
	if len(buffer) < 4096 {
		buffer = make([]byte, 4096)
	}
	req := makeReadUSNJournalRequest(journalID, startUSN, timeout, bytesToWaitFor)
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		handle,
		fsctlReadUSNJournal,
		(*byte)(unsafe.Pointer(&req)),
		uint32(unsafe.Sizeof(req)),
		&buffer[0],
		uint32(len(buffer)),
		&bytesReturned,
		nil,
	)
	if err != nil {
		if err == windows.ERROR_HANDLE_EOF {
			return startUSN, nil, nil
		}
		return startUSN, nil, err
	}
	return parseUSNChangeBuffer(buffer[:bytesReturned])
}

func makeReadUSNJournalRequest(journalID uint64, startUSN int64, timeout time.Duration, bytesToWaitFor uint64) readUSNJournalDataV0 {
	timeoutSeconds := uint64(0)
	if timeout > 0 {
		timeoutSeconds = uint64(timeout.Round(time.Second) / time.Second)
		if timeoutSeconds == 0 {
			timeoutSeconds = 1
		}
	}
	return readUSNJournalDataV0{
		StartUsn:          startUSN,
		ReasonMask:        0xffffffff,
		ReturnOnlyOnClose: 0,
		Timeout:           timeoutSeconds,
		BytesToWaitFor:    bytesToWaitFor,
		UsnJournalID:      journalID,
	}
}

// usnOverlappedReader issues FSCTL_READ_USN_JOURNAL against a volume handle
// opened for asynchronous I/O, so the blocking wait for the next change can be
// aborted with CancelIoEx when the service is stopping or a watchdog restart
// retires the replay loop.  The synchronous readUSNChangesWait path blocks
// inside the kernel and cannot be interrupted, so shutdown and recovery had to
// wait for the next filesystem change on a quiet volume.
type usnOverlappedReader struct {
	handle windows.Handle
	event  windows.Handle
	// req is held on the heap for the reader's lifetime: FSCTL_READ_USN_JOURNAL
	// is METHOD_NEITHER, so the kernel reads this input struct directly and it
	// must stay alive and unmoved until the overlapped request is reaped.
	req readUSNJournalDataV0
}

func openUSNOverlappedReader(volume string) (*usnOverlappedReader, error) {
	handle, err := openVolumeOverlapped(volume)
	if err != nil {
		return nil, err
	}
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return &usnOverlappedReader{handle: handle, event: event}, nil
}

func (r *usnOverlappedReader) close() {
	if r == nil {
		return
	}
	if r.event != 0 {
		windows.CloseHandle(r.event)
		r.event = 0
	}
	if r.handle != 0 {
		windows.CloseHandle(r.handle)
		r.handle = 0
	}
}

// read waits until the journal has at least one record after startUSN, or
// until cancel reports true.  A zero Timeout with a nonzero BytesToWaitFor
// leaves the request outstanding until a record arrives or the I/O is
// canceled, so cancel() is polled while the overlapped request is pending.
// canceled is true when the wait was aborted; the in-flight request is always
// drained before returning so the shared buffer can be reused.
func (r *usnOverlappedReader) read(journalID uint64, startUSN int64, buffer []byte, cancel func() bool) (nextUSN int64, changes []usnChange, canceled bool, err error) {
	if len(buffer) < 4096 {
		buffer = make([]byte, 4096)
	}
	// The overlapped request may outlive this call's local reasoning; keep the
	// reader (which pins r.req) and the output buffer alive until it is reaped.
	defer runtime.KeepAlive(r)
	defer runtime.KeepAlive(buffer)
	r.req = makeReadUSNJournalRequest(journalID, startUSN, 0, 1)
	_ = windows.ResetEvent(r.event)
	ov := &windows.Overlapped{HEvent: r.event}
	var bytesReturned uint32
	ioErr := windows.DeviceIoControl(
		r.handle,
		fsctlReadUSNJournal,
		(*byte)(unsafe.Pointer(&r.req)),
		uint32(unsafe.Sizeof(r.req)),
		&buffer[0],
		uint32(len(buffer)),
		&bytesReturned,
		ov,
	)
	if ioErr != nil {
		if ioErr != windows.ERROR_IO_PENDING {
			if ioErr == windows.ERROR_HANDLE_EOF {
				return startUSN, nil, false, nil
			}
			return startUSN, nil, false, ioErr
		}
		for {
			waitResult, waitErr := windows.WaitForSingleObject(r.event, 250)
			if waitErr != nil {
				_ = windows.CancelIoEx(r.handle, ov)
				_ = windows.GetOverlappedResult(r.handle, ov, &bytesReturned, true)
				return startUSN, nil, false, waitErr
			}
			if waitResult == windows.WAIT_OBJECT_0 {
				break
			}
			if cancel != nil && cancel() {
				_ = windows.CancelIoEx(r.handle, ov)
				_ = windows.GetOverlappedResult(r.handle, ov, &bytesReturned, true)
				return startUSN, nil, true, nil
			}
		}
		if err := windows.GetOverlappedResult(r.handle, ov, &bytesReturned, false); err != nil {
			if err == windows.ERROR_OPERATION_ABORTED {
				return startUSN, nil, true, nil
			}
			if err == windows.ERROR_HANDLE_EOF {
				return startUSN, nil, false, nil
			}
			return startUSN, nil, false, err
		}
	}
	next, parsed, parseErr := parseUSNChangeBuffer(buffer[:bytesReturned])
	return next, parsed, false, parseErr
}

func parseUSNChangeBuffer(buffer []byte) (int64, []usnChange, error) {
	if len(buffer) < 8 {
		return 0, nil, errors.New("USN change buffer too small")
	}
	nextUSN := int64(binary.LittleEndian.Uint64(buffer[:8]))
	changes, err := parseUSNRecords(buffer[8:])
	return nextUSN, changes, err
}

func parseUSNRecords(buffer []byte) ([]usnChange, error) {
	changes := make([]usnChange, 0, 128)
	for pos := uint32(0); pos < uint32(len(buffer)); {
		if uint32(len(buffer))-pos < 60 {
			break
		}
		record := buffer[pos:]
		recordLen := binary.LittleEndian.Uint32(record[0:4])
		if recordLen < 60 || pos+recordLen > uint32(len(buffer)) {
			return nil, errors.New("invalid USN record length")
		}
		major := binary.LittleEndian.Uint16(record[4:6])
		if major == 2 || major == 3 {
			frn, parent, attr, nameLen, nameOff, ok := parseUSNRecordFields(record, major)
			if !ok {
				pos += recordLen
				continue
			}
			if uint32(nameOff)+uint32(nameLen) > recordLen {
				return nil, errors.New("invalid USN record name")
			}
			nameBytes := record[nameOff : uint32(nameOff)+uint32(nameLen)]
			changes = append(changes, usnChange{
				FRN:       frn,
				ParentFRN: parent,
				USN:       parseUSNRecordUSN(record, major),
				Reason:    parseUSNRecordReason(record, major),
				Attr:      attr,
				Name:      windows.UTF16ToString(bytesToUTF16(nameBytes)),
			})
		}
		pos += recordLen
	}
	return changes, nil
}

func parseUSNRecordFields(record []byte, major uint16) (frn, parent uint64, attr uint32, nameLen, nameOff uint16, ok bool) {
	switch major {
	case 2:
		if len(record) < 60 {
			return 0, 0, 0, 0, 0, false
		}
		return fileReferenceRecordNumber(binary.LittleEndian.Uint64(record[8:16])),
			fileReferenceRecordNumber(binary.LittleEndian.Uint64(record[16:24])),
			binary.LittleEndian.Uint32(record[52:56]),
			binary.LittleEndian.Uint16(record[56:58]),
			binary.LittleEndian.Uint16(record[58:60]),
			true
	case 3:
		if len(record) < 76 {
			return 0, 0, 0, 0, 0, false
		}
		return fileReferenceRecordNumber(binary.LittleEndian.Uint64(record[8:16])),
			fileReferenceRecordNumber(binary.LittleEndian.Uint64(record[24:32])),
			binary.LittleEndian.Uint32(record[68:72]),
			binary.LittleEndian.Uint16(record[72:74]),
			binary.LittleEndian.Uint16(record[74:76]),
			true
	default:
		return 0, 0, 0, 0, 0, false
	}
}

func parseUSNRecordUSN(record []byte, major uint16) int64 {
	if major == 3 {
		return int64(binary.LittleEndian.Uint64(record[40:48]))
	}
	return int64(binary.LittleEndian.Uint64(record[24:32]))
}

func parseUSNRecordReason(record []byte, major uint16) uint32 {
	if major == 3 {
		return binary.LittleEndian.Uint32(record[56:60])
	}
	return binary.LittleEndian.Uint32(record[40:44])
}

func fileReferenceRecordNumber(ref uint64) uint64 {
	return ref & 0x0000FFFFFFFFFFFF
}

func bytesToUTF16(b []byte) []uint16 {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return u
}

func buildUSNPath(frn uint64, nodes map[uint64]usnNode, cache map[uint64]string, volume string) string {
	if path, ok := cache[frn]; ok {
		return path
	}
	chain := make([]uint64, 0, 16)
	seen := make(map[uint64]struct{}, 16)
	cur := frn
	var prefix string
	for depth := 0; depth < 1024; depth++ {
		if path, ok := cache[cur]; ok {
			prefix = path
			break
		}
		node, ok := nodes[cur]
		if !ok {
			break
		}
		if _, ok := seen[cur]; ok {
			return ""
		}
		seen[cur] = struct{}{}
		chain = append(chain, cur)
		if node.parentFRN == cur || node.parentFRN == 0 {
			prefix = volume + `\` + node.name
			cache[cur] = prefix
			chain = chain[:len(chain)-1]
			break
		}
		cur = node.parentFRN
	}
	if prefix == "" {
		if len(chain) == 0 {
			return ""
		}
		root := chain[len(chain)-1]
		node := nodes[root]
		prefix = volume + `\` + node.name
		cache[root] = prefix
		chain = chain[:len(chain)-1]
	}
	for i := len(chain) - 1; i >= 0; i-- {
		node := nodes[chain[i]]
		prefix += `\` + node.name
		cache[chain[i]] = prefix
	}
	return cache[frn]
}

func modeFromAttrs(attr uint32) uint32 {
	mode := attr
	if attr&fileAttributeDir != 0 {
		mode |= uint32(os.ModeDir)
	}
	return mode
}

func walkRoot(root string, idx *Index) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		name := d.Name()
		idx.Entries = append(idx.Entries, Entry{
			Path:      path,
			Name:      name,
			LowerPath: strings.ToLower(path),
			LowerName: strings.ToLower(name),
			Size:      info.Size(),
			Mode:      uint32(info.Mode()),
			ModUnix:   info.ModTime().UnixNano(),
		})
		return nil
	})
}

func buildOrders(idx *Index) {
	if idx.Compact {
		idx.CompactNameOrder = make([]int, len(idx.Records))
		for i := range idx.Records {
			idx.CompactNameOrder[i] = i
		}
		sort.Slice(idx.CompactNameOrder, func(i, j int) bool {
			a, b := idx.Records[idx.CompactNameOrder[i]], idx.Records[idx.CompactNameOrder[j]]
			aName, bName := compactLowerName(a), compactLowerName(b)
			if aName == bName {
				return idx.CompactNameOrder[i] < idx.CompactNameOrder[j]
			}
			return aName < bName
		})
		return
	}
	idx.NameOrder = make([]int, len(idx.Entries))
	idx.PathOrder = make([]int, len(idx.Entries))
	for i := range idx.Entries {
		idx.NameOrder[i] = i
		idx.PathOrder[i] = i
	}
	sort.Slice(idx.NameOrder, func(i, j int) bool {
		a, b := idx.Entries[idx.NameOrder[i]], idx.Entries[idx.NameOrder[j]]
		if a.LowerName == b.LowerName {
			return a.LowerPath < b.LowerPath
		}
		return a.LowerName < b.LowerName
	})
	sort.Slice(idx.PathOrder, func(i, j int) bool {
		return idx.Entries[idx.PathOrder[i]].LowerPath < idx.Entries[idx.PathOrder[j]].LowerPath
	})
}

func ensureCompactNameOrderSorted(idx *Index) {
	if idx == nil || !idx.Compact {
		return
	}
	if len(idx.CompactNameOrder) != len(idx.Records) {
		idx.CompactNameOrder = make([]int, len(idx.Records))
		for i := range idx.CompactNameOrder {
			idx.CompactNameOrder[i] = i
		}
	}
	sort.Slice(idx.CompactNameOrder, func(i, j int) bool {
		a, b := idx.Records[idx.CompactNameOrder[i]], idx.Records[idx.CompactNameOrder[j]]
		aName, bName := compactLowerName(a), compactLowerName(b)
		if aName == bName {
			return idx.CompactNameOrder[i] < idx.CompactNameOrder[j]
		}
		return aName < bName
	})
}

func cmdSearch(args []string, countOnly bool) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	var dbs stringList
	configPath := fs.String("config", "", "optional seekfs.toml config path")
	fs.Var(&dbs, "db", "index database path; repeatable")
	useService := fs.Bool("service", false, "query the installed seekfs service over its named pipe")
	forceLocal := fs.Bool("local", false, "do not auto-query the resident service")
	pipeName := fs.String("pipe", defaultServicePipe, "service named pipe")
	jsonOut := fs.Bool("json", false, "write machine-readable JSON")
	limit := fs.Int("n", 100, "maximum results")
	matchPath := fs.Bool("path", false, "match full path")
	under := fs.String("under", "", "only return results under this path")
	exists := fs.Bool("exists", false, "verify result paths still exist")
	cwdBias := fs.Bool("cwd-bias", false, "rank paths under the current working directory first")
	rootBias := fs.String("root-bias", "", "rank paths under this root first")
	recent := fs.String("recent", "", "only return results modified within this duration, for example 24h")
	modifiedAfter := fs.String("modified-after", "", "only return results modified after RFC3339 time or YYYY-MM-DD")
	caseSensitive := fs.Bool("case", false, "case-sensitive query matching")
	fuzzy := fs.Bool("fuzzy", false, "append close matches (edit distance) below exact results")
	if err := fs.Parse(normalizeSearchArgs(args)); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	// Track whether the user pinned concrete indexes with -db. A config file
	// that merely lists DBs must not force a cold local index load on every
	// invocation when the resident service already has those indexes resident.
	explicitDBs := len(dbs) > 0
	if len(dbs) == 0 && len(cfg.DBs) > 0 {
		dbs = append(dbs, cfg.DBs...)
	}
	if *pipeName == defaultServicePipe && cfg.ServicePipe != "" {
		*pipeName = cfg.ServicePipe
	}
	if cfg.OutputFormat == "json" {
		*jsonOut = true
	}
	if *limit == 100 && cfg.DefaultLimit > 0 {
		*limit = cfg.DefaultLimit
	}
	// Prefer the resident service unless the user explicitly asked for a local
	// search (--local) or pinned concrete -db indexes. This keeps the CLI as
	// fast as the UI instead of re-loading the whole index for every query.
	autoService := !*forceLocal && !explicitDBs
	queryArgs := append([]string(nil), fs.Args()...)
	if *matchPath && *under == "" {
		queryArgs, *under = extractUnderPathArg(queryArgs)
	}
	query := strings.TrimSpace(strings.Join(queryArgs, " "))
	if query == "" {
		return errors.New("query required")
	}
	opts := queryOptions{
		Query:         query,
		MatchPath:     *matchPath || queryLooksLoosePathScoped(query),
		Limit:         *limit,
		Under:         *under,
		Exists:        *exists,
		RootBias:      *rootBias,
		Recent:        *recent,
		ModifiedAfter: *modifiedAfter,
		CaseSensitive: *caseSensitive,
		Fuzzy:         *fuzzy,
	}
	if *cwdBias {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		opts.CWDBias = cwd
	}
	if *useService || autoService {
		if err := searchService(*pipeName, opts, countOnly, *jsonOut); err == nil {
			return nil
		}
		if *useService || len(dbs) == 0 {
			return err
		}
		// Auto-service failed but configured DBs exist: fall back to a direct
		// local search so the CLI still works without a running service.
	}
	if len(dbs) == 0 {
		dbs = append(dbs, defaultDB())
	}
	indexes, err := loadIndexes(dbs)
	if err != nil {
		return err
	}
	matches, err := searchAll(indexes, opts, countOnly)
	if err != nil {
		return err
	}
	if *jsonOut {
		resp := jsonSearchResponse{
			OK:       true,
			Query:    query,
			Count:    len(matches),
			Limit:    *limit,
			Complete: boolPtr(true),
		}
		if !countOnly {
			resp.Results = entriesToJSON(matches)
		}
		return writeJSON(os.Stdout, resp)
	}
	if countOnly {
		fmt.Println(len(matches))
		return nil
	}
	w := bufio.NewWriter(os.Stdout)
	for _, entry := range matches {
		fmt.Fprintln(w, entry.Path)
	}
	return w.Flush()
}
