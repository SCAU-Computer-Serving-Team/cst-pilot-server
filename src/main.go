// 遥测接收端 · 入口。
//
// 用法：
//
//	cst-pilot-server serve              常驻服务（默认）
//	cst-pilot-server export-csv [out]   导出会话明细 CSV
//	cst-pilot-server delete --mid M1024 | --device <id>
//	cst-pilot-server rollup             手动重算 daily_rollup
//
// 环境变量：
//
//	TELEMETRY_PORT    监听端口（默认 8787）
//	TELEMETRY_HOST    监听地址（默认 127.0.0.1）
//	TELEMETRY_DB      SQLite 文件路径（默认 ./telemetry.db）
//	OA_INTROSPECT_URL 生产必需的 OA 内省接口
//	OA_SERVICE_TOKEN  内省服务凭据
//	TELEMETRY_ALLOW_STUB 测试身份开关，仅本地联调使用
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

var buildVersion = "dev"
var buildTime = "unknown"

func envOptions() (port, host, dbPath string, auth authOptions) {
	port = os.Getenv("TELEMETRY_PORT")
	if port == "" {
		port = "8787"
	}
	host = os.Getenv("TELEMETRY_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	dbPath = os.Getenv("TELEMETRY_DB")
	if dbPath == "" {
		dbPath = "telemetry.db"
	}
	auth = authOptions{
		URL:          os.Getenv("OA_INTROSPECT_URL"),
		ServiceToken: os.Getenv("OA_SERVICE_TOKEN"),
		AllowStub:    os.Getenv("TELEMETRY_ALLOW_STUB") == "1",
	}
	return
}

func serve() error {
	port, host, dbPath, auth := envOptions()
	if err := validateAuthOptions(auth); err != nil {
		return err
	}
	// 2G 内存机器：给 GC 一个 24MiB 的软上限，超限时更激进回收而非无限堆积。
	debug.SetMemoryLimit(24 << 20)

	st, err := openStore(dbPath)
	if err != nil {
		return err
	}
	defer st.close()
	server := newTelemetryServer(serverOptions{store: st, auth: newIntrospector(auth)})

	daily := func() {
		if err := st.runRetention(); err != nil {
			fmt.Fprintf(os.Stderr, "daily retention failed: %v\n", err)
		}
		if err := st.refreshRollup(); err != nil {
			fmt.Fprintf(os.Stderr, "daily rollup failed: %v\n", err)
		}
	}
	daily() // 启动先跑一轮。
	ticker := time.NewTicker(24 * time.Hour)
	done := make(chan struct{})
	var maintenance sync.WaitGroup
	maintenance.Add(1)
	defer func() { close(done); ticker.Stop(); maintenance.Wait() }()
	go func() {
		defer maintenance.Done()
		for {
			select {
			case <-ticker.C:
				daily()
			case <-done:
				return
			}
		}
	}()

	ln, err := net.Listen("tcp", host+":"+port)
	if err != nil {
		return err
	}
	fmt.Printf("telemetry receiver %s listening on http://%s:%s (db: %s)\n", buildVersion, host, port, dbPath)
	if auth.URL == "" {
		fmt.Println("identity: stub mode (token format stub-<mid>-<device>)")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	shutdownDone := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(shutdownDone)
		select {
		case <-signals:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(ctx); err != nil {
				server.Close()
			}
		case <-stop:
		}
	}()
	err = server.Serve(ln)
	if err == http.ErrServerClosed {
		<-shutdownDone
		return nil
	}
	return err
}

func main() {
	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}
	var err error
	switch command {
	case "version":
		fmt.Printf("%s %s\n", buildVersion, buildTime)
	case "serve":
		err = serve()
	case "export-csv":
		err = exportCsv(args[1:])
	case "delete":
		err = deleteCmd(args[1:])
	case "rollup":
		err = rollupCmd()
	default:
		err = fmt.Errorf("unknown command %q (serve | version | export-csv | delete | rollup)", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func exportCsv(args []string) error {
	_, _, dbPath, _ := envOptions()
	st, err := openStore(dbPath)
	if err != nil {
		return err
	}
	csv, err := st.exportSessionsCsv()
	st.close()
	if err != nil {
		return err
	}
	out := "telemetry-sessions.csv"
	if len(args) > 0 {
		out = args[0]
	}
	if err := os.WriteFile(out, []byte(csv), 0o644); err != nil {
		return err
	}
	fmt.Printf("exported %d rows to %s\n", strings.Count(csv, "\n"), out)
	return nil
}

func deleteCmd(args []string) error {
	mid := flagValue(args, "--mid")
	device := flagValue(args, "--device")
	if mid == "" && device == "" {
		return fmt.Errorf("delete requires --mid or --device")
	}
	_, _, dbPath, _ := envOptions()
	st, err := openStore(dbPath)
	if err != nil {
		return err
	}
	var removed int64
	if mid != "" {
		removed, err = st.deleteByMid(mid)
	} else {
		removed, err = st.deleteByDevice(device)
	}
	if err == nil {
		err = st.refreshRollup()
	}
	st.close()
	if err != nil {
		return err
	}
	fmt.Printf("deleted %d session rows\n", removed)
	return nil
}

func rollupCmd() error {
	_, _, dbPath, _ := envOptions()
	st, err := openStore(dbPath)
	if err != nil {
		return err
	}
	err = st.refreshRollup()
	st.close()
	if err != nil {
		return err
	}
	fmt.Println("rollup refreshed")
	return nil
}

func flagValue(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
