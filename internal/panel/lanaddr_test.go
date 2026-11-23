package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// accessURL 的判据是「这个地址对其它设备是否可达」：
// 非回环 Host 直接用（对方既然访问到了，地址就是通的），回环时才去枚举网卡。
func TestAccessURL(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string // "" = 不校验具体值（依赖运行环境的网卡）
	}{
		{"局域网 IP 原样返回", "192.168.1.50:3012", "http://192.168.1.50:3012"},
		{"自定义端口保留", "10.0.0.7:8080", "http://10.0.0.7:8080"},
		{"主机名原样返回", "gateway.local:3012", "http://gateway.local:3012"},
		{"回环走网卡兜底", "127.0.0.1:3012", ""},
		{"localhost 走网卡兜底", "localhost:3012", ""},
		{"IPv6 回环走网卡兜底", "[::1]:3012", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/panel/api/overview", nil)
			r.Host = c.host
			got := accessURL(r)
			if c.want == "" {
				// 兜底取决于本机网卡：可能取到 IP，也可能取不到（容器/无网卡）。
				// 关键是不能返回回环地址——那个地址对其它设备没有意义。
				if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "::1") {
					t.Errorf("accessURL(%q) = %q, 不应返回回环地址", c.host, got)
				}
				return
			}
			if got != c.want {
				t.Errorf("accessURL(%q) = %q, want %q", c.host, got, c.want)
			}
		})
	}
}

// 根路径必须重定向到面板：局域网内其它设备只输入 "http://<IP>:3012" 即可打开界面。
func TestRootRedirectsToPanel(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	// panel 自身的 mux 不含根路径（挂在 /panel/ 下），故这里直接验证
	// 外层 handler 的行为见 internal/server 的测试；此处只确保
	// panel 不为根路径提供意外内容。
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "workbuddy-free") {
		t.Error("panel 不应自行服务根路径（应由 server.handler 重定向）")
	}
}

// 面板页面必须含基础 viewport 声明，否则手机浏览器退化为桌面缩放版。
// 注：面板 UI 已回退为 1413201 的桌面版（用户选择）；手机优先版
// （viewport-fit=cover / safe-area-inset）随 b6ca9fb 一同退场，
// 故此处只校验基础 viewport，不再断言刘海屏安全区声明。
func TestPanelMobileViewport(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `name="viewport"`) {
		t.Errorf("index.html 缺少基础 viewport 声明")
	}
}
