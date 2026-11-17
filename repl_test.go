package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunShellLinuxTracking(t *testing.T) {
	var mu sync.Mutex
	var seen []string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"result":{"token":"jwt"}}`))
	})
	mux.HandleFunc("/api/terminal/shell", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cmd, _ := body["command"].(string)
		mu.Lock()
		seen = append(seen, cmd)
		mu.Unlock()

		switch {
		case cmd == "pwd":
			_, _ = w.Write([]byte(`{"code":0,"result":"/root/linkexec"}`))
		case strings.Contains(cmd, "cd '..'"):
			_, _ = w.Write([]byte(`{"code":0,"result":"/root"}`))
		case strings.Contains(cmd, "pdw"):
			_, _ = w.Write([]byte(`{"code":-1,"message":"exit status 127"}`))
		case strings.Contains(cmd, "whoami"):
			_, _ = w.Write([]byte(`{"code":0,"result":"root"}`))
		case strings.Contains(cmd, "&& ls"):
			_, _ = w.Write([]byte(`{"code":0,"result":"file.txt"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"result":"OK"}`))
		}
	})
	mux.HandleFunc("/api/client/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"result":{"items":[
			{"Id":4,"HostName":"linux-a","IsConnect":true}
		],"total":1,"clientOnlineCount":1}}`))
	})
	mux.HandleFunc("/api/file/ls", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"result":{"items":[
			{"name":"scan.txt","size":12,"time":"2026-01-01 00:00:00","isDir":false}
		],"total":1}}`))
	})
	mux.HandleFunc("/api/dashboard/info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"result":{"version":"4.9.3","clientOnlineCount":1}}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(&Config{URL: srv.URL, User: "admin", Pass: "pw", HTTPTimeout: 5 * time.Second})
	if err := c.Login(); err != nil {
		t.Fatal(err)
	}

	input := strings.Join([]string{
		"whoami",
		"cd ..",
		"pwd",
		"ls",
		"pdw",
		":ls",
		":clients",
		":timeout 10",
		":raw on",
		":help",
		":exit",
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := runShell(c, 4, 5*time.Second, false, strings.NewReader(input), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	for _, want := range []string{
		"root",                   // whoami
		"/root",                  // cd .. 之后的 pwd
		"file.txt",               // 在 /root 下执行 ls
		"命令不存在（exit status 127）", // 退出码 127 的提示
		"scan.txt",               // :ls
		"linux-a",                // :clients
		"单条命令超时已设为 10s",          // :timeout
		"raw 模式: true",           // :raw
		"本地命令（以 : 开头",            // :help
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q，实际:\n%s", want, got)
		}
	}
	if strings.Contains(got, "agent-4>") {
		t.Errorf("非交互模式不应打印提示符:\n%s", got)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(seen, "\n")
	for _, want := range []string{
		"cd '/root/linkexec' && whoami",
		"cd '/root/linkexec' && cd '..' && pwd",
		"cd '/root' && ls",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("发送给 Agent 的命令缺少 %q，实际:\n%s", want, joined)
		}
	}
}

func TestRunShellWindowsTracking(t *testing.T) {
	var mu sync.Mutex
	var seen []string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"result":{"token":"jwt"}}`))
	})
	mux.HandleFunc("/api/terminal/shell", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cmd, _ := body["command"].(string)
		mu.Lock()
		seen = append(seen, cmd)
		mu.Unlock()

		switch {
		case cmd == "pwd":
			_, _ = w.Write([]byte(`{"code":-1,"message":"not recognized"}`))
		case cmd == "cd":
			_, _ = w.Write([]byte(`{"code":0,"result":"C:\\Users\\admin"}`))
		case strings.Contains(cmd, `cd /d ".."`):
			_, _ = w.Write([]byte(`{"code":0,"result":"C:\\Users"}`))
		case strings.Contains(cmd, "whoami"):
			_, _ = w.Write([]byte(`{"code":0,"result":"admin"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"result":"OK"}`))
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(&Config{URL: srv.URL, User: "admin", Pass: "pw", HTTPTimeout: 5 * time.Second})
	if err := c.Login(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	input := "whoami\ncd ..\npwd\n:exit\n"
	if err := runShell(c, 3, 5*time.Second, false, strings.NewReader(input), &out, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"admin", `C:\Users`} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q，实际:\n%s", want, got)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(seen, "\n")
	if !strings.Contains(joined, `cd /d "C:\Users\admin" && whoami`) {
		t.Errorf("未按跟踪目录包装 Windows 命令，实际:\n%s", joined)
	}
}
