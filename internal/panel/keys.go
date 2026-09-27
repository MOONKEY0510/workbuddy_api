// keys.go 面板「Key 管理」接口：托管 API 密钥的列表 / 新建 / 更新 / 删除。
//
// 权限边界：本组接口与其余面板接口一样只认**主密钥**（config.json → api_key）。
// 托管密钥（p.cfg.Keys 里那些）只用于调用 /v1/*、/status，**不能**进入管理面——
// 否则「发给客户端的调用密钥」等价于「面板完全控制权」，与解耦初衷相悖。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/keys"
)

// keyView 面板展示用的密钥视图（时间统一 RFC3339，前端按本地时区渲染）。
type keyView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Value     string `json:"value"` // 明文：前端默认掩码显示，点「显示」展开、支持复制
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used,omitempty"`
}

func keyViewOf(k keys.Key) keyView {
	v := keyView{
		ID:        k.ID,
		Name:      k.Name,
		Value:     k.Value,
		Enabled:   k.Enabled,
		CreatedAt: k.CreatedAt.Format(time.RFC3339),
	}
	if !k.LastUsed.IsZero() {
		v.LastUsed = k.LastUsed.Format(time.RFC3339)
	}
	return v
}

func keyViews(list []keys.Key) []keyView {
	out := make([]keyView, 0, len(list))
	for _, k := range list {
		out = append(out, keyViewOf(k))
	}
	return out
}

// keysList GET /panel/api/keys：托管密钥列表 + 主密钥是否启用（供前端提示）。
func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":          keyViews(p.cfg.Keys.List()),
		"auth_required": p.apiKey() != "",
	})
}

// keysCreate POST /panel/api/keys，body 可带 {"name":"..."}：新建并返回含明文的记录。
func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body) // 缺 body / 非 JSON = 用默认名
	}
	k, err := p.cfg.Keys.Create(body.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 新建 API 密钥 name=%q id=%s", k.Name, k.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": keyViewOf(k)})
}

// keyUpdate POST /panel/api/keys/{id}/update，body {"name":"...","enabled":true}：
// 面板按整行状态提交（改名与启停一次写完）。
func (p *Panel) keyUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	id := r.PathValue("id")
	var body struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	k, ok := p.cfg.Keys.Update(id, body.Name, body.Enabled)
	if !ok {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	log.Printf("panel: 更新 API 密钥 id=%s name=%q enabled=%v", k.ID, k.Name, k.Enabled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": keyViewOf(k)})
}

// keyRemove POST /panel/api/keys/{id}/remove：删除（立即失效，不可恢复）。
func (p *Panel) keyRemove(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "key store not available")
		return
	}
	id := r.PathValue("id")
	if !p.cfg.Keys.Remove(id) {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	log.Printf("panel: 删除 API 密钥 id=%s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
