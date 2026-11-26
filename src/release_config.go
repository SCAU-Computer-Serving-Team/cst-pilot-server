// 发行包分发 · 环境配置与公钥校验。
//
// 未配置 RELEASE_STORAGE_DIR 时模块完全关闭，不影响纯遥测实例（如 timserver_1）。
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultReleaseMaxArchiveBytes    = int64(256 << 20) // 256 MiB
	defaultReleaseMinFreeBytes       = uint64(4 << 30)  // 4 GiB
	defaultReleaseKeepCount          = 5
	defaultReleaseMaxTotalBytes      = int64(1 << 30)  // 1 GiB
	defaultReleaseHourlyBudgetBytes  = int64(10 << 30) // 10 GiB
	defaultReleaseDailyBudgetBytes   = int64(30 << 30) // 30 GiB
	defaultReleaseMaxDirectDownloads = 16
	minReleaseAdminTokenBytes        = 32
)

type releaseConfig struct {
	Enabled            bool
	StorageDir         string
	PublicKey          ed25519.PublicKey
	PublicBaseURL      string
	AdminToken         string
	AccelPrefix        string
	TrustProxy         bool
	MaxArchiveBytes    int64
	MinFreeBytes       uint64
	KeepCount          int
	MaxTotalBytes      int64
	HourlyBudgetBytes  int64
	DailyBudgetBytes   int64
	MaxDirectDownloads int
}

func loadReleaseConfigFromEnv() (releaseConfig, error) {
	storageDir := strings.TrimSpace(os.Getenv("RELEASE_STORAGE_DIR"))
	if storageDir == "" {
		return releaseConfig{Enabled: false}, nil
	}
	cfg := releaseConfig{
		Enabled:            true,
		StorageDir:         filepath.Clean(storageDir),
		PublicBaseURL:      strings.TrimRight(strings.TrimSpace(os.Getenv("RELEASE_PUBLIC_BASE_URL")), "/"),
		AdminToken:         strings.TrimSpace(os.Getenv("RELEASE_ADMIN_TOKEN")),
		AccelPrefix:        strings.TrimRight(strings.TrimSpace(os.Getenv("RELEASE_ACCEL_PREFIX")), "/"),
		TrustProxy:         strings.TrimSpace(os.Getenv("RELEASE_TRUST_PROXY")) == "1",
		MaxArchiveBytes:    defaultReleaseMaxArchiveBytes,
		MinFreeBytes:       defaultReleaseMinFreeBytes,
		KeepCount:          defaultReleaseKeepCount,
		MaxTotalBytes:      defaultReleaseMaxTotalBytes,
		HourlyBudgetBytes:  defaultReleaseHourlyBudgetBytes,
		DailyBudgetBytes:   defaultReleaseDailyBudgetBytes,
		MaxDirectDownloads: defaultReleaseMaxDirectDownloads,
	}

	rawKey := strings.TrimSpace(os.Getenv("RELEASE_PUBLIC_KEY"))
	pubKey, err := parseStrictReleasePublicKey(rawKey)
	if err != nil {
		return releaseConfig{}, fmt.Errorf("invalid RELEASE_PUBLIC_KEY: %w", err)
	}
	cfg.PublicKey = pubKey

	if v, ok, err := parseEnvInt64("RELEASE_MAX_ARCHIVE_BYTES"); err != nil {
		return releaseConfig{}, err
	} else if ok {
		cfg.MaxArchiveBytes = v
	}
	if v, ok, err := parseEnvUint64("RELEASE_MIN_FREE_BYTES"); err != nil {
		return releaseConfig{}, err
	} else if ok {
		cfg.MinFreeBytes = v
	}
	if v, ok, err := parseEnvInt("RELEASE_KEEP_COUNT"); err != nil {
		return releaseConfig{}, err
	} else if ok {
		cfg.KeepCount = v
	}
	if v, ok, err := parseEnvInt64("RELEASE_MAX_TOTAL_BYTES"); err != nil {
		return releaseConfig{}, err
	} else if ok {
		cfg.MaxTotalBytes = v
	}
	if v, ok, err := parseEnvInt64("RELEASE_HOURLY_BUDGET_BYTES"); err != nil {
		return releaseConfig{}, err
	} else if ok {
		cfg.HourlyBudgetBytes = v
	}
	if v, ok, err := parseEnvInt64("RELEASE_DAILY_BUDGET_BYTES"); err != nil {
		return releaseConfig{}, err
	} else if ok {
		cfg.DailyBudgetBytes = v
	}
	if v, ok, err := parseEnvInt("RELEASE_MAX_DIRECT_DOWNLOADS"); err != nil {
		return releaseConfig{}, err
	} else if ok {
		cfg.MaxDirectDownloads = v
	}

	if err := validateReleaseConfig(cfg); err != nil {
		return releaseConfig{}, err
	}
	return cfg, nil
}

func validateReleaseConfig(cfg releaseConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(cfg.StorageDir) == "" {
		return fmt.Errorf("RELEASE_STORAGE_DIR is required")
	}
	if len(cfg.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("RELEASE_PUBLIC_KEY is required")
	}
	if err := validateStrictEd25519PublicKey(cfg.PublicKey); err != nil {
		return fmt.Errorf("invalid RELEASE_PUBLIC_KEY: %w", err)
	}
	if err := validateReleasePublicBaseURL(cfg.PublicBaseURL); err != nil {
		return fmt.Errorf("invalid RELEASE_PUBLIC_BASE_URL: %w", err)
	}
	if cfg.AdminToken != "" && len(cfg.AdminToken) < minReleaseAdminTokenBytes {
		return fmt.Errorf("RELEASE_ADMIN_TOKEN must be at least %d bytes", minReleaseAdminTokenBytes)
	}
	if cfg.AccelPrefix != "" {
		if !strings.HasPrefix(cfg.AccelPrefix, "/") || strings.Contains(cfg.AccelPrefix, "..") || strings.ContainsAny(cfg.AccelPrefix, "\r\n\\") {
			return fmt.Errorf("invalid RELEASE_ACCEL_PREFIX")
		}
	}
	if cfg.MaxArchiveBytes <= 0 || cfg.MaxArchiveBytes > maxClientArchiveBytes {
		return fmt.Errorf("RELEASE_MAX_ARCHIVE_BYTES out of range")
	}
	if cfg.KeepCount < 1 || cfg.KeepCount > 1000 {
		return fmt.Errorf("RELEASE_KEEP_COUNT must be between 1 and 1000")
	}
	if cfg.MaxTotalBytes <= 0 {
		return fmt.Errorf("RELEASE_MAX_TOTAL_BYTES must be positive")
	}
	if cfg.HourlyBudgetBytes <= 0 || cfg.DailyBudgetBytes <= 0 {
		return fmt.Errorf("release bandwidth budgets must be positive")
	}
	if cfg.MaxDirectDownloads < 1 || cfg.MaxDirectDownloads > 512 {
		return fmt.Errorf("RELEASE_MAX_DIRECT_DOWNLOADS out of range")
	}
	return nil
}

func validateReleasePublicBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("must be configured")
	}
	u, err := validateReleaseHTTPSURL(raw)
	if err != nil {
		return err
	}
	if u.RawQuery != "" {
		return fmt.Errorf("base URL must not contain query parameters")
	}
	return nil
}

func parseStrictReleasePublicKey(encoded string) (ed25519.PublicKey, error) {
	if encoded == "" {
		return nil, fmt.Errorf("empty public key")
	}
	if len(encoded) > 128 {
		return nil, fmt.Errorf("public key too long")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 encoding")
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be 32 bytes")
	}
	pub := ed25519.PublicKey(raw)
	if err := validateStrictEd25519PublicKey(pub); err != nil {
		return nil, err
	}
	return pub, nil
}

func parseEnvInt(name string) (int, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s: %w", name, err)
	}
	return v, true, nil
}

func parseEnvInt64(name string) (int64, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s: %w", name, err)
	}
	return v, true, nil
}

func parseEnvUint64(name string) (uint64, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid %s: %w", name, err)
	}
	return v, true, nil
}

// validateReleaseHTTPSURL 对齐 cst-updater::manifest::validate_release_url。
func validateReleaseHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url")
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("requires credential-free https url without fragment")
	}
	host := strings.ToLower(strings.TrimRight(u.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return nil, fmt.Errorf("disallowed local host")
	}
	if err := rejectNonPublicIPHost(host); err != nil {
		return nil, err
	}
	return u, nil
}
