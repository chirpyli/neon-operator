# Resource Deletion with Finalizers — 生产级删除方案

> **状态**: 已实现并验证
> **日期**: 2026-07-02
> **版本**: v1.6

---

## 1. 问题背景

### 1.1 当前现状

neon-operator 管理 5 种 CRD 资源，创建时与上游 Storage Controller 交互：

| 资源                 | 创建时的外部调用                        |  删除时的清理  |                    OwnerReference 级联                    |
| -------------------- | --------------------------------------- | :------------: | :-------------------------------------------------------: |
| **Cluster**    | —                                      |       —       | StorageController/Broker Deployment+Service+JWT Secret ✅ |
| **Pageserver** | —                                      |       —       |             StatefulSet+Service+ConfigMap ✅             |
| **Safekeeper** | `POST /control/v1/safekeeper/{id}`    | **缺失** |                  StatefulSet+Service ✅                  |
| **Project**    | `PUT /v1/tenant/{id}/location_config` | **缺失** |                            —                            |
| **Branch**     | `POST /v1/tenant/{id}/timeline`       | **缺失** |              Deployment+Service+ConfigMap ✅              |

### 1.2 核心缺陷

**所有 5 个 Controller 都不包含 Finalizer 机制**，导致：

1. **Storage Controller 残留数据**：删除 Branch CR 后，Storage Controller 数据库中的 timeline 记录成为孤儿。删除 Project CR 后，tenant 记录残留。
2. **Safekeeper 未注销**：删除 Safekeeper CR 后，Storage Controller 仍认为该 safekeeper 在线，可能导致后续 timeline 分配到已不存在的节点。
3. **删除顺序无保障**：用户可能直接删除 Cluster，而 Project/Branch 仍引用它，导致状态不一致。
4. **无删除状态可观测性**：无法通过 `kubectl get` 或 status conditions 了解删除进度。

### 1.3 实际影响

用户在生产环境中发现：删除 `kubectl delete project` 和 `kubectl delete branch` 后，Storage Controller 的 `tenant_shards` 表仍残留 5 条记录：

```
b280ed9fc1c1c50a0bc3bc8e8c6e2dc7 | {"Attached":0} | "Active"
a454c1a8ec748404629288934a364acb | {"Attached":1} | "Active"
...
```

这些孤儿数据只能通过手动 `storcon_cli tenant delete` 或直接 SQL DELETE 清理。

---

## 2. 设计目标

1. **完整性**：所有需要外部清理的资源必须通过 Finalizer 保障
2. **可靠性**：即使 Storage Controller 临时不可达，Finalizer 确保不会丢失清理机会
3. **幂等性**：重复调和或重试不会产生副作用
4. **可观测性**：通过 Status Conditions 反映删除进度
5. **最小侵入**：不改变现有的创建/更新调和逻辑，仅在删除路径增加 Finalizer 处理

---

## 3. 架构设计

### 3.1 Finalizer 生命周期

```
资源创建
    │
    ▼
Reconcile (obj.DeletionTimestamp == nil)
    │
    ├─ Finalizer 不存在 → AddFinalizer → Update → Requeue
    │
    └─ Finalizer 已存在 → 正常业务调和逻辑
                              │
                              ▼
                        kubectl delete 触发
                              │
                              ▼
                        APIServer 设置 DeletionTimestamp
                              │
                              ▼
Reconcile (obj.DeletionTimestamp != nil && ContainsFinalizer)
    │
    ├─ 阶段 1: 设置 Condition "Terminating"=True
    ├─ 阶段 2: 调用 Storage Controller DELETE API
    │   ├─ 成功 → RemoveFinalizer → APIServer 删除 etcd 记录
    │   └─ 失败 → Requeue（Finalizer 保留，下次重试）
    │
    └─ 阶段 3: 子资源由 OwnerReference 级联自动删除
```

### 3.2 Finalizer 常量

```go
// utils/finalizer.go
const FinalizerName = "neon.oltp.molnett.org/finalizer"
```

统一使用一个 Finalizer 名称，简化管理。

### 3.3 通用 Finalizer 调和模式

所有 Controller 遵循统一的 `Reconcile` 入口模式：

```go
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    obj, err := r.get(ctx, req)
    if err != nil {
        return ctrl.Result{}, err
    }
    if obj == nil {
        // CR 已被彻底删除（finalizer 已移除且 etcd 记录已清理）
        return ctrl.Result{}, nil
    }

    // === 删除路径 ===
    if !obj.GetDeletionTimestamp().IsZero() {
        return r.finalize(ctx, obj)
    }

    // === 创建/更新路径 ===
    if !controllerutil.ContainsFinalizer(obj, utils.FinalizerName) {
        controllerutil.AddFinalizer(obj, utils.FinalizerName)
        if err := r.Update(ctx, obj); err != nil {
            return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
        }
        return ctrl.Result{Requeue: true}, nil
    }

    return r.reconcile(ctx, obj)
}
```

---

## 4. 各资源详细设计

### 4.1 Branch Controller

#### 删除职责

- 调用 Storage Controller `DELETE /v1/tenant/{tenantID}/timeline/{timelineID}` 删除 timeline
- K8s 子资源（Deployment/Service/ConfigMap）由 OwnerReference 自动级联删除

#### finalize 流程

```
Branch.DeletionTimestamp != nil
    │
    ├─ 提取 tenantID (从 Project CR)
    ├─ 提取 timelineID (从 Branch.Spec.TimelineID)
    │
    ├─ Project 不存在？（可能已被先删除）
    │   └─ 记录 Warning 日志，跳过外部 API 调用，直接 RemoveFinalizer
    │
    ├─ TimelineID 为空？（从未创建成功）
    │   └─ 直接 RemoveFinalizer
    │
    ├─ 调用 DELETE /v1/tenant/{tenantID}/timeline/{timelineID}
    │   ├─ 200/404 响应 → 视为成功
    │   ├─ 连接错误 → Requeue（5s 后重试）
    │   └─ 其他错误 → Requeue
    │
    └─ 成功后：RemoveFinalizer → Update → APIServer 完成删除
```

#### 新增条件类型

| Condition       | 含义                      |
| --------------- | ------------------------- |
| `Terminating` | True 表示正在执行外部清理 |

#### 关键决策

1. **Project 被先删除时**：跳过 Storage Controller 调用，直接移除 Finalizer。因为 Project 的 Finalizer 应该先调用 `DELETE /v1/tenant/{id}` 删除整个 tenant（级联删除其下所有 timeline），所以 timeline 可能已被清理。即使未被清理，也不应阻塞 Branch CR 的删除。
2. **TimelineID 为空时**：说明 Branch 从未成功创建（可能 reconcile 一直失败），直接移除 Finalizer。
3. **容忍 404**：上游 Storage Controller 的 `handle_timeline_delete` 会将底层 pageserver 的 404 转为 200，所以即使是重复删除也是幂等的。

### 4.2 Project Controller

#### 删除职责

- 调用 Storage Controller `DELETE /v1/tenant/{tenantID}` 删除 tenant（级联删除其下所有 timeline）

#### finalize 流程

```
Project.DeletionTimestamp != nil
    │
    ├─ TenantID 为空？（从未生成）
    │   └─ 直接 RemoveFinalizer
    │
    ├─ 调用 DELETE /v1/tenant/{tenantID}
    │   ├─ 200/404 响应 → 视为成功
    │   ├─ 连接错误 → Requeue
    │   └─ 其他错误 → Requeue
    │
    └─ 成功后：RemoveFinalizer → Update → APIServer 完成删除
```

#### 依赖检查决策

**不阻塞 Project 删除即使仍有 Branch**。原因：

- Kubernetes 的资源删除顺序应由用户/上层编排控制，不应在 Operator 层强制
- 如果 Project 被强制删除，其关联的 Branch 的 finalize 逻辑会检测到 Project 不存在并优雅降级
- 上游 Neon 的 Storage Controller 的 tenant delete 会自动级联删除该 tenant 下的所有 timeline

### 4.3 Safekeeper Controller

#### 删除职责

- **SC Decommission**：调用 `POST /control/v1/safekeeper/:id/scheduling_policy` 设置 `SchedulingPolicy=Decomissioned`
- SC 会停止该 safekeeper 的 reconciler、跳过心跳、排除调度
- K8s 子资源（StatefulSet/Service/ConfigMap/PDB）由 OwnerReference 自动级联删除

#### finalize 流程

```
Safekeeper.DeletionTimestamp != nil
    │
    ├─ SCClient.DecommissionSafekeeper() → POST /scheduling_policy
    │   ├─ 成功 → RemoveFinalizer
    │   └─ 失败？
    │       ├─ Cluster CR 已不存在 → SC 已不可恢复 → RemoveFinalizer
    │       │   （Cluster finalizer 的 cleanupSafekeeperNodes 已兜底处理）
    │       └─ Cluster CR 仍存在 → SC 临时不可达 → Requeue 重试
    │
    └─ RemoveFinalizer → Update → APIServer 完成删除
```

#### 关键决策

1. **SC 不可达时不再直接移除 Finalizer**（v1.6）：
   - 旧行为：`DecommissionSafekeeper` 失败 → 跳过 → 移除 Finalizer
   - 问题：safekeeper 以 `scheduling_policy='Active'` 残留在 SC 数据库中
   - 新行为：检查 Cluster CR 是否存在
     - Cluster 仍存在 → SC 临时不可达 → Requeue 重试
     - Cluster 已删除 → SC 已不可恢复 → 移除 Finalizer（Cluster finalizer 已兜底）

2. **SC 没有 safekeeper 的 DELETE 端点**：safekeeper 生命周期完全通过 `scheduling_policy` 状态机管理（Active/Activating/Pause/Decomissioned），数据库行永不物理删除。

3. **Cluster 级兜底清理**（v1.6）：
   - Cluster finalizer 在移除前调用 `cleanupSafekeeperNodes()`
   - 确保即使 Safekeeper finalizer 因 SC 不可达而失败，记录也会被标记为 Decomissioned

4. **Headless Service DNS 修复**（v1.6）：Safekeeper Headless Service 同样设置 `publishNotReadyAddresses: true`，避免与 Pageserver 相同的 DNS 死锁问题。

### 4.4 Pageserver Controller

#### 删除职责

- **安全下线（Graceful Drain）**：将节点上的 attached shard 迁移到其他节点
- **节点 Tombstone**：调用 SC `PUT /control/v1/node/{id}/delete` 将节点标记为 `lifecycle='Deleted'`
- K8s 子资源（StatefulSet/Service/ConfigMap/PDB）由 OwnerReference 自动级联删除

#### finalize 流程

```
Pageserver.DeletionTimestamp != nil
    │
    ├─ SCClient 未配置？
    │   └─ 直接 RemoveFinalizer（测试/非生产环境）
    │
    ├─ Phase 0: 准入检查
    │   ├─ ListNodeNodes() 获取所有节点
    │   ├─ SC 不可达？
    │   │   ├─ Cluster CR 已不存在 → SC 已不可恢复 → RemoveFinalizer
    │   │   └─ Cluster CR 仍存在 → SC 临时不可达 → Requeue 重试
    │   ├─ 节点不在 SC 中？ → RemoveFinalizer
    │   ├─ 节点已在 Draining？ → 跳到 Phase 2
    │   └─ 无其他可调度节点？ → Requeue（等待扩容或手动 force-delete）
    │
    ├─ Phase 1: 启动 Drain
    │   └─ StartNodeDrain() → PUT /control/v1/node/{id}/drain
    │
    ├─ Phase 2: 监控 Drain 进度
    │   ├─ GetNodeShards() 轮询 attached shard count
    │   ├─ attachedCount > 0 → 继续轮询
    │   └─ attachedCount == 0 → Phase 3
    │       └─ 超时（30min） → 设置 ConditionDrainTimeout
    │
    └─ Phase 3: Drain 完成
        ├─ ConfigureNode(PauseForRestart)
        ├─ StartNodeDelete(force=false) → PUT /node/{id}/delete
        │   └─ 失败？ → Requeue 重试（不可静默忽略）
        └─ RemoveFinalizer
```

#### 关键决策

1. **SC 不可达时不再直接移除 Finalizer**（v1.6）：

   - 旧行为：`ListNodeNodes()` 失败 → 跳过所有清理 → 移除 Finalizer
   - 问题：节点记录以 `lifecycle='Active'` 残留在 SC 数据库中
   - 新行为：检查 Cluster CR 是否存在
     - Cluster 仍存在 → SC 临时不可达 → Requeue 重试
     - Cluster 已删除 → SC 已不可恢复 → 移除 Finalizer（兜底，此时 Cluster finalizer 应已完成 tombstone）
2. **StartNodeDelete 错误不可静默忽略**（v1.6）：

   - 旧行为：`_ = r.SCClient.StartNodeDelete(...)` — 静默丢弃所有错误
   - 新行为：记录错误日志 + 设置 Condition + Requeue 重试
3. **Cluster 级兜底清理**（v1.6）：

   - Cluster finalizer 在移除前调用 `cleanupPageserverNodes()`
   - 使用 `force=true` 模式（跳过优雅 drain，直接 tombstone）
   - 确保即使 Pageserver finalizer 因 SC 不可达而失败，节点也能被 tombstone

4. **Headless Service DNS 死锁修复**（v1.6）：

   - SC 在接收 `/upcall/v1/re-attach` 时对 Pageserver 提供的 `listen_http_addr` 执行 DNS 解析（`tokio::net::lookup_host`），DNS 不可解析时返回 503
   - 旧行为：Headless Service 未设置 `publishNotReadyAddresses`（默认 `false`）
   - 问题：Pod 未 Ready → CoreDNS 不创建 A 记录 → SC DNS 解析失败 → 503 "unknown DNS name" → Pageserver 永远无法完成 re-attach → Pod 永远不为 Ready → **死锁**
   - 新行为：Headless Service 设置 `publishNotReadyAddresses: true`，未就绪 Pod 也能获得 DNS A 记录

### 4.5 Cluster Controller

#### 删除职责

- **阻塞 Cluster 删除**，直到所有依赖的 Project 已经清理完毕
- **SC 节点兜底清理**：在移除 Finalizer 前，强制 tombstone 所有 Pageserver 节点 + Decommission 所有 Safekeeper 节点
- 子资源（Deployment/Service/Secret）由 OwnerReference 级联删除，在 Finalizer 移除后生效

#### finalize 流程

```
Cluster.DeletionTimestamp != nil
    │
    ├─ 列出所有 Project (Spec.ClusterName == cluster.Name)
    │
    ├─ 存在关联的 Project？
    │   │
    │   ├─ 是 → 对尚未标记删除的 Project 发起 Delete
    │   │       └─ 设置 Status Condition "Terminating"=True
    │   │       └─ Requeue (10s 后重新检查)
    │   │
    │   └─ 否 → 所有依赖已清除
    │
    ├─ Step 3a: 兜底清理 SC 节点
    │   ├─ cleanupPageserverNodes() → 逐个 StartNodeDelete(force=true)
    │   └─ cleanupSafekeeperNodes() → 逐个 DecommissionSafekeeper()
    │       └─ 失败不阻塞（已尽力）
    │
    └─ Step 3b: 所有 Project 已清除 → RemoveFinalizer
                                         → APIServer 级联删除子资源
```

#### 关键决策

**新增 SC 节点兜底清理（v1.6）**：

SC 使用外部 PostgreSQL 数据库持久化节点记录：
- **Pageserver (nodes)**：Tombstone 机制（软删除），`lifecycle='Deleted'` 后 `list_nodes()` 不再加载
- **Safekeeper (safekeepers)**：调度策略状态机，`Decomissioned` 后停止 reconciler/心跳/调度

如果 Cluster finalizer 不主动清理节点：
- Pageserver：`lifecycle='Active'` 残留 → 重建时 re-attach 409 Conflict → Not Ready
- Safekeeper：`scheduling_policy='Active'` 残留 → SC 调度算法可能在畸形 AZ 场景下向不存在的节点分配 timeline

兜底流程：
1. `cleanupPageserverNodes()`：逐个调用 `PUT /control/v1/node/{id}/delete?force=true`（跳过 drain，直接 tombstone）
2. `cleanupSafekeeperNodes()`：逐个调用 `POST /control/v1/safekeeper/:id/scheduling_policy`（设置 Decomissioned）
3. 失败不阻塞——至少尝试了清理，比直接跳过要安全

#### 关键决策

**阻塞 Cluster 删除直到所有依赖 Project 清理完毕**。原因：

- Project 的 finalize 需要调用 Storage Controller API（DELETE tenant），而 Storage Controller 由 Cluster 管理
- 如果先删除 Cluster，Storage Controller Deployment 会被级联删除，导致 Project finalize 无限重试失败
- 通过 Finalizer 阻塞，确保清理顺序正确：

```
用户删除 Cluster
  → Cluster Finalizer 阻塞
  → 逐个删除 Project (仍能访问 Storage Controller)
  → Project Finalizer 调用 DELETE tenant 成功
  → Branch Finalizer 检测 Project 不存在，跳过
  → 所有 Project 清理完毕
  → Cluster 移除 Finalizer
  → K8s 级联删除 Storage Controller 等子资源
```

**为什么不需要显式删除 Branch**：Branch 依赖 Project（通过 `Spec.ProjectID`），Project 的 `DELETE tenant` 会在 Storage Controller 中级联删除其下的 timeline。Branch 的 finalize 在检测到 Project 不存在时会优雅降级（跳过 timeline 删除）。

---

## 5. Storage Controller API 集成

### 5.1 需要新增调用的 DELETE 端点

| 端点                                              | 方法   | 调用者                | 上游源码位置                                                         |
| ------------------------------------------------- | ------ | --------------------- | -------------------------------------------------------------------- |
| `/v1/tenant/{tenant_id}`                        | DELETE | Project Controller    | `storage_controller/src/http.rs` → `handle_tenant_delete`       |
| `/v1/tenant/{tenant_id}/timeline/{timeline_id}` | DELETE | Branch Controller     | 同上`handle_timeline_delete`                                       |
| `/control/v1/safekeeper/{id}`                   | DELETE | Safekeeper Controller | 待实现（上游暂无此端点，详见`docs/design/safekeeper-deletion.md`） |

### 5.2 HTTP 客户端配置

复用现有的 HTTP 调用模式：

- Timeout: 30 秒
- Base URL: 优先使用 `StorageControllerBaseURL`，否则通过 `storagecontroller.URL(clusterName)` 构造
- 使用 `http.NewRequestWithContext(ctx, ...)` 以支持 context 取消

### 5.3 错误处理策略

| 错误类型                      | 重试策略       | Finalizer 行为 |
| ----------------------------- | -------------- | -------------- |
| 网络连接失败（DNS/超时/拒绝） | 5s 后 Requeue  | 保留 Finalizer |
| 非 2xx/404 状态码             | 10s 后 Requeue | 保留 Finalizer |
| 404 Not Found                 | 视为成功       | 移除 Finalizer |
| 2xx 成功                      | —             | 移除 Finalizer |

---

## 6. 删除顺序与编排

### 6.1 资源依赖拓扑

```
Cluster (父)
  ├── Pageserver (引用 Cluster, OwnerReference)
  │     └── SC 数据库 Nodes 表 (节点注册，Tombstone 持久化)
  ├── Safekeeper (引用 Cluster, OwnerReference)
  ├── StorageController (由 Cluster 管理，OwnerReference)
  │     └── 外部 PostgreSQL 数据库 (SC 持久化层)
  └── StorageBroker (由 Cluster 管理，OwnerReference)

Project (引用 Cluster.Spec.ClusterName)
  └── Branch (引用 Project.Spec.ProjectID)
```

**关键依赖链**：Branch → Project → Cluster → StorageController

**SC 数据库节点生命周期**：

```
Pageserver 创建 → PS 启动 → /upcall/v1/re-attach → SC INSERT nodes (lifecycle='Active')
Cluster 删除   → Cluster.finalize() → PUT /node/{id}/delete?force=true → SC set_tombstone()
                                        → lifecycle='Deleted' (Tombstone，非物理删除)
重建 Cluster   → DELETE /debug/v1/tombstone/:id → SC 物理删除行（可选清理）
```

### 6.2 推荐删除顺序

**推荐**：直接删除 Cluster，Operator 会自动处理依赖顺序：

```bash
# 删除 Cluster — Operator 会自动：
# 1. 删除所有 Project（级联触发 Branch 清理）
# 2. 等待 Project/Branch 全部清理完毕
# 3. 兜底清理 SC 节点（StartNodeDelete force=true）
# 4. 移除 Cluster Finalizer，K8s 级联删除子资源
kubectl delete cluster <name> -n neon
```

**手动顺序**（如果需要分步控制）：

```bash
# 1. 删除所有 Branch
kubectl delete branches --all -n neon

# 2. 删除所有 Project
kubectl delete projects --all -n neon

# 3. 删除 Pageserver 和 Safekeeper（每个都会触发 drain/tombstone）
kubectl delete pageserver --all -n neon
kubectl delete safekeeper --all -n neon

# 4. 删除 Cluster（Cluster finalizer 会做兜底节点清理）
kubectl delete cluster --all -n neon
```

### 6.3 乱序删除的容错

| 场景                               | 行为                                                        |
| ---------------------------------- | ----------------------------------------------------------- |
| 删除 Cluster 时有 Project          | **阻塞**删除，先清理 Project                          |
| 删除 Cluster 时有 Branch           | Project 删除级联处理 Branch                                 |
| 先删 Project 再删 Branch           | Branch finalize 检测 Project 不存在 → 跳过外部 API         |
| 单独删除 Pageserver                | 执行完整 drain → tombstone 流程                            |
| 单独删除 Safekeeper                | 调用 DecommissionSafekeeper() → SC 不可达时 Requeue 重试   |
| SC 在 Pageserver finalize 时不可达 | 检查 Cluster CR 是否存在 → Requeue 重试（如 Cluster 存在） |
| SC 在 Safekeeper finalize 时不可达 | 检查 Cluster CR 是否存在 → Requeue 重试（如 Cluster 存在） |
| 手动移除 Cluster Finalizer         | SC 节点无 tombstone/decommission → 残留 Active → 重建冲突  |

### 6.4 删除终态验证

以下为 `kubectl delete cluster my-cluster -n neon` 后 SC 数据库的预期终态：

**nodes 表**：

```
 node_id | scheduling_policy | lifecycle
---------+-------------------+-----------
       1 | deleting          | deleted
       2 | deleting          | deleted
```

**safekeepers 表**：

```
 id | scheduling_policy
----+-------------------
  1 | Decomissioned
  2 | Decomissioned
  3 | Decomissioned
```

各字段含义：

| 表 | 字段 | 终态值 | 含义 |
|----|------|--------|------|
| nodes | `scheduling_policy` | `deleting` | SC 已接收删除指令，正在/已完成 shard 迁移和 tombstone |
| nodes | `lifecycle` | `deleted` | **Tombstone 已成功执行**，`list_nodes()` 不再加载 |
| safekeepers | `scheduling_policy` | `Decomissioned` | **已退役**，reconciler 停止、心跳跳过、不参与调度 |

**为什么这些终态是正确的**：
- nodes: `lifecycle='Deleted'` → `list_nodes()` 过滤 → 重建时 re-attach 不被阻塞
- safekeepers: `scheduling_policy='Decomissioned'` → 调度算法排除 → 不会被分配新 timeline
- 两表记录都**不会被物理删除**（设计如此），需要手动清理或通过 debug 端点

### 6.5 v1.6 变更总结

| 修复项 | 文件 | 描述 |
|--------|------|------|
| B1: Pageserver SC 不可达 fallback | `pageserver_controller.go` | 不再直接移除 finalizer，改为检查 Cluster CR 存在性 → Requeue 或兜底跳过 |
| B2: Pageserver StartNodeDelete 静默忽略 | `pageserver_controller.go` | 错误不再被 `_ =` 丢弃，失败时记录日志 + Requeue 重试 |
| B3: Cluster 级 Pageserver 兜底清理 | `cluster_controller.go` | 新增 `cleanupPageserverNodes()`，在移除 Cluster finalizer 前强制 tombstone 所有 Pageserver 节点 |
| B4: Tombstone 物理清理 | `sc_client.go` | 新增 `DeleteTombstone()` 方法，封装 `DELETE /debug/v1/tombstone/:node_id` |
| B5: Safekeeper SC 不可达 fallback | `safekeeper_controller.go` | 不再直接移除 finalizer，改为检查 Cluster CR 存在性 → Requeue 或兜底跳过 |
| B6: Cluster 级 Safekeeper 兜底清理 | `cluster_controller.go` | 新增 `cleanupSafekeeperNodes()`，在移除 Cluster finalizer 前强制 Decommission 所有 Safekeeper 节点 |
| B7: Headless Service DNS 死锁修复 | `specs/pageserver/service.go`, `specs/safekeeper/service.go` | Headless Service 设置 `publishNotReadyAddresses: true`，避免 SC re-attach DNS 解析死锁 |
| — | SCClient 共享 | `cmd/controller/main.go` | ClusterReconciler 注入共享 SCClient 实例（之前无 SCClient） |

---

## 7. 可观测性

### 7.1 Status Conditions

删除期间新增以下 Condition：

| Condition       | 资源 | 含义                      |
| --------------- | ---- | ------------------------- |
| `Terminating` | 全部 | True 表示正在执行外部清理 |

### 7.2 Kubernetes Events

删除流程中发出以下 Normal/Warning Events：

```
Normal   FinalizerAdded      "Finalizer neon.oltp.molnett.org/finalizer added"
Normal   ExternalCleanupStarted  "Calling DELETE /v1/tenant/{id}/timeline/{tid}"
Normal   ExternalCleanupSucceeded "Timeline deleted from Storage Controller"
Warning  ExternalCleanupFailed    "Failed to delete timeline: connection refused"
Warning  ParentResourceMissing    "Project {name} not found, skipping timeline deletion"
Normal   FinalizerRemoved     "Finalizer removed, resource will be deleted"
```

### 7.3 日志

每个阶段都使用结构化日志：

```
"msg"="Finalizing Branch deletion" "branch"="main-branch" "tenantID"="abc123" "timelineID"="def456"
"msg"="Timeline deletion succeeded" "status"=200
"msg"="Project not found, skipping external cleanup" "projectID"="my-project"
"msg"="Finalizer removed, Branch will be deleted by APIServer"
```

---

## 8. 测试策略

### 8.1 Fake StorageController 扩展

为 `test/fakes/storage_controller.go` 新增：

- `DELETE /v1/tenant/{id}/location_config` → 默认 200
- `DELETE /v1/tenant/{id}/timeline` → 默认 200
- `DELETE /control/v1/safekeeper/{id}` → 默认 200

新增 Hook 字段：

```go
type StorageController struct {
    // ...existing fields...
  
    // DeleteTenant override for DELETE /v1/tenant/{id}
    DeleteTenant http.HandlerFunc
  
    // DeleteTimeline override for DELETE /v1/tenant/{id}/timeline/{timeline_id}
    DeleteTimeline http.HandlerFunc
  
    // DeleteSafekeeper override for DELETE /control/v1/safekeeper/{id}
    DeleteSafekeeper http.HandlerFunc
}
```

### 8.2 单元测试覆盖

| 测试场景                               | 验证点                                                   |
| -------------------------------------- | -------------------------------------------------------- |
| Branch 正常删除                        | Finalizer 添加 → Timeline DELETE 调用 → Finalizer 移除 |
| Branch 删除时 Project 已不存在         | 跳过 API 调用，直接移除 Finalizer                        |
| Branch 删除时 TimelineID 为空          | 直接移除 Finalizer                                       |
| Branch 删除时 StorageController 不可达 | 重试，Finalizer 保留                                     |
| Project 正常删除                       | Finalizer 添加 → Tenant DELETE 调用 → Finalizer 移除   |
| Project 删除时 TenantID 为空           | 直接移除 Finalizer                                       |
| Safekeeper 正常删除                    | Finalizer 添加 → Finalizer 移除（不调用外部 API）       |
| Cluster 删除时有依赖                   | Warning Event 发出，不阻塞                               |

### 8.3 E2E 测试（建议）

```bash
# 完整删除流程
kubectl apply -f neon-cluster.yaml
# ...wait for ready...
kubectl delete branch main-branch -n neon
kubectl wait --for=delete branch/main-branch -n neon --timeout=60s
# 验证 Storage Controller 中 timeline 记录已清除

kubectl delete project my-project -n neon
kubectl wait --for=delete project/my-project -n neon --timeout=60s
# 验证 Storage Controller 中 tenant 记录已清除
```

---

## 9. 迁移路径

### 9.1 升级策略

1. **新部署**：无影响，新资源自动添加 Finalizer
2. **存量资源升级**：
   - 升级 Operator 镜像后，存量资源的下一次 Reconcile 会自动添加 Finalizer
   - 已经存在的 orphan 数据（Storage Controller 中的残留记录）不会自动清理
   - 推荐在升级前手动清理已有孤儿数据

### 9.2 回滚策略

1. 如果新版本有问题，回滚到旧版 Operator 镜像
2. 存量资源的 Finalizer 不会阻止旧版 Operator 的正常工作（旧版不检查 Finalizer）
3. 如果资源被 "卡住" 无法删除（Finalizer 存在但清理失败），手动移除：
   ```bash
   kubectl patch branch <name> -n neon -p '{"metadata":{"finalizers":[]}}' --type=merge
   ```

### 9.3 强制删除

如果 Finalizer 阻止删除且无法自动解决（如 Storage Controller 永久不可达）：

```bash
# 移除 Finalizer 并强制删除
kubectl patch branch <name> -n neon -p '{"metadata":{"finalizers":[]}}' --type=merge
kubectl delete branch <name> -n neon --force --grace-period=0
```

---

## 10. 实现清单

| #  | 文件                                             | 改动                                                                   | 优先级 | 状态 |
| -- | ------------------------------------------------ | ---------------------------------------------------------------------- | :----: | :--: |
| 1  | `utils/finalizer.go`                           | **新增** finalizer 常量                                          |   P0   |  ✅  |
| 2  | `internal/controller/branch_controller.go`     | Finalizer 模式 +`deleteTimeline()`                                   |   P0   |  ✅  |
| 3  | `internal/controller/project_controller.go`    | Finalizer 模式 +`deleteTenant()`                                     |   P0   |  ✅  |
| 4  | `internal/controller/safekeeper_controller.go` | Finalizer 模式 + Decommission（v1.6：SC 不可达时 Requeue，不再跳过） |   P0   |  ✅  |
| 5  | `internal/controller/pageserver_controller.go` | Finalizer 模式 + Graceful Drain + Tombstone                            |   P0   |  ✅  |
| 6  | `internal/controller/cluster_controller.go`    | Finalizer 模式 + 阻塞 Project 清理 + **SC 节点兜底 tombstone + Decommission** |   P0   |  ✅  |
| 7  | `internal/controller/sc_client.go`             | SC API 客户端（节点管理 + tenant/timeline +**DeleteTombstone**） |   P0   |  ✅  |
| 8  | `test/fakes/storage_controller.go`             | 新增 DELETE 端点                                                       |   P0   |  ✅  |
| 9  | `internal/controller/*_test.go`                | 单元测试                                                               |   P1   |  ✅  |
| 10 | `utils/status.go`                              | 新增 ConditionTerminating + Reason 常量                                |   P0   |  ✅  |
| 11 | `docs/design/resource-deletion-finalizer.md`   | **本文档（v1.6 更新）**                                          |   P1   |  ✅  |

### 实现细节

#### Controller 统一入口模式

所有 5 个 Controller 的 `Reconcile()` 方法均采用统一的 Finalizer 调和模式：

```go
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    obj, err := r.get(ctx, req)
    if obj == nil { return ctrl.Result{}, nil }

    // 删除路径
    if !obj.DeletionTimestamp.IsZero() {
        return r.finalize(ctx, obj)
    }

    // 创建/更新路径：确保 Finalizer
    if !controllerutil.ContainsFinalizer(obj, utils.FinalizerName) {
        controllerutil.AddFinalizer(obj, utils.FinalizerName)
        r.Update(ctx, obj)
        return ctrl.Result{Requeue: true}, nil
    }

    return r.reconcile(ctx, obj)
}
```

#### 各 Controller 的 finalize 方法

- **ProjectReconciler.finalize()**: 调用 `DELETE /v1/tenant/{tenantID}` 删除 tenant，失败时 5s 重试，404 视为成功
- **BranchReconciler.finalize()**: 调用 `DELETE /v1/tenant/{tenantID}/timeline/{timelineID}` 删除 timeline，失败时 5s 重试，404 视为成功，Project 不存在时跳过，TimelineID 为空时跳过
- **SafekeeperReconciler.finalize()**: 调用 `DecommissionSafekeeper()` POST SchedulingPolicy=Decomissioned；SC 不可达时检查 Cluster CR 是否存在 → Requeue 重试（Cluster 存在时，不可直接跳过）
- **PageserverReconciler.finalize()**: **Graceful Drain 三阶段流程**：
  - Phase 0: 准入检查（SC 可达 + 有其他可调度节点），SC 不可达时检查 Cluster CR 是否存在来决定 Requeue/跳过
  - Phase 1: `StartNodeDrain()` PUT `/node/{id}/drain` 启动 shard 迁移
  - Phase 2: `GetNodeShards()` 轮询直到 attached=0
  - Phase 3: `ConfigureNode(PauseForRestart)` + `StartNodeDelete(force=false)` → PUT `/node/{id}/delete` → SC 异步 tombstone
  - `StartNodeDelete` 错误不可静默忽略，失败时 Requeue 重试
- **ClusterReconciler.finalize()**: 列出所有依赖 Project → 发起 Delete → Requeue 等待 → 所有 Project 清理完毕后 → **兜底 SC 节点清理（`cleanupPageserverNodes` + `cleanupSafekeeperNodes`）** → 移除 Finalizer

#### 测试覆盖（envtest 集成测试）

| 测试文件                          |    场景数    | 覆盖场景                                                                                                          |
| --------------------------------- | :----------: | ----------------------------------------------------------------------------------------------------------------- |
| `project_controller_test.go`    |      5      | 创建→Available，正常删除，404 幂等删除，500 重试保留 Finalizer，TenantID 为空跳过外部调用                        |
| `branch_controller_test.go`     |      6      | 创建→TimelineID 分配，正常删除，404 幂等删除，500 重试保留 Finalizer，TimelineID 为空跳过，父 Project 已删除跳过 |
| `safekeeper_controller_test.go` |      3      | 创建 StatefulSet+Service，Available 翻转，正常删除（直接移除 Finalizer，不调用外部 API）                          |
| `pageserver_controller_test.go` |      4      | Pageserver 创建 golden tests（ConfigMap/StatefulSet/Service/PDB 生成验证），v1.6 更新 initScript DNS 修复         |
| **总计（envtest）**         | **20** | 全部通过（2026-07-02 验证）                                                                                       |

---

## 11. 未来扩展

1. **Graceful Drain**：Branch 删除前等待活跃连接排空
2. **Backup Before Delete**：Project 删除前触发最终全量备份
3. **S3 数据清理确认**：通过 Pageserver DeletionQueue 监控 S3 清理进度
4. **删除审计日志**：记录谁、何时删除了哪个资源
5. **Webhook 校验**：在删除前通过 Admission Webhook 校验依赖关系

