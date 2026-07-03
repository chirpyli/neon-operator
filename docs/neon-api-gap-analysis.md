# Neon Cloud API 差距分析

> 对标官方 Neon Cloud API (api-docs.neon.tech)，分析当前实现与官方 API 的差距，按优先级分类。

| 字段 | 内容                                                                  |
| ---- | --------------------------------------------------------------------- |
| 版本 | v2.2                                                                  |
| 日期 | 2026-07-01                                                            |
| 变更 | 同步 Production Readiness Roadmap v1.5：Phase 2 JWT 安全加固阶段性完成（组件间认证链路已全部实施、SC `--dev` 已移除、Token 持久化已完成）；API 端点实现状态无变化，P0 缺口仍为 Branch Restore（PITR）和 Database/Endpoint 级别 connection_uri 端点 |
| 参考 | [官方 Neon API v2](https://api-docs.neon.tech/reference/getting-started) |

---

## 一、总体覆盖率

| 类别           | 官方端点      | 已实现       | 覆盖率         |
| -------------- | ------------- | ------------ | -------------- |
| Project        | 5             | 5            | 100%           |
| Branch         | 6             | 6            | 100%           |
| Endpoint       | 8             | 8            | 100%           |
| Role           | 6             | 6            | 100%           |
| Database       | 6             | 5            | 83%            |
| Operation      | 2             | 2            | 100%           |
| Connection URI | 1             | 1            | 100%           |
| Snapshot       | 6             | 0            | 0%             |
| Organization   | 8             | 0            | 0%             |
| Auth           | 6             | 0            | 0%             |
| Billing        | 8             | 0            | 0%             |
| 其他           | 34            | 0            | 0%             |
| **总计** | **~95** | **34** | **~36%** |

> **Endpoints 路由设计差异**：官方路径为 `.../branches/{branch_id}/endpoints/{endpoint_id}`（嵌套在 branch 下），当前实现使用扁平化路径 `.../endpoints/{endpoint_id}`。功能上已覆盖全部 8 个官方端点，另增 1 个按分支过滤的 `listBranchEndpoints`（`GET .../branches/{branch_id}/endpoints`）。若需与官方 API 客户端兼容，可在后续版本中增加嵌套路径别名。
>
> **Connection URI 实现差异**：当前仅在 Project 级别实现（`GET .../connection_uri`，通过 query params 指定 database/role/branch/endpoint），Endpoint 级别和 Database 级别的 `connection_uri` 子资源端点尚未实现。

---

## 二、已实现端点 vs 缺失端点（按优先级）

### P0 — 必须补齐（影响基本使用）

#### 2.1 Project PATCH（更新）

| 方法      | 路径                      | 说明                    | 状态      |
| --------- | ------------------------- | ----------------------- | --------- |
| `PATCH` | `/api/v2/projects/{id}` | 更新 Project 名称、默认端点配置、历史保留期、IP 白名单（三态语义 Noop/Upsert/Remove） | ✅ 已实现 |

#### 2.2 Branch PATCH + 单条 GET + Restore

| 方法      | 路径                                                    | 说明                                 | 状态 |
| --------- | ------------------------------------------------------- | ------------------------------------ | ---- |
| `PATCH` | `/api/v2/projects/{pid}/branches/{id}`                | 更新 Branch（name、protected 等）    | ✅   |
| `POST`  | `/api/v2/projects/{pid}/branches/{id}/restore`        | 基于 LSN/Timestamp 恢复到指定时间点  | ❌   |
| `POST`  | `/api/v2/projects/{pid}/branches/{id}/set_as_default` | 设置默认分支                         | ✅   |

#### 2.3 Endpoint PATCH + 生命周期操作

| 方法      | 路径                                  | 说明                                   | 状态 |
| --------- | ------------------------------------- | -------------------------------------- | ---- |
| `PATCH` | `.../endpoints/{id}`                | 更新 Endpoint 配置（type、resources、disabled） | ✅ |
| `POST`  | `.../endpoints/{id}/start`          | 启动 Endpoint                          | ✅   |
| `POST`  | `.../endpoints/{id}/suspend`        | 挂起 Endpoint                          | ✅   |
| `POST`  | `.../endpoints/{id}/restart`        | 重启 Endpoint                          | ✅   |
| `GET`   | `.../endpoints/{id}/connection_uri` | 获取连接字符串                         | ❌（Project 级别已实现） |

#### 2.4 Role 单条 GET + PATCH

| 方法      | 路径                 | 说明               | 状态 |
| --------- | -------------------- | ------------------ | ---- |
| `GET`   | `.../roles/{name}` | 获取单个 Role 详情 | ✅   |
| `PATCH` | `.../roles/{name}` | 更新 Role 属性     | ✅   |

#### 2.5 Database 单条 GET + PATCH

| 方法      | 路径                                    | 说明                   | 状态 |
| --------- | --------------------------------------- | ---------------------- | ---- |
| `GET`   | `.../databases/{name}`                | 获取单个 Database 详情 | ✅   |
| `PATCH` | `.../databases/{name}`                | 更新 Database 属性     | ✅   |
| `GET`   | `.../databases/{name}/connection_uri` | 获取连接字符串         | ❌（Project 级别已实现） |

### P1 — 重要但非阻塞

#### 2.6 Branch Import

| 方法     | 路径                         | 说明                          | 状态 |
| -------- | ---------------------------- | ----------------------------- | ---- |
| `POST` | `.../branches/{id}/import` | 从外部 PG 导入数据创建 Branch | ❌   |

#### 2.7 Snapshot API（完整缺失）

| 方法       | 路径                   | 说明         | 状态 |
| ---------- | ---------------------- | ------------ | ---- |
| `POST`   | `.../snapshots`      | 创建快照     | ❌   |
| `GET`    | `.../snapshots`      | 列出快照     | ❌   |
| `GET`    | `.../snapshots/{id}` | 获取快照详情 | ❌   |
| `DELETE` | `.../snapshots/{id}` | 删除快照     | ❌   |

#### 2.8 Endpoint Connection URI

| 方法    | 路径                                  | 说明                   | 状态 |
| ------- | ------------------------------------- | ---------------------- | ---- |
| `GET` | `.../endpoints/{id}/connection_uri` | 生成带认证的连接字符串 | ❌   |

### P2 — 组织/认证/计费（多租户生产环境必需）

#### 2.9 Organization API（完整缺失）

| 方法       | 路径                           | 说明     | 状态 |
| ---------- | ------------------------------ | -------- | ---- |
| `GET`    | `/api/v2/organizations`      | 列出组织 | ❌   |
| `POST`   | `/api/v2/organizations`      | 创建组织 | ❌   |
| `GET`    | `/api/v2/organizations/{id}` | 获取组织 | ❌   |
| `PATCH`  | `/api/v2/organizations/{id}` | 更新组织 | ❌   |
| `DELETE` | `/api/v2/organizations/{id}` | 删除组织 | ❌   |
| `GET`    | `.../members`                | 列出成员 | ❌   |

#### 2.10 Auth API（完整缺失）

| 方法     | 路径                      | 说明         | 状态 |
| -------- | ------------------------- | ------------ | ---- |
| `POST` | `/api/v2/auth/login`    | 登录         | ❌   |
| `POST` | `/api/v2/auth/refresh`  | 刷新 Token   | ❌   |
| `POST` | `/api/v2/auth/logout`   | 登出         | ❌   |
| `GET`  | `/api/v2/auth/me`       | 获取当前用户 | ❌   |
| `POST` | `/api/v2/auth/api_keys` | 创建 API Key | ❌   |

#### 2.11 Billing API（完整缺失）

| 说明                           | 状态          |
| ------------------------------ | ------------- |
| 用量统计、计费、发票等 8+ 端点 | ❌ 全部未实现 |

---

## 三、优先级总结

| 优先级       | 端点数 | 说明                                                                                                            |
| ------------ | ------ | --------------------------------------------------------------------------------------------------------------- |
| **P0** | ~3     | 少量待补齐：Branch Restore、Endpoint connection_uri、Database connection_uri（Connection URI 已在 Project 级别实现，待补充子资源端点） |
| **P1** | ~10    | 重要：Branch Import、Snapshot API                                                                               |
| **P2** | ~20+   | 生产必需：Organization、Auth、Billing                                                                           |
| **P3** | ~26    | 长尾：Analytics、Usage Events、IP Allow、各种子资源                                                             |

---

## 四、实施建议

```
Phase 1 (P0): 补齐剩余 CRUD
├── Project PATCH ✅（已完成，支持 name/default_endpoint_settings/history_retention_seconds/ip_allow）
├── Branch PATCH ✅ + set_as_default ✅
├── Endpoint PATCH ✅ + start ✅ / suspend ✅ / restart ✅
├── Role GET single ✅ + PATCH ✅
├── Database GET single ✅ + PATCH ✅
├── Connection URI ✅（Project 级别已实现，Database/Endpoint 级别待补充子资源端点）
└── Branch Restore (PITR) 待实现

Phase 2 (P1): 核心功能
├── Endpoint/Database connection_uri 子资源端点
├── Branch Import (pg_dump/pg_restore)
├── Snapshot API (创建/列表/删除)
└── Compute Scale (CU 变更)

Phase 3 (P2): 多租户安全
├── Organization API
├── Auth (JWT + API Key)
└── RBAC (role-based access control)

Phase 4 (P2/P3): 计费和长尾
├── Billing API
├── Usage Events
├── IP Allow（已在 Project PATCH 中部分实现，待独立端点化）
└── 其余端点
```
