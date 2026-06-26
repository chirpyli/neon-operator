package pageserver

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"oltp.molnett.org/neon-operator/api/v1alpha1"
	"oltp.molnett.org/neon-operator/specs/storagebroker"
	"oltp.molnett.org/neon-operator/specs/storagecontroller"
)

func ConfigMap(ps *v1alpha1.Pageserver, bucketSecret *corev1.Secret) *corev1.ConfigMap {
	pageserverToml := fmt.Sprintf(`
# ===== 网络 =====
listen_pg_addr = "0.0.0.0:6400"
listen_http_addr = "0.0.0.0:9898"

# ===== 文件描述符 =====
max_file_descriptors = 1000

# ===== 页面缓存 =====
page_cache_size = 32768

# ===== Broker =====
broker_endpoint = "%s"
broker_keepalive_interval = "5s"

# ===== 控制平面 =====
control_plane_api = "%s/upcall/v1/"

# ===== PostgreSQL 分发目录 =====
pg_distrib_dir = "/usr/local/"

# ===== 远程存储 (S3) =====
[remote_storage]
bucket_name = "%s"
bucket_region = "%s"
prefix_in_bucket = "pageserver"
endpoint = "%s"

# ===== 磁盘驱逐 =====
[disk_usage_based_eviction]
max_usage_pct = 80
min_avail_bytes = 2000000000
period = "60s"

# ===== 租户默认配置 =====
[tenant_config]
checkpoint_distance = 268435456
compaction_threshold = 10
compaction_target_size = 134217728
gc_horizon = 67108864
gc_period = "1h"
pitr_interval = "7d"

# ===== 日志 =====
log_format = "json"

# ===== 并发控制 =====
concurrent_tenant_warmup = 8
background_task_maximum_delay = "10s"

# ===== 指标 =====
metric_collection_interval = "60s"
`,
		storagebroker.URL(ps.Spec.Cluster),
		storagecontroller.URL(ps.Spec.Cluster),
		string(bucketSecret.Data["BUCKET_NAME"]),
		string(bucketSecret.Data["AWS_REGION"]),
		string(bucketSecret.Data["AWS_ENDPOINT_URL"]),
	)

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name(ps),
			Namespace: ps.Namespace,
			Labels:    labels(ps),
		},
		Data: map[string]string{
			"pageserver.toml": pageserverToml,
		},
	}
}
