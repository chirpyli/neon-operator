# JWT 认证体系

## 一、JWT 概述

JWT（JSON Web Token）是一种紧凑、自包含的开放标准（RFC 7519），用于在各方之间安全地传输信息。信息经过数字签名（或加密），可以被验证和信任。

**结构**：由三部分组成，用点号分隔：`Header.Payload.Signature`
- **Header**：声明类型（JWT）和签名算法（如 HMAC SHA256 或 RSA）。
- **Payload**：包含声明（Claims），比如用户 ID、角色、过期时间等，可自定义。
- **Signature**：对头部和载荷的签名，防止篡改。

**工作流程**：
- 用户登录后，认证服务器生成 JWT 并返回给客户端。
- 客户端在后续请求中附带 JWT（通常在 Authorization 头里：`Bearer <token>`）。
- 服务端用公钥/密钥验证签名，从载荷中提取用户信息，授予相应权限。

**为什么用 JWT 而不是传统 Session？**
- **无状态**：认证信息自包含在 Token 里，服务端无需维护会话存储。这对水平弹性伸缩的 Compute 节点至关重要——新扩出来的 Pod 不需要访问共享 Redis 就能验证用户。
- **可传递**：Token 可以在微服务间安全传递，携带身份和权限，无需反复查询认证中心。
- **跨域友好**：适合移动端、单页应用和跨服务调用。

---

## 二、JWTManager 源码分析

> 源码：`utils/jwtmanager.go`

`JWTManager` 是 neon-operator 中 JWT 操作的核心组件，负责密钥管理、Token 签发/验证、公钥分发。

### 2.1 数据结构

```go
type JWTManager struct {
    privateKey ed25519.PrivateKey
    publicKey  ed25519.PublicKey
}
```

- 使用 **Ed25519** (EdDSA) 签名算法，属于非对称加密
- 私钥用于签发 Token，公钥用于验证 Token
- 相比 RSA/ECDSA，Ed25519 具有更短的密钥长度和更高的签名/验证性能

### 2.2 密钥加载：NewJWTManagerFromSecret

从 Kubernetes Secret 中加载 PEM 格式的密钥对：

```go
func NewJWTManagerFromSecret(secret *corev1.Secret) (*JWTManager, error)
```

**加载流程**：
1. 从 `secret.Data["private.pem"]` 读取私钥 PEM
2. 从 `secret.Data["public.pem"]` 读取公钥 PEM
3. 使用 `pem.Decode` 解码 PEM 块
4. 使用 `x509.ParsePKCS8PrivateKey` 解析私钥 → 断言为 `ed25519.PrivateKey`
5. 使用 `x509.ParsePKIXPublicKey` 解析公钥 → 断言为 `ed25519.PublicKey`
6. 返回 `&JWTManager{privateKey, publicKey}`

**错误处理**：对以下 6 种异常情况分别返回明确错误：
- 缺少 `private.pem` 或 `public.pem`
- PEM 解码失败
- PKCS8/PKIX 解析失败
- 密钥类型不是 Ed25519

### 2.3 Token 签发：GenerateToken

```go
func (jm *JWTManager) GenerateToken(claims map[string]any) (string, error)
```

- 使用 `github.com/lestrrat-go/jwx/v3/jwt` 库构建 Token
- 遍历 claims map，逐个设置到 Token 中
- 使用 `jwa.EdDSA()` 算法 + 私钥签名
- 返回签名字符串（`[]byte` → `string`）

### 2.4 便捷签发方法

```go
// 签发指定 scope 的通用 Token（如 "admin"、"generations_api"）
func (jm *JWTManager) GenerateScopeToken(subject, scope string, lifetime time.Duration) (string, error)

// 签发 PS → SC upcall 专用 Token（scope=generations_api）
func GenerateUpcallToken(jm *JWTManager, clusterName string) (string, error)

// 签发 PS → SK WAL 认证专用 Token（scope=safekeeperdata）
func GenerateSafekeeperToken(jm *JWTManager, clusterName string) (string, error)
```

`GenerateScopeToken` 是通用方法，通过 SHA256(private_key+cluster+scope) 确定性计算 `exp`，确保相同输入始终生成相同 token。`GenerateUpcallToken` 和 `GenerateSafekeeperToken` 封装了该通用方法，固定 scope 和 10 年（3650 天）生命周期。

### 2.5 Token 过期检测：IsTokenExpired

```go
func (jm *JWTManager) IsTokenExpired(tokenString string) bool
```

- 解码 JWT payload（不验证签名），提取 `exp` claim
- 与当前时间比较判断是否已过期
- 用于 `ensureComponentTokens` 中检测持久化 token 是否需要重新签发
- 对格式错误或缺少 `exp` 的 token 视为已过期，保证安全

### 2.6 Token 验证：VerifyToken

```go
func (jm *JWTManager) VerifyToken(tokenString string) (jwt.Token, error)
```

- 使用 `jwt.Parse` 解析 Token 字符串
- 传入 `jwa.EdDSA()` + 公钥进行签名验证
- 返回解析后的 `jwt.Token` 对象，可读取其中的 claims

### 2.7 公钥分发：ToJWK

```go
func (jm *JWTManager) ToJWK() *JWK
```

将 Ed25519 公钥转换为 **JWK (JSON Web Key)** 格式（RFC 8037），用于分发给其他组件验证 Token：

| JWK 字段    | 值                    | 说明                            |
|-------------|-----------------------|---------------------------------|
| `use`       | `"sig"`               | 密钥用途：签名                   |
| `key_ops`   | `["verify"]`          | 允许的操作：仅验证               |
| `alg`       | `"EdDSA"`             | 签名算法                        |
| `kid`       | `"neon-operator"`     | 密钥 ID，用于标识密钥            |
| `kty`       | `"OKP"`               | 密钥类型：Octet Key Pair         |
| `crv`       | `"Ed25519"`           | 椭圆曲线名称                     |
| `x`         | Base64RawURL(公钥)    | 公钥的 Base64URL（无填充）编码   |

`JWKResponse` 是 JWK 集合的包装结构，符合 JWKS (JWK Set) 标准：
```go
type JWKResponse struct {
    Keys []*JWK `json:"keys"`
}
```

---

## 三、完整数据流

```
                        ┌──────────────────────────────┐
                        │  K8s Secret                  │
                        │  Name: cluster-{c}-jwt        │
                        │  ├── private.pem              │
                        │  ├── public.pem               │
                        │  ├── pageserver_token         │ (admin scope)
                        │  ├── control_plane_token      │ (admin scope)
                        │  ├── safekeeper_token         │ (admin scope)
                        │  ├── pageserver_control_plane │ (generations_api)
                        │  └── pageserver_safekeeper    │ (safekeeperdata)
                        └────────┬─────────────────────┘
                                 │
                    NewJWTManagerFromSecret()
                                 │
                        ┌────────▼─────────────┐
                        │    JWTManager        │
                        │  - privateKey        │
                        │  - publicKey         │
                        └──┬──────────────┬────┘
                           │              │
              ┌────────────▼───┐   ┌──────▼──────────┐
              │  GenerateToken │   │    ToJWK()       │
              │  (签发Token)    │   │  (导出公钥JWK)    │
              └───┬───┬───┬────┘   └──────┬───────────┘
                  │   │   │               │
     ┌────────────▼┐  │   │   ┌───────────▼──────────┐
     │ PS Token    │  │   │   │  JWKResponse         │
     │ (generation │  │   │   │  → ConfigMap          │
     │  _api)      │  │   │   │  → compute_ctl        │
     └──────┬──────┘  │   │   └──────────────────────┘
            │         │   │
  ┌─────────▼────┐   │   ┌▼──────────────────────────┐
  │ PS → SK Token│   │   │ 组件 Token (admin scope)    │
  │ (safekeeper  │   │   │ → SC env: PAGESERVER_TOKEN │
  │  data)       │   │   │ → SC env: CONTROL_PLANE_   │
  └──────────────┘   │   │          TOKEN             │
                     │   │ → SC env: SAFEKEEPER_TOKEN │
                     │   └────────────────────────────┘
                     │
          ┌──────────▼──────────────┐
          │  Bearer Token           │
          │  → HTTP /configure      │
          │    Authorization头       │
          └─────────────────────────┘
```

### 3.0 路径 0：Token 持久化到 Secret（核心机制）

为避免每次 reconcile 重新签发 token 导致 StatefulSet/Deployment 频繁滚动更新，所有组件 Token **首次生成后持久化到 JWT Secret**，后续 reconcile 直接复用：

```
ClusterReconciler.reconcileStorageController()
  → ensureComponentTokens()
    1. 从 Secret 读取 pageserver_token / control_plane_token / safekeeper_token
    2. 调用 IsTokenExpired() 检查每个 token 的 exp 是否已过期
    3. 若全部存在且未过期 → 直接返回，跳过生成
    4. 若缺失或过期 → GenerateScopeToken(clusterName, scope, 3650d)
    5. 写回 Secret 对应 key

PageserverReconciler.createOrUpdate()
  → ensurePSAuthTokens()
    1. 从 Secret 读取 pageserver_control_plane_token / pageserver_safekeeper_token
    2. 调用 IsTokenExpired() 检查每个 token 的 exp 是否已过期
    3. 若全部存在且未过期 → 直接返回，跳过生成
    4. 若缺失或过期 → GenerateUpcallToken() / GenerateSafekeeperToken()
    5. 写回 Secret 对应 key
```

> **设计要点**：Token 不写入 Operator 日志，而是存储在 Secret 中。验证时应 `kubectl get secret cluster-{name}-jwt` 检查。

### 3.1 路径 A：公钥注入 ConfigMap（启动时）

`specs/compute/configmap.go:ConfigMap()` 在 Branch Reconciler 创建子资源时调用：

```
Branch Reconciler 读取 Secret(cluster-{c}-jwt)
  → NewJWTManagerFromSecret()
  → ToJWK()
  → 序列化 JWKResponse 写入 ConfigMap Data["spec.json"]
  → ConfigMap 挂载到 Compute Pod
  → compute_ctl 读取 spec.json 获取公钥
  → 用于验证后续 HTTP 请求中的 Bearer Token
```

### 3.2 路径 B：私钥签发 Token（运行时）

`specs/compute/spec.go:postComputeSpec()` 在调用 Compute 的 `/configure` 端点时执行：

```
postComputeSpec()
  → 读取 Secret(cluster-{c}-jwt)
  → NewJWTManagerFromSecret()
  → GenerateToken(claims)
     claims = {
       "compute_id": "<compute-id>",
       "aud":        "compute",
       "roles":      ["compute_ctl:admin"],
       "exp":        now + 1h,
       "iat":        now,
       "iss":        "neon-operator",
       "sub":        "<compute-id>",
     }
  → HTTP POST http://{compute-id}-admin.neon:3080/configure
      Authorization: Bearer <token>
      Content-Type: application/json
      Body: ComputeSpecResponse JSON
```

**Claims 字段含义**：

| Claim        | 值                     | 说明                          |
|--------------|------------------------|-------------------------------|
| `compute_id` | Compute Pod 的 ID      | 标识目标 Compute 实例           |
| `aud`        | `"compute"`            | 接收方标识                     |
| `roles`      | `["compute_ctl:admin"]`| Compute 控制端的管理员角色      |
| `exp`        | `now + 1h`             | Token 有效期 1 小时            |
| `iat`        | `now`                  | Token 签发时间                 |
| `iss`        | `"neon-operator"`      | Token 签发者                   |
| `sub`        | `<compute-id>`         | Token 主体（与 compute_id 相同）|

---

## 四、密钥与 Token 管理

### 4.1 Secret 命名规范

Secret 命名模式：`cluster-{clusterName}-jwt`，存储在集群命名空间。

```go
secretName := fmt.Sprintf("cluster-%s-jwt", clusterName)
```

### 4.2 Secret 完整结构

| Key | 类型 | 用途 |
|-----|------|------|
| `private.pem` | Ed25519 私钥 PEM | Operator 签发所有 Token |
| `public.pem` | Ed25519 公钥 PEM | 所有组件挂载用于验证 |
| `pageserver_token` | JWT (admin) | SC → PS 认证 |
| `control_plane_token` | JWT (admin) | SC → CP 认证 |
| `safekeeper_token` | JWT (admin) | SC → SK 认证 |
| `pageserver_control_plane_token` | JWT (generations_api) | PS → SC upcall |
| `pageserver_safekeeper_token` | JWT (safekeeperdata) | PS → SK WAL 认证 |

### 4.3 密钥格式

| Key | 内容格式 |
|-----|----------|
| `private.pem` | `x509.MarshalPKCS8PrivateKey` → PEM 编码 |
| `public.pem` | `x509.MarshalPKIXPublicKey` → PEM 编码 |

### 4.4 测试密钥生成

`test/fixtures/jwt.go:NewJWTSecret()` 提供测试用密钥工厂：

```go
func NewJWTSecret(clusterName, namespace string) (*corev1.Secret, error) {
    pub, priv, _ := ed25519.GenerateKey(rand.Reader)
    // ... Marshal + PEM Encode ...
    return &corev1.Secret{...}, nil
}
```

每次调用生成全新的随机密钥对，确保测试隔离。

---

## 五、测试覆盖

`utils/jwtmanager_test.go` 覆盖 6 个测试场景：

| 测试用例                              | 覆盖功能                     |
|---------------------------------------|------------------------------|
| `TestNewJWTManagerFromSecret`         | 4 种情况：正常/缺私钥/缺公钥/PEM 无效 |
| `TestGenerateAndVerifyToken`          | 签发→验证往返，含标准 claims 检查  |
| `TestVerifyToken_InvalidSignature`    | 不同密钥对验证应失败           |
| `TestToJWK`                           | JWK 各字段正确性 + JSON 序列化 |
| `TestGenerateToken_EmptyClaims`       | 空 claims 也能正常签发/验证     |
| `TestVerifyToken_InvalidToken`        | 4 种无效 token 字符串均正确报错 |

---

## 六、依赖库

- **[lestrrat-go/jwx](https://github.com/lestrrat-go/jwx)**：JWT/JWK/JWS/JWE 的 Go 实现
  - `jwx/v3/jwa`：算法常量（`EdDSA()`）
  - `jwx/v3/jwt`：Token 构建、签名、解析

---

## 七、设计要点总结

1. **Ed25519 非对称签名**：私钥仅 operator 持有，公钥通过 ConfigMap 分发到 Compute Pod，实现"签发/验证"分离
2. **PEM 编码存储**：使用标准 PKCS8/PKIX + PEM 格式存储在 K8s Secret 中，兼容标准密钥管理工具
3. **JWK 标准分发**：公钥以 RFC 8037 OKP 格式分发，任何支持 JWK 的验证方都能直接使用
4. **短时效运行时 Token**：Compute 配置 Token 有效期仅 1 小时，减少泄露风险
5. **长时效组件 Token 持久化 + 过期自愈**：组件间通信 Token（admin / generations_api / safekeeperdata）首次生成后持久化到 JWT Secret，通过 `IsTokenExpired()` 检测过期并自动重新签发，生命周期 10 年（3650 天）。既避免每次 reconcile 重新签发导致 Deployment/StatefulSet 频繁滚动更新，又防止确定性 exp 落入已过期时间窗口导致认证死锁
6. **双路径使用**：启动时注入 JWK（ConfigMap 挂载），运行时签发 Bearer Token（HTTP 调用），两者共用同一个 JWTManager