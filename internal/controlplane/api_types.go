package controlplane

import (
	"encoding/json"
	"time"
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

// ---------- Project PATCH types ----------

// ProjectUpdateRequest PATCH /api/v2/projects/{project_id} 请求体。
type ProjectUpdateRequest struct {
	Project ProjectUpdatePayload `json:"project"`
}

// ProjectUpdatePayload PATCH 请求体中的可更新字段。
// 所有字段为可选：nil/未出现=Noop（保持原值），非nil=Upsert（设置新值）。
// 对需要 Remove 语义的字段使用 Nullable[T] 包装。
type ProjectUpdatePayload struct {
	// Name 项目显示名称。nil: 保持不变，非nil: 更新名称。
	// +optional
	Name *string `json:"name,omitempty"`

	// DefaultEndpointSettings 默认端点配置。
	// nil: 保持不变，Upsert: 覆盖配置，Remove: 清空。
	// +optional
	DefaultEndpointSettings Nullable[DefaultEndpointSettingsUpdate] `json:"default_endpoint_settings"`

	// HistoryRetentionSeconds 历史数据保留期（秒）。
	// nil: 保持不变，非nil: 更新保留期。
	// +optional
	HistoryRetentionSeconds *int64 `json:"history_retention_seconds,omitempty"`

	// IPAllow IP 白名单配置。
	// nil: 保持不变，Upsert: 覆盖配置，Remove: 移除白名单。
	// +optional
	IPAllow Nullable[IPAllowConfigUpdate] `json:"ip_allow"`
}

// DefaultEndpointSettingsUpdate 端点默认配置的 PATCH 负载。
type DefaultEndpointSettingsUpdate struct {
	// Resources 计算资源规格。
	// +optional
	Resources *ComputeResources `json:"resources,omitempty"`
}

// IPAllowConfigUpdate IP 白名单的 PATCH 负载。
type IPAllowConfigUpdate struct {
	// PrimaryBranchOnly 是否仅对主分支生效。
	PrimaryBranchOnly bool `json:"primary_branch_only"`

	// SourceRanges 允许的 CIDR 范围列表。
	SourceRanges []string `json:"source_ranges"`
}

// IPAllowResp API 响应中的 IP 白名单信息。
type IPAllowResp struct {
	PrimaryBranchOnly bool     `json:"primary_branch_only"`
	SourceRanges      []string `json:"source_ranges"`
}

// ---------- Nullable — FieldPatch 三态语义 ----------

// Nullable 可空包装，用于区分 "未出现"、"显式 null"、"有值" 三种状态。
// 对应 Neon Rust 的 FieldPatch<T> 枚举：
//
//	JSON 字段未出现    → Nullable 自身为 nil (Go zero value) → Noop
//	JSON "field": "v"  → {Valid: true, Value: &v}              → Upsert
//	JSON "field": null → {Valid: true, Value: nil}             → Remove
type Nullable[T any] struct {
	Value *T
	Valid bool
}

// UnmarshalJSON 自定义 JSON 反序列化，区分 null 和未出现。
func (n *Nullable[T]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		n.Valid = true
		n.Value = nil
		return nil
	}
	n.Valid = true
	n.Value = new(T)
	return json.Unmarshal(data, n.Value)
}

// MarshalJSON 自定义 JSON 序列化。
func (n Nullable[T]) MarshalJSON() ([]byte, error) {
	if !n.Valid || n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}

// IsNoop 返回 true 表示该字段在 JSON 中未出现（保持原值）。
func (n Nullable[T]) IsNoop() bool { return !n.Valid }

// IsRemove 返回 true 表示该字段显式传入了 null（重置为默认）。
func (n Nullable[T]) IsRemove() bool { return n.Valid && n.Value == nil }

// IsUpsert 返回 true 表示该字段有具体值（更新）。
func (n Nullable[T]) IsUpsert() bool { return n.Valid && n.Value != nil }

// ----------
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
	ID                      string                   `json:"id"`
	Name                    string                   `json:"name"`
	PGVersion               int                      `json:"pgVersion"`
	Cluster                 string                   `json:"cluster,omitempty"`
	TenantID                string                   `json:"tenant_id,omitempty"`
	CreatedAt               time.Time                `json:"created_at"`
	UpdatedAt               time.Time                `json:"updated_at,omitempty"`
	DefaultSettings         *DefaultEndpointSettings `json:"default_endpoint_settings,omitempty"`
	HistoryRetentionSeconds int64                    `json:"history_retention_seconds,omitempty"`
	IPAllow                 *IPAllowResp             `json:"ip_allow,omitempty"`
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

// ---------- Branch PATCH types ----------

// BranchUpdateRequest PATCH /api/v2/projects/{project_id}/branches/{branch_id}
type BranchUpdateRequest struct {
	Branch BranchUpdatePayload `json:"branch"`
}

// BranchUpdatePayload 分支可更新字段。
// 所有字段为可选：nil/未出现=Noop，非nil=Upsert。
type BranchUpdatePayload struct {
	// Name 分支显示名称（1-256 字符）。
	Name *string `json:"name,omitempty"`
	// Protected 保护分支标记。
	Protected *bool `json:"protected,omitempty"`
}

// ---------- Role PATCH types ----------

// RoleUpdateRequest PATCH /api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}
type RoleUpdateRequest struct {
	Role RoleUpdatePayload `json:"role"`
}

// RoleUpdatePayload 角色可更新字段。
type RoleUpdatePayload struct {
	// Name 重命名角色。
	Name *string `json:"name,omitempty"`
	// Password 重置密码（明文）。
	Password *string `json:"password,omitempty"`
}

// RoleGetResponse GET single role 响应包装
type RoleGetResponse struct {
	Role RoleResponse `json:"role"`
}

// RoleUpdateResponse PATCH role 响应（含 operations 跟踪异步操作）
type RoleUpdateResponse struct {
	Role       RoleResponse        `json:"role"`
	Operations []OperationResponse `json:"operations"`
}

// ---------- Endpoint PATCH types ----------

// EndpointUpdateRequest PATCH /api/v2/projects/{project_id}/endpoints/{endpoint_id}
type EndpointUpdateRequest struct {
	Endpoint EndpointUpdatePayload `json:"endpoint"`
}

// EndpointUpdatePayload 端点可更新字段。
type EndpointUpdatePayload struct {
	// BranchID 将端点迁移到另一个分支。
	BranchID *string `json:"branch_id,omitempty"`
	// Resources 计算资源规格。
	Resources *ComputeResources `json:"resources,omitempty"`
	// Disabled 禁用以暂停连接。
	Disabled *bool `json:"disabled,omitempty"`
	// SuspendTimeoutSeconds Serverless 空闲自动挂起超时（预留）。
	SuspendTimeoutSeconds *int32 `json:"suspend_timeout_seconds,omitempty"`
}

// EndpointGetResponse GET single endpoint 响应包装
type EndpointGetResponse struct {
	Endpoint EndpointResponse `json:"endpoint"`
}

// EndpointActionResponse start/suspend/restart 响应（含 operations）
type EndpointActionResponse struct {
	Endpoint   EndpointResponse    `json:"endpoint"`
	Operations []OperationResponse `json:"operations"`
}

// ---------- Database PATCH types ----------

// DatabaseUpdateRequest PATCH /api/v2/projects/{project_id}/branches/{branch_id}/databases/{database_name}
type DatabaseUpdateRequest struct {
	Database DatabaseUpdatePayload `json:"database"`
}

// DatabaseUpdatePayload 数据库可更新字段。
type DatabaseUpdatePayload struct {
	// Name 重命名数据库。
	Name *string `json:"name,omitempty"`
	// OwnerName 修改数据库所有者。
	OwnerName *string `json:"owner_name,omitempty"`
}

// DatabaseGetResponse GET single database 响应包装
type DatabaseGetResponse struct {
	Database DatabaseResponse `json:"database"`
}

// ---------- Connection URI types ----------

// ConnectionURIResponse GET connection_uri 响应（URI + 元信息）
type ConnectionURIResponse struct {
	URI          string `json:"uri"`
	Pooled       bool   `json:"pooled"`
	DatabaseName string `json:"database_name"`
	RoleName     string `json:"role_name"`
	BranchID     string `json:"branch_id"`
	EndpointID   string `json:"endpoint_id"`
}
