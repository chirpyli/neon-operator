# 生产级 Neon 改造分析报告

> 分析当前 neon-operator 距离生产级 Neon 的差距，结合 Neon 架构和 Neon API 给出系统化改造方案与实施路线图。

| 字段 | 内容 |
|------|------|
| 版本 | v1.7 |
| 日期 | 2026-07-03 |
| 状态 | 持续更新 |
| 变更 | **v1.7: 节点故障自动恢复全面实现** — Pageserver/Safekeeper Controller 新增 `handleNodeFailure` 完整故障检测（Pending/Running-NotReady/Terminating/Failed/Unknown）、Pod 事件监听（`Watches(&corev1.Pod{}, ...)`）、`DeletionTimestamp` 优先检查修复、Safekeeper `NodeFailure` 配置支持（`DefaultSafekeeperConfig.nodeFailure`）；**Endpoint 创建修复** — `createEndpointForBranch` 函数修复 `ownerReferences.uid` 为空导致的 INTERNAL_ERROR；**v1.6: Pageserver 安全下线（Graceful Drain + Tombstone）、Cluster 级 SC 节点兜底 tomstone（cleanupPageserverNodes）、Pageserver finalizer SC 不可达容错加固、StartNodeDelete 错误不再静默忽略、SCClient 新增 DeleteTombstone 方法、initScript DNS 修复、Headless Service DNS 死锁修复（publishNotReadyAddresses=true）、Phase 2 安全加固阶段性完成（SC `--dev` 移除、全组件 JWT 认证链路、Component Token 持久化、Compute→SK walproposer 认证）** |

---

## 一、总体评估

| 维度 | 评估 | 说明 |
|------|------|------|
| 架构正确性 | ✅ 合理 | 存储计算分离、CRD + Controller 模式、SC/Broker 体系正确 |
| 功能完备性 | ⚠️ 可用 | 核心 CRUD + PATCH/Update + Endpoint 生命周期已实现，缺少 Snapshots / PITR |
| 基础设施 | ✅ 基本完成 | 全组件健康探针、PS/SK 反亲和+PDB+资源配置、SC/Broker/Compute 资源配置已实现；**节点故障自动恢复已实现**（PS/SK PVC 解绑重建）；多 AZ 待实现 |
| 安全性 | ✅ 阶段性完成 | SC `--dev` 已移除、全组件间 JWT 认证链路已启用（Operator→SC, SC 内部验证, PS/SK JWT 挂载, PS→SC upcall, PS→SK WAL, **Compute→SK walproposer**）、Component Token 持久化已完成、Token 确定性生成（SHA256 派生 exp 避免滚动重启）；API Key 用户认证、Safekeeper 间 TLS 仍待实现 |
| 生命周期 | ✅ 增强 | Compute 优雅终止已完成，SCRAM 密码管理、**节点故障自动恢复**（Pending/Running-NotReady/Terminating/Failed/Unknown 全状态检测）、Pageserver 安全下线已实现；扩缩容待实现 |
| 可观测性 | ❌ 缺失 | 无 Prometheus 指标、无 Grafana 仪表板、无告警规则 |
| API 覆盖率 | ~36% | 对标 Neon Cloud API ~95 端点，已实现 34 个（核心 CRUD 100%） |

---

## 二、Neon 架构概览

### 2.1 核心组件职责

```
┌───────────────────────────────────────────────────────────┐
│                      Control Plane                         │
│  ┌──────────┐  ┌──────────┐  ┌────────────┐               │
│  │ Console  │  │  Proxy   │  │  Operator  │               │
│  │(REST API)│  │(PG 路由) │  │(K8s协调器) │               │
│  └────┬─────┘  └────┬─────┘  └─────┬──────┘               │
│       │              │              │                       │
│  ┌────▼──────────────▼──────────────▼──────┐               │
│  │          Storage Controller             │               │
│  │  (心跳/调度/Generation/Quorum/Tenant)    │               │
│  └────┬──────────────────────────┬─────────┘               │
│       │                          │                          │
│  ┌────▼──────────┐    ┌──────────▼──────────┐              │
│  │   Safekeeper  │    │    Pageserver       │              │
│  │  (WAL/Paxos)  │    │ (S3存储 + 本地缓存) │              │
│  └───────┬───────┘    └──────────┬──────────┘              │
│          │                       │                          │
│   ┌──────▼──────┐         ┌──────▼──────┐                  │
│   │  Compute    │         │     S3      │                  │
│   │ (PG 实例)   │         │ (持久层)     │                  │
│   └─────────────┘         └─────────────┘                  │
└───────────────────────────────────────────────────────────┘
```

| 组件 | 职责 | 持久化 |
|------|------|--------|
| **Pageserver** | 存储引擎：接收 WAL，合并成 L0/L1 层，写入 S3 | 本地 NVMe（缓存）+ S3（持久层） |
| **Safekeeper** | WAL 共识：Paxos 多副本，仲裁写确认 | 本地盘 |
| **Storage Controller** | 集群大脑：心跳检测、租户调度、Generation 防脑裂、Broker 管理 | SC 数据库 |
| **Storage Broker** | 消息总线：Pub-Sub 广播集群事件 | SC 数据库 |
| **Compute** | 计算节点：PostgreSQL 实例，共享存储读取 Pageserver | 无状态 |
| **Control Plane** | 控制面：用户 API、认证、计费、编排 | 业务数据库 |

### 2.2 关键设计原则

- **S3 是唯一持久化层**：即使所有 Pageserver 下线，数据不会丢失
- **本地 NVMe 只是缓存**：Pageserver 丢失后可从 S3 重建
- **Safekeeper 需要 (N/2)+1 共识**：最少 3 个，生产建议 3~5 个
- **Generation Number 防脑裂**：SC 通过 Generation 确保同一时刻只有一个 Pageserver 写入
- **SSA 推进**：SC 通过 SSA Update 推进 Timeline 的 disk_consistent_lsn
- **Broker Pub-Sub**：所有组件通过 Broker 获取集群事件，不直连

---

## 三、差距矩阵

### 3.1 基础设施层

| 差距 | 影响 | Pageserver | Safekeeper | SC | Broker | Compute |
|------|------|:---:|:---:|:---:|:---:|:---:|
| 健康探针（Liveness/Readiness/Startup） | Pod 异常无法自动重启 | ✅ | ✅ | ✅ | ✅ | ✅ |
| 优雅终止（PreStop + GracePeriod） | PostgreSQL 未经 checkpoint 被 kill | ❌ | ❌ | ❌ | ❌ | ✅ |
| PodAntiAffinity | 同节点故障导致全部不可用 | ✅ | ✅ | ❌ | ❌ | ❌ |
| PDB（PodDisruptionBudget） | 滚动更新/驱逐时可能同时中断 | ✅ | ✅ | ❌ | ❌ | ❌ |
| Resource Requests/Limits | 无 QoS，可能被 OOMKilled | ✅ | ✅ | ✅ | ✅ | ✅ |
| **节点故障自动恢复** | 节点故障后 StatefulSet Pod 卡 Pending，PVC 粘滞导致无法重建 | ✅ **已实现** | ✅ **已实现** | N/A | N/A | N/A |
| 多 AZ 分布 | 无法容忍单个 AZ 故障 | ❌ | ❌ | ❌ | ❌ | ❌ |

### 3.2 安全层

| 差距 | 风险 | 状态 |
|------|------|:---:|
| SC `--dev` 模式 | ~~跳过所有认证和 Quorum 检查~~ 已移除，通过 `PUBLIC_KEY` 环境变量启用 JWT 验证 | ✅ |
| JWT 认证链路（Operator→SC） | ~~无认证~~ SCClient 已使用 `admin` scope JWT 签名请求 | ✅ |
| JWT 认证链路（SC 内部验证） | ~~SC API 完全开放~~ 通过 `PUBLIC_KEY` + `*_JWT_TOKEN` 环境变量注入，SC 运行时验证所有请求 | ✅ |
| Pageserver JWT | ~~无认证~~ JWT Volume 挂载 `public.pem` + ConfigMap 注入 `auth_validation_public_key_path` + `control_plane_api_token`（generations_api scope） | ✅ |
| PS→SC upcall 认证 | ~~无认证~~ `pageserver_control_plane_token`（generations_api scope）持久化到 Secret | ✅ |
| PS→SK WAL 认证 | ~~无认证~~ `pageserver_safekeeper_token`（safekeeperdata scope）持久化到 Secret | ✅ |
| Component Token 持久化 | ~~每次 reconcile 重新签发~~ `ensureComponentTokens()` + `ensurePSAuthTokens()` 首次生成后写入 Secret，后续直接复用 | ✅ |
| Safekeeper JWT | ~~无认证~~ JWT Volume 挂载 `public.pem` | ✅ |
| API 认证（API Key/JWT） | HTTP API 无用户认证（业务层面，非组件间通信） | ❌ |
| Safekeeper 间 TLS | WAL 传输明文 | ❌ |

### 3.3 生命周期层

| 差距 | 说明 | 状态 |
|------|------|------|
| Safekeeper 自动创建 | Cluster Controller 根据 `NumSafekeepers` 自动创建/删除 Safekeeper CR | ✅ |
| Pageserver 安全下线 | Graceful Drain + Tombstone 已实现；Cluster 级兜底 tombstone + SC 不可达容错已加固 | ✅ |
| Safekeeper 安全下线 | 缺少 `scheduling_policy=Offline` 注销机制 | ❌ |
| **节点故障自动恢复** | **Pageserver/Safekeeper Controller 已实现完整故障检测与恢复逻辑**：`handleNodeFailure` 检测 Pending/Running-NotReady/Terminating/Failed/Unknown 状态；`DeletionTimestamp` 优先检查修复；Pod 事件监听（`Watches(&corev1.Pod{}, ...)`）触发调和；Safekeeper `DefaultSafekeeperConfig.nodeFailure` 配置支持 | ✅ **已完成 (2026-07-03)** |
| 纵向扩缩容 | 无 CU 变更能力 | ❌ |
| 横向扩缩容 | Pageserver/Safekeeper 增减需手动操作 | ❌ |
| Compute 优雅终止 | `exec` PID 1 + preStop pg_ctl + 60s GracePeriod | ✅ **已完成 (2026-07-01)** |
| Compute 独立镜像管理 | `Cluster.Spec.ComputeImage` 字段：compute-node 镜像与 NeonImage 分离 | ✅ **已完成 (2026-07-01)** |
| Compute ImagePullPolicy | 设为 `IfNotPresent`，避免 `latest` tag 导致的 `Always` 拉取 | ✅ **已完成 (2026-07-01)** |
| Compute 资源配置 | Endpoint.Spec.Resources > Project.DefaultEndpointSettings > 默认 三级合并 | ✅ **已完成 (2026-07-01)** |
| SCRAM-SHA-256 密码管理 | RoleReconciler 自动生成密码 + SCRAM verifier，通过 ConfigMap 注入 Compute | ✅ **已完成 (2026-07-01)** |
| ConfigMap checksum 自动重启 | spec ConfigMap 变更自动触发 Deployment 滚动更新 | ✅ **已完成 (2026-07-01)** |
| Compute 挂起/恢复 | 无 Scale-to-Zero（API 路由已就绪：start/suspend/restart） | ⚠️ **API 已实现，Controller 逻辑待实现** |

### 3.4 API 完备性

| 类别 | 已实现 | 缺失 | 覆盖率 |
|------|--------|------|--------|
| Project | 5 (POST/GET list/GET single/PATCH/DELETE) | 0 | 100% |
| Branch | 6 (POST/GET list/GET single/DELETE/PATCH/set_as_default) | 0 | 100% |
| Endpoint | 8 (POST/GET list/GET single/DELETE/PATCH/start/suspend/restart) | 0 | 100% |
| Role | 6 (POST/GET list/DELETE/reset_password/GET single/PATCH) | 0 | 100% |
| Database | 6 (POST/GET list/DELETE/GET single/PATCH/connection_uri) | 0 | 100% |
| Operation | 2 (GET list/GET single) | 0 | 100% |
| Snapshot | 0 | 6 | 0% |
| 其他（Org/Auth/Billing） | 0 | ~50+ | 0% |
| **总计** | **33** | **~62** | **~35%** |

详见 [Neon API Gap Analysis](./neon-api-gap-analysis.md)。

### 3.5 可观测性

| 差距 | 说明 | 状态 |
|------|------|------|
| Prometheus 指标 | 无 `/metrics` 端点 | ❌ |
| Grafana Dashboard | 无预置仪表板 | ❌ |
| 结构化日志 | 有零散日志，无统一格式/级别 | ⚠️ |
| 告警规则 | 无告警定义 | ❌ |
| Tracing（分布式追踪） | 无 | ❌ |

---

## 四、实施路线图

### Phase 1：基础设施加固（**已完成**）

**目标**：确保每个组件在 K8s 中可靠运行，故障时自动恢复。全组件健康探针、资源配置、节点故障自动恢复已全部实现。

| 任务 | 组件 | 产出 | 状态 |
|------|------|------|:---:|
| 健康探针 | Pageserver | Liveness/Readiness/Startup: `GET /v1/status` + ProbeConfig 可覆盖 | ✅ |
| | Safekeeper | Liveness/Readiness/Startup: `GET /v1/status` + ProbeConfig 可覆盖 | ✅ |
| | Storage Controller | Liveness: `/live`, Readiness: `/ready`, Startup: `/status` + ProbeConfig 可覆盖 | ✅ |
| | Storage Broker | Liveness/Readiness/Startup: `GET /status` (port 50051) + ProbeConfig 可覆盖 | ✅ |
| | Compute Node | Liveness/Readiness/Startup: TCP Socket (port 55433)，阶段 B 升级为 HTTP 探针 | ✅ |
| PodAntiAffinity | Pageserver | requiredDuringScheduling: 不同 node | ✅ |
| | Safekeeper | requiredDuringScheduling: 不同 node（至少 3 个） | ✅ |
| | SC / Broker / Compute | SC: hard (required, hostname), Broker: soft (preferred, hostname) | ✅ |
| PDB | Pageserver | maxUnavailable=1 | ✅ |
| | Safekeeper | maxUnavailable=1 | ✅ |
| | SC / Broker | maxUnavailable=1 | ✅ |
| Resource Requests/Limits | Pageserver | CPU/Memory: 500m/256Mi req, 2/512Mi lim (支持覆盖) | ✅ |
| | Safekeeper | CPU/Memory: 500m/512Mi req, 2/2Gi lim | ✅ |
| | SC / Broker / Compute | SC: 250m-1/256-512Mi, Broker: 100m-500m/128-256Mi | ✅ |
| Cluster Controller 增强 | Cluster | 自动创建 Safekeeper CR（根据 NumSafekeepers） | ✅ |
| **节点故障自动恢复** | **Pageserver/Safekeeper** | **`handleNodeFailure` 完整故障检测（Pending/Running-NotReady/Terminating/Failed/Unknown）、Pod 事件监听、`DeletionTimestamp` 优先检查修复、Safekeeper `DefaultSafekeeperConfig.nodeFailure` 配置支持** | ✅ **已完成** |

**设计文档**：
- Pageserver: [pageserver-production.md](./design/pageserver-production.md)
- Safekeeper: [safekeeper-production.md](./design/safekeeper-production.md)
- 健康探针: [health-probes.md](./design/health-probes.md)
- SC/Broker 生产化: [sc-broker-production.md](./design/sc-broker-production.md)

### Phase 2：安全加固（剩余 ~1 周，P0）

**目标**：~~全链路 JWT 认证，移除 SC `--dev` 模式。~~ **阶段目标已完成**。剩余：API Key 用户认证、JWT 密钥轮转、Safekeeper 安全注销。

| 任务 | 产出 | 状态 |
|------|------|:---:|
| JWT 密钥管理 | Ed25519 密钥对生成、Secret 存储（`reconcileJWTKeys()` 在 Cluster Controller 中自动创建） | ✅ |
| Component Token 持久化 | `ensureComponentTokens()` + `ensurePSAuthTokens()` 首次生成后写入 Secret，后续 reconcile 直接复用，避免 Deployment/StatefulSet 频繁滚动更新 | ✅ |
| Operator → SC 认证 | SCClient 使用 `admin` scope JWT 签名所有管理请求（RegisterSafekeeper、DecommissionSafekeeper、Node API） | ✅ |
| SC 内部 JWT 验证 | SC 通过 `PUBLIC_KEY` 环境变量加载公钥 + `*_JWT_TOKEN` 环境变量注入各组件 Token，运行时验证所有请求 | ✅ |
| PS → SC upcall 认证 | `pageserver_control_plane_token`（generations_api scope）持久化到 Secret，ConfigMap 通过 `control_plane_api_token` 字段注入 | ✅ |
| PS → SK WAL 认证 | `pageserver_safekeeper_token`（safekeeperdata scope）持久化到 Secret | ✅ |
| SC/PK/SK JWT Volume | `utils.JWTVolume()` + `utils.JWTVolumeMount()` 统一挂载 `public.pem` 到所有组件 | ✅ |
| SC `--dev` 移除 | `storage_controller` 启动参数中已移除 `--dev`，通过 `PUBLIC_KEY` 环境变量启用 JWT 验证 | ✅ |
| HTTP API 认证 | API Key / JWT 用户认证（业务层） | ❌ |
| Safekeeper 安全注销 | `scheduling_policy=Offline` + `DELETE` 安全下线 | ❌ |
| JWT 密钥轮转 | 支持密钥到期后无缝更换 | ❌ |

**前置依赖**：Phase 1 完成（Safekeeper 自动创建 ✅ + 反亲和性 ✅，`--timeline-safekeeper-count ≥ 3`）

**设计文档**：[todo.md — JWT认证问题](./design/todo.md#jwt认证问题)、[todo.md — 去掉 Storage Controller --dev 模式](./design/todo.md#去掉-storage-controller---dev-模式)

### Phase 3：API 完善 + 可观测性（2 周，P1）

**目标**：补齐剩余核心 API，建立可观测性体系。

| 任务 | 说明 | 状态 |
|------|------|:---:|
| PATCH/Update 端点 | Project PATCH、Branch PATCH、Endpoint PATCH、Role PATCH、Database PATCH | ✅ |
| Endpoint 生命周期 API | start / suspend / restart 路由 | ✅ |
| 单条 GET | Role GET single、Database GET single | ✅ |
| Connection URI | `GET .../connection_uri` 生成连接字符串 | ✅ |
| Branch 默认设置 | `POST .../set_as_default` | ✅ |
| Endpoint 生命周期 Controller | start/suspend/restart 的 K8s reconcile 逻辑 | ❌ |
| Branch Restore | 基于 LSN/Timestamp 的 PITR 恢复 | ❌ |
| Prometheus 指标 | 全组件 `/metrics` 端点 | ❌ |
| Grafana Dashboard | SC 状态、Pageserver 指标、Compute 连接数、Safekeeper 延迟 | ❌ |
| 结构化日志 | 统一 JSON 格式、日志级别配置 | ❌ |
| ServiceExposure 配置 | PostgreSQL Service 对外暴露策略（ClusterIP/NodePort/LoadBalancer） | ✅ |

### Phase 4：高级功能（4 周，P2）

**目标**：声明式扩缩容、Scale-to-Zero、Snapshots。

| 任务 | 说明 |
|------|------|
| Compute Scale-to-Zero | 自动挂起/恢复 |
| 声明式扩缩容 | Pageserver/Safekeeper 数量变更（含 drain + 重新调度） |
| Snapshot API | 创建/列表/删除快照 |
| Branch Import | 从外部 PG 导入 |
| Node 故障自动恢复 | 检测 → PVC 解绑 → 重新调度 |
| Pageserver Filling 模式 | 新节点预热避免影响在线流量 |

**设计文档**：[pageserver-production.md](./design/pageserver-production.md) — Section 5 (Filling), Section 6 (Scale)

### Phase 5：生产就绪收尾（2 周，P2/P3）

**目标**：多实例 HA、备份验证、升级策略。

| 任务 | 说明 |
|------|------|
| Operator HA | LeaderElection + 多副本部署 |
| SC DB HA | Storage Controller 数据库多副本/备份 |
| PGBouncer | 连接池（可选，根据规模） |
| 备份验证 | 定期从备份恢复测试数据完整性 |
| 升级策略 | 滚动更新、版本兼容性矩阵 |
| 文档 | 操作手册、故障处理 Runbook、SLA 定义 |
| 安全审计 | 渗透测试、依赖扫描、CVE 修复 |

---

## 五、总计估算

| Phase | 时间 | 优先级 | 核心产出 | 进度 |
|-------|------|--------|---------|:---:|
| Phase 1：基础设施 | ~1.5 周 | P0 | 全组件健康探针已完成；剩余：SC/Broker/Compute 资源配置（反亲和、PDB）、PVC 恢复 | ~80% |
| Phase 2：安全加固 | 1 周（剩余） | P0 | 组件间 JWT 认证链路已完成（Operator→SC/SC内部/PS→SC/PS→SK/SC→PS/SK）、SC `--dev` 已移除、Token 持久化已完成；剩余：API Key 用户认证、密钥轮转、Safekeeper 安全下线 | ~73% |
| Phase 3：API + 观测 | 2 周 | P1 | Endpoint 生命周期 Controller、PITR、Prometheus/Grafana | ~60% |
| Phase 4：高级功能 | 4 周 | P2 | 声明式扩缩容、Scale-to-Zero、Snapshots、故障恢复 | 0% |
| Phase 5：收尾 | 2 周 | P2/P3 | Operator HA、升级策略、文档 | 0% |
| **合计** | **~10 周** | | | |

---

## 六、关键风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| SC `--dev` 移除联动范围大 | ~~需同时完成 JWT + Safekeeper 自动化 + 全链路认证~~ 已完成：JWT 全链路已实施，SC `--dev` 已移除 ✅ | Phase 2 阶段性完成，各组件已验证通过 |
| PVC 粘滞问题 | 节点故障后服务长时间不可用 | Phase 1 优先解决 |
| 上游 Neon 版本兼容 | 镜像版本锁定，上游更新可能引入 breaking changes | 锁定版本 + 定期回归测试 |
| S3 延迟 | Pageserver 冷启动从 S3 拉取数据可能数分钟 | StartupProbe 阈值调大 + 预热策略 |
| Compute/Neon 镜像分离 | neondatabase/neon 不含 compute_ctl，compute-node 不含 safekeeper | 已通过 `Cluster.Spec.ComputeImage` 字段解决 |

---

## 七、关联文档

| 文档 | 说明 |
|------|------|
| [API 实现清单](./api-inventory.md) | 已实现的 CRD / Controller / HTTP 端点 |
| [Neon API 差距分析](./neon-api-gap-analysis.md) | 对标 Neon Cloud API 的端点级差距 |
| [Pageserver 生产化设计](./design/pageserver-production.md) | 健康探针、PDB、扩缩容、节点故障 |
| [Safekeeper 生产化设计](./design/safekeeper-production.md) | CRD 架构、SC 注册、安全下线 |
| [SC/Broker 生产化设计](./design/sc-broker-production.md) | SC/Broker 资源限制、PDB、反亲和性 |
| [全组件健康探针设计](./design/health-probes.md) | 六组件探针深度分析、参数对齐、实施计划 |
| [Todo 清单](./design/todo.md) | 开发任务追踪 |
| [故障恢复](./failure-recovery.md) | Safekeeper 故障矩阵、恢复步骤 |
| [Reconcile 模式](./reconcile-pattern.md) | SSA 通用引擎分析 |
| [Control Plane 架构](./design/control-plane-architecture.md) | 控制面完整架构设计 |
| [上游 Neon 分析](./design/upstream-neon-analysis.md) | 上游 Neon 生产环境架构 |
