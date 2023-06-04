package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/alertmanager/api/v2/client"
	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/prometheus/alertmanager/cli"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"
)

var (
	AMClient      *client.AlertmanagerAPI
	ActiveSilence models.GettableSilences
	globalLogger  log.Logger
	lock          = new(sync.Mutex)
)

func main() {
	var (
		webConfig       = kingpinflag.AddFlags(kingpin.CommandLine, ":9100")
		metricsPath     = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		alertmanagerURL = kingpin.Flag("alertmanager-url", "Alertmanager url, seperated with comma").Required().URLList()
		refreshInterval = kingpin.Flag("refresh-interval",
			"refresh interval of alertmanager silence").Default("10s").String()
		webhookUrl = kingpin.Flag("webhook-url", "webhook url where silence would be post to").Required().URL()
	)
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("silence-scanner"))
	kingpin.CommandLine.UsageWriter(os.Stdout)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(promlogConfig)
	globalLogger = logger
	duration, err := time.ParseDuration(*refreshInterval)
	if err != nil {
		level.Error(logger).Log("message", "failed to parse duration", "detail", err)
		os.Exit(1)
	}
	scanSilence(alertmanagerURL, logger, **webhookUrl)
	ticker := time.NewTicker(duration)
	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			case _ = <-ticker.C:
				scanSilence(alertmanagerURL, logger, **webhookUrl)
			}
		}
	}()
	http.Handle(*metricsPath, promhttp.Handler())
	server := &http.Server{}
	if err := web.ListenAndServe(server, webConfig, logger); err != nil {
		level.Error(logger).Log("err", err)
		os.Exit(1)
	}
}

func getAMClient(servers *[]*url.URL, logger log.Logger) (*client.AlertmanagerAPI, error) {
	if AMClient != nil {
		return AMClient, nil
	}
	for _, u := range *servers {
		level.Info(logger).Log("msg", "checking am", "server", u)
		c := cli.NewAlertmanagerClient(u)
		status, err := c.General.GetStatus(nil)
		if err != nil || *status.Payload.Cluster.Status != "ready" {
			// 寻找下一个
			level.Warn(logger).Log("msg", "connect am failed", "error", err, "am-status", status)
			continue
		}
		AMClient = c
		return c, nil
	}
	return nil, errors.New("no valid alertmanager found")
}

func scanSilence(servers *[]*url.URL, logger log.Logger, webhookUrl url.URL) {
	lock.Lock()
	defer lock.Unlock()
	c, err := getAMClient(servers, logger)
	if err != nil {
		level.Error(logger).Log("msg", "failed to get alertmanager client, all servers cannot be reachable",
			"error", err)
		os.Exit(1)
	}
	silences, err := c.Silence.GetSilences(nil)
	if err != nil {
		level.Error(logger).Log("msg", "failed to get silence",
			"error", err)
		os.Exit(1)
	}
	var currentSilences models.GettableSilences
	for _, s := range silences.Payload {
		if *s.Status.State != "active" {
			continue
		}
		currentSilences = append(currentSilences, s)
	}
	level.Debug(logger).Log("msg", "printing all silences")
	for _, s := range currentSilences {
		j, _ := s.MarshalJSON()
		level.Debug(logger).Log("silence", j)
	}
	newSilences := getNewSilence(&ActiveSilence, &currentSilences)
	if newSilences == nil {
		level.Info(logger).Log("msg", "no new silences found in this round")
		return
	}
	level.Info(logger).Log("msg", "found new silences")
	for _, s := range newSilences {
		j, _ := s.MarshalJSON()
		level.Info(logger).Log("uuid", s.ID, s.Status, s.Comment)
		level.Debug(logger).Log("silence", j)
	}
	ActiveSilence = currentSilences
	data, err := json.Marshal(newSilences)
	if err != nil {
		level.Error(logger).Log("msg", "failed to marshal silences")
		return
	}
	response, err := http.Post(webhookUrl.String(), "application/json", bytes.NewBuffer(data))
	if err != nil {
		level.Error(logger).Log("msg", "failed to push silences")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		level.Error(logger).Log("msg", "webhook return code not ok")
		return
	}
}

type ByID models.GettableSilences

func (a ByID) Len() int           { return len(a) }
func (a ByID) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByID) Less(i, j int) bool { return *a[i].ID < *a[j].ID }

func getNewSilence(old *models.GettableSilences, new *models.GettableSilences) models.GettableSilences {
	var newSilences models.GettableSilences
	sort.Sort(ByID(*old))
	sort.Sort(ByID(*new))
	flattenOld := flattenSilences(*old)
	flattenNew := flattenSilences(*new)
	if flattenOld == flattenNew {
		return nil
	}
	level.Info(globalLogger).Log("old silence", flattenOld)
	level.Info(globalLogger).Log("new silence", flattenNew)
	for _, s := range *new {
		isNewSilence := true
		for _, s2 := range *old {
			if *s.ID == *s2.ID {
				isNewSilence = false
				break
			}
		}
		if isNewSilence {
			newSilences = append(newSilences, s)
		}
	}
	return newSilences
}

func flattenSilences(silences models.GettableSilences) string {
	f, err := json.Marshal(silences)
	if err != nil {
		level.Error(globalLogger).Log("msg", "failed to marshal silence",
			"error", err)
		os.Exit(1)
	}
	return string(f)
}
