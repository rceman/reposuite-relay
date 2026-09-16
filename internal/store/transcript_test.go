package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkTranscriptDir creates a minimal canonical-looking session dir with an
// empty transcript pair (what Sessions.Create produces).
func mkTranscriptDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript.idx"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openTr(t *testing.T, dir string) *Transcript {
	t.Helper()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func rec(seq uint64, typ string) Record {
	return Record{Version: RecordVersion, Seq: seq, Type: typ,
		At: time.Now(), Payload: json.RawMessage(`{"n":` + fmt.Sprint(seq) + `}`)}
}

func appendN(t *testing.T, tr *Transcript, from, to uint64) {
	t.Helper()
	for s := from; s <= to; s++ {
		if err := tr.Append(rec(s, "message.user")); err != nil {
			t.Fatalf("append seq %d: %v", s, err)
		}
	}
}

func TestEmptyTail(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	recs, through, err := tr.Tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 || through != 0 {
		t.Fatalf("empty tail: %d recs through=%d", len(recs), through)
	}
}

func TestAppendAndTail(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 10)

	recs, through, err := tr.Tail(4)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || recs[0].Seq != 7 || recs[3].Seq != 10 || through != 10 {
		t.Fatalf("tail(4): %v through=%d", seqs(recs), through)
	}
	recs, through, err = tr.Tail(50) // larger than total
	if err != nil || len(recs) != 10 || through != 10 {
		t.Fatalf("tail(50): %d recs %v", len(recs), err)
	}
	recs, through, err = tr.Tail(0)
	if err != nil || len(recs) != 0 || through != 10 {
		t.Fatalf("tail(0): %d recs through=%d %v", len(recs), through, err)
	}
	recs, through, err = tr.Tail(-3)
	if err != nil || len(recs) != 0 || through != 10 {
		t.Fatalf("tail(-3): %d recs through=%d %v", len(recs), through, err)
	}
}

func seqs(recs []Record) []uint64 {
	out := make([]uint64, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}

func TestSeqRules(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 3)
	// Non-contiguous growth is legal (transient events may consume seqs).
	if err := tr.Append(rec(9, "x")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(rec(9, "dup")); err == nil {
		t.Fatal("duplicate seq must fail")
	}
	if err := tr.Append(rec(5, "lower")); err == nil {
		t.Fatal("lower seq must fail")
	}
	if err := tr.Append(rec(0, "zero")); err == nil {
		t.Fatal("seq 0 must fail")
	}
	bad := rec(10, "")
	if err := tr.Append(bad); err == nil {
		t.Fatal("empty type must fail")
	}
	bad = rec(10, "t")
	bad.Version = 9
	if err := tr.Append(bad); err == nil {
		t.Fatal("bad version must fail")
	}
	bad = rec(10, "t")
	bad.At = time.Time{}
	if err := tr.Append(bad); err == nil {
		t.Fatal("missing timestamp must fail")
	}
	// Serialized persistence: reopen, tail must be consistent.
	tr.Close()
	tr = openTr(t, dir)
	recs, through, err := tr.Tail(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[2].Seq != 9 || through != 9 {
		t.Fatalf("after reopen: %v through=%d", seqs(recs), through)
	}
}

func TestMissingIndexRebuild(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	appendN(t, tr, 1, 5)
	tr.Close()
	// Delete the derived index — next open must rebuild it.
	if err := os.Remove(filepath.Join(dir, "transcript.idx")); err != nil {
		t.Fatal(err)
	}
	tr = openTr(t, dir)
	defer tr.Close()
	if tr.Len() != 5 {
		t.Fatalf("rebuilt len=%d", tr.Len())
	}
	recs, through, err := tr.Tail(2)
	if err != nil || len(recs) != 2 || recs[0].Seq != 4 || through != 5 {
		t.Fatalf("after rebuild: %v through=%d %v", seqs(recs), through, err)
	}
}

func TestTruncatedIndexRebuild(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	appendN(t, tr, 1, 4)
	tr.Close()
	// Corrupt the index: not a multiple of 8.
	idx := filepath.Join(dir, "transcript.idx")
	if err := os.WriteFile(idx, []byte{0, 0, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	tr = openTr(t, dir)
	defer tr.Close()
	if tr.Len() != 4 {
		t.Fatalf("len=%d after idx rebuild", tr.Len())
	}
}

func TestOutOfRangeIndexRebuild(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	appendN(t, tr, 1, 3)
	tr.Close()
	// Index pointing beyond transcript EOF.
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], 1<<40)
	if err := os.WriteFile(filepath.Join(dir, "transcript.idx"), b[:], 0o600); err != nil {
		t.Fatal(err)
	}
	tr = openTr(t, dir)
	defer tr.Close()
	recs, through, err := tr.Tail(1)
	if err != nil || len(recs) != 1 || recs[0].Seq != 3 || through != 3 {
		t.Fatalf("rebuild from out-of-range idx: %v %v", seqs(recs), err)
	}
}

func TestUnindexedTailRepair(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	appendN(t, tr, 1, 3)
	// Simulate a crash between jsonl sync and index append: append a
	// record line directly to the transcript without an index entry.
	line, _ := json.Marshal(rec(4, "message.user"))
	f, err := os.OpenFile(filepath.Join(dir, "transcript.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(line, '\n'))
	f.Close()
	tr.Close()
	tr = openTr(t, dir)
	defer tr.Close()
	if tr.Len() != 4 {
		t.Fatalf("unindexed tail must be re-indexed: len=%d", tr.Len())
	}
	recs, through, err := tr.Tail(2)
	if err != nil || recs[0].Seq != 3 || recs[1].Seq != 4 || through != 4 {
		t.Fatalf("repair: %v through=%d %v", seqs(recs), through, err)
	}
}

func TestPartialFinalRecordRecovered(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	appendN(t, tr, 1, 3)
	tr.Close()
	// Torn append: half-written JSON line at EOF.
	f, _ := os.OpenFile(filepath.Join(dir, "transcript.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	f.Write([]byte(`{"version":1,"seq":4,"type":"x","at":"2026`))
	f.Close()
	tr = openTr(t, dir)
	defer tr.Close()
	recs, through, err := tr.Tail(10)
	if err != nil || len(recs) != 3 || through != 3 {
		t.Fatalf("partial tail must be truncated: %v %v", seqs(recs), err)
	}
	// Appends continue cleanly after recovery.
	if err := tr.Append(rec(4, "next")); err != nil {
		t.Fatal(err)
	}
	recs, through, _ = tr.Tail(1)
	if recs[0].Seq != 4 || through != 4 {
		t.Fatalf("append after repair: %v", seqs(recs))
	}
}

func TestMalformedInteriorFails(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	appendN(t, tr, 1, 2)
	tr.Close()
	// Inject a malformed COMPLETE line in the middle + a valid record.
	jp := filepath.Join(dir, "transcript.jsonl")
	f, _ := os.OpenFile(jp, os.O_WRONLY|os.O_APPEND, 0o600)
	f.Write([]byte("{garbage}\n"))
	line, _ := json.Marshal(rec(3, "x"))
	f.Write(append(line, '\n'))
	f.Close()
	if _, err := OpenTranscript(dir); err == nil {
		t.Fatal("malformed interior record must fail closed")
	}
}

func TestNonMonotonicStoredSeqFails(t *testing.T) {
	dir := mkTranscriptDir(t)
	jp := filepath.Join(dir, "transcript.jsonl")
	f, _ := os.OpenFile(jp, os.O_WRONLY|os.O_APPEND, 0o600)
	for _, s := range []uint64{1, 2, 2, 3} {
		line, _ := json.Marshal(rec(s, "x"))
		f.Write(append(line, '\n'))
	}
	f.Close()
	if _, err := OpenTranscript(dir); err == nil {
		t.Fatal("non-monotonic durable seq must fail rebuild")
	}
}

// TestIndexIsDerived: after appends, deleting the index and reopening
// yields identical tail results — the index is never the source of truth.
func TestIndexIsDerived(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	appendN(t, tr, 1, 20)
	tr.Close()
	before, _, _ := func() ([]Record, uint64, error) {
		tr := openTr(t, dir)
		defer tr.Close()
		return tr.Tail(7)
	}()
	os.Remove(filepath.Join(dir, "transcript.idx"))
	tr = openTr(t, dir)
	defer tr.Close()
	after, through, err := tr.Tail(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 7 || len(before) != 7 || through != 20 {
		t.Fatalf("tail mismatch: %d/%d through=%d", len(after), len(before), through)
	}
	for i := range after {
		if after[i].Seq != before[i].Seq {
			t.Fatalf("record %d differs", i)
		}
	}
}

func TestTranscriptPermissions(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 2)
	for _, f := range []string{"transcript.jsonl", "transcript.idx"} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode %o", f, got)
		}
	}
}

// TestTailDoesNotScanPrefix: append a large-ish transcript, then verify
// Tail returns correct records through the index (structural proof of
// bounded tail reads — no wall-clock assertions).
func TestTailBoundedRead(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 5000)
	recs, through, err := tr.Tail(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].Seq != 4998 || recs[2].Seq != 5000 || through != 5000 {
		t.Fatalf("bounded tail: %v through=%d", seqs(recs), through)
	}
	// Index must have one 8-byte entry per record — and jsonl must be
	// bigger than the index.
	fi, _ := os.Stat(filepath.Join(dir, "transcript.idx"))
	if fi.Size() != 5000*8 {
		t.Fatalf("idx size %d want %d", fi.Size(), 5000*8)
	}
}

// TestTailBoundedBudget: the byte budget bounds the page while selection
// stays a backward index walk; hasMoreBefore reports truncation.
func TestTailBoundedBudget(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 20)

	// Source size of one record.
	recs, _, _, err := tr.TailBounded(1, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("one-record read: %v %v", recs, err)
	}
	one := int64(len(mustJSON(t, recs[0])) + 1)

	// Count bound only.
	recs, through, hasMore, err := tr.TailBounded(5, 0)
	if err != nil || len(recs) != 5 || !hasMore || through != 20 {
		t.Fatalf("count bound: %d through=%d hasMore=%v err=%v", len(recs), through, hasMore, err)
	}
	if recs[4].Seq != 20 {
		t.Fatalf("tail must be the newest records: %v", seqs(recs))
	}

	// Byte budget: three records fit exactly; the fourth does not.
	recs, _, hasMore, err = tr.TailBounded(20, 3*one)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || !hasMore || recs[0].Seq != 18 {
		t.Fatalf("byte bound: %v hasMore=%v", seqs(recs), hasMore)
	}

	// Budget for everything: no more-before.
	recs, _, hasMore, err = tr.TailBounded(20, 100*one)
	if err != nil || len(recs) != 20 || hasMore {
		t.Fatalf("full read: %d hasMore=%v err=%v", len(recs), hasMore, err)
	}
}

// TestTailBoundedOversizedRecord: a single canonical record that exceeds
// the page budget is an explicit bounded error — never an unbounded
// allocation, and the transcript stays intact.
func TestTailBoundedOversizedRecord(t *testing.T) {
	dir := mkTranscriptDir(t)
	tr := openTr(t, dir)
	defer tr.Close()
	appendN(t, tr, 1, 3)
	big := Record{
		Version: RecordVersion, Seq: 4, Type: "message.agent.completed",
		At:      time.Now(),
		Payload: json.RawMessage(`{"blob":"` + strings.Repeat("x", 2<<20) + `"}`),
	}
	if err := tr.Append(big); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := tr.TailBounded(10, 1<<20); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("want ErrRecordTooLarge, got %v", err)
	}
	// The transcript is intact: the record is still readable with a
	// sufficient budget.
	recs, through, _, err := tr.TailBounded(10, 8<<20)
	if err != nil || len(recs) != 4 || through != 4 {
		t.Fatalf("recovery read: %d through=%d err=%v", len(recs), through, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
