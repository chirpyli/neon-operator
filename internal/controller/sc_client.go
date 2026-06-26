package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
)

// SCClient is a typed HTTP client for the Storage Controller's management API.
// It handles JWT token generation and automatic redirect following (needed
// for SC HA where non-leader nodes redirect to the leader).
type SCClient struct {
	k8sClient client.Client
	// nonCachedReader bypasses the informer cache for cluster-scoped reads
	// (e.g. Node object). This is critical because the cached client's Get
	// blocks until the informer cache syncs — and if the controller lacks
	// RBAC to watch Nodes, the Node cache never syncs, causing the entire
	// reconcile loop to hang (MaxConcurrentReconciles=1 serializes all
	// safekeeper reconciles behind the blocked worker).
	nonCachedReader client.Reader
	BaseURL         string
	httpClient      *http.Client
	// SkipAuth disables JWT authentication. Used in tests where the fake SC
	// does not validate JWT tokens.
	SkipAuth bool
}

// NewSCClient creates a new storage controller API client.
// If baseURL is empty, the client will derive the URL from the cluster name
// using the standard Kubernetes service naming convention.
// nonCachedReader is used for cluster-scoped reads (Node) to avoid blocking
// on informer cache sync when Node RBAC is unavailable.
func NewSCClient(k8sClient client.Client, nonCachedReader client.Reader, baseURL string) *SCClient {
	return &SCClient{
		k8sClient:       k8sClient,
		nonCachedReader: nonCachedReader,
		BaseURL:         baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			// Follow redirects (required for SC HA leader forwarding)
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

// safekeeperUpsertRequest matches the SC's SafekeeperUpsert struct.
type safekeeperUpsertRequest struct {
	ID                 int64  `json:"id"`
	RegionID           string `json:"region_id"`
	Version            int64  `json:"version"`
	Host               string `json:"host"`
	Port               int32  `json:"port"`
	HTTPPort           int32  `json:"http_port"`
	AvailabilityZoneID string `json:"availability_zone_id"`
}

// schedulingPolicyRequest matches the SC's SafekeeperSchedulingPolicyRequest.
type schedulingPolicyRequest struct {
	SchedulingPolicy string `json:"scheduling_policy"`
}

// RegisterSafekeeper calls POST /control/v1/safekeeper/:id to upsert the
// safekeeper's connection information in the Storage Controller.
//
// The host is derived from the StatefulSet headless service DNS:
//
//	{cluster}-safekeeper-{id}.{cluster}-safekeeper-{id}-headless.{namespace}.svc.cluster.local
//
// This remains stable even when the pod is rescheduled to a different node.
//
// Uses JWT with Scope::Infra for authentication.
func (c *SCClient) RegisterSafekeeper(ctx context.Context, sk *neonv1alpha1.Safekeeper) error {
	log := logf.FromContext(ctx)

	baseURL := c.baseURL(sk.Spec.Cluster)
	url := fmt.Sprintf("%s/control/v1/safekeeper/%d", baseURL, sk.Spec.ID)

	// StatefulSet Pod FQDN:
	// {pod-name}.{headless-service}.{namespace}.svc.cluster.local
	host := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
		safekeeper.Name(sk),
		safekeeper.HeadlessName(sk),
		sk.Namespace,
	)

	// Read availability zone from the node the pod is scheduled on
	az := c.getNodeAvailabilityZone(ctx, sk)

	body := safekeeperUpsertRequest{
		ID:                 int64(sk.Spec.ID),
		RegionID:           "default",
		Version:            1,
		Host:               host,
		Port:               5454, // WAL (pg) port
		HTTPPort:           7676, // HTTP management port
		AvailabilityZoneID: az,
	}

	log.Info("Registering safekeeper with storage controller",
		"url", url, "id", sk.Spec.ID, "host", host, "az", az)

	return c.doRequest(ctx, sk.Namespace, sk.Spec.Cluster, http.MethodPost, url, body)
}

// DecommissionSafekeeper calls POST /control/v1/safekeeper/:id/scheduling_policy
// to set the safekeeper's scheduling policy to Decomissioned.
//
// Uses JWT with Scope::Admin for authentication.
//
// SC handles:
//  1. Stop the per-safekeeper reconciler
//  2. Stop assigning new timelines to this safekeeper
//  3. Heartbeater skips Decomissioned nodes
func (c *SCClient) DecommissionSafekeeper(ctx context.Context, sk *neonv1alpha1.Safekeeper) error {
	log := logf.FromContext(ctx)

	baseURL := c.baseURL(sk.Spec.Cluster)
	url := fmt.Sprintf("%s/control/v1/safekeeper/%d/scheduling_policy", baseURL, sk.Spec.ID)

	body := schedulingPolicyRequest{
		SchedulingPolicy: "Decomissioned",
	}

	log.Info("Decommissioning safekeeper in storage controller",
		"url", url, "id", sk.Spec.ID)

	return c.doRequest(ctx, sk.Namespace, sk.Spec.Cluster, http.MethodPost, url, body)
}

// baseURL returns the storage controller base URL.
func (c *SCClient) baseURL(clusterName string) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return storagecontroller.URL(clusterName)
}

// doRequest sends an authenticated HTTP request to the storage controller.
// It generates a JWT token with the appropriate scope and adds it as a Bearer token.
func (c *SCClient) doRequest(ctx context.Context, namespace, clusterName, method, url string, body any) error {
	log := logf.FromContext(ctx)

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Add JWT authentication (skipped in tests where fake SC doesn't validate)
	if !c.SkipAuth {
		token, err := c.generateJWT(ctx, clusterName, namespace)
		if err != nil {
			return fmt.Errorf("generate JWT token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("storage controller request failed: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error(err, "failed to close response body")
		}
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("storage controller returned %d: %s", resp.StatusCode, string(respBody))
}

// generateJWT creates a signed JWT token for authenticating with the storage controller.
// It reads the cluster's Ed25519 private key from the JWT secret (in the cluster namespace)
// and signs a token with both "infra" and "admin" scopes.
//
// Delegates to utils.JWTManager for key loading and signing, keeping the
// single Ed25519 signing implementation shared with compute token generation.
func (c *SCClient) generateJWT(ctx context.Context, clusterName, namespace string) (string, error) {
	secretName := fmt.Sprintf("cluster-%s-jwt", clusterName)

	var secret corev1.Secret
	if err := c.k8sClient.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: namespace,
	}, &secret); err != nil {
		return "", fmt.Errorf("get JWT secret %s in namespace %s: %w", secretName, namespace, err)
	}

	jm, err := utils.NewJWTManagerFromSecret(&secret)
	if err != nil {
		return "", fmt.Errorf("create JWT manager: %w", err)
	}

	now := time.Now()
	claims := map[string]any{
		"iss":   "neon-operator",
		"sub":   clusterName,
		"iat":   now.Unix(),
		"exp":   now.Add(5 * time.Minute).Unix(),
		"scope": "infra admin",
	}

	return jm.GenerateToken(claims)
}

// getNodeAvailabilityZone reads the K8s node topology label to determine
// the availability zone for the safekeeper's pod.
//
// IMPORTANT: Node reads must use nonCachedReader (uncached API reader) because
// the operator's RBAC may not include "nodes" list/watch — which prevents the
// Node informer cache from syncing. If we use the cached client, the Get call
// blocks indefinitely, stalling the entire safekeeper reconcile loop.
func (c *SCClient) getNodeAvailabilityZone(ctx context.Context, sk *neonv1alpha1.Safekeeper) string {
	log := logf.FromContext(ctx)
	pod := &corev1.Pod{}
	podName := safekeeper.Name(sk) + "-0"
	if err := c.k8sClient.Get(ctx, types.NamespacedName{
		Name:      podName,
		Namespace: sk.Namespace,
	}, pod); err != nil {
		// Pod 尚未创建或已消失：这是正常的瞬时状态。
		log.Info("无法读取 safekeeper pod，可用区回退为 unknown",
			"pod", podName, "error", err)
		return "unknown"
	}
	if pod.Spec.NodeName == "" {
		// Pod 已被创建但尚未被调度到节点。
		log.Info("safekeeper pod 尚未调度，可用区回退为 unknown",
			"pod", podName)
		return "unknown"
	}

	// Use nonCachedReader for Node to avoid blocking on informer cache sync.
	// Falls back to cached client if nonCachedReader is nil (e.g. in tests).
	node := &corev1.Node{}
	reader := c.nonCachedReader
	if reader == nil {
		reader = c.k8sClient
	}
	if err := reader.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
		log.Info("无法读取 Node 对象，可用区回退为 unknown",
			"node", pod.Spec.NodeName, "error", err)
		return "unknown"
	}
	if az, ok := node.Labels["topology.kubernetes.io/zone"]; ok {
		return az
	}
	// Node 存在但缺少拓扑标签 —— 通常是集群配置问题。
	log.Info("Node 缺少 topology.kubernetes.io/zone 标签，可用区回退为 unknown",
		"node", pod.Spec.NodeName)
	return "unknown"
}
