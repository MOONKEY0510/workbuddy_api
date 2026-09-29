package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCallsPageWindow 分页页码窗口算法（纯逻辑，用 node 在 vm 里直调 app.js 的
// callsPageWindow）。规则：页数 ≤10 全展开；否则首尾常驻 + 当前页 ±1，中间折叠为
// 省略号（当前页靠边时只折叠一侧）。调用记录是纯前端切片分页，翻页可达性全靠它。
func TestCallsPageWindow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; pager window check skipped")
	}
	script := appJSHarness + `
vm.runInContext(src, sandbox, { filename: 'app.js' }); // 先求值，函数声明才可用
const cases = [[1, 1], [1, 10], [5, 10], [1, 15], [8, 15], [15, 15], [2, 3]];
const out = vm.runInContext(
  'JSON.stringify(' + JSON.stringify(cases) + '.map(function (c) { return callsPageWindow(c[0], c[1]); }))',
  sandbox);
console.log('WINDOW:' + out);
process.exit(0);
`
	path := filepath.Join(t.TempDir(), "pager-window.cjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, path, "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node 执行失败: %v\n%s", err, out)
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "WINDOW:") {
			line = strings.TrimPrefix(l, "WINDOW:")
		}
	}
	if line == "" {
		t.Fatalf("未取到 WINDOW 行（app.js 求值失败？）:\n%s", out)
	}
	var got [][]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("解析窗口结果失败: %v (%s)", err, line)
	}
	seq := func(a, b int) []any {
		s := make([]any, 0, b-a+1)
		for i := a; i <= b; i++ {
			s = append(s, float64(i))
		}
		return s
	}
	dots := "…"
	want := [][]any{
		seq(1, 1),  // 单页
		seq(1, 10), // 恰好 10 页：全展开
		seq(1, 10), // 中间页也在 10 页内：全展开
		{float64(1), float64(2), dots, float64(15)},                               // 首页：右侧折叠
		{float64(1), dots, float64(7), float64(8), float64(9), dots, float64(15)}, // 中间页：两侧折叠
		{float64(1), dots, float64(14), float64(15)},                              // 末页：左侧折叠
		seq(1, 3), // 页数少：全展开
	}
	if len(got) != len(want) {
		t.Fatalf("用例数不匹配: got %d want %d（%s）", len(got), len(want), line)
	}
	for i := range want {
		if fmt.Sprint(got[i]) != fmt.Sprint(want[i]) {
			t.Errorf("case #%d: got %v want %v", i, got[i], want[i])
		}
	}
}

// TestClipboardGoesThroughCopyText 所有剪贴板写入必须走 copyText 统一封装。
//
// 为什么需要：clipboard API 只在 secure context（https / localhost）存在——用 http
// 访问内网 IP 上的面板时 navigator.clipboard 为 undefined，直接调用会同步抛
// TypeError，表现为「点复制提示失败」。Key 管理「复制」按钮就是这么坏的（券码复制
// 早已走 copyText 降级，新增入口没跟上）。本测试把「复制入口唯一」固化为不变量。
func TestClipboardGoesThroughCopyText(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(js), "\n")
	defIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "async function copyText") {
			defIdx = i
			break
		}
	}
	if defIdx < 0 {
		t.Fatal("app.js 中未找到 async function copyText（封装被删或改名，需同步本测试）")
	}
	endIdx := len(lines) - 1
	for i := defIdx + 1; i < len(lines); i++ {
		if lines[i] == "}" { // 顶格右括号 = 函数体结束（内部嵌套都有缩进）
			endIdx = i
			break
		}
	}
	var hits []int
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		if strings.Contains(l, "navigator.clipboard.writeText(") {
			hits = append(hits, i+1)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("navigator.clipboard.writeText 应只在 copyText 内出现 1 次，实际 %d 次（行 %v）——复制入口必须统一走 copyText", len(hits), hits)
	}
	if hits[0]-1 < defIdx || hits[0]-1 > endIdx {
		t.Errorf("clipboard.writeText 出现在 copyText（行 %d-%d）之外，行 %d：http 访问时该调用会抛 TypeError",
			defIdx+1, endIdx+1, hits[0])
	}
}

// TestBeautifySelectTargetsExist app.js 里 beautifySelect($('id')) 注册的每个 id
// 必须在 index.html 中存在。
//
// 为什么需要：beautifySelect 以 `if (!sel) return` 静默兜底，id 拼错时页面回落到
// 系统原生下拉（Windows 上的蓝色高亮弹层，与面板风格完全脱节），而 Go 侧测试全绿。
// 调用记录「字段过滤」的漏注册（新增 select 忘了加入注册列表）就是这么暴露的——
// 该测试把「注册列表 ↔ 页面元素」的一致性前移到 CI。
func TestBeautifySelectTargetsExist(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	re := regexp.MustCompile(`beautifySelect\(\$\('([^']+)'\)\)`)
	matches := re.FindAllStringSubmatch(string(js), -1)
	if len(matches) == 0 {
		t.Fatal("app.js 中未找到任何 beautifySelect 调用（正则或写法已变更，需同步本测试）")
	}
	for _, m := range matches {
		id := m[1]
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("beautifySelect($('%s')) 的目标 id 在 index.html 中不存在（下拉会回落原生样式）", id)
		}
	}
}

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// appJSHarness 是一段以 DOM 桩执行 app.js 的 Node 脚本前缀（TestAppJSTopLevelSmoke
// 与 TestCallsPageWindow 共用，避免两处各写一份桩后互相漂移）。设计：sandbox 是
// Proxy——未知属性一律返回 inert（可调用、可构造、取值返回自身），因此 app.js
// 顶层随意访问 DOM/浏览器 API 都不会崩，只验证"求值本身不抛错"。
// 调用方（process.argv[2] = app.js 路径）在 vm.createContext 之后自行追加断言代码。
const appJSHarness = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
`

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	script := appJSHarness + `try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(script); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages", "#keys"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}
