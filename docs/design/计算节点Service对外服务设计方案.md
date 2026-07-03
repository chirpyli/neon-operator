# ServiceExposure 设计方案

## 1. 背景与动机

### 1.1 当前架构

neon-operator 为每个 Branch 创建一个 compute 节点（Deployment + 两个 Service）：

```
Branch (CR)
 └── Deployment: {branch-name}-compute-node    (容器端口: 3080, 55433)
 └── Service:   {branch-name}-admin            (ClusterIP, 端口 3080)
 └── Service:   {branch-name}-postgres         (ClusterIP, 端口 55433)
```

当前 `PostgresService()` 和 `AdminService()` 创建的 Service **未显式设置 `Type` 字段**，默认值为 `ClusterIP`，这意味着：

- **集群内部**：可通过 `<branch-name>-postgres.<namespace>:55433` 访问 PostgreSQL
- **集群外部**：无法直接访问，必须通过 `kubectl port-forward`、Ingress 或其他代理层

```go
// specs/compute/service.go — 当前实现
Spec: corev1.ServiceSpec{
    Selector: map[string]string{...},
    Ports:    []corev1.ServicePort{...},
    // Type 未设置 → 默认 ClusterIP
},
```

### 1.2 两种 access 模式

neon-operator 支持两种对外服务访问模式：

```
模式 A: 无 Proxy（直接访问 compute）
  外部客户端 ──► [k8s NodePort/LB] ──► compute-node:55433

模式 B: 有 Proxy（参考 neon proxy，尚未实现）
  外部客户端 ──► [Proxy Service] ──► [Proxy] ──► compute-node:55433
```

本设计解决 **模式 A**，同时为模式 B 预留扩展空间。

---

## 2. Neon Proxy 架构参考

neon 项目中的 Proxy (`/home/postgres/works/opensource/neon/proxy`) 是一个 PostgreSQL 协议代理，部署在客户端与 compute 节点之间：

```
客户端 ──► Proxy (:4432) ──► Control Plane API ──► 获取 compute (host:port)
                                    │
                                    ▼
                              TCP/TLS 直连 compute 节点
```

### 2.1 Proxy 连接流程

```rust
// proxy/src/control_plane/client/cplane_proxy_v1.rs
let Some((host, port)) = parse_host_port(&body.address) else {
    return Err(WakeComputeError::BadComputeAddress(body.address));
};
let node = NodeInfo {
    conn_info: compute::ConnectInfo {
        host_addr,  // IP 地址
        host,       // 主机名
        port,       // 端口
        ssl_mode,   // SSL 模式
    },
    ...
};
```

Proxy 通过 Control Plane `/proxy_wake_compute` 获取 compute 地址（`host:port`），在集群内部直连 compute 节点的 PostgreSQL 端口（55433）。Proxy 可以配置 TLS，客户端通过 Proxy 认证后由 Proxy 转发到 compute。

### 2.2 ConnectInfo 数据结构

```rust
// proxy/src/compute/mod.rs
pub struct ConnectInfo {
    pub host_addr: Option<IpAddr>,
    pub host: Host,
    pub port: u16,
    pub ssl_mode: SslMode,
}
```

### 2.3 对设计的启示

- 有 Proxy 时：Proxy Service 对外暴露（NodePort/LoadBalancer），compute Service 保持 ClusterIP
- 无 Proxy 时：compute Service 直接对外暴露（NodePort/LoadBalancer）
- **`ServiceExposure` 子结构复用**：两种 Service 共用同一套暴露策略结构

---

## 3. 设计目标

1. **最小侵入**：在现有 Cluster CRD 中新增一个可选字段，不破坏已有功能
2. **灵活配置**：支持 `ClusterIP`（默认）、`NodePort`、`LoadBalancer` 三种 ServiceType
3. **生产安全**：预留 `externalTrafficPolicy`、`loadBalancerSourceRanges`、自定义 `annotations`
4. **平滑演进**：后续添加 Proxy 时，`ServiceExposure` 子结构可直接复用，无需重构
5. **Operator 自动管理**：reconciler 读取 Cluster 配置，自动创建对应类型的 Service

---

## 4. API 设计

### 4.1 ServiceExposure 结构体

在 `api/v1alpha1/cluster_types.go` 中新增 `ServiceExposure` 类型：

```go
// ServiceExposure 控制 Service 对外暴露的方式。
// 可用于 PostgreSQL Service 和未来的 Proxy Service。
type ServiceExposure struct {
    // Type 决定 Service 类型。
    // ClusterIP：仅集群内访问（默认）
    // NodePort：  通过节点 IP + 端口对外暴露
    // LoadBalancer：云环境自动创建外部 LB
    // +kubebuilder:default:="ClusterIP"
    // +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
    // +optional
    Type corev1.ServiceType `json:"type,omitempty"`

    // ExternalTrafficPolicy 控制外部流量的路由策略。
    // Cluster：流量均匀分发到所有 Pod（默认，可能丢失源 IP）
    // Local：  保留客户端真实 IP，但要求 Pod 在接收流量的节点上运行
    // +kubebuilder:validation:Enum=Cluster;Local
    // +optional
    ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicyType `json:"externalTrafficPolicy,omitempty"`

    // LoadBalancerSourceRanges 限制可访问的源 IP CIDR 列表。
    // 仅在 Type=LoadBalancer 时生效。用于安全白名单。
    // +optional
    LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`

    // Annotations 透传到 Service 的 annotations。
    // 用于云厂商 LB 配置，如 AWS NLB、GCP LB 等。
    // +optional
    Annotations map[string]string `json:"annotations,omitempty"`
}
```

### 4.2 ClusterSpec 变更

```go
type ClusterSpec struct {
    // ... 现有字段保持不变 ...

    // PostgresExposure 控制计算节点 PostgreSQL Service 对外暴露策略。
    // 不设置时默认为 ClusterIP（仅集群内访问）。
    // +optional
    PostgresExposure *ServiceExposure `json:"postgresExposure,omitempty"`

    // ProxyExposure 控制 Proxy Service 对外暴露策略（预留，Proxy 尚未实现）。
    // +optional
    // ProxyExposure *ServiceExposure `json:"proxyExposure,omitempty"`
}
```

### 4.3 字段默认值策略

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `postgresExposure` | `nil` | 不设置时，operator 视为 `{type: ClusterIP}` |
| `type` | `ClusterIP` | Kubernetes 原生默认值 |
| `externalTrafficPolicy` | 空（Kubernetes 默认 `Cluster`） | 仅在 NodePort/LoadBalancer 时有效 |
| `loadBalancerSourceRanges` | `nil`（无限制） | 仅在 LoadBalancer 时有效 |
| `annotations` | `nil` | 透传到 Service |

---

## 5. Operator 代码变更

### 5.1 `specs/compute/service.go` — Service 构建函数

```go
// PostgresService 创建 PostgreSQL 连接的 Service。
// serviceType 从 Cluster.Spec.PostgresExposure 读取并应用。
func PostgresService(
    branch *neonv1alpha1.Branch,
    project *neonv1alpha1.Project,
    exposure *neonv1alpha1.ServiceExposure,
) *corev1.Service {
    svc := createService(branch, project, ServiceConfig{
        Suffix:    "postgres",
        Component: "compute-postgres",
        PortName:  "postgres",
        Port:      55433,
    })
    applyServiceExposure(svc, exposure)
    return svc
}

// applyServiceExposure 将 ServiceExposure 配置应用到 Service 对象
func applyServiceExposure(svc *corev1.Service, exposure *neonv1alpha1.ServiceExposure) {
    if exposure == nil {
        // 默认行为，不使用 NodePort，type 留空即为 ClusterIP
        return
    }
    if exposure.Type != "" {
        svc.Spec.Type = exposure.Type
    }
    if exposure.ExternalTrafficPolicy != "" {
        svc.Spec.ExternalTrafficPolicy = exposure.ExternalTrafficPolicy
    }
    if len(exposure.LoadBalancerSourceRanges) > 0 {
        svc.Spec.LoadBalancerSourceRanges = exposure.LoadBalancerSourceRanges
    }
    if len(exposure.Annotations) > 0 {
        if svc.Annotations == nil {
            svc.Annotations = make(map[string]string)
        }
        for k, v := range exposure.Annotations {
            svc.Annotations[k] = v
        }
    }
}
```

### 5.2 `internal/controller/branch_create.go` — Reconciler 变更

```go
func (r *BranchReconciler) reconcilePostgresService(
    ctx context.Context,
    branch *neonv1alpha1.Branch,
    project *neonv1alpha1.Project,
) error {
    log := logf.FromContext(ctx)

    // 读取 Cluster 的 postgresExposure 配置
    cluster, err := r.getCluster(ctx, project.Spec.ClusterName, branch.Namespace)
    if err != nil {
        return err
    }

    var exposure *neonv1alpha1.ServiceExposure
    if cluster.Spec.PostgresExposure != nil {
        exposure = cluster.Spec.PostgresExposure
    }

    intendedService := compute.PostgresService(branch, project, exposure)
    // ... 后续 create/patch 逻辑不变 ...
}
```

### 5.3 Golden Test 更新

`specs/compute/golden_test.go` 需要更新 golden 测试数据，覆盖不同 `ServiceExposure` 组合：

- `postgres_service_default.yaml` — exposure 为 nil（ClusterIP 默认）
- `postgres_service_nodeport.yaml` — `{type: NodePort}`
- `postgres_service_loadbalancer.yaml` — `{type: LoadBalancer, loadBalancerSourceRanges: [...]}`

---

## 6. 使用示例

### 6.1 默认行为（零改动兼容）

```yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: my-cluster
  namespace: neon
spec:
  numSafekeepers: 3
  defaultPGVersion: 17
  neonImage: "neondatabase/neon:8463"
  bucketCredentialsSecret:
    name: bucket-creds
  storageControllerDatabaseSecret:
    name: storage-controller-db
    key: uri
  # postgresExposure 不设置 → 默认 ClusterIP，行为不变
```

### 6.2 NodePort（裸金属 / 自建集群，当前推荐）

```yaml
spec:
  # ... 其他字段 ...
  postgresExposure:
    type: NodePort
```

创建后 Service 自动分配 NodePort：

```bash
$ kubectl get svc -n neon main-branch-postgres
NAME                    TYPE       CLUSTER-IP     EXTERNAL-IP   PORT(S)           AGE
main-branch-postgres    NodePort   10.96.50.100   <none>        55433:31234/TCP   10s

# 从集群外连接（使用任意 k8s 节点 IP）
$ psql -h <任意节点IP> -p 31234 -U cloud_admin -d postgres
```

### 6.3 LoadBalancer（云上 k8s）

```yaml
spec:
  # ... 其他字段 ...
  postgresExposure:
    type: LoadBalancer
    externalTrafficPolicy: Local        # 保留客户端真实 IP
    loadBalancerSourceRanges:
      - "203.0.113.0/24"                # 办公网段白名单
      - "198.51.100.5/32"               # 特定运维机器
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-type: "nlb"
```

### 6.4 未来：有 Proxy 时的配置

```yaml
spec:
  # ... 其他字段 ...
  # compute 保持内部访问（默认 ClusterIP）
  # postgresExposure 不设置

  # Proxy 对外暴露（新增字段，需后续实现 Proxy reconciler）
  proxyExposure:
    type: LoadBalancer
    externalTrafficPolicy: Local
    loadBalancerSourceRanges:
      - "0.0.0.0/0"                    # Proxy 可公网访问
```

---

## 7. 与 Neon Proxy 的演进兼容性

### 7.1 架构对比

```
=== 阶段 1: 无 Proxy（当前设计目标）===
                         ┌──────────────┐
  外部客户端 ──NodePort──►│  k8s Service  │──► compute:55433
                         │  type:NodePort │
                         └──────────────┘

=== 阶段 2: 有 Proxy（未来）===
                         ┌──────────────┐    ┌─────────┐
  外部客户端 ──LB────────►│ Proxy Service│───►│  Proxy  │──内部──► compute:55433
                         │ type:LB      │    │  :4432  │
                         └──────────────┘    └─────────┘
```

### 7.2 ServiceExposure 复用

`ServiceExposure` 子结构不与任何特定 Service 绑定，可以同时用于：

| Service | 暴露字段 | 说明 |
|---------|---------|------|
| compute PostgresService | `Cluster.Spec.PostgresExposure` | 无 Proxy 时对外，有 Proxy 时保持 ClusterIP |
| Proxy Service（未来） | `Cluster.Spec.ProxyExposure` | 始终对外暴露 |
| compute AdminService | 固定 ClusterIP | 内部管理接口，不对外 |

### 7.3 Neon 项目中 Proxy 如何连接 compute

在 neon 项目中，Proxy 通过 Control Plane API 获取 compute 地址（参考 `proxy/src/control_plane/client/cplane_proxy_v1.rs`）：

```rust
let Some((host, port)) = parse_host_port(&body.address) else {
    return Err(WakeComputeError::BadComputeAddress(body.address));
};
```

当 operator 同时管理 Proxy 和 Compute 时，Control Plane（即 storage controller）返回的 compute 地址就是 k8s 内部 Service DNS 名（如 `main-branch-postgres.neon:55433`），Proxy 在集群内部通过 ClusterIP 直连 compute，无需 compute Service 对外暴露。

---

## 8. 完整改动清单

| 序号 | 文件 | 变更 |
|------|------|------|
| 1 | `api/v1alpha1/cluster_types.go` | 新增 `ServiceExposure` 结构体，`ClusterSpec` 新增 `PostgresExposure` 字段 |
| 2 | `specs/compute/service.go` | `PostgresService()` 增加 `exposure` 参数，新增 `applyServiceExposure()` 辅助函数 |
| 3 | `internal/controller/branch_create.go` | `reconcilePostgresService()` 读取 Cluster 的 `PostgresExposure` 并传入 |
| 4 | `internal/controller/branch_controller.go` | `SetupWithManager` 新增 `Watches(&Cluster{}, ...)`，Cluster 变更时自动触发 Branch reconcile |
| 5 | `specs/compute/golden_test.go` | 更新 golden test，覆盖多种 ServiceExposure 场景 |
| 6 | `specs/compute/testdata/postgres_service.yaml` | 更新 golden 数据（默认 ClusterIP，当前已一致） |
| 7 | `specs/compute/testdata/postgres_service_nodeport.yaml` | 新增 golden 数据（NodePort 场景） |
| 8 | `specs/compute/testdata/postgres_service_loadbalancer.yaml` | 新增 golden 数据（LoadBalancer 场景） |
| 9 | `config/crd/bases/neon.oltp.molnett.org_clusters.yaml` | `make manifests` 自动重新生成 |

### 8.1 Cluster Watch 触发机制（第 4 项详解）

`branch_controller.go` 通过 `Watches(&Cluster{}, ...)` 监听 Cluster 变更事件。当用户修改 Cluster 的 `postgresExposure` 时，控制器自动触发关联 Branch 重新 reconcile，更新 PostgresService。

**映射链路**：

```
Cluster → Project(Spec.ClusterName == Cluster.Name) → Branch(Spec.ProjectID == Project.Name)
```

**实现代码**（`branch_controller.go`）：

```go
// SetupWithManager ...
func (r *BranchReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&neonv1alpha1.Branch{}).
        Owns(&appsv1.Deployment{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ConfigMap{}).
        Watches(&neonv1alpha1.Cluster{},
            handler.EnqueueRequestsFromMapFunc(r.clusterToBranchRequests),
        ).
        Named("branch").
        Complete(r)
}

// clusterToBranchRequests 将 Cluster 变更映射为 Branch reconcile 请求
func (r *BranchReconciler) clusterToBranchRequests(ctx context.Context, obj client.Object) []reconcile.Request {
    cluster := obj.(*neonv1alpha1.Cluster)

    // 1. 列出所有引用该 Cluster 的 Project
    var projects neonv1alpha1.ProjectList
    r.List(ctx, &projects, client.InNamespace(cluster.Namespace))

    // 2. 收集匹配的 Project ID
    var projectIDs []string
    for _, p := range projects.Items {
        if p.Spec.ClusterName == cluster.Name {
            projectIDs = append(projectIDs, p.Name)
        }
    }

    // 3. 列出这些 Project 下的所有 Branch，生成 reconcile 请求
    var branches neonv1alpha1.BranchList
    r.List(ctx, &branches, client.InNamespace(cluster.Namespace))

    var requests []reconcile.Request
    for _, b := range branches.Items {
        for _, pid := range projectIDs {
            if b.Spec.ProjectID == pid {
                requests = append(requests, reconcile.Request{
                    NamespacedName: types.NamespacedName{
                        Name: b.Name, Namespace: b.Namespace,
                    },
                })
                break
            }
        }
    }
    return requests
}
```

**触发场景**：

| 用户操作 | 触发效果 |
|----------|---------|
| 新建/删除 Cluster | 触发关联 Branch 重新 reconcile |
| 修改 `postgresExposure.type` | Branch 的 PostgresService 自动更新 |
| 修改 `externalTrafficPolicy` | Service 自动更新 |
| 修改 `loadBalancerSourceRanges` | Service 自动更新 |
| 修改 `annotations` | Service 自动更新 |

### 8.2 DeepCopy 自动生成

`ServiceExposure` 结构体包含 `map[string]string` 和 `[]string` 类型，需要自动生成 `DeepCopy` 方法。运行：

```bash
make generate   # controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."
```

### 8.3 CRD 重新生成

```bash
make manifests  # controller-gen rbac:roleName=manager-role crd webhook paths="./..."
```

---

## 9. 测试策略

| 层级 | 测试内容 | 工具 |
|------|---------|------|
| 单元测试 | `applyServiceExposure()` 各种参数组合 | `go test` |
| Golden 测试 | 默认/NodePort/LoadBalancer 的 YAML 输出 | `specs/compute/golden_test.go` |
| 集成测试 | 创建 Cluster → 创建 Branch → 验证 Service 类型 | envtest |
| 手动验证 | deploy 到实际集群，psql 外部连接 | `kubectl` + `psql` |

---

## 10. 向后兼容性

- **零改动兼容**：不设置 `postgresExposure` 的现有 Cluster CR 行为完全不变，Service 保持 ClusterIP
- **CRD 兼容**：新字段为 `optional`，旧版本 CRD 的 YAML 不需要修改即可 apply
- **升级路径**：先 `make install` 更新 CRD，再 `make deploy` 更新 operator，已有的 Branch Service 不变（reconciler 检测到 `postgresExposure` 为空时跳过 Service patch）
