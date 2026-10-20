// queue.go 上游排队等待（业务码 6020）的识别与重试信息解析。
//
// 实测响应形态（WorkBuddy 聊天接口排队时）：
//
//	{"code":6020,"msg":"queue.waiting.title","requestId":"...",
//	 "data":{"queue_position":2710,"queue_size":"999+","retry_after":40,
//	         "estimated_wait":13550,"client_user_type":"free"}}
//
// 语义要点（修复「排队把号池打成熔断」）：
//   - 这是**全站排队**信号，与账号健康无关：同一时刻换任何账号都在同一条队里，
//     因此不得计入熔断/连败/冷却，也不该轮转换号。
//   - 外壳状态码不固定（实测可能 200 / 429 / 5xx），判定必须以 body 为准，且要
//     排在 Classify 所有状态码分支之前——否则 5xx 外壳会被误判 ErrServer 喂熔断。
package upstream

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// queueWaitCode 排队等待业务码（code 6020）。
const queueWaitCode = "6020"

// queueWaitMaxRetry 采信的上游建议等待上限（超过视为脏数据，回落本地策略）。
const queueWaitMaxRetry = 30 * time.Minute

// queueHeadLimit 200 + JSON 信封判定时最多预读的字节数（排队信封实测 < 300B）。
const queueHeadLimit = 4096

// IsQueueWait 报告 body 是否为上游排队响应（业务码 6020 / queue.waiting 文案 /
// queue_position 字段）。与状态码无关：外壳可能是 200、429 或 5xx。
func IsQueueWait(body string) bool {
	lower := strings.ToLower(body)
	return codeMarker(lower, queueWaitCode) ||
		strings.Contains(lower, "queue.waiting") ||
		strings.Contains(lower, "queue_position")
}

// QueueWait 排队响应的重试信息（字段缺失即零值，不报错）。
type QueueWait struct {
	RetryAfter time.Duration // 上游建议的重试等待（data.retry_after，秒）
	Position   int64         // 队列位置（data.queue_position）
	Size       string        // 队列长度（data.queue_size，可能是 "999+" 这类字符串）
}

// ParseQueueWait 宽松解析排队响应：
//   - RetryAfter 优先取 data.retry_after（秒）；缺失时回落 estimated_wait 的毫秒口径
//     （实测样本 retry_after=40 与 estimated_wait=13550 并存，40s 与 13.55s 同量级）。
//   - 非 JSON / 字段畸形一律返回零值，绝不 panic、不报错（分类路径不能因解析失败受伤）。
func ParseQueueWait(body string) QueueWait {
	var env struct {
		Data struct {
			RetryAfter    float64         `json:"retry_after"`
			EstimatedWait float64         `json:"estimated_wait"`
			Position      int64           `json:"queue_position"`
			Size          json.RawMessage `json:"queue_size"`
		} `json:"data"`
	}
	var q QueueWait
	if json.Unmarshal([]byte(body), &env) != nil {
		return q
	}
	q.Position = env.Data.Position
	// queue_size 可能是字符串（"999+"）或数字，统一成展示字符串。
	if len(env.Data.Size) > 0 {
		var s string
		if json.Unmarshal(env.Data.Size, &s) == nil {
			q.Size = s
		} else {
			var n int64
			if json.Unmarshal(env.Data.Size, &n) == nil {
				q.Size = strconv.FormatInt(n, 10)
			}
		}
	}
	sec := env.Data.RetryAfter
	if sec <= 0 && env.Data.EstimatedWait > 0 {
		sec = env.Data.EstimatedWait / 1000
	}
	if sec > 0 {
		if d := time.Duration(sec * float64(time.Second)); d <= queueWaitMaxRetry {
			q.RetryAfter = d
		}
	}
	return q
}

// multiReaderBody 把「已预读的前缀 + 尚未消费的流」拼回 io.ReadCloser：
// Close 仍落到原始 body（连接归还 / ctx 取消语义不变）。
type multiReaderBody struct {
	io.Reader
	io.Closer
}

// readJSONHead 预读一小段 JSON 前缀（最多 queueHeadLimit）：
// 每次 Read 只取当前已到达的数据，最多两轮——排队信封极小，正常一到两轮即完整；
// 不做无界循环，避免上游掐流时在分类阶段长时间阻塞。
func readJSONHead(br *bufio.Reader) []byte {
	buf := make([]byte, 0, 512)
	tmp := make([]byte, queueHeadLimit)
	for i := 0; i < 2 && len(buf) < queueHeadLimit; i++ {
		n, err := br.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if json.Valid(buf) {
				return buf
			}
		}
		if err != nil {
			return buf
		}
		if n == 0 {
			return buf
		}
	}
	return buf
}
