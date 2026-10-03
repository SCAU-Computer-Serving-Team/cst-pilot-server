// 遥测接收端 · 入口。
//
// 用法：
//   cst-pilot-server serve              常驻服务（默认）
//   cst-pilot-server export-csv [out]   导出会话明细 CSV
//   cst-pilot-server delete --mid M1024 | --device <id>
//   cst-pilot-server rollup             手动重算 daily_rollup
//
// 环境变量：
//   TELEMETRY_PORT    监听端口（默认 8787）
//   TELEMETRY_HOST    监听地址（默认 127.0.0.1）
//   TELEMETRY_DB      SQLite 文件路径（默认 ./telemetry.db）
//   OA_INTROSPECT_URL OA 内省接口；未配置时用桩（令牌格式 stub-<mid>-<device>）
//   OA_SERVICE_TOKEN  内省用的服务凭据
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

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
	}
	return
}

func serve() error {
	port, host, dbPath, auth := envOptions()
	// 2G 内存机器：给 GC 一个 24MiB 的软上限，超限时更激进回收而非无限堆积。
	debug.SetMemoryLimit(24 << 20)

	st, err := openStore(dbPath)
	if err != nil {
		return err
	}
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
	go func() {
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
	fmt.Printf("telemetry receiver listening on http://%s:%s (db: %s)\n", host, port, dbPath)
	if auth.URL == "" {
		fmt.Println("identity: stub mode (token format stub-<mid>-<device>)")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		server.Close()
	}()
	err = server.Serve(ln)
	close(done)
	ticker.Stop()
	st.close()
	if err == http.ErrServerClosed {
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
	case "serve":
		err = serve()
	case "export-csv":
		err = exportCsv(args[1:])
	case "delete":
		err = deleteCmd(args[1:])
	case "rollup":
		err = rollupCmd()
	default:
		err = fmt.Errorf("unknown command %q (serve | export-csv | delete | rollup)", command)
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
