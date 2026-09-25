package main

import (
	"net"
	"reflect"
	"testing"
)

func TestFilterLANAddrs(t *testing.T) {
	upBcast := net.FlagUp | net.FlagBroadcast
	down := net.FlagBroadcast
	upLoop := net.FlagUp | net.FlagLoopback
	upP2P := net.FlagUp | net.FlagPointToPoint

	cases := []struct {
		name   string
		ifaces []ifaceInfo
		want   []string
	}{
		{
			name: "常见的家用 Wi-Fi（物理网卡，中文名）",
			ifaces: []ifaceInfo{
				{Name: "WLAN", Flags: upBcast, Addrs: []string{"192.168.1.7"}},
			},
			want: []string{"192.168.1.7"},
		},
		{
			name: "跳过回环 / 未启用 / 点对点",
			ifaces: []ifaceInfo{
				{Name: "Loopback Pseudo-Interface 1", Flags: upLoop, Addrs: []string{"127.0.0.1"}},
				{Name: "以太网 2", Flags: down, Addrs: []string{"192.168.1.9"}},
				{Name: "是一个点对点网卡", Flags: upP2P, Addrs: []string{"10.1.1.1"}},
			},
			want: []string{},
		},
		{
			name: "跳过 VMware / Hyper-V / WSL 虚拟网卡",
			ifaces: []ifaceInfo{
				{Name: "VMware Network Adapter VMnet8", Flags: upBcast, Addrs: []string{"192.168.233.1"}},
				{Name: "vEthernet (Default Switch)", Flags: upBcast, Addrs: []string{"172.20.240.1"}},
				{Name: "vEthernet (WSL)", Flags: upBcast, Addrs: []string{"172.29.48.1"}},
				{Name: "以太网", Flags: upBcast, Addrs: []string{"192.168.31.20"}},
			},
			want: []string{"192.168.31.20"},
		},
		{
			name: "跳过「up 但没有广播/多播」的伪网卡（本机实测的 NodeBabyLink 就是这种）",
			ifaces: []ifaceInfo{
				{Name: "NodeBabyLink", Flags: net.FlagUp | net.FlagRunning, Addrs: []string{"10.222.222.1"}},
				{Name: "WLAN", Flags: upBcast, Addrs: []string{"192.168.8.120"}},
			},
			want: []string{"192.168.8.120"},
		},
		{
			name: "跳过 169.254 链路本地与 0.0.0.0",
			ifaces: []ifaceInfo{
				{Name: "以太网", Flags: upBcast, Addrs: []string{"169.254.13.7", "0.0.0.0"}},
			},
			want: []string{},
		},
		{
			name: "跳过 IPv6",
			ifaces: []ifaceInfo{
				{Name: "以太网", Flags: upBcast, Addrs: []string{"fe80::1", "2001:db8::1", "10.0.0.5"}},
			},
			want: []string{"10.0.0.5"},
		},
		{
			name: "私网排在公网前面",
			ifaces: []ifaceInfo{
				{Name: "以太网", Flags: upBcast, Addrs: []string{"8.8.8.8"}},
				{Name: "WLAN", Flags: upBcast, Addrs: []string{"10.2.3.4"}},
			},
			want: []string{"10.2.3.4", "8.8.8.8"},
		},
		{
			name: "跨网卡去重",
			ifaces: []ifaceInfo{
				{Name: "以太网", Flags: upBcast, Addrs: []string{"192.168.1.7"}},
				{Name: "WLAN", Flags: upBcast, Addrs: []string{"192.168.1.7"}},
			},
			want: []string{"192.168.1.7"},
		},
		{
			name: "172.16/12 边界",
			ifaces: []ifaceInfo{
				{Name: "以太网", Flags: upBcast, Addrs: []string{"172.15.0.1", "172.16.0.1", "172.31.255.254", "172.32.0.1"}},
			},
			want: []string{"172.16.0.1", "172.31.255.254", "172.15.0.1", "172.32.0.1"},
		},
	}

	for _, c := range cases {
		got := filterLANAddrs(c.ifaces)
		if got == nil {
			got = []string{}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsVirtualIface(t *testing.T) {
	for _, n := range []string{"VMware Network Adapter VMnet1", "vEthernet (WSL)", "Tailscale", "tun0"} {
		if !isVirtualIface(n) {
			t.Errorf("%s 应该被判定为虚拟网卡", n)
		}
	}
	for _, n := range []string{"以太网", "WLAN", "Ethernet", "Wi-Fi"} {
		if isVirtualIface(n) {
			t.Errorf("%s 不应该被判定为虚拟网卡", n)
		}
	}
}

// outboundIPv4 依赖真实网络环境，只做"不炸 + 结果合理"的弱断言。
func TestOutboundIPv4(t *testing.T) {
	ip := outboundIPv4()
	if ip == "" {
		t.Skip("当前环境问不出默认出口地址，跳过")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		t.Fatalf("outboundIPv4 返回了非 IPv4: %q", ip)
	}
	if parsed.IsLoopback() {
		t.Fatalf("outboundIPv4 不该返回回环地址: %q", ip)
	}
	if !isPrivateIPv4(parsed) {
		t.Fatalf("outboundIPv4 只接受私网地址，返回了 %q", ip)
	}
}
