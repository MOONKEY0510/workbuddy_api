// compensation.go 面板「积分补偿领取」接口：全账号批量领取活动补偿。
//
// 上游端点是官方「用量管理」页的手动领取入口（internal/upstream/blackcat.go
// ClaimCompensation），成功响应不携带金额（{"code":0,"msg":"OK"}），故本 handler
// 以"领取前后余额差"呈现到账情况：先 RunBalanceRefreshNow 刷新一次取基线，领取
// 完成后再刷新一次对比。账号量小（个位数），同步等待换确定性，与 balanceAll 同取舍。
package panel

import (
	"log"
	"net/http"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// compensationClaim 批量领取活动补偿（POST /panel/api/compensation/claim）。
//
// 每号结果独立：单号失败只影响该行；disabled 账号不展示（与 schoolVouchers 同口径）；
// global realm 无此活动，标 skipped 不发上游调用。status 取值：
//   - claimed   领取成功且余额有新增（delta = 本次到账）
//   - unchanged 领取成功但余额未变（已领过，或上游异步到账未落账）
//   - skipped   未发起请求（global realm）
//   - error     上游拒绝（error 透出原始 msg，如"已领取"）
func (p *Panel) compensationClaim(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	type row struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Realm    string `json:"realm"`
		Before   int64  `json:"before"`
		After    int64  `json:"after"`
		Delta    int64  `json:"delta"`
		Status   string `json:"status"`
		Err      string `json:"error,omitempty"`
	}

	// 领取前刷新一次取基线：池内 credits 最长滞后一个刷新周期（默认 5 分钟），
	// 直接拿旧值算差额会把这段时间的正常消耗混进"补偿到账"里。
	p.cfg.Scheduler.RunBalanceRefreshNow()
	accts := p.cfg.Pool.List()

	out := make([]row, len(accts))
	sem := make(chan struct{}, 3) // 与 schoolVouchers 同口径限并发，防上游风控
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth, realm string, before int64) {
			defer wg.Done()
			it := row{UID: a.UID, Nickname: a.Nickname, Realm: realm, Before: before}
			switch {
			case a.IsGlobal():
				it.Status = "skipped"
				it.Err = "global realm（无此活动）"
			default:
				sem <- struct{}{}
				if err := p.cfg.Upstream.ClaimCompensation(a); err != nil {
					it.Status = "error"
					it.Err = err.Error()
				} else {
					it.Status = "claimed" // 领取成功后按余额差修正为 unchanged
				}
				<-sem
			}
			out[i] = it
		}(i, a, st.Realm, st.Credits)
	}
	wg.Wait()

	// 领取后统一刷新余额取"后值"：上游响应无金额，差额即本次到账。
	p.cfg.Scheduler.RunBalanceRefreshNow()
	after := map[string]int64{}
	for _, st := range p.cfg.Pool.List() {
		after[st.UID] = st.Credits
	}
	claimed, unchanged, failed, skipped := 0, 0, 0, 0
	var deltaTotal int64
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID == "" {
			continue
		}
		it.After = after[it.UID]
		it.Delta = it.After - it.Before
		switch it.Status {
		case "claimed":
			if it.Delta > 0 {
				claimed++
				deltaTotal += it.Delta
			} else {
				it.Status = "unchanged"
				unchanged++
			}
		case "error":
			failed++
		case "skipped":
			skipped++
		}
		res = append(res, it)
	}
	log.Printf("panel: 补偿领取 到账=%d 无新增=%d 失败=%d 跳过=%d 合计+%d", claimed, unchanged, failed, skipped, deltaTotal)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": res,
		"claimed": claimed, "unchanged": unchanged, "failed": failed, "skipped": skipped,
		"delta_total": deltaTotal,
	})
}
