# Control Plane 对外暴露设计方案

| 字段 | 内容 |
|------|------|
| 版本 | v1.0 |
| 日期 | 2026-07-03 |
| 目标 | 生产级私有化部署 Neon 的 Control Plane 网络暴露方案 |
| 前置 | [control-plane-architecture.md](./control-plane-architecture.md), [jwt-production-design.md](./jwt-production-design.md) |

---

## 一、背景与目标

### 1.1 现状分析

当前 `controlplane` Service 配置（位于 `system` namespace）：

```yaml
# config/manager/manager.yaml 中的配置
apiVersion: v1
kind: Service
metadata:
  name: controlplane
  namespace: system
spec:
  type: ClusterIP       # 仅集群内部访问
  ports:
    - name: http
      port: 8081
      targetPort: controlplane
  selector:
    control-plane: controller-manager
    app.kubernetes.io/name: neon-operator-go
```

**实际架构**：
- controlplane 运行在 `controller-manager` Pod 内部，通过 `--controlplane-bind-address=:8082` 启动
- Service 暴露端口 8081，映射到容器端口 8082
- Deployment 为单副本，启用 leader-elect
- 用户当前看到的 `neon-controlplane` Service 可能是手动创建的

**问题**：
- 用户只能通过 `kubectl port-forward -n system svc/controlplane 8081:8081` 访问
- Service 类型硬编码为 ClusterIP，无法通过配置变更
- 无 TLS 加密，传输明文
- 无 API 认证，任何能到达此 Service 的请求都可执行操作
- 无访问控制，无 IP 白名单或网络隔离
- 单副本部署，无高可用保障

### 1.2 目标

设计一套**生产级私有化部署**的 Control Plane 暴露方案，满足：

| 需求 | 描述 |
|------|------|
| **网络可达** | 客户端可直接通过固定域名/IP 访问，无需端口转发 |
| **传输安全** | 强制 HTTPS，配置 TLS 证书 |
| **身份认证** | API 请求必须携带有效凭证（API Key / Bearer Token） |
| **访问控制** | 支持 IP 白名单、网络策略限制 |
| **高可用性** | Control Plane 多副本部署，支持故障切换 |
| **可观测性** | 集成监控、日志、审计 |

---

## 二、API 功能范围分析

### 2.1 当前已实现的 API 端点

Control Plane 端口 8081 上运行的服务分为两类：

| 类别 | 路径前缀 | 用途 | 调用方 |
|------|---------|------|--------|
| **内部接口** | `/compute/api/v2/computes/{id}/spec` | compute_ctl 获取配置 | Compute Pod |
| **内部接口** | `/notify-attach` | Storage Controller 通知 Compute 挂载 | Storage Controller |
| **内部接口** | `/notify-safekeepers` | Storage Controller 通知 SK 变更 | Storage Controller |
| **内部接口** | `/healthz`, `/readyz` | K8s 健康探针 | Kubernetes |
| **外部 API** | `/api/v2/projects/*` | 项目管理 | 客户端/控制台 |
| **外部 API** | `/api/v2/projects/{id}/branches/*` | 分支管理 | 客户端/控制台 |
| **外部 API** | `/api/v2/projects/{id}/endpoints/*` | 端点管理 | 客户端/控制台 |
| **外部 API** | `/api/v2/projects/{id}/branches/{id}/roles/*` | 角色管理 | 客户端/控制台 |
| **外部 API** | `/api/v2/projects/{id}/branches/{id}/databases/*` | 数据库管理 | 客户端/控制台 |

### 2.2 访问需求矩阵

| 路径 | 内部访问 | 外部访问 | 认证要求 |
|------|:---:|:---:|:---:|
| `/compute/api/v2/computes/*/spec` | ✅ | ❌ | JWT (tenant scope) |
| `/notify-attach` | ✅ | ❌ | JWT (admin scope) |
| `/notify-safekeepers` | ✅ | ❌ | JWT (admin scope) |
| `/healthz`, `/readyz` | ✅ | ❌ | 无（探针豁免） |
| `/api/v2/*` | ✅ | ✅ | API Key / Bearer Token |

---

## 三、方案对比与选型

### 3.1 可行方案对比

| 方案 | Service 类型 | 优点 | 缺点 | 适用场景 |
|------|-------------|------|------|---------|
| **方案 A** | ClusterIP + Ingress | 统一入口、TLS 终止、路由规则 | 需要 Ingress Controller | 标准 K8s 集群 |
| **方案 B** | LoadBalancer | 云原生、自动分配公网 IP | 依赖云厂商 LB | 公有云环境 |
| **方案 C** | NodePort + 外部 LB | 灵活、可控 | 需要外部 LB 配置 | 裸金属/私有云 |
| **方案 D** | ClusterIP + API Gateway | 全功能、安全能力强 | 需要额外组件 | 企业级部署 |

### 3.2 推荐方案：渐进式 NodePort → Ingress

**推荐理由**：
1. **快速落地**：先用 NodePort 解决"可达性"问题，无需等待 Ingress Controller 部署
2. **与项目一致**：项目中 `postgresExposure` 已支持 NodePort，可复用相同模式
3. **平滑迁移**：后续可无缝切换到 Ingress，Service 配置无需大幅改动
4. **最小侵入**：NodePort 只需修改 Service 类型，不涉及其他组件

**渐进式路线**：
- **Phase 0（当前）**：ClusterIP + kubectl port-forward（临时方案）
- **Phase 1（短期）**：NodePort + 外部 LB（解决可达性，快速验证）
- **Phase 2（中期）**：Ingress + TLS（生产级安全增强）
- **Phase 3（长期）**：API Gateway + SSO（企业级能力）

### 3.3 NodePort 方案详细分析

#### 3.3.1 为什么选择 NodePort 作为第一步？

| 维度 | NodePort | Ingress |
|------|:---:|:---:|
| **部署复杂度** | 低（仅修改 Service） | 高（需部署 Ingress Controller） |
| **配置工作量** | 小（3-5 行 YAML 变更） | 大（Ingress + TLS + 证书管理） |
| **验证速度** | 快（分钟级） | 慢（小时级） |
| **外部依赖** | 无 | 需要 Ingress Controller |
| **私有化适配** | 好（不依赖云厂商） | 中等（需要 DNS 配置） |

#### 3.3.2 NodePort 的安全风险与缓解

| 风险 | 缓解措施 |
|------|---------|
| **无 TLS 加密** | 后续 Phase 2 通过 Ingress 解决；临时可使用 `kubectl port-forward` + `ssh tunnel` |
| **端口暴露面大** | 配置网络策略限制访问源 IP；仅开放必要端口 |
| **端口冲突** | 固定 NodePort 端口号，避免自动分配冲突 |
| **单点故障** | 配置外部 LB 或 VIP 做节点级负载均衡 |

#### 3.3.3 与项目现有模式对齐

项目中 `postgresExposure` 已支持多种暴露方式：

```go
// api/v1alpha1/cluster_types.go
type ServiceExposure struct {
    Type                     corev1.ServiceType `json:"type,omitempty"`
    ExternalTrafficPolicy    corev1.ServiceExternalTrafficPolicyType `json:"externalTrafficPolicy,omitempty"`
    LoadBalancerSourceRanges []string                               `json:"loadBalancerSourceRanges,omitempty"`
    Annotations              map[string]string                       `json:"annotations,omitempty"`
}
```

### 3.3.4 设计：新增 `controlPlaneExposure` 字段

在 `ClusterSpec` 中新增 `ControlPlaneExposure` 字段，复用 `ServiceExposure` 结构：

```go
// api/v1alpha1/cluster_types.go
type ClusterSpec struct {
    // ... 现有字段 ...
    
    // ControlPlaneExposure 控制 Control Plane Service 对外暴露策略。
    // 不设置时默认为 ClusterIP（仅集群内访问）。
    // +optional
    ControlPlaneExposure *ServiceExposure `json:"controlPlaneExposure,omitempty"`
}
```

**用户配置示例**：

```yaml
# cluster.yaml
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: my-cluster
spec:
  numSafekeepers: 3
  numPageservers: 2
  # Phase 1: NodePort 暴露
  controlPlaneExposure:
    type: NodePort
    externalTrafficPolicy: Cluster
  # ... 其他配置 ...
```

### 3.3.5 `externalTrafficPolicy` 选择分析

| 策略 | 优点 | 缺点 | 适用场景 |
|------|------|------|---------|
| **Cluster**（默认） | 流量均匀分发到所有 Pod，无需每个节点都有 Pod | 可能丢失客户端源 IP | 私有化部署（节点数少） |
| **Local** | 保留客户端真实 IP | 要求 Pod 在接收流量的节点上运行，否则请求失败 | 公有云（节点数多，需要源 IP） |

**推荐**：私有化部署使用 `Cluster` 策略，因为节点数量通常较少（3-5 节点），且保留源 IP 的需求相对较低。

---

## 四、详细设计

### 4.1 Phase 1：NodePort 网络架构

```
                              ┌─────────────────────────────────┐
                              │         客户端 / 控制台           │
                              └───────────────────┬─────────────┘
                                                  │ HTTP
                                                  ▼
                              ┌─────────────────────────────────┐
                              │         外部 LB / VIP            │
                              │  • 节点级负载均衡                │
                              │  • 健康检查                      │
                              └───────────────────┬─────────────┘
                                                  │ NodePort 30881
                                                  ▼
                    ┌────────────────────────────┼────────────────────────────┐
                    │                            │                            │
                    ▼                            ▼                            ▼
          ┌─────────────────┐         ┌─────────────────┐         ┌─────────────────┐
          │     Node 1      │         │     Node 2      │         │     Node 3      │
          │  30881 ─────────┼─────────┤  30881 ─────────┼─────────┤  30881          │
          └────────┬────────┘         └────────┬────────┘         └────────┬────────┘
                   │                           │                           │
                   ▼                           ▼                           ▼
          ┌─────────────────┐         ┌─────────────────┐         ┌─────────────────┐
          │ controller-     │         │ controller-     │         │ controller-     │
          │  manager Pod    │         │  manager Pod    │         │  manager Pod    │
          │  (含 controlplane) │      │  (含 controlplane) │      │  (含 controlplane) │
          └─────────────────┘         └─────────────────┘         └─────────────────┘
                    │                            │                            │
                    └────────────────────────────┼────────────────────────────┘
                                                 │
                                                 ▼
                              ┌─────────────────────────────────┐
                              │      controlplane Service        │
                              │        (NodePort, 30881)         │
                              │     namespace: system            │
                              └─────────────────────────────────┘
```

### 4.2 Phase 1：NodePort Service 配置

**修改 Service 类型为 NodePort**，配置固定端口（位于 `system` namespace）：

```yaml
apiVersion: v1
kind: Service
metadata:
  name: controlplane
  namespace: system
spec:
  type: NodePort
  ports:
    - name: http
      port: 8081
      targetPort: controlplane
      nodePort: 30881
      protocol: TCP
  selector:
    control-plane: controller-manager
    app.kubernetes.io/name: neon-operator-go
  externalTrafficPolicy: Cluster
```

**关键配置说明**：

| 字段 | 值 | 说明 |
|------|------|------|
| `type` | `NodePort` | 暴露到节点端口 |
| `nodePort` | `30881` | 固定端口号，避免自动分配冲突 |
| `externalTrafficPolicy` | `Cluster` | 流量均匀分发到所有 Pod（私有化部署推荐） |
| `selector` | `control-plane: controller-manager` | 匹配 controller-manager Pod |

**选择 30881 的理由**：
- 符合 K8s NodePort 默认范围（30000-32767）
- 避免与 PostgreSQL 端口冲突（5432）
- 避免与 Pageserver 端口冲突（6400）
- 避免与 Safekeeper 端口冲突（5454）

### 4.3 Phase 1：Deployment 配置（controlplane 嵌入 controller-manager）

**注意**：controlplane 运行在 `controller-manager` Pod 内部，不是独立 Deployment。需要调整 Deployment 的副本数和 leader-elect 配置：

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
  namespace: system
spec:
  replicas: 3          # 从 1 增加到 3，实现高可用
  selector:
    matchLabels:
      control-plane: controller-manager
      app.kubernetes.io/name: neon-operator-go
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  template:
    metadata:
      annotations:
        kubectl.kubernetes.io/default-container: manager
      labels:
        control-plane: controller-manager
        app.kubernetes.io/name: neon-operator-go
    spec:
      containers:
        - command:
            - /manager
          args:
            - --leader-elect
            - --health-probe-bind-address=:8081
            - --controlplane-bind-address=:8082
          image: controller
          name: manager
          ports:
            - containerPort: 8082
              name: controlplane
              protocol: TCP
            - containerPort: 8443
              name: metrics
              protocol: TCP
          # ... 其他配置（探针、资源限制等）...
```

### 4.4 Phase 2：Ingress 网络架构（后续迁移）

```
                              ┌─────────────────────────────────┐
                              │         客户端 / 控制台           │
                              └───────────────────┬─────────────┘
                                                  │ HTTPS
                                                  ▼
                              ┌─────────────────────────────────┐
                              │          Ingress Controller      │
                              │  • TLS 终止                      │
                              │  • 路径路由                      │
                              │  • IP 白名单                      │
                              └───────────────────┬─────────────┘
                                                  │ HTTP
                                                  ▼
                    ┌────────────────────────────┼────────────────────────────┐
                    │                            │                            │
                    ▼                            ▼                            ▼
          ┌─────────────────┐         ┌─────────────────┐         ┌─────────────────┐
          │ controller-     │         │ controller-     │         │ controller-     │
          │  manager Pod    │         │  manager Pod    │         │  manager Pod    │
          │  (含 controlplane) │      │  (含 controlplane) │      │  (含 controlplane) │
          └─────────────────┘         └─────────────────┘         └─────────────────┘
                    │                            │                            │
                    └────────────────────────────┼────────────────────────────┘
                                                 │
                                                 ▼
                              ┌─────────────────────────────────┐
                              │      controlplane Service        │
                              │        (ClusterIP, 8081)         │
                              │     namespace: system            │
                              └─────────────────────────────────┘
```

### 4.5 Phase 2：Ingress Service 配置（后续迁移）

**恢复 ClusterIP 类型**，通过 Ingress 暴露（位于 `system` namespace）：

```yaml
apiVersion: v1
kind: Service
metadata:
  name: controlplane
  namespace: system
spec:
  type: ClusterIP
  ports:
    - name: http
      port: 8081
      targetPort: controlplane
      protocol: TCP
  selector:
    control-plane: controller-manager
    app.kubernetes.io/name: neon-operator-go
### 4.6 Phase 2：Ingress 配置设计

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: neon-controlplane
  namespace: system
  annotations:
    cert-manager.io/cluster-issuer: "selfsigning-issuer"
    nginx.ingress.kubernetes.io/whitelist-source-range: "10.0.0.0/8"
    nginx.ingress.kubernetes.io/limit-rps: "100"
    nginx.ingress.kubernetes.io/proxy-body-size: "10m"
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - api.neon.example.com
      secretName: neon-controlplane-tls
  rules:
    - host: api.neon.example.com
      http:
        paths:
          - path: /api/v2
            pathType: Prefix
            backend:
              service:
                name: controlplane
                port:
                  number: 8081
```

### 4.7 Phase 2：TLS 证书管理

#### 方案 4.7.1：使用 cert-manager（推荐）

```yaml
# 自签名证书（内部环境）
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigning-issuer
spec:
  selfSigned: {}
```

#### 方案 4.7.2：手动证书管理

```yaml
# 创建自签名证书
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout tls.key -out tls.crt \
  -subj "/CN=api.neon.example.com"

# 创建 Secret（注意：放在 system namespace）
kubectl create secret tls neon-controlplane-tls \
  --key=tls.key --cert=tls.crt -n system
```

### 4.8 API 认证设计（代码层面，与暴露方式无关）

**注意**：API 认证是代码层面的变更，可与 NodePort 暴露并行实施，不依赖 Ingress。

#### 4.8.1 认证架构

```
客户端请求
    │
    ▼
Ingress (TLS 终止)
    │
    ▼
API Gateway / Auth Middleware
    │
    ├─ 验证 Authorization Header
    │   └─ Bearer Token → 验证签名 → 提取用户身份
    │
    ├─ 验证 API Key
    │   └─ API-Key Header → 查询数据库 → 验证有效性
    │
    └─ 授权检查
        └─ 用户是否有权访问目标资源
    │
    ▼
Control Plane API
```

#### 4.5.2 API Key 方案（推荐）

**优点**：简单、易用、与 Neon Cloud API 兼容

**设计**：

```yaml
# API Key Secret 存储
apiVersion: v1
kind: Secret
metadata:
  name: neon-api-keys
  namespace: neon
type: Opaque
stringData:
  # 格式: <key_id>:<secret_value>
  api-key-admin: "admin_sk_abc123..."
  api-key-app1: "app_sk_xyz789..."
```

**认证中间件逻辑**：

| 步骤 | 操作 |
|------|------|
| 1 | 检查 `Authorization: Bearer <token>` 或 `X-API-Key: <key>` |
| 2 | 验证 key 存在且未过期 |
| 3 | 提取用户/组织 ID |
| 4 | 传递给下游 API 进行资源级授权 |

**API Key 生成规则**：
- 格式：`<prefix>_sk_<random_32_chars>`
- 前缀：`admin`（管理员）、`app`（应用）、`service`（服务）
- 长度：48 字符（含前缀和分隔符）

#### 4.8.3 JWT 方案（备选）

```yaml
# JWT Secret（复用现有的 JWT 密钥对）
apiVersion: v1
kind: Secret
metadata:
  name: cluster-my-cluster-jwt
  namespace: neon
type: Opaque
data:
  private.pem: <Ed25519 私钥>
  public.pem: <Ed25519 公钥>
```

**JWT Claims**：

```json
{
  "iss": "neon-operator",
  "sub": "user:admin@example.com",
  "aud": ["neon-api"],
  "iat": 1719900000,
  "exp": 1719986400,
  "scope": "admin",
  "org_id": "org-abc123"
}
```

### 4.9 网络策略

#### 4.9.1 Phase 1：NodePort 网络策略

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-controlplane-nodeport
  namespace: system
spec:
  podSelector:
    matchLabels:
      control-plane: controller-manager
  policyTypes:
    - Ingress
  ingress:
    # 允许企业内网访问（通过 NodePort）
    - from:
        - ipBlock:
            cidr: 10.0.0.0/8
      ports:
        - protocol: TCP
          port: 8081
    
    # 允许 Compute Pod 访问 /compute/api/v2/computes/*/spec
    - from:
        - podSelector:
            matchLabels:
              molnett.org/component: compute
      ports:
        - protocol: TCP
          port: 8081
    
    # 允许 Storage Controller 访问 /notify-attach, /notify-safekeepers
    - from:
        - podSelector:
            matchLabels:
              molnett.org/component: storage-controller
      ports:
        - protocol: TCP
          port: 8081

#### 4.9.2 Phase 2：Ingress 网络策略

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-controlplane-ingress
  namespace: system
spec:
  podSelector:
    matchLabels:
      control-plane: controller-manager
  policyTypes:
    - Ingress
  ingress:
    # 允许 Ingress Controller 访问（外部 API）
    - from:
        - namespaceSelector:
            matchLabels:
              name: ingress-nginx
      ports:
        - protocol: TCP
          port: 8081
    
    # 允许 Compute Pod 访问 /compute/api/v2/computes/*/spec
    - from:
        - podSelector:
            matchLabels:
              molnett.org/component: compute
      ports:
        - protocol: TCP
          port: 8081
    
    # 允许 Storage Controller 访问 /notify-attach, /notify-safekeepers
    - from:
        - podSelector:
            matchLabels:
              molnett.org/component: storage-controller
      ports:
        - protocol: TCP
          port: 8081
    
    # 允许健康探针
    - from:
        - podSelector: {}
      ports:
        - protocol: TCP
          port: 8081
```

### 4.10 监控与可观测性

#### 4.10.1 Prometheus 指标

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: neon-controlplane
  namespace: monitoring
spec:
  selector:
    matchLabels:
      control-plane: controller-manager
  endpoints:
    - port: http
      path: /metrics
      interval: 30s
  namespaceSelector:
    matchNames:
      - system
```

**关键指标**：

| 指标名 | 类型 | 描述 |
|--------|------|------|
| `neon_controlplane_request_total` | Counter | 请求总数 |
| `neon_controlplane_request_duration_seconds` | Histogram | 请求延迟分布 |
| `neon_controlplane_request_errors_total` | Counter | 错误请求数 |
| `neon_controlplane_active_connections` | Gauge | 当前活跃连接数 |
| `neon_controlplane_api_key_usage_total` | Counter | API Key 使用次数 |

#### 4.10.2 日志与审计

```yaml
# 日志格式（JSON 结构化）
apiVersion: v1
kind: ConfigMap
metadata:
  name: neon-controlplane-logging
  namespace: system
data:
  logging.yaml: |
    format: json
    level: info
    fields:
      app: neon-controlplane
      namespace: system
```

**审计日志字段**：

| 字段 | 描述 |
|------|------|
| `request_id` | 请求唯一标识 |
| `method` | HTTP 方法 |
| `path` | 请求路径 |
| `status` | HTTP 状态码 |
| `duration` | 请求耗时（ms） |
| `client_ip` | 客户端 IP |
| `api_key_id` | 使用的 API Key ID |
| `user_id` | 用户 ID |
| `org_id` | 组织 ID |
| `resource_id` | 操作的资源 ID |
| `error` | 错误信息（如有） |

---

## 五、渐进式实施路线图

### Phase 0（当前）：临时方案

| 任务 | 优先级 | 说明 |
|------|:---:|------|
| kubectl port-forward | P0 | 当前临时访问方式 |

### Phase 1（短期）：NodePort 基础暴露

| 任务 | 优先级 | 说明 |
|------|:---:|------|
| Service 类型变更 | P0 | ClusterIP → NodePort，配置固定端口 30881 |
| 外部 LB/VIP 配置 | P0 | 配置节点级负载均衡（可选） |
| Service 多副本 | P1 | 部署 3 副本，配置 HPA |
| 网络策略 | P1 | 限制内部访问，允许 NodePort 入口 |

### Phase 2（中期）：Ingress 安全增强

| 任务 | 优先级 | 说明 |
|------|:---:|------|
| Ingress 部署 | P0 | 配置域名和 TLS，暴露 `/api/v2` 路径 |
| TLS 证书配置 | P0 | 使用 cert-manager 或自签名证书 |
| API Key 认证 | P0 | 实现 API Key 生成、存储、验证 |
| Service 类型恢复 | P1 | NodePort → ClusterIP（通过 Ingress 暴露） |
| IP 白名单 | P1 | 通过 Ingress 注解配置 |
| 限流配置 | P1 | 配置请求速率和连接数限制 |
| 审计日志 | P1 | 实现结构化审计日志 |

### Phase 3（长期）：企业级能力

| 任务 | 优先级 | 说明 |
|------|:---:|------|
| API Gateway 集成 | P2 | 集成 Kong/APISIX/Ambassador |
| SSO 集成 | P2 | 支持 OIDC/SAML 认证 |
| RBAC 权限模型 | P2 | 细粒度资源访问控制 |
| 多租户隔离 | P2 | 组织级资源隔离 |

---

## 六、配置示例

### 6.1 Phase 1：NodePort 配置清单

```yaml
# control-plane-nodeport.yaml
apiVersion: v1
kind: Service
metadata:
  name: neon-controlplane
  namespace: neon
spec:
  type: NodePort
  ports:
    - name: http
      port: 8081
      targetPort: controlplane
      nodePort: 30881
      protocol: TCP
  selector:
    app: neon-controlplane
  externalTrafficPolicy: Local
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: neon-controlplane
  namespace: neon
spec:
  replicas: 3
  selector:
    matchLabels:
      app: neon-controlplane
  template:
    metadata:
      labels:
        app: neon-controlplane
    spec:
      containers:
        - name: controlplane
          image: neon-operator/controlplane:latest
          ports:
            - name: controlplane
              containerPort: 8082
          env:
            - name: WATCH_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
          resources:
            limits:
              cpu: 500m
              memory: 512Mi
            requests:
              cpu: 100m
              memory: 256Mi
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8082
            initialDelaySeconds: 15
            periodSeconds: 20
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8082
            initialDelaySeconds: 5
            periodSeconds: 10
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-controlplane-nodeport
  namespace: neon
spec:
  podSelector:
    matchLabels:
      app: neon-controlplane
  policyTypes:
    - Ingress
  ingress:
    - from:
        - ipBlock:
            cidr: 10.0.0.0/8
      ports:
        - protocol: TCP
          port: 8081
    - from:
        - podSelector:
            matchLabels:
              molnett.org/component: compute
      ports:
        - protocol: TCP
          port: 8081
    - from:
        - podSelector:
            matchLabels:
              molnett.org/component: storage-controller
      ports:
        - protocol: TCP
          port: 8081
```

### 6.2 Phase 1：NodePort 使用方式

```bash
# 部署 NodePort 配置
kubectl apply -f control-plane-nodeport.yaml

# 验证 Service 状态
kubectl get svc neon-controlplane -n neon
# 输出应显示: NodePort   10.x.x.x   <none>   8081:30881/TCP

# 直接访问（通过任意节点 IP）
curl -s http://<node-ip>:30881/api/v2/projects

# 通过外部 LB/VIP 访问（推荐）
curl -s http://<vip-ip>:30881/api/v2/projects

# 内部访问（Compute / Storage Controller）
# 继续使用 Service DNS: http://neon-controlplane.neon:8081
```

### 6.3 Phase 2：Ingress 配置清单

```yaml
# control-plane-ingress.yaml
apiVersion: v1
kind: Service
metadata:
  name: neon-controlplane
  namespace: neon
spec:
  type: ClusterIP
  ports:
    - name: http
      port: 8081
      targetPort: controlplane
  selector:
    app: neon-controlplane
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: neon-controlplane
  namespace: neon
  annotations:
    cert-manager.io/cluster-issuer: "selfsigning-issuer"
    nginx.ingress.kubernetes.io/whitelist-source-range: "10.0.0.0/8"
    nginx.ingress.kubernetes.io/limit-rps: "100"
    nginx.ingress.kubernetes.io/proxy-body-size: "10m"
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - api.neon.example.com
      secretName: neon-controlplane-tls
  rules:
    - host: api.neon.example.com
      http:
        paths:
          - path: /api/v2
            pathType: Prefix
            backend:
              service:
                name: neon-controlplane
                port:
                  number: 8081
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-controlplane-ingress
  namespace: neon
spec:
  podSelector:
    matchLabels:
      app: neon-controlplane
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              name: ingress-nginx
      ports:
        - protocol: TCP
          port: 8081
    - from:
        - podSelector:
            matchLabels:
              molnett.org/component: compute
      ports:
        - protocol: TCP
          port: 8081
    - from:
        - podSelector:
            matchLabels:
              molnett.org/component: storage-controller
      ports:
        - protocol: TCP
          port: 8081
```

### 6.4 Phase 2：Ingress 使用方式

```bash
# 升级到 Ingress 配置
kubectl apply -f control-plane-ingress.yaml

# 访问 API（生产环境）
curl -s -X POST "https://api.neon.example.com/api/v2/projects" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer admin_sk_abc123..." \
  -d '{
    "project": {
      "name": "my-app-db",
      "branch": {
        "name": "main",
        "role_name": "app_owner",
        "database_name": "appdb"
      }
    }
  }'

# 内部访问（Compute / Storage Controller）
# 继续使用 Service DNS: http://neon-controlplane.neon:8081
```

---

## 七、安全考量

### 7.1 风险矩阵

| 风险 | 概率 | 影响 | 缓解措施 |
|------|:---:|:---:|---------|
| API 无认证暴露 | 高 | 严重 | Phase 2 必须实施 API Key |
| TLS 证书过期 | 中 | 中等 | 使用 cert-manager 自动续期 |
| DDoS 攻击 | 中 | 中等 | Ingress 限流 + WAF |
| API Key 泄露 | 低 | 严重 | 定期轮换、最小权限原则 |
| 内部接口被外部访问 | 低 | 严重 | 网络策略 + Ingress 路径限制 |

### 7.2 安全最佳实践

1. **最小权限原则**：API Key 仅授予所需的最小权限范围
2. **定期轮换**：API Key 定期轮换（建议 90 天）
3. **敏感操作审计**：记录所有删除、修改操作
4. **速率限制**：防止暴力破解和 DDoS
5. **安全 Headers**：配置 HSTS、CSP、X-Frame-Options

---

## 八、总结

### 8.1 方案优势

| 维度 | 优势 |
|------|------|
| **安全性** | TLS 加密 + API Key 认证 + 网络策略隔离 |
| **可用性** | 多副本部署 + Ingress 负载均衡 |
| **可扩展性** | 渐进式实施，可叠加 API Gateway |
| **兼容性** | 与 Neon Cloud API v2 保持一致 |
| **运维友好** | 标准 K8s 资源，易于监控和管理 |

### 8.2 关键决策

1. **Ingress 作为主要暴露方式**：统一入口，支持 TLS 和路由规则
2. **API Key 作为认证方案**：简单易用，与 Neon Cloud 兼容
3. **内部/外部接口分离**：通过网络策略和 Ingress 路径限制实现
4. **渐进式实施**：先解决可达性，再增强安全性，最后添加企业级能力

### 8.3 后续工作

- Phase 1：完成基础暴露（Ingress + TLS + 多副本）
- Phase 2：实现 API Key 认证和访问控制
- Phase 3：集成 API Gateway 和企业级功能