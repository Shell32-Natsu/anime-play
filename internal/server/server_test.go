package server

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "message": "success",
			"data": map[string]any{
				"name":    filepath.Base(req.Path),
				"raw_url": "https://signed.example.com" + req.Path + "?sign=abc123",
			},
		})
	})
	return mux
}

func file(name string) openlist.Item { return openlist.Item{Name: name, Size: 1 << 30} }
func dir(name string) openlist.Item  { return openlist.Item{Name: name, IsDir: true} }

func newTestServer(t *testing.T) (*httptest.Server, *fakeOpenList, *mapping.Store) {
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

	client := openlist.New(upstream.URL, "test-token")
	idx := index.New(client, []string{"/anime", "/anime2"}, time.Hour)
	if err := idx.Scan(context.Background()); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	store, err := mapping.NewStore(filepath.Join(t.TempDir(), "mapping.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)

	srv := httptest.NewServer(New(idx, store).Handler())
	t.Cleanup(srv.Close)
	return srv, fake, store
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

	// 通过 /admin/save 写映射
	payload := `{"dir":"/anime2/K-ON!","names":["轻音少女","K-ON!"]}`
	res, err := http.Post(srv.URL+"/admin/save", "application/json", strings.NewReader(payload))
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
