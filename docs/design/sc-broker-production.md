# Storage Controller / Storage Broker 生产化加固设计

> 状态: 已实现 | 创建: 2026-07-02 | 实施: 2026-07-02 | 作者: AI Assistant

## 1. 背景

### 1.1 当前状态

Storage Controller 和 Storage Broker 是 Neon 控制面的核心组件，分别承担集群调度和消息总线的职责。目前两个组件的 Deployment 配置在生产化方面存在以下缺失：

| 能力                     |            Pageserver            |           Safekeeper           | Compute |    **SC**    |  **Broker**  |
| ------------------------ | :------------------------------: | :----------------------------: | :-----: | :-----------------: | :-----------------: |
| Resource Requests/Limits | ✅ (500m/256Mi req, 2/512Mi lim) | ✅ (500m/512Mi req, 2/2Gi lim) |   ✅   | ❌`resources: {}` | ❌`resources: {}` |
| PodDisruptionBudget      |       ✅ MaxUnavailable=1       |      ✅ MaxUnavailable=1      |   ❌   |         ❌         |         ❌         |
| Pod Anti-Affinity        |        ✅ hard (hostname)        |       ✅ hard (hostname)       |   ❌   |         ❌         |         ❌         |
| SecurityContext          |          ✅ (uid=1000)          |         ✅ (uid=1000)         |   ✅   |         ❌         |         ❌         |
| TerminationGracePeriod   |               60s               |              30s              |   60s   |         ❌         |         ❌         |

**核心风险**：

1. **无资源限制**：控制面组件可能在异常情况下无限占用资源，或在高负载时被 OOMKilled
2. **无 PDB**：滚动更新或节点驱逐时两个 Pod 可能同时中断（若多副本），单副本场景下节点维护会被阻塞
3. **无反亲和性**：若部署多副本，可能落在同一节点上，失去物理隔离
4. **无 SecurityContext**：容器可能以 root 运行，存在安全风险

### 1.2 组件职责摘要

| 组件                         | 类型                       | 依赖   |    数据持久化    |
| ---------------------------- | -------------------------- | ------ | :---------------: |
| **Storage Controller** | 有状态（需外部 PG 数据库） | Broker | ❌ (数据库在外部) |
| **Storage Broker**     | 无状态 (pub-sub)           | 无     |    ❌ (纯内存)    |

**Storage Controller** 是 Neon 的"集群大脑"，负责：

- Tenant/Shard CRUD 管理与调度
- Generation 单调递增保证数据安全
- Pageserver/Safekeeper 心跳监控（每 5s）
- Reconcile 引擎（128/256 并发）、实时迁移、Shard Split
- Leader 选举（通过数据库 `leader` 表）

**Storage Broker** 是消息总线，负责：

- Safekeeper ⇄ Pageserver 之间的 pub-sub 消息传递
- 通过 gRPC 双向流传输 `SafekeeperTimelineInfo`
- 纯内存操作，无持久化状态

## 2. 上游参考

### 2.1 来自 Neon 核心仓库的资源指引

源码分析（`/home/postgres/works/opensource/neon`）：

**Storage Controller 运行时特征**：

- 单线程 tokio 异步运行时 (`new_current_thread`)
- 99 个 blocking threads（数据库操作）
- 使用 jemalloc 全局分配器
- 内存模型：每个 tenant shard 约 8KB（`ServiceInner` wrapper），可扩展到百万 shard
- RFC 037 明确的 K8s 部署策略：`RollingUpdate, maxSurge=1, maxUnavailable=0`

**Storage Controller 数据库需求**（来自 `docs/storage_controller.md`）：

> *"The resource requirements for the database are very low: a single CPU core and 1GiB of memory should work well for most deployments. The physical size of the database is typically under a gigabyte."*

**Storage Broker 特征**：

- 纯内存 pub-sub，默认消息通道 32 + 16384
- 设计为"无状态"，依赖 K8s 容错而非内置复制
- Docker Compose 中作为最轻量服务运行（无资源限制配置）

### 2.2 与本 Operator 的差异

上游 Neon Cloud 的生产部署配置在内部仓库 `neondatabase/infra` 中，未开源。本 Operator 面向私有化部署，需自行设计方案，同时借鉴上游的设计理念。

## 3. 设计方案

### 3.1 资源配置设计

#### 3.1.1 设计原则

1. **保守低估**：Request 值设为正常工作所需的下限，不给调度器过高的门槛
2. **合理上限**：Limit 值留有合理的 burst 空间，但不无限占用
3. **参考上游**：基于 Neon 仓库文档中的资源指引
4. **对齐已有组件**：与 Pageserver/Safekeeper 的资源分配模式保持一致性
5. **可覆盖**：通过 `ClusterSpec` 提供可选覆盖字段，允许高级用户按需调整

#### 3.1.2 Storage Controller 资源配置

SC 进程的资源消耗分析：

| 组成部分            | 估算        | 说明                    |
| ------------------- | ----------- | ----------------------- |
| Tokio async runtime | 轻量        | 单线程，CPU 密集型      |
| 99 blocking threads | ~8MB 栈空间 | 每个线程 80KB 默认栈    |
| In-memory state     | ~8KB/shard  | 典型 1000 shard ≈ 8MB  |
| jemalloc metadata   | ~5-10MB     | 内存分配器开销          |
| DB connection pool  | ~10-20MB    | 99 connections 的缓冲区 |

**推荐配置**：

```yaml
resources:
  requests:
    cpu: 250m
    memory: 256Mi
  limits:
    cpu: 1
    memory: 512Mi
```

**设计理由**：

- **CPU 250m request**：SC 是单线程 async，正常运行约需 100-200m CPU。250m 提供安全边际
- **CPU 1 limit**：单线程模型天然无法利用多核，1 核 limit 足够。同时也为 blocking threads 的短时 CPU 密集操作留有余地
- **Memory 256Mi request**：1000 shard 场景约需 ~50MB，256Mi 留有充足余量
- **Memory 512Mi limit**：防止异常情况（如 goroutine 泄漏等效、内存碎片）导致无限增长。百万 shard 场景（~8GB）需用户自行覆盖

#### 3.1.3 Storage Broker 资源配置

Broker 资源消耗分析：

| 组成部分  | 估算          | 说明                         |
| --------- | ------------- | ---------------------------- |
| gRPC 服务 | 轻量          | tonic/h2 多路复用            |
| 消息缓冲  | ~32KB + 16MB* | 16384 条消息的 all-keys 通道 |
| 连接状态  | ~1MB          | 每连接少量记账               |

> *16MB 为上界估算（16384 × ~1KB/msg），实际取决于消息大小。

**推荐配置**：

```yaml
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 256Mi
```

**设计理由**：

- **CPU 100m request**：Broker 几乎纯网络 I/O 绑定，CPU 消耗极低
- **CPU 500m limit**：高并发 pub-sub 时允许短期 burst
- **Memory 128Mi request**：正常消息量下的合理请求
- **Memory 256Mi limit**：防止异常消息风暴导致 OOM。若业务消息量大，用户可通过覆盖字段调整

### 3.2 Pod Anti-Affinity 设计

#### 3.2.1 Storage Controller

**采用 hard (`requiredDuringScheduling`) 反亲和性**，与 Pageserver/Safekeeper 保持一致：

```yaml
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/component: storage-controller
            molnett.org/cluster: <cluster-name>
        topologyKey: kubernetes.io/hostname
```

**设计理由**：

1. SC 通过数据库 Leader 选举实现 HA，不需要 K8s 级别的多副本容错。同一集群通常只有 1 个活跃 Pod（非 leader 的 `/live` 返回 503，被 K8s 持续重启）
2. 在滚动更新期间（`maxSurge=1`），新旧两个 Pod 短暂共存。hard 反亲和确保它们不在同一节点，避免单节点故障时更新中断
3. 与 PS/SK 的模式一致，降低运维认知负担

#### 3.2.2 Storage Broker

**采用 soft (`preferredDuringScheduling`) 反亲和性**：

```yaml
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchLabels:
              app.kubernetes.io/component: storage-broker
              molnett.org/cluster: <cluster-name>
          topologyKey: kubernetes.io/hostname
```

**设计理由**：

1. Broker 是无状态服务，多副本之间无需强隔离
2. soft 反亲和让调度器尽量分散放置，但不阻塞调度
3. 若当前 Broker 仅 1 副本，soft 亲和性对单副本无影响，同时为未来多副本做了准备
4. **不与 SC 一样采用 hard 的原因**：Broker 未来可能扩展到 3+ 副本，hard 反亲和要求每个副本在不同节点，在节点数不足时会导致 Pod Pending，用 soft 避免此问题

### 3.3 PodDisruptionBudget 设计

#### 3.3.1 Storage Controller

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: storage-controller
      molnett.org/cluster: <cluster-name>
```

**设计理由**：

- SC 为单副本部署（`replicas: 1`），`maxUnavailable: 1` 表示**自愿中断最多允许 1 个 Pod 不可用**，即允许全部中断
- 这看似矛盾实则合理：SC 的 Leader 选举机制保证了重启后快速恢复，PDB 不应当阻塞节点维护
- 若使用 `maxUnavailable: 0`（即完全阻止自愿中断），会导致节点排空（drain）时卡死，运维体验差
- 与 PS/SK 的 PDB 模式一致

#### 3.3.2 Storage Broker

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/component: storage-broker
      molnett.org/cluster: <cluster-name>
```

**设计理由**：

- 当前 Broker 为单副本，`maxUnavailable: 1` 允许全部中断，不阻塞节点维护
- Broker 故障的恢复时间极短（秒级重启 + 无状态热启动），中断窗口 < 5s
- 如果未来 Broker 扩展到多副本，可将 PDB 改为 `maxUnavailable: 25%` 或 `minAvailable: 1`

### 3.4 SecurityContext

两个组件均添加非 root 运行配置：

```yaml
securityContext:
  runAsUser: 1000
  runAsGroup: 1000
  fsGroup: 1000
```

与 Pageserver/Safekeeper 保持一致。

### 3.5 TerminationGracePeriodSeconds

- **Storage Controller**: `60s` — 允许正在进行的 reconciliation 完成，step down leader 状态
- **Storage Broker**: `30s` — 无状态，快速终止

### 3.6 完整 Deployment Spec 变更汇总

#### 3.6.1 Storage Controller Deployment 新增

```yaml
spec:
  replicas: 1
  template:
    spec:
      terminationGracePeriodSeconds: 60
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - labelSelector:
                matchLabels:
                  app.kubernetes.io/component: storage-controller
                  molnett.org/cluster: <cluster>
              topologyKey: kubernetes.io/hostname
      containers:
        - name: storage-controller
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: 1
              memory: 512Mi
```

#### 3.6.2 Storage Broker Deployment 新增

```yaml
spec:
  replicas: 1
  template:
    spec:
      terminationGracePeriodSeconds: 30
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                labelSelector:
                  matchLabels:
                    app.kubernetes.io/component: storage-broker
                    molnett.org/cluster: <cluster>
                topologyKey: kubernetes.io/hostname
      containers:
        - name: storage-broker
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 256Mi
```

### 3.7 CR 类型扩展

在 `ClusterSpec` 中新增可选的资源覆盖字段，遵循现有的 `StorageControllerProbes` / `StorageBrokerProbes` 命名模式：

```go
// ClusterSpec 新增字段

// StorageControllerResources 可选覆盖 SC 容器的资源规格。
// 若未设置，使用 operator 内置默认值。
// +optional
StorageControllerResources *corev1.ResourceRequirements `json:"storageControllerResources,omitempty"`

// StorageBrokerResources 可选覆盖 Broker 容器的资源规格。
// 若未设置，使用 operator 内置默认值。
// +optional
StorageBrokerResources *corev1.ResourceRequirements `json:"storageBrokerResources,omitempty"`
```

## 4. 实现计划

### 4.1 变更文件清单

| #  | 文件                                      | 变更类型 | 说明                                                                |
| -- | ----------------------------------------- | :------: | ------------------------------------------------------------------- |
| 1  | `api/v1alpha1/cluster_types.go`         |   修改   | 新增`StorageControllerResources`、`StorageBrokerResources` 字段 |
| 2  | `specs/storagecontroller/deployment.go` |   修改   | 添加 Resources、Affinity、SecurityContext、TerminationGracePeriod   |
| 3  | `specs/storagecontroller/labels.go`     |   新增   | 提取标签函数，与 PS/SK 模式对齐                                     |
| 4  | `specs/storagecontroller/pdb.go`        |   新增   | PDB 定义                                                            |
| 5  | `specs/storagecontroller/defaults.go`   |   新增   | 默认资源常量 + 资源选择函数                                         |
| 6  | `specs/storagebroker/deployment.go`     |   修改   | 添加 Resources、Affinity、SecurityContext、TerminationGracePeriod   |
| 7  | `specs/storagebroker/labels.go`         |   新增   | 提取标签函数                                                        |
| 8  | `specs/storagebroker/pdb.go`            |   新增   | PDB 定义                                                            |
| 9  | `specs/storagebroker/defaults.go`       |   新增   | 默认资源常量 + 资源选择函数                                         |
| 10 | `internal/controller/cluster_create.go` |   修改   | SC/Broker PDB reconcile + 传递资源覆盖                              |
| 11 | `specs/storagecontroller/testdata/`     |   新增   | Golden test cases                                                   |
| 12 | `specs/storagebroker/testdata/`         |   新增   | Golden test cases                                                   |

### 4.2 实施顺序

```
Phase 1: 基础加固 (~1 天)
├── 1a. specs/storagecontroller/deployment.go  — 添加 Resources/Affinity/Security/Termination
├── 1b. specs/storagebroker/deployment.go      — 同上
├── 1c. specs/storagecontroller/defaults.go    — 默认资源 & 选择函数
├── 1d. specs/storagebroker/defaults.go         — 同上
└── 1e. 运行 golden tests 确认输出变更

Phase 2: PDB 支持 (~0.5 天)
├── 2a. specs/storagecontroller/pdb.go         — PDB spec
├── 2b. specs/storagebroker/pdb.go              — PDB spec
├── 2c. internal/controller/cluster_create.go  — PDB reconcile
└── 2d. Golden tests

Phase 3: CR 可覆盖 (~0.5 天)
├── 3a. api/v1alpha1/cluster_types.go          — 新增 Resource 字段
├── 3b. specs 层接入覆盖逻辑                      — 优先使用 CR 中的覆盖值
└── 3c. 更新 golden tests
```

### 4.3 风险与缓解

| 风险                                      | 概率 | 缓解措施                                      |
| ----------------------------------------- | :--: | --------------------------------------------- |
| 默认资源限制过小导致 SC/Broker OOMKill    |  低  | 基于上游文档 + 同类组件推算；提供 CR 覆盖字段 |
| PDB 阻塞节点维护                          |  中  | 使用`maxUnavailable: 1` 允许全部中断        |
| hard 反亲和导致 SC 无法调度（单节点集群） |  低  | 单节点集群可用`kubectl patch` 覆盖 affinity |
| golden test 大量变更                      |  中  | 按 Phase 分批实施，每批更新 golden 数据       |

### 4.4 回滚方案

所有变更均为新增字段和默认值，无破坏性变更。回滚只需恢复 git 版本即可。

## 5. 与其他设计文档的关系

| 文档                               | 关联点                                         |
| ---------------------------------- | ---------------------------------------------- |
| `health-probes.md`               | SC/Broker 探针参数已在健康探针设计中完成       |
| `jwt-production-design.md`       | SC JWT 认证已启用，资源配置不影响认证流程      |
| `resource-deletion-finalizer.md` | Finalizer 依赖 SC API 可用性，PDB 保证中断可控 |
| `safekeeper-deletion.md`         | SC 资源加固是 safekeeper 安全删除的前置依赖    |

## 6. 待决议

- [ ] SC 的 `maxUnavailable: 1` 是否过松？是否需要 `maxUnavailable: 0` 完全阻止自愿中断？
  - **倾向**：保持 `maxUnavailable: 1`，因为 Leader 选举提供快速恢复，不阻塞运维操作
- [ ] Broker 未来是否需要扩展到多副本？当前设计已预留 soft 反亲和 + PDB 可调空间
  - **倾向**：当前 1 副本即可，Broker 重启秒级，故障窗口极短

## 附录 A：资源配置速查表

| 组件                         |  CPU Request  |   CPU Limit   | Memory Request |  Memory Limit  |
| ---------------------------- | :------------: | :------------: | :-------------: | :-------------: |
| Pageserver                   |      500m      |       2       |      256Mi      |      512Mi      |
| Safekeeper                   |      500m      |       2       |      512Mi      |       2Gi       |
| **Storage Controller** | **250m** |  **1**  | **256Mi** | **512Mi** |
| **Storage Broker**     | **100m** | **500m** | **128Mi** | **256Mi** |

## 附录 B：上游 Neon 仓库关键参考

| 内容                                                         | 来源                                                     |
| ------------------------------------------------------------ | -------------------------------------------------------- |
| SC 数据库需求: 1 CPU + 1GiB RAM (not the SC process itself)  | `docs/storage_controller.md` L72                       |
| SC 内存: ~8KB/shard, 可扩展到百万 shard                      | RFC`2025-02-14-storage-controller.md` L62-63           |
| SC 为单线程 tokio + 99 blocking threads + jemalloc           | `storage_controller/src/main.rs` L338-345              |
| SC K8s 部署: RollingUpdate, maxSurge=1, maxUnavailable=0     | RFC`037-storage-controller-restarts.md`                |
| SC leader election: 通过 DB`leader` 表, 非 leader 返回 503 | RFC`037`                                               |
| Broker: 无状态 pub-sub, 依赖 K8s 容错                        | `docs/storage_broker.md`                               |
| Broker: 消息通道 32 + 16384, keepalive 5s                    | `storage_broker/src/bin/storage_broker.rs`, `lib.rs` |
