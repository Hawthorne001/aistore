# AIStore Kubernetes Observability

This document explains how to implement and use observability features for AIStore deployments in Kubernetes environments. Kubernetes provides additional tools and patterns for monitoring that complement AIStore's built-in observability features.

## Table of Contents
- [Kubernetes Monitoring Architecture](#kubernetes-monitoring-architecture)
- [Prerequisites](#prerequisites)
- [Deployment Methods](#deployment-methods)
  - [Using the AIS-K8s Repository](#method-1-using-the-ais-k8s-repository)
  - [Manual Configuration with Prometheus Operator](#method-2-manual-configuration-with-prometheus-operator)
- [Configuring AIStore for Kubernetes Monitoring](#configuring-aistore-for-kubernetes-monitoring)
- [Kubernetes-specific Metrics](#kubernetes-specific-metrics)
- [Grafana Dashboards for Kubernetes](#grafana-dashboards-for-kubernetes)
- [Alerting in Kubernetes](#alerting-in-kubernetes)
- [Log Management in Kubernetes](#log-management-in-kubernetes)
- [Operational Best Practices](#operational-best-practices)
- [Troubleshooting AIStore in Kubernetes](#troubleshooting-aistore-in-kubernetes)
- [Further Reading](#further-reading)
- [Related Observability Documentation](#related-observability-documentation)

## Kubernetes Monitoring Architecture

When deployed in Kubernetes, AIStore observability typically follows this architecture:

```
┌────────────────────────────────────────────────────────────┐
│                     Kubernetes Cluster                     │
│                                                            │
│  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐     │
│  │  AIStore    │    │ Prometheus  │    │  Grafana    │     │
│  │   Pods      │───▶│  Operator   │───▶│   Pods      │     │
│  │             │    │             │    │             │     │
│  └─────────────┘    └─────────────┘    └─────────────┘     │
│        │                   │                  │            │
│        │                   │                  │            │
│        ▼                   ▼                  ▼            │
│  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐     │
│  │ Kubernetes  │    │ AlertManager│    │ Persistent  │     │
│  │  Metrics    │    │    Pods     │    │  Storage    │     │
│  └─────────────┘    └─────────────┘    └─────────────┘     │
│                                                            │
└────────────────────────────────────────────────────────────┘
```

Key components in this architecture:
- **AIStore Pods**: Expose Prometheus metrics via the `/metrics` endpoint
- **Prometheus Operator**: Manages Prometheus instances and monitoring configurations
- **Grafana**: Provides visualization for both AIStore and Kubernetes metrics
- **AlertManager**: Handles alert routing and notifications
- **Kubernetes Metrics**: Standard metrics from the Kubernetes API
- **Persistent Storage**: For long-term metrics retention and Grafana state

## Prerequisites

Before setting up AIStore observability in Kubernetes, ensure you have:

- A functional Kubernetes cluster (v1.30+)
- AIStore deployed on the cluster
- [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) or its individual components:
  - Prometheus Operator
  - Prometheus Server
  - AlertManager
  - Grafana
- `kubectl` configured to access your cluster
- Helm v3+ (for chart-based installations)
- Storage classes configured for persistent volumes (if using persistent storage)

> **NOTE**: The YAML examples provided in this document are intended as reference templates that demonstrate the structure and key components required for AIStore observability in Kubernetes. These examples should be reviewed and validated by Kubernetes experts before applying to production environments. They may require adjustments based on your specific Kubernetes version, monitoring stack configuration, and AIStore deployment. API versions and specific field formats can vary between Kubernetes releases.


## Deployment Methods

### Method 1: Using the AIS-K8s Repository

The [AIS-K8s repository](https://github.com/NVIDIA/ais-k8s) provides pre-configured monitoring for AIStore. This is the recommended approach for production deployments.

```bash
# Clone the repository
git clone https://github.com/NVIDIA/ais-k8s
cd ais-k8s

# Deploy AIStore with monitoring
helm install ais-deployment ./helm/ais --set monitoring.enabled=true
```

For more detailed deployment options:

```bash
# View all available monitoring configuration options
helm show values ./helm/ais | grep -A20 monitoring

# Deploy with customized monitoring settings
helm install ais-deployment ./helm/ais \
  --set monitoring.enabled=true \
  --set monitoring.grafana.persistence.enabled=true \
  --set monitoring.prometheus.retention=15d
```

The AIS-K8s deployment includes:
- Properly configured ServiceMonitors for AIStore components
- Pre-built Grafana dashboards
- Default AlertManager rules
- Persistent storage configuration (optional)

### Method 2: Manual Configuration with Prometheus Operator

If you're using a custom deployment or need fine-grained control, you can manually configure the monitoring stack:

1. Deploy Prometheus Operator using Helm:

```bash
# Add the Prometheus community Helm repository
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

# Create a namespace for monitoring
kubectl create namespace monitoring

# Install the kube-prometheus-stack
helm install prometheus prometheus-community/kube-prometheus-stack \
  --namespace monitoring \
  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false
```

2. Create a ServiceMonitor for AIStore:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: ais-monitors
  namespace: monitoring
  labels:
    release: prometheus  # Match the release label used by your Prometheus instance
spec:
  selector:
    matchLabels:
      app: ais
  namespaceSelector:
    matchNames:
      - ais-namespace   # Replace with your AIStore namespace
  endpoints:
  - port: metrics       # Must match the service port name in AIStore service definition
    interval: 15s
    path: /metrics
    relabelings:
    - action: labelmap
      regex: __meta_kubernetes_pod_label_(.+)
    - sourceLabels: [__meta_kubernetes_pod_name]
      action: replace
      targetLabel: instance
```

3. Apply the ServiceMonitor:

```bash
kubectl apply -f servicemonitor.yaml
```

4. Import AIStore dashboards to Grafana:
   - Download dashboard JSONs from the AIS-K8s repository
   - Navigate to Grafana UI > Dashboards > Import
   - Upload the dashboard JSON files

## Configuring AIStore for Kubernetes Monitoring

> **Note**: The following YAML examples demonstrate the general structure but may need adjustments for your specific environment. JSON configuration within ConfigMaps should use proper JSON formatting without comments.

To ensure AIStore exposes metrics properly in Kubernetes:

1. Verify AIStore ConfigMap includes Prometheus configuration:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ais-config
data:
  ais.json: |
    {
      "prometheus": {
        "enabled": true,
        "pushgateway": ""      # Leave empty for pull-based metrics
      },
      # Other AIStore configuration...
    }
```

2. Check that AIStore Service definitions expose the metrics port:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: ais-targets
  labels:
    app: ais
    component: target
spec:
  ports:
  - name: metrics       # This name must match the port in ServiceMonitor
    port: 8081
    targetPort: 8081
  selector:
    app: ais
    component: target
---
apiVersion: v1
kind: Service
metadata:
  name: ais-proxies
  labels:
    app: ais
    component: proxy
spec:
  ports:
  - name: metrics
    port: 8080
    targetPort: 8080
  selector:
    app: ais
    component: proxy
```

3. Verify metrics are being exposed by checking directly:

```bash
# Forward a port to an AIStore target pod
kubectl port-forward -n ais-namespace pod/ais-target-0 8081:8081

# In another terminal, check metrics endpoint
curl localhost:8081/metrics
```

## Kubernetes-specific Metrics

When monitoring AIStore in Kubernetes, you should track both AIStore-specific metrics and Kubernetes infrastructure metrics:

### Pod Metrics

| Metric | Description | Use Case |
|--------|-------------|----------|
| `kube_pod_container_resource_usage_cpu_cores` | CPU usage by AIStore pods | Capacity planning, detect overloads |
| `kube_pod_container_resource_requests_cpu_cores` | CPU requested by pods | Resource allocation analysis |
| `kube_pod_container_resource_limits_cpu_cores` | CPU limits for pods | Resource constraint checks |
| `kube_pod_container_resource_usage_memory_bytes` | Memory usage by AIStore pods | Detect memory issues |
| `kube_pod_container_status_restarts_total` | Container restart count | Identify stability issues |
| `kube_pod_status_phase` | Pod status (running, pending, failed) | Monitor pod lifecycle |

### Volume Metrics

| Metric | Description | Use Case |
|--------|-------------|----------|
| `kubelet_volume_stats_available_bytes` | Available volume space | Capacity planning |
| `kubelet_volume_stats_capacity_bytes` | Total volume capacity | Storage provisioning |
| `kubelet_volume_stats_used_bytes` | Used volume space | Detect storage constraints |
| `kubelet_volume_stats_inodes_free` | Available inodes | Detect inode exhaustion |
| `volume_manager_total_volumes` | Volume count | Resource monitoring |

### Network Metrics

| Metric | Description | Use Case |
|--------|-------------|----------|
| `container_network_receive_bytes_total` | Network bytes received | Traffic analysis |
| `container_network_transmit_bytes_total` | Network bytes transmitted | Bandwidth usage |
| `container_network_receive_packets_total` | Network packets received | Network troubleshooting |
| `container_network_transmit_packets_total` | Network packets transmitted | Network troubleshooting |
| `container_network_receive_packets_dropped_total` | Dropped incoming packets | Detect network issues |
| `container_network_transmit_packets_dropped_total` | Dropped outgoing packets | Detect network issues |

### Node Metrics

| Metric | Description | Use Case |
|--------|-------------|----------|
| `node_cpu_seconds_total` | CPU usage by node | Node performance |
| `node_memory_MemAvailable_bytes` | Available memory | Resource management |
| `node_filesystem_avail_bytes` | Available filesystem space | Storage health |
| `node_network_transmit_bytes_total` | Node network usage | Network capacity planning |
| `node_disk_io_time_seconds_total` | Disk I/O time | Storage performance analysis |

## Grafana Dashboards for Kubernetes

For effective AIStore monitoring in Kubernetes, use a combination of specialized dashboards:

### 1. AIStore Application Dashboard

Focus on AIStore-specific metrics:
- Throughput and latency metrics (GET, PUT, DELETE operations)
- Operation rates and error counts
- Rebalance and resilver status
- Cache hit ratios and storage utilization

### 2. Kubernetes Resource Dashboard

Focus on Kubernetes infrastructure:
- Pod resource usage (CPU, memory) for AIStore components
- Network traffic between pods and nodes
- Volume usage and I/O performance
- Pod restarts and health status
- Node resource utilization

![AIStore Kubernetes Dashboard](https://github.com/NVIDIA/ais-k8s/blob/main/monitoring/images/grafana.png)

### 3. Combined Operational Dashboard

Correlate application and infrastructure metrics:
- AIStore performance versus underlying resource usage
- Impact of pod scheduling and restarts on operations
- Storage latency versus Kubernetes volume metrics
- Network throughput correlation with operation rates

### Example Dashboard Import

Import the AIStore Kubernetes dashboard:

1. Download the dashboard JSON from the [ais-k8s repository](https://github.com/NVIDIA/ais-k8s/blob/main/monitoring/kube-prom/dashboard-configmap/ais_dashboard.json)
2. In Grafana, navigate to Dashboards > Import
3. Upload the JSON file or paste its contents
4. Select your Prometheus data source
5. Customize dashboard variables if needed
6. Click Import

Create dashboard variables for better filtering:
- `namespace`: AIStore namespace
- `pod`: AIStore pod selector
- `node`: Kubernetes node selector
- `interval`: Time range for rate calculations

## Alerting in Kubernetes

Configure Prometheus AlertManager rules for proactive monitoring:

> **Note**: The following AlertManager configurations are examples that demonstrate structure and common alert patterns. You'll need to customize thresholds and selectors for your environment and ensure compatibility with your Prometheus Operator version.

### AlertManager Rules

Create a PrometheusRule resource for Kubernetes-specific alerts (and notice PromQL queries):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: ais-k8s-alerts
  namespace: monitoring
spec:
  groups:
  - name: ais.kubernetes.rules
    rules:
    - alert: AIStorePodRestartingFrequently
      expr: {% raw %}rate(kube_pod_container_status_restarts_total{namespace="ais-namespace"}[15m]) > 0.2{% endraw %}
      for: 5m
      labels:
        severity: warning
      annotations:
        summary: "AIStore pod restarting frequently"
        description: "Pod {% raw %}{{ $labels.pod }}{% endraw %} in namespace {% raw %}{{ $labels.namespace }}{% endraw %} is restarting frequently"

    - alert: AIStoreVolumeNearlyFull
      expr: {% raw %}kubelet_volume_stats_available_bytes{namespace="ais-namespace"} / kubelet_volume_stats_capacity_bytes{namespace="ais-namespace"} < 0.1{% endraw %}
      for: 5m
      labels:
        severity: warning
      annotations:
        summary: "AIStore volume nearly full"
        description: "Volume {% raw %}{{ $labels.persistentvolumeclaim }}{% endraw %} is at {% raw %}{{ $value | humanizePercentage }}{% endraw %} capacity"

    - alert: AIStoreHighNetworkTraffic
      expr: {% raw %}sum(rate(container_network_transmit_bytes_total{namespace="ais-namespace"}[5m])) > 1e9{% endraw %}
      for: 10m
      labels:
        severity: warning
      annotations:
        summary: "High network traffic"
        description: "Network traffic exceeds 1GB/s for 10 minutes"

    - alert: AIStorePodNotReady
      expr: {% raw %}sum by(namespace, pod) (kube_pod_status_phase{namespace="ais-namespace", phase=~"Pending|Unknown|Failed"}) > 0{% endraw %}
      for: 10m
      labels:
        severity: critical
      annotations:
        summary: "AIStore pod not ready"
        description: "Pod {% raw %}{{ $labels.pod }}{% endraw %} is in {% raw %}{{ $labels.phase }}{% endraw %} state for more than 10 minutes"

    - alert: AIStoreNodeHighLoad
      expr: {% raw %}node_load5{instance=~".*"} / on(instance) count by(instance) (node_cpu_seconds_total{mode="system"}) > 3{% endraw %}
      for: 10m
      labels:
        severity: warning
      annotations:
        summary: "High node load affecting AIStore"
        description: "Node {% raw %}{{ $labels.instance }}{% endraw %} has a high load average, which may affect AIStore performance"
```

### Alert Routing

Configure AlertManager to route notifications through appropriate channels:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: AlertmanagerConfig
metadata:
  name: ais-alert-routing
  namespace: monitoring
spec:
  route:
    receiver: 'ais-team-slack'
    group_by: ['alertname', 'namespace']
    group_wait: 30s
    group_interval: 5m
    repeat_interval: 12h
    routes:
    - matchers:
      - name: severity
        value: critical
      receiver: 'ais-team-pagerduty'
      continue: true
  receivers:
  - name: 'ais-team-slack'
    slackConfigs:
    - apiURL:
        key: slack-url
        name: alertmanager-slack-secret
      channel: '#ais-alerts'
      sendResolved: true
  - name: 'ais-team-pagerduty'
    pagerdutyConfigs:
    - routingKey:
        key: pagerduty-key
        name: alertmanager-pagerduty-secret
      sendResolved: true
```

## Log Management in Kubernetes

Centralized logging is essential for troubleshooting AIStore in Kubernetes environments.

### Centralized Logging Options

1. **ELK Stack (Elasticsearch, Logstash, Kibana)**:
   - Comprehensive but resource-intensive solution
   - Deploy using the Elastic Operator or Helm charts
   - Configure Filebeat as a DaemonSet to collect container logs

   ```bash
   # Add Elastic Helm repo
   helm repo add elastic https://helm.elastic.co
   helm repo update

   # Install Elasticsearch
   helm install elasticsearch elastic/elasticsearch \
     --namespace logging \
     --create-namespace

   # Install Kibana
   helm install kibana elastic/kibana \
     --namespace logging

   # Install Filebeat
   helm install filebeat elastic/filebeat \
     --namespace logging \
     --set daemonset.enabled=true
   ```

2. **Loki Stack**:
   - Lighter weight than ELK
   - Designed to work with Prometheus and Grafana
   - Uses Promtail for log collection

   ```bash
   # Add Grafana Helm repo
   helm repo add grafana https://grafana.github.io/helm-charts
   helm repo update

   # Install Loki Stack with Promtail
   helm install loki grafana/loki-stack \
     --namespace logging \
     --create-namespace \
     --set promtail.enabled=true \
     --set grafana.enabled=true
   ```

3. **Fluent Bit / Fluentd**:
   - Flexible log collectors that can send to various backends
   - Lower resource footprint (especially Fluent Bit)
   - Configure as a DaemonSet

   ```bash
   # Install Fluent Bit
   helm repo add fluent https://fluent.github.io/helm-charts
   helm repo update
   helm install fluent-bit fluent/fluent-bit \
     --namespace logging \
     --create-namespace
   ```

### AIStore-specific Logging Configuration

Configure log parsing for AIStore:

1. **For Fluent Bit**, add AIStore parsing rules:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: fluent-bit-parsers
  namespace: logging
data:
  parsers.conf: |
    [PARSER]
        Name        ais_json
        Format      json
        Time_Key    time
        Time_Format %Y-%m-%dT%H:%M:%S.%L
```

2. **For Promtail**, add AIStore-specific pipeline stages:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: promtail-config
  namespace: logging
data:
  promtail.yaml: |
    scrape_configs:
      - job_name: kubernetes-pods
        pipeline_stages:
          - json:
              expressions:
                level: level
                msg: msg
                time: time
          - labels:
              level:
```

3. Create Grafana dashboard for AIStore logs with useful queries:

For Loki:
```
{% raw %}{namespace="ais-namespace"} |= "error" | json | level="error"{% endraw %}
```

For Elasticsearch:
```
{% raw %}kubernetes.namespace:"ais-namespace" AND message:error AND level:error{% endraw %}
```

## Operational Best Practices

### Resource Allocation

Ensure proper resource allocation for AIStore components in Kubernetes:

- **Set appropriate resource requests and limits** for predictable performance:

```yaml
resources:
  requests:
    cpu: 1000m
    memory: 4Gi
  limits:
    cpu: 2000m
    memory: 8Gi
```

- Regularly monitor actual usage versus requested resources to optimize allocation:
  - Use `kubectl top pod` to view current resource usage
  - Analyze trends in Grafana dashboards
  - Adjust resource requests based on historical usage patterns

### Horizontal Pod Autoscaling

While AIStore targets don't typically use HPA, proxy nodes can benefit from autoscaling:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: ais-proxy-hpa
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: ais-proxy
  minReplicas: 3
  maxReplicas: 10
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 80
  - type: Resource
    resource:
      name: memory
      target:
        type: Utilization
        averageUtilization: 80
```

### Pod Disruption Budgets

Protect AIStore availability during cluster maintenance operations:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: ais-target-pdb
spec:
  minAvailable: 60%
  selector:
    matchLabels:
      app: ais
      component: target
```

Create separate PDBs for proxy and target components to ensure cluster stability.

### Readiness and Liveness Probes

Configure appropriate health checks for AIStore components:

```yaml
readinessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 30
  periodSeconds: 10
  timeoutSeconds: 5
  successThreshold: 1
  failureThreshold: 3
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 60
  periodSeconds: 15
  timeoutSeconds: 5
  successThreshold: 1
  failureThreshold: 3
```

### Storage Configuration

For production deployments:

1. Use local storage for mountpaths:
   - Provides better performance than network storage
   - Avoids network congestion for data access

2. Use separate storage for metadata:
   - Consider SSD/NVMe storage for metadata
   - Improves metadata operations performance

3. Configure storage affinity:
   - Keep pods on nodes with their persistent storage
   - Use node selectors or pod affinity rules

## Troubleshooting AIStore in Kubernetes

### Common Issues and Solutions

| Issue | Symptoms | Troubleshooting Commands |
|-------|----------|--------------------------|
| Pod won't start | Pod stuck in Pending state | `kubectl describe pod <pod-name>` |
| Configuration issues | Pod starts but AIStore service fails | `kubectl logs <pod-name>` |
| Performance degradation | High latency, low throughput | `kubectl top pod` |
| Network connectivity | Transport errors in logs | `kubectl exec <pod-name> -- ping <target>` |
| Storage issues | I/O errors, disk full | `kubectl exec <pod-name> -- df -h` |
| Service discovery | Health check failures | `kubectl exec <pod-name> -- curl -v localhost:8080/health` |
| Resource starvation | OOMKilled, CPU throttling | `kubectl describe pod <pod-name>; kubectl top pod <pod-name>` |

### Collecting Debug Information

Script to collect comprehensive debug information:

```bash
#!/bin/bash
NAMESPACE="ais-namespace"
OUTPUT_DIR="ais-debug-$(date +%Y%m%d-%H%M%S)"
mkdir -p $OUTPUT_DIR

echo "Collecting AIStore debug information from namespace $NAMESPACE..."

# Get all resources
kubectl get all -n $NAMESPACE -o wide > $OUTPUT_DIR/resources.txt

# Get pod descriptions
for pod in $(kubectl get pods -n $NAMESPACE -o name); do
  kubectl describe $pod -n $NAMESPACE > $OUTPUT_DIR/$(echo $pod | cut -d/ -f2)-describe.txt
  echo "Collected description for $pod"
done

# Get logs with timestamps
for pod in $(kubectl get pods -n $NAMESPACE -o name); do
  kubectl logs --timestamps=true $pod -n $NAMESPACE > $OUTPUT_DIR/$(echo $pod | cut -d/ -f2)-logs.txt
  echo "Collected logs for $pod"
done

# Get configmaps
kubectl get configmaps -n $NAMESPACE -o yaml > $OUTPUT_DIR/configmaps.yaml

# Get secrets (without revealing values)
kubectl get secrets -n $NAMESPACE -o yaml | grep -v "data:" > $OUTPUT_DIR/secrets-metadata.yaml

# Get PVCs and PVs
kubectl get pvc -n $NAMESPACE -o yaml > $OUTPUT_DIR/pvcs.yaml
kubectl get pv -o yaml > $OUTPUT_DIR/pvs.yaml

# Get services and endpoints
kubectl get services -n $NAMESPACE -o yaml > $OUTPUT_DIR/services.yaml
kubectl get endpoints -n $NAMESPACE -o yaml > $OUTPUT_DIR/endpoints.yaml

# Get metrics
kubectl top pods -n $NAMESPACE > $OUTPUT_DIR/pod-metrics.txt
kubectl top nodes > $OUTPUT_DIR/node-metrics.txt

# Get events sorted by time
kubectl get events -n $NAMESPACE --sort-by='.lastTimestamp' > $OUTPUT_DIR/events.txt

# Get node information
kubectl describe nodes > $OUTPUT_DIR/nodes.txt

# Collect prometheus metrics if available
for pod in $(kubectl get pods -n $NAMESPACE -l app=ais -o name); do
  podname=$(echo $pod | cut -d/ -f2)
  port=$(kubectl get pod $podname -n $NAMESPACE -o jsonpath='{.spec.containers[0].ports[?(@.name=="metrics")].containerPort}')
  if [ ! -z "$port" ]; then
    kubectl port-forward -n $NAMESPACE $pod $port:$port > /dev/null 2>&1 &
    pid=$!
    sleep 2
    curl -s localhost:$port/metrics > $OUTPUT_DIR/$podname-metrics.txt
    kill $pid
    echo "Collected metrics for $pod"
  fi
done

echo "Debug information collected in $OUTPUT_DIR"
```

### Analyzing Issues with AIStore Metrics

To correlate issues with metrics:

1. Check related Prometheus metrics:
   ```bash
   # Port forward to Prometheus
   kubectl port-forward -n monitoring svc/prometheus-operated 9090:9090

   # Open in browser: http://localhost:9090
   ```

2. Search for specific metrics:
   - Target metrics: `ais_target_*`
   - Proxy metrics: `ais_proxy_*`
   - Node state: `ais_target_state_flags`
   - Error counters: `ais_target_err_*`

3. Use PromQL for advanced analysis:
   ```
   # Check for correlated spikes
   rate(ais_target_get_ns_total[5m]) / rate(ais_target_get_count[5m])

   # Compare against node metrics
   rate(node_cpu_seconds_total{mode="user"}[5m])
   ```

## Further Reading

- [AIStore K8s Repository](https://github.com/NVIDIA/ais-k8s)
- [Prometheus Operator Documentation](https://prometheus-operator.dev/docs/developer/getting-started/)
- [Kubernetes Monitoring Best Practices](https://kubernetes.io/docs/tasks/debug-application-cluster/resource-usage-monitoring/)
- [Grafana Loki Documentation](https://grafana.com/docs/loki/latest/)
- [Fluent Bit Kubernetes Documentation](https://docs.fluentbit.io/manual/installation/kubernetes)
- [AIStore K8s Helm Chart Documentation](https://github.com/NVIDIA/ais-k8s/tree/main/helm/ais)

## Related Observability Documentation

| Document | Description |
|----------|-------------|
| [Overview](/docs/monitoring-overview.md) | Introduction to AIS observability |
| [CLI](/docs/monitoring-cli.md) | Command-line monitoring tools |
| [Logs](/docs/monitoring-logs.md) | Configuring, accessing, and utilizing AIS logs |
| [Prometheus](/docs/monitoring-prometheus.md) | Configuring Prometheus with AIS |
| [Metrics Reference](/docs/monitoring-metrics.md) | Complete metrics catalog |
| [Grafana](/docs/monitoring-grafana.md) | Visualizing AIS metrics with Grafana |
