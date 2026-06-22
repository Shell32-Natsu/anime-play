// Package epmap 维护「手动集数映射」：在 YAML 文件里为某个条目手动指定 集数 → 文件名，
// 覆盖文件名自动解析的结果。适用于命名混乱、解析不出集数或解析错误的条目。
//
// 文件格式（路径由 EPISODE_MAP_FILE 指定，默认 /data/episodes.yaml）：
//
//	entries:
//	  - dir: /anime/[Lilith-Raws] Bocchi the Rock! [Baha][1080p]
//	    episodes:
//	      1: "[Lilith-Raws] Bocchi the Rock! - 01 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4].mp4"
//	      12: "Bocchi Final.mp4"
//	      12.5: "Special.mkv"
//
// 与 mapping.json 一样：既可以直接编辑文件（fsnotify 监听目录 + 防抖热重载，解析失败保留旧数据），
// 也可以通过 /admin 的集数编辑页修改（先更新内存，再原子写盘）。
// 注意：通过网页保存会重写整个文件，手写的注释不会保留。
package epmap

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/donnyxia/anime-play/internal/fswatch"
)

type fileFormat struct {
	Entries []struct {
		Dir string `yaml:"dir"`
		// Episodes 集数 → 文件名。键允许整数 / 小数（如 12.5），可加引号。
		Episodes map[string]string `yaml:"episodes"`
	} `yaml:"entries"`
}

// Store 手动集数映射的内存数据。
type Store struct {
	path string

	mu sync.RWMutex
	// byDir: 条目目录 → (文件名 → 手动指定的集数)
	byDir map[string]map[string]float64

	stopWatch func()
}

// NewStore 加载 YAML 文件；文件不存在时以空数据启动。
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:  path,
		byDir: map[string]map[string]float64{},
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建集数映射文件目录失败: %w", err)
	}
	if err := s.reload(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("[epmap] 集数映射文件 %s 不存在，跳过（全部走文件名自动解析）", path)
		} else {
			return nil, err
		}
	}
	return s, nil
}

// reload 从磁盘读入；失败时返回 error 且不修改内存数据。
func (s *Store) reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f fileFormat
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("解析集数映射文件失败（保留旧数据）: %w", err)
	}

	byDir := map[string]map[string]float64{}
	count := 0
	for _, e := range f.Entries {
		if e.Dir == "" || len(e.Episodes) == 0 {
			continue
		}
		fileToNum := map[string]float64{}
		for numStr, fileName := range e.Episodes {
			if fileName == "" {
				continue
			}
			n, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return fmt.Errorf("条目 %s 的集数键 %q 不是数字（保留旧数据）", e.Dir, numStr)
			}
			fileToNum[fileName] = n
			count++
		}
		if len(fileToNum) > 0 {
			byDir[e.Dir] = fileToNum
		}
	}

	s.mu.Lock()
	s.byDir = byDir
	s.mu.Unlock()
	log.Printf("[epmap] 已加载 %d 个条目共 %d 条手动集数映射", len(byDir), count)
	return nil
}

// Overrides 返回某条目的手动集数映射（文件名 → 集数）；没有则返回 nil。
func (s *Store) Overrides(dir string) map[string]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.byDir[dir]
	if m == nil {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SetOverrides 更新某条目的手动集数映射（文件名 → 集数；空 map 或 nil 表示删除该条目的全部映射），
// 先更新内存再原子写盘。同一条目内不允许两个文件映射到同一个集数（YAML 格式是 集数→文件，无法表达）。
func (s *Store) SetOverrides(dir string, fileToNum map[string]float64) error {
	if dir == "" {
		return errors.New("dir 不能为空")
	}
	seen := map[float64]string{}
	for f, n := range fileToNum {
		if other, dup := seen[n]; dup {
			return fmt.Errorf("集数 %s 被指定给了多个文件（%q 与 %q）", strconv.FormatFloat(n, 'f', -1, 64), other, f)
		}
		seen[n] = f
	}

	s.mu.Lock()
	if len(fileToNum) == 0 {
		delete(s.byDir, dir)
	} else {
		cp := make(map[string]float64, len(fileToNum))
		for f, n := range fileToNum {
			cp[f] = n
		}
		s.byDir[dir] = cp
	}
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	return s.writeFile(snapshot)
}

// writeFileFormat 是写盘用的结构（集数 → 文件名，键用 float64 以输出 1 / 12.5 这种数字键）。
type writeFileFormat struct {
	Entries []writeEntry `yaml:"entries"`
}

type writeEntry struct {
	Dir      string             `yaml:"dir"`
	Episodes map[float64]string `yaml:"episodes"`
}

// snapshotLocked 调用方需持有 s.mu。
func (s *Store) snapshotLocked() []writeEntry {
	entries := make([]writeEntry, 0, len(s.byDir))
	for dir, fileToNum := range s.byDir {
		eps := make(map[float64]string, len(fileToNum))
		for f, n := range fileToNum {
			eps[n] = f
		}
		entries = append(entries, writeEntry{Dir: dir, Episodes: eps})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Dir < entries[j].Dir })
	return entries
}

// writeFile 原子写盘：写临时文件 → rename。
func (s *Store) writeFile(entries []writeEntry) error {
	header := "# anime-play 手动集数映射（集数 → 文件名）。此文件可由 /admin 集数编辑页重写，注释不会保留。\n"
	data, err := yaml.Marshal(writeFileFormat{Entries: entries})
	if err != nil {
		return err
	}
	data = append([]byte(header), data...)

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".episodes-*.yaml.tmp")
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
		return fmt.Errorf("替换集数映射文件失败: %w", err)
	}
	return nil
}

// Watch 监听 YAML 文件变更并热重载，重载成功后调用 onChange（用于让索引重建集数列表）。
func (s *Store) Watch(onChange func()) error {
	stop, err := fswatch.WatchFile(s.path, 200*time.Millisecond, func() {
		if err := s.reload(); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				log.Printf("[epmap] 集数映射文件已被删除，保留旧数据")
				return
			}
			log.Printf("[epmap] 热重载失败: %v", err)
			return
		}
		if onChange != nil {
			onChange()
		}
	})
	if err != nil {
		return err
	}
	s.stopWatch = stop
	log.Printf("[epmap] 正在监听 %s 的变更（热重载）", s.path)
	return nil
}

// Close 停止监听。
func (s *Store) Close() {
	if s.stopWatch != nil {
		s.stopWatch()
		s.stopWatch = nil
	}
}
