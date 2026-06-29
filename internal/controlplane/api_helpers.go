package controlplane

import (
	"net/http"
)

// =============================================================================
// 辅助函数 — 统一 JSON 响应
// =============================================================================

// writeJSON 写入 JSON 响应
func writeJSON(w http.ResponseWriter, status int, v interface{}) error {
	return encode(w, nil, status, v)
}

// writeAPIError 写入 API 错误响应（Neon-style: { "code": "...", "message": "..." }）
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	_ = encode(w, nil, status, ErrorResponse{Code: code, Message: message})
}
