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
			ProjectID:  ids.ProjectID,
			TimelineID: ids.TimelineID,
			PGVersion:  pgVersion,
			InitSource: "parent-data",
			Default:    true,
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

	return &ProjectResponse{
		ID:              project.Name,
		Name:            project.Spec.Name,
		PGVersion:       project.Spec.PGVersion,
		Cluster:         project.Spec.ClusterName,
		TenantID:        project.Spec.TenantID,
		CreatedAt:       project.CreationTimestamp.Time,
		DefaultSettings: toDefaultSettings(project.Spec.DefaultEndpointSettings),
	}, nil
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
		Name:         branch.Spec.ParentBranch, // 使用 parent 作为 name（metadata.name 是 ID）
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
			Name:         b.Spec.ParentBranch,
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
		},
		StringData: map[string]string{"password": password},
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

	for _, r := range list.Items {
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
