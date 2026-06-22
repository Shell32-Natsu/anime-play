// Package server 对外提供伪在线番剧站点的三件套端点（/search /play /refresh）
// 以及 /admin 映射管理界面。
//
// 对外 HTML 的 class 结构是给弹幕播放器写 XPath/CSS 选择器用的，保持稳定：
//
//	搜索页 /search?keyword=xxx
//	  div.anime-item > a.anime-link[href="/play?id=..."] > span.anime-title
//	播放页 /play?id=xxx[&ep=N]
//	  a.ep-link[href="/play?id=...&ep=N"]   集数链接，文本「第 N 话」
//	  video.player[src=signed-url]          仅带 ep 参数时出现，src 为实时取的直链
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/donnyxia/anime-play/internal/episode"
	"github.com/donnyxia/anime-play/internal/index"
	"github.com/donnyxia/anime-play/internal/mapping"
)

// EpisodeMapStore 手动集数映射存储（由 epmap.Store 实现）。
type EpisodeMapStore interface {
	// Overrides 返回某条目的「文件名 → 手动集数」；没有则返回 nil。
	Overrides(dir string) map[string]float64
	// SetOverrides 更新某条目的手动集数映射并持久化；空 map 表示清除。
	SetOverrides(dir string, fileToNum map[string]float64) error
}

// Server HTTP 服务。
type Server struct {
	idx        *index.Index
	mapping    *mapping.Store
	epmap      EpisodeMapStore
	adminToken string
	mux        *http.ServeMux
}

// New 创建 Server。adminToken 非空时，/admin、/admin/save、/refresh 这些管理端点
// 需要鉴权（HTTP Basic 的密码 = token，或请求头 X-Admin-Token）；为空则不鉴权，
// 仅适合可信内网。对外伪站点端点（/search /play）始终无鉴权，供播放器爬取。
func New(idx *index.Index, m *mapping.Store, em EpisodeMapStore, adminToken string) *Server {
	s := &Server{idx: idx, mapping: m, epmap: em, adminToken: adminToken, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /search", s.handleSearch)
	s.mux.HandleFunc("GET /play", s.handlePlay)
	s.mux.HandleFunc("GET /refresh", s.requireAdmin(s.handleRefresh))
	s.mux.HandleFunc("GET /admin", s.requireAdmin(s.handleAdmin))
	s.mux.HandleFunc("POST /admin/save", s.requireAdmin(s.handleAdminSave))
	s.mux.HandleFunc("GET /admin/episodes", s.requireAdmin(s.handleAdminEpisodes))
	s.mux.HandleFunc("POST /admin/episodes/save", s.requireAdmin(s.handleAdminEpisodesSave))
	s.mux.HandleFunc("GET /", s.handleRoot)
	return s
}

// requireJSONNoCSRF 校验 /admin 下写接口的 CSRF 防护头与 Content-Type；不满足时写出错误并返回 false。
// 跨站表单（form / text-plain enctype）无法携带自定义头；浏览器里带自定义头的跨域 fetch
// 会先发 CORS 预检，本服务不返回任何 CORS 允许头，预检即失败。
func requireJSONNoCSRF(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Requested-With") != "anime-play" {
		http.Error(w, "缺少 X-Requested-With 请求头（直接调用 API 时请加上 X-Requested-With: anime-play）", http.StatusForbidden)
		return false
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type 必须为 application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// requireAdmin 管理端点鉴权中间件；adminToken 为空时直接放行。
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" {
			next(w, r)
			return
		}
		equal := func(got string) bool {
			return subtle.ConstantTimeCompare([]byte(got), []byte(s.adminToken)) == 1
		}
		if equal(r.Header.Get("X-Admin-Token")) {
			next(w, r)
			return
		}
		if _, pass, ok := r.BasicAuth(); ok && equal(pass) {
			next(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="anime-play admin"`)
		http.Error(w, "需要管理口令（Basic 密码或 X-Admin-Token 请求头 = ADMIN_TOKEN）", http.StatusUnauthorized)
	}
}

// Handler 返回 http.Handler。
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusFound)
}

// playURL 生成播放页链接；条目 ID 是 OpenList 路径，可能含空格 / 括号 / & 等字符，
// 必须 query-escape 后再拼 URL。空格用 %20 而不是 +，避免在 HTML 属性里被转义成
// &#43; 实体，方便简单爬虫直接取 href。
func playURL(id string, ep int) template.URL {
	u := "/play?id=" + strings.ReplaceAll(url.QueryEscape(id), "+", "%20")
	if ep > 0 {
		u += "&ep=" + strconv.Itoa(ep)
	}
	return template.URL(u)
}

// episodesURL 生成 /admin 集数编辑页链接。
func episodesURL(id string) template.URL {
	return template.URL("/admin/episodes?id=" + strings.ReplaceAll(url.QueryEscape(id), "+", "%20"))
}

// ---------- /search ----------

type searchItem struct {
	PlayURL template.URL
	Title   string
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	kw := strings.ToLower(keyword)

	var items []searchItem
	for _, e := range s.idx.Entries() {
		names := s.mapping.NamesFor(e.ID)
		title := e.CleanedName
		if len(names) > 0 {
			title = names[0]
		}
		if kw == "" || matches(kw, names, e.CleanedName, e.DirName) {
			items = append(items, searchItem{PlayURL: playURL(e.ID, 0), Title: title})
		}
	}

	render(w, searchTmpl, map[string]any{
		"Keyword": keyword,
		"Items":   items,
	})
}

// matches 大小写不敏感的包含匹配：names 任一项包含关键词，或关键词包含在
// 清洗后的目录名 / 原目录名中（未映射兜底）。
func matches(kwLower string, names []string, cleaned, dirName string) bool {
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), kwLower) {
			return true
		}
	}
	if len(names) > 0 {
		return false // 已映射的条目只按 names 匹配
	}
	return strings.Contains(strings.ToLower(cleaned), kwLower) ||
		strings.Contains(strings.ToLower(dirName), kwLower)
}

// ---------- /play ----------

type playEpisode struct {
	N       int
	Title   string
	URL     template.URL
	Current bool
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	entry, ok := s.idx.Get(id)
	if !ok {
		http.Error(w, "条目不存在（索引可能尚未刷新，可访问 /refresh）", http.StatusNotFound)
		return
	}

	names := s.mapping.NamesFor(entry.ID)
	title := entry.CleanedName
	if len(names) > 0 {
		title = names[0]
	}

	episodes := make([]playEpisode, len(entry.Episodes))
	for i, ep := range entry.Episodes {
		episodes[i] = playEpisode{N: i + 1, Title: ep.Title, URL: playURL(entry.ID, i+1)}
	}

	data := map[string]any{
		"Title":    title,
		"Episodes": episodes,
		"Selected": false,
		"VideoURL": "",
		"EpTitle":  "",
		"Error":    "",
	}

	epStr := r.URL.Query().Get("ep")
	if epStr != "" {
		n, err := strconv.Atoi(epStr)
		if err != nil || n < 1 || n > len(entry.Episodes) {
			http.Error(w, "集数不存在", http.StatusNotFound)
			return
		}
		episodes[n-1].Current = true
		ep := entry.Episodes[n-1]
		data["Selected"] = true
		data["EpTitle"] = ep.Title

		// 仅在播放页被打开的此刻实时取签名直链（带短 TTL 缓存），保证签名新鲜
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		rawURL, err := s.idx.RawURL(ctx, ep.Path)
		if err != nil {
			log.Printf("[server] 取直链失败 %s: %v", ep.Path, err)
			data["Error"] = fmt.Sprintf("获取视频直链失败: %v", err)
		} else {
			data["VideoURL"] = rawURL
		}
	}

	render(w, playTmpl, data)
}

// ---------- /refresh ----------

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	err := s.idx.Scan(ctx)
	_, count, _ := s.idx.Status()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "刷新完成（部分失败）：当前 %d 个条目\n%v\n", count, err)
		return
	}
	fmt.Fprintf(w, "刷新完成：当前 %d 个条目\n", count)
}

// ---------- /admin ----------

type adminEntry struct {
	ID       string
	DirName  string
	Cleaned  string
	Names    string // 逗号分隔，便于编辑
	Mapped   bool
	Episodes int
	EpURL    template.URL // 集数编辑页链接
}

type adminGroup struct {
	Root    string
	Entries []adminEntry
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	groups := map[string][]adminEntry{}
	mappedCount := 0
	entries := s.idx.Entries()
	for _, e := range entries {
		names := s.mapping.NamesFor(e.ID)
		if len(names) > 0 {
			mappedCount++
		}
		groups[e.Root] = append(groups[e.Root], adminEntry{
			ID:       e.ID,
			DirName:  e.DirName,
			Cleaned:  e.CleanedName,
			Names:    strings.Join(names, ", "),
			Mapped:   len(names) > 0,
			Episodes: len(e.Episodes),
			EpURL:    episodesURL(e.ID),
		})
	}

	var groupList []adminGroup
	for root, ge := range groups {
		groupList = append(groupList, adminGroup{Root: root, Entries: ge})
	}
	sort.Slice(groupList, func(i, j int) bool { return groupList[i].Root < groupList[j].Root })

	lastScan, _, scanErr := s.idx.Status()
	scanErrStr := ""
	if scanErr != nil {
		scanErrStr = scanErr.Error()
	}
	lastScanStr := "尚未扫描"
	if !lastScan.IsZero() {
		lastScanStr = lastScan.Format("2006-01-02 15:04:05")
	}

	render(w, adminTmpl, map[string]any{
		"Groups":    groupList,
		"Total":     len(entries),
		"Mapped":    mappedCount,
		"Unmapped":  len(entries) - mappedCount,
		"LastScan":  lastScanStr,
		"ScanError": scanErrStr,
	})
}

type adminSaveRequest struct {
	Dir   string   `json:"dir"`
	Names []string `json:"names"`
}

func (s *Server) handleAdminSave(w http.ResponseWriter, r *http.Request) {
	if !requireJSONNoCSRF(w, r) {
		return
	}

	var req adminSaveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "请求体不是有效 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Dir == "" {
		http.Error(w, "dir 不能为空", http.StatusBadRequest)
		return
	}
	// 只允许给当前索引中存在的条目写映射，拒绝任意字符串注入映射文件
	if _, ok := s.idx.Get(req.Dir); !ok {
		http.Error(w, "dir 不是当前索引中的条目（新增番剧请先 /refresh）", http.StatusBadRequest)
		return
	}
	if err := s.mapping.SetNames(req.Dir, req.Names); err != nil {
		log.Printf("[server] 保存映射失败 %s: %v", req.Dir, err)
		http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ---------- /admin/episodes ----------

type adminEpisodeRow struct {
	FileName string
	// AutoTitle 文件名自动解析的结果（解析不到为「—」），仅供参考显示。
	AutoTitle string
	// Manual 当前手动指定的集数（字符串形式，空 = 未手动指定）。
	Manual string
	// CurrentTitle 当前实际生效的标题（手动 > 自动）。
	CurrentTitle string
}

func (s *Server) handleAdminEpisodes(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	entry, ok := s.idx.Get(id)
	if !ok {
		http.Error(w, "条目不存在（索引可能尚未刷新，可访问 /refresh）", http.StatusNotFound)
		return
	}

	var overrides map[string]float64
	if s.epmap != nil {
		overrides = s.epmap.Overrides(entry.ID)
	}

	rows := make([]adminEpisodeRow, 0, len(entry.Episodes))
	for _, ep := range entry.Episodes {
		row := adminEpisodeRow{FileName: ep.FileName, AutoTitle: "—", CurrentTitle: ep.Title}
		if n, ok := episode.Parse(ep.FileName); ok {
			row.AutoTitle = episode.FormatEpisodeNumber(n)
		}
		if n, ok := overrides[ep.FileName]; ok {
			row.Manual = strconv.FormatFloat(n, 'f', -1, 64)
		}
		rows = append(rows, row)
	}

	names := s.mapping.NamesFor(entry.ID)
	title := entry.CleanedName
	if len(names) > 0 {
		title = names[0]
	}

	render(w, adminEpisodesTmpl, map[string]any{
		"ID":      entry.ID,
		"DirName": entry.DirName,
		"Title":   title,
		"PlayURL": playURL(entry.ID, 0),
		"Rows":    rows,
	})
}

type adminEpisodesSaveRequest struct {
	Dir string `json:"dir"`
	// Episodes 文件名 → 集数字符串；空字符串或缺失表示该文件不做手动指定。
	Episodes map[string]string `json:"episodes"`
}

func (s *Server) handleAdminEpisodesSave(w http.ResponseWriter, r *http.Request) {
	if !requireJSONNoCSRF(w, r) {
		return
	}
	if s.epmap == nil {
		http.Error(w, "未启用手动集数映射", http.StatusInternalServerError)
		return
	}

	var req adminEpisodesSaveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "请求体不是有效 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	entry, ok := s.idx.Get(req.Dir)
	if !ok {
		http.Error(w, "dir 不是当前索引中的条目（新增番剧请先 /refresh）", http.StatusBadRequest)
		return
	}

	known := make(map[string]bool, len(entry.Episodes))
	for _, ep := range entry.Episodes {
		known[ep.FileName] = true
	}

	fileToNum := map[string]float64{}
	for file, numStr := range req.Episodes {
		numStr = strings.TrimSpace(numStr)
		if numStr == "" {
			continue
		}
		if !known[file] {
			http.Error(w, fmt.Sprintf("文件 %q 不在该条目中", file), http.StatusBadRequest)
			return
		}
		n, err := strconv.ParseFloat(numStr, 64)
		if err != nil || n < 0 {
			http.Error(w, fmt.Sprintf("文件 %q 的集数 %q 不是有效数字", file, numStr), http.StatusBadRequest)
			return
		}
		fileToNum[file] = n
	}

	if err := s.epmap.SetOverrides(req.Dir, fileToNum); err != nil {
		http.Error(w, "保存失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	// 立刻在内存里重建条目，让播放页马上生效（文件变更的 fsnotify 重载只是兜底）
	s.idx.ApplyOverrides()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ---------- 模板 ----------

func render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		log.Printf("[server] 渲染模板失败: %v", err)
	}
}

var searchTmpl = template.Must(template.New("search").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>搜索 - anime-play</title>
</head>
<body>
<div class="search-result">
{{- range .Items }}
  <div class="anime-item">
    <a class="anime-link" href="{{ .PlayURL }}">
      <span class="anime-title">{{ .Title }}</span>
    </a>
  </div>
{{- end }}
</div>
</body>
</html>
`))

var playTmpl = template.Must(template.New("play").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>{{ .Title }} - anime-play</title>
</head>
<body>
<h1 class="anime-title">{{ .Title }}</h1>
{{- if .Error }}
<p class="play-error">{{ .Error }}</p>
{{- end }}
{{- if .VideoURL }}
<video class="player" src="{{ .VideoURL }}" controls></video>
{{- end }}
<div class="ep-list">
{{- range .Episodes }}
  <a class="ep-link{{ if .Current }} ep-current{{ end }}" href="{{ .URL }}">{{ .Title }}</a>
{{- end }}
</div>
</body>
</html>
`))

var adminTmpl = template.Must(template.New("admin").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>映射管理 - anime-play</title>
<style>
  body { font-family: system-ui, "PingFang SC", "Microsoft YaHei", sans-serif; margin: 0; background: #f5f6f8; color: #222; }
  header { background: #2c3e50; color: #fff; padding: 14px 24px; }
  header h1 { margin: 0; font-size: 18px; }
  header .stats { font-size: 13px; opacity: .85; margin-top: 4px; }
  header a { color: #9fd3ff; }
  main { padding: 16px 24px 60px; max-width: 1100px; margin: 0 auto; }
  .scan-error { background: #fdecea; color: #b3261e; padding: 8px 12px; border-radius: 6px; margin-bottom: 12px; font-size: 13px; }
  h2.root { font-size: 15px; color: #555; margin: 24px 0 8px; }
  table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 8px; overflow: hidden; box-shadow: 0 1px 2px rgba(0,0,0,.06); }
  th, td { padding: 8px 10px; text-align: left; font-size: 13px; border-bottom: 1px solid #eee; vertical-align: top; }
  th { background: #fafbfc; color: #666; font-weight: 600; }
  td.dir { word-break: break-all; max-width: 380px; }
  td.dir .cleaned { color: #888; font-size: 12px; margin-top: 2px; }
  .badge { display: inline-block; padding: 1px 8px; border-radius: 10px; font-size: 12px; }
  .badge.ok { background: #e6f4ea; color: #137333; }
  .badge.miss { background: #fdecea; color: #b3261e; }
  input.names { width: 100%; box-sizing: border-box; padding: 5px 8px; border: 1px solid #ccc; border-radius: 5px; font-size: 13px; }
  button.save { margin-top: 4px; padding: 4px 14px; border: none; border-radius: 5px; background: #2c6fdb; color: #fff; cursor: pointer; font-size: 13px; }
  button.save:disabled { background: #9bb8e8; }
  .saved { color: #137333; font-size: 12px; margin-left: 8px; }
  .save-error { color: #b3261e; font-size: 12px; margin-left: 8px; }
  a.ep-edit { font-size: 12px; }
</style>
</head>
<body>
<header>
  <h1>anime-play 映射管理</h1>
  <div class="stats">
    条目 {{ .Total }} 个 · 已映射 {{ .Mapped }} · 未映射 {{ .Unmapped }} · 上次扫描 {{ .LastScan }} ·
    <a href="/refresh">手动刷新索引</a> · <a href="/search?keyword=">搜索页预览</a>
  </div>
</header>
<main>
{{- if .ScanError }}
  <div class="scan-error">上次扫描存在错误：{{ .ScanError }}</div>
{{- end }}
{{- range .Groups }}
  <h2 class="root">扫描根：{{ .Root }}（{{ len .Entries }} 个条目）</h2>
  <table>
    <thead>
      <tr><th style="width:42%">目录</th><th style="width:60px">状态</th><th style="width:80px">集数</th><th>番剧名 / 别名（逗号分隔，留空 = 删除映射）</th></tr>
    </thead>
    <tbody>
    {{- range .Entries }}
      <tr>
        <td class="dir">{{ .DirName }}<div class="cleaned">兜底名：{{ .Cleaned }}</div></td>
        <td>{{ if .Mapped }}<span class="badge ok">已映射</span>{{ else }}<span class="badge miss">未映射</span>{{ end }}</td>
        <td>{{ .Episodes }} <a class="ep-edit" href="{{ .EpURL }}">编辑</a></td>
        <td>
          <input class="names" type="text" data-dir="{{ .ID }}" value="{{ .Names }}" placeholder="例：孤独摇滚, ぼっち・ざ・ろっく！, Bocchi the Rock">
          <button class="save" type="button">保存</button><span class="msg"></span>
        </td>
      </tr>
    {{- end }}
    </tbody>
  </table>
{{- else }}
  <p>暂无条目。请确认 SCAN_ROOTS 配置正确，然后访问 <a href="/refresh">/refresh</a>。</p>
{{- end }}
</main>
<script>
document.addEventListener('click', async (ev) => {
  const btn = ev.target;
  if (!btn.classList.contains('save')) return;
  const td = btn.closest('td');
  const input = td.querySelector('input.names');
  const msg = td.querySelector('.msg');
  const names = input.value.split(/[,，]/).map(s => s.trim()).filter(Boolean);
  btn.disabled = true;
  msg.textContent = '';
  try {
    const res = await fetch('/admin/save', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'anime-play' },
      body: JSON.stringify({ dir: input.dataset.dir, names })
    });
    if (!res.ok) throw new Error(await res.text());
    msg.className = 'msg saved';
    msg.textContent = '已保存 ✓';
    const badge = btn.closest('tr').querySelector('.badge');
    if (names.length > 0) { badge.className = 'badge ok'; badge.textContent = '已映射'; }
    else { badge.className = 'badge miss'; badge.textContent = '未映射'; }
  } catch (e) {
    msg.className = 'msg save-error';
    msg.textContent = '保存失败: ' + e.message;
  } finally {
    btn.disabled = false;
  }
});
</script>
</body>
</html>
`))

var adminEpisodesTmpl = template.Must(template.New("admin-episodes").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>集数编辑 - {{ .Title }} - anime-play</title>
<style>
  body { font-family: system-ui, "PingFang SC", "Microsoft YaHei", sans-serif; margin: 0; background: #f5f6f8; color: #222; }
  header { background: #2c3e50; color: #fff; padding: 14px 24px; }
  header h1 { margin: 0; font-size: 18px; }
  header .sub { font-size: 13px; opacity: .85; margin-top: 4px; word-break: break-all; }
  header a { color: #9fd3ff; }
  main { padding: 16px 24px 80px; max-width: 1100px; margin: 0 auto; }
  .hint { font-size: 13px; color: #555; margin: 0 0 12px; line-height: 1.7; }
  table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 8px; overflow: hidden; box-shadow: 0 1px 2px rgba(0,0,0,.06); }
  th, td { padding: 8px 10px; text-align: left; font-size: 13px; border-bottom: 1px solid #eee; vertical-align: middle; }
  th { background: #fafbfc; color: #666; font-weight: 600; }
  td.file { word-break: break-all; }
  td.auto, td.cur { white-space: nowrap; color: #555; }
  input.ep-num { width: 90px; padding: 5px 8px; border: 1px solid #ccc; border-radius: 5px; font-size: 13px; }
  .toolbar { margin-top: 14px; display: flex; align-items: center; gap: 12px; }
  button.save-all { padding: 7px 22px; border: none; border-radius: 6px; background: #2c6fdb; color: #fff; cursor: pointer; font-size: 14px; }
  button.save-all:disabled { background: #9bb8e8; }
  .msg.saved { color: #137333; font-size: 13px; }
  .msg.save-error { color: #b3261e; font-size: 13px; }
</style>
</head>
<body>
<header>
  <h1>集数编辑：{{ .Title }}</h1>
  <div class="sub">{{ .ID }} · <a href="/admin">← 返回映射管理</a> · <a href="{{ .PlayURL }}">播放页预览</a></div>
</header>
<main>
  <p class="hint">
    在「手动集数」一栏填数字（支持小数，如 12.5）即可覆盖自动解析；留空 = 不手动指定，继续用自动解析结果。
    同一条目内两个文件不能填同一个集数。保存写入 episodes.yaml 并立刻生效。
  </p>
  <table>
    <thead>
      <tr><th>文件名</th><th style="width:110px">自动解析</th><th style="width:110px">当前生效</th><th style="width:130px">手动集数</th></tr>
    </thead>
    <tbody>
    {{- range .Rows }}
      <tr>
        <td class="file">{{ .FileName }}</td>
        <td class="auto">{{ .AutoTitle }}</td>
        <td class="cur">{{ .CurrentTitle }}</td>
        <td><input class="ep-num" type="text" inputmode="decimal" data-file="{{ .FileName }}" value="{{ .Manual }}" placeholder="留空=自动"></td>
      </tr>
    {{- end }}
    </tbody>
  </table>
  <div class="toolbar">
    <button class="save-all" type="button">保存全部</button><span class="msg"></span>
  </div>
</main>
<script>
const DIR = {{ .ID }};
document.querySelector('.save-all').addEventListener('click', async (ev) => {
  const btn = ev.target;
  const msg = document.querySelector('.toolbar .msg');
  const episodes = {};
  document.querySelectorAll('input.ep-num').forEach(inp => {
    const v = inp.value.trim();
    if (v !== '') episodes[inp.dataset.file] = v;
  });
  btn.disabled = true;
  msg.textContent = '';
  try {
    const res = await fetch('/admin/episodes/save', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'anime-play' },
      body: JSON.stringify({ dir: DIR, episodes })
    });
    if (!res.ok) throw new Error(await res.text());
    msg.className = 'msg saved';
    msg.textContent = '已保存 ✓ 刷新页面可查看新的「当前生效」列';
  } catch (e) {
    msg.className = 'msg save-error';
    msg.textContent = '保存失败: ' + e.message;
  } finally {
    btn.disabled = false;
  }
});
</script>
</body>
</html>
`))
