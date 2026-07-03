# Pageserver 生产级分析与设计方案

> 状态：深度调研中，P0 大部分已实现，P1-P3 方案级设计
> 调研范围：neon 源码 `/home/postgres/works/opensource/neon`（pageserver/ storage_controller/ libs/），operator 源码 `/home/postgres/works/opensource/neon-operator`，Neon Cloud API (`https://api-docs.neon.tech`)
> 最后更新：2026-07-02（根据最新代码同步）

---

## 目录

1. [当前实现现状](#1-当前实现现状)
   - [1.5 CRD 架构决策：Pageserver 的数量管理方式](#15-crd-架构决策-pageserver-的数量管理方式)
   - [1.6 分片与架构层关系](#16-分片shard与架构层关系)
2. [neon pageserver 源码分析](#2-neon-pageserver-源码分析)
   - [2.4 LocationConfig API：Tenant 附着/分离核心协议](#24-locationconfig-api-tenant-附着分离核心协议)
   - [2.5 Tenant 状态机](#25-tenant-状态机)
   - [2.6 冷启动与 Warmup 流程](#26-冷启动与-warmup-流程)
   - [2.7 Tenant 数据目录结构](#27-tenant-数据目录结构)
3. [生产级差距分析](#3-生产级差距分析)
4. [详细设计方案](#4-详细设计方案)
   - [4.3 P0-3: PodAntiAffinity 与多 AZ 反亲和性 (深度分析)](#43-p0-3-podantiaffinity-与多-az-反亲和性-深度分析)
   - [4.5 节点故障与 Pageserver 迁移机制](#45-节点故障与-pageserver-迁移机制-深度分析)
   - [4.11 生产环境 Pageserver 扩缩容机制](#411-生产环境-pageserver-扩缩容机制-深度分析)
   - [4.12 P2: Pageserver HTTP API 集成方案](#412-p2-pageserver-http-api-集成方案)
   - [4.13 P2: Operator 侧 Tenant 生命周期管理](#413-p2-operator-侧-tenant-生命周期管理)
5. [实施路线图](#5-实施路线图)
6. [附录 A: 文件改动预估](#附录-a-文件改动预估)
7. [附录 B: 设计决策记录](#附录-b-设计决策记录)
8. [附录 C: Cluster YAML 配置示例](#附录-c-cluster-yaml-配置示例)

---

## 1. 当前实现现状

### 1.1 已创建的资源

Operator 为每个 Pageserver CR 创建以下 Kubernetes 资源：

| 资源 | 说明 | 实现 |
|------|------|:---:|
| `StatefulSet` | 单副本，一个 InitContainer + 一个主容器 | ✅ |
| `Service` (ClusterIP) | 暴露 6400 (libpq) + 9898 (HTTP) | ✅ |
| `Headless Service` | StatefulSet 稳定网络标识 | ✅ |
| `ConfigMap` | `pageserver.toml` 配置文件（含完整生产参数） | ✅ |
| `PodDisruptionBudget` | maxUnavailable=1，防止维护时多个 PS 同时被驱逐 | ✅ |
| `Secret` (JWT) | PS→SC / PS→SK 认证 token 持久化 | ✅ |

### 1.2 当前容器配置

**InitContainer (setup-config, busybox)**：
- 写入 `/config/identity.toml`（`id={spec.id}`）
- 写入 `/config/metadata.json`（host、http_host、port、availability_zone 等）
- 拷贝 `/configmap/pageserver.toml` → `/config/pageserver.toml`

**主容器 (pageserver)**：
- 命令：`/usr/local/bin/pageserver`（无额外命令行参数）
- 只通过 `pageserver.toml` 配置
- 端口：6400 (libpq)、9898 (HTTP)
- 环境变量：`RUST_LOG=debug`、`DEFAULT_PG_VERSION=16`、5 个 S3 凭证变量、可选 `NEON_AUTH_TOKEN`
- 挂载：`/data/.neon/tenants`（PVC）、`/data/.neon`（EmptyDir，配置文件）、`/certs`（JWT public key Secret）
- ✅ LivenessProbe / ReadinessProbe / StartupProbe（已实现）
- ✅ PodAntiAffinity (Node 级别)（已实现）
- ✅ PDB（已实现）
- ✅ `TerminationGracePeriodSeconds: 60`（已实现）
- ✅ Resource Requests/Limits（已实现，默认 CPU=500m/2, Mem=256Mi/512Mi，可通过 `Spec.Resources` 覆盖）
- ✅ AvailabilityZone 字段（已实现，默认 `"se-ume"`，可通过 `Spec.AvailabilityZone` 覆盖）
- ✅ JWT Volume 挂载（`/certs/public.pem`）和 `control_plane_api_token`（已实现）
- **无 AZ 级 topologySpreadConstraints**
- `ImagePullPolicy: Always`
- `SecurityContext: uid=1000, gid=1000, fsGroup=1000`

### 1.3 当前 ConfigMap 内容

当前 `specs/pageserver/configmap.go` 生成的完整 `pageserver.toml`：

```toml
# 网络
listen_pg_addr = "0.0.0.0:6400"
listen_http_addr = "0.0.0.0:9898"

# Broker
broker_endpoint = "http://{cluster}-storage-broker.{ns}.svc:50051"
broker_keepalive_interval = "5s"

# 控制平面
control_plane_api = "http://{cluster}-storage-controller.{ns}.svc:8080/upcall/v1/"

# 认证 (NeonJWT)
http_auth_type = "NeonJWT"
pg_auth_type = "NeonJWT"
auth_validation_public_key_path = "/certs/public.pem"

# PostgreSQL 分发
pg_distrib_dir = "/usr/local/"

# 远程存储 (S3)
[remote_storage]
bucket_name = "{bucket}"
bucket_region = "{region}"
prefix_in_bucket = "pageserver"
endpoint = "{endpoint}"

# 磁盘驱逐
[disk_usage_based_eviction]
max_usage_pct = 80
min_avail_bytes = 2000000000  # 2GB
period = "60s"

# 租户默认配置
[tenant_config]
checkpoint_distance = "256 MB"
compaction_threshold = 10
compaction_target_size = "128 MB"
gc_horizon = "64 MB"
gc_period = "1 hr"
pitr_interval = "7 days"

# 并发控制
concurrent_tenant_warmup = 8
```

**已配置的关键生产参数**：

| 配置区 | 参数 | 状态 |
|--------|------|:---:|
| 认证 | `http_auth_type = NeonJWT`, `pg_auth_type = NeonJWT`, JWT public key 挂载 | ✅ |
| Broker | 连接地址 + keepalive 间隔 | ✅ |
| 控制平面 | API 地址 + token（`pageserver_control_plane_token`，scope: `generations_api`） | ✅ |
| 远程存储 | S3 凭证（bucket / region / endpoint / prefix） | ✅ |
| 磁盘驱逐 | `max_usage_pct=80%`, `min_avail_bytes=2GB`, `period=60s` | ✅ |
| 租户默认 | checkpoint_distance, compaction_threshold, gc_period, pitr_interval 等 | ✅ |
| 并发 | `concurrent_tenant_warmup=8` | ✅ |

**未配置的参数**（使用源码默认值）：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `max_file_descriptors` | 100 | ⚠️ 偏低，生产建议 1000+ |
| `page_cache_size` | 8192 页 | 建议根据内存调整 |
| `log_format` | 未设置（非 JSON） | 生产建议 `"json"` |
| `metric_collection_interval` | 10min | 建议配合 Prometheus scrape interval |
| `wait_lsn_timeout` | 300s | 默认可用 |
| `wal_redo_timeout` | 60s | 默认可用 |

### 1.4 Controller 逻辑

```
PageserverReconciler {
    client.Client
    Scheme   *runtime.Scheme
    SCClient *SCClient  // Storage Controller HTTP API 客户端
}

Reconcile 主流程:
  1. getPageserver() → 获取 CR
  2. 若正在删除（DeletionTimestamp != nil）：
     → finalize() — 完整四阶段安全下线流程（见下方）
  3. 若未删除：
     a. 确保 finalizer 存在
     b. handleNodeFailure() → 检测 Pod Pending + Volume Node Affinity Conflict
         → 超过阈值后自动删除 PVC + Pod（若 AutoRecover 启用）
     c. reconcile() → createPageserverResources()
     d. UpdateSTSBackedStatus() → 从 StatefulSet 更新 Conditions

createPageserverResources() 详细流程:
  1. ensurePSAuthTokens() — JWT Token 管理:
     ├── 从 Secret 读取 pageserver_control_plane_token (scope: generations_api)
     ├── 从 Secret 读取 pageserver_safekeeper_token (scope: safekeeperdata)
     └── 若 token 不存在或过期（IsTokenExpired），重新生成并持久化
         └── 使用 SHA256 确定性算法避免每次 reconcile 重新签发
  2. reconcileConfigMap() ← 读取 bucket Secret → 生成 pageserver.toml
  3. reconcile Service (ClusterIP)
  4. reconcile Headless Service
  5. reconcile PodDisruptionBudget (maxUnavailable=1)
  6. reconcile StatefulSet ← 需要 Cluster CR 获取 image
  7. syncSCState() → GET /control/v1/node/{id} → 更新 Status

Finalize（安全下线）四阶段流程:
  Phase 0: 准入检查 (Pre-drain)
    ├── 检查 SC 可达性
    └── 检查有其他可调度节点 (schedulable_nodes_count > 0)
  Phase 1: 启动 Drain
    └── PUT /control/v1/node/{node_id}/drain（若 Scheduling==Active）
  Phase 2: 监控 Drain 进度
    ├── 轮询 attached shard count（间隔 5s，超时 30min）
    └── 等待 attached shard count = 0
  Phase 3: 确认删除
    ├── PUT /control/v1/node/{node_id}/config {scheduling: "PauseForRestart"}
    ├── PUT /control/v1/node/{node_id}/delete
    └── removeFinalizer → K8s 级联清理

关键常量:
  - drainPollInterval = 5s
  - drainTimeout = 30min
  - nodeFailurePendingThreshold = 5min（默认，可通过 MaxPendingDuration 覆盖）
```

**JWT Token 管理**（`ensurePSAuthTokens`）：
- 使用 Ed25519 JWT scheme，两个 scope 分别签发
- `pageserver_control_plane_token`（scope: `generations_api`）：PS→SC upcall 认证
- `pageserver_safekeeper_token`（scope: `safekeeperdata`）：PS→SK WAL 认证
- Token 持久化到名为 `{cluster}-jwt` 的 Secret 中
- 通过 `IsTokenExpired` 检测过期，使用 SHA256 确定性算法避免每次 reconcile 重新签发
- JWT public key 通过 Volume（`/certs/public.pem`）挂载到 PS Pod

**Watches**（`SetupWithManager`）：监控 `Pageserver`, `StatefulSet`, `Service`, `ConfigMap`, `PodDisruptionBudget`

**Cluster Controller 集成**：Cluster Controller 通过 `Cluster.Spec.NumPageservers` 和 `Cluster.Spec.DefaultPageserverConfig` 字段支持声明式管理（相关 types 已定义，reconcilePageservers 逻辑待实现）。Shard 是 tenant 级别的概念，不由 Cluster 层控制（详见 [1.6 节](#16-分片shard与架构层关系)）。

---

### 1.5 CRD 架构决策：Pageserver 的数量管理方式

> **核心问题**：Pageserver 的数量应如何管理？是像 Safekeeper 一样通过 `Cluster.Spec.NumPageservers` 声明式管理，还是通过独立 CRD 手动创建？本节基于 neon 源码 (`storage_controller/`, `pageserver/`, `docs/`) 和 K8s Operator 设计模式的深度分析。

#### 1.5.1 当前状态与 Safekeeper 已有模式

| | Safekeeper | Pageserver |
|---|---|---|
| **CRD 存在** | ✅ 独立 Safekeeper CRD | ✅ 独立 Pageserver CRD |
| **Cluster 自动创建** | ✅ `reconcileSafekeepers()` 根据 `NumSafekeepers` 创建 | 🔜 Types 已定义（`NumPageservers`+`PageserverConfig`），reconcile 逻辑待实现 |
| **OwnerReference** | ✅ `ctrl.SetControllerReference(cluster, sk, ...)` | ❌ 无（待实现） |
| **Cluster 删除行为** | 级联删除 Safekeeper CR | Pageserver 不受影响 |
| **数量控制** | `Cluster.Spec.NumSafekeepers` | `Cluster.Spec.NumPageservers`（字段已定义） |
| **默认配置** | 无 | `Cluster.Spec.DefaultPageserverConfig`（字段已定义，含 StorageSize/Resources/InitialSchedulingPolicy/NodeFailure） |
| **关联方式** | Label `molnett.org/cluster` | Spec 字段 `Cluster string` |

Safekeeper 的成熟模式：
```
Cluster.Spec.NumSafekeepers=3
  → Cluster Controller 创建 3 个 Safekeeper CR（OwnerReference）
  → Safekeeper Controller reconcile → StatefulSet + Service + PDB
  → 缩容：Cluster Controller delete SK CR → SK finalizer 通知 SC decommission → K8s 删除
  → 删除 Cluster：级联删除所有 SK CR
```

#### 1.5.2 方案对比

##### 方案 A：Cluster.Spec.NumPageservers 声明式管理（与 Safekeeper 一致）

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
spec:
  numSafekeepers: 3
  numPageservers: 5     # 声明式期望数量
  defaultPageserverConfig:
    storageSize: 2Ti
```

**流程**：
- 扩容：`kubectl edit cluster` → 修改 `numPageservers: 5→8` → Controller 创建 3 个 PS CR → PS Controller 创建 3 组 K8s 资源
- 缩容：`kubectl edit cluster` → 修改 `numPageservers: 8→6` → Controller 发起 drain → 等待 SC 完成迁移 → 删除 2 个 PS CR
- 删除 Cluster：级联删除 PS CR → PS finalizer drain tenant → 清理 K8s 资源

##### 方案 B：独立 Pageserver CRD，手动管理（当前模式）

```yaml
# 需分别创建每个 PS
kubectl create -f pageserver-1.yaml
kubectl create -f pageserver-2.yaml
kubectl create -f pageserver-3.yaml
```

扩容 = 再创建几个 YAML，缩容 = 手动 `kubectl delete`（无 drain 保护，当前 finalizer 是空操作）。

#### 1.5.3 深度对比分析

| 维度 | 方案 A（Cluster.Spec 声明式） | 方案 B（独立 CRD 手动管理） |
|------|:--|:--|
| **K8s 声明式模型** | ✅ 期望状态 = `numPageservers`，控制器自动 reconcil | ❌ 命令式：手动 `kubectl create/delete` |
| **业界一致性** | ✅ 匹配 Strimzi/Zalando/ECK/CassKop 等主流 Operator | ❌ 未见 K8s Operator 使用此模式 |
| **代码库一致性** | ✅ 与 `NumSafekeepers` 完全相同的模式 | ❌ 与已有 Safekeeper 模式不一致 |
| **扩容操作** | ✅ 一键修改数字 | ⚠️ 需逐个创建新 CR |
| **缩容安全** | ✅ Cluster Controller 自动 drain → delete | ⚠️ 需用户手动 `kubectl delete`（但 finalizer 已实现 drain 保护） |
| **统一视图** | ✅ `kubectl get cluster` 看到完整拓扑 | ⚠️ 需分别查询 Cluster + PS CR |
| **PS CRD 保留** | ✅ PS CRD 仍作为一等 K8s 对象存在（由 Controller 创建） | ✅ PS CRD 手动创建 |
| **单 PS 运维** | ✅ `kubectl annotate pageserver X drain=true` | ✅ `kubectl delete pageserver X`（finalizer 自动执行 drain） |

#### 1.5.4 关键论点的重新审视

##### 论点 1："Pageserver 数量动态变化，不应放在 Cluster Spec 中"

**重新评估**：**这是对 K8s 声明式模型的误解。** K8s Spec 正是用来表达"期望状态"的——无论该状态是固定的还是动态变化的。例如 Strimzi Kafka 的 `kafka.replicas` 同样是动态变化的（业务负载驱动），但依然放在 Spec 中。**动态变化不是排除在 Spec 之外的理由，而是 Spec 存在的价值**。

##### 论点 2："Scheduler 冲突：SC 调度 Tenant→PS，Operator 不应干预数量"

**重新评估**：**SC scheduler 和 Operator 操作在不同的抽象层，不存在冲突。**
- SC scheduler：给定 N 个 pageserver，决定 tenant X 放在哪个 PS 上（placement optimization）
- Operator：决定需要多少 pageserver（capacity management）

两者是互补的，不是竞争的。类比：K8s scheduler 决定 Pod→Node 的放置，但 Node 的数量由 Cluster Autoscaler 或管理员决定——两者从不"冲突"。

##### 论点 3："Cluster 删除不能级联删除 PS（数据安全）"

**重新评估**：**这是对 K8s OwnerReference 语义的误解。**
- 1 PS 只能注册到 1 SC。SC 删除后，PS 上的数据对任何其他 SC 都不可见。
- 保留无主 PS 的"数据"实际上无法访问，没有实际意义。
- 如果确实需要迁移数据，应该**在删除 Cluster 之前**执行迁移操作（对应的正确流程）。
- K8s 中 OwnerReference 级联删除是**资源一致性**的标准实践。如果不想级联删除，可以用 `--cascade=orphan` 或先移除 OwnerReference。

**正确的安全流程**（无论方案 A 还是 B 都需要）：
```
准备迁移 → SC drain 所有 PS → 数据已迁移到 S3/新 Cell → 删除 Cluster → 级联删除 PS
```
不是在"删除 Cluster 后保留 PS"这个时间点做迁移。

##### 论点 4："SC scheduler 管理 Tenant→PS 映射，Operator 管理 Pod 生命周期，分工应清晰"

**重新评估**：**这正是方案 A 的设计。** 方案 A 中：
- Cluster Controller: 管理 PS **数量**（创建/删除 PS CRD）
- PS Controller: 管理单个 PS 的 **Pod 生命周期**（StatefulSet、Service、健康检查）
- SC scheduler: 管理 **Tenant→PS 映射**（placement、drain、fill）

三层职责清晰分离，各司其职。方案 B 的问题是：**用户承担了 PS 数量的管理职责**（手动创建/删除），这既不是 Operator 的职责，也不是 SC 的职责，而是一个"真空地带"。

#### 1.5.5 实现设计

##### ClusterSpec 扩展 — ✅ Types 已定义

**当前状态**：`NumPageservers` 和 `PageserverConfig` 字段已在 `api/v1alpha1/cluster_types.go` 中完整定义，`Cluster Controller` 的 `reconcilePageservers()` 逻辑待实现。

```go
// api/v1alpha1/cluster_types.go（已存在）
type ClusterSpec struct {
    // ...existing fields...

    // NumPageservers 指定期望的 pageserver 数量。
    // 默认为 1（开发测试），生产环境建议 ≥ 2。
    // +kubebuilder:default:=1
    // +kubebuilder:validation:Minimum:=1
    NumPageservers int32 `json:"numPageservers,omitempty"`

    // DefaultPageserverConfig 指定自动创建的 pageserver 的默认配置。
    // +optional
    DefaultPageserverConfig *PageserverConfig `json:"defaultPageserverConfig,omitempty"`
}

// api/v1alpha1/cluster_types.go（已存在）
type PageserverConfig struct {
    // StorageSize 指定 PS 的 PVC 大小。
    // +kubebuilder:default:="100Gi"
    StorageSize string `json:"storageSize,omitempty"`
    // Resources 指定 PS 的 CPU/内存配置。
    // +optional
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
    // InitialSchedulingPolicy 新节点注册后的初始调度策略。
    // +optional
    InitialSchedulingPolicy string `json:"initialSchedulingPolicy,omitempty"`
    // NodeFailure 节点故障恢复策略。
    // +optional
    NodeFailure *NodeFailureRecoveryConfig `json:"nodeFailure,omitempty"`
}
```

##### reconcilePageservers() 逻辑

```
1. desired := cluster.Spec.NumPageservers
2. existing := 列出 label molnett.org/cluster=<name> 的 Pageserver CR（按 ID 排序）
3. 扩容：对 ID > len(existing) 的，创建 Pageserver CR（OwnerReference）
4. 缩容：对 ID > desired 的，按 ID 降序处理：
   a. 标记 PS CR 为 draining（annotation）
   b. PS Controller 调用 SC PUT /node/:id/drain
   c. 轮询 SC GET /node/:id 等待 scheduling 变为 PauseForRestart
   d. 删除 PS CR
5. 更新 Cluster Status（已就绪 PS 数量）
```

##### Pageserver Controller finalizer 设计 — ✅ 已实现

**当前状态**：完整四阶段 finalize 流程已在 `internal/controller/pageserver_controller.go` 中实现，包含准入检查、drain 启动、进度监控、确认删除。

```go
// internal/controller/pageserver_controller.go（已实现）
func (r *PageserverReconciler) finalize(ctx context.Context, ps *v1alpha1.Pageserver) error {
    // Phase 0: 准入检查（SC 可达 + 有可调度节点）
    // Phase 1: PUT /control/v1/node/{id}/drain
    // Phase 2: 轮询 attached shard count（interval=5s, timeout=30min）
    // Phase 3: PUT /control/v1/node/{id}/config {scheduling: "PauseForRestart"}
    //         + PUT /control/v1/node/{id}/delete
    //         + removeFinalizer
}
```

**关键参数**：
- `drainPollInterval = 5s` — drain 进度轮询间隔
- `drainTimeout = 30min` — drain 整体超时
- `nodeFailurePendingThreshold = 5min` — 节点故障恢复触发阈值

##### 缩容安全保证

```
删除的排序策略：按 ID 降序（先删编号最大的）
  原因：ID 小的 PS 可能有更长的运行历史，更多的 tenant
  同时 drain 多个 PS：SC 的 drain_node() 内部并发 64 reconciler

超时处理：
  drain 超时（默认 1 小时）→ 设置 Condition (Degraded) → 不阻塞删除
  极端情况：force delete annotation → 跳过 drain（仅紧急使用）
```

##### 单 PS 运维操作（通过 annotation 实现）

```bash
# 替换某个具体的 pageserver（先 drain 再重建）
kubectl annotate pageserver my-cluster-ps-3 \
  pageserver.neon.oltp.molnett.org/action=replace

# 手动 drain 某个 PS（维护前准备）
kubectl annotate pageserver my-cluster-ps-3 \
  pageserver.neon.oltp.molnett.org/action=drain

# 均衡新节点（drain 的反操作）
kubectl annotate pageserver my-cluster-ps-3 \
  pageserver.neon.oltp.molnett.org/action=fill
```

**注意**：PS CRD 仍然作为一等 K8s 对象存在（`kubectl get pageservers` 可见），只是由 Cluster Controller 创建。这保留了可观测性和单 PS 操作能力。

#### 1.5.6 决策结论

```
┌─────────────────────────────────────────────────────────────────┐
│                    最终推荐方案                                   │
│                                                                  │
│  方案 A — Cluster.Spec.NumPageservers 声明式管理                  │
│                                                                  │
│  理由（按说服力排序）：                                            │
│  1. K8s 声明式模型 — Spec 表达期望状态，Controller 执行 reconcil   │
│  2. 与 Safekeeper 一致 — 同一代码库、同一模式、相同的维护体验       │
│  3. 业界标准实践 — 所有主流 Operator 使用此模式                    │
│  4. 安全性更好 — Controller 保证 drain-then-delete，优于手动删除   │
│  5. 三层职责分离 — Cluster(数量) / PS(Pod) / SC(Tenant→PS 映射)   │
│  6. PS CRD 仍保留 — 一等对象，支持单节点操作                       │
└─────────────────────────────────────────────────────────────────┘
```

**与 Safekeeper 模式的差异点**（实现细节，非架构差异）：

| | Safekeeper | Pageserver |
|---|---|---|
| **缩容步骤** | delete SK CR → finalizer decommission → 删除 | delete PS CR → finalizer **drain** → 等待迁移 → 删除 |
| **缩容等待** | decommission 是即时 API 调用 | drain 需要等待 shard 迁移完成 |
| **默认数量** | ≥ 3（WAL 协议硬要求） | ≥ 1（开发测试可 1，生产建议 ≥ 2） |
| **数量变化频率** | 极少 | 较频繁（随容量增长） |

架构上完全一致：**Cluster.Spec 声明 → Cluster Controller 创建 CRD → 各 Controller reconcile → finalizer 安全下线**。

---

### 1.6 分片（Shard）与架构层关系

> **核心问题**：分片是在创建 Tenant 时决定的，还是在创建 Cluster 时配置？ShardCount 和 NumPageservers 有什么区别？本节基于 neon 源码的深度分析。

#### 1.6.1 Shard 的基础概念

Neon 中 **Shard（分片）** 是一个 Tenant 的 keyspace 子分区。每个 shard 负责该 tenant 中一个子集的 page key。Shard 是 pageserver 上运行的最小存储单元。

**层级关系**（基于 `TenantShardId` 源码，`libs/utils/src/shard.rs:52-56`）：
```
Tenant (1个逻辑数据库)
  └── TenantShard (1..N个物理分区)
        ├── Shard 0: page key subset 0 (含 SLRU、aux files 等全局数据)
        ├── Shard 1: page key subset 1
        └── Shard N-1: page key subset N-1
              └── Timeline (每个 TenantShard 内部都有独立的时间线)
```

**数据分布算法**（`libs/pageserver_api/src/shard.rs:309-335`）：使用 "宽条带化"（wide striping），根据 `relNode` + `blockNum / stripe_size` 的哈希分配到不同 shard。默认 stripe size 为 16 MiB。

**关键源码证据**（`libs/pageserver_api/src/models.rs:479-500`）：
```rust
pub struct ShardParameters {
    pub count: ShardCount,            // shard 数量（0 = unsharded，即单 shard 传统模式）
    pub stripe_size: ShardStripeSize, // stripe 大小
}
impl Default for ShardParameters {
    fn default() -> Self {
        Self { count: ShardCount::new(0), stripe_size: DEFAULT_STRIPE_SIZE }
    }
}
```

#### 1.6.2 Shard 是 Tenant 级别的决策，不是 Cluster 级别的

分片配置发生在 **tenant 创建时**，由 `TenantCreateRequest.shard_parameters` 决定。每个 tenant 可以独立选择自己的 shard 数量。

**源码证据**（`libs/pageserver_api/src/controller_api.rs:19-36`）：
```rust
pub struct TenantCreateRequest {
    pub new_tenant_id: TenantShardId,
    pub generation: Option<u32>,
    pub shard_parameters: ShardParameters,   // tenant 创建时指定
    pub placement_policy: Option<PlacementPolicy>,
    pub config: TenantConfig,
}
```

当 `shard_parameters.count = 0`（默认值）时，tenant 是 unsharded（1 shard）；当 `count = N` 时，tenant 被拆分为 N 个 shard。

#### 1.6.3 ShardCount 与 NumPageservers 的区别

这是两个**正交维度**，不能混淆：

```
                         ShardCount (per-tenant)
                    1 shard           4 shards         8 shards
NumPageservers  ┌───────────────┬────────────────┬────────────────┐
  1 PS           │ 1 shard on    │ 4 shards on    │ 8 shards on    │
                 │ 1 PS ✓        │ 1 PS ✓         │ 1 PS ✓         │
  4 PS           │ 1 shard on    │ 4 shards on    │ 8 shards on    │
                 │ 1 PS (3 idle) │ 4 PS (最优)✓   │ 4 PS ✓         │
  8 PS           │ 1 shard on    │ 4 shards on    │ 8 shards on    │
                 │ 1 PS (7 idle) │ 4 PS (4 idle)  │ 8 PS (最优)✓   │
└───────────────┴────────────────┴────────────────┴────────────────┘
```

| 维度 | NumPageservers | ShardCount |
|------|---------------|------------|
| **语义** | 基础设施容量：有多少 PS 节点可用 | 业务配置：该 tenant 的数据分多少片 |
| **配置层** | Cluster（基础设施层） | **Tenant/Project（业务层）** |
| **决策者** | 平台运维（按容量需求） | 租户/应用开发者（按数据规模需求） |
| **变更频率** | 随着集群整体容量增长 | 随该 tenant 的数据量增长 |
| **默认值** | ≥1（生产 ≥2） | 1（unsharded，传统模式） |
| **SC 行为** | 注册 N 个 node 到 SC | SC 为该 tenant 创建 N 个 tenant shard |

**关键结论**：`NumPageservers` 控制的是 SC 可用的 "节点池大小"，`ShardCount` 控制的是 SC 为该 tenant "分配的分片数"。两者独立运作，互不冲突。

#### 1.6.4 `--initial-split-shards` 参数的正确理解

SC 启动参数 `--initial-split-shards` 当前硬编码为 `0`（`specs/storagecontroller/deployment.go:56-57`）：

```go
Args: []string{
    "--dev",
    "--initial-split-shards",
    "0",
},
```

**这个参数的正确含义**（基于 neon SC 源码）：它用于将 **已存在的 unsharded tenant** 拆分为 sharded mode——是一个 **数据迁移/升级参数**，不是新 tenant 的默认 shard 数量。设置为 0 表示不执行此迁移。

**不应将其误解为新 tenant 的 "默认 shard 数量"**。新 tenant 的 shard 数量由 `TenantCreateRequest.shard_parameters.count` 决定（从 API 调用传入）。

#### 1.6.5 对当前设计方案的影响

##### ✅ 不需要修改 Cluster CRD

`Cluster.Spec` 中的 `NumPageservers` 是正确的——它是容量管理参数。**不需要**将 `ShardCount` 加入 `Cluster.Spec`，因为：
- ShardCount 是 per-tenant 配置，不应提升到 Cluster 级别
- 不同 tenant 可能需要不同的 shard 数量（大租户多 shard，小租户单 shard）
- 强制所有 tenant 用相同 shard 数量违背了 neon 的设计意图

##### 🔜 需要在 Project CRD 中添加 Shard 配置（远期）

当前 operator 创建 tenant 时不传 shard 参数（`project_controller.go:240`）：
```go
requestBody := []byte(`{"mode": "AttachedSingle", "generation": 1, "tenant_conf": {}}`)
```

这意味着所有 tenant 都是 **unsharded（单 shard）模式**。当单个 tenant 数据量超过单个 pageserver 节点容量时，需要通过分片扩展。

远期可在 `ProjectSpec` 中增加：
```go
type ProjectSpec struct {
    // ...existing fields...

    // ShardCount 指定该 tenant 的分片数量。
    // 默认为 0（unsharded，即单 shard 传统模式）
    // 生产环境大租户可设为 ≥ 2，将数据分布到多个 pageserver
    // +optional
    // +kubebuilder:default:=0
    ShardCount int `json:"shardCount,omitempty"`
}
```

##### 🔜 SC 的 `--initial-split-shards` 不需要提到 Cluster CRD

这个参数是 SC 自身的启动行为，不应由 operator 的 CRD 管理。如果需要启用已有 tenant 的自动拆分，应通过 SC 的 ConfigMap 模板处理，而非 ClusterSpec 字段。

##### ✅ Pageserver 数量管理方案不受影响

无论 tenants 是否支持分片，`NumPageservers` 的声明式管理模式（与 Safekeeper 一致）都适用：
- 无分片：PS 数量决定负载上限（每个 tenant 占用 1 个 PS 节点）
- 有分片：PS 数量决定分片分布粒度（shard_count ≤ num_PS 时最优，每个 shard 一个 PS）

##### ✅ Compute Spec shard 处理已就绪

Operator 的 `specs/compute/spec.go` 已经正确处理了从 SC 获取 shard 信息并构建 `PageserverConnectionInfo`（包含 `ShardCount` 和 `Shards` map）。无论 tenant 是 unsharded 还是多 shard，compute 节点都能正确连接到对应的 pageserver。

#### 1.6.6 分片相关决策总结

| 配置项 | 级别 | 位置 | 当前状态 | 远期计划 |
|--------|------|------|---------|---------|
| **NumPageservers** | Cluster（基础设施） | `Cluster.Spec.NumPageservers` | 🔜 待实现 | 声明式容量管理 |
| **ShardCount** | Project（业务） | `Project.Spec.ShardCount` | ❌ 未实现（默认 1 shard） | 按 tenant 数据规模选择 |
| **--initial-split-shards** | SC 启动参数 | SC Deployment template | ✅ 硬编码为 0 | 通过 ConfigMap 模板管理 |
| **ShardStripeSize** | Project（业务） | 远期 `Project.Spec` | ❌ 未实现（使用默认值 16MiB） | 可选配置 |

---

## 2. neon pageserver 源码分析

### 2.1 启动流程

```
main()
  ├── 解析 --workdir (默认 .neon)
  ├── 读取 {workdir}/identity.toml → PageserverIdentity { id }
  ├── 读取 {workdir}/pageserver.toml → ConfigToml
  ├── 检测未知配置字段 (warn 但不阻止启动)
  ├── PageServerConf::parse_and_validate(id, config_toml, workdir)
  ├── 初始化日志、Sentry、tracing
  ├── 创建 tenants/ 目录 + syncfs (若 no_sync 未设置)
  ├── 初始化 virtual_file + page_cache
  └── start_pageserver()
       ├── 绑定端口 (提前发现端口冲突)
       ├── 连接 Broker、加载认证
       ├── 初始化租户管理器 (扫描本地 + 远程)
       ├── 启动磁盘驱逐任务
       ├── 启动 HTTP API (axum)
       ├── 启动 page_service (libpq :6400)
       └── 等待 SIGTERM → 优雅关闭
```

### 2.2 关键配置字段及生产建议

#### 2.2.1 网络与端口

| 参数 | 默认值 | 生产建议 |
|------|--------|----------|
| `listen_pg_addr` | `127.0.0.1:64000` | `0.0.0.0:6400` ✅ 已配置 |
| `listen_http_addr` | `127.0.0.1:9898` | `0.0.0.0:9898` ✅ 已配置 |
| `listen_https_addr` | `None` | 暂不需配置 |
| `listen_grpc_addr` | `None` (默认 `127.0.0.1:51051`) | 暂不需配置 |

**注册地址（metadata.json）**：

当前 operator 在 init container 中生成 `metadata.json`，使用 `{ps-name}.{namespace}` 格式的 ClusterIP DNS 作为注册地址。这种方式保证了地址在 Pod 重建/迁移后保持稳定：

```
# statefulset.go initScript 生成的内容：
{"host":"{cluster}-pageserver-{id}.{ns}",
 "http_host":"{cluster}-pageserver-{id}.{ns}",
 "http_port":9898,
 "port":6400}
```

**为什么这很重要**：Storage Controller 的 `registration_match()` 会严格校验 HTTP/PG 地址。如果 Pod IP 变化导致 metadata.json 中的地址变化，SC 会拒绝 re-attach 请求（`ApiError::Conflict("Node is already registered with different address")`）。使用 ClusterIP DNS 确保地址始终不变。 |

#### 2.2.2 磁盘持久化 (fsync)

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `no_sync` | `None`（即启用 fsync） | ⚠️ **与 safekeeper 不同**！Pageserver 也有 `no_sync` 参数，默认是安全的。生产环境不应设置此字段 |

与 safekeeper 一样，pageserver **默认开启 fsync**，无需显式配置。`no_sync = true` 仅在测试环境使用。

#### 2.2.3 数据持久性与缓存

| 配置区 | 关键参数 | 默认值 | 生产建议 |
|--------|---------|--------|----------|
| 页面缓存 | `page_cache_size` | 8192 页 | 根据内存调整，建议 32768+ |
| 文件描述符 | `max_file_descriptors` | 100 | ⚠️ **严重偏低**，生产建议 1000+ |
| 远程存储 | `remote_storage` | 无 | ✅ 已配置 S3 |

`max_file_descriptors = 100` 在生产环境下可能不足。Neon 官方建议至少 1000。

#### 2.2.4 磁盘驱逐 (Disk Usage Eviction)

```toml
[disk_usage_based_eviction]
enabled = true           # 默认启用
max_usage_pct = 80       # 磁盘使用率超过 80% 触发驱逐
min_avail_bytes = 2000000000  # 或可用空间低于 2GB
period = "60s"           # 每 60 秒检查一次
```

**生产级调整建议**：

| 场景 | `max_usage_pct` | `min_avail_bytes` | 理由 |
|------|:---:|------|------|
| 500GB NVMe | 75% | 20GB | 留足合并临时空间 |
| 1TB NVMe | 80% | 50GB | 大容量磁盘，阈值可稍高 |
| 2TB+ NVMe | 85% | 100GB | 大容量容忍度更高 |

驱逐任务会删除已上传 S3 的本地 Layer 文件，数据安全性由 S3 保证。

#### 2.2.5 租户默认配置 (tenant_config)

当前 operator 未配置任何 `tenant_config`，全部使用源码默认值。以下是生产环境需关注的参数：

| 参数 | 默认值 | 影响 | 生产建议 |
|------|--------|------|----------|
| `checkpoint_distance` | 256 MiB | L0 层大小，影响写放大 | 默认可用，大写入量场景调至 512MiB |
| `checkpoint_timeout` | 10 min | 最大检查点间隔 | 默认可用 |
| `compaction_target_size` | 128 MiB | L1 层大小 | 默认可用 |
| `compaction_threshold` | 10 | 触发合并的 L0 层数 | 默认可用 |
| `gc_horizon` | 64 MiB | WAL 垃圾回收保留量 | 默认可用 |
| `gc_period` | 1 hr | 垃圾回收间隔 | 默认可用 |
| `pitr_interval` | 7 days | PITR 时间窗口 | 根据业务需求调整 |
| `walreceiver_connect_timeout` | 10 s | 连接 safekeeper 超时 | 默认可用 |
| `max_lsn_wal_lag` | 1 GiB | safekeeper 间最大差距 | 高写入场景可调至 2-5 GiB |

#### 2.2.6 认证 (Authentication) — ✅ 已实现 NeonJWT

| 参数 | 当前 Operator |
|------|---------------|
| `http_auth_type` | ✅ `NeonJWT`（已配置） |
| `pg_auth_type` | ✅ `NeonJWT`（已配置） |
| `auth_validation_public_key_path` | ✅ `/certs/public.pem`（通过 Secret Volume 挂载） |
| `control_plane_api_token` | ✅ `pageserver_control_plane_token`（持久化到 Secret，scope: `generations_api`）|
| SK 认证 token | ✅ `pageserver_safekeeper_token`（持久化到 Secret，scope: `safekeeperdata`）|

JWT token 通过 `ensurePSAuthTokens()` 自动管理：支持过期检测、SHA256 确定性重新生成、Secret 持久化。JWT public key 通过 Volume 挂载到 PS Pod 的 `/certs/public.pem`。

#### 2.2.7 并发与性能

| 参数 | 默认值 | 生产建议 |
|------|--------|----------|
| `concurrent_tenant_warmup` | 8 | 默认可用，大量 tenant 时可增至 16 |
| `ingest_batch_size` | 100 | 默认可用 |
| `image_compression` | `Zstd(level=1)` | 默认可用，低速磁盘可调至 level=3 |
| `background_task_maximum_delay` | 10s | 默认可用 |
| `metric_collection_interval` | 10min | 建议配合 Prometheus scrape interval |

#### 2.2.8 控制平面 (Control Plane / Storage Controller)

| 参数 | 当前配置 | 说明 |
|------|---------|------|
| `control_plane_api` | ✅ 已配置 | `http://{cluster}-sc.{ns}:8888/upcall/v1/` |
| `control_plane_api_token` | ✅ 已配置 | 持久化到 Secret `pageserver_control_plane_token`，scope: generations_api |
| `control_plane_emergency_mode` | 未配置 (默认 false) | 紧急模式，不需要控制平面 |

### 2.3 健康端点

Pageserver 通过 HTTP API 暴露以下关键端点：

| 端点 | 方法 | 用途 |
|------|------|------|
| `/v1/status` | GET | 健康检查（返回 200 表示服务运行中） |
| `/v1/tenant/{tenant_id}/status` | GET | 单个租户状态 |
| `/v1/tenant` | GET | 所有租户列表 |
| `/metrics` | GET | Prometheus 指标 |
| `/v1/utilization` | GET | 磁盘利用率 |

**`/v1/status` 响应格式**：
```json
{
  "id": 0,
  "state": "active",
  "activity": {
    "status": "Normal"
  }
}
```

- `state`: `active` / `waiting` (尚未完成初始化)
- `activity.status`: `Normal` / `InitialLoading` / `InitialLoadingFailed`

### 2.4 LocationConfig API：Tenant 附着/分离核心协议

> 这是 pageserver 与 Storage Controller 之间最核心的 API，理解它对设计 Operator 的 pageserver 生命周期管理至关重要。

#### 2.4.1 API 定义

**文件**: `neon/pageserver/src/http/routes.rs:4057-4058`

```
PUT /v1/tenant/:tenant_shard_id/location_config
```

**Request Body** (`TenantLocationConfigRequest`, `libs/pageserver_api/src/models.rs:1380-1385`):

```json
{
  "mode": "AttachedSingle",
  "generation": 5,
  "secondary_conf": null,
  "shard_number": 0,
  "shard_count": 1,
  "shard_stripe_size": 0,
  "tenant_conf": {
    "checkpoint_distance": "256 MB",
    "compaction_threshold": 10,
    "gc_horizon": "64 MB",
    "pitr_interval": "7 days"
  }
}
```

**Handler 逻辑** (`routes.rs:1993-2065`):

```
put_tenant_location_config_handler():
  1. 解析 tenant_shard_id, flush_ms, lazy 参数
  2. 权限检查（需要 tenant 级别 JWT scope）
  3. 如果 mode == Detached:
     → 调用 tenant_manager.detach_tenant() — 从内存和磁盘删除本地数据
     → 幂等操作（重复调用不报错）
  4. 否则:
     → 将 LocationConfig 转为内部 LocationConf
     → 根据 lazy 参数决定 SpawnMode（是否延迟加载）
     → 调用 tenant_manager.upsert_location() — 执行实际的 position 变更
```

#### 2.4.2 LocationConfigMode 五种模式

**文件**: `libs/pageserver_api/src/models.rs:1328-1335`

| 模式 | SC 侧含义 | Pageserver 侧行为 |
|------|----------|-------------------|
| `AttachedSingle` | 该 shard 的唯一主附着点 | 可读写，可执行删除操作（drop tenant/timeline） |
| `AttachedMulti` | 迁移目标端（多个附着点之一） | 可读写，但**不执行删除操作**（保护源端数据） |
| `AttachedStale` | 迁移源端（已过期） | 只读，**不上传、不删除、不发送账单**，等待被 detach |
| `Secondary` | 只读热备副本 | 从 S3 下载并保持缓存同步（`warm: true`），不接收写入 |
| `Detached` | 已分离，本地无数据 | 本地数据被删除，pageserver 放弃该 shard |

**状态流转**（SC 控制的典型场景）：

```
新 Tenant 创建:
  SC → PS: PUT /v1/tenant/:id/location_config  {mode: "AttachedSingle", gen: 1}
          PS: 创建本地目录，开始接收 WAL

迁移 Tenant:
  SC → PS-target: PUT ...  {mode: "AttachedMulti", gen: 2}
          PS-target: 从 S3 下载数据，激活为第二个附着点
  SC → PS-source: PUT ...  {mode: "AttachedStale"}
          PS-source: 停止上传，停止删除，等待迁移完成
  SC → PS-source: PUT ...  {mode: "Detached"}
          PS-source: detach_tenant() — 删除本地目录和内存状态

添加 Secondary:
  SC → PS: PUT ...  {mode: "Secondary", secondary_conf: {warm: true}}
          PS: 从 S3 下载数据到本地缓存，不接收 WAL
```

#### 2.4.3 LocationConf 内部表示

**文件**: `neon/pageserver/src/tenant/config.rs:56-73`

```rust
pub(crate) struct LocationConf {
    pub(crate) mode: LocationMode,       // Attached / Secondary
    pub(crate) shard: ShardIdentity,     // shard_number, shard_count, stripe_size
    pub(crate) tenant_conf: TenantConfig, // 跨所有位置共享的租户级配置
}
```

**关键设计**: `tenant_conf` 在 `LocationConfig` 中是一个扁平字段，但在 pageserver 内部，它是跨所有位置**共享**的。即：同一 tenant 的不同 shard、不同 location 共享相同的 `checkpoint_distance`、`gc_horizon` 等配置。

#### 2.4.4 对 Operator 设计的影响

1. **Operator 不应直接调用此 API**：此 API 是 SC ↔ PS 之间的内部协议，由 SC Reconciler 驱动
2. **Operator 的职责是确保 PS Pod 存活**：PS 存活 → SC 心跳正常 → SC Reconciler 自动管理 location_config
3. **故障场景中的使用**：当 PS 本地盘丢失时，`empty_local_disk=true` 触发 SC 清除 observed locations → Reconciler 重新下发 location_config → PS 从 S3 冷启动

#### 2.4.5 关键源码引用

| 引用 | 说明 |
|------|------|
| `neon/pageserver/src/http/routes.rs:1993-2065` | `put_tenant_location_config_handler` 实现 |
| `neon/pageserver/src/http/routes.rs:4057-4058` | 路由注册 |
| `neon/libs/pageserver_api/src/models.rs:1328-1335` | `LocationConfigMode` 枚举定义 |
| `neon/libs/pageserver_api/src/models.rs:1344-1368` | `LocationConfig` 结构体 |
| `neon/libs/pageserver_api/src/models.rs:1380-1385` | `TenantLocationConfigRequest` 请求体 |
| `neon/pageserver/src/tenant/config.rs:18-31` | 内部 `AttachmentMode` (Single/Multi/Stale) |
| `neon/pageserver/src/tenant/config.rs:48-73` | 内部 `LocationMode` 和 `LocationConf` |

---

### 2.5 Tenant 状态机

> 理解 pageserver 内部的 tenant 状态机，有助于设计 Operator 的状态监控和故障诊断逻辑。

#### 2.5.1 TenantState 定义

**文件**: `neon/libs/pageserver_api/src/models.rs:60-107`

五个状态（上游 mermaid 状态图位于注释行 28-47）：

```
[*] ──spawn_attach()──▶ Attaching ──attach成功──▶ Activating ──infallible──▶ Active
                             │                                                │
                             │ attach失败                                      │ set_stopping()
                             ▼                                                ▼
                          Broken                                           Stopping
                             │                                                │
                             │ ignore/detach                                   │ remove_from_memory 完成
                             ▼                                                ▼
                           [*]                                              [*]
```

| 状态 | 含义 | Operator 应如何感知 |
|------|------|-------------------|
| `Attaching` | 正在从 S3 加载/恢复 | PS HTTP API 可达，但 tenant 不可用 |
| `Activating` | Attaching→Active 过渡态 | 瞬时状态，通常持续 < 1s |
| `Active` | 正常运行，可读写 | `/v1/tenant/:id/status` 返回 active |
| `Stopping` | 正在关闭（detach/shutdown 中） | Progress Barrier 阻塞，等待清理完成 |
| `Broken` | 失败状态，不可用 | 需要 SC 干预：重新调度或标记 Detached |

#### 2.5.2 TenantShard 和 TenantSlot

**文件**: `neon/pageserver/src/tenant/mgr.rs:70-98, 301-313`

```rust
// TenantManager 管理所有 tenant
pub struct TenantManager {
    tenants: RwLock<TenantsMap>,   // Initializing → Open → ShuttingDown
    // ...
}

// 每个 tenant 在 map 中的三种可能形态
pub(crate) enum TenantSlot {
    Attached(Arc<TenantShard>),      // 完整 Attached（Active/WarmingUp 等）
    Secondary(Arc<SecondaryTenant>),  // 仅本地缓存预热
    InProgress(Barrier),             // 正在变更中（attach/detach 进行中）
}
```

#### 2.5.3 对 Operator 设计的影响

1. **`/v1/status` 总是返回 200**：即使 tenant 全部 Broken，PS 本身仍存活
2. **Tenant 状态不是 Pod 级别的健康指标**：Pod 的 Readiness 不应依赖 tenant 状态
3. **监控 tenant 状态是运维需求**：Operator 可通过查询 `/v1/tenant` 获取所有 tenant 状态，用于诊断
4. **Broken tenant 不会自动恢复**：需要 SC Reconciler 检测并重新下发 location_config

#### 2.5.4 关键源码引用

| 引用 | 说明 |
|------|------|
| `neon/libs/pageserver_api/src/models.rs:60-107` | `TenantState` 枚举 |
| `neon/pageserver/src/tenant/mgr.rs:70-98` | `TenantSlot` 枚举 |
| `neon/pageserver/src/tenant/mgr.rs:301-313` | `TenantManager` 结构体 |
| `neon/pageserver/src/tenant/mgr.rs:842-1149` | `upsert_location()` 核心逻辑 |
| `neon/pageserver/src/tenant/mgr.rs:2001-2065` | `detach_tenant()` 分离逻辑 |

---

### 2.6 冷启动与 Warmup 流程

> Pageserver 节点重启后的加载过程，对理解 StartupProbe 时长设计和 SC 交互至关重要。

#### 2.6.1 启动阶段

**文件**: `neon/pageserver/src/bin/pageserver.rs:537-653`

```
Phase 1: "initial"                    STARTUP_IS_LOADING = 1
  ├── HTTP 服务器启动（/v1/status 立即可用）
  ├── Broker 连接建立
  └── 认证模块初始化

Phase 2: "initial_tenant_load_remote"
  ├── 扫描 S3 桶，发现所有属于本节点的 tenant shard
  ├── 下载 index_part.json（每个 tenant 的元数据索引）
  └── 创建 Timeline 对象和 RemoteTimelineClient

Phase 3: "initial_tenant_load"        STARTUP_IS_LOADING = 0
  ├── 加载本地 /data/.neon/tenants 中的已有 tenant
  ├── Tenant::spawn() → Attaching → Activating → Active
  └── 超时保护: background_task_maximum_delay（默认 10s）

Phase 4: "background_jobs_can_start"
  ├── compaction、GC、image creation 等后台任务启动
  └── pageserver 完全就绪
```

**关键设计点**：
- HTTP 服务器在 Phase 1 就启动 → K8s LivenessProbe 可以很早通过
- Phase 2-3 可能很慢（取决于 S3 网络和 tenant 数量）
- `concurrent_tenant_warmup`（默认 8）控制并发加载数
- 超时后未完成加载的 tenant 会被跳过，进入惰性加载模式

#### 2.6.2 Re-Attach 协议

Pageserver 启动后立即调用（Phase 1 完成后）：

```
POST /upcall/v1/re-attach
  body: {
    node_id: <NodeId>,                      // 来自 identity.toml
    register: {                              // 来自 metadata.json
      node_id,
      listen_http_addr, listen_http_port,
      listen_pg_addr, listen_pg_port,
      availability_zone_id
    },
    empty_local_disk: <bool>                 // /data/.neon/tenants 是否为空
  }

SC 处理:
  1. node_register() → registration_match() 校验地址一致性
  2. 递增所有相关 shard 的 generation number
  3. 返回该节点应配置的 tenant 列表（LocationConfig 数组）
  4. 标记节点状态: WarmingUp
```

**`empty_local_disk` 的重要性**：
- `true`：PS 本地盘为空（新 PVC 或已清理）→ SC 清除 observed locations → Reconciler 重新配置
- `false`：PS 本地盘有数据 → SC 保持 observed locations → 快速恢复

#### 2.6.3 对 Operator 设计的影响

1. **StartupProbe 300s 是合理的**：覆盖了 Phase 2-3 的最大允许时间
2. **首次 PS 启动不需要额外操作**：PS 自动执行 re-attach → SC 自动下发 tenant 配置
3. **PVC 删除后重建是安全的**：SC 通过 `handle_ps_local_disk_loss` 标志正确处理
4. **冷启动性能取决于 S3 网络**：需要足够的 S3 带宽和并发连接

#### 2.6.4 关键源码引用

| 引用 | 说明 |
|------|------|
| `neon/pageserver/src/bin/pageserver.rs:537-653` | 启动阶段定义 |
| `neon/pageserver/src/bin/pageserver.rs:547` | `STARTUP_IS_LOADING.set(1)` |
| `neon/pageserver/src/bin/pageserver.rs:637` | `STARTUP_IS_LOADING.set(0)` |
| `neon/storage_controller/src/service.rs:2377-2533` | `re_attach` 处理逻辑 |
| `neon/pageserver/src/tenant/mgr.rs:842-1149` | `upsert_location()` 处理 SC 返回的配置 |

---

### 2.7 Tenant 数据目录结构

> 理解 pageserver 的磁盘布局有助于设计 PVC 大小估算、备份策略和故障恢复。

#### 2.7.1 目录布局

```
/data/.neon/
├── identity.toml              # NodeId（跨重启不变）
├── pageserver.toml            # 配置文件
├── metadata.json              # 注册信息（host, port, AZ）
└── tenants/                   # PVC 挂载点
    └── <tenant_id>-<shard_number>-<shard_count>/
        ├── tenant_config.json  # 持久化的 tenant 配置
        ├── <timeline_id>/
        │   ├── metadata.json   # timeline 元数据
        │   └── layers/         # Layer 文件（本地缓存）
        │       ├── L0_*.layer   # L0 层（WAL 接收产生）
        │       └── L1_*.layer   # L1+ 层（Compaction 产生）
        └── ... (其他 timeline)
```

#### 2.7.2 index_part.json（远程存储）

**文件**: `neon/pageserver/src/tenant/remote_timeline_client.rs`

这是 S3 中每个 tenant 的元数据索引文件，包含：
- 所有 Layer 文件的列表（key, size, generation）
- 所有 Timeline 的元数据
- Generation number 用于防止冲突

**冷启动时**：PS 先下载 `index_part.json`，再按需下载具体的 Layer 文件。

#### 2.7.3 存储估算

```
PVC 大小估算:
  = (active tenant shard count) × (avg checkpoint_distance × compaction_threshold + avg compaction_target_size × layer_depth)
  + wal_redo 缓冲区
  + 20% 安全余量

建议:
  - 开发环境: 50Gi ~ 100Gi
  - 生产环境: 500Gi ~ 2Ti（取决于 tenant 数量和写入速率）
  - 本地盘仅作缓存，S3 是唯一持久化存储
```

#### 2.7.4 关键源码引用

| 引用 | 说明 |
|------|------|
| `neon/pageserver/src/tenant/mgr.rs:355` | `empty_local_disk` 检测逻辑 |
| `neon/pageserver/src/tenant/remote_timeline_client.rs` | index_part.json 处理 |
| `neon/pageserver/src/tenant.rs:1354-1449` | `TenantShard::spawn()` 加载流程 |

---

## 3. 生产级差距分析

### 3.1 差距总览

**与存储控制器的职责分工**：Storage Controller 已内置完整的故障检测、自动调度、generation number 数据安全机制。Operator 的职责是确保 K8s 环境正确适配 SC 的要求，详见 [4.5 节](#45-节点故障与-pageserver-迁移机制-深度分析)。

**CRD 架构决策**：Pageserver 数量通过 `Cluster.Spec.NumPageservers` 声明式管理（与 Safekeeper 模式一致），Cluster Controller 创建 PS CRD，PS Controller 管理 Pod 生命周期。详见 [1.5 节](#15-crd-架构决策-pageserver-的数量管理方式)。

```
┌──────────────────────────────────────────────────────────────────┐
│                    Pageserver 生产级差距                           │
├──────────┬───────────────────────────┬────────┬──────────────────┤
│ 优先级    │ 问题                       │ 影响    │ 当前状态          │
├──────────┼───────────────────────────┼────────┼──────────────────┤
│ ✅ 已实现 │ 健康探针 (Liveness/Readiness/Startup) │ — │ 已实现（见 4.1）   │
│ ✅ 已实现 │ 探针可配置化 (ProbeConfig)    │ — │ 已实现（见 4.1.8）      │
│ ✅ 已实现 │ Resource Requests/Limits  │ — │ 已实现，默认 CPU=500m/2, Mem=256Mi/512Mi，可通过 Spec.Resources 覆盖│
│ ✅ 已实现 │ PodAntiAffinity (Node 级别)  │ — │ 已实现 (statefulset.go) │
│ 🔴 P0    │ 无 AZ 级拓扑分布约束         │ 所有 PS 可能同 AZ │ 完全缺失（见 4.3 节）   │
│ 🟡 P1    │ metadata.json AZ 硬编码      │ SC AZ 调度未激活  │ Spec.AvailabilityZone 字段已存在，默认仍为 "se-ume"（见 4.3 节）│
│ ✅ 已实现 │ PDB (PodDisruptionBudget)    │ — │ 已实现（specs/pageserver/pdb.go，maxUnavailable=1）│
│ ✅ 已实现 │ 本地盘 PVC 粘滞处理         │ — │ handleNodeFailure 已实现，支持 AutoRecover（见 4.5 节）│
│ 🔴 P0    │ max_file_descriptors = 100 │ IO 性能瓶颈  │ 未配置（用默认值）     │
│ ✅ 已实现 │ 安全下线 (drain)            │ — │ 四阶段 finalize 已实现（见 1.4/4.6 节）│
│ ✅ 已实现 │ ConfigMap 生产调优参数       │ — │ 认证/Broker/S3/磁盘驱逐/tenant_config 均已配置（见 1.3 节）│
│ 🟡 P1    │ 无 Prometheus 监控集成      │ 无可观测性   │ 完全缺失              │
│ 🟡 P1    │ 无 SC 注册验证              │ 注册失败无感知│ 部分（syncSCState 同步节点状态）│
│ 🟡 P1    │ S3 凭证无轮换支持           │ 凭证过期宕  │ 完全缺失              │
│ ✅ 已实现 │ JWT Token 管理              │ — │ ensurePSAuthTokens 已实现（确定性生成+过期检测+持久化）│
│ 🟢 P2    │ 无法动态增减 Pageserver     │ 弹性不足    │ Types 已定义（NumPageservers），reconcile 逻辑待实现│
│ 🟢 P2    │ RUST_LOG=debug (生产)      │ 日志量巨大   │ 已配置但有隐患          │
│ 🟢 P2    │ 无备份完整性验证            │ 数据风险    │ 未规划                │
│ 🟢 P2    │ ImagePullPolicy: Always    │ 每次重建拉镜像│ 可优化              │
│ 🟢 P2    │ SC handle_ps_local_disk_loss 未确认 │ 冷启动行为不确定│ 需 SC 配置        │
│ 🟢 P2    │ 缺少 PS HTTP API 集成层     │ 运维能力受限 │ 未实现（见 4.12）       │
│ 🟢 P2    │ 缺少 Tenant 生命周期监控     │ 故障定位困难 │ 未实现（见 4.13）       │
└──────────┴───────────────────────────┴────────┴──────────────────┘
```

### 3.2 与 Safekeeper 生产级实现对比

Safekeeper 已在下述方面达到生产级，Pageserver 可参考：

| 功能 | Safekeeper | Pageserver |
|------|:----------:|:----------:|
| LivenessProbe | ✅ `/v1/status` | ✅ `/v1/status`（已实现） |
| ReadinessProbe | ✅ `/v1/status` | ✅ `/v1/status`（已实现） |
| StartupProbe | ✅ `/v1/status` (60s) | ✅ `/v1/status` (300s)（已实现） |
| PodAntiAffinity (Node) | ✅ RequiredDuringScheduling | ✅ RequiredDuringScheduling (已实现) |
| topologySpreadConstraints (AZ) | N/A (SK 不需要 AZ 感知) | ❌ (完全缺失，见 4.3 节) |
| metadata.json AZ 真实值 | N/A | 🔶 Spec.AvailabilityZone 字段已存在，默认仍硬编码 "se-ume" |
| PDB | ✅ maxUnavailable=1 | ✅ maxUnavailable=1（已实现） |
| Resource Requests/Limits | ✅ CPU/Memory | ✅ CPU=500m/2, Mem=256Mi/512Mi（已实现） |
| TerminationGracePeriod | ✅ 30s | ✅ 60s（已实现） |
| SC 注册 | ✅ Upsert API | ✅ re-attach 自动注册 |
| 安全注销 (Decommission) | ✅ | ✅ 四阶段 finalize（已实现：drain → monitor → delete → removeFinalizer） |
| **节点故障恢复** | N/A | ✅ handleNodeFailure + AutoRecover（已实现） |
| 故障转移 (SC 调度) | N/A (safekeeper 不调度) | ✅ SC 内置 |
| **Cluster 自动创建** | ✅ `NumSafekeepers` → OwnerReference | 🔜 Types 已定义（`NumPageservers`+`PageserverConfig`），reconcile 逻辑待实现 |
| **安全下线** | ✅ decommission → SC 移除 | ✅ drain → SC 迁移 shard → 删除（已实现） |
| **JWT Token 管理** | ✅ | ✅ ensurePSAuthTokens + 确定性生成 + 过期检测（已实现） |
| **服务多 Tenant** | ✅ 该 Cell 的所有 Timeline | ✅ 该 Cell 的所有 Tenant Shard |

**关键差异**：
- Safekeeper 是主动向 SC 注册（Upsert API），Pageserver 是启动时通过 re-attach 自动注册
- Pageserver 的故障转移由 SC 自动完成（心跳检测 → demote → reschedule → reconcile），Operator 只需确保 pod 能正常重启
- Pageserver 的地址稳定性依赖 K8s ClusterIP DNS（已在当前实现中正确处理）
- **Pageserver 缩容比 Safekeeper 复杂**：需要先 drain tenant（等待 SC 迁移 shard），而非直接 decommission。但 finalize 流程已完整实现
- **Pageserver 天然绑定到单一 Cell**：1 PS 只能注册 1 SC。不存在跨 Cluster 共享 Pageserver 的机制。每个 PS 服务该 Cell 内的多个 Tenant（cell 内多租户）
- **集群级扩缩容**：`Cluster.Spec.NumPageservers` 和 `PageserverConfig` 类型已定义，`Cluster Controller` 的 `reconcilePageservers()` 逻辑待实现

---

## 4. 详细设计方案

### 4.1 P0-1: 健康探针 (Health Probes) — 深度分析与设计验证

> **状态更新 (2026-06)**：Pageserver 的三种探针（Liveness/Readiness/Startup）已在 `specs/pageserver/statefulset.go` 中完整实现。当前分析重点是：验证现有实现是否符合上游 Neon 的设计意图，识别剩余差距。

#### 4.1.1 上游 Neon 健康检查体系（深度源码分析）

##### 4.1.1.1 Pageserver `/v1/status` 端点

**源码**：`neon/pageserver/src/http/routes.rs:549-557`

```rust
async fn status_handler(
    request: Request<Body>,
    _cancel: CancellationToken,
) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    let config = get_config(&request);
    json_response(StatusCode::OK, StatusResponse { id: config.id })
}
```

**关键特征**：
- **总是返回 HTTP 200**。不做任何内部健康检查——不检查数据库、存储连接、租户状态
- **响应体**：`{"id": <NodeId>}`（`NodeId` 是 `u64` 包装类型）
- **认证白名单**：`/v1/status` 在 auth 白名单中（`routes.rs:136`），JWT 启用时也无需认证
- **何时可用**：HTTP 服务器启动后立即可用——**在租户从 S3 加载完成之前**

##### 4.1.1.2 Pageserver `/v1/utilization` 端点（SC 心跳目标）

**源码**：`neon/pageserver/src/http/routes.rs:3219-3267` + `neon/pageserver/src/utilization.rs`

SC **不使用 `/v1/status` 做心跳检测**，而是使用 `/v1/utilization`：

```rust
async fn get_utilization(...) -> Result<Response<Body>, ApiError> {
    let state = get_state(&r);
    let mut g = state.latest_utilization.lock().await;
    // regenerate at most 1Hz
    if !still_valid {
        let doc = crate::utilization::regenerate(state.conf, path, &state.tenant_manager)
            .map_err(ApiError::InternalServerError)?;
    }
}
```

**`utilization::regenerate()` 执行的实际操作**：
- 调用 `statvfs` 获取磁盘使用情况
- 遍历所有租户，汇总 `disk_wanted_bytes` 和 shard 数量
- 序列化为 `PageserverUtilization` JSON

**响应体**：
```json
{
    "disk_usage_bytes": 123456789,
    "free_space_bytes": 987654321,
    "disk_wanted_bytes": 50000000,
    "disk_usable_pct": 90,
    "shard_count": 42,
    "max_shard_count": 2500,
    "utilization_score": 500000,
    "captured_at": "2024-02-21T10:02:59.000Z"
}
```

**为什么 SC 用 `/v1/utilization` 而不是 `/v1/status`**：
- `/v1/utilization` 执行真实的系统调用（`statvfs`）——如果磁盘 I/O 阻塞，请求会失败
- `/v1/status` 只是纯内存操作，进程卡死但 HTTP loop 存活时仍返回 200
- `/v1/utilization` 是"深度健康检查"，`/v1/status` 只是"进程存活检查"

##### 4.1.1.3 Storage Controller 心跳参数

**源码**：`neon/storage_controller/src/service.rs:127-137` + `neon/storage_controller/src/heartbeater.rs`

| 参数 | 默认值 | 说明 |
|------|:---:|------|
| `max_offline_interval` | **30s** | 节点无心跳响应的最长容忍时间（超过后标记 Offline） |
| `max_warming_up_interval` | **300s** | Pageserver 重启/Cold start 期间的宽限时间 |
| `heartbeat_interval` | **5s** | SC 向每个节点发送心跳的间隔 |

**Pageserver 心跳状态机**（`heartbeater.rs:214-227`）：
```
get_utilization() 成功 → PageserverState::Available { last_seen_at, utilization }
get_utilization() 失败 + 节点处于 WarmingUp → PageserverState::WarmingUp { started_at }
get_utilization() 失败 + 非 WarmingUp → PageserverState::Offline
```

**Safekeeper 心跳状态机**（`heartbeater.rs:320-448`）：
```
get_utilization() 成功 → SafekeeperState::Available { last_seen_at, utilization }
get_utilization() 失败 → SafekeeperState::Offline
```
> Safekeeper 没有 `WarmingUp` 状态——启动快，无需长宽限

##### 4.1.1.4 Storage Controller 自身的健康端点

**源码**：`neon/storage_controller/src/http.rs:1621-1672`

SC 有三个专用的健康端点（被 Neon 的 K8s 部署使用）：

| 端点 | 用途 | 行为 |
|------|------|------|
| `GET /status` | **K8s startup probe** | 总是返回 200 `{}`，HTTP 服务器启动即可 |
| `GET /live` | **K8s liveness probe** | 检查 `startup_complete` AND `is_leader` → 200，否则 503 |
| `GET /ready` | **K8s readiness probe** | 检查 `startup_complete` → 200，否则 503 |

非 leader 的 SC 实例只允许访问 `/ready`、`/status`、`/metrics`，其他请求返回 503（`http.rs:1829-1868`）。

##### 4.1.1.5 Pageserver 启动阶段与冷启动

**源码**：`neon/pageserver/src/bin/pageserver.rs:537-653`

```
Phase 1: "initial"              STARTUP_IS_LOADING = 1   HTTP 服务器启动 ✓
Phase 2: "initial_tenant_load_remote"                    从 S3 下载租户元数据
Phase 3: "initial_tenant_load"   STARTUP_IS_LOADING = 0  本地加载完成
Phase 4: "background_jobs_can_start"                     后台任务开始(compaction/GC)
```

**关键设计**：
- Pageserver 的 HTTP 服务器在 Phase 1 就启动——此时 `/v1/status` 立即可用
- Phase 2-3 有超时保护（`background_task_maximum_delay`），即使超时也会继续启动
- `/v1/status` 在 Phase 1-3 期间都返回 200——它不反映租户加载状态
- `STARTUP_IS_LOADING` 是一个 Prometheus gauge（0=完成，1=加载中），可被 Prometheus 监控

#### 4.1.2 当前 neon-operator 实现验证

**已实现**：`specs/pageserver/statefulset.go:144-183`

```go
// 三种探针全部使用 /v1/status HTTP 端点（端口 9898）
LivenessProbe:   { path: "/v1/status", initialDelay: 30s, period: 10s, timeout: 5s, failure: 3 }
ReadinessProbe:  { path: "/v1/status", initialDelay: 10s, period: 5s,  timeout: 3s, failure: 2 }
StartupProbe:    { path: "/v1/status", initialDelay: 10s, period: 10s, timeout: 5s, failure: 30 }
```

#### 4.1.3 参数对齐分析：K8s 探针 vs SC 心跳参数

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        K8s 探针 vs SC 心跳 参数对齐                        │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  K8s LivenessProbe                     SC max_offline_interval            │
│  ┌─────────────────────────┐           ┌───────────────────────┐         │
│  │ failureThreshold: 3     │           │ default: 30s          │         │
│  │ periodSeconds: 10       │    对齐    │                       │         │
│  │ → 最多 ~30s 检测失败    │ ◄──────── │ SC 在 30s 无心跳后    │         │
│  │ → K8s 重启 Pod          │           │ 标记节点 Offline       │         │
│  └─────────────────────────┘           └───────────────────────┘         │
│                                                                          │
│  K8s StartupProbe                      SC max_warming_up_interval        │
│  ┌─────────────────────────┐           ┌───────────────────────┐         │
│  │ failureThreshold: 30    │           │ default: 300s         │         │
│  │ periodSeconds: 10       │    对齐    │                       │         │
│  │ → 最多 300s 冷启动窗口  │ ◄──────── │ SC 在 300s 内等待     │         │
│  └─────────────────────────┘           │ pageserver 完成启动    │         │
│                                        └───────────────────────┘         │
│  K8s ReadinessProbe                    SC heartbeat_interval             │
│  ┌─────────────────────────┐           ┌───────────────────────┐         │
│  │ periodSeconds: 5        │    对齐    │ default: 5s           │         │
│  └─────────────────────────┘           └───────────────────────┘         │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
```

**结论**：K8s 探针参数与 SC 心跳参数**高度对齐**，这是一致性设计的有力证据。

#### 4.1.4 `/v1/status` vs `/v1/utilization` 作为探针端点的争议分析

| 维度 | `/v1/status`（当前使用） | `/v1/utilization`（备选） |
|------|:---:|:---:|
| **认证要求** | 无需 JWT（白名单） | 需要 JWT（非白名单） |
| **系统调用** | 无（纯内存） | `statvfs` + 租户遍历 |
| **检测能力** | 仅检测 HTTP 服务器存活 | 检测磁盘 I/O 阻塞 |
| **可用时机** | HTTP 服务器启动后立即 | HTTP 服务器启动后立即 |
| **上游 SC 使用** | 不使用（仅作 human check） | **心跳检测目标** |
| **CPU 开销** | 几乎为零 | 有（1Hz 限速，`statvfs`） |

**当前使用 `/v1/status` 的决定是正确的**，理由：

1. **认证无依赖**：`/v1/status` 在 auth 白名单中，无论 JWT 是否启用都能正常工作
2. **SC 心跳机制是真正的健康检查**：SC 每 5s 通过 `/v1/utilization` 做深度心跳。K8s 探针的职责是**检测进程卡死**（进程级别），而非业务健康（业务级别）
3. **分层清晰**：
   - K8s LivenessProbe → 进程卡死 → 重启 Pod → SC 检测到重启 → WarmingUp 状态 → 重新调度 shard
   - SC Heartbeat → 业务不可用 → 标记 Offline → 触发 shard 调度
4. **`/v1/utilization` 在生产环境需要 JWT**：在 `--dev` 模式移除后，`/v1/utilization` 需要有效的 JWT token，K8s probe 无法直接提供

#### 4.1.5 剩余差距（已全部解决）

> **状态更新 (2026-06-29)**：P0 和 P1 项均已实现。以下为历史记录和最终状态。

| 差距 | 严重程度 | 状态 | 说明 |
|------|:---:|:---:|------|
| **SC Deployment 无健康探针** | 🔴 高 | ✅ 已解决 | `specs/storagecontroller/deployment.go` 已添加 StartupProbe (`/status`)、LivenessProbe (`/live`)、ReadinessProbe (`/ready`) |
| **探针参数硬编码** | 🟡 中 | ✅ 已解决 | `PageserverSpec` 和 `SafekeeperSpec` 已添加 `LivenessProbe`/`ReadinessProbe`/`StartupProbe` 可选的 `*ProbeConfig` 字段 |
| **`/v1/status` 不反映租户加载状态** | 🟡 中 | 🟢 无需修复 | 这是上游 Neon 的 intentional design，且 StartupProbe 覆盖了冷启动窗口 |
| **无 `/v1/live` `/v1/ready` 端点** | 🟢 低 | 🟢 无需修复 | Pageserver 只有 `/v1/status`，这是上游 Neon 的设计，operator 无需引入新端点 |

#### 4.1.6 四级健康检查层次模型

```
Layer 1: K8s StartupProbe    ─── 覆盖冷启动窗口（300s）
        │                        /v1/status → HTTP 200
        │                        失败 → 不触发任何重启，仅阻塞 Liveness/Readiness
        │
Layer 2: K8s LivenessProbe   ─── 检测进程卡死（~30s 内）
        │                        /v1/status → HTTP 200
        │                        失败 → kill & restart Pod → SC 检测到重启
        │
Layer 3: K8s ReadinessProbe  ─── 流量就绪信号（~10s 内）
        │                        /v1/status → HTTP 200
        │                        失败 → Pod 从 Service Endpoint 移除
        │
Layer 4: SC Heartbeat        ─── 深度业务健康（每 5s）
                                 /v1/utilization → statvfs+tenant stats
                                 失败 → SC 标记 Offline → shard 迁移调度
```

#### 4.1.7 Storage Controller Deployment 健康探针（✅ 已实现）

> **已实现 (2026-06-29)**：`specs/storagecontroller/deployment.go` 中已添加三种探针，参数如下：

| 探针 | 端点 | 参数 | 说明 |
|------|------|------|------|
| **StartupProbe** | `GET /status` | initialDelay=5s, period=10s, failure=6 (~60s) | SC 启动快（无冷启动），60s 内 HTTP 服务应启动 |
| **LivenessProbe** | `GET /live` | initialDelay=10s, period=10s, timeout=5s, failure=3 | `/live` 检查 `startup_complete AND is_leader`，非 leader 返回 503 |
| **ReadinessProbe** | `GET /ready` | initialDelay=5s, period=5s, timeout=3s, failure=2 | `/ready` 仅检查 `startup_complete` |

**注意**：在 operator 的单实例部署模式（LeaderElection=false）下，`/live` 的 `is_leader` 检查总是通过。当未来启用 Leader Election 时，`/live` 会正确区分 leader/follower。

> **已实现**。
> - 代码：`specs/storagecontroller/deployment.go` → `Deployment()` 函数
> - 测试：`specs/storagecontroller/testdata/deployment.yaml` golden file

#### 4.1.8 探针可配置化（✅ 已实现）

> **已实现 (2026-06-29)**：通过 `ProbeConfig` 类型和 `probeWithConfig()` 辅助函数实现探针参数可配置化。

**API 类型定义**（`api/v1alpha1/pageserver_types.go`）：

```go
// ProbeConfig allows overriding the default container health probe parameters.
// Only threshold/hysteresis parameters are exposed; the endpoint path, port,
// and scheme are fixed by the operator (they correspond to upstream Neon's
// design where only /v1/status is unauthenticated and suitable for K8s probes).
type ProbeConfig struct {
    InitialDelaySeconds *int32 `json:"initialDelaySeconds,omitempty"`
    PeriodSeconds       *int32 `json:"periodSeconds,omitempty"`
    TimeoutSeconds      *int32 `json:"timeoutSeconds,omitempty"`
    FailureThreshold    *int32 `json:"failureThreshold,omitempty"`
}

type PageserverSpec struct {
    // ...
    LivenessProbe  *ProbeConfig `json:"livenessProbe,omitempty"`
    ReadinessProbe *ProbeConfig `json:"readinessProbe,omitempty"`
    StartupProbe   *ProbeConfig `json:"startupProbe,omitempty"`
}
```

**探针构建**（`specs/pageserver/statefulset.go`）：

```go
// 使用 probeWithConfig 辅助函数构建探针，cfg 非 nil 时覆盖默认参数
LivenessProbe:  probeWithConfig("/v1/status", 9898, 30, 10, 5, 3, ps.Spec.LivenessProbe),
ReadinessProbe: probeWithConfig("/v1/status", 9898, 10, 5, 3, 2, ps.Spec.ReadinessProbe),
StartupProbe:   probeWithConfig("/v1/status", 9898, 10, 10, 5, 30, ps.Spec.StartupProbe),
```

**`probeWithConfig` 辅助函数**（同时存在于 `specs/pageserver/statefulset.go` 和 `specs/safekeeper/statefulset.go`）：

```go
func probeWithConfig(path string, port int, initialDelay, period, timeout, failure int32, cfg *v1alpha1.ProbeConfig) *corev1.Probe {
    probe := &corev1.Probe{
        ProbeHandler: corev1.ProbeHandler{
            HTTPGet: &corev1.HTTPGetAction{
                Path: path, Port: intstr.FromInt(port), Scheme: corev1.URISchemeHTTP,
            },
        },
        InitialDelaySeconds: initialDelay,
        PeriodSeconds:       period,
        TimeoutSeconds:      timeout,
        FailureThreshold:    failure,
    }
    if cfg == nil { return probe }
    if cfg.InitialDelaySeconds != nil { probe.InitialDelaySeconds = *cfg.InitialDelaySeconds }
    if cfg.PeriodSeconds != nil       { probe.PeriodSeconds = *cfg.PeriodSeconds }
    if cfg.TimeoutSeconds != nil      { probe.TimeoutSeconds = *cfg.TimeoutSeconds }
    if cfg.FailureThreshold != nil    { probe.FailureThreshold = *cfg.FailureThreshold }
    return probe
}
```

**设计原则**：
- 默认值不变（与上游 Neon 对齐），仅提供覆盖能力
- `Path` 和 `Port` 不从 CRD 暴露（固定为 `/v1/status` 和对应端口），只暴露阈值参数
- `cfg == nil` 时使用默认值，`cfg != nil` 时字段级覆盖（零值字段不覆盖）

#### 4.1.9 关键源码引用

| 引用 | 说明 |
|------|------|
| `neon/pageserver/src/http/routes.rs:549-557` | `/v1/status` handler — 总是 200, 返回 `{"id":<NodeId>}` |
| `neon/pageserver/src/http/routes.rs:3219-3267` | `/v1/utilization` handler — SC 心跳目标 |
| `neon/pageserver/src/utilization.rs` | `regenerate()` — statvfs + 租户统计 |
| `neon/pageserver/src/bin/pageserver.rs:537-653` | 启动阶段：initial → initial_tenant_load_remote → initial_tenant_load → background_jobs |
| `neon/pageserver/src/bin/pageserver.rs:547` | `STARTUP_IS_LOADING.set(1)` — 加载中标记 |
| `neon/pageserver/src/bin/pageserver.rs:637` | `STARTUP_IS_LOADING.set(0)` — 加载完成标记 |
| `neon/storage_controller/src/service.rs:129` | `MAX_OFFLINE_INTERVAL_DEFAULT = 30s` |
| `neon/storage_controller/src/service.rs:137` | `MAX_WARMING_UP_INTERVAL_DEFAULT = 300s` |
| `neon/storage_controller/src/heartbeater.rs:214-227` | Pageserver 心跳状态机 |
| `neon/storage_controller/src/heartbeater.rs:320-448` | Safekeeper 心跳状态机 |
| `neon/storage_controller/src/http.rs:1621-1672` | SC `/status` `/live` `/ready` 端点 |
| `neon/libs/pageserver_api/src/models.rs:1376-1378` | `StatusResponse { id: NodeId }` 结构体 |
| `neon/libs/pageserver_api/src/models/utilization.rs` | `PageserverUtilization` 结构体 |

#### 4.1.10 总结

| 项目 | Pageserver | Safekeeper | Storage Controller |
|------|:---:|:---:|:---:|
| **LivenessProbe** | ✅ | ✅ | ✅ （已实现） |
| **ReadinessProbe** | ✅ | ✅ | ✅ （已实现） |
| **StartupProbe** | ✅ | ✅ | ✅ （已实现） |
| **探针端点选择** | ✅ `/v1/status`（正确） | ✅ `/v1/status`（正确） | ✅ `/status` `/live` `/ready` |
| **参数与 SC 对齐** | ✅ 高度对齐 | ✅ 高度对齐 | N/A |
| **可配置性** | ✅ ProbeConfig（已实现） | ✅ ProbeConfig（已实现） | N/A |

**所有健康探针优化已完成**。

---

---

### 4.2 P0-2: Resource Requests/Limits — ✅ 已实现

#### 当前实现

`specs/pageserver/statefulset.go` 已实现资源默认值和可配置化：

```go
// 默认资源配置
Requests: corev1.ResourceList{
    corev1.ResourceCPU:    resource.MustParse("500m"),
    corev1.ResourceMemory: resource.MustParse("256Mi"),
}
Limits: corev1.ResourceList{
    corev1.ResourceCPU:    resource.MustParse("2"),
    corev1.ResourceMemory: resource.MustParse("512Mi"),
}

// 可通过 Spec.Resources 覆盖
if ps.Spec.Resources != nil {
    // 应用用户配置
}
```

**配置思路**：
- Pageserver 是 IO + CPU 密集型服务（WAL 处理、compaction、page service）
- 默认值适合开发/小规模场景，生产需根据 tenant 数量和写入速率调整
- 内存需求主要来自 `page_cache` + 操作系统 page cache

#### 实现文件

- `api/v1alpha1/pageserver_types.go` → `PageserverSpec.Resources *corev1.ResourceRequirements`（✅）
- `specs/pageserver/statefulset.go` → `podSpec()` 使用 spec 中的资源配置（✅）

---

### 4.3 P0-3: PodAntiAffinity 与多 AZ 反亲和性 (深度分析)

> **核心问题**：当前的反亲和性设计是否足够？是否缺少 AZ（可用区）维度的反亲和性？本节基于上游 neon 源码 (`storage_controller/src/scheduler.rs`, `storage_controller/src/node.rs`, `libs/pageserver_api/src/controller_api.rs`) 和 Neon Cloud 官方 HA 文档的深度分析。

#### 4.3.1 当前实现现状

**K8s Node 级别反亲和性**（`specs/pageserver/statefulset.go`，已实现）：

```go
Affinity: &corev1.Affinity{
    PodAntiAffinity: &corev1.PodAntiAffinity{
        RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
            {
                LabelSelector: &metav1.LabelSelector{
                    MatchLabels: LabelSelector(ps),  // app.kubernetes.io/component=pageserver + molnett.org/cluster
                },
                TopologyKey: "kubernetes.io/hostname",
            },
        },
    },
},
```

`LabelSelector()` 返回跨实例标签（`labels.go`）：
```go
func LabelSelector(ps *v1alpha1.Pageserver) map[string]string {
    return map[string]string{
        "app.kubernetes.io/component": "pageserver",
        ClusterLabel:                  ps.Spec.Cluster,
    }
}
```

**AvailabilityZone 字段**（`api/v1alpha1/pageserver_types.go`，已实现）：

```go
type PageserverSpec struct {
    // ...
    // AvailabilityZone 指定 PS 向 SC 报告的可用区 ID。
    // 默认为 "se-ume"。
    // +optional
    AvailabilityZone string `json:"availabilityZone,omitempty"`
}
```

**当前 initScript 使用 AZ 参数**（`statefulset.go`）：

Init container 的 shell 脚本已接收 az 参数并写入 metadata.json：
```bash
echo "{\"host\":\"%s.%s\",\"http_host\":\"%s.%s\",\"http_port\":9898,\"port\":6400,\"availability_zone_id\":\"%s\"}"
```

**已实现的能力**：
- 同一 Cluster 的所有 pageserver Pod 不会部署在同一 K8s 节点上（`kubernetes.io/hostname` 级别）
- `AvailabilityZone` 字段已在 Spec 中，但**默认值仍为 `"se-ume"`**（需用户手动设置真实 AZ）
- **缺失**：K8s AZ 级别的 `topologySpreadConstraints`（Layer 2）
- **缺失**：Operator 自动从 K8s Node label 推断 AZ 并注入（Layer 3 自动化）

#### 4.3.2 上游 Neon 的 AZ 感知架构（深度源码分析）

##### SC Scheduler 的 AZ 感知调度逻辑

上游 Neon 的 Storage Controller Scheduler（`scheduler.rs`）具有完整的 AZ 感知能力：

**① 节点 AZ 属性**（`scheduler.rs:46`）：
```rust
struct SchedulerNode {
    shard_count: usize,
    attached_shard_count: usize,
    home_shard_count: usize,  // 该 AZ 作为 home 的 shard 数
    az: AvailabilityZone,     // 节点所属 AZ
    may_schedule: MaySchedule,
}
```

**② AZ 匹配评分**（`scheduler.rs:84-99`）：
```rust
enum AzMatch {
    Yes,      // 节点在 shard 的 preferred_az 中
    No,       // 节点不在 preferred_az 中
    Unknown,  // shard 没有设置 preferred_az
}
```

**③ Attached 位置调度（主副本）**— `AttachmentAzMatch`（`scheduler.rs:101-118`）：
```
AZ 评分优先级（越低越优先）：
  Yes(0) > Unknown(1) > No(2)
```
主 shard **优先放在 preferred_az** 中——减少跨 AZ 计算延迟，使 Compute 和 Pageserver 在同 AZ。

**④ Secondary 位置调度（辅助副本）**— `SecondaryAzMatch`（`scheduler.rs:126-148`）：
```
AZ 评分优先级（越低越优先）：
  No(0) > Unknown(1) > Yes(2)
```
辅助 shard **避免放在 preferred_az** 中——确保 AZ 故障时 secondary 在另一个 AZ 中可用。

**⑤ 新 Tenant AZ 选择**— `get_az_for_new_tenant()`（`scheduler.rs:819-852`）：
```
选择 home_shard_count 最低的 AZ → 跨 AZ 均衡分配新 tenant
```

**⑥ 调度核心流程**（`scheduler.rs:733-800`）：
```rust
fn schedule_shard<Tag: ShardTag>(
    &mut self,
    hard_exclude: &[NodeId],
    preferred_az: &Option<AvailabilityZone>,
    context: &ScheduleContext,
) -> Result<NodeId, ScheduleError> {
    // 1. 计算所有节点的评分（包含 AZ 匹配度）
    let scores = self.compute_node_scores::<Tag::Score>(hard_exclude, preferred_az, context);
    // 2. 排除过载节点
    // 3. 排序选最优（AZ 匹配 > 亲和性 > 利用率）
    // 4. 返回得分最低的节点
}
```

**⑦ AZ 迁移时的不变性约束**（`node.rs:134-143`）：
```rust
fn registration_match(&self, register_req: &NodeRegisterRequest) -> bool {
    self.id == register_req.node_id
        && self.listen_http_addr == register_req.listen_http_addr
        && self.listen_http_port == register_req.listen_http_port
        && self.listen_pg_addr == register_req.listen_pg_addr
        && self.listen_pg_port == register_req.listen_pg_port
        && self.availability_zone_id == register_req.availability_zone_id
        // ^^ AZ 变更 → 注册被拒绝，需管理员介入
}
```

**关键发现**：SC scheduler 的 AZ 感知能力**已经完整存在**，但**当前完全被禁用**——因为所有 pageserver 通过 metadata.json 上报的 `availability_zone_id` 都是硬编码的 `"se-ume"`（所有节点看起来在同一 AZ）。

##### Neon Cloud 的 AZ 故障处理策略（官方 HA 文档）

来自 [Neon HA 文档](https://neon.com/docs/introduction/high-availability)：

| 场景 | 策略 | 恢复时间 |
|------|------|---------|
| **存储组件** | 始终跨多 AZ 分布 | — |
| **Pageserver Primary** | 在 Compute 同 AZ | — |
| **Pageserver Secondary** | 在不同 AZ（HA 保证） | 秒级切换 |
| **Compute 节点** | 单 AZ 运行，故障后重新调度到健康 AZ | 1-10 分钟 |
| **全 AZ 故障** | Compute 重新调度到其他健康 AZ | 1-10 分钟 |

#### 4.3.3 差距分析：当前设计缺少什么

```
┌─────────────────────────────────────────────────────────────────────┐
│                    反亲和性层次模型                                   │
│                                                                      │
│  Layer 1: K8s Node 级别       Layer 2: K8s AZ 级别                   │
│  ┌─────────────────────┐      ┌──────────────────────────────┐      │
│  │ PodAntiAffinity     │      │ topologySpreadConstraints     │      │
│  │ topologyKey:        │      │ topologyKey:                 │      │
│  │   hostname          │      │   topology.kubernetes.io/zone│      │
│  │                     │      │                              │      │
│  │ 状态: ✅ 已实现      │      │ 状态: ❌ 完全缺失             │      │
│  └─────────────────────┘      └──────────────────────────────┘      │
│                                                                      │
│  Layer 3: SC 调度器 AZ 感知     Layer 4: AZ 信息注入                  │
│  ┌─────────────────────┐      ┌──────────────────────────────┐      │
│  │ SC Scheduler:       │      │ metadata.json:               │      │
│  │  - AzMatch 评分     │      │   availability_zone_id       │      │
│  │  - preferred_az     │      │                              │      │
│  │  - get_az_for_new   │      │ 状态: ❌ 硬编码 "se-ume"       │      │
│  │    _tenant()        │      │   → SC scheduler AZ 感知     │      │
│  │                     │      │     被完全禁用                │      │
│  │ 状态: ✅ SC 已支持   │      │                              │      │
│  └─────────────────────┘      └──────────────────────────────┘      │
└─────────────────────────────────────────────────────────────────────┘
```

| 差距 | 严重程度 | 影响 |
|------|:---:|------|
| **无 AZ 级 K8s 反亲和性** | 🔴 高 | 所有 pageserver 可能被调度到同一 AZ，单 AZ 故障全部不可用 |
| **metadata.json AZ 硬编码** | 🔴 高 | SC scheduler 的 AZ 感知能力完全被禁用（所有节点报同一 AZ） |
| **无 AZ 信息注入机制** | 🔴 高 | 无法从 K8s Node 标签自动获取真实 AZ |
| **PageserverSpec 无 AZ 字段** | 🟡 中 | 无法手动覆盖 AZ（跨 AZ 迁移场景需要） |

##### 场景推演：当前配置下的 AZ 故障

```
假设：4 个 pageserver 全部被 K8s 调度到 AZ-A
     metadata.json 全部报告 "se-ume"（同一 AZ）

场景：AZ-A 故障
  ├── 所有 Pageserver Pod → Terminating/Pending
  ├── SC 检测所有节点 Offline → 无处调度！(schedulable_nodes_count = 0)
  ├── SC scheduler 无法迁移 shard（所有节点在同一 AZ，全部不可用）
  ├── Compute 无法读取数据
  └── 完全服务中断 ← 无 AZ 冗余

如果配置了多 AZ：
  ├── Pageserver-0,1 在 AZ-A → Offline
  ├── Pageserver-2,3 在 AZ-B → Active（心跳正常）
  ├── SC 检测 AZ-A 节点 Offline → demote_attached → schedule 到 AZ-B
  ├── SC 将 attached shard 迁移到 AZ-B 的 secondary（已在其他 AZ）
  └── 秒级恢复（secondary 已在维护热数据副本）
```

#### 4.3.4 设计方案：四层反亲和性体系

##### Layer 1：K8s Node 级别反亲和性（已实现 ✅）

```yaml
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: pageserver
            molnett.org/cluster: <cluster-name>
        topologyKey: kubernetes.io/hostname
```

**状态**：`statefulset.go:95-106` 已实现，无需修改。

##### Layer 2：K8s AZ 级别拓扑分布约束（新增 🔴）

使用 `topologySpreadConstraints` 确保 pageserver 跨 AZ 均匀分布：

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: DoNotSchedule          # 硬约束：必须跨 AZ
    labelSelector:
      matchLabels:
        app.kubernetes.io/component: pageserver
        molnett.org/cluster: <cluster-name>
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway         # 软约束：尽力而为（节点级已有 anti-affinity）
    labelSelector:
      matchLabels:
        app.kubernetes.io/component: pageserver
        molnett.org/cluster: <cluster-name>
```

**为什么用 `topologySpreadConstraints` 而非 AZ 级 `podAntiAffinity`**：
- `podAntiAffinity` 是"互斥"语义：同一 AZ 不能有两个 pod → 3 个 AZ 只能放 3 个 PS
- `topologySpreadConstraints` 是"均匀分布"语义：4 个 PS 在 3 个 AZ 中均匀分布 → 1,1,2（`maxSkew: 1`）
- Neon Cloud 模式中，不是一个 AZ 只有一个 PS，而是**尽量均匀分布**

**`whenUnsatisfiable` 策略选择**：

| 策略 | 行为 | 适用场景 |
|------|------|---------|
| `DoNotSchedule` | AZ 数不足则不调度 | 生产环境（P0）：强制多 AZ |
| `ScheduleAnyway` | AZ 数不足仍调度 | 开发环境：单 AZ 也能跑 |

**建议**：默认使用 `ScheduleAnyway`（不阻塞开发环境），生产通过 Cluster CRD 覆盖为 `DoNotSchedule`。

##### Layer 3：AZ 信息注入 metadata.json（新增 🔴）

核心思路：通过 K8s Downward API 将 Node 的 zone 标签注入 Pod，Init 容器在生成 `metadata.json` 时使用。

**3a. 在 PodSpec 中暴露 Node Zone**：

```yaml
env:
  - name: NODE_ZONE
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName   # 先拿到 nodeName
```

**3b. 在 InitContainer 中注入真实 AZ**：

```bash
# 通过 nodeName 查询 node 的 zone label
NODE_NAME=$(cat /etc/podinfo/nodeName)
ZONE=$(kubectl get node $NODE_NAME -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')
if [ -z "$ZONE" ]; then
    ZONE="unknown"  # fallback
fi

echo "id=%d" > /config/identity.toml
echo "{\"host\":\"%s.%s\"," \
     "\"http_host\":\"%s.%s\"," \
     "\"http_port\":9898,\"port\":6400," \
     "\"availability_zone_id\":\"$ZONE\"}" > /config/metadata.json
```

**优化方案**：如果 K8s 集群版本 ≥ 1.17，可以直接使用 Downward API 的 `metadata.annotations` 或通过 ServiceAccount 查询。但最可靠的方式是通过 init container 调用 K8s API 查询 node label。

**更简单的方案**（推荐）：使用 K8s 1.17+ 的 `fieldRef: metadata.annotations` 不支持动态 label。替代方案是使用 `downwardAPI` volume 暴露 `metadata.labels`：

实际上，K8s 标准方式是通过 `downwardAPI` 在 Pod 启动后读取 `/etc/podinfo/labels`：
```yaml
volumes:
  - name: podinfo
    downwardAPI:
      items:
        - path: "labels"
          fieldRef:
            fieldPath: metadata.labels
```

但 Kubernetes 的 `metadata.labels` 是 Pod 的 labels，不是 Node 的 labels。需要通过以下方式获取 Node zone：

**推荐实现**：在 Controller 中读取 Node 的 zone label，写入 Pageserver Spec 或 ConfigMap，由 InitContainer 使用：

```go
// pageserver_controller.go 中注入
func (r *PageserverReconciler) getNodeZone(ctx context.Context, podName, namespace string) (string, error) {
    pod := &corev1.Pod{}
    if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, pod); err != nil {
        return "", err
    }
    if pod.Spec.NodeName == "" {
        return "unknown", nil
    }
    node := &corev1.Node{}
    if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
        return "", err
    }
    if zone, ok := node.Labels["topology.kubernetes.io/zone"]; ok {
        return zone, nil
    }
    return "unknown", nil
}
```

然后将 zone 通过环境变量注入 InitContainer：
```yaml
env:
  - name: PAGESERVER_AZ
    value: "us-east-1a"
```

**最终推荐方案**（TODO 阶段的简化实现）：
- 在 `PageserverSpec` 中增加 `AvailabilityZoneID` 字段
- Cluster Controller 创建 PS CR 时，通过 K8s API 查询 Node 的 zone 并设置到 Spec
- InitContainer 从环境变量 `PAGESERVER_AZ` 读取 → 写入 metadata.json
- 如果 Spec 中有 `AvailabilityZoneID`，优先使用；否则 fallback 到 `"unknown"`

##### Layer 4：SC Scheduler AZ 感知（已内建 ✅，需激活数据流）

SC scheduler 的 AZ 感知是完整且经过测试的（`scheduler.rs:1178-1371` 中的 `az_scheduling`、`az_scheduling_for_new_tenant`、`az_selection_many` 测试）。

**数据流打通后自动生效**：
```
K8s Node zone label → Controller 注入 → metadata.json → SC node.availability_zone_id
                                                              ↓
SC scheduler: AzMatch(Yes/No/Unknown) → preferred_az 感知调度
                                                              ↓
                            ┌─ Attached: 优先同 AZ 节点（减少延迟）
                            └─ Secondary: 优先不同 AZ 节点（HA 保证）
```

#### 4.3.5 完整 PodSpec 设计

```go
func podSpec(ps *v1alpha1.Pageserver, image, serviceName string, az string) corev1.PodSpec {
    return corev1.PodSpec{
        // ... SecurityContext ...

        // Layer 1: Node 级别反亲和性（保留）
        Affinity: &corev1.Affinity{
            PodAntiAffinity: &corev1.PodAntiAffinity{
                RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
                    {
                        LabelSelector: &metav1.LabelSelector{
                            MatchLabels: LabelSelector(ps),
                        },
                        TopologyKey: "kubernetes.io/hostname",
                    },
                },
            },
        },

        // Layer 2: AZ 级别拓扑分布约束（新增）
        TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
            {
                MaxSkew:           1,
                TopologyKey:       "topology.kubernetes.io/zone",
                WhenUnsatisfiable: corev1.ScheduleAnyway,  // 开发环境友好；生产可改为 DoNotSchedule
                LabelSelector: &metav1.LabelSelector{
                    MatchLabels: LabelSelector(ps),
                },
            },
        },

        // ... InitContainers ...
        InitContainers: []corev1.Container{
            {
                Name:    "setup-config",
                Image:   "busybox:latest",
                Command: []string{"/bin/sh", "-c"},
                Args: []string{
                    fmt.Sprintf(initScript,
                        ps.Spec.ID,
                        serviceName, ps.Namespace,
                        serviceName, ps.Namespace,
                        az),  // Layer 3: AZ 注入
                },
                Env: []corev1.EnvVar{
                    {Name: "PAGESERVER_AZ", Value: az},  // Layer 3: AZ 环境变量
                },
                VolumeMounts: []corev1.VolumeMount{
                    {Name: "pageserver-config", MountPath: "/configmap"},
                    {Name: "config", MountPath: "/config"},
                },
            },
        },

        // ... Containers, Volumes (unchanged) ...
    }
}
```

更新 `initScript` 常量：
```go
const initScript = `echo "id=%d" > /config/identity.toml

echo "{\"host\":\"%s.%s\"," \
     "\"http_host\":\"%s.%s\"," \
     "\"http_port\":9898,\"port\":6400," \
     "\"availability_zone_id\":\"%s\"}" > /config/metadata.json

cp /configmap/pageserver.toml /config/pageserver.toml
`
```

#### 4.3.6 PageserverSpec 扩展 — 🔶 部分实现

```go
// api/v1alpha1/pageserver_types.go（已存在）
type PageserverSpec struct {
    // ... existing fields ...

    // AvailabilityZone 指定 pageserver 向 Storage Controller 报告的可用区 ID。
    // 默认为 "se-ume"（需用户手动设置为真实 AZ）。
    // +optional
    AvailabilityZone string `json:"availabilityZone,omitempty"`
}
```

**当前状态**：字段已存在，initScript 已使用 az 参数。缺失的是：
- **自动推断**：Operator 尚无从 K8s Node label 自动获取 zone 的能力
- **默认值问题**：默认值仍为 `"se-ume"`，如果用户不手动设置，SC AZ 调度仍被禁用

#### 4.3.7 Controller 注入逻辑

Controller 在 reconcile 时注入 AZ 信息到 StatefulSet：

```go
func (r *PageserverReconciler) createPageserverResources(ctx context.Context, ps *neonv1alpha1.Pageserver) error {
    // ...
    az := ps.Spec.AvailabilityZoneID
    if az == "" {
        // 尝试从已运行的 Pod 获取 Node zone
        if nodeZone, err := r.getPodNodeZone(ctx, pageserverspec.Name(ps)+"-0", ps.Namespace); err == nil {
            az = nodeZone
        }
    }
    if az == "" {
        az = "unknown"
    }

    sts := pageserverspec.StatefulSet(ps, image)  // 需要增加 az 参数
    // ...
}

func (r *PageserverReconciler) getPodNodeZone(ctx context.Context, podName, namespace string) (string, error) {
    pod := &corev1.Pod{}
    if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, pod); err != nil {
        return "", err
    }
    if pod.Spec.NodeName == "" {
        return "", nil
    }
    node := &corev1.Node{}
    if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
        return "", err
    }
    if zone, ok := node.Labels["topology.kubernetes.io/zone"]; ok {
        return zone, nil
    }
    return "", nil
}
```

#### 4.3.8 StatefulSet 函数签名变更 — ✅ 已实现

```go
// 当前签名（已实现）
func StatefulSet(ps *v1alpha1.Pageserver, image, az string) *appsv1.StatefulSet
```

**az 参数已生效**：initScript 已将 az 写入 metadata.json 的 `availability_zone_id` 字段。

#### 4.3.9 AZ 不可变约束处理

SC 的 `registration_match()` 要求 AZ 不变。当需要跨 AZ 迁移 pageserver 时：

```
正确流程：
1. kubectl annotate pageserver X drain=true  → SC 迁移所有 shard
2. 等待 drain 完成（attached shard count = 0）
3. SC API: DELETE /control/v1/node/{id}/delete  → 清理 SC 旧记录
4. 修改 Spec.AvailabilityZoneID 为目标 AZ
5. 删除 Pod → StatefulSet 在新节点重建 → 重新注册 SC（新 AZ）
```

**注意**：如果直接修改 AZ 而不先清理 SC 记录，SC 会返回 `ApiError::Conflict("Node is already registered with different address")`。

#### 4.3.10 实施优先级

| 优先级 | 改动 | 说明 |
|:---:|------|------|
| ✅ P0 | `StatefulSet()` 函数签名变更（增加 az 参数） | ✅ 已实现 |
| ✅ P0 | `initScript` 使用 az 参数替代硬编码 | ✅ 已实现 |
| ✅ P0 | `PageserverSpec.AvailabilityZone` 字段 | ✅ 已实现 |
| 🔴 P0 | `topologySpreadConstraints` 加入 PodSpec | 确保 K8s AZ 级别分布 |
| 🟡 P1 | Controller 注入 Node zone 逻辑 | 自动化 AZ 推断（从 K8s Node label） |
| 🟢 P2 | AZ 迁移工作流（drain → SC 清理 → AZ 更新 → 重建） | 运维场景 |

#### 4.3.11 与 Safekeeper 的差异

| 维度 | Pageserver | Safekeeper |
|------|-----------|------------|
| **K8s Node 反亲和** | ✅ `RequiredDuringScheduling` | ✅ `RequiredDuringScheduling` |
| **K8s AZ 分布** | 🔜 `topologySpreadConstraints` | ❌ 不需要（Safekeeper 数据量小，WAL 协议不要求 AZ 感知） |
| **SC AZ 感知** | ✅ scheduler 使用 `preferred_az` | ❌ SC 不关心 Safekeeper AZ |
| **AZ 信息注入** | 🔜 metadata.json 需要真实 AZ | N/A |

Safekeeper 不需要 AZ 感知的核心原因：
- Safekeeper 通过 Paxos 共识保证一致性，与物理位置无关
- Safekeeper 数据量小（WAL），不涉及跨 AZ 的大数据传输
- Pageserver 需要与 Compute 同 AZ 以减少 page 读取延迟（SC scheduler 的核心优化目标）

#### 4.3.12 关键源码引用

| 引用 | 说明 |
|------|------|
| `neon/storage_controller/src/scheduler.rs:46` | `SchedulerNode.az: AvailabilityZone` |
| `neon/storage_controller/src/scheduler.rs:84-99` | `AzMatch` 枚举和匹配逻辑 |
| `neon/storage_controller/src/scheduler.rs:101-148` | `AttachmentAzMatch` vs `SecondaryAzMatch` 排序（前者偏好同 AZ，后者避开同 AZ） |
| `neon/storage_controller/src/scheduler.rs:733-800` | `schedule_shard()` 核心调度算法 |
| `neon/storage_controller/src/scheduler.rs:819-852` | `get_az_for_new_tenant()` 跨 AZ 均衡 |
| `neon/storage_controller/src/node.rs:134-143` | `registration_match()` AZ 不变性约束 |
| `neon/storage_controller/src/scheduler.rs:1178-1371` | `az_scheduling` / `az_scheduling_for_new_tenant` / `az_selection_many` 测试 |
| `neon/libs/pageserver_api/src/controller_api.rs:63` | `NodeRegisterRequest.availability_zone_id` |
| | [Neon HA 文档](https://neon.com/docs/introduction/high-availability) |

#### 4.3.13 总结

当前反亲和性设计在**节点级别已实现**，AZ 维度**部分实现**：

| 层级 | 状态 | 阻塞项 |
|------|:---:|------|
| K8s Node 反亲和 | ✅ 已实现 | — |
| AZ 字段 | ✅ 已实现 | 默认值 `"se-ume"` 需手动设置 |
| StatefulSet + initScript AZ 注入 | ✅ 已实现 | 数据流已通畅 |
| K8s AZ 拓扑分布 | ❌ 缺失 | 需添加 `topologySpreadConstraints` |
| Controller 自动 AZ 推断 | ❌ 缺失 | 需从 K8s Node label 自动获取 zone |

**当前问题**：默认 AZ 为 `"se-ume"`，若用户不手动在 Spec 中设置真实 AZ，所有 pageserver 仍向 SC 报告同一 AZ → SC scheduler 的 AZ 感知能力被禁用。

---

### 4.4 P0-4: PodDisruptionBudget — ✅ 已实现

#### 当前实现

`specs/pageserver/pdb.go` 已实现：

```go
func PodDisruptionBudget(ps *v1alpha1.Pageserver) *policyv1.PodDisruptionBudget {
    return &policyv1.PodDisruptionBudget{
        Spec: policyv1.PodDisruptionBudgetSpec{
            MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
            Selector: &metav1.LabelSelector{
                MatchLabels: LabelSelector(ps), // 跨所有 pageserver 的集群级选择器
            },
        },
    }
}
```

**设计要点**：
- `MaxUnavailable=1`：保证最多一个 pageserver 同时被驱逐
- 使用 `LabelSelector()` 跨所有 pageserver（非单实例），确保集群级别的限制
- Controller 在 `createPageserverResources()` 中 reconcile（已实现）

#### 实现文件

- `specs/pageserver/pdb.go` → PDB 工厂函数（✅）
- `internal/controller/pageserver_create.go` → reconcile PDB（✅）

---

### 4.5 节点故障与 Pageserver 迁移机制 (深度分析)

> **状态**：Operator 侧的节点故障恢复（`handleNodeFailure`）已实现。Storage Controller（SC）已**内置了完整的自动故障转移和重新调度机制**。Operator 的角色是确保 K8s 层面的基础设施正确性，以及处理本地磁盘故障等 K8s 特有问题。本节基于 neon 源码 (storage_controller/, pageserver/) 的深度分析。

#### 4.5.1 Storage Controller 的自动调度机制

##### 心跳与故障检测

SC 通过 `heartbeater.rs` 对每个已注册的 pageserver 进行周期性健康检查：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `heartbeat_interval` | 5s | 心跳间隔，向 pageserver 的 `/v1/utilization` 发 GET 请求 |
| `max_offline_interval` | 30s | 连续失败超过此时间 → 标记为 `Offline` |
| `max_warming_up_interval` | 300s | 重启期间更宽松的宽限期，此时间过后才标记 `Offline` |

pageserver 状态机：
```
Offline → WarmingUp (re-attach后) → Active (心跳恢复)
Active  → WarmingUp (重连/重启)   → Offline (心跳失败超时)
Active  → Offline   (心跳连续失败 > 30s)
```

##### 节点离线时的自动处理 (`handle_node_availability_transition::ToOffline`)

当 SC 检测到 pageserver 变为 `Offline`（位于 `service.rs:7988`）：

```
1. 清除 observed state
   └── 将该节点上所有 tenant shard 的 observed location 设为 None（状态未知）

2. 安全检查（防止错误判断）：
   ├── 单节点集群？ → 跳过（无处可迁移）
   └── 所有节点都不可用？ → 跳过（避免 detach 所有租户）

3. demote_attached (service.rs:8026)：
   ├── 将故障节点上的 attached shard 降级为 secondary
   ├── attached = None（取消主附着）
   └── 将该节点加入 secondary 列表

4. schedule() (scheduler.rs:733)：
   ├── 为每个 shard 选择新的 attached 节点
   ├── 调度策略：AZ 亲和性 > 利用率均衡 > 节点 ID
   └── 排除已不可用的节点

5. maybe_reconcile_shard():
   └── 启动 Reconciler 将 intent 状态同步到 pageserver
```

**调度策略**（`scheduler.rs:733-800`）：
- **AZ 亲和性**：优先选择与 shard 相同 AZ 的节点
- **负载均衡**：优先选择 shard 数少、磁盘利用率低的节点
- **软约束**：同 tenant 的 shard 尽量分布到不同节点（通过 AffinityScore）
- **过载保护**：过载节点（utilization > 阈值）会被排除，除非没有替代节点
- **确定性**：相同条件下选择最低 NodeId，保证结果可复现

##### 节点恢复时的自动处理 (`handle_node_availability_transition::ToActive`)

当 pageserver 恢复心跳变为 `Active`（`service.rs:8058`）：

```
1. 遍历所有 tenant shard
2. 找到 observed location 为 None 的 shard
3. 启动 Reconciler 重新配置这些 shard
4. 后台负载均衡暂时未实现（代码中有 TODO 注释）
```

#### 4.5.2 Re-Attach 流程（Pageserver 启动/重连核心协议）

这是 pageserver 与 SC 之间最关键的交互（位于 `service.rs:2377-2533`）：

```
Pageserver 启动
  ├── 读取 identity.toml → NodeId（身份标识，跨重启不变）
  ├── 读取 metadata.json → 网络地址（http_host, pg_host 等）
  └── 调用 POST /upcall/v1/re-attach
       body: {
         node_id: <NodeId>,
         register: <NodeRegisterRequest>,  // 来自 metadata.json
         empty_local_disk: <bool>          // 本地 /data/.neon/tenants 是否为空
       }

Storage Controller 处理：
  1. 若 register 不为 None → node_register()
     ├── 检查 registration_match():
     │   ├── http_addr 必须匹配 ✅ (K8s ClusterIP DNS 保证稳定)
     │   ├── pg_addr 必须匹配   ✅
     │   ├── http_port/pg_port 必须匹配 ✅
     │   ├── availability_zone_id 必须匹配 ✅
     │   └── 不匹配 → ApiError::Conflict（拒绝，需管理员介入）
     ├── DNS 解析验证：检查 http_host 是否可解析
     └── 写入持久化存储

  2. 持久化：递增该节点上所有 shard 的 generation number

  3. 若 empty_local_disk 为 true 且 handle_ps_local_disk_loss 启用：
     └── 清除该节点上所有 shard 的 observed location

  4. 构造响应：返回该节点应配置的 tenant 列表
     ├── 已 attached 的 → mode: AttachedSingle, gen: 新值
     └── secondary 的 → mode: Secondary, gen: None

  5. 将节点设为 WarmingUp 状态
```

**关键点**：
- `retry_http_forever`：pageserver 会**无限重试**直到 re-attach 成功。如果地址不匹配被 SC 拒绝，会一直重试 → 需要管理员删除 SC 中旧节点记录
- `empty_local_disk`：pageserver 自动检测 `/data/.neon/tenants` 是否为空，若是则上报（`tenant/mgr.rs:355`）
- Generation number 递增：防止脑裂，旧节点的操作会被 `/upcall/v1/validate` 拒绝

#### 4.5.3 Generation Number 机制（数据安全核心）

Generation number（`docs/rfcs/025-generation-numbers.md`）是防止脑裂的核心：

```
每次 re-attach：
  └── SC 递增该节点上所有 shard 的 generation

Pageserver 在删除操作前：
  └── 调用 POST /upcall/v1/validate 验证其 generation 仍然有效
       └── generation 过期 → 拒绝删除操作

S3 对象键：
  └── 包含 generation number，防止旧节点覆盖新节点的数据
```

这意味着：如果一个 pageserver 被 k8s 错误地"复活"（旧 Pod 未真正终止），其 generation 已过期，所有写操作都会被拒绝。

#### 4.5.4 K8s 环境下 Pageserver 节点迁移的完整流程

以下是在 K8s 环境中，pageserver 所在节点宕机后迁移到新节点的端到端流程：

```
时间线：

T+0s:    节点宕机（物理故障/kubelet 不响应）
T+30s:   K8s node controller 标记节点 NotReady
T+40s:   SC 心跳连续失败 > 30s (max_offline_interval)
         ├── 标记 pageserver 为 Offline
         ├── demote_attached: 将该节点上所有 shard 降为 secondary
         ├── schedule: 将 shard 重新调度到其他可用 pageserver
         └── 启动 Reconciler 执行迁移

T+5min:  K8s 驱逐 Pod (default pod-eviction-timeout)
T+5min:  StatefulSet controller 创建新 Pod（新节点）
         ├── 旧 PVC 仍绑定到旧节点的 PV（本地盘场景）
         ├── 新 Pod 尝试挂载 PVC → 失败（PV 在旧节点上）
         └── Pod 状态：Pending

<Operator 介入处理本地盘 PVC 问题>（见 4.5.5）

T+X:     Operator 检测到 Pod Pending > 阈值 → 删除 PVC → 删除 Pod
T+X+5s:  StatefulSet 创建新 Pod + 新 PVC（在新节点上）
         ├── Init container 写入 identity.toml（NodeId 不变）
         ├── Init container 写入 metadata.json（使用稳定的 ClusterIP DNS）
         └── Init container 写入 pageserver.toml

T+X+10s: Pageserver 主容器启动
         ├── 读取 identity.toml → NodeId = 不变
         ├── 读取 metadata.json → 地址 = 不变（ClusterIP DNS 稳定）
         ├── 读取 pageserver.toml → 配置不变
         ├── 检测 /data/.neon/tenants 为空 → empty_local_disk = true
         └── 调用 POST /upcall/v1/re-attach

SC 处理 re-attach：
         ├── node_register: DNS 地址匹配 → ✅
         ├── empty_local_disk = true → 清除 observed locations
         ├── 递增所有相关 shard 的 generation
         ├── 返回 tenant 列表
         └── 标记节点为 WarmingUp

T+X+30s: Pageserver 处理 re-attach 响应
         ├── 配置返回的 tenant
         └── 从 S3 下载 Layer 文件（按需/预热）

T+X+60s: SC 心跳检测到 pageserver 可用
         ├── 标记节点为 Active
         ├── 发现 observed locations = None 的 shard
         └── 启动 Reconciler 重新附着

T+X+~2min: 服务恢复
         ├── SC 通知 Compute 更新路由
         └── 业务恢复正常
```

**总预期恢复时间**（取决于 Operator 如何处理 PVC 问题）：
- 如果使用网络存储（EBS/Ceph）：~2-5 分钟（SC 自动 failover + pod 重新调度）
- 如果使用本地盘 + Operator PVC 清理：~5-10 分钟（含 Operator 检测和 PVC 重建时间）

#### 4.5.5 Operator 需要处理的 K8s 特有问题

虽然 SC 已经处理了 pageserver 层面的故障转移，但 K8s 环境引入了两个 Operator 必须处理的问题：

##### 问题 1：本地盘 PVC 粘滞

```
本地 PV → 绑定到特定节点磁盘
节点宕机 → PV 不可用
StatefulSet 创建新 Pod → 挂载同名 PVC → PVC 指向旧节点 PV → Pod 永久 Pending
```

**正确的处理方式**：

```
1. Operator 检测 Pod Pending 状态（> 3 分钟）
2. 检查 Pending 原因：是否是 "volume node affinity conflict"
3. 确认旧节点确实不可恢复（Node NotReady > 阈值）
4. 安全操作：
   a. 删除 Pageserver Pod（非级联删除 PVC）
   b. 删除旧 PVC
   c. StatefulSet controller 自动重建 Pod + 新 PVC
5. 新 Pod 在新节点启动 → 全量冷启动（从 S3 恢复）
```

**关于数据安全的说明**：
- Pageserver 本地盘只是**缓存**，S3 是唯一持久化存储
- 删除 PVC 不会丢失任何已持久化数据
- 未上传到 S3 的增量数据可能丢失（操作窗口内的 WAL），但 safekeeper 中仍有可能保留
- 配合 SC 的 generation number 机制，不会产生数据损坏

##### 问题 2：网络存储场景（EBS/Ceph RBD 等）

如果使用支持跨节点重新附着的网络存储：
```
网络 PV (EBS/Ceph) → 可从旧节点 detach → attach 到新节点
StatefulSet 创建新 Pod → 挂载同名 PVC → PV 重新 attach 到新节点
新 Pod 启动 → /data/.neon/tenants 有数据 → empty_local_disk = false
→ Pageserver 从本地盘加载已有数据 → 恢复更快
```

恢复时间：~2-3 分钟（无需 S3 全量下载，只有增量恢复）

这种场景下，Operator 不需要删除 PVC，K8s 会自动处理 PV 重新附着。

#### 4.5.6 Pageserver CR Spec — ✅ 已实现

`api/v1alpha1/pageserver_types.go` 中已定义：

```go
type PageserverSpec struct {
    // ...existing fields...

    // NodeFailure 控制节点故障时的自动恢复策略
    // +optional
    NodeFailure *NodeFailureRecoveryConfig `json:"nodeFailure,omitempty"`
}

type NodeFailureRecoveryConfig struct {
    // AutoRecover 是否在节点宕机时自动删除 PVC 并重建
    // 默认 false
    // +optional
    AutoRecover bool `json:"autoRecover,omitempty"`

    // MaxPendingDuration 在判定 PVC 无法恢复前，Pod Pending 的最大等待时间
    // 默认 5 分钟
    // +optional
    MaxPendingDuration *metav1.Duration `json:"maxPendingDuration,omitempty"`
}
```

**Controller 实现**（`pageserver_controller.go`）：

```go
func (r *PageserverReconciler) handleNodeFailure(ctx context.Context, ps *v1alpha1.Pageserver) error {
    // 检测 Pod Pending + Volume Node Affinity Conflict
    // 超过阈值（默认 5min）后自动删除 PVC + Pod
    // StatefulSet controller 重建 Pod + 新 PVC
}
```

#### 4.5.7 Pageserver Status 的故障相关 Condition

```go
type PageserverStatus struct {
    // ...existing fields...

    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// 新增 Condition Types:
// - "NodeRecoveryInProgress"  : 正在执行节点故障恢复（Pod Pending → PVC 清理 → 重建）
// - "NodeRecovered"           : 节点故障恢复完成
// - "SCReattached"            : 已在 SC 成功 re-attach
```

#### 4.5.8 Operator 与 Storage Controller 的职责划分

| 职责 | 负责方 | 机制 |
|------|--------|------|
| 检测 pageserver 不可用 | **SC** (heartbeat) | GET /v1/utilization 每 5s |
| 判断节点离线 | **SC** (availability) | 心跳失败 > max_offline_interval (30s) |
| 故障转移（demote + reschedule） | **SC** (scheduler) | demote_attached → schedule → reconcile |
| Tenant shard 迁移执行 | **SC** (reconciler) | LocationConfig API → 目标 pageserver |
| Generation number 递增（防脑裂） | **SC** (persistence) | re_attach 时自动执行 |
| Compute 路由更新 | **SC** (compute_hook) | 通知 Postgres 客户端 pageserver 位置变更 |
| K8s Pod 重新调度 | **K8s** (StatefulSet controller) | Node NotReady → Pod 驱逐 → 新节点创建 |
| 本地盘 PVC 粘滞处理 | **Operator** | 检测 Pending → 清理 PVC → 触发重建 |
| Pageserver 容器健康检查 | **K8s** (probes) + **Operator** | Liveness/Readiness/Startup |
| 计划内迁移（节点维护） | **Operator** + **SC** | SC drain API + K8s 安全驱逐 |

**核心结论**：SC 已经是一个成熟的编排器，Operator 的角色是"K8s 桥接层"——确保 K8s 环境正确适配 SC 的要求（地址稳定性、磁盘管理），而不是重新实现编排逻辑。

---

### 4.6 P0-6: 安全下线 (Safe Decommission) — ✅ 已实现

#### 当前实现

完整四阶段 finalize 流程已在 `internal/controller/pageserver_controller.go` 中实现：

```
Phase 0: 准入检查 (Pre-drain Check)
  ├── 检查 SC 可达性
  └── 检查有其他可调度节点（schedulable_nodes_count > 0）

Phase 1: 启动 Drain
  ├── PUT /control/v1/node/{node_id}/drain（若 Scheduling==Active）
  └── scheduling 变为 Draining

Phase 2: 监控 Drain 进度
  ├── 轮询 SC GET /control/v1/node/{node_id}/shards（间隔 5s）
  ├── 等待 attached shard count = 0
  └── 超时保护：30 分钟

Phase 3: 确认下线
  ├── PUT /control/v1/node/{node_id}/config {scheduling: "PauseForRestart"}
  ├── PUT /control/v1/node/{node_id}/delete
  └── removeFinalizer → K8s 级联清理
```

**关键常量**：
- `drainPollInterval = 5s`（进度轮询间隔）
- `drainTimeout = 30min`（整体超时）

**实现文件**：
- `api/v1alpha1/pageserver_types.go` → Status 扩展（SCSchedulingPolicy, SCAvailability, SCAttachedShardCount 等）（✅）
- `internal/controller/sc_client.go` → 完整 SC 节点管理 API 封装（✅）
- `internal/controller/pageserver_controller.go` → finalize 逻辑（✅）

---

### 4.7 P1-1: ConfigMap 生产调优 — ✅ 大部分已实现

#### 当前实现

`specs/pageserver/configmap.go` 已生成完整的生产级配置（详见 [1.3 节](#13-当前-configmap-内容)），包含：

| 配置区 | 参数 | 状态 |
|--------|------|:---:|
| 网络 | listen_pg_addr, listen_http_addr | ✅ |
| Broker | broker_endpoint, broker_keepalive_interval | ✅ |
| 控制平面 | control_plane_api | ✅ |
| 认证 | http_auth_type=NeonJWT, pg_auth_type=NeonJWT, JWT public key path | ✅ |
| 远程存储 | S3 bucket/region/endpoint/prefix | ✅ |
| 磁盘驱逐 | max_usage_pct=80, min_avail_bytes=2GB, period=60s | ✅ |
| 租户配置 | checkpoint_distance, compaction_threshold, gc_period, pitr_interval 等 | ✅ |
| 并发控制 | concurrent_tenant_warmup=8 | ✅ |

#### 剩余差距（使用源码默认值）

| 参数 | 默认值 | 建议 |
|------|--------|------|
| `max_file_descriptors` | 100 | ⚠️ 严重偏低，生产建议 1000+ |
| `page_cache_size` | 8192 页 | 建议根据内存调整至 32768+ |
| `log_format` | 未设置（非 JSON） | 生产建议 `"json"` 便于日志采集 |
| `metric_collection_interval` | 10min | 建议调整为 60s 配合 Prometheus |

#### 实现文件

- `specs/pageserver/configmap.go` → 生成完整 TOML 配置（✅，大部分已完成）
- 剩余：在 ConfigMap 中添加 `max_file_descriptors`, `page_cache_size`, `log_format`, `metric_collection_interval`

---

### 4.8 P1-2: Prometheus 监控集成

#### 方案

Pageserver 在 `:9898/metrics` 暴露 Prometheus 指标。需要：

1. **Service 端口声明**：确保 9898 端口已暴露 ✅ 已实现
2. **ServiceMonitor CR**：创建 Prometheus Operator ServiceMonitor

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {pageserver-name}
spec:
  selector:
    matchLabels:
      molnett.org/pageserver: {pageserver-name}
  endpoints:
    - port: http
      path: /metrics
      interval: 30s
```

3. **关键指标**：
   - `pageserver_tenant_count` — 租户数量
   - `pageserver_timeline_count` — timeline 数量
   - `pageserver_disk_usage_bytes` — 磁盘使用量
   - `pageserver_wal_ingest_bytes` — WAL 摄入速率
   - `pageserver_getpage_requests` — 页面请求速率
   - `pageserver_compaction_*` — 合并指标
   - `pageserver_remote_storage_*` — S3 操作指标

4. **PrometheusRule**：告警规则

```yaml
groups:
  - name: pageserver
    rules:
      - alert: PageserverDown
        expr: up{job="pageserver"} == 0
        for: 5m
      - alert: PageserverHighDiskUsage
        expr: pageserver_disk_usage_ratio > 0.85
        for: 10m
      - alert: PageserverTenantLoadFailed
        expr: pageserver_tenant_failed_loads > 0
```

#### 实现位置

- `config/prometheus/` → 新增 ServiceMonitor + PrometheusRule 模板
- 通过 kustomize 管理，用户可选启用

---

### 4.9 P1-3: Pageserver Status 扩展 — ✅ 大部分已实现

#### 当前实现

`api/v1alpha1/pageserver_types.go` 中 `PageserverStatus` 已包含：

```go
type PageserverStatus struct {
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`  // ✅
    Conditions         []metav1.Condition `json:"conditions,omitempty"`          // ✅
    
    // 已实现字段
    RegisteredWithSC    bool   `json:"registeredWithSC,omitempty"`    // ✅
    SCSchedulingPolicy  string `json:"scSchedulingPolicy,omitempty"`  // ✅ (Active/Filling/Draining等)
    SCAvailability      string `json:"scAvailability,omitempty"`      // ✅ (Active/WarmingUp/Offline)
    SCAttachedShardCount int    `json:"scAttachedShardCount,omitempty"` // ✅
    SCTotalShardCount   int    `json:"scTotalShardCount,omitempty"`   // ✅
    NodeID              uint64 `json:"nodeID,omitempty"`              // ✅
}
```

**kubebuilder 打印列**（已实现）：
```
Cluster, Available, Progressing, SC (RegisteredWithSC), Shards, Age
```

**剩余差距**（P2 级别）：
| 字段 | 用途 | 优先级 |
|------|------|:---:|
| `TenantCount` | 当前 PS 上的 tenant 数量 | 🟢 P2 |
| `DiskUsage` | 磁盘使用量 | 🟢 P2 |

#### 实现文件

- `api/v1alpha1/pageserver_types.go` → Status 字段已扩展（✅）
- `internal/controller/pageserver_controller.go` → syncSCState 更新 SC 相关字段（✅）

---

### 4.10 P1-4: RUST_LOG 调整

当前 `RUST_LOG=debug` 在生产环境会产生大量日志：
- 改为 `RUST_LOG=info`（默认）
- 或通过 ConfigMap 控制特定模块日志级别

```toml
# 在 pageserver.toml 中
log_format = "json"
```

并通过环境变量控制：
```yaml
env:
  - name: RUST_LOG
    value: "info,pageserver=info,walredo=warn"
```

---

### 4.11 生产环境 Pageserver 扩缩容机制 (深度分析)

> **核心问题**：基于 SC 源码深入分析，生产环境中 Pageserver 扩缩容的完整机制是怎样的？对当前设计方案有哪些影响？
> 本节基于 `storage_controller/src/service.rs`、`storage_controller/src/http.rs`、`storage_controller/src/scheduler.rs` 和 `libs/pageserver_api/src/controller_api.rs` 的详细研究。

#### 4.11.1 Storage Controller 节点管理 API 列表

基于 SC 路由注册（`http.rs:2327-2396`），以下是精确的 API 端点：

| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| `POST` | `/control/v1/node` | `Scope::Infra` | 注册新节点（由 pageserver 端调用） |
| `GET` | `/control/v1/node` | `Scope::Infra` | 列出所有节点及其状态 |
| `GET` | `/control/v1/node/:node_id` | `Scope::Infra` | 获取单个节点详情（返回 `NodeDescribeResponse`） |
| `PUT` | `/control/v1/node/:node_id/config` | `Scope::Admin` | 修改节点 configuration（availability + scheduling policy） |
| `GET` | `/control/v1/node/:node_id/shards` | `Scope::Admin` | 查询节点上的 shard 列表 |
| `PUT` | `/control/v1/node/:node_id/drain` | `Scope::Infra` | 启动 drain（将 attached shard 迁移到其他节点） |
| `DELETE` | `/control/v1/node/:node_id/drain` | `Scope::Infra` | 取消正在进行的 drain |
| `PUT` | `/control/v1/node/:node_id/fill` | `Scope::Infra` | 启动 fill（仅接收新 shard，不接收迁移） |
| `DELETE` | `/control/v1/node/:node_id/fill` | `Scope::Infra` | 取消 fill，恢复 Active |
| `PUT` | `/control/v1/node/:node_id/delete` | `Scope::Admin` | 启动节点删除（后台清理 task） |
| `DELETE` | `/control/v1/node/:node_id/delete` | `Scope::Infra` | 取消正在进行的删除 |

**权限说明**：
- `Scope::Infra`：基础设施操作，operator 需要的基础权限
- `Scope::Admin`：管理操作，需要更高权限（当前 SC dev 模式下全部可用）

#### 4.11.2 SC 内部 NodeSchedulingPolicy 状态机

基于 `libs/pageserver_api/src/controller_api.rs:396-403`：

```rust
pub enum NodeSchedulingPolicy {
    Active,           // 正常状态，可接收新 shard 和现有 shard
    Filling,          // 仅接收新 shard placement（用于新节点预热）
    Pause,            // 暂停调度（不接收新 shard，现有 shard 不变）
    PauseForRestart,  // 重启前暂停（同 Pause，语义更明确）
    Draining,         // 正在排空：shard 被迁移到其他节点
    Deleting,         // 正在删除：后台清理 task 执行中
}
```

`may_schedule()` 方法（`node.rs:229-243`）：仅 `Active` 和 `Filling` 状态可以调度新 shard。

**完整的状态流转**：

```
新节点注册 → Active
                ├── PUT /drain → Draining
                │   ├── drain 完成 → Draining（保持）
                │   │   └── 用户/operator 设 PauseForRestart 或 Deleting
                │   └── DELETE /drain → Active（取消）
                │
                ├── PUT /fill → Filling
                │   └── DELETE /fill → Active（取消）
                │
                ├── PUT /config {scheduling: Pause} → Pause
                │   └── PUT /config {scheduling: Active} → Active
                │
                ├── PUT /config {scheduling: PauseForRestart} → PauseForRestart
                │   └── PUT /config {scheduling: Active} → Active
                │
                └── PUT /delete → Deleting
                    └── 删除完成 → tomstone（可从数据库清除）
```

**关键设计点**：
1. **Drain 完成后不自动转状态**：drain 完成后节点保持 `Draining`，需要 operator 显式设置下一个状态（通常是 `PauseForRestart` 或 `Deleting`）
2. **Filling 需要一个 Active 节点存在**：start_node_fill 要求至少 1 个 `may_schedule()` 的节点已存在
3. **Drain 要求有其他可调度节点**：start_node_drain 需要 `schedulable_nodes_count > 0`
4. **单节点集群不能 drain**：必须至少有 1 个其他可调度节点

#### 4.11.3 Scale-Up（扩容）完整流程分析

当 operator 增加 `NumPageservers` 时（如 4→5），新节点的完整生命周期：

```
Step 1: Operator 创建新 Pageserver CR
  └── PS Controller 创建 StatefulSet → Pod 启动 → pageserver 进程启动

Step 2: Pageserver 自动注册到 SC
  └── pageserver 启动 → 读取 identity.toml (NodeId) → 
      调用 POST /upcall/v1/re-attach → SC 执行 node_register()
  └── SC 返回该节点应配置的 tenant 列表（此时为空）
  └── SC 标记节点状态: Availability::WarmingUp → 心跳恢复 → Active
  └── SchedulingPolicy: Active（默认）

Step 3: 新节点已就绪，但无 shard！
  └── SC 不会自动将已存在的 shard 迁移到新节点
  └── 只有新创建的 tenant 的 shard 会被 SC scheduler 分配到新节点

Step 4（可选但推荐）: Operator 将新节点设为 Filling
  ├── PUT /control/v1/node/{node_id}/fill
  ├── SchedulingPolicy → Filling
  └── SC 将新 tenant 创建请求优先分配到 Filling 节点（负载均衡）

Step 5（可选）: 手动迁移 tenant 到新节点
  └── PUT /control/v1/tenant/{tenant_shard_id}/scheduling_policy → PlacementPolicy
```

**SC 的调度优先级**（`scheduler.rs:733-800`）：
- Filling 节点会在调度中被优先选择（作为容量扩展的目标）
- Active 节点也会参与调度（如果 Filling 节点过载）
- **没有自动 rebalance**：已有的 tenant shard 不会自动迁移到新节点

**生产建议**：
- 新节点加入后，设为 `Filling` 模式让其逐渐承接新 tenant
- 当节点上的 shard 数达到目标后，切换回 `Active`（`PUT /config {scheduling: Active}`）
- 如需将已有 tenant 迁移到新节点，需通过 SC 的 PlacementPolicy API 显式触发（或 drain-fill 组合策略）

#### 4.11.4 Scale-Down（缩容）完整流程分析

当 operator 减少 `NumPageservers` 时（如 5→4），需下线 1 个节点。**这是 SC 中最复杂的操作之一**。

##### 核心流程代码路径

```
PUT /control/v1/node/{node_id}/drain
  ├── service.rs:8305 start_node_drain()
  │   ├── 前置条件检查：
  │   │   ├── 节点必须已注册（node_available = true）
  │   │   ├── 节点 scheduling 必须是 Active（否则报错）
  │   │   ├── 没有其他进行中的操作（ongoing_operation = None）
  │   │   └── 存在其他可调度节点（schedulable_nodes_count > 0）
  │   ├── 设置 scheduling = Draining
  │   └── 启动后台 Tokio task: drain_node()
  │
  ├── service.rs:9586 drain_node()（后台异步执行）
  │   ├── 遍历所有 tenant shard（每次最多 MAX_RECONCILES_PER_OPERATION=64 个）
  │   │   ├── 检查 shard 是否在 drain 节点上有 attached location
  │   │   │   └── TenantShardDrain::tenant_shard_eligible_for_drain
  │   │   ├── 确定目标节点：
  │   │   │   ├── TenantShardDrainAction::RescheduleToSecondary → 迁移到已有 secondary
  │   │   │   ├── TenantShardDrainAction::Reconcile → 重新 reconcile 给其他节点
  │   │   │   └── TenantShardDrainAction::Skip → 跳过（不在该节点或不适合）
  │   │   ├── 检查 secondary lag（默认 ≤ 256MB）
  │   │   │   ├── SecondaryWarmupTimeout = 30s
  │   │   │   └── SecondaryDownloadRequestTimeout = 5s
  │   │   └── 执行迁移 → RescheduleToSecondary 或 Reconcile
  │   └── 全部 shard 迁移完成 → drain 结束
  │
  └── 取消：
      DELETE /control/v1/node/{node_id}/drain
      ├── service.rs:8411 cancel_node_drain()
      └── 恢复 scheduling = Active
```

##### Drain 中的容错设计

```
Drain 中的关键参数：
  max_secondary_lag_bytes = 256MB（可配置）
  SECONDARY_WARMUP_TIMEOUT = 30s    → secondary 必须在此时间内追上
  SECONDARY_DOWNLOAD_REQUEST_TIMEOUT = 5s → 下载请求超时
  MAX_RECONCILES_PER_OPERATION = 64 → 每轮最多处理 64 个 shard

Drain 中的重试与跳过逻辑：
  1. Secondary lag > 262MB → 跳过，下轮重试
  2. 无法确定 lag → 跳过，下轮重试
  3. 获取 lag 失败 → 跳过，下轮重试
  4. 只有 lag ≤ 256MB 时才执行迁移
```

##### 完整缩容时间估算

```
时间模型（基于 SC 内部参数）：
  - 每个 shard 迁移时间 ≈ warmup(30s) + download(5s) + switch(即时)
  - 每 64 个 shard 并发处理
  - 假设节点上有 100 个 shard：
    最优情况（所有 secondary 已就绪）→ ~2 轮 → ~2 分钟
    最差情况（所有 secondary 都是冷数据）→ 每轮 ~30s × 2 轮 → ~1-2 分钟
  - 假设节点上有 1000 个 shard：
    最优 → ~16 轮 → ~30 秒
    最差 → ~16 轮 × 35s → ~9 分钟
```

#### 4.11.5 对当前设计方案的优化建议

##### 优化 1：P0 安全下线流程需要细化

当前设计（第 1132-1148 行）的三阶段方案方向正确，但需要基于 SC 实际 API 细化：

**正确的四阶段缩容流程**：

```
Phase 0: 准入检查（Pre-drain Check）
  ├── GET /control/v1/node/{node_id}
  └── 验证：availability = Active, scheduling = Active, 
      存在其他 schedulable_nodes_count > 0

Phase 1: 启动 Drain
  ├── PUT /control/v1/node/{node_id}/drain
  ├── 返回 202 Accepted
  └── scheduling 变为 Draining（后台 task 开始）

Phase 2: 监控 Drain 进度
  ├── 轮询 GET /control/v1/node/{node_id}（间隔 5s）
  ├── 检查：scheduling 仍为 Draining（进行中）
  ├── 检查：node 上的 attached shard count
  │   └── GET /control/v1/node/{node_id}/shards → 
  │       筛选 attached = true 的数量
  ├── 超时：默认 30 分钟（生产 shard 数量可能很多）
  └── Drain 完成标志：attached shard count = 0
      ├── **注意**：SC 不会自动将 scheduling 从 Draining 切换！
      └── scheduling 仍为 Draining，但后台 task 已结束

Phase 3: 确认与下线
  ├── 确认 attached shard count = 0
  ├── PUT /control/v1/node/{node_id}/config
  │   {scheduling: "PauseForRestart"}
  ├── 可选：PUT /control/v1/node/{node_id}/delete
  │   （正式标记删除，清理 SC 数据库记录）
  └── 删除 Pageserver CR → K8s 级联清理

Phase 4: 异常处理
  ├── 超时 → 设置 Condition "DrainTimeout"
  │   └── DELETE /control/v1/node/{node_id}/drain → 取消
  │   └── 或：force-delete annotation → 跳过 drain
  ├── 用户取消 → DELETE /control/v1/node/{node_id}/drain
  └── 目标节点不可用 → drain 自动跳过 → 等待恢复
```

**关键优化点**：
1. **Phase 0 准入检查** — 当前设计缺少，单节点集群 drain 会直接失败
2. **Drain 完成标志** — 当前设计假设 SC 会自动切换状态，实际上不会。operator 必须通过检查 `attached shard count = 0` 来判断 drain 完成
3. **超时策略** — 当前"10 分钟"过于乐观，生产环境建议 30 分钟
4. **Drain 后状态清理** — 必须显式设 `PauseForRestart` 或 `Deleting`

##### 优化 2：新增 Scale-Up Filling 模式

当前设计缺少扩容时的节点预热机制。建议增加：

```go
type PageserverSpec struct {
    // ...existing fields...

    // InitialSchedulingPolicy 新节点注册后的初始调度策略。
    // "Active"（默认）：立即参与全量调度
    // "Filling"：仅接收新 shard placement（用于新节点预热，避免过度接收）
    // +optional
    // +kubebuilder:default:="Active"
    InitialSchedulingPolicy string `json:"initialSchedulingPolicy,omitempty"`
}
```

Filling 模式的使用场景：
- 新节点加入时逐步承接负载，避免大量 shard 同时迁移造成性能震荡
- 配合 SC scheduler 的 `NodeSecondarySchedulingScore`（`scheduler.rs:224`），Filling 节点会在新 tenant 创建时被优先选择

##### 优化 3：Drain 进度可观测性

当前设计缺少 drain 进度监控。建议 `PageserverStatus` 增加：

```go
type PageserverStatus struct {
    // ...existing fields...

    // SCSchedulingPolicy mirrors SC's NodeSchedulingPolicy
    // "Active", "Filling", "Pause", "PauseForRestart", "Draining", "Deleting"
    SCSchedulingPolicy string `json:"scSchedulingPolicy,omitempty"`

    // SCAvailability mirrors SC's NodeAvailability
    // "Active", "WarmingUp", "Offline"
    SCAvailability string `json:"scAvailability,omitempty"`

    // SCAttachedShardCount 当前节点作为 Attached 的 shard 数量
    SCAttachedShardCount int `json:"scAttachedShardCount,omitempty"`

    // SCTotalShardCount 当前节点上的 shard 总数（Attached + Secondary）
    SCTotalShardCount int `json:"scTotalShardCount,omitempty"`
}
```

Drain 进度计算：
```
progress = (initial_attached - current_attached) / initial_attached × 100%
```

Operator Controller 在 reconcile 中定期查询 SC 状态并更新这些字段。

##### 优化 4：缩容安全约束

```go
// reconcilePageservers() 中的缩容逻辑扩充：

func (r *ClusterReconciler) scaleDownPageserver(ctx context.Context, ps pageserverv1alpha1.Pageserver) error {
    // 1. 准入检查
    nodeStatus, err := r.scClient.GetNode(ctx, ps.Status.NodeID)
    if err != nil { return err }
    
    if nodeStatus.Scheduling != "Active" {
        return fmt.Errorf("node %d currently %s, cannot drain", ps.Status.NodeID, nodeStatus.Scheduling)
    }
    
    // 2. 检查其他可调度节点
    allNodes, err := r.scClient.ListNodes(ctx)
    schedulableCount := 0
    for _, n := range allNodes {
        if n.ID != ps.Status.NodeID && n.Availability == "Active" {
            if n.Scheduling == "Active" || n.Scheduling == "Filling" {
                schedulableCount++
            }
        }
    }
    if schedulableCount == 0 {
        return fmt.Errorf("no other schedulable nodes to drain to")
    }
    
    // 3. 启动 drain
    if err := r.scClient.StartNodeDrain(ctx, ps.Status.NodeID); err != nil {
        return err
    }
    
    // 4. 轮询等待
    return r.waitForDrain(ctx, ps.Status.NodeID, 30*time.Minute)
}
```

##### 优化 5：单 PS 运维操作映射

当前设计中的 annotation 操作需要映射到精确的 SC API：

| Annotation Value | SC API | SchedulingPolicy | 说明 |
|-----------------|--------|-----------------|------|
| `drain` | `PUT /node/:id/drain` | `Draining` | 迁移所有 attached shard |
| `cancel-drain` | `DELETE /node/:id/drain` | 恢复 `Active` | 取消正在进行的 drain |
| `fill` | `PUT /node/:id/fill` | `Filling` | 仅接收新 shard |
| `cancel-fill` | `DELETE /node/:id/fill` | 恢复 `Active` | 取消 fill |
| `pause` | `PUT /node/:id/config {scheduling: Pause}` | `Pause` | 暂停调度 |
| `replace` | `drain` → 等待 → `delete PS CR` → 重建 | `Draining`→`PauseForRestart`→重建 | 完整替换流程 |
| `force-delete` | 跳过 drain，直接 `PUT /node/:id/delete` | `Deleting` | 紧急操作 |

##### 优化 6：解决 Drain 完成后 scheduling 不自动切换的问题

SC 的 drain 完成后 **不会自动切换 scheduling policy**。Operator 需要在检测到 drain 完成后，显式调用 `PUT /node/:id/config`：

```go
func (r *PageserverReconciler) finalize(ctx context.Context, ps *v1alpha1.Pageserver) error {
    // ... start drain ...
    
    // 等待 drain 完成
    for {
        status, err := r.scClient.GetNode(ctx, ps.Status.NodeID)
        if err != nil { return err }
        
        // 检查是否所有 attached shard 已迁移
        shards, err := r.scClient.GetNodeShards(ctx, ps.Status.NodeID)
        if err != nil { return err }
        
        attachedCount := countAttached(shards)
        if attachedCount == 0 {
            // Drain 完成！显式设置调度策略
            if err := r.scClient.ConfigureNode(ctx, ps.Status.NodeID, 
                nil, ptr.To("PauseForRestart")); err != nil {
                return err
            }
            break
        }
        
        time.Sleep(5 * time.Second)
    }
    
    // 可选：标记为 Deleting（清理 SC 数据库记录）
    _ = r.scClient.StartNodeDelete(ctx, ps.Status.NodeID)
    
    // 移除 finalizer → K8s 清理
    return r.removeFinalizer(ctx, ps)
}
```

#### 4.11.6 影响评估总结

| 影响类别 | 当前设计 | 需要优化 | 严重程度 |
|---------|---------|---------|:---:|
| **缩容流程** | 三阶段（Drain → Migrate → Shutdown） | 四阶段（Pre-check → Drain → Monitor → Confirm） | 🔴 高 |
| **Drain 完成检测** | 假设 "轮询 SC API 确认迁移完成" | 必须检查 `attached shard count = 0`，SC 不自动切换 scheduling | 🔴 高 |
| **Drain 后状态** | 未说明 | 必须显式设 `PauseForRestart` 或 `Deleting` | 🔴 高 |
| **准入检查** | 未实现 | Phase 0 检查：节点 Active、存在其他可调度节点 | 🟡 中 |
| **超时策略** | 10 分钟 | 建议 30 分钟（生产 shard 数量可能上千） | 🟡 中 |
| **扩容预热** | 未设计 | 新增 Filling 模式，新节点逐步承接负载 | 🟡 中 |
| **Drain 进度** | 无监控 | 需增加 SCShardCount、SCAttachedShardCount 到 Status | 🟡 中 |
| **SC 状态同步** | scState 字段 | 细化为 SCSchedulingPolicy + SCAvailability + ShardCount | 🟡 中 |
| **单 PS 运维** | annotation 驱动 | 需要映射到精确的 SC API（见优化 5） | 🟢 低 |
| **Filling 模式** | 未提及 | 需要在 ClusterSpec 中增加 InitialSchedulingPolicy | 🟢 低 |

#### 4.11.7 SC API 调用汇总（给 Controller 开发用）

```go
// StorageControllerClient 需要新增/扩展的方法：

// 节点运维
func (c *SCClient) GetNode(ctx context.Context, nodeID uint64) (*NodeDescribeResponse, error)
    // GET /control/v1/node/{node_id}
    // 返回：id, availability, scheduling, shard counts, addresses

func (c *SCClient) ListNodeNodes(ctx context.Context) ([]*NodeDescribeResponse, error)
    // GET /control/v1/node

func (c *SCClient) GetNodeShards(ctx context.Context, nodeID uint64) ([]*ShardDescribeResponse, error)
    // GET /control/v1/node/{node_id}/shards

func (c *SCClient) ConfigureNode(ctx context.Context, nodeID uint64, availability *string, scheduling *string) error
    // PUT /control/v1/node/{node_id}/config
    // body: {"availability": "Active", "scheduling": "Pause"}

// Drain 操作
func (c *SCClient) StartNodeDrain(ctx context.Context, nodeID uint64) error
    // PUT /control/v1/node/{node_id}/drain
    // 返回 202 Accepted

func (c *SCClient) CancelNodeDrain(ctx context.Context, nodeID uint64) error
    // DELETE /control/v1/node/{node_id}/drain
    // 返回 202 Accepted

// Fill 操作（扩容预热）
func (c *SCClient) StartNodeFill(ctx context.Context, nodeID uint64) error
    // PUT /control/v1/node/{node_id}/fill
    // 返回 202 Accepted

func (c *SCClient) CancelNodeFill(ctx context.Context, nodeID uint64) error
    // DELETE /control/v1/node/{node_id}/fill
    // 返回 202 Accepted

// 删除操作
func (c *SCClient) StartNodeDelete(ctx context.Context, nodeID uint64, force bool) error
    // PUT /control/v1/node/{node_id}/delete?force={force}

func (c *SCClient) CancelNodeDelete(ctx context.Context, nodeID uint64) error
    // DELETE /control/v1/node/{node_id}/delete
```

#### 4.11.8 边缘场景分析

##### 场景 1：单节点集群缩容（缩到 0 PS）

```
请求：drain 最后一个可用 PS
结果：start_node_drain() → PreconditionFailed("No other schedulable nodes to drain to")
处理：拒绝缩容，日志警告，支持 force-delete annotation 跳过 drain
```

##### 场景 2：Drain 过程中目标节点也变为不可用

```
时间线：
  1. drain started → shard A 迁移到 node-2
  2. node-2 心跳失败 → SC 标记 Offline
  3. SC scheduler 将 shard A 重新调度到 node-3
  4. drain 后台 task 自动感知 topology 变化（通过 iterator 重新读取）
  5. drain 继续：可迁移的 shard 继续迁移，不可迁移的跳过重试
```

SC 的 drain_node 每次迭代都重新查询 `tenants` 和 `scheduler` 状态，因此可以自动适应 topology 变化。

##### 场景 3：Drain 超时

```
supervisor 监控 drain 进度：
  1. 启动 drain + 记录开始时间
  2. 每 5s 检查进度
  3. 30 分钟后 still draining：
     a. 设置 Condition "DrainTimeout"
     b. 发送告警
     c. 不强制终止（让后台 task 继续）
     d. 如需强制：kubectl annotate pageserver X force-delete=true
```

##### 场景 4：新节点加入后的 Filling 策略决策

```
operator reconcilePageservers 逻辑：
  if actualPS < desiredPS:
    创建新 PS CR
    PS Controller 等待 pod ready + SC 注册完成
    检查 InitialSchedulingPolicy:
      "Active" → 无需额外操作（SC 默认 Active）
      "Filling" → PUT /node/{id}/fill
                     → 新节点只接收新建 tenant 的 shard
                     → 不会因已有 tenant 的调度而造成性能震荡
    (远期) 配合 resource 指标自动从 Filling → Active 切换
```

---

### 4.12 P2: Pageserver HTTP API 集成方案

> **背景**：Pageserver 暴露了丰富的 HTTP API（tenant 管理、状态查询、指标采集），但当前 Operator 未集成任何一个。本节设计一个轻量级集成层，使 Operator 具备对 pageserver 运行状态的深度可观测性。

#### 4.12.1 API 端点总览

基于 `neon/pageserver/src/http/routes.rs:4023-4276` 的路由注册：

| 分类 | 方法 | 路径 | 认证要求 | Operator 用途 |
|------|------|------|:---:|------|
| **健康** | GET | `/v1/status` | 白名单 | K8s 探针（✅ 已使用） |
| **指标** | GET | `/metrics` | 否 | Prometheus 采集 |
| **Profile** | GET | `/profile/cpu` | 否 | 性能诊断 |
| | GET | `/profile/heap` | 否 | 内存诊断 |
| **Tenant** | GET | `/v1/tenant` | JWT | **列出所有 tenant，用于状态监控** |
| | GET | `/v1/tenant/:id` | JWT | **查询单个 tenant 详细状态** |
| | GET | `/v1/tenant/:id/synthetic_size` | JWT | 计算 tenant 逻辑大小 |
| | DELETE | `/v1/tenant/:id` | JWT(admin) | 删除 tenant（危险操作） |
| | PUT | `/v1/tenant/:id/location_config` | JWT(admin) | **设置 location config**（SC 专用） |
| | PUT | `/v1/tenant/:id/time_travel_remote_storage` | JWT | 时间旅行恢复 |
| | POST | `/v1/tenant/:id/reset` | JWT | 重置 tenant（detach + re-attach） |
| **Timeline** | GET | `/v1/tenant/:tid/timeline` | JWT | 列出 tenant 的所有 timeline |
| | GET | `/v1/tenant/:tid/timeline/:tlid` | JWT | timeline 详细状态 |
| | POST | `/v1/tenant/:tid/timeline` | JWT(admin) | 创建 timeline |
| | DELETE | `/v1/tenant/:tid/timeline/:tlid` | JWT(admin) | 删除 timeline |
| **LSN** | GET | `/v1/tenant/:tid/timeline/:tlid/wait_lsn` | JWT | 等待 LSN 到达 |
| **Util** | GET | `/v1/utilization` | JWT | **SC 心跳目标**（磁盘使用、shard 数） |
| **Config** | PUT | `/v1/tenant/config` | JWT(admin) | 全量更新 tenant config |
| | PATCH | `/v1/tenant/config` | JWT(admin) | 增量更新 tenant config |
| | GET | `/v1/tenant/:id/config` | JWT | 获取 tenant config |

**关键发现**：`Neon Cloud API`（`https://api-docs.neon.tech`）**不直接暴露 pageserver 级别的 API**。用户通过 Project/Branch/Endpoint 等高层抽象管理，pageserver 细节由 Neon 内部控制面板（Storage Controller）封装。

#### 4.12.2 Operator 需要集成的 API（按优先级）

##### P1: Tenant 状态监控

```go
// PSPageserverClient — pageserver HTTP API 客户端
type PSPageserverClient struct {
    baseURL    string        // http://{ps-name}.{ns}:9898
    jwtToken   string        // 用于认证（control_plane_token, admin scope）
    httpClient *http.Client
}

// ListTenants 获取 pageserver 上所有 tenant 的状态
func (c *PSPageserverClient) ListTenants(ctx context.Context) ([]TenantStatus, error)
    // GET /v1/tenant
    // 返回: [{tenant_shard_id, state, timelines_count, ...}]

// GetTenantStatus 获取单个 tenant 的详细状态
func (c *PSPageserverClient) GetTenantStatus(ctx context.Context, tenantID string) (*TenantDetailStatus, error)
    // GET /v1/tenant/{tenant_id}
```

**监控指标**（用于 Operator Status 更新）：

```
从 /v1/tenant 获取:
  - tenant_count          → PageserverStatus.TenantCount
  - broken_tenant_count   → 告警触发条件
  - warming_up_count      → 冷启动进度指示

从 /v1/utilization 获取:
  - disk_usage_bytes      → PageserverStatus.DiskUsage
  - free_space_bytes      → 磁盘空间告警
  - shard_count           → PageserverStatus.ShardCount
  - utilization_score     → 负载均衡参考

从 /v1/status 获取:
  - state (active/waiting)→ PageserverStatus.State
  - activity.status       → InitialLoading 进度
```

##### P2: 运维操作

```go
// Tenant 运维操作（需 admin scope JWT）
func (c *PSPageserverClient) GetTenantTimelines(ctx context.Context, tenantID string) ([]TimelineInfo, error)
    // GET /v1/tenant/{tenant_id}/timeline

func (c *PSPageserverClient) GetTenantConfig(ctx context.Context, tenantID string) (*TenantConfig, error)
    // GET /v1/tenant/{tenant_id}/config
```

**注意**：以下操作应由 SC 驱动，Operator 不应直接调用：
- `PUT /v1/tenant/:id/location_config` — SC Reconciler 专用
- `DELETE /v1/tenant/:id` — SC 通过 location_config Detached 模式管理
- timeline 创建/删除 — SC 管理

#### 4.12.3 API 客户端设计

```go
// 文件: internal/controller/ps_client.go（新增）

type PSPageserverClient struct {
    baseURL    string
    jwtToken   string
    httpClient *http.Client
}

func NewPSPageserverClient(ps *v1alpha1.Pageserver, jwtToken string) *PSPageserverClient {
    return &PSPageserverClient{
        baseURL: fmt.Sprintf("http://%s.%s:9898",
            pageserverspec.Name(ps), ps.Namespace),
        jwtToken:   jwtToken,
        httpClient: &http.Client{Timeout: 10 * time.Second},
    }
}

// 认证: 使用 pageserver_control_plane_token（generations_api scope）
// 注意：/v1/status 在白名单中，无需 JWT
// /v1/tenant 系列需要 JWT，但 pageserver_control_plane_token 包含足够的 scope
```

#### 4.12.4 Controller 集成

```go
// 在 createPageserverResources() 的最后调用（已有 syncSCState 调用点）
func (r *PageserverReconciler) syncPSState(ctx context.Context, ps *v1alpha1.Pageserver) error {
    // 创建 PS API 客户端
    client := NewPSPageserverClient(ps, psControlPlaneToken)

    // 获取 tenant 状态
    tenants, err := client.ListTenants(ctx)
    if err != nil {
        // 网络错误或 PS 未就绪 → 不阻塞 reconcile
        log.Info("无法获取 pageserver tenant 状态（可能尚未就绪）", "error", err)
        return nil
    }

    // 更新 Status
    ps.Status.TenantCount = len(tenants)
    ps.Status.BrokenTenantCount = countByState(tenants, "Broken")
    // ...
}
```

#### 4.12.5 认证方案

| API | JWT 要求 | 当前 Operator 持有的 token | 是否可用 |
|-----|---------|--------------------------|:---:|
| `/v1/status` | 白名单（无 JWT） | N/A | ✅ |
| `/v1/tenant` (list) | `pageserverapi` scope | — | ❌ 需要新增 |
| `/v1/tenant/:id` (status) | `pageserverapi` scope | — | ❌ 需要新增 |
| `/v1/utilization` | `pageserverapi` scope | — | ❌ SC 专用 |

**当前 Operator 持有的 token**（通过 `ensurePSAuthTokens` 持久化到 JWT Secret）：
- `pageserver_control_plane_token`：scope = `generations_api`（PS → SC upcall 专用）
- `pageserver_safekeeper_token`：scope = `safekeeperdata`（PS → SK WAL 认证）

**问题**：上述两个 token 的 scope 都**不包含 `pageserverapi`**，无法用于查询 `/v1/tenant` 系列 API。

**解决方案**：可选方案：
1. **方案 A**：新增 `pageserver_api_token`（scope: `pageserverapi`），仅用于 Operator 的监控查询
2. **方案 B**：Operator 通过 SC 间接获取 pageserver 状态（GET /control/v1/node/:node_id 已包含 shard 信息）
3. **推荐方案 B**：SC 已经收集了所有 pageserver 的心跳数据（包括 `/v1/utilization` 的结果），Operator 通过 SC API 获取是最简洁的路径

**结论**：Operator 应优先通过 SC API 获取 pageserver 运行状态（已设计的 `GetNode`、`GetNodeShards` 等）。直接调用 PS API 仅在 SC 不可用时的降级路径（运维排查用途）。

---

### 4.13 P2: Operator 侧 Tenant 生命周期管理

> **背景**：当前 Operator 中 tenant 由 SC 全权管理（创建、附着、迁移、删除），Operator 不感知 tenant 级别的事件。本节设计 Operator 侧如何通过 SC API 感知和辅助 tenant 生命周期。

#### 4.13.1 Tenant 生命周期全景

```
Tenant 创建:
  SC 接收请求 → 计算 placement → 选择目标 PS 节点
    → PUT /v1/tenant/:id/location_config (mode: AttachedSingle)
    → PS 创建本地目录，开始接收 WAL
    → PS 状态: Attaching → Activating → Active

Tenant 迁移 (drain):
  SC PUT /node/:id/drain
    → SC 为每个 attached shard 创建 Secondary 副本
    → Secondary 追上 WAL → SC 切换 Attached 到新节点
    → 旧节点: AttachedStale → Detached

Tenant 删除:
  SC PUT /v1/tenant/:id/location_config (mode: Detached)
    → PS detach_tenant() → 删除本地数据 + 内存状态
    → SC 标记 tenant 为 tombstone → 后台清理 S3 数据

Tenant 故障 (PS 宕机):
  SC 检测 Offline → demote_attached → schedule 到其他 PS
    → 新 PS: AttachedMulti → 从 S3 恢复
    → 旧 PS 恢复后: re-attach → SC 清除 observed locations
```

#### 4.13.2 Operator 的可观测点

| 事件 | 检测方式 | Operator 行为 |
|------|---------|--------------|
| Tenant 创建 | SC `GET /control/v1/node/:id/shards` 变化 | 更新 Status.SCShardCount |
| Tenant 迁移开始 | PS 状态 `Draining` | 记录日志，更新 Condition |
| Tenant 迁移完成 | `SCAttachedShardCount = 0` | 触发 Phase 3（PauseForRestart） |
| Tenant 故障 | SC 心跳失败 → node Offline | 见 4.5 节故障恢复流程 |
| Tenant Broken | SC `GET /control/v1/node/:id/shards` 中 state 字段 | 告警，记录原因 |
| 磁盘使用告警 | SC 心跳中的 `disk_usage_bytes` 超阈值 | 触发 `PageserverHighDiskUsage` 告警 |

#### 4.13.3 零侵入设计原则

**Operator 不管理 tenant，只监控 tenant**：

```
✅ Operator 的职责:
  ├── 确保 PS Pod 存活（健康探针 + 故障恢复）
  ├── 确保 PS 正确注册到 SC（re-attach 协议 + 地址稳定性）
  ├── 提供 PS 数量管理（声明式扩缩容）
  ├── 监控 PS 上的 tenant 状态（通过 SC API）
  └── 辅助安全下线（drain 协调）

❌ Operator 不应做:
  ├── 创建/删除 tenant（由 SC 和上层 API 管理）
  ├── 直接调用 PS location_config API（SC Reconciler 专用）
  ├── 直接管理 tenant 的 S3 存储
  └── 干预 SC 的调度决策
```

#### 4.13.4 数据流总览

```
┌──────────────┐     ┌────────────────┐     ┌──────────────┐
│   Operator   │────▶│ Storage Ctrl   │────▶│  Pageserver  │
│              │     │                │     │              │
│ • PS 数量    │     │ • Tenant 创建   │     │ • HTTP API   │
│ • Pod 生命周期│     │ • shard 调度    │     │ • WAL 接收   │
│ • K8s 探针   │     │ • 故障检测      │     │ • 磁盘驱逐   │
│ • Drain 协调 │     │ • 心跳收集      │     │ • 冷热启动   │
└──────┬───────┘     └───────┬────────┘     └──────┬───────┘
       │                     │                      │
       │  GET /node/:id      │  PUT /location_config│
       │  PUT /node/:id/drain│  GET /v1/utilization │
       │  PUT /node/:id/config│                     │
       │◄───────────────────▶│◄────────────────────▶│
       │                     │                      │
       │  SC API 客户端      │  SC Reconciler       │
       │  (sc_client.go)     │  (neon 内部)          │
       └─────────────────────┘                      │
                                                    │
       ┌────────────────────────────────────────────┘
       │  Operator 降级路径（SC 不可用时）:
       │  GET /v1/tenant  (页面状态查看)
       │  GET /v1/status  (进程健康)
       └──────────────────────────────────────────────
```

---

## 5. 实施路线图

### Phase 1: 基础健康与高可用（P0 项）— ✅ 大部分已完成

```
目标：Pageserver Pod 具备自愈能力和基本生产可用性

已实现 ✅:
  ├── specs/pageserver/statefulset.go
  │   ├── LivenessProbe / ReadinessProbe / StartupProbe
  │   ├── PodAntiAffinity (RequiredDuringScheduling, node 级别)
  │   ├── 探针可配置化 (ProbeConfig)
  │   ├── TerminationGracePeriodSeconds: 60
  │   ├── Resource Requests/Limits (默认 CPU=500m/2, Mem=256Mi/512Mi, 可覆盖)
  │   ├── AvailabilityZone 字段 + initScript AZ 注入
  │   ├── StatefulSet 签名变更 (ps, image, az) — ✅
  │   └── JWT Volume 挂载（/certs/public.pem）
  ├── specs/pageserver/pdb.go
  │   └── PodDisruptionBudget (maxUnavailable=1)
  ├── specs/pageserver/service.go
  │   └── Headless Service publishNotReadyAddresses=true
  ├── specs/pageserver/configmap.go
  │   └── 完整生产参数（认证/Broker/S3/磁盘驱逐/tenant_config/并发）
  ├── specs/storagecontroller/deployment.go
  │   └── StartupProbe + LivenessProbe + ReadinessProbe
  ├── api/v1alpha1/pageserver_types.go
  │   ├── AvailabilityZone 字段 — ✅
  │   ├── Resources 字段 — ✅
  │   ├── NodeFailure 字段 — ✅
  │   └── ProbeConfig 类型 — ✅
  ├── api/v1alpha1/cluster_types.go
  │   ├── NumPageservers 字段 — ✅
  │   └── PageserverConfig 类型（含 StorageSize/Resources/InitialSchedulingPolicy/NodeFailure）— ✅
  └── Golden file 同步 — ✅

待实现 🔴:
  └── specs/pageserver/statefulset.go
      └── 添加 topologySpreadConstraints (AZ 级别) — 仅此一项
```

### Phase 2: 配置调优 + 可观测性（P1 项，~1 周）

```
目标：生产级性能配置和监控

1. specs/pageserver/configmap.go
   - 增加 max_file_descriptors = 1000
   - 增加 page_cache_size = 32768
   - 增加 log_format = "json"
   - 增加 metric_collection_interval = "60s"

2. config/prometheus/ (新增)
   - ServiceMonitor 模板
   - PrometheusRule 模板

3. api/v1alpha1/pageserver_types.go
   - 新增 TenantCount, DiskUsage 等字段（P2 级别）
```

### Phase 3: 翻转为声明式扩缩容（P1，~1 周）

```
目标：Cluster Controller 自动管理 Pageserver 生命周期（与 Safekeeper 一致）

背景：以下全部已在 Phase 1&2 中单独实现，Phase 3 仅需串联 Cluster Controller：
  ✅ Pageserver CR finalize（四阶段安全下线）
  ✅ SC API 客户端（drain, configure, delete, list, get node/shards）
  ✅ Pageserver Status（SCSchedulingPolicy, SCAvailability, SCAttachedShardCount 等）
  ✅ NodeFailure recovery（handleNodeFailure + AutoRecover）
  ✅ Cluster.Spec.NumPageservers + Cluster.Spec.DefaultPageserverConfig (types 已定义)
  ✅ JWT Token 管理（ensurePSAuthTokens）

待实现：
  internal/controller/cluster_controller.go（或 cluster_create.go）
  └── 新增 reconcilePageservers()（与 reconcileSafekeepers() 同级）
      ├── 扩容：创建 PS CR → 设置 OwnerReference → 等待 Pod Ready + SC 注册
      │   └── 可选：根据 InitialSchedulingPolicy 设置新建节点调度策略
      └── 缩容：按 ID 降序处理
          └── 直接 delete PS CR → PS Controller finalize 自动执行四阶段 drain
              （无需 Cluster Controller 重复实现 drain 逻辑）
```

### Phase 4: 弹性与高级特性（P2 项，待定）

```
目标：AZ 感知 + 备份验证 + API 集成 + Tenant 可观测性

1. K8s topologySpreadConstraints (AZ 级别) — 仅此一项 P0 剩余
2. Controller 自动从 K8s Node label 推断 AZ — 自动化
3. 基于 SC API 的 Tenant 生命周期监控（见 4.13 节）
4. Pageserver HTTP API 集成层 — 运维诊断用（见 4.12 节）
5. S3 备份完整性校验
6. 零停机升级策略
7. Grafana Dashboard
8. PS→SC upcall 认证链路优化（长期，配合 upstream neon JWT scheme 演进）
```

---

## 附录 A: 文件改动预估

| # | 文件 | Phase | 改动类型 | 内容 | 状态 |
|---|------|:---:|:---:|------|:---:|
| 1 | `specs/pageserver/statefulset.go` | P1 | 修改 | Probes + AntiAffinity + Resources + AZ + JWT | ✅ 已完成 |
| 2 | `specs/pageserver/pdb.go` | P1 | 新增 | PodDisruptionBudget (maxUnavailable=1) | ✅ 已完成 |
| 3 | `specs/pageserver/configmap.go` | P2 | 修改 | 生产级 TOML 配置（认证/Broker/S3/驱逐/tenant_config） | ✅ 大部分完成 |
| 4 | `specs/pageserver/labels.go` | P1 | 修改 | 三层标签体系（资源/选择器/跨实例） | ✅ 已完成 |
| 5 | `api/v1alpha1/pageserver_types.go` | P1/P2/P3 | 修改 | Resources + AvailabilityZone + NodeFailure + ProbeConfig + Status | ✅ 已完成 |
| 6 | `api/v1alpha1/cluster_types.go` | P3 | 修改 | NumPageservers + PageserverConfig | ✅ types 已完成 |
| 7 | `internal/controller/pageserver_controller.go` | P1/P2/P3 | 修改 | finalize(四阶段) + handleNodeFailure + syncSCState + JWT | ✅ 已完成 |
| 8 | `internal/controller/pageserver_create.go` | P1 | 修改 | PDB reconcile + SC sync + JWT ensure + resource 传递 | ✅ 已完成 |
| 9 | `internal/controller/sc_client.go` | P3 | 修改 | 完整 SC 节点管理 API（drain/configure/delete/list/get/shards） | ✅ 已完成 |
| 10 | `internal/controller/cluster_controller.go` | P3 | 修改 | 新增 reconcilePageservers()（声明式扩缩容） | 🔴 待实现 |
| 11 | `utils/jwtmanager.go` + `utils/jwt_volumes.go` | P1 | 新增 | JWT 确定性生成 + 过期检测 + Volume 挂载 | ✅ 已完成 |
| 12 | `config/prometheus/servicemonitor.yaml` | P2 | 新增 | ServiceMonitor | 🔴 待实现 |
| 13 | `config/prometheus/prometheusrule.yaml` | P2 | 新增 | 告警规则 | 🔴 待实现 |
| 14 | `specs/pageserver/testdata/*.yaml` | P1/P2 | 更新 | Golden files | ✅ 已完成 |
| 15 | SC ConfigMap（外部配置） | P2 | 确认 | `handle_ps_local_disk_loss = true` | 🔴 待确认 |
| 16 | `specs/pageserver/statefulset.go` | P0 | 修改 | topologySpreadConstraints (AZ 级别) | 🔴 待实现 |

> **总结**：15 项改动中 **10 项已完成**（✅），仅剩 5 项待实现（🔴）。其中只有 topologySpreadConstraints 为 P0 级别，其余为 P1-P2。

---

## 附录 B: 设计决策记录

### B.1 为什么 Pageserver 始终是单副本？

| 原因 | 说明 |
|------|------|
| S3 是唯一持久化 | Pageserver 本地盘只是缓存，不需要副本冗余 |
| 写放大 | 多副本会导致每份 WAL 被多次处理 |
| SC 管理 shard | Storage Controller 通过 shard 分配管理冗余逻辑 |
| Neon 官方设计 | "there are no Page Server replicas" |

多 pageserver 节点通过 SC 的 shard 分配实现负载分担，而非数据副本。

### B.2 为什么用 `RequiredDuringScheduling` 而非 `Preferred`？

与 safekeeper 保持一致。Pageserver 同样是关键组件，同节点挂了影响面太大。

### B.3 节点故障恢复为什么不是全自动？

风险考虑：
- 误判节点宕机可能导致不必要的 PVC 删除
- 本地缓存清空后冷启动对业务有性能影响
- 首次实现作为 opt-in + 人工确认模式，后续可逐步自动化

### B.4 pageserver.toml 未知字段静默忽略的设计意义

neon 源码中 `ConfigToml` 的反序列化使用 `#[serde(default)]`，并且检测未知字段只 warn 不报错。这意味着新配置字段可以安全地加入到 pageserver.toml 而不影响旧版本 pageserver 的启动——设计文档中写入的新字段不会导致兼容性问题。

### B.5 为什么不用 Pod IP 而用 ClusterIP DNS 作为注册地址？

Storage Controller 的 `registration_match()` 要求 pageserver 重新注册时 HTTP 和 PG 地址必须与之前一致（`node.rs:134`）。如果使用 Pod IP，Pod 重建后 IP 会变化 → SC 拒绝注册 → pageserver 无限重试 → 需要管理员手动删除 SC 中的旧节点记录。

使用 ClusterIP DNS（如 `{ps-name}.{ns}`）解决问题：
- ClusterIP 是虚拟 IP，Pod 重建后不变
- K8s DNS 自动解析到新 Pod
- SC 的 DNS 验证也能通过（`tokio::net::lookup_host`）

当前 operator 已经正确使用 ClusterIP DNS（见 `statefulset.go` 的 initScript），无需修改。

### B.6 为什么 Pageserver 的故障转移不由 Operator 实现？

| 方面 | SC 实现 | Operator 自行实现 |
|------|---------|-------------------|
| 故障检测 | 心跳机制精确检测 pageserver 进程状态 | 只能检测 Pod 状态（粗糙） |
| 调度决策 | 了解每个 shard 的 AZ 亲和性、利用率 | 不了解 SC 内部状态 |
| 数据安全 | Generation number 防止脑裂 | 无法感知 generation |
| 迁移执行 | Reconciler 直接调用 pageserver API | 无法控制 pageserver 内部状态 |

SC 已经是一个完整的编排器，Operator 重新实现这些逻辑不仅重复，还会引入数据安全风险。Operator 的职责是确保 K8s 基础设施正确（地址稳定、磁盘管理、pod 恢复），而不是在应用层重新编排。

### B.7 `handle_ps_local_disk_loss` 标志的重要性

当 pageserver 丢失本地盘（新 PVC）后重新注册时，会上报 `empty_local_disk=true`。SC 中的 `re_attach` 方法检查 `self.config.handle_ps_local_disk_loss` 标志：

- **启用**：SC 主动清除该节点上所有 shard 的 observed locations → Reconciler 会重新配置这些 shard
- **未启用**：SC 不做额外处理 → observed locations 中保留旧信息 → 可能导致 Reconciler 状态不一致

**建议**：生产环境部署 SC 时启用 `--handle-ps-local-disk-loss` 标志。当前 neon 源码中该选项为 `clap(long, default_value_t = false)`，默认关闭，需要显式开启。

### B.8 Pageserver 数量管理：为什么用 Cluster.Spec（与 Safekeeper 一致）

| 维度 | Safekeeper | Pageserver |
|------|-----------|------------|
| **管理方式** | `Cluster.Spec.NumSafekeepers` → Cluster Controller 创建 SK CR | `Cluster.Spec.NumPageservers` → Cluster Controller 创建 PS CR（**相同模式**） |
| **数量特征** | 固定（≥3，WAL 共识协议要求） | 动态（1~N，由容量需求驱动） |
| **SC 数据模型** | timeline→safekeepers 映射 | 所有 PS 注册到该 SC，SC scheduler 分配 Tenant→PS |
| **Cell 归属** | 绑定至该 Cell（Cluster） | **同样绑定至该 Cell**（1 PS 只注册 1 SC） |
| **Cell 内多 Tenant** | per-timeline | cross-tenant pool |
| **缩容差异** | decommission（即时 API 调用，WAL 协议保证安全） | drain（需等待 SC 迁移 tenant shard，再删除） |
| **删除 Cluster** | 级联删除 SK CR | 级联删除 PS CR（PS finalizer 先 drain 确保数据安全） |

#### 架构一致性

Safekeeper 和 Pageserver 遵循**完全相同的 K8s 声明式模式**：

```
Step 1: 用户声明    Cluster.Spec.NumSafekeepers=3 / NumPageservers=5
Step 2: Cluster Controller  创建/删除 Safekeeper CR / Pageserver CR
Step 3: 各自 Controller     reconcile → StatefulSet, Service, PDB
Step 4: finalizer            SK: decommission → PS: drain → 删除
```

唯一差异是 finalizer 的实现复杂度：SK decommission 是即时 API 调用，PS drain 需要等待 SC 迁移 tenant shard。

#### 之前的错误分析

第一版分析推荐"独立 CRD，不由 Cluster 创建"，基于以下**已被否定**的假设：

| 错误假设 | 事实 |
|----------|------|
| "PS 可能服务其他 Cluster 的 Tenant" | 1 PS = 1 SC，不存在跨 Cluster 共享 |
| "动态数量不应写在 Spec 中" | K8s Spec 正是表达动态期望状态的机制 |
| "SC scheduler 与 Operator 冲突" | 操作在不同抽象层：SC 管 Tenant→PS placement，Operator 管 PS 数量 |
| "让用户手动创 PS CR 更灵活" | 手动操作无 drain 保护，声明式管理有 Controller 保证安全 |

#### 业界验证

所有主流 K8s 数据库/存储 Operator 都使用声明式数量管理：
- **Strimzi Kafka**: `Kafka.spec.kafka.replicas`
- **Zalando Postgres**: `postgresql.spec.numberOfInstances`
- **ECK Elasticsearch**: `Elasticsearch.spec.nodeSets[].count`
- **CassKop**: `CassandraCluster.spec.nodes`

无一使用"独立 CRD 手动创建"模式。

详细分析见 [1.5 节](#15-crd-架构决策-pageserver-的数量管理方式)。

---

## 附录 C: Cluster YAML 配置示例

### C.1 Cluster CRD 中 Pageserver 相关字段

```go
// api/v1alpha1/cluster_types.go

type ClusterSpec struct {
    // ... 其他字段 ...

    // NumPageservers 指定期望的 pageserver 数量。
    // 默认为 1（开发测试），生产环境建议 ≥ 2。
    // +kubebuilder:default:=1
    // +kubebuilder:validation:Minimum:=1
    NumPageservers int32 `json:"numPageservers,omitempty"`

    // DefaultPageserverConfig 指定自动创建的 pageserver 的默认配置。
    DefaultPageserverConfig *PageserverConfig `json:"defaultPageserverConfig,omitempty"`
}

type PageserverConfig struct {
    // StorageSize 指定 PS 的 PVC 大小。
    // +kubebuilder:default:="100Gi"
    StorageSize string `json:"storageSize,omitempty"`

    // Resources 指定 PS 的 CPU/内存配置。若未设置，使用 operator 内置默认值。
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

    // InitialSchedulingPolicy 新节点注册后的初始 SC 调度策略。
    // "Active"（默认）：立即参与全量调度。
    // "Filling"：仅接收新 shard placement（用于新节点预热）。
    // +kubebuilder:default:="Active"
    InitialSchedulingPolicy string `json:"initialSchedulingPolicy,omitempty"`

    // NodeFailure 控制节点故障时的自动恢复策略。
    // 用于为自动创建的 Pageserver 统一设置故障恢复行为。
    NodeFailure *NodeFailureRecoveryConfig `json:"nodeFailure,omitempty"`
}

type NodeFailureRecoveryConfig struct {
    // AutoRecover 是否在节点宕机时自动删除 PVC 并重建。
    // 默认 false（需要人工确认），可设为 true 启用自动恢复。
    AutoRecover bool `json:"autoRecover,omitempty"`

    // MaxPendingDuration 在判定 PVC 无法恢复前，Pod Pending 的最大等待时间。
    // 默认 5 分钟。
    MaxPendingDuration *metav1.Duration `json:"maxPendingDuration,omitempty"`
}
```

### C.2 开发/测试环境（单 Pageserver，低资源）

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: dev-cluster
  namespace: neon
spec:
  numSafekeepers: 3
  numPageservers: 1                              # 开发环境 1 个即可
  defaultPGVersion: 17
  neonImage: "ghcr.io/neondatabase/neon:latest"
  bucketCredentialsSecret:
    name: bucket-credentials
  storageControllerDatabaseSecret:
    name: storage-controller-pg-cluster
    key: uri
  defaultPageserverConfig:
    storageSize: 50Gi                            # 开发环境用小盘
    resources:
      requests:
        cpu: "500m"
        memory: 256Mi
      limits:
        cpu: "2"
        memory: 512Mi
```

**说明**：
- `numPageservers: 1` — 单节点开发模式，不启用扩容/缩容
- `resources` 使用开发环境默认值，可通过本字段覆盖
- `initialSchedulingPolicy` 未设置，默认 `Active`

### C.3 生产环境（多 Pageserver，高可用）

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: prod-cluster
  namespace: neon
spec:
  numSafekeepers: 3
  numPageservers: 4                              # 生产环境至少 2 个，按容量需求设置
  defaultPGVersion: 17
  neonImage: "ghcr.io/neondatabase/neon:8463"    # 生产使用固定版本
  bucketCredentialsSecret:
    name: bucket-credentials
  storageControllerDatabaseSecret:
    name: storage-controller-pg-cluster
    key: uri
  defaultPageserverConfig:
    storageSize: 2Ti                             # 生产环境大容量
    initialSchedulingPolicy: Active
    resources:
      requests:
        cpu: "4"
        memory: 8Gi
      limits:
        cpu: "8"
        memory: 16Gi
    nodeFailure:                                 # 生产环境启用自动恢复
      autoRecover: true
      maxPendingDuration: 5m
```

**说明**：
- `numPageservers: 4` — 4 个 pageserver 分担 Tenant 负载，由 SC scheduler 自动分配
- `storageSize: 2Ti` — 根据数据量预估设置 PVC 大小
- `resources` — 覆盖 operator 内置默认值，匹配生产机器规格
- `neonImage` 使用固定 tag 而非 `latest`，保证可复现
- `nodeFailure` — 生产环境开启自动恢复，节点宕机 5 分钟后自动删 PVC 重建

### C.4 扩容操作（声明式修改）

扩容：只需修改 `numPageservers` 字段。

```bash
# 从 4 个扩容到 6 个
kubectl patch cluster prod-cluster -n neon --type merge \
  -p '{"spec":{"numPageservers":6}}'
```

**Operator 自动执行**：
1. Cluster Controller 检测到 `numPageservers: 4→6`
2. 创建 2 个新的 Pageserver CR（带 OwnerReference）
3. Pageserver Controller 为每个新 CR 创建 StatefulSet + Service + PDB + ConfigMap
4. Pod 启动 → SC 注册 → `InitialSchedulingPolicy` 决定初始调度模式

### C.5 缩容操作（安全 Drain）

缩容：同样修改 `numPageservers`，Operator 会先安全 drain 再删除。

```bash
# 从 6 个缩容到 4 个
kubectl patch cluster prod-cluster -n neon --type merge \
  -p '{"spec":{"numPageservers":4}}'
```

**Operator 自动执行**：
1. Cluster Controller 检测到 `numPageservers: 6→4`
2. 选择 2 个待删除的 PS（优先选 attached shard 少的）
3. **Phase 0**：准入检查（是否有其他可调度节点）
4. **Phase 1**：`PUT /control/v1/node/{id}/drain` — 通知 SC 迁移 shard
5. **Phase 2**：轮询 `GET /node/{id}/shards` — 监控 attached shard count → 0
6. **Phase 3**：确认迁移完成 → `PauseForRestart` + `DELETE /node/{id}` → 删除 PS CR
7. K8s 级联清理 StatefulSet、Service、PVC 等资源

**超时处理**：30 分钟超时 → Condition `DrainTimeout` → 告警通知运维介入。

**取消缩容**：修改 `numPageservers` 恢复原值（仍需手动 `PUT /node/{id}/fill` 取消 drain）。

### C.6 Filling 模式：分阶段扩容

新节点使用 `Filling` 策略避免瞬时热点：

```yaml
spec:
  numPageservers: 3
  defaultPageserverConfig:
    initialSchedulingPolicy: Filling             # 新节点仅接收新 shard
```

**使用场景**：
- 新节点刚加入集群，本地缓存为空
- `Filling` 策略让 SC 仅将新创建的 shard 分配给它
- 避免已有热点 shard 迁移过来导致性能抖动
- 预热完成后，手动或通过日志判断，改为 `Active`

转换为 Active：
```bash
# 通过 SC API 手动将节点设为 Active
kubectl exec -n neon deploy/storage-controller -- \
  curl -X PUT http://localhost:8080/control/v1/node/{id}/fill -d '{"scheduling":"Active"}'
```

### C.7 Pageserver CR 状态查看

每个由 Cluster 自动创建的 Pageserver CR 都反映其实时状态：

```bash
$ kubectl get pageserver -n neon
NAME                       CLUSTER       READY   SC       SHARDS   AGE
prod-cluster-pageserver-0  prod-cluster  True    Active   3/10     5d
prod-cluster-pageserver-1  prod-cluster  True    Active   4/10     5d
prod-cluster-pageserver-2  prod-cluster  True    Active   3/10     5d
prod-cluster-pageserver-3  prod-cluster  True    Active   0/10     5d
```

各列含义：
- **CLUSTER** — 所属 Cluster（通过 `molnett.org/cluster` label 关联）
- **READY** — Pod 是否 Ready + SC 注册状态
- **SC** — Storage Controller 中的调度策略（Active / Filling / Draining / PauseForRestart）
- **SHARDS** — attached shard 数 / 总 shard 数

### C.8 字段完整参考

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `spec.numPageservers` | `int32` | `1` | 期望的 pageserver 数量，最小 1 |
| `spec.defaultPageserverConfig.storageSize` | `string` | `100Gi` | PVC 存储大小 |
| `spec.defaultPageserverConfig.initialSchedulingPolicy` | `string` | `Active` | 新节点 SC 调度策略：`Active` / `Filling` |
| `spec.defaultPageserverConfig.resources` | `ResourceRequirements` | 内置默认值 | CPU/内存 requests 和 limits |
| `spec.defaultPageserverConfig.nodeFailure.autoRecover` | `bool` | `false` | 是否自动删除 PVC 重建 |
| `spec.defaultPageserverConfig.nodeFailure.maxPendingDuration` | `Duration` | `5m` | Pod Pending 判定阈值 |
| `.status.conditions[type=PageserversAvailable]` | `Condition` | — | Pageserver 集群是否就绪 |
| `.status.conditions[type=Draining]` | `Condition` | — | 是否有 pageserver 正在 drain |
| `.status.conditions[type=DrainComplete]` | `Condition` | — | Drain 是否完成 |
| `.status.conditions[type=NodeRecoveryInProgress]` | `Condition` | — | 是否正在执行节点故障恢复 |

### C.9 节点故障自动恢复配置

#### 配置层级

`NodeFailure` 可在两个层级配置：

**层级 1：Cluster 级别（统一默认）**— 通过 `defaultPageserverConfig.nodeFailure` 为所有自动创建的 Pageserver 统一设置：

```yaml
spec:
  defaultPageserverConfig:
    nodeFailure:
      autoRecover: true
      maxPendingDuration: 5m
```

**层级 2：Pageserver 级别（单独覆盖）**— 直接修改单个 Pageserver CR，覆盖 Cluster 级默认：

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Pageserver
metadata:
  name: prod-cluster-pageserver-0
spec:
  nodeFailure:
    autoRecover: false             # 针对特定节点关闭自动恢复
    maxPendingDuration: 10m
```

#### 行为说明

| `autoRecover` | 行为 |
|:---:|------|
| `false`（默认） | 节点宕机后 PVC 粘滞 → Pod 永久 Pending → 需人工介入 |
| `true` | Pod Pending 超过 `maxPendingDuration` → Operator 自动删 PVC + 删 Pod → StatefulSet 新节点重建 |

#### 触发条件（Operator 内部逻辑）

1. Pod `Phase=PodPending`
2. 原因：volume node affinity conflict（PVC 绑定到已宕机节点的 PV）
3. Pending 时长 > `maxPendingDuration`
4. 满足以上 → 自动化执行：Delete Pod → Delete PVC → STS 自动重建

#### 恢复流程

```
节点宕机
  → Pod Pending（PVC 粘滞）
  → 等待 maxPendingDuration（默认 5 分钟）
  → Condition: NodeRecoveryInProgress=True
  → Delete Pod（非级联）
  → Delete PVC
  → StatefulSet 创建新 Pod + 新 PVC（在新节点）
  → Pageserver 从 S3 冷启动
  → Register to SC
  → 恢复完成
```
