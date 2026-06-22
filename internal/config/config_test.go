package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clearServiceEnv 清掉本服务用到的环境变量，避免测试间互相影响。
func clearServiceEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OPENLIST_BASE_URL", "OPENLIST_TOKEN", "SCAN_ROOTS", "LISTEN_PORT",
		"MAPPING_FILE", "REFRESH_INTERVAL", "RAWURL_CACHE_TTL", "ENV_FILE",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestFromEnvWithDotEnvFile(t *testing.T) {
	clearServiceEnv(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := `# anime-play 配置
OPENLIST_BASE_URL=http://192.168.1.10:5244/
export OPENLIST_TOKEN="alist-secret-token"
SCAN_ROOTS=/anime, /盘B/番剧   # 多个根
REFRESH_INTERVAL=15m
RAWURL_CACHE_TTL='45m'

LISTEN_PORT=9090
`
	if err := os.WriteFile(envFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_FILE", envFile)
	// 真实环境变量优先于 .env
	t.Setenv("LISTEN_PORT", "7000")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenListBaseURL != "http://192.168.1.10:5244" {
		t.Errorf("OpenListBaseURL = %q", cfg.OpenListBaseURL)
	}
	if cfg.OpenListToken != "alist-secret-token" {
		t.Errorf("OpenListToken = %q", cfg.OpenListToken)
	}
	if len(cfg.ScanRoots) != 2 || cfg.ScanRoots[0] != "/anime" || cfg.ScanRoots[1] != "/盘B/番剧" {
		t.Errorf("ScanRoots = %v", cfg.ScanRoots)
	}
	if cfg.RefreshInterval != 15*time.Minute || cfg.RawURLCacheTTL != 45*time.Minute {
		t.Errorf("RefreshInterval=%v RawURLCacheTTL=%v", cfg.RefreshInterval, cfg.RawURLCacheTTL)
	}
	if cfg.ListenPort != "7000" {
		t.Errorf("真实环境变量应优先于 .env，ListenPort = %q", cfg.ListenPort)
	}
}

func TestFromEnvMissingRequired(t *testing.T) {
	clearServiceEnv(t)
	// 默认 .env（当前目录下）不存在：不应报“文件不存在”，而是因缺少必填项报错
	if _, err := FromEnv(); err == nil {
		t.Fatal("缺少必填项时应报错")
	}

	clearServiceEnv(t)
	t.Setenv("OPENLIST_BASE_URL", "http://x:5244")
	t.Setenv("OPENLIST_TOKEN", "t")
	if _, err := FromEnv(); err == nil {
		t.Fatal("缺少 SCAN_ROOTS 时应报错")
	}
}

func TestFromEnvExplicitEnvFileMissing(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "no-such.env"))
	t.Setenv("OPENLIST_BASE_URL", "http://x:5244")
	t.Setenv("OPENLIST_TOKEN", "t")
	t.Setenv("SCAN_ROOTS", "/anime")
	if _, err := FromEnv(); err == nil {
		t.Fatal("显式指定的 ENV_FILE 不存在时应报错")
	}
}
