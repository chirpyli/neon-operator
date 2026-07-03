# neon 部署

## 一、创建neon namespace
```sh
(base) postgres@slpc:~$ kubectl create namespace neon
namespace/neon created
```

```yaml
# neon-namespace.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: neon
```

## 二、 准备Local PV
需要事先为neon、cloudnative-pg、minio提供，local PV 。有2种方式：
- 为节点创建local PV
- 使用local-path-provisioner

创建StorageClass
可参考文档： https://www.cnblogs.com/johnnyzen/p/19837960

其他方案：
- Longhorn
- OpenEBS(Local PV)


> 其中，cloudnative-pg可以使用云盘

这里采用`local-path-provisioner`的方式。
```sh
# 安装local-path-provisioner
(base) postgres@slpc:~$ kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.36/deploy/local-path-storage.yaml
namespace/local-path-storage created
serviceaccount/local-path-provisioner-service-account created
role.rbac.authorization.k8s.io/local-path-provisioner-role created
clusterrole.rbac.authorization.k8s.io/local-path-provisioner-role created
rolebinding.rbac.authorization.k8s.io/local-path-provisioner-bind created
clusterrolebinding.rbac.authorization.k8s.io/local-path-provisioner-bind created
deployment.apps/local-path-provisioner created
storageclass.storage.k8s.io/local-path created
configmap/local-path-config created
(base) postgres@slpc:~$ kubectl get storageclass
NAME         PROVISIONER             RECLAIMPOLICY   VOLUMEBINDINGMODE      ALLOWVOLUMEEXPANSION   AGE
local-path   rancher.io/local-path   Delete          WaitForFirstConsumer   false                  18s
# 设为默认 StorageClass
(base) postgres@slpc:~$ kubectl patch storageclass local-path -p '{"metadata": {"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
storageclass.storage.k8s.io/local-path patched
(base) postgres@slpc:~$ kubectl get storageclass
NAME                   PROVISIONER             RECLAIMPOLICY   VOLUMEBINDINGMODE      ALLOWVOLUMEEXPANSION   AGE
local-path (default)   rancher.io/local-path   Delete          WaitForFirstConsumer   false                  3m

```

## 三、 安装部署PostgreSQL数据库
安装cloudnative-pg operator
```sh
# 安装 CNPG Operator（会创建所有需要的 CRD，包括 Cluster）
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.29/releases/cnpg-1.29.1.yaml

# 确认 CRD 已安装
(base) postgres@slpc$ kubectl get crd 
NAME                                      CREATED AT
backups.postgresql.cnpg.io                2026-06-18T03:44:00Z
clusterimagecatalogs.postgresql.cnpg.io   2026-06-18T03:44:00Z
clusters.postgresql.cnpg.io               2026-06-18T03:44:00Z
databases.postgresql.cnpg.io              2026-06-18T03:44:00Z
failoverquorums.postgresql.cnpg.io        2026-06-18T03:44:00Z
imagecatalogs.postgresql.cnpg.io          2026-06-18T03:44:00Z
poolers.postgresql.cnpg.io                2026-06-18T03:44:00Z
publications.postgresql.cnpg.io           2026-06-18T03:44:00Z
scheduledbackups.postgresql.cnpg.io       2026-06-18T03:44:00Z
subscriptions.postgresql.cnpg.io          2026-06-18T03:44:00Z

# 确认 Operator Pod 正在运行
(base) postgres@slpc$ kubectl get pods -n cnpg-system
NAME                                       READY   STATUS    RESTARTS   AGE
cnpg-controller-manager-6949985b66-qtshf   1/1     Running   0          2m17s

```

部署PostgreSQL，需要创建数据库，用户，密码
```yaml
# cnpg.yml
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: storage-controller-pg-cluster
  namespace: neon
spec:
  instances: 2
  storage:
    size: 1Gi
  bootstrap:
    initdb:
      database: storage_controller
      owner: storage_controller
      secret:
        name: storage-controller-user

---
apiVersion: v1
kind: Secret
metadata:
  name: storage-controller-user
  namespace: neon
data:
  username: c3RvcmFnZV9jb250cm9sbGVy
  password: c3RvcmFnZV9jb250cm9sbGVy
type: kubernetes.io/basic-auth
```

```sh
postgres@slpc:~/works/opensource/cloudnative-pg$ echo -n 'storage_controller' | base64
c3RvcmFnZV9jb250cm9sbGVy

postgres@slpc:~/works/opensource/neon-operator/docs$ kubectl apply -f cnpg.yml 
cluster.postgresql.cnpg.io/storage-controller-pg-cluster created
secret/storage-controller-user created
```

也可以本地部署PostgreSQL：
```sql
-- 1. 创建数据库用户，设置密码
CREATE ROLE storage_controller WITH LOGIN PASSWORD 'storage_controller';

-- 2. 创建数据库，指定所有者为该用户
CREATE DATABASE storage_controller OWNER storage_controller;

-- 3. 授予该用户对数据库的所有权限
GRANT ALL PRIVILEGES ON DATABASE storage_controller TO storage_controller;

-- 4. 退出
-- psql -U storage_controller -h 192.168.232.128 -d storage_controller
```

## 四、 部署S3
使用minio。
或直接提供S3 ，创建bucket

```sh
(base) postgres@slpc:~/works/opensource$ kubectl kustomize github.com/minio/operator\?ref=v7.1.1 | kubectl apply -f -
namespace/minio-operator created
customresourcedefinition.apiextensions.k8s.io/policybindings.sts.min.io created
customresourcedefinition.apiextensions.k8s.io/tenants.minio.min.io created
serviceaccount/minio-operator created
clusterrole.rbac.authorization.k8s.io/minio-operator-role created
clusterrolebinding.rbac.authorization.k8s.io/minio-operator-binding created
service/operator created
service/sts created
deployment.apps/minio-operator created
```

部署minio:
```yaml
# minio-tenant.yaml
apiVersion: minio.min.io/v2
kind: Tenant
metadata:
  name: minio
  namespace: neon
spec:
  pools:
    - name: pool-0
      servers: 1          # 开发测试用 1 个 server
      volumesPerServer: 1 # 1 个磁盘
      volumeClaimTemplate:
        spec:
          accessModes:
            - ReadWriteOnce
          resources:
            requests:
              storage: 8Gi
  requestAutoCert: false   # 开发环境关闭 TLS
  features:
    enableSFTP: false

```

minio-tenant.yaml 执行apply之后，创建用户密码
```sh
kubectl -n neon create secret generic minio-configuration \
  --from-literal=config.env="export MINIO_ROOT_USER=minioadmin
export MINIO_ROOT_PASSWORD=minioadmin123"

```

更新
```yaml
apiVersion: minio.min.io/v2
kind: Tenant
metadata:
  name: minio
  namespace: neon
spec:
  pools:
    - name: pool-0
      servers: 1          # 开发测试用 1 个 server
      volumesPerServer: 1 # 1 个磁盘
      volumeClaimTemplate:
        spec:
          accessModes:
            - ReadWriteOnce
          resources:
            requests:
              storage: 8Gi
  users:
    - name: minio-credentials
  requestAutoCert: false   # 开发环境关闭 TLS
```
执行`kubectl apply -f docs/minio-tenant.yaml`

创建bucket
```sh
kubectl -n neon exec minio-pool-0-0 --container minio -- mc alias set local http://localhost:9000 minioadmin minioadmin123 && kubectl -n neon exec minio-pool-0-0 --container minio -- mc mb -p local/neon-data
```

S3部署完成：
Service:  minio-hl.neon.svc.cluster.local:9000
User:     minioadmin
Password: minioadmin123
Bucket:   neon-data

创建S3为Neon使用的Secret
```sh
kubectl -n neon create secret generic bucket-credentials \
  --from-literal=AWS_ACCESS_KEY_ID=minioadmin \
  --from-literal=AWS_SECRET_ACCESS_KEY=minioadmin123 \
  --from-literal=AWS_REGION=us-east-1 \
  --from-literal=BUCKET_NAME=neon-data \
  --from-literal=AWS_ENDPOINT_URL=http://minio-hl.neon.svc:9000
```


不使用k8s中的minio，直接部署minio。
```sh
(base) postgres@slpc:~$ minio server miniodata/ --console-address :9001
Formatting 1st pool, 1 set(s), 1 drives per set.
WARNING: Host local has more than 0 drives of set. A host failure will result in data becoming unavailable.
WARNING: Detected default credentials 'minioadmin:minioadmin', we recommend that you change these values with 'MINIO_ROOT_USER' and 'MINIO_ROOT_PASSWORD' environment variables
MinIO Object Storage Server
Copyright: 2015-2023 MinIO, Inc.
License: GNU AGPLv3 <https://www.gnu.org/licenses/agpl-3.0.html>
Version: RELEASE.2023-11-11T08-14-41Z (go1.21.4 linux/amd64)

Status:         1 Online, 0 Offline. 
S3-API: http://192.168.232.128:9000  http://172.17.0.1:9000  http://10.244.0.0:9000  http://127.0.0.1:9000           
RootUser: minioadmin 
RootPass: minioadmin 

Console: http://192.168.232.128:9001 http://172.17.0.1:9001 http://10.244.0.0:9001 http://127.0.0.1:9001      
RootUser: minioadmin 
RootPass: minioadmin 

Command-line: https://min.io/docs/minio/linux/reference/minio-mc.html#quickstart
   $ mc alias set 'myminio' 'http://192.168.232.128:9000' 'minioadmin' 'minioadmin'

Documentation: https://min.io/docs/minio/linux/index.html
Warning: The standard parity is set to 0. This can lead to data loss.

┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
┃ You are running an older version of MinIO released 2 years before the latest release ┃
┃ Update: Run `mc admin update`                                                        ┃
┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛

```

```sh
(base) postgres@slpc:~$ mc alias set 'myminio' 'http://192.168.232.128:9000' 'minioadmin' 'minioadmin'
Added `myminio` successfully.

# 创建bucket neondata
```



## 五、构建并部署 neon-operator

### 5.1 前提

- 本地已安装 Go 1.22+、Docker
- `~/.kube/config` 指向目标集群
- 已在集群中创建 `neon` namespace（步骤一）

### 5.2 一键部署（推荐）

```bash
cd /home/postgres/works/opensource/neon-operator

# 使用 git commit hash 作为唯一版本标签
export IMG=192.168.232.128:5000/neon/neon-operator
export TAG=$(git describe --tags --always --dirty)

# 一步完成：编译 → 构建镜像 → 推送 → 部署（含 CRD + RBAC + Deployment）
make release IMG_OPERATOR=$IMG:$TAG

# 等待 Operator 就绪
kubectl rollout status deployment/neon-controller-manager -n neon --timeout=120s
```

### 5.3 分步执行

```bash
# Step 1: 编译 Go 二进制
make build

# Step 2: 构建 Docker 镜像
make docker-build IMG_OPERATOR=$IMG:$TAG

# Step 3: 推送到私有 registry
make docker-push IMG_OPERATOR=$IMG:$TAG

# Step 4: 部署到集群
make deploy IMG_OPERATOR=$IMG:$TAG
```

### 5.4 部署后触发工作负载滚动更新

Operator 是事件驱动的，重启后不会自动触发子 Controller reconcile 来更新已存在的工作负载 Pod。
如需让新代码的效果应用到 Pageserver/Safekeeper/StorageBroker/StorageController，需手动 annotation：

```bash
# 触发所有组件 reconcile
kubectl annotate cluster my-cluster reconcile-trigger="$(date +%s)" --overwrite -n neon
for sk in $(kubectl get safekeeper -n neon -o name); do
  kubectl annotate $sk reconcile-trigger="$(date +%s)" --overwrite -n neon
done
for ps in $(kubectl get pageserver -n neon -o name); do
  kubectl annotate $ps reconcile-trigger="$(date +%s)" --overwrite -n neon
done

# 监控滚动更新进度
kubectl rollout status deployment/my-cluster-storage-controller -n neon
kubectl rollout status deployment/my-cluster-storage-broker -n neon
```

> **为什么需要用 annotation 触发？** Operator 采用事件驱动架构，各 Controller 只在其管理的 CR（Cluster/Safekeeper/Pageserver）发生变更时执行 reconciliation。重启 operator Pod 不产生任何 CR 变更事件，因此工作负载 Deployment/StatefulSet 不会被自动更新。

---

## 六、部署Cluster

当前未实现safekeeper反亲和性，


```yaml
---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Cluster
metadata:
  name: my-cluster
  namespace: neon
spec:
  numSafekeepers: 3                       # Safekeeper 数量，最少 3 个（共识要求）
  defaultPGVersion: 17                     # 默认 PostgreSQL 版本：14/15/16/17
  neonImage: "ghcr.io/neondatabase/neon:latest"      # Neon 组件镜像（生产环境使用固定版本；若无法访问外网，改用本地 registry 路径如 192.168.232.128:5000/neondatabase/neon:8463）
  bucketCredentialsSecret:
    name: bucket-credentials               # 引用 Step 1 创建的 Secret
  storageControllerDatabaseSecret:
    name: storage-controller-pg-cluster-app
    key: uri

---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Pageserver
metadata:
  name: pageserver-0
  namespace: neon
spec:
  cluster: my-cluster                     # 绑定到的 Cluster 名称
  id: 0                                   # Pageserver ID（手动管理，0, 1, 2...）
  bucketCredentialsSecret:
    name: bucket-credentials
  storageConfig:
    size: 10Gi                            # PVC 大小

---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-1
  namespace: neon
spec:
  cluster: my-cluster
  id: 1
  storageConfig:
    size: 2Gi
---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-2
  namespace: neon
spec:
  cluster: my-cluster
  id: 2
  storageConfig:
    size: 2Gi
---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-3
  namespace: neon
spec:
  cluster: my-cluster
  id: 3
  storageConfig:
    size: 2Gi

---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Project
metadata:
  name: my-project
  namespace: neon
spec:
  cluster: my-cluster                     # 所属 Cluster
  pgVersion: 17                           # PG 版本，可覆盖 Cluster 默认值
  tenantId: ""                            # 留空则 Operator 自动生成 32 位 tenant ID

---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Branch
metadata:
  name: main-branch
  namespace: neon
spec:
  projectID: my-project                   # 父 Project 名称
  pgVersion: 17
  timelineID: ""                          # 留空则 Operator 自动生成
```


---

删除租户:
通过 storcon_cli 命令行工具:
```sh
storcon_cli --api-url http://storage-controller:9090 tenant-delete --tenant-id <TENANT_ID>
```

通过 HTTP API 直接调用:
`DELETE http://storage-controller:9090/v1/tenant/{tenant_id}`  


```yaml
---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Pageserver
metadata:
  name: pageserver-0
  namespace: neon
spec:
  cluster: my-cluster                     # 绑定到的 Cluster 名称
  id: 0                                   # Pageserver ID（手动管理，0, 1, 2...）
  bucketCredentialsSecret:
    name: bucket-credentials
  storageConfig:
    size: 10Gi                            # PVC 大小

apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Pageserver
metadata:
  name: pageserver-1
  namespace: neon
spec:
  cluster: my-cluster                     # 绑定到的 Cluster 名称
  id: 1                                   # Pageserver ID（手动管理，0, 1, 2...）
  bucketCredentialsSecret:
    name: bucket-credentials
  storageConfig:
    size: 10Gi                            # PVC 大小

---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-1
  namespace: neon
spec:
  cluster: my-cluster
  id: 1
  storageConfig:
    size: 2Gi
---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-2
  namespace: neon
spec:
  cluster: my-cluster
  id: 2
  storageConfig:
    size: 2Gi
---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Safekeeper
metadata:
  name: safekeeper-3
  namespace: neon
spec:
  cluster: my-cluster
  id: 3
  storageConfig:
    size: 2Gi

---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Project
metadata:
  name: my-project
  namespace: neon
spec:
  cluster: my-cluster                     # 所属 Cluster
  pgVersion: 17                           # PG 版本，可覆盖 Cluster 默认值
  tenantId: ""                            # 留空则 Operator 自动生成 32 位 tenant ID

---
apiVersion: neon.oltp.molnett.org/v1alpha1
kind: Branch
metadata:
  name: main-branch        # Branch 名称（纯 timeline 容器，不含 Compute）
  namespace: neon
spec:
  projectID: my-project                   # 父 Project 名称
  pgVersion: 17
  timelineID: ""                          # 留空则 Operator 自动生成

---
```