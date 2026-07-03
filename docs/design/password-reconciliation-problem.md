# 计算节点密码调谐问题分析

## 问题概述

当前 neon-operator 对计算节点的用户密码管理存在调谐回退问题：当用户手动修改 PostgreSQL 角色密码后，Pod 重启或 `compute_ctl` 周期性调谐会导致密码被回退为 spec 中硬编码的原始密码。

## 背景：两层调谐架构

```
┌────────────────────────────────────────────────────────────┐
│  neon-operator (Go) — Kubernetes 层调谐                     │
│                                                             │
│  Reconcile() {                                             │
│    1. 检查 Deployment/Service/ConfigMap                    │
│    2. 生成 spec JSON → 写入 ConfigMap + control-plane API  │
│  }                                                          │
│  管理的资源: Deployment, Service, ConfigMap                  │
│  ❌ 不直接连接 PostgreSQL                                   │
└──────────────────────┬─────────────────────────────────────┘
                       │  spec JSON
                       ▼
┌────────────────────────────────────────────────────────────┐
│  compute_ctl (Rust) — PostgreSQL 层调谐                     │
│                                                             │
│  apply_spec() {                                            │
│    Phase::CreatePrivilegedRole  → neon_superuser           │
│    Phase::CreateAndAlterRoles   → 对比 pg_authid vs spec   │
│    Phase::CreateDatabases       → 对比 pg_database vs spec │
│    Phase::ApplySettings         → ALTER SYSTEM SET ...     │
│  }                                                          │
│  管理的资源: PostgreSQL roles, databases, settings          │
│  ❌ 不管 Kubernetes 资源                                    │
└────────────────────────────────────────────────────────────┘
```

`neon-operator` 不连接 PostgreSQL，它只管理 Kubernetes 资源。实际的 PostgreSQL 调谐（roles、databases、settings）由 pod 内的 `compute_ctl` 进程完成，它从 control-plane 拉取 spec JSON 后连接 `127.0.0.1:55433` 执行 SQL。

## 密码相关代码链路

### 1. neon-operator：spec 生成（`specs/compute/spec.go`）

```go
Roles: []Role{
    {
        Name:              "postgres",
        EncryptedPassword: "SCRAM-SHA-256$4096:Km5/...", // 硬编码的 scram verifier
        Options:           nil,
    },
},
```

### 2. compute_ctl：密码调谐逻辑（`compute_tools/src/spec_apply.rs`）

```rust
ApplySpecPhase::CreateAndAlterRoles => {
    match db_role {
        Some(db_role) => {
            // 核心比较：数据库中的密码 vs spec 中的密码
            if db_role.encrypted_password != role.encrypted_password {
                Some(Operation {
                    query: format!(
                        "ALTER ROLE {} {}",
                        role.name.pg_quote(),
                        role.to_pg_options(),  // 包含 PASSWORD 子句
                    ),
                })
            }
        }
        None => {
            // 角色不存在 → CREATE ROLE ... PASSWORD
        }
    }
}
```

### 3. compute_ctl：密码序列化（`pg_helpers.rs`）

```rust
fn to_pg_options(&self) -> String {
    if let Some(pass) = &self.encrypted_password {
        if pass.starts_with("SCRAM-SHA-256") {
            write!(params, " PASSWORD '{pass}'")     // SCRAM verifier 原样使用
        } else {
            write!(params, " PASSWORD 'md5{pass}'")  // 旧 MD5 格式加前缀
        }
    } else {
        params.push_str(" PASSWORD NULL");           // 无密码 → 删除密码
    }
}
```

### 4. Role 结构体定义（`compute_api/src/spec.rs`）

```rust
pub struct Role {
    pub name: PgIdent,
    pub encrypted_password: Option<String>,  // None 表示不设置密码
    pub options: GenericOptions,
}
```

## 问题场景分析

### 场景一：Pod 重启导致密码回退

```
1. 创建 Pod → compute_ctl 拉取 spec
       spec.encrypted_password = "SCRAM-SHA-256$4096:Km5/..."

2. CREATE ROLE postgres PASSWORD 'SCRAM-SHA-256$4096:Km5/...'

3. pg_authid 存储: SCRAM-SHA-256$4096:Km5/...

4. 用户手动改密
       ALTER ROLE postgres PASSWORD 'my-new-password'

5. pg_authid 存储: SCRAM-SHA-256$4096:<新salt>:<新key>

6. ⚡ Pod 挂了，重启

7. compute_ctl 重新从 control-plane 拉取 spec
       spec.encrypted_password = "SCRAM-SHA-256$4096:Km5/..."  ← 旧的！

8. db_role.encrypted_password != role.encrypted_password
       → ALTER ROLE postgres PASSWORD 'SCRAM-SHA-256$4096:Km5/...'

9. 密码被回退！用户修改丢失
```

### 场景二：spec 中不带密码（`EncryptedPassword: null`）的情况

```
1. spec.encrypted_password = null

2. CREATE ROLE postgres LOGIN PASSWORD NULL

3. 用户手动设密: ALTER ROLE postgres PASSWORD 'my-password'

4. pg_authid 存储: SCRAM-SHA-256$...

5. Pod 重启，compute_ctl 重新拉取 spec
       spec.encrypted_password = null

6. None != Some("SCRAM-SHA-256$...")
       → ALTER ROLE postgres PASSWORD NULL  ← 密码被删除！
```

**结论：当前两种写法都有问题：**
- 带密码 → 用户改密被回退
- 不带密码（`null`）→ 用户密码被删除

## 根本原因

`compute_ctl` 将 `Role.encrypted_password` 视为 PostgreSQL 角色密码的唯一真相源（source of truth），它的设计意图是"spec 说什么，数据库就是什么"，不区分"首次创建"和"调谐更新"两种场景。

`Option<String>` 类型在这里有歧义：
- 无法表达 **"不管理密码"** 的语义——`None` 被解释为"密码应为空"（`PASSWORD NULL`）
- 需要一个三态值来区分：`Set(hash)` | `Null`（密码应为空） | `Unmanaged`（不干预密码）

## 各环境密码管理需求

| 环境 | 需求 |
|------|------|
| **Neon Cloud** | 平台管理密码，用户通过 Console 或 API 修改，API 层会把密码写回 control-plane spec |
| **自建环境（neon-operator）** | 用户通过 Kubernetes Secret 管理密码，初始设密后应允许用户自行修改，operator 不应回退 |

Neon Cloud 的设计假设是 spec 始终为真相源，用户的密码修改会通过 API 写回 spec。但 neon-operator 缺少这个反馈回路。

## 修复方案

### 推荐方案：修改 compute_ctl 的密码比较逻辑

让 `None` 表示 **"不管理密码"**，spec 中有密码时才做调谐对比。

**修改 `compute_tools/src/spec_apply.rs`：**

```rust
ApplySpecPhase::CreateAndAlterRoles => {
    match db_role {
        Some(db_role) => {
            // 只有 spec 明确提供了密码时才比较和调谐
            let password_changed = match (&role.encrypted_password, &db_role.encrypted_password) {
                (Some(spec_pwd), Some(db_pwd)) => spec_pwd != db_pwd,
                (Some(_), None) => true,   // spec 有密码，DB 无 → SET
                _ => false,                 // spec 无密码 → 不干预 DB
            };
            if password_changed {
                Some(Operation {
                    query: format!(
                        "ALTER ROLE {} {}",
                        role.name.pg_quote(),
                        role.to_pg_options(),
                    ),
                })
            } else {
                None
            }
        }
        None => {
            // 角色不存在，首次创建
            // ...
        }
    }
}
```

**同步修改 neon-operator `specs/compute/spec.go`：**

```go
Roles: []Role{
    {
        Name:              "postgres",
        EncryptedPassword: "",  // 空字符串 → JSON null → compute_ctl 不管理密码
        Options:           nil,
    },
},
```

**密码初始设置**：通过 Kubernetes Secret + postStart Hook 在 Pod 首次创建时一次性设置，后续不再通过 spec 管理。

### 备选方案

1. **完全由 spec 管理密码**：不修改 compute_ctl，维持 spec 为源。适用于平台型产品（如 Neon Cloud），用户通过 API 修改密码时同步更新 spec。但对自建场景不友好。

2. **在 Role struct 中增加 `managed` 字段**：新增字段标识密码是否由 spec 管理。改动较大，但语义清晰。

3. **Operator 端拦截**：在 operator 的 reconcicle 循环中检测用户密码变更并同步回 spec。实现复杂，需要连接 PostgreSQL。

## 附录：相关文件

| 文件 | 位置 | 作用 |
|------|------|------|
| `specs/compute/spec.go` | neon-operator | 生成 compute spec JSON（含 Role 定义 + aggregateRoles/aggregateDatabases） |
| `specs/compute/endpoint_spec.go` | neon-operator | Endpoint ConfigMap 生成（引用 spec.go 的聚合函数） |
| `specs/compute/deployment.go` | neon-operator | 构建 compute pod 的容器定义 |
| `utils/scram.go` | neon-operator | SCRAM-SHA-256 密码加密（Go 实现） |
| `internal/controller/role_controller.go` | neon-operator | Role Controller（密码生成、SCRAM 加密、Endpoint 触发） |
| `internal/controlplane/api_service.go` | neon-operator | Control Plane API（Project/Role 创建、Secret 管理） |
| `compute_tools/src/spec_apply.rs` | neon 源码 | compute_ctl 的 spec 应用逻辑（密码对比在此） |
| `compute_tools/src/pg_helpers.rs` | neon 源码 | Role → PASSWORD SQL 序列化 |
| `compute_api/src/spec.rs` | neon 源码 | Role 结构体定义（`encrypted_password: Option<String>`） |
| `compute_tools/src/spec.rs` | neon 源码 | 从 control-plane 拉取 spec 的客户端 |

## 附录 B：已修复的 Bug

### B.1 Secret 命名不一致导致密码不匹配（2026-06-29）

**现象**：API 返回的密码与 PostgreSQL 中实际配置的密码不一致，用户认证失败。

**根因**：API (`api_service.go`) 创建 Secret 时使用命名 `{branchID}-{roleName}-password`，但 Role Controller (`role_controller.go`) 查找 `role-{role.Name}-password`。Controller 找不到 API 创建的 Secret，便重新生成随机密码存入另一个 Secret，导致 API 返回的密码与 SCRAM verifier 对应的密码不匹配。

**修复**：将 API 中三处 Secret 创建逻辑统一为 `role-{branchID}-{roleName}-password`（对应 `role-{role.Name}-password`）。

**涉及文件**：
- `internal/controlplane/api_service.go` (CreateProject, CreateRole, ResetPassword)
- `internal/controller/role_controller.go` (ensurePasswordSecret)
