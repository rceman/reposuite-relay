package store

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

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
