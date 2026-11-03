package main

// 首次配置向导（-setup）：问超管数量 → 逐个问账号/密码（不回显）→ 写 users.json。
//
// 为什么做成程序内向导，而不是一串命令行参数：
//   · 密码不该出现在命令行（会进 shell 历史 / 进程列表）
//   · Windows 的 start.bat、Linux/macOS 的 start.sh 都能复用同一套问答
//   · 生成的 users.json 放在数据库同目录，主程序启动时**自动发现并启用登录**，
//     这样日常启动完全不需要任何认证相关参数

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// defaultUsersPath 认证文件的默认位置：数据库同目录下的 users.json。
func defaultUsersPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "users.json")
}

// runSetup 执行向导，返回进程退出码。
func runSetup(path string) int {
	fmt.Println("=== 超管设置 ===")
	fmt.Println("留空数量 = 不启用登录（仅适合受信任局域网）")

	nStr := strings.TrimSpace(os.Getenv("MEDREVIEW_ADMINS"))
	if nStr == "" {
		fmt.Print("超管数量（0 = 不启用登录）: ")
		s, err := readLine()
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取失败:", err)
			return 1
		}
		nStr = strings.TrimSpace(s)
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 0 || n > 20 {
		fmt.Fprintln(os.Stderr, "数量需为 0-20 的整数")
		return 1
	}

	if n == 0 {
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("已存在 %s；如需彻底关闭登录，请手动删除该文件。\n", path)
		} else {
			fmt.Println("未启用登录。")
		}
		return 0
	}

	users := map[string]string{}
	admins := map[string]bool{}
	for i := 1; i <= n; i++ {
		var name string
		for {
			fmt.Printf("超管 %d/%d 账号: ", i, n)
			s, err := readLine()
			if err != nil {
				fmt.Fprintln(os.Stderr, "读取失败:", err)
				return 1
			}
			name = strings.TrimSpace(s)
			if name == "" {
				fmt.Println("账号不能为空")
				continue
			}
			if !validUserName(name) {
				fmt.Println("账号需 1-32 个字符，不能含冒号或斜杠")
				continue
			}
			if _, exists := users[name]; exists {
				fmt.Println("该账号已输入过")
				continue
			}
			break
		}
		var pw string
		for {
			p1, err := promptPassword("密码（至少 8 位，输入不回显）: ")
			if err != nil {
				return 1
			}
			if len([]rune(p1)) < 8 {
				fmt.Println("密码至少 8 位")
				continue
			}
			p2, err := promptPassword("再输一次: ")
			if err != nil {
				return 1
			}
			if p1 != p2 {
				fmt.Println("两次输入不一致")
				continue
			}
			pw = p1
			break
		}
		h, err := bcrypt.GenerateFromPassword([]byte(pw), 10)
		if err != nil {
			fmt.Fprintln(os.Stderr, "生成哈希失败:", err)
			return 1
		}
		users[name] = string(h)
		admins[name] = true
	}

	if err := persistUsers(path, users, admins); err != nil {
		fmt.Fprintln(os.Stderr, "写入失败:", err)
		return 1
	}
	fmt.Printf("\n已写入 %s（%d 个超管）\n", path, len(users))
	fmt.Println("以后启动会自动启用登录；要改密码请登录后在仪表盘里改，或重新运行 -setup。")
	return 0
}
