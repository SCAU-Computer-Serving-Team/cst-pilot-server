// 安装凭据首次使用时绑定公钥；后续上传须持有对应私钥，不依赖 OA 登录。
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var installationPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type anonymousLimiter struct {
	mu     sync.Mutex
	minute int64
	counts map[string]int
}

// 对连接来源与安装分别限流，内存有界；反向代理下来源限额由该入口共享。
func (l *anonymousLimiter) allow(peer, installation string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	minute := time.Now().Unix() / 60
	if l.minute != minute {
		l.minute = minute
		l.counts = make(map[string]int)
	}
	if len(l.counts) >= 2048 {
		return false
	}
	peerKey, installKey := "ip:"+peer, "installation:"+installation
	if l.counts[peerKey] >= 120 || l.counts[installKey] >= 60 {
		return false
	}
	l.counts[peerKey]++
	l.counts[installKey]++
	return true
}

func verifyInstallation(r *http.Request, body []byte, installation string) (string, bool) {
	if !installationPattern.MatchString(installation) {
		return "", false
	}
	stamp := r.Header.Get("X-Telemetry-Time")
	milliseconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || time.Since(time.UnixMilli(milliseconds)).Abs() > 10*time.Minute {
		return "", false
	}
	encoded := r.Header.Get("X-Telemetry-Key")
	if len(encoded) > 128 {
		return "", false
	}
	der, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "", false
	}
	public, ok := key.(ed25519.PublicKey)
	if !ok {
		return "", false
	}
	signature, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Telemetry-Signature"))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return "", false
	}
	digest := sha256.Sum256(body)
	message := strings.Join([]string{"telemetry-upload", installation, stamp, hex.EncodeToString(digest[:])}, "\n")
	return encoded, ed25519.Verify(public, []byte(message), signature)
}

// 接收端绑定公钥只证明安装凭据持有权，不证明设备真实性或自然人身份。
func (s *store) registerInstallation(installation, publicKey string) (bool, error) {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO installations (installation_id, public_key, first_seen_at) VALUES (?, ?, ?)`, installation, publicKey, utcNow(time.Now()))
	if err != nil {
		return false, err
	}
	var stored string
	err = s.db.QueryRow("SELECT public_key FROM installations WHERE installation_id = ?", installation).Scan(&stored)
	return stored == publicKey, err
}
