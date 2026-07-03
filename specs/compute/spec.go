package compute

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/utils"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ShardIndex represents a shard number and count pair
type ShardIndex struct {
	ShardNumber uint8 `json:"shard_number"`
	ShardCount  uint8 `json:"shard_count"`
}

// String returns the hex representation of the shard index
func (s ShardIndex) String() string {
	return fmt.Sprintf("%02x%02x", s.ShardNumber, s.ShardCount)
}

// MarshalJSON implements custom JSON marshaling for ShardIndex
func (s ShardIndex) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON implements custom JSON unmarshaling for ShardIndex
func (s *ShardIndex) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}

	if len(str) != 4 {
		return fmt.Errorf("invalid shard index string length: %d", len(str))
	}

	decodedbytes, err := hex.DecodeString(str)
	if err != nil {
		return err
	}

	s.ShardNumber = decodedbytes[0]
	s.ShardCount = decodedbytes[1]
	return nil
}

// ParseShardIndex parses a hex string into a ShardIndex
func ParseShardIndex(s string) (ShardIndex, error) {
	var si ShardIndex
	err := si.UnmarshalJSON([]byte(fmt.Sprintf(`"%s"`, s)))
	return si, err
}

// PageserverShardConnectionInfo contains connection information for a pageserver shard
type PageserverShardConnectionInfo struct {
	ID       *uint64 `json:"id,omitempty"`
	LibpqURL *string `json:"libpq_url,omitempty"`
	GrpcURL  *string `json:"grpc_url,omitempty"`
}

// PageserverShardInfo contains information about pageserver shards
type PageserverShardInfo struct {
	Pageservers []PageserverShardConnectionInfo `json:"pageservers"`
}

type ComputeHookNotifyRequestShard struct {
	NodeID      uint64 `json:"node_id"`
	ShardNumber uint32 `json:"shard_number"`
}
type ComputeHookNotifyRequest struct {
	TenantID   string                          `json:"tenant_id"`
	StripeSize *uint32                         `json:"stripe_size,omitempty"`
	Shards     []ComputeHookNotifyRequestShard `json:"shards"`
}

type NotifySafekeepersSafekeeper struct {
	ID       uint64  `json:"id"`
	Hostname *string `json:"hostname,omitempty"`
}

type NotifySafekeepersRequest struct {
	TenantID    string                        `json:"tenant_id"`
	TimelineID  string                        `json:"timeline_id"`
	Generation  uint32                        `json:"generation"`
	Safekeepers []NotifySafekeepersSafekeeper `json:"safekeepers"`
}

// Role represents a database role configuration
type Role struct {
	Name              string      `json:"name"`
	EncryptedPassword string      `json:"encrypted_password"`
	Options           interface{} `json:"options"`
}

// Database represents a database configuration in the compute spec.
// Replaces the previous []interface{} weak type.
type Database struct {
	Name    string      `json:"name"`
	Owner   string      `json:"owner"`
	Options interface{} `json:"options"`
}

type SettingsEntry struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Vartype string `json:"vartype"`
}

// ClusterConfig represents cluster configuration in the compute spec
type ClusterConfig struct {
	ClusterID string          `json:"cluster_id"`
	Name      string          `json:"name"`
	Roles     []Role          `json:"roles"`
	Databases []Database      `json:"databases"`
	Settings  []SettingsEntry `json:"settings"`
}

// PageserverConnectionInfo represents pageserver connection information
type PageserverConnectionInfo struct {
	ShardCount int                            `json:"shard_count"`
	Shards     map[string]PageserverShardInfo `json:"shards"`
}

// ComputeCtlConfig represents compute control configuration
type ComputeCtlConfig struct {
	JWKS *utils.JWKResponse `json:"jwks"`
}

// ComputeSpec represents the main compute specification
type ComputeSpec struct {
	FormatVersion            float64                  `json:"format_version"`
	SuspendTimeoutSeconds    int                      `json:"suspend_timeout_seconds"`
	Cluster                  ClusterConfig            `json:"cluster"`
	DeltaOperations          []interface{}            `json:"delta_operations"`
	SafekeepersGeneration    *uint32                  `json:"safekeepers_generation,omitempty"`
	SafekeeperConnstrings    []string                 `json:"safekeeper_connstrings"`
	PageserverConnectionInfo PageserverConnectionInfo `json:"pageserver_connection_info"`
	// StorageAuthToken 是 walproposer 连接 safekeeper 和 pageserver 时使用的认证 token。
	// 该值通过 NEON_AUTH_TOKEN 环境变量传入 compute 节点，作为 JWT 密码完成 safekeeper
	// 的 JWT 认证。walproposer 源码 libpagestore.c 中硬编码读取此环境变量。
	StorageAuthToken string `json:"storage_auth_token,omitempty"`
	// Mode 指定 compute 启动模式。
	// "Primary" (read_write): WAL proposer，参与 safekeeper 共识。
	// "Replica" (read_only): WAL follower，动态跟随分支 tip（hot standby）。
	// 值必须首字母大写以匹配 compute_ctl 的 Rust ComputeMode 枚举。
	// 缺省时默认为 Primary。
	Mode string `json:"mode,omitempty"`
}

// ComputeSpecResponse represents the complete JSON response
type ComputeSpecResponse struct {
	Spec             ComputeSpec      `json:"spec"`
	ComputeCtlConfig ComputeCtlConfig `json:"compute_ctl_config"`
	Status           string           `json:"status"`
}

// TenantInfoGetter provides tenant shard information from the storage controller.
// Implemented by SCClient (with JWT auth) and StorageControllerClient (bare HTTP).
type TenantInfoGetter interface {
	GetTenantInfo(ctx context.Context, clusterName, namespace, tenantID string) (*TenantInfo, error)
}

func RefreshConfiguration(ctx context.Context,
	log *slog.Logger,
	k8sClient client.Client,
	request ComputeHookNotifyRequest,
	deployment *appsv1.Deployment,
	computeBaseURL string) error {
	computeId, err := extractComputeID(deployment)
	if err != nil {
		return err
	}

	clusterName, err := extractClusterName(deployment)
	if err != nil {
		return fmt.Errorf("failed to extract clustername from deployment: %w", err)
	}

	spec, err := GenerateComputeSpec(ctx, log, k8sClient, &request, computeId, nil)
	if err != nil {
		return fmt.Errorf("failed to generate compute spec: %w", err)
	}

	return postComputeSpec(ctx, log, k8sClient, spec, deployment, computeId, clusterName, computeBaseURL)
}

func postComputeSpec(ctx context.Context,
	log *slog.Logger,
	k8sClient client.Client,
	spec *ComputeSpecResponse,
	deployment *appsv1.Deployment,
	computeId, clusterName, computeBaseURL string) error {
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("failed to marshal spec JSON: %w", err)
	}

	secretName := fmt.Sprintf("cluster-%s-jwt", clusterName)
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: secretName, Namespace: "neon"}, secret); err != nil {
		return fmt.Errorf("failed to get JWT secret %s: %w", secretName, err)
	}

	jwtManager, err := utils.NewJWTManagerFromSecret(secret)
	if err != nil {
		return fmt.Errorf("failed to create JWT manager from secret: %w", err)
	}

	now := time.Now()
	// ComputeClaims 对应上游 neon/libs/compute_api/src/requests.rs 中的结构体：
	//   compute_id: Option<String>
	//   scope:      Option<ComputeClaimsScope>  // "compute_ctl:admin"
	//   aud:        Option<Vec<String>>         // ["compute"]
	//
	// compute_ctl/authorize.rs 中的 verify() 使用 jsonwebtoken::decode<ComputeClaims>
	// 将 JWT payload 反序列化。aud 必须为数组（serde_json 不会将 string 自动转为 Vec<String>），
	// scope 必须为字符串值 "compute_ctl:admin"（对应 ComputeClaimsScope::Admin）。
	claims := map[string]any{
		"compute_id": computeId,
		"aud":        []string{"compute"},
		"scope":      "compute_ctl:admin",
		"exp":        now.Add(1 * time.Hour).Unix(),
		"iat":        now.Unix(),
		"iss":        "neon-operator",
		"sub":        computeId,
	}

	tokenString, err := jwtManager.GenerateToken(claims)
	if err != nil {
		return fmt.Errorf("failed to generate JWT token: %w", err)
	}

	adminServiceName := fmt.Sprintf("endpoint-%s-admin", computeId)
	adminServiceKey := client.ObjectKey{Name: adminServiceName, Namespace: deployment.Namespace}
	adminService := &corev1.Service{}
	if err := k8sClient.Get(ctx, adminServiceKey, adminService); err != nil {
		return fmt.Errorf("failed to get admin service %s: %w", adminServiceKey, err)
	}

	url := fmt.Sprintf("http://%s.%s:3080/configure", adminServiceName, adminService.Namespace)
	if computeBaseURL != "" {
		url = computeBaseURL + "/configure"
	}
	log.Info("Calling /configure endpoint", "url", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(specBytes))
	if err != nil {
		return fmt.Errorf("failed to create request for service %s: %w", adminServiceName, err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", tokenString))
	req.Header.Set("Content-Type", "application/json")
	computeClient := &http.Client{Timeout: 2 * time.Second}
	resp, err := computeClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call /configure for service %s: %w", adminServiceName, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error("Failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("failed to call /configure for service %s: %s", adminServiceName, resp.Status)
	}
	log.Info("Successfully called /configure", "service", adminServiceName, "url", url)
	return nil
}

func extractComputeID(deployment *appsv1.Deployment) (string, error) {
	if annotations := deployment.GetAnnotations(); annotations != nil {
		if id, ok := annotations["neon.compute_id"]; ok {
			return id, nil
		}
	}
	return "", fmt.Errorf("failed to extract compute ID from annotations")
}

// GenerateComputeSpec generates a compute specification JSON response.
// tenantInfoGetter is used for the /spec path to fetch shard info from the
// storage controller. Pass nil to use the bare HTTP StorageControllerClient
// (legacy / test path); pass an SCClient for JWT-authenticated requests.
func GenerateComputeSpec(
	ctx context.Context,
	log *slog.Logger,
	k8sClient client.Client,
	request *ComputeHookNotifyRequest,
	computeID string,
	tenantInfoGetter TenantInfoGetter,
) (*ComputeSpecResponse, error) {
	log.Info("Starting compute spec generation", "compute_id", computeID)

	// 1. Find the compute deployment to get cluster context
	deployment, err := findComputeDeployment(ctx, k8sClient, computeID)
	if err != nil {
		log.Error("Failed to find compute deployment", "compute_id", computeID, "error", err)
		return nil, err
	}

	clusterName, err := extractClusterName(deployment)
	if err != nil {
		log.Error("Failed to extract cluster name from deployment", "error", err)
		return nil, err
	}

	log.Info("Found cluster name", "cluster_name", clusterName)

	tenantID, ok := deployment.GetLabels()["neon.tenant_id"]
	if !ok {
		err := fmt.Errorf("deployment missing neon.tenant_id label")
		log.Error("Missing required label", "error", err)
		return nil, err
	}

	timelineID, ok := deployment.GetLabels()["neon.timeline_id"]
	if !ok {
		err := fmt.Errorf("deployment missing neon.timeline_id label")
		log.Error("Missing required label", "error", err)
		return nil, err
	}

	log.Info("Found tenant and timeline", "tenant_id", tenantID, "timeline_id", timelineID)

	// 2. Get project and branch details
	project, branch, err := findProjectAndBranch(ctx, k8sClient, tenantID, timelineID)
	if err != nil {
		log.Error("Failed to find project and branch", "error", err,
			"tenant_id", tenantID, "timeline_id", timelineID)
		return nil, err
	}

	// 3. Get JWT keys from cluster secret
	jwks, err := getJWTKeysFromSecret(ctx, k8sClient, clusterName)
	if err != nil {
		log.Error("Failed to get JWT keys from secret", "cluster_name", clusterName, "error", err)
		return nil, err
	}

	log.Info("Successfully retrieved JWT keys")

	// 4. 从 Role/Database CR 聚合用户和数据库（与 EndpointConfigMap 共用同一套聚合逻辑）
	roles := aggregateRoles(ctx, k8sClient, branch.Name, project)
	databases := aggregateDatabases(ctx, k8sClient, branch.Name)

	log.Info("Aggregated roles and databases", "roles", len(roles), "databases", len(databases))

	// 5. 查询集群实际的 Safekeeper CR，获取真实 ID 列表
	safekeeperIDs, err := listSafekeeperIDs(ctx, k8sClient, clusterName)
	if err != nil {
		log.Warn("Failed to list safekeepers, falling back to default IDs 1,2,3", "error", err)
		safekeeperIDs = []uint32{1, 2, 3}
	}
	if len(safekeeperIDs) == 0 {
		log.Warn("No safekeepers found for cluster, falling back to default IDs 1,2,3",
			"cluster", clusterName)
		safekeeperIDs = []uint32{1, 2, 3}
	}

	log.Info("Using safekeeper IDs from cluster", "ids", safekeeperIDs)

	safekeeperConnstrings := make([]string, len(safekeeperIDs))
	for i, id := range safekeeperIDs {
		safekeeperConnstrings[i] = fmt.Sprintf(
			"postgresql://postgres:@%s-safekeeper-%d.neon:5454",
			clusterName, id,
		)
	}

	// Determine if this compute is read-only based on endpoint type label.
	// Only read_write endpoints are WAL proposers; read_only endpoints are followers.
	// If no endpoint-type label is present (e.g. legacy or non-endpoint compute),
	// default to read-write mode for backward compatibility.
	endpointType := deployment.GetLabels()["molnett.org/endpoint-type"]
	readOnly := endpointType == "read_only"
	// Map endpoint type to compute_ctl mode:
	//   read_write → "Primary" (WAL proposer, synced safekeepers)
	//   read_only  → "Replica" (WAL follower, hot standby, no WAL proposal)
	// NOTE: values MUST be capitalized ("Primary"/"Replica") to match compute_ctl's
	// Rust ComputeMode enum variants — lowercase will cause deserialization failure.
	computeMode := "Primary"
	if readOnly {
		computeMode = "Replica"
	}
	log.Info("Compute endpoint type", "type", endpointType, "readOnly", readOnly, "mode", computeMode)

	// 6. 生成 safekeeper WAL 端口 JWT 认证 token（scope=safekeeperdata）
	// 直接读取 JWT Secret 获取私钥签发 token
	var safekeeperAuthToken string
	jwtSecretName := fmt.Sprintf("cluster-%s-jwt", clusterName)
	jwtSecret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: jwtSecretName, Namespace: "neon"}, jwtSecret); err != nil {
		log.Warn("Failed to get JWT secret, safekeeper auth token will be empty", "error", err)
	} else {
		jwtMgr, err := utils.NewJWTManagerFromSecret(jwtSecret)
		if err != nil {
			log.Warn("Failed to create JWT manager, safekeeper auth token will be empty", "error", err)
		} else {
			safekeeperAuthToken, err = utils.GenerateSafekeeperToken(jwtMgr, clusterName)
			if err != nil {
				log.Warn("Failed to generate safekeeper auth token", "error", err)
			}
		}
	}

	// 7. Build postgres settings
	settings := buildPostgresSettings(clusterName, safekeeperIDs, project.Spec.TenantID, branch.Spec.TimelineID, safekeeperAuthToken, readOnly)

	// 8. Generate spec
	shards := make(map[string]PageserverShardInfo)

	var actualRequest *ComputeHookNotifyRequest
	if request != nil {
		// /notify-attach 路径：已有完整 shard 信息
		actualRequest = request
	} else {
		// /spec 路径：需从 storage-controller 获取 shard 信息
		// 优先使用 JWT-authenticated TenantInfoGetter，兜底裸 HTTP
		namespace := deployment.Namespace

		// Layer 2: 重试获取 TenantInfo，最多 3 次，间隔 1s/2s/4s 退避
		var tenantInfo *TenantInfo
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if tenantInfoGetter != nil {
				tenantInfo, lastErr = tenantInfoGetter.GetTenantInfo(ctx, clusterName, namespace, tenantID)
			} else {
				storageClient := NewStorageControllerClient(clusterName)
				tenantInfo, lastErr = storageClient.GetTenantInfo(ctx, log, tenantID)
			}
			if lastErr == nil {
				break
			}
			log.Warn("GetTenantInfo failed, retrying",
				"attempt", attempt+1, "tenantID", tenantID, "error", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}

		if lastErr != nil {
			// Layer 3: storage-controller 不可达 → 返回 Empty 状态
			// compute_ctl 将等待 /notify-attach → /configure 推送完整 spec
			log.Warn("Storage controller unavailable, returning Empty status",
				"tenantID", tenantID, "error", lastErr)

			return &ComputeSpecResponse{
				Spec: ComputeSpec{
					FormatVersion:         1.0,
					SuspendTimeoutSeconds: -1,
					Cluster: ClusterConfig{
						ClusterID: project.Spec.TenantID,
						Name:      project.Name,
						Roles:     roles,
						Databases: databases,
						Settings:  settings,
					},
					DeltaOperations:       []interface{}{},
					SafekeeperConnstrings: safekeeperConnstrings,
					StorageAuthToken:      safekeeperAuthToken,
					Mode:                  computeMode,
					PageserverConnectionInfo: PageserverConnectionInfo{
						ShardCount: 0,
						Shards:     map[string]PageserverShardInfo{},
					},
				},
				ComputeCtlConfig: ComputeCtlConfig{
					JWKS: jwks,
				},
				Status: "empty",
			}, nil // ← 返回 nil error，HTTP 层返回 200 而非 500
		}

		log.Info("Retrieved tenant info", "tenantID", tenantID, "shards", len(tenantInfo.Shards))

		fallbackShards := make([]ComputeHookNotifyRequestShard, len(tenantInfo.Shards))
		for i, shard := range tenantInfo.Shards {
			fallbackShards[i] = ComputeHookNotifyRequestShard{
				NodeID:      shard.NodeAttached,
				ShardNumber: uint32(i),
			}
		}

		actualRequest = &ComputeHookNotifyRequest{
			TenantID:   tenantInfo.TenantID,
			StripeSize: &tenantInfo.StripeSize,
			Shards:     fallbackShards,
		}
	}

	for _, shard := range actualRequest.Shards {
		shardIndex := ShardIndex{
			ShardNumber: 0,
			ShardCount:  uint8(len(actualRequest.Shards)),
		}

		shards[shardIndex.String()] = PageserverShardInfo{
			Pageservers: []PageserverShardConnectionInfo{
				{
					ID: &shard.NodeID,
					LibpqURL: stringPtr(fmt.Sprintf(
						"postgres://no_user@%s-pageserver-%d.neon:6400",
						clusterName, shard.NodeID,
					)),
					GrpcURL: nil,
				},
			},
		}
	}

	spec := &ComputeSpecResponse{
		Spec: ComputeSpec{
			FormatVersion:         1.0,
			SuspendTimeoutSeconds: -1,
			Cluster: ClusterConfig{
				ClusterID: project.Spec.TenantID,
				Name:      project.Name,
				Roles:     roles,
				Databases: databases,
				Settings:  settings,
			},
			DeltaOperations:       []interface{}{},
			SafekeeperConnstrings: safekeeperConnstrings,
			StorageAuthToken:      safekeeperAuthToken,
			Mode:                  computeMode,
			PageserverConnectionInfo: PageserverConnectionInfo{
				ShardCount: len(shards),
				Shards:     shards,
			},
		},
		ComputeCtlConfig: ComputeCtlConfig{
			JWKS: jwks,
		},
		Status: "attached",
	}

	return spec, nil
}

// Helper function to create string pointer
func stringPtr(s string) *string {
	return &s
}

// FindTenantDeployments finds deployments with the specified tenant ID
func FindTenantDeployments(ctx context.Context,
	k8sClient client.Client,
	tenantID string) (*appsv1.DeploymentList, error) {
	deploymentList := &appsv1.DeploymentList{}

	// List all deployments with the tenant_id label
	err := k8sClient.List(ctx, deploymentList, client.MatchingLabels{
		"neon.tenant_id": tenantID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list deployments: %w", err)
	}

	if len(deploymentList.Items) == 0 {
		return nil, fmt.Errorf("no deployment available with the tenantID %s", tenantID)
	}

	return deploymentList, nil
}

func FindTenantTimelineDeployments(ctx context.Context,
	k8sClient client.Client,
	tenantID, timelineID string) (*appsv1.DeploymentList, error) {
	deploymentList := &appsv1.DeploymentList{}

	if err := k8sClient.List(ctx, deploymentList, client.MatchingLabels{
		"neon.tenant_id":   tenantID,
		"neon.timeline_id": timelineID,
	}); err != nil {
		return nil, fmt.Errorf("failed to list deployments: %w", err)
	}

	return deploymentList, nil
}

func RefreshSafekeepersConfiguration(ctx context.Context,
	log *slog.Logger,
	k8sClient client.Client,
	req NotifySafekeepersRequest,
	deployment *appsv1.Deployment,
	computeBaseURL string) error {
	computeId, err := extractComputeID(deployment)
	if err != nil {
		return err
	}

	clusterName, err := extractClusterName(deployment)
	if err != nil {
		return fmt.Errorf("failed to extract clustername from deployment: %w", err)
	}

	// 传入空 ComputeHookNotifyRequest 避免进入 SC 查询路径。
	// RefreshSafekeepersConfiguration 只需基础 spec 结构，safekeeper connstrings
	// 会随后被直接覆盖，因此无需从 Storage Controller 获取 shard 信息。
	spec, err := GenerateComputeSpec(ctx, log, k8sClient, &ComputeHookNotifyRequest{}, computeId, nil)
	if err != nil {
		return fmt.Errorf("failed to generate compute spec: %w", err)
	}

	connstrings := make([]string, len(req.Safekeepers))
	for i, sk := range req.Safekeepers {
		connstrings[i] = fmt.Sprintf(
			"postgresql://postgres:@%s-safekeeper-%d.%s:5454",
			clusterName, sk.ID, deployment.Namespace,
		)
	}
	spec.Spec.SafekeeperConnstrings = connstrings
	gen := req.Generation
	spec.Spec.SafekeepersGeneration = &gen

	return postComputeSpec(ctx, log, k8sClient, spec, deployment, computeId, clusterName, computeBaseURL)
}

// Placeholder functions that need to be implemented elsewhere
func findComputeDeployment(ctx context.Context, k8sClient client.Client, computeID string) (*appsv1.Deployment, error) {
	deploymentList := &appsv1.DeploymentList{}

	// List all deployments with the compute annotation
	err := k8sClient.List(ctx, deploymentList, client.MatchingLabels{
		"molnett.org/component": "compute",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list deployments: %w", err)
	}

	// Find deployment with matching compute_id annotation
	for _, deployment := range deploymentList.Items {
		if annotations := deployment.GetAnnotations(); annotations != nil {
			if computeIDAnnotation, exists := annotations["neon.compute_id"]; exists && computeIDAnnotation == computeID {
				return &deployment, nil
			}
		}
	}

	return nil, fmt.Errorf("deployment with compute_id %s not found", computeID)
}

func extractClusterName(deployment *appsv1.Deployment) (string, error) {
	if annotations := deployment.GetAnnotations(); annotations != nil {
		if clusterName, exists := annotations["neon.cluster_name"]; exists {
			return clusterName, nil
		}
	}

	if labels := deployment.GetLabels(); labels != nil {
		if clusterName, exists := labels["molnett.org/cluster"]; exists {
			return clusterName, nil
		}
	}

	return "", fmt.Errorf("cluster name not found in deployment metadata")
}

func findProjectAndBranch(
	ctx context.Context,
	k8sClient client.Client,
	tenantID, timelineID string,
) (*neonv1alpha1.Project, *neonv1alpha1.Branch, error) {
	// Find project by tenant ID
	projectList := &neonv1alpha1.ProjectList{}
	err := k8sClient.List(ctx, projectList)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list projects: %w", err)
	}

	var project *neonv1alpha1.Project
	for i, p := range projectList.Items {
		if p.Spec.TenantID == tenantID {
			project = &projectList.Items[i]
			break
		}
	}
	if project == nil {
		return nil, nil, fmt.Errorf("project with tenant_id %s not found", tenantID)
	}

	// Find branch by timeline ID
	branchList := &neonv1alpha1.BranchList{}
	err = k8sClient.List(ctx, branchList)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list branches: %w", err)
	}

	var branch *neonv1alpha1.Branch
	for i, b := range branchList.Items {
		if b.Spec.TimelineID == timelineID && b.Spec.ProjectID == project.Name {
			branch = &branchList.Items[i]
			break
		}
	}
	if branch == nil {
		return nil, nil, fmt.Errorf("branch with timeline_id %s not found", timelineID)
	}

	return project, branch, nil
}

func getJWTKeysFromSecret(
	ctx context.Context,
	k8sClient client.Client,
	clusterName string,
) (*utils.JWKResponse, error) {
	secretName := fmt.Sprintf("cluster-%s-jwt", clusterName)
	secret := &corev1.Secret{}

	if err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "neon"}, secret); err != nil {
		return nil, fmt.Errorf("failed to get JWT secret %s: %w", secretName, err)
	}

	jwtManager, err := utils.NewJWTManagerFromSecret(secret)
	if err != nil {
		return nil, fmt.Errorf("failed to create JWT manager from secret: %w", err)
	}

	return &utils.JWKResponse{
		Keys: []*utils.JWK{jwtManager.ToJWK()},
	}, nil
}

func buildPostgresSettings(clusterName string, safekeeperIDs []uint32, tenantID, timelineID, safekeeperAuthToken string, readOnly bool) []SettingsEntry {
	skParts := make([]string, len(safekeeperIDs))
	for i, id := range safekeeperIDs {
		skParts[i] = fmt.Sprintf("%s-safekeeper-%d.neon:5454", clusterName, id)
	}

	entries := []SettingsEntry{
		{Name: "fsync", Value: "off", Vartype: "bool"},
		{Name: "wal_level", Value: "logical", Vartype: "enum"},
		{Name: "wal_log_hints", Value: "on", Vartype: "bool"},
		{Name: "log_connections", Value: "on", Vartype: "bool"},
		{Name: "port", Value: "55433", Vartype: "integer"},
		{Name: "shared_buffers", Value: "16MB", Vartype: "string"},
		{Name: "max_connections", Value: "100", Vartype: "integer"},
		{Name: "listen_addresses", Value: "0.0.0.0", Vartype: "string"},
		{Name: "max_wal_senders", Value: "10", Vartype: "integer"},
		{Name: "max_replication_slots", Value: "10", Vartype: "integer"},
		{Name: "wal_sender_timeout", Value: "5s", Vartype: "string"},
		{Name: "wal_keep_size", Value: "0", Vartype: "integer"},
		{Name: "password_encryption", Value: "scram-sha-256", Vartype: "enum"},
		{Name: "restart_after_crash", Value: "off", Vartype: "bool"},
		{Name: "shared_preload_libraries", Value: "neon", Vartype: "string"},
		{
			Name:    "neon.safekeepers",
			Value:   strings.Join(skParts, ","),
			Vartype: "string",
		},
		{Name: "neon.timeline_id", Value: timelineID, Vartype: "string"},
		{Name: "neon.tenant_id", Value: tenantID, Vartype: "string"},
		{Name: "neon.max_file_cache_size", Value: "1GB", Vartype: "string"},
	}

	// Safekeeper WAL 端口 JWT 认证 token。
	// Safekeeper 通过 --pg-auth-public-key-path 要求所有 WAL 连接（端口 5454）
	// 提供有效 JWT token（scope=safekeeperdata），walproposer 通过此 GUC 使用。
	if safekeeperAuthToken != "" {
		entries = append(entries, SettingsEntry{
			Name: "neon.safekeepers_auth_token", Value: safekeeperAuthToken, Vartype: "string",
		})
	}

	// Only read_write endpoints act as WAL proposers.
	// Read-only replicas consume WAL but do not participate in proposer election.
	if !readOnly {
		entries = append(entries, SettingsEntry{
			Name: "synchronous_standby_names", Value: "walproposer", Vartype: "string",
		})
	}

	return entries
}

// listSafekeeperIDs 查询集群中所有 Safekeeper CR，提取 spec.id 并按升序排列返回。
// 失败时返回 error，调用方自行决定降级策略。
func listSafekeeperIDs(ctx context.Context, k8sClient client.Client, clusterName string) ([]uint32, error) {
	skList := &neonv1alpha1.SafekeeperList{}
	err := k8sClient.List(ctx, skList, client.MatchingLabels{
		"molnett.org/cluster": clusterName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list safekeepers: %w", err)
	}

	ids := make([]uint32, len(skList.Items))
	for i, sk := range skList.Items {
		ids[i] = sk.Spec.ID
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// StorageControllerClient provides bare HTTP (unauthenticated) access to the
// Storage Controller. Deprecated: use controller.SCClient instead, which
// authenticates all requests with JWT tokens. This client remains only for
// backward compatibility in tests and when SCClient is unavailable.
//
// Deprecated: 所有生产路径已切换至 controller.SCClient（带 JWT 认证）。
// 此类型仅保留作为 GenerateComputeSpec 中 tenantInfoGetter==nil 时的兜底路径。
type StorageControllerClient struct {
	clusterName string
}

// NewStorageControllerClient creates a bare HTTP client to the storage controller.
//
// Deprecated: use controller.NewSCClient instead for JWT-authenticated access.
func NewStorageControllerClient(clusterName string) *StorageControllerClient {
	return &StorageControllerClient{clusterName: clusterName}
}

type TenantInfo struct {
	TenantID   string      `json:"tenant_id"`
	StripeSize uint32      `json:"stripe_size"`
	Shards     []ShardInfo `json:"shards"`
}

type ShardInfo struct {
	NodeAttached uint64 `json:"node_attached"`
}

// GetTenantInfo calls GET /control/v1/tenant/{tenantID} with bare HTTP (no JWT).
//
// Deprecated: use controller.SCClient.GetTenantInfo instead for JWT-authenticated access.
func (c *StorageControllerClient) GetTenantInfo(
	ctx context.Context,
	log *slog.Logger,
	tenantID string,
) (*TenantInfo, error) {
	url := fmt.Sprintf("%s/control/v1/tenant/%s", storagecontroller.URL(c.clusterName), tenantID)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get tenant info: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error("failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage controller returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	log.Info("Response body", "tenantID", tenantID, "body", string(body))

	var tenantInfo TenantInfo
	if err := json.Unmarshal(body, &tenantInfo); err != nil {
		return nil, fmt.Errorf("failed to decode tenant info: %w", err)
	}

	return &tenantInfo, nil
}
