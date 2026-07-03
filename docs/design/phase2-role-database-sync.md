# Phase 2+: Role/Database 同步设计方案

> 基于对上游 Neon 源码、Neon API 及 neon-operator 现状的深度调研。

| 字段 | 内容                                                                                        |
| ---- | ------------------------------------------------------------------------------------------- |
| 版本 | v1.1 |
| 日期 | 2026-06-29 |
| 变更 | Phase 2.1-2.5 已实现并验证，目标状态移至 Phase 2.6+ |
| 来源 | `/home/postgres/works/opensource/neon`、`https://api-docs.neon.tech`、`neon-operator` |

---

## 1. 调研总结

### 1.1 compute_ctl 的密码处理行为

**结论：compute_ctl 只接受 `encrypted_password`（SCRAM-SHA-256 或 md5 格式），不接受明文 `password`。**

**源码证据**（`neon/libs/compute_api/src/spec.rs:544-551`）：

```rust
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Role {
    pub name: PgIdent,
    pub encrypted_password: Option<String>,  // 只有这一个密码字段，没有 password 字段
    pub options: GenericOptions,
}
```

`Role` 结构体中只有 `encrypted_password: Option<String>`，没有任何明文 `password` 字段。这意味着 spec JSON 中传递的密码必须是预加密格式。

**SQL 生成逻辑**（`neon/compute_tools/src/pg_helpers.rs:139-168`）：

```rust
fn to_pg_options(&self) -> String {
    let mut params: String = self.options.as_pg_options();
    params.push_str(" LOGIN");
    if let Some(pass) = &self.encrypted_password {
        if pass.starts_with("SCRAM-SHA-256") {
            write!(params, " PASSWORD '{pass}'")  // SCRAM verifier 原样使用
        } else {
            write!(params, " PASSWORD 'md5{pass}'") // 旧格式加 md5 前缀
        }
    } else {
        params.push_str(" PASSWORD NULL");  // None → 密码为空
    }
    params
}
```

**支持的密码格式**：

| 格式          | 示例                                              | SQL 生成                                           |
| ------------- | ------------------------------------------------- | -------------------------------------------------- |
| SCRAM-SHA-256 | `SCRAM-SHA-256$4096:salt$stored_key:server_key` | `PASSWORD 'SCRAM-SHA-256$...'`                   |
| MD5 hash      | `6b1d16b78004bbd51fa06af9eda75972`              | `PASSWORD 'md56b1d16b78004bbd51fa06af9eda75972'` |
| `null`      | —                                                | `PASSWORD NULL`                                  |

**结论：operator 必须将明文密码转换为 SCRAM-SHA-256 格式后再写入 ConfigMap。**

---

### 1.2 Neon API 的角色/数据库管理

#### 角色接口

| 方法       | 路径                                                            | 说明         |
| ---------- | --------------------------------------------------------------- | ------------ |
| `POST`   | `/projects/{pid}/branches/{bid}/roles`                        | 创建角色     |
| `GET`    | `/projects/{pid}/branches/{bid}/roles`                        | 列出角色     |
| `GET`    | `/projects/{pid}/branches/{bid}/roles/{name}`                 | 获取角色详情 |
| `DELETE` | `/projects/{pid}/branches/{bid}/roles/{name}`                 | 删除角色     |
| `GET`    | `/projects/{pid}/branches/{bid}/roles/{name}/reveal_password` | 获取密码     |
| `POST`   | `/projects/{pid}/branches/{bid}/roles/{name}/reset_password`  | 重置密码     |

**创建角色请求** (`RoleCreateRequest`)：

```json
{
  "role": {
    "name": "string (required, max 63 bytes)",
    "no_login": "boolean (optional)"
  }
}
```

**创建角色响应** (`RoleOperations`)：

```json
{
  "role": {
    "branch_id": "string",
    "name": "string",
    "password": "string (仅创建时返回一次!)",
    "protected": "boolean",
    "authentication_method": "password | oauth | no_login",
    "created_at": "date-time",
    "updated_at": "date-time"
  },
  "operations": [
    {
      "id": "uuid",
      "action": "apply_config",
      "status": "scheduling | running | finished | failed",
      ...
    }
  ]
}
```

**关键发现**：

- 创建角色时**不能指定密码**，密码由平台自动生成并在响应中**一次性返回**
- 密码存储在 `role.password` 字段（明文），标记为 `x-sensitive`
- 创建后会触发 `apply_config` 操作，将新角色推送到 compute_ctl
- 可以通过 `reveal_password` 接口获取密码（如果平台支持存储）
- 可以通过 `reset_password` 重置密码

#### 数据库接口

| 方法       | 路径                                                | 说明           |
| ---------- | --------------------------------------------------- | -------------- |
| `POST`   | `/projects/{pid}/branches/{bid}/databases`        | 创建数据库     |
| `GET`    | `/projects/{pid}/branches/{bid}/databases`        | 列出数据库     |
| `GET`    | `/projects/{pid}/branches/{bid}/databases/{name}` | 获取数据库详情 |
| `PATCH`  | `/projects/{pid}/branches/{bid}/databases/{name}` | 更新数据库     |
| `DELETE` | `/projects/{pid}/branches/{bid}/databases/{name}` | 删除数据库     |

**创建数据库请求** (`DatabaseCreateRequest`)：

```json
{
  "database": {
    "name": "string (required)",
    "owner_name": "string (required)"
  }
}
```

**创建数据库响应** (`DatabaseOperations`)：

```json
{
  "database": {
    "id": "int64",
    "branch_id": "string",
    "name": "string",
    "owner_name": "string",
    "created_at": "date-time",
    "updated_at": "date-time"
  },
  "operations": [{ "action": "apply_config", ... }, { "action": "suspend_compute", ... }]
}
```

---

### 1.3 上游 Neon 的 SCRAM 密码加密实现

Neon 项目中 **只有 Rust 实现**，没有 Go 实现。核心函数位于：

**`neon/libs/proxy/postgres-protocol2/src/password/mod.rs`**：

```rust
const SCRAM_DEFAULT_ITERATIONS: u32 = 4096;
const SCRAM_DEFAULT_SALT_LEN: usize = 16;

/// 将明文密码加密为 SCRAM-SHA-256 格式
/// 返回值: "SCRAM-SHA-256$4096:<base64_salt>$<base64_stored_key>:<base64_server_key>"
pub async fn scram_sha_256(password: &[u8]) -> String {
    let mut salt: [u8; SCRAM_DEFAULT_SALT_LEN] = [0; SCRAM_DEFAULT_SALT_LEN];
    let mut rng = rand::rng();
    rng.fill_bytes(&mut salt);
    scram_sha_256_salt(password, salt).await
}

pub(crate) async fn scram_sha_256_salt(password: &[u8], salt: [u8; SCRAM_DEFAULT_SALT_LEN]) -> String {
    // 1. SASLprep (RFC 4013) 密码规范化
    // 2. PBKDF2: salted_password = Hi(password, salt, 4096)
    // 3. ClientKey = HMAC_SHA256(salted_password, "Client Key")
    // 4. StoredKey = SHA256(ClientKey)
    // 5. ServerKey = HMAC_SHA256(salted_password, "Server Key")
    // 6. 格式化为 "SCRAM-SHA-256$4096:<salt>$<stored_key>:<server_key>"
}
```

**SCRAM-SHA-256 verifier 格式**：

```
SCRAM-SHA-256$<iterations>:<base64_salt>$<base64_stored_key>:<base64_server_key>
```

示例：

```
SCRAM-SHA-256$4096:QSXCR+Q6sek8bf92$FO+9jBb3MUukt6jJnzjPZOWc5ow/Pu6JtPyju0aqaE8=:qxJ1SbmSAi5EcS0J5Ck/cKAm/+Ixa+Kwp63f4OHDgzo=
```

---

### 1.4 compute_ctl 的配置热更新机制

compute_ctl 提供以下 HTTP 接口用于配置更新：

**External Server**（需要 JWT 认证）：

| 路由               | 方法 | 说明                                           |
| ------------------ | ---- | ---------------------------------------------- |
| `/configure`     | POST | 接收完整`ComputeSpec` JSON，触发异步配置应用 |
| `/dbs_and_roles` | GET  | 获取当前 PG 中的角色和数据库列表               |
| `/status`        | GET  | 获取 compute 状态                              |
| `/terminate`     | POST | 安全终止 compute                               |

**Internal Server**（localhost only）：

| 路由                       | 方法 | 说明                                                          |
| -------------------------- | ---- | ------------------------------------------------------------- |
| `/refresh_configuration` | POST | 通知 compute_ctl 从 control plane 重新拉取配置（Hadron 模式） |

**`POST /configure` 的处理流程**（`neon/compute_tools/src/http/routes/configure.rs`）：

1. 接收 `ConfigurationRequest { spec: ComputeSpec }`
2. 解析为 `ParsedSpec`，验证 spec 合法性
3. 设置 compute 状态为 `ConfigurationPending`
4. configurator 后台线程异步执行 `reconfigure()`：
   - 调用 `apply_config()` → `apply_spec_sql()`
   - 按指定阶段顺序执行 SQL：`CreatePrivilegedRole → CreateAndAlterRoles → CreateAndAlterDatabases → ...`
5. 完成后设置状态为 `Configured`

**`apply_spec_sql()` 阶段顺序**（`neon/compute_tools/src/spec_apply.rs`）：

```
1. CreatePrivilegedRole       → 创建 cloud_admin 等特权角色
2. DropInvalidDatabases        → 删除标记为 invalid 的数据库
3. RenameRoles                 → 角色重命名（来自 delta_operations）
4. CreateAndAlterRoles         → ★ 创建/修改角色（含密码对比和调谐）
5. RenameAndDeleteDatabases    → 数据库重命名/删除
6. CreateAndAlterDatabases     → ★ 创建/修改数据库
7. CreateSchemaNeon            → 创建 neon schema
8. 并行的 per-database 阶段     → 权限管理
9. HandleOtherExtensions       → 安装扩展
10. HandleNeonExtension         → 安装 neon 扩展
11. CreateAvailabilityCheck     → 健康检查表
12. DropRoles                  → 删除标记的角色
```

**关键发现**：

- `/configure` 接收**完整 ComputeSpec**，不是增量更新
- compute_ctl 内部通过对比 `pg_authid` 和 spec 来决定 `CREATE` vs `ALTER` vs `NOOP`
- 密码比较逻辑：`db_role.encrypted_password != role.encrypted_password`

---

### 1.5 compute_ctl 的角色/数据库对比逻辑

**CreateAndAlterRoles**（`neon/compute_tools/src/spec_apply.rs:786-840`）：

```rust
// 遍历 spec 中的每个角色
for role in spec.cluster.roles {
    match ctx.roles.get(&role.name) {
        Some(db_role) => {
            // 角色已存在 → 比较密码，不同则 ALTER ROLE
            if db_role.encrypted_password != role.encrypted_password {
                generate_operation("ALTER ROLE ... PASSWORD '...'")
            } else {
                // 密码相同 → 跳过
            }
        }
        None => {
            // 角色不存在 → CREATE ROLE
            if !is_jwks_role {
                generate_operation("CREATE ROLE ... INHERIT CREATEROLE CREATEDB BYPASSRLS REPLICATION IN ROLE cloud_admin LOGIN PASSWORD '...'")
            } else {
                generate_operation("CREATE ROLE ... LOGIN PASSWORD '...'")
            }
        }
    }
}
```

**CreateAndAlterDatabases**（`neon/compute_tools/src/spec_apply.rs:923-984`）：

```rust
// 遍历 spec 中的每个数据库
for db in spec.cluster.databases {
    match ctx.dbs.get(&db.name) {
        Some(existing_db) => {
            // 数据库已存在 → 比较 owner，不同则 ALTER DATABASE OWNER TO
            if db.owner != existing_db.owner {
                generate_operation("ALTER DATABASE ... OWNER TO ...")
            }
        }
        None => {
            // 数据库不存在 → CREATE DATABASE ... OWNER ...
            // 然后 GRANT ALL PRIVILEGES ON DATABASE ... TO cloud_admin
        }
    }
}
```

---

## 2. 当前状态 vs 目标状态

### 2.1 当前状态（修复后 v2）

```
用户创建 Role CR (appuser)
    ↓
RoleController 生成明文密码 → 存入 Secret ✅
    ↓
RoleController 读取密码 → SCRAM-SHA-256 → Role.Status.EncryptedPassword ✅
    ↓
triggerEndpointReconcile (修改 Branch Annotation) ✅
    ↓
EndpointController 被触发 ✅
    ↓
GenerateComputeSpec() 调用 aggregateRoles()/aggregateDatabases()
    → roles: [postgres, appuser]  ✅
    → databases: [neondb]         ✅
    ↓
ConfigMap 挂载到 Pod，compute_ctl 读取
    → roles 包含 appuser         ✅
    ↓
PG 实例中 CREATE ROLE appuser WITH SCRAM-SHA-256 ✅
    → 用户可用 API 返回的密码直接连接 ✅
```

**修复的 Bug**：
- **Secret 命名不一致**：API 创建 Secret 时使用 `role-{branchID}-{roleName}-password`（与 Controller 的 `role-{role.Name}-password` 匹配），避免 Controller 因找不到 Secret 而重新生成密码
- **ComputeSpec 聚合**：`GenerateComputeSpec()` 调用 `aggregateRoles()`/`aggregateDatabases()` 查询 K8s API 获取所有 Role/Database CR，与 `EndpointConfigMap()` 逻辑一致
- **Docker 层缓存**：`make docker-build` 的 `COPY bin/manager .` 可能使用缓存层，需 `--no-cache` 重建

### 2.2 目标状态（Phase 2.6+ 待实现）

```
用户创建 Role CR (appuser)
    ↓
RoleController:
  1. 生成明文密码 ✅
  2. 存入 Secret ✅
  3. SCRAM-SHA-256 加密密码 → 更新 Role Status ⬜ 新增
  4. 触发 Endpoint reconcile ✅
    ↓
EndpointController:
  5. List 同分支的所有 Role CR ⬜ 新增
  6. List 同分支的所有 Database CR ⬜ 新增
  7. 聚合到 ConfigMap 的 roles[] 和 databases[] ⬜ 新增
  8. 通过 POST /configure 热更新 running Pod ⬜ 新增
    ↓
compute_ctl 读取 ConfigMap:
  → roles: [postgres, appuser]  ✅
  → databases: [neondb]         ✅
    ↓
PG 实例中:
  → CREATE ROLE appuser LOGIN PASSWORD 'SCRAM-SHA-256$...'
  → CREATE DATABASE neondb OWNER appuser
```

---

## 3. 设计方案

### 3.1 整体架构

```
┌─────────────────────────────────────────────────────────────┐
│                     K8s 层 (neon-operator)                   │
│                                                              │
│  RoleReconciler                                              │
│    ├─ 生成明文密码 → Secret                                   │
│    ├─ SCRAM-SHA-256 加密 → Role.Status.EncryptedPassword ⬜  │
│    └─ 触发 Endpoint reconcile (Annotation 方式)              │
│                                                              │
│  DatabaseReconciler                                         │
│    ├─ 校验 owner role 存在                                   │
│    └─ 触发 Endpoint reconcile (Annotation 方式)              │
│                                                              │
│  EndpointReconciler                                         │
│    ├─ List Role CRs (同 branch) → 聚合到 ConfigMap ⬜       │
│    ├─ List Database CRs (同 branch) → 聚合到 ConfigMap ⬜   │
│    ├─ 生成 ConfigMap (roles + databases + postgres) ⬜       │
│    └─ POST /configure → 热更新 running Pod ⬜                │
│                                                              │
└──────────────────────┬──────────────────────────────────────┘
                       │  ConfigMap + POST /configure
                       ▼
┌─────────────────────────────────────────────────────────────┐
│                  Pod 层 (compute_ctl)                         │
│                                                              │
│  compute_ctl                                                 │
│    ├─ 读取 ConfigMap → ComputeSpec                            │
│    ├─ CreateAndAlterRoles (比较 pg_authid vs spec)           │
│    └─ CreateAndAlterDatabases (比较 pg_database vs spec)     │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### 3.2 SCRAM-SHA-256 密码加密方案

由于 neon-operator 是 Go 项目，而上游 Neon 只有 Rust 的 SCRAM 实现，需要在 Go 侧实现 SCRAM-SHA-256 密码加密。

#### 方案 A：Go 原生实现 SCRAM-SHA-256（推荐）

在 `utils/` 下新增 `scram.go`，实现 SCRAM-SHA-256 verifier 生成：

```go
package utils

import (
    "crypto/hmac"
    "crypto/rand"
    "crypto/sha256"
    "encoding/base64"
    "fmt"

    "golang.org/x/crypto/pbkdf2"
)

const (
    scramIterations = 4096
    scramSaltLen    = 16
)

// SCRAMSHA256 将明文密码加密为 SCRAM-SHA-256 verifier 字符串
// 格式: SCRAM-SHA-256$4096:<base64_salt>$<base64_stored_key>:<base64_server_key>
func SCRAMSHA256(password []byte) (string, error) {
    // 1. 生成随机 salt
    salt := make([]byte, scramSaltLen)
    if _, err := rand.Read(salt); err != nil {
        return "", fmt.Errorf("generate salt: %w", err)
    }

    // 2. PBKDF2: salted_password = Hi(password, salt, 4096)
    saltedPassword := pbkdf2.Key(password, salt, scramIterations, sha256.Size, sha256.New)

    // 3. ClientKey = HMAC_SHA256(salted_password, "Client Key")
    clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))

    // 4. StoredKey = SHA256(ClientKey)
    storedKey := sha256Hash(clientKey)

    // 5. ServerKey = HMAC_SHA256(salted_password, "Server Key")
    serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))

    return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
        scramIterations,
        base64.StdEncoding.EncodeToString(salt),
        base64.StdEncoding.EncodeToString(storedKey),
        base64.StdEncoding.EncodeToString(serverKey),
    ), nil
}

func hmacSHA256(key, msg []byte) []byte {
    mac := hmac.New(sha256.New, key)
    mac.Write(msg)
    return mac.Sum(nil)
}

func sha256Hash(data []byte) []byte {
    h := sha256.Sum256(data)
    return h[:]
}
```

**依赖**：`golang.org/x/crypto/pbkdf2`（已在标准 crypto 子包中，无需额外依赖）。

**注意**：完整实现还需要 SASLprep (RFC 4013) 密码规范化。可以使用 `golang.org/x/net/idna` 或 `github.com/jrick/saslprep` 等库。如果只支持 ASCII 密码，可跳过此步骤。

#### 方案 B：调用外部 Rust 二进制（不推荐）

编译上游 Neon 的 `scram_sha_256` 为独立二进制，通过 `exec.Command` 调用。增加运维复杂度，不推荐。

#### 方案 C：直接生成 MD5 格式（不推荐）

使用 `md5(password + username)` 格式。安全性较低，不符合现代安全标准。

**推荐方案 A**，完整 Go 实现 SCRAM-SHA-256。

---

### 3.3 Role CR Status 扩展

在 `RoleStatus` 中新增 `EncryptedPassword` 字段，避免每次 reconcile 都重新计算 SCRAM hash：

```go
// api/v1alpha1/role_types.go

type RoleStatus struct {
    ObservedGeneration int64                          `json:"observedGeneration,omitempty"`
    PasswordSecretRef  *corev1.SecretReference         `json:"passwordSecretRef,omitempty"`
    EncryptedPassword  string                          `json:"encryptedPassword,omitempty"`  // SCRAM-SHA-256$...
    Protected          bool                            `json:"protected,omitempty"`
    Phase              string                          `json:"phase,omitempty"`
    Conditions         []metav1.Condition               `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}
```

**RoleController reconcile 流程扩展**：

```
Reconcile(ctx, req):
  1. 获取 Role CR
  2. 如果 deletionTimestamp != nil → finalize
  3. ensureFinalizer
  4. validateBranchExists
  5. ensurePasswordSecret() → 生成明文密码 → 存入 Secret
  6. 如果 Role.Status.EncryptedPassword == "":
     a. 从 Secret 读取明文密码
     b. SCRAMSHA256(password) → encryptedPassword
     c. 更新 Role.Status.EncryptedPassword  ⬜ 新增
  7. 如果明文密码变化（Secret 重建）:
     a. 重新计算 SCRAM hash → 更新 Status  ⬜ 新增
  8. triggerEndpointReconcile
  9. 更新 Status
```

---

### 3.4 EndpointConfigMap 聚合 Role/Database CR

修改 `specs/compute/endpoint_spec.go` 的 `EndpointConfigMap()` 函数，使其能够查询和聚合 Role/Database CR。

#### 函数签名变更

需要增加 `context.Context` 和 `client.Client` 参数以查询 K8s API：

```go
func EndpointConfigMap(
    ctx context.Context,           // ⬜ 新增
    k8sClient client.Client,       // ⬜ 新增
    endpoint *neonv1alpha1.Endpoint,
    branch *neonv1alpha1.Branch,
    project *neonv1alpha1.Project,
    jwtSecret corev1.Secret,
) (*corev1.ConfigMap, error) {
```

#### 聚合逻辑

```go
// 聚合 Role CR → spec roles
func aggregateRoles(ctx context.Context, k8sClient client.Client, branchID string) ([]Role, error) {
    // 1. 默认 postgres 角色（始终存在）
    roles := []Role{
        {
            Name:              "postgres",
            EncryptedPassword: "SCRAM-SHA-256$4096:...", // 与现有硬编码保持一致
            Options:           nil,
        },
    }

    // 2. 查询同分支的 Role CR
    var roleList neonv1alpha1.RoleList
    if err := k8sClient.List(ctx, &roleList,
        client.MatchingFields{"spec.branchID": branchID},
    ); err != nil {
        return nil, err
    }

    // 3. 转换为 spec Role
    for _, roleCR := range roleList.Items {
        // 跳过系统保护角色
        if roleCR.Status.Protected {
            continue
        }
        // 跳过 postgres（已默认添加）
        if roleCR.Spec.Name == "postgres" {
            continue
        }
        // 跳过 no_login 角色
        if roleCR.Spec.AuthenticationMethod == "no_login" {
            continue
        }
        // 需要 EncryptedPassword 已生成
        if roleCR.Status.EncryptedPassword == "" {
            log.Warn("role has no encrypted password, skipping", "role", roleCR.Spec.Name)
            continue
        }
        roles = append(roles, Role{
            Name:              roleCR.Spec.Name,
            EncryptedPassword: roleCR.Status.EncryptedPassword,
            Options:           buildOptions(roleCR.Spec.AuthenticationMethod),
        })
    }
    return roles, nil
}

// 聚合 Database CR → spec databases
func aggregateDatabases(ctx context.Context, k8sClient client.Client, branchID string) ([]Database, error) {
    var dbList neonv1alpha1.DatabaseList
    if err := k8sClient.List(ctx, &dbList,
        client.MatchingFields{"spec.branchID": branchID},
    ); err != nil {
        return nil, err
    }

    databases := make([]Database, 0, len(dbList.Items))
    for _, dbCR := range dbList.Items {
        owner := dbCR.Spec.OwnerName
        if owner == "" {
            owner = "cloud_admin" // 默认 owner
        }
        databases = append(databases, Database{
            Name:  dbCR.Spec.Name,
            Owner: owner,
        })
    }
    return databases, nil
}
```

#### Database 类型定义

当前 `Database` 是 `[]interface{}`，需要定义为具体类型：

```go
type Database struct {
    Name  string      `json:"name"`
    Owner string      `json:"owner"`
    Options interface{} `json:"options"`
}
```

#### Spec 中的 roles 优先级规则

| 场景                        | `postgres` 角色   | 用户自定义角色 |
| --------------------------- | ------------------- | -------------- |
| 无 Role CR                  | ✅ 硬编码添加       | 无             |
| 有 Role CR（含 postgres）   | ✅ 使用 CR 中的配置 | 按 CR 添加     |
| 有 Role CR（不含 postgres） | ✅ 硬编码添加       | 按 CR 添加     |

---

### 3.5 配置热更新方案

当 ConfigMap 更新后，运行的 compute pod 不会自动重新读取。需要主动推送新配置。

#### 方案对比

| 方案                     | 优点                    | 缺点                            | 推荐度 |
| ------------------------ | ----------------------- | ------------------------------- | ------ |
| A: POST /configure       | 即时生效，Neon 原生方式 | 需要 JWT 认证，网络直接访问 Pod | ⭐⭐⭐ |
| B: 重启 Pod              | 简单可靠                | 断连接，不优雅                  | ⭐⭐   |
| C: refresh_configuration | Hadron 模式             | 需要 HCC 支持，不适用于当前架构 | ⭐     |

#### 推荐方案 A: POST /configure

当前代码已经有 `RefreshConfiguration` 函数（`specs/compute/spec.go:155-177`），可以直接复用：

```go
// specs/compute/spec.go

// RefreshConfiguration 已经实现了通过 HTTP POST /configure 推送 spec 的功能
// 参考 internal/controlplane/routes.go:notifyAttach 的使用方式
func RefreshConfiguration(ctx context.Context,
    log *slog.Logger,
    k8sClient client.Client,
    request ComputeHookNotifyRequest,
    deployment *appsv1.Deployment,
    computeBaseURL string) error
```

在 `EndpointReconciler.reconcile()` 中增加：

```go
// reconcile() 中，ConfigMap 更新后的处理
if configMapUpdated {
    // 如果 Pod 正在运行，推送热更新
    if isDeploymentReady(deployment) {
        if err := compute.RefreshConfiguration(ctx, log, k8sClient, notifyRequest, deployment, adminURL); err != nil {
            log.Warn("failed to push hot config update, pod will pick up on restart",
                "error", err)
        }
    }
}
```

**降级策略**：如果 `POST /configure` 失败（Pod 未就绪或网络不通），无需报错。ConfigMap 已经更新，下次 Pod 重启时会自动使用新配置。热更新只是"锦上添花"。

**adminURL 计算**：

```go
func getComputeAdminURL(endpoint *neonv1alpha1.Endpoint) string {
    return fmt.Sprintf("http://%s-admin.%s.svc.cluster.local:3080",
        endpoint.Name, endpoint.Namespace)
}
```

---

### 3.6 Endpoint Controller 监控 Role/Database CR

当前 Endpoint Controller 通过 Branch Annotation 间接触发。Phase 2+ 可以直接 Watch Role/Database CR。

```go
func (r *EndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&neonv1alpha1.Endpoint{}).
        Owns(&appsv1.Deployment{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ConfigMap{}).
        // ⬜ 新增：Watch Role CR 变更
        Watches(&neonv1alpha1.Role{},
            handler.EnqueueRequestsFromMapFunc(r.mapRoleToEndpoint),
        ).
        // ⬜ 新增：Watch Database CR 变更
        Watches(&neonv1alpha1.Database{},
            handler.EnqueueRequestsFromMapFunc(r.mapDatabaseToEndpoint),
        ).
        Named("endpoint").
        Complete(r)
}

// mapRoleToEndpoint 将 Role 变更映射到对应的 Endpoint reconcile 请求
func (r *EndpointReconciler) mapRoleToEndpoint(ctx context.Context, obj client.Object) []reconcile.Request {
    role := obj.(*neonv1alpha1.Role)
    // 查找同 Branch 的 Endpoint
    endpoint := findEndpointByBranch(ctx, r.Client, role.Spec.BranchID)
    if endpoint == nil {
        return nil
    }
    return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(endpoint)}}
}
```

**渐进式实现策略**：

- 初期可以保留 Annotation 间接触发方式作为兜底
- Watch Role/Database CR 作为 Phase 2+ 的优化
- 两种方式可以共存，不冲突

---

### 3.7 角色删除处理

当 Role CR 被删除时，需要从 ConfigMap 中移除对应角色。

**当前 finalizer 机制**已经存在（`role_controller.go:finalize()`），只需要确保：

1. Role CR 删除 → finalizer 清理密码 Secret
2. `triggerEndpointReconcile()` → Endpoint Controller 重新生成 ConfigMap
3. ConfigMap 的 roles 列表中不再包含已删除的角色
4. `POST /configure` 推送新 spec → compute_ctl 删除 PG 中的角色

**注意**：compute_ctl 的 `apply_spec_sql()` 阶段包括 `DropRoles`（排在最后），会自动删除不在 spec 中的角色。但角色删除可能失败（如在数据库中拥有对象），需要处理错误。

---

## 4. 实现计划

### 4.1 分阶段实现

| 阶段                | 内容                                                               | 涉及文件                                                                   | 优先级 | 状态 |
| ------------------- | ------------------------------------------------------------------ | -------------------------------------------------------------------------- | ------ | ---- |
| **Phase 2.1** | Go 实现 SCRAM-SHA-256 加密                                         | `utils/scram.go` (新建)                                                  | P0     | ✅ 已完成 |
| **Phase 2.2** | Role.Status 增加 EncryptedPassword，RoleController 计算 SCRAM hash | `api/v1alpha1/role_types.go`, `internal/controller/role_controller.go` | P0     | ✅ 已完成 |
| **Phase 2.3** | ComputeSpec 聚合 Role CR → roles 列表                              | `specs/compute/spec.go` (GenerateComputeSpec), `specs/compute/endpoint_spec.go` | P0     | ✅ 已完成 |
| **Phase 2.4** | ComputeSpec 聚合 Database CR → databases 列表                      | `specs/compute/spec.go` (GenerateComputeSpec), `specs/compute/endpoint_spec.go` | P0     | ✅ 已完成 |
| **Phase 2.5** | API 创建 Role 时 Secret 命名与 Controller 一致                      | `internal/controlplane/api_service.go`                                    | P0     | ✅ 已完成 |
| **Phase 2.6** | Endpoint Controller 在 ConfigMap 更新后调用 POST /configure        | `internal/controller/endpoint_controller.go`                             | P1     | ⬜ 待实现 |
| **Phase 2.7** | Endpoint Controller Watch Role/Database CR                         | `internal/controller/endpoint_controller.go`                             | P1     | ⬜ 待实现 |
| **Phase 2.8** | 角色/数据库删除时的级联处理                                        | `role_controller.go`, `database_controller.go`                         | P2     | ⬜ 待实现 |

### 4.2 各阶段详细任务

#### Phase 2.1: SCRAM-SHA-256 加密

- [ ] 新建 `utils/scram.go`
- [ ] 实现 `SCRAMSHA256(password []byte) (string, error)` 函数
- [ ] 添加单元测试：验证生成的 verifier 可被 PostgreSQL 接受
- [ ] 可选：调研并引入 SASLprep 库

#### Phase 2.2: Role Status 扩展

- [ ] `RoleStatus` 新增 `EncryptedPassword string` 字段
- [ ] `RoleReconciler.Reconcile()` 中增加 SCRAM 加密逻辑：
  - 如果 `EncryptedPassword` 为空且密码 Secret 存在 → 读取 Secret，计算 SCRAM hash，更新 Status
  - 如果 Secret 重建（密码变化）→ 重新计算 SCRAM hash
- [ ] 更新 CRD YAML 定义
- [ ] 添加索引：`spec.branchID` 字段索引（用于 List 查询）

#### Phase 2.3: EndpointConfigMap 聚合 Role CR

- [ ] 定义 `Database` 结构体（替代当前的 `[]interface{}`）
- [ ] 实现 `aggregateRoles()` 函数
- [ ] 修改 `EndpointConfigMap()` 签名（增加 ctx、k8sClient 参数）
- [ ] 更新函数调用链（`endpoint_create.go:reconcileEndpointConfigMap()`）
- [ ] 更新测试

#### Phase 2.4: EndpointConfigMap 聚合 Database CR

- [ ] 实现 `aggregateDatabases()` 函数
- [ ] 在 `EndpointConfigMap()` 中调用
- [ ] 更新测试

#### Phase 2.5: 配置热更新

- [ ] 在 `EndpointReconciler` 中判断 ConfigMap 是否变化
- [ ] 如果 ConfigMap 变化且 Deployment 就绪，调用 `RefreshConfiguration()`
- [ ] 处理热更新失败降级逻辑

#### Phase 2.6: Watch Role/Database CR

- [ ] 实现 `mapRoleToEndpoint()` 映射函数
- [ ] 实现 `mapDatabaseToEndpoint()` 映射函数
- [ ] 更新 `SetupWithManager()`

#### Phase 2.7: 删除级联

- [ ] 验证 Role 删除 → ConfigMap 更新 → compute_ctl 删除 PG 角色的完整链路
- [ ] 验证 Database 删除 → ConfigMap 更新 → compute_ctl 删除 PG 数据库的完整链路
- [ ] 处理删除失败场景（如角色拥有数据库对象）

### 4.3 测试策略

| 测试类型 | 测试内容                                               |
| -------- | ------------------------------------------------------ |
| 单元测试 | `SCRAMSHA256()` 函数正确性                           |
| 单元测试 | `aggregateRoles()` / `aggregateDatabases()` 正确性 |
| 集成测试 | 创建 Role CR → 验证 ConfigMap 包含对应角色            |
| 集成测试 | 创建 Database CR → 验证 ConfigMap 包含对应数据库      |
| 集成测试 | 删除 Role CR → 验证 ConfigMap 移除对应角色            |
| E2E 测试 | 完整链路：Role CR → PG 中存在对应角色                 |
| E2E 测试 | 完整链路：Database CR → PG 中存在对应数据库           |
| E2E 测试 | 密码更新 → PG 中密码同步更新                          |

---

## 5. 附录

### 5.1 关键文件索引

| 文件                       | 位置                                                 | 作用                                                 |
| -------------------------- | ---------------------------------------------------- | ---------------------------------------------------- |
| `endpoint_spec.go`       | `neon-operator/specs/compute/`                     | Endpoint ConfigMap 生成（Phase 2+ 主修改点）         |
| `spec.go`                | `neon-operator/specs/compute/`                     | ComputeSpec/Role/Database 定义、RefreshConfiguration |
| `role_controller.go`     | `neon-operator/internal/controller/`               | Role CR reconcile（需增加 SCRAM 加密）               |
| `database_controller.go` | `neon-operator/internal/controller/`               | Database CR reconcile                                |
| `endpoint_controller.go` | `neon-operator/internal/controller/`               | Endpoint reconcile（需增加 Watch + 热更新）          |
| `endpoint_create.go`     | `neon-operator/internal/controller/`               | Endpoint 资源创建协调                                |
| `role_types.go`          | `neon-operator/api/v1alpha1/`                      | Role CRD 类型（需扩展 Status）                       |
| `database_types.go`      | `neon-operator/api/v1alpha1/`                      | Database CRD 类型                                    |
| `spec.rs`                | `neon/libs/compute_api/src/`                       | 上游 Role/Database 结构体（参考）                    |
| `spec_apply.rs`          | `neon/compute_tools/src/`                          | compute_ctl spec 应用逻辑（参考）                    |
| `pg_helpers.rs`          | `neon/compute_tools/src/`                          | 密码 SQL 生成逻辑（参考）                            |
| `configure.rs`           | `neon/compute_tools/src/http/routes/`              | POST /configure 接口（参考）                         |
| `scram_sha_256`          | `neon/libs/proxy/postgres-protocol2/src/password/` | 上游 SCRAM 实现（参考）                              |

### 5.2 参考链接

- Neon API 文档：https://api-docs.neon.tech/reference/getting-started-with-neon-api
- Neon API OpenAPI：https://api-docs.neon.tech/llms.txt
- PostgreSQL SCRAM-SHA-256：https://www.postgresql.org/docs/current/sasl-authentication.html
- RFC 5802 (SCRAM)：https://tools.ietf.org/html/rfc5802
- RFC 7677 (SCRAM-SHA-256)：https://tools.ietf.org/html/rfc7677
