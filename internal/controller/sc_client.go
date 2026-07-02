package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/compute"
	"oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
)

// SCClient 是 Storage Controller 管理 API 的类型化 HTTP 客户端。
// 它处理 JWT token 生成和自动跟随重定向（SC HA 场景下非 leader 节点会重定向到 leader）。
type SCClient struct {
	k8sClient client.Client
	// nonCachedReader 绕过 informer 缓存进行集群级别的读取
	//（例如 Node 对象）。这一点很关键，因为缓存客户端的 Get
	// 会阻塞直到 informer 缓存同步——如果 controller 没有
	// RBAC 权限来 watch Node，Node 缓存永远不会同步，导致整个
	// reconcile 循环被挂死（MaxConcurrentReconciles=1 将所有
	// safekeeper reconcile 串行化在阻塞 worker 之后）。
	nonCachedReader client.Reader
	BaseURL         string
	httpClient      *http.Client
	// SkipAuth 禁用 JWT 认证。用于测试场景（fake SC 不验证 JWT token）。
	SkipAuth bool
}

// NewSCClient 创建一个新的 storage controller API 客户端。
// 如果 baseURL 为空，客户端将根据集群名称推导 URL，
// 使用标准的 Kubernetes service 命名约定。
// nonCachedReader 用于集群级别的读取（Node），避免在
// Node RBAC 不可用时因 informer 缓存同步而阻塞。
func NewSCClient(k8sClient client.Client, nonCachedReader client.Reader, baseURL string) *SCClient {
	return &SCClient{
		k8sClient:       k8sClient,
		nonCachedReader: nonCachedReader,
		BaseURL:         baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			// 跟随重定向（SC HA leader 转发需要）
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

// safekeeperUpsertRequest 对应 SC 的 SafekeeperUpsert 结构体。
type safekeeperUpsertRequest struct {
	ID                 int64  `json:"id"`
	RegionID           string `json:"region_id"`
	Version            int64  `json:"version"`
	Host               string `json:"host"`
	Port               int32  `json:"port"`
	HTTPPort           int32  `json:"http_port"`
	AvailabilityZoneID string `json:"availability_zone_id"`
}

// schedulingPolicyRequest 对应 SC 的 SafekeeperSchedulingPolicyRequest 结构体。
type schedulingPolicyRequest struct {
	SchedulingPolicy string `json:"scheduling_policy"`
}

// RegisterSafekeeper 调用 POST /control/v1/safekeeper/:id
// 在 Storage Controller 中 upsert safekeeper 的连接信息。
//
// host 基于 StatefulSet headless service DNS 生成：
//
//	{cluster}-safekeeper-{id}.{cluster}-safekeeper-{id}-headless.{namespace}.svc.cluster.local
//
// 即使 pod 被重新调度到其他节点，该地址也保持稳定。
//
// 使用 "admin" scope JWT（SC master key）进行认证。
func (c *SCClient) RegisterSafekeeper(ctx context.Context, sk *neonv1alpha1.Safekeeper) error {
	log := logf.FromContext(ctx)

	baseURL := c.baseURL(sk.Spec.Cluster)
	url := fmt.Sprintf("%s/control/v1/safekeeper/%d", baseURL, sk.Spec.ID)

	// StatefulSet Pod FQDN（STS 副本数 = 1，序号 = 0）：
	// {pod-name}.{headless-service}.{namespace}.svc.cluster.local
	host := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local",
		safekeeper.Name(sk),
		safekeeper.HeadlessName(sk),
		sk.Namespace,
	)

	// 从 pod 调度到的节点读取可用区信息
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

// DecommissionSafekeeper 调用 POST /control/v1/safekeeper/:id/scheduling_policy
// 将 safekeeper 的调度策略设置为 Decomissioned。
//
// 使用 "admin" scope JWT（SC master key）进行认证。
//
// SC 会执行以下操作：
//  1. 停止该 safekeeper 对应的 reconciler
//  2. 停止向该 safekeeper 分配新的 timeline
//  3. Heartbeater 跳过 Decomissioned 节点
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

// ActivateSafekeeper 调用 POST /control/v1/safekeeper/:id/scheduling_policy
// 将 safekeeper 的调度策略设置为 Active。
//
// RegisterSafekeeper 之后，新记录的 safekeeper 处于 "Activating" 状态，
// 而已有记录可能保留之前的 scheduling_policy（例如上次运行的 "Decomissioned" 状态）。
// 必须调用此方法显式激活，safekeeper 才能参与 tenant 调度。
//
// 使用 "admin" scope JWT（SC master key）进行认证。
func (c *SCClient) ActivateSafekeeper(ctx context.Context, sk *neonv1alpha1.Safekeeper) error {
	log := logf.FromContext(ctx)

	baseURL := c.baseURL(sk.Spec.Cluster)
	url := fmt.Sprintf("%s/control/v1/safekeeper/%d/scheduling_policy", baseURL, sk.Spec.ID)

	body := schedulingPolicyRequest{
		SchedulingPolicy: "Active",
	}

	log.Info("Activating safekeeper in storage controller",
		"url", url, "id", sk.Spec.ID)

	return c.doRequest(ctx, sk.Namespace, sk.Spec.Cluster, http.MethodPost, url, body)
}

// baseURL 返回 storage controller 的基础 URL。
func (c *SCClient) baseURL(clusterName string) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return storagecontroller.URL(clusterName)
}

// doRequest 向 storage controller 发送带认证的 HTTP 请求。
// 生成具有适当 scope 的 JWT token 并添加为 Bearer token。
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

	// 添加 JWT 认证（测试环境中 fake SC 不验证，跳过）
	if !c.SkipAuth {
		token, err := c.adminJWT(ctx, clusterName, namespace)
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

// generateJWT 创建用于与 storage controller 认证的已签名 JWT token。
// 从 JWT secret（位于集群命名空间内）读取集群的 Ed25519 私钥，
// 并使用指定的 scope 签名 token。
//
// scope 必须是有效的上游 Scope 枚举值（小写，例如 "admin"、"infra"、
// "generations_api"）。参见上游 neon 仓库中的 libs/utils/src/auth.rs。
//
// SC 实现了 "Admin master key" 机制：如果请求的 scope 与端点要求的 scope
// 不匹配，SC 会回退检查该 scope 是否是 Admin — 如果是，则允许请求。
// 这意味着 "admin" scope 可以访问所有 SC 端点，
// 我们将其作为 operator 的统一认证方案。
//
// 委托 utils.JWTManager 进行 key 加载和签名，
// 保持 Ed25519 签名实现与 compute token 生成共享同一实现。
func (c *SCClient) generateJWT(ctx context.Context, clusterName, namespace, scope string) (string, error) {
	secretName := utils.JWTSecretName(clusterName)

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
		"scope": scope,
	}

	return jm.GenerateToken(claims)
}

// adminJWT 生成 "admin" scope 的 JWT，即 SC master key，
// 可以访问所有 SC 管理端点。
func (c *SCClient) adminJWT(ctx context.Context, clusterName, namespace string) (string, error) {
	return c.generateJWT(ctx, clusterName, namespace, "admin")
}

// SafekeeperHTTPToken 生成用于访问 safekeeper HTTP 管理 API（端口 7676）的 JWT token。
// safekeeper 使用与 storage controller 相同的 Ed25519 公钥验证 token
// （通过 --http-auth-public-key-path 配置）。
//
// 使用 "safekeeperdata" scope，这是 safekeeper auth.rs 要求的。
// safekeeper 只接受 Tenant 和 SafekeeperData scope；
// Admin、PageServerApi 等 scope 会被显式拒绝。
func (c *SCClient) SafekeeperHTTPToken(ctx context.Context, clusterName, namespace string) (string, error) {
	return c.generateJWT(ctx, clusterName, namespace, "safekeeperdata")
}

// =============================================================================
// Pageserver 节点管理 API
// =============================================================================

// NodeDescribeResponse 对应 SC 的 NodeDescribeResponse，
// 用于 GET /control/v1/node 和 GET /control/v1/node/:id 端点。
type NodeDescribeResponse struct {
	ID               uint64 `json:"id"`
	Availability     string `json:"availability"`
	Scheduling       string `json:"scheduling"`
	ListenHTTPAddr   string `json:"listen_http_addr"`
	ListenPGAddr     string `json:"listen_pg_addr"`
	ListenHTTPPort   int32  `json:"listen_http_port"`
	ListenPGPort     int32  `json:"listen_pg_port"`
	Host             string `json:"host"`
	AvailabilityZone string `json:"availability_zone_id"`
}

// NodeListResponse 对应 SC 的 GET /control/v1/node 响应。
type NodeListResponse struct {
	Nodes []NodeDescribeResponse `json:"nodes"`
}

// ShardDescribeResponse 对应 SC 的 shard 信息，
// 用于 GET /control/v1/node/:id/shards 端点。
type ShardDescribeResponse struct {
	TenantShardID string `json:"tenant_shard_id"`
	Attached      bool   `json:"attached"`
	Secondary     bool   `json:"secondary"`
}

// NodeShardsResponse 对应 SC 的 GET /control/v1/node/:id/shards 响应。
type NodeShardsResponse struct {
	Shards []ShardDescribeResponse `json:"shards"`
}

// nodeConfigRequest 是 PUT /control/v1/node/:id/config 的请求体。
type nodeConfigRequest struct {
	Availability *string `json:"availability,omitempty"`
	Scheduling   *string `json:"scheduling,omitempty"`
}

// GetNode 调用 GET /control/v1/node/:node_id 获取单个节点状态。
func (c *SCClient) GetNode(ctx context.Context, clusterName, namespace string, nodeID uint64) (*NodeDescribeResponse, error) {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d", baseURL, nodeID)

	resp, err := c.doRequestGet(ctx, namespace, clusterName, url)
	if err != nil {
		return nil, fmt.Errorf("get node %d: %w", nodeID, err)
	}

	var node NodeDescribeResponse
	if err := json.Unmarshal(resp, &node); err != nil {
		return nil, fmt.Errorf("unmarshal node response: %w", err)
	}
	return &node, nil
}

// ListNodeNodes 调用 GET /control/v1/node 列出所有节点。
func (c *SCClient) ListNodeNodes(ctx context.Context, clusterName, namespace string) ([]NodeDescribeResponse, error) {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node", baseURL)

	resp, err := c.doRequestGet(ctx, namespace, clusterName, url)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	var result NodeListResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal node list response: %w", err)
	}
	return result.Nodes, nil
}

// GetNodeShards 调用 GET /control/v1/node/:node_id/shards 列出节点上的 shard。
func (c *SCClient) GetNodeShards(ctx context.Context, clusterName, namespace string, nodeID uint64) ([]ShardDescribeResponse, error) {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d/shards", baseURL, nodeID)

	resp, err := c.doRequestGet(ctx, namespace, clusterName, url)
	if err != nil {
		return nil, fmt.Errorf("get node shards: %w", err)
	}

	var result NodeShardsResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("unmarshal shards response: %w", err)
	}
	return result.Shards, nil
}

// ConfigureNode 调用 PUT /control/v1/node/:node_id/config 修改
// 节点的 availability 和/或 scheduling policy。不需要修改的字段传 nil。
func (c *SCClient) ConfigureNode(ctx context.Context, clusterName, namespace string, nodeID uint64, availability, scheduling *string) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d/config", baseURL, nodeID)

	body := nodeConfigRequest{
		Availability: availability,
		Scheduling:   scheduling,
	}

	return c.doRequest(ctx, namespace, clusterName, http.MethodPut, url, body)
}

// StartNodeDrain 调用 PUT /control/v1/node/:node_id/drain 开始排空节点
// （将其上的 attached shard 迁移到其他节点）。
func (c *SCClient) StartNodeDrain(ctx context.Context, clusterName, namespace string, nodeID uint64) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d/drain", baseURL, nodeID)

	return c.doRequest(ctx, namespace, clusterName, http.MethodPut, url, nil)
}

// CancelNodeDrain 调用 DELETE /control/v1/node/:node_id/drain
// 取消正在进行的排空操作。
func (c *SCClient) CancelNodeDrain(ctx context.Context, clusterName, namespace string, nodeID uint64) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d/drain", baseURL, nodeID)

	return c.doRequest(ctx, namespace, clusterName, http.MethodDelete, url, nil)
}

// StartNodeFill 调用 PUT /control/v1/node/:node_id/fill
// 将节点设置为 Filling 模式（仅接收新 shard 分配，不接收已有 shard）。
func (c *SCClient) StartNodeFill(ctx context.Context, clusterName, namespace string, nodeID uint64) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d/fill", baseURL, nodeID)

	return c.doRequest(ctx, namespace, clusterName, http.MethodPut, url, nil)
}

// CancelNodeFill 调用 DELETE /control/v1/node/:node_id/fill
// 取消 Filling 模式并恢复为 Active。
func (c *SCClient) CancelNodeFill(ctx context.Context, clusterName, namespace string, nodeID uint64) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d/fill", baseURL, nodeID)

	return c.doRequest(ctx, namespace, clusterName, http.MethodDelete, url, nil)
}

// StartNodeDelete 调用 PUT /control/v1/node/:node_id/delete 标记节点为删除。
// 如果 force 为 true，节点将被跳过排空直接删除。
func (c *SCClient) StartNodeDelete(ctx context.Context, clusterName, namespace string, nodeID uint64, force bool) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d/delete?force=%t", baseURL, nodeID, force)

	return c.doRequest(ctx, namespace, clusterName, http.MethodPut, url, nil)
}

// CancelNodeDelete 调用 DELETE /control/v1/node/:node_id/delete
// 取消正在进行的节点删除操作。
func (c *SCClient) CancelNodeDelete(ctx context.Context, clusterName, namespace string, nodeID uint64) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/node/%d/delete", baseURL, nodeID)

	return c.doRequest(ctx, namespace, clusterName, http.MethodDelete, url, nil)
}

// doRequestGet 发送一个带认证的 GET 请求，返回响应体。
func (c *SCClient) doRequestGet(ctx context.Context, namespace, clusterName, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if !c.SkipAuth {
		token, err := c.adminJWT(ctx, clusterName, namespace)
		if err != nil {
			return nil, fmt.Errorf("generate JWT token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("storage controller request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return respBody, nil
	}

	return nil, fmt.Errorf("storage controller returned %d: %s", resp.StatusCode, string(respBody))
}

// DeleteTenant 调用 DELETE /v1/tenant/:tenant_id 从 storage controller 删除 tenant。
// 在 200、404（已删除）或 409（不可恢复冲突，例如找不到 pageserver — tenant
// 从未被正确调度，无数据需要清理）时返回 nil。
func (c *SCClient) DeleteTenant(ctx context.Context, clusterName, namespace, tenantID string) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/v1/tenant/%s", baseURL, tenantID)

	err := c.doRequest(ctx, namespace, clusterName, http.MethodDelete, url, nil)
	if err != nil && (isSCNotFound(err) || isSCConflict(err)) {
		return nil // 404/409 = 幂等成功
	}
	return err
}

// DeleteTimeline calls DELETE /v1/tenant/:tenant_id/timeline/:timeline_id to
// delete a timeline from the storage controller. Returns nil on 200 or 404.
func (c *SCClient) DeleteTimeline(ctx context.Context, clusterName, namespace, tenantID, timelineID string) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/v1/tenant/%s/timeline/%s", baseURL, tenantID, timelineID)

	err := c.doRequest(ctx, namespace, clusterName, http.MethodDelete, url, nil)
	if err != nil && isSCNotFound(err) {
		return nil // 404 = 已删除，幂等成功
	}
	return err
}

// CreateTenant 调用 PUT /v1/tenant/:tenant_id/location_config
// 在 storage controller 上创建或配置 tenant。
func (c *SCClient) CreateTenant(ctx context.Context, clusterName, namespace, tenantID string, mode string, generation int) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/v1/tenant/%s/location_config", baseURL, tenantID)

	body := map[string]interface{}{
		"mode":        mode,
		"generation":  generation,
		"tenant_conf": map[string]interface{}{},
	}
	return c.doRequest(ctx, namespace, clusterName, http.MethodPut, url, body)
}

// CreateTimeline calls POST /v1/tenant/:tenant_id/timeline to create a timeline
// on the storage controller. Returns nil on 2xx or 409 (Conflict = already exists).
func (c *SCClient) CreateTimeline(ctx context.Context, clusterName, namespace, tenantID, timelineID string, pgVersion int) error {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/v1/tenant/%s/timeline", baseURL, tenantID)

	body := map[string]interface{}{
		"new_timeline_id": timelineID,
		"pg_version":      pgVersion,
	}
	err := c.doRequest(ctx, namespace, clusterName, http.MethodPost, url, body)
	if err != nil && isSCConflict(err) {
		return nil // 409 = timeline 已存在，幂等成功
	}
	return err
}

// GetTenantInfo calls GET /control/v1/tenant/:tenant_id to retrieve tenant
// shard information from the storage controller. Implements compute.TenantInfoGetter.
func (c *SCClient) GetTenantInfo(ctx context.Context, clusterName, namespace, tenantID string) (*compute.TenantInfo, error) {
	baseURL := c.baseURL(clusterName)
	url := fmt.Sprintf("%s/control/v1/tenant/%s", baseURL, tenantID)

	respBody, err := c.doRequestGet(ctx, namespace, clusterName, url)
	if err != nil {
		return nil, fmt.Errorf("failed to get tenant info: %w", err)
	}

	var info compute.TenantInfo
	if err := json.Unmarshal(respBody, &info); err != nil {
		return nil, fmt.Errorf("failed to decode tenant info: %w", err)
	}

	return &info, nil
}

// isSCNotFound 检查错误消息是否表示 SC 返回了 404。
func isSCNotFound(err error) bool {
	return strings.Contains(err.Error(), "returned 404")
}

// isSCConflict 检查错误消息是否表示 SC 返回了 409。
func isSCConflict(err error) bool {
	return strings.Contains(err.Error(), "returned 409")
}

// getNodeAvailabilityZone 读取 K8s 节点的 topology label，
// 确定 safekeeper pod 所在的可用区。
//
// 重点：Node 读取必须使用 nonCachedReader（非缓存的 API reader），
// 因为 operator 的 RBAC 可能不包含对 nodes 的 list/watch 权限，
// 这会导致 Node informer 缓存永远无法同步。如果使用缓存客户端，
// Get 调用将无限期阻塞，导致整个 safekeeper reconcile 循环卡死。
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

	// 使用 nonCachedReader 读取 Node，避免因 informer 缓存同步而阻塞。
	// 如果 nonCachedReader 为 nil（例如测试环境），回退到缓存客户端。
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
