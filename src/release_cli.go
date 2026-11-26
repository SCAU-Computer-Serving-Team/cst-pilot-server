// 发行包分发 · CLI 管理子命令（支持在线走本机接口与 --offline 离线操作）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func releaseCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("release requires subcommand (list | publish | activate | gc)")
	}
	sub := args[0]
	rest := args[1:]
	offline := hasFlag(rest, "--offline")
	endpoint := strings.TrimRight(flagValue(rest, "--endpoint"), "/")
	if endpoint == "" {
		port, host, _, _ := envOptions()
		if host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		endpoint = "http://" + host + ":" + port
	}

	switch sub {
	case "list":
		if offline {
			return releaseListOffline()
		}
		return releaseHTTPJSON(http.MethodGet, endpoint+"/v1/admin/releases", nil, 30*time.Second)
	case "publish":
		manifestPath := flagValue(rest, "--manifest")
		archivePath := flagValue(rest, "--archive")
		channel := flagValue(rest, "--channel")
		if channel == "" {
			channel = "stable"
		}
		activate := !hasFlag(rest, "--no-activate")
		if manifestPath == "" || archivePath == "" {
			return fmt.Errorf("release publish requires --manifest <path> and --archive <path>")
		}
		if offline {
			return releasePublishOffline(manifestPath, archivePath, channel, activate)
		}
		return releasePublishOnline(endpoint, manifestPath, archivePath, channel, activate)
	case "activate":
		channel := flagValue(rest, "--channel")
		version := flagValue(rest, "--version")
		if channel == "" || version == "" {
			return fmt.Errorf("release activate requires --channel <stable|beta> and --version <ver>")
		}
		if offline {
			return releaseActivateOffline(channel, version)
		}
		body, _ := json.Marshal(map[string]string{"version": version})
		return releaseHTTPJSON(http.MethodPut, endpoint+"/v1/admin/releases/channels/"+url.PathEscape(channel), body, 30*time.Second)
	case "gc":
		if offline {
			return releaseGCOffline()
		}
		return releaseHTTPJSON(http.MethodPost, endpoint+"/v1/admin/releases/gc", nil, 60*time.Second)
	default:
		return fmt.Errorf("unknown release subcommand %q (list | publish | activate | gc)", sub)
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func openOfflineReleaseStore() (*releaseStore, error) {
	cfg, err := loadReleaseConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("RELEASE_STORAGE_DIR is required for --offline release commands")
	}
	return openReleaseStore(cfg)
}

func releaseListOffline() error {
	st, err := openOfflineReleaseStore()
	if err != nil {
		return err
	}
	return printIndentedJSON(st.inventorySnapshot())
}

func releasePublishOffline(manifestPath, archivePath, channel string, activate bool) error {
	st, err := openOfflineReleaseStore()
	if err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	archiveFile, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archiveFile.Close()
	res, err := st.publishStreaming(channel, activate, manifestBytes, func() (io.Reader, error) {
		return archiveFile, nil
	}, "cli-offline")
	if err != nil {
		return err
	}
	return printIndentedJSON(res)
}

func releaseActivateOffline(channel, version string) error {
	st, err := openOfflineReleaseStore()
	if err != nil {
		return err
	}
	rec, err := st.activateChannel(channel, version, "cli-offline")
	if err != nil {
		return err
	}
	return printIndentedJSON(map[string]any{"channel": channel, "record": rec})
}

func releaseGCOffline() error {
	st, err := openOfflineReleaseStore()
	if err != nil {
		return err
	}
	evicted, err := st.runGC("cli-offline")
	if err != nil {
		return err
	}
	return printIndentedJSON(map[string]any{"evicted": evicted})
}

func releaseAdminTokenFromEnv() (string, error) {
	tok := strings.TrimSpace(os.Getenv("RELEASE_ADMIN_TOKEN"))
	if tok == "" {
		return "", fmt.Errorf("RELEASE_ADMIN_TOKEN is required for online release CLI commands")
	}
	return tok, nil
}

func releaseHTTPJSON(method, targetURL string, body []byte, timeout time.Duration) error {
	token, err := releaseAdminTokenFromEnv()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, targetURL, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err == nil {
		return printIndentedJSON(parsed)
	}
	fmt.Println(string(raw))
	return nil
}

func releasePublishOnline(endpoint, manifestPath, archivePath, channel string, activate bool) error {
	token, err := releaseAdminTokenFromEnv()
	if err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	archiveFile, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archiveFile.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		p1, err := mw.CreateFormFile("manifest", filepath.Base(manifestPath))
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := p1.Write(manifestBytes); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		p2, err := mw.CreateFormFile("archive", filepath.Base(archivePath))
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(p2, archiveFile); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := mw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()

	u := fmt.Sprintf("%s/v1/admin/releases?channel=%s&activate=%t", endpoint, url.QueryEscape(channel), activate)
	req, err := http.NewRequest(http.MethodPost, u, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := &http.Client{
		Timeout:       releaseUploadTotalTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err == nil {
		return printIndentedJSON(parsed)
	}
	fmt.Println(string(raw))
	return nil
}

func printIndentedJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
