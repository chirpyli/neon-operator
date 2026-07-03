# 故障恢复分析

## 目录

1. [Safekeeper 宕机恢复](#1-safekeeper-宕机恢复)
2. [StatefulSet + 本地盘 PVC 问题](#2-statefulset--本地盘-pvc-问题)

---

## 1. Safekeeper 宕机恢复

### 1.1 当前机制：完全依赖 K8s 原生能力

Safekeeper Pod **没有配置任何 liveness/readiness probe**：

```go
func podSpec(sk *v1alpha1.Safekeeper, image, serviceName string) corev1.PodSpec {
    return corev1.PodSpec{
        Containers: []corev1.Container{{
            Name:    "safekeeper",
            Image:   image,
            Command: []string{"/usr/local/bin/safekeeper"},
            // 没有 LivenessProbe, ReadinessProbe, StartupProbe
        }},
    }
}
```

neon-operator **不做任何恢复操作**，只更新 Status Conditions。

### 1.2 宕机后的行为矩阵

| 场景 | K8s 行为 | neon-operator 行为 |
|------|----------|-------------------|
| 进程 crash（exit≠0） | StatefulSet 控制器重启容器 | 无动作 |
| 节点宕机 | Pod 超时后强制删除，调度到其他节点 | 无动作 |
| PVC 完好 | 新 Pod 重新挂载同一块 PVC，数据不丢 | 无动作 |
| 存储损坏 | Pod 持续 CrashLoopBackOff | 更新 Status 为 NotAvailable |
| 网络分区 | K8s 无感知（进程未退出） | 无动作 |

### 1.3 唯一的 observable 行为

Controller 更新 Status Condition：

```go
if err := utils.UpdateSTSBackedStatus(ctx, r.Client, safekeeper, stsName, "Safekeeper", createErr); err != nil {
```

`UpdateSTSBackedStatus` 检查 `ReadyReplicas >= DesiredReplicas`，Pod 未就绪时：

```
Available: False   (Reason: ChildPodNotReady)
Progressing: True  (Reason: Reconciling)
```

### 1.4 Neon 协议层面的容错

Safekeeper 使用多副本 WAL 共识协议，需要至少 `(NumSafekeepers / 2) + 1` 个节点存活（Quorum）：

```go
// +kubebuilder:default:=3
// +kubebuilder:validation:Minimum:=3
NumSafekeepers uint8 `json:"numSafekeepers"`
```

- **3 个 Safekeeper 中挂 1 个** → 不影响数据库运行（2/3 满足 Quorum）
- **挂 2 个** → 数据库不可用

### 1.5 缺失的关键机制

| 缺失项 | 后果 | 影响 |
|--------|------|:--:|
| **无 Liveness Probe** | 进程僵死但未退出时 Pod 永远不重启 | 🔴 高 |
| **无 Readiness Probe** | 未完成 WAL 同步就被加入 Service | 🟡 中 |
| **无 PodDisruptionBudget** | 维护时可能同时驱逐多个 Safekeeper | 🟡 中 |
| **无反亲和性 (Anti-Affinity)** | 多个 Safekeeper 可能在同一节点 | 🔴 高 |
| **无自动扩缩容** | `NumSafekeepers` 不在 Controller 中使用 | 🟡 中 |
| **无备份恢复** | 完全依赖 PVC + S3 bucket | 🟡 中 |

### 1.6 建议改进

```go
// 1. 添加健康探针
LivenessProbe: &corev1.Probe{
    ProbeHandler: corev1.ProbeHandler{
        HTTPGet: &corev1.HTTPGetAction{
            Path: "/v1/live",
            Port: intstr.FromInt(7676),
        },
    },
    InitialDelaySeconds: 10,
    PeriodSeconds: 5,
},
ReadinessProbe: &corev1.Probe{
    ProbeHandler: corev1.ProbeHandler{
        HTTPGet: &corev1.HTTPGetAction{
            Path: "/v1/ready",
            Port: intstr.FromInt(7676),
        },
    },
    InitialDelaySeconds: 5,
    PeriodSeconds: 3,
},

// 2. 添加反亲和性
Affinity: &corev1.Affinity{
    PodAntiAffinity: &corev1.PodAntiAffinity{
        RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
            LabelSelector: &metav1.LabelSelector{
                MatchLabels: map[string]string{
                    "app.kubernetes.io/component": "safekeeper",
                },
            },
            TopologyKey: "kubernetes.io/hostname",
        }},
    },
},
```

### 1.7 当前恢复步骤

| 场景 | 恢复方式 |
|------|---------|
| 进程 crash | K8s 自动重启，无需人工介入 |
| 节点宕机 | K8s 自动调度，重新挂载 PVC |
| PVC 损坏 | 手动删除 PVC → 删除 Pod → 从 S3 恢复 |
| Safekeeper CR 误删 | 重新 `kubectl apply` |

---

## 2. StatefulSet + 本地盘 PVC 问题

### 2.1 问题本质

StatefulSet 创建 PVC 是 `ReadWriteOnce`，本地盘的 PV 绑定在特定节点的物理磁盘上。

| 存储类型 | 特性 | 节点宕机 → 调度到新节点 |
|----------|------|----------------------|
| **云盘** (EBS/PD/AzureDisk) | 网络存储，任意节点挂载 | ✅ 自动挂载，数据完好 |
| **本地盘** (hostPath/local-pv) | 数据在特定节点物理磁盘上 | ❌ 卡死，Pod 永远 Pending |

### 2.2 完整卡死流程

```
初始状态：
  Node-1: safekeeper-1 (Pod) + PVC (数据在 /mnt/disks/vol1)
  Node-2: (空闲)
  Node-3: (空闲)

├── Node-1 宕机
│
├── StatefulSet Controller 检测到 Pod 失联
│
├── 尝试创建新 Pod
│   → 调度器检查 PVC 依赖
│   → PVC 绑定了本地 PV，volume 在 Node-1 的磁盘上
│   → ReadWriteOnce，不能跨节点挂载
│
├── 调度器决策：
│   ├── PV 有 nodeAffinity → Pod 只能调度到 Node-1
│   │   └── Node-1 已宕机 → Pod 永远 Pending
│   └── PV 无 nodeAffinity → Pod 可能调度到新节点
│       └── Kubelet 尝试挂载本地卷 → CreateContainerError
│
└── 最终：kubectl get pods
    safekeeper-1    0/1     Pending   0   5h     ← 永远起不来
```

### 2.3 neon-operator 不会做任何事

Controller 只报告状态，不会删除 PVC、重建 StatefulSet 或迁移数据。Pod Pending 时：

```
Available: False   (Reason: ChildPodNotReady)
Progressing: True
```

### 2.4 人工恢复步骤

```bash
# 1. 确认 Pod 卡住的原因
kubectl describe pod safekeeper-1
# 常见提示：
#   "0/3 nodes are available: 1 node(s) had volume node affinity conflict"
#   或 "MountVolume.SetUp failed for volume "pvc-xxx": hostPath type check failed"

# 2. 删除 PVC（⚠️ 数据会丢失！）
kubectl delete pvc safekeeper-storage-safekeeper-1

# 3. 删除 Pod（StatefulSet 重建，创建新 PVC）
kubectl delete pod safekeeper-1

# 4. 新 Pod 调度到健康节点，创建新 PVC（空数据）
#    Safekeeper 从 S3 bucket 恢复 WAL 数据
```

### 2.5 根本解决方案

| 方案 | 原理 | 场景 |
|------|------|------|
| **云盘 StorageClass** | PVC 绑定网络存储，跨节点透明挂载 | ✅ 生产环境首选 |
| **Rook/Ceph/Longhorn** | 开源分布式存储 | ✅ 自建机房 |
| **local-path-provisioner** | 牺牲高可用 | ⚠️ 仅开发环境 |
| **S3 恢复** | 接受数据丢失，从 S3 重放 | ⚠️ 需实现恢复流程 |

### 2.6 当前代码约束

当前 neon-operator 的 `StorageConfig` 中 StorageClass 是完全开放的 `*string` 类型，没有任何约束或默认值来防止用户误用本地盘：

```go
type StorageConfig struct {
    // +optional
    StorageClassName *string `json:"storageClassName,omitempty"`
    Size             string  `json:"size"`
}
```

建议至少增加文档说明或 CRD validation 提示用户使用网络存储。

---

## 3. Compute Pod 优雅终止与信号处理

### 3.1 问题：Pod 卡在 Terminating

Compute Pod 删除时出现长时间 Terminating 状态，典型场景：

```
NAME                                    READY   STATUS        RESTARTS   AGE
endpoint-ep-87498e53-5766bc4fbc-qfv96   0/1     Terminating   0          20m
```

**根因分析**：

| 层 | 问题 | 后果 |
|:---|:---|:---|
| PID 1 信号转发 | 容器以 `bash -c "cmd1 && cmd2"` 启动，bash 是 PID 1。K8s 发 SIGTERM 给 bash，bash **不转发**给 compute_ctl/postgres | PostgreSQL 收不到关闭信号，无法做 checkpoint |
| 宽限期不足 | 未设置 `terminationGracePeriodSeconds`，默认 30s | PostgreSQL 可能来不及完成 checkpoint |
| 无 preStop 钩子 | 无主动关闭逻辑 | 完全依赖进程的信号处理 |

如果容器还处于 `ContainerCreating` 状态（镜像拉取卡死），SIGTERM 根本无法送达，Pod 卡住时间远超 30s 默认宽限期，需要 `--force --grace-period=0` 强制删除。

### 3.2 修复方案

**代码变更**（`specs/compute/endpoint_spec.go`、`specs/compute/deployment.go`）：

```yaml
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 60          # (1) 60s 宽限期
      containers:
      - command:
        - bash
        - -c
        - echo "$INITIAL_SPEC_JSON" > /var/spec.json
          && exec /usr/local/bin/compute_ctl ...  # (2) exec 替换 PID 1
        lifecycle:
          preStop:                                 # (3) preStop 主动关闭 PG
            exec:
              command:
              - /bin/sh
              - -c
              - /usr/local/bin/pg_ctl stop
                -D /.neon/data/pgdata
                -m fast -t 25 || true
```

**三重防御序列**：

```
K8s delete Pod
  │
  ├─ (1) preStop: pg_ctl stop -m fast -t 25
  │       PostgreSQL 做 checkpoint + 回滚活跃事务
  │       最长等 25s，失败不阻塞（|| true）
  │
  ├─ (2) SIGTERM → compute_ctl (PID 1)
  │       exec 让 compute_ctl 直接成为 PID 1
  │
  └─ (3) 60s 宽限期结束 → SIGKILL 兜底
```

### 3.3 为什么 bash 是 PID 1 是个问题

Linux 内核对待 PID 1 有特殊规则：PID 1 不会收到它没有注册 handler 的信号。Bash 对 SIGTERM 的默认行为在 PID 1 和非 PID 1 时**不同**：

| 进程 | SIGTERM 行为 |
|:---|:---|
| bash (PID 1) | **忽略**不认识的信号，不转发给子进程 |
| bash (非 PID 1) | 正常终止 |

通过 `exec` 让 `compute_ctl` 成为 PID 1，K8s 的 SIGTERM 直接送达，不再通过 bash 中转。

### 3.4 强制删除补救步骤

当 Pod 因镜像拉取卡死等场景无法正常终止时：

```bash
kubectl delete pod -n <namespace> <pod-name> --force --grace-period=0
```

> ⚠️ `--force` 直接从 etcd 删除 Pod 记录，不等待 kubelet 确认。仅在 Pod 无法正常终止时使用。

### 3.5 相关设计

详见 [Control Plane Architecture 5.2.4](./design/control-plane-architecture.md#524-compute-pod-优雅终止设计)。
