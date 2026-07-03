# 上游 Neon 生产环境架构分析

> 基于对 `/home/postgres/works/opensource/neon` 源码的分析，理解 compute_ctl 在生产环境中的正确行为。

| 字段 | 内容 |
|------|------|
| 版本 | v1.0 |
| 日期 | 2026-06-24 |
| 来源 | `/home/postgres/works/opensource/neon` 源码 |

---

## 1. 核心架构 — 谁调用谁

```
                    ┌──────────────────────────────────────┐
                    │           Storage Controller         │
                    │  (compute_hook.rs)                   │
                    │                                      │
                    │  notify_attach() ─────────────────┐  │
                    │  notify_safekeepers() ────┐       │  │
                    └─────────────┬─────────────┼───────┼──┘
                                  │             │       │
                                  │     PUT /notify-     │
                                  │     safekeepers      │
                                  │             │       │
                    ┌─────────────▼─────────────▼───────▼──┐
                    │         Control Plane (console)      │
                    │                                      │
                    │  GET  /compute/.../{id}/spec         │
                    │  PUT  /notify-attach                 │
                    │  PUT  /notify-safekeepers            │
                    │  POST /compute/.../{id}/configure    │
                    └──────────────┬───────────────────────┘
                                   │
                    GET /spec      │   POST /configure
                    (拉取配置)      │   (推送配置)
                                   │
                    ┌──────────────▼───────────────────────┐
                    │          compute_ctl                 │
                    │                                      │
                    │  1. GET /spec → 获取 ComputeSpec     │
                    │  2. 接收 POST /configure → 更新配置   │
                    │  3. 启动 PostgreSQL                   │
                    └──────────────────────────────────────┘
```

### 关键发现：`/notify-attach` 的调用者

**`/notify-attach` 是 Storage Controller 调用的，不是 compute_ctl！**

在 upstream 代码 `storage_controller/src/compute_hook.rs` 中：

```rust
impl ApiMethod for ComputeHookTenant {
    const API_PATH: &'static str = "notify-attach";
    // ...
}
```

Storage Controller 通过 `ComputeHook` 模块，在检测到 pageserver shard 拓扑变化时，向控制平面发送 `/notify-attach` 请求，触发 compute 节点重新配置。

---

## 2. compute_ctl 启动流程（生产环境）

### 2.1 两种配置获取模式

```rust
// compute_tools/src/bin/compute_ctl.rs

fn get_config(cli: &Cli) -> Result<ComputeConfig> {
    // 模式 A: 本地文件 (开发/测试)
    if let Some(ref config) = cli.config {
        let file = File::open(config)?;
        return Ok(serde_json::from_reader(&file)?);
    }

    // 模式 B: 从控制平面拉取 (生产)
    get_config_from_control_plane(cli.control_plane_uri, &cli.compute_id)
}
```

在本地测试中 (`neon_local`)，`Endpoint::start()` 构建完整 ComputeSpec 写入 `config.json`，compute_ctl 以 `--config config.json` 启动。

**在生产环境中，compute_ctl 以 `--control-plane-uri` 启动，无 `--config` 文件。**

### 2.2 /spec API 重试策略

```rust
// compute_tools/src/spec.rs: get_config_from_control_plane()

// 最多 3 次尝试，间隔 100ms
while attempt < 4 {
    match do_control_plane_request(&cp_uri, &jwt) {
        Ok(config_resp) => {
            match config_resp.status {
                // Empty → 返回 ComputeConfig { spec: None }
                //   计算节点已知但尚未分配到任何 timeline
                ControlPlaneComputeStatus::Empty => Ok(config_resp.into()),

                // Attached + spec 有值 → 成功，返回完整配置
                ControlPlaneComputeStatus::Attached if config_resp.spec.is_some() => Ok(...),
                // Attached + spec 为空 → 错误
                ControlPlaneComputeStatus::Attached => bail!("spec is empty"),
            }
        }
        Err((retry, msg, status)) => {
            if retry {
                // 503/502/网络错误 → 重试
                error!("attempt {} failed: {}", attempt, msg);
            } else {
                // 500/404 → 不可重试，立即退出
                bail!(msg);
            }
        }
    }
}
```

| HTTP 状态码 | 行为 |
|-------------|------|
| `200 + spec + Attached` | 成功，返回完整配置 |
| `200 + Empty` | 返回空配置，等待 `/configure` |
| `503 SERVICE_UNAVAILABLE` | 可重试 |
| `502 BAD_GATEWAY` | 可重试 |
| 网络错误 | 可重试 |
| **500 / 404 / 其他** | **不可重试，立即退出** |

### 2.3 Empty 状态 → Attached 状态转换

当 `/spec` 返回 `Status=Empty`（spec 为 None）：

1. compute_ctl 进入等待状态
2. 启动 HTTP 服务器监听 `/configure` 端点
3. **等待控制平面通过 `/configure` 推送完整 ComputeSpec**
4. 收到 spec 后提取 `tenant_id`、`timeline_id` 等，启动 PostgreSQL

---

## 3. tenant_id 从哪里来

### 3.1 compute_ctl **从不随机生成 tenant_id**

```rust
// compute_tools/src/compute.rs: ParsedSpec::try_from()

let tenant_id = if let Some(tenant_id) = spec.tenant_id {
    tenant_id                                                    // 新方式: spec.tenant_id
} else {
    let guc = spec.cluster.settings.find("neon.tenant_id")
        .ok_or(anyhow::anyhow!("tenant id should be provided"))?; // 旧方式: GUC
    TenantId::from_str(&guc)?
};
```

**tenant_id 只来自两个地方：**
1. `spec.tenant_id` 顶层字段（推荐方式）
2. `spec.cluster.settings` 中的 `neon.tenant_id` GUC（向后兼容）

如果两者都没有 → **硬错误退出**，不存在"随机生成"的逻辑。

### 3.2 控制平面是 tenant_id 的唯一权威

在生产环境中，控制平面 (console) 从数据库中获取 endpoint 信息，构建完整的 `ComputeSpec`（含 `spec.tenant_id`），通过 `/spec` 或 `/configure` 返回给 compute_ctl。

**compute_ctl 本身不生成也不存储 tenant_id，它完全依赖控制平面下发的 spec。**

---

## 4. 完整 ComputeSpec 格式

```json
{
  "format_version": 1.0,
  "spec": {
    "tenant_id": "...",
    "timeline_id": "...",
    "cluster": {
      "cluster_id": "...",
      "name": "...",
      "settings": [
        {"name": "neon.tenant_id", "value": "...", "vartype": "string"},
        {"name": "neon.timeline_id", "value": "...", "vartype": "string"}
      ],
      "roles": [...],
      "databases": [...]
    },
    "pageserver_connection_info": {
      "shard_count": 1,
      "shards": {
        "0001": {
          "pageservers": [
            {"id": 0, "libpq_url": "postgres://...", "grpc_url": null}
          ]
        }
      }
    },
    "safekeeper_connstrings": ["..."],
    "mode": "Primary"
  },
  "compute_ctl_config": {
    "jwks": {"keys": [...]}
  },
  "status": "Attached"
}
```

---

## 5. 与当前 Operator 对比

| 维度 | 上游 Neon (生产) | 当前 Operator |
|------|------------------|---------------|
| **Branch 与 Compute 关系** | Branch 是纯 timeline 容器，Compute 由 Endpoint 提供 | ✅ **已对齐** — Branch 不自动创建 Compute，Endpoint CRD 管理计算实例 |
| **Endpoint Type** | `read_write` / `read_only` | ✅ **已实现** — `read_write` → WAL proposer，`read_only` → follower |
| **read_write 唯一性** | 每 Branch 最多 1 个 | ✅ **已实现** — API 层面校验 |
| compute_ctl 启动方式 | `--control-plane-uri` (无 --config) | `--control-plane-uri` (有 INITIAL_SPEC_JSON) |
| 初始 spec 内容 | 控制平面从 DB 构建完整 spec | ConfigMap 中仅 JWKS，无 cluster |
| `/spec` 端点 | 返回完整 spec 或 Empty | 可能因 GetTenantInfo 失败而 500 |
| `/notify-attach` 调用者 | **Storage Controller** (compute_hook.rs) | _同_ |
| tenant_id 来源 | 始终从 spec 获取，永不随机生成 | _compute_ctl 不应随机生成_ |
| 空状态处理 | Empty → 等待 `/configure` 推送 | ⚠️ 未明确实现 /configure 推送流程 |

---

## 6. 运算符问题根因再分析

### 6.1 /notify-attach 中的 "随机 tenant_id" 来自哪里？

根据上游分析，**compute_ctl 绝不随机生成 tenant_id**。那么用户日志中 `/notify-attach` 携带的随机 ID 最可能来自：

1. **Storage Controller 的 ComputeHook** — storage controller 内部维护 tenant→pageserver 映射，如果它使用了错误的 tenant_id（例如从错误的数据源读取），那么它发起的 `/notify-attach` 就会带有不匹配的 ID。

2. **时序问题** — 在 Operator 的 reconcile 流程中，可能 storage controller 先于 project/branch CR 创建就开始了 notify 循环，此时 tenant_id 还不正确。

### 6.2 Compute Pod Error 的真正原因

```
compute_ctl 启动
  └─ 读 INITIAL_SPEC_JSON → 仅有 JWKS
  └─ GET /spec → 返回 500 (GetTenantInfo 失败)
  └─ compute_ctl: retry 3 次均 500 → 退出
  └─ Kubernetes: pod Error → 重启
  └─ 同时: storage controller 调用 /notify-attach (可能是不同的 tenant_id)
  └─ 控制面找不到 Deployment → 500 (但这不影响 compute_ctl)
```

**所以"随机 tenant_id"在 `/notify-attach` 日志中出现，但这不是导致 compute pod Error 的直接原因。** Pod Error 的根本原因是 `/spec` 返回 500。

---

## 7. 对原有设计方案的确认与修正

### 7.1 方案一：ConfigMap 注入 cluster 配置 ✅ 确认

上游中 compute_ctl 从 spec 的 `cluster.cluster_id` 或 `cluster.settings.neon.tenant_id` 获取 tenant_id。在 ConfigMap 中加入 cluster 配置是对齐上游做法的正确修复。

### 7.2 方案二：GenerateComputeSpec 降级 ✅ 确认

上游 `/spec` 可以返回 `Empty` 状态（spec 为 None），compute_ctl 会等待 `/configure` 推送。我们不需要返回 Empty，但需要确保 `/spec` 不因外部依赖而 500。

### 7.3 新增关注点：/notify-attach 的调用者

需要确认 storage controller (在我们的部署中) 是如何获取 tenant_id 来调用 `/notify-attach` 的。如果 storage controller 使用了不同的 ID 生成逻辑，这是另一个需要修复的问题。

---

## 8. 附录：上游源码关键路径

| 功能 | 路径 |
|------|------|
| compute_ctl 入口 | `compute_tools/src/bin/compute_ctl.rs` |
| /spec 获取逻辑 | `compute_tools/src/spec.rs: get_config_from_control_plane()` |
| tenant_id 提取 | `compute_tools/src/compute.rs: ParsedSpec::try_from()` |
| ComputeSpec 结构 | `libs/compute_api/src/spec.rs` |
| /notify-attach 发起方 | `storage_controller/src/compute_hook.rs: ComputeHook::notify_attach()` |
| notify-attach 请求体 | `storage_controller/src/compute_hook.rs: NotifyAttachRequest` |
| 本地测试 spec 构建 | `control_plane/src/endpoint.rs: Endpoint::start()` |
| /configure 处理 | `compute_tools/src/http/routes/configure.rs` |
