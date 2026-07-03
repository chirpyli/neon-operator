# API 实现清单

> 对当前 neon-operator 项目已实现的所有 API（CRD 资源类型、Controller、HTTP 端点）的完整清单。

| 字段 | 内容 |
|------|------|
| 版本 | v2.0 |
| 日期 | 2026-07-01 |

---

## 一、CRD 资源类型（9 个）

| # | CRD | API Group | 说明 |
|---|-----|-----------|------|
| 1 | `Cluster` | `neon.oltp.molnett.org/v1alpha1` | 集群拓扑：定义 Pageserver 数量、Safekeeper 数量、默认 Pageserver 配置 |
| 2 | `Pageserver` | `neon.oltp.molnett.org/v1alpha1` | 存储节点：管理单个 Pageserver Pod（StatefulSet + Service + ConfigMap） |
| 3 | `Safekeeper` | `neon.oltp.molnett.org/v1alpha1` | WAL 节点：管理单个 Safekeeper Pod（StatefulSet + Service） |
| 4 | `Branch` | `neon.oltp.molnett.org/v1alpha1` | 分支：Name、PGVersion、ProjectID、ParentBranch、ParentLSN、InitSource 等 |
| 5 | `Project` | `neon.oltp.molnett.org/v1alpha1` | 项目：多租户入口，关联多个 Branch |
| 6 | `Endpoint` | `neon.oltp.molnett.org/v1alpha1` | 计算节点：Deployment + Service + ConfigMap，管理 Compute Pod |
| 7 | `Role` | `neon.oltp.molnett.org/v1alpha1` | 数据库角色：branchID、roleName、encryptedPassword（SCRAM-SHA-256） |
| 8 | `Database` | `neon.oltp.molnett.org/v1alpha1` | 数据库：branchID、databaseName |
| 9 | `Operation` | `neon.oltp.molnett.org/v1alpha1` | 异步操作：记录 Project/Branch/Endpoint 的创建/删除操作状态 |

CRD YAML 定义位于 `config/crd/bases/`，共 9 个文件。

---

## 二、Controller（9 个）

所有 Controller 使用 `controller-runtime` Reconciler 模式，通过 SSA（Server-Side Apply）管理子资源。

| # | Controller | 管理资源 | 模式 |
|---|-----------|---------|------|
| 1 | `ClusterController` | StorageController Deployment + StorageBroker Service | 手动创建 SC 和 Broker |
| 2 | `PageserverController` | Pageserver StatefulSet, Service, ConfigMap | `ReconcileSSA[Pageserver]` |
| 3 | `SafekeeperController` | Safekeeper StatefulSet, Service | `ReconcileSSA[Safekeeper]` |
| 4 | `BranchController` | Timeline CR（Storage Controller API 调用） | 外部 API + 状态同步 |
| 5 | `ProjectController` | 关联 Branch + Endpoint + Role + Database 的聚合状态 | 聚合 Controller |
| 6 | `EndpointController` | Compute Deployment, Service, ConfigMap | `ReconcileSSA[Endpoint]` |
| 7 | `RoleController` | 密码生成、Secret 存储、SCRAM-SHA-256 加密、ComputeSpec 聚合 | 密码管理 + SSA |
| 8 | `DatabaseController` | Database 状态管理、ComputeSpec 聚合 | SSA |
| 9 | `OperationController` | Operation 生命周期跟踪 | 状态机 |

所有 Controller 在 `cmd/controller/main.go` 中注册，LeaderElection 默认为 `false`。

---

## 三、HTTP 端点

ControlPlane 作为 `manager.Runnable` 运行在 BindAddr（默认 `:8082`），前缀 `/api/v2/`。

### 3.1 REST API（35 个端点）

#### Project

| 方法 | 路径 | 状态 |
|------|------|------|
| `POST` | `/api/v2/projects` | ✅ 一键创建：Project + Branch + Endpoint + Role + Database + Operation |
| `GET` | `/api/v2/projects` | ✅ 列出所有 Project |
| `GET` | `/api/v2/projects/{id}` | ✅ 获取单个 Project 详情 |
| `PATCH` | `/api/v2/projects/{id}` | ✅ 部分更新 Project（name、default_endpoint_settings、history_retention_seconds、ip_allow） |
| `DELETE` | `/api/v2/projects/{id}` | ✅ 删除 Project 及关联资源 |

#### Branch

| 方法 | 路径 | 状态 |
|------|------|------|
| `POST` | `/api/v2/projects/{pid}/branches` | ✅ 创建 Branch |
| `GET` | `/api/v2/projects/{pid}/branches` | ✅ 列出 Project 下所有 Branch |
| `GET` | `/api/v2/projects/{pid}/branches/{id}` | ✅ 获取单个 Branch 详情 |
| `DELETE` | `/api/v2/projects/{pid}/branches/{id}` | ✅ 删除 Branch |
| `PATCH` | `/api/v2/projects/{pid}/branches/{id}` | ✅ 更新 Branch（name、protected） |
| `POST` | `/api/v2/projects/{pid}/branches/{id}/set_as_default` | ✅ 设置默认分支 |

#### Endpoint

| 方法 | 路径 | 状态 |
|------|------|------|
| `POST` | `/api/v2/projects/{pid}/endpoints` | ✅ 创建 Endpoint（body 含 branch_id） |
| `GET` | `/api/v2/projects/{pid}/endpoints` | ✅ 列出 Project 下所有 Endpoint |
| `GET` | `/api/v2/projects/{pid}/branches/{bid}/endpoints` | ✅ 列出指定 Branch 的 Endpoint |
| `GET` | `/api/v2/projects/{pid}/endpoints/{id}` | ✅ 获取单个 Endpoint 详情 |
| `DELETE` | `/api/v2/projects/{pid}/endpoints/{id}` | ✅ 删除 Endpoint |
| `PATCH` | `/api/v2/projects/{pid}/endpoints/{id}` | ✅ 更新 Endpoint（type、resources、disabled） |
| `POST` | `/api/v2/projects/{pid}/endpoints/{id}/start` | ✅ 启动 Endpoint |
| `POST` | `/api/v2/projects/{pid}/endpoints/{id}/suspend` | ✅ 挂起 Endpoint |
| `POST` | `/api/v2/projects/{pid}/endpoints/{id}/restart` | ✅ 重启 Endpoint |

#### Role

| 方法 | 路径 | 状态 |
|------|------|------|
| `POST` | `/api/v2/projects/{pid}/branches/{bid}/roles` | ✅ 创建 Role |
| `GET` | `/api/v2/projects/{pid}/branches/{bid}/roles` | ✅ 列出 Role |
| `GET` | `/api/v2/projects/{pid}/branches/{bid}/roles/{name}` | ✅ 获取单个 Role 详情 |
| `PATCH` | `/api/v2/projects/{pid}/branches/{bid}/roles/{name}` | ✅ 更新 Role 属性 |
| `DELETE` | `/api/v2/projects/{pid}/branches/{bid}/roles/{name}` | ✅ 删除 Role |
| `POST` | `/api/v2/projects/{pid}/branches/{bid}/roles/{name}/reset_password` | ✅ 重置密码 |

#### Database

| 方法 | 路径 | 状态 |
|------|------|------|
| `POST` | `/api/v2/projects/{pid}/branches/{bid}/databases` | ✅ 创建 Database |
| `GET` | `/api/v2/projects/{pid}/branches/{bid}/databases` | ✅ 列出 Database |
| `GET` | `/api/v2/projects/{pid}/branches/{bid}/databases/{name}` | ✅ 获取单个 Database 详情 |
| `PATCH` | `/api/v2/projects/{pid}/branches/{bid}/databases/{name}` | ✅ 更新 Database 属性 |
| `DELETE` | `/api/v2/projects/{pid}/branches/{bid}/databases/{name}` | ✅ 删除 Database |

#### Connection URI

| 方法 | 路径 | 状态 |
|------|------|------|
| `GET` | `/api/v2/projects/{pid}/connection_uri` | ✅ 获取连接字符串（query: database_name, role_name, branch_id, endpoint_id, pooled） |

#### Operation

| 方法 | 路径 | 状态 |
|------|------|------|
| `GET` | `/api/v2/projects/{pid}/operations` | ✅ 列出 Operation |
| `GET` | `/api/v2/projects/{pid}/operations/{id}` | ✅ 获取单个 Operation 详情 |

### 3.2 Compute 协议端点（3 个）

| 方法 | 路径 | 用途 |
|------|------|------|
| `GET` | `/compute/api/v2/computes/{compute_id}/spec` | Compute Pod 拉取自己的启动配置 |
| `POST` | `/notify-attach` | Storage Controller 回调：通知 Compute 已挂载 |
| `POST` | `/notify-safekeepers` | Storage Controller 回调：通知 Safekeeper 变更 |

### 3.3 健康检查端点（2 个）

| 方法 | 路径 | 用途 |
|------|------|------|
| `GET` | `/health` | Liveness 探针 |
| `GET` | `/ready` | Readiness 探针 |

---

## 四、已实现的核心能力

| 能力 | 状态 | 关联组件 |
|------|------|---------|
| SCRAM-SHA-256 密码管理 | ✅ | `utils/scram.go`, `RoleController` |
| ComputeSpec 聚合（Roles + Databases） | ✅ | `specs/compute/spec.go`, `GenerateComputeSpec()` |
| SSA 通用 Reconciliation 引擎 | ✅ | `internal/controller/` — `ReconcileSSA[T]` + `DeepDerivative` 漂移检测 |
| Project 一键创建（原子操作） | ✅ | `api_service.go`, `CreateProject()` |
| Project PATCH 部分更新（三态语义） | ✅ | `api_service.go`, `UpdateProject()`, `Nullable[T]` |
| Operation 异步跟踪 | ✅ | `OperationController` |
| Storage Controller 部署 | ✅ | `ClusterController` |
| Storage Broker 部署 | ✅ | `ClusterController` |

---

## 五、核心代码文件

| 文件 | 职责 |
|------|------|
| `api/v1alpha1/cluster_types.go` | Cluster CRD 类型定义 |
| `api/v1alpha1/branch_types.go` | Branch CRD 类型定义 |
| `api/v1alpha1/*_types.go` | 其余 7 个 CRD 类型定义 |
| `internal/controlplane/api_service.go` | HTTP API 业务逻辑（~1300 行） |
| `internal/controlplane/routes.go` | 路由注册 + Compute 协议端点 |
| `internal/controlplane/api_routes.go` | REST API 路由注册 |
| `internal/controller/cluster_controller.go` | Cluster 拓扑协调 |
| `internal/controller/pageserver_controller.go` | Pageserver 状态协调 |
| `internal/controller/safekeeper_controller.go` | Safekeeper 状态协调 |
| `internal/controller/endpoint_controller.go` | Compute 生命周期管理 |
| `internal/controller/branch_controller.go` | Timeline 外部 API 同步 |
| `internal/controller/project_controller.go` | 资源聚合 |
| `internal/controller/role_controller.go` | 密码 + SCRAM |
| `internal/controller/database_controller.go` | Database 管理 |
| `internal/controller/operation_controller.go` | Operation 状态机 |
| `specs/compute/spec.go` | ComputeSpec 生成逻辑 |
| `specs/pageserver/statefulset.go` | Pageserver StatefulSet 构建 |
| `specs/safekeeper/statefulset.go` | Safekeeper StatefulSet 构建 |
| `specs/storagecontroller/deployment.go` | StorageController Deployment 构建 |
| `utils/scram.go` | SCRAM-SHA-256 实现 |
