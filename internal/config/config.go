// Package config 从环境变量加载服务配置。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config 是服务的全部运行配置，全部来自环境变量。
type Config struct {
	// OpenListBaseURL OpenList 实例地址，例如 http://192.168.1.10:5244
	OpenListBaseURL string
	// OpenListToken OpenList 管理员 token
	OpenListToken string
	// ScanRoots 扫描的根路径，支持多个
	ScanRoots []string
	// ListenPort HTTP 监听端口
	ListenPort string
	// MappingFile 番剧名映射文件路径（应位于挂载 volume 内）
	MappingFile string
	// EpisodeMapFile 手动集数映射 YAML 文件路径（可选，应位于挂载 volume 内）
	EpisodeMapFile string
	// AdminToken 可选的 /admin 管理页访问口令；为空时 /admin 不做鉴权（仅建议在可信内网使用）
	AdminToken string
	// RefreshInterval 目录索引自动刷新间隔
	RefreshInterval time.Duration
	// RawURLCacheTTL 视频直链缓存 TTL，应保守地小于 OpenList 签名有效期
	RawURLCacheTTL time.Duration
}

// FromEnv 读取环境变量并校验必填项。
//
// 在读取之前会先尝试加载 .env 文件（路径取 ENV_FILE，默认 ./.env）；
// .env 只补充缺失的变量，已存在的真实环境变量优先。
func FromEnv() (*Config, error) {
	envFile := envDefault("ENV_FILE", ".env")
	if err := loadDotEnv(envFile); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("加载 %s 失败: %w", envFile, err)
		}
		// 默认 .env 不存在是正常情况；显式指定 ENV_FILE 却不存在则报错
		if os.Getenv("ENV_FILE") != "" {
			return nil, fmt.Errorf("ENV_FILE 指定的文件不存在: %s", envFile)
		}
	}

	cfg := &Config{
		OpenListBaseURL: strings.TrimRight(os.Getenv("OPENLIST_BASE_URL"), "/"),
		OpenListToken:   os.Getenv("OPENLIST_TOKEN"),
		ListenPort:      envDefault("LISTEN_PORT", "8080"),
		MappingFile:     envDefault("MAPPING_FILE", "/data/mapping.json"),
		EpisodeMapFile:  envDefault("EPISODE_MAP_FILE", "/data/episodes.yaml"),
		AdminToken:      os.Getenv("ADMIN_TOKEN"),
	}

	if cfg.OpenListBaseURL == "" {
		return nil, fmt.Errorf("OPENLIST_BASE_URL 未设置")
	}
	if cfg.OpenListToken == "" {
		return nil, fmt.Errorf("OPENLIST_TOKEN 未设置")
	}

	rootsRaw := os.Getenv("SCAN_ROOTS")
	if rootsRaw == "" {
		return nil, fmt.Errorf("SCAN_ROOTS 未设置")
	}
	for _, r := range strings.Split(rootsRaw, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !strings.HasPrefix(r, "/") {
			r = "/" + r
		}
		cfg.ScanRoots = append(cfg.ScanRoots, strings.TrimRight(r, "/"))
	}
	if len(cfg.ScanRoots) == 0 {
		return nil, fmt.Errorf("SCAN_ROOTS 不含有效路径")
	}

	var err error
	cfg.RefreshInterval, err = parseDurationEnv("REFRESH_INTERVAL", 30*time.Minute)
	if err != nil {
		return nil, err
	}
	cfg.RawURLCacheTTL, err = parseDurationEnv("RAWURL_CACHE_TTL", time.Hour)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDurationEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s 解析失败（示例: 30m, 1h）: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s 必须大于 0", key)
	}
	return d, nil
}
