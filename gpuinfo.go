package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// gpuClassPath 显示适配器的设备类（控制面板「设备管理器-显示适配器」就是读这个键）。
const gpuClassPath = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`

// gpuCard 一块显卡的识别结果。
type gpuCard struct {
	Desc string // 注册表里的原名
	Kind string // "dgpu-nv" / "dgpu-amd" / "igpu" / "skip"
}

// classifyGPU 按显卡名判断它是什么卡。认不出来或属于虚拟显示设备的，一律 skip
// （那些"远程桌面/虚拟显示器"适配器当成独显会让用户以为有硬件加速可用）。
func classifyGPU(desc string) string {
	d := strings.ToLower(desc)
	switch {
	case strings.Contains(d, "virtual"), strings.Contains(d, "remote"),
		strings.Contains(d, "indirect"), strings.Contains(d, "idd"):
		return "skip" // 虚拟显示适配器 / 间接显示设备
	case strings.Contains(d, "nvidia"), strings.Contains(d, "geforce"),
		strings.Contains(d, "quadro"), strings.Contains(d, "tesla"):
		return "dgpu-nv"
	case strings.Contains(d, "radeon"), strings.Contains(d, "amd"):
		return "dgpu-amd"
	case strings.Contains(d, "intel"):
		return "igpu"
	case strings.Contains(d, "microsoft"):
		return "skip" // 基本显示适配器（没装驱动）
	}
	return "skip"
}

// gpuSumFileName 探测结果文件名（放 %TEMP%）。start.bat 读它，不靠 shell 管道/重定向。
const gpuSumFileName = "medreview-gpu-sum.txt"

// printGPUInfo 只探测列显卡，打完就退出（-gpuinfo）。给 start.bat 启动时问显卡用。
//
// 为什么不用 PowerShell / wmic：实测在 bat 环境里
// `powershell -NoProfile -Command "Get-CimInstance Win32_VideoController ..." > 文件`
// 会**返回码 0、目标文件被创建但是 0 字节、stderr 也是空的** —— 静默失败。
// 注册表这条路是纯进程内读取，毫秒级，不受 shell/管道/安全策略影响。
//
// ⚠️ 为什么结果写文件而不是让 bat 捕获 stdout（三个坑都实测过）：
//  1. `"exe" -gpuinfo > 文件` 在带 set /p 交互的 bat 里会**返回码 0、文件 0 字节**地静默失败；
//  2. `for /f ... in (`反引号命令`)` 捕获同样不稳（会冒出"系统找不到文件"再把输出拼在后面）；
//  3. Go 往管道写数据不转代码页（UTF-8），cmd 按 GBK 解码 → 中文全乱码；
//     顺带 cmd 只把 CRLF 当换行，只给 LF 的话整个输出会被当成一整行。
// 所以：**exe 自己写文件（Go 写文件绝对可靠），bat 只用 for /f 读文件**，全程不经过管道。
func printGPUInfo() {
	hasDGPU, hasIGPU := false, false
	denc, ddec := "", ""
	for _, n := range readGPUDescriptions() {
		switch classifyGPU(n) {
		case "dgpu-nv":
			hasDGPU, denc, ddec = true, "nvenc", "cuda"
		case "dgpu-amd":
			hasDGPU, denc, ddec = true, "amf", "d3d11va"
		case "igpu":
			hasIGPU = true
		}
	}
	sum := fmt.Sprintf("DGPU=%d;DENC=%s;DDEC=%s;IGPU=%d",
		b2i(hasDGPU), denc, ddec, b2i(hasIGPU))
	if dir := os.Getenv("TEMP"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, gpuSumFileName), []byte(sum+"\r\n"), 0o644); err != nil {
			log.Printf("写显卡探测结果失败（start.bat 会当没有独显核显）: %v", err)
		}
	}
	fmt.Println(sum)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// readGPUDescriptions 读显示适配器类下所有子键的 DriverDesc（0000…0009 那种编号键）。
// 读不到就返回空 —— 调用方按"没有独显核显"处理，不报错。
func readGPUDescriptions() []string {
	var out []string
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, gpuClassPath,
		registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return out
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return out
	}
	for _, sub := range names {
		k, err := registry.OpenKey(root, sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		desc, _, err := k.GetStringValue("DriverDesc")
		k.Close()
		if err == nil && strings.TrimSpace(desc) != "" {
			out = append(out, strings.TrimSpace(desc))
		}
	}
	return out
}
