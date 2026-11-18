package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPProtocol(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"no/such"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := serveMCP(&Config{}, nil, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("应返回 4 条响应，实际 %d:\n%s", len(lines), out.String())
	}

	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &init); err != nil {
		t.Fatal(err)
	}
	if init.Result.ProtocolVersion != "2025-03-26" {
		t.Fatalf("协议版本未回显: %q", init.Result.ProtocolVersion)
	}
	if init.Result.ServerInfo.Name != "vshell-cli" || init.Result.ServerInfo.Version != version {
		t.Fatalf("serverInfo 异常: %+v", init.Result.ServerInfo)
	}

	var list struct {
		Result struct {
			Tools []mcpTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Result.Tools) != 17 {
		t.Fatalf("工具数量应为 17，实际 %d", len(list.Result.Tools))
	}
	seen := map[string]bool{}
	for _, tool := range list.Result.Tools {
		if tool.Name == "" || tool.Description == "" {
			t.Fatalf("工具定义不完整: %+v", tool)
		}
		if seen[tool.Name] {
			t.Fatalf("工具名重复: %s", tool.Name)
		}
		seen[tool.Name] = true
		if tool.InputSchema["type"] != "object" {
			t.Fatalf("工具 schema 异常: %s", tool.Name)
		}
	}

	var unknown struct {
		Error *mcpError `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[3]), &unknown); err != nil {
		t.Fatal(err)
	}
	if unknown.Error == nil || unknown.Error.Code != -32601 {
		t.Fatalf("未知方法应返回 -32601: %s", lines[3])
	}
}

func TestMCPFraming(t *testing.T) {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := writeMCPMessage(bw, map[string]any{"ok": true}, true); err != nil {
		t.Fatal(err)
	}
	raw := buf.String()
	if !strings.HasPrefix(raw, "Content-Length: ") || !strings.Contains(raw, "\r\n\r\n{") {
		t.Fatalf("帧格式异常: %q", raw)
	}

	body, framed, err := readMCPMessage(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !framed {
		t.Fatal("应识别为 Content-Length 帧")
	}
	if !json.Valid(body) {
		t.Fatalf("帧体不是 JSON: %q", body)
	}
}

func TestMCPConfigSnippet(t *testing.T) {
	cfg := &Config{ConfigFile: "/tmp/demo-config.ini", Insecure: true}
	snippet, err := mcpConfigSnippet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(snippet), &doc); err != nil {
		t.Fatal(err)
	}
	srv, ok := doc.MCPServers["vshell-cli"]
	if !ok {
		t.Fatalf("缺少 vshell-cli 配置: %s", snippet)
	}
	if srv.Command == "" {
		t.Fatal("command 为空")
	}
	if strings.Join(srv.Args, " ") != "--insecure --config /tmp/demo-config.ini mcp" {
		t.Fatalf("args 异常: %v", srv.Args)
	}
}

func TestMCPToolExec(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","result":{"token":"jwt-mcp"}}`))
	})
	mux.HandleFunc("/api/terminal/shell", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["id"] != "3" || body["command"] != "whoami" {
			_, _ = w.Write([]byte(`{"code":-1,"message":"bad request"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","result":"\r\nC:\\>whoami\r\nWIN-TEST\\demo\r\nC:\\>"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &Config{URL: srv.URL, User: "admin", Pass: "pw", HTTPTimeout: 5e9}
	s := &mcpServer{cfg: cfg, c: NewClient(cfg)}
	v, err := s.toolExec(map[string]any{"client_id": float64(3), "command": "whoami"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := v.(map[string]any)["output"].(string)
	if !strings.Contains(out, `WIN-TEST\demo`) {
		t.Fatalf("输出异常: %q", out)
	}
}

func TestMCPToolInfo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","result":{"token":"jwt-mcp"}}`))
	})
	mux.HandleFunc("/api/dashboard/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Token") != "jwt-mcp" {
			_, _ = w.Write([]byte(`{"code":-1,"message":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","result":{"version":"9.9.9","clientCount":2,"clientOnlineCount":1}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &Config{URL: srv.URL, User: "admin", Pass: "pw", HTTPTimeout: 5e9}
	c := NewClient(cfg)
	s := &mcpServer{cfg: cfg, c: c}
	v, err := s.toolInfo()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("返回值类型异常: %T", v)
	}
	d, ok := m["dashboard"].(*Dashboard)
	if !ok || d.Version != "9.9.9" {
		t.Fatalf("dashboard 异常: %+v", m["dashboard"])
	}
}
