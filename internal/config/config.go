// Package config 读写飞书应用凭据和状态目录。
package config

// 凭据和状态目录。
// 状态目录默认 ~/.claude/channels/feishu，可用 FEISHU_STATE_DIR 改。
// 凭据优先读环境变量，其次读状态目录下的 .env。

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	StateDir  string
	AppID     string
	AppSecret string
	// feishu（默认）或 lark（国际版）
	Domain string
}

func StateDir() string {
	if d := os.Getenv("FEISHU_STATE_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "channels", "feishu")
}

func envFilePath(dir string) string { return filepath.Join(dir, ".env") }

func readEnvFile(path string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return m
}

func Load() (*Config, error) {
	dir := StateDir()
	file := readEnvFile(envFilePath(dir))
	get := func(k string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return file[k]
	}
	c := &Config{
		StateDir:  dir,
		AppID:     get("FEISHU_APP_ID"),
		AppSecret: get("FEISHU_APP_SECRET"),
		Domain:    strings.ToLower(get("FEISHU_DOMAIN")),
	}
	if c.Domain == "" {
		c.Domain = "feishu"
	}
	if c.AppID == "" || c.AppSecret == "" {
		return c, errors.New("缺少 FEISHU_APP_ID / FEISHU_APP_SECRET，先运行 `feishu-channel configure <app_id> <app_secret>`")
	}
	return c, nil
}

func SaveCredentials(appID, appSecret, domain string) (string, error) {
	dir := StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := envFilePath(dir)
	content := fmt.Sprintf("FEISHU_APP_ID=%s\nFEISHU_APP_SECRET=%s\nFEISHU_DOMAIN=%s\n", appID, appSecret, domain)
	return p, os.WriteFile(p, []byte(content), 0o600)
}
