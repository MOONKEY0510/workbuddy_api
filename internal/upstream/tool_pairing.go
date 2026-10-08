// tool_pairing.go 出站请求体的 tool_call↔tool 结果配对规范化
// （吸收参考仓库 sse.ts:91-123 resolveToolPairing 语义，适配网关的 OpenAI wire 消息形态）。
//
// 背景：OpenAI 兼容协议要求带 tool_calls 的 assistant 消息，其每一个 tool_call id 都必须
// 有对应的 role:tool 结果消息、且结果**紧接着连续出现**；反之 role:tool 消息也必须有
// 对应的前置 tool_call。缺任一侧、插进别的消息、结果乱序、同一 id 重复，上游都会以
// HTTP 400 code=11148（tool calls and tool results do not match）拒绝整个请求并提示
// 「请新建任务」——整条会话报废，且直接重试无效。
//
// 工具执行失败时（参数非法、超时、工具不存在、用户中断……）客户端会把 assistant 的
// tool_calls 持久化进会话历史，却写不回结果消息；并行工具调用的结果还常以**完成顺序**
// 回填。这条坏历史随后被每次请求原样重放。网关是最后一道防线：发出请求前把工具记录
// 整理成规范形态让会话自愈——宁可丢一轮工具上下文，也好过整条会话死亡。
//
// 规范化（canonicalizeToolPairing，一趟扫描完成）：
//  1. 批内重复 id 只保留首个调用；无 id 的调用无法按 id 配对 → 丢弃；
//  2. **请求级 id 唯一**：同一 call id 在更早的批里出现过时改写为唯一值，调用与其结果
//     同步改写——上游按 id 匹配调用/结果，重复 id 会被判"对不上"（长会话里模型按轮
//     复用 call_0/call_1 这类序号 id 是常见形态）；
//  3. **残缺 parameters 的调用**（流被掐断留下的 arguments 非合法 JSON）整条剔除——
//     这种记录客户端会持久化并反复重放，是上游判「内容已损坏」的另一来源；
//  4. 结果按**调用顺序**落位（并行调用的结果按完成顺序回填 = 上游判配对断裂）；
//  5. 缺结果的调用、缺调用的结果（含出现在调用之前的"早到"结果）、同 id 的重复结果
//     一律丢弃；
//  6. 插在批与其结果之间的非 tool 消息挪到整组之后（结果必须连续）；
//  7. 无任何工具流量时零改动零分配（返回原 slice）。
package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

// canonicalizeToolPairing 把 messages 整理成上游可接受的工具记录形态（见文件头注释）。
// 返回清理后的 slice（无改动时等于原 slice，勿依赖其是否新分配）及是否发生改动。
func canonicalizeToolPairing(messages []any) ([]any, bool) {
	if !hasToolTraffic(messages) {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	// usedIDs / idSeq 跨批共享：保证整个请求内 call id 唯一（重复的改写，首次出现的原样保留）。
	usedIDs := make(map[string]bool)
	idSeq := 0
	for i := 0; i < len(messages); i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			out = append(out, messages[i])
			continue
		}
		role, _ := msg["role"].(string)
		if role == "tool" {
			// 组头处理时会把本批结果一并取走并推进下标；走到这里的结果 = 没有归属
			// （调用不存在 / 已配过 / 早于调用）→ 孤儿，丢弃。缺了它上游才认。
			changed = true
			continue
		}
		if role != "assistant" {
			out = append(out, messages[i])
			continue
		}
		tcs, has := msg["tool_calls"].([]any)
		if !has || len(tcs) == 0 {
			out = append(out, messages[i])
			continue
		}
		// 批内规范化：残缺参数的调用整条剔除、同 id 只留首个、无 id 的调用丢弃，
		// 并给出每条调用的「唯一 id」（首次出现保持原样，重复的改写）。
		kept := make([]any, 0, len(tcs))
		origIDs := make([]string, 0, len(tcs)) // 与 kept 同序：客户端/上游看到的原 id（结果按它匹配）
		uniqIDs := make([]string, 0, len(tcs)) // 与 kept 同序：出站用的唯一 id（结果同步改写）
		batchSeen := make(map[string]bool, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			id, _ := tc["id"].(string)
			if id == "" || batchSeen[id] {
				continue
			}
			if !validToolArguments(tc) {
				// 流被掐断留下的残缺 arguments：这种"调用"永远无法被正确执行，客户端却会
				// 把它持久化进会话并每次重放（上游据此判「内容已损坏」）。整条剔除。
				changed = true
				continue
			}
			batchSeen[id] = true
			uniq := id
			if usedIDs[id] {
				// 该 id 在更早的批里已用过（模型按轮复用序号 id）：改写为唯一值，
				// 调用与结果必须同步改写，否则上游按 id 匹配不上。
				uniq = uniqueToolCallID(id, usedIDs, &idSeq)
				tc["id"] = uniq
				changed = true
			}
			usedIDs[id] = true
			usedIDs[uniq] = true
			kept = append(kept, tc)
			origIDs = append(origIDs, id)
			uniqIDs = append(uniqIDs, uniq)
		}
		if len(kept) != len(tcs) {
			changed = true
		}
		if len(kept) == 0 {
			delete(msg, "tool_calls")
			out = append(out, messages[i])
			continue
		}
		if len(kept) != len(tcs) {
			msg["tool_calls"] = kept
		}
		// 结果收集：槽位 = 调用顺序（结果按槽位落位即为「按调用顺序输出」）。
		slots := make([]any, len(kept))
		slotOf := make(map[string]int, len(kept))
		for idx, id := range origIDs {
			slotOf[id] = idx
		}
		var between []any
		filled := 0
		ordered := true // 结果到达顺序是否已与调用顺序一致
		lastSlot := -1
		j := i + 1
		for j < len(messages) {
			mm, ok := messages[j].(map[string]any)
			if !ok {
				break
			}
			r, _ := mm["role"].(string)
			if r == "tool" {
				id, _ := mm["tool_call_id"].(string)
				slot, ok := slotOf[id]
				if !ok {
					// 不是本批的（孤儿 / 无 id / 后批）：跳过不吞——它没有归属，外层会
					// 按孤儿剔除；这里**继续**扫描（而非停手），否则一条夹在本批结果
					// 之前的杂散结果会让后面合法的结果配不上对（整批工具上下文白丢）。
					// 扫描窗口仍被下面「下一组组头」的 break 界住，不会越组收编。
					changed = true
					j++
					continue
				}
				if slots[slot] != nil {
					changed = true // 同一调用重复结果：丢弃
					j++
					continue
				}
				if len(between) > 0 {
					// 这条结果前面夹着别的消息 → 它被前移，顺序确实变了。
					changed = true
				}
				if uid := uniqIDs[slot]; uid != origIDs[slot] {
					// 该调用的 id 被改写为唯一值：结果同步改写（上游按 id 匹配调用/结果）。
					mm["tool_call_id"] = uid
					changed = true
				}
				slots[slot] = messages[j]
				filled++
				if slot < lastSlot {
					ordered = false // 结果乱序：重排（上游按位置校验配对）
				}
				lastSlot = slot
				j++
				continue
			}
			if filled == 0 {
				break // 本批尚无结果：不吞任何东西（交外层处理）
			}
			// 下一组 assistant.tool_calls 是新的组头，绝不能当插入物吞掉：一旦被收进
			// between，它永远不再被外层循环当作组头处理，它自己那批结果也就永远得不到
			// 重排。必须 break 交还外层循环。
			if r == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			// 同批结果尚未收齐/刚收齐时的中间消息：暂存，等本批结果收齐后整体后移。
			// 注意这里**不算改动**——若其后没有任何本批结果，这些消息在输出里的相对
			// 位置与输入完全一致（依旧紧跟结果之后），报告改动会白白重建整份载荷。
			between = append(between, messages[j])
			j++
		}
		if !ordered {
			changed = true
		}
		// 调用侧与结果侧按**同一份槽位**对称裁剪：缺结果/被丢的调用不留半截配对。
		trimmed := kept
		results := make([]any, 0, filled)
		if filled < len(kept) {
			changed = true
			trimmed = make([]any, 0, filled)
			for idx, tci := range kept {
				if slots[idx] == nil {
					continue
				}
				trimmed = append(trimmed, tci)
				results = append(results, slots[idx])
			}
			if len(trimmed) == 0 {
				delete(msg, "tool_calls")
				out = append(out, messages[i])
				out = append(out, between...)
				i = j - 1
				continue
			}
			msg["tool_calls"] = trimmed
		} else {
			for _, res := range slots {
				results = append(results, res)
			}
		}
		out = append(out, messages[i])
		out = append(out, results...)
		out = append(out, between...)
		i = j - 1
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// validToolArguments 报告一条 assistant.tool_calls 条目的 function.arguments 是否为
// 完整合法的 JSON。流被掐断时残留的 arguments 是残缺分片（如 `{"cmd":"ls`），客户端会
// 把这条调用持久化进会话并反复重放——上游校验工具记录时判「内容已损坏」（11148）。
// 非字符串形态（对象/缺省）不做判定：那是别的 client 的合法写法；空串按缺省处理。
func validToolArguments(tc map[string]any) bool {
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		return true
	}
	args, ok := fn["arguments"].(string)
	if !ok || strings.TrimSpace(args) == "" {
		return true
	}
	return json.Valid([]byte(args))
}

// uniqueToolCallID 为本请求内「已经出现过的 call id」分配唯一替代值（原值 + 序号，
// 跳过已被占用的候选）。只改写重复出现的那一条，首次出现保持原样——正常会话零改动。
func uniqueToolCallID(orig string, used map[string]bool, seq *int) string {
	for {
		*seq++
		cand := fmt.Sprintf("%s_%d", orig, *seq)
		if !used[cand] {
			return cand
		}
	}
}

// DiagnoseToolRecords 汇总请求体内工具记录的形状，供上游 11148（判「工具记录对不上」）
// 的现场取证：调用/结果条数、重复 id、残缺 arguments、未配对条目。只读，不改动入参。
// 输出形如：calls=3 results=3 dup_ids=0 bad_args=0 unpaired_calls=1 orphan_results=0
func DiagnoseToolRecords(body []byte) string {
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if len(body) == 0 || json.Unmarshal(body, &obj) != nil {
		return "unparsable body"
	}
	callSeen := map[string]int{}   // id → 出现次数
	pending := map[string]int{}    // 已见调用、尚未见结果的 id 计数
	calls, results, badArgs, orphan := 0, 0, 0, 0
	for _, m := range obj.Messages {
		switch m["role"] {
		case "assistant":
			tcs, _ := m["tool_calls"].([]any)
			for _, tci := range tcs {
				tc, _ := tci.(map[string]any)
				if tc == nil {
					continue
				}
				id, _ := tc["id"].(string)
				calls++
				callSeen[id]++
				pending[id]++
				if !validToolArguments(tc) {
					badArgs++
				}
			}
		case "tool":
			results++
			id, _ := m["tool_call_id"].(string)
			if pending[id] > 0 {
				pending[id]--
			} else {
				orphan++
			}
		}
	}
	dups, unpaired := 0, 0
	for id, n := range callSeen {
		if id == "" {
			continue
		}
		if n > 1 {
			dups += n - 1
		}
	}
	for _, n := range pending {
		unpaired += n
	}
	return fmt.Sprintf("calls=%d results=%d dup_ids=%d bad_args=%d unpaired_calls=%d orphan_results=%d",
		calls, results, dups, badArgs, unpaired, orphan)
}

// hasToolTraffic 报告 messages 是否含任何工具流量（assistant.tool_calls 或 role:tool）。
// 无工具流量 → 规范化零改动零分配，热路径（绝大多数请求）不受影响。
func hasToolTraffic(messages []any) bool {
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "tool" {
			return true
		}
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			return true
		}
	}
	return false
}

// StripToolTraffic 剥离请求体内**全部**工具记录（assistant.tool_calls 与 role:tool 消息），
// 供上游返回 11148 时的自愈重试使用（handler 一次性重试的输入体）。
//
// 为什么整段剥离而不是继续精细化配对：11148 是上游对「这条会话的工具记录」的终态判定，
// 规范化后仍被拒，说明差异落在网关看不到的维度上。丢掉整轮工具上下文换来会话继续可用
// （对齐参考仓库「宁可丢一轮工具上下文，也好过整条会话死亡」）；新产生的工具调用/结果
// 都是良构的，会话随轮次自然收敛。
//
// 附带清理：删掉 tool_calls 后既无正文也无思考内容的 assistant 消息（纯工具轮产物，
// 空 content 会被部分上游按畸形消息拒掉）。tools / tool_choice 保持不动——工具定义
// 本身不是问题，模型后续仍可正常调用工具。
// 返回（新 body, 是否发生改动）；无工具流量时返回原 body 与 false（调用方据此跳过重试）。
func StripToolTraffic(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, false
	}
	kept := make([]any, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		switch msg["role"] {
		case "tool":
			changed = true
			continue
		case "assistant":
			if _, has := msg["tool_calls"]; has {
				delete(msg, "tool_calls")
				changed = true
			}
			if !hasVisibleContent(msg) {
				changed = true
				continue // 空壳 assistant（纯工具轮）：删除，避免空 content 畸形消息
			}
		}
		kept = append(kept, msg)
	}
	if !changed {
		return body, false
	}
	obj["messages"] = kept
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// hasVisibleContent 报告 assistant 消息剥离 tool_calls 后是否仍有可见内容
// （正文或思考链）。content 为字符串时判非空；数组/对象形态一律视为有内容（不深挖）。
func hasVisibleContent(msg map[string]any) bool {
	if s, ok := msg["content"].(string); ok {
		if strings.TrimSpace(s) != "" {
			return true
		}
	} else if c, has := msg["content"]; has && c != nil {
		return true
	}
	if s, ok := msg["reasoning_content"].(string); ok && strings.TrimSpace(s) != "" {
		return true
	}
	return false
}
