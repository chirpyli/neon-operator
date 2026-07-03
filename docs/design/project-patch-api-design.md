# Project PATCH API 设计方案

> 对标 Neon Cloud API `PATCH /api/v2/projects/{project_id}`，基于 Neon 源码深度调研编写。

| 字段 | 内容 |
| ---- | ---- |
| 版本 | v1.0 |
| 日期 | 2026-06-29 |
| 参考 | [Neon UpdateProject API](https://api-docs.neon.tech/reference/updateproject) |

---

## 一、API 接口规范

### 1.1 请求

```
PATCH /api/v2/projects/{project_id}
Content-Type: application/json
```

**路径参数**：

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `project_id` | string | 是 | 项目唯一标识（对应 K8s CR metadata.name） |

**请求体 JSON Schema**：

```json
{
  "project": {
    "name": "my-renamed-project",
    "default_endpoint_settings": {
      "resources": {
        "cpu": "2",
        "memory": "8Gi"
      }
    },
    "history_retention_seconds": 86400,
    "ip_allow": {
      "primary_branch_only": true,
      "source_ranges": ["203.0.113.0/24", "198.51.100.14/32"]
    }
  }
}
```

**字段说明**：

| 字段 | 类型 | PATCH 语义 | 说明 |
|------|------|------------|------|
| `project` | object | 必填 | 包含要修改字段的容器 |
| `project.name` | string | Upsert/Noop | 项目显示名称（1-256字符） |
| `project.default_endpoint_settings` | object | Upsert/Remove/Noop | 新建端点的默认资源配置。传 `null` 清空。 |
| `project.default_endpoint_settings.resources` | object | Upsert/Remove/Noop | CPU/Memory 资源规格。传 `null` 清空。 |
| `project.history_retention_seconds` | int | Upsert/Noop | 历史数据保留期（秒），默认 604800（7天） |
| `project.ip_allow` | object | Upsert/Remove/Noop | IP 白名单。传 `null` 移除所有规则。 |

**关键设计约束**：
- **未传字段 → 保持原值不变**（对标 Neon `FieldPatch::Noop`）
- **传值 → 更新为新值**（对标 Neon `FieldPatch::Upsert`）
- **传 `null` → 重置为默认值**（对标 Neon `FieldPatch::Remove`）
- 不允许 `PATCH` 修改 `cluster`、`tenantId`、`pgVersion`（创建后不可变）

### 1.2 响应

**200 OK** — 返回更新后的完整 Project 对象：

```json
{
  "project": {
    "id": "project-a1b2c3d4",
    "name": "my-renamed-project",
    "pgVersion": 17,
    "cluster": "my-cluster",
    "tenant_id": "abcdef1234567890abcdef1234567890",
    "default_endpoint_settings": {
      "resources": {
        "cpu": "2",
        "memory": "8Gi"
      }
    },
    "history_retention_seconds": 86400,
    "ip_allow": {
      "primary_branch_only": true,
      "source_ranges": ["203.0.113.0/24", "198.51.100.14/32"]
    },
    "created_at": "2026-06-29T12:00:00Z",
    "updated_at": "2026-06-29T13:00:00Z"
  }
}
```

**错误响应**：

| HTTP 状态码 | `code` | 触发条件 |
|-------------|--------|----------|
| 400 | `INVALID_REQUEST` | JSON 格式错误，或请求体为空 |
| 400 | `INVALID_NAME` | name 长度不在 1-256 范围 |
| 404 | `PROJECT_NOT_FOUND` | project_id 不存在 |
| 409 | `IMMUTABLE_FIELD` | 尝试修改 cluster/tenantId/pgVersion |
| 422 | `VALIDATION_ERROR` | history_retention_seconds 超出范围 |
| 500 | `INTERNAL_ERROR` | K8s API 调用失败 |

### 1.3 与 Neon 官方 API 对比

| 特性 | Neon 官方 | 本设计 | 说明 |
|------|-----------|--------|------|
| PATCH 语义 | FieldPatch\<T\>（Rust） | `*T` / `nil` 指针（Go） | 等价设计，语言差异 |
| null=Remove | 支持 | 支持 | JSON `null` → 重置默认值 |
| 未传=Noop | 支持 | 支持 | 未出现的字段保持不变 |
| name | 可更新 | 可更新 | — |
| default_endpoint_settings | 可更新 | 可更新 | — |
| history_retention_seconds | 可更新 | 可更新 | — |
| ip_allow | 可更新 | 可更新 | — |
| autoscaling_limit_min_cu | Planned | 预留扩展 | Phase 2+ |
| autoscaling_limit_max_cu | Planned | 预留扩展 | Phase 2+ |
| 响应格式 | `{ "project": {...} }` | `{ "project": {...} }` | 完全兼容 |

---

## 二、FieldPatch 三态模式设计

这是本设计的核心，对标 Neon 源码中 `libs/pageserver_api/src/models.rs` 的 `FieldPatch<T>` 枚举：

```rust
// Neon 源码 (models.rs:511-529)
pub enum FieldPatch<T> {
    Upsert(T),   // 设置/更新值
    Remove,      // 删除/清空值（JSON null）
    Noop,        // 保持原值不变
}

#[serde(default)]                           // 默认 = Noop
#[serde(skip_serializing_if = "is_noop")]   // 不序列化 Noop
```

### 2.1 Go 等效实现

在 Go 中，使用 **指针 + `omitempty`** 实现等价的三态语义：

```go
// ProjectUpdatePayload — PATCH 请求体中的 Project 可更新字段。
// 所有字段均为 *T 指针类型：nil=Noop（不变）、非nil=Upsert（设置）。
// JSON "field":null 反序列化后对应非 nil 的零值指针，由业务逻辑处理 Remove。
type ProjectUpdatePayload struct {
    // Name 项目显示名称。 nil: 保持不变，非 nil: 更新名称。
    // +optional
    Name *string `json:"name,omitempty"`

    // DefaultEndpointSettings 默认端点配置。
    // nil: 保持不变，非 nil: 覆盖配置。
    // JSON "default_endpoint_settings": null → 重置为 nil。
    // +optional
    DefaultEndpointSettings *DefaultEndpointSettingsUpdate `json:"default_endpoint_settings,omitempty"`

    // HistoryRetentionSeconds 历史数据保留期（秒）。
    // nil: 保持不变，非 nil: 更新保留期。
    // +optional
    HistoryRetentionSeconds *int64 `json:"history_retention_seconds,omitempty"`

    // IPAllow IP 白名单配置。
    // nil: 保持不变，非 nil: 覆盖配置。
    // JSON "ip_allow": null → 移除所有 IP 限制。
    // +optional
    IPAllow *IPAllowConfigUpdate `json:"ip_allow,omitempty"`
}

// ProjectUpdateRequest — PATCH /api/v2/projects/{project_id} 请求体
type ProjectUpdateRequest struct {
    Project ProjectUpdatePayload `json:"project"`
}
```

### 2.2 三态判定表

| JSON 输入 | Go 反序列化结果 | 语义 | 对应 Neon |
|-----------|----------------|------|-----------|
| 字段未出现 | 指针 = `nil` | Noop — 保持原值 | `FieldPatch::Noop` |
| `"name": "new"` | `*string = &"new"` | Upsert — 设置新值 | `FieldPatch::Upsert(T)` |
| `"name": null` | `*string` 为 nil（omitempty 跳过已存在于 payload 中的 null） | **需要包装类型处理** | `FieldPatch::Remove` |

### 2.3 `null` = Remove 的处理

Go 标准 JSON 无法区分 "未出现" 和 "显式 null"（都是 `nil`）。解决方案：对需要 Remove 语义的字段使用 **自定义 `Nullable[T]` 包装类型**：

```go
// Nullable 是一个 JSON 可空包装，用于区分"未出现"和"显式 null"。
// - JSON 字段不出现 → Nullable 本身为 nil → Noop
// - JSON "field": "value" → Nullable{Value: &value, Valid: true} → Upsert
// - JSON "field": null     → Nullable{Value: nil, Valid: true}     → Remove
type Nullable[T any] struct {
    Value *T
    Valid bool
}

func (n *Nullable[T]) UnmarshalJSON(data []byte) error {
    if string(data) == "null" {
        n.Valid = true   // 显式 null → Remove
        n.Value = nil
        return nil
    }
    n.Valid = true
    n.Value = new(T)
    return json.Unmarshal(data, n.Value)
}

func (n Nullable[T]) MarshalJSON() ([]byte, error) {
    if !n.Valid {
        return []byte("null"), nil  // 不序列化（不应出现）
    }
    if n.Value == nil {
        return []byte("null"), nil  // Remove → null
    }
    return json.Marshal(*n.Value)
}

func (n Nullable[T]) IsNoop() bool  { return !n.Valid }
func (n Nullable[T]) IsRemove() bool { return n.Valid && n.Value == nil }
func (n Nullable[T]) IsUpsert() bool { return n.Valid && n.Value != nil }
```

**使用示例**：对需要 Remove 语义的字段（`DefaultEndpointSettings`、`IPAllow`）使用 `Nullable`：

```go
type ProjectUpdatePayload struct {
    Name                    *string                        `json:"name,omitempty"`
    DefaultEndpointSettings Nullable[DefaultEndpointSettingsUpdate] `json:"default_endpoint_settings"`
    HistoryRetentionSeconds *int64                         `json:"history_retention_seconds,omitempty"`
    IPAllow                 Nullable[IPAllowConfigUpdate]  `json:"ip_allow"`
}
```

**不需要 Remove 的字段**（如 `name`、`history_retention_seconds`）：直接用 `*T` + `omitempty`，不支持显式 null（用户无需对名称做 Remove）。

### 2.4 applyPatch 核心逻辑

```go
// applyProjectPatch 将 PATCH 请求中的变更应用到现有的 Project CR Spec 上。
// 对标 Neon 的 TenantConfig::apply_patch() 方法（models.rs:789-827）。
func (s *apiService) applyProjectPatch(
    current *neonv1.Project,
    patch ProjectUpdatePayload,
) (changed bool, err error) {
    spec := &current.Spec

    // --- name: Upsert / Noop ---
    if patch.Name != nil {
        if len(*patch.Name) < 1 || len(*patch.Name) > 256 {
            return false, newError("INVALID_NAME", "project name must be 1-256 characters")
        }
        spec.Name = *patch.Name
        changed = true
    }

    // --- default_endpoint_settings: Upsert / Remove / Noop ---
    if patch.DefaultEndpointSettings.IsUpsert() {
        ds := patch.DefaultEndpointSettings.Value
        if spec.DefaultEndpointSettings == nil {
            spec.DefaultEndpointSettings = &neonv1.EndpointDefaults{}
        }
        if ds.Resources != nil {
            spec.DefaultEndpointSettings.Resources = toResourceRequirements(ds.Resources)
            changed = true
        }
    } else if patch.DefaultEndpointSettings.IsRemove() {
        spec.DefaultEndpointSettings = nil
        changed = true
    }

    // --- history_retention_seconds: Upsert / Noop ---
    if patch.HistoryRetentionSeconds != nil {
        spec.HistoryRetentionSeconds = *patch.HistoryRetentionSeconds
        changed = true
    }

    // --- ip_allow: Upsert / Remove / Noop ---
    if patch.IPAllow.IsUpsert() {
        cfg := patch.IPAllow.Value
        spec.IPAllow = &neonv1.IPAllowConfig{
            PrimaryBranchOnly: cfg.PrimaryBranchOnly,
            SourceRanges:      cfg.SourceRanges,
        }
        changed = true
    } else if patch.IPAllow.IsRemove() {
        spec.IPAllow = nil
        changed = true
    }

    return changed, nil
}
```

---

## 三、数据模型扩展

### 3.1 ProjectSpec 新增字段

需要在 `api/v1alpha1/project_types.go` 中新增两个字段：

```go
type ProjectSpec struct {
    // ... 现有字段 ...

    // HistoryRetentionSeconds 历史数据保留期（秒）。
    // 控制 PITR（Point-In-Time Recovery）的时间窗口。
    // 默认 604800（7 天）。
    // +optional
    // +kubebuilder:default:=604800
    // +kubebuilder:validation:Minimum:=0
    HistoryRetentionSeconds int64 `json:"historyRetentionSeconds,omitempty"`

    // IPAllow IP 白名单配置。
    // nil 表示不对 IP 进行限制。
    // +optional
    IPAllow *IPAllowConfig `json:"ipAllow,omitempty"`
}

// IPAllowConfig 定义 IP 访问控制规则
type IPAllowConfig struct {
    // PrimaryBranchOnly 是否仅对主分支生效
    // +optional
    PrimaryBranchOnly bool `json:"primaryBranchOnly,omitempty"`

    // SourceRanges 允许的 CIDR 范围列表
    // +optional
    SourceRanges []string `json:"sourceRanges,omitempty"`
}
```

### 3.2 ProjectResponse 扩展

在 `internal/controlplane/api_types.go` 中扩展响应类型，使其返回新增字段：

```go
type ProjectResponse struct {
    // ... 现有字段 ...

    HistoryRetentionSeconds int64          `json:"history_retention_seconds,omitempty"`
    IPAllow                 *IPAllowResp   `json:"ip_allow,omitempty"`
}

type IPAllowResp struct {
    PrimaryBranchOnly bool     `json:"primary_branch_only"`
    SourceRanges      []string `json:"source_ranges"`
}
```

### 3.3 PATCH 相关新类型

在 `internal/controlplane/api_types.go` 中新增：

```go
// ---------- Project PATCH ----------

// ProjectUpdateRequest PATCH /api/v2/projects/{project_id}
type ProjectUpdateRequest struct {
    Project ProjectUpdatePayload `json:"project"`
}

// ProjectUpdatePayload PATCH 请求体中的可更新字段。
// 所有字段为可选。未出现的字段保持原值不变。
type ProjectUpdatePayload struct {
    Name                    *string                                        `json:"name,omitempty"`
    DefaultEndpointSettings Nullable[DefaultEndpointSettingsUpdate]       `json:"default_endpoint_settings"`
    HistoryRetentionSeconds *int64                                         `json:"history_retention_seconds,omitempty"`
    IPAllow                 Nullable[IPAllowConfigUpdate]                 `json:"ip_allow"`
}

// DefaultEndpointSettingsUpdate 端点默认配置的 PATCH 负载
type DefaultEndpointSettingsUpdate struct {
    Resources *ComputeResources `json:"resources,omitempty"`
}

// IPAllowConfigUpdate IP 白名单的 PATCH 负载
type IPAllowConfigUpdate struct {
    PrimaryBranchOnly bool     `json:"primary_branch_only"`
    SourceRanges      []string `json:"source_ranges"`
}

// ---------- Nullable type ----------

// Nullable 可空包装，区分 "未出现"、"显式 null"、"有值" 三种状态。
// 用于 PATCH 请求中的 FieldPatch 三态语义：
//   - nil → Noop（字段未出现）
//   - {Valid: true, Value: nil} → Remove（JSON null）
//   - {Valid: true, Value: &T} → Upsert（有值）
type Nullable[T any] struct {
    Value *T
    Valid bool
}

func (n *Nullable[T]) UnmarshalJSON(data []byte) error {
    if string(data) == "null" {
        n.Valid = true
        n.Value = nil
        return nil
    }
    n.Valid = true
    n.Value = new(T)
    return json.Unmarshal(data, n.Value)
}

func (n Nullable[T]) MarshalJSON() ([]byte, error) {
    if !n.Valid || n.Value == nil {
        return []byte("null"), nil
    }
    return json.Marshal(*n.Value)
}

func (n Nullable[T]) IsNoop() bool  { return !n.Valid }
func (n Nullable[T]) IsRemove() bool { return n.Valid && n.Value == nil }
func (n Nullable[T]) IsUpsert() bool { return n.Valid && n.Value != nil }
```

---

## 四、实施步骤

### Phase 1: 核心类型与 CRD 扩展

**文件**：`api/v1alpha1/project_types.go`

1. 在 `ProjectSpec` 中新增 `HistoryRetentionSeconds int64` 和 `IPAllow *IPAllowConfig`
2. 新增 `IPAllowConfig` 结构体
3. 运行 `make generate` 重新生成 deepcopy 代码
4. 运行 `make manifests` 重新生成 CRD YAML

**文件**：`internal/controlplane/api_types.go`

5. 新增 `ProjectUpdateRequest`、`ProjectUpdatePayload`
6. 新增 `DefaultEndpointSettingsUpdate`、`IPAllowConfigUpdate`
7. 新增 `Nullable[T]` 泛型类型及 JSON 序列化方法
8. 扩展 `ProjectResponse` 增加 `HistoryRetentionSeconds`、`IPAllow` 字段

### Phase 2: 业务逻辑层

**文件**：`internal/controlplane/api_service.go`

9. 实现 `UpdateProject(ctx, projectID, req) (*ProjectResponse, error)` 方法：
   - 从 K8s 获取 Project CR
   - 验证权限和存在性
   - 调用 `applyProjectPatch()` 计算变更
   - 若无变更则直接返回当前状态（200 OK，幂等）
   - 使用 `client.MergeFrom` 执行 K8s PATCH 更新 CR
   - 构造并返回 `ProjectResponse`

10. 实现 `applyProjectPatch(current, patch) (changed, error)` 辅助函数
11. 扩展 `toDefaultSettings()` → `toProjectResponse()` 映射新增字段

### Phase 3: HTTP 路由与 Handler

**文件**：`internal/controlplane/api_routes.go`

12. 注册路由：`mux.Handle("PATCH /api/v2/projects/{project_id}", ...)`
13. 实现 `patchProject` handler 方法

```go
func (h *apiHandler) patchProject(w http.ResponseWriter, r *http.Request) {
    defer func() {
        if err := r.Body.Close(); err != nil {
            h.log.Error("failed to close request body", "error", err)
        }
    }()

    projectID := r.PathValue("project_id")

    var req ProjectUpdateRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON request body")
        return
    }

    resp, err := h.svc.UpdateProject(r.Context(), projectID, req)
    if err != nil {
        h.handleAPIError(w, err)
        return
    }

    _ = writeJSON(w, http.StatusOK, map[string]interface{}{"project": resp})
}
```

14. 扩展 `handleAPIError` 支持新增错误码（IMMUTABLE_FIELD → 409, VALIDATION_ERROR → 422）

### Phase 4: 部署验证

15. `make manifests generate build docker-build docker-push deploy`
16. 验证 PATCH 请求：
```bash
# 更新名称
curl -X PATCH http://localhost:8080/api/v2/projects/project-a1b2c3d4 \
  -H "Content-Type: application/json" \
  -d '{"project":{"name":"renamed-project"}}'

# 更新资源配置
curl -X PATCH http://localhost:8080/api/v2/projects/project-a1b2c3d4 \
  -H "Content-Type: application/json" \
  -d '{"project":{"default_endpoint_settings":{"resources":{"cpu":"4","memory":"16Gi"}}}}'

# 清空 IP 限制（Remove 语义）
curl -X PATCH http://localhost:8080/api/v2/projects/project-a1b2c3d4 \
  -H "Content-Type: application/json" \
  -d '{"project":{"ip_allow":null}}'
```

---

## 五、关键设计决策

### 5.1 为何使用 K8s Patch 而非 Update

| 方式 | 优点 | 缺点 |
|------|------|------|
| `client.Update()` | 简单直接 | 需要 Read-Modify-Write，有竞态风险 |
| `client.Patch()` (MergeFrom) | **原子操作**，仅发送变更字段，无竞态 | 需要事先 DeepCopy 原始对象 |

使用 `client.MergeFrom(original)`：对 K8s 资源执行 **strategic merge patch**，仅修改用户在请求中指定的字段，避免无意覆盖并发修改。

### 5.2 为何需要 Nullable[T] 包装

Go 标准 JSON 无法区分 `"field": null`（显式 null）和字段不出现（都是 `nil`）。`Nullable[T]` 通过 `Valid` 布尔标志保存"是否在 JSON 中出现"的元信息，是 `FieldPatch::Remove` 在 Go 中的最低成本实现。

该类型约束对标 Neon Rust 源码的 serde 反序列化行为：
- `#[serde(default)]` → 默认 Noop
- `#[serde(skip_serializing_if = "FieldPatch::is_noop")]` → 不序列化 Noop

### 5.3 幂等性保证

- 未传字段 → Noop（不变），多次 PATCH 相同请求结果一致
- 无实际变更时返回 200 + 当前状态（不触发写操作）
- 对标 Neon 官方 API 行为

### 5.4 不可变字段保护

创建后不可修改的字段（`cluster`、`tenantId`、`pgVersion`）不在 `ProjectUpdatePayload` 中，从 API 层面杜绝修改企图。

---

## 六、文件变更清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `api/v1alpha1/project_types.go` | 修改 | 新增 HistoryRetentionSeconds、IPAllowConfig |
| `api/v1alpha1/zz_generated.deepcopy.go` | 自动生成 | `make generate` 产出 |
| `config/crd/bases/neon.oltp.molnett.org_projects.yaml` | 自动生成 | `make manifests` 产出 |
| `internal/controlplane/api_types.go` | 修改 | 新增 PATCH 类型、Nullable、扩展 Response |
| `internal/controlplane/api_routes.go` | 修改 | 注册 PATCH 路由 + handler |
| `internal/controlplane/api_service.go` | 修改 | 实现 UpdateProject 方法 |
| `internal/controlplane/routes_test.go` | 新增测试 | PATCH API 单元测试 |

---

## 七、与 Architecture 的关系

```
用户 / SDK
    │
    │  PATCH /api/v2/projects/{project_id}
    │  { "project": { "name": "...", ... } }
    ▼
┌─────────────────────────────┐
│  ControlPlane HTTP Server   │  ← api_routes.go (路由)
│  (controlplane 包)           │
│                             │
│  apiHandler.patchProject()  │  ← 请求解析
│       │                     │
│       ▼                     │
│  apiService.UpdateProject() │  ← 业务逻辑 (api_service.go)
│       │                     │
│       ├─ k8sClient.Get()          ── 读取 Project CR
│       ├─ applyProjectPatch()      ── 计算字段级变更
│       ├─ k8sClient.Patch(MergeFrom) ── 原子更新 CR
│       └─ toProjectResponse()      ── 构造 API 响应
└─────────────┬───────────────┘
              │
              ▼  Project.Spec.Name / DefaultEndpointSettings 更新
┌─────────────────────────────┐
│  Kubernetes API Server       │
│  Project CR (修改后)         │
│       │                     │
│       ▼  watch event         │
│  ProjectReconciler           │  ← 无需变更（现有调和逻辑不变）
│  (internal/controller)       │
│       │                     │
│       ▼  PUT /v1/tenant/{id}/location_config (已附带新 TenantConfig)
│  Storage Controller          │
└─────────────────────────────┘
```

**关键点**：PATCH API 直接修改 K8s CR Spec，Controller 的调和循环自动感知变更并传播到下游（Storage Controller → Pageserver）。无需在 API 层直接调用 Storage Controller 的 `PATCH /v1/tenant/config`。
