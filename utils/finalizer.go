/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

const (
	// FinalizerName 是所有 neon-operator CRD 统一使用的 Finalizer 字符串。
	// 它确保外部资源（Storage Controller 中的记录）在 Kubernetes 资源从 etcd 中删除之前被清理。
	FinalizerName = "neon.oltp.molnett.org/finalizer"
)
