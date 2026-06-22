package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadDotEnv 读取 .env 文件并把其中的键值写入进程环境变量。
// 已存在的环境变量优先（不覆盖），即真实 env > .env 文件。
//
// 支持的语法：
//   - KEY=VALUE，每行一条
//   - 行首 # 为注释；空行忽略
//   - 可选的 export 前缀
//   - 值两侧的单/双引号会被去掉；# 在未加引号的值中视为行尾注释
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s 第 %d 行不是 KEY=VALUE 格式: %q", path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("%s 第 %d 行缺少变量名", path, lineNo)
		}

		// 去引号；未加引号时去掉行尾 # 注释
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}

		if _, exists := os.LookupEnv(key); exists {
			continue // 真实环境变量优先
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
