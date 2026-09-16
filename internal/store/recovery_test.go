package store

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
