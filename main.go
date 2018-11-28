package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/sfxclient"
	kv "gopkg.in/Clever/kayvee-go.v6/logger"
	elastic "gopkg.in/olivere/elastic.v5"
)

var kvlog kv.KayveeLogger
var sfxSink *sfxclient.HTTPSink

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

	sfxSink = sfxclient.NewHTTPSink()
	sfxSink.AuthToken = signalfxAPIKey

	kvlog = kv.New("log-monitor-es")
}

func getLatestTimestamps(esClient *elastic.Client) (map[string]time.Time, error) {
	hostname := elastic.NewTermsAggregation().Field("hostname").Size(200)
	timestamp := elastic.NewMaxAggregation().Field("timestamp")
	hostname = hostname.SubAggregation("latestTimes", timestamp)

	q := elastic.NewBoolQuery()
	q = q.Must(elastic.NewTermQuery("title", "heartbeat"))
	q = q.Must(elastic.NewRangeQuery("timestamp").Gte("now-1h").Lte("now"))

	searchResult, err := esClient.Search().
		Index(elasticsearchIndex).
		Query(q).
		SearchType("count").
		Aggregation("hosts", hostname).
		Pretty(true).
		Timeout("15s").
		Do(context.TODO())

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
	now := time.Now()
	for host, timestamp := range timestamps {
		dimensions := map[string]string{
			"hostname":    host,
			"component":   componentName,
			"environment": environment,
		}

		datum := sfxclient.Gauge(metricName, dimensions, timestamp.Unix())
		datumLag := sfxclient.GaugeF(fmt.Sprintf("%s-lag", metricName), dimensions, float64(now.Sub(timestamp))/float64(time.Second))
		points = append(points, datum, datumLag)
	}

	return sfxSink.AddDatapoints(context.TODO(), points)
}

type ec2IPChecker struct {
	ec2api            ec2iface.EC2API
	lastCheck         time.Time
	privateIPsRunning map[string]struct{}
}

func (e *ec2IPChecker) updateCache() error {
	if e.privateIPsRunning != nil && time.Now().Sub(e.lastCheck) < 1*time.Minute {
		return nil
	}

	privateIPsRunning := map[string]struct{}{}
	if err := e.ec2api.DescribeInstancesPages(&ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("instance-state-name"),
			Values: []*string{aws.String("running")},
		}},
	}, func(output *ec2.DescribeInstancesOutput, lastPage bool) bool {
		for _, res := range output.Reservations {
			for _, instance := range res.Instances {
				if instance.PrivateIpAddress != nil {
					privateIPsRunning[*instance.PrivateIpAddress] = struct{}{}
				}
			}
		}
		return true
	}); err != nil {
		return err
	}

	e.privateIPsRunning = privateIPsRunning
	e.lastCheck = time.Now()
	return nil
}

func (e *ec2IPChecker) IsRunning(ip string) (bool, error) {
	if err := e.updateCache(); err != nil {
		return false, err
	}
	_, ok := e.privateIPsRunning[ip]
	return ok, nil
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

	sess := session.New()
	ec2api := ec2.New(sess)
	ec2ip := &ec2IPChecker{ec2api: ec2api}

	for c := time.Tick(15 * time.Second); ; <-c {
		timestamps, err := getLatestTimestamps(esClient)
		if err != nil {
			kvlog.ErrorD("timestamp", kv.M{"error": err.Error()})
			continue
		}

		// correct the data for instances that aren't running
		for hostname := range timestamps {
			if strings.HasPrefix(hostname, "ip-") {
				// parse IP address out of ES hostnames of the form ip-10-0-0-1
				ip := strings.Replace(strings.TrimPrefix(hostname, "ip-"), "-", ".", -1)
				running, err := ec2ip.IsRunning(ip)
				if err != nil {
					kvlog.ErrorD("ec2-ip-check", kv.M{"error": err.Error()})
				} else if !running {
					// set to now so that signalfx's last datapoint is ok
					timestamps[hostname] = time.Now()
				}
			}
		}

		// Log the number of hosts reported
		kvlog.DebugD("timestamp", kv.M{"count": len(timestamps)})

		err = sendToSignalFX(timestamps)
		if err != nil {
			kvlog.ErrorD("send-to-signalfx", kv.M{"error": err.Error()})
			continue
		}
		kvlog.Trace("sent-to-signalfx")
	}
}
