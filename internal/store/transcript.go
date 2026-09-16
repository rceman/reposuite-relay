// transcript.go implements the compact durable transcript primitive:
// an append-only transcript.jsonl plus a derived transcript.idx of
// fixed-width uint64 record offsets for O(1) bounded tail reads.
//
// The Relay transcript holds presentation/audit/reconnect records only —
// it is NOT the model context and is never fed back into a harness. The
// native harness store stays authoritative for conversation state.
//
// seq is assigned by the caller's canonical event allocator and only
// needs to strictly increase: gaps are legal because transient live
// events may consume sequence values without being persisted.
//
// Crash model: append writes the JSONL record and syncs BEFORE the index
// offset, so the index never intentionally commits ahead of the
// transcript. A crash between the two leaves an unindexed tail record,
// which the next Open repairs by rebuilding the derived index. A partial
// final JSON line is crash residue and is truncated; a malformed complete
// record in the interior is corruption and fails closed.
package store

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RecordVersion is the durable record envelope version.
const RecordVersion = 1

// ErrRecordTooLarge means one canonical durable record alone exceeds the
// caller's byte budget: the record cannot be served within the bound and
// is reported explicitly instead of allocating an unbounded response.
// The transcript itself is untouched.
var ErrRecordTooLarge = errors.New("record exceeds byte budget")

// Record is one durable transcript entry.
type Record struct {
	Version int             `json:"version"`
	Seq     uint64          `json:"seq"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (r Record) validate() error {
	if r.Version != RecordVersion {
		return fmt.Errorf("unsupported record version %d", r.Version)
	}
	if r.Seq == 0 {
		return fmt.Errorf("seq must be > 0")
	}
	if r.Type == "" {
		return fmt.Errorf("empty type")
	}
	if r.At.IsZero() {
		return fmt.Errorf("missing timestamp")
	}
	return nil
}

// Transcript is the open durable transcript of one session. Appends are
// serialized internally; Tail reads seek directly through the index.
// After a canonical-side failure (JSONL write/sync, unrecoverable index
// error) the instance is poisoned: every subsequent op returns the same
// error deterministically until Close/reopen — never operating from
// stale in-memory offsets.
type Transcript struct {
	dir   string
	jsonl *os.File // append + positioned reads
	idx   *os.File // append + positioned reads
	hooks *Hooks

	mu       sync.Mutex
	jsize    int64  // current jsonl byte size
	n        int    // indexed record count
	lastSeq  uint64 // seq of the last indexed/appended record
	poisoned error  // set on canonical failure; instance is dead
}

// OpenTranscript opens the transcript pair inside a canonical session
// dir. transcript.jsonl must exist (canonical layout); transcript.idx may
// be missing — it is derived and rebuilt on demand. The open path is
// ~O(1) in transcript size: it only checks index alignment and the last
// indexed record, and rebuilds on any inconsistency.
func OpenTranscript(dir string) (*Transcript, error) {
	jp := filepath.Join(dir, transcriptFile)
	ip := filepath.Join(dir, indexFile)
	if fi, err := os.Lstat(jp); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("transcript %s missing or not a regular file", jp)
	}
	jf, err := os.OpenFile(jp, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", jp, err)
	}
	idx, err := os.OpenFile(ip, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		jf.Close()
		return nil, fmt.Errorf("open %s: %w", ip, err)
	}
	t := &Transcript{dir: dir, jsonl: jf, idx: idx, hooks: defaultHooks()}
	if err := t.openCheck(); err != nil {
		jf.Close()
		idx.Close()
		return nil, fmt.Errorf("open transcript in %s: %w", dir, err)
	}
	return t, nil
}

// openCheck performs the cheap healthy-open validation and rebuilds the
// derived index when it is missing or inconsistent.
func (t *Transcript) openCheck() error {
	jfi, err := t.jsonl.Stat()
	if err != nil {
		return err
	}
	ifi, err := t.idx.Stat()
	if err != nil {
		return err
	}
	t.jsize = jfi.Size()
	isize := ifi.Size()

	healthy := func() bool {
		if isize%8 != 0 {
			return false
		}
		n := int(isize / 8)
		if n == 0 {
			return t.jsize == 0 // empty pair is healthy
		}
		last, err := t.readOffset(n - 1)
		if err != nil || last >= t.jsize {
			return false
		}
		// The final indexed record must reach the valid EOF exactly: read
		// forward from its offset and require exactly one complete record
		// ending at jsize. Trailing bytes mean an unindexed tail or torn
		// append — both need a rebuild.
		recs, _, err := t.scanFrom(last)
		if err != nil || len(recs) != 1 {
			return false
		}
		t.lastSeq = recs[0].Seq
		t.n = n
		return true
	}
	if !healthy() {
		if err := t.rebuildLocked(); err != nil {
			return err
		}
	}
	return nil
}

// readOffset reads index entry i (8-byte big-endian record offset).
func (t *Transcript) readOffset(i int) (int64, error) {
	var b [8]byte
	if _, err := t.idx.ReadAt(b[:], int64(i)*8); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b[:])), nil
}

// scanFrom decodes complete JSONL records starting at byte offset off.
// It returns the decoded records and the byte offset of the first
// incomplete tail line (== jsize when the file ends cleanly).
func (t *Transcript) scanFrom(off int64) ([]Record, int64, error) {
	sr := io.NewSectionReader(t.jsonl, off, t.jsize-off)
	br := bufio.NewReaderSize(sr, 64*1024)
	var recs []Record
	pos := off
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if err == io.EOF {
				// Partial final line (crash residue), not a record.
				return recs, pos, nil
			}
			var r Record
			if jerr := json.Unmarshal(line, &r); jerr != nil {
				return nil, pos, fmt.Errorf("malformed record at offset %d: %w", pos, jerr)
			}
			if verr := r.validate(); verr != nil {
				return nil, pos, fmt.Errorf("invalid record at offset %d: %w", pos, verr)
			}
			recs = append(recs, r)
			pos += int64(len(line))
		}
		if err == io.EOF {
			return recs, pos, nil
		}
		if err != nil {
			return nil, pos, err
		}
	}
}

// rebuildLocked rescans the canonical transcript once: validates every
// complete record (strictly increasing seq), truncates a torn final
// line, and atomically replaces the derived index. Called under mu (or
// during single-threaded Open).
func (t *Transcript) rebuildLocked() error {
	recs, tail, err := t.scanFrom(0)
	if err != nil {
		return fmt.Errorf("rebuild transcript index: %w", err)
	}
	var last uint64
	for _, r := range recs {
		if r.Seq <= last {
			return fmt.Errorf("rebuild transcript index: non-monotonic seq %d after %d", r.Seq, last)
		}
		last = r.Seq
	}
	if tail < t.jsize {
		if err := t.jsonl.Truncate(tail); err != nil {
			return fmt.Errorf("rebuild transcript index: truncate tail: %w", err)
		}
		if err := t.jsonl.Sync(); err != nil {
			return fmt.Errorf("rebuild transcript index: sync transcript: %w", err)
		}
	}
	// Write the new index beside the old one, then rename atomically.
	tmp, err := os.CreateTemp(t.dir, tmpPrefix+"idx-*")
	if err != nil {
		return fmt.Errorf("rebuild transcript index: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Chmod(0o600)
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("rebuild transcript index: %w", err)
	}
	buf := make([]byte, 8)
	off := int64(0)
	sr := io.NewSectionReader(t.jsonl, 0, tail)
	br := bufio.NewReaderSize(sr, 64*1024)
	for i := 0; i < len(recs); i++ {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return fail(err)
		}
		binary.BigEndian.PutUint64(buf, uint64(off))
		if _, err := t.hooks.WriteFile(tmp, buf); err != nil {
			return fail(fmt.Errorf("write: %w", err))
		}
		off += int64(len(line))
	}
	if err := t.hooks.SyncFile(tmp); err != nil {
		return fail(fmt.Errorf("sync: %w", err))
	}
	tmp.Close()
	// COMMIT POINT for the derived index: rename installs the new file.
	// After it the old handle is unlinked — the live Transcript must swap
	// to the new path no matter what follows, or be poisoned.
	if err := t.hooks.Rename(tmpPath, filepath.Join(t.dir, indexFile)); err != nil {
		return fail(fmt.Errorf("rename: %w", err))
	}
	// Swap the index handle to the rebuilt file FIRST — a failed dir sync
	// or reopen must never leave the transcript on the obsolete unlinked
	// handle.
	newIdx, err := os.OpenFile(filepath.Join(t.dir, indexFile), os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return &UncertainError{
			Op:  "rebuild transcript index",
			Err: fmt.Errorf("index committed but reopen failed: %w", err),
		}
	}
	_ = t.idx.Close()
	t.idx = newIdx
	if err := t.hooks.SyncDir(t.dir); err != nil {
		// Index is installed and live; only durability of the rename is
		// uncertain. The transcript stays consistent in-memory.
		t.jsize = tail
		t.n = len(recs)
		t.lastSeq = last
		return &UncertainError{
			Op:  "rebuild transcript index",
			Err: fmt.Errorf("index committed but dir sync failed: %w", err),
		}
	}
	t.jsize = tail
	t.n = len(recs)
	t.lastSeq = last
	return nil
}

// Append durably records one entry: JSONL record first (synced), then the
// index offset (synced). seq must strictly increase — the index is never
// allowed to commit ahead of the canonical transcript.
//
// COMMIT POINT: a successful JSONL sync makes the record canonical. If the
// index write/sync then fails, the derived index is rebuilt immediately;
// when repair succeeds the append completes consistently, when it cannot
// the transcript is poisoned rather than continuing from stale offsets.
func (t *Transcript) Append(r Record) error {
	if err := r.validate(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.poisoned != nil {
		return t.poisoned
	}
	if r.Seq <= t.lastSeq {
		return fmt.Errorf("seq %d not after last durable seq %d", r.Seq, t.lastSeq)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	off := t.jsize
	if _, err := t.hooks.WriteFile(t.jsonl, append(line, '\n')); err != nil {
		// Bytes may be partially in the file — the transcript can no
		// longer trust its own offsets.
		t.poisoned = fmt.Errorf("append record seq %d: %w", r.Seq, err)
		return t.poisoned
	}
	if err := t.hooks.SyncFile(t.jsonl); err != nil {
		// The record may or may not be durable — ambiguous. Poison.
		t.poisoned = &UncertainError{
			Op:  "append record " + fmt.Sprint(r.Seq),
			Err: fmt.Errorf("transcript sync failed: %w", err),
		}
		return t.poisoned
	}
	// Canonical record committed. Index write is derived bookkeeping.
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(off))
	var ierr error
	if _, ierr = t.hooks.WriteFile(t.idx, b[:]); ierr == nil {
		ierr = t.hooks.SyncFile(t.idx)
	}
	if ierr != nil {
		// The durable record exists but the index is behind — repair
		// the derived index now so subsequent appends stay consistent.
		// The record is already in the canonical file, so include it in
		// the rebuild's scan range first.
		t.jsize = off + int64(len(line)+1)
		if rerr := t.rebuildLocked(); rerr != nil {
			t.poisoned = &UncertainError{
				Op:  "append record " + fmt.Sprint(r.Seq),
				Err: fmt.Errorf("index write failed (%v) and repair failed: %w", ierr, rerr),
			}
			return t.poisoned
		}
		return nil // repaired: durable record + consistent derived index
	}
	t.jsize = off + int64(len(line)+1)
	t.n++
	t.lastSeq = r.Seq
	return nil
}

// Tail returns the latest up-to-limit durable records plus lastSeq. It is
// TailBounded with no byte budget.
func (t *Transcript) Tail(limit int) ([]Record, uint64, error) {
	recs, through, _, err := t.TailBounded(limit, 0)
	return recs, through, err
}

// TailBounded returns the latest durable records that fit BOTH a record
// count bound (limit) and a source-byte bound (maxBytes; 0 = unbounded),
// plus lastSeq and whether older records exist before the returned
// window. Selection walks the derived index BACKWARD from the last
// record — record sizes come from consecutive index offsets — so it is
// O(returned) index reads and never scans the lifetime transcript prefix.
//
// If the newest record alone exceeds maxBytes, ErrRecordTooLarge is
// returned: an explicit bounded failure, never an unbounded allocation.
func (t *Transcript) TailBounded(limit int, maxBytes int64) ([]Record, uint64, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.poisoned != nil {
		return nil, 0, false, t.poisoned
	}
	if limit <= 0 || t.n == 0 {
		return nil, t.lastSeq, false, nil
	}
	// Backward size walk: sizes[i] = offsets[i+1]-offsets[i], and the
	// final record runs to EOF.
	var sizes int64
	start := t.n
	for i := t.n - 1; i >= 0 && t.n-i <= limit; i-- {
		off, err := t.readOffset(i)
		if err != nil {
			return nil, 0, false, fmt.Errorf("tail: index read: %w", err)
		}
		end := t.jsize
		if i+1 < t.n {
			if end, err = t.readOffset(i + 1); err != nil {
				return nil, 0, false, fmt.Errorf("tail: index read: %w", err)
			}
		}
		size := end - off
		if size <= 0 {
			return nil, 0, false, fmt.Errorf("tail: non-positive record size %d at index %d", size, i)
		}
		if maxBytes > 0 && sizes+size > maxBytes {
			if i == t.n-1 {
				return nil, 0, false, fmt.Errorf("tail: %w (%d bytes > %d)", ErrRecordTooLarge, size, maxBytes)
			}
			break
		}
		sizes += size
		start = i
	}
	if start == t.n {
		return nil, t.lastSeq, false, nil
	}
	off, err := t.readOffset(start)
	if err != nil {
		return nil, 0, false, fmt.Errorf("tail: index read: %w", err)
	}
	sr := io.NewSectionReader(t.jsonl, off, t.jsize-off)
	br := bufio.NewReaderSize(sr, 64*1024)
	recs := make([]Record, 0, t.n-start)
	pos := off
	for i := start; i < t.n; i++ {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, 0, false, fmt.Errorf("tail: read record at offset %d: %w", pos, err)
		}
		var r Record
		if jerr := json.Unmarshal(line, &r); jerr != nil {
			return nil, 0, false, fmt.Errorf("tail: malformed record at offset %d: %w", pos, jerr)
		}
		recs = append(recs, r)
		pos += int64(len(line))
	}
	return recs, t.lastSeq, start > 0, nil
}

// Len returns the number of durable records.
func (t *Transcript) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// LastSeq returns the highest durable seq (0 on an empty transcript).
func (t *Transcript) LastSeq() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastSeq
}

// Close releases the transcript files.
func (t *Transcript) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	jerr := t.jsonl.Close()
	ierr := t.idx.Close()
	if jerr != nil {
		return jerr
	}
	return ierr
}
