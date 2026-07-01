package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	neonv1 "oltp.molnett.org/neon-operator/api/v1alpha1"
)

// neonIDLength is the length of generated Neon IDs (tenants, timelines).
const neonIDLength = 32

// =============================================================================
// apiService — Control Plane API 业务逻辑层
// =============================================================================

type apiService struct {
	log       *slog.Logger
	k8sClient client.Client
	namespace string
}

// =============================================================================
// ID 和密码生成
// =============================================================================

func generateNeonID() string {
	b := make([]byte, neonIDLength/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func generateResourceID(prefix string) string {
	return prefix + "-" + uuid.New().String()[:8]
}

func generateOperationID() string {
	return "op-" + uuid.New().String()
}

func generatePassword() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const length = 32
	password := make([]byte, length)
	for i := range password {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		password[i] = charset[n.Int64()]
	}
	return string(password)
}

// =============================================================================
// Project 服务
// =============================================================================

// CreateProject 创建完整项目生态：Project + Branch + Endpoint + Role + Database + Operation
func (s *apiService) CreateProject(ctx context.Context, req ProjectCreateRequest) (*CreatedProjectResponse, error) {
	now := time.Now()

	// 生成 ID
	ids := GeneratedIDs{
		ProjectID:  generateResourceID("project"),
		TenantID:   generateNeonID(),
		BranchID:   generateResourceID("br"),
		TimelineID: generateNeonID(),
		EndpointID: generateResourceID("ep"),
		OpID:       generateOperationID(),
	}

	projectName := req.Project.Name
	if projectName == "" {
		return nil, newError("INVALID_NAME", "project name is required")
	}
	pgVersion := req.Project.PGVersion
	if pgVersion == 0 {
		pgVersion = 17
	}
	clusterName := req.Project.Cluster

	branchName := "main"
	roleName := projectName + "_owner"
	dbName := "neondb"

	if req.Project.Branch != nil {
		if req.Project.Branch.Name != "" {
			branchName = req.Project.Branch.Name
		}
		if req.Project.Branch.RoleName != "" {
			roleName = req.Project.Branch.RoleName
		}
		if req.Project.Branch.DatabaseName != "" {
			dbName = req.Project.Branch.DatabaseName
		}
	}

	// 1. 创建 Project CR
	project := &neonv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ids.ProjectID,
			Namespace: s.namespace,
		},
		Spec: neonv1.ProjectSpec{
			Name:        projectName,
			ClusterName: clusterName,
			TenantID:    ids.TenantID,
			PGVersion:   pgVersion,
		},
	}
	if req.Project.DefaultSettings != nil && req.Project.DefaultSettings.Resources != nil {
		project.Spec.DefaultEndpointSettings = &neonv1.EndpointDefaults{
			Resources: toResourceRequirements(req.Project.DefaultSettings.Resources),
		}
	}
	if err := s.k8sClient.Create(ctx, project); err != nil {
		return nil, fmt.Errorf("create project CR: %w", err)
	}
	s.log.Info("created project CR", "projectID", ids.ProjectID)

	// 2. 创建 Branch CR
	branch := &neonv1.Branch{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ids.BranchID,
			Namespace: s.namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(project, neonv1.GroupVersion.WithKind("Project")),
			},
		},
		Spec: neonv1.BranchSpec{
			Name:        branchName,
			ProjectID:   ids.ProjectID,
			TimelineID:  ids.TimelineID,
			PGVersion:   pgVersion,
			InitSource:  "parent-data",
			Default:     true,
		},
	}
	if err := s.k8sClient.Create(ctx, branch); err != nil {
		_ = s.k8sClient.Delete(ctx, project)
		return nil, fmt.Errorf("create branch CR: %w", err)
	}
	s.log.Info("created branch CR", "branchID", ids.BranchID, "projectID", ids.ProjectID)

	// 3. 创建 Endpoint CR
	endpointType := "read_write"
	var endpointResources *corev1.ResourceRequirements
	if req.Project.Endpoint != nil {
		if req.Project.Endpoint.Type != "" {
			endpointType = req.Project.Endpoint.Type
		}
		if req.Project.Endpoint.Resources != nil {
			endpointResources = toResourceRequirements(req.Project.Endpoint.Resources)
		}
	}
	if endpointResources == nil && req.Project.DefaultSettings != nil && req.Project.DefaultSettings.Resources != nil {
		endpointResources = toResourceRequirements(req.Project.DefaultSettings.Resources)
	}

	endpoint := &neonv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ids.EndpointID,
			Namespace: s.namespace,
			Labels: map[string]string{
				"molnett.org/branch": ids.BranchID,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(branch, neonv1.GroupVersion.WithKind("Branch")),
			},
		},
		Spec: neonv1.EndpointSpec{
			BranchID:  ids.BranchID,
			Type:      endpointType,
			Resources: endpointResources,
			Exposure:  toNeonExposure(req.Project.Endpoint),
		},
	}
	if err := s.k8sClient.Create(ctx, endpoint); err != nil {
		return nil, fmt.Errorf("create endpoint CR: %w", err)
	}
	s.log.Info("created endpoint CR", "endpointID", ids.EndpointID, "branchID", ids.BranchID)

	// 4. 创建 Role CR (初始角色)
	password := generatePassword()
	role := &neonv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", ids.BranchID, roleName),
			Namespace: s.namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(branch, neonv1.GroupVersion.WithKind("Branch")),
			},
		},
		Spec: neonv1.RoleSpec{
			BranchID:             ids.BranchID,
			Name:                 roleName,
			AuthenticationMethod: "password",
		},
		Status: neonv1.RoleStatus{
			Protected: true,
			Phase:     "active",
		},
	}
	if err := s.k8sClient.Create(ctx, role); err != nil {
		return nil, fmt.Errorf("create role CR: %w", err)
	}

	// 创建密码 Secret
	// 命名必须与 Role Controller 的 ensurePasswordSecret 保持一致：
	//   role-{role.Name}-password
	secretName := fmt.Sprintf("role-%s-%s-password", ids.BranchID, roleName)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(role, neonv1.GroupVersion.WithKind("Role")),
			},
		},
		StringData: map[string]string{
			"password": password,
		},
	}
	if err := s.k8sClient.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("create role password secret: %w", err)
	}
	if role.Status.PasswordSecretRef == nil {
		role.Status.PasswordSecretRef = &corev1.SecretReference{
			Name:      secretName,
			Namespace: s.namespace,
		}
	}
	if err := s.k8sClient.Status().Update(ctx, role); err != nil {
		s.log.Warn("failed to update role status with secret ref", "error", err)
	}

	// 5. 创建 Database CR
	database := &neonv1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", ids.BranchID, dbName),
			Namespace: s.namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(branch, neonv1.GroupVersion.WithKind("Branch")),
			},
		},
		Spec: neonv1.DatabaseSpec{
			BranchID:  ids.BranchID,
			Name:      dbName,
			OwnerName: roleName,
		},
	}
	if err := s.k8sClient.Create(ctx, database); err != nil {
		return nil, fmt.Errorf("create database CR: %w", err)
	}

	// 6. 创建 Operation CR
	op := &neonv1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ids.OpID,
			Namespace: s.namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(project, neonv1.GroupVersion.WithKind("Project")),
			},
		},
		Spec: neonv1.OperationSpec{
			Action:     "create_project",
			Status:     "scheduling",
			ProjectID:  ids.ProjectID,
			BranchID:   ids.BranchID,
			EndpointID: ids.EndpointID,
		},
	}
	if err := s.k8sClient.Create(ctx, op); err != nil {
		return nil, fmt.Errorf("create operation CR: %w", err)
	}

	// 组装响应
	resp := &CreatedProjectResponse{
		Project: ProjectResponse{
			ID:        ids.ProjectID,
			Name:      projectName,
			PGVersion: pgVersion,
			Cluster:   clusterName,
			TenantID:  ids.TenantID,
			CreatedAt: now,
			UpdatedAt: now,
			DefaultSettings: &DefaultEndpointSettings{
				Resources: toComputeResources(endpointResources),
			},
		},
		Branch: &BranchResponse{
			ID:           ids.BranchID,
			Name:         branchName,
			ProjectID:    ids.ProjectID,
			CurrentState: "init",
			Default:      true,
			InitSource:   "parent-data",
			CreatedAt:    now,
		},
		Endpoints: []EndpointResponse{{
			ID:           ids.EndpointID,
			BranchID:     ids.BranchID,
			Type:         endpointType,
			CurrentState: "init",
			CreatedAt:    now,
		}},
		Roles: []RoleResponse{{
			Name:                 roleName,
			Password:             password,
			Protected:            true,
			BranchID:             ids.BranchID,
			AuthenticationMethod: "password",
			CreatedAt:            now,
		}},
		Databases: []DatabaseResponse{{
			ID:        1,
			Name:      dbName,
			OwnerName: roleName,
			BranchID:  ids.BranchID,
			CreatedAt: now,
		}},
		Operations: []OperationResponse{{
			ID:         ids.OpID,
			ProjectID:  ids.ProjectID,
			BranchID:   ids.BranchID,
			EndpointID: ids.EndpointID,
			Action:     "create_project",
			Status:     "scheduling",
			CreatedAt:  now,
		}},
	}

	return resp, nil
}

// GetProject 获取项目详情
func (s *apiService) GetProject(ctx context.Context, projectID string) (*ProjectResponse, error) {
	project := &neonv1.Project{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: projectID, Namespace: s.namespace}, project); err != nil {
		if isNotFound(err) {
			return nil, newError("PROJECT_NOT_FOUND", "project '"+projectID+"' not found")
		}
		return nil, fmt.Errorf("get project: %w", err)
	}

	return s.toProjectResponse(project), nil
}

// ListProjects 列出项目
func (s *apiService) ListProjects(ctx context.Context) (*ProjectListResponse, error) {
	list := &neonv1.ProjectList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	projects := make([]ProjectResponse, 0, len(list.Items))
	for _, p := range list.Items {
		projects = append(projects, ProjectResponse{
			ID:        p.Name,
			Name:      p.Spec.Name,
			PGVersion: p.Spec.PGVersion,
			Cluster:   p.Spec.ClusterName,
			TenantID:  p.Spec.TenantID,
			CreatedAt: p.CreationTimestamp.Time,
		})
	}

	return &ProjectListResponse{
		Projects:   projects,
		Pagination: &Pagination{HasMore: false},
	}, nil
}

// UpdateProject 部分更新项目配置（对标 Neon PATCH /api/v2/projects/{project_id}）。
// 使用 K8s MergeFrom patch 实现原子更新，未传字段保持原值。
func (s *apiService) UpdateProject(ctx context.Context, projectID string, req ProjectUpdateRequest) (*ProjectResponse, error) {
	project := &neonv1.Project{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: projectID, Namespace: s.namespace}, project); err != nil {
		if isNotFound(err) {
			return nil, newError("PROJECT_NOT_FOUND", "project '"+projectID+"' not found")
		}
		return nil, fmt.Errorf("get project: %w", err)
	}

	// 保存原始对象用于生成 strategic merge patch
	original := project.DeepCopy()

	changed, err := s.applyProjectPatch(project, req.Project)
	if err != nil {
		return nil, err
	}

	if !changed {
		// 无实际变更，直接返回当前状态（幂等）
		return s.toProjectResponse(project), nil
	}

	// 执行 K8s Patch（原子操作，仅发送变更字段）
	if err := s.k8sClient.Patch(ctx, project, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("patch project: %w", err)
	}

	s.log.Info("patched project", "projectID", projectID)

	return s.toProjectResponse(project), nil
}

// applyProjectPatch 将 PATCH 请求中的变更应用到 Project CR Spec（内存中）。
// 返回 changed=true 表示有实际变更需要持久化。
func (s *apiService) applyProjectPatch(project *neonv1.Project, patch ProjectUpdatePayload) (changed bool, err error) {
	spec := &project.Spec

	// --- name: Upsert / Noop ---
	if patch.Name != nil {
		if len(*patch.Name) < 1 || len(*patch.Name) > 256 {
			return false, newError("INVALID_NAME", "project name must be 1-256 characters")
		}
		spec.Name = *patch.Name
		changed = true
	}

	// --- default_endpoint_settings: Upsert / Remove / Noop ---
	if patch.DefaultEndpointSettings.IsUpsert() {
		ds := patch.DefaultEndpointSettings.Value
		if spec.DefaultEndpointSettings == nil {
			spec.DefaultEndpointSettings = &neonv1.EndpointDefaults{}
		}
		if ds.Resources != nil {
			spec.DefaultEndpointSettings.Resources = toResourceRequirements(ds.Resources)
			changed = true
		}
	} else if patch.DefaultEndpointSettings.IsRemove() {
		spec.DefaultEndpointSettings = nil
		changed = true
	}

	// --- history_retention_seconds: Upsert / Noop ---
	if patch.HistoryRetentionSeconds != nil {
		if *patch.HistoryRetentionSeconds < 0 {
			return false, newError("VALIDATION_ERROR", "history_retention_seconds must be >= 0")
		}
		spec.HistoryRetentionSeconds = *patch.HistoryRetentionSeconds
		changed = true
	}

	// --- ip_allow: Upsert / Remove / Noop ---
	if patch.IPAllow.IsUpsert() {
		cfg := patch.IPAllow.Value
		spec.IPAllow = &neonv1.IPAllowConfig{
			PrimaryBranchOnly: cfg.PrimaryBranchOnly,
			SourceRanges:      cfg.SourceRanges,
		}
		changed = true
	} else if patch.IPAllow.IsRemove() {
		spec.IPAllow = nil
		changed = true
	}

	return changed, nil
}

// toProjectResponse 将 Project CR 转换为 API 响应。
func (s *apiService) toProjectResponse(project *neonv1.Project) *ProjectResponse {
	return &ProjectResponse{
		ID:                      project.Name,
		Name:                    project.Spec.Name,
		PGVersion:               project.Spec.PGVersion,
		Cluster:                 project.Spec.ClusterName,
		TenantID:                project.Spec.TenantID,
		CreatedAt:               project.CreationTimestamp.Time,
		UpdatedAt:               now(),
		DefaultSettings:         toDefaultSettings(project.Spec.DefaultEndpointSettings),
		HistoryRetentionSeconds: project.Spec.HistoryRetentionSeconds,
		IPAllow:                 toIPAllowResponse(project.Spec.IPAllow),
	}
}

// toIPAllowResponse 将 CR 中的 IPAllowConfig 转换为 API 响应格式。
func toIPAllowResponse(cfg *neonv1.IPAllowConfig) *IPAllowResp {
	if cfg == nil {
		return nil
	}
	return &IPAllowResp{
		PrimaryBranchOnly: cfg.PrimaryBranchOnly,
		SourceRanges:      cfg.SourceRanges,
	}
}

// DeleteProject 删除项目
func (s *apiService) DeleteProject(ctx context.Context, projectID string) (*DeleteResponse, error) {
	project := &neonv1.Project{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: projectID, Namespace: s.namespace}, project); err != nil {
		if isNotFound(err) {
			return nil, newError("PROJECT_NOT_FOUND", "project '"+projectID+"' not found")
		}
		return nil, fmt.Errorf("get project: %w", err)
	}

	// 创建 delete operation
	opID := generateOperationID()
	op := &neonv1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opID,
			Namespace: s.namespace,
		},
		Spec: neonv1.OperationSpec{
			Action:    "delete_project",
			Status:    "scheduling",
			ProjectID: projectID,
		},
	}
	if err := s.k8sClient.Create(ctx, op); err != nil {
		return nil, fmt.Errorf("create delete operation: %w", err)
	}

	projectName := project.Spec.Name

	// K8s 级联删除依赖 ownerReferences
	if err := s.k8sClient.Delete(ctx, project); err != nil {
		return nil, fmt.Errorf("delete project: %w", err)
	}

	return &DeleteResponse{
		Project: &ProjectResponse{
			ID:   projectID,
			Name: projectName,
		},
		Operations: []OperationResponse{{
			ID:        opID,
			ProjectID: projectID,
			Action:    "delete_project",
			Status:    "scheduling",
			CreatedAt: now(),
		}},
	}, nil
}

// =============================================================================
// Branch 服务
// =============================================================================

// CreateBranch 创建分支（可选附带 Endpoint）
func (s *apiService) CreateBranch(ctx context.Context, projectID string, req BranchCreateRequest) (*CreatedBranchResponse, error) {
	now := time.Now()

	// 验证 Project 存在
	project := &neonv1.Project{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: projectID, Namespace: s.namespace}, project); err != nil {
		if isNotFound(err) {
			return nil, newError("PROJECT_NOT_FOUND", "project '"+projectID+"' not found")
		}
		return nil, fmt.Errorf("get project: %w", err)
	}

	branchID := generateResourceID("br")
	timelineID := generateNeonID()
	opID := generateOperationID()

	branchName := req.Branch.Name
	if branchName == "" {
		branchName = "branch-" + uuid.New().String()[:8]
	}
	initSource := req.Branch.InitSource
	if initSource == "" {
		initSource = "parent-data"
	}

	// 查找 parent branch
	parentBranchName := req.Branch.ParentID
	if parentBranchName == "" {
		// 找默认分支
		defaultBranch, err := s.findDefaultBranch(ctx, projectID)
		if err != nil {
			return nil, err
		}
		parentBranchName = defaultBranch.Name
	}

	branch := &neonv1.Branch{
		ObjectMeta: metav1.ObjectMeta{
			Name:      branchID,
			Namespace: s.namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(project, neonv1.GroupVersion.WithKind("Project")),
			},
		},
		Spec: neonv1.BranchSpec{
			Name:            branchName,
			ProjectID:       projectID,
			TimelineID:      timelineID,
			PGVersion:       project.Spec.PGVersion,
			ParentBranch:    parentBranchName,
			ParentLSN:       req.Branch.ParentLSN,
			ParentTimestamp: req.Branch.ParentTimestamp,
			InitSource:      initSource,
			Protected:       req.Branch.Protected,
			Default:         false,
		},
	}
	if err := s.k8sClient.Create(ctx, branch); err != nil {
		return nil, fmt.Errorf("create branch CR: %w", err)
	}

	// 创建可选的 Endpoints
	endpoints := make([]EndpointResponse, 0, len(req.Endpoints))
	for _, ep := range req.Endpoints {
		epResp, err := s.createEndpointForBranch(ctx, project, branchID, ep)
		if err != nil {
			s.log.Error("failed to create endpoint for branch", "error", err)
			continue
		}
		endpoints = append(endpoints, *epResp)
	}

	// 创建 Operation
	op := &neonv1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opID,
			Namespace: s.namespace,
		},
		Spec: neonv1.OperationSpec{
			Action:    "create_branch",
			Status:    "scheduling",
			ProjectID: projectID,
			BranchID:  branchID,
		},
	}
	if err := s.k8sClient.Create(ctx, op); err != nil {
		return nil, fmt.Errorf("create operation: %w", err)
	}

	return &CreatedBranchResponse{
		Branch: BranchResponse{
			ID:           branchID,
			Name:         branchName,
			ProjectID:    projectID,
			ParentID:     parentBranchName,
			ParentLSN:    req.Branch.ParentLSN,
			CurrentState: "init",
			InitSource:   initSource,
			Protected:    req.Branch.Protected,
			CreatedAt:    now,
		},
		Endpoints: endpoints,
		Operations: []OperationResponse{{
			ID:        opID,
			ProjectID: projectID,
			BranchID:  branchID,
			Action:    "create_branch",
			Status:    "scheduling",
			CreatedAt: now,
		}},
	}, nil
}

// GetBranch 获取分支详情
func (s *apiService) GetBranch(ctx context.Context, projectID, branchID string) (*BranchResponse, error) {
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found in project '"+projectID+"'")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found in project '"+projectID+"'")
	}

	return &BranchResponse{
		ID:           branch.Name,
		Name:         branch.Spec.Name,
		ProjectID:    projectID,
		ParentID:     branch.Spec.ParentBranch,
		ParentLSN:    branch.Spec.ParentLSN,
		CurrentState: "ready",
		Default:      branch.Spec.Default,
		Protected:    branch.Spec.Protected,
		InitSource:   branch.Spec.InitSource,
		CreatedAt:    branch.CreationTimestamp.Time,
	}, nil
}

// ListBranches 列出项目的所有分支
func (s *apiService) ListBranches(ctx context.Context, projectID string) (*BranchListResponse, error) {
	list := &neonv1.BranchList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}

	branches := make([]BranchResponse, 0)
	for _, b := range list.Items {
		if b.Spec.ProjectID != projectID {
			continue
		}
		branches = append(branches, BranchResponse{
			ID:           b.Name,
			Name:         b.Spec.Name,
			ProjectID:    projectID,
			ParentID:     b.Spec.ParentBranch,
			CurrentState: "ready",
			Default:      b.Spec.Default,
			Protected:    b.Spec.Protected,
			InitSource:   b.Spec.InitSource,
			CreatedAt:    b.CreationTimestamp.Time,
		})
	}

	return &BranchListResponse{Branches: branches}, nil
}

// DeleteBranch 删除分支
func (s *apiService) DeleteBranch(ctx context.Context, projectID, branchID string) (*DeleteResponse, error) {
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found in project '"+projectID+"'")
	}
	if branch.Spec.Default {
		return nil, newError("DEFAULT_BRANCH_DELETE", "default branch cannot be deleted")
	}
	if branch.Spec.Protected {
		return nil, newError("PROTECTED_BRANCH", "protected branch cannot be deleted")
	}

	opID := generateOperationID()
	op := &neonv1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opID,
			Namespace: s.namespace,
		},
		Spec: neonv1.OperationSpec{
			Action:    "delete_branch",
			Status:    "scheduling",
			ProjectID: projectID,
			BranchID:  branchID,
		},
	}
	if err := s.k8sClient.Create(ctx, op); err != nil {
		return nil, fmt.Errorf("create operation: %w", err)
	}

	// 删除 Branch（级联删除子资源）
	if err := s.k8sClient.Delete(ctx, branch); err != nil {
		return nil, fmt.Errorf("delete branch: %w", err)
	}

	return &DeleteResponse{
		Branch: &BranchResponse{ID: branchID},
		Operations: []OperationResponse{{
			ID:        opID,
			ProjectID: projectID,
			BranchID:  branchID,
			Action:    "delete_branch",
			Status:    "scheduling",
			CreatedAt: now(),
		}},
	}, nil
}

// UpdateBranch 部分更新分支（对标 Neon PATCH /api/v2/projects/{project_id}/branches/{branch_id}）。
func (s *apiService) UpdateBranch(ctx context.Context, projectID, branchID string, req BranchUpdateRequest) (*BranchResponse, error) {
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	original := branch.DeepCopy()
	changed := false

	if req.Branch.Name != nil {
		n := *req.Branch.Name
		if len(n) < 1 || len(n) > 256 {
			return nil, newError("INVALID_NAME", "branch name must be 1-256 characters")
		}
		branch.Spec.Name = n
		changed = true
	}
	if req.Branch.Protected != nil {
		if branch.Spec.Protected && !*req.Branch.Protected {
			return nil, newError("PROTECTED_BRANCH", "cannot unprotect a protected branch")
		}
		branch.Spec.Protected = *req.Branch.Protected
		changed = true
	}

	if !changed {
		return s.toBranchResponse(branch, projectID), nil
	}

	if err := s.k8sClient.Patch(ctx, branch, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("patch branch: %w", err)
	}
	s.log.Info("patched branch", "branchID", branchID)

	return s.toBranchResponse(branch, projectID), nil
}

// SetBranchAsDefault 设置分支为项目默认分支。
func (s *apiService) SetBranchAsDefault(ctx context.Context, projectID, branchID string) (*BranchResponse, error) {
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	// 幂等：已经是默认分支
	if branch.Spec.Default {
		return s.toBranchResponse(branch, projectID), nil
	}

	// 1. 列出同项目所有分支，设置 default=false
	branchList := &neonv1.BranchList{}
	if err := s.k8sClient.List(ctx, branchList, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	for _, b := range branchList.Items {
		if b.Spec.ProjectID == projectID && b.Spec.Default {
			orig := b.DeepCopy()
			b.Spec.Default = false
			if err := s.k8sClient.Patch(ctx, &b, client.MergeFrom(orig)); err != nil {
				s.log.Warn("failed to unset default on branch", "branchID", b.Name, "error", err)
			}
		}
	}

	// 2. 设置目标分支 default=true
	original := branch.DeepCopy()
	branch.Spec.Default = true
	if err := s.k8sClient.Patch(ctx, branch, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("set branch as default: %w", err)
	}

	s.log.Info("set branch as default", "branchID", branchID)
	return s.toBranchResponse(branch, projectID), nil
}

// toBranchResponse converts a Branch CR to API response.
func (s *apiService) toBranchResponse(branch *neonv1.Branch, projectID string) *BranchResponse {
	return &BranchResponse{
		ID:           branch.Name,
		Name:         branch.Spec.Name,
		ProjectID:    projectID,
		ParentID:     branch.Spec.ParentBranch,
		ParentLSN:    branch.Spec.ParentLSN,
		CurrentState: "ready",
		Default:      branch.Spec.Default,
		Protected:    branch.Spec.Protected,
		InitSource:   branch.Spec.InitSource,
		CreatedAt:    branch.CreationTimestamp.Time,
	}
}

// =============================================================================
// Endpoint 服务
// =============================================================================

func (s *apiService) CreateEndpoint(ctx context.Context, projectID string, req EndpointCreateRequest) (*CreatedEndpointResponse, error) {
	now := time.Now()

	// 验证 Branch 存在且在项目内
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: req.Endpoint.BranchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+req.Endpoint.BranchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+req.Endpoint.BranchID+"' not in project '"+projectID+"'")
	}

	epResp, err := s.createEndpointForBranch(ctx, &neonv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: projectID, Namespace: s.namespace},
		Spec:       neonv1.ProjectSpec{PGVersion: branch.Spec.PGVersion},
	}, req.Endpoint.BranchID, EndpointPayload{
		Type:      req.Endpoint.Type,
		Resources: req.Endpoint.Resources,
		Exposure:  req.Endpoint.Exposure,
	})
	if err != nil {
		return nil, err
	}

	opID := generateOperationID()
	op := &neonv1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opID,
			Namespace: s.namespace,
		},
		Spec: neonv1.OperationSpec{
			Action:     "start_compute",
			Status:     "scheduling",
			ProjectID:  projectID,
			BranchID:   req.Endpoint.BranchID,
			EndpointID: epResp.ID,
		},
	}
	if err := s.k8sClient.Create(ctx, op); err != nil {
		return nil, fmt.Errorf("create operation: %w", err)
	}

	return &CreatedEndpointResponse{
		Endpoint: *epResp,
		Operations: []OperationResponse{{
			ID:         opID,
			ProjectID:  projectID,
			BranchID:   req.Endpoint.BranchID,
			EndpointID: epResp.ID,
			Action:     "start_compute",
			Status:     "scheduling",
			CreatedAt:  now,
		}},
	}, nil
}

func (s *apiService) createEndpointForBranch(ctx context.Context, project *neonv1.Project, branchID string, ep EndpointPayload) (*EndpointResponse, error) {
	now := time.Now()
	endpointID := generateResourceID("ep")
	epType := ep.Type
	if epType == "" {
		epType = "read_write"
	}

	// Per Neon design: at most one read_write endpoint per branch.
	// Reject if the branch already has a read_write endpoint and we're trying to create another.
	if epType == "read_write" {
		var existingEndpoints neonv1.EndpointList
		if err := s.k8sClient.List(ctx, &existingEndpoints, client.InNamespace(s.namespace)); err != nil {
			return nil, fmt.Errorf("list endpoints for uniqueness check: %w", err)
		}
		for _, existing := range existingEndpoints.Items {
			if existing.Spec.BranchID == branchID && existing.Spec.Type == "read_write" {
				return nil, newError("READ_WRITE_ENDPOINT_EXISTS",
					"branch '"+branchID+"' already has a read_write endpoint '"+existing.Name+"'")
			}
		}
	}

	var resources *corev1.ResourceRequirements
	if ep.Resources != nil {
		resources = toResourceRequirements(ep.Resources)
	}

	endpoint := &neonv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      endpointID,
			Namespace: s.namespace,
			Labels: map[string]string{
				"molnett.org/branch": branchID,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(
					&neonv1.Branch{ObjectMeta: metav1.ObjectMeta{Name: branchID, Namespace: s.namespace}},
					neonv1.GroupVersion.WithKind("Branch"),
				),
			},
		},
		Spec: neonv1.EndpointSpec{
			BranchID:  branchID,
			Type:      epType,
			Resources: resources,
			Exposure:  toNeonExposure(&ep),
		},
	}
	if err := s.k8sClient.Create(ctx, endpoint); err != nil {
		return nil, fmt.Errorf("create endpoint CR: %w", err)
	}

	return &EndpointResponse{
		ID:           endpointID,
		BranchID:     branchID,
		Type:         epType,
		CurrentState: "init",
		CreatedAt:    now,
	}, nil
}

// GetEndpoint 获取端点详情
func (s *apiService) GetEndpoint(ctx context.Context, projectID, endpointID string) (*EndpointResponse, error) {
	endpoint := &neonv1.Endpoint{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: endpointID, Namespace: s.namespace}, endpoint); err != nil {
		if isNotFound(err) {
			return nil, newError("ENDPOINT_NOT_FOUND", "endpoint '"+endpointID+"' not found")
		}
		return nil, fmt.Errorf("get endpoint: %w", err)
	}

	return &EndpointResponse{
		ID:           endpoint.Name,
		BranchID:     endpoint.Spec.BranchID,
		Type:         endpoint.Spec.Type,
		Host:         endpoint.Status.Host,
		Port:         endpoint.Status.Port,
		CurrentState: endpoint.Status.Phase,
		Disabled:     endpoint.Spec.Disabled,
		CreatedAt:    endpoint.CreationTimestamp.Time,
	}, nil
}

// ListEndpoints 列出项目的所有端点
func (s *apiService) ListEndpoints(ctx context.Context, projectID string) (*EndpointListResponse, error) {
	// 先找项目的所有分支
	branchIDs := make(map[string]bool)
	branchList := &neonv1.BranchList{}
	if err := s.k8sClient.List(ctx, branchList, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	for _, b := range branchList.Items {
		if b.Spec.ProjectID == projectID {
			branchIDs[b.Name] = true
		}
	}

	list := &neonv1.EndpointList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}

	endpoints := make([]EndpointResponse, 0)
	for _, ep := range list.Items {
		if !branchIDs[ep.Spec.BranchID] {
			continue
		}
		endpoints = append(endpoints, EndpointResponse{
			ID:           ep.Name,
			BranchID:     ep.Spec.BranchID,
			Type:         ep.Spec.Type,
			Host:         ep.Status.Host,
			Port:         ep.Status.Port,
			CurrentState: ep.Status.Phase,
			Disabled:     ep.Spec.Disabled,
			CreatedAt:    ep.CreationTimestamp.Time,
		})
	}

	return &EndpointListResponse{Endpoints: endpoints}, nil
}

// ListEndpointsForBranch 列出指定分支的所有端点
func (s *apiService) ListEndpointsForBranch(ctx context.Context, projectID, branchID string) (*EndpointListResponse, error) {
	// 校验 branch 属于 project
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	list := &neonv1.EndpointList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}

	endpoints := make([]EndpointResponse, 0)
	for _, ep := range list.Items {
		if ep.Spec.BranchID != branchID {
			continue
		}
		endpoints = append(endpoints, EndpointResponse{
			ID:           ep.Name,
			BranchID:     ep.Spec.BranchID,
			Type:         ep.Spec.Type,
			Host:         ep.Status.Host,
			Port:         ep.Status.Port,
			CurrentState: ep.Status.Phase,
			Disabled:     ep.Spec.Disabled,
			CreatedAt:    ep.CreationTimestamp.Time,
		})
	}

	return &EndpointListResponse{Endpoints: endpoints}, nil
}

// DeleteEndpoint 删除端点
func (s *apiService) DeleteEndpoint(ctx context.Context, projectID, endpointID string) (*DeleteResponse, error) {
	endpoint := &neonv1.Endpoint{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: endpointID, Namespace: s.namespace}, endpoint); err != nil {
		if isNotFound(err) {
			return nil, newError("ENDPOINT_NOT_FOUND", "endpoint '"+endpointID+"' not found")
		}
		return nil, fmt.Errorf("get endpoint: %w", err)
	}

	opID := generateOperationID()
	op := &neonv1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opID,
			Namespace: s.namespace,
		},
		Spec: neonv1.OperationSpec{
			Action:     "delete_endpoint",
			Status:     "scheduling",
			ProjectID:  projectID,
			EndpointID: endpointID,
		},
	}
	if err := s.k8sClient.Create(ctx, op); err != nil {
		return nil, fmt.Errorf("create operation: %w", err)
	}

	if err := s.k8sClient.Delete(ctx, endpoint); err != nil {
		return nil, fmt.Errorf("delete endpoint: %w", err)
	}

	return &DeleteResponse{
		Endpoint: &EndpointResponse{ID: endpointID, Type: endpoint.Spec.Type},
		Operations: []OperationResponse{{
			ID:         opID,
			ProjectID:  projectID,
			EndpointID: endpointID,
			Action:     "delete_endpoint",
			Status:     "scheduling",
			CreatedAt:  now(),
		}},
	}, nil
}

// UpdateEndpoint 部分更新端点（对标 Neon PATCH）。
func (s *apiService) UpdateEndpoint(ctx context.Context, projectID, endpointID string, req EndpointUpdateRequest) (*EndpointGetResponse, error) {
	endpoint := &neonv1.Endpoint{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: endpointID, Namespace: s.namespace}, endpoint); err != nil {
		if isNotFound(err) {
			return nil, newError("ENDPOINT_NOT_FOUND", "endpoint '"+endpointID+"' not found")
		}
		return nil, fmt.Errorf("get endpoint: %w", err)
	}

	original := endpoint.DeepCopy()
	changed := false

	// --- BranchID 变更（迁移端点）---
	if req.Endpoint.BranchID != nil {
		newBranchID := *req.Endpoint.BranchID
		// 验证目标 Branch 存在
		targetBranch := &neonv1.Branch{}
		if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: newBranchID, Namespace: s.namespace}, targetBranch); err != nil {
			if isNotFound(err) {
				return nil, newError("BRANCH_NOT_FOUND", "target branch '"+newBranchID+"' not found")
			}
			return nil, fmt.Errorf("get target branch: %w", err)
		}
		// 如果是 read_write 端点，检查目标分支没有其他 read_write
		if endpoint.Spec.Type == "read_write" {
			var epList neonv1.EndpointList
			if err := s.k8sClient.List(ctx, &epList, client.InNamespace(s.namespace)); err != nil {
				return nil, fmt.Errorf("list endpoints: %w", err)
			}
			for _, ep := range epList.Items {
				if ep.Spec.BranchID == newBranchID && ep.Spec.Type == "read_write" && ep.Name != endpointID {
					return nil, newError("READ_WRITE_ENDPOINT_EXISTS",
						"branch '"+newBranchID+"' already has a read_write endpoint '"+ep.Name+"'")
				}
			}
		}
		endpoint.Spec.BranchID = newBranchID
		if endpoint.Labels == nil {
			endpoint.Labels = make(map[string]string)
		}
		endpoint.Labels["molnett.org/branch"] = newBranchID
		changed = true
	}

	// --- Resources 变更 ---
	if req.Endpoint.Resources != nil {
		endpoint.Spec.Resources = toResourceRequirements(req.Endpoint.Resources)
		changed = true
	}

	// --- Disabled 变更 ---
	if req.Endpoint.Disabled != nil {
		endpoint.Spec.Disabled = *req.Endpoint.Disabled
		changed = true
	}

	// --- SuspendTimeoutSeconds 变更（预留）---
	if req.Endpoint.SuspendTimeoutSeconds != nil {
		// Serverless 预留字段，暂存但不处理逻辑
		_ = *req.Endpoint.SuspendTimeoutSeconds
		changed = true
	}

	if !changed {
		return &EndpointGetResponse{
			Endpoint: EndpointResponse{
				ID:           endpoint.Name,
				BranchID:     endpoint.Spec.BranchID,
				Type:         endpoint.Spec.Type,
				Host:         endpoint.Status.Host,
				Port:         endpoint.Status.Port,
				CurrentState: endpoint.Status.Phase,
				Disabled:     endpoint.Spec.Disabled,
				CreatedAt:    endpoint.CreationTimestamp.Time,
			},
		}, nil
	}

	if err := s.k8sClient.Patch(ctx, endpoint, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("patch endpoint: %w", err)
	}

	s.log.Info("patched endpoint", "endpointID", endpointID)

	return &EndpointGetResponse{
		Endpoint: EndpointResponse{
			ID:           endpoint.Name,
			BranchID:     endpoint.Spec.BranchID,
			Type:         endpoint.Spec.Type,
			Host:         endpoint.Status.Host,
			Port:         endpoint.Status.Port,
			CurrentState: endpoint.Status.Phase,
			Disabled:     endpoint.Spec.Disabled,
			CreatedAt:    endpoint.CreationTimestamp.Time,
		},
	}, nil
}

// StartEndpoint 启动端点（disabled=false）。
func (s *apiService) StartEndpoint(ctx context.Context, projectID, endpointID string) (*EndpointActionResponse, error) {
	endpoint := &neonv1.Endpoint{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: endpointID, Namespace: s.namespace}, endpoint); err != nil {
		if isNotFound(err) {
			return nil, newError("ENDPOINT_NOT_FOUND", "endpoint '"+endpointID+"' not found")
		}
		return nil, fmt.Errorf("get endpoint: %w", err)
	}

	// 幂等：已经在运行
	if !endpoint.Spec.Disabled {
		return &EndpointActionResponse{
			Endpoint: EndpointResponse{
				ID:           endpoint.Name,
				BranchID:     endpoint.Spec.BranchID,
				Type:         endpoint.Spec.Type,
				Host:         endpoint.Status.Host,
				Port:         endpoint.Status.Port,
				CurrentState: endpoint.Status.Phase,
				Disabled:     false,
				CreatedAt:    endpoint.CreationTimestamp.Time,
			},
		}, nil
	}

	original := endpoint.DeepCopy()
	endpoint.Spec.Disabled = false
	if err := s.k8sClient.Patch(ctx, endpoint, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("start endpoint: %w", err)
	}

	s.log.Info("started endpoint", "endpointID", endpointID)

	return &EndpointActionResponse{
		Endpoint: EndpointResponse{
			ID:           endpoint.Name,
			BranchID:     endpoint.Spec.BranchID,
			Type:         endpoint.Spec.Type,
			Host:         endpoint.Status.Host,
			Port:         endpoint.Status.Port,
			CurrentState: "starting",
			Disabled:     false,
			CreatedAt:    endpoint.CreationTimestamp.Time,
		},
	}, nil
}

// SuspendEndpoint 挂起端点（disabled=true）。
func (s *apiService) SuspendEndpoint(ctx context.Context, projectID, endpointID string) (*EndpointActionResponse, error) {
	endpoint := &neonv1.Endpoint{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: endpointID, Namespace: s.namespace}, endpoint); err != nil {
		if isNotFound(err) {
			return nil, newError("ENDPOINT_NOT_FOUND", "endpoint '"+endpointID+"' not found")
		}
		return nil, fmt.Errorf("get endpoint: %w", err)
	}

	// 幂等：已经挂起
	if endpoint.Spec.Disabled {
		return &EndpointActionResponse{
			Endpoint: EndpointResponse{
				ID:           endpoint.Name,
				BranchID:     endpoint.Spec.BranchID,
				Type:         endpoint.Spec.Type,
				Host:         endpoint.Status.Host,
				Port:         endpoint.Status.Port,
				CurrentState: endpoint.Status.Phase,
				Disabled:     true,
				CreatedAt:    endpoint.CreationTimestamp.Time,
			},
		}, nil
	}

	original := endpoint.DeepCopy()
	endpoint.Spec.Disabled = true
	if err := s.k8sClient.Patch(ctx, endpoint, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("suspend endpoint: %w", err)
	}

	s.log.Info("suspended endpoint", "endpointID", endpointID)

	return &EndpointActionResponse{
		Endpoint: EndpointResponse{
			ID:           endpoint.Name,
			BranchID:     endpoint.Spec.BranchID,
			Type:         endpoint.Spec.Type,
			Host:         endpoint.Status.Host,
			Port:         endpoint.Status.Port,
			CurrentState: "stopping",
			Disabled:     true,
			CreatedAt:    endpoint.CreationTimestamp.Time,
		},
	}, nil
}

// RestartEndpoint 重启端点：suspend → start。
func (s *apiService) RestartEndpoint(ctx context.Context, projectID, endpointID string) (*EndpointActionResponse, error) {
	endpoint := &neonv1.Endpoint{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: endpointID, Namespace: s.namespace}, endpoint); err != nil {
		if isNotFound(err) {
			return nil, newError("ENDPOINT_NOT_FOUND", "endpoint '"+endpointID+"' not found")
		}
		return nil, fmt.Errorf("get endpoint: %w", err)
	}

	// 创建 Operation CR 追踪重启进度
	opID := generateOperationID()
	op := &neonv1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opID,
			Namespace: s.namespace,
		},
		Spec: neonv1.OperationSpec{
			Action:     "restart_endpoint",
			Status:     "scheduling",
			ProjectID:  projectID,
			EndpointID: endpointID,
		},
	}
	if err := s.k8sClient.Create(ctx, op); err != nil {
		return nil, fmt.Errorf("create operation: %w", err)
	}

	// 执行 suspend → start 两步操作
	original := endpoint.DeepCopy()

	// Step 1: suspend
	endpoint.Spec.Disabled = true
	if err := s.k8sClient.Patch(ctx, endpoint, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("suspend for restart: %w", err)
	}

	// Step 2: start（K8s Patch 后立即 start，Controller 负责顺序执行）
	// 重新获取最新版本
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: endpointID, Namespace: s.namespace}, endpoint); err != nil {
		return nil, fmt.Errorf("re-get endpoint for restart: %w", err)
	}
	startOriginal := endpoint.DeepCopy()
	endpoint.Spec.Disabled = false
	if err := s.k8sClient.Patch(ctx, endpoint, client.MergeFrom(startOriginal)); err != nil {
		return nil, fmt.Errorf("start after restart: %w", err)
	}

	s.log.Info("restarted endpoint", "endpointID", endpointID)

	return &EndpointActionResponse{
		Endpoint: EndpointResponse{
			ID:           endpoint.Name,
			BranchID:     endpoint.Spec.BranchID,
			Type:         endpoint.Spec.Type,
			Host:         endpoint.Status.Host,
			Port:         endpoint.Status.Port,
			CurrentState: "starting",
			Disabled:     false,
			CreatedAt:    endpoint.CreationTimestamp.Time,
		},
		Operations: []OperationResponse{{
			ID:         opID,
			ProjectID:  projectID,
			EndpointID: endpointID,
			Action:     "restart_endpoint",
			Status:     "scheduling",
			CreatedAt:  op.CreationTimestamp.Time,
		}},
	}, nil
}

// =============================================================================
// Role 服务
// =============================================================================

// CreateRole 创建角色
func (s *apiService) CreateRole(ctx context.Context, projectID, branchID string, req RoleCreateRequest) (*RoleResponse, error) {
	now := time.Now()

	// 验证 Branch
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	roleName := req.Role.Name
	if roleName == "" {
		return nil, newError("INVALID_NAME", "role name is required")
	}
	authMethod := req.Role.AuthenticationMethod
	if authMethod == "" {
		authMethod = "password"
	}

	password := generatePassword()

	role := &neonv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", branchID, roleName),
			Namespace: s.namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(branch, neonv1.GroupVersion.WithKind("Branch")),
			},
		},
		Spec: neonv1.RoleSpec{
			BranchID:             branchID,
			Name:                 roleName,
			AuthenticationMethod: authMethod,
		},
	}
	if err := s.k8sClient.Create(ctx, role); err != nil {
		return nil, fmt.Errorf("create role CR: %w", err)
	}

	secretName := fmt.Sprintf("role-%s-%s-password", branchID, roleName)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.namespace,
			Labels: map[string]string{
				"molnett.org/component": "role-password",
				"molnett.org/role":      role.Name,
				"molnett.org/branch":    branch.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(role, neonv1.GroupVersion.WithKind("Role")),
			},
		},
		StringData: map[string]string{
			"password": password,
			"username": roleName,
		},
	}
	if err := s.k8sClient.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("create password secret: %w", err)
	}

	return &RoleResponse{
		Name:                 roleName,
		Password:             password,
		Protected:            false,
		BranchID:             branchID,
		AuthenticationMethod: authMethod,
		CreatedAt:            now,
	}, nil
}

// ListRoles 列出分支的所有角色
func (s *apiService) ListRoles(ctx context.Context, projectID, branchID string) (*RoleListResponse, error) {
	list := &neonv1.RoleList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}

	roles := make([]RoleResponse, 0)
	for _, r := range list.Items {
		if r.Spec.BranchID != branchID {
			continue
		}
		roles = append(roles, RoleResponse{
			Name:                 r.Spec.Name,
			Protected:            r.Status.Protected,
			BranchID:             branchID,
			AuthenticationMethod: r.Spec.AuthenticationMethod,
			CreatedAt:            r.CreationTimestamp.Time,
		})
	}

	return &RoleListResponse{Roles: roles}, nil
}

// DeleteRole 删除角色
func (s *apiService) DeleteRole(ctx context.Context, projectID, branchID, roleName string) error {
	// 搜索角色
	list := &neonv1.RoleList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return fmt.Errorf("list roles: %w", err)
	}

	for _, r := range list.Items {
		if r.Spec.BranchID == branchID && r.Spec.Name == roleName {
			if r.Status.Protected {
				return newError("PROTECTED_ROLE", "protected role '"+roleName+"' cannot be deleted")
			}
			if err := s.k8sClient.Delete(ctx, &r); err != nil {
				return fmt.Errorf("delete role: %w", err)
			}
			return nil
		}
	}
	return newError("ROLE_NOT_FOUND", "role '"+roleName+"' not found")
}

// ResetPassword 重置角色密码
func (s *apiService) ResetPassword(ctx context.Context, projectID, branchID, roleName string) (*ResetPasswordResponse, error) {
	list := &neonv1.RoleList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}

	for i := range list.Items {
		r := &list.Items[i]
		if r.Spec.BranchID == branchID && r.Spec.Name == roleName {
			newPassword := generatePassword()

			secretName := fmt.Sprintf("role-%s-%s-password", branchID, roleName)
			secret := &corev1.Secret{}
			if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: secretName, Namespace: s.namespace}, secret); err == nil {
				secret.StringData = map[string]string{"password": newPassword}
				if err := s.k8sClient.Update(ctx, secret); err != nil {
					return nil, fmt.Errorf("update password secret: %w", err)
				}
			} else {
				newSecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      secretName,
						Namespace: s.namespace,
					},
					StringData: map[string]string{"password": newPassword},
				}
				if err := s.k8sClient.Create(ctx, newSecret); err != nil {
					return nil, fmt.Errorf("create password secret: %w", err)
				}
			}

			// Clear EncryptedPassword to force Controller to recompute SCRAM verifier
			roleCopy := r.DeepCopy()
			if roleCopy.Status.EncryptedPassword != "" {
				roleCopy.Status.EncryptedPassword = ""
				if err := s.k8sClient.Status().Update(ctx, roleCopy); err != nil {
					s.log.Warn("failed to clear EncryptedPassword after password reset", "error", err)
				}
			}

			return &ResetPasswordResponse{
				Role: RoleResponse{
					Name:      roleName,
					Password:  newPassword,
					BranchID:  branchID,
					CreatedAt: now(),
				},
			}, nil
		}
	}

	return nil, newError("ROLE_NOT_FOUND", "role '"+roleName+"' not found")
}

// GetRole 获取单个角色详情。
func (s *apiService) GetRole(ctx context.Context, projectID, branchID, roleName string) (*RoleGetResponse, error) {
	// 验证 Branch 存在
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	list := &neonv1.RoleList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}

	for _, r := range list.Items {
		if r.Spec.BranchID == branchID && r.Spec.Name == roleName {
			return &RoleGetResponse{
				Role: RoleResponse{
					Name:                 r.Spec.Name,
					Protected:            r.Status.Protected,
					BranchID:             branchID,
					AuthenticationMethod: r.Spec.AuthenticationMethod,
					CreatedAt:            r.CreationTimestamp.Time,
				},
			}, nil
		}
	}

	return nil, newError("ROLE_NOT_FOUND", "role '"+roleName+"' not found")
}

// UpdateRole 更新角色（对标 Neon PATCH，支持重命名和密码重置）。
// 策略 B：不改名 CR metadata.name，仅更新 spec.Name + Secret username。
func (s *apiService) UpdateRole(ctx context.Context, projectID, branchID, roleName string, req RoleUpdateRequest) (*RoleUpdateResponse, error) {
	// 验证 Branch 存在
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	// 查找 Role CR
	list := &neonv1.RoleList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	var role *neonv1.Role
	for i := range list.Items {
		if list.Items[i].Spec.BranchID == branchID && list.Items[i].Spec.Name == roleName {
			role = &list.Items[i]
			break
		}
	}
	if role == nil {
		return nil, newError("ROLE_NOT_FOUND", "role '"+roleName+"' not found")
	}
	if role.Status.Protected {
		return nil, newError("ROLE_PROTECTED", "protected role '"+roleName+"' cannot be modified")
	}

	var ops []OperationResponse
	original := role.DeepCopy()
	changed := false

	// --- 密码变更 ---
	if req.Role.Password != nil {
		newPassword := *req.Role.Password
		if len(newPassword) < 8 {
			return nil, newError("VALIDATION_ERROR", "password must be at least 8 characters")
		}

		// 原地更新 Secret
		secretName := fmt.Sprintf("role-%s-%s-password", branchID, roleName)
		secret := &corev1.Secret{}
		if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: secretName, Namespace: s.namespace}, secret); err != nil {
			if isNotFound(err) {
				return nil, newError("ROLE_NOT_FOUND", "password secret not found for role '"+roleName+"'")
			}
			return nil, fmt.Errorf("get password secret: %w", err)
		}
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data["password"] = []byte(newPassword)
		if err := s.k8sClient.Update(ctx, secret); err != nil {
			return nil, fmt.Errorf("update password secret: %w", err)
		}

		// 清除 EncryptedPassword，触发 Controller 重算 SCRAM
		role.Status.EncryptedPassword = ""
		changed = true

		// 创建 Operation CR 追踪密码推送到 compute_ctl
		opID := generateOperationID()
		op := &neonv1.Operation{
			ObjectMeta: metav1.ObjectMeta{
				Name:      opID,
				Namespace: s.namespace,
			},
			Spec: neonv1.OperationSpec{
				Action:    "reset_password",
				Status:    "scheduling",
				ProjectID: projectID,
				BranchID:  branchID,
			},
		}
		if err := s.k8sClient.Create(ctx, op); err != nil {
			s.log.Warn("failed to create operation for password change", "error", err)
		} else {
			ops = append(ops, OperationResponse{
				ID:        opID,
				ProjectID: projectID,
				BranchID:  branchID,
				Action:    "reset_password",
				Status:    "scheduling",
				CreatedAt: op.CreationTimestamp.Time,
			})
		}
	}

	// --- 名称变更（策略 B：不改名 CR metadata.name）---
	if req.Role.Name != nil {
		newName := *req.Role.Name
		if len(newName) < 1 || len(newName) > 63 {
			return nil, newError("INVALID_NAME", "role name must be 1-63 characters")
		}

		// 检查新名称不冲突
		for _, r := range list.Items {
			if r.Spec.BranchID == branchID && r.Spec.Name == newName && r.Name != role.Name {
				return nil, newError("VALIDATION_ERROR", "role '"+newName+"' already exists in this branch")
			}
		}

		// 更新 Spec.Name（CR metadata.name 不变）
		role.Spec.Name = newName
		changed = true

		// 更新 Secret 中的 username 字段
		secretName := fmt.Sprintf("role-%s-%s-password", branchID, roleName)
		secret := &corev1.Secret{}
		if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: secretName, Namespace: s.namespace}, secret); err == nil {
			if secret.Data == nil {
				secret.Data = make(map[string][]byte)
			}
			secret.Data["username"] = []byte(newName)
			if err := s.k8sClient.Update(ctx, secret); err != nil {
				s.log.Warn("failed to update username in secret", "error", err)
			}
		}
	}

	if !changed {
		return &RoleUpdateResponse{
			Role: RoleResponse{
				Name:                 roleName,
				Protected:            role.Status.Protected,
				BranchID:             branchID,
				AuthenticationMethod: role.Spec.AuthenticationMethod,
				CreatedAt:            role.CreationTimestamp.Time,
			},
		}, nil
	}

	// Patch Role CR
	if err := s.k8sClient.Patch(ctx, role, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("patch role: %w", err)
	}

	// 更新 Status（EncryptedPassword 已清除）
	if err := s.k8sClient.Status().Update(ctx, role); err != nil {
		s.log.Warn("failed to update role status after patch", "error", err)
	}

	s.log.Info("patched role", "roleName", roleName, "branchID", branchID)

	return &RoleUpdateResponse{
		Role: RoleResponse{
			Name:                 role.Spec.Name,
			Protected:            role.Status.Protected,
			BranchID:             branchID,
			AuthenticationMethod: role.Spec.AuthenticationMethod,
			CreatedAt:            role.CreationTimestamp.Time,
		},
		Operations: ops,
	}, nil
}

// =============================================================================
// Database 服务
// =============================================================================

// CreateDatabase 创建数据库
func (s *apiService) CreateDatabase(ctx context.Context, projectID, branchID string, req DatabaseCreateRequest) (*DatabaseResponse, error) {
	now := time.Now()

	// 验证 Branch
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	dbName := req.Database.Name
	if dbName == "" {
		return nil, newError("INVALID_NAME", "database name is required")
	}
	ownerName := req.Database.OwnerName

	database := &neonv1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", branchID, dbName),
			Namespace: s.namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(branch, neonv1.GroupVersion.WithKind("Branch")),
			},
		},
		Spec: neonv1.DatabaseSpec{
			BranchID:  branchID,
			Name:      dbName,
			OwnerName: ownerName,
		},
	}
	if err := s.k8sClient.Create(ctx, database); err != nil {
		return nil, fmt.Errorf("create database CR: %w", err)
	}

	return &DatabaseResponse{
		ID:        1,
		Name:      dbName,
		OwnerName: ownerName,
		BranchID:  branchID,
		CreatedAt: now,
	}, nil
}

// ListDatabases 列出所有数据库
func (s *apiService) ListDatabases(ctx context.Context, projectID, branchID string) (*DatabaseListResponse, error) {
	list := &neonv1.DatabaseList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}

	dbSeq := 0
	databases := make([]DatabaseResponse, 0)
	for _, d := range list.Items {
		if d.Spec.BranchID != branchID {
			continue
		}
		dbSeq++
		databases = append(databases, DatabaseResponse{
			ID:        dbSeq,
			Name:      d.Spec.Name,
			OwnerName: d.Spec.OwnerName,
			BranchID:  branchID,
			CreatedAt: d.CreationTimestamp.Time,
		})
	}

	return &DatabaseListResponse{Databases: databases}, nil
}

// DeleteDatabase 删除数据库
func (s *apiService) DeleteDatabase(ctx context.Context, projectID, branchID, dbName string) error {
	list := &neonv1.DatabaseList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return fmt.Errorf("list databases: %w", err)
	}

	for _, d := range list.Items {
		if d.Spec.BranchID == branchID && d.Spec.Name == dbName {
			if err := s.k8sClient.Delete(ctx, &d); err != nil {
				return fmt.Errorf("delete database: %w", err)
			}
			return nil
		}
	}
	return newError("DATABASE_NOT_FOUND", "database '"+dbName+"' not found")
}

// GetDatabase 获取单个数据库详情。
func (s *apiService) GetDatabase(ctx context.Context, projectID, branchID, dbName string) (*DatabaseGetResponse, error) {
	// 验证 Branch 存在
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	list := &neonv1.DatabaseList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}

	for _, d := range list.Items {
		if d.Spec.BranchID == branchID && d.Spec.Name == dbName {
			return &DatabaseGetResponse{
				Database: DatabaseResponse{
					ID:        1,
					Name:      d.Spec.Name,
					OwnerName: d.Spec.OwnerName,
					BranchID:  branchID,
					CreatedAt: d.CreationTimestamp.Time,
				},
			}, nil
		}
	}

	return nil, newError("DATABASE_NOT_FOUND", "database '"+dbName+"' not found")
}

// UpdateDatabase 更新数据库（对标 Neon PATCH，支持重命名和 owner 变更）。
func (s *apiService) UpdateDatabase(ctx context.Context, projectID, branchID, dbName string, req DatabaseUpdateRequest) (*DatabaseGetResponse, error) {
	// 验证 Branch 存在
	branch := &neonv1.Branch{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: branchID, Namespace: s.namespace}, branch); err != nil {
		if isNotFound(err) {
			return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not found")
		}
		return nil, fmt.Errorf("get branch: %w", err)
	}
	if branch.Spec.ProjectID != projectID {
		return nil, newError("BRANCH_NOT_FOUND", "branch '"+branchID+"' not in project '"+projectID+"'")
	}

	// 查找 Database CR
	list := &neonv1.DatabaseList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	var database *neonv1.Database
	for i := range list.Items {
		if list.Items[i].Spec.BranchID == branchID && list.Items[i].Spec.Name == dbName {
			database = &list.Items[i]
			break
		}
	}
	if database == nil {
		return nil, newError("DATABASE_NOT_FOUND", "database '"+dbName+"' not found")
	}

	original := database.DeepCopy()
	changed := false

	// --- 名称变更 ---
	if req.Database.Name != nil {
		newName := *req.Database.Name
		if len(newName) < 1 || len(newName) > 63 {
			return nil, newError("INVALID_NAME", "database name must be 1-63 characters")
		}
		// 检查目标名称不冲突
		for _, d := range list.Items {
			if d.Spec.BranchID == branchID && d.Spec.Name == newName && d.Name != database.Name {
				return nil, newError("DATABASE_NAME_EXISTS", "database '"+newName+"' already exists in this branch")
			}
		}
		database.Spec.Name = newName
		changed = true
	}

	// --- Owner 变更 ---
	if req.Database.OwnerName != nil {
		newOwner := *req.Database.OwnerName
		// 验证 Role 存在于同一 Branch
		roleList := &neonv1.RoleList{}
		if err := s.k8sClient.List(ctx, roleList, client.InNamespace(s.namespace)); err != nil {
			return nil, fmt.Errorf("list roles: %w", err)
		}
		roleFound := false
		for _, r := range roleList.Items {
			if r.Spec.BranchID == branchID && r.Spec.Name == newOwner {
				roleFound = true
				break
			}
		}
		if !roleFound {
			return nil, newError("ROLE_NOT_FOUND", "owner role '"+newOwner+"' not found in branch '"+branchID+"'")
		}
		database.Spec.OwnerName = newOwner
		changed = true
	}

	if !changed {
		return &DatabaseGetResponse{
			Database: DatabaseResponse{
				Name:      dbName,
				OwnerName: database.Spec.OwnerName,
				BranchID:  branchID,
				CreatedAt: database.CreationTimestamp.Time,
			},
		}, nil
	}

	if err := s.k8sClient.Patch(ctx, database, client.MergeFrom(original)); err != nil {
		return nil, fmt.Errorf("patch database: %w", err)
	}

	s.log.Info("patched database", "dbName", dbName, "branchID", branchID)

	return &DatabaseGetResponse{
		Database: DatabaseResponse{
			Name:      database.Spec.Name,
			OwnerName: database.Spec.OwnerName,
			BranchID:  branchID,
			CreatedAt: database.CreationTimestamp.Time,
		},
	}, nil
}

// GetConnectionURI 构造数据库连接字符串（对标 Neon GET /api/v2/projects/{project_id}/connection_uri）。
func (s *apiService) GetConnectionURI(ctx context.Context, projectID string, databaseName, roleName string, branchID, endpointID string, pooled bool) (*ConnectionURIResponse, error) {
	// 1. 确定分支：若未传则选项目默认分支
	if branchID == "" {
		defaultBranch, err := s.findDefaultBranch(ctx, projectID)
		if err != nil {
			return nil, err
		}
		branchID = defaultBranch.Name
	}

	// 2. 确定端点：若未传则选分支的 read_write 端点
	if endpointID == "" {
		epList := &neonv1.EndpointList{}
		if err := s.k8sClient.List(ctx, epList, client.InNamespace(s.namespace)); err != nil {
			return nil, fmt.Errorf("list endpoints: %w", err)
		}
		for _, ep := range epList.Items {
			if ep.Spec.BranchID == branchID && ep.Spec.Type == "read_write" {
				endpointID = ep.Name
				break
			}
		}
		if endpointID == "" {
			return nil, newError("ENDPOINT_NOT_FOUND", "no read_write endpoint found for branch '"+branchID+"'")
		}
	}

	// 3. 获取 Endpoint 信息（Host/Port）
	endpoint := &neonv1.Endpoint{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: endpointID, Namespace: s.namespace}, endpoint); err != nil {
		if isNotFound(err) {
			return nil, newError("ENDPOINT_NOT_FOUND", "endpoint '"+endpointID+"' not found")
		}
		return nil, fmt.Errorf("get endpoint: %w", err)
	}

	host := endpoint.Status.Host
	port := endpoint.Status.Port
	if host == "" {
		host = endpoint.Name + "." + s.namespace + ".svc.cluster.local"
	}
	if port == 0 {
		port = 5432
	}

	// 4. 获取密码
	password := ""
	roleList := &neonv1.RoleList{}
	if err := s.k8sClient.List(ctx, roleList, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	var targetRole *neonv1.Role
	for i := range roleList.Items {
		if roleList.Items[i].Spec.BranchID == branchID && roleList.Items[i].Spec.Name == roleName {
			targetRole = &roleList.Items[i]
			break
		}
	}
	if targetRole == nil {
		return nil, newError("ROLE_NOT_FOUND", "role '"+roleName+"' not found")
	}

	if targetRole.Status.PasswordSecretRef != nil {
		secret := &corev1.Secret{}
		secretKey := client.ObjectKey{
			Name:      targetRole.Status.PasswordSecretRef.Name,
			Namespace: targetRole.Status.PasswordSecretRef.Namespace,
		}
		if secretKey.Namespace == "" {
			secretKey.Namespace = s.namespace
		}
		if err := s.k8sClient.Get(ctx, secretKey, secret); err == nil {
			if pw, ok := secret.Data["password"]; ok {
				password = string(pw)
			}
		}
	}

	// 5. 验证数据库存在
	dbFound := false
	dbList := &neonv1.DatabaseList{}
	if err := s.k8sClient.List(ctx, dbList, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	for _, d := range dbList.Items {
		if d.Spec.BranchID == branchID && d.Spec.Name == databaseName {
			dbFound = true
			break
		}
	}
	if !dbFound {
		return nil, newError("DATABASE_NOT_FOUND", "database '"+databaseName+"' not found")
	}

	// 6. 构造 URI
	uri := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s",
		roleName, password, host, port, databaseName)

	// 如果 pooled 为 true，后续可对接 PgBouncer 地址替换

	return &ConnectionURIResponse{
		URI:          uri,
		Pooled:       pooled,
		DatabaseName: databaseName,
		RoleName:     roleName,
		BranchID:     branchID,
		EndpointID:   endpointID,
	}, nil
}

// =============================================================================
// Operation 服务
// =============================================================================

// ListOperations 列出项目的所有操作
func (s *apiService) ListOperations(ctx context.Context, projectID string) (*OperationListResponse, error) {
	list := &neonv1.OperationList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}

	operations := make([]OperationResponse, 0)
	for _, op := range list.Items {
		if op.Spec.ProjectID != projectID {
			continue
		}
		operations = append(operations, opToResponse(&op))
	}

	return &OperationListResponse{Operations: operations}, nil
}

// GetOperation 获取单个操作详情
func (s *apiService) GetOperation(ctx context.Context, projectID, operationID string) (*OperationResponse, error) {
	op := &neonv1.Operation{}
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: operationID, Namespace: s.namespace}, op); err != nil {
		if isNotFound(err) {
			return nil, newError("OPERATION_NOT_FOUND", "operation '"+operationID+"' not found")
		}
		return nil, fmt.Errorf("get operation: %w", err)
	}
	if op.Spec.ProjectID != projectID {
		return nil, newError("OPERATION_NOT_FOUND", "operation '"+operationID+"' not found in project '"+projectID+"'")
	}

	resp := opToResponse(op)
	return &resp, nil
}

// =============================================================================
// 辅助函数
// =============================================================================

func (s *apiService) findDefaultBranch(ctx context.Context, projectID string) (*neonv1.Branch, error) {
	list := &neonv1.BranchList{}
	if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	for _, b := range list.Items {
		if b.Spec.ProjectID == projectID && b.Spec.Default {
			return &b, nil
		}
	}
	return nil, newError("DEFAULT_BRANCH_NOT_FOUND", "no default branch found for project '"+projectID+"'")
}

func opToResponse(op *neonv1.Operation) OperationResponse {
	var updatedAt *time.Time
	if op.Status.CompletedAt != nil {
		updatedAt = &op.Status.CompletedAt.Time
	}
	return OperationResponse{
		ID:         op.Name,
		ProjectID:  op.Spec.ProjectID,
		BranchID:   op.Spec.BranchID,
		EndpointID: op.Spec.EndpointID,
		Action:     op.Spec.Action,
		Status:     op.Spec.Status,
		Error:      op.Spec.Error,
		CreatedAt:  op.CreationTimestamp.Time,
		UpdatedAt:  updatedAt,
	}
}

func toResourceRequirements(r *ComputeResources) *corev1.ResourceRequirements {
	if r == nil {
		return nil
	}
	rr := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	if r.CPU != "" {
		cpuQty := resource.MustParse(r.CPU)
		rr.Requests[corev1.ResourceCPU] = cpuQty
		rr.Limits[corev1.ResourceCPU] = cpuQty
	}
	if r.Memory != "" {
		memQty := resource.MustParse(r.Memory)
		rr.Requests[corev1.ResourceMemory] = memQty
		rr.Limits[corev1.ResourceMemory] = memQty
	}
	if len(rr.Requests) == 0 {
		return nil
	}
	return rr
}

func toComputeResources(rr *corev1.ResourceRequirements) *ComputeResources {
	if rr == nil {
		return nil
	}
	cr := &ComputeResources{}
	if cpu, ok := rr.Requests[corev1.ResourceCPU]; ok {
		cr.CPU = cpu.String()
	}
	if mem, ok := rr.Requests[corev1.ResourceMemory]; ok {
		cr.Memory = mem.String()
	}
	return cr
}

func toNeonExposure(ep *EndpointPayload) *neonv1.ServiceExposure {
	if ep == nil || ep.Exposure == nil {
		return nil
	}
	exposure := &neonv1.ServiceExposure{
		Type: corev1.ServiceType(ep.Exposure.Type),
	}
	if len(ep.Exposure.SourceRanges) > 0 {
		exposure.LoadBalancerSourceRanges = ep.Exposure.SourceRanges
	}
	return exposure
}

func toDefaultSettings(ds *neonv1.EndpointDefaults) *DefaultEndpointSettings {
	if ds == nil {
		return nil
	}
	return &DefaultEndpointSettings{
		Resources: toComputeResources(ds.Resources),
	}
}

// newError 创建统一错误
func newError(code, message string) *apiError {
	return &apiError{Code: code, Message: message}
}

type apiError struct {
	Code    string
	Message string
}

func (e *apiError) Error() string {
	return e.Code + ": " + e.Message
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

func now() time.Time {
	return time.Now().UTC()
}
