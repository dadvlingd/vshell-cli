package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseINI(t *testing.T) {
	src := "\ufeff# 注释\n; 另一种注释\nurl = http://127.0.0.1:8082\n\n[server]\nusername = admin\npassword = \"p # 1 = x\"\n"
	m, err := parseINI(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if m["url"] != "http://127.0.0.1:8082" {
		t.Fatalf("url 解析异常: %q", m["url"])
	}
	if m["server.username"] != "admin" {
		t.Fatalf("username 解析异常: %q", m["server.username"])
	}
	if m["server.password"] != "p # 1 = x" {
		t.Fatalf("password 解析异常: %q", m["server.password"])
	}

	if _, err := parseINI(strings.NewReader("bad line")); err == nil {
		t.Fatal("缺少 = 的行应报错")
	}
	if _, err := parseINI(strings.NewReader("[vshell")); err == nil {
		t.Fatal("未闭合的节应报错")
	}
}

func TestApplyConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	data := "[vshell]\nurl = http://cfg\nusername = cfguser\npassword = cfgpass\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	// 命令行/环境变量的值优先，配置文件只补空。
	cfg := &Config{ConfigFile: path, URL: "http://flag"}
	exists, err := applyConfigFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("配置文件应存在")
	}
	if cfg.URL != "http://flag" {
		t.Fatalf("URL 不应被覆盖: %q", cfg.URL)
	}
	if cfg.User != "cfguser" || cfg.Pass != "cfgpass" {
		t.Fatalf("配置文件字段未生效: %+v", cfg)
	}

	cfg2 := &Config{ConfigFile: path}
	if _, err := applyConfigFile(cfg2); err != nil {
		t.Fatal(err)
	}
	if cfg2.URL != "http://cfg" || cfg2.User != "cfguser" || cfg2.Pass != "cfgpass" {
		t.Fatalf("配置文件读取异常: %+v", cfg2)
	}
}

func TestReadConfigFileAliases(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"server", "[server]\nurl = http://s\nuser = u\npass = p\n"},
		{"no-section", "url = http://s\nusername = u\npassword = p\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.ini")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			fc, exists, err := readConfigFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !exists {
				t.Fatal("配置文件应存在")
			}
			if fc.URL != "http://s" || fc.User != "u" || fc.Pass != "p" {
				t.Fatalf("读取异常: %+v", fc)
			}
		})
	}
}

func TestEnsureConfigTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	created, err := ensureConfigTemplate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("应创建配置文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[vshell]", "url =", "username =", "password ="} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("模板缺少 %q:\n%s", want, data)
		}
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("模板权限应为 0600，实际 %04o", perm)
		}
	}

	if err := os.WriteFile(path, []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err = ensureConfigTemplate(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("已存在的文件不应被重新创建")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "custom" {
		t.Fatalf("不应覆盖已有配置: %q", data)
	}
}

func TestConfigFileFromGlobals(t *testing.T) {
	t.Setenv("VSHELL_CONFIG", "")

	cfg, _, err := parseGlobals([]string{"--config", "x.ini", "info"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigFile != "x.ini" {
		t.Fatalf("--config 解析异常: %q", cfg.ConfigFile)
	}

	cfg, _, err = parseGlobals([]string{"info"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigFile != defaultConfigFile {
		t.Fatalf("默认路径异常: %q", cfg.ConfigFile)
	}

	t.Setenv("VSHELL_CONFIG", "env.ini")
	cfg, _, err = parseGlobals([]string{"info"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigFile != "env.ini" {
		t.Fatalf("VSHELL_CONFIG 未生效: %q", cfg.ConfigFile)
	}

	cfg, _, err = parseGlobals([]string{"--config", "flag.ini", "info"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigFile != "flag.ini" {
		t.Fatalf("--config 应覆盖环境变量: %q", cfg.ConfigFile)
	}
}
