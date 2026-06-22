package mapping

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSetNamesPersistsAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SetNames("/anime/Bocchi", []string{"孤独摇滚", "Bocchi the Rock", " ", "孤独摇滚"}); err != nil {
		t.Fatal(err)
	}

	// 内存生效（去重、去空白）
	names := s.NamesFor("/anime/Bocchi")
	if len(names) != 2 || names[0] != "孤独摇滚" || names[1] != "Bocchi the Rock" {
		t.Fatalf("NamesFor = %v", names)
	}

	// 磁盘生效
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) != 1 || f.Entries[0].Dir != "/anime/Bocchi" {
		t.Fatalf("file entries = %+v", f.Entries)
	}

	// 目录里不应残留临时文件
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("残留临时文件: %v", matches)
	}

	// 空 names = 删除映射
	if err := s.SetNames("/anime/Bocchi", nil); err != nil {
		t.Fatal(err)
	}
	if got := s.NamesFor("/anime/Bocchi"); got != nil {
		t.Fatalf("删除后仍返回 %v", got)
	}
}

func TestExternalEditHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mapping.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Watch(); err != nil {
		t.Fatal(err)
	}

	// 模拟外部工具的原子写：写临时文件 + rename
	write := func(content string) {
		tmp := filepath.Join(dir, "ext.tmp")
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"entries":[{"dir":"/anime/Frieren","names":["葬送的芙莉莲"]}]}`)
	waitFor(t, func() bool { return len(s.NamesFor("/anime/Frieren")) == 1 })

	// 写入损坏 JSON：应保留旧映射
	write(`{"entries":[{"dir":"/anime/Frieren","na`)
	time.Sleep(600 * time.Millisecond)
	if got := s.NamesFor("/anime/Frieren"); len(got) != 1 || got[0] != "葬送的芙莉莲" {
		t.Fatalf("损坏文件后旧映射丢失: %v", got)
	}

	// 再次写入有效内容：恢复更新
	write(`{"entries":[{"dir":"/anime/Frieren","names":["葬送的芙莉莲","Sousou no Frieren"]}]}`)
	waitFor(t, func() bool { return len(s.NamesFor("/anime/Frieren")) == 2 })
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
