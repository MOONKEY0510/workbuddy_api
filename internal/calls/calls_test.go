package calls

import (
	"strings"
	"testing"
)

// TestRingListNewestFirst 最新在前，且回绕后顺序不乱。
func TestRingListNewestFirst(t *testing.T) {
	r := NewRing(3)
	for i := 1; i <= 5; i++ {
		r.Add(Entry{Model: string(rune('a' + i - 1))})
	}
	got := r.List()
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0].Model != "e" || got[2].Model != "c" {
		t.Fatalf("顺序错误: %v %v %v", got[0].Model, got[1].Model, got[2].Model)
	}
	if got[0].Seq <= got[2].Seq {
		t.Errorf("seq 应为最新在前: %d <= %d", got[0].Seq, got[2].Seq)
	}
	if r.Cap() != 3 {
		t.Errorf("cap=%d", r.Cap())
	}
}

// TestAddTruncatesErr 超长错误原文入库前截断（上游可能回整页 HTML）。
func TestAddTruncatesErr(t *testing.T) {
	r := NewRing(1)
	long := strings.Repeat("x", maxErrLen+500)
	r.Add(Entry{Err: long, Kind: "waf_block", Status: 403})
	e := r.List()[0]
	if len(e.Err) != maxErrLen+len("…") {
		t.Errorf("Err 未截断到上限: len=%d", len(e.Err))
	}
	if !strings.HasSuffix(e.Err, "…") {
		t.Errorf("截断应带省略号: %q", e.Err[max(0, len(e.Err)-5):])
	}
	if e.Kind != "waf_block" || e.Status != 403 {
		t.Errorf("失败明细字段丢失: %+v", e)
	}
	// 短错误原样保留
	r.Add(Entry{Err: "boom"})
	if got := r.List()[0].Err; got != "boom" {
		t.Errorf("短错误不应改写: %q", got)
	}
}

// TestAddFillsTimeAndSeq 缺省时间自动补齐，Seq 单调递增（前端据此判新旧）。
func TestAddFillsTimeAndSeq(t *testing.T) {
	r := NewRing(2)
	r.Add(Entry{})
	r.Add(Entry{})
	list := r.List()
	if list[0].T == "" || list[1].T == "" {
		t.Error("时间未补齐")
	}
	if list[0].Seq == list[1].Seq {
		t.Error("Seq 必须唯一")
	}
}

// TestNilRingSafe nil 接收者安全（未装配 calls 时全流程不 panic）。
func TestNilRingSafe(t *testing.T) {
	var r *Ring
	r.Add(Entry{})
	if r.List() != nil {
		t.Error("nil ring List 应为 nil")
	}
	if r.Cap() != 0 {
		t.Error("nil ring Cap 应为 0")
	}
}
