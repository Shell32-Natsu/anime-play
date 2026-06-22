// anime-play：把 OpenList 上散乱命名的本地番剧伪装成一个在线番剧站点，
// 让弹幕播放器按「在线站点」的爬取流程拿到视频，从而给本地番剧挂上弹幕。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/donnyxia/anime-play/internal/config"
	"github.com/donnyxia/anime-play/internal/index"
	"github.com/donnyxia/anime-play/internal/mapping"
	"github.com/donnyxia/anime-play/internal/openlist"
	"github.com/donnyxia/anime-play/internal/server"
)

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}
	log.Printf("OpenList: %s, 扫描根: %v, 刷新间隔: %s, 直链缓存 TTL: %s",
		cfg.OpenListBaseURL, cfg.ScanRoots, cfg.RefreshInterval, cfg.RawURLCacheTTL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 番剧名映射：加载 + fsnotify 热重载
	store, err := mapping.NewStore(cfg.MappingFile)
	if err != nil {
		log.Fatalf("加载映射文件失败: %v", err)
	}
	defer store.Close()
	if err := store.Watch(); err != nil {
		log.Fatalf("启动映射文件监听失败: %v", err)
	}

	// 条目索引：启动时扫描一次（失败不退出，可稍后 /refresh），并按间隔自动刷新
	client := openlist.New(cfg.OpenListBaseURL, cfg.OpenListToken)
	idx := index.New(client, cfg.ScanRoots, cfg.RawURLCacheTTL)

	scanCtx, cancelScan := context.WithTimeout(ctx, 10*time.Minute)
	if err := idx.Scan(scanCtx); err != nil {
		log.Printf("启动扫描出现错误（服务继续运行，可访问 /refresh 重试）: %v", err)
	}
	cancelScan()
	idx.StartAutoRefresh(ctx, cfg.RefreshInterval)

	if cfg.AdminToken == "" {
		log.Printf("提示：未设置 ADMIN_TOKEN，/admin 与 /refresh 不做鉴权（仅建议在可信内网这样部署）")
	}

	srv := &http.Server{
		Addr:              ":" + cfg.ListenPort,
		Handler:           server.New(idx, store, cfg.AdminToken).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("监听 :%s（对外端点 /search /play /refresh，管理页 /admin）", cfg.ListenPort)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
	log.Println("已退出")
	os.Exit(0)
}
