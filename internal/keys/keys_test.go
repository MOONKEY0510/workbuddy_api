package keys

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateVerifyDisableRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s := New(path)

	k, err := s.Create("Claude Code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Value, "sk-") || len(k.Value) < 20 {
		t.Errorf("value=%q looks wrong", k.Value)
	}
	if !k.Enabled {
		t.Error("new key must be enabled")
	}
	if k.ID == "" {
		t.Error("new key must carry an id")
	}

	// 命中：返回同一条记录并刷新 LastUsed。
	got, ok := s.Verify(k.Value)
	if !ok || got.ID != k.ID {
		t.Fatalf("verify failed: ok=%v id=%q want %q", ok, got.ID, k.ID)
	}
	if got.LastUsed.IsZero() {
		t.Error("LastUsed must be recorded on successful verify")
	}

	// 未命中：错误 / 空 token 一律拒绝。
	if _, ok := s.Verify("sk-nope"); ok {
		t.Error("unknown token must not verify")
	}
	if _, ok := s.Verify(""); ok {
		t.Error("empty token must not verify")
	}
	if _, ok := s.Verify("   "); ok {
		t.Error("blank token must not verify")
	}

	// 停用后立即拒绝（记录保留，便于复查）。
	if _, ok := s.Update(k.ID, "Claude Code", false); !ok {
		t.Fatal("disable failed")
	}
	if _, ok := s.Verify(k.Value); ok {
		t.Error("disabled key must not verify")
	}

	// 重新启用 + 改名。
	if _, ok := s.Update(k.ID, "改名了", true); !ok {
		t.Fatal("re-enable failed")
	}
	if got, ok := s.Verify(k.Value); !ok || got.Name != "改名了" {
		t.Errorf("re-enable verify: ok=%v name=%q", ok, got.Name)
	}

	// 删除：立即失效，重复删除报 false。
	if !s.Remove(k.ID) {
		t.Fatal("remove failed")
	}
	if _, ok := s.Verify(k.Value); ok {
		t.Error("removed key must not verify")
	}
	if s.Remove(k.ID) {
		t.Error("remove of missing id must report false")
	}
	if s.Count() != 0 {
		t.Errorf("count=%d want 0", s.Count())
	}
}

func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s := New(path)
	k1, err := s.Create("A")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := s.Create("B")
	if err != nil {
		t.Fatal(err)
	}

	s2 := New(path)
	if s2.Count() != 2 {
		t.Fatalf("count=%d want 2", s2.Count())
	}
	list := s2.List()
	if list[0].ID != k2.ID || list[1].ID != k1.ID {
		t.Errorf("order wrong: got %q,%q want %q,%q", list[0].ID, list[1].ID, k2.ID, k1.ID)
	}
	if _, ok := s2.Verify(k1.Value); !ok {
		t.Error("persisted key must verify after reload")
	}

	// 落盘形态：版本化对象（供将来加字段）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Version int `json:"version"`
		Keys    []struct {
			ID string `json:"id"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("persisted file not json: %v", err)
	}
	if f.Version != version {
		t.Errorf("version=%d want %d", f.Version, version)
	}
}

func TestCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(path) // 不 panic、不阻塞启动
	if s.Count() != 0 {
		t.Errorf("count=%d want 0 on corrupt file", s.Count())
	}
	if _, err := s.Create("after-corrupt"); err != nil {
		t.Fatal(err)
	}
	if s2 := New(path); s2.Count() != 1 {
		t.Errorf("after overwrite, count=%d want 1", s2.Count())
	}
}

func TestNameNormalizeAndCap(t *testing.T) {
	s := New("")
	k, err := s.Create("   ")
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != "未命名" {
		t.Errorf("blank name → %q want 未命名", k.Name)
	}
	k2, err := s.Create(strings.Repeat("称", nameLimit+10))
	if err != nil {
		t.Fatal(err)
	}
	if r := []rune(k2.Name); len(r) != nameLimit {
		t.Errorf("name runes=%d want %d", len(r), nameLimit)
	}
}

func TestMaxKeys(t *testing.T) {
	s := New("")
	for i := 0; i < maxKeys; i++ {
		if _, err := s.Create("k"); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := s.Create("overflow"); err == nil {
		t.Error("create beyond maxKeys must fail")
	}
}

// LastUsed 落盘防抖：窗口内只改内存（不把磁盘 IO 绑到请求数），窗口过后落盘。
func TestLastUsedFlushDebounced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s := New(path)
	k, err := s.Create("A")
	if err != nil {
		t.Fatal(err)
	}
	// 落盘的 LastUsed（零值 = 从未使用）。
	persistedLastUsed := func() time.Time {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Keys []struct {
				LastUsed time.Time `json:"last_used"`
			} `json:"keys"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("parse store: %v", err)
		}
		if len(f.Keys) == 0 {
			return time.Time{}
		}
		return f.Keys[0].LastUsed
	}

	if !persistedLastUsed().IsZero() {
		t.Error("fresh key must have zero last_used on disk")
	}
	if _, ok := s.Verify(k.Value); !ok {
		t.Fatal("verify failed")
	}
	if !persistedLastUsed().IsZero() {
		t.Error("last_used must not be flushed within the debounce window")
	}

	base := time.Now()
	s.now = func() time.Time { return base.Add(lastUsedFlushWindow + time.Second) }
	if _, ok := s.Verify(k.Value); !ok {
		t.Fatal("verify failed (after window)")
	}
	if persistedLastUsed().IsZero() {
		t.Error("last_used must be flushed after the debounce window")
	}
}

// Flush 在无变更时空操作、有变更时落盘（进程退出路径）。
func TestFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s := New(path)
	if err := s.Flush(); err != nil {
		t.Fatalf("flush on clean store: %v", err)
	}
	k, err := s.Create("A")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.dirty = true // 模拟「有变更未落盘」
	s.mu.Unlock()
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, ok := New(path).Verify(k.Value); !ok {
		t.Error("flushed key must be readable after reload")
	}
}
