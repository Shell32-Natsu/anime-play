// Package mapping 维护「条目目录 → 可搜索番剧名集合」的手动映射。
//
// 特性：
//   - JSON 文件持久化（位于挂载 volume 内）；
//   - 所有写入走「临时文件 + rename」原子写，防止写到一半损坏；
//   - fsnotify 监听映射文件所在【目录】（而不是单个文件，避免原子替换后 inode 失效），
//     带 ~200ms 防抖，外部直接改文件也能热重载；
//   - 重读失败（文件写到一半 / JSON 不完整）时保留旧的内存映射，绝不清空。
package mapping

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Entry 映射文件中的一条记录。
type Entry struct {
	// Dir 条目对应的 OpenList 完整路径（唯一键）。
	Dir string `json:"dir"`
	// Names 该条目的番剧名及所有别名（中文 / 日文 / 罗马音 / 简称）。
	Names []string `json:"names"`
}

type fileFormat struct {
	Entries []Entry `json:"entries"`
}

// Store 内存映射 + 文件持久化。
type Store struct {
	path string

	mu    sync.RWMutex
	byDir map[string][]string

	watcher  *fsnotify.Watcher
	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewStore 创建 Store 并尝试加载已有映射文件；文件不存在时以空映射启动。
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:   path,
		byDir:  map[string][]string{},
		stopCh: make(chan struct{}),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建映射文件目录失败: %w", err)
	}
	if err := s.reload(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("[mapping] 映射文件 %s 不存在，以空映射启动", path)
		} else {
			return nil, err
		}
	}
	return s, nil
}

// reload 从磁盘读入映射；失败时返回 error 且不修改内存状态。
func (s *Store) reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("解析映射文件失败（保留旧映射）: %w", err)
	}
	byDir := make(map[string][]string, len(f.Entries))
	for _, e := range f.Entries {
		names := normalizeNames(e.Names)
		if e.Dir == "" || len(names) == 0 {
			continue
		}
		byDir[e.Dir] = names
	}
	s.mu.Lock()
	s.byDir = byDir
	s.mu.Unlock()
	log.Printf("[mapping] 已加载 %d 条映射", len(byDir))
	return nil
}

// NamesFor 返回某条目的映射 names；未映射返回 nil。
func (s *Store) NamesFor(dir string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := s.byDir[dir]
	if names == nil {
		return nil
	}
	out := make([]string, len(names))
	copy(out, names)
	return out
}

// SetNames 更新某条目的 names（空列表表示删除映射），先更新内存再原子写盘。
func (s *Store) SetNames(dir string, names []string) error {
	names = normalizeNames(names)

	s.mu.Lock()
	if len(names) == 0 {
		delete(s.byDir, dir)
	} else {
		s.byDir[dir] = names
	}
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	return s.writeFile(snapshot)
}

// snapshotLocked 调用方需持有 s.mu。
func (s *Store) snapshotLocked() []Entry {
	entries := make([]Entry, 0, len(s.byDir))
	for dir, names := range s.byDir {
		ns := make([]string, len(names))
		copy(ns, names)
		entries = append(entries, Entry{Dir: dir, Names: ns})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Dir < entries[j].Dir })
	return entries
}

// writeFile 原子写盘：写临时文件 → rename。
func (s *Store) writeFile(entries []Entry) error {
	data, err := json.MarshalIndent(fileFormat{Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".mapping-*.json.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后变成 no-op

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("替换映射文件失败: %w", err)
	}
	return nil
}

// Watch 启动 fsnotify 监听（监听文件所在目录），外部修改映射文件后自动热重载。
func (s *Store) Watch() error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("创建 fsnotify watcher 失败: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := w.Add(dir); err != nil {
		w.Close()
		return fmt.Errorf("监听目录 %s 失败: %w", dir, err)
	}
	s.watcher = w

	go s.watchLoop()
	log.Printf("[mapping] 正在监听 %s 的变更（热重载）", s.path)
	return nil
}

const debounceWindow = 200 * time.Millisecond

func (s *Store) watchLoop() {
	target := filepath.Base(s.path)
	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-s.stopCh:
			return
		case event, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != target {
				continue
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			// 防抖：~200ms 内的连续事件合并为一次重读
			if timer == nil {
				timer = time.NewTimer(debounceWindow)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounceWindow)
			}
			timerC = timer.C
		case <-timerC:
			timerC = nil
			if err := s.reload(); err != nil {
				// 失败保护：保留旧映射，等待下次有效变更
				log.Printf("[mapping] 热重载失败: %v", err)
			}
		case err, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[mapping] fsnotify 错误: %v", err)
		}
	}
}

// Close 停止监听。
func (s *Store) Close() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.watcher != nil {
			s.watcher.Close()
		}
	})
}

func normalizeNames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = trimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// trimSpace 去掉首尾空白（含全角空格）。
func trimSpace(s string) string {
	return strings.Trim(s, " \t\r\n　")
}
