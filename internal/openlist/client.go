// Package openlist 封装 OpenList 的 /api/fs/list 与 /api/fs/get 两个接口。
//
// 设计原则（最小化对远程网盘的触碰）：
//   - List 永远以 refresh=false 调用，不强制穿透 OpenList 缓存；
//   - Get（取签名直链）只在真正要播放某集时由上层调用，本包不做任何预取。
package openlist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client OpenList API 客户端。
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New 创建客户端。baseURL 不带末尾斜杠。
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Item /api/fs/list 返回的目录项。
type Item struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

type listRequest struct {
	Path     string `json:"path"`
	Password string `json:"password"`
	Page     int    `json:"page"`
	PerPage  int    `json:"per_page"`
	Refresh  bool   `json:"refresh"`
}

type listResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Content []Item `json:"content"`
		Total   int    `json:"total"`
	} `json:"data"`
}

type getRequest struct {
	Path     string `json:"path"`
	Password string `json:"password"`
}

type getResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		RawURL string `json:"raw_url"`
	} `json:"data"`
}

// List 列出 path 下的全部条目（per_page=0 表示不分页，refresh 固定为 false）。
func (c *Client) List(ctx context.Context, path string) ([]Item, error) {
	var resp listResponse
	err := c.post(ctx, "/api/fs/list", listRequest{
		Path:     path,
		Password: "",
		Page:     1,
		PerPage:  0,
		Refresh:  false,
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("fs/list %q: %w", path, err)
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("fs/list %q: code=%d message=%s", path, resp.Code, resp.Message)
	}
	return resp.Data.Content, nil
}

// GetRawURL 获取文件的签名直链。签名有时效，调用方负责短 TTL 缓存，绝不长期保存。
func (c *Client) GetRawURL(ctx context.Context, path string) (string, error) {
	var resp getResponse
	err := c.post(ctx, "/api/fs/get", getRequest{Path: path, Password: ""}, &resp)
	if err != nil {
		return "", fmt.Errorf("fs/get %q: %w", path, err)
	}
	if resp.Code != 200 {
		return "", fmt.Errorf("fs/get %q: code=%d message=%s", path, resp.Code, resp.Message)
	}
	if resp.Data.RawURL == "" {
		return "", fmt.Errorf("fs/get %q: raw_url 为空", path)
	}
	return resp.Data.RawURL, nil
}

func (c *Client) post(ctx context.Context, apiPath string, reqBody, respBody any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+apiPath, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, respBody); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
