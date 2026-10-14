// lanaddr.go 面板的「从其它设备访问」地址推导。
//
// 面板可以同时被两个视角打开：
//   - 本机：手机 App 的 WebView / 本机浏览器，地址是 127.0.0.1；
//   - 其它设备：局域网内的电脑、平板，地址是网关机器的局域网 IP。
//
// 「复制访问地址」按钮要给出的是后者——从 127.0.0.1 复制出来的地址对别人没用。
// 因此优先用请求的 Host（对方既然能访问到，这个地址就一定是通的），
// 只有当 Host 是回环地址时才去枚举网卡找局域网 IP。
package panel

import (
	"net"
	"net/http"
	"strings"
)

// accessURL 返回「其它设备可以用的」面板地址。
//
// 判据是请求自带的 Host：非回环时直接用（它还自动带上了对方用的端口，
// 端口若被改过也照样正确）；回环时才枚举网卡兜底。
func accessURL(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "" && !isLoopbackHost(host) {
		return "http://" + r.Host
	}
	if ip := outboundIP(); ip != "" {
		port := "3012"
		if _, p, err := net.SplitHostPort(r.Host); err == nil && p != "" {
			port = p
		}
		return "http://" + net.JoinHostPort(ip, port)
	}
	return ""
}

// isLoopbackHost 判断 host 是否为回环地址（含 "localhost" 与 IPv6 回环）。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// outboundIP 找一个本机局域网 IPv4 地址。
//
// ⚠️ 为什么不直接枚举网卡：Go 的 net.Interfaces() 在 Linux 上走 netlink socket
// （interface_linux.go 的 syscall.NetlinkRIB），而 **Android 的 SELinux 禁止普通
// 应用域创建 netlink socket**——在手机上调用它只会拿到权限错误，枚举出 0 个网卡
// （本项目的安卓版实测如此：同一个二进制在手机上返回空，在桌面上正常）。
//
// 因此首选「UDP dial 取本地出口地址」这一经典技巧：向一个外部地址 connect 一个
// UDP socket，connect 不发任何数据包，只让内核按路由表选定出口，再读 LocalAddr。
// 它只用普通 socket（untrusted_app 允许），桌面与手机都能用。
//
// 拿不到默认路由时（纯内网、飞行模式）退回枚举网卡——桌面 Linux/Windows 走这条，
// 手机在特定网络下也可能走到，能取到就取到。
func outboundIP() string {
	if ip := dialOutboundIP(); ip != "" {
		return ip
	}
	return ifaceIPv4()
}

// dialOutboundIP 用 UDP connect 探测出口地址。
// 目标地址只需「能匹配到默认路由」，不要求可达（UDP connect 不发包）。
// 依次尝试多个目标：公网 IP 走默认路由；全都失败时试一下局域网网关段。
func dialOutboundIP() string {
	for _, target := range []string{"8.8.8.8:53", "1.1.1.1:53", "114.114.114.114:53"} {
		conn, err := net.Dial("udp", target)
		if err != nil {
			continue
		}
		addr, ok := conn.LocalAddr().(*net.UDPAddr)
		conn.Close()
		if !ok || addr.IP == nil {
			continue
		}
		ip4 := addr.IP.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsUnspecified() || ip4.IsLinkLocalUnicast() {
			continue
		}
		return ip4.String()
	}
	return ""
}

// ifaceIPv4 枚举网卡挑一个局域网 IPv4（桌面环境的主要路径；Android 上通常返回空）。
//
// 优先级：无线网卡（wlan/wl/en0）→ 其它非虚拟网卡 → 第一个可用地址。
// 跳过虚拟/隧道网卡：它们的地址对外不可达，复制出去会让人连不上。
func ifaceIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var fallback string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "virbr") ||
			strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "tap") ||
			strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "zt") ||
			strings.HasPrefix(name, "lo") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			addr := ip4.String()
			if strings.HasPrefix(name, "wlan") || strings.HasPrefix(name, "wl") ||
				strings.HasPrefix(name, "en0") || strings.HasPrefix(name, "wifi") {
				return addr
			}
			if fallback == "" {
				fallback = addr
			}
		}
	}
	return fallback
}
