package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
		Version: RecordVersion,
		Seq:     4,
		Type:    "message.agent.completed",
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
