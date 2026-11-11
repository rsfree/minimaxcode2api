// cmd/server minimaxcode2api 入口：加载配置、组装账号池与 HTTP 前门。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/config"
	"github.com/linguo2625469/minimaxcode2api/internal/oauth"
	"github.com/linguo2625469/minimaxcode2api/internal/panel"
	"github.com/linguo2625469/minimaxcode2api/internal/pool"
	"github.com/linguo2625469/minimaxcode2api/internal/reqlog"
	"github.com/linguo2625469/minimaxcode2api/internal/server"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
	"github.com/linguo2625469/minimaxcode2api/internal/upstream"
	"github.com/linguo2625469/minimaxcode2api/internal/usage"
)

func main() {
	configPath := flag.String("config", "./config.json", "配置文件路径")
	flag.Parse()

	log.SetFlags(log.LstdFlags)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	holder := config.NewHolder(cfg)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	accounts, err := store.Open(filepath.Join(cfg.DataDir, "accounts.json"))
	if err != nil {
		log.Fatalf("加载账号失败: %v", err)
	}
	usageTracker, err := usage.Open(filepath.Join(cfg.DataDir, "usage.json"))
	if err != nil {
		log.Fatalf("加载用量失败: %v", err)
	}

	oc := oauth.New(holder)
	up := upstream.New(holder)
	pl := pool.New(holder, accounts)
	maxLog := cfg.Logging.MaxEntries
	if maxLog <= 0 {
		maxLog = 1000
	}
	logs := reqlog.New(maxLog)

	srv := server.New(holder, accounts, pl, oc, up, usageTracker, logs)
	panel.New(panel.Deps{
		Cfg:     holder,
		Store:   accounts,
		Pool:    pl,
		OAuth:   oc,
		Up:      up,
		Usage:   usageTracker,
		Logs:    logs,
		Version: server.Version,
		Start:   time.Now(),
	}).Mount(srv.Mux)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		// 流式响应可能很长，不设 WriteTimeout（由上游 ctx 控制）。
		IdleTimeout: 120 * time.Second,
	}

	enabled := 0
	for _, a := range accounts.List() {
		if !a.Disabled {
			enabled++
		}
	}
	log.Printf("minimaxcode2api %s 启动中", server.Version)
	log.Printf("  监听      : %s", cfg.Listen)
	log.Printf("  上游      : %s", cfg.Upstream.BaseURL)
	log.Printf("  账号      : %d 个（启用 %d）", len(accounts.List()), enabled)
	log.Printf("  管理台    : http://%s/panel/", panelAddr(cfg.Listen))
	log.Printf("  数据目录  : %s", cfg.DataDir)
	if len(accounts.List()) == 0 {
		log.Printf("  提示      : 还没有账号，打开管理台用「设备授权」添加")
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Printf("收到退出信号，正在收尾…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if err := usageTracker.Save(); err != nil {
		log.Printf("保存用量失败: %v", err)
	}
	if err := accounts.Save(); err != nil {
		log.Printf("保存账号失败: %v", err)
	}
	log.Printf("已退出")
}

// panelAddr 把监听地址转成面板可点的 URL。
//
// ":7864" 这种只写端口的，补 127.0.0.1；"0.0.0.0:7864" 换成 127.0.0.1
// （本机访问用）；其它（如 "192.168.1.2:7864"）原样用。
func panelAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	if host, port, ok := strings.Cut(listen, ":"); ok {
		switch host {
		case "0.0.0.0", "", "[::]":
			return "127.0.0.1:" + port
		}
	}
	return listen
}
