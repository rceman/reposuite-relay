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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RecordVersion is the durable record envelope version.
const RecordVersion = 1

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
type Transcript struct {
	dir   string
	jsonl *os.File // append + positioned reads
	idx   *os.File // append + positioned reads

	mu      sync.Mutex
	jsize   int64  // current jsonl byte size
	n       int    // indexed record count
	lastSeq uint64 // seq of the last indexed/appended record
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
	t := &Transcript{dir: dir, jsonl: jf, idx: idx}
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
		if err := t.rebuild(); err != nil {
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

// rebuild rescans the canonical transcript once: validates every complete
// record (strictly increasing seq), truncates a torn final line, and
// atomically replaces the derived index.
func (t *Transcript) rebuild() error {
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
	buf := make([]byte, 8)
	off := int64(0)
	sr := io.NewSectionReader(t.jsonl, 0, tail)
	br := bufio.NewReaderSize(sr, 64*1024)
	for i := 0; i < len(recs); i++ {
		line, err := br.ReadBytes('\n')
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("rebuild transcript index: rescan: %w", err)
		}
		binary.BigEndian.PutUint64(buf, uint64(off))
		if _, err := tmp.Write(buf); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("rebuild transcript index: write: %w", err)
		}
		off += int64(len(line))
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("rebuild transcript index: sync: %w", err)
	}
	tmp.Close()
	if err := os.Rename(tmpPath, filepath.Join(t.dir, indexFile)); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rebuild transcript index: rename: %w", err)
	}
	if err := syncDir(t.dir); err != nil {
		return fmt.Errorf("rebuild transcript index: %w", err)
	}
	// Swap the index handle to the rebuilt file.
	_ = t.idx.Close()
	idx, err := os.OpenFile(filepath.Join(t.dir, indexFile), os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("rebuild transcript index: reopen: %w", err)
	}
	t.idx = idx
	t.jsize = tail
	t.n = len(recs)
	t.lastSeq = last
	return nil
}

// Append durably records one entry: JSONL record first (synced), then the
// index offset (synced). seq must strictly increase — the index is never
// allowed to commit ahead of the canonical transcript.
func (t *Transcript) Append(r Record) error {
	if err := r.validate(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.Seq <= t.lastSeq {
		return fmt.Errorf("seq %d not after last durable seq %d", r.Seq, t.lastSeq)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	off := t.jsize
	if _, err := t.jsonl.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append record seq %d: %w", r.Seq, err)
	}
	if err := t.jsonl.Sync(); err != nil {
		return fmt.Errorf("append record seq %d: sync: %w", r.Seq, err)
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(off))
	if _, err := t.idx.Write(b[:]); err != nil {
		return fmt.Errorf("append index seq %d: %w", r.Seq, err)
	}
	if err := t.idx.Sync(); err != nil {
		return fmt.Errorf("append index seq %d: sync: %w", r.Seq, err)
	}
	t.jsize = off + int64(len(line)+1)
	t.n++
	t.lastSeq = r.Seq
	return nil
}

// Tail returns the latest up-to-limit durable records plus throughSeq —
// the seq a reconnecting client subscribes after. It seeks directly into
// the tail through the index and never scans the lifetime prefix.
// limit <= 0 returns no records (throughSeq is still the last seq).
func (t *Transcript) Tail(limit int) ([]Record, uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if limit <= 0 || t.n == 0 {
		return nil, t.lastSeq, nil
	}
	start := t.n - limit
	if start < 0 {
		start = 0
	}
	off, err := t.readOffset(start)
	if err != nil {
		return nil, 0, fmt.Errorf("tail: index read: %w", err)
	}
	recs, _, err := t.scanFrom(off)
	if err != nil {
		return nil, 0, fmt.Errorf("tail: %w", err)
	}
	return recs, t.lastSeq, nil
}

// Len returns the number of durable records.
func (t *Transcript) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
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
