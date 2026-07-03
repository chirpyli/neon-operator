## 部署最新代码到集群

全部部署流程分为两部分：**构建并推送 Operator 镜像**，以及**重建集群资源**（当 CRD spec 或 sub-resource spec 发生变更时）。

### 一、构建并推送 Operator 镜像

Makefile 和 `config/manager/kustomization.yaml` 已统一预置镜像地址 `192.168.232.128:5000/neon/neon-operator:latest`，直接用 make target 即可：

```sh
# 一键发布（编译 + 构建镜像 + 推送 + 部署，四步合一）
make release

# 等待新 Pod 就绪
kubectl rollout status deployment/neon-controller-manager -n neon --timeout=120s
```

或者分步执行：

```sh
# 1. 编译二进制 + 构建镜像（已内置 --no-cache 防止缓存旧二进制）
make docker-build

# 2. 推送到私有 registry
make docker-push

# 3. 部署到集群（更新 CRD + 重启 operator）
make deploy

# 4. 等待新 Pod 就绪
kubectl rollout status deployment/neon-controller-manager -n neon --timeout=120s
```

如果要使用其他镜像地址或版本：

```sh
make release IMG_OPERATOR=my-registry/neon-operator:v0.2.0
```

> **关键说明**：
> - `make build` 编译生成 `bin/manager`，`make docker-build` 已依赖 `make build`
> - `docker-build` 已内置 `--no-cache`，防止 COPY 层缓存导致旧二进制被打包
> - `make deploy` 内部已包含 `make manifests`（重新生成 CRD），CRD 变更会自动更新到集群
> - `IMG_OPERATOR` 默认值与 `config/manager/kustomization.yaml` 一致，不传参即可正常工作

### 二、验证新代码是否生效

Operator 重启后会自动 reconcile 所有已有资源。以下命令可验证 JWT 等核心变更是否已生效：

```sh
# 1. SC JWT 配置：通过环境变量注入 PUBLIC_KEY 和各组件 Token
kubectl get deployment my-cluster-storage-controller -n neon \
  -o jsonpath='{.spec.template.spec.containers[0].env[*].name}' | tr ' ' '\n' \
  | grep -E 'PUBLIC_KEY|JWT_TOKEN' | wc -l | xargs -I{} sh -c \
  '[ {} -ge 4 ] && echo "✅ SC JWT 环境变量已注入(4项)" || echo "❌ SC 缺少 JWT 配置"'

# 2. SC JWT Volume 挂载
kubectl get deployment my-cluster-storage-controller -n neon \
  -o jsonpath='{.spec.template.spec.volumes[*].name}' | grep -q 'jwt' \
  && echo "✅ SC JWT Volume 已挂载" || echo "❌ SC 缺少 JWT Volume"

# 3. PS ConfigMap：应有 JWT 认证配置
kubectl get cm my-cluster-pageserver-1 -n neon \
  -o jsonpath='{.data.pageserver\.toml}' | grep -q 'auth_validation_public_key_path' \
  && echo "✅ PS JWT 配置已注入" || echo "❌ PS 缺少 JWT 配置"

# 4. PS Volumes：应有 JWT volume
kubectl get sts my-cluster-pageserver-1 -n neon \
  -o jsonpath='{.spec.template.spec.volumes[*].name}' | grep -q 'jwt' \
  && echo "✅ PS JWT Volume 已挂载" || echo "❌ PS 缺少 JWT Volume"

# 5. JWT Secret：验证 Token 已持久化（不再从日志 grep）
kubectl get secret cluster-my-cluster-jwt -n neon \
  -o jsonpath='{.data}' | python3 -c "
import sys, json
d = json.load(sys.stdin)
keys = ['pageserver_token', 'control_plane_token', 'safekeeper_token',
        'pageserver_control_plane_token', 'pageserver_safekeeper_token']
for k in keys:
    status = '✅' if k in d else '❌'
    print(f'  {status} {k}')
" 2>/dev/null || echo "⚠ 无法读取 JWT Secret（可能未创建）"
```

### 三、重建集群资源（CRD spec 或 sub-resource spec 变更时）

当 Pageserver ConfigMap、StatefulSet、SC Deployment 等 sub-resource 的模板有变更时，需要删除并重建 Cluster 资源，让 Operator 用新逻辑重新生成：

```sh
# 1. 按依赖逆序删除子资源
kubectl delete pageserver --all -n neon
kubectl delete safekeeper --all -n neon
kubectl delete cluster --all -n neon

# 2. 等待清理完毕
kubectl get all -n neon

# 3. 确认 PVC 是否要保留（如需清空数据则一并删除）
# kubectl delete pvc -n neon -l app.kubernetes.io/instance=my-cluster

# 4. 重建 Cluster（Operator 自动创建 SC + Broker + JWT Secret）
kubectl apply -f yaml/cluster.yaml

# 5. 等待 Cluster 就绪
kubectl wait --for=condition=Available cluster/my-cluster -n neon --timeout=120s

# 6. 创建 Pageserver + Safekeeper
kubectl apply -f yaml/pageserver.yaml
kubectl apply -f yaml/safekeeper.yaml
```

> **注意**：如果只需更新 Operator 逻辑（不涉及 CRD spec 变更），执行"一、构建并推送 Operator 镜像"即可，Operator 重启后会自动 reconcile 所有已有资源。

## 用户如何

结合 README、CRD 类型定义、示例 YAML 和代码逻辑，以下是完整的用户使用指南。

---

### 一、创建 Namespace

所有资源（Operator、Cluster、Project、Branch 等）都部署在同一个 `neon` namespace 中。

```bash
# 创建 neon namespace
kubectl create namespace neon

# 验证
kubectl get namespace neon
```

也可以使用 YAML 文件创建：

```yaml
# 00-namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: neon
```

```bash
kubectl apply -f 00-namespace.yaml
```

---

### 二、前置条件

在安装 Operator 之前，需要准备好以下外部依赖：

| 依赖                        | 说明                                                            |
| --------------------------- | --------------------------------------------------------------- |
| **Kubernetes 集群**   | 1.28+，已配置`kubectl`                                        |
| **S3 兼容对象存储**   | MinIO / AWS S3 / Rook-Ceph 等，用于 Neon 的 WAL/数据层存储      |
| **PostgreSQL 数据库** | 供 StorageController 存储元数据，任意 PostgreSQL 实例均可       |
| **持久化存储 (PVC)**  | 建议 NVMe SSD StorageClass，Pageserver 和 Safekeeper 各需本地盘 |

---

### 三、安装 Operator

```bash
# 1. 编译并构建镜像
make docker-build

# 2. 安装 CRD 到集群
make install

# 3. 部署 Operator（Deployment + RBAC + ServiceAccount）
#    镜像地址已在 config/manager/kustomization.yaml 中预置
make deploy
```

部署完成后，`neon` namespace 中会运行一个单副本的 Operator Pod：

```bash
kubectl get pods -n neon | grep controller-manager
# neon-controller-manager-xxxxxxxxxx-xxxxx   1/1     Running   0          10s
```

kustomize 配置说明（`config/default/kustomization.yaml`）：
- `namespace: neon` — 所有资源部署在 neon namespace
- `namePrefix: neon-` — 资源名称统一加前缀，如 Deployment: `neon-controller-manager`，Service: `neon-controlplane`

---

### 四、操作流程总览

资源创建必须严格按照依赖顺序，否则 Controller 会因为依赖不满足而持续报错：

```
Step 1: 创建 Prerequisite Secrets（外部依赖密钥）
   │
Step 2: 创建 Cluster（顶层资源，创建 JWT Keys → StorageController → StorageBroker）
   │
Step 3: 创建 Pageserver / Safekeeper（存储层，绑定到 Cluster）
   │
Step 4: 创建 Project（租户隔间）
   │
Step 5: 创建 Branch（数据库分支）
   │
Step 6: 创建 Endpoint（计算接入点，提供 PostgreSQL 连接）
```

> **架构说明**：最新代码将 Branch 与 Compute 解耦，计算节点由 Endpoint CRD 独立管理。每个 Branch 可以有多个 Endpoint（如 `read_write` + `read_only`）。

---

### 四-附、REST API 用法

除了通过 `kubectl apply` 管理 CR 外，Operator 还提供了一套对标 Neon Cloud API v2 的 REST API，运行在 `neon-controlplane` Service（端口 8081）上。

---

#### 快速获取 API 地址

```bash
# 通过 port-forward 本地访问（推荐）
kubectl port-forward -n neon svc/neon-controlplane 8081:8081 &
API_BASE="http://localhost:8081/api/v2"

# 或直接使用 cluster IP
API_BASE="http://$(kubectl get svc neon-controlplane -n neon -o jsonpath='{.spec.clusterIP}'):8081/api/v2"
```

---

#### API 端点总览

| 资源      | 方法       | 路径                                                                                    | HTTP 成功码 | 说明                                                  |
| --------- | ---------- | --------------------------------------------------------------------------------------- | ----------- | ----------------------------------------------------- |
| Project   | `POST`   | `/api/v2/projects`                                                                    | 201         | 一键创建项目（含 Branch+Endpoint+Role+DB+Operation）  |
| Project   | `GET`    | `/api/v2/projects`                                                                    | 200         | 列出所有项目                                          |
| Project   | `GET`    | `/api/v2/projects/{project_id}`                                                       | 200         | 获取单个项目详情                                      |
| Project   | `PATCH`  | `/api/v2/projects/{project_id}`                                                       | 200         | 部分更新项目配置（name、端点配置、保留期、IP 白名单） |
| Project   | `DELETE` | `/api/v2/projects/{project_id}`                                                       | 200         | 删除项目（级联删除所有子资源）                        |
| Branch    | `POST`   | `/api/v2/projects/{project_id}/branches`                                              | 201         | 创建分支（可选附带 Endpoint）                         |
| Branch    | `GET`    | `/api/v2/projects/{project_id}/branches`                                              | 200         | 列出项目下所有分支                                    |
| Branch    | `GET`    | `/api/v2/projects/{project_id}/branches/{branch_id}`                                  | 200         | 获取单个分支详情                                      |
| Branch    | `DELETE` | `/api/v2/projects/{project_id}/branches/{branch_id}`                                  | 200         | 删除分支（级联删除子资源）                            |
| Endpoint  | `POST`   | `/api/v2/projects/{project_id}/endpoints`                                             | 201         | 创建计算端点                                          |
| Endpoint  | `GET`    | `/api/v2/projects/{project_id}/endpoints`                                             | 200         | 列出项目下所有端点                                    |
| Endpoint  | `GET`    | `/api/v2/projects/{project_id}/endpoints/{endpoint_id}`                               | 200         | 获取端点详情                                          |
| Endpoint  | `DELETE` | `/api/v2/projects/{project_id}/endpoints/{endpoint_id}`                               | 200         | 删除端点                                              |
| Role      | `POST`   | `/api/v2/projects/{project_id}/branches/{branch_id}/roles`                            | 201         | 创建数据库角色（自动生成密码）                        |
| Role      | `GET`    | `/api/v2/projects/{project_id}/branches/{branch_id}/roles`                            | 200         | 列出分支下所有角色                                    |
| Role      | `DELETE` | `/api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}`                | 200         | 删除角色（受保护角色不可删）                          |
| Role      | `POST`   | `/api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}/reset_password` | 200         | 重置角色密码                                          |
| Database  | `POST`   | `/api/v2/projects/{project_id}/branches/{branch_id}/databases`                        | 201         | 创建数据库                                            |
| Database  | `GET`    | `/api/v2/projects/{project_id}/branches/{branch_id}/databases`                        | 200         | 列出分支下所有数据库                                  |
| Database  | `DELETE` | `/api/v2/projects/{project_id}/branches/{branch_id}/databases/{database_name}`        | 200         | 删除数据库                                            |
| Operation | `GET`    | `/api/v2/projects/{project_id}/operations`                                            | 200         | 列出项目下所有操作记录                                |
| Operation | `GET`    | `/api/v2/projects/{project_id}/operations/{operation_id}`                             | 200         | 获取单个操作详情                                      |

---

#### 一、创建与更新操作（POST / PATCH）

#### 1.0 更新项目配置 — `PATCH /api/v2/projects/{project_id}`

部分更新已有项目的配置，支持三态语义（Noop / Upsert / Remove）。

**可更新字段：**

| 字段                          | 类型                  | 语义                   | 说明                                      |
| ----------------------------- | --------------------- | ---------------------- | ----------------------------------------- |
| `name`                      | `string`            | Upsert / Noop          | 项目显示名称（1-256 字符）                |
| `default_endpoint_settings` | `object` / `null` | Upsert / Remove / Noop | 新建端点的默认资源配置，传`null` 移除   |
| `history_retention_seconds` | `int64`             | Upsert / Noop          | 历史数据保留期（秒），默认 604800（7 天） |
| `ip_allow`                  | `object` / `null` | Upsert / Remove / Noop | IP 白名单配置，传`null` 移除            |

**三态语义规则：**

- 字段不出现 → **Noop**（保持原值）
- `"field": "value"` → **Upsert**（设置为新值）
- `"field": null` → **Remove**（重置为空/默认）

**请求体结构：**

```
PATCH /api/v2/projects/{project_id}
{
  "project": {
    "name": "<string, 可选>",
    "default_endpoint_settings": {                          // null 表示移除
      "resources": { "cpu": "<string>", "memory": "<string>" }
    },
    "history_retention_seconds": <int64, 可选>,
    "ip_allow": {                                           // null 表示移除
      "primary_branch_only": <bool>,
      "source_ranges": ["<CIDR>"]
    }
  }
}
```

**示例：**

```bash
# 仅更新名称
curl -s -X PATCH "$API_BASE/projects/$PROJECT_ID" \
  -H "Content-Type: application/json" \
  -d '{"project":{"name":"New Name"}}' | python3 -m json.tool

# 设置 IP 白名单
curl -s -X PATCH "$API_BASE/projects/$PROJECT_ID" \
  -H "Content-Type: application/json" \
  -d '{
    "project": {
      "ip_allow": {
        "primary_branch_only": true,
        "source_ranges": ["10.0.0.0/8", "192.168.1.0/24"]
      }
    }
  }' | python3 -m json.tool

# 设置历史保留期为 14 天
curl -s -X PATCH "$API_BASE/projects/$PROJECT_ID" \
  -H "Content-Type: application/json" \
  -d '{"project":{"history_retention_seconds":1209600}}' | python3 -m json.tool

# 移除 IP 白名单（传 null）
curl -s -X PATCH "$API_BASE/projects/$PROJECT_ID" \
  -H "Content-Type: application/json" \
  -d '{"project":{"ip_allow":null}}' | python3 -m json.tool

# 多字段同时更新
curl -s -X PATCH "$API_BASE/projects/$PROJECT_ID" \
  -H "Content-Type: application/json" \
  -d '{
    "project": {
      "name": "Prod Project v2",
      "history_retention_seconds": 2592000,
      "default_endpoint_settings": {
        "resources": {"cpu": "2", "memory": "4Gi"}
      }
    }
  }' | python3 -m json.tool
```

**响应示例**（200 OK）：

```json
{
  "project": {
    "id": "project-a1b2c3d4",
    "name": "New Name",
    "pgVersion": 17,
    "cluster": "my-cluster",
    "tenant_id": "f3a8b4c1...",
    "created_at": "2026-06-29T10:30:00Z",
    "updated_at": "2026-06-30T15:00:00Z",
    "default_endpoint_settings": {
      "resources": { "cpu": "500m", "memory": "512Mi" }
    },
    "history_retention_seconds": 1209600,
    "ip_allow": {
      "primary_branch_only": true,
      "source_ranges": ["10.0.0.0/8", "192.168.1.0/24"]
    }
  }
}
```

**PATCH 错误码：**

| HTTP | Code                  | 场景                              |
| ---- | --------------------- | --------------------------------- |
| 400  | `INVALID_REQUEST`   | JSON 格式错误                     |
| 400  | `INVALID_NAME`      | 名称为空或超过 256 字符           |
| 404  | `PROJECT_NOT_FOUND` | 项目不存在                        |
| 422  | `VALIDATION_ERROR`  | `history_retention_seconds` < 0 |

---

#### 1.1 一键创建完整项目 — `POST /api/v2/projects`

最推荐的入口，一次调用自动创建 Project + Branch + Endpoint + Role + Database + Operation 共 6 类资源。

**请求体完整结构：**

```
POST /api/v2/projects
{
  "project": {
    "name": "<string, 必填>",                          // 项目显示名称
    "pgVersion": <int, 可选, 默认 17>,                 // PostgreSQL 版本：14/15/16/17
    "cluster": "<string, 可选>",                        // 所属 Cluster 名称
    "branch": {                                        // 初始分支配置（可选）
      "name": "<string, 默认 'main'>",                 // 分支名称
      "role_name": "<string, 默认 '{name}_owner'>",    // 初始角色名
      "database_name": "<string, 默认 'neondb'>"       // 初始数据库名
    },
    "endpoint": {                                      // 初始端点配置（可选）
      "type": "<string, 默认 'read_write'>",          // 端点类型：read_write / read_only
      "resources": {                                   // 计算资源
        "cpu": "<string, 示例 '500m'>",
        "memory": "<string, 示例 '512Mi'>"
      }
    },
    "default_endpoint_settings": {                     // 新建端点的默认配置（可选）
      "resources": {
        "cpu": "<string>",
        "memory": "<string>"
      }
    }
  }
}
```

**示例：**

```bash
curl -s -X POST "$API_BASE/projects" \
  -H "Content-Type: application/json" \
  -d '{
    "project": {
      "name": "my-app",
      "pgVersion": 17,
      "cluster": "my-cluster",
      "branch": {
        "name": "main",
        "role_name": "appuser",
        "database_name": "neondb"
      },
      "endpoint": {
        "type": "read_write",
        "resources": { "cpu": "500m", "memory": "512Mi" }
      },
      "default_endpoint_settings": {
        "resources": { "cpu": "500m", "memory": "512Mi" }
      }
    }
  }' | python3 -m json.tool
```

**响应**（201 Created）— 返回 `CreatedProjectResponse`：

```json
{
  "project": {
    "id": "project-a1b2c3d4",
    "name": "my-app",
    "pgVersion": 17,
    "cluster": "my-cluster",
    "tenant_id": "f3a8b4c1...",
    "created_at": "2026-06-29T10:30:00Z",
    "default_endpoint_settings": {
      "resources": { "cpu": "500m", "memory": "512Mi" }
    }
  },
  "branch": {
    "id": "br-e5f6g7h8",
    "name": "main",
    "project_id": "project-a1b2c3d4",
    "current_state": "init",
    "default": true,
    "created_at": "2026-06-29T10:30:00Z"
  },
  "endpoints": [{
    "id": "ep-i9j0k1l2",
    "branch_id": "br-e5f6g7h8",
    "type": "read_write",
    "current_state": "init",
    "created_at": "2026-06-29T10:30:00Z"
  }],
  "connection_uris": [{
    "connection_uri": "postgresql://app_owner:<password>@<host>:<port>/neondb"
  }],
  "roles": [{
    "name": "app_owner",
    "password": "aB3x...",
    "protected": true,
    "branch_id": "br-e5f6g7h8",
    "authentication_method": "password",
    "created_at": "2026-06-29T10:30:00Z"
  }],
  "databases": [{
    "id": 1,
    "name": "neondb",
    "owner_name": "app_owner",
    "branch_id": "br-e5f6g7h8",
    "created_at": "2026-06-29T10:30:00Z"
  }],
  "operations": [{
    "id": "op-...",
    "project_id": "project-a1b2c3d4",
    "action": "create_project",
    "status": "scheduling",
    "created_at": "2026-06-29T10:30:00Z"
  }]
}
```

> **注意**：响应中的 `password` 仅在此次创建响应中返回明文，请妥善保存。后续可通过 `reset_password` 接口重置。

---

#### 1.2 创建分支 — `POST /api/v2/projects/{project_id}/branches`

从已有分支 fork 出新分支，可选附带 Endpoint。

**请求体：**

```
{
  "branch": {
    "name": "<string, 可选>",                           // 分支名称
    "parent_id": "<string, 可选>",                      // 父分支 ID，不传则自动找默认分支
    "parent_lsn": "<string, 可选, 如 '0/12345678'>",    // 按 LSN 精确位点创建
    "parent_timestamp": "<string, 可选, RFC3339>",       // PITR 时间点创建
    "init_source": "<string, 可选>",                     // "parent-data"(默认) 或 "schema-only"
    "protected": <bool, 可选>                            // 是否受保护
  },
  "endpoints": [{                                       // 可选附带端点列表
    "type": "<string, 默认 'read_write'>",
    "resources": { "cpu": "<string>", "memory": "<string>" }
  }]
}
```

**示例：**

```bash
# 获取 project_id
PROJECT_ID=$(curl -s "$API_BASE/projects" | python3 -c "import sys,json; print(json.load(sys.stdin)['projects'][0]['id'])")

# Fork 默认分支
curl -s -X POST "$API_BASE/projects/$PROJECT_ID/branches" \
  -H "Content-Type: application/json" \
  -d '{
    "branch": {
      "name": "dev",
      "init_source": "parent-data"
    },
    "endpoints": [{
      "type": "read_write",
      "resources": { "cpu": "250m", "memory": "256Mi" }
    }]
  }' | python3 -m json.tool

# PITR 分支（恢复到指定时间点）
curl -s -X POST "$API_BASE/projects/$PROJECT_ID/branches" \
  -H "Content-Type: application/json" \
  -d '{
    "branch": {
      "name": "recovery-202506",
      "parent_timestamp": "2025-06-15T12:00:00Z",
      "init_source": "parent-data"
    }
  }' | python3 -m json.tool
```

**响应**（201 Created）— 返回 `CreatedBranchResponse`：

```json
{
  "branch": {
    "id": "br-xxxxxxxx",
    "name": "dev",
    "project_id": "project-a1b2c3d4",
    "parent_id": "br-e5f6g7h8",
    "current_state": "init",
    "default": false,
    "protected": false,
    "init_source": "parent-data",
    "created_at": "2026-06-29T10:32:00Z"
  },
  "endpoints": [...],
  "operations": [...]
}
```

---

#### 1.3 创建 Endpoint — `POST /api/v2/projects/{project_id}/endpoints`

为已有分支创建计算端点。

**请求体：**

```
{
  "endpoint": {
    "branch_id": "<string, 必填>",                      // 所属分支 ID
    "type": "<string, 默认 'read_write'>",              // read_write 或 read_only
    "resources": { "cpu": "<string>", "memory": "<string>" },
    "disabled": <bool, 可选>,                            // 是否禁用
    "exposure": {                                        // 服务暴露策略（可选）
      "type": "<ClusterIP|NodePort|LoadBalancer>",
      "source_ranges": ["<CIDR>"]
    }
  }
}
```

**重要限制**：每个 Branch 最多只能有 **1 个** `read_write` Endpoint，多个 `read_only` Endpoint 则无限制。

```bash
BRANCH_ID="br-e5f6g7h8"

# 创建 read_write Endpoint
curl -s -X POST "$API_BASE/projects/$PROJECT_ID/endpoints" \
  -H "Content-Type: application/json" \
  -d "{
    \"endpoint\": {
      \"branch_id\": \"$BRANCH_ID\",
      \"type\": \"read_write\",
      \"resources\": { \"cpu\": \"500m\", \"memory\": \"512Mi\" }
    }
  }" | python3 -m json.tool

# 再创建一个 read_only Endpoint（只读，可以有多个）
curl -s -X POST "$API_BASE/projects/$PROJECT_ID/endpoints" \
  -H "Content-Type: application/json" \
  -d "{
    \"endpoint\": {
      \"branch_id\": \"$BRANCH_ID\",
      \"type\": \"read_only\"
    }
  }" | python3 -m json.tool
```

**响应**（201 Created）— 返回 `CreatedEndpointResponse`：

```json
{
  "endpoint": {
    "id": "ep-xxxxxxxx",
    "branch_id": "br-e5f6g7h8",
    "type": "read_write",
    "current_state": "init",
    "created_at": "2026-06-29T10:33:00Z"
  },
  "operations": [{
    "id": "op-...",
    "action": "start_compute",
    "status": "scheduling",
    ...
  }]
}
```

```sh
curl -s -X POST "http://localhost:8081/api/v2/projects/project-c10d8182/endpoints"   -H "Content-Type: application/json"   -d "{
    \"endpoint\": {
      \"branch_id\": \"br-c912d116\",
      \"type\": \"read_write\",
      \"resources\": { \"cpu\": \"500m\", \"memory\": \"512Mi\" },
      parent_id: "br-c912d116"
    }
  }" | python3 -m json.tool
{
    "code": "INVALID_REQUEST",
    "message": "invalid JSON request body"
}

```

---

#### 1.4 创建角色 — `POST /api/v2/projects/{project_id}/branches/{branch_id}/roles`

创建数据库角色，自动生成密码并返回明文。

**请求体：**

```
{
  "role": {
    "name": "<string, 必填>",                           // 角色名称
    "authentication_method": "<string, 默认 'password'>" // 认证方式
  }
}
```

```bash
curl -s -X POST "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID/roles" \
  -H "Content-Type: application/json" \
  -d '{"role": {"name": "reader"}}' | python3 -m json.tool
```

**响应**（201 Created）— 返回 `{"role": {...}}`，含明文密码。

---

#### 1.5 创建数据库 — `POST /api/v2/projects/{project_id}/branches/{branch_id}/databases`

**请求体：**

```
{
  "database": {
    "name": "<string, 必填>",                           // 数据库名称
    "owner_name": "<string, 可选>"                       // 所有者角色名
  }
}
```

```bash
curl -s -X POST "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID/databases" \
  -H "Content-Type: application/json" \
  -d '{"database": {"name": "analytics", "owner_name": "reader"}}' | python3 -m json.tool
```

---

#### 二、查询操作（GET）

#### 2.1 查询示例汇总

```bash
# 列出所有项目
curl -s "$API_BASE/projects" | python3 -m json.tool

# 获取单个项目详情
curl -s "$API_BASE/projects/$PROJECT_ID" | python3 -m json.tool

# 列出项目下所有分支
curl -s "$API_BASE/projects/$PROJECT_ID/branches" | python3 -m json.tool

# 获取单个分支详情
curl -s "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID" | python3 -m json.tool

# 列出项目下所有端点（通过分支关联）
curl -s "$API_BASE/projects/$PROJECT_ID/endpoints" | python3 -m json.tool

# 获取单个端点详情
curl -s "$API_BASE/projects/$PROJECT_ID/endpoints/$ENDPOINT_ID" | python3 -m json.tool

# 列出分支下所有角色
curl -s "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID/roles" | python3 -m json.tool

# 列出分支下所有数据库
curl -s "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID/databases" | python3 -m json.tool

# 列出操作记录
curl -s "$API_BASE/projects/$PROJECT_ID/operations" | python3 -m json.tool

# 获取单个操作详情
curl -s "$API_BASE/projects/$PROJECT_ID/operations/$OP_ID" | python3 -m json.tool
```

#### 2.2 查询响应结构参考

| 端点                          | 响应包装                  | 顶层键                            | HTTP 成功码 |
| ----------------------------- | ------------------------- | --------------------------------- | ----------- |
| `GET /api/v2/projects`      | `ProjectListResponse`   | `"projects"` + `"pagination"` | 200         |
| `GET /api/v2/projects/{id}` | 单对象包装                | `"project"`                     | 200         |
| `GET .../branches`          | `BranchListResponse`    | `"branches"`                    | 200         |
| `GET .../branches/{id}`     | 单对象包装                | `"branch"`                      | 200         |
| `GET .../endpoints`         | `EndpointListResponse`  | `"endpoints"`                   | 200         |
| `GET .../endpoints/{id}`    | 单对象包装                | `"endpoint"`                    | 200         |
| `GET .../roles`             | `RoleListResponse`      | `"roles"`                       | 200         |
| `GET .../databases`         | `DatabaseListResponse`  | `"databases"`                   | 200         |
| `GET .../operations`        | `OperationListResponse` | `"operations"`                  | 200         |
| `GET .../operations/{id}`   | 单对象包装                | `"operation"`                   | 200         |

---

#### 三、删除操作（DELETE）

所有 DELETE 操作成功均返回 HTTP **200**（非 204），响应体为 `DeleteResponse`：

```json
{
  "project": { "id": "<id>", "name": "<name>" },   // 仅 DELETE Project 时
  "branch": { "id": "<id>" },                       // 仅 DELETE Branch 时
  "endpoint": { "id": "<id>", "type": "<type>" },   // 仅 DELETE Endpoint 时
  "operations": [{ "id": "op-...", "action": "...", "status": "scheduling", ... }]
}
```

每次删除操作会自动创建一个 `"action": "delete_xxx"` 的 Operation 记录。

---

#### 3.1 删除 Project — `DELETE /api/v2/projects/{project_id}`

删除项目及其所有子资源。

**级联行为**：

- Project 删除时，K8s 通过 `ownerReferences` 机制自动级联删除：
  - 所有直接子 Branch
  - 所有 Operation（`ownerReference` 指向 Project）
- 每个 Branch 再级联删除其下的 Endpoint、Role、Database（均通过 `ownerReference`）
- 角色关联的密码 Secret 通过 `ownerReference` 一并清理

```bash
curl -s -X DELETE "$API_BASE/projects/$PROJECT_ID" | python3 -m json.tool
```

**响应示例**（200 OK）：

```json
{
  "project": {
    "id": "project-a1b2c3d4",
    "name": "my-app"
  },
  "operations": [{
    "id": "op-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
    "project_id": "project-a1b2c3d4",
    "action": "delete_project",
    "status": "scheduling",
    "created_at": "2026-06-29T10:40:00Z"
  }]
}
```

**错误情况**：

- 项目不存在：`404` + `"code": "PROJECT_NOT_FOUND"`

---

#### 3.2 删除 Branch — `DELETE /api/v2/projects/{project_id}/branches/{branch_id}`

删除分支及其子资源（Endpoint、Role、Database 等）。

**删除保护**：

| 保护条件                                | HTTP 状态码  | 错误 Code                 |
| --------------------------------------- | ------------ | ------------------------- |
| 分支是默认分支（`default: true`）     | 409 Conflict | `DEFAULT_BRANCH_DELETE` |
| 分支标记为受保护（`protected: true`） | 409 Conflict | `PROTECTED_BRANCH`      |

```bash
curl -s -X DELETE "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID" | python3 -m json.tool
```

**响应示例**（200 OK）：

```json
{
  "branch": { "id": "br-xxxxxxxx" },
  "operations": [{
    "id": "op-...",
    "project_id": "project-a1b2c3d4",
    "branch_id": "br-xxxxxxxx",
    "action": "delete_branch",
    "status": "scheduling",
    "created_at": "2026-06-29T10:41:00Z"
  }]
}
```

**级联行为**：Branch 通过 `ownerReference` 级联删除其下的所有 Endpoint、Role、Database 及密码 Secret。

---

#### 3.3 删除 Endpoint — `DELETE /api/v2/projects/{project_id}/endpoints/{endpoint_id}`

```bash
curl -s -X DELETE "$API_BASE/projects/$PROJECT_ID/endpoints/$ENDPOINT_ID" | python3 -m json.tool
```

**响应示例**（200 OK）：

```json
{
  "endpoint": { "id": "ep-xxxxxxxx", "type": "read_write" },
  "operations": [{
    "id": "op-...",
    "action": "delete_endpoint",
    "status": "scheduling",
    ...
  }]
}
```

**注意**：Endpoint 本身没有删除保护，但如果没有 Endpoint，用户无法连接到数据库。删除唯一的 `read_write` Endpoint 后，可通过 `POST .../endpoints` 重新创建一个。

---

#### 3.4 删除 Role — `DELETE /api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}`

```bash
curl -s -X DELETE "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID/roles/reader" | python3 -m json.tool
```

**响应**（200 OK）：

```json
{ "deleted": true }
```

**保护条件**：

| 条件                                                 | HTTP | Code               |
| ---------------------------------------------------- | ---- | ------------------ |
| `status.protected: true`（如初始 `_owner` 角色） | 409  | `PROTECTED_ROLE` |
| 角色不存在                                           | 404  | `ROLE_NOT_FOUND` |

---

#### 3.5 删除 Database — `DELETE /api/v2/projects/{project_id}/branches/{branch_id}/databases/{database_name}`

```bash
curl -s -X DELETE "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID/databases/analytics" | python3 -m json.tool
```

**响应**（200 OK）：

```json
{ "deleted": true }
```

**注意**：Database 没有保护机制，任何数据库都可以被删除。

---

#### 四、密码管理

#### 4.1 重置角色密码 — `POST /api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}/reset_password`

生成新密码并返回明文，旧密码立即失效。

```bash
curl -s -X POST "$API_BASE/projects/$PROJECT_ID/branches/$BRANCH_ID/roles/app_owner/reset_password" | python3 -m json.tool
```

**响应**（200 OK）— 返回 `ResetPasswordResponse`：

```json
{
  "role": {
    "name": "app_owner",
    "password": "xY9z...（新密码）",
    "branch_id": "br-e5f6g7h8",
    "created_at": "2026-06-29T10:45:00Z"
  }
}
```

> 密码通过 K8s Secret（名称格式为 `role-{branch_id}-{role_name}-password`）存储，重置时直接更新该 Secret。Secret 命名与 Role Controller 的 `ensurePasswordSecret` 保持一致，确保 API 返回的密码即为实际生效的 PostgreSQL 密码。

---

#### 五、错误码参考

所有错误响应格式统一为：

```json
{
  "code": "<错误代码>",
  "message": "<人类可读描述>"
}
```

| HTTP 状态码 | Code                           | 适用端点                           | 说明                                             |
| ----------- | ------------------------------ | ---------------------------------- | ------------------------------------------------ |
| 400         | `INVALID_REQUEST`            | 所有 POST                          | JSON 请求体格式错误                              |
| 400         | `INVALID_NAME`               | POST role/database / PATCH project | 名称字段为空或超出长度                           |
| 404         | `PROJECT_NOT_FOUND`          | project GET/DELETE/PATCH           | 项目不存在                                       |
| 404         | `BRANCH_NOT_FOUND`           | branch GET/DELETE                  | 分支不存在或不属于该项目                         |
| 404         | `ENDPOINT_NOT_FOUND`         | endpoint GET/DELETE                | 端点不存在                                       |
| 404         | `ROLE_NOT_FOUND`             | role DELETE/reset                  | 角色不存在                                       |
| 404         | `DATABASE_NOT_FOUND`         | database DELETE                    | 数据库不存在                                     |
| 404         | `OPERATION_NOT_FOUND`        | operation GET                      | 操作记录不存在                                   |
| 404         | `DEFAULT_BRANCH_NOT_FOUND`   | branch POST                        | 创建分支时未找到默认父分支                       |
| 409         | `DEFAULT_BRANCH_DELETE`      | branch DELETE                      | 不能删除默认分支                                 |
| 409         | `PROTECTED_BRANCH`           | branch DELETE                      | 不能删除受保护分支                               |
| 409         | `PROTECTED_ROLE`             | role DELETE                        | 不能删除受保护角色                               |
| 409         | `READ_WRITE_ENDPOINT_EXISTS` | endpoint POST                      | 该分支已有 read_write 端点                       |
| 422         | `VALIDATION_ERROR`           | project PATCH                      | 参数校验失败（如 history_retention_seconds < 0） |
| 500         | `INTERNAL_ERROR`             | 所有                               | 内部服务器错误                                   |

---

#### 六、资源生命周期与级联删除

#### 6.1 OwnerReference 关系树

所有 API 创建的资源通过 K8s `ownerReference` 建立父子关系，确保级联删除：

```
Project (K8s CR)
  ├── Operation[]       (ownerReference → Project)
  └── Branch[]          (ownerReference → Project)
        ├── Endpoint[]  (ownerReference → Branch)
        ├── Role[]      (ownerReference → Branch)
        │     └── Secret (密码)  (ownerReference → Role)
        └── Database[]  (ownerReference → Branch)
```

当删除父资源时，K8s 垃圾回收器会自动清理所有子资源，无需逐一手动删除。

#### 6.2 清理顺序建议

如果通过 API 逐层删除，推荐的顺序为：

```
1. 删除 Endpoint（停止计算节点，释放资源）
2. 删除 Branch（级联删除其下的 Endpoint / Role / Database）
3. 删除 Project（级联删除其下的 Branch / Operation）
```

但实际由于 `ownerReference` 级联机制，**直接删除 Project 即可清理所有子资源**。

```bash
# 最简方式：直接删除 Project，一键清理所有
curl -s -X DELETE "$API_BASE/projects/$PROJECT_ID" | python3 -m json.tool
```

---

#### 七、API vs kubectl 对比

| 场景             | 推荐方式      | 原因                                                              |
| ---------------- | ------------- | ----------------------------------------------------------------- |
| 一键创建完整项目 | **API** | 自动生成 ID、密码，创建 Project+Branch+Endpoint+Role+DB+Operation |
| 删除/清理资源    | **API** | RESTful，支持级联删除，自动创建操作审计记录                       |
| 密码管理         | **API** | 自动生成/重置密码，返回明文                                       |
| 精细控制 CR 配置 | kubectl       | 直接操作 K8s CR，可设置所有字段                                   |
| CI/CD 流水线     | **API** | RESTful 接口，易于集成                                            |
| 日常运维查看     | kubectl       | `kubectl get` / `kubectl describe` 更直观                     |

---

#### 八、与 K8s 资源的映射关系

API 路径参数与 K8s 资源名称的对应关系：

| API 参数          | K8s 资源         | K8s Name（metadata.name）           |
| ----------------- | ---------------- | ----------------------------------- |
| `project_id`    | `Project` CR   | `project-xxxxxxxx` (8 位随机 hex) |
| `branch_id`     | `Branch` CR    | `br-xxxxxxxx` (8 位随机 hex)      |
| `endpoint_id`   | `Endpoint` CR  | `ep-xxxxxxxx` (8 位随机 hex)      |
| `role_name`     | `Role` CR      | `{branch_id}-{role_name}`         |
| `database_name` | `Database` CR  | `{branch_id}-{database_name}`     |
| `operation_id`  | `Operation` CR | `op-{full-uuid}`                  |

> **注意**：API 使用 `project_id` / `branch_id` / `endpoint_id` 作为路径参数，这些 ID 就是 K8s 资源的 `metadata.name`。因此可以通过 API 获取 ID 后，直接使用 `kubectl get project <id> -n neon` 查看底层 K8s 资源。

---

### 五、Step 1：创建前置 Secret

#### 5.1 对象存储凭证 Secret

Cluster 和 Pageserver 都需要访问 S3 兼容对象存储：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bucket-credentials      # 这个名字要和 Cluster.spec.bucketCredentialsSecret.name 一致
  namespace: neon
type: Opaque
stringData:
  AWS_ACCESS_KEY_ID: "your-access-key"
  AWS_SECRET_ACCESS_KEY: "your-secret-key"
  AWS_REGION: "us-east-1"
  BUCKET_NAME: "neon-data"
  ENDPOINT: "http://minio.neon.svc:9000"
```

部署：

```bash
kubectl apply -f bucket-credentials.yaml
```

#### 5.2 StorageController 数据库 Secret

StorageController 需要一个 PostgreSQL 数据库存储 shard 拓扑、租户映射等元数据：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: storage-controller-pg-cluster-app   # 这个名字要和 Cluster.spec.storageControllerDatabaseSecret.name 一致
  namespace: neon
type: Opaque
stringData:
  uri: "postgresql://storcon:password@postgres-host:5432/storage_controller"
```

部署：

```bash
kubectl apply -f storage-controller-db-secret.yaml
```

---

### 六、Step 2：创建 Cluster

Cluster 是顶层资源，Operator 会为其自动创建 JWT 密钥对、StorageController Deployment/Service、StorageBroker Deployment/Service。最新代码还支持自动创建 Pageserver 和 Safekeeper 实例。

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: my-cluster
  namespace: neon
spec:
  numSafekeepers: 3                       # Safekeeper 数量，最少 3 个（共识要求）
  numPageservers: 1                        # Pageserver 数量，生产环境建议 ≥2
  defaultPGVersion: 17                     # 默认 PostgreSQL 版本：14/15/16/17
  neonImage: "neondatabase/neon:9129"      # Neon 组件镜像，可使用本地 registry 加速
  bucketCredentialsSecret:
    name: bucket-credentials               # 引用 Step 1 创建的 Secret
  storageControllerDatabaseSecret:
    name: storage-controller-pg-cluster-app
    key: uri
  postgresExposure:                        # 控制 PostgreSQL Service 对外暴露方式（可选）
    type: ClusterIP                        # ClusterIP | NodePort | LoadBalancer
  defaultPageserverConfig:                 # 自动创建的 Pageserver 默认配置（可选）
    storageSize: "100Gi"
    initialSchedulingPolicy: Active        # Active | Filling
  defaultSafekeeperStorage:               # 自动创建的 Safekeeper 默认存储配置（可选）
    size: "30Gi"
```

> **本地镜像加速**：如果无法访问外网 registry（如 `ghcr.io`），可将 neon 镜像推送到本地 registry 后使用本地路径：
> ```yaml
> neonImage: "192.168.232.128:5000/neondatabase/neon:8463"
> ```
> 部署前先用 `docker images | grep neondatabase/neon` 确认本地已有镜像，然后 `docker tag` + `docker push` 到本地 registry。

```bash
kubectl apply -f cluster.yaml
```

#### 验证 Cluster 状态

```bash
# 查看自定义列：Available / Progressing / Age
kubectl get clusters -n neon

# 查看详细信息
kubectl get cluster my-cluster -n neon -o yaml

# 观察子资源是否自动创建
kubectl get deployment -n neon | grep my-cluster
# 应该看到：my-cluster-storage-controller  和  my-cluster-storage-broker

kubectl get service -n neon | grep my-cluster
kubectl get secret -n neon | grep my-cluster-jwt
```

当 `Available=True` 且 `Progressing=False` 时，说明 Cluster 已就绪：

```bash
kubectl get cluster my-cluster -n neon -o jsonpath='{.status.conditions[?(@.type=="Available")].status}'
# 输出: True
```

---

### 七、Step 3：创建 Pageserver 和 Safekeeper

这些是存储层组件，每个实例都有独立的 PVC 持久化存储。

> **新特性**：如果 Cluster 中指定了 `numPageservers`、`defaultPageserverConfig`、`defaultSafekeeperStorage`，Operator 会自动创建对应数量的实例，无需手动逐个创建 YAML。手动创建 CR 仍然支持，用于覆盖个别实例的配置。

#### 7.1 自动创建（推荐）

只需在 Cluster 中设置以下字段，Operator 会通过 Controller 自动创建：

```yaml
# cluster.yaml 中的配置
spec:
  numSafekeepers: 3
  numPageservers: 2
  defaultPageserverConfig:
    storageSize: "100Gi"
  defaultSafekeeperStorage:
    size: "30Gi"
```

#### 7.2 手动创建（覆盖自动配置）

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Pageserver
metadata:
  name: pageserver-0
  namespace: neon
spec:
  cluster: my-cluster                     # 绑定到的 Cluster 名称
  id: 0                                   # Pageserver ID（手动管理，0, 1, 2...）
  bucketCredentialsSecret:
    name: bucket-credentials
  storageConfig:
    size: 50Gi                            # PVC 大小
```

```bash
kubectl apply -f pageserver.yaml
```

多个 Pageserver 需逐个创建（如不使用自动创建特性）：

```yaml
# pageserver-1.yaml
metadata:
  name: pageserver-1
spec:
  id: 1
  ...
```

#### 7.3 手动创建 Safekeeper

Safekeeper 是 WAL 共识层，需要至少 3 个。每个 Safekeeper 也需要独立创建：

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-1
  namespace: neon
spec:
  cluster: my-cluster
  id: 1
  storageConfig:
    size: 30Gi
---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-2
  namespace: neon
spec:
  cluster: my-cluster
  id: 2
  storageConfig:
    size: 30Gi
---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-3
  namespace: neon
spec:
  cluster: my-cluster
  id: 3
  storageConfig:
    size: 30Gi
```

```bash
kubectl apply -f safekeepers.yaml
```

---

### 八、Step 4：创建 Project

Project 是租户隔离单位，对应 Neon 中的一个"项目"（类似传统数据库中的一个 database cluster）：

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Project
metadata:
  name: my-project                        # K8s 内部标识（DNS label 约束）
  namespace: neon
spec:
  cluster: my-cluster                     # 所属 Cluster
  name: "My Project"                      # 项目显示名称（区别于 metadata.name）
  pgVersion: 17                           # PG 版本，可覆盖 Cluster 默认值
  tenantId: ""                            # 留空则 Operator 自动生成 32 位 tenant ID
  defaultEndpointSettings:                # 新建 Endpoint 的默认资源配置（可选）
    resources:
      requests:
        cpu: "500m"
        memory: "512Mi"
      limits:
        cpu: "1000m"
        memory: "1Gi"
```

```bash
kubectl apply -f project.yaml
```

创建成功后，Operator 会通过 StorageController API 分配一个 Tenant ID（类似于 Neon 云服务中的 project_id）。

---

### 九、Step 5：创建 Branch

Branch 是 Project 下的数据库分支（类似 Git 分支），利用 Neon 的 Copy-on-Write 存储实现低成本分支。

#### 9.1 创建主分支（初始分支）

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Branch
metadata:
  name: main-branch
  namespace: neon
spec:
  projectID: my-project                   # 父 Project 名称
  pgVersion: 17
  timelineID: ""                          # 留空则 Operator 自动生成
  default: true                           # 设为项目的默认分支
  protected: true                         # 保护分支，不可直接删除
```

如果这是 Project 下的第一个 Branch 且没有指定 `parentBranch`，则会创建一个全新的空分支（相当于 `initdb`）。

#### 9.2 从父分支创建子分支

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Branch
metadata:
  name: dev-branch
  namespace: neon
spec:
  projectID: my-project
  pgVersion: 17
  parentBranch: main-branch               # 父分支名称
  parentLSN: ""                           # 指定起始 LSN（如 "0/12345678"），与 parentTimestamp 互斥
  parentTimestamp: ""                     # PITR 时间点（如 "2025-01-01T00:00:00Z"），与 parentLSN 互斥
  initSource: "parent-data"               # "parent-data": 从父分支复制全量数据（默认）
                                           # "schema-only": 仅复制 schema
  protected: false
```

**分支创建模式说明**：

| 字段                | 说明                                                                   |
| ------------------- | ---------------------------------------------------------------------- |
| `parentBranch`    | 不为空时从已有分支创建；为空则创建全新初始分支                         |
| `parentLSN`       | 指定起始 LSN 创建分支（精确位点）                                      |
| `parentTimestamp` | 按时间点创建分支（PITR），与`parentLSN` 互斥，优先使用 `parentLSN` |
| `initSource`      | `parent-data`：复制全量数据 / `schema-only`：仅 schema             |
| `protected`       | 受保护分支不可直接删除                                                 |
| `default`         | 每个 Project 只能有一个默认分支，默认分支不可删除                      |

---

### 十、Step 6：创建 Endpoint（计算接入点）

> **架构变更**：最新代码将 Branch 与 Compute 解耦，计算节点不再由 Branch 自动创建，而是通过 Endpoint CRD 独立管理。

Endpoint 是 PostgreSQL 的连接入口，每个 Branch 可以有多个 Endpoint：

#### 10.1 创建 read_write Endpoint（可写）

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Endpoint
metadata:
  name: main-endpoint-rw
  namespace: neon
spec:
  branchID: main-branch                   # 所属 Branch 名称
  type: read_write                        # read_write 或 read_only
  disabled: false                         # 设为 true 可将 replicas 缩为 0
  resources:                              # 计算资源（可选，未设置时继承 Project 默认值）
    requests:
      cpu: "500m"
      memory: "512Mi"
    limits:
      cpu: "1000m"
      memory: "1Gi"
  exposure:                               # 覆盖 Cluster 级别的暴露策略（可选）
    type: ClusterIP                       # ClusterIP | NodePort | LoadBalancer
```

**限制**：每个 Branch 最多只能有 **1 个** `read_write` 类型的 Endpoint。

#### 10.2 创建 read_only Endpoint（只读）

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Endpoint
metadata:
  name: main-endpoint-ro
  namespace: neon
spec:
  branchID: main-branch
  type: read_only                         # WAL follower，只读
  disabled: false
```

`read_only` Endpoint 可以有多个，它们作为 WAL follower，不参与 WAL 共识，仅提供只读查询。

**Endpoint 类型与 Compute Mode 的映射关系：**

| Endpoint Type | compute_ctl Mode | PG 行为                                            | WAL 消费方式 |
|:------------- |:---------------- |:-------------------------------------------------- |:---|
| `read_write` | `Primary`        | WAL proposer，参与 safekeeper 共识，可读写         | walproposer 推送 WAL 到 safekeeper |
| `read_only`  | `Replica`        | WAL follower，hot standby，动态跟随分支 tip LSN   | PostgreSQL 原生流复制从 safekeeper 拉取 WAL（`primary_conninfo`） |

> **注意**：Mode 字段为 operator 自动设置，用户无需在 CR 中指定。Mode 值必须大写首字母以匹配 compute_ctl 的 Rust 枚举定义（`Primary`/`Replica`/`Static`），小写将导致 compute-node 反序列化失败。默认情况下（无 endpoint-type label 的遗留 deployment），缺省为 `Primary` 模式以保持向后兼容。
>
> **Replica WAL 同步**：read_only 端点的 operator 自动配置 `primary_conninfo` 指向所有 safekeeper，使用 PostgreSQL 原生 streaming replication 协议获取 WAL。这与 neon_local 控制平面的设计一致。Replica 不消费复制槽（replication slot），不产生额外存储成本，且启动后几乎即时同步。

#### 10.3 验证 Endpoint 状态

```bash
# 查看 Endpoint 列表
kubectl get endpoints -n neon

# 输出示例：
# NAME                BRANCH         TYPE         PHASE   HOST   AGE
# main-endpoint-rw    main-branch    read_write   active x.x.x.x 1m
# main-endpoint-ro    main-branch    read_only    active x.x.x.x 1m

# 查看详情
kubectl describe endpoint main-endpoint-rw -n neon
```

#### 10.4 连接数据库

```bash
# 查看 Endpoint 的 Service
kubectl get svc -n neon -l molnett.org/component=compute-postgres

# 端口转发连接
kubectl port-forward -n neon svc/<endpoint-name>-pg 5432:55433

# 本地连接
psql -h localhost -p 5432 -U cloud_admin -d neondb
```

#### 10.5 Endpoint 生命周期

```
creating_compute → starting → active → stopping → stopped
```

可以修改 `spec.disabled` 来启停计算节点（Service 会保留，但 Deployment replicas 变为 0）。

---

### 十一、监控与运维

#### 11.1 查看所有资源

```bash
# 查看所有 CRD 资源
kubectl get clusters,projects,branches,endpoints,pageservers,safekeepers -n neon

# 输出示例：
# NAME                    AVAILABLE   PROGRESSING   AGE
# cluster.my-cluster      True        False         5m
#
# NAME                       AVAILABLE   PROGRESSING   AGE
# project.my-project         True        False         2m
#
# NAME                        AVAILABLE   PROGRESSING   AGE
# branch.main-branch          True        False         1m
#
# NAME                BRANCH         TYPE         PHASE   HOST   AGE
# endpoint.main-ep    main-branch    read_write   active         1m
```

#### 11.2 查看 Operator 日志

```bash
kubectl logs -n neon deployment/neon-controller-manager -f
```

#### 11.3 Prometheus 指标

Operator 在 `:8443/metrics` 暴露基础 Prometheus 指标（HTTPS），可以配置 ServiceMonitor 接入 Prometheus 采集。对应的 Service 为 `neon-controller-manager-metrics-service`。

#### 11.4 健康检查

```bash
curl http://<operator-pod-ip>:8081/healthz
curl http://<operator-pod-ip>:8081/readyz
```

---

### 十二、清理

资源的删除顺序和创建顺序相反（因为存在依赖关系）：

```bash
# 1. 删除 Endpoint（先删除计算节点）
kubectl delete endpoint main-endpoint-rw main-endpoint-ro -n neon

# 2. 删除 Branch
kubectl delete branch main-branch -n neon

# 3. 删除 Project
kubectl delete project my-project -n neon

# 4. 删除 Pageserver 和 Safekeeper
kubectl delete pageserver pageserver-0 -n neon
kubectl delete safekeeper safekeeper-1 safekeeper-2 safekeeper-3 -n neon

# 5. 删除 Cluster（级联删除 JWT Secret、StorageController、StorageBroker）
kubectl delete cluster my-cluster -n neon
```

---

### 十三、完整示例：一键部署脚本

将所有资源写入同一个目录后，可以批量应用：

```bash
# 按依赖顺序执行
kubectl apply -f 00-namespace.yaml        # 创建 neon namespace（如果需要）
kubectl apply -f 01-secrets.yaml          # bucket-creds + storcon-db
kubectl apply -f 02-cluster.yaml          # Cluster
kubectl apply -f 03-storage.yaml          # Pageserver + Safekeeper
kubectl apply -f 04-project.yaml          # Project
kubectl apply -f 05-branch.yaml           # Branch
kubectl apply -f 06-endpoint.yaml         # Endpoint(s)

# 等待全部就绪
kubectl wait --for=condition=Available cluster my-cluster -n neon --timeout=120s
kubectl wait --for=condition=Available project my-project -n neon --timeout=120s
kubectl wait --for=condition=Available branch main-branch -n neon --timeout=120s
```

---

### 十四、当前限制

| 限制                                    | 影响                                                           |
| --------------------------------------- | -------------------------------------------------------------- |
| **Compute 与 Branch 解耦**        | Compute Pod 不再由 Branch CR 自动创建，需额外创建 Endpoint CR  |
| **read_write Endpoint 最多 1 个** | 每个 Branch 只能有 1 个可写计算节点                            |
| **无自动扩缩容**                  | Pageserver 数量可配置但无 HPA 动态伸缩                         |
| **无分支编排工作流**              | 分支创建通过 CR 管理，无内置的 CI/CD 分支工作流                |
| **Only dev 环境**                 | 尚未完成 Day 2 运维和性能优化（注意：最新代码已移除 `--dev`，启用 JWT 认证） |
| **无 Serverless 暂停/恢复**       | Endpoint 不支持闲置自动暂停，计算节点常驻运行                  |

---

## 前置工作清单

### 1. 外部 PostgreSQL 数据库（强制）

StorageController **不是 operator 创建的**，而是需要一个外部 PostgreSQL 来存元数据：

```44:44:api/v1alpha1/cluster_types.go
	// Must have a field named "uri"
	StorageControllerDatabaseSecret *corev1.SecretKeySelector `json:"storageControllerDatabaseSecret"`
```

需要先创建包含 `uri` 字段的 Secret：

```bash
kubectl create secret generic storage-controller-db \
  --from-literal=uri="postgresql://user:pass@host:5432/dbname"
```

代码中也有 CNPG 示例（`yaml/cnpg.yaml`），可快速部署一个本地 PG。

### 2. S3 对象存储凭证（强制）

```40:40:api/v1alpha1/cluster_types.go
	BucketCredentialsSecret *corev1.SecretReference `json:"bucketCredentialsSecret"`
```

```bash
kubectl create secret generic bucket-creds \
  --from-literal=AWS_ACCESS_KEY_ID=xxx \
  --from-literal=AWS_SECRET_ACCESS_KEY=xxx
```

Pageserver 也会读取这个 Secret 注入 ConfigMap。

### 3. Kubernetes 集群（强制）

- K8s >= 1.28（推荐 1.31+）
- 建议支持 NVMe 的 PVC StorageClass

### 4. 安装 Operator 和 CRD（强制）

```bash
make install          # 安装 CRD
make docker-build     # 构建镜像
make deploy           # 部署 operator
```

---

## 完整启动顺序

```
步骤1: 创建 Namespace      ──► kubectl create ns neon
步骤2: 外部 PostgreSQL    ──► kubectl apply -f yaml/cnpg.yaml
步骤3: Secret              ──► bucket-creds + storage-controller-db
步骤4: 安装 Operator       ──► make install && make deploy
步骤5: 创建 Cluster CR     ──► 自动创建 StorageController/Broker/JWT Secret + Pageserver/Safekeeper
步骤6: 创建 Project CR     ──► 注册 Tenant
步骤7: 创建 Branch CR      ──► 创建 Timeline（支持从父分支 fork / PITR）
步骤8: 创建 Endpoint CR    ──► 启动 Compute Pod + Service（read_write / read_only）
```

前置条件总结：**K8s 集群 + 外部 PostgreSQL + S3 对象存储 + 两个 Secret（DB URI + S3 凭证）**。

---

### 十五、连接数据库

#### 15.1 获取连接信息

```bash
# 端口转发访问 controlplane API
kubectl port-forward -n neon svc/neon-controlplane 8081:8081 &

# 创建项目并获取连接信息
curl -s -X POST "http://localhost:8081/api/v2/projects" \
  -H "Content-Type: application/json" \
  -d '{
    "project": {
      "name": "my-app",
      "pgVersion": 17,
      "cluster": "my-cluster",
      "branch": {
        "name": "main",
        "role_name": "appuser",
        "database_name": "neondb"
      },
      "endpoint": {
        "type": "read_write",
        "resources": { "cpu": "500m", "memory": "512Mi" }
      },
      "default_endpoint_settings": {
        "resources": { "cpu": "500m", "memory": "512Mi" }
      }
    }
  }' | python3 -m json.tool
```

响应中 `password` 字段即为 PostgreSQL 实际密码（已修复 Secret 命名一致性问题，密码可立即使用）。

#### 15.2 通过 NodePort 连接

```bash
# 查看 NodePort
kubectl get svc -n neon -l molnett.org/component=compute-postgres

# 连接（示例：NodeIP=192.168.232.128, Port=31116）
PGPASSWORD=<password> psql -U appuser -h 192.168.232.128 -p 31116 -d neondb
```

#### 15.3 通过 port-forward 连接

```bash
# 端口转发（Endpoint 名称从创建响应获取，如 ep-47362951）
kubectl port-forward -n neon svc/endpoint-<ep-id>-postgres 5432:55433 &

# 本地连接
PGPASSWORD=<password> psql -U appuser -h localhost -p 5432 -d neondb
```

#### 15.4 在 Pod 内直接连接

```bash
POD=$(kubectl get pods -n neon -l molnett.org/component=compute-postgres -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n neon $POD -- /bin/sh -c "PGPASSWORD=<password> psql -U appuser -d neondb -p 55433"
```

#### 15.5 密码管理

- **API 返回的密码仅一次可见**，请妥善保存
- 可通过 API 重置：`POST .../roles/{role_name}/reset_password`
- 密码以 SCRAM-SHA-256 格式存储在 PostgreSQL 中，安全性高
- K8s Secret 命名：`role-{branch_id}-{role_name}-password`

