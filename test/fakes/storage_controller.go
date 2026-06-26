// Package fakes 提供 operator 所依赖的上游服务（storage_controller 和 compute_ctl）
// 的内存 HTTP 测试替身。在测试中，使用它们将 reconciler 和 controlplane handler
// 指向一个可访问的服务器。
package fakes

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// StorageController 是上游 storage_controller HTTP API 的测试替身。
type StorageController struct {
	server *httptest.Server

	// LocationConfig 覆盖 PUT /v1/tenant/{id}/location_config 的默认 200 处理函数，非 nil 时生效。
	LocationConfig http.HandlerFunc

	// Timeline 覆盖 POST /v1/tenant/{id}/timeline 的默认 201 处理函数，非 nil 时生效。
	Timeline http.HandlerFunc

	// DeleteTenant 覆盖 DELETE /v1/tenant/{id} 的默认 200 处理函数，非 nil 时生效。
	DeleteTenant http.HandlerFunc

	// DeleteTimeline 覆盖 DELETE /v1/tenant/{id}/timeline 的默认 200 处理函数，非 nil 时生效。
	DeleteTimeline http.HandlerFunc

	// RegisterSafekeeper 覆盖 POST /control/v1/safekeeper/{id} 的默认 200 处理函数，非 nil 时生效。
	RegisterSafekeeper http.HandlerFunc

	// DecommissionSafekeeper 覆盖 POST /control/v1/safekeeper/{id}/scheduling_policy 的默认 200 处理函数，非 nil 时生效。
	DecommissionSafekeeper http.HandlerFunc

	mu    sync.Mutex
	calls []Call
}

// Call 记录测试替身接收到的请求。
type Call struct {
	Method string
	Path   string
	Body   []byte
}

// NewStorageController 启动一个假的 storage_controller HTTP 服务器。
// 调用者使用完毕后必须调用 Close() 关闭。
func NewStorageController() *StorageController {
	sc := &StorageController{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/tenant/{id}/location_config", func(w http.ResponseWriter, r *http.Request) {
		sc.dispatch(w, r, sc.LocationConfig, http.StatusOK)
	})
	mux.HandleFunc("POST /v1/tenant/{id}/timeline", func(w http.ResponseWriter, r *http.Request) {
		sc.dispatch(w, r, sc.Timeline, http.StatusCreated)
	})
	mux.HandleFunc("DELETE /v1/tenant/{id}", func(w http.ResponseWriter, r *http.Request) {
		sc.dispatch(w, r, sc.DeleteTenant, http.StatusOK)
	})
	mux.HandleFunc("DELETE /v1/tenant/{tenant_id}/timeline/{timeline_id}", func(w http.ResponseWriter, r *http.Request) {
		sc.dispatch(w, r, sc.DeleteTimeline, http.StatusOK)
	})
	mux.HandleFunc("POST /control/v1/safekeeper/{id}", func(w http.ResponseWriter, r *http.Request) {
		sc.dispatch(w, r, sc.RegisterSafekeeper, http.StatusOK)
	})
	mux.HandleFunc("POST /control/v1/safekeeper/{id}/scheduling_policy", func(w http.ResponseWriter, r *http.Request) {
		sc.dispatch(w, r, sc.DecommissionSafekeeper, http.StatusOK)
	})
	sc.server = httptest.NewServer(mux)
	return sc
}

// URL 返回测试服务器的 base URL，可直接赋值给
// ProjectReconciler.StorageControllerBaseURL 等字段使用。
func (sc *StorageController) URL() string { return sc.server.URL }

// Close 关闭测试服务器，可多次安全调用。
func (sc *StorageController) Close() { sc.server.Close() }

// Calls 返回所有已记录请求的副本，按到达顺序排列。
func (sc *StorageController) Calls() []Call {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := make([]Call, len(sc.calls))
	copy(out, sc.calls)
	return out
}

// Reset 清空已记录的调用，用于在多个子测试间复用同一个 fake 实例。
func (sc *StorageController) Reset() {
	sc.mu.Lock()
	sc.calls = nil
	sc.mu.Unlock()
}

func (sc *StorageController) dispatch(
	w http.ResponseWriter,
	r *http.Request,
	override http.HandlerFunc,
	defaultStatus int,
) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()

	sc.mu.Lock()
	sc.calls = append(sc.calls, Call{Method: r.Method, Path: r.URL.Path, Body: body})
	sc.mu.Unlock()

	if override != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		override(w, r)
		return
	}
	w.WriteHeader(defaultStatus)
}
