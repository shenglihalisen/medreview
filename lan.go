package main

import (
	"net"
	"strings"
	"sync"
	"time"
)

// 虚拟网卡名字黑名单。
// Windows 上 VMware / Hyper-V / WSL / VPN 的虚拟网卡既是 UP 状态、又常常是私网地址，
// 不按名字过滤就会把这些"看着像内网、实际连不通"的地址打给用户。
// 反过来不能用白名单：中文 Windows 的物理网卡叫「以太网」「WLAN」，
// 拿 ethernet / wlan 去匹配只会全部落空。
var virtualIfaceKeys = []string{
	"vmware", "virtualbox", "vethernet", "hyper-v", "wsl", "docker",
	"tailscale", "zerotier", "radmin", "loopback", "tap-", "tun",
	"bluetooth", "vpn", "openvpn", "wireguard", "npcap", "teredo",
}

func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, k := range virtualIfaceKeys {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// isPrivateIPv4 判断 RFC1918 私网地址（10/8、172.16/12、192.168/16）。
func isPrivateIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 10:
		return true
	case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
		return true
	case v4[0] == 192 && v4[1] == 168:
		return true
	}
	return false
}

// ifaceInfo 是 filterLANAddrs 的输入。抽成纯数据是为了让过滤逻辑可测 ——
// 在测试里伪造网卡，比在本机真造一块 VMware 网卡现实得多。
type ifaceInfo struct {
	Name  string
	Flags net.Flags
	Addrs []string
}

// filterLANAddrs 从网卡列表里挑出"手机该用的那个地址"。
// 私网地址排在前面（家里 Wi-Fi 通常是 192.168.x.x，公司网络可能是 10.x）。
func filterLANAddrs(ifaces []ifaceInfo) []string {
	var priv, other []string
	seen := map[string]bool{}
	for _, f := range ifaces {
		if f.Flags&net.FlagUp == 0 || f.Flags&net.FlagLoopback != 0 || f.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		// 真正的局域网网卡一定是广播或多播的（以太网/Wi-Fi 都是）。
		// 本机实测有一块 up|running 但既无 broadcast 也无 multicast 的 "NodeBabyLink"（10.222.222.1/32），
		// 这种点对点虚拟链路手机根本连不上，靠这一条能干净滤掉。
		if f.Flags&(net.FlagBroadcast|net.FlagMulticast) == 0 {
			continue
		}
		if isVirtualIface(f.Name) {
			continue
		}
		for _, s := range f.Addrs {
			ip := net.ParseIP(s)
			if ip == nil || ip.To4() == nil {
				continue // IPv6 先不管：让用户在手机上敲 http://[fe80::...] 太折腾
			}
			// IsLinkLocalUnicast 同时覆盖 169.254/16 与 fe80::/10
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			host := ip.String()
			if seen[host] {
				continue
			}
			seen[host] = true
			if isPrivateIPv4(ip) {
				priv = append(priv, host)
			} else {
				other = append(other, host)
			}
		}
	}
	return append(priv, other...)
}

// outboundIPv4 问一句"本机走哪块网卡出网"，拿到那块网卡的地址。
// 用 UDP Dial 而不是真发包：UDP 无连接，Dial 只做一次路由选择，不发任何数据。
// 这个地址最可能就是手机连得通的那个（手机和电脑在同一个 Wi-Fi = 同一个出口）。
func outboundIPv4() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	a, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || a.IP == nil {
		return ""
	}
	ip := a.IP.To4()
	if ip == nil || ip.IsLoopback() || !isPrivateIPv4(ip) {
		return ""
	}
	return ip.String()
}

// lanIPv4s 读本机真实网卡并过滤出可用的局域网地址。
// 出口地址排最前面 —— 启动 banner 里「下载页」用的是第一个地址，
// 排在前面才能保证那一条是手机真能打开的。
func lanIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	list := make([]ifaceInfo, 0, len(ifaces))
	for _, in := range ifaces {
		as, err := in.Addrs()
		if err != nil {
			continue
		}
		addrs := make([]string, 0, len(as))
		for _, a := range as {
			switch v := a.(type) {
			case *net.IPNet:
				addrs = append(addrs, v.IP.String())
			case *net.IPAddr:
				addrs = append(addrs, v.IP.String())
			}
		}
		list = append(list, ifaceInfo{Name: in.Name, Flags: in.Flags, Addrs: addrs})
	}
	// 每次都重新枚举网卡 + 做一次 UDP 路由探测太重（进程启动那次除外）：
	// 每个 HTTP 请求的 CORS 来源判断都要问"这是不是本机的局域网地址"，
	// 裸调会把它变成每次请求一次网卡枚举 + 一次 dial。统一走 cachedLANIPv4s。
	out := filterLANAddrs(list)
	if ip := outboundIPv4(); ip != "" {
		head := []string{ip}
		for _, x := range out {
			if x != ip {
				head = append(head, x)
			}
		}
		return head
	}
	return out
}

var (
	lanMu      sync.Mutex
	lanCache   []string
	lanCacheAt time.Time
	lanFilled  bool
)

// cachedLANIPv4s 缓存 lanIPv4s 的结果 60 秒。
// 请求路径上（CORS 来源判断）只准调这个，不准直接调 lanIPv4s。
// 没网卡时结果可能是 nil，用 lanFilled 区分"确实没有"和"还没算过"，
// 否则会退化成每个请求都重算一遍。
func cachedLANIPv4s() []string {
	lanMu.Lock()
	defer lanMu.Unlock()
	if lanFilled && time.Since(lanCacheAt) < 60*time.Second {
		return lanCache
	}
	lanCache = lanIPv4s()
	lanCacheAt = time.Now()
	lanFilled = true
	return lanCache
}
