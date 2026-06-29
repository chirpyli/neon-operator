package controlplane

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// =============================================================================
// Neon API v2 兼容的请求/响应类型
// =============================================================================

// ---------- Request types ----------

// ProjectCreateRequest POST /api/v2/projects
type ProjectCreateRequest struct {
	Project ProjectCreatePayload `json:"project"`
}

// ProjectCreatePayload 项目创建负载
type ProjectCreatePayload struct {
	Name            string                   `json:"name"`
	PGVersion       int                      `json:"pgVersion,omitempty"`
	Cluster         string                   `json:"cluster,omitempty"`
	Branch          *BranchPayload           `json:"branch,omitempty"`
	Endpoint        *EndpointPayload         `json:"endpoint,omitempty"`
	DefaultSettings *DefaultEndpointSettings `json:"default_endpoint_settings,omitempty"`
}

// DefaultEndpointSettings 端点默认配置
type DefaultEndpointSettings struct {
	Resources *ComputeResources `json:"resources,omitempty"`
}

// ComputeResources 计算资源
type ComputeResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// BranchPayload 分支创建负载
type BranchPayload struct {
	Name         string `json:"name,omitempty"`
	RoleName     string `json:"role_name,omitempty"`
	DatabaseName string `json:"database_name,omitempty"`
}

// EndpointPayload 端点创建负载
type EndpointPayload struct {
	Type      string            `json:"type,omitempty"`
	Resources *ComputeResources `json:"resources,omitempty"`
	Exposure  *ServiceExposure  `json:"exposure,omitempty"`
}

// ServiceExposure 服务暴露配置
type ServiceExposure struct {
	Type         string   `json:"type,omitempty"`
	SourceRanges []string `json:"source_ranges,omitempty"`
}

// BranchCreateRequest POST /api/v2/projects/{project_id}/branches
type BranchCreateRequest struct {
	Branch    BranchCreatePayload `json:"branch"`
	Endpoints []EndpointPayload   `json:"endpoints,omitempty"`
}

// BranchCreatePayload 分支创建详细负载
type BranchCreatePayload struct {
	Name            string `json:"name,omitempty"`
	ParentID        string `json:"parent_id,omitempty"`
	ParentLSN       string `json:"parent_lsn,omitempty"`
	ParentTimestamp string `json:"parent_timestamp,omitempty"`
	InitSource      string `json:"init_source,omitempty"`
	Protected       bool   `json:"protected,omitempty"`
}

// EndpointCreateRequest POST /api/v2/projects/{project_id}/endpoints
type EndpointCreateRequest struct {
	Endpoint EndpointCreatePayload `json:"endpoint"`
}

// EndpointCreatePayload 端点创建详细负载
type EndpointCreatePayload struct {
	BranchID  string            `json:"branch_id"`
	Type      string            `json:"type,omitempty"`
	Resources *ComputeResources `json:"resources,omitempty"`
	Disabled  bool              `json:"disabled,omitempty"`
	Exposure  *ServiceExposure  `json:"exposure,omitempty"`
}

// RoleCreateRequest POST /api/v2/projects/{project_id}/branches/{branch_id}/roles
type RoleCreateRequest struct {
	Role RoleCreatePayload `json:"role"`
}

// RoleCreatePayload 角色创建负载
type RoleCreatePayload struct {
	Name                 string `json:"name"`
	AuthenticationMethod string `json:"authentication_method,omitempty"`
}

// DatabaseCreateRequest POST /api/v2/projects/{project_id}/branches/{branch_id}/databases
type DatabaseCreateRequest struct {
	Database DatabaseCreatePayload `json:"database"`
}

// DatabaseCreatePayload 数据库创建负载
type DatabaseCreatePayload struct {
	Name      string `json:"name"`
	OwnerName string `json:"owner_name,omitempty"`
}

// ---------- Response types ----------

// ProjectResponse 项目响应
type ProjectResponse struct {
	ID              string                   `json:"id"`
	Name            string                   `json:"name"`
	PGVersion       int                      `json:"pgVersion"`
	Cluster         string                   `json:"cluster,omitempty"`
	TenantID        string                   `json:"tenant_id,omitempty"`
	CreatedAt       time.Time                `json:"created_at"`
	UpdatedAt       time.Time                `json:"updated_at,omitempty"`
	DefaultSettings *DefaultEndpointSettings `json:"default_endpoint_settings,omitempty"`
}

// BranchResponse 分支响应
type BranchResponse struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	ProjectID    string    `json:"project_id"`
	ParentID     string    `json:"parent_id,omitempty"`
	ParentLSN    string    `json:"parent_lsn,omitempty"`
	CurrentState string    `json:"current_state,omitempty"`
	Default      bool      `json:"default,omitempty"`
	Protected    bool      `json:"protected,omitempty"`
	InitSource   string    `json:"init_source,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

// EndpointResponse 端点响应
type EndpointResponse struct {
	ID           string    `json:"id"`
	BranchID     string    `json:"branch_id"`
	Type         string    `json:"type"`
	Host         string    `json:"host,omitempty"`
	Port         int32     `json:"port,omitempty"`
	CurrentState string    `json:"current_state,omitempty"`
	Disabled     bool      `json:"disabled,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// RoleResponse 角色响应
type RoleResponse struct {
	Name                 string    `json:"name"`
	Password             string    `json:"password,omitempty"`
	Protected            bool      `json:"protected,omitempty"`
	BranchID             string    `json:"branch_id"`
	AuthenticationMethod string    `json:"authentication_method,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// DatabaseResponse 数据库响应
type DatabaseResponse struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	OwnerName string    `json:"owner_name"`
	BranchID  string    `json:"branch_id"`
	CreatedAt time.Time `json:"created_at"`
}

// OperationResponse 操作响应
type OperationResponse struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	BranchID   string     `json:"branch_id,omitempty"`
	EndpointID string     `json:"endpoint_id,omitempty"`
	Action     string     `json:"action"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

// ConnectionURI 连接信息
type ConnectionURI struct {
	ConnectionURI        string            `json:"connection_uri"`
	ConnectionParameters map[string]string `json:"connection_parameters"`
}

// ---------- Aggregate response types ----------

// CreatedProjectResponse POST /api/v2/projects 201
type CreatedProjectResponse struct {
	Project        ProjectResponse     `json:"project"`
	Branch         *BranchResponse     `json:"branch,omitempty"`
	Endpoints      []EndpointResponse  `json:"endpoints"`
	ConnectionURIs []ConnectionURI     `json:"connection_uris"`
	Roles          []RoleResponse      `json:"roles"`
	Databases      []DatabaseResponse  `json:"databases"`
	Operations     []OperationResponse `json:"operations"`
}

// CreatedBranchResponse POST .../branches 201
type CreatedBranchResponse struct {
	Branch     BranchResponse      `json:"branch"`
	Endpoints  []EndpointResponse  `json:"endpoints"`
	Operations []OperationResponse `json:"operations"`
}

// CreatedEndpointResponse POST .../endpoints 201
type CreatedEndpointResponse struct {
	Endpoint   EndpointResponse    `json:"endpoint"`
	Operations []OperationResponse `json:"operations"`
}

// ProjectListResponse GET /api/v2/projects 200
type ProjectListResponse struct {
	Projects   []ProjectResponse `json:"projects"`
	Pagination *Pagination       `json:"pagination,omitempty"`
}

// BranchListResponse GET .../branches 200
type BranchListResponse struct {
	Branches []BranchResponse `json:"branches"`
}

// EndpointListResponse GET .../endpoints 200
type EndpointListResponse struct {
	Endpoints []EndpointResponse `json:"endpoints"`
}

// RoleListResponse GET .../roles 200
type RoleListResponse struct {
	Roles []RoleResponse `json:"roles"`
}

// DatabaseListResponse GET .../databases 200
type DatabaseListResponse struct {
	Databases []DatabaseResponse `json:"databases"`
}

// OperationListResponse GET .../operations 200
type OperationListResponse struct {
	Operations []OperationResponse `json:"operations"`
}

// Pagination 分页信息
type Pagination struct {
	Cursor  string `json:"cursor,omitempty"`
	HasMore bool   `json:"has_more"`
}

// ResetPasswordResponse POST .../reset_password 200
type ResetPasswordResponse struct {
	Role RoleResponse `json:"role"`
}

// DeleteResponse 删除响应
type DeleteResponse struct {
	Project    *ProjectResponse    `json:"project,omitempty"`
	Branch     *BranchResponse     `json:"branch,omitempty"`
	Endpoint   *EndpointResponse   `json:"endpoint,omitempty"`
	Operations []OperationResponse `json:"operations"`
}

// ErrorResponse 统一错误响应
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---------- compute_ctl 兼容类型 ----------

// Cluster在ComputeSpec中的定义 (复制以避免循环依赖)
type clusterDef struct {
	ClusterID string        `json:"cluster_id"`
	Name      string        `json:"name"`
	Roles     []clusterRole `json:"roles"`
	Databases []clusterDB   `json:"databases"`
}

type clusterRole struct {
	Name        string `json:"name"`
	Password    string `json:"password,omitempty"`
	ConnLimit   int    `json:"conn_limit,omitempty"`
	IsSuperuser bool   `json:"is_superuser,omitempty"`
	CanLogin    bool   `json:"can_login,omitempty"`
}

type clusterDB struct {
	Name      string `json:"name"`
	OwnerName string `json:"owner_name"`
}

// ---------- 资源限制检查类型 ----------

// ResourceQuota 资源配额 (Phase 3+)
type ResourceQuota struct {
	MaxProjects  int `json:"max_projects,omitempty"`
	MaxBranches  int `json:"max_branches,omitempty"`
	MaxEndpoints int `json:"max_endpoints,omitempty"`
}

// GeneratedIDs 创建 Project 时生成的内部 ID
type GeneratedIDs struct {
	ProjectID  string
	BranchID   string
	EndpointID string
	TenantID   string
	TimelineID string
	OpID       string
}

// computeSpecData 传递给 compute_ctl 的完整 spec 数据
type computeSpecData struct {
	TenantID              string                       `json:"tenant_id"`
	TimelineID            string                       `json:"timeline_id"`
	PageserverConnInfo    interface{}                  `json:"pageserver_conn_info,omitempty"`
	SafekeeperConnstrings []string                     `json:"safekeeper_connstrings,omitempty"`
	StorageAuthToken      string                       `json:"storage_auth_token,omitempty"`
	Cluster               clusterDef                   `json:"cluster"`
	Mode                  string                       `json:"mode,omitempty"`
	Resources             *corev1.ResourceRequirements `json:"resources,omitempty"`
}
