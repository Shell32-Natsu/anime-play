package server

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/donnyxia/anime-play/internal/epmap"
	"github.com/donnyxia/anime-play/internal/index"
	"github.com/donnyxia/anime-play/internal/mapping"
	"github.com/donnyxia/anime-play/internal/openlist"
)

// fakeOpenList 模拟 OpenList 的 /api/fs/list 与 /api/fs/get。
type fakeOpenList struct {
	t        *testing.T
	tree     map[string][]openlist.Item // path -> items
	listHits atomic.Int64
	getHits  atomic.Int64
	// rawURLHost 非空时，fs/get 返回的直链用 http://<rawURLHost> 作为前缀
	// （模拟直链经由 OpenList 自身代理的形态）；为空则返回外部 CDN 形态的直链。
	rawURLHost string
}

func (f *fakeOpenList) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/fs/list", func(w http.ResponseWriter, r *http.Request) {
		f.listHits.Add(1)
		if r.Header.Get("Authorization") != "test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Path    string `json:"path"`
			Refresh bool   `json:"refresh"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Refresh {
			f.t.Errorf("fs/list 收到 refresh=true（必须为 false，避免穿透到远程网盘）")
		}
		items, ok := f.tree[req.Path]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "message": "object not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "message": "success",
			"data": map[string]any{"content": items, "total": len(items)},
		})
	})
	mux.HandleFunc("/api/fs/get", func(w http.ResponseWriter, r *http.Request) {
		f.getHits.Add(1)
		var req struct {
			Path string `json:"path"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		base := "https://signed.example.com"
		if f.rawURLHost != "" {
			base = "http://" + f.rawURLHost + "/d"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "message": "success",
			"data": map[string]any{
				"name":    filepath.Base(req.Path),
				"raw_url": base + req.Path + "?sign=abc123",
			},
		})
	})
	return mux
}

func file(name string) openlist.Item { return openlist.Item{Name: name, Size: 1 << 30} }
func dir(name string) openlist.Item  { return openlist.Item{Name: name, IsDir: true} }

func newTestServer(t *testing.T) (*httptest.Server, *fakeOpenList, *mapping.Store) {
	srv, fake, store, _ := newTestServerWithToken(t, "")
	return srv, fake, store
}

func newTestServerWithToken(t *testing.T, adminToken string) (*httptest.Server, *fakeOpenList, *mapping.Store, *epmap.Store) {
	t.Helper()
	fake := &fakeOpenList{
		t: t,
		tree: map[string][]openlist.Item{
			"/anime": {
				dir("[Lilith-Raws] Bocchi the Rock! [Baha][1080p]"),
				dir("Frieren"),
				file("readme.txt"),
			},
			"/anime/[Lilith-Raws] Bocchi the Rock! [Baha][1080p]": {
				file("[Lilith-Raws] Bocchi the Rock! - 01 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4].mp4"),
				file("[Lilith-Raws] Bocchi the Rock! - 02 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4].mp4"),
				file("[Lilith-Raws] Bocchi the Rock! - SP Menu.mp4"),
				file("cover.jpg"),
			},
			// 「番剧名/季/视频」结构
			"/anime/Frieren":          {dir("Season 1")},
			"/anime/Frieren/Season 1": {file("Frieren.S01E01.1080p.mkv"), file("Frieren.S01E02.1080p.mkv")},
			// 第二个扫描根
			"/anime2": {dir("K-ON!")},
			"/anime2/K-ON!": {
				file("[VCB-Studio] K-ON! [01][Ma10p_1080p][x265_flac].mkv"),
				file("OST.flac"),
			},
		},
	}
	upstream := httptest.NewServer(fake.handler())
	t.Cleanup(upstream.Close)

	epStore, err := epmap.NewStore(filepath.Join(t.TempDir(), "episodes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(epStore.Close)

	client := openlist.New(upstream.URL, "test-token")
	idx := index.New(client, []string{"/anime", "/anime2"}, time.Hour, epStore)
	if err := idx.Scan(context.Background()); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	store, err := mapping.NewStore(filepath.Join(t.TempDir(), "mapping.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)

	srv := httptest.NewServer(New(idx, store, epStore, Options{AdminToken: adminToken}).Handler())
	t.Cleanup(srv.Close)
	return srv, fake, store, epStore
}

func get(t *testing.T, rawURL string) (int, string) {
	t.Helper()
	res, err := http.Get(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func TestScanFindsEntriesWithoutFsGet(t *testing.T) {
	_, fake, _ := newTestServer(t)
	if got := fake.getHits.Load(); got != 0 {
		t.Errorf("扫描期调用了 %d 次 fs/get，要求为 0", got)
	}
}

func TestSearchAndPlayFlow(t *testing.T) {
	srv, fake, store := newTestServer(t)

	// 空 keyword 返回全部条目
	code, body := get(t, srv.URL+"/search?keyword=")
	if code != 200 {
		t.Fatalf("search code = %d", code)
	}
	if n := strings.Count(body, `class="anime-item"`); n != 3 {
		t.Fatalf("空关键词应返回 3 个条目，实际 %d:\n%s", n, body)
	}

	// 未映射兜底：清洗后的目录名可命中
	_, body = get(t, srv.URL+"/search?keyword=bocchi")
	if !strings.Contains(body, `class="anime-title"`) || !strings.Contains(body, "Bocchi the Rock!") {
		t.Fatalf("清洗目录名兜底未命中:\n%s", body)
	}

	// 中文搜不到（尚未映射）
	_, body = get(t, srv.URL+"/search?keyword=孤独摇滚")
	if strings.Contains(body, `class="anime-item"`) {
		t.Fatalf("未映射时中文关键词不应命中:\n%s", body)
	}

	// 配置映射后命中
	bocchiID := "/anime/[Lilith-Raws] Bocchi the Rock! [Baha][1080p]"
	if err := store.SetNames(bocchiID, []string{"孤独摇滚", "ぼっち・ざ・ろっく！"}); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, srv.URL+"/search?keyword=孤独摇滚")
	if !strings.Contains(body, `<span class="anime-title">孤独摇滚</span>`) {
		t.Fatalf("映射后中文关键词应命中且标题用映射名:\n%s", body)
	}

	// 提取播放页链接（href 在 HTML 属性里，需按 HTML 实体反转义，等价于真实 HTML 解析器的行为）
	idx := strings.Index(body, `class="anime-link" href="`)
	rest := body[idx+len(`class="anime-link" href="`):]
	playPath := html.UnescapeString(rest[:strings.Index(rest, `"`)])

	// 播放页：集数列表，标题「第 N 话」，SP 排末尾用原文件名
	code, body = get(t, srv.URL+playPath)
	if code != 200 {
		t.Fatalf("play code = %d", code)
	}
	if !strings.Contains(body, ">第 1 话</a>") || !strings.Contains(body, ">第 2 话</a>") {
		t.Fatalf("播放页缺少「第 N 话」集数链接:\n%s", body)
	}
	if !strings.Contains(body, "SP Menu.mp4</a>") {
		t.Fatalf("解析不到集数的视频应以原文件名列出:\n%s", body)
	}
	if strings.Contains(body, `class="player"`) {
		t.Fatalf("不带 ep 参数时不应出现播放器（不应取直链）:\n%s", body)
	}
	if fake.getHits.Load() != 0 {
		t.Fatalf("不带 ep 的播放页不应调用 fs/get")
	}

	// 带 ep=1：实时取直链写入 <video class="player">
	_, body = get(t, srv.URL+playPath+"&ep=1")
	if !strings.Contains(body, `<video class="player" src="https://signed.example.com`) {
		t.Fatalf("播放页缺少带签名直链的 video 标签:\n%s", body)
	}
	if got := fake.getHits.Load(); got != 1 {
		t.Fatalf("播放一集应只调用 1 次 fs/get，实际 %d", got)
	}

	// TTL 内重复播放同一集：复用缓存，不再调 fs/get
	get(t, srv.URL+playPath+"&ep=1")
	if got := fake.getHits.Load(); got != 1 {
		t.Fatalf("TTL 内重复播放不应再调 fs/get，实际 %d 次", got)
	}

	// 播放另一集：再调一次
	get(t, srv.URL+playPath+"&ep=2")
	if got := fake.getHits.Load(); got != 2 {
		t.Fatalf("播放第二集后 fs/get 应为 2 次，实际 %d", got)
	}
}

func TestNestedSeasonStructureAndMultiRoot(t *testing.T) {
	srv, _, _ := newTestServer(t)

	queryEsc := func(s string) string {
		return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}

	// 「番剧名/季/视频」结构：条目应是 Season 1 这一层
	_, body := get(t, srv.URL+"/search?keyword=frieren")
	if !strings.Contains(body, queryEsc("/anime/Frieren/Season 1")) {
		t.Fatalf("嵌套季结构未识别为条目:\n%s", body)
	}

	// 第二个扫描根的条目也在索引里
	_, body = get(t, srv.URL+"/search?keyword=k-on")
	if !strings.Contains(body, queryEsc("/anime2/K-ON!")) {
		t.Fatalf("第二个扫描根的条目未进入索引:\n%s", body)
	}
}

func TestAdminPageAndSave(t *testing.T) {
	srv, _, store := newTestServer(t)

	code, body := get(t, srv.URL+"/admin")
	if code != 200 {
		t.Fatalf("admin code = %d", code)
	}
	for _, want := range []string{"扫描根：/anime", "扫描根：/anime2", "未映射"} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin 页缺少 %q:\n%s", want, body)
		}
	}

	// 缺少 X-Requested-With 头（模拟跨站表单 CSRF）：应被拒绝
	payload := `{"dir":"/anime2/K-ON!","names":["轻音少女","K-ON!"]}`
	res, err := http.Post(srv.URL+"/admin/save", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 防护头时应返回 403，实际 %d", res.StatusCode)
	}
	if names := store.NamesFor("/anime2/K-ON!"); names != nil {
		t.Fatalf("被拒绝的请求不应写入映射: %v", names)
	}

	// 带上 /admin 页面 fetch 使用的请求头：保存成功
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/save", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "anime-play")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("admin/save code = %d", res.StatusCode)
	}
	if names := store.NamesFor("/anime2/K-ON!"); len(names) != 2 || names[0] != "轻音少女" {
		t.Fatalf("保存后映射不正确: %v", names)
	}

	// 搜索按新映射命中
	_, body = get(t, srv.URL+"/search?keyword=轻音")
	if !strings.Contains(body, `<span class="anime-title">轻音少女</span>`) {
		t.Fatalf("保存映射后搜索未命中:\n%s", body)
	}
}

func TestAdminSaveRejectsUnknownDir(t *testing.T) {
	srv, _, store := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/save",
		strings.NewReader(`{"dir":"/not/in/index","names":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "anime-play")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("索引外的 dir 应被拒绝，实际 %d", res.StatusCode)
	}
	if store.NamesFor("/not/in/index") != nil {
		t.Fatal("索引外的 dir 不应写入映射")
	}
}

func TestAdminTokenAuth(t *testing.T) {
	srv, _, _, _ := newTestServerWithToken(t, "secret-token")

	// 对外端点不受影响
	if code, _ := get(t, srv.URL+"/search?keyword="); code != 200 {
		t.Fatalf("/search 不应需要鉴权，code = %d", code)
	}
	if code, _ := get(t, srv.URL+"/play?id=/anime2/K-ON!"); code != 200 {
		t.Fatalf("/play 不应需要鉴权，code = %d", code)
	}

	// 管理端点未带口令：401
	for _, path := range []string{"/admin", "/refresh"} {
		if code, _ := get(t, srv.URL+path); code != http.StatusUnauthorized {
			t.Fatalf("%s 未带口令应返回 401，实际 %d", path, code)
		}
	}

	// Basic 密码 = token：放行
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin", nil)
	req.SetBasicAuth("admin", "secret-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("Basic 鉴权应放行，实际 %d", res.StatusCode)
	}

	// X-Admin-Token 头：放行；错误口令：401
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/refresh", nil)
	req.Header.Set("X-Admin-Token", "secret-token")
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("X-Admin-Token 应放行，实际 %d", res.StatusCode)
	}
	req.Header.Set("X-Admin-Token", "wrong")
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误口令应 401，实际 %d", res.StatusCode)
	}
}

func TestManualEpisodeOverrides(t *testing.T) {
	fake := &fakeOpenList{
		t: t,
		tree: map[string][]openlist.Item{
			"/anime": {dir("Bocchi")},
			"/anime/Bocchi": {
				file("Bocchi - 01.mkv"),
				file("Bocchi - 02.mkv"),
				file("ED-MV.mp4"), // 自动解析不出集数
			},
		},
	}
	upstream := httptest.NewServer(fake.handler())
	t.Cleanup(upstream.Close)

	// 手动集数映射：把 ED-MV.mp4 指定为第 13 话，并把 02 覆盖成第 5 话
	yamlPath := filepath.Join(t.TempDir(), "episodes.yaml")
	content := "entries:\n  - dir: /anime/Bocchi\n    episodes:\n      13: ED-MV.mp4\n      5: Bocchi - 02.mkv\n"
	if err := os.WriteFile(yamlPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	epStore, err := epmap.NewStore(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(epStore.Close)

	client := openlist.New(upstream.URL, "test-token")
	idx := index.New(client, []string{"/anime"}, time.Hour, epStore)
	if err := idx.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	store, err := mapping.NewStore(filepath.Join(t.TempDir(), "mapping.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	srv := httptest.NewServer(New(idx, store, epStore, Options{}).Handler())
	t.Cleanup(srv.Close)

	_, body := get(t, srv.URL+"/play?id=/anime/Bocchi")
	for _, want := range []string{">第 1 话</a>", ">第 5 话</a>", ">第 13 话</a>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("播放页缺少 %q（手动集数映射未生效）:\n%s", want, body)
		}
	}
	if strings.Contains(body, ">第 2 话</a>") || strings.Contains(body, "ED-MV.mp4</a>") {
		t.Fatalf("手动映射应覆盖自动解析结果:\n%s", body)
	}
	// 顺序应为 1、5、13：ep=3 应对应 ED-MV（第 13 话）
	listCalls := fake.listHits.Load()

	// 修改 YAML 后 ApplyOverrides：纯内存重建，不应产生新的 fs/list
	if err := epStore.Watch(idx.ApplyOverrides); err != nil {
		t.Fatal(err)
	}
	content = "entries:\n  - dir: /anime/Bocchi\n    episodes:\n      99: ED-MV.mp4\n"
	if err := os.WriteFile(yamlPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, b := get(t, srv.URL+"/play?id=/anime/Bocchi")
		return strings.Contains(b, ">第 99 话</a>") && strings.Contains(b, ">第 2 话</a>")
	})
	if fake.listHits.Load() != listCalls {
		t.Fatalf("应用手动集数映射不应重新请求 OpenList（fs/list %d -> %d）", listCalls, fake.listHits.Load())
	}
}

func TestAdminEpisodesPageAndSave(t *testing.T) {
	srv, _, _, epStore := newTestServerWithToken(t, "")

	bocchiID := "/anime/[Lilith-Raws] Bocchi the Rock! [Baha][1080p]"
	epPage := "/admin/episodes?id=" + url.QueryEscape(bocchiID)

	// 编辑页：列出文件名、自动解析结果与输入框
	code, body := get(t, srv.URL+epPage)
	if code != 200 {
		t.Fatalf("admin/episodes code = %d", code)
	}
	for _, want := range []string{"SP Menu.mp4", `class="ep-num"`, "第 1 话", "第 2 话", "保存全部"} {
		if !strings.Contains(body, want) {
			t.Fatalf("集数编辑页缺少 %q:\n%s", want, body)
		}
	}

	// 保存：把 SP Menu.mp4 手动指定为第 13 话
	doSave := func(payload string, wantCode int) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/episodes/save", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "anime-play")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != wantCode {
			t.Fatalf("episodes/save code = %d（期望 %d）: %s", res.StatusCode, wantCode, b)
		}
		return string(b)
	}

	doSave(`{"dir":"`+bocchiID+`","episodes":{"[Lilith-Raws] Bocchi the Rock! - SP Menu.mp4":"13"}}`, 200)

	// 内存与磁盘都生效
	if ov := epStore.Overrides(bocchiID); ov["[Lilith-Raws] Bocchi the Rock! - SP Menu.mp4"] != 13 {
		t.Fatalf("保存后 Overrides = %v", ov)
	}
	// 播放页立刻生效（无需 /refresh）
	_, body = get(t, srv.URL+"/play?id="+url.QueryEscape(bocchiID))
	if !strings.Contains(body, ">第 13 话</a>") || strings.Contains(body, "SP Menu.mp4</a>") {
		t.Fatalf("保存集数后播放页未立刻生效:\n%s", body)
	}
	// 编辑页显示当前手动值
	_, body = get(t, srv.URL+epPage)
	if !strings.Contains(body, `value="13"`) {
		t.Fatalf("编辑页未显示已保存的手动集数:\n%s", body)
	}

	// 非法请求：不在该条目中的文件 / 非数字 / 重复集数 / 缺 CSRF 头
	doSave(`{"dir":"`+bocchiID+`","episodes":{"nope.mkv":"1"}}`, http.StatusBadRequest)
	doSave(`{"dir":"`+bocchiID+`","episodes":{"[Lilith-Raws] Bocchi the Rock! - SP Menu.mp4":"abc"}}`, http.StatusBadRequest)
	doSave(`{"dir":"`+bocchiID+`","episodes":{"[Lilith-Raws] Bocchi the Rock! - 01 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4].mp4":"1","[Lilith-Raws] Bocchi the Rock! - 02 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4].mp4":"1"}}`, http.StatusBadRequest)
	res, err := http.Post(srv.URL+"/admin/episodes/save", "application/json", strings.NewReader(`{"dir":"`+bocchiID+`","episodes":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 防护头时应返回 403，实际 %d", res.StatusCode)
	}

	// 清空：全部留空 = 删除该条目的手动映射
	doSave(`{"dir":"`+bocchiID+`","episodes":{}}`, 200)
	if ov := epStore.Overrides(bocchiID); ov != nil {
		t.Fatalf("清空后 Overrides 应为 nil: %v", ov)
	}
	_, body = get(t, srv.URL+"/play?id="+url.QueryEscape(bocchiID))
	if !strings.Contains(body, "SP Menu.mp4</a>") {
		t.Fatalf("清空手动映射后应回到自动解析:\n%s", body)
	}
}

func TestRewriteRawURLHost(t *testing.T) {
	cases := []struct {
		name         string
		rawURL       string
		requestHost  string
		openlistHost string
		want         string
	}{
		{
			name:         "内网地址换成 Tailscale 地址，端口保留",
			rawURL:       "http://192.168.1.10:5244/d/anime/ep01.mkv?sign=abc",
			requestHost:  "100.64.0.5:8080",
			openlistHost: "192.168.1.10",
			want:         "http://100.64.0.5:5244/d/anime/ep01.mkv?sign=abc",
		},
		{
			name:         "外部网盘 CDN 直链不动",
			rawURL:       "https://cdn.example.com/file?sign=abc",
			requestHost:  "100.64.0.5:8080",
			openlistHost: "192.168.1.10",
			want:         "https://cdn.example.com/file?sign=abc",
		},
		{
			name:         "请求主机名与直链相同则不动",
			rawURL:       "http://192.168.1.10:5244/d/x.mkv",
			requestHost:  "192.168.1.10:8080",
			openlistHost: "192.168.1.10",
			want:         "http://192.168.1.10:5244/d/x.mkv",
		},
		{
			name:         "用域名访问也替换",
			rawURL:       "http://192.168.1.10:5244/d/x.mkv",
			requestHost:  "nas.tailnet-xxxx.ts.net:8080",
			openlistHost: "192.168.1.10",
			want:         "http://nas.tailnet-xxxx.ts.net:5244/d/x.mkv",
		},
		{
			name:         "直链无端口时只换主机名",
			rawURL:       "http://192.168.1.10/d/x.mkv",
			requestHost:  "100.64.0.5:8080",
			openlistHost: "192.168.1.10",
			want:         "http://100.64.0.5/d/x.mkv",
		},
		{
			name:         "IPv6 请求主机名加方括号",
			rawURL:       "http://192.168.1.10:5244/d/x.mkv",
			requestHost:  "[fd7a::1234]:8080",
			openlistHost: "192.168.1.10",
			want:         "http://[fd7a::1234]:5244/d/x.mkv",
		},
	}
	for _, c := range cases {
		if got := rewriteRawURLHost(c.rawURL, c.requestHost, c.openlistHost); got != c.want {
			t.Errorf("%s: rewriteRawURLHost = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPlayPageRewritesRawURLHost(t *testing.T) {
	fake := &fakeOpenList{
		t: t,
		tree: map[string][]openlist.Item{
			"/anime":        {dir("Bocchi")},
			"/anime/Bocchi": {file("Bocchi - 01.mkv")},
		},
	}
	upstream := httptest.NewServer(fake.handler())
	t.Cleanup(upstream.Close)
	upstreamHost := strings.TrimPrefix(upstream.URL, "http://") // 127.0.0.1:port
	upstreamName, upstreamPort, _ := net.SplitHostPort(upstreamHost)

	client := openlist.New(upstream.URL, "test-token")
	idx := index.New(client, []string{"/anime"}, time.Hour, nil)
	if err := idx.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	store, err := mapping.NewStore(filepath.Join(t.TempDir(), "mapping.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)

	srv := httptest.NewServer(New(idx, store, nil, Options{
		OpenListHost:      upstreamName,
		RewriteRawURLHost: true,
	}).Handler())
	t.Cleanup(srv.Close)

	// 模拟客户端通过 Tailscale 主机名访问本服务（Host 头不同于 OpenList 配置的主机名）
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/play?id=/anime/Bocchi&ep=1", nil)
	req.Host = "100.64.0.5:8080"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	want := `<video class="player" src="https://signed.example.com/anime/Bocchi/Bocchi%20-%2001.mkv?sign=abc123"`
	_ = want // fake 返回的直链主机是 signed.example.com（外部 CDN 形态），不应被改写
	if !strings.Contains(string(body), `src="https://signed.example.com`) {
		t.Fatalf("外部直链不应被改写:\n%s", body)
	}

	// 让 fake 返回「指向 OpenList 自身」的直链（主机名 = OpenList 主机名），应被改写为请求 Host 的主机名
	fake.rawURLHost = upstreamName + ":" + upstreamPort
	idx2 := index.New(client, []string{"/anime"}, time.Hour, nil)
	if err := idx2.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(New(idx2, store, nil, Options{
		OpenListHost:      upstreamName,
		RewriteRawURLHost: true,
	}).Handler())
	t.Cleanup(srv2.Close)

	req, _ = http.NewRequest(http.MethodGet, srv2.URL+"/play?id=/anime/Bocchi&ep=1", nil)
	req.Host = "100.64.0.5:8080"
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()

	wantPrefix := `src="http://100.64.0.5:` + upstreamPort + `/`
	if !strings.Contains(string(body), wantPrefix) {
		t.Fatalf("指向 OpenList 的直链应改写为请求主机名（期望包含 %q）:\n%s", wantPrefix, body)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

func TestRefreshEndpoint(t *testing.T) {
	srv, fake, _ := newTestServer(t)

	// 新增一个条目后手动 /refresh
	fake.tree["/anime"] = append(fake.tree["/anime"], dir("New Show"))
	fake.tree["/anime/New Show"] = []openlist.Item{file("New Show - 01.mkv")}

	code, body := get(t, srv.URL+"/refresh")
	if code != 200 || !strings.Contains(body, "刷新完成") {
		t.Fatalf("refresh 失败 code=%d body=%s", code, body)
	}
	_, body = get(t, srv.URL+"/search?keyword=new+show")
	if !strings.Contains(body, "New Show") {
		t.Fatalf("刷新后新增条目未出现在搜索结果:\n%s", body)
	}
}
