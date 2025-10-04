**AIStore: High-Performance, Scalable Storage for AI Workloads**

![License](https://img.shields.io/badge/license-MIT-blue.svg)
![Version](https://img.shields.io/badge/version-v3.28-green.svg)
![Go Report Card](https://goreportcard.com/badge/github.com/NVIDIA/aistore)

AIStore (AIS) is a lightweight distributed storage stack tailored for AI applications. It's an elastic cluster that can grow and shrink at runtime and can be ad-hoc deployed, with or without Kubernetes, anywhere from a single Linux machine to a bare-metal cluster of any size. Built from scratch, AIS provides linear scale-out, consistent performance, and a flexible deployment model.

AIS consistently shows [balanced I/O distribution and linear scalability](https://aistore.nvidia.com/blog/2025/07/26/smooth-max-line-speed) across an arbitrary number of clustered nodes. The system supports fast data access, reliability, and rich customization for data transformation workloads.

## Features

* ✅ **Multi-Cloud Access:** Seamlessly access and manage content across multiple [cloud backends](/docs/overview.md#at-a-glance) (including AWS S3, GCS, Azure, OCI), with the additional benefit of fast-tier performance and configurable data redundancy.
* ✅ **Deploy Anywhere:** AIS runs on any Linux machine, virtual or physical. Deployment options range from a single [Docker container](https://github.com/NVIDIA/aistore/blob/main/deploy/prod/docker/single/README.md) and [Google Colab](https://aistore.nvidia.com/blog/2024/09/18/google-colab-aistore) to petascale [Kubernetes clusters](https://github.com/NVIDIA/ais-k8s). There are [no built-in limitations](https://github.com/NVIDIA/aistore/blob/main/docs/overview.md#no-limitations-principle) on deployment size or functionality.
* ✅ **High Availability:** Redundant control and data planes. Self-healing, end-to-end protection, n-way mirroring, and erasure coding. Arbitrary number of lightweight access points.
* ✅ **HTTP-based API:** A feature-rich, native API (with user-friendly SDKs for Go and Python), and compliant [Amazon S3 API](/docs/s3compat.md) for running unmodified S3 clients.
* ✅ **Monitoring:** Comprehensive observability with integrated Prometheus metrics, Grafana dashboards, detailed logs with configurable verbosity, and CLI-based performance tracking for complete cluster visibility and troubleshooting. See [Observability](/docs/monitoring-overview.md) for details.
* ✅ **Unified Namespace:** Attach AIS clusters together to provide fast, unified access to the entirety of hosted datasets, allowing users to reference shared buckets with cluster-specific identifiers.
* ✅ **Turn-key Cache:** In addition to robust data protection features, AIS offers a per-bucket configurable LRU-based cache with eviction thresholds and storage capacity watermarks.
* ✅ **ETL Offload:** Execute I/O intensive data transformations [close to the data](/docs/etl.md), either inline (on-the-fly as part of each read request) or offline (batch processing, with the destination bucket populated with transformed results).
* ✅ **Existing File Datasets:** Ingest file datasets from any local or remote source, either on-demand (ad-hoc) or through asynchronous [batch](/docs/overview.md#promote-local-or-shared-files).
* ✅ **Data Consistency:** Guaranteed [consistency](/docs/overview.md#read-after-write-consistency) across all gateways, with [write-through](/docs/overview.md#write-through) semantics in presence of [remote backends](/docs/overview.md#backend-provider).
* ✅ **Small File Optimization:** AIS supports TAR, ZIP, TAR.GZ, and TAR.LZ4 serialization for batching and processing small files. Features include [initial sharding](https://aistore.nvidia.com/blog/2024/08/16/ishard), distributed shuffle (re-sharding), appending to existing shards, listing contained files, and [more](/docs/overview.md#shard).
* ✅ **Kubernetes:** For production deployments, we developed the [AIS/K8s Operator](https://github.com/NVIDIA/ais-k8s/tree/main/operator). A dedicated GitHub [repository](https://github.com/NVIDIA/ais-k8s) contains Ansible scripts, Helm charts, and deployment guidance.
* ✅ **Authentication and Access Control:** OAuth 2.0-compatible [authentication server (AuthN)](/docs/authn.md)[^authn-beta].
* ✅ **Batch Jobs:** Start, monitor, and control cluster-wide [batch operations](/docs/batch.md).

The feature set is actively growing and also includes: [adding/removing nodes at runtime](/docs/lifecycle_node.md), managing [TLS certificates](/docs/cli/x509.md) at runtime, listing, copying, prefetching, and transforming [virtual directories](/docs/howto_virt_dirs.md), executing [presigned S3 requests](/docs/s3compat.md#presigned-s3-requests), adaptive [rate limiting](/docs/rate_limit.md), and more.

> For the original **white paper** and design philosophy, please see [AIStore Overview](/docs/overview.md), which also includes high-level block diagram, terminology, APIs, CLI, and more.
> For our 2024 KubeCon presentation, please see [AIStore: Enhancing petascale Deep Learning across Cloud backends](https://www.youtube.com/watch?v=N-d9cbROndg).

## CLI

AIS includes an integrated, scriptable [CLI](/docs/cli.md) for managing clusters, buckets, and objects, running and monitoring batch jobs, viewing and downloading logs, generating performance reports, and more:

```console
$ ais <TAB-TAB>

advanced         config           get              object           scrub            tls
alias            cp               help             performance      search           wait
archive          create           job              prefetch         show
auth             download         log              put              space-cleanup
blob-download    dsort            ls               remote-cluster   start
bucket           etl              ml               rmb              stop
cluster          evict            mpu              rmo              storage
```

## Developer Tools

AIS runs natively on Kubernetes and features open format - thus, the freedom to copy or move your data from AIS at any time using the familiar Linux `tar(1)`, `scp(1)`, `rsync(1)` and similar.

For developers and data scientists, there's also:

* [Go API](https://github.com/NVIDIA/aistore/tree/main/api) used in [CLI](/docs/cli.md) and [benchmarking tools](/docs/aisloader.md)
* [Python SDK](https://github.com/NVIDIA/aistore/tree/main/python/aistore/sdk) + [Reference Guide](/docs/python_sdk.md)
* [PyTorch integration](https://github.com/NVIDIA/aistore/tree/main/python/aistore/pytorch) and usage examples
* [Boto3 support](https://github.com/NVIDIA/aistore/tree/main/python/aistore/botocore_patch)

## Quick Start

1. Read the [Getting Started Guide](/docs/getting_started.md) for a 5-minute local install, or
2. Run a [minimal](https://github.com/NVIDIA/aistore/tree/main/deploy/prod/docker/single) AIS cluster consisting of a single gateway and a single storage node, or
3. Clone the repo and run `make kill cli aisloader deploy` followed by `ais show cluster`

---------------------

## Deployment options

AIS deployment options, as well as intended (development vs. production vs. first-time) usages, are all [summarized here](https://github.com/NVIDIA/aistore/blob/main/deploy/README.md).

Since the prerequisites essentially boil down to having Linux with a disk the deployment options range from [all-in-one container](https://github.com/NVIDIA/aistore/tree/main/deploy/prod/docker/single) to a petascale bare-metal cluster of any size, and from a single VM to multiple racks of high-end servers. Practical use cases require, of course, further consideration.

Some of the most popular deployment options include:

| Option | Use Case |
| --- | ---|
| [Local playground](https://github.com/NVIDIA/aistore/blob/main/docs/getting_started.md#local-playground) | AIS developers or first-time users, Linux or Mac OS. Run `make kill cli aisloader deploy <<< $'N\nM'`, where `N` is a number of [targets](/docs/overview.md#target), `M` is a number of [gateways](/docs/overview.md#proxy) |
| Minimal production-ready deployment | This option utilizes preinstalled docker image and is targeting first-time users or researchers (who could immediately start training their models on smaller datasets) |
| [Docker container](https://github.com/NVIDIA/aistore/tree/main/deploy/prod/docker/single) | Quick testing and evaluation; single-node setup |
| [GCP/GKE automated install](https://github.com/NVIDIA/aistore/blob/main/docs/getting_started.md#kubernetes-playground) | Developers, first-time users, AI researchers |
| [Large-scale production deployment](https://github.com/NVIDIA/ais-k8s) | Requires Kubernetes; provided via [ais-k8s](https://github.com/NVIDIA/ais-k8s) |

> For performance tuning, see [performance](/docs/performance.md) and [AIS K8s Playbooks](https://github.com/NVIDIA/ais-k8s/tree/main/playbooks/host-config).

## Existing Datasets

AIS supports multiple ingestion modes:

* ✅ **On Demand:** Transparent cloud access during workloads.
* ✅ **PUT:** Locally accessible files and directories.
* ✅ **Promote:** Import local target directories and/or NFS/SMB shares mounted on AIS targets.
* ✅ **Copy:** Full buckets, virtual subdirectories (recursively or non-recursively), lists or ranges (via Bash expansion).
* ✅ **Download:** HTTP(S)-accessible datasets and objects.
* ✅ **Prefetch:** Remote buckets or selected objects (from remote buckets), including subdirectories, lists, and/or ranges.
* ✅ **Archive:** [Group and store](https://aistore.nvidia.com/blog/2024/08/16/ishard) related small files from an original dataset.

## Install from Release Binaries

You can install the CLI and benchmarking tools using:

```console
./scripts/install_from_binaries.sh --help
```

The script installs [aisloader](/docs/aisloader.md) and [CLI](/docs/cli.md) from the latest or previous GitHub [release](https://github.com/NVIDIA/aistore/releases) and enables CLI auto-completions.

## PyTorch integration

PyTorch integration is a growing set of datasets (both iterable and map-style), samplers, and dataloaders:

* [Taxonomy of abstractions and API reference](/docs/pytorch.md)
* [AIS plugin for PyTorch: usage examples](https://github.com/NVIDIA/aistore/tree/main/python/aistore/pytorch/README.md)
* [Jupyter notebook examples](https://github.com/NVIDIA/aistore/tree/main/python/examples/pytorch/)

## AIStore Badge

Let others know your project is powered by high-performance AI storage:

[![aistore](https://img.shields.io/badge/powered%20by-AIStore-76B900?style=flat&labelColor=000000)](https://github.com/NVIDIA/aistore)

```markdown
[![aistore](https://img.shields.io/badge/powered%20by-AIStore-76B900?style=flat&labelColor=000000)](https://github.com/NVIDIA/aistore)
```

## More Docs & Guides

* [Overview and Design](/docs/overview.md)
* [Getting Started](/docs/getting_started.md)
* [Buckets and Bucket Management](/docs/bucket.md)
* [Observability](/docs/monitoring-overview.md)
* [Technical Blog](https://aistore.nvidia.com/blog)
* [S3 Compatibility](/docs/s3compat.md)
* [Batch Jobs](/docs/batch.md)
* [Performance](/docs/performance.md) and [CLI: performance](/docs/cli/performance.md)
* [CLI Reference](/docs/cli.md)
* [Authentication](/docs/authn.md)[^authn-beta]
* [Production Deployment: Kubernetes Operator, Ansible Playbooks, Helm Charts, Monitoring](https://github.com/NVIDIA/ais-k8s)

[^authn-beta]: AuthN is under development and has *NOT* gone through a complete security audit.

### How to find information

* See [Extended Index](/docs/docs.md)
* Use CLI `search` command, e.g.: `ais search copy`
* Clone the repository and run `git grep`, e.g.: `git grep -n out-of-band -- "*.md"`

## License

MIT

## Author

Alex Aizman (NVIDIA)
