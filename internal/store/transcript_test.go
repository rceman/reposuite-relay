package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	return Record{
		Version: RecordVersion,
		Seq:     seq,
		Type:    typ,
		At:      time.Now(),
		Payload: json.RawMessage(`{"n":` + fmt.Sprint(seq) + `}`),
	}
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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
