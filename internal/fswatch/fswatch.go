// Package fswatch 提供「监听单个文件变更」的小工具，供映射文件 / 集数配置热重载使用。
//
// 实现要点：
//   - 监听文件所在【目录】而不是文件本身：原子写（临时文件 + rename）会换 inode，
//     基于单文件的监听在替换后会失效；
//   - 带防抖：编辑器保存往往触发多个事件，合并为一次回调；
//   - 回调中自行处理读取失败（本包只负责通知）。
package fswatch

import (
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchFile 监听 path 的变更，变更经 debounce 防抖后调用 onChange。
// 返回停止函数；启动失败返回 error。
func WatchFile(path string, debounce time.Duration, onChange func()) (stop func(), err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("创建 fsnotify watcher 失败: %w", err)
	}
	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, fmt.Errorf("监听目录 %s 失败: %w", dir, err)
	}

	stopCh := make(chan struct{})
	go loop(w, filepath.Base(path), debounce, onChange, stopCh)

	var stopped bool
	return func() {
		if stopped {
			return
		}
		stopped = true
		close(stopCh)
		w.Close()
	}, nil
}

func loop(w *fsnotify.Watcher, target string, debounce time.Duration, onChange func(), stopCh <-chan struct{}) {
	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-stopCh:
			return
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != target {
				continue
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)
			}
			timerC = timer.C
		case <-timerC:
			timerC = nil
			onChange()
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("[fswatch] fsnotify 错误: %v", err)
		}
	}
}
