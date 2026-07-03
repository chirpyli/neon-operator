# Phase 1 P0 补齐基础 CRUD — 设计方案

> 对标 Neon Cloud API，基于 neon 源码深度调研 + 官方 API 文档编写。覆盖 Branch / Endpoint / Role / Database 四个资源的 PATCH、子资源和生命周期端点。

| 字段 | 内容 |
| ---- | ---- |
| 版本 | v1.0 |
| 日期 | 2026-06-30 |
| 参考 | [Neon API Reference v2.0](https://api-docs.neon.tech/reference/getting-started-with-neon-api) |
| 依赖 | [Project PATCH 设计](./project-patch-api-design.md) — Nullable[T] 三态语义 |

---

## 调研总结

### 上游 Neon 架构

Neon 的 REST API（`/api/v2/...`）由 **Console/Control-Plane 服务**实现（闭源/独立部署），不在开源存储层（neon 仓库）中。开源仓库包含：

| 开源组件 | Branch | Endpoint | Role | Database |
|---------|--------|----------|------|----------|
| **Pageserver** | Timeline CRUD（POST/GET/DELETE），无 PATCH 更新属性 | - | - | - |
| **compute_ctl** | - | /configure 全量 spec 下发 | /dbs_and_roles 批量查询 | /dbs_and_roles 批量查询 |
| **Proxy** | BranchNotFound 错误码 | /wake_compute, /get_endpoint_access_control | - | - |

**关键结论：所有 PATCH/子资源端点均需在 operator 的 Control Plane API 层自行实现。**

### 现有 Operator 实现基线

| 资源 | 已实现 | 缺失 | CRD 可更新字段 |
|------|--------|------|---------------|
| **Branch** | POST/GET(list)/GET(single)/DELETE | PATCH, set_as_default | Spec.Protected, Spec.Default |
| **Endpoint** | POST/GET(list)/GET(single)/DELETE | PATCH, start, suspend, restart | Spec.Resources, Spec.Disabled, Spec.Exposure |
| **Role** | POST/GET(list)/DELETE, reset_password | GET single, PATCH | Spec.AuthenticationMethod, Spec.Name(仅创建后不可变) |
| **Database** | POST/GET(list)/DELETE | GET single, PATCH, connection_uri | Spec.OwnerName, Spec.Name(仅创建后不可变) |

### 官方 Neon API 对照

| 端点 | 方法 | 路径 | 对标功能 |
|------|------|------|---------|
| Update branch | `PATCH` | `/api/v2/projects/{pid}/branches/{id}` | 更新 name, protected |
| Set branch default | `POST` | `/api/v2/projects/{pid}/branches/{id}/set_as_default` | 设为项目默认分支 |
| Update endpoint | `PATCH` | `/api/v2/projects/{pid}/endpoints/{id}` | 更新 compute 规格 |
| Start endpoint | `POST` | `/api/v2/projects/{pid}/endpoints/{id}/start` | 启动 compute |
| Suspend endpoint | `POST` | `/api/v2/projects/{pid}/endpoints/{id}/suspend` | 挂起 compute |
| Restart endpoint | `POST` | `/api/v2/projects/{pid}/endpoints/{id}/restart` | 重启 compute |
| Get role | `GET` | `/api/v2/projects/{pid}/branches/{bid}/roles/{name}` | 获取单个角色 |
| Update role | `PATCH` | `...` — Neon 通过 /configure 下发 spec 间接实现 | 更新密码（仅此一个可更新字段） |
| Get database | `GET` | `/api/v2/projects/{pid}/branches/{bid}/databases/{name}` | 获取单个数据库 |
| Update database | `PATCH` | `...` | 更新 name, owner |
| Connection URI | `GET` | `/api/v2/projects/{pid}/connection_uri` | 构造连接字符串 |

> **角色 PATCH**：Neon 上游本身没有独立 PATCH 端点，角色更新（仅密码）通过 compute_ctl `/configure` 的全量 spec 下发实现。本方案按产品级需求增加独立 PATCH 端点。

---

## 一、Branch PATCH + set_as_default

### 1.1 API 接口

#### PATCH /api/v2/projects/{project_id}/branches/{branch_id}

**请求体：**

```json
{
  "branch": {
    "name": "feature-x",
    "protected": true
  }
}
```

**可更新字段：**

| 字段 | 类型 | 语义 | 说明 |
|------|------|------|------|
| `branch.name` | string | Upsert/Noop | 分支显示名称（1-256 字符） |
| `branch.protected` | bool | Upsert/Noop | 保护分支（protected=true 不可直接删除） |

**不可更新字段（创建后不可变）：**
- `parent_id`（父分支关系）
- `parent_lsn`（分支起始点）
- `pg_version`（PG 版本）

**响应：** 200 OK，返回完整的 Branch 对象。

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 400 | `INVALID_REQUEST` | JSON 格式错误 |
| 400 | `INVALID_NAME` | 名称为空或超 256 字符 |
| 404 | `BRANCH_NOT_FOUND` | 分支不存在或不属于该项目 |
| 409 | `BRANCH_PROTECTED` | 对 protected 分支执行非法操作 |

#### POST /api/v2/projects/{project_id}/branches/{branch_id}/set_as_default

**请求体：** 无（对标 Neon API，无需 body）

**行为：**
1. 验证当前分支存在且属于该项目
2. 在 Project 的 K8s Namespace 中，将所有 Branch 的 `spec.default` 设为 `false`
3. 将当前分支的 `spec.default` 设为 `true`
4. 返回 200 OK + 更新后的 Branch 对象

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 404 | `BRANCH_NOT_FOUND` | 分支不存在 |
| 409 | `ALREADY_DEFAULT` | 已经是默认分支（幂等，仍返回 200） |

### 1.2 CRD 变更分析

**当前 BranchSpec 已包含所需字段**，无需修改：

```go
type BranchSpec struct {
    // ... 现有字段 ...
    Protected bool `json:"protected,omitempty"`
    Default   bool `json:"default,omitempty"`
}
```

### 1.3 K8s 实现策略

```
PATCH branch
  └── apiService.UpdateBranch()
        ├── GET Branch CR from K8s
        ├── 验证 name 范围 + 不可变字段不改变
        ├── MergeFrom patch → 仅更新 name / protected
        └── 返回 toBranchResponse()

POST set_as_default
  └── apiService.SetBranchAsDefault()
        ├── GET 目标 Branch CR
        ├── LIST 同项目所有 Branch CRs（by projectID label）
        ├── 批量 PATCH: 所有 spec.default=false
        ├── PATCH: 目标 spec.default=true
        └── 返回 toBranchResponse()
```

**关键设计决策：set_as_default 的事务性**

由于 K8s 不支持跨资源事务，`set_as_default` 采用 **best-effort 语义**：
1. 先 PATCH 所有其他分支的 `default=false`（如果某一步失败，已修改的保持 false，不影响正确性）
2. 最后 PATCH 目标分支 `default=true`
3. 如果第 2 步失败，调用方重试即可（幂等操作）

---

## 二、Endpoint PATCH + 生命周期

### 2.1 API 接口

#### PATCH /api/v2/projects/{project_id}/endpoints/{endpoint_id}

**请求体：**

```json
{
  "endpoint": {
    "branch_id": "br-quiet-breeze",
    "resources": {
      "cpu": "2",
      "memory": "8Gi"
    },
    "disabled": false,
    "suspend_timeout_seconds": 300
  }
}
```

**可更新字段：**

| 字段 | 类型 | 语义 | 说明 |
|------|------|------|------|
| `endpoint.branch_id` | string | Upsert/Noop | 将端点迁移到另一个分支 |
| `endpoint.resources` | object | Upsert/Noop | CPU/Memory 资源规格 |
| `endpoint.disabled` | bool | Upsert/Noop | 禁用以暂停连接（replicas=0） |
| `endpoint.suspend_timeout_seconds` | int | Upsert/Noop | [Serverless 预留] 空闲自动挂起超时 |

**不可更新字段（创建后不可变）：**
- `type`（read_write / read_only）

**响应：** 200 OK，返回完整的 Endpoint 对象（含 connection URI）。

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 400 | `INVALID_REQUEST` | JSON 格式错误 |
| 404 | `ENDPOINT_NOT_FOUND` | 端点不存在 |
| 409 | `READ_WRITE_ENDPOINT_EXISTS` | 分支已有 read_write 端点，不可迁移第二个 |

#### POST /api/v2/projects/{project_id}/endpoints/{endpoint_id}/start

**请求体：** 无

**行为：** 将 `spec.disabled` 设为 `false`，触发 controller 启动 Pod。

**幂等性：** 如果端点已经在运行中（`phase=active`），直接返回 200。

#### POST /api/v2/projects/{project_id}/endpoints/{endpoint_id}/suspend

**请求体：** 无

**行为：** 将 `spec.disabled` 设为 `true`，触发 controller 将 Deployment replicas 设为 0。

**幂等性：** 如果端点已经是 suspended（`phase=stopped`），直接返回 200。

#### POST /api/v2/projects/{project_id}/endpoints/{endpoint_id}/restart

**请求体：** 无

**行为：**
1. 先 suspend（disabled=true）
2. 等待 Phase 变为 stopped
3. 再 start（disabled=false）
4. 返回 200 + 包含 operations 数组（用于跟踪重启进度）

**K8s 简化策略：** 对于当前非 Serverless 架构，restart 直接触发 Pod 滚动重启即可：
```
PATCH endpoint.spec.disabled=true → 等 controller 确认 stopped → PATCH endpoint.spec.disabled=false
```

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 404 | `ENDPOINT_NOT_FOUND` | 端点不存在 |
| 409 | `ENDPOINT_BUSY` | 端点正在执行其他操作 |

### 2.2 CRD 变更分析

**当前 EndpointSpec 已具备基础，需增加 `SuspendTimeoutSeconds`：**

当前代码已在注释中预留：
```go
// [未来 Serverless 预留]
// SuspendTimeoutSeconds 无活动后暂停的超时秒数, -1=永不暂停。
// +optional
// SuspendTimeoutSeconds *int32 `json:"suspendTimeoutSeconds,omitempty"`
```

**建议变更：** 取消注释，正式启用该字段。同时增加 `BranchID` 的可更新性注释。

### 2.3 K8s 实现策略

```
PATCH endpoint
  └── apiService.UpdateEndpoint()
        ├── GET Endpoint CR
        ├── 验证 branch_id 变更不违反 read_write 唯一约束
        ├── MergeFrom patch → resources / disabled / branchID / suspendTimeoutSeconds
        └── 返回 toEndpointResponse()（含 connection_uri）

POST start
  └── apiService.StartEndpoint()
        ├── GET Endpoint CR
        ├── 若 spec.disabled=false → 幂等返回 200
        ├── PATCH spec.disabled=false
        └── 返回当前状态

POST suspend
  └── apiService.SuspendEndpoint()
        ├── GET Endpoint CR
        ├── 若 spec.disabled=true → 幂等返回 200
        ├── PATCH spec.disabled=true
        └── 返回当前状态

POST restart
  └── apiService.RestartEndpoint()
        ├── GET Endpoint CR
        ├── 创建 Operation CR (type="restart_endpoint")
        ├── PATCH spec.disabled=true
        ├── Controller 检测到 disabled=true，设置 replicas=0
        ├── Controller 等待 stopped，然后设置 disabled=false
        └── 返回 200 + operations[]
```

**start/suspend 的 K8s 实现**：核心机制是通过 `disabled` 字段控制 Deployment replicas（0 = suspend, 1 = start）。Controller 监听该字段变更并执行相应操作。这符合 K8s 的声明式理念。

---

## 三、Role GET single + PATCH

### 3.1 API 接口

#### GET /api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}

**路由注册：** 修改参数名从 `{role_name}` 为使用 PathValue 获取（与 PATCH 共享同一路由模式）

**请求体：** 无

**响应（200 OK）：**

```json
{
  "role": {
    "name": "myapp_user",
    "branch_id": "br-quiet-breeze",
    "protected": false,
    "created_at": "2026-01-15T08:30:00Z",
    "updated_at": "2026-06-30T10:00:00Z",
    "authentication_method": "password"
  }
}
```

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 404 | `ROLE_NOT_FOUND` | 角色不存在或不属于该分支 |

#### PATCH /api/v2/projects/{project_id}/branches/{branch_id}/roles/{role_name}

**请求体：**

```json
{
  "role": {
    "name": "renamed_user",
    "password": "new-secure-password"
  }
}
```

**可更新字段：**

| 字段 | 类型 | 语义 | 说明 |
|------|------|------|------|
| `role.name` | string | Upsert/Noop | 重命名角色 |
| `role.password` | string | Upsert/Noop | 重置密码（明文，服务端 SCRAM 加密后存储） |

**上游对比：**
- Neon 上游 compute_ctl 中唯一可更新的角色字段是 `encrypted_password`
- 重命名通过 `DeltaOp(action="rename_role")` 实现
- 本方案统一暴露 name + password 更新

**响应：** 200 OK，返回完整的 Role 对象（含 operations 数组用于跟踪异步操作）。

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 400 | `INVALID_NAME` | 名称为空或超 63 字符 |
| 404 | `ROLE_NOT_FOUND` | 角色不存在 |
| 409 | `ROLE_PROTECTED` | 不可修改系统保护角色 |
| 422 | `VALIDATION_ERROR` | 密码不符合复杂度要求 |

### 3.2 CRD 变更分析

**当前 Role 模型已满足需求，无需修改 CRD。**

RoleSpec 已有所有必要字段：
```go
type RoleSpec struct {
    BranchID             string `json:"branchID"`
    Name                 string `json:"name"`
    AuthenticationMethod string `json:"authenticationMethod,omitempty"`
}
```

RoleStatus 已有密码管理字段：
```go
type RoleStatus struct {
    PasswordSecretRef *corev1.SecretReference `json:"passwordSecretRef,omitempty"`
    EncryptedPassword string                  `json:"encryptedPassword,omitempty"`
    Protected         bool                    `json:"protected,omitempty"`
}
```

### 3.3 K8s 实现策略

角色 CR 的命名约定为 `{branch_name}-{role_name}`，定位单个角色通过 `branchID + roleName` 组合。

#### 3.3.1 当前实现的前置问题

在实现 PATCH 之前，需修复以下问题以确保基础一致性：

| # | 问题 | 现状 | 修复方案 |
|---|------|------|----------|
| 1 | **Secret 结构不一致** | Controller 路径写 `password` + `username` + Labels + ownerReference；API 路径(`CreateRole`)仅写 `password` | API 路径创建 Secret 时补齐 `username`、Labels、ownerReference |
| 2 | **SCRAM verifier 过期** | `ensureEncryptedPassword` 在 `EncryptedPassword != ""` 时直接跳过，密码变更后 verifier 不更新 | 密码变更时 API 层清除 `RoleStatus.EncryptedPassword = ""`，Controller 检测到空值后重新计算 SCRAM |
| 3 | **ResetPassword 不更新 EncryptedPassword** | 仅更新 Secret，不触发 verifier 重算 | ResetPassword 时清除 `EncryptedPassword`，让 Controller 在下一次 reconcile 时重新计算 |

#### 3.3.2 PATCH 实现流程

```
GET single role
  └── apiService.GetRole(branchID, roleName)
        ├── 验证 Branch 存在
        ├── GET Role CR (by branchID + roleName)
        ├── 验证属于该 Branch
        └── 返回 toRoleResponse()

PATCH role
  └── apiService.UpdateRole(branchID, roleName, req)
        ├── 验证 Branch 存在
        ├── GET Role CR（通过 branchID + roleName 定位）
        ├── 验证非 Protected 角色
        ├── [仅密码变更] 密码更新子流程
        │     ├── 生成新明文密码
        │     ├── 原地更新 Secret.StringData["password"] = newPassword
        │     ├── 清除 RoleStatus.EncryptedPassword = ""（触发 Controller 重算 SCRAM）
        │     └── 创建 Operation CR（追踪异步密码推送到 compute_ctl）
        ├── [仅名称变更] 重命名子流程（详见 3.3.3）
        │     ├── 更新 RoleSpec.Name = newName
        │     ├── 更新 Secret.Data["username"] = newName
        │     ├── CR metadata.name 保持不变（不触发重命名）
        │     └── Controller 生成 DeltaOp(action="rename_role") 推给 compute_ctl
        ├── [两者变更] 组合以上两步
        ├── Patch Role CR（spec 字段）
        └── 返回 toRoleResponse() + operations[]
```

> **关键决策：不改名 CR 的 `metadata.name`。** Kubernetes 不支持 rename 操作——修改 `metadata.name` 意味着必须先删除旧 CR 再创建新 CR，这会带来非原子操作、UID 变化导致的 OwnerReference 断裂、Controller 竞态窗口等风险。详见下文 3.3.4 策略对比。

**密码更新子流程（对标 Neon）：**

1. API 层生成新密码明文，原地更新 Secret 的 `password` 字段
2. API 层清除 `RoleStatus.EncryptedPassword = ""`（强制 Controller 重算 SCRAM verifier）
3. Controller 检测到 `EncryptedPassword` 为空 → 从 Secret 读取新密码 → 计算 SCRAM-SHA-256 → 写回 Status
4. Controller 将新 verifier 写入 compute_ctl 的 `/configure` spec
5. compute_ctl 执行 `ALTER ROLE ... LOGIN PASSWORD '...'`
6. Operation 标记完成

**名称变更子流程（策略 B：不改名 CR metadata.name）：**

1. API 层更新 `RoleSpec.Name = newName`
2. API 层原地更新 Secret 的 `username` 字段为 `newName`
3. **CR 的 `metadata.name` 保持不变** —— Secret 名称不变，`PasswordSecretRef` 无需更新，所有下游引用不中断
4. Controller 生成 `DeltaOp(action="rename_role")` 推给 compute_ctl
5. compute_ctl 执行 `ALTER ROLE old_name RENAME TO new_name`

#### 3.3.3 角色命名约定

| 概念 | 命名 | 说明 |
|------|------|------|
| CR `metadata.name` | `{branchID}-{roleName}` | 创建时确定，**后续不变**（即使角色被重命名） |
| Secret 名称 | `role-{branchID}-{roleName}-password` | 创建时确定，**后续不变** |
| `RoleSpec.Name` | 用户指定的角色名 | **可变更**（重命名场景） |
| Secret `username` | 同步于 `RoleSpec.Name` | 重命名时更新 |
| `PasswordSecretRef` | 指向 Secret | **始终不变**（Secret 名称不变） |

#### 3.3.4 设计决策：重命名策略对比（B vs C）

| 维度 | 策略 B（不改名 CR）✅ | 策略 C（重命名 CR）❌ |
|------|----------------------|----------------------|
| **可行性** | Kubernetes 原生支持 | K8s 不支持 rename，需 delete + create 模拟 |
| **原子性** | 所有更新通过 Patch，失败无副作用 | 多步链路（删除→创建→迁移引用），任一步失败导致不一致态 |
| **Secret 泄漏** | 不产生孤儿 Secret | delete + create 必须手动清理旧 Secret，失败导致泄漏 |
| **UID 变化** | UID 不变，OwnerReference 链保持 | UID 变化，所有依赖的级联清理断裂 |
| **Controller 竞态** | 无 | 旧 CR 删除触发 finalizer（清理 PG 角色）vs 新 CR 创建（创建 PG 角色），并发冲突 |
| **下游引用** | 无需更新（CR metadata.name 不变） | Endpoint/Branch/Operation 中的名称引用全部需同步更新 |
| **Secret 名称一致性** | Secret 名含旧 roleName（内部实现细节，对外不可见） | 名称一致但代价过大 |

**结论：策略 B 是唯一合理选择。** Secret 名称包含旧角色名是内部实现细节，调用方通过 `PasswordSecretRef` 或 `connection_uri` API 获取凭据，从不直接按名称查找 Secret，因此不影响任何功能。

---

## 四、Database GET single + PATCH + connection_uri

### 4.1 API 接口

#### GET /api/v2/projects/{project_id}/branches/{branch_id}/databases/{database_name}

**请求体：** 无

**响应（200 OK）：**

```json
{
  "database": {
    "name": "myapp",
    "branch_id": "br-quiet-breeze",
    "owner_name": "myapp_owner",
    "created_at": "2026-01-15T08:30:00Z",
    "updated_at": "2026-06-30T10:00:00Z"
  }
}
```

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 404 | `DATABASE_NOT_FOUND` | 数据库不存在或不属于该分支 |

#### PATCH /api/v2/projects/{project_id}/branches/{branch_id}/databases/{database_name}

**请求体：**

```json
{
  "database": {
    "name": "renamed_db",
    "owner_name": "new_owner"
  }
}
```

**可更新字段：**

| 字段 | 类型 | 语义 | 说明 |
|------|------|------|------|
| `database.name` | string | Upsert/Noop | 重命名数据库 |
| `database.owner_name` | string | Upsert/Noop | 修改数据库所有者角色 |

**上游对比：**
- Neon 上游 compute_ctl 的 `CreateAndAlterDatabases` 阶段支持：
  - `ALTER DATABASE ... OWNER TO ...`（owner 变更）
  - 重命名通过 `DeltaOp(action="rename_db")` 实现

**响应：** 200 OK，返回完整的 Database 对象。

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 400 | `INVALID_NAME` | 名称为空或超 63 字符 |
| 404 | `DATABASE_NOT_FOUND` | 数据库不存在 |
| 409 | `DATABASE_NAME_EXISTS` | 目标名称已被占用 |

#### GET /api/v2/projects/{project_id}/connection_uri

**查询参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `branch_id` | string | 否 | 分支 ID，默认使用项目默认分支 |
| `endpoint_id` | string | 否 | 端点 ID，默认使用分支的 read_write 端点 |
| `database_name` | string | **是** | 数据库名称 |
| `role_name` | string | **是** | 角色名称 |
| `pooled` | boolean | 否 | 返回带 pgbouncer 连接池的连接 URI |

**响应（200 OK）：**

```json
{
  "uri": "postgresql://rolename:password@ep-quiet-breeze-123456.us-east-1.aws.neon.tech/dbname?sslmode=require",
  "pooled": false,
  "database_name": "neondb",
  "role_name": "neondb_owner",
  "branch_id": "br-quiet-breeze",
  "endpoint_id": "ep-dawn-butterfly-123456"
}
```

**错误码：**

| HTTP | Code | 场景 |
|------|------|------|
| 400 | `INVALID_REQUEST` | 缺少必填参数 |
| 404 | `BRANCH_NOT_FOUND` | 分支不存在 |
| 404 | `ENDPOINT_NOT_FOUND` | 端点不存在 |
| 404 | `DATABASE_NOT_FOUND` | 数据库不存在 |
| 404 | `ROLE_NOT_FOUND` | 角色不存在 |

> #### 设计决策：为什么返回 URI + 元信息，而非 Neon 官方的 URI-only？
>
> Neon 官方 `connection_uri` 仅返回 `{"uri": "..."}`。Operator 选择扩展为 URI + 元信息（`pooled`, `database_name`, `role_name`, `branch_id`, `endpoint_id`），原因如下：
>
> | 维度 | Neon 官方（URI-only） | Operator（URI + 元信息） |
> |------|----------------------|-------------------------|
> | **默认值透明度** | DNS host 中嵌入 `ep-xxx` 可反解析 | K8s Service host（`my-svc.ns.svc.cluster.local`）**不可反解析**为资源 ID——不返回元信息，调用方无从得知实际使用了哪个 branch/endpoint |
> | **API 链式调用** | Dashboard 闭环，弱需求 | CI/CD Pipeline 常见场景：获取 URI → 发现问题 → PATCH endpoint，需 `endpoint_id` 做链路调用 |
> | **可观测性** | Console UI 可查 | 仅有 API + kubectl，元信息可直接写入日志/审计，无需额外 API 调用 |
> | **向后兼容** | — | 仅在 `uri` 字段基础上**新增**字段，完全兼容 Neon 官方响应结构 |

### 4.2 CRD 变更分析

**当前 Database 模型已满足需求，无需修改 CRD。**

```go
type DatabaseSpec struct {
    BranchID  string `json:"branchID"`
    Name      string `json:"name"`
    OwnerName string `json:"ownerName,omitempty"`
}
```

### 4.3 K8s 实现策略

```
GET single database
  └── apiService.GetDatabase(branchID, dbName)
        ├── 验证 Branch 存在
        ├── GET Database CR (by name)
        │    └── 使用 label 或遍历同 branchID 的 databases
        ├── 验证属于该 Branch
        └── 返回 toDatabaseResponse()

PATCH database
  └── apiService.UpdateDatabase(branchID, dbName, req)
        ├── 验证 Branch 存在
        ├── GET Database CR
        ├── 若 name 变更:
        │     ├── 检查目标名称不冲突
        │     ├── 更新 spec.Name
        │     └── Controller 生成 DeltaOp(action="rename_db")
        ├── 若 owner_name 变更:
        │     ├── 验证目标 Role 存在于同一 Branch
        │     ├── 更新 spec.OwnerName
        │     └── Controller 执行 ALTER DATABASE ... OWNER TO ...
        ├── MergeFrom patch
        └── 返回 toDatabaseResponse()

GET connection_uri
  └── apiService.GetConnectionURI(projectID, params)
        ├── 确定 branch: 若未传则选项目默认分支
        ├── 确定 endpoint: 若未传则选分支的 read_write 端点
        ├── GET Endpoint CR → 获取 Host, Port
        ├── GET Role CR → 从 Secret 获取密码明文
        ├── 构造 URI: postgresql://{role}:{password}@{host}:{port}/{db}?sslmode=require
        ├── 若 pooled=true:
        │    └── 添加 ?options=--cluster=...&pooler=true 或替换 host 为 pooler 地址
        └── 返回 URI
```

**connection_uri 关键设计：**

1. **密码获取**：从 Role 关联的 K8s Secret 中读取明文密码（非 SCRAM verifier）
2. **Host/Port**：从 EndpointStatus.Host 和 EndpointStatus.Port 获取（由 Endpoint Controller 写入）
3. **SSL**：如果 cluster 配置了 TLS，使用 `sslmode=require`；否则使用 `sslmode=disable`
4. **Pooler**：如果配置了 PgBouncer，pooled=true 时 host 替换为 pooler Service 地址

---

## 五、通用设计模式

### 5.1 代码组织

延续 Project PATCH 的既有模式：

```
internal/controlplane/
├── api_types.go       # Nullable[T], 请求/响应类型
├── api_routes.go      # 路由注册
├── api_service.go     # 业务逻辑（新增方法）
├── api_handlers.go    # HTTP handler 层
└── *_test.go          # 单元测试
```

### 5.2 实施顺序

| 优先级 | 端点 | 复杂度 | 理由 |
|--------|------|--------|------|
| P0-1 | Branch PATCH + set_as_default | 低 | CRD 已完备，仅需 API 层代码 |
| P0-2 | Role GET single + PATCH | 中 | 需处理密码加密和 Secret 操作 |
| P0-3 | Database GET single + PATCH | 中 | 需处理重命名和 owner 变更 |
| P0-4 | Endpoint PATCH + start/suspend/restart | 高 | 涉及生命周期状态机和 controller 联动 |
| P0-5 | Connection URI | 中 | 需端到端的 host/port/secret 组装 |

### 5.3 测试策略

每个资源类型包含以下测试套件：

1. **HTTP handler 单元测试** — 覆盖正常流程、错误路径、幂等性
2. **API Service 单元测试** — 使用 fake K8s client，验证 Patch 语义
3. **集成测试** — envtest 框架（已有基础设施），验证 controller 生命周期
4. **响应结构快照测试** — 验证 JSON 响应与 Neon API 兼容

### 5.4 安全考量

| 端点 | 安全措施 |
|------|---------|
| Branch PATCH | 禁止修改 protected=true → false（需单独 unstuck 操作） |
| set_as_default | 必须验证分支属于目标项目 |
| Endpoint start/suspend | 禁止对 409 状态（正在操作中）的端点重复操作 |
| Role PATCH | 禁止修改 Protected 系统角色；密码必须校验复杂度 |
| Database PATCH | 禁止删除最后一个数据库（分支至少保留 1 个数据库） |
| connection_uri | 密码明文仅在内存中使用，不记录日志 |

---

## 六、与 Neon 官方 API 的兼容性差异

| 方面 | Neon 官方 | Operator 实现 | 差异说明 |
|------|----------|--------------|---------|
| Endpoint suspend | 真正的 scale-to-zero，释放计算资源 | K8s Deployment replicas=0，Pod 删除 | 语义等价，行为略有不同 |
| Endpoint restart | suspend → start 两步 | 滚动重启（或 suspend→start） | 简化实现 |
| Role PATCH | 无独立端点，通过 /configure | 独立 PATCH 端点 | 增强体验 |
| connection_uri | 返回单一连接字符串 `{"uri": "..."}` | 返回 `uri` + `pooled`/`database_name`/`role_name`/`branch_id`/`endpoint_id` | 见 4.1 节设计决策：K8s host 不可反解析为资源 ID，需元信息支撑链路调用与可观测性 |
| Operation 跟踪 | 全量异步操作跟踪 | 对复杂操作（restart/rename）创建 Operation CR | 对标实现 |

---

## 七、实施计划

```
Week 1: Branch PATCH + set_as_default
  ├── api_types.go: BranchUpdateRequest, BranchSetDefault
  ├── api_service.go: UpdateBranch(), SetBranchAsDefault()
  ├── api_routes.go: register routes
  └── *_test.go: unit + integration tests

Week 2: Role GET single + PATCH
  ├── api_types.go: RoleUpdateRequest
  ├── api_service.go: GetRole(), UpdateRole()
  ├── api_routes.go: register routes
  ├── Controller: handle role rename + password change
  └── *_test.go: unit + integration tests

Week 3: Database GET single + PATCH + connection_uri
  ├── api_types.go: DatabaseUpdateRequest, ConnectionURIRequest
  ├── api_service.go: GetDatabase(), UpdateDatabase(), GetConnectionURI()
  ├── api_routes.go: register routes
  ├── Controller: handle database rename + owner change
  └── *_test.go: unit + integration tests

Week 4: Endpoint PATCH + start/suspend/restart
  ├── api_types.go: EndpointUpdateRequest
  ├── api_service.go: UpdateEndpoint(), StartEndpoint(), SuspendEndpoint(), RestartEndpoint()
  ├── api_routes.go: register routes
  ├── Controller: handle suspend/start lifecycle
  ├── Enable SuspendTimeoutSeconds in CRD
  └── *_test.go: unit + integration tests
```
