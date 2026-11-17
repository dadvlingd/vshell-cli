package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSplitCurlResponse(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 3\r\n\r\nabc"
	hdrs, body := splitCurlResponse(raw)
	if body != "abc" {
		t.Errorf("响应体 = %q, 期望 %q", body, "abc")
	}
	if hdrs["Content-Type"] != "text/plain" {
		t.Errorf("Content-Type = %q", hdrs["Content-Type"])
	}
	if hdrs["HTTP/1.1 200 OK"] != "" {
		t.Errorf("不应把状态行当请求头")
	}

	hdrs, body = splitCurlResponse("no headers here")
	if body != "no headers here" || len(hdrs) != 0 {
		t.Errorf("无头部时应原样返回: body=%q hdrs=%v", body, hdrs)
	}
}

func TestSplitRemotePath(t *testing.T) {
	cases := []struct {
		in      string
		dir     string
		name    string
		wantErr bool
	}{
		{"C:/scan.txt", "C:/", "scan.txt", false},
		{"C:/Users/a/b.txt", "C:/Users/a", "b.txt", false},
		{"C:\\Users\\a\\b.txt", "C:/Users/a", "b.txt", false},
		{"/tmp/x", "/tmp", "x", false},
		{"C:/", "", "", true},
		{"scan.txt", "", "", true},
	}
	for _, c := range cases {
		dir, name, err := splitRemotePath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: 期望报错，实际 dir=%q name=%q", c.in, dir, name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: 意外错误: %v", c.in, err)
			continue
		}
		if dir != c.dir || name != c.name {
			t.Errorf("%q: 得到 (%q,%q)，期望 (%q,%q)", c.in, dir, name, c.dir, c.name)
		}
	}
}

func TestExtractShellOutput(t *testing.T) {
	raw := "Microsoft Windows [Version 10.0]\r\nC:\\Users\\a>whoami\r\nwin-a\\admin\r\n\r\nC:\\Users\\a>"
	got := extractShellOutput(raw, "whoami")
	if !strings.Contains(got, "win-a\\admin") {
		t.Fatalf("未提取到命令输出: %q", got)
	}
	if strings.Contains(got, "C:\\Users\\a>") {
		t.Fatalf("输出中残留提示符: %q", got)
	}

	if got := extractShellOutput("", "x"); got != "(无输出)" {
		t.Fatalf("空输入应返回 (无输出)，实际 %q", got)
	}
}

func TestParseGlobals(t *testing.T) {
	t.Setenv("VSHELL_URL", "")
	t.Setenv("VSHELL_USER", "")
	t.Setenv("VSHELL_PASS", "")

	cfg, rest, err := parseGlobals([]string{"--url", "http://x:1", "--user", "u", "--json", "exec", "3", "whoami"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "http://x:1" || cfg.User != "u" || !cfg.JSON {
		t.Fatalf("解析结果异常: %+v", cfg)
	}
	if strings.Join(rest, " ") != "exec 3 whoami" {
		t.Fatalf("剩余参数异常: %v", rest)
	}

	if _, _, err := parseGlobals([]string{"--url=http://y:2", "clients"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseGlobals([]string{"--bogus", "clients"}); err == nil {
		t.Fatal("未知全局参数应报错")
	}
}

func TestLoginCatUpload(t *testing.T) {
	var gotAuth, gotToken string
	mux := http.NewServeMux()

	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["username"] != "admin" || body["password"] != "pw" {
			_, _ = w.Write([]byte(`{"code":-1,"message":"bad credentials"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","result":{"token":"jwt-123"}}`))
	})

	mux.HandleFunc("/api/file/cat", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("Token")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["path"] != "C:/" || body["target"] != "scan.txt" {
			_, _ = w.Write([]byte(`{"code":-1,"message":"bad path"}`))
			return
		}
		content := base64.StdEncoding.EncodeToString([]byte("hello"))
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","result":{"content":"` + content + `"}}`))
	})

	mux.HandleFunc("/api/client/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"result":{"items":[{"Id":3,"HostName":"win-a","LocalIP":"10.0.0.5","Addr":"10.0.0.5:5000","OsName":"Windows","IsConnect":true}],"total":1,"clientOnlineCount":1}}`))
	})

	mux.HandleFunc("/api/file/upload", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart 解析失败: %v", err)
		}
		if r.FormValue("id") != "3" {
			t.Errorf("id 字段异常: %q", r.FormValue("id"))
		}
		if r.FormValue("path") != "C:/up.txt" {
			t.Errorf("path 字段异常: %q", r.FormValue("path"))
		}
		if r.Header.Get("ignoreCancelToken") != "true" {
			t.Errorf("缺少 ignoreCancelToken 头")
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("缺少文件字段: %v", err)
		} else {
			defer f.Close()
			b, _ := io.ReadAll(f)
			if string(b) != "payload" {
				t.Errorf("文件内容异常: %q", b)
			}
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","url":"C:/up.txt"}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(&Config{URL: srv.URL, User: "admin", Pass: "pw", HTTPTimeout: 5 * time.Second})
	if err := c.Login(); err != nil {
		t.Fatal(err)
	}
	if c.Token != "jwt-123" {
		t.Fatalf("token=%q", c.Token)
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:pw"))
	if c.Basic != wantBasic {
		t.Fatalf("basic=%q", c.Basic)
	}

	data, err := c.ReadFile(3, "C:/scan.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("读文件内容=%q", data)
	}
	if gotAuth != wantBasic || gotToken != "jwt-123" {
		t.Fatalf("请求头未带双认证: auth=%q token=%q", gotAuth, gotToken)
	}

	list, err := c.Agents(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].HostName != "win-a" {
		t.Fatalf("agent list 异常: %+v", list)
	}

	dir := t.TempDir()
	path := dir + "/up.txt"
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Upload(3, path, "C:/up.txt", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK() || resp.URL != "C:/up.txt" {
		t.Fatalf("upload 响应异常: %+v", resp)
	}
}

func TestSocks5DialAndTunnelHTTP(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok-through-tunnel"))
	}))
	defer target.Close()

	proxyAddr := startFakeSocks(t)

	conn, err := dialSOCKS5(proxyAddr, strings.TrimPrefix(target.URL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	c := NewClient(&Config{HTTPTimeout: 3 * time.Second})
	port := proxyAddr[strings.LastIndex(proxyAddr, ":")+1:]
	p, _ := strconv.Atoi(port)
	res, err := c.TunnelHTTP(p, "GET", target.URL, nil, "", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || !strings.Contains(res.Body, "ok-through-tunnel") {
		t.Fatalf("隧道请求异常: %+v", res)
	}
}

// ---- 测试辅助 ----

func startFakeSocks(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go handleFakeSocks(conn)
		}
	}()
	return l.Addr().String()
}

func handleFakeSocks(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	methods := make([]byte, int(buf[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	_, _ = conn.Write([]byte{0x05, 0x00})

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	var host string
	switch head[3] {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = string(b)
	default:
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(conn, pb); err != nil {
		return
	}
	port := int(pb[0])<<8 | int(pb[1])

	up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	go func() {
		_, _ = io.Copy(up, conn)
	}()
	_, _ = io.Copy(conn, up)
}
