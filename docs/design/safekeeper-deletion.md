# Safekeeper 删除设计

## 问题概述

当前 operator 中 Safekeeper 的删除流程仅移除了 Kubernetes Finalizer，**未向 Storage Controller 发起注销请求**。这意味着 Storage Controller 中有关该 safekeeper 的注册记录不会被清理。

## 原因分析

### 1. Storage Controller 缺少 DELETE 端点

Neon Storage Controller 中 safekeeper 的 HTTP API 路由如下：

```
GET    /control/v1/safekeeper                      # 列表
GET    /control/v1/safekeeper/{id}                  # 获取单个（仅测试用）
POST   /control/v1/safekeeper/{id}                  # upsert
POST   /control/v1/safekeeper/{id}/scheduling_policy # 设置调度策略
```

**没有 DELETE 端点**，也没有在 upsert 结构中提供 deactive/delete 标志位。直接调用 DELETE 会导致 405 Method Not Allowed 错误。

### 2. 数据迁移问题

Safekeeper 负责存储 WAL 日志，是 Neon 存储架构中的关键组件：

- Safekeeper 上可能存有多个 timeline 的未消费 WAL 数据
- 直接删除 safekeeper 可能导致 WAL 数据丢失
- 需要先确保相关 timeline 的 WAL 已被 pageserver 消费完毕
- 可能需要将剩余 WAL 数据迁移到其他 safekeeper

因此，safekeeper 的删除不应是简单的"从注册表删除"，而是一个需要协调多个组件的复杂流程。

### 3. 现有处理方式的局限

当前实现（`internal/controller/safekeeper_controller.go` 中的 `finalize` 方法）直接移除 Finalizer 而不做任何外部清理：

```go
func (r *SafekeeperReconciler) finalize(ctx context.Context, sk *neonv1alpha1.Safekeeper) (ctrl.Result, error) {
    // ...
    controllerutil.RemoveFinalizer(sk, utils.FinalizerName)
    if err := r.Update(ctx, sk); err != nil {
        return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
    }
    return ctrl.Result{}, nil
}
```

这是一个临时简化方案，存在以下风险：
- Storage Controller 中残留僵尸记录
- 可能在被删除 safekeeper 上有 WAL 数据未被消费

## 潜在方案

### 方案 A：仅标记停用（推荐）

在 Storage Controller 的 POST `/control/v1/safekeeper/{id}` 请求中添加 deactive 机制，将 safekeeper 标记为 inactive 而非直接删除。

**优点**：保留历史记录，支持审计和问题排查。
**需要完成**：
1. Neon 侧在 `SafekeeperUpsert` 结构体中添加 `active` 字段
2. Storage Controller 的调度逻辑需要过滤 inactive 的 safekeeper
3. Operator 在 finalize 时发送 deactive 请求

### 方案 B：添加 DELETE 端点并处理数据迁移

在 Storage Controller 中添加 DELETE 端点，并在删除前检查数据迁移状态。

**需要完成**：
1. Neon 侧添加 DELETE 路由和 handler
2. 实现数据迁移检查逻辑
3. 在 PostgreSQL operator 中调用 DELETE 端点

### 方案 C：保持现状（当前）

不向 Storage Controller 发送任何注销请求，仅移除 Kubernetes 资源。

**建议**：仅作为过渡方案，后续应选择方案 A 或 B。

## 相关文件

| 文件 | 说明 |
|------|------|
| `neon/storage_controller/src/http.rs` | Storage Controller 路由定义，无 DELETE safekeeper 端点 |
| `neon/storage_controller/src/persistence.rs` | SafekeeperUpsert 结构体，无 active/deactive 字段 |
| `neon-operator/internal/controller/safekeeper_controller.go` | Operator 中 safekeeper 的 finalize 逻辑 |

## 决策记录

| 日期 | 决策 | 说明 |
|------|------|------|
| 2025-06 | 暂时跳过 safekeeper 注销 | Storage Controller 缺少 DELETE 端点，且涉及数据迁移问题，先以简化方式处理 |
