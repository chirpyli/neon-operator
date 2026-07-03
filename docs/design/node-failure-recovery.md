# 节点故障自动恢复机制设计文档

## 1. 问题描述

在生产环境中，当 Kubernetes 节点发生故障（如节点宕机、网络隔离等）时，Neon Operator 当前存在以下问题：

| 问题 | 位置 | 影响 |
|------|------|------|
| Pod 卡在 Terminating 状态 | Kubernetes StatefulSet | 无法创建新 Pod |
| SC 节点仍标记为 Active | Storage Controller Heartbeater | 可能继续向不可用节点分配 shard |
| Operator 不处理 Terminating 状态 | pageserver_controller.go | 故障检测失效 |
| Safekeeper 无故障恢复机制 | safekeeper_controller.go | 节点故障后无法自动恢复 |

### 1.1 问题根因分析

#### 1.1.1 Pod 卡在 Terminating 状态

当节点变为 NotReady 后，Kubernetes 会尝试删除该节点上的 Pod，但由于节点不可达，Kubelet 无法执行删除操作，导致 Pod 卡在 Terminating 状态。

#### 1.1.2 StatefulSet 无法创建新 Pod

StatefulSet 的控制器逻辑是：必须先删除旧 Pod（状态变为 Terminated），才能创建新的 Pod。由于旧 Pod 卡在 Terminating 状态，新 Pod 无法创建。

#### 1.1.3 SC 节点仍 Active

Storage Controller 通过 heartbeater 定期向 pageserver/safekeeper 发送心跳请求来检测节点可用性。当节点挂掉但容器进程仍在运行（网络隔离场景），pageserver 仍能响应心跳，SC 会认为节点仍为 Active。

即使节点完全宕机，SC 的 `max_offline_interval` 默认值为 30 秒，在这段时间内节点仍被视为可用。

#### 1.1.4 Operator 故障检测失效

当前 `handleNodeFailure` 函数只处理两种场景：
- Pod 处于 Pending 状态（volume node affinity conflict）
- Pod 处于 Running 状态但 Ready=False

当节点挂掉时，Pod 通常处于以下状态：
- Terminating（Kubernetes 尝试删除）
- Running 但 Ready=False（节点 NotReady）
- Failed（容器崩溃）
- Unknown（节点不可达）

原逻辑无法处理 Terminating、Failed、Unknown 状态，导致故障检测失效。

#### 1.1.5 Safekeeper 无故障恢复机制

Safekeeper 控制器当前没有实现任何节点故障检测与恢复逻辑。

### 1.2 故障场景时序图

```
时间轴
  │
  │ 节点正常运行
  │  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
  │  │  Node1      │    │  Node2      │    │  Node3      │
  │  │  PS1 + SK1  │    │  PS2 + SK2  │    │  PS3 + SK3  │
  │  └──────┬──────┘    └──────┬──────┘    └──────┬──────┘
  │         │                  │                  │
  ▼         ▼                  ▼                  ▼
T1├────────────────────────────────────────────────────────
  │  节点 Node1 发生故障（宕机/网络隔离）
  │         │
  │         ▼
  │  K8s 标记 Node1 NotReady
  │  K8s 开始删除 Node1 上的 Pod
  │         │
  │         ▼
  │  Pod 进入 Terminating 状态
  │  ┌─────────────┐
  │  │  PS1 Pod    │◄─── 卡在 Terminating
  │  │  SK1 Pod    │    无法完成删除
  │  └─────────────┘
  │         │
  │         ▼
T2├────────────────────────────────────────────────────────
  │  Operator 检测到故障
  │  (当前逻辑：无法检测 Terminating 状态)
  │         │
  │         ▼
  │  StatefulSet 无法创建新 Pod
  │  (等待旧 Pod Terminated)
  │         │
  │         ▼
T3├────────────────────────────────────────────────────────
  │  SC heartbeater 继续检测
  │  (如果容器仍运行，节点仍为 Active)
  │  SC 可能继续向不可用节点分配 shard
  │         │
  │         ▼
  │  服务中断持续...
```

## 2. 设计目标

本设计方案旨在实现以下目标：

1. **自动检测节点故障**：支持检测 Pod 的 Terminating、Failed、Unknown 状态，以及节点 NotReady 状态
2. **自动触发故障恢复**：删除故障 Pod 和 PVC，触发 StatefulSet 在新节点上重建
3. **同步 SC 节点状态**：在触发恢复前，将故障节点在 SC 中标记为 Pause，防止继续分配 shard
4. **Safekeeper 故障恢复**：为 Safekeeper 添加与 Pageserver 类似的故障检测与恢复逻辑
5. **生产级可用性**：提供可配置的超时阈值、恢复策略，支持手动干预和强制恢复
6. **兼容性**：保持与现有 API 的向后兼容性

## 3. 解决方案设计

### 3.1 整体架构

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Kubernetes Cluster                           │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌──────────┐      ┌──────────┐      ┌──────────┐                   │
│  │  Node1   │      │  Node2   │      │  Node3   │                   │
│  │ (故障)   │      │ (正常)   │      │ (正常)   │                   │
│  └────┬─────┘      └────┬─────┘      └────┬─────┘                   │
│       │                 │                 │                          │
│       ▼                 ▼                 ▼                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                  │
│  │ PS1 Pod     │  │ PS2 Pod     │  │ PS3 Pod     │                  │
│  │ Terminating │  │ Running     │  │ Running     │                  │
│  └─────┬───────┘  └─────┬───────┘  └─────┬───────┘                  │
│        │                │                │                           │
│        │                │                │                           │
│        ▼                ▼                ▼                           │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │              Neon Operator Controller                        │    │
│  │                                                             │    │
│  │  ┌─────────────────────────────────────────────────────┐    │    │
│  │  │           handleNodeFailure 函数                     │    │    │
│  │  │                                                     │    │    │
│  │  │  ┌──────────┐  ┌──────────┐  ┌──────────────────┐   │    │    │
│  │  │  │Pending   │  │Running   │  │Terminating/Failed│   │    │    │
│  │  │  │检测      │  │检测      │  │/Unknown 检测     │   │    │    │
│  │  │  └────┬─────┘  └────┬─────┘  └────────┬─────────┘   │    │    │
│  │  │       │             │                 │               │    │    │
│  │  │       └──────┬──────┴─────────────────┘               │    │    │
│  │  │              ▼                                        │    │    │
│  │  │  ┌───────────────────────────────────────────┐       │    │    │
│  │  │  │         triggerRecovery                   │       │    │    │
│  │  │  │                                           │       │    │    │
│  │  │  │  1. SC: 将节点标记为 Pause                │       │    │    │
│  │  │  │  2. 删除故障 Pod                          │       │    │    │
│  │  │  │  3. 删除绑定的 PVC                        │       │    │    │
│  │  │  │  4. StatefulSet 创建新 Pod 在新节点上      │       │    │    │
│  │  │  └───────────────────────────────────────────┘       │    │    │
│  │  └─────────────────────────────────────────────────────┘    │    │
│  └─────────────────────────────────────────────────────────────┘    │
│                              │                                       │
│                              ▼                                       │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │               Storage Controller (SC)                        │    │
│  │                                                             │    │
│  │  ┌─────────────────────────────────────────────────────┐    │    │
│  │  │            Heartbeater 模块                         │    │    │
│  │  │                                                     │    │    │
│  │  │  PS1: Pause (Operator 设置)                         │    │    │
│  │  │  PS2: Active                                        │    │    │
│  │  │  PS3: Active                                        │    │    │
│  │  └─────────────────────────────────────────────────────┘    │    │
│  └─────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.2 API 设计

#### 3.2.1 Pageserver API 扩展

Pageserver 已支持 `NodeFailure` 配置，无需修改：

```go
type NodeFailureRecoveryConfig struct {
    AutoRecover bool `json:"autoRecover,omitempty"`
    MaxPendingDuration *metav1.Duration `json:"maxPendingDuration,omitempty"`
}

type PageserverSpec struct {
    // ... 其他字段
    NodeFailure *NodeFailureRecoveryConfig `json:"nodeFailure,omitempty"`
}
```

#### 3.2.2 Safekeeper API 扩展

为 Safekeeper 添加 `NodeFailure` 配置：

```go
type SafekeeperSpec struct {
    ID uint32 `json:"id"`
    Cluster string `json:"cluster"`
    StorageConfig StorageConfig `json:"storageConfig"`
    
    // 新增：节点故障自动恢复配置
    NodeFailure *NodeFailureRecoveryConfig `json:"nodeFailure,omitempty"`
    
    LivenessProbe *ProbeConfig `json:"livenessProbe,omitempty"`
    ReadinessProbe *ProbeConfig `json:"readinessProbe,omitempty"`
    StartupProbe *ProbeConfig `json:"startupProbe,omitempty"`
}
```

#### 3.2.3 配置示例

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Pageserver
metadata:
  name: my-cluster-pageserver-1
spec:
  id: 1
  cluster: my-cluster
  storageConfig:
    size: 10Gi
  nodeFailure:
    autoRecover: true           # 启用自动故障恢复
    maxPendingDuration: 5m      # 故障检测阈值

---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: my-cluster-safekeeper-1
spec:
  id: 1
  cluster: my-cluster
  storageConfig:
    size: 10Gi
  nodeFailure:
    autoRecover: true
    maxPendingDuration: 5m
```

### 3.3 故障检测逻辑设计

#### 3.3.1 扩展 handleNodeFailure 函数

扩展 `handleNodeFailure` 函数，支持检测以下状态：

| Pod 状态 | 检测条件 | 恢复策略 |
|----------|----------|----------|
| Pending | 存在 volume node affinity conflict，且超过阈值 | 删除 PVC + Pod |
| Running | Pod Ready=False 且所在节点 NotReady，超过阈值 | 删除 PVC + Pod |
| Terminating | DeletionTimestamp 非空，且超过阈值 | 强制删除 Pod + 删除 PVC |
| Failed | Pod 状态为 Failed | 删除 PVC + Pod |
| Unknown | Pod 状态为 Unknown，且所在节点 NotReady | 删除 PVC + Pod |

#### 3.3.2 检测流程

```
handleNodeFailure(ps)
    │
    ├─ 检查 NodeFailure.AutoRecover 是否启用
    │
    ├─ 获取 Pod 状态
    │
    ├─ 根据 Pod 状态分发处理：
    │   │
    │   ├─ Pending → handlePendingPodFailure
    │   │
    │   ├─ Running → handleRunningPodFailure
    │   │
    │   ├─ Terminating → handleTerminatingPodFailure
    │   │
    │   ├─ Failed → handleFailedPodFailure
    │   │
    │   └─ Unknown → handleUnknownPodFailure
    │
    └─ 返回 (requeue, error)
```

#### 3.3.3 新增处理函数

**handleTerminatingPodFailure**

检测 Pod 是否卡在 Terminating 状态超过阈值：

```go
func (r *PageserverReconciler) handleTerminatingPodFailure(
    ctx context.Context, ps *neonv1alpha1.Pageserver, 
    pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
    
    // 计算 Terminating 持续时间
    terminatingDuration := time.Since(pod.DeletionTimestamp.Time)
    
    // 检查是否超过阈值
    if terminatingDuration < threshold {
        log.Info("Pod Terminating 时间未超过阈值", 
            "pod", podName, "duration", terminatingDuration)
        return false, nil
    }
    
    // 检查所在节点状态（可选，但推荐）
    if pod.Spec.NodeName != "" {
        node := &corev1.Node{}
        if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err == nil {
            // 节点 NotReady 时更确定是节点故障
            for _, c := range node.Status.Conditions {
                if c.Type == corev1.NodeReady && c.Status == corev1.ConditionFalse {
                    log.Info("Pod Terminating 且节点 NotReady，触发自动恢复",
                        "pod", podName, "node", pod.Spec.NodeName)
                    return r.triggerRecovery(ctx, ps, pod, podName, 
                        "NodeNotReady", 
                        fmt.Sprintf("节点 %s NotReady，Pod Terminating %v", 
                            pod.Spec.NodeName, terminatingDuration))
                }
            }
        }
    }
    
    // 即使节点状态未知，Terminating 超过阈值也触发恢复
    log.Info("Pod 卡在 Terminating 状态超过阈值，触发自动恢复",
        "pod", podName, "duration", terminatingDuration)
    
    return r.triggerRecovery(ctx, ps, pod, podName, 
        "PodTerminatingStuck",
        fmt.Sprintf("Pod Terminating %v，正在删除 PVC 以触发重建", terminatingDuration))
}
```

**handleFailedPodFailure**

处理 Failed 状态的 Pod：

```go
func (r *PageserverReconciler) handleFailedPodFailure(
    ctx context.Context, ps *neonv1alpha1.Pageserver, 
    pod *corev1.Pod, podName string) (bool, error) {
    
    log.Info("Pod 处于 Failed 状态，触发自动恢复", "pod", podName)
    
    return r.triggerRecovery(ctx, ps, pod, podName,
        "PodFailed",
        fmt.Sprintf("Pod %s Failed，正在删除 PVC 以触发重建", podName))
}
```

**handleUnknownPodFailure**

处理 Unknown 状态的 Pod（通常表示节点不可达）：

```go
func (r *PageserverReconciler) handleUnknownPodFailure(
    ctx context.Context, ps *neonv1alpha1.Pageserver, 
    pod *corev1.Pod, podName string, threshold time.Duration) (bool, error) {
    
    // Unknown 状态通常意味着节点不可达
    if pod.Spec.NodeName == "" {
        return false, nil
    }
    
    node := &corev1.Node{}
    if err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
        log.Info("无法获取节点状态", "node", pod.Spec.NodeName)
        return false, nil
    }
    
    nodeReady := false
    var nodeNotReadySince time.Time
    for _, c := range node.Status.Conditions {
        if c.Type == corev1.NodeReady {
            nodeReady = c.Status == corev1.ConditionTrue
            if !nodeReady && c.LastTransitionTime.IsZero() {
                nodeNotReadySince = pod.CreationTimestamp.Time
            } else if !nodeReady {
                nodeNotReadySince = c.LastTransitionTime.Time
            }
            break
        }
    }
    
    if nodeReady {
        return false, nil
    }
    
    nodeNotReadyDuration := time.Since(nodeNotReadySince)
    if nodeNotReadyDuration < threshold {
        log.Info("节点 NotReady 时间未超过阈值", 
            "node", pod.Spec.NodeName, "duration", nodeNotReadyDuration)
        return false, nil
    }
    
    log.Info("Pod Unknown 且节点 NotReady 超过阈值，触发自动恢复",
        "pod", podName, "node", pod.Spec.NodeName, "duration", nodeNotReadyDuration)
    
    return r.triggerRecovery(ctx, ps, pod, podName,
        "NodeNotReady",
        fmt.Sprintf("节点 %s NotReady %v，Pod Unknown，正在删除 PVC 以触发重建", 
            pod.Spec.NodeName, nodeNotReadyDuration))
}
```

### 3.4 故障恢复流程设计

#### 3.4.1 triggerRecovery 函数扩展

扩展 `triggerRecovery` 函数，在删除 Pod 和 PVC 之前，先将节点在 SC 中标记为 Pause：

```go
func (r *PageserverReconciler) triggerRecovery(
    ctx context.Context, ps *neonv1alpha1.Pageserver, 
    pod *corev1.Pod, podName, reason, message string) (bool, error) {
    
    log := logf.FromContext(ctx)
    
    // 更新状态，标记恢复中
    _ = utils.PatchStatus(ctx, r.Client, ps, func(p *neonv1alpha1.Pageserver) {
        utils.SetCondition(p, p.StatusConditions(), utils.ConditionNodeRecoveryInProgress,
            metav1.ConditionTrue, reason, message)
    })
    
    // 步骤1：将节点在 SC 中标记为 Pause，防止继续分配 shard
    if r.SCClient != nil {
        clusterName := ps.Spec.Cluster
        nodeID := ps.Spec.ID
        
        // 获取当前调度策略
        node, err := r.SCClient.GetNode(ctx, clusterName, ps.Namespace, nodeID)
        if err == nil && node.Scheduling != "Pause" && node.Scheduling != "Deleting" {
            // 只有非 Pause/Deleting 状态才需要设置
            pausePolicy := "Pause"
            if err := r.SCClient.ConfigureNode(ctx, clusterName, ps.Namespace, nodeID, nil, &pausePolicy); err != nil {
                log.Info("ConfigureNode(Pause) 失败，继续执行恢复", "error", err)
                // 不中断恢复流程
            } else {
                log.Info("已将节点在 SC 中标记为 Pause", "nodeID", nodeID)
            }
        }
    }
    
    // 步骤2：强制删除 Pod（使用 foreground 或 background 模式）
    deleteOptions := &client.DeleteOptions{
        GracePeriodSeconds: ptr.To(int64(0)), // 强制删除
    }
    
    if err := r.Delete(ctx, pod, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
        log.Error(err, "删除 Pod 失败")
        return true, err
    }
    log.Info("已删除 Pod", "pod", podName)
    
    // 步骤3：删除绑定的 PVC
    pvcName := pageserverspec.Name(ps) + "-" + storageVolumeName + "-0"
    pvc := &corev1.PersistentVolumeClaim{}
    if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: ps.Namespace}, pvc); err == nil {
        if err := r.Delete(ctx, pvc); err != nil {
            log.Error(err, "删除 PVC 失败")
            return true, err
        }
        log.Info("已删除 PVC，StatefulSet 将创建新 PVC 在新节点上", "pvc", pvcName)
    }
    
    return true, nil
}
```

#### 3.4.2 Safekeeper 故障恢复

为 Safekeeper 添加类似的故障检测与恢复逻辑：

```go
func (r *SafekeeperReconciler) handleNodeFailure(ctx context.Context, sk *neonv1alpha1.Safekeeper) (bool, error) {
    if sk.Spec.NodeFailure == nil || !sk.Spec.NodeFailure.AutoRecover {
        return false, nil
    }
    
    threshold := nodeFailurePendingThreshold
    if sk.Spec.NodeFailure.MaxPendingDuration != nil {
        threshold = sk.Spec.NodeFailure.MaxPendingDuration.Duration
    }
    
    podName := safekeeperspec.Name(sk) + "-0"
    pod := &corev1.Pod{}
    err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: sk.Namespace}, pod)
    if err != nil {
        if apierrors.IsNotFound(err) {
            return false, nil
        }
        return false, fmt.Errorf("获取 pod: %w", err)
    }
    
    switch pod.Status.Phase {
    case corev1.PodPending:
        return r.handlePendingPodFailure(ctx, sk, pod, podName, threshold)
    case corev1.PodRunning:
        return r.handleRunningPodFailure(ctx, sk, pod, podName, threshold)
    case corev1.PodFailed:
        return r.handleFailedPodFailure(ctx, sk, pod, podName)
    case corev1.PodUnknown:
        return r.handleUnknownPodFailure(ctx, sk, pod, podName, threshold)
    }
    
    if pod.DeletionTimestamp != nil {
        return r.handleTerminatingPodFailure(ctx, sk, pod, podName, threshold)
    }
    
    return false, nil
}
```

### 3.5 SC 状态同步机制

在触发恢复前，Operator 需要将故障节点在 SC 中标记为 Pause，防止 SC 继续向该节点分配 shard。

#### 3.5.1 SC API 调用

使用 `ConfigureNode` API 将节点调度策略设置为 Pause：

```
PUT /control/v1/node/:id/configure
{
    "scheduling_policy": "Pause"
}
```

#### 3.5.2 状态同步流程

```
Operator                    Storage Controller
    │                             │
    │  检测到节点故障              │
    │                             │
    │  ── ConfigureNode(Pause) ──►│
    │                             │
    │  ◄── 成功/失败 ──           │
    │                             │
    │  删除 Pod                   │
    │                             │
    │  删除 PVC                   │
    │                             │
    │  StatefulSet 创建新 Pod      │
    │                             │
    │  新 Pod 注册到 SC            │
    │  ── RegisterNode ──────────►│
    │                             │
    │  ◄── 设置为 Active ──       │
    │                             │
```

### 3.6 安全考虑与边界条件

#### 3.6.1 误判防护

- **阈值机制**：设置合理的默认阈值（5 分钟），避免因短暂网络抖动触发恢复
- **节点状态确认**：检测 Pod 状态时，同时检查节点状态，减少误判
- **手动禁用**：支持通过 `autoRecover: false` 禁用自动恢复

#### 3.6.2 数据安全性

- **SC 状态同步**：在删除 Pod 前先将节点标记为 Pause，确保 SC 不会继续向该节点分配 shard
- **数据迁移**：对于 Pageserver，数据存储在 S3 bucket 中，新 Pod 启动后会从 S3 恢复数据
- **Safekeeper 数据**：Safekeeper 使用本地存储，故障恢复后数据会丢失，但由于 Safekeeper 通常部署为 3 副本，数据会从其他副本同步

#### 3.6.3 恢复失败处理

- **PVC 删除失败**：如果 PVC 删除失败，记录日志并返回错误，触发下一次 reconcile
- **SC 不可达**：如果 SC 不可达，记录状态并继续执行恢复（删除 Pod 和 PVC）
- **节点恢复**：如果在恢复过程中节点恢复正常，StatefulSet 会在新节点上创建 Pod，旧 Pod 最终会被清理

#### 3.6.4 并发控制

- **状态锁定**：使用 `ConditionNodeRecoveryInProgress` 状态防止并发恢复
- **Requeue 控制**：恢复成功后返回 `requeue=true`，确保 StatefulSet 能够及时创建新 Pod

## 4. 实施计划

### 4.1 代码修改清单

| 文件 | 修改内容 |
|------|----------|
| `api/v1alpha1/safekeeper_types.go` | 添加 `NodeFailure` 字段 |
| `internal/controller/pageserver_controller.go` | 扩展 `handleNodeFailure`，添加 Terminating/Failed/Unknown 处理 |
| `internal/controller/safekeeper_controller.go` | 添加 `handleNodeFailure` 及相关处理函数 |
| `specs/safekeeper/statefulset.go` | 添加 PVC 删除支持（如果需要） |
| `utils/status.go` | 添加新的 Condition 类型（如需要） |

### 4.2 测试计划

#### 4.2.1 单元测试

- 测试 `handleTerminatingPodFailure` 函数
- 测试 `handleFailedPodFailure` 函数
- 测试 `handleUnknownPodFailure` 函数
- 测试 Safekeeper 的故障检测逻辑

#### 4.2.2 集成测试

- 模拟节点 NotReady 场景，验证自动恢复是否触发
- 模拟 Pod Terminating 场景，验证强制删除逻辑
- 模拟 SC 不可达场景，验证恢复流程是否继续

#### 4.2.3 生产验证

- 在测试环境中模拟节点故障，验证端到端恢复流程
- 验证数据一致性和服务可用性

### 4.3 向后兼容性

- 现有 API 保持不变，`NodeFailure` 为可选字段
- 默认 `autoRecover: false`，保持现有行为
- Safekeeper 的 `NodeFailure` 字段为新增，不影响现有部署

## 5. 监控与可观测性

### 5.1 状态条件

| Condition | 含义 |
|-----------|------|
| `NodeRecoveryInProgress` | 节点故障恢复正在进行中 |
| `NodeFailureDetected` | 检测到节点故障 |
| `SCNodePaused` | 节点已在 SC 中标记为 Pause |

### 5.2 指标

建议添加以下 Prometheus 指标：

```
# HELP neon_pageserver_node_failure_recovery_total 节点故障恢复次数
# TYPE neon_pageserver_node_failure_recovery_total counter
neon_pageserver_node_failure_recovery_total{pageserver="my-cluster-pageserver-1",reason="Terminating"} 1

# HELP neon_pageserver_node_failure_recovery_duration_seconds 节点故障恢复耗时
# TYPE neon_pageserver_node_failure_recovery_duration_seconds histogram
neon_pageserver_node_failure_recovery_duration_seconds_bucket{pageserver="my-cluster-pageserver-1",le="10"} 1
```

### 5.3 日志

在关键步骤添加日志：

- 检测到故障时记录详细信息
- 触发恢复时记录恢复原因和步骤
- SC 状态同步结果
- Pod 和 PVC 删除结果

## 6. 总结

本设计方案通过扩展故障检测逻辑、添加 Safekeeper 故障恢复机制、同步 SC 节点状态，实现了节点故障的自动检测与恢复。方案具有以下特点：

1. **全面性**：支持检测 Terminating、Failed、Unknown 等多种故障状态
2. **生产级**：提供可配置的阈值、手动干预支持、状态同步机制
3. **兼容性**：保持与现有 API 的向后兼容性
4. **可观测性**：提供状态条件、指标和日志支持

该方案能够有效解决生产环境中节点故障导致的服务中断问题，确保 Neon 数据库集群的高可用性。