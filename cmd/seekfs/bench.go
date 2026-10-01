package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func benchResultHash(resp serviceResponse, countOnly bool) string {
	h := sha256.New()
	if countOnly {
		fmt.Fprintf(h, "count:%d\n", resp.Count)
	} else {
		for _, path := range resp.Results {
			h.Write([]byte(path))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func benchDiagnosticsFromResponse(resp serviceResponse) benchDiagnostics {
	complete := ""
	if resp.Complete != nil {
		complete = strconv.FormatBool(*resp.Complete)
	}
	return benchDiagnostics{
		Source:          resp.Source,
		Driver:          benchResponseDriver(resp),
		Candidates:      resp.Candidates,
		RecordsVerified: resp.RecordsVerified + resp.ComponentRecordsVerified + resp.FilenameRecordsVerified,
		BlocksDecoded:   resp.BlocksDecoded,
		BlocksSkipped:   resp.BlocksSkipped,
		Complete:        complete,
	}
}

func benchResponseDriver(resp serviceResponse) string {
	if resp.FilenameDriver != "" {
		return resp.FilenameDriver
	}
	if resp.ComponentDriver != "" {
		return resp.ComponentDriver
	}
	if resp.ScalarDriver != "" {
		return resp.ScalarDriver
	}
	return resp.PlannerMode
}

func sameBenchDiagnostics(a, b benchDiagnostics) bool {
	return a == b
}

func readBenchQueries(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func benchModeName(useService, useResident, countOnly bool) string {
	if useService {
		if countOnly {
			return "service-count"
		}
		return "service"
	}
	if useResident {
		return "resident-local"
	}
	return "local"
}

func recordBenchSource(global map[string]int, perQuery map[string]map[string]int, query string, source string) {
	if source == "" {
		source = "unknown"
	}
	global[source]++
	if perQuery[query] == nil {
		perQuery[query] = make(map[string]int)
	}
	perQuery[query][source]++
}

func recordBenchOptional(global map[string]int, perQuery map[string]map[string]int, query string, value string) {
	if value == "" {
		return
	}
	global[value]++
	if perQuery[query] == nil {
		perQuery[query] = make(map[string]int)
	}
	perQuery[query][value]++
}

func runBenchQuery(useService bool, useResident bool, countOnly bool, pipeName string, indexes []*Index, volumes []*serviceVolumeIndex, opts queryOptions, trace *searchTrace) error {
	if useService {
		_, err := benchServiceRequest(pipeName, opts, countOnly)
		return err
	}
	if useResident {
		opts.Trace = trace
		_, err := searchServiceVolumes(volumes, opts, false)
		return err
	}
	_, err := searchAll(indexes, opts, false)
	return err
}

func waitResidentBackgroundIndexes(volumes []*serviceVolumeIndex, maxWait time.Duration) {
	if maxWait <= 0 || len(volumes) == 0 {
		return
	}
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		allDone := true
		for _, vol := range volumes {
			if vol == nil {
				continue
			}
			nameOrderState := vol.nameOrderStateString()
			nameTrigramState := vol.nameTrigramStateString()
			if nameOrderState == "pending" || nameOrderState == "building" ||
				nameTrigramState == "pending" || nameTrigramState == "building" {
				allDone = false
				break
			}
		}
		if allDone {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func benchServiceQuery(pipeName string, opts queryOptions) (serviceResponse, error) {
	return benchServiceRequest(pipeName, opts, false)
}

func benchServiceRequest(pipeName string, opts queryOptions, countOnly bool) (serviceResponse, error) {
	resp, err := callService(pipeName, serviceRequestFromOptions(opts, countOnly))
	if err != nil {
		return serviceResponse{}, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Message)
	}
	return resp, nil
}

func latencyStats(values []float64) map[string]float64 {
	stats := map[string]float64{"min": 0, "median": 0, "p90": 0, "p95": 0, "p99": 0, "max": 0}
	if len(values) == 0 {
		return stats
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	stats["min"] = sorted[0]
	stats["median"] = percentile(sorted, 0.50)
	stats["p90"] = percentile(sorted, 0.90)
	stats["p95"] = percentile(sorted, 0.95)
	stats["p99"] = percentile(sorted, 0.99)
	stats["max"] = sorted[len(sorted)-1]
	return stats
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

type serviceRequest struct {
	Command       string              `json:"command"`
	PluginConfig  string              `json:"plugin_config,omitempty"`
	Volume        string              `json:"volume,omitempty"`
	DB            string              `json:"db,omitempty"`
	Query         string              `json:"query,omitempty"`
	MatchPath     bool                `json:"match_path,omitempty"`
	Limit         int                 `json:"limit,omitempty"`
	CountOnly     bool                `json:"count_only,omitempty"`
	Under         string              `json:"under,omitempty"`
	Exists        bool                `json:"exists,omitempty"`
	CWDBias       string              `json:"cwd_bias,omitempty"`
	RootBias      string              `json:"root_bias,omitempty"`
	Recent        string              `json:"recent,omitempty"`
	ModifiedAfter string              `json:"modified_after,omitempty"`
	CaseSensitive bool                `json:"case_sensitive,omitempty"`
	Fuzzy         bool                `json:"fuzzy,omitempty"`
	DeadlineUnix  int64               `json:"deadline_unix,omitempty"`
	RequestSeq    int64               `json:"request_seq,omitempty"`
	SinceVolumes  []watchVolumeCursor `json:"since_volumes,omitempty"`
	Baseline      bool                `json:"baseline,omitempty"`
	// CancelOverride is a transport-injected cancellation predicate used by the
	// remote transport for connection-scoped cancellation.  It never crosses
	// the wire and is never set from client-supplied input.
	CancelOverride func() bool `json:"-"`
}
