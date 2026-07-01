package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const shutdownTimeout = 10 * time.Second

// ControlPlane is a controller-runtime manager.Runnable that serves the
// HTTP control-plane endpoints (notify-attach, compute spec, health)
// and the Neon-compatible REST API v2.
type ControlPlane struct {
	Log            *slog.Logger
	Client         client.Client
	BindAddr       string
	ComputeBaseURL string
	// Namespace is the Kubernetes namespace where CRDs are managed.
	// Defaults to "" (all namespaces). Set via WATCH_NAMESPACE env.
	Namespace string
	// NonCachedReader bypasses the informer cache for cluster-scoped reads
	// (e.g. Node object). Used by the SCClient for JWT-authenticated SC API calls.
	NonCachedReader client.Reader
}

// Start implements manager.Runnable. The manager invokes it after caches sync
// and tears it down when ctx is canceled.
func (cp *ControlPlane) Start(ctx context.Context) error {
	if cp.Log == nil {
		return fmt.Errorf("controlplane: Log is required")
	}
	if cp.Client == nil {
		return fmt.Errorf("controlplane: Client is required")
	}
	if cp.BindAddr == "" {
		return fmt.Errorf("controlplane: BindAddr is required")
	}

	srv := newServer(cp.Log, cp.Client, cp.ComputeBaseURL, cp.Namespace, cp.NonCachedReader)
	httpServer := &http.Server{
		Addr:              cp.BindAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		cp.Log.Info("controlplane listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func newServer(log *slog.Logger, k8sClient client.Client, computeBaseURL string, namespace string, nonCachedReader client.Reader) http.Handler {
	mux := http.NewServeMux()

	addRoutes(mux, log, k8sClient, computeBaseURL, namespace, nonCachedReader)

	return mux
}

func encode[T any](w http.ResponseWriter, _ *http.Request, status int, v T) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// decode is a utility function for future API endpoints
//
//nolint:unused
func decode[T any](r *http.Request) (T, error) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		return v, fmt.Errorf("decode json: %w", err)
	}
	return v, nil
}
