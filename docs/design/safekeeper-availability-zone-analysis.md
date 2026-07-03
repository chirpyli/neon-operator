# Safekeeper 可用区（AZ）问题调研分析

> **状态**: 调研完成
> **日期**: 2026-06-26

---

## 1. 当前实现概览

当前代码中，`availability_zone_id` 的获取链路如下：

```
RegisterSafekeeper()
  └─ getNodeAvailabilityZone()
       ├─ 通过 k8sClient.Get 读取 Pod {safekeeper.Name}-0
       ├─ 检查 Pod.Spec.NodeName 是否为空
       ├─ 通过 nonCachedReader.Get 读取 Node 对象
       └─ 读取 node.Labels["topology.kubernetes.io/zone"]
            └─ 若不存在 → 返回 "unknown"
```

发送给 Storage Controller 的请求体结构（`internal/controller/sc_client.go`）：

```go
type safekeeperUpsertRequest struct {
    ID                 int64  `json:"id"`
    RegionID           string `json:"region_id"`
    Version            int64  `json:"version"`
    Host               string `json:"host"`
    Port               int32  `json:"port"`
    HTTPPort           int32  `json:"http_port"`
    AvailabilityZoneID string `json:"availability_zone_id"`
}
```

`RegisterSafekeeper` 当前仅在 `createSafekeeperResources` 中调用一次（best-effort）。

---

## 2. 识别出的问题

### 问题 1：`"unknown"` 作为 AZ 值发给 SC（🟡 中风险）

四种情况都会返回 `"unknown"` 字符串发给 SC：

- Pod 尚未创建
- Pod 未调度（`NodeName` 为空）
- Node 读取失败
- Node 无 `topology.kubernetes.io/zone` 标签

**分析**：SC 的 `SafekeeperUpsert.availability_zone_id` 是 `String` 类型（非 `Option<String>`），不接受 NULL，因此发 `"unknown"` 作为降级值本身是合理的。但关键问题是：

> 如果集群**后来添加了拓扑标签**，或者 Pod **被重新调度到有 AZ 标签的节点**，SC 中的 `availability_zone_id` 会不会更新？

**当前行为**：`RegisterSafekeeper` 仅在创建阶段调用一次。之后如果 Pod 漂移或 AZ 信息变更，**不会触发重新注册**，SC 中的 AZ 信息可能过时。

**建议**：在 `reconcile` 的 `Ready` 分支中也调用 `RegisterSafekeeper`（利用 upsert 的幂等性），确保 AZ 变更时能同步到 SC。

---

### 问题 2：Pod 漂移后 AZ 变更不会更新 SC（🟡 中风险）

设计文档 `safekeeper-production.md` 详细分析了「节点故障重调度」场景，结论是：

> host（headless service DNS）不变 → 不需要重新调用 upsert

**但这个结论忽略了 `availability_zone_id` 的变化**。如果 Pod 从 `node-a`（az-a）漂移到 `node-b`（az-b）：

| 参数 | 漂移前 | 漂移后 | 需要更新？ |
|------|--------|--------|:---------:|
| host（headless service DNS） | `sk-0.sk-hs.ns.svc` | `sk-0.sk-hs.ns.svc` | 不变 ✅ |
| port | `5454` | `5454` | 不变 ✅ |
| http_port | `7676` | `7676` | 不变 ✅ |
| **availability_zone_id** | `az-a` | `az-b` | **变化 ⚠️** |

**SC 侧的后果**：SC 使用 `availability_zone_id` 来做 safekeeper 的调度决策（anti-affinity）。如果 AZ 信息不正确，SC 可能做出错误的调度决策（例如认为两个 safekeeper 在同一 AZ，实际上已经分离）。

**核心结论**：设计文档的结论需要修正 — 不是「完全不需要重新 upsert」，而是「host/port 不变时 upsert 是幂等的，可以安全重复调用」。应在 `reconcile` 的 `Ready` 分支也调用 `RegisterSafekeeper`，利用幂等性来自动修正 AZ 变化。

---

### 问题 3：`topology.kubernetes.io/zone` 标签的兼容性（🟢 低风险）

Kubernetes 标准拓扑标签有两个版本：

| 标签 | 状态 |
|------|------|
| `topology.kubernetes.io/zone` | 推荐（K8s 1.17+） |
| `failure-domain.beta.kubernetes.io/zone` | 已弃用（某些旧集群仍在使用） |

**当前代码**只读取 `topology.kubernetes.io/zone`，如果集群使用旧标签或自定义标签，会返回 `"unknown"`。

**建议**：增加 fallback 标签读取：

```go
// 优先级：标准标签 > 旧版 beta 标签
for _, label := range []string{
    "topology.kubernetes.io/zone",
    "failure-domain.beta.kubernetes.io/zone",
} {
    if az, ok := node.Labels[label]; ok {
        return az
    }
}
```

---

### 问题 4：`"unknown"` 字符串在 SC 侧的处理语义（🟡 中风险）

设计文档中的 API 示例显示了合法的 AZ 值：

```json
"availability_zone_id": "az-a"
```

SC 的 Rust 代码中 `availability_zone_id: String`（非 `Option<String>`），意味着 SC 期望该字段始终有值。

当前发 `"unknown"` 是合理的降级值，但需要确认 SC 侧的处理逻辑：

- **期望行为**：SC 将 `"unknown"` 视为「无 AZ 信息」并跳过该 safekeeper 的 anti-affinity 计算。
- **风险行为**：SC 将 `"unknown"` 视为一个合法的 AZ 名称，把所有 AZ 未知的 safekeeper 放在同一组。

**如果 SC 把 `"unknown"` 当成合法 AZ**，多个 safekeeper 都会被分配 `availability_zone_id="unknown"`，导致 SC 认为它们在同一个 AZ，从而破坏高可用调度（anti-affinity 失效）。

**建议**：确认 SC 侧源码对 `"unknown"` 的处理方式。如果存在问题，可考虑改为空字符串或预定义的哨兵值。

---

## 3. 问题汇总

| # | 问题 | 严重度 | 根因 | 建议修复 |
|---|------|:---:|------|------|
| 1 | `"unknown"` 发给 SC 后，如果集群后来配置了 AZ 标签，SC 不会更新 | 🟡 中 | `RegisterSafekeeper` 只在创建时调用一次 | 在 `reconcile` 的 `Ready` 分支也调用 upsert（幂等） |
| 2 | Pod 漂移后 AZ 变化不更新 SC | 🟡 中 | 同上 | 同上（在 reconcile 中定期 upsert） |
| 3 | 仅读取标准标签，不兼容旧集群 | 🟢 低 | 未做 fallback | 增加 `failure-domain.beta.kubernetes.io/zone` fallback |
| 4 | `"unknown"` 可能被 SC 视为合法 AZ | 🟡 中 | SC 侧行为不明确 | 确认 SC 对 `"unknown"` 的处理；考虑使用空字符串或特殊值 |

---

## 4. 后续行动

1. **确认 SC 侧 `availability_zone_id` 的语义**：查看 SC 源码确认该字段是否用于 anti-affinity 调度决策，以及 `"unknown"` 如何处理。
2. **实施修复 1 & 2**：在 safekeeper controller 的 `Ready` 分支中增加 `RegisterSafekeeper` 调用，利用 upsert 幂等性同步 AZ。
3. **实施修复 3**：增加旧版拓扑标签的兼容读取。
4. **回归验证**：验证 Pod 漂移后 SC 中 AZ 是否会正确更新。
