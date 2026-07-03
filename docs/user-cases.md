# 私有化部署Neon

需要的资源：
- S3，可用minio，要求高可靠
- 高可用的PostgreSQL
- k8s集群， 至少3节点，要求高可靠，etcd不能单点，至少3节点etcd集群 
- 节点需要有SSD本地盘（建议NVMe，普通磁盘则性能差一些，正确性不受影响）


使用流程：
1. 安装部署neon-operator
2. 创建Neon集群，参考配置如下：
```yaml
---
# 外部 MinIO S3 凭证（运行在 slpc:9000，非 K8s 内部）
apiVersion: v1
kind: Secret
metadata:
  name: bucket-credentials
  namespace: neon
stringData:
  AWS_ACCESS_KEY_ID: "minioadmin"
  AWS_ENDPOINT_URL: "http://192.168.232.128:9000"
  AWS_REGION: "us-east-1"
  AWS_SECRET_ACCESS_KEY: "minioadmin"
  BUCKET_NAME: "neondata"

---
# 依赖的PostgreSQL数据库
apiVersion: v1
kind: Secret
metadata:
  name: storage-controller-pg-cluster
  namespace: neon
type: Opaque
stringData:
  uri: "postgres://storage_controller:storage_controller@192.168.232.128:5432/storage_controller"

---
# Neon cluster集群
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: my-cluster
  namespace: neon
spec:
  numSafekeepers: 3                       # Safekeeper 数量，最少 3 个（共识要求）
  numPageservers: 2                        # Pageserver 数量，默认 1（开发测试），生产 ≥ 2
  defaultPGVersion: 17                     # 默认 PostgreSQL 版本：14/15/16/17
  neonImage: "ghcr.io/neondatabase/neon:latest"      # Neon 组件镜像（外网；如不可用，改用本地 registry 路径）
  bucketCredentialsSecret:
    name: bucket-credentials               # 引用上面定义的 Secret → 外部 MinIO
  storageControllerDatabaseSecret:
    name: storage-controller-pg-cluster
    key: uri
  defaultPageserverConfig:                 # 自动创建的 Pageserver 默认配置
    storageSize: 10Gi                      # PVC 大小
    initialSchedulingPolicy: Active        # 新节点调度策略：Active | Filling
    resources:                             # CPU/内存配置（可选，不设则使用 operator 内置默认值）
      requests:
        cpu: "1"
        memory: 256Mi
      limits:
        cpu: "2"
        memory: 1Gi
    nodeFailure:                           # 节点故障自动恢复策略（可选）
      autoRecover: true                   # 是否自动删除 PVC 重建（默认 false）
      maxPendingDuration: 5m              # Pod Pending 判定阈值（默认 5m）
  defaultSafekeeperStorage:
    size: 8Gi
  defaultSafekeeperConfig:                 # 自动创建的 Safekeeper 默认配置
    nodeFailure:                           # 节点故障自动恢复策略（可选）
      autoRecover: true                    # 是否自动删除 PVC 重建（默认 false）
      maxPendingDuration: 5m               # Pod Pending/Terminating 判定阈值（默认 5m）
  postgresExposure:    # 指定 计算节点Postgres 服务的暴露方式， 默认 ClusterIP， 因为没有proxy才需要进行设置，有proxy时，计算节点不对外进行暴露
    type: NodePort

```



3. 用户通过API创建Project/Branch/Endpoint
参考示例：
```sh
curl -s -X POST "http://localhost:8081/api/v2/projects"   -H "Content-Type: application/json"   -d '{
    "project": {
      "name": "myapp",     # 租户名称
      "pgVersion": 17,      # PostgreSQL 版本：14/15/16/17
      "cluster": "my-cluster",   # Neon cluster集群名称
      "branch": {
        "name": "main",     # 分支名称
        "role_name": "appuser",      
        "database_name": "neondb"
      },
      "endpoint": {
        "type": "read_write",    
        "resources": { "cpu": "500m", "memory": "512Mi" }
      },
      "default_endpoint_settings": {
        "resources": { "cpu": "500m", "memory": "512Mi" }
      }
    }
  }'
```
4. 连接数据库

# 用户使用参考






设置端口转发

```sh
kubectl port-forward -n neon svc/neon-controlplane 8081:8081
Forwarding from 127.0.0.1:8081 -> 8082
Forwarding from [::1]:8081 -> 8082
```

## Projects

### 查看当前Project(租户)情况

```sh
curl -s "http://localhost:8081/api/v2/projects" | python3 -m json.tool
{
    "projects": [],
    "pagination": {
        "has_more": false
    }
}
```

### 创建一个Project(租户)：

```sh
curl -s -X POST "http://localhost:8081/api/v2/projects"   -H "Content-Type: application/json"   -d '{
    "project": {
      "name": "myapp",
      "pgVersion": 17,
      "cluster": "my-cluster",
      "branch": {
        "name": "main",
        "role_name": "appuser",
        "database_name": "neondb"
      },
      "endpoint": {
        "type": "read_write",
        "resources": { "cpu": "500m", "memory": "512Mi" }
      },
      "default_endpoint_settings": {
        "resources": { "cpu": "500m", "memory": "512Mi" }
      }
    }
  }' | python3 -m json.tool
{
    "project": {
        "id": "project-663fd0da",
        "name": "myapp",
        "pgVersion": 17,
        "cluster": "my-cluster",
        "tenant_id": "f4763905597763d7c9de9dc76e4e119c",
        "created_at": "2026-07-01T03:15:35.946126404Z",
        "updated_at": "2026-07-01T03:15:35.946126404Z",
        "default_endpoint_settings": {
            "resources": {
                "cpu": "500m",
                "memory": "512Mi"
            }
        }
    },
    "branch": {
        "id": "br-7dd8ad42",
        "name": "main",
        "project_id": "project-663fd0da",
        "current_state": "init",
        "default": true,
        "init_source": "parent-data",
        "created_at": "2026-07-01T03:15:35.946126404Z",
        "updated_at": "0001-01-01T00:00:00Z"
    },
    "endpoints": [
        {
            "id": "ep-6a29723a",
            "branch_id": "br-7dd8ad42",
            "type": "read_write",
            "current_state": "init",
            "created_at": "2026-07-01T03:15:35.946126404Z"
        }
    ],
    "connection_uris": null,
    "roles": [
        {
            "name": "appuser",
            "password": "tC2eMMnFDSpiHqYd0XWuEsvf2adYRBVL",
            "protected": true,
            "branch_id": "br-7dd8ad42",
            "authentication_method": "password",
            "created_at": "2026-07-01T03:15:35.946126404Z"
        }
    ],
    "databases": [
        {
            "id": 1,
            "name": "neondb",
            "owner_name": "appuser",
            "branch_id": "br-7dd8ad42",
            "created_at": "2026-07-01T03:15:35.946126404Z"
        }
    ],
    "operations": [
        {
            "id": "op-ee24b638-193d-4016-a3db-ebf23222ffab",
            "project_id": "project-663fd0da",
            "branch_id": "br-7dd8ad42",
            "endpoint_id": "ep-6a29723a",
            "action": "create_project",
            "status": "scheduling",
            "created_at": "2026-07-01T03:15:35.946126404Z"
        }
    ]
}
```

### 查看Projects:

```sh
curl -s "http://localhost:8081/api/v2/projects" | python3 -m json.tool
{
    "projects": [
        {
            "id": "project-0c6bd918",
            "name": "myapp",
            "pgVersion": 17,
            "cluster": "my-cluster",
            "tenant_id": "5b26ae6af932733752c5cf7f2bded0fb",
            "created_at": "2026-07-01T01:57:07Z",
            "updated_at": "0001-01-01T00:00:00Z"
        }
    ],
    "pagination": {
        "has_more": false
    }
}
```

### 查看某一Projects:

```sh
curl -s "http://localhost:8081/api/v2/projects/project-0c6bd918" | python3 -m json.tool
{
    "project": {
        "id": "project-0c6bd918",
        "name": "myapp",
        "pgVersion": 17,
        "cluster": "my-cluster",
        "tenant_id": "5b26ae6af932733752c5cf7f2bded0fb",
        "created_at": "2026-07-01T01:57:07Z",
        "updated_at": "2026-07-01T02:02:38.526578719Z",
        "default_endpoint_settings": {
            "resources": {
                "cpu": "500m",
                "memory": "512Mi"
            }
        },
        "history_retention_seconds": 604800
    }
}
```

### 删除Project:

```sh
curl -s -X DELETE "http://localhost:8081/api/v2/projects/project-0c6bd918" | python3 -m json.tool
{
    "project":{
        "id":"project-0c6bd918",
        "name":"myapp",
        "pgVersion":0,
        "created_at":"0001-01-01T00:00:00Z",
        "updated_at":"0001-01-01T00:00:00Z"},
        "operations":[{"id":"op-0d929395-87c1-4671-ac41-4d47071e9c71","project_id":"project-0c6bd918",
        "action":"delete_project",
        "status":"scheduling",
        "created_at":"2026-07-01T02:07:43.747573649Z"}]
}
```

## Branches

### 查看某一Projects的Branches:
```sh
curl -s "http://localhost:8081/api/v2/projects/project-c10d8182/branches" | python3 -m json.tool
{
    "branches": [
        {
            "id": "br-dee8d418",
            "name": "main",
            "project_id": "project-c10d8182",
            "current_state": "ready",
            "default": true,
            "init_source": "parent-data",
            "created_at": "2026-07-03T02:55:42Z",
            "updated_at": "0001-01-01T00:00:00Z"
        },
        {
            "id": "br-c912d116",
            "name": "develop",
            "project_id": "project-c10d8182",
            "parent_id": "br-dee8d418",
            "current_state": "ready",
            "init_source": "parent-data",
            "created_at": "2026-07-03T03:13:10Z",
            "updated_at": "0001-01-01T00:00:00Z"
        }
    ]
}
```

### 获取某个分支的详情
```sh
curl -s "http://localhost:8081/api/v2/projects/project-39fd73f6/branches/br-061cfd1c" | python3 -m json.tool
{
    "branch": {
        "id": "br-061cfd1c",
        "name": "main",
        "project_id": "project-39fd73f6",
        "current_state": "ready",
        "default": true,
        "init_source": "parent-data",
        "created_at": "2026-07-01T05:32:01Z",
        "updated_at": "0001-01-01T00:00:00Z"
    }
}
```

### 创建分支
```sh
curl -s -X POST "http://localhost:8081/api/v2/projects/project-c10d8182/branches" \
  -H "Content-Type: application/json" \
  -d '{
    "branch": {
      "name": "develop",
      "init_source": "parent-data"
    },
    "endpoints": [{
      "type": "read_write",
      "resources": { "cpu": "250m", "memory": "256Mi" }
    }]
  }' | python3 -m json.tool
{
    "branch": {
        "id": "br-c912d116",
        "name": "develop",
        "project_id": "project-c10d8182",
        "parent_id": "br-dee8d418",
        "current_state": "init",
        "init_source": "parent-data",
        "created_at": "2026-07-03T03:13:10.661840804Z",
        "updated_at": "0001-01-01T00:00:00Z"
    },
    "endpoints": [],
    "operations": [
        {
            "id": "op-ffa65e0d-4a88-4c65-9c9a-6a0279c8609d",
            "project_id": "project-c10d8182",
            "branch_id": "br-c912d116",
            "action": "create_branch",
            "status": "scheduling",
            "created_at": "2026-07-03T03:13:10.661840804Z"
        }
    ]
}
```

## Endpoints

### 列出指定分支的Endpoints
```sh
curl -s "http://localhost:8081/api/v2/projects/project-39fd73f6/branches/br-061cfd1c/endpoints" | python3 -m json.tool
{
    "endpoints": [
        {
            "id": "ep-5b7e9ca2",
            "branch_id": "br-061cfd1c",
            "type": "read_write",
            "port": 55433,
            "current_state": "active",
            "created_at": "2026-07-01T05:32:01Z"
        }
    ]
}
```

### 获取单个Endpoint详情

```sh
curl -s "http://localhost:8081/api/v2/projects/project-39fd73f6/endpoints/ep-5b7e9ca2" | python3 -m json.tool
{
    "endpoint": {
        "id": "ep-5b7e9ca2",
        "branch_id": "br-061cfd1c",
        "type": "read_write",
        "port": 55433,
        "current_state": "active",
        "created_at": "2026-07-01T05:32:01Z"
    }
}
```

### 删除Endpoint
```sh
curl -s -X DELETE  "http://localhost:8081/api/v2/projects/project-c10d8182/endpoints/ep-894d5fde" | python3 -m json.tool
{
    "endpoint": {
        "id": "ep-894d5fde",
        "branch_id": "",
        "type": "read_write",
        "created_at": "0001-01-01T00:00:00Z"
    },
    "operations": [
        {
            "id": "op-01c7e117-08c5-4e1d-89c2-6da6d368b309",
            "project_id": "project-c10d8182",
            "endpoint_id": "ep-894d5fde",
            "action": "delete_endpoint",
            "status": "scheduling",
            "created_at": "2026-07-03T06:00:09.433052479Z"
        }
    ]
}
```

### 创建只读Endpoint

```sh
curl -s -X POST "http://localhost:8081/api/v2/projects/project-ab6edcd8/endpoints" \
  -H "Content-Type: application/json" \
  -d "{
    \"endpoint\": {
      \"branch_id\": \"br-c6720c39\",
      \"type\": \"read_only\"
    }
  }" | python3 -m json.tool
{
    "endpoint": {
        "id": "ep-a5928d59",
        "branch_id": "br-dee8d418",
        "type": "read_only",
        "current_state": "init",
        "created_at": "2026-07-03T04:43:42.178646128Z"
    },
    "operations": [
        {
            "id": "op-6e4dbe91-e919-4959-acde-ed7292d9f09a",
            "project_id": "project-c10d8182",
            "branch_id": "br-dee8d418",
            "endpoint_id": "ep-a5928d59",
            "action": "start_compute",
            "status": "scheduling",
            "created_at": "2026-07-03T04:43:42.178619528Z"
        }
    ]
}
```


## Roles

### 列出Roles

```sh
curl -s "http://localhost:8081/api/v2/projects/project-663fd0da/branches/br-7dd8ad42/roles" | python3 -m json.tool
{
    "roles": [
        {
            "name": "appuser",
            "branch_id": "br-7dd8ad42",
            "authentication_method": "password",
            "created_at": "2026-07-01T03:15:35Z"
        }
    ]
}
```

## Databases

### 列出Databases

```sh
curl -s "http://localhost:8081/api/v2/projects/project-663fd0da/branches/br-7dd8ad42/databases" | python3 -m json.tool
{
    "databases": [
        {
            "id": 1,
            "name": "neondb",
            "owner_name": "appuser",
            "branch_id": "br-7dd8ad42",
            "created_at": "2026-07-01T03:15:35Z"
        }
    ]
}
```

---

## 内部状态

2个pageserver的情况下，2个租户分别在2个pageserver上
```sql
storage_controller=> select * from nodes ;
 node_id | scheduling_policy |                                 listen_http_addr                                  | listen_http_port |                                  list
en_pg_addr                                   | listen_pg_port | availability_zone_id | listen_https_port | lifecycle | listen_grpc_addr | listen_grpc_port 
---------+-------------------+-----------------------------------------------------------------------------------+------------------+--------------------------------------
---------------------------------------------+----------------+----------------------+-------------------+-----------+------------------+------------------
       1 | active            | my-cluster-pageserver-1-0.my-cluster-pageserver-1-headless.neon.svc.cluster.local |             9898 | my-cluster-pageserver-1-0.my-cluster-
pageserver-1-headless.neon.svc.cluster.local |           6400 | se-ume               |                   | active    |                  |                 
       2 | active            | my-cluster-pageserver-2-0.my-cluster-pageserver-2-headless.neon.svc.cluster.local |             9898 | my-cluster-pageserver-2-0.my-cluster-
pageserver-2-headless.neon.svc.cluster.local |           6400 | se-ume               |                   | active    |                  |                 
(2 rows)

storage_controller=> select * from tenant_shards ;
            tenant_id             | shard_number | shard_count | shard_stripe_size | generation | generation_pageserver | placement_policy | splitting | config | schedulin
g_policy | preferred_az_id 
----------------------------------+--------------+-------------+-------------------+------------+-----------------------+------------------+-----------+--------+----------
---------+-----------------
 ecdfa3bcf172a2676fe8ce4953d274c6 |            0 |           0 |              2048 |          2 |                     1 | {"Attached":1}   |         0 | {}     | "Active" 
         | se-ume
 e9f70316b31aeb93f22d4039ceeeb33e |            0 |           0 |              2048 |          2 |                     2 | {"Attached":1}   |         0 | {}     | "Active" 
         | se-ume
(2 rows)
```