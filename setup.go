package main

// 首次配置向导（-setup）：问超管数量 → 逐个问账号/密码（不回显）→ 写 users.json。
//
// 为什么做成程序内向导，而不是一串命令行参数：
//   · 密码不该出现在命令行（会进 shell 历史 / 进程列表）
//   · Windows 的 start.bat、Linux/macOS 的 start.sh 都能复用同一套问答
//   · 生成的 users.json 放在数据库同目录，主程序启动时**自动发现并启用登录**，
//     这样日常启动完全不需要任何认证相关参数

import (
	"encoding/json"
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
func runSetup(path string, ephemeral bool) int {
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
		return setupDisableLogin(path)
	}

	// 合并而不是重写：启动器每次启动都会问一遍数量，如果这里直接覆盖，
	// 上一轮在仪表盘里加的用户、改过的密码会在下次启动时被悄悄抹掉。
	// 现在的语义是"输入的账号新增或覆盖同名的，其余原样保留"。
	users := map[string]string{}
	admins := map[string]bool{}
	if fileExists(path) {
		oldUsers, oldAdmins, rerr := readUsersDoc(path)
		if rerr != nil {
			// 文件存在但读不动（半截写入、手改坏）：宁可拒绝，也不覆盖
			fmt.Fprintf(os.Stderr, "已有 %s 但无法解析：%v\n", path, rerr)
			fmt.Fprintln(os.Stderr, "为避免抹掉看不清的内容，本次不修改。请先备份并修好（或删掉）该文件。")
			return 1
		}
		for k, v := range oldUsers {
			users[k] = v
		}
		for k := range oldAdmins {
			admins[k] = true
		}
		if len(users) > 0 {
			fmt.Printf("已存在 %s（%d 个账号），将保留；本次输入的账号会覆盖同名项。\n", path, len(users))
		}
	}
	before := len(users)
	// entered 只记"这一轮输过谁"。不能直接查 users —— 合并模式下 users 里本来就有
	// 旧账号，那样会让"给老账号换密码"被误判成"已输入过"，永远改不了。
	entered := map[string]bool{}
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
			if entered[name] {
				fmt.Println("该账号已输入过")
				continue
			}
			entered[name] = true
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

	if err := persistUsers(path, users, admins, ephemeral); err != nil {
		fmt.Fprintln(os.Stderr, "写入失败:", err)
		return 1
	}
	fmt.Printf("\n已写入 %s（共 %d 个账号，本次新增 %d）\n", path, len(users), len(users)-before)
	if ephemeral {
		fmt.Println("本次运行有效：正常退出时会自动删除该文件，下次启动需要重新配置。")
	} else {
		fmt.Println("已持久化：以后启动会自动启用登录；要改密码请登录后在仪表盘里改。")
	}
	return 0
}

// setupDisableLogin 处理「数量填 0」= 关闭登录。
//
// 这里原来是无条件 os.Remove —— 输一个 0（手滑、或者以为只是"这次不要超管"）
// 就把持久化的团队账号连同密码全删了，而且没有任何提示，属于不可逆的静默数据丢失。
// 现在：
//   - 没有文件 → 直接说"未启用登录"，什么都不做；
//   - 有文件 → 先报出里面有几个账号，要求明确回答 y 才删；
//   - 读不到/解析不了 → 拒绝动手（宁可不关，也不能删掉看不懂的东西）；
//   - 非交互（管道/服务，读不到回答）→ 默认保留，并说明怎么真正关闭。
func setupDisableLogin(path string) int {
	if !fileExists(path) {
		fmt.Println("未启用登录。")
		return 0
	}
	users, _, err := readUsersDoc(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告：%s 存在但无法解析（%v），未做任何修改。\n", path, err)
		return 1
	}
	fmt.Printf("已存在 %s（%d 个账号）：删除它等于永久丢弃这些账号与密码。\n", path, len(users))
	fmt.Print("确认删除并关闭登录？[y/N]: ")
	s, rerr := readLine()
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "！当前不是交互终端，无法确认，已保留原账号文件（下次启动仍会要求登录）。")
		fmt.Fprintln(os.Stderr, "  确实要关闭登录：删掉上面这个文件，再启动。")
		return 0
	}
	if !isYes(s) {
		fmt.Println("已保留原账号文件：下次启动仍会启用登录。")
		return 0
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "删除失败:", err)
		return 1
	}
	fmt.Printf("已删除 %s，未启用登录。\n", path)
	return 0
}

// readUsersDoc 读 users.json 里的账号与超管名单（不做 bcrypt 校验，
// 那是 loadAuthConfig 的职责；这里只关心"有哪些人"）。
func readUsersDoc(path string) (map[string]string, map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var doc struct {
		Users  map[string]string `json:"users"`
		Admins []string          `json:"admins"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Users == nil {
		doc.Users = map[string]string{}
	}
	admins := map[string]bool{}
	for _, n := range doc.Admins {
		if _, ok := doc.Users[n]; ok {
			admins[n] = true
		}
	}
	return doc.Users, admins, nil
}

// isYes 认 y / yes / 是 / 确认（大小写不敏感）。
func isYes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "是", "确认", "确定":
		return true
	}
	return false
}
