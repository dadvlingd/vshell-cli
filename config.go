package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const defaultConfigFile = "config.ini"

const configTemplate = `# vshell-cli 配置文件
# 填入面板地址、用户名和密码后保存，下次运行会自动读取。
# 也可以继续用 --url/--user/--password 或 VSHELL_URL/VSHELL_USER/VSHELL_PASS 覆盖。
# 注意：密码以明文保存，建议执行 chmod 600 config.ini。

[vshell]
url =
username =
password =
`

type fileConfig struct {
	URL  string
	User string
	Pass string
}

// parseINI 解析 key = value 形式，支持 [section] 与整行 # / ; 注释。
// 返回值使用 "section.key" 作为键；没有节时直接使用 "key"。
func parseINI(r io.Reader) (map[string]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	out := make(map[string]string)
	section := ""
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("第 %d 行: 节缺少 ]", lineNo)
			}
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("第 %d 行: 缺少 =", lineNo)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return nil, fmt.Errorf("第 %d 行: 键为空", lineNo)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if section != "" {
			key = section + "." + key
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func iniValue(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

func readConfigFile(path string) (fileConfig, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileConfig{}, false, nil
	}
	if err != nil {
		return fileConfig{}, false, err
	}
	defer f.Close()

	m, err := parseINI(f)
	if err != nil {
		return fileConfig{}, true, err
	}
	return fileConfig{
		URL:  iniValue(m, "vshell.url", "server.url", "url"),
		User: iniValue(m, "vshell.username", "vshell.user", "server.username", "server.user", "username", "user"),
		Pass: iniValue(m, "vshell.password", "vshell.pass", "server.password", "server.pass", "password", "pass"),
	}, true, nil
}

// applyConfigFile 只填充命令行参数和环境变量未提供的字段。
// 优先级：命令行 > 环境变量 > config.ini。
func applyConfigFile(cfg *Config) (bool, error) {
	fc, exists, err := readConfigFile(cfg.ConfigFile)
	if err != nil || !exists {
		return exists, err
	}
	if cfg.URL == "" {
		cfg.URL = fc.URL
	}
	if cfg.User == "" {
		cfg.User = fc.User
	}
	if cfg.Pass == "" {
		cfg.Pass = fc.Pass
	}
	return true, nil
}

// ensureConfigTemplate 在文件不存在时创建模板；已存在时保持原文件不变。
func ensureConfigTemplate(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := io.WriteString(f, configTemplate); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func displayPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func warnConfigPermissions(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "提示: %s 权限为 %04o，建议执行 chmod 600 %s\n", displayPath(path), perm, displayPath(path))
	}
}

func printConfigSetup(w io.Writer, path string) {
	shown := displayPath(path)
	fmt.Fprintf(w, `
未找到配置文件，已创建模板:
  %s

请填写 [vshell] 下的 url、username、password，然后重新运行。

密码会以明文保存在该文件中，建议限制文件权限。
`, shown)
	if runtime.GOOS != "windows" {
		fmt.Fprintf(w, "  chmod 600 %s\n", shown)
	}
	fmt.Fprintf(w, `
也可以继续用参数或环境变量临时覆盖:
  vshell-cli --url <地址> --user <用户名> info
  VSHELL_URL=... VSHELL_USER=... VSHELL_PASS=... vshell-cli info

运行 vshell-cli help 查看全部命令。
`)
}

func printConfigSummary(cfg *Config, exists, created bool, createErr error) {
	switch {
	case created:
		printConfigSetup(os.Stdout, cfg.ConfigFile)
		return
	case createErr != nil:
		fmt.Fprintf(os.Stderr, "\n提示: 无法创建 %s: %v\n", displayPath(cfg.ConfigFile), createErr)
		return
	case !exists:
		fmt.Printf("\n配置文件: %s（不存在）\n", displayPath(cfg.ConfigFile))
		fmt.Println("请重新运行 vshell-cli 生成模板。")
		return
	}

	fmt.Printf("\n配置文件: %s\n", displayPath(cfg.ConfigFile))
	fmt.Printf("  url      = %s\n", configShow(cfg.URL))
	fmt.Printf("  username = %s\n", configShow(cfg.User))
	pass := "未设置"
	if cfg.Pass != "" {
		pass = "已设置"
	}
	fmt.Printf("  password = %s\n", pass)

	var missing []string
	if cfg.URL == "" {
		missing = append(missing, "url")
	}
	if cfg.User == "" {
		missing = append(missing, "username")
	}
	if cfg.Pass == "" {
		missing = append(missing, "password")
	}
	if len(missing) > 0 {
		fmt.Printf("提示: 仍缺少 %s，请编辑配置文件。\n", strings.Join(missing, "、"))
	} else {
		fmt.Println("配置已就绪。")
	}
}

func configShow(s string) string {
	if s == "" {
		return "未设置"
	}
	return s
}
