package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/sfxclient"
	"golang.org/x/net/context"
	kv "gopkg.in/Clever/kayvee-go.v3/logger"
	"gopkg.in/olivere/elastic.v3"
)

var kvlog *kv.Logger
var sfxSink *sfxclient.HTTPDatapointSink

// Config vars
var componentName, elasticsearchIndex, elasticsearchURI, environment, signalfxAPIKey, metricName string

// getEnv looks up an environment variable given and exits if it does not exist.
func getEnv(envVar string) string {
	val := os.Getenv(envVar)
	if val == "" {
		log.Fatalf("Must specify env variable %s", envVar)
	}
	return val
}

func init() {
	elasticsearchURI = getEnv("ELASTICSEARCH_URI")
	elasticsearchIndex = getEnv("ELASTICSEARCH_INDEX")
	signalfxAPIKey = getEnv("SIGNALFX_API_KEY")
	metricName = getEnv("METRIC_NAME")
	componentName = getEnv("COMPONENT_NAME")
	environment = getEnv("DEPLOY_ENV")

	sfxSink = sfxclient.NewHTTPDatapointSink()
	sfxSink.AuthToken = signalfxAPIKey

	kvlog = kv.New("log-monitor-es")
}

func getLatestTimestamps(esClient *elastic.Client) (map[string]time.Time, error) {
	hostname := elastic.NewTermsAggregation().Field("Hostname").Size(200)
	timestamp := elastic.NewMaxAggregation().Field("Timestamp")
	hostname = hostname.SubAggregation("latestTimes", timestamp)

	searchResult, err := esClient.Search().
		Index(elasticsearchIndex).
		Query(elastic.NewTermQuery("Title", "heartbeat")).
		SearchType("count").
		Aggregation("hosts", hostname).
		Pretty(true).
		Timeout("15s").
		Do()

	if err != nil {
		return nil, fmt.Errorf("Error while searching: %s", err)
	}

	agg, found := searchResult.Aggregations.Terms("hosts")
	if !found {
		return nil, fmt.Errorf("No results found: %s", err)
	}

	results := map[string]time.Time{}
	for _, hostBucket := range agg.Buckets {
		// Every bucket should have the hostname field as key.
		host := hostBucket.Key.(string)

		// The sub-aggregation latestTimes
		maxTime, found := hostBucket.Max("latestTimes")
		if found {
			// Convert from milliseconds (as returned by Elasticsearch) to
			// seconds (as needed by time.Unix()). Sub-second resolution
			// does not matter for this monitor.
			results[host] = time.Unix(int64(*maxTime.Value)/1000, 0)
		}
	}
	return results, nil
}

func sendToSignalFX(timestamps map[string]time.Time) error {
	points := []*datapoint.Datapoint{}
	for host, timestamp := range timestamps {
		dimensions := map[string]string{
			"hostname":    host,
			"component":   componentName,
			"environment": environment,
		}

		datum := sfxclient.Gauge(metricName, dimensions, timestamp.Unix())
		points = append(points, datum)
	}

	return sfxSink.AddDatapoints(context.TODO(), points)
}

func main() {
	// For AWS logs-* clusters, access is controlled by IP address so no signing is needed,
	// but since AWS blocks some APIs, sniffing and healthchecks are disabled.
	esClient, err := elastic.NewClient(
		elastic.SetURL(elasticsearchURI),
		elastic.SetScheme("https"),
		elastic.SetSniff(false),
		elastic.SetHealthcheck(false),
	)

	if err != nil {
		log.Fatalf("Failed to create ES client: %s\n", err)
	}

	tick := time.Tick(15 * time.Second)
	for {
		timestamps, err := getLatestTimestamps(esClient)
		if err != nil {
			kvlog.ErrorD("timestamp", kv.M{"error": err.Error()})
			continue
		}

		// Log the number of hosts reported
		kvlog.InfoD("timestamp", kv.M{
			"count": len(timestamps),
		})

		err = sendToSignalFX(timestamps)
		if err != nil {
			kvlog.ErrorD("send-to-signalfx", kv.M{"error": err.Error()})
			continue
		}
		kvlog.Info("sent-to-signalfx")

		<-tick
	}
}
