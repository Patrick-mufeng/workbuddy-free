package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 根路径必须 302 到面板：局域网内其它设备只要在浏览器输入
// "http://<网关IP>:3012" 就能打开管理界面，不用记 /panel/ 后缀。
//
// 用 302 而非 301：面板路径若将来变动，301 会被浏览器永久缓存且用户清不掉。
func TestRootRedirectsToPanel(t *testing.T) {
	h := NewHandler(Config{Panel: http.NotFoundHandler()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("GET / code=%d want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/panel/" {
		t.Errorf("Location=%q want /panel/", loc)
	}
}

// 未注入面板时（例如只做网关用途）根路径不应重定向到不存在的地方，
// 而应维持原来的 404 行为——否则用户会被送到一个空页面。
func TestRootWithoutPanel(t *testing.T) {
	h := NewHandler(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("无面板时 GET / code=%d want 404", rec.Code)
	}
}
