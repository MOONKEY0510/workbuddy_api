package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDegradeSelfUnblock 连败降权到期后自动回池（"自己解封"）：不需要任何显式复位，
// healthy/选号只看截止时间；降权期内不可被 normal 选号选中。
func TestDegradeSelfUnblock(t *testing.T) {
	p := New("")
	p.SetDegrade(2, 40*time.Millisecond, time.Hour) // 2 连败、40ms 降权（测试用短时长）
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteFailures("u1") // 1 次：未达阈，不降权
	if uids := p.AvailableUIDs(); len(uids) != 1 {
		t.Fatalf("未达阈不应出池: %v", uids)
	}
	p.NoteFailures("u1") // 达阈 → 降权
	st, _ := p.Status("u1")
	if st.DegradeUntil.IsZero() || !time.Now().Before(st.DegradeUntil) {
		t.Fatalf("达阈应触发降权: %+v", st.DegradeUntil)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("降权期内不应参与 normal 选号: %v", uids)
	}
	time.Sleep(60 * time.Millisecond)
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "u1" {
		t.Fatalf("降权到期后应自动回池（自己解封）: %v", uids)
	}
}

// TestDegradeNotExtendedWithinWindow 降权期内再次连败达阈：不延长、不翻倍（防用户重试
// 把降权越堆越厚）——计数清零，到期放行后需重新连败满阈值才再降。
func TestDegradeNotExtendedWithinWindow(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1")
	first, _ := p.Status("u1")
	p.NoteFailures("u1")
	p.NoteFailures("u1") // 降权期内再次达阈
	second, _ := p.Status("u1")
	if !first.DegradeUntil.Equal(second.DegradeUntil) {
		t.Fatalf("降权期内不应延长: %v -> %v", first.DegradeUntil, second.DegradeUntil)
	}
}

// TestReviveClearsDegrade 面板「解冻」按钮对降权号同样可见（frozen = disabled || cool > 0，
// 降权计入 cool）：Revive 必须一并清掉连败计数与降权截止，否则点完按钮账号仍被挡在池外，
// 表现为「连败降权解不了封」。
func TestReviveClearsDegrade(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1")

	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true（账号存在）")
	}
	st, _ := p.Status("u1")
	if !st.DegradeUntil.IsZero() || st.ConsecutiveFails != 0 {
		t.Fatalf("Revive 后降权状态未清: degrade_until=%v consecutive=%d", st.DegradeUntil, st.ConsecutiveFails)
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "u1" {
		t.Fatalf("Revive 后应立即可选: %v", uids)
	}
}
