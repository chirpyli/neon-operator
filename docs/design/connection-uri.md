# Connection URI API 设计方案

| 字段 | 内容 |
|------|------|
| 版本 | v1.0 |
| 日期 | 2026-07-01 |
| 状态 | Draft |
| 作者 | neon-operator team |

---

## 1. 概述

### 1.1 功能定位

`GET /api/v2/projects/{project_id}/connection_uri` 是 Neon API 中面向开发者最常用的端点之一。它为应用程序返回一个**开箱即用**的 PostgreSQL 连接字符串（connection URI），用户拷贝即可连接数据库，无需手动拼接 host、port、password 等信息。

### 1.2 对标上游

| 特性 | Neon Cloud (上游) | neon-operator (当前) | 目标 |
|------|-------------------|---------------------|------|
| 基本 URI 拼接 | ✅ | ✅ | ✅ |
| SSL 参数 `?sslmode=require` | ✅ | ❌ | P0 |
| Pooled 连接 (`-pooler` 后缀) | ✅ | ❌ (用 bool flag) | P0 |
| `connection_parameters` 返回 | ✅ | ❌ | P0 |
| 自定义主机名（类 `ep-xxx.region.aws.neon.tech`） | ✅ | ❌ (K8s 内部地址) | P1 |
| 多端点选择（read_only） | ✅ | ❌ (只选 read_write) | P1 |
| 连接 URI 缓存 | ✅ | ❌ | P2 |

---

## 2. 上游 Neon API 规范

### 2.1 请求

```
GET /projects/{project_id}/connection_uri?database_name={name}&role_name={name}[&branch_id={id}][&endpoint_id={id}][&pooled={true|false}]
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `project_id` | string (path) | 是 | 项目 ID |
| `database_name` | string (query) | 是 | 数据库名 |
| `role_name` | string (query) | 是 | 角色名（用户） |
| `branch_id` | string (query) | 否 | 分支 ID，不传则选默认分支 |
| `endpoint_id` | string (query) | 否 | 端点 ID，不传则选分支的 read_write 端点 |
| `pooled` | boolean (query) | 否 | `true` 返回连接池 URI（添加 `-pooler` 主机名后缀） |

### 2.2 响应

```json
{
  "uri": "postgresql://neondb_owner:password@ep-cool-darkness-a5u37xcv.us-east-2.aws.neon.tech/neondb?sslmode=require&channel_binding=require",
  "connection_parameters": {
    "host": "ep-cool-darkness-a5u37xcv.us-east-2.aws.neon.tech",
    "port": "5432",
    "database": "neondb",
    "user": "neondb_owner",
    "password": "password",
    "sslmode": "require",
    "channel_binding": "require"
  }
}
```

### 2.3 URI 格式

**直连 (Direct)**：
```
postgresql://{role_name}:{password}@{hostname}/{database_name}?sslmode=require&channel_binding=require
```

**连接池 (Pooled)**：
```
postgresql://{role_name}:{password}@{hostname}-pooler.{region}.aws.neon.tech/{database_name}?sslmode=require
```

**主机名格式**: `ep-{human_readable_id}.{region}.aws.neon.tech`

---

## 3. 当前实现分析 (`api_service.go:2026-2134`)

### 3.1 已有流程

```
Query Params → 确定 branch → 确定 endpoint → 读取 endpoint.Status.Host/Port → 查找密码 Secret → 拼接 URI
```

```go
uri := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s",
    roleName, password, host, port, databaseName)
```

### 3.2 缺陷

| # | 问题 | 严重程度 |
|---|------|---------|
| 1 | **无 `sslmode` 参数** — 生产环境 Postgres 必须启用 TLS | 🔴 P0 |
| 2 | **无 `connection_parameters`** — 官方 API 必返回的结构化字段，用于各语言驱动 dsn 拼接 | 🔴 P0 |
| 3 | **Pooled 为 bool 而非影响主机名** — 上游是通过 `-pooler` 后缀嵌入主机名，而非独立字段 | 🔴 P0 |
| 4 | **主机名使用 K8s Service DNS** (`ep-xxx.namespace.svc.cluster.local`) — 在集群外不可达 | 🟡 P1 |
| 5 | **无法指定 read_only 端点** — 只选 read_write，API 应尊重 `endpoint_id` 参数或支持角色选择 | 🟡 P1 |
| 6 | **多次全量 List 调用** — 每次请求都遍历所有 Role/Database/Endpoint CR，无缓存 | 🟢 P2 |
| 7 | **密码明文多次出现** — `generatePassword()` 密码直接写入响应、Secret、URI，缺少最小泄漏原则 | 🟢 P2 |

---

## 4. 详细设计

### 4.1 架构概览

```
┌─────────────────────────────────────────────────────────────┐
│                    API Handler Layer                         │
│  getConnectionURI()                                          │
│  ├── 解析 query params                                       │
│  ├── 调用 service 层                                         │
│  └── 返回 JSON                                              │
└─────────────────────┬───────────────────────────────────────┘
                      │
┌─────────────────────▼───────────────────────────────────────┐
│                    Service Layer                             │
│  GetConnectionURI(ctx, projectID, opts)                      │
│  ├── 1. Resolve Branch (default or explicit)                │
│  ├── 2. Resolve Endpoint (prefer read_write, explicit)      │
│  ├── 3. Validate Database exists                            │
│  ├── 4. Validate Role exists                                │
│  ├── 5. Read Password from Secret                           │
│  ├── 6. Build Hostname (external-facing)                    │
│  ├── 7. Build URI + connection_parameters                   │
│  └── 8. Apply Pooled transformation (hostname suffix)       │
└─────────────────────┬───────────────────────────────────────┘
                      │
┌─────────────────────▼───────────────────────────────────────┐
│                 Hostname Resolver                            │
│  ├── Mode: External  → LoadBalancer / Ingress Hostname      │
│  ├── Mode: Internal  → K8s Service DNS                       │
│  └── Mode: Pooled    → Append "-pooler" to hostname         │
└─────────────────────────────────────────────────────────────┘
```

### 4.2 数据模型

#### 4.2.1 Request (Go types)

```go
// ConnectionURIOptions 连接 URI 请求参数
type ConnectionURIOptions struct {
    DatabaseName string // query: database_name (required)
    RoleName     string // query: role_name (required)
    BranchID     string // query: branch_id (optional)
    EndpointID   string // query: endpoint_id (optional)
    Pooled       bool   // query: pooled (optional, default false)
}
```

#### 4.2.2 Response (更新)

```go
// ConnectionURIResponse 连接 URI 响应（对标 Neon API）
type ConnectionURIResponse struct {
    URI                  string            `json:"uri"`
    ConnectionParameters map[string]string `json:"connection_parameters"`
}
```

#### 4.2.3 ConnectionParameters 标准字段

| 字段 | 来源 | 示例 |
|------|------|------|
| `host` | Endpoint Status.Host | `ep-cool-a5u37xcv.us-east-1.example.com` |
| `port` | Endpoint Status.Port | `5432` |
| `database` | 请求参数 | `neondb` |
| `user` | 请求参数 | `neondb_owner` |
| `password` | Secret | `****` |
| `sslmode` | 配置 | `require` |
| `channel_binding` | 配置 | `require` |

### 4.3 核心实现

#### 4.3.1 Hostname 解析策略

```go
// HostnameMode 主机名模式
type HostnameMode string

const (
    HostnameModeExternal HostnameMode = "external" // LoadBalancer/Ingress 对外地址
    HostnameModeInternal HostnameMode = "internal" // K8s Service DNS 集群内地址
)

// resolveEndpointHostname 解析端点的对外主机名
//
// 优先级：
//   1. Endpoint Status.Host (由 Controller 从 Service/Ingress 写入)
//   2. Fallback: K8s Service DNS
func (s *apiService) resolveEndpointHostname(
    endpoint *neonv1.Endpoint,
    mode HostnameMode,
) (string, int32) {
    host := endpoint.Status.Host
    port := endpoint.Status.Port

    if host == "" {
        // Fallback: 集群内部地址
        host = fmt.Sprintf("%s.%s.svc.cluster.local", endpoint.Name, s.namespace)
        s.log.Warn("endpoint has no external hostname, falling back to cluster DNS",
            "endpointID", endpoint.Name)
    }
    if port == 0 {
        port = 5432
    }

    return host, port
}
```

#### 4.3.2 连接池主机名转换

```go
// applyPooledHostname 对主机名应用连接池转换
//
// 规则:
//   - 如果主机名包含已知域名后缀 (如 .neon.tech, .svc.cluster.local):
//     在主机名前插入 "pooler." 或追加 "-pooler" (视部署模式)
//   - 否则: 保持原主机名 + 独立 pooler 端口 (PgBouncer sidecar port)
//
// 对于 K8s 内部部署，推荐使用 PgBouncer sidecar 模式:
//   - 直连 → compute Service (port 5432)
//   - 连接池 → pgbouncer Service (port 6432)
func (s *apiService) applyPooledHostname(host string, port int32) (string, int32) {
    if s.poolerStrategy == PoolerStrategySidecar {
        // Sidecar 模式: 使用 PgBouncer 对应的 Service port
        return host, s.poolerPort // default 6432
    }
    // DNS 变换模式 (未来)
    return host, port
}
```

**Pooler 部署模式**:

| 模式 | 描述 | URI 变化 |
|------|------|---------|
| **Sidecar** (推荐) | PgBouncer 与 Compute Pod 同 Pod，独立 Service Port 6432 | `port:5432` → `port:6432` |
| **DNS Transform** | 主机名前加 `-pooler` 后缀，需外部 DNS 配置 | `ep-xxx.example.com` → `ep-xxx-pooler.example.com` |

#### 4.3.3 URI Builder

```go
// buildConnectionURI 构造完整的 PostgreSQL 连接 URI
//
// 格式:
//   postgresql://{user}:{password}@{host}:{port}/{database}?sslmode=require&channel_binding=require
//
// 特殊字符编码:
//   - user, password, database 中的特殊字符需要 URL 编码
//   - @ : / 等字符在密码中可能导致解析错误
func buildConnectionURI(params ConnectionParameters) string {
    // URL 编码用户、密码、数据库名
    user := url.QueryEscape(params.User)
    password := url.QueryEscape(params.Password)
    database := url.QueryEscape(params.Database)

    // 构建基础 URI
    uri := fmt.Sprintf("postgresql://%s:%s@%s:%s/%s",
        user, password, params.Host, params.Port, database)

    // 附加查询参数
    q := url.Values{}
    if params.SSLMode != "" {
        q.Set("sslmode", params.SSLMode)
    }
    if params.ChannelBinding != "" {
        q.Set("channel_binding", params.ChannelBinding)
    }
    if len(q) > 0 {
        uri += "?" + q.Encode()
    }

    return uri
}
```

#### 4.3.4 完整 service 方法

```go
// GetConnectionURI 构造数据库连接字符串（对标 Neon GET /connection_uri）
//
// 流程:
//  1. 校验 project_id
//  2. 确定 branch (显式指定 or 默认分支)
//  3. 确定 endpoint (显式指定 or branch 的 read_write)
//  4. 验证 endpoint 可达 (phase=active)
//  5. 验证 database 和 role 存在
//  6. 读取密码 Secret
//  7. 构造 URI + connection_parameters
//  8. 应用 Pooled 转换
func (s *apiService) GetConnectionURI(
    ctx context.Context,
    projectID string,
    opts ConnectionURIOptions,
) (*ConnectionURIResponse, error) {

    // ========================================
    // Step 1: 验证 Project 存在
    // ========================================
    project := &neonv1.Project{}
    if err := s.k8sClient.Get(ctx,
        client.ObjectKey{Name: projectID, Namespace: s.namespace}, project); err != nil {
        if isNotFound(err) {
            return nil, newError("PROJECT_NOT_FOUND", "project '"+projectID+"' not found")
        }
        return nil, fmt.Errorf("get project: %w", err)
    }

    // ========================================
    // Step 2: 确定 Branch
    // ========================================
    var branch *neonv1.Branch
    if opts.BranchID != "" {
        branch = &neonv1.Branch{}
        if err := s.k8sClient.Get(ctx,
            client.ObjectKey{Name: opts.BranchID, Namespace: s.namespace}, branch); err != nil {
            if isNotFound(err) {
                return nil, newError("BRANCH_NOT_FOUND", "branch '"+opts.BranchID+"' not found")
            }
            return nil, fmt.Errorf("get branch: %w", err)
        }
        if branch.Spec.ProjectID != projectID {
            return nil, newError("BRANCH_NOT_FOUND",
                "branch '"+opts.BranchID+"' not in project '"+projectID+"'")
        }
    } else {
        var err error
        branch, err = s.findDefaultBranch(ctx, projectID)
        if err != nil {
            return nil, err
        }
    }

    // ========================================
    // Step 3: 确定 Endpoint
    // ========================================
    var endpoint *neonv1.Endpoint
    if opts.EndpointID != "" {
        endpoint = &neonv1.Endpoint{}
        if err := s.k8sClient.Get(ctx,
            client.ObjectKey{Name: opts.EndpointID, Namespace: s.namespace}, endpoint); err != nil {
            if isNotFound(err) {
                return nil, newError("ENDPOINT_NOT_FOUND",
                    "endpoint '"+opts.EndpointID+"' not found")
            }
            return nil, fmt.Errorf("get endpoint: %w", err)
        }
        if endpoint.Spec.BranchID != branch.Name {
            return nil, newError("ENDPOINT_NOT_FOUND",
                "endpoint '"+opts.EndpointID+"' not in branch '"+branch.Name+"'")
        }
    } else {
        // 查找 read_write endpoint
        epList := &neonv1.EndpointList{}
        if err := s.k8sClient.List(ctx, epList,
            client.InNamespace(s.namespace)); err != nil {
            return nil, fmt.Errorf("list endpoints: %w", err)
        }
        for i := range epList.Items {
            if epList.Items[i].Spec.BranchID == branch.Name &&
                epList.Items[i].Spec.Type == "read_write" {
                endpoint = &epList.Items[i]
                break
            }
        }
        if endpoint == nil {
            return nil, newError("ENDPOINT_NOT_FOUND",
                "no read_write endpoint found for branch '"+branch.Name+"'")
        }
    }

    // ========================================
    // Step 4: Endpoint 状态检查
    // ========================================
    if endpoint.Spec.Disabled {
        return nil, newError("ENDPOINT_DISABLED",
            "endpoint '"+endpoint.Name+"' is disabled, start it first")
    }
    if endpoint.Status.Phase != "active" {
        s.log.Warn("endpoint not active, URI may not be functional",
            "endpointID", endpoint.Name,
            "phase", endpoint.Status.Phase)
        // 不阻止返回 URI，允许用户在启动过程中获取连接信息
    }

    // ========================================
    // Step 5 & 6: 验证 Database + Role，获取密码
    // ========================================
    if err := s.validateDatabaseExists(ctx, branch.Name, opts.DatabaseName); err != nil {
        return nil, err
    }

    password, err := s.getRolePassword(ctx, branch.Name, opts.RoleName)
    if err != nil {
        return nil, err
    }

    // ========================================
    // Step 7: 构造主机名
    // ========================================
    host, port := s.resolveEndpointHostname(endpoint, s.hostnameMode)

    // ========================================
    // Step 8: 构造 URI
    // ========================================
    params := ConnectionParameters{
        Host:            host,
        Port:            strconv.Itoa(int(port)),
        User:            opts.RoleName,
        Password:        password,
        Database:        opts.DatabaseName,
        SSLMode:         s.tlsConfig.SSLMode,         // "require"
        ChannelBinding:  s.tlsConfig.ChannelBinding,  // "require" (SCRAM channel binding)
    }

    // Pooled 转换
    if opts.Pooled {
        pooledHost, pooledPort := s.applyPooledHostname(host, port)
        params.Host = pooledHost
        params.Port = strconv.Itoa(int(pooledPort))
    }

    uri := buildConnectionURI(params)

    // ========================================
    // Step 9: 构造响应
    // ========================================
    return &ConnectionURIResponse{
        URI: uri,
        ConnectionParameters: map[string]string{
            "host":             params.Host,
            "port":             params.Port,
            "database":         params.Database,
            "user":             params.User,
            "password":         params.Password,
            "sslmode":          params.SSLMode,
            "channel_binding":  params.ChannelBinding,
            "branch_id":        branch.Name,
            "endpoint_id":      endpoint.Name,
            "project_id":       projectID,
        },
    }, nil
}
```

#### 4.3.5 辅助方法

```go
// validateDatabaseExists 验证数据库 CR 存在
func (s *apiService) validateDatabaseExists(
    ctx context.Context, branchID, dbName string,
) error {
    list := &neonv1.DatabaseList{}
    if err := s.k8sClient.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
        return fmt.Errorf("list databases: %w", err)
    }
    for _, d := range list.Items {
        if d.Spec.BranchID == branchID && d.Spec.Name == dbName {
            return nil
        }
    }
    return newError("DATABASE_NOT_FOUND",
        "database '"+dbName+"' not found in branch '"+branchID+"'")
}

// getRolePassword 从 Secret 获取角色密码
func (s *apiService) getRolePassword(
    ctx context.Context, branchID, roleName string,
) (string, error) {
    // 1. 找到 Role CR 以获取 SecretRef
    roleList := &neonv1.RoleList{}
    if err := s.k8sClient.List(ctx, roleList, client.InNamespace(s.namespace)); err != nil {
        return "", fmt.Errorf("list roles: %w", err)
    }
    var targetRole *neonv1.Role
    for i := range roleList.Items {
        if roleList.Items[i].Spec.BranchID == branchID &&
            roleList.Items[i].Spec.Name == roleName {
            targetRole = &roleList.Items[i]
            break
        }
    }
    if targetRole == nil {
        return "", newError("ROLE_NOT_FOUND", "role '"+roleName+"' not found")
    }

    // 2. 读取 Secret
    if targetRole.Status.PasswordSecretRef == nil {
        // 尝试命名约定 fallback
        secretName := fmt.Sprintf("role-%s-%s-password", branchID, roleName)
        targetRole.Status.PasswordSecretRef = &corev1.SecretReference{
            Name:      secretName,
            Namespace: s.namespace,
        }
    }

    secret := &corev1.Secret{}
    secretKey := client.ObjectKey{
        Name:      targetRole.Status.PasswordSecretRef.Name,
        Namespace: targetRole.Status.PasswordSecretRef.Namespace,
    }
    if secretKey.Namespace == "" {
        secretKey.Namespace = s.namespace
    }
    if err := s.k8sClient.Get(ctx, secretKey, secret); err != nil {
        return "", newError("ROLE_NOT_FOUND",
            "password secret for role '"+roleName+"' not found")
    }

    if pw, ok := secret.Data["password"]; ok {
        return string(pw), nil
    }
    return "", newError("ROLE_NOT_FOUND",
        "password not found in secret for role '"+roleName+"'")
}
```

### 4.4 TLS/SSL 配置

```go
// TLSConfig TLS 连接配置
type TLSConfig struct {
    // SSLMode PostgreSQL SSL 模式
    // "disable" | "allow" | "prefer" | "require" | "verify-ca" | "verify-full"
    // 生产环境: "require" (TLS 加密, 不验证 CA)
    // 更高安全: "verify-full" (验证证书链 + 主机名)
    SSLMode string `json:"sslMode"` // default: "require"

    // ChannelBinding SCRAM channel binding
    // "" | "require" | "prefer" | "disable"
    // "require": 启用 channel binding, 防止 MITM 攻击
    ChannelBinding string `json:"channelBinding"` // default: "require"

    // CACertificate CA 证书 (用于 verify-full 模式)
    // +optional
    CACertificate string `json:"caCertificate,omitempty"`
}

// DefaultTLSConfig 默认生产级 TLS 配置
var DefaultTLSConfig = TLSConfig{
    SSLMode:        "require",
    ChannelBinding: "require",
}
```

### 4.5 Pooler 配置

```go
// PoolerStrategy 连接池策略
type PoolerStrategy string

const (
    PoolerStrategySidecar     PoolerStrategy = "sidecar"      // PgBouncer sidecar (推荐)
    PoolerStrategyStandalone  PoolerStrategy = "standalone"   // 独立 PgBouncer Deployment
    PoolerStrategyDNS         PoolerStrategy = "dns"          // DNS 变换 (-pooler 后缀)
)

// PoolerConfig 连接池配置
type PoolerConfig struct {
    Enabled  bool          `json:"enabled"`
    Strategy PoolerStrategy `json:"strategy"`
    Port     int32         `json:"port"` // default: 6432
}
```

### 4.6 错误处理

| 错误码 | HTTP Status | 场景 |
|--------|-------------|------|
| `PROJECT_NOT_FOUND` | 404 | Project 不存在 |
| `BRANCH_NOT_FOUND` | 404 | Branch 不存在或不属于 Project |
| `ENDPOINT_NOT_FOUND` | 404 | Endpoint 不存在或不属于 Branch |
| `ENDPOINT_DISABLED` | 409 | Endpoint 已禁用, 需先 start |
| `DATABASE_NOT_FOUND` | 404 | Database 不存在 |
| `ROLE_NOT_FOUND` | 404 | Role 不存在或密码 Secret 缺失 |
| `DEFAULT_BRANCH_NOT_FOUND` | 404 | 无默认分支且未指定 branch_id |

---

## 5. 实施路线图

### Phase 1: P0 — 生产可用（1-2 天）

```
├── 1.1 添加 sslmode 和 channel_binding 到 URI ✅
├── 1.2 添加 connection_parameters 到响应 ✅
├── 1.3 URI 特殊字符 URL 编码 ✅
├── 1.4 完善 pooled 逻辑（PgBouncer sidecar port 切换）✅
└── 1.5 Endpoint 状态检查 (disabled/phase) ✅
```

**具体改动**：
- `api_types.go`: 更新 `ConnectionURIResponse` 增加 `ConnectionParameters`
- `api_service.go`: 重构 `GetConnectionURI` 方法
- `api_routes.go`: handler 不需要改动（路径正确）

### Phase 2: P1 — 优化体验（2-3 天）

```
├── 2.1 支持自定义 External Hostname Pattern
│      (配置 ep-{name}.{region}.{domain} 模式)
├── 2.2 支持 ReadOnly endpoint 选择
├── 2.3 Branch 级别 connection_uri 端点
│      GET /.../branches/{bid}/connection_uri
└── 2.4 Database 级别 connection_uri 端点
       GET /.../branches/{bid}/databases/{name}/connection_uri
```

### Phase 3: P2 — 缓存与安全（1-2 天）

```
├── 3.1 Endpoint Hostname 缓存 (减少 K8s API 调用)
├── 3.2 密码读取限流 (防止暴力读取 Secret)
├── 3.3 连接 URI 审计日志
└── 3.4 密码最小泄漏 (secret 引用优先于明文)
```

---

## 6. 对比当前代码变更清单

### 6.1 `api_types.go` 变更

```diff
- type ConnectionURIResponse struct {
-     URI          string `json:"uri"`
-     Pooled       bool   `json:"pooled"`
-     DatabaseName string `json:"database_name"`
-     RoleName     string `json:"role_name"`
-     BranchID     string `json:"branch_id"`
-     EndpointID   string `json:"endpoint_id"`
- }
+ type ConnectionURIResponse struct {
+     URI                  string            `json:"uri"`
+     ConnectionParameters map[string]string `json:"connection_parameters"`
+ }
```

### 6.2 `api_service.go` 变更

| 当前 | 目标 |
|------|------|
| 一个 `GetConnectionURI` 方法 ~110 行 | 拆分为 `GetConnectionURI` (~60 行) + 辅助方法 |
| `fmt.Sprintf("postgresql://%s:%s@%s:%d/%s")` | `buildConnectionURI(params)` — 含 URL 编码 + SSL 参数 |
| `pooled` 仅存 bool 不改变 URI | Pooled 时修改 port (6432) 或 hostname (-pooler) |
| 无 endpoint 健康检查 | 检查 `Disabled` 和 `Phase` |

---

## 7. 安全考量

| 风险 | 缓解措施 |
|------|---------|
| 密码在 URI 明文 | `sslmode=require` 确保传输加密；RBAC 限制 API 访问 |
| 密码在日志泄漏 | Handler 日志不打印 URI；Service 日志使用 masked password |
| URI 被中间人截获 | `channel_binding=require` 防止 MITM 重放 |
| 内部地址泄漏到外部 | `hostnameMode` 控制对外暴露策略 |
| 暴力枚举连接信息 | Rate limiting on `/connection_uri` endpoint |

---

## 8. 测试计划

### 8.1 单元测试

- `TestBuildConnectionURI` — URI 格式正确性
- `TestBuildConnectionURI_SpecialChars` — 特殊字符 URL 编码
- `TestBuildConnectionURI_SSLParams` — SSL 参数正确附加
- `TestBuildConnectionURI_Pooled` — Pooled 端口/主机名转换
- `TestGetConnectionURI_DefaultBranch` — 未指定 branch 时用默认
- `TestGetConnectionURI_ExplicitEndpoint` — 指定 read_only endpoint
- `TestGetConnectionURI_EndpointDisabled` — 禁用端点返回 409
- `TestGetConnectionURI_RoleNotFound` — 角色不存在返回 404

### 8.2 集成测试

- 创建 Project → Branch → Endpoint → Role → Database 后调 connection_uri
- 验证返回的 URI 可被 `psql` 连接
- 验证 `pooled=true` 返回正确端口

---

## 9. 参考

- [Neon API: Get Connection URI](https://api-docs.neon.tech/reference/getconnectionuri.md)
- [PostgreSQL Connection String Docs](https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-CONNSTRING)
- [SCRAM Channel Binding](https://www.postgresql.org/docs/current/sasl-authentication.html#SASL-SCRAM-SHA-256)
- [Neon Proxy Architecture](https://github.com/neondatabase/neon/tree/main/proxy)
- [neon-operator Connection URI 实现](internal/controlplane/api_service.go:2026-2134)
