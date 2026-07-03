# Compute 服务模型

> 源码：`specs/compute/service.go`、`specs/compute/endpoint_spec.go`

本文分析 operator 为每个 **Endpoint** 创建的 Kubernetes Service 资源的设计与职责。Compute 实例不再由 Branch 直接创建，而是通过 Endpoint CRD 独立管理。

---

## 一、ServiceConfig 工厂参数

`ServiceConfig` 是所有 Compute Service 的统一工厂参数结构体，定义在 `specs/compute/service.go:13-18`：

```go
type ServiceConfig struct {
    Suffix    string
    Component string
    PortName  string
    Port      int32
}
```

### 1.1 字段含义

每个字段都映射到 `createService` 生成的 Kubernetes Service 资源的特定位置：

| 字段                | 映射路径                                                                 | 示例值                                          | 作用                                                            |
| ------------------- | ------------------------------------------------------------------------ | ----------------------------------------------- | --------------------------------------------------------------- |
| **Suffix**    | `metadata.name` = `{branch}-{Suffix}`                                | `"admin"` → Service 名 `test-branch-admin` | 决定 Service 资源的唯一名称，区分同一 Branch 下的不同 Service   |
| **Component** | `metadata.labels["molnett.org/component"]`                             | `"compute-admin"`                             | 标识 Service 的组件类型，用于 label selector 查询和资源归类     |
| **PortName**  | `spec.ports[].name`                                                    | `"admin"`                                     | Kubernetes 端口标识名，同一 Service 内端口名必须唯一            |
| **Port**      | `spec.ports[].port`（监听端口）`spec.ports[].targetPort`（目标端口） | `3080`                                        | 同时设定 Service 暴露端口和容器目标端口（二者相等，无端口映射） |

### 1.2 Port 为什么同时用作 port 和 targetPort？

```49:49:specs/compute/service.go
TargetPort: intstr.FromInt(int(config.Port)),
```

同一个值既用于 Service 监听端口又用于 Pod 内容器端口，不做端口转换。设计意图是降低排障复杂度——Service 的 3080 一定通向容器的 3080，没有中间映射。

---

## 二、createService 工厂函数

```20:54:specs/compute/service.go
func createService(branch *neonv1alpha1.Branch, project *neonv1alpha1.Project, config ServiceConfig) *corev1.Service {
    serviceName := fmt.Sprintf("%s-%s", branch.Name, config.Suffix)
    labels := map[string]string{
        "molnett.org/cluster":   project.Spec.ClusterName,
        "molnett.org/component": config.Component,
        "molnett.org/branch":    branch.Name,
        "neon.timeline_id":      branch.Spec.TimelineID,
        "neon.tenant_id":        project.Spec.TenantID,
    }

    return &corev1.Service{
        ObjectMeta: metav1.ObjectMeta{
            Name:      serviceName,
            Namespace: branch.Namespace,
            Labels:    labels,
        },
        Spec: corev1.ServiceSpec{
            Selector: map[string]string{
                "app": fmt.Sprintf("%s-compute-node", branch.Name),
            },
            Ports: []corev1.ServicePort{
                {
                    Name:       config.PortName,
                    Port:       config.Port,
                    Protocol:   corev1.ProtocolTCP,
                    TargetPort: intstr.FromInt(int(config.Port)),
                },
            },
        },
    }
}
```

关键设计点：

- **5 个 label**：`cluster`、`component`、`branch`、`timeline_id`、`tenant_id`，完整标识所属项目与分支，方便通过 label selector 查询
- **裸 selector**：只用 `app` 这一个 label 匹配 Pod，不使用全部 labels。Kubernetes Service 的 label selector 只需区分"流量该发给谁"，多余的 label 会限制匹配范围
- **port == targetPort**：直接映射，无中间转换

---

## 三、AdminService 与 PostgresService

两个公有的构造函数通过不同的 `ServiceConfig` 参数调用同一个 `createService` 工厂：

```56:72:specs/compute/service.go
func AdminService(branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) *corev1.Service {
    return createService(branch, project, ServiceConfig{
        Suffix:    "admin",
        Component: "compute-admin",
        PortName:  "admin",
        Port:      3080,
    })
}

func PostgresService(branch *neonv1alpha1.Branch, project *neonv1alpha1.Project) *corev1.Service {
    return createService(branch, project, ServiceConfig{
        Suffix:    "postgres",
        Component: "compute-postgres",
        PortName:  "postgres",
        Port:      55433,
    })
}
```

### 3.1 对比表

| 维度                        | AdminService                     | PostgresService                    |
| --------------------------- | -------------------------------- | ---------------------------------- |
| **Suffix**            | `admin`                        | `postgres`                       |
| **Component label**   | `compute-admin`                | `compute-postgres`               |
| **Port**              | 3080                             | 55433                              |
| **生成的 Service 名** | `{branch}-admin`               | `{branch}-postgres`              |
| **目标进程**          | `compute_ctl`（进程管理器）    | `postgres`（数据库引擎）         |
| **通信协议**          | HTTP REST（`/configure`）      | PostgreSQL Wire Protocol           |
| **调用方**            | neon-operator                    | Pageserver、Safekeeper、用户客户端 |
| **流量性质**          | 低频控制指令                     | 高频数据读写                       |
| **认证方式**          | JWT Bearer Token（Ed25519 签名） | PostgreSQL 原生认证                |

### 3.2 功能详解

**AdminService（控制面通道）**

AdminService 是 operator 操作 compute 的唯一入口。消费逻辑在 `specs/compute/spec.go:215-248`：

```
operator 构造 ComputeSpec 配置
  → JWTManager.GenerateToken() 签发 Bearer Token
  → HTTP POST http://{branch}-admin.neon:3080/configure
     Authorization: Bearer <token>
     Body: { pageserver 地址, safekeeper 地址, tenant_id, timeline_id, ... }
  → compute_ctl 验签后应用配置，启动/重启 PostgreSQL
```

没有 AdminService，operator 无法告诉 compute：该连接哪个 Pageserver、哪个 Safekeeper、当前 branch 的 timeline_id 是什么。

**PostgresService（数据面通道）**

PostgreSQL 对外提供数据库服务的标准端点。消费者包括：

| 消费者                                   | 场景                              |
| ---------------------------------------- | --------------------------------- |
| **Pageserver**                     | 推送 WAL 日志、响应 page requests |
| **Safekeeper**                     | WAL 复制流                        |
| **存储控制器**                     | 数据库管理操作                    |
| **用户客户端 / Serverless Driver** | 执行 SQL 查询                     |

### 3.3 架构总览

两个 Service 通过相同的 `selector: app: {branch}-compute-node` 将流量路由到同一个 Compute Pod，Pod 内运行着两个监听不同端口的进程：

```
┌──────────────────────────────────────────────────────┐
│  Compute Pod                                         │
│                                                      │
│  ┌──────────────────┐   ┌──────────────────────┐     │
│  │   compute_ctl    │   │      postgres        │     │
│  │   :3080          │───│      :55433          │     │
│  │   (进程管理器)     │   │   (数据库引擎)         │     │
│  └────────┬─────────┘   └───────────┬──────────┘     │
└───────────┼─────────────────────────┼────────────────┘
            │                         │
      AdminService             PostgresService
      (port 3080)               (port 55433)
            │                         │
     ┌──────▼──────┐          ┌───────▼───────┐
     │  operator   │          │  pageserver   │
     │  (配置下发)   │          │  (WAL 推送)    │
     └─────────────┘          │  safekeeper   │
                              │  用户客户端     │
                              └───────────────┘
```

Deployment 中 compute_ctl 的启动命令印证了这两个端口：

```43:45:specs/compute/testdata/deployment.yaml
        - echo "$INITIAL_SPEC_JSON" > /var/spec.json && /usr/local/bin/compute_ctl
          --pgdata /.neon/data/pgdata --connstr=postgresql://cloud_admin:@0.0.0.0:55433/postgres
          --compute-id test-branch -p http://neon-controlplane.neon:8081 --pgbin /usr/local/bin/postgres
```

- `--connstr=postgresql://cloud_admin:@0.0.0.0:55433/postgres` — compute_ctl 通过此连接串管理 PostgreSQL，确认数据库引擎监听在 **55433**
- compute_ctl 自身监听 **3080** 等待 operator 的 `/configure` 请求

---

## 四、Golden Test 与 Test Fixture

### 4.1 测试机制

`specs/compute/golden_test.go` 对三个 Compute 子资源做黄金测试：

```22:34:specs/compute/golden_test.go
    cases := []struct {
        name string
        obj  any
    }{
        {"deployment", compute.Deployment(branch, project)},
        {"admin_service", compute.AdminService(branch, project)},
        {"postgres_service", compute.PostgresService(branch, project)},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            testutils.AssertGolden(t, "testdata/"+tc.name+".yaml", tc.obj)
        })
    }
```

测试逻辑：

1. 用固定输入（`test-branch`, `test-cluster`, `test-project`）调用工厂函数生成 K8s 对象
2. 将对象序列化为 YAML
3. 与 `testdata/` 目录下的对应 `.yaml` 文件逐字比较
4. 不匹配则报 unified diff；传 `-update-golden` 标志则重写快照文件

### 4.2 testdata 目录结构

| 文件                      | 生成函数              | Kind               |
| ------------------------- | --------------------- | ------------------ |
| `deployment.yaml`       | `Deployment()`      | apps/v1 Deployment |
| `admin_service.yaml`    | `AdminService()`    | v1 Service         |
| `postgres_service.yaml` | `PostgresService()` | v1 Service         |

### 4.3 AdminService Fixture 解读

```1:22:specs/compute/testdata/admin_service.yaml
apiVersion: v1
kind: Service
metadata:
  creationTimestamp: null
  labels:
    molnett.org/branch: test-branch
    molnett.org/cluster: test-cluster
    molnett.org/component: compute-admin
    neon.tenant_id: ""
    neon.timeline_id: ""
  name: test-branch-admin
  namespace: neon
spec:
  ports:
  - name: admin
    port: 3080
    protocol: TCP
    targetPort: 3080
  selector:
    app: test-branch-compute-node
status:
  loadBalancer: {}
```

| 字段                      | 值                           | 含义                                                                        |
| ------------------------- | ---------------------------- | --------------------------------------------------------------------------- |
| `metadata.name`         | `test-branch-admin`        | 命名规则：`{branch}-admin`                                                |
| `molnett.org/component` | `compute-admin`            | 区别于 Deployment 的`compute` 和 Postgres Service 的 `compute-postgres` |
| `spec.ports[0].port`    | `3080`                     | compute_ctl 的管理端口                                                      |
| `spec.selector.app`     | `test-branch-compute-node` | 将流量路由到对应 Deployment 创建的 Pod                                      |

### 4.4 label 中的空字符串

Fixture 中 `neon.tenant_id: ""` 和 `neon.timeline_id: ""` 是因为测试夹具中 `NewBranch` / `NewProject` 没有填充这些字段。运行时这些值来自 Branch/Project CR 的 spec，会被动态填入。

### 4.5 Label 体系

#### Component Labels 三层区分

| 资源            | component label      |
| --------------- | -------------------- |
| Deployment      | `compute`          |
| AdminService    | `compute-admin`    |
| PostgresService | `compute-postgres` |

#### Endpoint Type Label（关键运行时区分）

Deployment 额外携带 `molnett.org/endpoint-type` label，用于运行时区分 compute 实例的 WAL 角色：

| label 值       | WAL 角色               | `synchronous_standby_names` | 说明                                   |
| :------------- | :--------------------- | :---------------------------- | :------------------------------------- |
| `read_write` | **WAL Proposer** | `walproposer`               | 唯一的可写实例，向 Safekeeper 推送 WAL |
| `read_only`  | **WAL Follower** | 不设置                        | 只读消费 WAL 流                        |

此 label 在 `specs/compute/endpoint_spec.go` 中设置，并在 `specs/compute/spec.go` 的 `GenerateComputeSpec` 中读取，决定 postgresql.conf 的 `synchronous_standby_names` 配置。两个实例若同时设为 WAL proposer 会导致后启动者 PANIC。

这使得可以通过 label selector 单独查询或操作某类资源，例如 `kubectl get deploy -l molnett.org/endpoint-type=read_write`。

---

## 五、设计要点总结

1. **关注点分离**：控制面和数据面走不同的 Service，互不干扰。即使 PostgreSQL 崩溃，compute_ctl 的 3080 端口仍可响应（报告状态、接收重启指令）
2. **独立扩缩容**：虽然当前两个 Service 指向同一个 Pod，但未来可以独立变更（比如 AdminService 加 sidecar proxy 做审计）
3. **安全边界清晰**：AdminService 只暴露给 operator（通过 JWT 认证），PostgresService 暴露给存储层和用户，网络策略可以按 Service 粒度分别配置
4. **工厂模式复用**：`ServiceConfig` + `createService` 将公共逻辑集中管理，两个 Service 仅通过 4 个参数区分，避免代码重复
5. **Golden Test 保障**：任何对 Service 工厂函数的变更都会被 golden test 捕获，确保生成的 K8s 资源不被意外修改

