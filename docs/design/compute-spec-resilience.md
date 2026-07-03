# 设计方案：Compute Spec 韧性增强 — 防止 compute_ctl 随机生成 tenant_id

| 字段 | 内容 |
|------|------|
| 版本 | v1.0 |
| 日期 | 2026-06-24 |
| 作者 | — |
| 状态 | 草案 |

---

## 1. 问题背景

### 1.1 现象

删除 compute pod 后，新 pod 以 `Error` 状态退出，持续重启。每次重启，compute_ctl 在 `PUT /notify-attach` 请求中携带一个**不同的随机** `tenant_id`，导致控制面找不到对应的 Deployment，最终 pod 无法正常运行。

```
# 第一次 Error
tenant_id: b280ed9fc1c1c50a0bc3bc8e8c6e2dc7

# 第二次 Error (pod 重启)
tenant_id: ccdb0ad68a0c1a86d1da9bde2433f3fd

# CR 中的实际值
tenant_id: a454c1a8ec748404629288934a364acb   (从未变化)
```

### 1.2 影响范围

- Pod 删除/重启后可能进入 Error → CrashLoopBackOff 循环
- 每次重启生成新随机 ID，循环无法自愈
- 用户服务不可用，必须人工介入（如重启 controller 或等待存储控制器恢复）

---

## 2. 根因分析

### 2.1 完整启动链路

```
compute_ctl 进程启动
│
├─ 步骤 ①: 读取 $INITIAL_SPEC_JSON (来自 ConfigMap)
│   └─ 当前内容: {"format_version":"1.0", "compute_ctl_config":{"jwks":{...}}}
│   └─ 缺少: cluster 配置 (tenant_id / timeline_id 等)
│
├─ 步骤 ②: 调 GET /compute/api/v2/computes/{compute_id}/spec (控制面)
│   └─ 控制面 handleComputeSpec() 调用 GenerateComputeSpec(ctx, log, k8sClient, nil, computeID)
│       │
│       ├─ 从 Deployment labels 提取 tenant_id / timeline_id    ✔
│       ├─ 查找 Project / Branch CR                             ✔
│       ├─ 从 Secret 获取 JWT keys                               ✔
│       ├─ 查询 Safekeeper CR 列表                               ✔ (有降级)
│       └─ 调 GetTenantInfo(storage-controller)                 ✘ 可能失败
│           │
│           └─ HTTP GET http://{cluster}-storage-controller:8080/control/v1/tenant/{id}
│              │
│              ├─ 网络不可达 (短暂 DNS 解析失败/Service 未就绪)
│              ├─ 超时 (10s)
│              └─ storage-controller pod 重启中
│
│   结果: 返回 500 Internal Server Error
│
├─ 步骤 ③: compute_ctl 未收到正确 spec
│   └─ compute_ctl 使用硬编码/随机生成的 tenant_id
│   └─ 每次启动生成不同的随机 ID
│
├─ 步骤 ④: 调 PUT /notify-attach 携带随机 tenant_id
│   └─ 控制面 notifyAttach() 调 FindTenantDeployments(tenantID)
│   └─ 按 "neon.tenant_id" label 找不到 Deployment → 返回 500
│
└─ 步骤 ⑤: Pod 退出 → Kubernetes 重启 → 回到步骤 ①
            每次重启生成新的随机 ID → 死循环
```

### 2.2 关键矛盾

| 组件 | tenant_id | 来源 |
|------|-----------|------|
| Project CR | `a454c1a8...` (正确) | Controller reconcile 时写入 |
| Branch CR (timelineID) | `ac040664...` (正确) | Controller reconcile 时写入 |
| Deployment labels | `a454c1a8...` (正确) | `Deployment()` 函数从 CR 读取 |
| ConfigMap `spec.json` | **无** | `ConfigMap()` 函数未包含 cluster |
| compute_ctl 启动时 | **随机** | ConfigMap 没有提供，GET /spec 失败 |

核心矛盾：**Deployment labels 有正确的 tenant_id，但 compute_ctl 不读 label，它只通过 HTTP 与控制面交互。控制面 `/spec` 接口又依赖外部的 storage-controller HTTP API，一旦该 API 不可达就失败。**

### 2.3 为什么 storage-controller API 偶尔不可达

- Pod 启动时序：compute pod 和控制面 pod 几乎同时启动，此时 storage-controller Service 的 DNS 解析可能尚未在 Pod 内生效
- 网络抖动：Kubernetes Service iptables 规则同步延迟
- storage-controller 自身重启或繁忙

---

## 3. 设计方案

### 3.1 总体思路

两个互补的修改，共同确保 compute_ctl 在任何情况下都能获取正确的 `tenant_id`：

| 修改点 | 策略 | 目标 |
|--------|------|------|
| **ConfigMap `spec.json`** | 静态注入 `cluster.cluster_id` | compute_ctl 启动就有正确 ID，不依赖网络 |
| **`GenerateComputeSpec` 降级** | storage-controller 不可达时返回最小配置 | `/spec` 接口不再因外部依赖失败而 500 |

### 3.2 修改点一：ConfigMap 注入 cluster 配置

#### 3.2.1 当前 ConfigMap 结构

```json
{
  "format_version": "1.0",
  "compute_ctl_config": {
    "jwks": { "keys": [...] }
  }
}
```

#### 3.2.2 目标 ConfigMap 结构

```json
{
  "format_version": "1.0",
  "cluster": {
    "cluster_id": "a454c1a8ec748404629288934a364acb",
    "name": "my-project",
    "roles": [],
    "databases": [],
    "settings": []
  },
  "compute_ctl_config": {
    "jwks": { "keys": [...] }
  }
}
```

#### 3.2.3 实现细节

**文件**: `specs/compute/configmap.go` → 函数 `ConfigMap()`

改动内容：

1. 新增 `clusterConfig` 结构体（最小必要字段）
2. `computeSpec` 结构体增加 `Cluster clusterConfig` 字段
3. 从 `project.Spec.TenantID` 和 `project.Name` 构建 cluster 配置
4. `roles` / `databases` / `settings` 初始化为空数组（完整值由 `/spec` 接口在后续提供）

```go
// 新增结构体
type clusterConfig struct {
    ClusterID string        `json:"cluster_id"`
    Name      string        `json:"name"`
    Roles     []interface{} `json:"roles"`
    Databases []interface{} `json:"databases"`
    Settings  []interface{} `json:"settings"`
}

// computeSpec 增加 Cluster 字段
type computeSpec struct {
    FormatVersion     string           `json:"format_version"`
    Cluster           clusterConfig    `json:"cluster"`
    ComputeCtlConfig  computeCtlConfig `json:"compute_ctl_config"`
}
```

#### 3.2.4 安全性说明

- `tenant_id` 在 Project CR 中是 **immutable** 的（一旦生成就不再变化），放入静态 ConfigMap 是安全的
- `roles` / `databases` / `settings` 仅提供空数组占位，不会与动态配置产生冲突
- `timeline_id` 不放入 ConfigMap，因为它由 compute_ctl 通过 `/spec` 动态获取（如果 `/spec` 成功的话，首次调用提供完整值）

### 3.3 修改点二：GenerateComputeSpec 降级处理

#### 3.3.1 当前逻辑 (request == nil 分支)

```go
// 当前: GetTenantInfo 失败 → 直接 return error → /spec 返回 500
storageClient := NewStorageControllerClient(clusterName)
tenantInfo, err := storageClient.GetTenantInfo(ctx, log, tenantID)
if err != nil {
    log.Error("Failed to retrieve tenant info", ...)
    return nil, err    // ← compute_ctl 收不到 spec → 随机生成 ID
}
```

#### 3.3.2 目标逻辑

```go
storageClient := NewStorageControllerClient(clusterName)
tenantInfo, err := storageClient.GetTenantInfo(ctx, log, tenantID)
if err != nil {
    // storage-controller 暂时不可达，使用最小默认单 shard 配置
    log.Warn("Failed to retrieve tenant info, using default single-shard config", ...)
    actualRequest = &ComputeHookNotifyRequest{
        TenantID:   tenantID,
        Shards:     []ComputeHookNotifyRequestShard{{NodeID: 0, ShardNumber: 0}},
    }
} else {
    // 正常路径
    log.Info("Retrieved tenant info", ...)
    // ... 原有逻辑
}
```

#### 3.3.3 降级行为说明

| 场景 | 行为 |
|------|------|
| storage-controller 正常 | 行为不变，从 API 获取完整 shard 信息 |
| storage-controller 暂时不可达 | 返回单 shard 默认配置 (node_id=0)，保证 tenant_id 正确 |
| storage-controller 永久不可达 | compute_ctl 仍能获取正确 spec → `notify-attach` 成功 |
| storage-controller 恢复后 | `/notify-attach` 会携带正确的 shard 信息重新推送配置 |

#### 3.3.4 为什么 NodeID=0 是安全的

- `NodeID=0` 对应 pageserver-0，在标准部署中始终存在
- 真正的 shard 信息会在 storage-controller 恢复后通过 `/notify-attach` 回调重新推送完整配置
- 这是一个**启动时保底**策略，不影响正常运行后的正确性

---

## 4. 修改清单

### 4.1 文件变更

| 文件 | 函数 | 变更类型 | 说明 |
|------|------|----------|------|
| `specs/compute/configmap.go` | `ConfigMap()` | 修改 | `computeSpec` 增加 `Cluster clusterConfig` 字段，注入 `cluster_id` |
| `specs/compute/spec.go` | `GenerateComputeSpec()` | 修改 | `GetTenantInfo` 失败时降级到默认单 shard 配置，不返回 error |

### 4.2 不涉及的文件

| 文件 | 原因 |
|------|------|
| `internal/controlplane/routes.go` | 无需修改，`handleComputeSpec` 和 `notifyAttach` 的错误处理逻辑不变 |
| `internal/controller/branch_create.go` | 无需修改，`reconcileConfigMap` 使用 `DeepDerivative` 自动检测 ConfigMap 数据变更并更新 |
| `specs/compute/deployment.go` | 无需修改，Deployment 的 labels 和 annotations 已经正确 |
| 任何 CRD 类型定义 | 无需修改，项目逻辑不变 |

---

## 5. 数据流变化对比

### 5.1 修改前 (当前)

```
compute_ctl 启动
  │
  ▼
ConfigMap (无 cluster 配置)
  │
  ▼
GET /spec ──→ GenerateComputeSpec
                  │
                  ├─ GetTenantInfo(storage-controller)
                  │     │
                  │     ├─ 成功 → spec 含正确 ID → notify-attach 成功 ✔
                  │     └─ 失败 → 500 Error → compute_ctl 随机生成 ID ✘ (死循环)
                  │
                  └─ 404 Not Found → compute_ctl 随机生成 ID ✘
```

### 5.2 修改后 (目标)

```
compute_ctl 启动
  │
  ▼
ConfigMap (含 cluster.cluster_id)
  │                            
  │  compute_ctl 已经知道正确的 tenant_id
  │
  ▼
GET /spec ──→ GenerateComputeSpec
                  │
                  ├─ GetTenantInfo(storage-controller)
                  │     │
                  │     ├─ 成功 → spec 含完整 shard 信息 ✔
                  │     └─ 失败 → 降级到单 shard 默认配置 ✔ (tenant_id 正确)
                  │
                  └─ 即使整体失败 → ConfigMap 已提供 cluster_id，compute_ctl 不随机生成 ✔
```

---

## 6. 兼容性分析

### 6.1 向后兼容

| 维度 | 评估 |
|------|------|
| ConfigMap 结构变更 | `DeepDerivative` 检测到差异后自动 Patch 更新，不影响现有 pod |
| `GenerateComputeSpec` 行为 | 仅 `GetTenantInfo` 失败路径变化，成功路径无影响 |
| compute_ctl 兼容性 | `cluster.cluster_id` 是上游 Neon 标准字段，compute_ctl 原生支持 |
| 现有集群升级 | 旧 ConfigMap 被自动更新为包含 cluster 配置的版本 |

### 6.2 边界情况

| 场景 | 预期行为 |
|------|----------|
| Project CR 的 tenantId 为空字符串 `""` | `ConfigMap()` 不变（tenantId 为空时 Controller 会在 reconcile 时生成），与现有逻辑一致 |
| 多次 pod 删除 | tenant_id 始终从 ConfigMap 获取，不变 |
| storage-controller 永久不可用 | compute pod 仍能正常运行（ConfigMap 提供正确 ID，降级逻辑提供最小 shard 配置） |

---

## 7. 测试计划

### 7.1 单元测试

| 测试用例 | 覆盖范围 |
|----------|----------|
| `ConfigMap()` 生成的 JSON 包含 `cluster.cluster_id` | `configmap_test.go` |
| `ConfigMap()` 中 `cluster_id` 等于 `project.Spec.TenantID` | `configmap_test.go` |
| `GenerateComputeSpec` request=nil + GetTenantInfo 成功 → 返回完整 spec | `spec_test.go` |
| `GenerateComputeSpec` request=nil + GetTenantInfo 失败 → 降级返回默认 shard | `spec_test.go` |
| 降级返回的 spec 中 `TenantID` 与 Deployment label 一致 | `spec_test.go` |

### 7.2 集成测试

1. **正常启动**：完整集群部署后，compute pod 正常运行
2. **Pod 删除**：`kubectl delete pod` 后，新 pod 使用相同 tenant_id 启动成功
3. **无 storage-controller**：模拟 storage-controller Service 不可达，compute pod 仍能启动
4. **ConfigMap 热更新**：修改代码部署后，已有 ConfigMap 被自动更新

---

## 8. 部署步骤

```bash
# 1. 构建新镜像
make docker-build IMG=neon-operator:latest

# 2. 推送镜像 (根据环境调整)
make docker-push IMG=neon-operator:latest

# 3. 部署新版本
make deploy IMG=neon-operator:latest

# 4. 等待 controller 启动并就绪
kubectl wait --for=condition=ready pod -l app=neon-controller-manager -n neon --timeout=120s

# 5. 验证 ConfigMap 已更新 (应包含 cluster.cluster_id)
kubectl get configmap -n neon main-branch-compute-spec \
  -o jsonpath='{.data.spec\.json}' | python3 -m json.tool | head -15

# 6. 触发 compute pod 重建
kubectl delete pods -n neon -l molnett.org/component=compute

# 7. 验证新 pod 启动成功
kubectl wait --for=condition=ready pod -l molnett.org/component=compute -n neon --timeout=300s
```

---

## 9. 附录

### 9.1 相关代码引用

| 组件 | 文件路径 |
|------|----------|
| ConfigMap 生成 | `specs/compute/configmap.go` → `ConfigMap()` |
| Spec 生成 | `specs/compute/spec.go` → `GenerateComputeSpec()` |
| Deployment 生成 | `specs/compute/deployment.go` → `Deployment()` |
| HTTP 路由 | `internal/controlplane/routes.go` |
| Reconciler | `internal/controller/branch_create.go` → `reconcileConfigMap()` |
| StorageController 客户端 | `specs/storagecontroller/names.go` → `URL()`<br>`specs/compute/spec.go` → `GetTenantInfo()` |
| Project CR 类型 | `api/v1alpha1/project_types.go` |
| Branch CR 类型 | `api/v1alpha1/branch_types.go` |

### 9.2 compute_ctl 启动参数

```bash
compute_ctl \
  --pgdata /.neon/data/pgdata \
  --connstr=postgresql://cloud_admin:@0.0.0.0:55433/postgres \
  --compute-id {branch.Name} \
  -p http://neon-controlplane.neon:8081 \
  --pgbin /usr/local/bin/postgres
```

- `INITIAL_SPEC_JSON` 环境变量被写入 `/var/spec.json` 供 compute_ctl 读取
- `-p` 参数指向控制面地址，compute_ctl 通过此地址调用 `/spec` 和 `/notify-attach`
