package store

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertAndListPending(t *testing.T) {
	s := openTemp(t)

	recs := []Record{
		{MsgID: 3, DialogID: 100, Size: 900, FileName: "big.mp4", MediaType: "document"},
		{MsgID: 1, DialogID: 100, Size: 100, FileName: "small.jpg", MediaType: "photo"},
		{MsgID: 2, DialogID: 100, Size: 500, FileName: "mid.mp4", MediaType: "document"},
	}
	added, err := s.UpsertManifest(recs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 {
		t.Errorf("added = %d, want 3", added)
	}

	// re-upsert is idempotent: no new rows
	added, err = s.UpsertManifest(recs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("second added = %d, want 0", added)
	}

	pending, err := s.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want 3", len(pending))
	}
	// smallest first
	if pending[0].MsgID != 1 || pending[1].MsgID != 2 || pending[2].MsgID != 3 {
		t.Errorf("order = %d,%d,%d, want 1,2,3", pending[0].MsgID, pending[1].MsgID, pending[2].MsgID)
	}
}

func TestMarkDoneRemovesFromPending(t *testing.T) {
	s := openTemp(t)
	_, _ = s.UpsertManifest([]Record{{MsgID: 1, DialogID: 1, Size: 10}})

	if err := s.MarkDone(1, 10, "/x/1_1_a.jpg"); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ListPending()
	if len(pending) != 0 {
		t.Errorf("pending = %d, want 0", len(pending))
	}
	counts, _ := s.Counts()
	if counts[StatusDone] != 1 {
		t.Errorf("done = %d, want 1", counts[StatusDone])
	}
}

func TestMarkAttemptFailsAfterMax(t *testing.T) {
	s := openTemp(t)
	_, _ = s.UpsertManifest([]Record{{MsgID: 1, DialogID: 1, Size: 10}})

	const max = 3
	// attempts 1 and 2 keep it pending
	for i := 1; i <= 2; i++ {
		if err := s.MarkAttempt(1, 5, "size mismatch", max); err != nil {
			t.Fatal(err)
		}
		pending, _ := s.ListPending()
		if len(pending) != 1 {
			t.Fatalf("after attempt %d: pending = %d, want 1", i, len(pending))
		}
	}
	// third attempt trips it to failed
	if err := s.MarkAttempt(1, 5, "size mismatch", max); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ListPending()
	if len(pending) != 0 {
		t.Errorf("pending = %d, want 0", len(pending))
	}
	failed, _ := s.ListFailed()
	if len(failed) != 1 {
		t.Errorf("failed = %d, want 1", len(failed))
	}

	// ResetFailed puts it back
	n, _ := s.ResetFailed()
	if n != 1 {
		t.Errorf("reset = %d, want 1", n)
	}
	pending, _ = s.ListPending()
	if len(pending) != 1 {
		t.Errorf("after reset pending = %d, want 1", len(pending))
	}
}

func TestMetaAndMaxMsgID(t *testing.T) {
	s := openTemp(t)
	if _, ok, _ := s.GetMeta("channel_id"); ok {
		t.Error("expected missing meta")
	}
	_ = s.SetMeta("channel_id", "42")
	v, ok, _ := s.GetMeta("channel_id")
	if !ok || v != "42" {
		t.Errorf("meta = %q,%v", v, ok)
	}

	if id, _ := s.MaxMsgID(); id != 0 {
		t.Errorf("MaxMsgID empty = %d, want 0", id)
	}
	_, _ = s.UpsertManifest([]Record{{MsgID: 7, DialogID: 1}, {MsgID: 99, DialogID: 1}})
	if id, _ := s.MaxMsgID(); id != 99 {
		t.Errorf("MaxMsgID = %d, want 99", id)
	}
}

func TestWatermarkMonotonic(t *testing.T) {
	s := openTemp(t)

	if wm, _ := s.LastMsgID(); wm != 0 {
		t.Errorf("initial watermark = %d, want 0", wm)
	}

	if err := s.AdvanceLastMsgID(2913); err != nil {
		t.Fatal(err)
	}
	if wm, _ := s.LastMsgID(); wm != 2913 {
		t.Errorf("watermark = %d, want 2913", wm)
	}

	// lower and zero ids never rewind it
	_ = s.AdvanceLastMsgID(2000)
	_ = s.AdvanceLastMsgID(0)
	if wm, _ := s.LastMsgID(); wm != 2913 {
		t.Errorf("watermark = %d, want 2913 (no rewind)", wm)
	}

	// a higher id advances it
	_ = s.AdvanceLastMsgID(2915)
	if wm, _ := s.LastMsgID(); wm != 2915 {
		t.Errorf("watermark = %d, want 2915", wm)
	}
}
