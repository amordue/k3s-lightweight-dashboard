package model

import "time"

type PodSample struct {
	Namespace    string
	WorkloadKind string
	WorkloadName string
	PodName      string
	NodeName     string
	CPUMilli     int64
	MemoryBytes  int64
}

type Totals struct {
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
}

type PodSummary struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workloadKind"`
	WorkloadName string `json:"workloadName"`
	PodName      string `json:"podName"`
	NodeName     string `json:"nodeName"`
	CPUMilli     int64  `json:"cpuMilli"`
	MemoryBytes  int64  `json:"memoryBytes"`
}

type WorkloadSummary struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workloadKind"`
	WorkloadName string `json:"workloadName"`
	PodCount     int    `json:"podCount"`
	CPUMilli     int64  `json:"cpuMilli"`
	MemoryBytes  int64  `json:"memoryBytes"`
}

type WorkloadSelector struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workloadKind"`
	WorkloadName string `json:"workloadName"`
}

type WorkloadHistorySeries struct {
	Namespace    string  `json:"namespace"`
	WorkloadKind string  `json:"workloadKind"`
	WorkloadName string  `json:"workloadName"`
	Totals       Totals  `json:"totals"`
	History      []Point `json:"history"`
}

type WorkloadHistoryResponse struct {
	LatestScrape time.Time               `json:"latestScrape"`
	Window       string                  `json:"window"`
	Series       []WorkloadHistorySeries `json:"series"`
}

type NamespaceSummary struct {
	Namespace     string `json:"namespace"`
	WorkloadCount int    `json:"workloadCount"`
	PodCount      int    `json:"podCount"`
	CPUMilli      int64  `json:"cpuMilli"`
	MemoryBytes   int64  `json:"memoryBytes"`
}

type Point struct {
	Timestamp   time.Time `json:"timestamp"`
	CPUMilli    int64     `json:"cpuMilli"`
	MemoryBytes int64     `json:"memoryBytes"`
}

type ClusterSummary struct {
	LatestScrape time.Time          `json:"latestScrape"`
	Totals       Totals             `json:"totals"`
	Namespaces   []NamespaceSummary `json:"namespaces"`
	Workloads    []WorkloadSummary  `json:"workloads"`
	Pods         []PodSummary       `json:"pods"`
}

type ClusterHistoryResponse struct {
	LatestScrape time.Time `json:"latestScrape"`
	Window       string    `json:"window"`
	History      []Point   `json:"history"`
}

type NamespaceDetail struct {
	LatestScrape time.Time          `json:"latestScrape"`
	Namespace    string             `json:"namespace"`
	Totals       Totals             `json:"totals"`
	Workloads    []WorkloadSummary  `json:"workloads"`
	Pods         []PodSummary       `json:"pods"`
	History      []Point            `json:"history"`
}

type WorkloadDetail struct {
	LatestScrape time.Time        `json:"latestScrape"`
	Namespace    string           `json:"namespace"`
	WorkloadKind string           `json:"workloadKind"`
	WorkloadName string           `json:"workloadName"`
	Totals       Totals           `json:"totals"`
	Pods         []PodSummary     `json:"pods"`
	History      []Point          `json:"history"`
}

type PodDetail struct {
	LatestScrape time.Time `json:"latestScrape"`
	Namespace    string    `json:"namespace"`
	WorkloadKind string    `json:"workloadKind"`
	WorkloadName string    `json:"workloadName"`
	PodName      string    `json:"podName"`
	NodeName     string    `json:"nodeName"`
	Totals       Totals    `json:"totals"`
	History      []Point   `json:"history"`
}