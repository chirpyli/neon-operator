# AGENT.md

本文档为在此仓库中编写代码提供指引。

## 项目概述

这是一个用于管理 Neon 数据库集群的 Kubernetes Operator，使用 Go 语言编写。该 Operator 使用 controller-runtime 库，遵循 Kubernetes Controller 模式，并使用自定义资源定义（CRD）。

## 架构

项目遵循标准的 Go 项目结构：

- **`cmd/controller/`**：主 Controller 二进制，运行 Operator 并提供 HTTP 健康检查/指标接口
- **`cmd/controlplane/`**：控制平面二进制，用于管理存储组件
- **`internal/controller/`**：核心 Controller 及调谐逻辑
- **`internal/controlplane/`**：控制平面工具与服务
- **`api/`**：API 定义及 CRD 结构体

### Controller

该 Operator 并发运行多个 Controller：
- **Cluster Controller**：管理 NeonCluster 资源及整体集群状态
- **Project Controller**：处理 Neon 项目生命周期
- **Branch Controller**：管理项目内的数据库分支

Controller 位于 `internal/controller/` 目录下，每个 Controller 都实现了 controller-runtime 的 reconciler 模式。

## 开发命令

### 构建与运行
```bash
# 构建 Operator 的 Docker 镜像
make docker-build

# 在本地集群中运行 Operator
make run
```

### 测试
```bash
# 运行单元测试
make test

# 运行集成测试（需要已安装 CRD）
make test-e2e
```

### CRD 管理
```bash
# 从 Go 代码生成 CRD
make generate

# 将 CRD 安装到集群
make install
```

### 代码质量
```bash
# 使用 Go fmt 格式化代码
make fmt
```

## 外部依赖

**重要提示**：该 Operator 当前要求主 Neon 仓库克隆到本仓库的相邻目录下，构建才能正常进行。Neon 仓库需要至少执行一次 `make`。

目录结构应为：
```
parent-directory/
├── neon/             # 主 Neon 仓库
└── neon-operator/    # 本仓库
```

## 运行时配置

Operator 在 8080 端口上暴露 HTTP 接口：
- `/health` — 健康检查端点
- `/metrics` — Prometheus 指标
- `/` — 诊断信息

## neon源码
获取neon相关的内容，可通过源码进行分析：
- neon github仓库为：https://github.com/neondatabase/neon.git
- neon 本地源码为：/home/postgres/works/opensource/neon