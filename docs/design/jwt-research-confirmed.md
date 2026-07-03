# JWT 上游源码调研确认报告

> 基于对 neon 上游源码的深度调研，确认此前设计文档中所有待确认项的实际答案。
> 调研日期：2026-07-01

---

## 1. 核心结论速览

| # | 待确认项 | 结论 | 影响评估 |
|---|---------|------|---------|
| 1 | Scope 序列化格式 | `lowercase` 连写，非 snake_case | 🔴 **当前 operator scope 格式完全错误** |
| 2 | Scope 是单值还是数组 | **单值**，Admin 是万能 scope | 🔴 `"infra admin"` 无法被解析 |
| 3 | SC 健康端点 JWT 豁免 | ✅ `/status`, `/live`, `/ready` 均在 allowlist | 🟢 移 `--dev` 后探针安全 |
| 4 | PS 健康端点 JWT 豁免 | ✅ `/v1/status` 在 allowlist | 🟢 探针安全 |
| 5 | SK 健康端点 JWT 豁免 | ✅ `/v1/status` 在 allowlist | 🟢 探针安全 |
| 6 | SwappableJwtAuth 热加载 | **无文件监听**，仅 PS 有手动 reload API | 🟡 不影响基础功能，轮换需额外设计 |
| 7 | Pageserver config 字段名 | `http_auth_type`, `pg_auth_type`, `grpc_auth_type`, `auth_validation_public_key_path`, `control_plane_api_token` | 🟢 字段名已确认 |
| 8 | Safekeeper CLI 参数 | `--pg-auth-public-key-path`, `--http-auth-public-key-path` | 🟢 与设计文档一致 |

---

## 2. 详细调研结果

### 2.1 Scope 序列化格式 [🔴 关键发现]

**上游定义** (`libs/utils/src/auth.rs`):

```rust
#[derive(Debug, Serialize, Deserialize, Clone, Copy, PartialEq)]
#[serde(rename_all = "lowercase")]
pub enum Scope {
    Tenant,
    TenantEndpoint,
    PageServerApi,
    SafekeeperData,
    #[serde(rename = "generations_api")]
    GenerationsApi,
    Admin,
    Infra,
    Scrubber,
    #[serde(rename = "controller_peer")]
    ControllerPeer,
}
```

**关键发现**：`#[serde(rename_all = "lowercase")]` 的效果是**全部字符转为小写后连写**，**不是** `snake_case`。

| 枚举变体 | 实际序列化值 | 错误假设 | 正确否 |
|---------|------------|---------|:---:|
| `Tenant` | `"tenant"` | - | ✅ |
| `TenantEndpoint` | `"tenantendpoint"` | `"tenant_endpoint"` | ❌ |
| `PageServerApi` | `"pageserverapi"` | `"page_server_api"` | ❌ |
| `SafekeeperData` | `"safekeeperdata"` | `"safekeeper_data"` | ❌ |
| `GenerationsApi` | `"generations_api"` | `"generationsapi"` | ⚠️ 显式覆盖 |
| `Admin` | `"admin"` | - | ✅ |
| `Infra` | `"infra"` | - | ✅ |
| `Scrubber` | `"scrubber"` | - | ✅ |
| `ControllerPeer` | `"controller_peer"` | `"controllerpeer"` | ⚠️ 显式覆盖 |

**上游 Python 测试代码确认** (`test_runner/fixtures/auth_tokens.py`):

```python
class TokenScope(StrEnum):
    ADMIN = "admin"
    PAGE_SERVER_API = "pageserverapi"      # ← 全小写连写
    GENERATIONS_API = "generations_api"     # ← 下划线分隔（显式 rename）
    SAFEKEEPER_DATA = "safekeeperdata"       # ← 全小写连写
    TENANT = "tenant"
    SCRUBBER = "scrubber"
    INFRA = "infra"
```

---

### 2.2 Scope 是单值，不是数组 [🔴 关键发现]

**Claims 结构体定义** (`libs/utils/src/auth.rs`):

```rust
pub struct Claims {
    pub tenant_id: Option<TenantId>,
    pub endpoint_id: Option<Uuid>,
    pub scope: Scope,   // ← 单值，不是 Vec<Scope>
}
```

**结论**：每个 JWT token 只能有一个 scope。不存在"多个 scope 的组合"。

#### 当前 Operator 的问题

`sc_client.go:223-229` 生成的 token：
```go
claims := map[string]any{
    "scope": "infra admin",  // ← 错误！这既不是 "infra" 也不是 "admin"
}
```

上游 Rust 的反序列化器尝试将 `"infra admin"` 解析为 `Scope` 枚举时会**失败**，因为在 `lowercase` 映射下没有任何枚举变体匹配 `"infra admin"`。

#### 正确的做法

Storage Controller 各端点分别需要什么 scope：

| SC 端点 | 需要 Scope | 正确序列化值 |
|---------|-----------|------------|
| `/control/v1/safekeeper/*` (upsert, get, list) | `Scope::Infra` | `"infra"` |
| `/control/v1/node` (register)+ scheduling_policy 等管理操作 | `Scope::Admin` | `"admin"` |
| `/upcall/v1/*` (re-attach, validate 等) | `Scope::GenerationsApi` | `"generations_api"` |
| `/v1/tenant/*` (pageserver 代理) | `Scope::PageServerApi` | `"pageserverapi"` |
| `/control/v1/step_down` (SC 节点间) | `Scope::ControllerPeer` | `"controller_peer"` |
| metadata health (scrubber) | `Scope::Scrubber` | `"scrubber"` |

**Admin 万能钥匙机制** (`storage_controller/src/http.rs`):

```rust
fn check_permissions(request: &Request<Body>, required_scope: Scope) -> Result<(), ApiError> {
    check_permission_with(request, |claims| {
        match crate::auth::check_permission(claims, required_scope) {
            Err(e) => match crate::auth::check_permission(claims, Scope::Admin) {
                Ok(()) => Ok(()),  // Admin 访问任意端点都通过
                Err(_) => Err(e),
            },
            Ok(()) => Ok(()),
        }
    })
}
```

**这意味着**：如果 Operator 为所有 SC 请求都签 `"admin"` scope 的 token，则所有端点都可通过。这样实现最简单，但安全粒度不够细。推荐拆分：
- Safekeeper 注册/查询 → `"infra"` scope
- Pageserver 节点管理等 → `"admin"` scope

---

### 2.3 健康端点 JWT 豁免 [🟢 已确认安全]

#### Storage Controller

`make_router` 函数中，路由注册**之前**先添加 JWT middleware，然后在 middleware 回调中通过 `allowlist_routes` 跳过特定路径：

```rust
const allowlist_routes: &[&str] = &[
    "/status", "/live", "/ready",
    "/metrics", "/profile/cpu", "/profile/heap",
];
```

```
middleware 回调逻辑:
  路径在 allowlist_routes 中 → None (不验证 JWT)
  路径不在 allowlist_routes 中 → Some(auth) (需要 JWT)
```

**结论**：移除 `--dev` 后，SC 的健康探针 `/status`, `/live`, `/ready` **不需要 JWT 认证**，K8s 探针可直接使用，无需修改。

#### Pageserver

```rust
const allowlist_routes: &[&str] = &[
    "/v1/status", "/v1/doc", "/swagger.yml",
    "/metrics", "/profile/cpu", "/profile/heap",
];
```

**结论**：PS 的 `/v1/status` 在 allowlist 中，不需要 JWT。K8s 探针安全。

#### Safekeeper

```rust
const ALLOWLIST_ROUTES: &[&str] = &[
    "/v1/status", "/metrics", "/profile/cpu", "/profile/heap",
];
```

**结论**：SK 的 `/v1/status` 在 allowlist 中，不需要 JWT。K8s 探针安全。

**总结**：所有三个组件的健康端点都在 JWT middleware 的 allowlist 中，移除 `--dev` 后探针不会受影响。

---

### 2.4 SwappableJwtAuth 热加载机制 [🟡 无自动文件监听]

#### 核心实现

```rust
pub struct SwappableJwtAuth(ArcSwap<JwtAuth>);
```

使用 `arc_swap::ArcSwap` 实现无锁原子交换：
- `swap(new_jwt_auth)`: 原子替换
- `decode(token)`: 使用当前最新的 key 解码

#### 关键结论：没有自动文件监听器

- **没有** `inotify`/`notify`/`tokio::fs::watch` 等文件系统监听
- **没有** 定期轮询重载
- 只有 Pageserver 暴露了 `POST /v1/reload_auth_validation_keys` 手动重载端点
- Storage Controller 和 Safekeeper **没有** reload API

#### 各组件对比

| 组件 | 使用 SwappableJwtAuth | 热加载端点 | 备注 |
|------|:---:|:---:|------|
| Pageserver (HTTP) | ✅ | `POST /v1/reload_auth_validation_keys` | 唯一支持热加载 |
| Pageserver (PG) | ✅ | 同上（共享同一实例） | - |
| Pageserver (gRPC) | ✅ | 同上 | - |
| Safekeeper (HTTP) | ✅ | ❌ 无 | 用了 SwappableJwtAuth 但不暴露 reload |
| Safekeeper (PG) | ❌ 普通 `Arc<JwtAuth>` | ❌ | 完全不支持热加载 |
| Storage Controller | ✅ | ❌ 无 | 支持 swap 但不暴露 reload |

#### 对密钥轮换的影响

由于各组件的 JWT 公钥通过 K8s Secret Volume 挂载，Secret 更新后：
- **K8s kubelet 会在 ~60s 内同步 Secret Volume 内容**
- 但组件若不重载，仍使用旧 key

**短期方案**（Phase 2）：密钥不轮换，或轮换时滚动更新 Pod
**中期方案**（Phase 3+）：为 SC 和 SK 添加 `/reload_auth_validation_keys` 端点（参考 PS 实现），Operator 在更新 Secret 后调用

---

### 2.5 Pageserver JWT 配置精确字段名 [🟢 已确认]

#### ConfigMap / pageserver.toml 字段

```toml
# ===== 认证配置 =====
http_auth_type = "NeonJWT"                       # HTTP 管理 API 认证类型
pg_auth_type = "NeonJWT"                         # libpq 连接认证（compute → PS）
grpc_auth_type = "NeonJWT"                       # gRPC 连接认证（compute → PS）
auth_validation_public_key_path = "/certs/public.pem"  # JWT 公钥路径

# ===== Control Plane 通信 =====
control_plane_api = "http://storage-controller.{ns}:1234/upcall/v1/"  # SC upcall URL
control_plane_api_token = "<jwt_token>"           # PS → SC upcall 的 Bearer token
control_plane_emergency_mode = false               # 紧急模式（跳过 SC generation 验证）
```

#### Pageserver → Safekeeper 认证

使用环境变量（不退在 pageserver.toml 中，因为 token 是 secret）：

```bash
export NEON_AUTH_TOKEN="<jwt_with_scope_safekeeperdata>"
```

实现：`SAFEKEEPER_AUTH_TOKEN: OnceCell<Arc<String>>` （全局变量，通过环境变量设置）

#### Pageserver 三种 AuthType

```rust
pub enum AuthType {
    Trust,    // 无认证
    NeonJWT,  // JWT 认证（需要 auth_validation_public_key_path）
}
```

**注意**：如果 `http_auth_type`/`pg_auth_type`/`grpc_auth_type` 任一为 `NeonJWT`，则 `auth_validation_public_key_path` **必须**存在（否则启动失败）。

---

### 2.6 Safekeeper JWT 配置精确参数 [🟢 已确认]

#### CLI 参数

```bash
safekeeper \
  --pg-auth-public-key-path /certs/public.pem              # WAL 服务端点 JWT (--listen-pg)
  --pg-tenant-only-auth-public-key-path /certs/public.pem   # 租户专用 WAL 端点 (--listen-pg-tenant-only)
  --http-auth-public-key-path /certs/public.pem             # HTTP 管理端点 JWT (--listen-http)
```

#### Scope 权限

Safekeeper 只接受两种 scope：

| Scope | 条件 | 用途 |
|-------|------|------|
| `"safekeeperdata"` | 无限制 | 全数据 + 管理权限（Pageserver 使用） |
| `"tenant"` | 需 tenant_id 匹配 | 特定租户的 WAL 数据（compute 使用） |

**拒绝的 scope**：`Admin`、`PageServerApi`、`GenerationsApi`、`Infra`、`Scrubber`、`ControllerPeer`、`TenantEndpoint`
（会返回 `ineligible for Safekeeper auth` 错误）

#### 关键设计点

| 端点 | Auth 实现 | 热加载 |
|------|:---:|:---:|
| `--listen-http` (7676) | `Arc<SwappableJwtAuth>` | ❌ 无 reload API |
| `--listen-pg` (5454) | `Arc<JwtAuth>` (普通) | ❌ 不支持 |
| `--listen-pg-tenant-only` | `Arc<JwtAuth>` (普通) | ❌ 不支持 |

对于 operator 部署，`--pg-tenant-only` 通常不需要（由 `--listen-pg` 覆盖）。

---

### 2.7 PEM 格式兼容性 [🟢 已验证]

上游 SC 使用以下方式加载公钥 (`JwtAuth::from_key_path` → `from_key`):

```rust
let decoding_key = DecodingKey::from_ed_pem(key_bytes).map_err(...)?;
```

使用的是 `jsonwebtoken` crate 的 `DecodingKey::from_ed_pem()`。

Operator 使用 Go `crypto/ed25519` 生成密钥，`jwtmanager.go` 通过 `x509.MarshalPKIXPublicKey` 输出 PEM。这是标准的 PKCS#8/X.509 格式，与 `jsonwebtoken` 的 `from_ed_pem()` 兼容。

**但注意**：在 `testdata/` 中有基于 PEM 的测试用例（如 `specs/compute/testdata/deployment.yaml`），可以用这些测试 secret 与上游进行端到端验证。

---

## 3. 当前 Operator 实现的问题总结

### 3.1 Scope 格式完全错误 🔴

```go
// ❌ 当前 sc_client.go
claims := map[string]any{
    "scope": "infra admin",  // 既不是 "infra" 也不是 "admin"
}

// ✅ 正确做法
// 方案 A：简单但粒度粗（推荐先采用）
claims := map[string]any{
    "scope": "admin",  // SC 中 Admin 是万能 scope
}

// 方案 B：精确但需要拆分客户端方法
func (c *SCClient) generateInfraJWT() string {
    // scope: "infra" → safekeeper register/upsert/get/list
}
func (c *SCClient) generateAdminJWT() string {
    // scope: "admin" → node management, scheduling_policy
}
```

### 3.2 已确认正确实现

| 实现项 | 状态 |
|-------|:---:|
| Ed25519 密钥生成 | ✅ Go `crypto/ed25519` 与 Rust `jsonwebtoken` 兼容 |
| Compute JWK 分发 | ✅ ConfigMap 格式正确 |
| Compute `/configure` token | ✅ scope/aud/roles 格式正确 |
| SC `--dev` 硬编码 | ❌ 需移除 |

---

## 4. 更新后的移除 `--dev` 评估

### 4.1 是否可以移除？

**答案**：修正 scope 格式后，可以安全移除 SC `--dev`。

| # | 前置条件 | 之前状态 | 调研后确认 |
|---|---------|---------|-----------|
| 1 | SC Pod 挂载 JWT 公钥 | 🔴 阻塞 | 需添加 Volume + `--auth-validation-public-key-path` |
| 2 | Operator SCClient scope 正确 | 🔴 阻塞 | **必须修正** `"infra admin"` → `"admin"` (或 `"infra"`) |
| 3 | SC 健康探针 JWT 豁免 | 🔴 阻塞 | ✅ `/status`, `/live`, `/ready` 已在 allowlist |
| 4 | PS upcall JWT | 🟡 重要 | PS 需要 `control_plane_api_token` (scope: `"generations_api"`) |
| 5 | SK → SC 心跳 | 🟢 不阻塞 | SK 心跳通过 Broker 中转，不走直接 SC HTTP |

### 4.2 修正后的最小实施路径

```
Step 1: 修正 sc_client.go scope 格式
        "infra admin" → "admin"
        
Step 2: SC Deployment 添加 JWT Volume 挂载
        + JWTVolume() volume
        + JWTVolumeMount() mount to /certs
        + --auth-validation-public-key-path /certs/public.pem
        
Step 3: 移除 --dev
        去掉 hardcoded "--dev" 参数
        
Step 4: Pageserver 注入 GenerationsApi token
        + control_plane_api_token 配置到 toml
        或通过环境变量注入
```

### 4.3 SafeKeeper → SC 通信确认

Safekeeper 的心跳通过 Storage Broker 中转（不会直接 HTTP 调用 SC）。SK 唯一可能直接调用 SC 的场景是通过 SC 的 safekeeper management API，但目前 operator 的 SCClient 通过 safekeeper 注册 API 直接跟 SC 交互。这在 Step 2 中已覆盖。

---

## 5. 更新后的实施建议

### 5.1 Scope 修正（紧急）

`sc_client.go:generateJWT()` 需要立即修正。建议方案：

```go
// 按功能拆分 JWT 生成
func (c *SCClient) generateJWT(ctx context.Context, clusterName string, scope string) (string, error) {
    claims := map[string]any{
        "iss":   "neon-operator",
        "sub":   clusterName,
        "iat":   now.Unix(),
        "exp":   now.Add(5 * time.Minute).Unix(),
        "scope": scope,  // "admin" | "infra" | "generations_api"
    }
    return jm.GenerateToken(claims)
}
```

SCClient 各方法使用：
- `registerSafekeeper` → scope: `"infra"` (或用 `"admin"` 万能)
- `configureNode` → scope: `"admin"`
- `nodeStatus` → scope: `"admin"` 或 `"infra"`
- 其他管理操作 → scope: `"admin"`

**简便方案**（推荐先采用）：所有 SC 操作统一用 `"admin"` scope，因为 Admin 在 SC 中确实是万能 scope。

### 5.2 Pageserver → SC upcall

Pageserver 需要 `control_plane_api_token` (scope: `"generations_api"`)，由 Operator 在部署时生成一个长有效期的 token 并写入 pageserver.toml。这属于 Phase 2.2 的内容，不影响 Phase 2.1（SC --dev 移除）。

### 5.3 密钥轮换设计调整

原设计假设 SwappableJwtAuth 会监听文件自动重载，调研确认**没有此功能**。更新后的轮换方案：

**Phase 2 短期**：密钥不轮换，或轮换时触发 Pod 滚动更新（新 Pod 挂载新 Secret Volume）
**Phase 3+**：为 SC/SK 添加 `/reload_auth_validation_keys` 端点（参考 PS 实现），Operator 更新 Secret 后调用 reload API

---

## 6. 附录：上游源码关键文件索引

| 文件路径 | 关键内容 |
|---------|---------|
| `libs/utils/src/auth.rs` | Scope 枚举、Claims、SwappableJwtAuth、JwtAuth |
| `storage_controller/src/http.rs` | SC 路由注册、allowlist、JWT middleware、scope 检查 |
| `storage_controller/src/auth.rs` | SC 的 `check_permission` 实现 |
| `storage_controller/src/main.rs` | SC 启动、`--dev`、公钥加载 |
| `pageserver/src/config.rs` | PS 配置结构、auth 字段、`dev_mode` |
| `pageserver/src/http/routes.rs` | PS 路由注册、allowlist、`reload_auth_validation_keys` handler |
| `pageserver/src/auth.rs` | PS 的 scope 权限检查 |
| `pageserver/src/controller_upcall_client.rs` | PS → SC upcall 实现（Bearer token 注入） |
| `safekeeper/src/http/routes.rs` | SK 路由注册、allowlist、JWT middleware |
| `safekeeper/src/auth.rs` | SK 的 scope 权限检查 |
| `safekeeper/src/bin/safekeeper.rs` | SK 启动、CLI 参数 |
| `control_plane/src/bin/neon_local.rs` | neon_local 的 token 生成参考 |
| `test_runner/fixtures/auth_tokens.py` | Python 测试中的 TokenScope（确认序列化格式） |
