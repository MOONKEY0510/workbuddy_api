// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	return VerifyToken(key, BearerToken(r))
}

// VerifyToken 校验任意来源提取的 token 是否与 key 相等（常量时间）。
// key 为空表示"未启用鉴权"，恒返回 true；token 为空时仍走一次摘要比较，
// 保持耗时形状一致（不因"缺凭证"提前返回而泄露时序）。
//
// 供多协议入口复用：Anthropic 的 x-api-key、Gemini 的 x-goog-api-key / ?key=
// 等凭证位置由调用方提取后统一在此比对，鉴权口径只有一份。
func VerifyToken(key, token string) bool {
	if key == "" {
		return true
	}
	if token == "" {
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	return subtle.ConstantTimeCompare(digest(token), digest(key)) == 1
}

// BearerToken 提取 `Authorization: Bearer <token>` 中的 token（前缀大小写敏感，
// 与 VerifyBearer 同一解析口径）；缺头 / 方案不符返回空串。
// 托管密钥（internal/keys）的校验复用本函数，避免两处各写一份解析规则。
func BearerToken(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		return ""
	}
	return authz[len(bearerPrefix):]
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
