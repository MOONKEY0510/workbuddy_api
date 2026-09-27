package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/keys"
)

func newKeysPanel(t *testing.T) (*Panel, *keys.Store) {
	t.Helper()
	ks := keys.New(filepath.Join(t.TempDir(), "api_keys.json"))
	return New(Config{Version: "test", APIKey: "master", Keys: ks}), ks
}

// panelReq 以指定 token 发一次面板请求。
func panelReq(p *Panel, method, path, body, token string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	p.ServeHTTP(rec, req)
	return rec
}

// 托管密钥不能进入面板管理面（面板只认主密钥）——否则「发给客户端的调用凭证」
// 等价于「面板完全控制权」，与密钥管理解耦的初衷相悖。
func TestManagedKeyCannotAccessPanel(t *testing.T) {
	p, ks := newKeysPanel(t)
	k, err := ks.Create("client")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/panel/api/keys", "/panel/api/overview", "/panel/api/config", "/panel/api/logs"} {
		rec := panelReq(p, "GET", path, "", k.Value)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with managed key: code=%d want 401", path, rec.Code)
		}
	}
}

// 密钥管理接口全链路：列表 → 新建 → 停用 → 改名/启用 → 删除（均走主密钥）。
func TestKeyManagementRoundtrip(t *testing.T) {
	p, _ := newKeysPanel(t)
	const master = "master"

	rec := panelReq(p, "GET", "/panel/api/keys", "", master)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"keys":[]`) {
		t.Errorf("want empty list, got %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"auth_required":true`) {
		t.Errorf("want auth_required=true, got %s", rec.Body)
	}

	rec = panelReq(p, "POST", "/panel/api/keys", `{"name":"Claude Code"}`, master)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body)
	}
	var created struct {
		Key struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Value   string `json:"value"`
			Enabled bool   `json:"enabled"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create payload: %v (%s)", err, rec.Body)
	}
	if created.Key.ID == "" || created.Key.Value == "" || !created.Key.Enabled {
		t.Fatalf("unexpected create payload: %s", rec.Body)
	}
	if created.Key.Name != "Claude Code" {
		t.Errorf("name=%q want Claude Code", created.Key.Name)
	}

	// 列表可见（接口回明文供前端复制；掩码在前端做）。
	rec = panelReq(p, "GET", "/panel/api/keys", "", master)
	if !strings.Contains(rec.Body.String(), created.Key.Value) {
		t.Error("created key must be listed")
	}

	// 停用（面板按整行状态提交：名字原样带回）。
	rec = panelReq(p, "POST", "/panel/api/keys/"+created.Key.ID+"/update",
		`{"name":"Claude Code","enabled":false}`, master)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("disable response: %s", rec.Body)
	}

	// 改名 + 重新启用
	rec = panelReq(p, "POST", "/panel/api/keys/"+created.Key.ID+"/update",
		`{"name":"笔记本","enabled":true}`, master)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "笔记本") {
		t.Errorf("rename: code=%d body=%s", rec.Code, rec.Body)
	}

	// 未知 id / 非法 body
	if rec = panelReq(p, "POST", "/panel/api/keys/nope/update", `{"name":"x","enabled":true}`, master); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id update: code=%d want 404", rec.Code)
	}
	if rec = panelReq(p, "POST", "/panel/api/keys/"+created.Key.ID+"/update", `{bad`, master); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body update: code=%d want 400", rec.Code)
	}

	// 删除：立即从列表消失，重复删除 404
	if rec = panelReq(p, "POST", "/panel/api/keys/"+created.Key.ID+"/remove", "", master); rec.Code != http.StatusOK {
		t.Fatalf("remove: code=%d body=%s", rec.Code, rec.Body)
	}
	rec = panelReq(p, "GET", "/panel/api/keys", "", master)
	if strings.Contains(rec.Body.String(), created.Key.ID) {
		t.Error("removed key must disappear from list")
	}
	if rec = panelReq(p, "POST", "/panel/api/keys/"+created.Key.ID+"/remove", "", master); rec.Code != http.StatusNotFound {
		t.Errorf("double remove: code=%d want 404", rec.Code)
	}
}

// 未注入密钥库 → 501（与其余可选依赖同口径）；无密钥 → 401（鉴权层先拦）。
func TestKeysAPIWithoutStore(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "master"})
	if rec := panelReq(p, "GET", "/panel/api/keys", "", "master"); rec.Code != http.StatusNotImplemented {
		t.Errorf("code=%d want 501", rec.Code)
	}
	if rec := panelReq(p, "GET", "/panel/api/keys", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: code=%d want 401", rec.Code)
	}
}
