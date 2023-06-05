package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/prometheus/alertmanager/api/v2/client"
	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/prometheus/alertmanager/cli"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"
	"io"
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
	scanExecuted  = promauto.NewCounter(prometheus.CounterOpts{
		Name: "silence_scanner_executed_total",
		Help: "Number of scanner executed",
	})
	scanTimeTaken = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "silence_scanner_time_taken_seconds",
		Help: "Total seconds taken by silence scanner",
	})
	scanDuration = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "silence_scanner_interval",
		Help: "Time interval of silence scanner",
	})
	lastScanChanged = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "silence_scanner_last_change",
		Help: "Timestamp of last scan change",
	})
)

type Scanner struct {
	alertmanagerURL []*url.URL
	webhookUrl      url.URL
	persistence     bool
	s3Endpoint      string
	s3Secure        bool
	s3Bucket        string
	s3SilencePath   string
	s3SecretID      string
	s3SecretKey     string
	logger          log.Logger
}

func main() {
	var (
		webConfig       = kingpinflag.AddFlags(kingpin.CommandLine, ":9100")
		metricsPath     = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		alertmanagerURL = kingpin.Flag("alertmanager-url", "Alertmanager url, seperated with comma").Required().URLList()
		refreshInterval = kingpin.Flag("refresh-interval",
			"refresh interval of alertmanager silence").Default("10s").String()
		webhookUrl  = kingpin.Flag("webhook-url", "webhook url where silence would be post to").Required().URL()
		persistence = kingpin.Flag("persistence", "enable persistence to avoid data loss when program restarts").Bool()
		// using s3 as persistence
		s3Endpoint    = kingpin.Flag("s3.endpoint", "s3 endpoint used by persistence, no protocol, eg: localhost:9090").String()
		s3Secure      = kingpin.Flag("s3.secure", "is s3 endpoint secured, if secured, https will be used").Default("false").Bool()
		s3Bucket      = kingpin.Flag("s3.bucket", "s3 bucket used by persistence").String()
		s3SilencePath = kingpin.Flag("s3.path", "path that silence data is stored").Default("silences.json").String()
		// set this in parameter, or pickup AWS_ACCESS_KEY_ID and AWS_ACCESS_KEY by aws sdk
		s3SecretID  = kingpin.Flag("s3.secret-id", "s3 secret id used by persistence").Envar("AWS_ACCESS_KEY_ID").String()
		s3SecretKey = kingpin.Flag("s3.secret-key", "s3 secret key used by persistence").Envar("AWS_ACCESS_KEY").String()
	)
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("silence-Scanner"))
	kingpin.CommandLine.UsageWriter(os.Stdout)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(promlogConfig)
	globalLogger = logger
	duration, err := time.ParseDuration(*refreshInterval)
	if err != nil {
		_ = level.Error(logger).Log("message", "failed to parse duration", "detail", err)
		os.Exit(1)
	}
	scanDuration.Set(duration.Abs().Seconds())
	silenceScanner := Scanner{
		alertmanagerURL: *alertmanagerURL,
		webhookUrl:      **webhookUrl,
		persistence:     *persistence,
		s3Endpoint:      *s3Endpoint,
		s3Secure:        *s3Secure,
		s3Bucket:        *s3Bucket,
		s3SilencePath:   *s3SilencePath,
		s3SecretID:      *s3SecretID,
		s3SecretKey:     *s3SecretKey,
		logger:          logger,
	}
	silenceScanner.run()
	ticker := time.NewTicker(duration)
	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			case _ = <-ticker.C:
				now := time.Now()
				silenceScanner.run()
				scanTimeTaken.Add(time.Now().Sub(now).Seconds())
			}
		}
	}()
	http.Handle(*metricsPath, promhttp.Handler())
	server := &http.Server{}
	if err := web.ListenAndServe(server, webConfig, logger); err != nil {
		_ = level.Error(logger).Log("err", err)
		os.Exit(1)
	}
}

func getAMClient(servers *[]*url.URL, logger log.Logger) (*client.AlertmanagerAPI, error) {
	if AMClient != nil {
		return AMClient, nil
	}
	for _, u := range *servers {
		_ = level.Info(logger).Log("msg", "checking am", "server", u)
		c := cli.NewAlertmanagerClient(u)
		status, err := c.General.GetStatus(nil)
		if err != nil || *status.Payload.Cluster.Status != "ready" {
			// 寻找下一个
			_ = level.Warn(logger).Log("msg", "connect am failed", "error", err, "am-status", status)
			continue
		}
		AMClient = c
		return c, nil
	}
	return nil, errors.New("no valid alertmanager found")
}

func (s *Scanner) run() {
	lock.Lock()
	defer lock.Unlock()
	scanExecuted.Inc()
	c, err := getAMClient(&s.alertmanagerURL, s.logger)
	if err != nil {
		_ = level.Error(s.logger).Log("msg", "failed to get alertmanager client, all servers cannot be reachable",
			"error", err)
		os.Exit(1)
	}
	silences, err := c.Silence.GetSilences(nil)
	if err != nil {
		_ = level.Error(s.logger).Log("msg", "failed to get silence",
			"error", err)
		os.Exit(1)
	}
	var currentSilences models.GettableSilences
	for _, silence := range silences.Payload {
		if *silence.Status.State != "active" {
			continue
		}
		currentSilences = append(currentSilences, silence)
	}
	_ = level.Debug(s.logger).Log("msg", "printing all silences")
	for _, silence := range currentSilences {
		j, _ := silence.MarshalJSON()
		_ = level.Debug(s.logger).Log("silence", j)
	}
	s3Client, err := getS3Client(s.s3SecretID, s.s3SecretKey, s.s3Endpoint, s.s3Secure)
	if err != nil {
		_ = level.Error(s.logger).Log("msg", "failed to get s3 client", "error", err)
	}
	oldSilence := s.getOldSilence(s3Client)
	newSilences := getNewSilence(&oldSilence, &currentSilences)
	if newSilences == nil {
		_ = level.Info(s.logger).Log("msg", "no new silences found in this round")
		return
	}
	lastScanChanged.SetToCurrentTime()
	_ = level.Info(s.logger).Log("msg", "found new silences")
	for _, silence := range newSilences {
		j, _ := silence.MarshalJSON()
		_ = level.Info(s.logger).Log("uuid", silence.ID, silence.Status, silence.Comment)
		_ = level.Debug(s.logger).Log("silence", j)
	}
	data, err := json.Marshal(newSilences)
	if err != nil {
		_ = level.Error(s.logger).Log("msg", "failed to marshal silences")
		return
	}
	response, err := http.Post(s.webhookUrl.String(), "application/json", bytes.NewBuffer(data))
	if err != nil {
		_ = level.Error(s.logger).Log("msg", "failed to push silences")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = level.Error(s.logger).Log("msg", "webhook return code not ok")
		return
	}
	s.storeSilences(s3Client, &currentSilences)
}

func (s *Scanner) getOldSilence(s3Client *minio.Client) models.GettableSilences {
	if !s.persistence {
		return ActiveSilence
	}
	reader, err := s3Client.GetObject(context.Background(), s.s3Bucket, s.s3SilencePath, minio.GetObjectOptions{})
	if err != nil {
		_ = level.Error(globalLogger).Log("msg", "get object error", "error", err)
		return nil
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		_ = level.Error(globalLogger).Log("msg", "failed to read data from s3", "error", err)
		return nil
	}
	level.Debug(s.logger).Log("msg", "got data from s3", "silences", string(data))
	var silences models.GettableSilences
	err = json.Unmarshal(data, &silences)
	if err != nil {
		_ = level.Error(globalLogger).Log("msg", "error unmarshal silence from s3", "error", err)
		return nil
	}
	return silences
}

func (s *Scanner) storeSilences(s3Client *minio.Client, silences *models.GettableSilences) {
	if !s.persistence {
		ActiveSilence = *silences
	}
	// marshal
	data, err := json.Marshal(silences)
	if err != nil {
		_ = level.Error(globalLogger).Log("msg", "failed to marsha silence", "error", err)
	}
	reader := bytes.NewReader(data)
	dataSize := len(data)
	_, err = s3Client.PutObject(context.Background(), s.s3Bucket, s.s3SilencePath, reader, int64(dataSize), minio.PutObjectOptions{})
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
	_ = level.Debug(globalLogger).Log("old silence", flattenOld)
	_ = level.Debug(globalLogger).Log("new silence", flattenNew)
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
		_ = level.Error(globalLogger).Log("msg", "failed to marshal silence",
			"error", err)
		os.Exit(1)
	}
	return string(f)
}

func getS3Client(s3secretId string, s3secretKey string, s3endpoint string, s3Secure bool) (*minio.Client, error) {
	if s3secretId == "" || s3secretKey == "" {
		_ = level.Error(globalLogger).Log("msg", "persistence enabled but secret id and secret key not set")
		os.Exit(1)
	}
	s3Client, err := minio.New(s3endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s3secretId, s3secretKey, ""),
		Secure: s3Secure,
	})
	if err != nil {
		_ = level.Error(globalLogger).Log("msg", "init s3 client failed", "error", err)
	}
	return s3Client, err
}
