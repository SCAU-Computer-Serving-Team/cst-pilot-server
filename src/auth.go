// 遥测接收端 · 身份解析。
//
// OA 内省接口未实现前用桩顶替：令牌格式 stub-<mid>-<device_id> 直接解析；
// OA_INTROSPECT_URL 配置后切换为真内省。结果按令牌 SHA-256 缓存 60 秒，只存内存。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"
)

const cacheTTL = 60 * time.Second

type identity struct {
	MID      string `json:"mid"`
	DeviceID string `json:"device_id"`
}

type introspectResult struct {
	active   bool
	reason   string // expired | invalid | revoked | password-changed；error 时为空
	identity identity
	err      bool // 内省服务不可用
}

type authOptions struct {
	URL          string
	ServiceToken string
}

type introspector struct {
	opts  authOptions
	mu    sync.Mutex
	cache map[string]cachedResult
}

type cachedResult struct {
	at     time.Time
	result introspectResult
}

func newIntrospector(opts authOptions) *introspector {
	return &introspector{opts: opts, cache: make(map[string]cachedResult)}
}

func (i *introspector) introspect(token string) introspectResult {
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	i.mu.Lock()
	if hit, ok := i.cache[key]; ok && time.Since(hit.at) < cacheTTL {
		i.mu.Unlock()
		return hit.result
	}
	i.mu.Unlock()

	var result introspectResult
	if i.opts.URL == "" {
		result = stubIntrospect(token)
	} else {
		result = remoteIntrospect(token, i.opts)
	}

	i.mu.Lock()
	if len(i.cache) > 512 {
		i.cache = make(map[string]cachedResult)
	}
	i.cache[key] = cachedResult{at: time.Now(), result: result}
	i.mu.Unlock()
	return result
}

var stubPattern = regexp.MustCompile(`^stub-([A-Za-z0-9]+)-([A-Za-z0-9_-]+)$`)

// stubIntrospect 桩：仅接受 stub-<mid>-<device> 形状，供联调与 e2e 使用。
func stubIntrospect(token string) introspectResult {
	m := stubPattern.FindStringSubmatch(token)
	if m == nil {
		return introspectResult{active: false, reason: "invalid"}
	}
	return introspectResult{active: true, identity: identity{MID: m[1], DeviceID: m[2]}}
}

var introspectClient = &http.Client{Timeout: 5 * time.Second}

func remoteIntrospect(token string, opts authOptions) introspectResult {
	body, err := jsonMarshal(map[string]string{"token": token})
	if err != nil {
		return introspectResult{err: true}
	}
	req, err := http.NewRequest(http.MethodPost, opts.URL, bytesReader(body))
	if err != nil {
		return introspectResult{err: true}
	}
	req.Header.Set("Content-Type", "application/json")
	if opts.ServiceToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.ServiceToken)
	}
	resp, err := introspectClient.Do(req)
	if err != nil {
		return introspectResult{err: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return introspectResult{err: true}
	}
	var payload struct {
		Active    bool   `json:"active"`
		MID       string `json:"mid"`
		DeviceID  string `json:"device_id"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return introspectResult{err: true}
	}
	if payload.Active && payload.MID != "" && payload.DeviceID != "" {
		return introspectResult{active: true, identity: identity{MID: payload.MID, DeviceID: payload.DeviceID}}
	}
	if payload.Reason == "revoked" || payload.Reason == "password-changed" {
		return introspectResult{active: false, reason: payload.Reason}
	}
	if payload.Reason == "expired" {
		return introspectResult{active: false, reason: "expired"}
	}
	return introspectResult{active: false, reason: "invalid"}
}
