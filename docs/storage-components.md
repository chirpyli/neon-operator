# 存储组件与 ControlPlane 分析

## 目录

1. [StorageController Deployment 分析](#1-storagecontroller-deployment-分析)
2. [ControlPlane 设计分析](#2-controlplane-设计分析)
3. [Pageserver 扩缩容现状](#3-pageserver-扩缩容现状)

---

## 1. StorageController Deployment 分析

### 1.1 文件结构

```
specs/storagecontroller/deployment.go (84行)
├── 命名：{cluster-name}-storage-controller
├── TypeMeta + ObjectMeta
├── Spec 顶层（Replicas, Strategy, Selector）
├── Pod Template Metadata
└── Pod Spec（Container + Env + Ports）
```

### 1.2 命名与服务发现

```go
func Name(clusterName string) string {
    return clusterName + "-storage-controller"
}
```

Project Controller 用这个命名约定构建 HTTP 调用地址：

```go
base = storagecontroller.URL(project.Spec.ClusterName)
// → "http://my-cluster-storage-controller:8080"
```

不需要服务注册中心，只需要知道 Cluster Name 就能推断地址。

### 1.3 关键配置

| 配置项 | 值 | 说明 |
|--------|----|------|
| Replicas | 1 | 单例，有状态元数据服务 |
| Strategy | RollingUpdate | 可容忍短暂双实例共存 |
| 端口 | 8080 | HTTP 管理 API |
| Image | `cluster.Spec.NeonImage` | 统一版本 |
| ImagePullPolicy | PullIfNotPresent | 开发友好 |

### 1.4 Container 启动参数

```bash
storage_controller --dev -l 0.0.0.0:8080 \
    --control-plane-url http://neon-controlplane:8081 \
    --initial-split-shards 0
```

| 参数 | 说明 | 风险 |
|------|------|:--:|
| `--dev` | 开发模式，跳过安全检查 | 🔴 生产需移除 |
| `-l 0.0.0.0:8080` | 监听所有接口 | ✅ |
| `--control-plane-url` | 硬编码的控制面板地址 | 🟡 需可配置化 |
| `--initial-split-shards 0` | 单实例不分片 | ✅ |

### 1.5 DATABASE_URL 环境变量

```go
Env: []corev1.EnvVar{{
    Name: "DATABASE_URL",
    ValueFrom: &corev1.EnvVarSource{
        SecretKeyRef: &corev1.SecretKeySelector{
            Name: cluster.Spec.StorageControllerDatabaseSecret.Name,
            Key:  cluster.Spec.StorageControllerDatabaseSecret.Key,
        },
    },
}},
```

从 Secret 注入数据库连接串，不在 CR YAML 中暴露：

```
Cluster.Spec.StorageControllerDatabaseSecret
  ├── .Name → Secret 名称
  └── .Key  → Secret 中的 key（通常为 "uri"）
                → Secret.Data["uri"] = "postgresql://..." → DATABASE_URL
```

### 1.6 Golden Test

`specs/storagecontroller/testdata/deployment.yaml` 作为期望输出的 golden file，测试中用于对比，确保代码改动不会意外改变生成的 Deployment 结构。

---

## 2. ControlPlane 设计分析

### 2.1 不是完整实现

neon-operator 的 `internal/controlplane/` **不是完整的 Neon Control Plane**，而是一个最小化的 HTTP 回调适配器。

| Neon Control Plane 典型功能 | neon-operator 是否实现 |
|---------------------------|:---:|
| 用户认证/授权 | ❌ |
| Project/Branch 管理 API | ❌（由 K8s CRD + Controller 替代） |
| 计费 | ❌ |
| 管理控制台 | ❌ |
| Compute 配置下发 | ✅ 部分 |
| Tenant Attach 通知处理 | ✅ |
| Safekeeper 变更通知 | ✅ |
| 健康检查 | ✅ |

### 2.2 架构定位

```
传统 Neon: 用户 → HTTP API → Control Plane → Storage Controller
neon-operator: 用户 → kubectl apply → K8s API → Controller → Storage Controller
```

**CRD + Controller 替代了传统 Control Plane 的 CRUD API**，`internal/controlplane/` 只处理必须用 HTTP 的回调。

### 2.3 四条路由

```go
mux.Handle("/compute/api/v2/computes/{compute_id}/spec", handleComputeSpec(...))
mux.Handle("/notify-attach", notifyAttach(...))
mux.Handle("/notify-safekeepers", notifySafekeepers(...))
mux.Handle("/healthz", handleHealthCheck())
mux.Handle("/readyz", handleHealthCheck())
```

| 路由 | 方法 | 调用方 | 模式 |
|------|:--:|--------|:--:|
| `/compute/.../spec` | GET | Compute Pod（启动时） | 拉 |
| `/notify-attach` | POST | StorageController | 推 |
| `/notify-safekeepers` | POST | StorageController | 推 |
| `/healthz`, `/readyz` | GET | K8s Probes | — |

### 2.4 推拉结合的配置下发

**拉模式**：Compute Pod 启动时调用 `GET /compute/.../spec` → ControlPlane 生成完整 ComputeSpec → 返回 JSON。

`GenerateComputeSpec` 流程：
```
1. 从 Deployment labels 提取 tenant_id, timeline_id, cluster_name
2. 查 Project + Branch CR 获取元数据
3. 读 JWT Secret 生成 JWKS
4. 构建 Safekeeper 连接串（命名约定推算）
5. 调用 StorageController API 获取 Shard 信息
6. 组装完整的 ComputeSpec + ComputeCtlConfig + Status
```

**推模式**：StorageController 回调 `POST /notify-attach` → ControlPlane 查找相关 Compute Deployment → 生成配置 → POST 推送到每个 Compute Pod 的 `/configure` 端点。

### 2.5 实现方式

```go
type ControlPlane struct {
    Log            *slog.Logger
    Client         client.Client
    BindAddr       string    // 默认 :8082
    ComputeBaseURL string
}
```

实现 `manager.Runnable` 接口，在 `main.go` 中注册：

```go
mgr.Add(&controlplane.ControlPlane{
    Log:      slog.New(slog.NewJSONHandler(os.Stdout, nil)),
    Client:   mgr.GetClient(),
    BindAddr: ":8082",
})
```

### 2.6 已知对齐问题

| 组件 | 地址 | 端口 |
|------|------|:--:|
| StorageController 参数 | `neon-controlplane:8081` | 8081 |
| neon-operator ControlPlane | 默认 `:8082` | 8082 |

端口不一致，部署时需确保 StorageController 的 `--control-plane-url` 指向 neon-operator ControlPlane 的 K8s Service。

---

## 3. Pageserver 扩缩容现状

### 3.1 结论：不支持内置扩缩容

- **StatefulSet replicas 硬编码为 1**
- **PageserverSpec 中没有 replicas 字段**
- **Controller 没有扩缩容逻辑**

### 3.2 当前扩容方式

创建多个 Pageserver CR，每个使用不同的 ID：

```yaml
# 6 个独立的 Pageserver CR
kind: NeonPageserver
metadata:
  name: basic-cluster-pageserver-1
spec:
  id: 1
---
kind: NeonPageserver
metadata:
  name: basic-cluster-pageserver-2
spec:
  id: 2
# ... 以此类推
```

每个 CR → 一个独立 StatefulSet（1 副本）→ 独立 PVC。

### 3.3 如果要支持扩缩容

| 改动点 | 说明 |
|--------|------|
| `PageserverSpec` 增加 `Replicas` 字段 | 让用户声明期望副本数 |
| `StatefulSet()` 读取 `Replicas` | 替代硬编码的 `ptr.To(int32(1))` |
| Controller 处理缩容时的 PVC 清理 | StatefulSet 缩容不自动删 PVC |
| 考虑 Pageserver 的 Neon 内部限制 | ID 必须唯一，多副本需不同 ID 注册逻辑 |
