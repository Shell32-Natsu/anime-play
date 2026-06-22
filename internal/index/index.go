// Package index 负责扫描 OpenList 目录树、维护内存条目索引，以及视频直链的短 TTL 缓存。
//
// 远程网盘最小读写原则：
//   - 扫描期只调 fs/list（refresh=false），零 fs/get；集数全靠文件名解析；
//   - 搜索 / 列集数只读内存索引，按 REFRESH_INTERVAL 定时刷新或 /refresh 手动刷新；
//   - fs/get 只在播放某一集时调用一次，结果按 RAWURL_CACHE_TTL 短期缓存。
package index

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/donnyxia/anime-play/internal/episode"
	"github.com/donnyxia/anime-play/internal/openlist"
)

// maxScanDepth 单个扫描根之下的最大递归深度（防止异常目录结构导致扫描失控）。
const maxScanDepth = 8

// Episode 条目中的一集。
type Episode struct {
	// FileName 原始文件名。
	FileName string
	// Path OpenList 完整路径。
	Path string
	// Number 解析出的集数；HasNumber 为 false 时无意义。
	Number    float64
	HasNumber bool
	// Title 显示标题：「第 N 话」，解析不到集数时回退原文件名。
	Title string
}

// Entry 一个条目（= 一季），即「直接包含视频文件的文件夹」。
type Entry struct {
	// ID 即 OpenList 完整路径，跨多个扫描根不会冲突。
	ID string
	// Root 该条目来自哪个扫描根（/admin 按根分组显示用）。
	Root string
	// DirName 文件夹名（最后一段）。
	DirName string
	// CleanedName 清洗后的目录名，未映射时的搜索兜底。
	CleanedName string
	// Episodes 集数列表：有集数的按集数升序在前，解析不到的按文件名排在末尾。
	Episodes []Episode
}

// Index 内存条目索引。
type Index struct {
	client *openlist.Client
	roots  []string

	mu        sync.RWMutex
	entries   []*Entry
	byID      map[string]*Entry
	lastScan  time.Time
	scanError error

	rawURLTTL time.Duration
	urlMu     sync.Mutex
	urlCache  map[string]cachedURL

	scanMu sync.Mutex // 防止并发触发多次扫描
}

type cachedURL struct {
	url     string
	expires time.Time
}

// New 创建索引。
func New(client *openlist.Client, roots []string, rawURLTTL time.Duration) *Index {
	return &Index{
		client:    client,
		roots:     roots,
		byID:      map[string]*Entry{},
		rawURLTTL: rawURLTTL,
		urlCache:  map[string]cachedURL{},
	}
}

// Scan 重新扫描所有根路径并整体替换内存索引。
// 失败时返回 error；若部分根失败、部分成功，成功部分仍会写入索引。
func (ix *Index) Scan(ctx context.Context) error {
	ix.scanMu.Lock()
	defer ix.scanMu.Unlock()

	start := time.Now()
	var entries []*Entry
	var errs []string

	for _, root := range ix.roots {
		rootEntries, err := ix.scanDir(ctx, root, root, 0)
		if err != nil {
			log.Printf("[index] 扫描根 %s 失败: %v", root, err)
			errs = append(errs, fmt.Sprintf("%s: %v", root, err))
			continue
		}
		entries = append(entries, rootEntries...)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	byID := make(map[string]*Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}

	ix.mu.Lock()
	ix.entries = entries
	ix.byID = byID
	ix.lastScan = time.Now()
	if len(errs) > 0 {
		ix.scanError = fmt.Errorf("%s", strings.Join(errs, "; "))
	} else {
		ix.scanError = nil
	}
	ix.mu.Unlock()

	log.Printf("[index] 扫描完成：%d 个条目，耗时 %s", len(entries), time.Since(start).Round(time.Millisecond))
	if len(errs) > 0 {
		return fmt.Errorf("部分根扫描失败: %s", strings.Join(errs, "; "))
	}
	return nil
}

// scanDir 递归扫描；某个文件夹直接含视频文件则作为一个条目，同时继续向下找嵌套条目。
func (ix *Index) scanDir(ctx context.Context, root, dir string, depth int) ([]*Entry, error) {
	if depth > maxScanDepth {
		log.Printf("[index] 超过最大深度 %d，跳过 %s", maxScanDepth, dir)
		return nil, nil
	}
	items, err := ix.client.List(ctx, dir)
	if err != nil {
		// 根目录失败向上抛；子目录失败只记录，不让整次扫描挂掉
		if depth == 0 {
			return nil, err
		}
		log.Printf("[index] 列目录 %s 失败（已跳过）: %v", dir, err)
		return nil, nil
	}

	var videos []openlist.Item
	var subdirs []openlist.Item
	for _, it := range items {
		if it.IsDir {
			subdirs = append(subdirs, it)
		} else if episode.IsVideo(it.Name) {
			videos = append(videos, it)
		}
	}

	var entries []*Entry
	if len(videos) > 0 {
		entries = append(entries, buildEntry(root, dir, videos))
	}
	for _, sd := range subdirs {
		child, err := ix.scanDir(ctx, root, joinPath(dir, sd.Name), depth+1)
		if err != nil {
			return nil, err
		}
		entries = append(entries, child...)
	}
	return entries, nil
}

func buildEntry(root, dir string, videos []openlist.Item) *Entry {
	dirName := dir
	parent := ""
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dirName = dir[i+1:]
		parent = dir[:i]
	}

	cleaned := episode.CleanDirName(dirName)
	// 「番剧名/季/视频」结构：季层目录名（Season 1 / 第二季 / SP 等）本身不含番剧名，
	// 兜底搜索名拼上父目录名，否则按番剧名搜索永远搜不到。
	if episode.IsGenericSeasonName(dirName) && parent != "" && parent != root {
		parentName := parent
		if i := strings.LastIndex(parent, "/"); i >= 0 {
			parentName = parent[i+1:]
		}
		cleaned = strings.TrimSpace(episode.CleanDirName(parentName) + " " + cleaned)
	}

	e := &Entry{
		ID:          dir,
		Root:        root,
		DirName:     dirName,
		CleanedName: cleaned,
	}
	for _, v := range videos {
		num, ok := episode.Parse(v.Name)
		ep := Episode{
			FileName:  v.Name,
			Path:      joinPath(dir, v.Name),
			Number:    num,
			HasNumber: ok,
		}
		if ok {
			ep.Title = episode.FormatEpisodeNumber(num)
		} else {
			ep.Title = v.Name
		}
		e.Episodes = append(e.Episodes, ep)
	}
	// 有集数的按集数升序排前面；解析不到的排末尾，按文件名排序
	sort.SliceStable(e.Episodes, func(i, j int) bool {
		a, b := e.Episodes[i], e.Episodes[j]
		if a.HasNumber && b.HasNumber {
			if a.Number != b.Number {
				return a.Number < b.Number
			}
			return a.FileName < b.FileName
		}
		if a.HasNumber != b.HasNumber {
			return a.HasNumber
		}
		return a.FileName < b.FileName
	})
	return e
}

func joinPath(dir, name string) string {
	return strings.TrimRight(dir, "/") + "/" + name
}

// Entries 返回全部条目快照。
func (ix *Index) Entries() []*Entry {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]*Entry, len(ix.entries))
	copy(out, ix.entries)
	return out
}

// Get 按条目 ID（OpenList 路径）取条目。
func (ix *Index) Get(id string) (*Entry, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e, ok := ix.byID[id]
	return e, ok
}

// Status 返回上次扫描时间与错误（/admin 显示用）。
func (ix *Index) Status() (time.Time, int, error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.lastScan, len(ix.entries), ix.scanError
}

// RawURL 取某个文件的签名直链：TTL 内复用缓存，过期才真正调一次 fs/get。
func (ix *Index) RawURL(ctx context.Context, path string) (string, error) {
	now := time.Now()

	ix.urlMu.Lock()
	if c, ok := ix.urlCache[path]; ok && now.Before(c.expires) {
		ix.urlMu.Unlock()
		return c.url, nil
	}
	ix.urlMu.Unlock()

	url, err := ix.client.GetRawURL(ctx, path)
	if err != nil {
		return "", err
	}

	ix.urlMu.Lock()
	ix.urlCache[path] = cachedURL{url: url, expires: now.Add(ix.rawURLTTL)}
	// 顺手清掉已过期的缓存项，避免无限增长
	for k, c := range ix.urlCache {
		if now.After(c.expires) {
			delete(ix.urlCache, k)
		}
	}
	ix.urlMu.Unlock()
	return url, nil
}

// StartAutoRefresh 启动定时自动刷新，直到 ctx 取消。
func (ix *Index) StartAutoRefresh(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := ix.Scan(ctx); err != nil {
					log.Printf("[index] 自动刷新失败: %v", err)
				}
			}
		}
	}()
}
