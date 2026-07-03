# Safekeeper 生产级自动化部署 — 详细设计

> 状态: 草案  
> 日期: 2025-06  
> 关联: [上游 neon 分析](./upstream-neon-analysis.md), [todo 清单](./todo.md), [safekeeper 删除](./safekeeper-deletion.md)

---

## 目录

1. [背景与动机](#1-背景与动机)
2. [CRD 架构决策：合入 Cluster vs 独立定义](#2-crd-架构决策合入-cluster-vs-独立定义)
3. [上游 neonatal 调研](#3-上游-neon-调研)
4. [全面设计方案](#4-全面设计方案)
5. [实施计划](#5-实施计划)
6. [风险评估](#6-风险评估)
7. [附录](#7-附录)

---

## 1. 背景与动机

### 1.1 当前状态

neon-operator 中 Safekeeper 当前存在以下主要缺口：

| 维度 | 当前状态 | 影响 |
|------|---------|:--:|
| **自动创建** | `ClusterSpec.NumSafekeepers=3`（默认值），但 Cluster Controller 不创建 Safekeeper CR | 🔴 |
| **健康探针** | ✅ 已实现 Liveness / Readiness / Startup Probe（见 3.2 深度分析） | — |
| **反亲和性** | ✅ 已实现 PodAntiAffinity (Node 级) | — |
| **PDB** | 没有 PodDisruptionBudget，维护时可能同时驱逐多个 | 🟡 |
| **删除流程** | 只清理 K8s 资源，不向 Storage Controller 发送注销请求 | 🟡 |
| **状态聚合** | Cluster Status 不反映 Safekeeper 状态 | 🟡 |

### 1.2 为什么现在做

Safekeeper 是 Neon "计算存储分离" 架构中 WAL 持久性的基石：

```
Compute Node ──WAL──→ Safekeeper(s) ──WAL──→ Pageserver ──→ S3
                          │                        │
                      WAL Quorum              L0/L1 Delta Layers
                      (N/2 + 1 = 2/3)
```

- **WAL Paxos 共识协议**依赖 ≥3 个 Safekeeper 形成 Quorum（`(N/2)+1`）
- 当前手动创建 Safekeeper 的方式无法保证 3 个副本分布在 3 个不同节点
- 缺失的健康探针意味着进程僵死时 K8s 无法自动恢复
- 后续 Phase 3（JWT 认证）和 Phase 4（去除 `--dev`）都依赖 Safekeeper 先就绪

### 1.3 目标

完成 Safekeeper 的生产级自动化部署，实现：
- Cluster Controller 根据 `NumSafekeepers` 自动创建/更新/删除 Safekeeper CR
- 每个 Safekeeper Pod 具备完整的健康探针
- 自动保证多副本反亲和性（不同节点）
- PodDisruptionBudget 保障维护期间 Quorum 不丢失
- Cluster Status 聚合所有 Safekeeper 的就绪状态

---

## 2. CRD 架构决策：合入 Cluster vs 独立定义

### 2.1 当前架构

```
Cluster (CR)                        ── 顶层入口
├── 自动创建: StorageController + StorageBroker
├── 声明控制: NumSafekeepers=3
└── 不自动创建: Safekeeper, Pageserver, Project

Safekeeper (CR)                     ── 独立 CR，需手动创建
├── Spec: id, cluster, storageConfig
├── Status: conditions
└── 由 Safekeeper Controller 管理
     ├── Service (普通 + Headless)
     └── StatefulSet (单副本)
```

### 2.2 方案对比

#### 方案 A：保持独立 CRD（推荐 ✅）

```yaml
# 用户只需创建 Cluster，Operator 自动创建 Safekeeper CR
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: my-neon
spec:
  numSafekeepers: 3
  neonImage: "neondatabase/neon:latest"
```

Operator 行为：
```
Cluster Reconcile
  ├── JWT Secret
  ├── Storage Controller
  ├── Storage Broker
  └── reconcileSafekeepers()          # 新增
       ├── 计算期望数量 (numSafekeepers)
       ├── 创建不足的 Safekeeper CR
       ├── 删除多余的 Safekeeper CR
       └── 更新 Cluster Status

每个 Safekeeper CR → Safekeeper Controller
  ├── Service (ClusterIP)
  ├── Headless Service
  ├── StatefulSet (1 replica, 含 Probes+AntiAffinity)
  ├── PodDisruptionBudget              # 新增
  └── 向 Storage Controller 注册
```

#### 方案 B：合入 Cluster CRD

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: my-neon
spec:
  safekeepers:                          # 嵌入到 Cluster spec
    count: 3
    storageConfig:
      size: 10Gi
  neonImage: "neondatabase/neon:latest"
```

所有 Safekeeper 资源由 Cluster Controller 直接管理（不再有独立的 Safekeeper CR）。

### 2.3 对比分析

| 维度 | 方案 A：独立 CRD | 方案 B：合入 Cluster |
|------|:--:|:--:|
| **声明式状态** | ✅ 每个 Safekeeper 有独立的期望/实际状态 | ❌ 多副本状态混在一起 |
| **故障隔离** | ✅ 单 Safekeeper 故障不影响其他 | ❌ Cluster Controller 需处理所有 |
| **K8s 原生可观测性** | ✅ `kubectl get safekeepers` 一目了然 | ❌ 只能在 Cluster status 中查 |
| **Operator 模式** | ✅ Reconcile 单对象，逻辑清晰 | ❌ Cluster 控制器臃肿 |
| **扩缩容安全** | ✅ 逐个 CR 独立操作，可审计 | ❌ 原子操作点多，难回溯 |
| **Finalizer 机制** | ✅ 每个 Safekeeper 独立 Finalizer，删除安全 | ❌ 需内部实现类似机制 |
| **与现有架构一致** | ✅ Pageserver 也是独立 CRD | ❌ 不一致 |
| **代码复杂度** | ✅ Cluster Controller 新增 ~80 行 | ❌ Cluster Controller 新增 ~300 行 |
| **用户体验** | ✅ `kubectl get safekeepers` + 自动创建 | ✅ 用户只操作一个 CR |
| **调试便利性** | ✅ 可单独 `kubectl delete safekeeper X` 修复 | ❌ 必须操作 Cluster |
| **测试独立性** | ✅ Safekeeper 测试不依赖 Cluster 测试改动 | ❌ 测试耦合 |

### 2.4 深度分析

#### 2.4.1 为什么独立 CRD 更适合生产级

**1. 故障恢复**

当 3 个 Safekeeper 中 1 个发生问题时：

```
方案 A (独立 CRD)：
  kubectl delete safekeeper X          # 删除故障 Safekeeper
  → SafekeeperFinalizer → Storage Controller 注销
  → K8s 级联删除 StatefulSet + PVC
  → Cluster Controller 检测到 count < 3
  → 自动创建新 Safekeeper CR
  → Safekeeper Controller 创建新 StatefulSet + 注册

方案 B (合入)：
  kubectl edit cluster my-neon         # 修改 safekeepers: {count: 2}
  # 无法精确删除某个故障 Safekeeper
  # 或者需要实现额外的选择机制
```

**2. Status 表达**

```
方案 A：
  kubectl get safekeepers
  NAME                          AVAILABLE   PROGRESSING   AGE
  my-neon-safekeeper-1          True        False         3d
  my-neon-safekeeper-2          True        False         3d
  my-neon-safekeeper-3          False       True          3d

方案 B：
  kubectl get cluster my-neon -o yaml
  # 需要解析嵌套的 status.safekeepers[].conditions[]
```

**3. 控制器职责**

Kubernetes Operator 最佳实践：一个 Controller 管理一种 CRD。这称为"微控制器"（micro-controller）模式，类似 etcd-operator。

Cluster Controller 的职责是"编排"，通过创建子 CR 来声明意图；Safekeeper Controller 的职责是"实现"，将 Safekeeper CR 转化为 K8s 工作负载。

**4. 与 Storage Controller 交互**

每个 Safekeeper 独立向 Storage Controller 注册（`POST /control/v1/safekeeper/{id}`），这是 Neon 架构的核心设计。独立 CRD 天然匹配这种"每个 Safekeeper 独立身份"的模型。

#### 2.4.2 合入方案的优势场景

合入方案在某些场景下也有优势：

- **极简部署**：单文件定义整个集群
- **原子性保证**：所有资源作为一个整体创建/删除
- **减少 CR 数量**：一个集群 = 1 个 CR（而不是 1+N 个）

但这些优势在**生产级**场景中不够重要：
- 自动创建机制已经解决了"极简部署"问题（用户仍然只需创建 1 个 Cluster CR）
- 原子性在 Kubernetes 中并非绝对保证（多个子资源创建仍然是非原子的）
- CR 数量本身不影响 K8s 性能

### 2.5 决策

**推荐方案 A：保持独立 CRD，Cluster Controller 自动创建**

理由：
1. Operator 最佳实践：单一职责控制器
2. K8s 原生可观测性：独立 `kubectl get` 查询
3. 故障隔离与修复：可单点操作
4. 与现有架构一致（Pageserver 同模式）
5. 与 Neon 上游架构匹配（每个 Safekeeper 独立 ID 和生命周期）
6. 代码改动量更小、更安全

---

## 3. 上游 Neon 调研

### 3.1 Safekeeper HTTP API

**`neon/safekeeper/src/http/routes.rs`** 中可用的 HTTP 端点：

| 方法 | 路径 | 认证 | 用途 |
|------|------|:--:|------|
| GET | `/v1/status` | 无需认证 | 健康检查，返回 `{ "id": <node_id> }` |
| GET | `/metrics` | 无需认证 | Prometheus 指标 |
| GET | `/v1/utilization` | 需要认证 | Timeline 数量统计 |
| GET | `/v1/debug/filesystem_usage` | 需要认证 | 磁盘使用诊断 |
| PUT | `/v1/failpoints` | 需要认证 | 故障注入（测试用） |
| PUT | `/v1/tenant/:tenant_id/timeline/:timeline_id` | 需要认证 | Timeline 驱逐/恢复 |
| DELETE | `/v1/tenant/:tenant_id` | 需要认证 | 删除租户 |
| POST | `/v1/tenant/timeline` | 需要认证 | 创建 Timeline |
| GET | `/v1/tenant/:tenant_id/timeline/:timeline_id` | 需要认证 | 获取 Timeline 状态 |

**关键发现**：
- **只有 `/v1/status` 可用作健康探针**（无需认证）
- 没有独立的 `/v1/live` 或 `/v1/ready` 端点
- 健康探针建议使用 `/v1/status`，它在 ALLOWLIST_ROUTES 中，不要求 JWT

### 3.2 Safekeeper 健康探针 — 深度分析与实现验证

> **状态更新 (2026-06)**：Safekeeper 的三种探针（Liveness/Readiness/Startup）已在 `specs/safekeeper/statefulset.go` 中完整实现。本节验证现有实现与上游 Neon 设计的对齐情况，并分析剩余差距。

#### 3.2.1 上游 Neon Safekeeper 健康端点分析

**① `/v1/status` 端点**（`neon/safekeeper/src/http/routes.rs:45-50`）：

```rust
async fn status_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    let conf = get_conf(&request);
    let status = SafekeeperStatus { id: conf.my_id };
    json_response(StatusCode::OK, status)
}
```

- **总是返回 HTTP 200**，只返回 `{"id": <NodeId>}`
- **在 auth 白名单中**（`routes.rs:711`），无需 JWT
- **不做任何内部健康检查**——不检查 WAL、不检查远程存储、不检查 timeline 状态

**② `/v1/utilization` 端点**（`neon/safekeeper/src/http/routes.rs:125-129`）：

```rust
async fn utilization_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?;
    let global_timelines = get_global_timelines(&request);
    let utilization = global_timelines.get_timeline_counts();
    json_response(StatusCode::OK, utilization)
}
```

- SC 心跳的目标端点（与 pageserver 一致）
- 统计活跃 timeline 数量（过滤掉正在创建中和被取消的）
- 响应体：`{"timeline_count": 42}`

**③ SC 心跳中 Safekeeper 与 Pageserver 的关键区别**：

| 特征 | Pageserver | Safekeeper |
|------|-----------|------------|
| 心跳端点 | `/v1/utilization` | `/v1/utilization` |
| `WarmingUp` 状态 | ✅ 有 | ❌ **无** |
| `max_warming_up_interval` | 300s（冷启动长宽限） | N/A |
| `max_offline_interval` | 30s | 30s |
| 离线判定 | 失败 + 非 WarmingUp → Offline | 失败 → Offline（立即） |

Safekeeper **没有 WarmingUp 状态**——启动快速，不需要长宽限。

#### 3.2.2 当前实现验证

**已实现**（`specs/safekeeper/statefulset.go:117-156`）：

```go
LivenessProbe:   { path: "/v1/status", port: 7676, initialDelay: 10s, period: 10s, timeout: 5s, failure: 3 }
ReadinessProbe:  { path: "/v1/status", port: 7676, initialDelay: 5s,  period: 5s,  timeout: 3s, failure: 2 }
StartupProbe:    { path: "/v1/status", port: 7676, initialDelay: 5s,  period: 5s,  timeout: 5s, failure: 12 }
```

#### 3.2.3 参数对齐与差异分析（与 Pageserver 对比）

| 参数 | Pageserver | Safekeeper | 差异原因 |
|------|-----------|------------|----------|
| LivenessProbe.initialDelaySeconds | 30s | 10s | Pageserver 启动更慢（租户加载） |
| StartupProbe.failureThreshold | 30 (~300s) | 12 (~60s) | Pageserver 冷启动需从 S3 加载 |
| LivenessProbe.periodSeconds | 10s | 10s | ✅ 一致 |
| LivenessProbe.failureThreshold | 3 | 3 | ✅ 一致 |

**关键对齐**：Safekeeper 的 `StartupProbe.failureThreshold = 12`（~60s）是合理的——Safekeeper 不需要冷启动从 S3 加载，启动通常 10-30 秒完成。

**与 SC 心跳参数对齐**：
- K8s LivenessProbe ~30s 检测失败 → SC `max_offline_interval = 30s` → ✅ 对齐
- Safekeeper 无 `WarmingUp` → StartupProbe 较短（60s）→ ✅ 对齐

#### 3.2.4 与 Pageserver 的四级健康检查模型对比

```
Safekeeper 健康层次：

Layer 1: K8s StartupProbe    ─── 覆盖启动窗口（60s）
        │                        /v1/status → HTTP 200
        │
Layer 2: K8s LivenessProbe   ─── 检测进程卡死（~30s 内）
        │                        /v1/status → HTTP 200
        │
Layer 3: K8s ReadinessProbe  ─── 流量就绪信号（~10s 内）
        │                        /v1/status → HTTP 200
        │
Layer 4: SC Heartbeat        ─── 深度健康检查（每 5s）
                                 /v1/utilization → timeline 计数
                                 失败 → 立即标记 Offline（无 WarmingUp 过渡）
```

**与 Pageserver 的核心差异**：Safekeeper Layer 4 无 `WarmingUp` 缓冲——一旦心跳失败立即标记 Offline。这不需要 Layer 2 的任何特殊配合。

#### 3.2.5 剩余差距（已全部解决）

> **状态更新 (2026-06-29)**：所有可解决的差距均已实现。

| 差距 | 严重程度 | 状态 | 说明 |
|------|:---:|:---:|------|
| 探针参数硬编码 | 🟡 中 | ✅ 已解决 | `SafekeeperSpec` 已添加 `LivenessProbe`/`ReadinessProbe`/`StartupProbe` 可选的 `*ProbeConfig` 字段，通过 `probeWithConfig()` 辅助函数实现可配置 |
| 无 `/v1/live` `/v1/ready` 端点 | 🟢 低 | 🟢 无需修复 | 上游 Safekeeper 只有 `/v1/status`，与 Pageserver 一致 |

**实现详情**：
- API 类型：`api/v1alpha1/safekeeper_types.go` → `SafekeeperSpec` 新增 `LivenessProbe`/`ReadinessProbe`/`StartupProbe *ProbeConfig`
- 探针构建：`specs/safekeeper/statefulset.go` → `podSpec()` 使用 `probeWithConfig()` 构建可配置探针
- 完整可配置化方案参见 [Pageserver 4.1.8 节](./pageserver-production.md#418-探针可配置化已实现)

#### 3.2.6 关键源码引用

| 引用 | 说明 |
|------|------|
| `neon/safekeeper/src/http/routes.rs:45-50` | `/v1/status` handler — 总是 200, `{"id":<NodeId>}` |
| `neon/safekeeper/src/http/routes.rs:125-129` | `/v1/utilization` handler — SC 心跳目标 |
| `neon/safekeeper/src/http/routes.rs:741` | 路由注册 `GET /v1/status` |
| `neon/safekeeper/src/http/routes.rs:711` | `/v1/status` 在白名单中 |
| `neon/storage_controller/src/heartbeater.rs:320-448` | Safekeeper 心跳状态机（无 WarmingUp） |
| `neon/libs/safekeeper_api/src/models.rs:18-20` | `SafekeeperStatus { id: NodeId }` |

### 3.3 Storage Controller 中的 Safekeeper 管理

**文件**: `neon/storage_controller/src/service/safekeeper_service.rs`

#### 调度策略生命周期

```
Activating ──→ Active ──→ Offline ──→ (从调度中移除)
                  │                      ↑
                  └────→ Offline ────────┘
```

对应的 `SkSchedulingPolicy`：
- `Activating`: 新注册，等待心跳验证
- `Active`: 正常参与调度
- `Offline`: 心跳丢失/主动下线，不参与调度
- `Decommissioned`: 永久注销（如果实现）

#### HTTP API

**已确认的路由** (来自 `storage_controller/src/http.rs` 的 router 定义):

| 方法 | 路径 | 认证 | 用途 |
|------|------|:--:|------|
| GET | `/control/v1/safekeeper` | 需要认证 | 列出所有 safekeeper |
| GET | `/control/v1/safekeeper/:id` | 需要认证 | 获取单个 safekeeper（仅测试用） |
| POST | `/control/v1/safekeeper/:id` | 需要认证 | **Upsert（注册/更新）** |
| POST | `/control/v1/safekeeper/:id/scheduling_policy` | 需要认证 | 设置调度策略 |

**关键发现**: Storage Controller **没有** `DELETE /control/v1/safekeeper/:id` 端点。safekeeper 节点级别的删除无法通过 HTTP API 完成。需要通过 `scheduling_policy` 机制来管理 safekeeper 的生命周期（设为 `Decomissioned` → SC 停止 reconciler, heartbeater 跳过该节点）。

#### 注册请求体

```json
{
    "id": 1,
    "region_id": "se-ume",
    "version": 1,
    "host": "my-neon-safekeeper-1.my-neon-safekeeper-1-headless",
    "port": 5454,
    "http_port": 7676,
    "availability_zone_id": "az-a"
}
```

### 3.4 Safekeeper 命令行参数

**文件**: `neon/safekeeper/bin/safekeeper.rs`

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--id` | 必填 | Safekeeper 唯一 ID（≥1） |
| `--listen-pg` | `127.0.0.1:5454` | Compute 连接端口 |
| `--listen-http` | `127.0.0.1:7676` | HTTP API 端口 |
| `--advertise-pg` | 从 `listen-pg` 推导 | 向 broker 公布的地址 |
| `--broker-endpoint` | 可选 | Storage Broker gRPC 地址 |
| `--datadir` | `./data` | 数据目录 |
| `--no-sync` | 未设置（默认 fsync 启用） | 跳过 fsync，不等待数据安全写入磁盘（不安全，仅开发/测试用） |
| `--wal-backup-parallel-jobs` | 5 | WAL 备份到远程存储的并发上传数 |
| `--disable-wal-backup` | 未设置（默认启用） | 禁用 WAL 远程备份 |
| `--remote-storage` | 无 | 远程存储 S3 配置（TOML 内联表） |

**生产级注意事项**：
- safekeeper **默认开启 fsync**，不需要显式设置任何同步参数
- 生产环境**不应设置** `--no-sync`，该标志会跳过 fsync，导致数据持久性风险
- safekeeper 默认行为就是生产安全的，WAL 写入会等待 fsync 完成才向 Compute 返回确认
- `--disable-wal-backup` / `--wal-backup-parallel-jobs` / `--remote-storage` 用于控制 WAL 远程备份行为

---

## 4. 全面设计方案

### 4.1 总体架构

```
┌─────────────────────────────────────────────────────────────────────┐
│                         生产级 Safekeeper 部署                       │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Cluster Controller                          Safekeeper Controller   │
│  ┌───────────────────┐                      ┌────────────────────┐  │
│  │ reconcileCluster()│                      │ reconcileSK()      │  │
│  │  ├─ JWT Secret    │                      │  ├─ Service        │  │
│  │  ├─ SC + Broker   │                      │  ├─ Headless Svc   │  │
│  │  ├─ safekeepers() │  ── 创建/删除 CR ──→  │  ├─ StatefulSet    │  │
│  │  │  (NEW)         │                      │  │  └─ Probes ✨    │  │
│  │  └─ updateStatus()│                      │  │  └─ AntiAff ✨   │  │
│  └───────────────────┘                      │  ├─ PDB ✨          │  │
│         │                                    │  ├─ 注册 SC        │  │
│         │  Status 聚合 ✨                     │  └─ 注销 SC ✨     │  │
│         ▼                                    └────────────────────┘  │
│  Cluster Status                                                      │
│  ├─ StorageControllerAvailable                                       │
│  ├─ StorageBrokerAvailable                                           │
│  └─ SafekeepersAvailable ✨                                          │
│      ├─ Ready: 3/3                                                   │
│      └─ Condition: SafekeeperQuorum                                  │
└─────────────────────────────────────────────────────────────────────┘
```

### 4.2 Cluster Controller 改动

#### 4.2.1 reconcileSafekeepers 函数

**文件**: `internal/controller/cluster_create.go` (新增)

```go
// reconcileSafekeepers ensures the correct number of Safekeeper CRs exist
// for the cluster, creating missing ones and deleting excess ones.
func (r *ClusterReconciler) reconcileSafekeepers(
    ctx context.Context,
    cluster *neonv1alpha1.Cluster,
) error {
    desired := int(cluster.Spec.NumSafekeepers)

    // Step 1: List existing Safekeeper CRs owned by this cluster
    var existing neonv1alpha1.SafekeeperList
    if err := r.List(ctx, &existing,
        client.InNamespace(cluster.Namespace),
        client.MatchingLabels{
            safekeepers.ClusterLabel: cluster.Name,
        },
    ); err != nil {
        return fmt.Errorf("list safekeepers: %w", err)
    }

    // Step 2: Build a map of existing IDs
    existingMap := make(map[uint32]*neonv1alpha1.Safekeeper)
    for i := range existing.Items {
        sk := &existing.Items[i]
        existingMap[sk.Spec.ID] = sk
    }

    // Step 3: Create missing Safekeeper CRs
    for id := uint32(1); id <= uint32(desired); id++ {
        if _, exists := existingMap[id]; !exists {
            if err := r.createSafekeeper(ctx, cluster, id); err != nil {
                return fmt.Errorf("create safekeeper %d: %w", id, err)
            }
        }
    }

    // Step 4: Delete excess Safekeeper CRs (scale down)
    for _, sk := range existingMap {
        if sk.Spec.ID > uint32(desired) {
            if err := r.deleteSafekeeper(ctx, sk); err != nil {
                return fmt.Errorf("delete safekeeper %d: %w", sk.Spec.ID, err)
            }
        }
    }

    return nil
}

// createSafekeeper creates a new Safekeeper CR for the given cluster and ID.
func (r *ClusterReconciler) createSafekeeper(
    ctx context.Context,
    cluster *neonv1alpha1.Cluster,
    id uint32,
) error {
    sk := &neonv1alpha1.Safekeeper{
        ObjectMeta: metav1.ObjectMeta{
            Name:      safekeeperName(cluster.Name, id),
            Namespace: cluster.Namespace,
            Labels: map[string]string{
                safekeepers.ClusterLabel: cluster.Name,
                // Propagate other common labels as needed
            },
        },
        Spec: neonv1alpha1.SafekeeperSpec{
            ID:      id,
            Cluster: cluster.Name,
            StorageConfig: neonv1alpha1.StorageConfig{
                Size: "10Gi", // Default; make configurable later
            },
        },
    }
    // Set owner reference for cascading deletion
    if err := ctrl.SetControllerReference(cluster, sk, r.Scheme); err != nil {
        return err
    }
    return r.Create(ctx, sk)
}

// deleteSafekeeper initiates deletion of an excess Safekeeper CR.
func (r *ClusterReconciler) deleteSafekeeper(
    ctx context.Context,
    sk *neonv1alpha1.Safekeeper,
) error {
    // Only delete if not already being deleted
    if sk.DeletionTimestamp != nil {
        return nil
    }
    // First, mark the safekeeper as Offline in Storage Controller
    // to gracefully drain it from quorum
    if err := r.decommissionSafekeeperFromStorageController(ctx, sk); err != nil {
        // Log warning but don't block deletion; safekeeper finalizer
        // handles the cleanup independently. SC will also detect
        // the safekeeper as Offline via heartbeat timeout.
        log.FromContext(ctx).Error(err, "failed to decommission safekeeper in SC, proceeding with deletion")
    }
    return r.Delete(ctx, sk)
}

// decommissionSafekeeperFromStorageController sets the safekeeper's
// scheduling policy to Offline in the Storage Controller.
// The SC will stop assigning new timelines to this safekeeper and
// eventually remove it from the active set.
func (r *ClusterReconciler) decommissionSafekeeperFromStorageController(
    ctx context.Context,
    sk *neonv1alpha1.Safekeeper,
) error {
    url := fmt.Sprintf("%s/control/v1/safekeeper/%d/scheduling_policy",
        r.StorageControllerBaseURL, sk.Spec.ID)
    body := map[string]string{"scheduling_policy": "Decomissioned"}
    jsonBody, _ := json.Marshal(body)

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
        bytes.NewReader(jsonBody))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return fmt.Errorf("set SC scheduling policy: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode >= 300 {
        return fmt.Errorf("SC returned %d", resp.StatusCode)
    }
    return nil
}
```

#### 4.2.2 Cluster Controller reconcile 流程改动

**文件**: `internal/controller/cluster_controller.go` (修改)

```go
func (r *ClusterReconciler) reconcile(ctx context.Context, cluster *neonv1alpha1.Cluster) error {
    if err := r.createClusterResources(ctx, cluster); err != nil {
        return err
    }
    // NEW: Reconcile Safekeeper CRs
    if err := r.reconcileSafekeepers(ctx, cluster); err != nil {
        return err
    }
    return r.updateStatus(ctx, cluster)
}
```

#### 4.2.3 Cluster Status 聚合

**文件**: `internal/controller/cluster_controller.go` (修改)

```go
func (r *ClusterReconciler) updateStatus(ctx context.Context, cluster *neonv1alpha1.Cluster) error {
    // ... existing StorageController/StorageBroker status ...

    // NEW: Aggregate Safekeeper status
    var sks neonv1alpha1.SafekeeperList
    if err := r.List(ctx, &sks,
        client.InNamespace(cluster.Namespace),
        client.MatchingLabels{safekeepers.ClusterLabel: cluster.Name},
    ); err != nil {
        return err
    }

    ready := 0
    for _, sk := range sks.Items {
        if meta.IsStatusConditionTrue(sk.Status.Conditions, "Available") {
            ready++
        }
    }

    quorumRequired := int(cluster.Spec.NumSafekeepers)/2 + 1
    quorumMet := ready >= quorumRequired

    meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
        Type:   "SafekeepersAvailable",
        Status: metav1.ConditionFalse,
        Reason: "SafekeepersNotReady",
        Message: fmt.Sprintf("%d/%d ready (quorum=%d)",
            ready, len(sks.Items), quorumRequired),
    })

    if quorumMet {
        meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
            Type:    "SafekeepersAvailable",
            Status:  metav1.ConditionTrue,
            Reason:  "SafekeepersQuorumMet",
            Message: fmt.Sprintf("%d/%d ready (quorum=%d)",
                ready, len(sks.Items), quorumRequired),
        })
    }

    return r.Status().Update(ctx, cluster)
}
```

### 4.3 Safekeeper Controller 改动

#### 4.3.1 删除流程：安全注销

**文件**: `internal/controller/safekeeper_controller.go` (修改)

```go
func (r *SafekeeperReconciler) finalize(
    ctx context.Context,
    sk *neonv1alpha1.Safekeeper,
) (ctrl.Result, error) {
    // Step 1: Mark as Decomissioned in Storage Controller
    // Storage Controller 没有 DELETE safekeeper 端点，通过 scheduling_policy 管理生命周期。
    // Decomissioned → SC 停止 reconciler，heartbeater 跳过该节点。
    if err := r.setSchedulingPolicy(ctx, sk, "Decomissioned"); err != nil {
        // If SC is unreachable, still proceed with deletion
        // (the SC will detect the safekeeper as Offline via heartbeat timeout anyway)
        log.FromContext(ctx).Error(err, "failed to set SC scheduling policy to Decomissioned")
    }

    // Step 2: Remove K8s Finalizer
    controllerutil.RemoveFinalizer(sk, utils.FinalizerName)
    if err := r.Update(ctx, sk); err != nil {
        return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
    }
    return ctrl.Result{}, nil
}

func (r *SafekeeperReconciler) setSchedulingPolicy(
    ctx context.Context,
    sk *neonv1alpha1.Safekeeper,
    policy string,
) error {
    url := fmt.Sprintf("%s/control/v1/safekeeper/%d/scheduling_policy",
        r.StorageControllerBaseURL, sk.Spec.ID)

    type SchedulingPolicyRequest struct {
        SchedulingPolicy string `json:"scheduling_policy"`
    }
    body := SchedulingPolicyRequest{SchedulingPolicy: policy}
    jsonBody, _ := json.Marshal(body)

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
        bytes.NewReader(jsonBody))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return fmt.Errorf("set SC scheduling policy: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode >= 300 {
        return fmt.Errorf("SC returned %d", resp.StatusCode)
    }
    return nil
}
```

**关于 DELETE 端点缺失的说明**：

上游 Neon 的 Storage Controller 当前**没有** `DELETE /control/v1/safekeeper/:id` 端点。
safekeeper 节点删除通过以下机制实现：

1. 将 safekeeper 的 `scheduling_policy` 设为 `Decomissioned` —— SC 停止 reconciler，不再分配新 timeline
2. 已有 timeline 通过 `safekeeper_migrate` API 迁移到其他 safekeeper
3. SC heartbeater 跳过 `Decomissioned` 节点，内存中标记为 Offline
4. DB 中的记录保留，便于重建时直接 re-upsert 恢复

> **注意**：`SkSchedulingPolicy` 枚举值为 `Active`、`Activating`、`Pause`、`Decomissioned`（注意 upstream 的拼写）。`"Offline"` 不是合法的 scheduling_policy 值，必须使用 `"Decomissioned"`。

#### 4.3.2 Operator 在恢复流程中的职责（仅配置传递）

**结论**：Operator **不负责** 检测节点变更或触发 `pull_timeline`。恢复由 Storage Controller 全权负责。详见 [4.5.3 架构决策分析](#453-谁负责触发恢复-架构决策分析)。

**Operator 需要在 StatefulSet 中传递的关键配置**：

在 `specs/safekeeper/statefulset.go` 中，确保 safekeeper 启动参数正确配置了 SC 和 broker 连接：

```go
func args(sk *neonv1alpha1.Safekeeper) []string {
    return []string{
        // ... existing args ...
        "--id=" + strconv.FormatUint(uint64(sk.Spec.ID), 10),

        // Storage Controller 连接（用于 safekeeper 自注册）
        "--storage-controller-url=" + scURL,  // e.g., http://storage-controller:50051

        // Storage Broker 连接（safekeeper 通过 broker 与 SC/peer 通信）
        "--broker-addr=" + brokerAddr,         // e.g., http://storage-broker:50051

        // JWT Token（用于 SC 和 peer 认证）
        "--auth-token=$(JWT_TOKEN)",
    }
}
```

**Operator 只需确保**：
1. safekeeper 进程可以连接到 SC 和 broker
2. safekeeper 的 `--id` 稳定不变
3. safekeeper 可以访问 peer safekeeper 的网络地址

**不做的事情**：
- 不查询 safekeeper 的 `/v1/status` 或 `/v1/timeline`
- 不查询 peer safekeeper
- 不调用 `POST /v1/pull_timeline`
- 不判断哪些 timeline 需要恢复

### 4.4 StatefulSet 规格改动

**文件**: `specs/safekeeper/statefulset.go` (修改)

```go
func podSpec(sk *v1alpha1.Safekeeper, image, serviceName string) corev1.PodSpec {
    return corev1.PodSpec{
        SecurityContext: &corev1.PodSecurityContext{
            RunAsUser:  ptr.To(int64(1000)),
            RunAsGroup: ptr.To(int64(1000)),
            FSGroup:    ptr.To(int64(1000)),
        },
        // NEW: Pod Anti-Affinity
        Affinity: &corev1.Affinity{
            PodAntiAffinity: &corev1.PodAntiAffinity{
                RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
                    {
                        LabelSelector: &metav1.LabelSelector{
                            // Use LabelSelector(sk), NOT Labels(sk).
                            // LabelSelector intentionally excludes per-instance labels
                            // ("molnett.org/safekeeper") so that ALL safekeeper pods
                            // of the cluster repel each other, not just their own identity.
                            MatchLabels: LabelSelector(sk),
                        },
                        TopologyKey: "kubernetes.io/hostname",
                    },
                },
            },
        },
        // NEW: Termination Grace Period
        TerminationGracePeriodSeconds: ptr.To(int64(30)),
        Containers: []corev1.Container{
            {
                Name:    "safekeeper",
                Image:   image,
                Command: []string{"/usr/local/bin/safekeeper"},
                Args: []string{
                    fmt.Sprintf("--id=%d", sk.Spec.ID),
                    "--broker-endpoint=" + storagebroker.URL(sk.Spec.Cluster),
                    "--listen-pg=0.0.0.0:5454",
                    "--listen-http=0.0.0.0:7676",
                    fmt.Sprintf("--advertise-pg=%s:5454", serviceName),
                    "--datadir", "/data",
                    // NOTE: Safekeeper defaults to safe fsync behavior.
                    // Do NOT use --no-sync in production.
                },
                Ports: []corev1.ContainerPort{
                    {Name: "pg", ContainerPort: 5454},
                    {Name: "http", ContainerPort: 7676},
                },
                VolumeMounts: []corev1.VolumeMount{
                    {Name: "safekeeper-storage", MountPath: "/data"},
                },
                // NEW: Health Probes
                LivenessProbe: &corev1.Probe{
                    ProbeHandler: corev1.ProbeHandler{
                        HTTPGet: &corev1.HTTPGetAction{
                            Path:   "/v1/status",
                            Port:   intstr.FromInt(7676),
                            Scheme: corev1.URISchemeHTTP,
                        },
                    },
                    InitialDelaySeconds: 10,
                    PeriodSeconds:       10,
                    TimeoutSeconds:      5,
                    FailureThreshold:    3,
                },
                ReadinessProbe: &corev1.Probe{
                    ProbeHandler: corev1.ProbeHandler{
                        HTTPGet: &corev1.HTTPGetAction{
                            Path:   "/v1/status",
                            Port:   intstr.FromInt(7676),
                            Scheme: corev1.URISchemeHTTP,
                        },
                    },
                    InitialDelaySeconds: 5,
                    PeriodSeconds:       5,
                    TimeoutSeconds:      3,
                    FailureThreshold:    2,
                },
                StartupProbe: &corev1.Probe{
                    ProbeHandler: corev1.ProbeHandler{
                        HTTPGet: &corev1.HTTPGetAction{
                            Path:   "/v1/status",
                            Port:   intstr.FromInt(7676),
                            Scheme: corev1.URISchemeHTTP,
                        },
                    },
                    InitialDelaySeconds: 5,
                    PeriodSeconds:       5,
                    TimeoutSeconds:      5,
                    FailureThreshold:    12,
                },
                Resources: corev1.ResourceRequirements{
                    Requests: corev1.ResourceList{
                        corev1.ResourceCPU:    resource.MustParse("500m"),
                        corev1.ResourceMemory: resource.MustParse("512Mi"),
                    },
                    Limits: corev1.ResourceList{
                        corev1.ResourceCPU:    resource.MustParse("2"),
                        corev1.ResourceMemory: resource.MustParse("2Gi"),
                    },
                },
            },
        },
    }
}
```

#### 4.4.1 注册机制深度分析：Operator 如何将 Safekeeper 注册到 Storage Controller

这是一个关键架构问题：在非 Hadron 的 Operator 管理模式下，safekeeper 应该通过什么机制向 Storage Controller（SC）注册自己？

##### 4.4.1.1 Broker 不是注册机制

经过对上游 Neon 源码的深入分析，**storage_broker 本质上是一个 pub-sub 消息代理，不是注册机制**：

**storage_broker 架构**（`storage_broker/src/bin/storage_broker.rs`，约 840 行）：
- 无状态 gRPC pub-sub 服务，维护 `HashMap<TenantTimelineId, broadcast::Sender>`
- 4 个 gRPC 方法：`PublishSafekeeperInfo`、`SubscribeSafekeeperInfo`、`SubscribeByFilter`、`PublishOne`
- 内部只有 3 种消息：`SafekeeperTimelineInfo`、`SafekeeperDiscoveryRequest`、`SafekeeperDiscoveryResponse`

**storage_broker 的使用方**：

| 组件 | 推送 | 订阅 | 用途 |
|------|------|------|------|
| **Safekeeper** | `push_loop` — 每秒发布 `SafekeeperTimelineInfo`（含 LSN、connstr）| `pull_loop` — 订阅所有 timeline 状态；`discover_loop` — 响应 discovery 请求 | **Peer 间状态同步 + 发现** |
| **Pageserver** | — | 订阅 `SafekeeperTimelineInfo` + 发送 `SafekeeperDiscoveryRequest` | 发现 safekeeper 并确定从谁拉 WAL |
| **Storage Controller** | — | — | **完全不使用 broker** |

**关键发现**：在 `storage_controller/src/` 中搜索 `broker` 或 `storage_broker::`，结果为零。SC 对 broker **完全无感知**。

Safekeeper 通过 broker 做的所有事（push/pull/discover）都是为了 **safekeeper 间和 pageserver-to-safekeeper 的通信**——与 SC 注册完全无关。

##### 4.4.1.2 Hadron 路径分析——存在但未被调用

Safekeeper 代码中有完整的 Hadron（HCC）注册实现（`safekeeper/src/hadron.rs` L29-150），流程为：
```
safekeeper 启动 → build_node_registeration_request()
                → POST {hcc_base_url}/hadron-internal/v1/sk
                → 无限重试 backoff
```

但 **`hadron::register()` 从未在 safekeeper 的启动流程中被调用**。`safekeeper/src/bin/safekeeper.rs` 中 `hcc_base_url` 参数仅在 `enable_pull_timeline_on_startup` 时用于 `hcc_pull_timelines()`——用于从 HCC 拉取应有 timeline 列表，而非注册。

**结论：Hadron 路径的注册函数代码虽完整，但未被集成到 safekeeper 主流程。**

##### 4.4.1.3 SC 的 Upsert API —— 唯一的注册入口

SC 提供 `POST /control/v1/safekeeper/:id` upsert API（`http.rs` L1708-1747，`safekeeper_service.rs` L788-848），这是 **SC 中唯一的 safekeeper 注册/更新入口**：

**SafekeeperUpsert 请求体**（`persistence.rs` L2519-2533）：
```rust
pub(crate) struct SafekeeperUpsert {
    pub(crate) id: i64,
    pub(crate) region_id: String,
    pub(crate) version: i64,
    pub(crate) host: String,        // WAL 服务地址
    pub(crate) port: i32,            // WAL 端口
    pub(crate) http_port: i32,       // HTTP 管理端口
    pub(crate) https_port: Option<i32>,
    pub(crate) availability_zone_id: String,
}
```

**upsert 调用时发生了什么**（`safekeeper_service.rs` L788-848）：

| 步骤 | 操作 | 副作用 |
|------|------|--------|
| 1 | 写入 DB（`INSERT ... ON CONFLICT DO UPDATE`） | 持久化 host/port/http_port 等 |
| 2 | 更新内存 HashMap | 新 safekeeper 以 `SkSchedulingPolicy::Activating` 创建 |
| 3 | `start_reconciler(node_id)` | 启动 per-safekeeper reconciler 后台任务 |
| 4 | 更新 prometheus metrics | — |

**关键行为**：
- upsert **不验证** safekeeper 的网络连通性——只是写入 DB + 内存
- 新 upsert 的 safekeeper 初始 `scheduling_policy = Activating`
- **Activating→Active 的自动转换**由心跳循环完成（`service.rs` L1423-1455）：当心跳检测到 `Activating` 的 safekeeper 连通正常，自动在 DB 和内存中升级为 `Active`
- upsert 会**启动 per-safekeeper reconciler 后台任务**（通过 `start_reconciler`）——但心跳是统一的，每轮从 `locked.safekeepers.clone()` 获取所有节点并批量轮询。注意：heartbeater 只跳过 `Decomissioned` 节点，不跳过 `Pause` 节点

##### 4.4.1.4 JWT 认证要求

**关键发现**: SC 的两个关键 API 都有 JWT 认证要求：

| API | 认证 Scope | 说明 |
|-----|-----------|------|
| `POST /control/v1/safekeeper/:id` (upsert) | `Scope::Infra` | 需要 `infra` scope |
| `POST /control/v1/safekeeper/:id/scheduling_policy` | `Scope::Admin` | 需要 `admin` scope |

Operator 通过以下方式满足认证要求：
1. Cluster Controller 在创建时生成 Ed25519 密钥对（`cluster-{name}-jwt` Secret）
2. SC Client 每次调用时用私钥签发 JWT token（5 分钟有效期），包含 `scope: "infra admin"`
3. 所有 SC HTTP 调用携带 `Authorization: Bearer {token}` 头

> **安全说明**：集群范围内 `Scope::Admin` 意味着 Operator 可以操作整个 SC。Operator 以最小权限签发短期 token（5 分钟），勿使用长期 token。

##### 4.4.1.5 架构决策：Operator 调用 Upsert API

| 方式 | 可行性 | 分析 |
|------|:------:|------|
| **Safekeeper 自注册 via broker** | ❌ | SC 根本不使用 broker，无法获知 safekeeper 存在 |
| **Safekeeper 自注册 via SC API** | ⚠️ | Safekeeper 代码中没有此逻辑；需要在 safekeeper 二进制中增加 SC HTTP 连接和注册代码（侵入性强） |
| **Hadron HCC 注册** | ❌ | 未集成到主流程；引入额外 HCC 组件，增加复杂度 |
| **Operator 调用 SC Upsert API** | ✅ ✅ | Operator 作为"部署脚本"角色；K8s 原生感知 Pod 状态；无需修改 safekeeper 或 SC 代码 |

**选择 Operator 调用 Upsert API 的理由**：

1. **上游标准模式**：在标准 Neon 架构中，正是由"外部部署脚本/控制平面"调用 SC 的 upsert API 来注册 safekeeper。Operator 就是这个"部署脚本"的 K8s 原生替代。
2. **信息完备**：Operator 在创建 StatefulSet 时已知道所有注册所需信息（id、host/域名、port、http_port）。
3. **生命周期对齐**：Operator 的 reconcile 循环天然可以处理注册→监控→退役的完整生命周期。
4. **零侵入**：不需要修改 safekeeper 或 SC 代码。Operator 使用标准 HTTP 客户端调用已有 API。
5. **地址稳定性**：使用 StatefulSet headless Service 的 DNS 名称（如 `sk-1.safekeeper.ns.svc.cluster.local`），Pod 重调度到新节点时 DNS 不变 → host 不变 → 无需重新注册。
6. **SC 高可用转发**：SC 的 `maybe_forward` 机制使非 Leader 节点返回重定向。Operator 的 HTTP 客户端必须跟随重定向（最多 3 跳），否则在 SC HA 部署下可能请求失败。

##### 4.4.1.6 节点故障重调度时的注册行为

这是关键场景：当 safekeeper Pod 因节点故障被调度到新节点时，是否需要重新注册？

```
场景：Node A 故障 → Pod 漂移到 Node B
─────────────────────────────────────────
StatefulSet 名称：       sk-1  (不变)
Service DNS：            sk-1.safekeeper.ns.svc.cluster.local  (不变)
safekeeper --id：        1  (不变)
WAL 端口：               5454  (不变)
HTTP 端口：              7676  (不变)
─────────────────────────────────────────
SC DB 中的记录：         host=sk-1..., port=5454, http_port=7676
新 Pod 地址：            host=sk-1..., port=5454, http_port=7676
─────────────────────────────────────────
结论：host 完全一致 → 不需要重新调用 upsert！
```

SC heartbeater 的流程：
1. Pod 在新节点恢复运行 → safekeeper 进程启动 → 连接 broker → 开始 push/pull
2. SC 心跳轮询 `host:http_port`（DNS 不变）→ 检测到 Available
3. SC 自动执行 Activating→Active 转换（如果之前是 Activating）或恢复为 Available
4. Timeline 恢复由 SC 的 safekeeper_reconciler 调度 Pull 操作处理

**唯一需要重新 upsert 的情况**：如果使用 Pod IP 而非 Service DNS 作为 host（不推荐，强烈建议使用 headless Service DNS）。

##### 4.4.1.7 完整注册流程

```
┌──────────────────────────────────────────────────────────────────────┐
│                                                                      │
│  Operator                              Storage Controller            │
│  ────────                              ─────────────────             │
│                                                                      │
│  ① 创建 Safekeeper CR                                              │
│     ├─ .spec.id = N                                                 │
│     └─ .spec.cluster = "mycluster"                                  │
│                                                                      │
│  ② Reconcile: 创建 StatefulSet                                      │
│     ├─ --id=N                                                       │
│     ├─ --broker-endpoint=storage-broker:50051                       │
│     ├─ --listen-pg=0.0.0.0:5454                                     │
│     ├─ --listen-http=0.0.0.0:7676                                   │
│     ├─ --advertise-pg=sk-N.svc.ns.svc.cluster.local:5454            │
│     └─ headless service: sk-N.svc.ns.svc.cluster.local              │
│                                                                      │
│  ③ 等待 Pod Ready (ReadinessProbe /v1/status)                       │
│                                                                      │
│  ④ 调用 SC Upsert API             ──→  POST /control/v1/safekeeper/N│
│     {                                                                │
│       "id": N,                                                       │
│       "host": "sk-N.svc.ns.svc.cluster.local",                      │
│       "port": 5454,                                                  │
│       "http_port": 7676,                                             │
│       "availability_zone_id": "zone-a"  ← 从 Node topology 读取     │
│     }                                                                │
│                                                                      │
│  ⑤ 更新 Safekeeper Status          ←── SC 返回 200 OK               │
│     status.registeredWithSC = true                                   │
│                                                                      │
│  ⑥                                Heartbeater 检测到 Available      │
│                                    scheduling_policy: Activating     │
│                                    → 自动升级为 Active               │
│                                    → start_reconciler                │
│                                                                      │
│  ⑦ Safekeeper 正常工作                                              │
│     broker push/pull: 发现 peer，同步 LSN                            │
│     broker discover: pageserver 发现 safekeeper                      │
│     SC heartbeater:   持续监控可用性                                  │
│     SC reconciler:    处理 Pull/Exclude/Delete 操作                   │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

##### 4.4.1.8 退役(Decommission)流程

删除 Safekeeper 时，Operator 通过 `POST /control/v1/safekeeper/:id/scheduling_policy` 设置 `Decomissioned`：

```
Operator 删除流程：
① 设置 scheduling_policy = Decomissioned
   ├─ SC 停止 reconciler（不调度新 Pull/Exclude/Delete）
   ├─ SC 不再分配新 timeline 到该 safekeeper
   └─ SC heartbeater 跳过 Decomissioned 节点
② SC reconciler 对已有 timeline 执行迁移/删除
③ 等待所有 timeline 迁移完成
④ 删除 Safekeeper CR → 级联删除 StatefulSet/PVC/Service
```

> **注意**：SC 没有 safekeeper 的 DELETE HTTP 端点。Safekeeper 的退役（不再参与集群）是通过 `scheduling_policy=Decomissioned` 实现的。DB 中的记录可以保留（重建时 re-upsert 即可复用），也可以通过直接操作 SC 数据库清理。

##### 4.4.1.9 注册 vs 不注册的对比

| 场景 | 不注册 SC | 注册 SC 后 |
|------|----------|-----------|
| **SC 知道 safekeeper** | ❌ SC 完全不知道该 safekeeper 存在 | ✅ SC DB + 内存中都有记录 |
| **Heartbeater 监控** | ❌ 不会轮询该 safekeeper | ✅ 定期轮询，追踪 Available/Offline |
| **Timeline 分配** | ❌ SC 不会分配 timeline 到该 safekeeper | ✅ SC 分配 timeline（通过 `sk_set`） |
| **Pull 操作调度** | ❌ SC 无法调度 Pull | ✅ SC 通过 reconciler 调度 Pull |
| **故障自动恢复** | ❌ SC 无法检测和响应 | ✅ SC 检测故障并触发恢复 |
| **Scheduling Policy** | ❌ 无法控制 | ✅ 可精细控制 Active/Pause/Decomissioned |
| **Safekeeper 间发现** | ✅ broker peer 发现（不受影响） | ✅ broker（不受影响） |
| **Pageserver→Safekeeper** | ✅ broker discovery（不受影响） | ✅ broker（不受影响） |

> **关键理解**：broker 处理 safekeeper↔safekeeper 和 pageserver↔safekeeper 的发现与同步；SC 处理集群级管理（scheduling、migration、failure recovery）。两者正交互补，互不依赖。

#### 4.4.2 Operator 实现：注册与退役

##### 4.4.2.1 注册函数

**文件**: `internal/controller/safekeeper_controller.go` (修改)

在 reconcile 循环中，创建资源之后、更新状态之前，调用 `registerWithSC()`：

```go
func (r *SafekeeperReconciler) reconcileSK(
    ctx context.Context,
    sk *neonv1alpha1.Safekeeper,
) error {
    // ... existing: create Service, StatefulSet, PDB ...

    // Wait for Pod to be ready before registering with SC
    if ready, err := r.isPodReady(ctx, sk); err != nil {
        return err
    } else if !ready {
        log.FromContext(ctx).Info("safekeeper pod not ready yet, requeue")
        return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
    }

    // Register/Update safekeeper with Storage Controller
    if err := r.registerWithSC(ctx, sk); err != nil {
        return fmt.Errorf("register with SC: %w", err)
    }

    // ... existing: update status ...
    return nil
}

// registerWithSC calls the Storage Controller's upsert API to register
// or update the safekeeper's connection information.
//
// This is safe to call idempotently – SC's upsert uses INSERT ... ON CONFLICT
// DO UPDATE. If the safekeeper is already registered with the same host/port,
// the call is effectively a no-op. If host/port changed (e.g., service DNS
// reconfiguration), SC will update its records atomically.
//
// The headless Service DNS name is used as the host, which remains stable
// across pod rescheduling to different nodes.
func (r *SafekeeperReconciler) registerWithSC(
    ctx context.Context,
    sk *neonv1alpha1.Safekeeper,
) error {
    // Build the headless service DNS name for this safekeeper
    // StatefulSet pod FQDN: {pod-name}.{headless-service}.{namespace}.svc.cluster.local
    // With StatefulSet named "safekeeper" and pod "safekeeper-0":
    //   → safekeeper-0.safekeeper.{ns}.svc.cluster.local
    host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
        sk.Spec.Cluster, sk.Spec.ID,    // pod name
        safekeeper.ServiceName(sk.Spec.Cluster), // headless service name
        sk.Namespace,
    )

    // Read availability zone from the node the pod is scheduled on
    az := r.getNodeAvailabilityZone(ctx, sk)

    req := SCUpsertRequest{
        ID:                 int64(sk.Spec.ID),
        RegionID:           "default",
        Version:            1,
        Host:               host,
        Port:               5454, // WAL (pg) port
        HTTPPort:           7676, // HTTP management port
        AvailabilityZoneID: az,
    }

    url := fmt.Sprintf("%s/control/v1/safekeeper/%d",
        r.StorageControllerURL, sk.Spec.ID)

    jsonBody, err := json.Marshal(req)
    if err != nil {
        return fmt.Errorf("marshal upsert request: %w", err)
    }

    httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
        bytes.NewReader(jsonBody))
    if err != nil {
        return fmt.Errorf("create request: %w", err)
    }
    httpReq.Header.Set("Content-Type", "application/json")

    resp, err := http.DefaultClient.Do(httpReq)
    if err != nil {
        return fmt.Errorf("call SC upsert: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode >= 300 {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("SC upsert failed (%d): %s", resp.StatusCode, string(body))
    }

    log.FromContext(ctx).Info("safekeeper registered with Storage Controller",
        "host", host, "id", sk.Spec.ID, "az", az)
    return nil
}

// getNodeAvailabilityZone reads the node topology label to determine
// the availability zone for anti-affinity-aware scheduling.
func (r *SafekeeperReconciler) getNodeAvailabilityZone(
    ctx context.Context,
    sk *neonv1alpha1.Safekeeper,
) string {
    pod := &corev1.Pod{}
    if err := r.Get(ctx, client.ObjectKey{
        Name:      safekeeper.PodName(sk.Spec.Cluster, sk.Spec.ID),
        Namespace: sk.Namespace,
    }, pod); err != nil {
        return "unknown"
    }
    if pod.Spec.NodeName == "" {
        return "unknown"
    }
    node := &corev1.Node{}
    if err := r.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
        return "unknown"
    }
    // K8s standard topology label
    if az, ok := node.Labels["topology.kubernetes.io/zone"]; ok {
        return az
    }
    return "unknown"
}
```

##### 4.4.2.2 退役（Decommission）实现修正

**文件**: `internal/controller/cluster_controller.go` (修改)

当前设计文档中使用 `"Offline"` 作为 scheduling_policy，但 SC 的 `SkSchedulingPolicy` 枚举实际值为 `Active`、`Activating`、`Pause`、`Decomissioned`。必须修正为 `"Decomissioned"`：

```go
// decommissionSafekeeperFromStorageController sets the safekeeper's
// scheduling policy to Decomissioned in the Storage Controller.
//
// SC handles:
//   1. Stop the per-safekeeper reconciler
//   2. Stop assigning new timelines to this safekeeper
//   3. Heartbeater skips Decomissioned nodes
func (r *ClusterReconciler) decommissionSafekeeperFromStorageController(
    ctx context.Context,
    sk *neonv1alpha1.Safekeeper,
) error {
    url := fmt.Sprintf("%s/control/v1/safekeeper/%d/scheduling_policy",
        r.StorageControllerBaseURL, sk.Spec.ID)

    // CORRECT: Use "Decomissioned" (SC's spelling)
    // "Offline" is NOT a valid SkSchedulingPolicy value.
    // Valid: Active, Activating, Pause, Decomissioned
    body := map[string]string{"scheduling_policy": "Decomissioned"}
    jsonBody, _ := json.Marshal(body)

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
        bytes.NewReader(jsonBody))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return fmt.Errorf("set SC scheduling policy: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode >= 300 {
        return fmt.Errorf("SC returned %d", resp.StatusCode)
    }
    return nil
}
```

##### 4.4.2.3 StatefulSet 参数补充

在 `specs/safekeeper/statefulset.go` 的 `args()` 中调整参数：

```go
func args(sk *neonv1alpha1.Safekeeper, serviceName string) []string {
    return []string{
        fmt.Sprintf("--id=%d", sk.Spec.ID),
        "--broker-endpoint=" + storagebroker.URL(sk.Spec.Cluster),
        "--listen-pg=0.0.0.0:5454",
        "--listen-http=0.0.0.0:7676",
        // headless service DNS for advertise-pg
        // Remains stable even when pod is rescheduled to a new node
        fmt.Sprintf("--advertise-pg=%s-%d.%s.%s.svc.cluster.local:5454",
            sk.Spec.Cluster, sk.Spec.ID,
            serviceName, sk.Namespace),
        "--datadir", "/data",
        // NOTE: Safekeeper defaults to safe fsync behavior.
        // Do NOT use --no-sync in production.
        // NOT needed:
        //   --storage-controller-url  (operator handles SC registration)
        //   --hcc-base-url            (Hadron only)
        //   --enable-pull-timeline-on-startup  (Handled by SC reconciler)
    }
}
```

##### 4.4.2.4 Safekeeper CRD Status 扩展

```go
type SafekeeperStatus struct {
    // ... existing fields ...

    // RegisteredWithSC indicates whether the safekeeper has been
    // successfully registered with the Storage Controller via the upsert API.
    // +optional
    RegisteredWithSC bool `json:"registeredWithSC,omitempty"`

    // SchedulingPolicy reflects the safekeeper's current scheduling policy
    // in the Storage Controller (Active/Activating/Pause/Decomissioned).
    // +optional
    SchedulingPolicy string `json:"schedulingPolicy,omitempty"`
}
```

### 4.5 存储方案：本地盘 + pull_timeline 自动恢复

#### 4.5.1 为什么用本地盘

Neon 生产环境（neon.tech）使用本地 NVMe SSD 作为 safekeeper 存储，非云盘。
safekeeper 的 WAL 数据天然有多副本（≥3 个 safekeeper 形成 Paxos quorum），
单个节点故障时数据可从 peer safekeeper 完整重建。

本地盘的优势：
- **性能**：NVMe 本地盘延迟远低于网络云盘，对 WAL 写入延迟敏感
- **成本**：无需支付云盘额外费用
- **架构匹配**：Neon 的多副本共识协议天然容忍单节点数据丢失

#### 4.5.2 故障恢复核心机制：pull_timeline

当 safekeeper 节点故障、Pod 调度到新节点后，本地盘为空。Safekeeper 内置了
`POST /v1/pull_timeline` API，可从 peer safekeeper 完整拉取 timeline 数据。

**上游 neon 源码分析**（`safekeeper/src/pull_timeline.rs`）：

```
pull_timeline 恢复流程：
┌──────────────────────────────────────────────────────────┐
│ 1. 查询所有 peer safekeeper 的 timeline_status            │
│    GET /v1/tenant/{tid}/timeline/{tlid}                   │
│    收集每个 peer 的 (epoch, flush_lsn, term, commit_lsn)   │
│                                                          │
│ 2. 选择最先进的 peer（按 epoch→flush_lsn→term 排序）       │
│    └→ 允许最多 1 个 peer 不可达（容忍故障）                │
│                                                          │
│ 3. 从最佳 peer 下载 tar snapshot                          │
│    GET /v1/tenant/{tid}/timeline/{tlid}/snapshot          │
│    包含：safekeeper.control + WAL segments               │
│                                                          │
│ 4. 解包 + 逐文件 fsync + validate                         │
│    └→ 验证 commit_lsn / flush_lsn 一致性                  │
│                                                          │
│ 5. 加载 timeline 到全局 map                               │
│    └→ 可选：执行 membership_switch（如果提供了 mconf）     │
└──────────────────────────────────────────────────────────┘
```

**关键特性**：
- **全量数据**：拉取的是完整 timeline 状态（control file + WAL segments），不是增量
- **自动选主**：自动选择最先进的 peer 作为源
- **容错**：允许 1 个 peer 不可达
- **原子性**：通过临时目录 + fsync 保证数据完整性

#### 4.5.3 谁负责触发恢复？—— 架构决策分析

这是一个关键架构问题：节点变更的检测和 `pull_timeline` 的触发应该由谁负责——Operator 还是 Storage Controller？

##### 4.5.3.1 Storage Controller 已有的能力（上游 neon 源码分析）

Storage Controller（SC）是 Neon 集群的"控制平面"，负责管理所有节点和 timeline 的生命周期。
通过分析 `storage_controller/src/` 源码，SC 已经拥有以下完整基础设施：

**a. Safekeeper 注册与持久化**（`service/safekeeper_service.rs` L788-848、`persistence.rs` L1340-1353）：

```
POST /control/v1/safekeeper/:id → upsert_safekeeper()
├── 写入 DB（host, port, http_port, scheduling_policy, ...）
├── 更新内存 HashMap<NodeId, Safekeeper>
├── 新 safekeeper 初始 scheduling_policy = Activating
└── 启动 per-safekeeper reconciler
```

Safekeeper 注册时提供完整信息（`SafekeeperUpsert`）：
```
id, region_id, version, host, port, http_port, https_port, availability_zone_id
```

当 safekeeper 在**新节点**重启并重新注册时，SC 的 upsert 操作会更新 host/port。

**b. Heartbeater 可用性检测**（`heartbeater.rs` L320-456、`service.rs` L1287-1456）：

SC 每 5 秒（`HEARTBEAT_INTERVAL_DEFAULT`）并行轮询所有 safekeeper 的 `/v1/status`（实际调用 `get_utilization()`），
维护每个 safekeeper 的状态：
```
SafekeeperState::Available { last_seen_at, utilization }  // 健康
SafekeeperState::Offline                                  // 不可达
```

心跳循环中（`service.rs` L1410-1456），SC 自动处理状态变化：
- 检测 `Activating → Available` 转换 → 自动将调度策略升级为 `Active`
- 追踪 `Available → Offline` 转换 → 标记节点不可用

**c. Safekeeper Reconciler — 已经支持 Pull 操作**（`safekeeper_reconciler.rs` L339-390）：

```rust
SafekeeperTimelineOpKind::Pull => {
    let http_hosts = req.host_list.iter()
        .filter(|(node_id, _)| *node_id != our_id)
        .map(|(_, hostname)| hostname.clone())
        .collect();
    let pull_req = PullTimelineRequest { http_hosts, tenant_id, timeline_id, mconf };
    client.pull_timeline(&pull_req).await  // 调用 safekeeper 的 /v1/pull_timeline
}
```

SC 的 reconciler 已经在 `safekeeper_migrate`（RFC-035 流程的 Step 4）中使用 Pull 操作——基础设施完全就绪。

**d. Timeline 成员关系**（`persistence.rs` L2604-2614）：

SC 的数据库 `timelines` 表中存储 `sk_set`（Vec of safekeeper NodeId），精确记录每个 timeline 属于哪些 safekeeper：

```sql
-- SC 数据库保留每个 timeline 的完整成员信息
timelines: { tenant_id, timeline_id, generation, sk_set: [sk1, sk2, sk3], ... }
```

##### 4.5.3.2 对比分析：Operator vs Storage Controller

| 维度 | Operator 触发 | Storage Controller 触发 |
|------|-------------|----------------------|
| **检测节点变更** | ✅ 天然能力（Pod.spec.nodeName） | ❌ 无 k8s 感知 |
| **检测 safekeeper 数据为空** | ❌ 需查询 safekeeper HTTP API | ✅ 心跳已轮询 `/v1/status` |
| **知道哪些 timeline 需要恢复** | ❌ 需查询 peer safekeeper + 交叉比对 | ✅ DB 中 `sk_set` 直接给出 |
| **知道 peer HTTP 地址** | ❌ 需从 K8s Service 推导或查询 SC | ✅ DB 中有所有 safekeeper 的 host/port |
| **Pull 操作基础设施** | ❌ 需从零构建 HTTP 客户端 | ✅ 已有 `SafekeeperTimelineOpKind::Pull` |
| **原子性与重试** | ❌ 需自行实现 | ✅ 已有 pending_ops + reconciler 重试 |
| **安全认证（JWT）** | ❌ 需 Operator 持有 neon JWT | ✅ SC 天然持有 |
| **跨层边界** | ❌ K8s 编排器做应用层数据恢复 | ✅ 控制平面做自己的事 |

**结论：Storage Controller 应该在 Operator 的配合下负责检测和触发恢复。**

原因：
1. SC 是 Neon 架构中的"单一真相源"——它知道所有 safekeeper、所有 timeline、它们的成员关系
2. SC 已经有完整的 Pull 操作基础设施（reconciler + pending_ops + 重试）
3. Operator 如果做这件事，需要大量重复造轮子：HTTP 客户端、JWT 认证、peer 发现、重试逻辑
4. Operator 跨层做应用级数据恢复违反关注点分离原则

##### 4.5.3.3 正确的恢复流程

```
节点故障恢复全流程（SC 负责恢复）：
┌───────────────────────────────────────────────────────────────────────┐
│                                                                       │
│  K8s / Operator                   Storage Controller                  │
│  ─────────────                    ─────────────────                   │
│                                                                       │
│  ① Node A 宕机                                                        │
│     K8s 检测 Pod 不可用                                                │
│     └→ 调度到 Node B                                                   │
│                                                                       │
│  ② Safekeeper Pod 在 Node B 启动                                      │
│     本地盘为空                                                        │
│     Operator 确保 StatefulSet 配置了:                                  │
│     - --id=X（稳定不变）                                               │
│     - --broker-addr=...（连接 broker）                                 │
│     - SC URL + JWT（认证）                                            │
│                                                                       │
│  ③ Safekeeper 进程启动                     ← 收到注册请求              │
│     通过 broker 或直接向 SC 注册               upsert_safekeeper      │
│     └→ SC 更新 host/port（如 IP 变了）       └→ host/port 持久化到 DB │
│                                                                       │
│  ④                                         Heartbeater 轮询           │
│                                            GET /v1/status             │
│                                            └→ Available！             │
│                                                                       │
│  ⑤                                         检测到需要恢复：           │
│                                            - safekeeper Available     │
│                                            - 但 /v1/timeline 为空     │
│                                            - DB 中 sk_set 含有此 SK   │
│                                                                       │
│  ⑥                                         查询 timelines 表：        │
│                                            SELECT * FROM timelines    │
│                                            WHERE sk_set @> [sk_id]    │
│                                            → [tl1, tl2, tl3, ...]     │
│                                                                       │
│  ⑦                                         对每个缺失的 timeline：    │
│                                            schedule_request(Pull)     │
│                                            ├─ tenant_id: T            │
│                                            ├─ timeline_id: TL         │
│                                            ├─ host_list: [peer1, ...] │
│                                            └─ safekeeper: sk          │
│                                                                       │
│  ⑧                                         Safekeeper Reconciler     │
│                                            调用 pull_timeline API     │
│                                            POST /v1/pull_timeline     │
│                                            {tenant_id, timeline_id,    │
│                                             http_hosts: [peers...]}    │
│                                                                       │
│  ⑨  Safekeeper 从 peer 下载                ← 监控恢复进度             │
│      tar snapshot + 加载                      pending_ops 状态跟踪     │
│      恢复完成，正常服务                                                 │
│                                                                       │
└───────────────────────────────────────────────────────────────────────┘
```

##### 4.5.3.4 职责划分

| 组件 | 职责 |
|------|------|
| **Operator** | 1) 确保 safekeeper StatefulSet 正确配置了 SC URL、broker 地址、JWT token<br>2) 确保 `--id` 参数稳定（StatefulSet 序列号）<br>3) PodAntiAffinity 保证副本分散<br>4) PDB 防止多副本同时下线<br>5) **不参与** pull_timeline 调用 |
| **Storage Controller** | 1) 接收 safekeeper 注册并维护 host/port<br>2) Heartbeater 检测可用性变化<br>3) 检测"safekeeper 上线但无 timeline"场景<br>4) 调度 Pull 操作（通过已有的 reconciler 基础设施）<br>5) 跟踪恢复进度（pending_ops） |
| **Safekeeper** | 1) 启动时向 SC 注册<br>2) 响应 heartbeater 的 `/v1/status` 查询<br>3) 执行 `/v1/pull_timeline`（从 peer 拉取数据） |

#### 4.5.4 上游 SC 需要增加的能力

当前 SC 缺少的唯一能力：**自动检测"safekeeper 重新上线但数据为空"并触发 Pull**。

SC 已有所有基础组件，只需增加少量逻辑：

```rust
// 在 service.rs 的 heartbeat 循环中 (L1410-1456)，增加恢复检测：

// 现有逻辑：更新 safekeeper 可用性
for (id, state) in deltas.0 {
    let Some(sk) = safekeepers.get_mut(&id) else { continue; };
    sk.set_availability(state);
}

// 新增：检测 safekeeper 从 Offline 恢复后是否需要 Pull
// (在 heartbeat 循环之后)

// 伪代码：
for (sk_id, state) in deltas.0 {
    match state {
        SafekeeperState::Available { .. } => {
            // 检查该 safekeeper 之前是否离线
            // 检查其 /v1/timeline 是否为空
            // 如果为空，从 timelines 表查 sk_set 含有此 sk_id 的所有 timeline
            // 对每个 timeline，通过 safekeeper_reconcilers.schedule_request(Pull)
        }
        _ => {}
    }
}
```

**不需要新增 SC API 端点**——所有操作都通过已有的内部机制完成。

#### 4.5.5 Operator 的实现（仅做配置传递 + 状态观察）

**Safekeeper CRD Status 扩展**（可选，用于观察恢复进度）：

```go
type SafekeeperStatus struct {
    // ... existing fields ...

    // LastObservedNode records the node name from the most recent status check.
    // This is purely observational; recovery is handled by Storage Controller.
    // +optional
    LastObservedNode string `json:"lastObservedNode,omitempty"`
}
```

**Operator 不需要**：
- 查询 peer safekeeper 的 timeline 列表
- 调用 `POST /v1/pull_timeline`
- 判断哪些 timeline 需要恢复
- 持有 neon JWT token（除非用于初始注册）

**Operator 需要确保**：
- StatefulSet 的 safekeeper 启动参数正确配置了 SC 和 broker 地址
- safekeeper 进程可以通过网络访问 SC 和 peer safekeeper

#### 4.5.6 存储要求总结

| 环境 | 存储方案 | 恢复机制 |
|------|---------|---------|
| **生产** | 本地 NVMe SSD（local-pv） | SC 自动检测 + Pull 调度 |
| **开发/测试** | 本地盘（local-path 等） | 同上，或手动重建 |

> **关键前提**：`pull_timeline` 恢复依赖至少存在一个健康的 peer safekeeper。
> 如果同时故障 ≥2 个 safekeeper，Quorum 本身已丢失，恢复也无法进行。
> 因此需要 PodAntiAffinity 保证副本分布在 ≥3 个不同节点。

### 4.6 PodDisruptionBudget

**文件**: `specs/safekeeper/pdb.go` (新增)

```go
package safekeeper

import (
    policyv1 "k8s.io/api/policy/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/util/intstr"
    neonv1alpha1 "github.com/molnett/neon-operator/api/v1alpha1"
)

// PodDisruptionBudget creates a PDB for the safekeeper.
// Ensures at most 1 safekeeper is disrupted at a time,
// maintaining quorum during voluntary disruptions.
func PodDisruptionBudget(sk *neonv1alpha1.Safekeeper) *policyv1.PodDisruptionBudget {
    return &policyv1.PodDisruptionBudget{
        ObjectMeta: metav1.ObjectMeta{
            Name:      Name(sk),
            Namespace: sk.Namespace,
            Labels:    Labels(sk),
        },
        Spec: policyv1.PodDisruptionBudgetSpec{
            MaxUnavailable: ptr.To(intstr.FromInt(1)),
            Selector: &metav1.LabelSelector{
                MatchLabels: LabelSelector(sk),
            },
        },
    }
}
```

Safekeeper Controller 需要新增 `Owns(&policyv1.PodDisruptionBudget{})` 并在 `createSafekeeperResources` 中创建 PDB。

### 4.7 Safekeeper Labels 完善

**文件**: `specs/safekeeper/labels.go` (修改)

```go
const ClusterLabel = "molnett.org/cluster"

func Labels(sk *neonv1alpha1.Safekeeper) map[string]string {
    return map[string]string{
        "app.kubernetes.io/name":      "safekeeper",
        "app.kubernetes.io/component": "safekeeper",
        "app.kubernetes.io/part-of":   "neon",
        ClusterLabel:                   sk.Spec.Cluster,
        "molnett.org/safekeeper":       sk.Name,
    }
}

// LabelSelector returns labels for Service/PDB selectors.
// DO NOT include "molnett.org/safekeeper" in selector
// as it would break when the name changes.
func LabelSelector(sk *neonv1alpha1.Safekeeper) map[string]string {
    return map[string]string{
        "app.kubernetes.io/component": "safekeeper",
        ClusterLabel:                   sk.Spec.Cluster,
    }
}
```

### 4.8 Cluster API 类型改动

**文件**: `api/v1alpha1/cluster_types.go`

```go
type ClusterSpec struct {
    // 决定集群中运行多少个 safekeeper。必须是 >= 3 的奇数以确保 quorum。
    // +kubebuilder:default:=3
    // +kubebuilder:validation:Minimum:=3
    // +kubebuilder:validation:Maximum:=7
    NumSafekeepers uint8 `json:"numSafekeepers"`

    // NEW: Default storage config for auto-created safekeepers.
    // If not set, defaults to Size=10Gi with no storage class.
    // +optional
    DefaultSafekeeperStorage *neonv1alpha1.StorageConfig `json:"defaultSafekeeperStorage,omitempty"`
    ...
}
```

### 4.9 完整改动清单

| # | 文件 | 改动类型 | 内容 |
|---|------|:--:|------|
| 1 | `api/v1alpha1/cluster_types.go` | 修改 | 新增 `DefaultSafekeeperStorage` 字段 |
| 2 | `internal/controller/cluster_controller.go` | 修改 | reconcile 流程增加 `reconcileSafekeepers()`；status 增加聚合 |
| 3 | `internal/controller/cluster_create.go` | 新增 | `reconcileSafekeepers()`, `createSafekeeper()`, `deleteSafekeeper()` |
| 4 | `internal/controller/safekeeper_controller.go` | 修改 | 新增 `registerWithSC()` 调用 SC upsert API；finalize 实现 Decomissioned 安全注销 |
| 5 | `internal/controller/safekeeper_create.go` | 修改 | 创建 PDB 逻辑 |
| 6 | `internal/controller/sc_client.go` | 新增 | SC HTTP 客户端封装（upsert、setSchedulingPolicy） |
| 7 | `specs/safekeeper/statefulset.go` | 修改 | 添加 Probes + AntiAffinity + 注册所需参数（safekeeper 默认已启用 fsync，无需额外配置） |
| 8 | `specs/safekeeper/pdb.go` | 新增 | PodDisruptionBudget 工厂函数 |
| 9 | `specs/safekeeper/labels.go` | 修改 | 完善标签体系 |
| 10 | `specs/safekeeper/testdata/statefulset.yaml` | 更新 | Golden file 同步 |
| 11 | `specs/safekeeper/testdata/service.yaml` | 更新 | Golden file 同步 |
| 12 | `specs/safekeeper/testdata/headless_service.yaml` | 更新 | Golden file 同步 |
| 13 | `specs/safekeeper/testdata/pdb.yaml` | 新增 | Golden 测试 |
| 14 | `internal/controller/cluster_controller_test.go` | 修改 | 增加 Safekeeper 自动化测试 |
| 15 | `internal/controller/safekeeper_controller_test.go` | 修改 | 增加 Probe/PDB/注册/注销测试 |
| 16 | `api/v1alpha1/safekeeper_types.go` | 修改 | Status 新增 `RegisteredWithSC`、`SchedulingPolicy` |
| 17 | `config/crd/bases/neon.oltp.molnett.org_clusters.yaml` | 更新 | CRD 重新生成 |

---

## 5. 实施计划

### Phase 2a: 基础健康与高可用（Week 1）

```
目标：Safekeeper Pod 具备自愈能力和反亲和性

1. 修改 specs/safekeeper/statefulset.go
   - 添加 LivenessProbe, ReadinessProbe, StartupProbe
   - 添加 PodAntiAffinity (RequiredDuringScheduling)
   - 添加 Resource Requests/Limits
   - 添加 TerminationGracePeriodSeconds
   - 调整 --advertise-pg 为 headless service DNS
   - 注：safekeeper 默认已启用 fsync，无需显式设置同步参数
2. 修改 specs/safekeeper/labels.go
   - 完善 Label 体系，使 AntiAffinity label 选择器正确
3. 新增 internal/controller/sc_client.go
   - SC HTTP 客户端封装（upsert、setSchedulingPolicy）
4. 修改 internal/controller/safekeeper_controller.go
   - 新增 registerWithSC() 调用 SC upsert API
   - 等待 Pod Ready 后再注册
5. 更新 Golden Test 文件
6. 更新 Safekeeper Controller 测试
7. 验证：3 个 Pod 强制分布在不同节点
```

### Phase 2b: 自动创建与状态聚合（Week 1-2）

```
目标：Cluster Controller 自动创建 Safekeeper CR

1. 修改 api/v1alpha1/cluster_types.go
   - 新增 DefaultSafekeeperStorage 字段
   - 重新生成 CRD
2. 修改 internal/controller/cluster_controller.go
   - reconcile 中调用 reconcileSafekeepers()
   - updateStatus 聚合 Safekeeper 状态
3. 新增 cluster_create.go 中函数
   - reconcileSafekeepers()
   - createSafekeeper()
   - deleteSafekeeper()
4. 编写 Cluster Controller 测试
5. 编写 E2E 测试
```

### Phase 2c: PDB 与安全注销（Week 2）

```
目标：维护安全和删除安全

1. 新增 specs/safekeeper/pdb.go
2. 修改 safekeeper_create.go 创建 PDB
3. 修改 safekeeper_controller.go
   - finalize 实现安全注销（Decomissioned → 删除）
   - SetupWithManager 增加 Owns(PDB)
4. 完善 safekeeper_controller.go 注册流程
   - registerWithSC() 传入正确的 host（headless service DNS）
   - 从 Node topology 标签读取 availability_zone_id
5. 编写测试
```

### Phase 2d: 扩缩容协调（Week 3）

```
目标：安全扩缩容

1. 缩容保护：
   - 验证缩容后仍满足 quorum (>=3)
   - 缩容前先将目标 safekeeper 设为 Decomissioned
   - 等待 Storage Controller 确认后再删除
2. 扩容协调：
   - 确保新 safekeeper 注册成功才标记就绪
3. 编写集成测试
```

### Phase 2e: 本地盘故障恢复（SC 负责）（Week 3）

```
目标：节点故障后自动恢复，由 Storage Controller 负责

Operator 侧（本 Phase 的工作）：
1. 确保 safekeeper StatefulSet 正确配置：
   - --id= 稳定不变（StatefulSet 序列号）
   - --storage-controller-url= 或 --broker-addr= 正确指向 SC
   - --auth-token= 或 JWT token 环境变量正确注入
2. 可选：SafekeeperStatus 增加 LastObservedNode（仅观察，不用于触发恢复）

Storage Controller 侧（上游 neon 需要增加的能力）：
1. Heartbeat 循环中检测 safekeeper Available 但数据为空
2. 查询 timelines 表获取该 safekeeper 应拥有的 timeline（通过 sk_set）
3. 通过已有的 safekeeper_reconciler 调度 Pull 操作
4. pending_ops 跟踪恢复进度

测试：
- 模拟节点故障 + Pod 漂移
- 验证 SC 自动触发 Pull 操作
- 验证 safekeeper 数据恢复
```

---

## 6. 风险评估

| 风险 | 影响 | 缓解措施 |
|------|:--:|------|
| 缩容导致 Quorum 丢失 | 🔴 数据库不可写 | 缩容前验证 `numSafekeepers >= 3`，逐个下线；Operator 阻止缩到 <3 |
| 删除时 WAL 数据未消费 | 🟡 数据丢失 | 先 `Decomissioned` 调度策略 → SC 停止分配新 timeline；已有 timeline 通过 safekeeper_migrate 迁移 |
| SC 无 DELETE safekeeper 端点 | 🟡 注册记录残留 | 通过 `scheduling_policy=Decomissioned` 退役；DB 记录保留便于重建；可向上游提 PR 添加 DELETE 端点 |
| 本地盘节点故障导致 Pod 漂移 | 🟡 新节点本地盘为空 | SC 持续心跳检测 + DB 中 sk_set 精确知道每个 safekeeper 应有哪些 timeline；SC 通过已有 reconciler 自动调度 Pull 操作恢复数据，Operator 仅需确保 safekeeper 能连接 SC/broker |
| `/v1/status` 作为探针不够精细 | 🟢 低 | 基于 upstream neon 的 ALLOWLIST_ROUTES 设计，此为唯一无需认证的端点 |
| SC 不可达时 Finalizer 卡死 | 🟡 删除阻塞 | Finalizer 中 SC 调用失败仅警告，不阻塞删除；SC 通过心跳超时自行检测 |

---

## 7. 附录

### 7.1 Safekeeper Quorum 计算

```
N = NumSafekeepers (必须是 >= 3 的奇数)
Q = N/2 + 1 (Quorum)

N=3: Q=2  (容忍 1 个故障)
N=5: Q=3  (容忍 2 个故障)
N=7: Q=4  (容忍 3 个故障)
```

### 7.2 与后续 Phase 的衔接

```
Phase 2 (本阶段)        Phase 3                  Phase 4
Safekeeper 自动化  ──→  JWT 全链路认证  ──→  去除 --dev + HA 部署
├─ 自动创建 3+ SK       ├─ SC 配置 public-key      ├─ Storage Controller Strict
├─ Health Probes        ├─ safekeeper JWT token    ├─ PDB
├─ AntiAffinity         ├─ Operator→SC JWT         └─ 监控告警
├─ PDB                  └─ ControlPlane JWT
├─ 安全注销
├─ Status 聚合
└─ 恢复由 SC 负责 (Operator 仅传配置)
```

### 7.3 参考文件

| 文件 | 说明 |
|------|------|
| `neon/safekeeper/src/http/routes.rs` | Safekeeper HTTP 端点实现 |
| `neon/safekeeper/src/bin/safekeeper.rs` | Safekeeper CLI 参数 |
| `neon/storage_controller/src/service/safekeeper_service.rs` | SC 的 Safekeeper 管理 |
| `neon/storage_controller/src/service/safekeeper_reconciler.rs` | SC 的 Safekeeper Reconciler（Pull/Exclude/Delete） |
| `neon/storage_controller/src/heartbeater.rs` | SC 的 Heartbeater（Safekeeper 可用性检测） |
| `neon/storage_controller/src/safekeeper.rs` | SC 的 Safekeeper 数据结构 |
| `neon/storage_controller/src/persistence.rs` | SC 的持久化层（timelines.sk_set, safekeeper 注册） |
| `neon/storage_controller/src/http.rs` | SC 的 HTTP 路由 |
| `neon-operator/docs/failure-recovery.md` | 故障恢复分析 |
| `neon-operator/docs/design/safekeeper-deletion.md` | 原有删除设计 |
| `neon-operator/docs/design/todo.md` | 整体演进计划 |

### 7.4 上游 neon pull_timeline 源码分析

> 本节记录了上游 neon 源码中 `pull_timeline` 机制的详细实现，供 Operator 开发时参考。

#### 核心入口：`safekeeper/src/pull_timeline.rs`

**`handle_request()`** (L449-687)：

```rust
async fn handle_request(
    request: PullTimelineRequest,      // {tenant_id, timeline_id, http_hosts: [peer_urls], mconf}
    sk_auth_token: Option<String>,
    ca_certs: Option<Certificate>,
    global_timelines: Arc<GlobalTimelines>,
    wait_for_peer_timeline_status: bool,
) -> Result<PullTimelineResponse>
```

执行流程：

1. **成员检查**（L456-463）：如果提供了 `mconf`，验证此 safekeeper 是配置成员
2. **幂等检查**（L465-493）：如果 timeline 已存在，只执行 membership switch，返回成功
3. **并行查询 peers**（L506-556）：对所有 `http_hosts` 并行调用 `GET /v1/tenant/{tid}/timeline/{tlid}`
   - 允许最多 1 个 peer 返回错误（`min_required_successful = hosts.len() - 1`，至少 1 个）
4. **选择最优源**（L615-637）：按 `(epoch, flush_lsn, term, commit_lsn)` 降序排序，选最大值
5. **下载并加载**（L689-786）：调用 `pull_timeline()` 函数：
   - 创建临时目录
   - `GET /v1/tenant/{tid}/timeline/{tlid}/snapshot?destination_id={my_id}` 下载 tar
   - 逐文件解压 + `sync_all()` + `fsync` 目录
   - 验证 commit_lsn/flush_lsn 一致性
   - `global_timelines.load_temp_timeline()` 加载到全局 map
   - 可选 `membership_switch()`

#### Snapshot 端点：`safekeeper/src/http/routes.rs` L273-334

```rust
async fn timeline_snapshot_handler(request) {
    // Put WAL removal on hold during snapshot
    tli.set_wal_removal_on_hold(true);
    // Stream tar archive containing:
    //   - safekeeper.control (control file, bincode)
    //   - *.partial WAL segment files
    // Creates a streaming tar body
}
```

#### Storage Controller 的 safekeeper_migrate（备选方案）

位于 `storage_controller/src/service/safekeeper_service.rs` L1127-1424，基于 RFC-035 的 8 步 joint consensus 算法：

```
Step 1: CAS increment generation, set new_sk_set in DB
Step 2: PUT configuration (joint_conf) on OLD set → need quorum
Step 3: Notify cplane/compute
Step 4: pull_timeline to NEW safekeepers (并行调用 safekeeper /v1/pull_timeline)
Step 5: PUT configuration (joint_conf) on NEW set → wait for sync
Step 6: Commit new_conf (without new_sk_set) to DB
Step 7: Create Exclude pending ops
Step 8: finish_safekeeper_migration
        → PUT configuration on new set
        → Exclude old safekeepers
        → Notify cplane
```

**Operater 选择 `pull_timeline` 而非 `safekeeper_migrate` 的原因**：
- 恢复的是同一 safekeeper（相同 ID），不是替换
- 无需 generation 变更
- 流程简单，出错面小
- 无需通知 cplane 或更改 compute 配置

#### 关键 Safekeeper HTTP API 端点汇总

| 端点 | 用途 | Operator 使用场景 |
|------|------|-----------------|
| `GET /v1/status` | 健康状态 | 探针、检测是否需要恢复 |
| `GET /v1/timeline` | 列出所有 timeline | 从 peer 获取 timeline 列表 |
| `GET /v1/tenant/{tid}/timeline/{tlid}` | TimelineStatus | 检查 membership（peers 字段含本 SK ID） |
| `POST /v1/pull_timeline` | 从 peer 拉取 timeline | **核心恢复 API** |
| `POST /v1/tenant/{tid}/timeline/{tlid}/configuration` | 更新配置 | membership switch |

#### Storage Controller API 端点汇总

| 端点 | 用途 | Operator 使用场景 |
|------|------|-----------------|
| `POST /control/v1/safekeeper/{id}` | 注册/更新 safekeeper | 初始化注册 |
| `POST /control/v1/safekeeper/{id}/scheduling_policy` | 设置调度策略 | 安全注销 (Decomissioned) |
| `POST /v1/tenant/{tid}/timeline/{tlid}/safekeeper_migrate` | Timeline 迁移 | 备选恢复方案 |
| `GET /control/v1/safekeeper` | 列出所有 safekeeper | 获取 peer 列表 |
