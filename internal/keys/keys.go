// Package keys 托管 API 密钥库：面板「Key 管理」创建 / 停用 / 删除的调用密钥。
//
// 与主密钥（config.json → api_key）的分工：
//   - 主密钥：面板登录 + 全部 API；修改要动配置文件；
//   - 托管密钥（本包）：只用于调用 /v1/*、/status，**不能**登录面板——面板端点是
//     账号与配置管理面，只认主密钥。于是「发给客户端 / 同事的调用凭证」与「管理面
//     凭证」彻底解耦：某一枚泄露或不再使用时撤销它即可，主密钥无需更换、其他客户端
//     不受影响。
//
// 存储：与 state.json 同目录的 api_keys.json（0600，tmp + rename 原子写）。
// 明文落盘的理由与 auths/ 一致（本机 / 私有部署），且面板需要「显示 / 复制」；
// 面板 UI 默认掩码显示，按需展开。
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// version 存储格式版本（前向兼容留白：将来加字段不必猜格式）。
	version = 1
	// maxKeys 密钥数量上限：个人网关场景足够，同时兜住文件无界增长。
	maxKeys = 50
	// nameLimit 备注名按 rune 计的长度上限。
	nameLimit = 40
	// lastUsedFlushWindow 「最近使用」落盘防抖窗口：API 流量下每个请求都会刷新
	// LastUsed，逐次落盘等于把磁盘 IO 绑到请求数上（而这是纯观测字段）。
	lastUsedFlushWindow = 30 * time.Second
)

// Key 单枚托管密钥。
type Key struct {
	ID        string    `json:"id"`         // 稳定标识（面板操作用）：随机 8 字节 hex
	Name      string    `json:"name"`       // 备注名（如 "Claude Code"）
	Value     string    `json:"value"`      // 密钥明文（sk-...）
	Enabled   bool      `json:"enabled"`    // 停用后校验直接拒绝（保留记录便于复查）
	CreatedAt time.Time `json:"created_at"` //
	// LastUsed 最近一次成功校验时刻（零值 = 从未使用）。注意 time.Time 是结构体，
	// omitempty 对它无效——落盘恒带该键，零值序列化为 0001-01-01T00:00:00Z；
	// 面板视图层（panel/keyView）会把它转成空串省略。
	LastUsed time.Time `json:"last_used"`
}

// file 落盘结构。
type file struct {
	Version int   `json:"version"`
	Keys    []Key `json:"keys"`
}

// Store 密钥库（并发安全）。
type Store struct {
	mu       sync.Mutex
	path     string
	keys     []Key
	dirty    bool
	lastSave time.Time
	now      func() time.Time
}

// New 打开（或初始化）密钥库。文件不存在 = 空库（首次使用无需预建）；
// 文件损坏 = 打 WARN 后按空库继续（不因一个观测文件挡启动），下次变更会覆盖它。
// path 为空 = 纯内存库（测试用，变更不落盘）。
func New(path string) *Store {
	s := &Store{path: path, keys: []Key{}, now: time.Now}
	s.load()
	return s
}

// Describe 返回供启动日志透出的摘要（不含任何密钥明文）。
func (s *Store) Describe() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) == 0 {
		return "0 枚（面板「Key 管理」可创建）"
	}
	on := 0
	for _, k := range s.keys {
		if k.Enabled {
			on++
		}
	}
	return fmt.Sprintf("%d 枚（启用 %d）", len(s.keys), on)
}

// Count 返回密钥总数（含停用项）。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// List 返回全部密钥的副本（创建顺序，最新在前）。副本随带明文：面板需要
// 「显示 / 复制」完整密钥。
func (s *Store) List() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, len(s.keys))
	copy(out, s.keys)
	return out
}

// Verify 校验一枚调用密钥：命中启用项则刷新 LastUsed 并返回该记录。
//
// 常量时间比较（两侧先 SHA-256 摘要再 ConstantTimeCompare）：长度差异被摘要吸收，
// 不因前缀匹配长度泄露信息；且遍历全部密钥**不提前返回**，避免用响应时间标出
// 「命中了第几枚」。
func (s *Store) Verify(token string) (Key, bool) {
	if strings.TrimSpace(token) == "" {
		return Key{}, false
	}
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	hit := -1
	for i := range s.keys {
		if !s.keys[i].Enabled {
			continue
		}
		want := sha256.Sum256([]byte(s.keys[i].Value))
		if subtle.ConstantTimeCompare(sum[:], want[:]) == 1 && hit < 0 {
			hit = i
		}
	}
	if hit < 0 {
		return Key{}, false
	}
	s.keys[hit].LastUsed = s.now()
	s.dirty = true
	if s.now().Sub(s.lastSave) >= lastUsedFlushWindow {
		s.persistLocked()
	}
	return s.keys[hit], true
}

// Create 新建一枚密钥并返回含明文的记录（面板展示 / 复制用）。
func (s *Store) Create(name string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) >= maxKeys {
		return Key{}, fmt.Errorf("密钥数量已达上限 %d，请先删除不再使用的密钥", maxKeys)
	}
	idRaw := make([]byte, 8)
	if _, err := rand.Read(idRaw); err != nil {
		return Key{}, fmt.Errorf("gen key id: %w", err)
	}
	valRaw := make([]byte, 24)
	if _, err := rand.Read(valRaw); err != nil {
		return Key{}, fmt.Errorf("gen key value: %w", err)
	}
	k := Key{
		ID:        hex.EncodeToString(idRaw),
		Name:      cleanName(name),
		Value:     "sk-" + base64.RawURLEncoding.EncodeToString(valRaw),
		Enabled:   true,
		CreatedAt: s.now(),
	}
	s.keys = append([]Key{k}, s.keys...) // 最新在前（与 List 展示顺序一致）
	s.persistLocked()
	return k, nil
}

// Update 更新备注名与启停状态（面板保存整行）；id 不存在返回 false。
func (s *Store) Update(id, name string, enabled bool) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != id {
			continue
		}
		s.keys[i].Name = cleanName(name)
		s.keys[i].Enabled = enabled
		s.persistLocked()
		return s.keys[i], true
	}
	return Key{}, false
}

// Remove 删除一枚密钥（立即失效）；id 不存在返回 false。
func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != id {
			continue
		}
		s.keys = append(s.keys[:i], s.keys[i+1:]...)
		s.persistLocked()
		return true
	}
	return false
}

// Flush 立即落盘（进程退出前调用；无变更时空操作）。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		log.Printf("ERR: [keys] 落盘 %s 失败: %v", s.path, err)
		return err
	}
	return nil
}

// load 读取落盘状态（调用方为 New，无并发）。
func (s *Store) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [keys] 读取 %s 失败（按空库继续）: %v", s.path, err)
		}
		return
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("WARN: [keys] 解析 %s 失败（按空库继续，下次变更会覆盖该文件）: %v", s.path, err)
		return
	}
	seen := make(map[string]bool, len(f.Keys))
	for _, k := range f.Keys {
		k.Name = cleanName(k.Name)
		// 防御：缺关键字段 / ID 重复的脏记录丢弃（避免同 ID 无法定位）。
		if k.ID == "" || strings.TrimSpace(k.Value) == "" || seen[k.ID] {
			continue
		}
		seen[k.ID] = true
		s.keys = append(s.keys, k)
	}
	s.lastSave = s.now()
}

// saveLocked 原子落盘（调用方持锁）。path 为空 = 纯内存，直接清脏标记。
func (s *Store) saveLocked() error {
	if s.path == "" {
		s.dirty = false
		return nil
	}
	raw, err := json.MarshalIndent(file{Version: version, Keys: s.keys}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir keys dir: %w", err)
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	s.lastSave = s.now()
	return nil
}

// persistLocked 落盘并记录失败（调用方持锁）。落盘失败不阻断内存变更——进程内
// 立即生效，但重启后会丢，故用 ERR 让运维可见；dirty 保持为真，Flush 会重试。
func (s *Store) persistLocked() {
	if err := s.saveLocked(); err != nil {
		log.Printf("ERR: [keys] 落盘 %s 失败（进程内已生效，重启会丢）: %v", s.path, err)
	}
}

// cleanName 归一化备注名：trim；空则给默认名；按 rune 截断到上限。
func cleanName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "未命名"
	}
	if r := []rune(name); len(r) > nameLimit {
		name = string(r[:nameLimit])
	}
	return name
}
