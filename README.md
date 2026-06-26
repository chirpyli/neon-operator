# Neon Kubernetes Operator

一个用于管理自托管 [Neon](https://neon.com) Postgres 数据库集群的 Kubernetes Operator。通过该 Operator，您可以在 Kubernetes 上（云端和本地部署）管理 Neon 控制平面所需的所有组件。

*本产品与 Neon 不存在任何关联，亦未以任何方式获得 Neon 的背书。*

## 项目状态

本 Operator 可用于开发和测试环境。它实现了 Neon 的核心架构组件，并提供基本的集群管理能力。目前我们正在推进 Day 1 和 Day 2 运维工作，因此性能尚未优化。

### 与托管 Neon 的差异

与完整托管版 Neon 服务相比，当前自托管 Operator 存在以下限制：

- **不支持计算自动伸缩（Compute Auto-scaling）**：计算实例持续运行，不会缩容至零
- **手动租户分片（Tenant Sharding）**：租户分片需手动配置或由特定条件触发
- **性能优化**：Day 2 运维和性能调优仍在开发中
- **功能完备性**：托管版 Neon 中的部分高级功能尚未实现

### 已实现的功能

- **Neon 架构组件**：Pageservers、Safekeepers、Storage Broker 和 Storage Controller
- **基本分支（Branching）**：在项目（Project）内创建新的数据库分支
- **持久化存储**：为 Pageserver 和 Safekeeper 提供可配置的存储
- **端到端测试**：用于验证 Operator 功能的端到端测试套件

### 架构

本 Operator 实现了 Neon 计算与存储分离的架构：

- **Pageservers**：处理来自缓存和对象存储的读取请求
- **Safekeepers**：提供共识机制和 WAL 持久性保证
- **Storage Broker**：协调存储操作
- **Storage Controller**：管理存储集群状态
- **Compute Nodes（计算节点）**：连接存储层的 PostgreSQL 实例

每个组件均以 Kubernetes 工作负载形式运行，并具备持久化存储和服务发现能力。

### 未来计划

#### 功能重构
- **Notify Hooks**：完全支持 notify-attach 钩子，用于重新配置 Compute 以与其他 Pageserver 通信

#### Day 2 运维
- [] Pageserver 退役或故障时自动排空（#21）
- [] 删除对象时清理关联的租户和 Timeline（#10）

#### 性能
- [] PGBouncer 支持

## 兼容性

本 Operator 已在以下环境中测试通过：
- **Neon 组件**：Release 9129（始终支持最新版 Compute）
- **Kubernetes**：1.28+
- **存储**：需要兼容 S3 的对象存储

## 前置条件

### 必要依赖

- Go 工具链（1.21 及以上）
- Kubernetes 集群（1.28+）
- 已针对集群配置的 kubectl
- make 命令运行器
- [Tilt](https://tilt.dev/)（可选，用于本地开发）
- Docker（用于构建镜像）

### 存储要求

- **对象存储**：兼容 S3 的存储（如 AWS S3、Rook/Ceph、MinIO）
- **持久化卷**：建议使用支持 NVMe 的 PVC 以获得最佳性能
  - 使用标准存储亦可，但性能将显著下降
  - 需要支持 512 字节扇区大小
- **数据库**：用于 Storage Controller 的 PostgreSQL 实例（可使用任意服务商/CNPG）

## 开发

建议使用单用途 Kind 集群进行本地开发。

### 基于 Tilt 的本地开发

适合开发过程中的快速迭代：

```bash
# 启动 Tilt（检测到变更时自动重新构建和部署）
tilt up

# 查看 Tilt UI 界面
tilt up --web
```

### 手动开发

```bash
# 安装 CRD
make install
```

## 测试

### 单元测试
```bash
make test
```

### 端到端测试
```bash
# 运行完整端到端测试套件（构建镜像并测试集群生命周期）
make test-e2e

# 清理残留的测试集群
make cleanup-test-e2e
```

## 构建

```bash
# 构建 manager 二进制文件
make build

# 构建 Docker 镜像
make docker-build
```

## 使用方式

### 安装 Operator

1. 生成并应用 CRD：
```bash
make install
```

2. 部署 Operator：
```bash
make deploy
```

### 部署流程

创建资源的正确顺序如下：

1. **安装 Operator**：部署 Operator 和 CRD
2. **创建集群（Cluster）**：部署 NeonCluster 资源，等待所有组件就绪
3. **创建项目（Project）**：集群就绪后，创建 NeonProject 资源
4. **创建分支（Branch）**：在项目内创建 NeonBranch 资源

**重要提示**：必须先确保整个集群可用，然后才能创建项目和分支。请在继续创建依赖资源之前监控集群状态。

### 创建 Neon 集群

```yaml
apiVersion: oltp.molnett.org/v1alpha1
kind: NeonCluster
metadata:
  name: my-neon-cluster
spec:
  storage:
    pageserver:
      storageClass: "fast-ssd"
      size: "10Gi"
    safekeeper:
      storageClass: "fast-ssd"
      size: "5Gi"
```

### 创建项目

```yaml
kind: NeonProject
apiVersion: oltp.molnett.org/v1
metadata:
  name: molnett-project
spec:
  cluster_name: basic-cluster
  id: neon-project
  name: neon-project
  pg_version: "PG17"
```

### 创建分支

```yaml
kind: NeonBranch
apiVersion: oltp.molnett.org/v1
metadata:
  name: neon-main
spec:
  name: main
  pg_version: "PG17"
  default_branch: true
  project_id: neon-project
```

## 监控

Operator 在 8080 端口暴露以下 HTTP 端点：
- `/health` - 健康检查端点
- `/metrics` - Prometheus 指标（基础）
- `/` - 诊断信息

## 贡献

欢迎贡献！请参阅 [CONTRIBUTING.md](CONTRIBUTING.md) 文件了解如何参与贡献的详细信息。

## 许可证

Apache License 2.0 - 详见 [LICENSE](LICENSE) 文件。
