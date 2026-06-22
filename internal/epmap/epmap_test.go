package epmap

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sampleYAML = `# 手动集数映射
entries:
  - dir: /anime/Bocchi
    episodes:
      1: "ED-MV.mp4"
      12.5: Special.mkv
      "3": "quoted-key.mkv"
  - dir: /anime/Empty
    episodes: {}
`

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "episodes.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ov := s.Overrides("/anime/Bocchi")
	if len(ov) != 3 {
		t.Fatalf("Overrides = %v", ov)
	}
	if ov["ED-MV.mp4"] != 1 || ov["Special.mkv"] != 12.5 || ov["quoted-key.mkv"] != 3 {
		t.Fatalf("Overrides 数值不对: %v", ov)
	}
	if s.Overrides("/anime/Empty") != nil {
		t.Fatal("空 episodes 的条目应返回 nil")
	}
	if s.Overrides("/anime/Other") != nil {
		t.Fatal("未配置的条目应返回 nil")
	}
}

func TestMissingFileIsOK(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "no-such.yaml"))
	if err != nil {
		t.Fatalf("文件不存在不应报错: %v", err)
	}
	defer s.Close()
	if s.Overrides("/anime/x") != nil {
		t.Fatal("应为空数据")
	}
}

func TestInvalidEpisodeKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "episodes.yaml")
	bad := "entries:\n  - dir: /a\n    episodes:\n      abc: foo.mkv\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path); err == nil {
		t.Fatal("非数字集数键应报错")
	}
}

func TestSetOverridesPersistsAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "episodes.yaml")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 重复集数：拒绝
	if err := s.SetOverrides("/anime/X", map[string]float64{"a.mkv": 1, "b.mkv": 1}); err == nil {
		t.Fatal("重复集数应报错")
	}

	if err := s.SetOverrides("/anime/X", map[string]float64{"DISC1_t00.mkv": 1, "Special.mkv": 12.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOverrides("/anime/Y", map[string]float64{"foo.mp4": 3}); err != nil {
		t.Fatal(err)
	}

	// 重新从磁盘加载：内容一致（验证写盘格式可被解析）
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("重新加载写出的 YAML 失败: %v", err)
	}
	defer s2.Close()
	ov := s2.Overrides("/anime/X")
	if len(ov) != 2 || ov["DISC1_t00.mkv"] != 1 || ov["Special.mkv"] != 12.5 {
		t.Fatalf("round-trip 后数据不一致: %v", ov)
	}
	if s2.Overrides("/anime/Y")["foo.mp4"] != 3 {
		t.Fatalf("round-trip 后 /anime/Y 数据不一致: %v", s2.Overrides("/anime/Y"))
	}

	// 清空一个条目
	if err := s.SetOverrides("/anime/Y", nil); err != nil {
		t.Fatal(err)
	}
	s3, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if s3.Overrides("/anime/Y") != nil {
		t.Fatal("清空后磁盘上不应再有 /anime/Y")
	}

	// 目录里不应残留临时文件
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("残留临时文件: %v", matches)
	}
}

func TestHotReloadAndFailureProtection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "episodes.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	changed := make(chan struct{}, 8)
	if err := s.Watch(func() { changed <- struct{}{} }); err != nil {
		t.Fatal(err)
	}

	// 写入损坏 YAML：保留旧数据，且不应触发 onChange
	if err := os.WriteFile(path, []byte("entries:\n  - dir: [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	select {
	case <-changed:
		t.Fatal("损坏文件不应触发 onChange")
	default:
	}
	if ov := s.Overrides("/anime/Bocchi"); len(ov) != 3 {
		t.Fatalf("损坏文件后旧数据丢失: %v", ov)
	}

	// 写入有效内容：更新并触发 onChange
	valid := "entries:\n  - dir: /anime/Bocchi\n    episodes:\n      7: NewFile.mkv\n"
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("有效变更未触发 onChange")
	}
	if ov := s.Overrides("/anime/Bocchi"); len(ov) != 1 || ov["NewFile.mkv"] != 7 {
		t.Fatalf("热重载后数据不对: %v", ov)
	}
}
