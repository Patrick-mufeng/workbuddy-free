package upstream

import (
	"fmt"
	"net/http"
	"testing"
)

// gateway401HTML 复刻上游 APISIX/openresty 网关的 401 错误页形状（生产实测）：
// 纯 HTML、无业务信封、无 12153 等 marker —— Classify 只能归 ErrClient。
const gateway401HTML = `<html>
<head><title>401 Authorization Required</title></head>
<body>
<center><h1>401 Authorization Required</h1></center>
<hr><center>openresty</center>
<p><em>Powered by <a href="https://apisix.apache.org/">APISIX</a>.</em></p>
</body>
</html>`

// TestIsGatewayUnauthorized 网关层 HTML 401（accessToken 被网关拒绝）应可识别；
// 业务层 401（12153 session 死）、其他状态码、非 *Error 一律不匹配。
func TestIsGatewayUnauthorized(t *testing.T) {
	gw := &Error{Kind: Classify(http.StatusUnauthorized, gateway401HTML), Status: 401, Msg: gateway401HTML}
	if gw.Kind != ErrClient {
		t.Fatalf("HTML 401 应归 ErrClient（网关层无业务 marker），got %v", gw.Kind)
	}
	if !IsGatewayUnauthorized(gw) {
		t.Errorf("网关 HTML 401 应返回 true")
	}

	sessionDead := &Error{
		Kind:   Classify(http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`),
		Status: 401,
		Msg:    `{"code":12153}`,
	}
	if sessionDead.Kind != ErrSessionDead {
		t.Fatalf("12153 JSON 401 应归 ErrSessionDead，got %v", sessionDead.Kind)
	}
	if IsGatewayUnauthorized(sessionDead) {
		t.Errorf("业务层 12153 401 不走网关 401 兜底（session 确死，直接按既有策略禁用）")
	}

	hardCredit := &Error{Kind: Classify(http.StatusPaymentRequired, "insufficient credit"), Status: 402, Msg: "insufficient credit"}
	if IsGatewayUnauthorized(hardCredit) {
		t.Errorf("402 不应匹配")
	}
	if IsGatewayUnauthorized(fmt.Errorf("plain error")) {
		t.Errorf("非 *Error 不应匹配")
	}
	if IsGatewayUnauthorized(nil) {
		t.Errorf("nil 不应匹配")
	}
}
