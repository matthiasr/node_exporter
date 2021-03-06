// +build !nomdadm

package collector

import (
	"fmt"
	"io/ioutil"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"
)

var (
	statusfile   = "/proc/mdstat"
	statuslineRE = regexp.MustCompile(`(\d+) blocks .*\[(\d+)/(\d+)\] \[[U_]+\]`)
	buildlineRE  = regexp.MustCompile(`\((\d+)/\d+\)`)
)

type mdStatus struct {
	mdName       string
	isActive     bool
	disksActive  int64
	disksTotal   int64
	blocksTotal  int64
	blocksSynced int64
}

type mdadmCollector struct{}

func init() {
	Factories["mdadm"] = NewMdadmCollector
}

func evalStatusline(statusline string) (active, total, size int64, err error) {
	matches := statuslineRE.FindStringSubmatch(statusline)

	// +1 to make it more obvious that the whole string containing the info is also returned as matches[0].
	if len(matches) < 3+1 {
		return 0, 0, 0, fmt.Errorf("too few matches found in statusline: %s", statusline)
	} else {
		if len(matches) > 3+1 {
			return 0, 0, 0, fmt.Errorf("too many matches found in statusline: %s", statusline)
		}
	}

	size, err = strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s in statusline: %s", err, statusline)
	}

	total, err = strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s in statusline: %s", err, statusline)
	}
	active, err = strconv.ParseInt(matches[3], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s in statusline: %s", err, statusline)
	}

	return active, total, size, nil
}

// Gets the size that has already been synced out of the sync-line.
func evalBuildline(buildline string) (int64, error) {
	matches := buildlineRE.FindStringSubmatch(buildline)

	// +1 to make it more obvious that the whole string containing the info is also returned as matches[0].
	if len(matches) < 1+1 {
		return 0, fmt.Errorf("too few matches found in buildline: %s", buildline)
	}

	if len(matches) > 1+1 {
		return 0, fmt.Errorf("too many matches found in buildline: %s", buildline)
	}

	syncedSize, err := strconv.ParseInt(matches[1], 10, 64)

	if err != nil {
		return 0, fmt.Errorf("%s in buildline: %s", err, buildline)
	}

	return syncedSize, nil
}

// Parses an mdstat-file and returns a struct with the relevant infos.
func parseMdstat(mdStatusFilePath string) ([]mdStatus, error) {
	content, err := ioutil.ReadFile(mdStatusFilePath)
	if err != nil {
		return []mdStatus{}, fmt.Errorf("error parsing %s: %s", statusfile, err)
	}

	mdStatusFile := string(content)

	lines := strings.Split(mdStatusFile, "\n")
	var currentMD string

	// Each md has at least the deviceline, statusline and one empty line afterwards
	// so we will have probably something of the order len(lines)/3 devices
	// so we use that for preallocation.
	estimateMDs := len(lines) / 3
	mdStates := make([]mdStatus, 0, estimateMDs)

	for i, l := range lines {
		if l == "" {
			// Skip entirely empty lines.
			continue
		}

		if l[0] == ' ' {
			// Those lines are not the beginning of a md-section.
			continue
		}

		if strings.HasPrefix(l, "Personalities") || strings.HasPrefix(l, "unused") {
			// We aren't interested in lines with general info.
			continue
		}

		mainLine := strings.Split(l, " ")
		if len(mainLine) < 3 {
			return mdStates, fmt.Errorf("error parsing mdline: %s", l)
		}
		currentMD = mainLine[0]               // name of md-device
		isActive := (mainLine[2] == "active") // activity status of said md-device

		if len(lines) <= i+3 {
			return mdStates, fmt.Errorf("error parsing %s: entry for %s has fewer lines than expected", statusfile, currentMD)
		}

		active, total, size, err := evalStatusline(lines[i+1]) // parse statusline, always present

		if err != nil {
			return mdStates, fmt.Errorf("error parsing %s: %s", statusfile, err)
		}

		// Now get the number of synced blocks.
		var syncedBlocks int64

		// Get the line number of the syncing-line.
		var j int
		if strings.Contains(lines[i+2], "bitmap") { // then skip the bitmap line
			j = i + 3
		} else {
			j = i + 2
		}

		// If device is syncing at the moment, get the number of currently synced bytes,
		// otherwise that number equals the size of the device.
		if strings.Contains(lines[j], "recovery") || strings.Contains(lines[j], "resync") {
			syncedBlocks, err = evalBuildline(lines[j])
			if err != nil {
				return mdStates, fmt.Errorf("error parsing %s: %s", statusfile, err)
			}
		} else {
			syncedBlocks = size
		}

		mdStates = append(mdStates, mdStatus{currentMD, isActive, active, total, size, syncedBlocks})

	}

	return mdStates, nil
}

// Just returns the pointer to an empty struct as we only use throwaway-metrics.
func NewMdadmCollector() (Collector, error) {
	return &mdadmCollector{}, nil
}

var (
	isActiveDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "md", "is_active"),
		"Indicator whether the md-device is active or not.",
		[]string{"device"},
		nil,
	)

	disksActiveDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "md", "disks_active"),
		"Number of active disks of device.",
		[]string{"device"},
		nil,
	)

	disksTotalDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "md", "disks"),
		"Total number of disks of device.",
		[]string{"device"},
		nil,
	)

	blocksTotalDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "md", "blocks"),
		"Total number of blocks on device.",
		[]string{"device"},
		nil,
	)

	blocksSyncedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, "md", "blocks_synced"),
		"Number of blocks synced on device.",
		[]string{"device"},
		nil,
	)
)

func (c *mdadmCollector) Update(ch chan<- prometheus.Metric) (err error) {
	// take care we don't crash on non-existent statusfiles
	_, err = os.Stat(statusfile)
	if os.IsNotExist(err) {
		// no such file or directory, nothing to do, just return
		return nil
	}

	if err != nil { // now things get weird, better to return
		return err
	}

	// First parse mdstat-file...
	mdstate, err := parseMdstat(statusfile)
	if err != nil {
		return fmt.Errorf("error parsing %s: %s", statusfile, err)
	}

	// ... and then plug the result into the metrics to be exported.
	var isActiveFloat float64
	for _, mds := range mdstate {

		log.Debugf("collecting metrics for device %s", mds.mdName)

		if mds.isActive {
			isActiveFloat = 1
		} else {
			isActiveFloat = 0
		}

		ch <- prometheus.MustNewConstMetric(
			isActiveDesc,
			prometheus.GaugeValue,
			isActiveFloat,
			mds.mdName,
		)

		ch <- prometheus.MustNewConstMetric(
			disksActiveDesc,
			prometheus.GaugeValue,
			float64(mds.disksActive),
			mds.mdName,
		)

		ch <- prometheus.MustNewConstMetric(
			disksTotalDesc,
			prometheus.GaugeValue,
			float64(mds.disksTotal),
			mds.mdName,
		)

		ch <- prometheus.MustNewConstMetric(
			blocksTotalDesc,
			prometheus.GaugeValue,
			float64(mds.blocksTotal),
			mds.mdName,
		)

		ch <- prometheus.MustNewConstMetric(
			blocksSyncedDesc,
			prometheus.GaugeValue,
			float64(mds.blocksSynced),
			mds.mdName,
		)

	}

	return nil
}
