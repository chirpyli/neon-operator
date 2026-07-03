# Neon Control Plane & Operator 架构设计方案

> 基于对 `neon-operator`、上游 `neon` 源码 (Pageserver/Safekeeper/Compute/Storage Controller) 及 Neon 生产 API (api-v2) 的完整分析

| 字段 | 内容 |
|------|------|
| 版本 | v3.3 |
| 日期 | 2026-07-01 |
| 产出 | `/home/postgres/works/opensource/neon-operator/docs/design/control-plane-architecture.md` |
| 变更 | 新增 5.2.4 Compute Pod 优雅终止设计：exec PID 1、preStop pg_ctl、terminationGracePeriodSeconds: 60 |

> **设计约束**: 暂不考虑 autoscaling (无 CU 弹性扩缩)，暂不考虑 serverless (无自动休眠/唤醒)。固定资源分配模式。为未来 serverless 预留接口和迁移路径。

---

## 一、上游 Neon 生产架构概览

### 1.1 核心组件分层

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Neon Cloud (SaaS)                           │
│                                                                     │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────────────────┐ │
│  │   Console     │  │   Proxy      │  │   Endpoint Storage        │ │
│  │ (控制面 API)  │  │ (PG协议路由) │  │   (LFC 预热/卸载)        │ │
│  └──────┬───────┘  └──────┬───────┘  └───────────────────────────┘ │
│         │                 │                                         │
│  ┌──────▼─────────────────▼──────────────────────────────────────┐ │
│  │                    Storage Controller                          │ │
│  │  • 租户 → Pageserver Shard 映射                                │ │
│  │  • Timeline 生命周期管理                                        │ │
│  │  • 节点注册/调度/健康管理                                        │ │
│  │  • ComputeHook: /notify-attach → Console                       │ │
│  │  • Reconciler: intent → observed 持续调和                       │ │
│  └──────┬─────────────────────────────────────────────────────────┘ │
│         │                                                            │
│  ┌──────▼─────────────────────────────────────────────────────────┐ │
│  │                    Storage Broker (gRPC pub-sub)                │ │
│  │  • Safekeeper ↔ Pageserver 消息路由                              │ │
│  └──────┬──────────────────────────┬───────────────────────────────┘ │
│         │                          │                                  │
│  ┌──────▼──────┐            ┌──────▼──────────┐                      │
│  │  Pageserver │  ...xN     │   Safekeeper    │  ...xN (≥3)          │
│  │  (存储引擎) │            │   (WAL 持久化)  │                      │
│  └─────────────┘            └─────────────────┘                      │
│                                                                      │
│  ┌──────────────┐                                                     │
│  │ Compute Node │  ...xN (每个 Endpoint 至少 1 个)                    │
│  │ (无状态 PG)  │                                                     │
│  └──────────────┘                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 1.2 Neon 生产 API (api-v2) 完整端点

Neon 的生产 API 基础 URL: `https://console.neon.tech/api/v2/`，采用 Bearer Token 认证。

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/v2/projects` | 创建项目（含初始分支、角色、数据库） |
| `GET` | `/api/v2/projects` | 列出项目 |
| `GET` | `/api/v2/projects/{project_id}` | 获取项目详情 |
| `PATCH` | `/api/v2/projects/{project_id}` | 更新项目 |
| `DELETE` | `/api/v2/projects/{project_id}` | 删除项目 |
| `POST` | `/api/v2/projects/{project_id}/branches` | 创建分支（可选附带 endpoint） |
| `GET` | `/api/v2/projects/{project_id}/branches` | 列出分支 |
| `GET` | `/api/v2/projects/{project_id}/branches/{branch_id}` | 获取分支详情 |
| `PATCH` | `/api/v2/projects/{project_id}/branches/{branch_id}` | 更新分支 |
| `DELETE` | `/api/v2/projects/{project_id}/branches/{branch_id}` | 删除分支 |
| `POST` | `/api/v2/projects/{project_id}/endpoints` | 创建计算端点 |
| `GET` | `/api/v2/projects/{project_id}/endpoints` | 列出端点 |
| `GET` | `/api/v2/projects/{project_id}/endpoints/{endpoint_id}` | 获取端点详情 |
| `PATCH` | `/api/v2/projects/{project_id}/endpoints/{endpoint_id}` | 更新端点 |
| `DELETE` | `/api/v2/projects/{project_id}/endpoints/{endpoint_id}` | 删除端点 |
| `POST` | `/api/v2/projects/{project_id}/branches/{branch_id}/roles` | 创建角色 |
| `GET` | `/api/v2/projects/{project_id}/branches/{branch_id}/roles` | 列出角色 |
| `DELETE` | `/api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}` | 删除角色 |
| `POST` | `/api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}/reset_password` | 重置角色密码 |
| `POST` | `/api/v2/projects/{project_id}/branches/{branch_id}/databases` | 创建数据库 |
| `GET` | `/api/v2/projects/{project_id}/branches/{branch_id}/databases` | 列出数据库 |
| `PATCH` | `/api/v2/projects/{project_id}/branches/{branch_id}/databases/{database_name}` | 更新数据库 |
| `DELETE` | `/api/v2/projects/{project_id}/branches/{branch_id}/databases/{database_name}` | 删除数据库 |
| `GET` | `/api/v2/projects/{project_id}/operations` | 查询操作状态 |
| `GET` | `/api/v2/projects/{project_id}/operations/{operation_id}` | 获取操作详情 |

### 1.3 Neon 生产 API 关键 Schema

#### POST `/api/v2/projects` — 创建项目

```json
// 请求体
{
  "project": {
    "name": "my-project",              // 必填, 1-256 char
    "region_id": "aws-us-east-2",       // 可选, 区域
    "pg_version": 16,                   // 可选, 14-18, 默认 17
    "provisioner": "k8s-pod",          // 可选
    "branch": {                          // 可选, 初始分支
      "name": "main",                   // 可选, 默认 "main"
      "role_name": "myapp_owner",       // 可选, 默认 "{db}_owner"
      "database_name": "neondb"         // 可选, 默认 "neondb"
    },
    "default_endpoint_settings": {
      "resources": {                     // 可选, 默认端点计算资源
        "cpu": "1",
        "memory": "2Gi"
      }
    },
    "settings": {
      "allowed_ips": {"primary_branch_only": false, "ips": ["0.0.0.0/0"]}
    }
  }
}

// 响应体 201
{
  "project": { "id": "...", "name": "...", "pg_version": 16, "proxy_host": "...", ... },
  "connection_uris": [{ "connection_uri": "postgresql://...", "connection_parameters": {...} }],
  "roles": [{ "name": "myapp_owner", "password": "...", "protected": false }],
  "databases": [{ "id": 1, "name": "neondb", "owner_name": "myapp_owner" }],
  "operations": [{ "id": "...", "action": "create_project", "status": "running" }]
}
```

#### POST `/api/v2/projects/{project_id}/branches` — 创建分支

```json
// 请求体
{
  "branch": {
    "parent_id": "br-parent",           // 父分支 ID, 空则从默认分支
    "name": "feature-x",                // 可选, 1-256 char
    "parent_lsn": "0/12345678",         // 可选, 指定分支点 LSN
    "parent_timestamp": "2024-01-01T00:00:00Z",  // 可选, 时间点分支
    "protected": false,
    "init_source": "parent-data"        // "parent-data" | "schema-only"
  },
  "endpoints": [
    {
      "type": "read_write",             // read_write | read_only
      "resources": {                     // 可选, 计算资源
        "cpu": "1",
        "memory": "2Gi"
      }
    }
  ]
}

// 响应体 201
{
  "branch": {
    "id": "br-xxx", "name": "feature-x", "project_id": "...",
    "parent_id": "br-parent", "current_state": "init",
    "created_at": "...", ...
  },
  "endpoints": [{
    "id": "ep-xxx", "host": "ep-xxx.us-east-2.aws.neon.tech",
    "branch_id": "br-xxx", "type": "read_write",
    "current_state": "init"
  }],
  "operations": [{ "id": "...", "action": "create_branch", "status": "scheduling" }]
}
```

#### POST `/api/v2/projects/{project_id}/endpoints` — 创建端点

```json
// 请求体
{
  "endpoint": {
    "branch_id": "br-xxx",              // 必填
    "type": "read_write",               // read_write | read_only
    "resources": {                       // 可选, 计算资源
      "cpu": "1",
      "memory": "2Gi"
    },
    "provisioner": "k8s-pod",
    "disabled": false
  }
}

// 响应体 201
{
  "endpoint": {
    "id": "ep-xxx", "host": "ep-xxx.region.neon.tech",
    "branch_id": "br-xxx", "type": "read_write",
    "current_state": "init", "region_id": "..."
  },
  "operations": [{ "id": "...", "action": "start_compute", "status": "running" }]
}
```

#### POST `/api/v2/projects/{project_id}/branches/{branch_id}/roles` — 创建角色

```json
// 请求体
{
  "role": {
    "name": "app_user",
    "authentication_method": "password"  // "password" | "oauth" | "no_login"
  }
}

// 响应体 201
{
  "role": {
    "name": "app_user", "password": "...",
    "protected": false, "branch_id": "...",
    "created_at": "..." , "authentication_method": "password"
  }
}
```

### 1.4 上游关键设计特征

| 特征 | 说明 |
|------|------|
| **异步操作** | Project/Branch/Endpoint 创建均触发异步 Operation，返回 `operations[]` 数组，状态为 `scheduling/running/finished/failed` |
| **Endpoint ≠ Compute** | Endpoint 是用户可见的"连接入口"，背后映射到一个 Compute Pod；一个 Branch 可有多个 Endpoint |
| **Branch 层次** | Branch 有 `parent_id` + `parent_lsn`/`parent_timestamp` 支持按 LSN 或时间点创建分支 |
| **分支类型** | `parent-data`（默认，复制全量数据）或 `schema-only`（仅复制 schema） |
| **只读副本** | 通过 `type: "read_only"` 的 Endpoint 实现 |
| **角色认证** | 支持 `password` / `oauth` / `no_login` 三种认证方式 |
| **固定资源** | 当前设计去掉了 CU 弹性伸缩，采用固定 CPU/Memory 资源分配 |

### 1.5 Neon 存储层深度架构

#### 1.5.1 Pageserver 分层存储 (Layered Repository)

Neon 的核心创新是 **WAL → Layer File → S3** 的分层存储模型：

```
              用户写入
                 │
                 ▼
┌──────────────────────────────────────┐
│          Compute Node                │
│  WAL 生成 → 推送至 Safekeeper        │
└──────────────┬───────────────────────┘
               │ WAL 流 (Safekeeper Pull/Push)
               ▼
┌──────────────────────────────────────┐
│     Safekeeper (≥3, Quorum)          │
│  WAL 暂存/冗余                       │
│  Commit LSN = Majority 确认          │
└──────────────┬───────────────────────┘
               │ WAL Replication (PG Protocol)
               ▼
┌──────────────────────────────────────┐
│         Pageserver                   │
│                                      │
│  In-Memory Layer (热点 WAL)          │
│       │                              │
│       ▼ Freeze + Flush                │
│  L0 Delta Layer (磁盘, 短 LSN 范围)  │
│       │                              │
│       ▼ Compaction                    │
│  L1 Image/Delta Layer (磁盘, 宽 LSN) │
│       │                              │
│       ▼ Upload                        │
│  S3/对象存储 (Remote Storage)        │
│                                      │
│  Page Service: GetPage@LSN ← Compute │
└──────────────────────────────────────┘
```

**层文件类型**（定义于 `docs/pageserver-storage.md`）：

| 层类型 | 位置 | 描述 |
|--------|------|------|
| **In-Memory Layer** | 内存 | 当前接收的 WAL，未冻结 |
| **L0 Delta Layer** | 本地磁盘 | 冻结后的 WAL 增量，覆盖全 key space，窄 LSN 范围 |
| **L1 Image Layer** | 本地磁盘 + S3 | 某 LSN 上某 key 范围的快照 |
| **L1 Delta Layer** | 本地磁盘 + S3 | 某 key 范围某 LSN 区间的增量 |

**Copy-on-Write 分支存储**:
- 每个 Timeline 对应于一个分支
- 子 Timeline 的 `ancestor_timeline` 指向父 Timeline，`ancestor_lsn` 记录分支点
- 子 Timeline **仅存储变更部分** 的层文件
- 未修改的数据从祖先 Timeline 逐级回溯读取
- 物理路径: `.neon/tenants/<tenant_id>/timelines/<timeline_id>/`

#### 1.5.2 WAL 完整链路

```
1. Client 执行 INSERT/UPDATE/DELETE
      │
2. Compute Node (Postgres) 生成 WAL 记录
      │
3. Compute 将 WAL 推送到 Safekeeper (WAL Sender/PG流复制协议)
      │
4. Safekeeper 多数派 (Quorum) 确认提交
      │  └─ Commit LSN = Majority 写入成功的 LSN
      │
5. Pageserver 通过 WAL Receiver (PG流复制协议) 从 Safekeeper 拉取 WAL
      │
6. Pageserver 将 WAL 写入 In-Memory Layer
      │
7. 当 In-Memory Layer 达到 checkpoint_distance (默认 256MB):
      │  └─ 冻结 → Flush 到磁盘 L0 Delta Layer → Safekeeper 截断已处理 WAL
      │
8. 后台 Compaction: L0 → L1 合并 (减少文件数, 扩大 key/LSN 范围)
      │
9. 后台 Upload: L1 文件上传到 S3/对象存储
      │
10. GC: 删除已上传且超过保留期的本地 L1 文件
```

**关键 LSN 位点**:
- `last_record_lsn`: Timeline 最新 WAL 位置（持续前进）
- `disk_consistent_lsn`: 已持久化到磁盘的最小 LSN 位点
- `remote_consistent_lsn`: 已上传到 S3 的最小 LSN 位点

#### 1.5.3 Compute 启动流程 (compute_ctl)

来自 `compute_tools/src/compute.rs` 的完整源码分析：

```
compute_ctl 启动
  │
  ├─1. 读取 Spec (SPEC_JSON env / CLI -S / HTTP Control Plane API)
  │     ComputeSpec 包含: tenant_id, timeline_id, pageserver_conn_info,
  │                        safekeeper_connstrings, storage_auth_token,
  │                        cluster.roles, cluster.databases, mode
  │
  ├─2. 连接 Pageserver 获取 Basebackup
  │     ├─ gRPC: pageserver_page_api::get_base_backup(tenant, timeline, lsn)
  │     └─ Libpq: psql basebackup tenant_id timeline_id [lsn] --gzip
  │     解压到 PGDATA 目录
  │
  ├─3. 验证 Safekeeper 同步 (Primary 模式)
  │     ├─ check_safekeepers_synced() → 快速 quorum ping
  │     └─ 否则 postgres --sync-safekeepers 等待 WAL 追上
  │
  ├─4. 生成 postgresql.conf
  │     ├─ neon.safekeepers = <safekeeper_connstrings>
  │     ├─ primary_conninfo = <safekeeper connstrings, 选 leader>
  │     └─ shared_preload_libraries = 'neon'
  │
  ├─5. 生成 pg_hba.conf
  │     └─ host all all all md5  (兼容 SCRAM-SHA-256)
  │
  ├─6. 启动 Postgres 进程
  │     └─ postgres -D <pgdata> -p <port> ...
  │
  └─7. compute_ctl 持续运行, 监控 PG 状态
        └─ Admin API: GET /status, POST /configure (热重载 roles/databases)
```

#### 1.5.4 Storage Controller (SC) 职责

来自 `storage_controller/src/` 的完整分析：

| 职责 | 说明 |
|------|------|
| **Tenant → Shard 映射** | 创建 tenant 时决定 shard 数量和分布 |
| **Node 注册/发现** | Pageserver/Safekeeper 启动时调用 `POST /control/v1/node` 注册 |
| **调度** | 根据 AZ、资源利用率选择合适的 Pageserver 承载 shard |
| **Generation 管理** | 处理 Pageserver 故障转移，协调新旧 generation 切换 |
| **Reconciler** | 持续调和 `intent`（期望状态）→ `observed`（实际状态） |
| **Compute Hook** | `/notify-attach` 回调通知 Console Compute 已挂载到 Pageserver |

**核心 API 端点**:

| 方法 | 路径 | 用途 |
|------|------|------|
| `POST` | `/v1/tenant` | 创建租户 |
| `POST` | `/v1/tenant/{id}/timeline` | 创建 Timeline (Branch from parent) |
| `PUT` | `/v1/tenant/{id}/location_config` | 设置租户存储位置配置 |
| `POST` | `/control/v1/node` | Pageserver/Safekeeper 节点注册 |
| `GET` | `/control/v1/tenant/{id}` | 获取租户详细信息 |

---

## 二、当前 neon-operator 架构分析

### 2.1 已实现的 CRD 和控制器

| CRD | 层级 | 职责 | 完整度 |
|-----|------|------|:---:|
| **Cluster** | 基础设施 | 管理 SC/Broker Deployment、JWT Secret、Safekeeper/Pageserver CR 自动创建 | 90% |
| **Pageserver** | 基础设施 | StatefulSet + ConfigMap + Service + PDB + SC 节点同步 | 85% |
| **Safekeeper** | 基础设施 | StatefulSet + Service + PDB + SC 注册 | 70% |
| **Project** | 逻辑资源 | 生成 TenantID → 调用 SC API 创建 tenant | 40% |
| **Branch** | 逻辑资源 | 生成 TimelineID → 调用 SC API 创建 timeline → 创建 Compute Deployment | 35% |

### 2.2 当前 Compute Node 暴露方式

当前每个 Branch 创建 **两个** Kubernetes Service：

| Service | 类型 | 端口 | 用途 |
|---------|------|------|------|
| `{branch}-admin` | ClusterIP | 3080 | compute_ctl HTTP API（/status, /configure, /metrics） |
| `{branch}-postgres` | 可配 (ClusterIP/NodePort/LoadBalancer) | 55433 | PostgreSQL 客户端连接 |

暴露配置通过 `Cluster.Spec.PostgresExposure` 控制：

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
spec:
  postgresExposure:
    type: LoadBalancer           # ClusterIP | NodePort | LoadBalancer
    externalTrafficPolicy: Local
    loadBalancerSourceRanges:
      - "10.0.0.0/8"
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-type: nlb
```

### 2.3 功能缺失分析

#### 🔴 P0 阻断

| 缺失项 | 说明 |
|--------|------|
| **缺少 Branch-from-Branch** | `BranchSpec` 无 `parentBranch` 字段，SC API 调用永远走 Bootstrap 模式 |
| ~~**缺少 Endpoint CRD**~~ | ✅ **已实现 (2026-06-26)** — Branch 与 Compute 解耦，Endpoint 独立管理计算实例。支持 `read_write`/`read_only` 类型、WAL proposer 区分、每 Branch 最多 1 个 read_write 校验 |
| **缺少 Role/User 管理** | ComputeSpec 的 `cluster.roles` 硬编码为 `[]` |
| **缺少 Database 管理** | ComputeSpec 的 `cluster.databases` 硬编码为 `[]` |
| **SC --dev 模式** | 无 JWT 认证，API 完全开放 |

#### 🟡 P1 重要

| 缺失项 | 说明 |
|--------|------|
| **无 Control Plane API** | 用户只能通过 `kubectl apply` 创建 K8s CR，无不面向用户的 REST API |
| **Compute 资源不可配** | Deployment resources 硬编码 |
| **ComputeSpec 韧性** | SC 不可达时 compute_ctl 可能启动失败 |
| **无多租户隔离** | 所有 Project/Branch 在同一 Namespace |
| **无 Operation 追踪** | 无异步操作状态查询机制 |

---

## 三、目标架构设计

### 3.1 设计原则

1. **参照 Neon 生产 API (api-v2)**：Control Plane 暴露的 REST API 与 Neon Cloud API 保持一致
2. **Proxy 预留，暂不实现**：当前阶段计算节点直接对外暴露，通过预留的 Proxy CRD 和接口为未来平滑引入做准备
3. **计算节点直接暴露**：每个 Endpoint 创建独立的 Postgres Service (LoadBalancer)，用户直接连接
4. **CRD 作为内部基础设施层**：Project/Branch CRD 由 Control Plane 内部管理，不直接暴露给用户

### 3.2 目标分层架构

```
                                   Internet / VPN
                                        │
                    ┌───────────────────┼───────────────────┐
                    │                   ▼                   │
                    │   ┌───────────────────────────┐      │
                    │   │  Control Plane API        │      │
                    │   │  (REST api-v2)            │      │
                    │   │  • Bearer Token 认证      │      │
                    │   │  • Project / Branch /     │      │
                    │   │    Endpoint / Role CRUD   │      │
                    │   │  • Operation 追踪         │      │
                    │   │  • 状态聚合               │      │
                    │   └─────────┬─────────────────┘      │
                    │             │                         │
                    │             ├─ 管理 CRD (Project,     │
                    │             │   Branch, Endpoint)     │
                    │             │                         │
                    │             ▼                         │
    ┌───────────────┼──────────────────────────────────────┼───────────┐
    │               │       K8s Cluster                    │           │
    │               │                                      │           │
    │               │  ┌───────────────────────────┐      │           │
    │               │  │  Operator Manager         │      │           │
    │               │  │  (Project/Branch/         │      │           │
    │               │  │   Endpoint Controllers)   │      │           │
    │               │  └───────────────────────────┘      │           │
    │               │                                      │           │
    │               │  ┌───────────────────────────┐      │           │
    │               │  │  Storage Controller Pod   │      │           │
    │               │  └───────────────────────────┘      │           │
    │               │                                      │           │
    │               │  ┌────────────┐  ┌────────────┐     │           │
    │               │  │ Storage    │  │ Safekeeper │     │           │
    │               │  │ Broker     │  │ x3         │     │           │
    │               │  └────────────┘  └────────────┘     │           │
    │               │                                      │           │
    │               │  ┌────────────────────────────┐     │           │
    │  ┌────────────┼──│  Pageserver x3+            │     │           │
    │  │            │  └────────────────────────────┘     │           │
    │  │            │                                      │           │
    │  │            │  ┌──────────┐  ┌──────────┐        │           │
    │  │            │  │ Compute  │  │ Compute  │  ...   │           │
    │  │            │  │ Pod A    │  │ Pod B    │        │           │
    │  │            │  └────┬─────┘  └────┬─────┘        │           │
    │  │            │       │ LB:55433    │ LB:55433      │           │
    │  │            │       ▼             ▼                │           │
    └──┼────────────┴─────────────────────────────────────┘
       │
       │  用户通过 LoadBalancer 直接连接:
       │    psql -h <lb-host> -p 55433 -U <role> -d <db>
       │
       │  [预留] 未来引入 Proxy 后:
       │    psql -h <proxy-host> -p 5432 -U <role>
       │    Proxy SNI 路由到正确 Compute
       │
```

### 3.3 关键设计决策：无 Proxy 阶段的网络暴露

**当前无 Proxy 时，每个 Endpoint 的计算节点独立对外暴露：**

```
用户 → LoadBalancer:55433 (Endpoint A 的 Postgres Service)
      → Compute Pod A → Pageserver (page_service)
                       → Safekeeper (WAL)

用户 → LoadBalancer:55434 (Endpoint B 的 Postgres Service)
      → Compute Pod B → Pageserver (page_service)
                       → Safekeeper (WAL)
```

**预留 Proxy 引入路径：**
- Cluster CR 新增 `proxyConfig` 字段（可选，当前不激活）
- Proxy CRD 预留定义（代码中注册但不部署）
- 每个 Compute 保留 Admin Service (ClusterIP:3080)，为 Proxy 提供 `POST /configure` 推送入口
- 当 Proxy 引入后，Postgres Service 切换为 ClusterIP，由 Proxy 统一路由

---

## 四、Control Plane API 详细设计

### 4.1 基础信息

| 项目 | 值 |
|------|-----|
| Base URL | `http://<control-plane-host>:8080/api/v2/` |
| 认证 | `Authorization: Bearer <api_key>` |
| Content-Type | `application/json` |
| 异步模型 | 写操作返回 `operations[]`，状态为 `scheduling → running → finished/failed` |

### 4.2 完整 API 端点定义

#### 4.2.1 项目管理

**POST `/api/v2/projects`** — 创建项目（= 创建 Tenant + 初始 Branch + 初始 Endpoint）

```
请求:
{
  "project": {
    "name": "my-app-db",                    // 必填, 1-256 char
    "pg_version": 16,                       // 可选, 默认 17
    "cluster": "my-cluster",                // 可选, 指定技术集群 (对应 K8s Cluster CR)
    "branch": {                              // 可选, 初始分支配置
      "name": "main",                        // 可选, 默认 "main"
      "role_name": "myapp_owner",            // 可选
      "database_name": "neondb"              // 可选, 默认 "neondb"
    },
    "endpoint": {                            // 可选, 初始端点配置
      "type": "read_write",                  // read_write
      "resources": {                         // 可选, 计算资源配置
        "cpu": "1",
        "memory": "2Gi"
      },
      "exposure": {                          // 可选, 覆盖全局暴露策略
        "type": "LoadBalancer",
        "source_ranges": ["10.0.0.0/8"]
      }
    },
    "default_endpoint_settings": {           // 可选, 分支级端点默认值
      "resources": {                          // 默认计算资源
        "cpu": "0.5",
        "memory": "1Gi"
      }
    }
  }
}

响应 201:
{
  "project": {
    "id": "project-abc123",                  // 生成的项目 ID (K8s name 兼容)
    "name": "my-app-db",
    "pg_version": 16,
    "cluster": "my-cluster",
    "tenant_id": "...",                      // Neon tenant ID (内部)
    "created_at": "2026-06-26T12:00:00Z",
    "updated_at": "2026-06-26T12:00:00Z"
  },
  "branch": {
    "id": "br-xxx",                          // 初始分支 ID
    "name": "main",
    "project_id": "project-abc123",
    "parent_id": null,                       // 主分支无 parent
    "default": true,
    "current_state": "init",
    "created_at": "2026-06-26T12:00:00Z"
  },
  "endpoints": [{
    "id": "ep-xxx",
    "branch_id": "br-xxx",
    "type": "read_write",
    "host": "<lb-host>",                     // LoadBalancer 地址
    "port": 55433,
    "current_state": "init",
    "created_at": "2026-06-26T12:00:00Z"
  }],
  "connection_uris": [{
    "connection_uri": "postgresql://myapp_owner:<password>@<lb-host>:55433/neondb",
    "connection_parameters": {
      "host": "<lb-host>",
      "port": "55433",
      "database": "neondb",
      "role": "myapp_owner",
      "password": "..."
    }
  }],
  "roles": [{
    "name": "myapp_owner",
    "password": "...",
    "protected": false,
    "branch_id": "br-xxx"
  }],
  "databases": [{
    "id": 1,
    "name": "neondb",
    "owner_name": "myapp_owner",
    "branch_id": "br-xxx"
  }],
  "operations": [{
    "id": "op-uuid",
    "project_id": "project-abc123",
    "action": "create_project",
    "status": "scheduling",
    "created_at": "2026-06-26T12:00:00Z"
  }]
}
```

**GET `/api/v2/projects`** — 列出项目

```
响应 200:
{
  "projects": [
    {
      "id": "project-abc123",
      "name": "my-app-db",
      "pg_version": 16,
      "cluster": "my-cluster",
      "created_at": "2026-06-26T12:00:00Z"
    }
  ],
  "pagination": {
    "cursor": "next-cursor",
    "has_more": false
  }
}
```

**GET `/api/v2/projects/{project_id}`** — 获取项目详情

```
响应 200:
{
  "project": {
    "id": "project-abc123",
    "name": "my-app-db",
    "pg_version": 16,
    "tenant_id": "...",
    "cluster": "my-cluster",
    "default_endpoint_settings": { ... },
    "created_at": "2026-06-26T12:00:00Z",
    "updated_at": "2026-06-26T12:00:00Z"
  }
}
```

**DELETE `/api/v2/projects/{project_id}`** — 删除项目

```
响应 200:
{
  "project": { "id": "project-abc123", "name": "my-app-db", ... },
  "operations": [{ "id": "...", "action": "delete_project", "status": "scheduling" }]
}
```

#### 4.2.2 分支管理

**POST `/api/v2/projects/{project_id}/branches`** — 创建分支

```
请求:
{
  "branch": {
    "name": "feature-x",                     // 可选, 默认为生成名
    "parent_id": "br-parent",                // 可选, 父分支, 空=从默认分支创建
    "parent_lsn": "0/12345678",              // 可选, 指定分支点 LSN
    "parent_timestamp": "2026-06-25T00:00:00Z", // 可选, 时间点分支
    "init_source": "parent-data",            // 可选, "parent-data" | "schema-only"
    "protected": false                       // 可选
  },
  "endpoints": [                             // 可选, 创建同时创建端点
    {
      "type": "read_write",
      "resources": { "cpu": "0.5", "memory": "1Gi" },
      "exposure": { "type": "LoadBalancer" }
    }
  ]
}

响应 201:
{
  "branch": {
    "id": "br-feature-x",
    "project_id": "project-abc123",
    "parent_id": "br-parent",
    "name": "feature-x",
    "current_state": "init",
    "default": false,
    "init_source": "parent-data",
    "created_at": "..."
  },
  "endpoints": [{ "id": "ep-xxx", "type": "read_write", "host": "...", ... }],
  "operations": [{ "id": "...", "action": "create_branch", "status": "scheduling" }]
}
```

**GET `/api/v2/projects/{project_id}/branches`** — 列出分支

```
响应 200:
{
  "branches": [
    {
      "id": "br-xxx", "name": "main", "default": true,
      "current_state": "ready", "created_at": "..."
    },
    {
      "id": "br-feature-x", "name": "feature-x", "default": false,
      "parent_id": "br-xxx", "current_state": "ready"
    }
  ]
}
```

**GET `/api/v2/projects/{project_id}/branches/{branch_id}`** — 获取分支详情

```
响应 200:
{
  "branch": {
    "id": "br-feature-x",
    "project_id": "project-abc123",
    "parent_id": "br-xxx",
    "parent_lsn": "0/12345678",
    "name": "feature-x",
    "current_state": "ready",
    "default": false,
    "protected": false,
    "init_source": "parent-data",
    "created_at": "...",
    "updated_at": "..."
  }
}
```

**DELETE `/api/v2/projects/{project_id}/branches/{branch_id}`** — 删除分支

```
约束:
- 不能删除默认分支 (default=true)
- 不能删除有子分支的分支

响应 200:
{
  "branch": { "id": "br-feature-x", "name": "feature-x" },
  "operations": [{ "id": "...", "action": "delete_branch", "status": "scheduling" }]
}
```

#### 4.2.3 端点管理

**POST `/api/v2/projects/{project_id}/endpoints`** — 创建端点

```
请求:
{
  "endpoint": {
    "branch_id": "br-feature-x",             // 必填, 关联分支
    "type": "read_write",                    // read_write | read_only
    "resources": {                           // 可选, 计算资源
      "cpu": "1",
      "memory": "2Gi"
    },
    "disabled": false,                       // 可选, 是否禁用
    "exposure": {                            // 可选, 覆盖全局暴露策略
      "type": "LoadBalancer",
      "source_ranges": ["203.0.113.0/24"]
    }
  }
}

响应 201:
{
  "endpoint": {
    "id": "ep-readonly-xxx",
    "branch_id": "br-feature-x",
    "type": "read_only",
    "host": "<lb-host>",
    "port": 55433,
    "current_state": "init",
    "disabled": false,
    "created_at": "..."
  },
  "operations": [{ "id": "...", "action": "start_compute", "status": "scheduling" }]
}
```

**GET `/api/v2/projects/{project_id}/endpoints`** — 列出端点

```
响应 200:
{
  "endpoints": [
    {
      "id": "ep-xxx", "branch_id": "br-xxx", "type": "read_write",
      "host": "<lb-host>", "port": 55433,
      "current_state": "active", "created_at": "..."
    }
  ]
}
```

**DELETE `/api/v2/projects/{project_id}/endpoints/{endpoint_id}`** — 删除端点

```
响应 200:
{
  "endpoint": { "id": "ep-xxx", "type": "read_write" },
  "operations": [{ "id": "...", "action": "delete_endpoint", "status": "scheduling" }]
}
```

#### 4.2.4 角色管理

**POST `/api/v2/projects/{project_id}/branches/{branch_id}/roles`** — 创建角色

```
请求:
{
  "role": {
    "name": "app_readonly_user",             // 必填, 角色名
    "authentication_method": "password"      // 可选, 默认 "password"
  }
}

响应 201:
{
  "role": {
    "name": "app_readonly_user",
    "password": "<generated-password>",
    "protected": false,
    "branch_id": "br-feature-x",
    "authentication_method": "password",
    "created_at": "..."
  }
}
```

**DELETE `/api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}`** — 删除角色

**POST `/api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}/reset_password`** — 重置角色密码

```
响应 200:
{
  "role": {
    "name": "app_readonly_user",
    "password": "<new-password>",
    "branch_id": "br-feature-x"
  }
}
```

#### 4.2.5 数据库管理

**POST `/api/v2/projects/{project_id}/branches/{branch_id}/databases`** — 创建数据库

```
请求:
{
  "database": {
    "name": "analytics",                     // 必填, 数据库名
    "owner_name": "app_readonly_user"        // 可选, 默认创建分支的角色
  }
}

响应 201:
{
  "database": {
    "id": 2,
    "name": "analytics",
    "owner_name": "app_readonly_user",
    "branch_id": "br-feature-x",
    "created_at": "..."
  }
}
```

#### 4.2.6 操作追踪

**GET `/api/v2/projects/{project_id}/operations`** — 查询操作列表

```
响应 200:
{
  "operations": [
    {
      "id": "op-uuid",
      "project_id": "project-abc123",
      "branch_id": "br-xxx",                  // 可选
      "endpoint_id": "ep-xxx",                // 可选
      "action": "create_project",
      "status": "scheduling",
      "error": null,
      "created_at": "2026-06-26T12:00:00Z",
      "updated_at": "2026-06-26T12:00:05Z"
    }
  ]
}
```

**GET `/api/v2/projects/{project_id}/operations/{operation_id}`** — 获取操作详情

### 4.3 错误响应格式

所有 API 统一使用 `GeneralError` 格式：

```json
{
  "code": "BRANCH_NOT_FOUND",
  "message": "branch 'br-xxx' not found in project 'project-abc123'"
}
```

HTTP 状态码：
- `400` — 请求参数错误
- `401` — 未认证或 API Key 无效
- `403` — 无权限
- `404` — 资源不存在
- `409` — 冲突（如分支已有 read_write 端点）
- `423` — 资源锁定（操作进行中）
- `429` — 速率限制
- `503` — 服务不可用

---

## 五、Control Plane 内部实现

### 5.1 技术栈

| 组件 | 技术选型 |
|------|----------|
| API 框架 | Go + Gin / Echo |
| 认证 | API Key (SHA256 哈希存储) |
| 状态存储 | Kubernetes CRD (Project, Branch, Endpoint, Role, Database, Operation) |
| SC 通信 | HTTP (重用现有函数) |
| K8s 交互 | controller-runtime Client |

### 5.2 Project 创建全流程

```
POST /api/v2/projects                     // Control Plane API
  │
  ├─① 认证: 验证 Bearer Token
  │      └─ 解析 API Key → 获取用户/组织身份
  │
  ├─② 校验:
  │      ├─ name 格式: ^[a-z0-9-]{1,60}$
  │      ├─ pg_version: 14-18
  │      └─ 配额检查: 当前项目数 < 限制
  │
  ├─③ 生成 IDs:
  │      ├─ project_id = "project-{random}"
  │      ├─ tenant_id = neon.GenerateNeonID()  (Hex 格式)
  │      ├─ timeline_id = neon.GenerateNeonID()
  │      ├─ endpoint_id = "ep-{random}"
  │      ├─ branch_id = "br-{random}"
  │      └─ operation_id = uuid.New()
  │
  ├─④ 创建 Project CR (K8s):
  │      kind: Project
  │      spec:
  │        cluster: "my-cluster"
  │        tenantID: "{tenant_id}"
  │        pgVersion: 16
  │        name: "my-app-db"
  │
  ├─⑤ 创建 Branch CR (K8s):
  │      kind: Branch
  │      spec:
  │        projectID: "project-abc123"
  │        timelineID: "{timeline_id}"
  │        pgVersion: 16
  │        parentBranch: ""              // 主分支, 无 parent
  │        initSource: "parent-data"
  │
  ├─⑥ 创建 Endpoint CR (K8s):
  │      kind: Endpoint
  │      spec:
  │        branchID: "br-xxx"
  │        type: "read_write"
  │        resources: { cpu: "1", memory: "2Gi" }
  │        exposure: { type: "LoadBalancer" }
  │
  ├─⑦ 创建 Role CR (K8s):
  │      kind: Role                                        # 新增 CRD
  │      spec:
  │        branchID: "br-xxx"
  │        name: "myapp_owner"
  │        authenticationMethod: "password"
  │
  ├─⑧ 创建 Database CR (K8s):
  │      kind: Database                                    # 新增 CRD
  │      spec:
  │        branchID: "br-xxx"
  │        name: "neondb"
  │        ownerName: "myapp_owner"
  │
  ├─⑨ 创建 Operation CR (K8s):
  │      kind: Operation                                   # 新增 CRD
  │      spec:
  │        action: "create_project"
  │        status: "scheduling"
  │        projectID: "project-abc123"
  │
  └─⑩ 返回聚合响应 (201):
        project + branch + endpoints + roles +
        databases + connection_uris + operations
```

#### 5.2.1 Tenant 创建（Project Controller reconcile 触发）

```
ProjectReconciler.Reconcile()       // 检测到新 Project CR
  │
  ├─ Phase "creating_tenant":
  │    └─ PUT /v1/tenant/{tenant_id}/location_config
  │         body: {"mode": "AttachedSingle", "generation": 1, "tenant_conf": {}}
  │    └─ 更新 Project.Status.Phase = "active"
```

#### 5.2.2 Timeline 创建（Branch Controller reconcile 触发）

```
BranchReconciler.Reconcile()        // 检测到新 Branch CR
  │
  ├─ Phase "creating_timeline":
  │    ├─ 获取 Project → tenantID
  │    ├─ POST /v1/tenant/{tenant_id}/timeline
  │    │     body: {
  │    │       "new_timeline_id": "{timeline_id}",
  │    │       "ancestor_timeline_id": "{parent_timeline_id}",  // 有 parent 时
  │    │       "pg_version": 16
  │    │     }
  │    └─ 更新 Branch.Status.Phase = "active"
```

#### 5.2.3 Compute 创建（Endpoint Controller reconcile 触发）

```
EndpointReconciler.Reconcile()      // 检测到新 Endpoint CR
  │
  ├─ Phase "creating_compute":
  │    ├─ 获取 Branch, Project
  │    ├─ 构建 ComputeSpec:
  │    │    └─ 从 Endpoint CR 获取 type (read_write / read_only)
  │    │    └─ 从 Project+Branch CR 获取 tenant_id, timeline_id
  │    │    └─ 从 Safekeeper CR 获取连接信息
  │    │    └─ [Phase 2] 从 Role CR 获取 roles → cluster.roles
  │    │    └─ [Phase 2] 从 Database CR 获取 databases → cluster.databases
  │    │
  │    ├─ 创建 K8s 资源:
  │    │    ├─ ConfigMap ({endpoint}-compute-spec, 含初始 spec.json)
  │    │    ├─ Deployment (compute-node 容器)
  │    │    │    Label: molnett.org/endpoint-type={read_write|read_only}
  │    │    │    ├─ read_write → synchronous_standby_names=walproposer
  │    │    │    └─ read_only  → synchronous_standby_names 不设置
  │    │    ├─ AdminService (ClusterIP:3080, 控制通道)
  │    │    └─ PostgresService (type 取自 Endpoint.Spec.Exposure, port=55433)
  │    │
  │    └─ 等待 Compute Pod Ready
  │         └─ 更新 Endpoint.Status.Host = LB 地址
  │         └─ 更新 Endpoint.Status.Port = 55433
```

**WAL Proposer 架构说明**：

Neon 的 WAL proposer 机制确保同一 timeline 上只有一个 compute 实例负责 WAL 写入：

| Endpoint Type | compute_ctl Mode | WAL 角色 | `synchronous_standby_names` | WAL 消费方式 | 说明 |
|:---|:---|:---|:---|:---|:---|
| `read_write` | `Primary` | **WAL Proposer** | `walproposer` | walproposer → safekeeper | 唯一可写实例，向 Safekeeper 推送 WAL |
| `read_only` | `Replica` | **WAL Follower** | 不设置 | safekeeper → PostgreSQL 原生流复制 | hot standby，通过 `primary_conninfo` 从 safekeeper 拉取 WAL，不参与 proposer 选举 |

**Replica WAL 消费机制（参考 neon_local 的 `setup_pg_conf` ComputeMode::Replica 分支）**：

Replica compute 不使用 walproposer，而是通过 PostgreSQL 原生物理流复制从 safekeeper 消费 WAL。operator 自动为 `read_only` 端点配置以下 postgresql.conf 参数：

```
primary_conninfo = 'host=<sk_hosts> port=<sk_ports> options='-c timeline_id=<tid> tenant_id=<tid>' application_name=replica replication=true'
primary_slot_name = 'repl_<timeline_id>_'
hot_standby = on
```

这使得 Replica 端点启动后能通过 safekeeper 的 WAL 流实时跟随 Primary 的写入变更。

Mode 字段通过以下两条路径注入到 compute_ctl：
1. **初始 spec.json**（ConfigMap `endpoint-{name}-spec`）：`EndpointConfigMap()` 根据 `EndpointSpec.Type` 设置 mode
2. **运行时 spec**（`/configure` POST）：`GenerateComputeSpec()` 从 Deployment label `molnett.org/endpoint-type` 推断 mode

**为什么必须区分？**

如果两个 compute 实例同时配置 `synchronous_standby_names=walproposer`，会争夺同一 timeline 的 WAL proposer 角色。后启动者检测到 `basebackup LSN < flush LSN` 时 PANIC 退出。通过 Endpoint Type 驱动该配置，确保每个 timeline 只有一个 WAL proposer。

**唯一性约束**：Control Plane API (`createEndpointForBranch`) 在创建 `read_write` endpoint 前检查 Branch 是否已有 read_write endpoint，若有则返回 `READ_WRITE_ENDPOINT_EXISTS` 错误。对应 Neon 生产 API 的错误码 `409 Conflict`。

#### 5.2.4 Compute Pod 优雅终止设计

**问题**：

Endpoint Deployment 中容器通过 `bash -c "cmd1 && cmd2"` 启动，bash 作为 PID 1 不转发 K8s 发送的 SIGTERM 给子进程 `compute_ctl`/`postgres`。同时缺少 `terminationGracePeriodSeconds`（默认仅 30s）和 `preStop` 钩子，导致 Pod 删除时 PostgreSQL 可能未经 checkpoint 就被 SIGKILL 强制终止，造成：
- WAL 日志丢失
- 下次启动需要更长的 crash recovery 时间
- Pod 长时间卡在 `Terminating` 状态

**设计：三重防御**

```
K8s delete Pod
  │
  ├─ (1) preStop hook: pg_ctl stop -m fast -t 25
  │       └─ 主动触发 PostgreSQL 干净关闭（checkpoint + 回滚活跃事务）
  │       └─ 等待最长 25s，|| true 确保 pg 已死时不阻塞终止
  │
  ├─ (2) SIGTERM → PID 1 (compute_ctl)
  │       └─ exec /usr/local/bin/compute_ctl 替换 bash 成为 PID 1
  │       └─ compute_ctl 直接接收信号，不再被 bash 拦截
  │
  └─ (3) terminationGracePeriodSeconds: 60s
          └─ preStop 25s + SIGTERM 处理 35s，总计 60s 窗口
          └─ 与 pageserver 的 60s 保持一致，超时后 SIGKILL 兜底
```

**PodSpec 变更**（`specs/compute/endpoint_spec.go`、`specs/compute/deployment.go`）：

| 字段 | 变更前 | 变更后 | 说明 |
|:---|:---|:---|:---|
| `Command` | `... && /usr/local/bin/compute_ctl ...` | `... && exec /usr/local/bin/compute_ctl ...` | `exec` 让 compute_ctl 替换 bash 成为 PID 1 |
| `TerminationGracePeriodSeconds` | 未设置（默认 30s） | `60` | 给予 PostgreSQL checkpoint 足够时间 |
| `Lifecycle.PreStop` | 无 | `pg_ctl stop -D /.neon/data/pgdata -m fast -t 25` | 主动关闭 PostgreSQL |

**设计权衡**：保留 bash 包装（而非拆分为 initContainer），因为 `echo "$INITIAL_SPEC_JSON" > /var/spec.json` 是一次性初始化操作，一行 `exec` 即可解决问题，改动最小且不影响现有行为。

**pg_ctl 参数说明**：
- `-m fast`：回滚活跃事务 + 正常 checkpoint（比 `smart` 更快，比 `immediate` 更安全）
- `-t 25`：等待最长 25 秒（60s 宽限期内预留 35s 给 SIGTERM→SIGKILL 流程）
- 使用 `|| true` 兜底：PostgreSQL 已停止时 `pg_ctl` 返回非零，不应阻塞删除

### 5.3 Control Plane 服务自身部署

Control Plane API 作为 Kubernetes Deployment 部署：

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: neon-controlplane
  namespace: neon-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: neon-controlplane
  template:
    spec:
      serviceAccountName: neon-controlplane
      containers:
      - name: api
        image: neon-operator/controlplane:latest
        ports:
        - containerPort: 8080
        env:
        - name: KUBECONFIG
          value: ""
        - name: STORAGE_CONTROLLER_URL
          value: "http://my-cluster-storage-controller.neon:8080"
---
apiVersion: v1
kind: Service
metadata:
  name: neon-controlplane
  namespace: neon-system
spec:
  type: LoadBalancer
  ports:
  - port: 8080
    targetPort: 8080
  selector:
    app: neon-controlplane
```

### 5.4 密码安全处理

- Role 创建时由 Control Plane API 生成随机密码（`crypto/rand` 32 char 字母数字）
- 密码同时存入 K8s Secret（命名：`role-{branchID}-{roleName}-password`，与 Role Controller 保持一致）
- Role Controller 的 `ensurePasswordSecret` 检查 Secret 是否已存在：
  - 已存在 → 读取密码，计算 SCRAM-SHA-256 verifier → 更新 `Role.Status.EncryptedPassword`
  - 不存在（Controller 先于 API 运行）→ 自行生成密码存入 Secret
- SCRAM-SHA-256 实现位于 `utils/scram.go`，算法与上游 Neon (Rust) 一致：PBKDF2-HMAC-SHA256, 4096 iterations, 16-byte salt
- 密码仅在创建和重置时返回给 API 调用方，不通过 GET 接口返回
- Connection URI 中包含密码，一次性返回
- **重要**：Secret 命名必须与 Controller 一致，否则 Controller 会重新生成密码导致 API 返回的密码无效

---

## 六、新增/修改的 K8s CRD

### 6.1 修改清单

| CRD | 操作 | 变更 |
|-----|------|------|
| **Project** | 修改 | 增加 `name`, `defaultEndpointSettings` 字段 |
| **Branch** | 修改 | 增加 `parentBranch`, `parentLSN`, `parentTimestamp`, `initSource`, `protected` 字段 |
| **Endpoint** | ✅ **已实现** | 管理 Compute 节点生命周期和网络暴露的完整 CRD。实现要点见 5.2.3 |
| **Role** | 新增 | 管理 Postgres 角色，映射到 ComputeSpec.cluster.roles |
| **Database** | 新增 | 管理 Postgres 数据库，映射到 ComputeSpec.cluster.databases |
| **Operation** | 新增 | 追踪异步操作状态 |
| **Proxy** | 预留 | 代码定义但暂不注册 | Controller

### 6.2 CRD 类型定义

#### 6.2.1 Endpoint CRD（新增）

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type Endpoint struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec   EndpointSpec   `json:"spec"`
    Status EndpointStatus `json:"status,omitempty"`
}

type EndpointSpec struct {
    // BranchID 引用所属的 Branch
    // +required
    BranchID string `json:"branchID"`

    // Type 端点类型: read_write 或 read_only
    // 每个 Branch 最多一个 read_write 端点
    // +kubebuilder:validation:Enum=read_write;read_only
    // +required
    Type string `json:"type"`

    // Resources 计算资源请求 (固定资源, 无弹性扩缩)
    // +optional
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

    // Disabled 是否禁用连接
    // +optional
    Disabled bool `json:"disabled,omitempty"`

    // Exposure 覆盖 Cluster 级别的 PostgresExposure
    // 不设置时使用 Cluster.Spec.PostgresExposure
    // +optional
    Exposure *ServiceExposure `json:"exposure,omitempty"`

    // [未来 Serverless 预留]
    // SuspendTimeoutSeconds 无活动后暂停的超时秒数, -1=永不暂停
    // 当前阶段未实现, 计算节点常驻运行. Phase 4+ 启用.
    // +optional
    // SuspendTimeoutSeconds *int32 `json:"suspendTimeoutSeconds,omitempty"`
}

type EndpointStatus struct {
    // Phase 当前生命周期阶段
    // "creating_compute" → "starting" → "active" → "stopping" → "stopped"
    // [未来 Serverless] active → idle → suspended
    // +optional
    Phase string `json:"phase,omitempty"`

    // Host 对外连接地址 (LoadBalancer hostname 或 Node IP)
    // +optional
    Host string `json:"host,omitempty"`

    // Port PostgreSQL 端口
    // +optional
    Port int32 `json:"port,omitempty"`

    // Conditions 详细状态条件
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

#### 6.2.2 Role CRD（新增）

```go
type RoleSpec struct {
    // BranchID 所属分支
    BranchID string `json:"branchID"`

    // Name Postgres 角色名
    Name string `json:"name"`

    // AuthenticationMethod 认证方式
    // +kubebuilder:validation:Enum=password;oauth;no_login
    // +kubebuilder:default:=password
    AuthenticationMethod string `json:"authenticationMethod,omitempty"`
}

type RoleStatus struct {
    // PasswordSecretRef 密码所在的 K8s Secret 引用
    PasswordSecretRef *corev1.SecretReference `json:"passwordSecretRef,omitempty"`

    // Protected 是否为系统保护角色
    Protected bool `json:"protected,omitempty"`

    // Phase "active"
    Phase string `json:"phase,omitempty"`
}
```

#### 6.2.3 Database CRD（新增）

```go
type DatabaseSpec struct {
    // BranchID 所属分支
    BranchID string `json:"branchID"`

    // Name 数据库名
    Name string `json:"name"`

    // OwnerName 拥有者角色名
    OwnerName string `json:"ownerName,omitempty"`
}

type DatabaseStatus struct {
    Phase string `json:"phase,omitempty"`
}
```

#### 6.2.4 Operation CRD（新增）

```go
type OperationSpec struct {
    // Action 操作类型
    // create_project, delete_project, create_branch, delete_branch,
    // create_endpoint, delete_endpoint, reset_role_password, ...
    Action string `json:"action"`

    // Status 执行状态
    // scheduling, running, finished, failed
    Status string `json:"status"`

    // ProjectID 相关项目
    ProjectID string `json:"projectID"`

    // BranchID 相关分支, 可选
    BranchID string `json:"branchID,omitempty"`

    // EndpointID 相关端点, 可选
    EndpointID string `json:"endpointID,omitempty"`

    // Error 错误信息, failed 时填充
    Error string `json:"error,omitempty"`

    // FailuresCount 失败次数
    FailuresCount int32 `json:"failuresCount,omitempty"`
}

type OperationStatus struct {
    // CompletedAt 完成时间
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`

    // TotalDurationMs 总耗时(毫秒)
    TotalDurationMs int64 `json:"totalDurationMs,omitempty"`
}
```

#### 6.2.5 修改 Project CRD

```go
type ProjectSpec struct {
    // ClusterName 目标技术集群
    ClusterName string `json:"clusterName"`

    // TenantID Neon tenant ID, 自动生成, 不可修改
    TenantID string `json:"tenantID,omitempty"`

    // PGVersion PostgreSQL 版本
    PGVersion int `json:"pgVersion"`

    // +new Name 项目显示名称 (用于 API 响应)
    Name string `json:"name"`

    // +new DefaultEndpointSettings 新建端点默认配置 (固定资源)
    DefaultEndpointSettings *EndpointDefaults `json:"defaultEndpointSettings,omitempty"`
}

// EndpointDefaults 新建端点的默认资源配置
// [未来 Serverless] 可扩展 AutoscalingLimitMinCU/MaxCU
type EndpointDefaults struct {
    // Resources 默认计算资源
    // +optional
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}
```

#### 6.2.6 修改 Branch CRD

```go
type BranchSpec struct {
    ProjectID   string `json:"projectID"`
    TimelineID  string `json:"timelineID,omitempty"`
    PGVersion   int    `json:"pgVersion"`

    // +new ParentBranch 父分支名称, 为空=创建主分支
    ParentBranch string `json:"parentBranch,omitempty"`

    // +new ParentLSN 分支起始 LSN
    ParentLSN string `json:"parentLSN,omitempty"`

    // +new ParentTimestamp 时间点分支
    ParentTimestamp string `json:"parentTimestamp,omitempty"`

    // +new InitSource 初始化源: "parent-data" | "schema-only"
    InitSource string `json:"initSource,omitempty"`

    // +new Protected 是否保护分支
    Protected bool `json:"protected,omitempty"`

    // +new Default 是否为项目默认分支
    Default bool `json:"default,omitempty"`
}
```

#### 6.2.7 预留 Proxy CRD（不注册 Controller）

```go
// Proxy CRD 预留定义，当前不注册到 Scheme，不启动 Controller。
// 未来引入 Proxy 时启用：加入 SchemeBuilder，注册 Controller。
type ProxySpec struct {
    // Replicas Proxy 副本数
    Replicas int32 `json:"replicas,omitempty"`

    // Resources 资源配置
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

    // Exposure Proxy 对外暴露配置
    Exposure *ServiceExposure `json:"exposure,omitempty"`

    // JWTSecretRef 用于验证客户端 JWT 的 Secret 引用
    JWTSecretRef *corev1.SecretReference `json:"jwtSecretRef,omitempty"`
}

type ProxyStatus struct {
    Phase      string             `json:"phase,omitempty"`
    Conditions []metav1.Condition `json:"conditions,omitempty"`
    Host       string             `json:"host,omitempty"`
    Port       int32              `json:"port,omitempty"`
}
```

### 6.3 Cluster CR 新增字段

```go
type ClusterSpec struct {
    // ... 现有字段 ...

    // +new PostgresExposure 全局 Postgres Service 暴露默认策略
    // 每个 Endpoint 可通过 Endpoint.Spec.Exposure 覆盖
    // +optional
    PostgresExposure *ServiceExposure `json:"postgresExposure,omitempty"`

    // +new ProxyConfig 预留 Proxy 部署配置 (当前不生效, Phase 3 启用)
    // +optional
    ProxyConfig *ProxyConfig `json:"proxyConfig,omitempty"`
}

type ProxyConfig struct {
    // Enabled 是否启用 Proxy 部署。默认 false。
    // 设为 true 且 Operator 升级到 Phase 3 版本后将创建 Proxy。
    // +kubebuilder:default:=false
    Enabled bool `json:"enabled,omitempty"`

    // Replicas Proxy 副本数
    // +kubebuilder:default:=2
    Replicas int32 `json:"replicas,omitempty"`

    // Resources Proxy 资源配置
    // +optional
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

    // Exposure Proxy 对外暴露配置
    // +optional
    Exposure *ServiceExposure `json:"exposure,omitempty"`
}
```

---

## 七、API 创建租户/分支的完整流程总结

### 7.1 用户视角

```
1. POST /api/v2/projects
   输入: { "project": { "name": "my-db", "pg_version": 16 } }
   输出: project + branch + endpoint + connection_uri + operation

2. 轮询操作状态
   GET /api/v2/projects/{project_id}/operations/{op_id}
   → status: "scheduling" → "running" → "finished"

3. 使用返回的 connection_uri 连接数据库
   psql "postgresql://myapp_owner:<pass>@<host>:55433/neondb"

4. 创建开发分支
   POST /api/v2/projects/{project_id}/branches
   { "branch": { "name": "dev", "parent_id": "{main_branch_id}" }, "endpoints": [{ "type": "read_write" }] }
```

### 7.2 内部流程

```
用户 API 调用
  ├─ Control Plane API → 创建 K8s CRD (Project, Branch, Endpoint, Role, Database, Operation)
  │
  ├─ ProjectController → PUT /v1/tenant/{id}/location_config → SC API
  ├─ BranchController  → POST /v1/tenant/{id}/timeline  → SC API
  ├─ EndpointController → 创建 Compute Deployment/Service/ConfigMap
  ├─ RoleController     → role 变更触发 ComputeSpec 重新生成 → POST /configure
  ├─ DatabaseController → database 变更触发 ComputeSpec 重新生成 → POST /configure
  └─ OperationController → 聚合各 Controller 状态 → 更新 Operation CR status
```

### 7.3 ComputeSpec 生成（层次化）

```
ComputeSpec 由 EndpointController 生成:
  cluster.cluster_id   ← Project.Spec.TenantID
  cluster.name         ← Project.Spec.Name
  cluster.roles        ← 查询所有 Role CR (branch filter) → 聚合
  cluster.databases    ← 查询所有 Database CR (branch filter) → 聚合
  cluster.settings     ← Project.Spec.DefaultEndpointSettings + Endpoint.Spec

  tenant_id            ← Project.Spec.TenantID (顶层, 首选)
  timeline_id          ← Branch.Spec.TimelineID
  pageserver_connection_info ← SC.GetTenantInfo(tenant_id)
  safekeeper_connstrings     ← Safekeeper CR 列表
  storage_auth_token         ← Cluster JWT Secret

  mode                 ← Endpoint.Spec.Type → "Primary" / "Replica"
```

---

## 八、分阶段实施路线图

### Phase 1: 核心 CRD 增强 + API 基础 (当前 → 4 周)

| 优先级 | 任务 | 说明 |
|:---:|------|------|
| ~~P0~~ | ~~新增 Endpoint CRD + Controller~~ | ✅ **已完成 (2026-06-26)** |
| ~~P0~~ | ~~新增 Role + Database CRD + Controller~~ | ✅ **已完成 (2026-06-29)** — SCRAM-SHA-256 密码加密、Secret 管理 |
| ~~P0~~ | ~~ComputeSpec 生成聚合所有 CR 数据~~ | ✅ **已完成 (2026-06-29)** — aggregateRoles/aggregateDatabases |
| ~~P1~~ | ~~Control Plane API 基础框架~~ | ✅ **已完成** — Go HTTP `/api/v2/*` REST API, port 8081 |
| ~~P1~~ | ~~新增 Operation CRD~~ | ✅ **已完成** |
| P0 | Branch CR 增加 `parentBranch`/`parentLSN`/`initSource` 字段 | 解锁 Branch-from-Branch |
| P1 | Control Plane 认证增强 | Bearer Token 等

### Phase 2: API 完整实现 (第 5-8 周)

| 优先级 | 任务 | 说明 |
|:---:|------|------|
| ~~P0~~ | ~~POST /projects 完整链路打通~~ | ✅ **已完成** — Tenant + Timeline + Compute 全自动 |
| ~~P0~~ | ~~POST .../roles, .../databases~~ | ✅ **已完成** — Role/Database 管理 API + SCRAM-SHA-256 |
| ~~P1~~ | ~~密码安全存储~~ | ✅ **已完成** — Secret 命名与 Controller 统一，密码可立即使用 |
| P0 | POST /projects/:id/branches | Branch from parent 完整流程 |
| P0 | POST /projects/:id/endpoints | 独立创建 Endpoint，支持 read_only |
| P0 | GET /projects/:id/operations | 操作状态查询 |
| P1 | Connection URI 自动构建 | 返回可用的连接字符串 |
| P1 | 项目级别暴露策略 | DefaultEndpointSettings + 每 Endpoint 覆盖 |
| P1 | 集成测试 | 端到端 API → 数据库连接测试 |

### Phase 3: 安全增强 + 容量管理 + Proxy (第 9-12 周)

| 优先级 | 任务 | 说明 |
|:---:|------|------|
| P0 | SC 去掉 --dev 模式 | JWT 全链路认证 |
| P0 | ComputeSpec 降级方案 | SC 不可达时仍能启动 |
| P1 | Proxy 部署 (启用预留 CRD) | SNI 路由, 统一 5432 端口 |
| P1 | 多租户 Namespace 隔离 | 每 Project 独立 Namespace（可选） |
| P1 | NetworkPolicy | 限制组件间访问 |
| P1 | 配额限制 | maxProjects, maxBranches, maxEndpoints |
| P1 | API Key 管理 | 创建/吊销 API Key API |
| P2 | Helm Chart | 一键部署 |
| P2 | Prometheus 指标 | Control Plane + Operator 指标 |

### Phase 4: 高级特性 + 生产就绪 (Phase 3 之后)

| 优先级 | 任务 | 说明 |
|:---:|------|------|
| P1 | 审计日志 | API 调用审计 |
| P1 | 自动备份恢复 | PITR 策略集成 |
| P1 | [可选] Serverless 自动 Suspend/Resume | 空闲计算节点暂停和唤醒 (预留接口启用) |
| P1 | [可选] Autoscaling (HPA) | 基于 K8s HPA/VPA 的计算弹性扩缩 |
| P2 | Grafana Dashboard | 运维监控面板 |
| P2 | 端到端混沌测试 | 故障注入和恢复测试 |

---

## 九、与 Neon 生产 API 的差异说明

| 维度 | Neon 生产 | 当前设计 | 原因 |
|------|-----------|----------|------|
| **Proxy** | 有, SNI 路由到 Compute | 无, 每个 Compute 独立 LB | Proxy 复杂度高, Phase 3 引入 |
| **Autoscaling** | min_cu/max_cu 动态扩缩 | 固定 CPU/Memory | 暂不考虑, Phase 4 可选 (HPA) |
| **Serverless (Suspend/Resume)** | 自动暂停空闲 Compute | 未实现 | 暂不考虑, Phase 4 可选 |
| **区域** (region_id) | 多区域 | 单 K8s 集群 | 自建场景, 集群即区域 |
| **Billing/计费** | 完整计费系统 | 无 | SaaS 功能, 非自建需求 |
| **VPC/PrivateLink** | AWS VPC/Azure VNet | 未实现 | Phase 3+ 需求 |
| **OAuth 角色** | 支持 OAuth 认证 | 仅 password 认证 | OAuth 需要 IdP 集成 |
| **Logical Replication** | 支持 | 未实现 | Phase 4+ 需求 |
| **Schema-only Branch** | 支持 | 支持 (initSource) | ✅ |
| **Time Travel Branch** | 支持 (parent_timestamp) | 支持 (parentTimestamp) | ✅ |
| **LSN Branch** | 支持 (parent_lsn) | 支持 (parentLSN) | ✅ |
| **Read-only Endpoint** | 支持 | 支持 | ✅ |
| **Operation 追踪** | 支持 | 支持 | ✅ |

---

## 十、架构决策记录 (ADR)

### ADR-001: Proxy 预留而非立即实现

**状态**: 已采纳

**决策**: 当前不实现 Proxy 组件，但通过以下方式为未来引入做准备：
1. Cluster CR 保留 `proxyConfig` 字段（comment 标注为预留）
2. 代码中定义 Proxy CRD 类型但不注册 Scheme
3. Compute 保留 Admin Service (ClusterIP:3080) 作为控制通道
4. 外部直接通过 LoadBalancer 连接 Compute

**理由**:
- Proxy 实现复杂度高（SNI 路由、LFC 预热、连接池、SSL 终止）
- 当前阶段直连 Compute 可满足开发测试和中小规模部署需求
- 保留的两条路径使未来迁移成本最低

**未来迁移路径**:
1. 启用 Proxy Controller → 自动创建 Proxy Deployment+Service
2. 用户连接从 `<compute-lb>:55433` 迁移到 `<proxy-lb>:5432`
3. Compute PostgresService 类型从 LoadBalancer 切换为 ClusterIP

### ADR-002: CRD 作为内部状态存储而非用户 API

**状态**: 已采纳

**决策**: Project/Branch/Endpoint/Role/Database/Operation CR 由 Control Plane API 创建和管理，不直接暴露给终端用户。

**理由**:
- 提供用户友好的 REST API（遵循 Neon api-v2 规范）
- CRD 作为"基础设施即代码"的内部实现细节
- 高级用户仍可通过 `kubectl` 操作 CRD（GitOps 兼容）
- CRD 提供声明式调和、状态缓存、Watch 机制

### ADR-003: PostgresService 暴露策略可覆盖

**状态**: 已采纳

**决策**: 建立三级暴露策略优先级：
1. Endpoint.Spec.Exposure（最高优先级，每端点覆盖）
2. Cluster.Spec.PostgresExposure（集群默认）
3. 硬编码 ClusterIP（当以上都未设置）

**理由**: 这使得安全性要求不同的 Endpoint（如只读副本）可使用不同的网络策略。

### ADR-004: 暂不考虑 Autoscaling 和 Serverless

**状态**: 已采纳

**决策**: 当前版本采用固定资源分配模式，不实现 CU 弹性伸缩和自动休眠/唤醒。

**理由**:
- Autoscaling 需要 K8s HPA/VPA 集成，依赖指标采集和调度策略，增加复杂度
- Serverless (Suspend/Resume) 需要 Page Server 端的 Warm-up 机制、LFC 预热和快速的 Compute 冷启动优化
- 当前阶段固定资源分配可满足中小规模部署需求
- 数据结构中预留了相关字段（注释标注 [未来 Serverless]），未来可平滑启用

**未来迁移路径（当需要时）**:
1. Autoscaling: 启用 K8s HPA，添加 CU→CPU/Memory 转换层，EndpointSpec 增加 min/max 资源上下限
2. Serverless: 启用 SuspendTimeoutSeconds，EndpointController 实现空闲检测→Deployment scale-to-zero→连接唤醒逻辑

---

## 附录 C: 未来 Serverless/Autoscaling 预留说明

当前架构为未来的 Serverless 和 Autoscaling 预留了以下接口：

### C.1 EndpointSpec 预留字段

```go
// [未来 Serverless]
// 当 EndpointSpec.SuspendTimeoutSeconds > 0 时,
// EndpointController 监控活跃连接数, 超时后将 Deployment replicas 缩为 0
// 连接到达时, Proxy (Phase 3+) 或 Watch 机制触发回滚
// SuspendTimeoutSeconds *int32 `json:"suspendTimeoutSeconds,omitempty"`
```

### C.2 EndpointStatus Phase 扩展

```
当前: creating_compute → starting → active → stopping → stopped
未来: ... → active → idle (检测到的空闲) → suspended (Compute 已暂停) → resuming (唤醒中)
```

### C.3 Autoscaling 预留

```go
// [未来 Autoscaling]
// EndpointSpec 可扩展:
//   AutoscalingLimits *AutoscalingLimits `json:"autoscalingLimits,omitempty"`
//
// type AutoscalingLimits struct {
//     MinCPU    string `json:"minCPU"`    // e.g. "0.5"
//     MaxCPU    string `json:"maxCPU"`    // e.g. "4"
//     MinMemory string `json:"minMemory"` // e.g. "1Gi"
//     MaxMemory string `json:"maxMemory"` // e.g. "16Gi"
// }
//
// 或采用 CU 模型 (对齐 Neon 生产):
// type AutoscalingLimitsCU struct {
//     MinCU float64 `json:"minCU"` // 0.25 CU = 0.25 vCPU + 1GB RAM
//     MaxCU float64 `json:"maxCU"` // 1 CU = 1 vCPU + 4GB RAM
// }
```

### C.4 Proxy + Serverless 协同

未来 Proxy 引入后，Serverless 唤醒流程：
```
1. Client 连接 Proxy
2. Proxy 发现目标 Compute 处于 suspended 状态
3. Proxy 调用 Control Plane API: POST /v2/wake_compute
4. Control Plane 将 Deployment replicas: 0→1
5. Compute Pod 启动 (冷启动, compute_ctl → basebackup → Postgres ready)
6. Proxy 通过 AdminService POST /configure 推送配置
7. Proxy 将 Client 连接转发到 Compute
```

### C.5 数据库端连接池 (PgBouncer) 预留

```go
// [未来 PgBouncer]
// 当每个 Compute Pod 内嵌 PgBouncer sidecar 容器时:
//   EndpointSpec.ConnectionPooling *ConnectionPoolingConfig
//
// type ConnectionPoolingConfig struct {
//     Enabled          bool   `json:"enabled"`
//     PoolMode         string `json:"poolMode"`         // session | transaction | statement
//     DefaultPoolSize  int32  `json:"defaultPoolSize"`  // 默认每用户连接池大小
//     MaxClientConn    int32  `json:"maxClientConn"`    // 最大客户端连接数
// }
```

---


## 附录 A: Control Plane API Spec (OpenAPI 摘录)

```yaml
openapi: "3.0.2"
info:
  title: Neon Operator Control Plane API
  version: v2.0
servers:
  - url: http://{host}:8080/api/v2
paths:
  /projects:
    post:
      summary: Create a project
      operationId: createProject
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/ProjectCreateRequest'
      responses:
        '201':
          description: Project created
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/CreatedProject'
    get:
      summary: List projects
      operationId: listProjects
      responses:
        '200':
          description: Projects list
  /projects/{project_id}:
    get:
      summary: Get project details
    delete:
      summary: Delete project
  /projects/{project_id}/branches:
    post:
      summary: Create branch
    get:
      summary: List branches
  /projects/{project_id}/branches/{branch_id}:
    get:
      summary: Get branch details
    delete:
      summary: Delete branch
  /projects/{project_id}/endpoints:
    post:
      summary: Create endpoint
    get:
      summary: List endpoints
  /projects/{project_id}/endpoints/{endpoint_id}:
    get:
      summary: Get endpoint details
    delete:
      summary: Delete endpoint
  /projects/{project_id}/branches/{branch_id}/roles:
    post:
      summary: Create role
    get:
      summary: List roles
  /projects/{project_id}/branches/{branch_id}/roles/{role_name}:
    delete:
      summary: Delete role
  /projects/{project_id}/branches/{branch_id}/roles/{role_name}/reset_password:
    post:
      summary: Reset role password
  /projects/{project_id}/branches/{branch_id}/databases:
    post:
      summary: Create database
    get:
      summary: List databases
  /projects/{project_id}/operations:
    get:
      summary: List operations
  /projects/{project_id}/operations/{operation_id}:
    get:
      summary: Get operation details
```

## 附录 B: 目录结构建议

```
neon-operator/
├── cmd/
│   ├── controller/          # 现有 Operator manager
│   │   └── main.go
│   └── controlplane/        # [NEW] Control Plane API server
│       └── main.go
├── internal/
│   ├── controller/          # 现有 Controller (不变)
│   │   ├── cluster_controller.go
│   │   ├── pageserver_controller.go
│   │   ├── safekeeper_controller.go
│   │   ├── project_controller.go    # 修改
│   │   ├── branch_controller.go     # 修改
│   │   ├── endpoint_controller.go   # [NEW]
│   │   ├── role_controller.go       # [NEW]
│   │   ├── database_controller.go   # [NEW]
│   │   └── operation_controller.go  # [NEW]
│   └── controlplane/        # [NEW] Control Plane 内部逻辑
│       ├── server.go        # HTTP server setup
│       ├── router.go        # 路由注册
│       ├── auth.go          # API Key 认证
│       ├── handler/         # 请求处理器
│       │   ├── project.go
│       │   ├── branch.go
│       │   ├── endpoint.go
│       │   ├── role.go
│       │   ├── database.go
│       │   └── operation.go
│       └── service/         # 业务逻辑层
│           ├── project_service.go
│           ├── branch_service.go
│           └── endpoint_service.go
├── api/v1alpha1/
│   ├── cluster_types.go     # 修改, 增加 ProxyConfig
│   ├── project_types.go     # 修改, 增加 Name/Defaults
│   ├── branch_types.go      # 修改, 增加 parent/initSource
│   ├── endpoint_types.go    # [NEW]
│   ├── role_types.go        # [NEW]
│   ├── database_types.go    # [NEW]
│   ├── operation_types.go   # [NEW]
│   └── proxy_types.go       # [NEW] 预留
└── config/crd/bases/
    ├── ... 现有 CRD YAML ...
    ├── neon.oltp.molnett.org_endpoints.yaml  # [NEW]
    ├── neon.oltp.molnett.org_roles.yaml      # [NEW]
    ├── neon.oltp.molnett.org_databases.yaml  # [NEW]
    └── neon.oltp.molnett.org_operations.yaml # [NEW]
```
