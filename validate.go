// 遥测接收端 · 校验。
//
// 整批拒绝与逐条拒收的规则见主仓库 doc/telemetry/receiver/SPEC.md「校验」。
// 拒一条就是永久丢一条，条件取最保守的一组；其余一律宽容。
package main

var acceptedVersions = map[string]bool{"0.1": true}

const (
	envelopeMaxRecords = 50
	envelopeMaxBytes   = 1024 * 1024
	maxTools           = 512
	maxModels          = 32
	maxErrors          = 10
)

type envelope struct {
	Records []map[string]any `json:"records"`
}

// validateEnvelope 整批级校验；返回 records（nil 表示整批拒绝，code 带原因）。
func validateEnvelope(body []byte) (records []map[string]any, status int, code string) {
	var e envelope
	if err := jsonUnmarshal(body, &e); err != nil || e.Records == nil {
		return nil, 400, "invalid_body"
	}
	if len(e.Records) == 0 {
		return nil, 400, "empty_batch"
	}
	if len(e.Records) > envelopeMaxRecords {
		return nil, 400, "too_many_records"
	}
	return e.Records, 0, ""
}

// validateRecord 逐条校验；返回拒绝 reason，空串表示收下。
func validateRecord(record map[string]any) string {
	v, ok := record["v"].(string)
	if !ok || !acceptedVersions[v] {
		return "unknown-version"
	}
	if record["type"] != "session" {
		return "unknown-type"
	}
	id, ok := record["recordId"].(string)
	if !ok || id == "" || len(id) > 64 {
		return "bad-record-id"
	}
	if list, ok := record["tools"].([]any); ok && len(list) > maxTools {
		return "too-many-elements"
	}
	if list, ok := record["models"].([]any); ok && len(list) > maxModels {
		return "too-many-elements"
	}
	if list, ok := record["errors"].([]any); ok && len(list) > maxErrors {
		return "too-many-elements"
	}
	return ""
}
