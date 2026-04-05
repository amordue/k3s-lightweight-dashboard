package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"k3s-dashboard/internal/model"
	"k3s-dashboard/internal/storage"
)

type Status struct {
	Ready       bool      `json:"ready"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	Interval    string    `json:"interval"`
	Source      string    `json:"source,omitempty"`
}

type podMetadata struct {
	workloadKind  string
	workloadName  string
	nodeName      string
}

type kubeletSummary struct {
	Pods []kubeletPodStats `json:"pods"`
}

type kubeletPodStats struct {
	PodRef     kubeletPodRef           `json:"podRef"`
	Containers []kubeletContainerStats `json:"containers"`
}

type kubeletPodRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type kubeletContainerStats struct {
	CPU    *kubeletCPUStats    `json:"cpu,omitempty"`
	Memory *kubeletMemoryStats `json:"memory,omitempty"`
}

type kubeletCPUStats struct {
	UsageNanoCores *uint64 `json:"usageNanoCores,omitempty"`
}

type kubeletMemoryStats struct {
	WorkingSetBytes *uint64 `json:"workingSetBytes,omitempty"`
}

type Collector struct {
	clientset     kubernetes.Interface
	metricsClient metricsclient.Interface
	store         *storage.Store
	interval      time.Duration
	logger        *log.Logger

	mu          sync.RWMutex
	ready       bool
	lastSuccess time.Time
	lastError   string
	lastSource  string
}

func New(cfg *rest.Config, store *storage.Store, interval time.Duration, logger *log.Logger) (*Collector, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	metricsAPI, err := metricsclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &Collector{
		clientset:     clientset,
		metricsClient: metricsAPI,
		store:         store,
		interval:      interval,
		logger:        logger,
	}, nil
}

func (c *Collector) Run(ctx context.Context) {
	c.scrape(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.scrape(ctx)
		}
	}
}

func (c *Collector) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return Status{
		Ready:       c.ready,
		LastSuccess: c.lastSuccess,
		LastError:   c.lastError,
		Interval:    c.interval.String(),
		Source:      c.lastSource,
	}
}

func (c *Collector) scrape(ctx context.Context) {
	scrapeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	samples, source, err := c.collectSamples(scrapeCtx)
	if err != nil {
		c.setError(err)
		c.logger.Printf("metrics scrape failed: %v", err)
		return
	}

	if len(samples) == 0 {
		c.setError(fmt.Errorf("no pod samples returned from %s", source))
		return
	}

	scrapedAt := time.Now().UTC().Truncate(time.Second)
	if err := c.store.InsertSnapshot(scrapeCtx, scrapedAt, samples); err != nil {
		c.setError(err)
		c.logger.Printf("persist scrape failed: %v", err)
		return
	}

	if err := c.store.PruneBefore(scrapeCtx, scrapedAt.Add(-c.store.Retention())); err != nil {
		c.logger.Printf("retention prune failed: %v", err)
	}

	c.setSuccess(scrapedAt, source)
	if c.logger != nil {
		c.logger.Printf("captured %d pod samples via %s", len(samples), source)
	}
}

func (c *Collector) collectSamples(ctx context.Context) ([]model.PodSample, string, error) {
	metadata, err := c.loadPodMetadata(ctx)
	if err != nil {
		return nil, "", err
	}

	podMetrics, err := c.metricsClient.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{})
	if err == nil {
		samples := make([]model.PodSample, 0, len(podMetrics.Items))
		for _, metric := range podMetrics.Items {
			var cpuMilli int64
			var memoryBytes int64

			for _, container := range metric.Containers {
				cpuMilli += container.Usage.Cpu().MilliValue()
				memoryBytes += container.Usage.Memory().Value()
			}

			entry := metadata[namespacedKey(metric.Namespace, metric.Name)]
			samples = append(samples, model.PodSample{
				Namespace:    metric.Namespace,
				WorkloadKind: entry.workloadKind,
				WorkloadName: entry.workloadName,
				PodName:      metric.Name,
				NodeName:     entry.nodeName,
				CPUMilli:     cpuMilli,
				MemoryBytes:  memoryBytes,
			})
		}

		if len(samples) > 0 {
			return samples, "metrics-api", nil
		}
		err = fmt.Errorf("metrics API returned no pod samples")
	}

	if c.logger != nil {
		c.logger.Printf("metrics API unavailable, attempting kubelet summary fallback: %v", err)
	}

	samples, fallbackErr := c.collectFromKubeletSummary(ctx, metadata)
	if fallbackErr != nil {
		return nil, "", fmt.Errorf("metrics API failed: %v; kubelet summary fallback failed: %w", err, fallbackErr)
	}

	return samples, "kubelet-summary", nil
}

func (c *Collector) loadPodMetadata(ctx context.Context) (map[string]podMetadata, error) {
	podList, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	replicaSetList, err := c.clientset.AppsV1().ReplicaSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list replica sets: %w", err)
	}

	jobList, err := c.clientset.BatchV1().Jobs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}

	replicaSetOwners := make(map[string]string, len(replicaSetList.Items))
	for _, replicaSet := range replicaSetList.Items {
		if owner := controllerOwner(replicaSet.OwnerReferences); owner != nil && owner.Kind == "Deployment" {
			replicaSetOwners[namespacedKey(replicaSet.Namespace, replicaSet.Name)] = owner.Name
		}
	}

	jobOwners := make(map[string]string, len(jobList.Items))
	for _, job := range jobList.Items {
		if owner := controllerOwner(job.OwnerReferences); owner != nil && owner.Kind == "CronJob" {
			jobOwners[namespacedKey(job.Namespace, job.Name)] = owner.Name
		}
	}

	metadata := make(map[string]podMetadata, len(podList.Items))
	for index := range podList.Items {
		pod := &podList.Items[index]
		workloadKind, workloadName := resolveWorkload(pod, replicaSetOwners, jobOwners)
		metadata[namespacedKey(pod.Namespace, pod.Name)] = podMetadata{
			workloadKind: workloadKind,
			workloadName: workloadName,
			nodeName:     pod.Spec.NodeName,
		}
	}

	return metadata, nil
}

func (c *Collector) collectFromKubeletSummary(ctx context.Context, metadata map[string]podMetadata) ([]model.PodSample, error) {
	nodes := make(map[string]struct{})
	for _, entry := range metadata {
		if entry.nodeName == "" {
			continue
		}
		nodes[entry.nodeName] = struct{}{}
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("no scheduled pods available for kubelet summary fallback")
	}

	samples := make([]model.PodSample, 0, len(metadata))
	seen := make(map[string]struct{})
	for nodeName := range nodes {
		var summary kubeletSummary
		path := fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", nodeName)
		result := c.clientset.CoreV1().RESTClient().Get().AbsPath(path).Do(ctx)
		raw, err := result.Raw()
		if err != nil {
			return nil, fmt.Errorf("fetch kubelet summary for node %s: %w", nodeName, err)
		}
		if err := json.Unmarshal(raw, &summary); err != nil {
			return nil, fmt.Errorf("decode kubelet summary for node %s: %w", nodeName, err)
		}

		for _, podStats := range summary.Pods {
			key := namespacedKey(podStats.PodRef.Namespace, podStats.PodRef.Name)
			entry, ok := metadata[key]
			if !ok {
				continue
			}

			var cpuMilli int64
			var memoryBytes int64
			for _, container := range podStats.Containers {
				if container.CPU != nil && container.CPU.UsageNanoCores != nil {
					cpuMilli += int64(*container.CPU.UsageNanoCores / 1000000)
				}
				if container.Memory != nil && container.Memory.WorkingSetBytes != nil {
					memoryBytes += int64(*container.Memory.WorkingSetBytes)
				}
			}

			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}

			samples = append(samples, model.PodSample{
				Namespace:    podStats.PodRef.Namespace,
				WorkloadKind: entry.workloadKind,
				WorkloadName: entry.workloadName,
				PodName:      podStats.PodRef.Name,
				NodeName:     entry.nodeName,
				CPUMilli:     cpuMilli,
				MemoryBytes:  memoryBytes,
			})
		}
	}

	return samples, nil
}

func namespacedKey(namespace string, name string) string {
	return namespace + "/" + name
}

func controllerOwner(owners []metav1.OwnerReference) *metav1.OwnerReference {
	for index := range owners {
		owner := owners[index]
		if owner.Controller != nil && *owner.Controller {
			return &owner
		}
	}

	if len(owners) == 0 {
		return nil
	}

	return &owners[0]
}

func resolveWorkload(pod *corev1.Pod, replicaSetOwners map[string]string, jobOwners map[string]string) (string, string) {
	if pod == nil {
		return "Pod", "unknown"
	}

	owner := controllerOwner(pod.OwnerReferences)
	if owner == nil {
		return "Pod", pod.Name
	}

	switch owner.Kind {
	case "ReplicaSet":
		if deploymentName, ok := replicaSetOwners[namespacedKey(pod.Namespace, owner.Name)]; ok {
			return "Deployment", deploymentName
		}
		return "ReplicaSet", owner.Name
	case "Job":
		if cronJobName, ok := jobOwners[namespacedKey(pod.Namespace, owner.Name)]; ok {
			return "CronJob", cronJobName
		}
		return "Job", owner.Name
	default:
		return owner.Kind, owner.Name
	}
}

func (c *Collector) setSuccess(scrapedAt time.Time, source string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ready = true
	c.lastSuccess = scrapedAt
	c.lastError = ""
	c.lastSource = source
}

func (c *Collector) setError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastError = err.Error()
	if c.lastSuccess.IsZero() {
		c.ready = false
	}
}