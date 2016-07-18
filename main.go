package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/signalfx/golib/datapoint"
	"github.com/signalfx/golib/sfxclient"
	"golang.org/x/net/context"
	kv "gopkg.in/Clever/kayvee-go.v3/logger"
)

// Why the strange indending?  Why not use ` around the whole multi-line query?
// ES is very particular about newlines and whitespace in general.
// The only unquoted whitespace allowed are newlines after json objects.
const esQuery = `{"ignore_unavailable":true}` + "\n" +
	`{"size":1,` +
	/**/ `"sort":{"Timestamp":"desc"},` +
	/**/ `"query":` +
	/**/ `{"filtered":` +
	/******/ `{"query":` +
	/*********/ `{"query_string":{"query":"*","analyze_wildcard":true}}` +
	/******/ "}}}\n"

const tsFormat = "2006-01-02T15:04:05"

type queryResults struct {
	Error     string `json:"error"`
	Responses []struct {
		Hits struct {
			Hits []resultHit `json:"hits"`
		} `json:"hits"`
	} `json:"responses"`
}

type resultHit struct {
	Source map[string]interface{} `json:"_source"`
	Sort   []int                  `json:"sort"`
}

var kvlog *kv.Logger
var sfxSink *sfxclient.HTTPDatapointSink

// Config vars
var esEndpoint, signalfxAPIKey, monitorName string

// getEnv looks up an environment variable given and exits if it does not exist.
func getEnv(envVar string) string {
	val := os.Getenv(envVar)
	if val == "" {
		log.Fatalf("Must specify env variable %s", envVar)
	}
	return val
}

func init() {
	esEndpoint = getEnv("ELASTICSEARCH_ENDPOINT")
	signalfxAPIKey = getEnv("SIGNALFX_API_KEY")
	monitorName = getEnv("MONITOR_NAME")

	sfxSink = sfxclient.NewHTTPDatapointSink()
	sfxSink.AuthToken = signalfxAPIKey

	kvlog = kv.New("log-monitor-es")
}

func getLatestTimestamp(esClient *http.Client) (time.Time, error) {
	reader := strings.NewReader(esQuery)
	res, err := esClient.Post(esEndpoint, "application/json", reader)
	if err != nil {
		return time.Time{}, err
	}

	var result queryResults
	decoder := json.NewDecoder(res.Body)
	if err := decoder.Decode(&result); err != nil {
		return time.Time{}, err
	}

	if res.StatusCode != 200 {
		return time.Time{}, fmt.Errorf("Error retrieving latest log: %s", result.Error)
	}

	if len(result.Responses) < 1 {
		return time.Time{}, fmt.Errorf("Error: no response from elastic search")
	}

	if len(result.Responses[0].Hits.Hits) < 1 {
		return time.Time{}, fmt.Errorf("Error: no results from elastic search")
	}

	source := result.Responses[0].Hits.Hits[0].Source
	timestamp, ok := source["Timestamp"]
	if !ok {
		return time.Time{}, fmt.Errorf("Error: no timestamp found on log line")
	}

	switch timestamp := timestamp.(type) {
	case string:
		return time.Parse(tsFormat, timestamp)
	default:
		return time.Time{}, fmt.Errorf("Error: timestamp incorrect type: %+#v", timestamp)
	}
}

func sendToSignalFX(timestamp time.Time) error {
	dimensions := map[string]string{"hostname": monitorName}

	datum := sfxclient.Gauge("log-monitor", dimensions, timestamp.Unix())
	points := []*datapoint.Datapoint{datum}

	return sfxSink.AddDatapoints(context.TODO(), points)
}

func main() {
	esClient := &http.Client{
		Timeout: 15 * time.Second,
	}

	tick := time.Tick(15 * time.Second)
	for {
		timestamp, err := getLatestTimestamp(esClient)
		if err != nil {
			kvlog.ErrorD("timestamp", kv.M{"error": err.Error()})
			continue
		}
		kvlog.InfoD("timestamp", kv.M{
			"timestamp": timestamp.Format(time.RFC3339),
			"delta-ms":  time.Now().Sub(timestamp) / time.Millisecond,
		})

		err = sendToSignalFX(timestamp)
		if err != nil {
			kvlog.ErrorD("send-to-signalfx", kv.M{"error": err.Error()})
			continue
		}
		kvlog.Info("sent-to-signalfx")

		<-tick
	}
}
