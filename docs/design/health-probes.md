# 全组件健康探针 — 生产级设计方案

> 状态: ✅ 已实现（P0 完成，P1 部分完成）
> 日期: 2026-07-01
> 基于: neon 源码 `/home/postgres/works/opensource/neon`，operator 源码 `/home/postgres/works/opensource/neon-operator`

---

## 目录

1. [概述与设计原则](#1-概述与设计原则)
2. [上游 Neon 健康端点全貌](#2-上游-neon-健康端点全貌)
3. [当前 neon-operator 探针现状](#3-当前-neon-operator-探针现状)
4. [全组件探针详细设计](#4-全组件探针详细设计)
   - [4.1 Storage Controller](#41-storage-controller)
   - [4.2 Storage Broker](#42-storage-broker)
   - [4.3 Pageserver](#43-pageserver)
   - [4.4 Safekeeper](#44-safekeeper)
   - [4.5 Compute Node](#45-compute-node)
5. [ProbeConfig 扩展方案](#5-probeconfig-扩展方案)
6. [probeWithConfig 函数统一](#6-probewithconfig-函数统一)
7. [实施计划](#7-实施计划)
8. [验证方案](#8-验证方案)
9. [附录 A: 参考项目设计模式](#9-附录-a-参考项目设计模式)
10. [附录 B: 各组件端点一览](#10-附录-b-各组件端点一览)

---

## 1. 概述与设计原则

### 1.1 为什么健康探针是生产级的基础

Kubernetes 通过三种探针实现对 Pod 生命周期的自动化管理：

| 探针 | 职责 | 失败后果 |
|------|------|---------|
| **StartupProbe** | 保护慢启动容器，启动期间抑制 Liveness/Readiness | 超时后 kubelet 重启容器 |
| **LivenessProbe** | 检测进程是否存活（卡死/死锁） | 失败后 kubelet 重启容器 |
| **ReadinessProbe** | 检测服务是否可接受流量 | 失败后从 Service Endpoint 摘除 |

对于 Neon 这种存储计算分离的分布式数据库，探针需要与 Storage Controller（SC）的心跳机制协同工作：

```
K8s LivenessProbe ~30s  →  SC max_offline_interval = 30s
K8s StartupProbe  ~300s →  SC max_warming_up_interval = 300s
K8s ReadinessProbe ~5s  →  SC heartbeat_interval = 5s
```

**核心原则：K8s 必须在 SC 判定组件 Offline 之前完成检测和重启**，避免 SC 和 K8s 同时介入而产生竞态。

### 1.2 四级健康检查模型

```
Layer 1: K8s StartupProbe      ─── 覆盖启动窗口，确保容器不因慢启动被杀
            │
Layer 2: K8s LivenessProbe     ─── 检测进程僵死（~30s 内重启），轻量级
            │
Layer 3: K8s ReadinessProbe    ─── 流量就绪信号（~10s 内摘除/恢复），快速响应
            │
Layer 4: SC Heartbeat           ─── 深度业务健康检查（每 5s），由 SC 独立执行
            │                       /v1/utilization 或 gRPC health check
```

**职责分离**: K8s 探针只做进程级/网络级检查（轻量、无副作用），业务级健康由 SC Heartbeat 负责。

### 1.3 设计原则

| 原则 | 说明 |
|------|------|
| **Liveness = 进程级** | 轻量、不依赖外部系统、无副作用。不检查数据库连通性、不执行业务逻辑 |
| **Startup 保护慢启动** | Pageserver 从 S3 冷启动可达 300s，StartupProbe 窗口必须覆盖 |
| **Readiness = 快速响应** | 小 period + 低 failureThreshold，快速从 Service 摘除/恢复 |
| **时序对齐 SC** | `检测窗口 < SC 超时参数`，确保 K8s 先行介入 |
| **只用无需认证的端点** | 探针不能依赖 JWT 认证链路（认证本身也是被监控对象） |
| **timeout < period** | 防止并发探测堆积 |
| **failureThreshold ≥ 3（Liveness）** | 防止 GC pause / 网络瞬断触发误重启 |
| **可配置但端点固定** | 用户可调阈值，但 Path/Port/Scheme 不可改（对齐上游 API） |

### 1.4 检测窗口计算公式

```
检测窗口 = (failureThreshold − 1) × periodSeconds + timeoutSeconds

示例: Pageserver LivenessProbe
  = (3 − 1) × 10 + 5 = 25s → < SC max_offline_interval = 30s ✓
```

---

## 2. 上游 Neon 健康端点全貌

基于对上游 neon 源码（`/home/postgres/works/opensource/neon`）的深度分析：

### 2.1 Pageserver

| 端点 | 路径 | HTTP 方法 | 认证 | 返回 |
|------|------|:---:|:---:|------|
| **Status** | `/v1/status` | GET | 无需 | `200 {"id": <NodeId>}` |
| Utilization | `/v1/utilization` | GET | 需 JWT | Timeline 统计 |
| Metrics | `/metrics` | GET | 无需 | Prometheus 指标 |

**关键源码**: `neon/pageserver/src/http/routes.rs:549-557`

```rust
async fn status_handler(request: Request<Body>) -> Result<Response<Body>, ApiError> {
    check_permission(&request, None)?; // None = 白名单路由，返回 Ok
    let state = get_state(&request);
    let status = StatusResponse { id: state.id };
    json_response(StatusCode::OK, status)
}
```

- `/v1/status` 在 `ALLOWLIST_ROUTES` 中，不要求 JWT
- **无实质性内部检查**——仅确认 HTTP 服务器能响应请求
- 不存在 `/v1/live` 或 `/v1/ready` 端点
- Pageserver 启动分 4 个 Phase，Phase 1 即启动 HTTP 服务器

### 2.2 Safekeeper

| 端点 | 路径 | HTTP 方法 | 认证 | 返回 |
|------|------|:---:|:---:|------|
| **Status** | `/v1/status` | GET | 无需 | `200 {"id": <NodeId>}` |
| Utilization | `/v1/utilization` | GET | 需 JWT | Timeline 统计 |

**关键源码**: `neon/safekeeper/src/http/routes.rs:45-50`

与 Pageserver `/v1/status` 完全一致的设计——在 auth 白名单中，无内部状态检查。

### 2.3 Storage Controller（唯一具备完整三探针端点的组件）

| 端点 | 路径 | 检查逻辑 | 返回值 |
|------|------|------|------|
| **Startup** | `GET /status` | 无（HTTP 服务器就绪即可） | `200 {}` |
| **Liveness** | `GET /live` | `startup_complete AND is_leader` | `200` 或 `503` |
| **Readiness** | `GET /ready` | `startup_complete` | `200` 或 `503` |

**关键源码**: `neon/storage_controller/src/http.rs:1621-1672`

```rust
// /live — k8s liveness probe
async fn live_handler(State(state): State<Arc<ServiceState>>) -> Result<StatusCode, StatusCode> {
    if state.startup_complete.load(Ordering::Acquire) {
        if *state.leadership_status.read() == LeadershipStatus::Leader {
            return Ok(StatusCode::OK);
        }
    }
    Err(StatusCode::SERVICE_UNAVAILABLE)
}

// /ready — k8s readiness probe
async fn ready_handler(State(state): State<Arc<ServiceState>>) -> Result<StatusCode, StatusCode> {
    if state.startup_complete.load(Ordering::Acquire) {
        return Ok(StatusCode::OK);
    }
    Err(StatusCode::SERVICE_UNAVAILABLE)
}
```

**SC 是唯一正确实现 Liveness/Readiness 语义区分的组件**：
- **`/live`** = startup_complete **AND** is_leader：非 leader 实例的 liveness 也返回 503，确保 K8s 只保留 leader 存活
- **`/ready`** = startup_complete：仅检查初始 I/O 完成（与远程 pageserver 节点协调）
- 非 leader 实例只允许访问 `/ready`、`/status`、`/metrics`（`prologue_leadership_status_check_middleware`）

### 2.4 Storage Broker

| 端点 | 路径 | HTTP 方法 | 认证 | 返回 |
|------|------|:---:|:---:|------|
| **Status** | `/status` | GET | 无需 | `200` (空 body) |
| Metrics | `/metrics` | GET | 无需 | Prometheus 指标 |

**关键源码**: `neon/storage_broker/src/bin/storage_broker.rs:636-662`

```rust
// We serve only metrics and healthcheck through http1.
async fn http1_handler(req: hyper::Request<Incoming>) -> ... {
    match (req.method(), req.uri().path()) {
        (&Method::GET, "/metrics") => { /* Prometheus metrics */ }
        (&Method::GET, "/status") => hyper::Response::builder()
            .status(StatusCode::OK)
            .body(BoxBody::new(Empty::new()))
            .unwrap(),
        _ => { /* 404 */ }
    }
}
```

- `/status` 仅确认 HTTP 服务器在监听，无任何内部状态检查
- Broker 通过 `--listen-addr` 同时提供 HTTP/1（状态+指标）和 gRPC（BrokerService）
- 默认端口 `50051`（`DEFAULT_LISTEN_ADDR = "127.0.0.1:50051"`）
- **端口复用**：同一端口通过 HTTP 版本协商（HTTP/1 → http1_handler，HTTP/2 → gRPC）

### 2.5 Compute（compute_ctl）

| 端点 | 路径 | HTTP 方法 | 认证 | 返回 | 状态 |
|------|------|:---:|:---:|------|:---:|
| Status | `/status` | GET | 需 JWT | `ComputeStatusResponse` | ✅ |
| Liveness | `/hadron_liveness_probe` | GET | 需 JWT | `"ok"` / Error | ⚠️ **NOT ENABLED YET** |
| Writability | `/check_writability` | POST | 需 JWT | `true` / Error | ✅ |
| Metrics | `/metrics` | GET | 无需 | Prometheus 指标 | ✅ |

**`/hadron_liveness_probe`**（`neon/compute_tools/src/http/routes/hadron_liveness_probe.rs`）:
- 内部调用 `pg_isready -p <port>` 检查 PostgreSQL 是否接受连接
- 注释明确标注 **"NOTE: NOT ENABLED YET"**
- 需要 JWT 认证

**`/check_writability`**（`neon/compute_tools/src/http/routes/check_writability.rs`）:
- 检查 compute 状态为 `Running`
- 执行实际 SQL 写入验证数据库可写（`INSERT INTO public.health_check ... ON CONFLICT DO UPDATE`）
- 需要 JWT 认证

### 2.6 汇总

| 组件 | Startup 端点 | Liveness 端点 | Readiness 端点 | 认证要求 |
|------|:---:|:---:|:---:|:---:|
| Pageserver | `/v1/status` | `/v1/status` | `/v1/status` | 无需 |
| Safekeeper | `/v1/status` | `/v1/status` | `/v1/status` | 无需 |
| Storage Controller | `/status` | `/live` ⭐ | `/ready` ⭐ | 无需 |
| Storage Broker | `/status` | `/status` | `/status` | 无需 |
| Compute | N/A (需 JWT) | N/A (未启用) | N/A (需 JWT) | 全部需 JWT |
| Proxy | `/v1/status` | `/v1/status` | `/v1/status` | 无需 |

---

## 3. 当前 neon-operator 探针现状

### 3.1 状态矩阵

| 组件 | StartupProbe | LivenessProbe | ReadinessProbe | ProbeConfig 可覆盖 | 评估 |
|------|:---:|:---:|:---:|:---:|------|
| **Storage Controller** | ✅ `/status` | ✅ `/live` | ✅ `/ready` | ✅ `ClusterSpec.StorageControllerProbes` | ✅ **已实现** |
| **Storage Broker** | ✅ `/status` | ✅ `/status` | ✅ `/status` | ✅ `ClusterSpec.StorageBrokerProbes` | ✅ **已实现** |
| **Pageserver** | ✅ `/v1/status` | ✅ `/v1/status` | ✅ `/v1/status` | ✅ `PageserverSpec.*Probe` | ✅ **生产就绪** |
| **Safekeeper** | ✅ `/v1/status` | ✅ `/v1/status` | ✅ `/v1/status` | ✅ `SafekeeperSpec.*Probe` | ✅ **生产就绪** |
| **Compute Node** | ✅ TCP 55433 | ✅ TCP 55433 | ✅ TCP 55433 | ❌ 硬编码 | ✅ **已实现（阶段 A TCP）** |
| **Operator Manager** | ❌ | ✅ `/healthz` | ✅ `/readyz` | ❌ YAML 固定 | 基本可用 |

### 3.2 当前探针默认值

| 参数 | SC startup | SC liveness | SC readiness | PS startup | PS liveness | PS readiness | SK startup | SK liveness | SK readiness |
|------|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|
| **Path** | `/status` | `/live` | `/ready` | `/v1/status` | `/v1/status` | `/v1/status` | `/v1/status` | `/v1/status` | `/v1/status` |
| **Port** | 8080 | 8080 | 8080 | 9898 | 9898 | 9898 | 7676 | 7676 | 7676 |
| **initialDelay** | 5s | 10s | 5s | 10s | 30s | 10s | 5s | 10s | 5s |
| **period** | 10s | 10s | 5s | 10s | 10s | 5s | 5s | 10s | 5s |
| **timeout** | 5s | 5s | 3s | 5s | 5s | 3s | 5s | 5s | 3s |
| **failureThreshold** | 6 | 3 | 2 | 30 | 3 | 2 | 12 | 3 | 2 |
| **检测窗口** | ~60s | ~30s | ~10s | ~300s | ~30s | ~10s | ~60s | ~30s | ~10s |

### 3.3 与 SC 心跳参数对齐验证

```
Pageserver:
  Liveness 检测窗口: (3−1)×10+5 = 25s < SC max_offline_interval=30s     ✓
  Startup 检测窗口:  (30−1)×10+5 = 295s ≈ SC max_warming_up_interval=300s ✓
  Readiness period:  5s = SC heartbeat_interval=5s                        ✓

Safekeeper:
  Liveness 检测窗口: (3−1)×10+5 = 25s < SC max_offline_interval=30s     ✓
  Startup 检测窗口:  (12−1)×5+5 = 60s（无冷启动）                         ✓

Storage Controller:
  Liveness 检测窗口: (3−1)×10+5 = 25s                                    ✓
  Startup 检测窗口:  (6−1)×10+5 = 55s（SC 启动快）                        ✓
```

---

## 4. 全组件探针详细设计

### 4.1 Storage Controller

**现状**: 已实现三探针，使用上游 SC 原生的 `/status`、`/live`、`/ready` 端点。**硬编码，不支持 ProbeConfig 覆盖**。

**设计决策**: 保持现有探针参数不变（已经过验证），添加 ProbeConfig 支持。

**推荐配置**（无变化）:

```go
StartupProbe:   probeWithConfig("/status", Port, 5,  10, 5, 6,  cfg.StartupProbe)
LivenessProbe:  probeWithConfig("/live",   Port, 10, 10, 5, 3,  cfg.LivenessProbe)
ReadinessProbe: probeWithConfig("/ready",  Port, 5,  5,  3, 2,  cfg.ReadinessProbe)
```

**关键设计点**:
- **`/live` 的特殊语义**: 非 leader SC 实例返回 503。这意味着如果 leader 切换，旧 leader 会被 K8s 重启（符合预期——确保只有 leader 存活）
- **`/ready` vs `/live`**: ready 只检查 `startup_complete`，不检查 leader 状态。这确保候选者在启动后能从 Service 接收流量用于 `/ready`、`/status`、`/metrics`
- **StartupProbe 60s 窗口**: SC 启动快，不需要 S3 冷启动

**ProbeConfig 来源**: 在 `ClusterSpec` 中添加 `StorageControllerProbes` 字段（见第 5 节）。

### 4.2 Storage Broker

**现状**: **完全没有任何探针**。这是最高优先级待实现项。

**上游端点分析**:
- Broker 在 `--listen-addr` 端口上同时提供 HTTP/1 和 gRPC（HTTP/2）
- `GET /status` 返回空 body `200 OK`，在 HTTP/1 handler 中
- 端口 `50051`（`DEFAULT_LISTEN_ADDR`）

**设计决策**: 使用 HTTP GET `/status` 作为探针端点。

| 论点 | 选择 HTTP GET 而非 TCP Socket 的理由 |
|------|------|
| 深度 | HTTP GET 验证了 HTTP handler 正常工作（不只是端口绑定） |
| 轻量 | Broker `/status` 不执行任何业务逻辑，开销极小 |
| 一致性 | 与 SC、Pageserver、Safekeeper 的 HTTP 探针模式一致 |
| 协议检测 | 端口同时服务 HTTP/1 和 HTTP/2，TCP socket 可能误判 gRPC 为存活 |

**推荐配置**:

```go
StartupProbe:   probeWithConfig("/status", Port, 5,  5,  5, 6,  cfg.StartupProbe)   // ~30s 窗口
LivenessProbe:  probeWithConfig("/status", Port, 10, 10, 5, 3,  cfg.LivenessProbe)  // ~25s 窗口
ReadinessProbe: probeWithConfig("/status", Port, 5,  5,  3, 2,  cfg.ReadinessProbe)  // ~8s 窗口
```

**参数理由**:

| 参数 | 值 | 理由 |
|------|:--:|------|
| StartupProbe.failureThreshold | 6 (~30s) | Broker 是无状态 pub-sub 服务，启动极快（< 10s），30s 足够 |
| LivenessProbe.periodSeconds | 10s | 与其他组件一致的探测频率 |
| ReadinessProbe.periodSeconds | 5s | 快速摘除/恢复，Pub-Sub 中断影响全局 |

### 4.3 Pageserver

**现状**: ✅ 已完整实现，无需修改。

当前实现:
```go
LivenessProbe:  probeWithConfig("/v1/status", 9898, 30, 10, 5, 3,  ps.Spec.LivenessProbe)
ReadinessProbe: probeWithConfig("/v1/status", 9898, 10, 5,  3, 2,  ps.Spec.ReadinessProbe)
StartupProbe:   probeWithConfig("/v1/status", 9898, 10, 10, 5, 30, ps.Spec.StartupProbe)
```

**设计理由**（已验证，详见 [pageserver-production.md](./pageserver-production.md) 4.1 节）:
- StartupProbe 300s 窗口 → 覆盖 S3 冷启动（`max_warming_up_interval = 300s`）
- LivenessProbe 30s 窗口 → 先于 SC 判定 Offline（`max_offline_interval = 30s`）
- LivenessProbe initialDelay=30s → Pageserver 启动阶段多（4 个 Phase）
- **`/v1/status` 是唯一无需 JWT 认证的端点**

**无变化**。Pageserver 探针已通过深度源码分析和完整实现验证，达到生产级标准。

### 4.4 Safekeeper

**现状**: ✅ 已完整实现，无需修改。

当前实现:
```go
LivenessProbe:  probeWithConfig("/v1/status", 7676, 10, 10, 5, 3,  sk.Spec.LivenessProbe)
ReadinessProbe: probeWithConfig("/v1/status", 7676, 5,  5,  3, 2,  sk.Spec.ReadinessProbe)
StartupProbe:   probeWithConfig("/v1/status", 7676, 5,  5,  5, 12, sk.Spec.StartupProbe)
```

**设计理由**（已验证，详见 [safekeeper-production.md](./safekeeper-production.md) 3.2 节）:
- StartupProbe 60s 窗口 → Safekeeper 无冷启动，通常 10-30s 完成
- LivenessProbe initialDelay=10s（小于 Pageserver 的 30s）→ 启动更快
- **Safekeeper 无 WarmingUp 状态**：心跳失败后 SC 立即标记 Offline

**与 Pageserver 的核心差异**:
- Pageserver LivenessProbe initialDelay=30s（启动慢）vs Safekeeper=10s（启动快）
- Pageserver StartupProbe failureThreshold=30（冷启动）vs Safekeeper=12（无冷启动）

**无变化**。Safekeeper 探针已达到生产级标准。

### 4.5 Compute Node

**现状**: **完全没有任何探针**（`specs/compute/deployment.go`）。仅有 `preStop` hook 用于优雅关闭。

**挑战分析**:

上游 Neon compute_ctl 暴露了三个可用于健康检查的端点，但全部存在可用性问题：

| 端点 | 问题 | 结论 |
|------|------|------|
| `/hadron_liveness_probe` | 标记 "NOT ENABLED YET"，且需 JWT 认证 | ❌ 不可用 |
| `/check_writability` | 需 JWT 认证，执行实际 SQL 写入 | ❌ 不可用（当前无 JWT 链路） |
| `/status` | 需 JWT 认证 | ❌ 不可用（当前无 JWT 链路） |

**结论**: 上游 compute 的所有 HTTP 健康端点均需 JWT 认证，在当前 JWT 体系未建立前无法使用。

**设计方案**: 分两阶段推进。

#### 阶段 A：当前阶段（无 JWT）— TCP Socket 探针

使用 `tcpSocket` 探针检测 PostgreSQL 端口是否在监听。

```go
// 阶段 A 实现
StartupProbe: &corev1.Probe{
    ProbeHandler: corev1.ProbeHandler{
        TCPSocket: &corev1.TCPSocketAction{
            Port: intstr.FromInt(ComputePort),  // 55433
        },
    },
    InitialDelaySeconds: 5,
    PeriodSeconds:       10,
    TimeoutSeconds:      5,
    FailureThreshold:    30,  // ~300s 启动窗口
},
LivenessProbe: &corev1.Probe{
    ProbeHandler: corev1.ProbeHandler{
        TCPSocket: &corev1.TCPSocketAction{
            Port: intstr.FromInt(ComputePort),
        },
    },
    InitialDelaySeconds: 10,
    PeriodSeconds:       10,
    TimeoutSeconds:      5,
    FailureThreshold:    3,
},
ReadinessProbe: &corev1.Probe{
    ProbeHandler: corev1.ProbeHandler{
        TCPSocket: &corev1.TCPSocketAction{
            Port: intstr.FromInt(ComputePort),
        },
    },
    InitialDelaySeconds: 5,
    PeriodSeconds:       5,
    TimeoutSeconds:      3,
    FailureThreshold:    2,
},
```

**为什么 TCP Socket 是合理的选择**:

1. **PostgreSQL 仅在完全准备好后才 bind 端口**：PostgreSQL 的 postmaster 在完成所有恢复和初始化后才开始监听。TCP Socket 探测到端口开放 = PostgreSQL 已就绪。
2. **零开销**: TCP Socket 探针比 HTTP 探针更轻量，不产生 HTTP 解析开销。
3. **无认证依赖**: 不依赖 JWT 链路。
4. **K8s 原生支持**: 不需要容器内安装额外工具。

**检测窗口**:
- Startup: `(30−1)×10+5 = 295s` — PostgreSQL 启动 + 初始恢复可能需要 2-3 分钟
- Liveness: `(3−1)×10+5 = 25s`
- Readiness: `(2−1)×5+3 = 8s`

#### 阶段 B：JWT 就绪后（目标状态）— HTTP 探针升级

当 JWT 认证链路建立后，升级为 HTTP 探针：

```go
// 阶段 B 目标配置
StartupProbe:   httpProbeWithJWT("/status",                 ComputeHTTPPort, ...)
LivenessProbe:  httpProbeWithJWT("/hadron_liveness_probe",   ComputeHTTPPort, ...)
ReadinessProbe: httpProbeWithJWT("/check_writability",       ComputeHTTPPort, ...)
```

**渐进升级路径**:

```
阶段 A (当前)                       阶段 B (JWT 就绪后)
─────────────────────        ──────────────────────────
Startup:  TCP 55433     →    Startup:  HTTP /status
Liveness: TCP 55433     →    Liveness: HTTP /hadron_liveness_probe (pg_isready)
Readiness:TCP 55433     →    Readiness:HTTP /check_writability (SQL write)
```

**pg_isready vs TCP Socket 对比**:

| 维度 | TCP Socket | pg_isready (exec) | HTTP `/hadron_liveness_probe` |
|------|:---:|:---:|:---:|
| 检查深度 | 端口监听 | PostgreSQL 接受连接 | PostgreSQL 接受连接 |
| 开销 | 极低 | 中等（fork 进程） | 低（HTTP 请求） |
| 需要 JWT | 否 | 否 | 是 |
| K8s 原生度 | ✅ 内置 | ✅ exec 探针 | ✅ HTTP 探针 |
| PostgreSQL 感知 | 否 | ✅ | ✅ |

**exec pg_isready 备选方案**: 如果未来需求需要更深的 PG 级检查且 JWT 未就绪，可临时使用：

```yaml
readinessProbe:
  exec:
    command: ["pg_isready", "-h", "localhost", "-p", "55433", "-U", "cloud_admin", "-d", "postgres"]
  initialDelaySeconds: 5
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 2
```

但 exec 探针有众所周知的缺点（每次 fork 进程、增加 kubelet 负载），CNPG 等成熟项目也尽量避免高频 exec 探针。因此**当前阶段推荐 TCP Socket**，阶段 B 迁移到 HTTP。

---

## 5. ProbeConfig 扩展方案

### 5.1 现状

当前 `ProbeConfig` 已用于 `PageserverSpec` 和 `SafekeeperSpec`：

```go
type ProbeConfig struct {
    InitialDelaySeconds *int32 `json:"initialDelaySeconds,omitempty"`
    PeriodSeconds       *int32 `json:"periodSeconds,omitempty"`
    TimeoutSeconds      *int32 `json:"timeoutSeconds,omitempty"`
    FailureThreshold    *int32 `json:"failureThreshold,omitempty"`
}
```

### 5.2 扩展：ClusterSpec 级别的 StorageController 和 StorageBroker 探针

为 Storage Controller 和 Storage Broker 的 Deployment 添加 ProbeConfig 覆盖。

#### 方案：在 ClusterSpec 中添加字段

```go
// api/v1alpha1/cluster_types.go
type ClusterSpec struct {
    // ... existing fields ...

    // StorageControllerProbes overrides the default health probe
    // configuration for the Storage Controller Deployment.
    // +optional
    StorageControllerProbes *ClusterComponentProbes `json:"storageControllerProbes,omitempty"`

    // StorageBrokerProbes overrides the default health probe
    // configuration for the Storage Broker Deployment.
    // +optional
    StorageBrokerProbes *ClusterComponentProbes `json:"storageBrokerProbes,omitempty"`
}

// ClusterComponentProbes groups probe configurations for a cluster-scoped component.
type ClusterComponentProbes struct {
    // +optional
    LivenessProbe  *ProbeConfig `json:"livenessProbe,omitempty"`
    // +optional
    ReadinessProbe *ProbeConfig `json:"readinessProbe,omitempty"`
    // +optional
    StartupProbe   *ProbeConfig `json:"startupProbe,omitempty"`
}
```

#### 使用方式

```go
// specs/storagecontroller/deployment.go
func Deployment(cluster *v1alpha1.Cluster) *appsv1.Deployment {
    var pc *v1alpha1.ClusterComponentProbes
    if cluster.Spec.StorageControllerProbes != nil {
        pc = cluster.Spec.StorageControllerProbes
    }
    // ...
    StartupProbe:   probeWithConfig("/status", Port, 5,  10, 5, 6,  pc.StartupProbe),
    LivenessProbe:  probeWithConfig("/live",   Port, 10, 10, 5, 3,  pc.LivenessProbe),
    ReadinessProbe: probeWithConfig("/ready",  Port, 5,  5,  3, 2,  pc.ReadinessProbe),
}
```

**为什么不复用 `ProbeConfig`**: Storage Controller 和 Storage Broker 是 Deployment（通过 `ClusterSpec` 配置），不是独立 CRD。需要单独的配置入口。

### 5.3 不扩展的部分（明确排除）

| 项 | 原因 |
|------|------|
| **探针 Path/Port 可配置** | 端点由上游 Neon 定义，错误配置会导致探针失效 |
| **Compute Node ProbeConfig** | 阶段 A 使用 TCP Socket，无 HTTP 端点可配；阶段 B 再开放 |
| **Operator Manager ProbeConfig** | controller-runtime 内置的 `/healthz`/`/readyz` 无需配置 |

---

## 6. probeWithConfig 函数统一

### 6.1 现状

`probeWithConfig` 函数在 `specs/pageserver/statefulset.go` 和 `specs/safekeeper/statefulset.go` 中各有一份**完全相同的拷贝**。

### 6.2 统一方案

提取到共享包 `utils/probes.go`：

```go
// utils/probes.go
package utils

import (
    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/util/intstr"
    "oltp.molnett.org/neon-operator/api/v1alpha1"
)

// ProbeWithConfig builds a *corev1.Probe using the given defaults, then
// applies any overrides from cfg. The path, port, and scheme are fixed
// because they correspond to upstream Neon's API design.
func ProbeWithConfig(
    path string, port int, scheme corev1.URIScheme,
    initialDelay, period, timeout, failure int32,
    cfg *v1alpha1.ProbeConfig,
) *corev1.Probe {
    probe := &corev1.Probe{
        ProbeHandler: corev1.ProbeHandler{
            HTTPGet: &corev1.HTTPGetAction{
                Path:   path,
                Port:   intstr.FromInt(port),
                Scheme: scheme,
            },
        },
        InitialDelaySeconds: initialDelay,
        PeriodSeconds:       period,
        TimeoutSeconds:      timeout,
        FailureThreshold:    failure,
    }
    if cfg == nil {
        return probe
    }
    if cfg.InitialDelaySeconds != nil {
        probe.InitialDelaySeconds = *cfg.InitialDelaySeconds
    }
    if cfg.PeriodSeconds != nil {
        probe.PeriodSeconds = *cfg.PeriodSeconds
    }
    if cfg.TimeoutSeconds != nil {
        probe.TimeoutSeconds = *cfg.TimeoutSeconds
    }
    if cfg.FailureThreshold != nil {
        probe.FailureThreshold = *cfg.FailureThreshold
    }
    return probe
}

// TCPProbe builds a tcpSocket probe (for components without HTTP endpoints).
func TCPProbe(
    port int,
    initialDelay, period, timeout, failure int32,
    cfg *v1alpha1.ProbeConfig,
) *corev1.Probe {
    probe := &corev1.Probe{
        ProbeHandler: corev1.ProbeHandler{
            TCPSocket: &corev1.TCPSocketAction{
                Port: intstr.FromInt(port),
            },
        },
        InitialDelaySeconds: initialDelay,
        PeriodSeconds:       period,
        TimeoutSeconds:      timeout,
        FailureThreshold:    failure,
    }
    if cfg == nil {
        return probe
    }
    // same override logic as ProbeWithConfig
    // ...
    return probe
}
```

**优势**:
- 消除代码重复
- 一处修改全局生效
- 方便单元测试
- `TCPProbe` 为 Compute 阶段 A 提供支持

---

## 7. 实施计划

### 实施状态

| 优先级 | 任务 | 状态 |
|:---:|------|:---:|
| **P0** | Storage Broker 添加三探针 | ✅ 已完成 |
| **P0** | Compute Node 添加三探针（TCP Socket） | ✅ 已完成 |
| **P1** | probeWithConfig 函数统一到 utils 包 | ✅ 已完成 |
| **P1** | Storage Controller 探针改为可配置（ProbeConfig） | ✅ 已完成 |
| **P2** | Compute 阶段 B：HTTP 探针升级 | 🔜 待 JWT 体系建立 |

### 已实现文件清单

| 文件 | 改动 | 说明 |
|------|:---:|------|
| `utils/probes.go` | 新建 | `ProbeWithConfig()` + `TCPProbe()` + `applyProbeConfig()` |
| `utils/probes_test.go` | 新建 | 单元测试（nil cfg、部分覆盖、TCP 探针） |
| `specs/storagebroker/deployment.go` | 修改 | `/status:50051` 三探针 + `StorageBrokerProbes` 可配置 |
| `specs/storagecontroller/deployment.go` | 修改 | `/status`/`/live`/`/ready:8080` + `StorageControllerProbes` 可配置 |
| `specs/compute/deployment.go` | 修改 | TCP 55433 三探针（阶段 A） |
| `specs/pageserver/statefulset.go` | 修改 | 引用 `utils.ProbeWithConfig` |
| `specs/safekeeper/statefulset.go` | 修改 | 引用 `utils.ProbeWithConfig` |
| `api/v1alpha1/cluster_types.go` | 修改 | 添加 `ClusterComponentProbes`、`StorageControllerProbes`、`StorageBrokerProbes` |
| `config/crd/bases/neon.oltp.molnett.org_clusters.yaml` | 自动生成 | `make generate` 输出 |

---

## 8. 验证方案

### 8.1 单元测试

```go
// utils/probes_test.go
func TestProbeWithConfig_NilConfig(t *testing.T) {
    probe := ProbeWithConfig("/v1/status", 9898, corev1.URISchemeHTTP, 30, 10, 5, 3, nil)
    assert.Equal(t, int32(30), probe.InitialDelaySeconds)
    assert.Equal(t, int32(10), probe.PeriodSeconds)
    assert.Equal(t, int32(5), probe.TimeoutSeconds)
    assert.Equal(t, int32(3), probe.FailureThreshold)
}

func TestProbeWithConfig_PartialOverride(t *testing.T) {
    cfg := &v1alpha1.ProbeConfig{
        InitialDelaySeconds: ptr.To(int32(60)),
    }
    probe := ProbeWithConfig("/v1/status", 9898, corev1.URISchemeHTTP, 30, 10, 5, 3, cfg)
    assert.Equal(t, int32(60), probe.InitialDelaySeconds) // overridden
    assert.Equal(t, int32(10), probe.PeriodSeconds)       // default
}

func TestTCPProbe_Basic(t *testing.T) {
    probe := TCPProbe(55433, 5, 10, 5, 30, nil)
    assert.NotNil(t, probe.TCPSocket)
    assert.Equal(t, intstr.FromInt(55433), probe.TCPSocket.Port)
}
```

### 8.2 Golden 测试

所有 `specs/*/golden_test.go` 的 golden 文件需更新以反映新的探针配置。运行：

```bash
cd specs/storagebroker && go test -update ./...
cd specs/compute && go test -update ./...
cd specs/storagecontroller && go test -update ./...  # 仅当添加 ProbeConfig
```

### 8.3 集成验证

1. **部署验证**: 部署 Cluster CR，确认所有 Pod 的探针正确注入：

```bash
# 检查每个组件的探针配置
kubectl get deployment <cluster>-storage-controller -o json | jq '.spec.template.spec.containers[0] | {startup, liveness, readiness}'
kubectl get deployment <cluster>-storage-broker -o json | jq '.spec.template.spec.containers[0] | {startup, liveness, readiness}'
kubectl get statefulset <cluster>-pageserver-1 -o json | jq '.spec.template.spec.containers[0] | {startup, liveness, readiness}'
kubectl get statefulset <cluster>-safekeeper-1 -o json | jq '.spec.template.spec.containers[0] | {startup, liveness, readiness}'
kubectl get deployment <branch>-compute-node -o json | jq '.spec.template.spec.containers[0] | {startup, liveness, readiness}'
```

2. **启动验证**: 观察 Pod 事件，确认 StartupProbe 在预期窗口内通过：

```bash
kubectl describe pod <pod-name> | grep -A5 "startupProbe\|livenessProbe\|readinessProbe"
kubectl get events --field-selector involvedObject.name=<pod-name> --watch
```

3. **故障注入验证**:

```bash
# 模拟进程挂起
kubectl exec <pod> -- kill -STOP 1
# 等待探针检测窗口后，确认 Pod 被重启
kubectl get pod <pod> -w
```

4. **SC 对齐验证**: 在 SC 日志中确认 SC 判定 Offline 的时间点晚于 K8s 重启 Pod 的时间点：

```
# 预期时序
T+0s:  进程挂起
T+~25s: K8s LivenessProbe 失败 → 重启 Pod
T+~30s: SC 心跳超时 → 标记 Offline（已晚于 K8s）
```

---

## 9. 附录 A: 参考项目设计模式

### 9.1 CNPG (CloudNativePG) v1.29

PostgreSQL Operator 的探针设计：

| 特征 | CNPG 做法 | Neon 对齐 |
|------|----------|-----------|
| **Liveness 机制** | 主实例隔离检查（Primary Isolation Check） | N/A（由 SC 心跳覆盖） |
| **Readiness 机制** | `pg_isready` 基于的连接检查 | SC Heartbeat `/v1/utilization` |
| **StartupProbe** | 支持，可配置 | ✅ 已实现 |
| **探针可配置** | 完整 CRD 暴露 `spec.probes.*` | ✅ ProbeConfig 模式 |
| **探针超时** | `livenessProbeTimeout=30s`，由公式推导 failureThreshold | ✅ 显式配置 |

### 9.2 Strimzi Kafka Operator

| 特征 | Strimzi 做法 | 启示 |
|------|-------------|------|
| **Readiness** | 检查 Kafka 是否在 Controller 中注册 | 类似 SC 的心跳模型 |
| **TCP Socket** | Kafka 使用 TCP Socket 探针（无 HTTP 端点时） | 验证了 Compute 阶段 A 的 TCP Socket 方案 |

### 9.3 Kubernetes 官方最佳实践

来自 [K8s 文档](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/):

| 建议 | Neon 对齐情况 |
|------|:---:|
| Liveness 不应检查外部依赖 | ✅ 所有探针只检查本地端点 |
| 轻量级探针（避免重计算） | ✅ `/v1/status` 无业务逻辑 |
| StartupProbe 用于慢启动容器 | ✅ Pageserver 300s 窗口 |
| Readiness 失败不重启，只摘除流量 | ✅ 语义正确 |
| `initialDelaySeconds` 应合理设置 | ✅ 基于组件特性定制 |

---

## 10. 附录 B: 各组件端点一览

### 已认证上游端点

```
Pageserver (端口 9898):
  GET  /v1/status          ← 探针使用（无需认证）
  GET  /v1/utilization     ← SC 心跳（需 JWT）
  GET  /metrics            ← Prometheus（无需认证）
  (另有 6+ 管理端点，均需 JWT)

Safekeeper (端口 7676):
  GET  /v1/status          ← 探针使用（无需认证）
  GET  /v1/utilization     ← SC 心跳（需 JWT）
  (另有 2+ 管理端点，均需 JWT)

Storage Controller (端口 8080):
  GET  /status             ← Startup 探针
  GET  /live               ← Liveness 探针（需是 leader）
  GET  /ready              ← Readiness 探针
  GET  /metrics            ← Prometheus
  POST /upcall/v1/*        ← Pageserver 回调
  PUT  /control/v1/*        ← Safekeeper 管理
  (另有 10+ 管理端点)

Storage Broker (端口 50051):
  GET  /status             ← 探针使用
  GET  /metrics            ← Prometheus
  gRPC BrokerService        ← 主业务（Pub-Sub）

Compute / compute_ctl (端口 未知):
  GET  /status             ← 详细状态（需 JWT）
  GET  /hadron_liveness_probe ← pg_isready（需 JWT，未启用）
  POST /check_writability   ← SQL 写入检查（需 JWT）
  GET  /metrics            ← Prometheus（无需认证）
```

### 探针端点 = Auth 白名单验证

上游 Neon 源码中，Pageserver 和 Safekeeper 的 `/v1/status` 被显式加入 `ALLOWLIST_ROUTES`，这是探针可以使用该端点的根本原因：

```rust
// neon/pageserver/src/http/routes.rs
const ALLOWLIST_ROUTES: &[&str] = &["/v1/status"];
// neon/safekeeper/src/http/routes.rs
const ALLOWLIST_ROUTES: &[&str] = &["/v1/status"];
```

Storage Controller 和 Storage Broker 不要求任何认证（当前 `--dev` 模式下），因此所有端点均可直接使用。
