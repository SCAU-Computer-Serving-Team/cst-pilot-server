// 小工具：集中 json 与 reader 的引用，业务文件不重复 import。
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// utcNow 返回与 ts 版 new Date().toISOString() 一致的毫秒精度 UTC ISO（字典序即时间序）。
func utcNow(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
