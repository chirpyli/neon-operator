
# todo清单

## ✅ Branch/Endpoint 职责分离（已完成）

Branch 与 Compute 解耦，Endpoint CRD 独立管理计算实例。详见 [control-plane-architecture.md](./control-plane-architecture.md)。

## ✅ Role/Database CRD + SCRAM-SHA-256 密码管理（已完成）

- Role/Database CRD 已定义并注册
- Role Controller：密码生成 → Secret 存储 → SCRAM-SHA-256 加密 → Status.EncryptedPassword
- SCRAM-SHA-256 实现：`utils/scram.go`（PBKDF2-HMAC-SHA256, 4096 iterations, 16-byte salt）
- ComputeSpec 聚合：`GenerateComputeSpec()` 调用 `aggregateRoles()`/`aggregateDatabases()`
- Secret 命名统一：API 使用 `role-{branchID}-{roleName}-password` 与 Controller 一致

相关文件：`utils/scram.go`, `internal/controller/role_controller.go`, `specs/compute/spec.go`, `internal/controlplane/api_service.go`

详细设计：[phase2-role-database-sync.md](./phase2-role-database-sync.md)

## ✅ Control Plane REST API（全部完成）

对标 Neon API v2 的 REST API，运行在 `neon-controlplane` Service（端口 8081）。

### 已实现端点（完整清单）

**Projects:**
- POST /api/v2/projects — 一键创建项目（含 Branch + Endpoint + Role + Database + Operation）
- GET /api/v2/projects — 列出项目
- GET /api/v2/projects/{id} — 获取项目详情
- PATCH /api/v2/projects/{id} — 部分更新项目（name、default_endpoint_settings、history_retention_seconds、ip_allow，支持三态语义 Noop/Upsert/Remove）
- DELETE /api/v2/projects/{id} — 删除项目（K8s 级联删除）

**Branches:**
- POST .../branches — 创建分支（支持 parent_id、parent_lsn、parent_timestamp、init_source）
- GET .../branches — 列出分支
- GET .../branches/{id} — 获取分支详情
- PATCH .../branches/{id} — 更新分支（name、protected）
- DELETE .../branches/{id} — 删除分支（防删 default/protected）
- POST .../branches/{id}/set_as_default — 设默认分支（批量取消其他 default → 设目标 default）

**Endpoints:**
- POST .../endpoints — 创建端点（含 read_write 唯一性检查）
- GET .../endpoints — 列出项目所有端点
- GET .../branches/{branch_id}/endpoints — 列出分支端点
- GET .../endpoints/{id} — 获取端点详情（含 Host/Port/Phase）
- PATCH .../endpoints/{id} — 更新端点（BranchID 迁移、Resources、Disabled、SuspendTimeoutSeconds）
- DELETE .../endpoints/{id} — 删除端点
- POST .../endpoints/{id}/start — 启动端点（Disabled=false，幂等）
- POST .../endpoints/{id}/suspend — 挂起端点（Disabled=true，幂等）
- POST .../endpoints/{id}/restart — 重启端点（suspend → start + Operation 追踪）

**Roles:**
- POST .../roles — 创建角色（生成密码 + Secret 存储）
- GET .../roles — 列出角色
- GET .../roles/{name} — 获取角色详情
- PATCH .../roles/{name} — 更新角色（策略 B：不改 CR metadata.name，仅更新 spec.Name + Secret；支持密码重置）
- DELETE .../roles/{name} — 删除角色（防删 protected）
- POST .../roles/{name}/reset_password — 重置密码（更新 Secret + 清除 EncryptedPassword 触发 SCRAM 重算）

**Databases:**
- POST .../databases — 创建数据库
- GET .../databases — 列出数据库
- GET .../databases/{name} — 获取数据库详情
- PATCH .../databases/{name} — 更新数据库（重命名 + owner 变更，含冲突检查）
- DELETE .../databases/{name} — 删除数据库

**Connection URI:**
- GET .../connection_uri — 获取连接字符串（支持 database_name/role_name/branch_id/endpoint_id/pooled 参数）

**Operations:**
- GET .../operations — 列出操作
- GET .../operations/{id} — 获取操作详情

### 关键设计决策

- **三态语义**：`Nullable[T]` 封装实现 Noop/Upsert/Remove，对标 Neon Rust 的 `FieldPatch<T>`
- **Role 重命名**：策略 B — 不改 CR metadata.name，仅更新 spec.Name + Secret username
- **Endpoint restart**：通过 Operation CR 追踪异步两步操作（suspend → start）
- **SetBranchAsDefault**：批量取消其他分支 default=false → 目标分支 default=true
- **GetConnectionURI**：自动解析默认分支 + read_write 端点；密码从 Role Secret 实时读取

相关文件：`internal/controlplane/api_service.go`, `internal/controlplane/api_routes.go`, `internal/controlplane/api_types.go`

### ⚠️ 部署注意事项

```bash
# 使用版本号标签（推荐），避免 :latest 缓存问题
export IMG=192.168.232.128:5000/neon/neon-operator
export TAG=$(git describe --tags --always --dirty)

# 一步完成：编译 → 构建镜像 → 推送 → 部署
make release IMG_OPERATOR=$IMG:$TAG

# 等待 Operator 就绪
kubectl rollout status deployment/neon-controller-manager -n neon --timeout=120s

# 触发工作负载滚动更新
kubectl annotate cluster my-cluster reconcile-trigger="$(date +%s)" --overwrite -n neon
for sk in $(kubectl get safekeeper -n neon -o name); do
  kubectl annotate $sk reconcile-trigger="$(date +%s)" --overwrite -n neon
done
for ps in $(kubectl get pageserver -n neon -o name); do
  kubectl annotate $ps reconcile-trigger="$(date +%s)" --overwrite -n neon
done
```

**说明**:
- `make release` 串行执行 `docker-build` → `docker-push` → `deploy`
- `IMG_OPERATOR` 默认使用 git commit hash 作为 tag，确保每次部署唯一
- operator 是事件驱动的，重启 operator 不会触发子 Controller reconcile，需手动 annotation 触发工作负载 Pod 滚动更新

## ✅ Branch 支持完整分支模式（已完成）

BranchSpec 已支持：
- `ParentBranch`（name-based 父分支引用）、`ParentLSN`、`ParentTimestamp` — 从已有分支创建子分支
- `InitSource`: `parent-data`（默认）/ `schema-only` — 控制数据复制策略
- `Protected` / `Default` — 分支保护和默认标记
- `Name` — 显示名称，Project 内唯一

API 层 `CreateBranch` 自动解析 parent_id → 默认分支 → parent data 链路，支持通过 parent_lsn/parent_timestamp 定点分支创建。


## ✅ Safekeeper 生产级自动化部署（已完成）

详见 [safekeeper-production.md](./safekeeper-production.md)

### 核心决策

- **CRD 架构**：保持独立 Safekeeper CRD，Cluster Controller 自动创建（不合并到 Cluster）
- **✅ 健康探针**：已实现 `/v1/status:7676` 三探针（Startup/Liveness/Readiness），支持 `SafekeeperSpec.*Probe` 覆盖
- **删除机制**：通过 `scheduling_policy=Offline` 注销（SC 当前无 DELETE 端点）
- **存储要求**：生产环境必须使用云盘，禁止本地盘

## ✅ SC/Broker 生产化加固（已完成）

详见 [sc-broker-production.md](./sc-broker-production.md)

### 核心决策

| 维度 | Storage Controller | Storage Broker |
|------|-------------------|----------------|
| CPU Request | 250m | 100m |
| CPU Limit | 1 | 500m |
| Memory Request | 256Mi | 128Mi |
| Memory Limit | 512Mi | 256Mi |
| Anti-Affinity | hard (required, hostname) | soft (preferred, hostname) |
| PDB | maxUnavailable=1 | maxUnavailable=1 |
| SecurityContext | runAsUser=1000 | runAsUser=1000 |
| TerminationGracePeriod | 60s | 30s |
| CR 可覆盖 | `StorageControllerResources` | `StorageBrokerResources` |

### 设计理由

- **SC CPU 1 核 limit**：单线程 tokio 运行时，无法利用多核；60s GracePeriod 允许 leader step-down
- **SC hard 反亲和**：单副本部署，滚动更新时短暂 2 Pod 共存需物理隔离
- **Broker soft 反亲和**：无状态服务，为未来多副本扩展预留空间，不阻塞调度
- **Broker 30s GracePeriod**：无状态，秒级终止
- **PDB maxUnavailable=1**：允许自愿中断，不阻塞节点维护（Leader 选举 + 秒级重启保证快速恢复）

### 实施步骤

```
Phase 1: 基础加固
├── specs/storagecontroller/deployment.go — Resources/Affinity/SecurityContext/GracePeriod
├── specs/storagebroker/deployment.go — 同上
├── specs/storagecontroller/defaults.go — 默认资源 + 选择函数
└── specs/storagebroker/defaults.go — 同上

Phase 2: PDB
├── specs/storagecontroller/pdb.go
├── specs/storagebroker/pdb.go
└── internal/controller/cluster_create.go — PDB reconcile

Phase 3: CR 覆盖
├── api/v1alpha1/cluster_types.go — 新增 Resources 字段
└── specs 层接入覆盖逻辑
```

## safekeeper当前删除的逻辑不对



## ✅ JWT认证问题（已完成）

### 现状
- ✅ **已完成并修复**：全链路 JWT 认证已实施，包括 Operator→SC admin scope、SC PUBLIC_KEY 验证、PS/SK JWT Volume 挂载
- ✅ **IsTokenExpired 过期自愈**（v1.6）：`ensureComponentTokens()` 在复用持久化 token 前检测过期，防止确定性 exp 落入已过期时间窗口
- ✅ **TokenDefaultLifetime 升级**（v1.6）：从 365 天升级为 3650 天（10 年）
- ✅ **DNS 死锁修复**（v1.6）：Headless Service `publishNotReadyAddresses=true`，修复 SC re-attach DNS 解析死锁
- ✅ **SC --dev 已移除**，改为 PUBLIC_KEY 环境变量启用 JWT 验证

详细设计见 [jwt-production-design.md](./jwt-production-design.md) 和 [jwt.md](../jwt.md)。

## 去掉 Storage Controller `--dev` 模式

### 现状

Storage Controller 部署时使用 `--dev` 标志，对应 `StrictMode::Dev`，跳过所有安全检查和认证。

### `--dev` 控制的内容

| 模式 | StrictMode | 行为 |
|------|------------|------|
| 开发（当前） | `StrictMode::Dev` | 跳过所有安全检查和认证 |
| 生产（目标） | `StrictMode::Strict` | 强制认证和 quorum 检查 |

去掉 `--dev` 后，storage_controller 启动时会强制校验：

1. 必须提供 `public_key` — JWT 认证公钥
2. 必须提供 `pageserver_jwt_token`
3. 必须提供 `control_plane_jwt_token`
4. 必须提供 `safekeeper_jwt_token`
5. 必须指定 `--control-plane-url`
6. `--timeline-safekeeper-count` 必须 ≥ 3

### 需要做的改动

**① `specs/storagecontroller/deployment.go` — 核心改动**

当前 args 仅 `--dev` + 地址 + `--control-plane-url`，需要新增：
- `--public-key`（挂载 JWT Secret 文件）
- `--pageserver-jwt-token` / `--control-plane-jwt-token` / `--safekeeper-jwt-token`
- `--timeline-safekeeper-count` 从 `cluster.Spec.NumSafekeepers` 读取
- Deployment 中新增 Volume + VolumeMount 挂载 JWT Secret

**② `specs/storagecontroller/testdata/deployment.yaml` — 同步更新 golden file**

与 deployment.go 保持一致。

**③ `internal/controller/safekeeper_create.go` — 注册请求加 JWT 认证**

当前 safekeeper 注册 HTTP 请求没有 Authorization header，去掉 `--dev` 后 storage_controller 会校验，需添加。

**④ `internal/controller/cluster_create.go` — JWT 签发逻辑扩展**

当前只生成了 Ed25519 密钥对存入 Secret。需要扩展为：根据 `private.pem` 签发 `pageserver_jwt_token`、`control_plane_jwt_token`、`safekeeper_jwt_token`，传入 storage_controller。

**⑤ `internal/controlplane/routes.go` — ControlPlane JWT 认证**

storage_controller 回调 operator 的 `/notify-attach` 和 `/notify-safekeepers` 时带 JWT token，operator 的 ControlPlane 需验证该 token。

**⑥ Safekeeper 部署自动化**

去掉 `--dev` 后强制 `--timeline-safekeeper-count ≥ 3`：
- 当前 Cluster Controller 不会自动创建 Safekeeper CR
- 需让 Cluster Controller 根据 `NumSafekeepers` 自动创建 Safekeeper CR
- 需配置 Safekeeper 反亲和性（避免 3 个在同一节点）
- 需实现 Safekeeper 注销机制（当前删除只清理 K8s 资源，storage_controller 端残留）

### 建议：当前阶段不建议去掉

| 维度 | 结论 |
|------|------|
| 改动范围 | 至少 5~6 个文件，涉及 JWT 体系 + Safekeeper 自动化 + 全链路认证 |
| 前置依赖 | Safekeeper 自动创建 + 注销机制 + JWT 签发三条路都要跑通 |
| 当前定位 | 项目适合开发测试和学习 Neon 架构，`--dev` 模式匹配此目标 |

### 建议演进路线

```
Phase 1 (当前)     Phase 2 (Safekeeper 完善)    Phase 3 (安全增强)      Phase 4 (生产就绪)
─────────────────  ────────────────────────  ────────────────────────  ────────────────
--dev 模式          Cluster Controller         JWT 签发体系              去除 --dev
safekeepers=false  自动创建 3+ Safekeeper     全链路 JWT 认证           HA 部署
直接创建 Timeline  Safekeeper 反亲和性         ControlPlane 认证        PDB + 限流
                   注册 + quorum 协调         Pageserver JWT 认证       备份恢复
                   安全注销机制                Safekeeper 间 TLS        监控告警
```