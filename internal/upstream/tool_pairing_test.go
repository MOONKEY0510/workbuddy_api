package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// msgsOf 把 JSON 片段（messages 数组）解成 []any，便于构造用例。
func msgsOf(t *testing.T, raw string) []any {
	t.Helper()
	var msgs []any
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return msgs
}

// rolesOf 提取输出里的 role 序列（非对象元素跳过）。
func rolesOf(t *testing.T, msgs []any) []string {
	t.Helper()
	var out []string
	for _, m := range msgs {
		if msg, ok := m.(map[string]any); ok {
			if role, ok := msg["role"].(string); ok {
				out = append(out, role)
			}
		}
	}
	return out
}

// toolIDsOf 提取 assistant.tool_calls 的 id 序列。
func toolIDsOf(t *testing.T, msg any) []string {
	t.Helper()
	m, _ := msg.(map[string]any)
	if m == nil {
		return nil
	}
	var ids []string
	for _, tci := range asSlice(m["tool_calls"]) {
		tc, _ := tci.(map[string]any)
		if tc == nil {
			continue
		}
		if id, _ := tc["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// asSlice 测试内的小工具（生产代码里的同名单测辅助在 convert，不在本包）。
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// TestCanonicalizeToolPairingNoTraffic 无工具流量：原 slice 原样返回（零改动零分配）。
func TestCanonicalizeToolPairingNoTraffic(t *testing.T) {
	msgs := msgsOf(t, `[{"role":"system","content":"s"},{"role":"user","content":"hi"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if changed {
		t.Fatal("无工具流量不应报告改动")
	}
	if len(out) != 2 {
		t.Fatalf("消息被改动: %v", out)
	}
}

// TestCanonicalizeToolPairingOrderedPairUntouched 配对完整且顺序正确：零改动。
func TestCanonicalizeToolPairingOrderedPairUntouched(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"user","content":"run"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"},
		{"role":"user","content":"next"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if changed {
		t.Fatalf("配对完整的请求不应被改动: %v", rolesOf(t, out))
	}
	if len(out) != 4 {
		t.Fatalf("消息数被改动: %d", len(out))
	}
}

// TestCanonicalizeToolPairingReordersResults 结果乱序（并行工具调用按完成顺序回填）：
// 按调用顺序重排——上游按位置校验配对，乱序 = 11148。
func TestCanonicalizeToolPairingReordersResults(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c2","content":"two"},
		{"role":"tool","tool_call_id":"c1","content":"one"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("乱序结果应被重排")
	}
	if got := rolesOf(t, out); len(got) != 3 || got[0] != "assistant" || got[1] != "tool" || got[2] != "tool" {
		t.Fatalf("roles=%v", got)
	}
	first, _ := out[1].(map[string]any)["tool_call_id"].(string)
	second, _ := out[2].(map[string]any)["tool_call_id"].(string)
	if first != "c1" || second != "c2" {
		t.Errorf("结果顺序 = %s,%s want c1,c2（按调用顺序）", first, second)
	}
}

// TestCanonicalizeToolPairingDropsUnpaired 缺结果的调用与缺调用的结果对称剔除：
// 任一侧留半截，上游都判 11148。
func TestCanonicalizeToolPairingDropsUnpaired(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"},
		{"role":"user","content":"u"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"user","content":"after"},
		{"role":"tool","tool_call_id":"c9","content":"orphan"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("存在无法配对的记录，应被收敛")
	}
	if got := rolesOf(t, out); strings.Join(got, ",") != "assistant,tool,user,assistant,user" {
		t.Fatalf("roles=%v want assistant,tool,user,assistant,user", got)
	}
	// c2 无结果 → tool_calls 键整个删除（不留空数组）。
	if ids := toolIDsOf(t, out[3]); len(ids) != 0 {
		t.Errorf("缺结果的调用未被裁剪: %v", ids)
	}
	// 孤儿结果（c9）被删除。
	for _, m := range out {
		if msg, ok := m.(map[string]any); ok && msg["role"] == "tool" {
			if id, _ := msg["tool_call_id"].(string); id == "c9" {
				t.Error("孤儿 tool 结果未被剔除")
			}
		}
	}
}

// TestCanonicalizeToolPairingStrayResultDoesNotBreakPairing 夹在批结果之前的杂散结果
// （无归属 / 没有 id）不得让后面合法的结果配不上对——否则整批工具上下文白丢。
func TestCanonicalizeToolPairingStrayResultDoesNotBreakPairing(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","content":"no id"},
		{"role":"tool","tool_call_id":"c1","content":"one"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("杂散结果应被剔除")
	}
	if got := rolesOf(t, out); strings.Join(got, ",") != "assistant,tool" {
		t.Fatalf("roles=%v want assistant,tool", got)
	}
	if ids := toolIDsOf(t, out[0]); strings.Join(ids, ",") != "c1" {
		t.Errorf("合法调用被误删: %v", ids)
	}
	if content, _ := out[1].(map[string]any)["content"].(string); content != "one" {
		t.Errorf("保留的不是合法结果: %q", content)
	}
}

// TestCanonicalizeToolPairingPartialBatchKeepsPaired 批内部分配对：只留配对成功的那条调用
// 与结果（历史实现会整批删调用、留下无主结果——出站载荷仍是半截配对）。
func TestCanonicalizeToolPairingPartialBatchKeepsPaired(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"one"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("半截配对应被裁剪")
	}
	if got := rolesOf(t, out); strings.Join(got, ",") != "assistant,tool" {
		t.Fatalf("roles=%v", got)
	}
	if ids := toolIDsOf(t, out[0]); strings.Join(ids, ",") != "c1" {
		t.Errorf("保留的调用 = %v want [c1]", ids)
	}
}

// TestCanonicalizeToolPairingResultBeforeCall 结果出现在调用之前（早到）：两侧都剔除，
// 不留「先见结果后见调用」的畸形顺序。
func TestCanonicalizeToolPairingResultBeforeCall(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"tool","tool_call_id":"c1","content":"early"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("早到结果应被剔除")
	}
	if got := rolesOf(t, out); len(got) != 1 || got[0] != "assistant" {
		t.Fatalf("roles=%v want [assistant]", got)
	}
	if ids := toolIDsOf(t, out[0]); len(ids) != 0 {
		t.Errorf("无结果的调用未被裁剪: %v", ids)
	}
}

// TestCanonicalizeToolPairingDedupesIDs 同批重复 id：只留首个调用、只留首个结果
// （重复 = 上游看到的调用/结果数量对不上）。
func TestCanonicalizeToolPairingDedupesIDs(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"one"},
		{"role":"tool","tool_call_id":"c1","content":"dup"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("重复 id 应被去重")
	}
	if got := rolesOf(t, out); strings.Join(got, ",") != "assistant,tool" {
		t.Fatalf("roles=%v want assistant,tool", got)
	}
	if ids := toolIDsOf(t, out[0]); len(ids) != 1 {
		t.Errorf("重复调用未被去重: %v", ids)
	}
	if content, _ := out[1].(map[string]any)["content"].(string); content != "one" {
		t.Errorf("保留的不是首个结果: %q", content)
	}
}

// TestCanonicalizeToolPairingIDlessToolDropped 没有 tool_call_id 的 tool 消息：
// 无法按 id 配对 → 剔除（否则上游判工具记录不完整）。
func TestCanonicalizeToolPairingIDlessToolDropped(t *testing.T) {
	msgs := msgsOf(t, `[{"role":"user","content":"u"},{"role":"tool","content":"x"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("无 id 的 tool 消息应被剔除")
	}
	if got := rolesOf(t, out); len(got) != 1 || got[0] != "user" {
		t.Fatalf("roles=%v want [user]", got)
	}
}

// TestCanonicalizeToolPairingMovesInterleavedMessage 插在结果之间的非 tool 消息
// （Codex image_resize_notice）挪到整组之后：结果必须连续。
func TestCanonicalizeToolPairingMovesInterleavedMessage(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"one"},
		{"role":"developer","content":"<image_resize_notice>"},
		{"role":"tool","tool_call_id":"c2","content":"two"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("插入消息应被挪后")
	}
	if got := rolesOf(t, out); strings.Join(got, ",") != "assistant,tool,tool,developer" {
		t.Fatalf("roles=%v want assistant,tool,tool,developer", got)
	}
}

// TestCanonicalizeToolPairingKeepsLaterBatch 插入物是下一组组头（assistant.tool_calls）时
// 不得被吞：它自己那批结果同样要（保持）配对。
func TestCanonicalizeToolPairingKeepsLaterBatch(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c2","content":"two"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("c1 无结果应被裁剪")
	}
	if got := rolesOf(t, out); strings.Join(got, ",") != "assistant,assistant,tool" {
		t.Fatalf("roles=%v want assistant,assistant,tool", got)
	}
	if ids := toolIDsOf(t, out[1]); strings.Join(ids, ",") != "c2" {
		t.Errorf("第二组被吞/被裁剪: %v", ids)
	}
}

// TestCanonicalizeToolPairingUniquifiesRepeatedCallIDs 长会话里模型按轮复用同一 call id
// （call_0 / call_1 这类序号 id）：上游按 id 匹配调用与结果，重复 id 会被判"对不上"。
// 规范化的处置是**改写重复项为唯一 id**（调用与其结果同步改写），首次出现保持原样。
func TestCanonicalizeToolPairingUniquifiesRepeatedCallIDs(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[{"id":"call_0","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_0","content":"first"},
		{"role":"user","content":"next"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_0","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_0","content":"second"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("重复 id 应被改写")
	}
	if got := rolesOf(t, out); strings.Join(got, ",") != "assistant,tool,user,assistant,tool" {
		t.Fatalf("roles=%v", got)
	}
	firstID := toolIDsOf(t, out[0])
	secondID := toolIDsOf(t, out[3])
	if len(firstID) != 1 || len(secondID) != 1 {
		t.Fatalf("调用数异常: %v / %v", firstID, secondID)
	}
	if firstID[0] != "call_0" {
		t.Errorf("首次出现应保持原 id: %v", firstID)
	}
	if secondID[0] == "call_0" {
		t.Errorf("重复 id 未被改写: %v", secondID)
	}
	// 结果必须与调用同步改写，否则上游按 id 匹配不上（等于没修）。
	r1, _ := out[1].(map[string]any)["tool_call_id"].(string)
	r2, _ := out[4].(map[string]any)["tool_call_id"].(string)
	if r1 != firstID[0] || r2 != secondID[0] {
		t.Errorf("结果 id 未与调用同步: %q/%q vs %q/%q", r1, r2, firstID[0], secondID[0])
	}
	// 全请求 id 唯一。
	seen := map[string]bool{}
	for _, id := range append(append([]string{}, firstID...), secondID...) {
		if seen[id] {
			t.Errorf("改写后仍有重复 id: %q", id)
		}
		seen[id] = true
	}
	if got := DiagnoseToolRecords(marshalMessages(t, out)); strings.Contains(got, "dup_ids=0") == false {
		t.Errorf("诊断应报告无重复 id: %s", got)
	}
}

// TestCanonicalizeToolPairingDropsTruncatedArguments 残缺 arguments（流被掐断留下的调用）
// 是上游判「内容已损坏」的另一来源：整条剔除（其残片结果按孤儿一并剔除）。
func TestCanonicalizeToolPairingDropsTruncatedArguments(t *testing.T) {
	msgs := msgsOf(t, `[
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"ls"}}]},
		{"role":"tool","tool_call_id":"c1","content":"failed to parse"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c2","content":"ok"}]`)
	out, changed := canonicalizeToolPairing(msgs)
	if !changed {
		t.Fatal("残缺参数的调用应被剔除")
	}
	if got := rolesOf(t, out); strings.Join(got, ",") != "assistant,assistant,tool" {
		t.Fatalf("roles=%v want assistant,assistant,tool", got)
	}
	if ids := toolIDsOf(t, out[0]); len(ids) != 0 {
		t.Errorf("残缺调用未被剔除: %v", ids)
	}
	if ids := toolIDsOf(t, out[1]); strings.Join(ids, ",") != "c2" {
		t.Errorf("良构调用被误伤: %v", ids)
	}
}

// TestDiagnoseToolRecords 现场取证：把"上游说的对不上"翻译成可读形状数据。
func TestDiagnoseToolRecords(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"f","arguments":"{\"cmd\":"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"},
		{"role":"tool","tool_call_id":"c9","content":"orphan"}]}`)
	got := DiagnoseToolRecords(body)
	for _, want := range []string{"calls=3", "results=2", "dup_ids=1", "bad_args=1", "orphan_results=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("诊断缺少 %q: %s", want, got)
		}
	}
	if got := DiagnoseToolRecords(nil); got != "unparsable body" {
		t.Errorf("空 body 诊断=%q", got)
	}
}

// marshalMessages 把 messages 重新序列化成出站 body（诊断函数按 body 工作）。
func marshalMessages(t *testing.T, msgs []any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestClassifyToolMismatch 11148 归 ErrToolMismatch（请求级终态）：
// 归 ErrClient 会喂连败降权——一条坏会话足以把整池账号逐个降权出池。
func TestClassifyToolMismatch(t *testing.T) {
	// 用户实报的上游原文（Codex → kimi-k3 → 11148）。
	real := `{"code":11148,"msg":"tool calls and tool results do not match, please start a new conversation and retry",` +
		`"requestId":"728c2d6e0fff315132e23c6932f72b2a","extError":{"code":"400001","message":"tool calls and tool results do not match",` +
		`"param":"","type":"invalid_request_error","StatusCode":400,"Request":null,"Response":null},` +
		`"displayMsg":{"en":"Incomplete tool records. Please start a new task","zh":"工具记录不完整，请新建任务"},` +
		`"displayTips":{"en":"The tool call records in this conversation do not match and the content is corrupted, so retrying will not help. Please start a new task.","zh":"对话里的工具调用记录对不上，内容已损坏，直接重试无效。请新建任务重新开始。"},` +
		`"actions":["NEW_CONVERSATION","SUBMIT_FEEDBACK"]}`
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"实报 body（400）", http.StatusBadRequest, real},
		{"空白容差的 code 形态", http.StatusBadRequest, `{"code": 11148, "msg":"x"}`},
		{"中文字案形态（displayMsg）", http.StatusBadRequest, `{"code":0,"displayMsg":{"zh":"工具记录不完整，请新建任务"}}`},
		{"包在 5xx 外壳里也认", http.StatusServiceUnavailable, `{"code":11148,"msg":"tool calls and tool results do not match"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != ErrToolMismatch {
				t.Fatalf("Classify=%v want ErrToolMismatch", got)
			}
			if (ErrToolMismatch).String() != "tool_mismatch" {
				t.Fatalf("kind 词表漂移: %q", (ErrToolMismatch).String())
			}
			if hint := GatewayHint(ErrToolMismatch, c.body, HintContext{}); hint == "" {
				t.Error("tool_mismatch 应给 hint（新建会话指向）")
			}
		})
	}
	// 防过宽：普通的 400 业务错误不得被误判。
	if got := Classify(400, `{"code":1,"msg":"bad request"}`); got == ErrToolMismatch {
		t.Error("普通 4xx 被误判为 tool_mismatch")
	}
}

// TestStripToolTraffic 自愈重试用：剥离全部工具记录 + 删掉空壳 assistant。
func TestStripToolTraffic(t *testing.T) {
	body := []byte(`{"model":"kimi-k3","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"do it"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"},` +
		`{"role":"assistant","content":"done"},` +
		`{"role":"user","content":"next"}],` +
		`"tools":[{"type":"function","function":{"name":"f"}}]}`)
	out, changed := StripToolTraffic(body)
	if !changed {
		t.Fatal("含工具记录应报告改动")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	msgs := obj["messages"].([]any)
	if got := rolesOf(t, msgs); strings.Join(got, ",") != "system,user,assistant,user" {
		t.Fatalf("roles=%v want system,user,assistant,user", got)
	}
	for _, m := range msgs {
		if _, has := m.(map[string]any)["tool_calls"]; has {
			t.Error("tool_calls 未被剥离")
		}
	}
	// 工具定义保留：剥离的是历史记录，不是工具能力。
	if _, has := obj["tools"]; !has {
		t.Error("tools 不应被剥离")
	}
	// 无工具流量：零改动、原 body 返回。
	if _, changed := StripToolTraffic([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); changed {
		t.Error("无工具流量不应报告改动")
	}
	if _, changed := StripToolTraffic(nil); changed {
		t.Error("空 body 不应报告改动")
	}
}
