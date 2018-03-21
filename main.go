package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3"
	googlepb "github.com/golang/protobuf/ptypes/timestamp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
)

type timeSeries struct {
	// struct used to hold timeseries
	Name   string                `json:"Name,omitempty"`
	Labels map[string]string     `json:"labels,omitempty"`
	Points []*monitoringpb.Point `json:"points,omitempty"`
}

func generateLabelID(labels map[string]string) string {
	var ID strings.Builder
	for k, v := range labels {
		fmt.Fprintf(&ID, "%s:%s ", k, v)
	}
	return ID.String()
}

func filterMetric(metricType string) bool {
	// function to filter metrics based on type
	// returns true if the metric should be kept, otherwise returns false

	// ignore aws and agent metrics
	// e.g. "aws.googleapis.com/S3/NumberOfObjects/Sum" or "agent.googleapis.com/redis/connections/total"
	if strings.HasPrefix(metricType, "aws") || strings.HasPrefix(metricType, "agent") || strings.HasPrefix(metricType, "avere") {
		return false
	}

	return true
}

func listAndParseTimeSeries(metricType, projectID string, c *monitoring.MetricClient, timeDelta int) (map[string]timeSeries, error) {
	// takes a metric type such as "compute.google.apis.com/instance/cpu/usage_time" and calls each time series, and parses it
	// returns the timeseries as a map of timeseries ID's to timeSeries structs

	fmt.Println("Scraping metric: ", metricType)

	tsMap := make(map[string]timeSeries)

	// see https://godoc.org/google.golang.org/genproto/googleapis/monitoring/v3#ListTimeSeriesRequest
	// at minimum, require a Name, Filter string, and Interval.
	// If there is no StartTime, the request will succeed and not return anything.
	listTimeSeriesReq := &monitoringpb.ListTimeSeriesRequest{
		Name:   "projects/" + projectID,
		Filter: "metric.type = \"" + metricType + "\"",
		Interval: &monitoringpb.TimeInterval{
			EndTime: &googlepb.Timestamp{
				Seconds: time.Now().Unix(),
			},
			StartTime: &googlepb.Timestamp{
				Seconds: time.Now().Add(time.Duration(-1*timeDelta) * time.Minute).Unix(),
			},
		},
	}

	listTimeSeriesIter := c.ListTimeSeries(context.Background(), listTimeSeriesReq)
	for {
		resp, err := listTimeSeriesIter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Printf("\nError listing timeseries: %v\n", err)
			return tsMap, err
		}

		//resp returns a TimeSeries struct (https://godoc.org/google.golang.org/genproto/googleapis/monitoring/v3#TimeSeries)
		ts := resp

		//fmt.Printf("Found %v points for %v with value type %v with label %v\n", len(ts.Points), ts.Metric.Type, ts.ValueType, ts.Metric.Labels)
		tsMap[generateLabelID(ts.Metric.Labels)] = timeSeries{
			Labels: ts.Metric.Labels,
			Points: ts.Points,
		}
	}
	return tsMap, nil
}

func getMetrics(ctx context.Context, client *monitoring.MetricClient, wantedMetrics []string, projectID string, timeDelta int) int {
	f, err := os.Create("./metrics.json")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	c := make(chan string, len(wantedMetrics))
	pointsCount := 0
	//uses a sync.waitgroup to collect all the goroutines
	var wg sync.WaitGroup
	for _, metricType := range wantedMetrics {
		wg.Add(1)
		go func(metricType string) {
			defer wg.Done()

			tsm, err := listAndParseTimeSeries(metricType, projectID, client, timeDelta)
			if err != nil {
				log.Printf("\nError getting metric %v", metricType)
				return
			}
			if len(tsm) == 0 {
				return
			}

			for _, ts := range tsm {
				pointsCount += len(ts.Points)
			}

			outJSON, err := json.Marshal(tsm)
			if err != nil {
				log.Println(err)
				return
			}

			c <- string(outJSON)
			return
		}(metricType)
	}
	wg.Wait()
	close(c)

	for outJSON := range c {
		f.WriteString(outJSON)
		f.WriteString("\n")
	}
	return pointsCount
}

func collectMetrics(ctx context.Context, serviceAccountLocation, projectID string) (*monitoring.MetricClient, []string, int) {
	// constructs a metric client from a service account
	client, err := monitoring.NewMetricClient(ctx, option.WithServiceAccountFile(serviceAccountLocation))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	// requires a Name to list all Metrics
	listMetricsReq := &monitoringpb.ListMetricDescriptorsRequest{
		Name: "projects/" + projectID,
	}

	//metrics to collect
	wantedMetrics := make([]string, 0)

	// lists all the available metrics and prints them
	listMetricsIter := client.ListMetricDescriptors(ctx, listMetricsReq)
	metricsCount := 0
	for {
		resp, err := listMetricsIter.Next()
		if err == iterator.Done {
			fmt.Println("No more metrics found")
			break
		}
		if err != nil {
			log.Fatalf("Error listing metric descriptors %v", err)
		}

		//resp returns a MetricDescriptor struct (https://godoc.org/google.golang.org/genproto/googleapis/api/metric#MetricDescriptor)
		fmt.Println(resp.Type)

		// for example only, this only scrapes the google cloud compute stuff
		if filterMetric(resp.Type) {
			wantedMetrics = append(wantedMetrics, resp.Type)
		}
		metricsCount++
	}
	return client, wantedMetrics, metricsCount

}

func main() {

	serviceAccountLocation := flag.String("service-account", "", "Path to service account. Will fail if not provided")
	projectID := flag.String("project-id", "", "ID of the google cloud project. Will fail if not provided")

	timeDelta := flag.Int("time-delta", 5, "The start time of the oldest metric. Defaults to 5 minutes from now.")

	flag.Parse()

	if *serviceAccountLocation == "" || *projectID == "" {
		log.Fatalf("No service account or project ID provided")
	}

	ctx := context.Background()

	start := time.Now()

	client, wantedMetrics, metricsCount := collectMetrics(ctx, *serviceAccountLocation, *projectID)
	pointsCount := getMetrics(ctx, client, wantedMetrics, *projectID, *timeDelta)

	fmt.Println("Results: ")
	fmt.Println("\tPossible Metrics: ", metricsCount)
	fmt.Println("\tCrawled Metrics: ", len(wantedMetrics))
	fmt.Println("\tCrawled Points: ", pointsCount)
	fmt.Println("\tTime range: ", *timeDelta)
	fmt.Println("\tTook: ", time.Since(start))

}
