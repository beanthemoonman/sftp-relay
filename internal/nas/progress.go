package nas

import (
	"regexp"
	"strconv"
	"strings"
)

// Progress is one snapshot scraped from a line of lftp output.
type Progress struct {
	Bytes      int64 // bytes transferred so far, as lftp reports them
	Total      int64 // total bytes, when the line carries one (0 otherwise)
	Percent    int   // -1 when the line carries no percentage
	SpeedBPS   int64
	ETASeconds int64
	Final      bool // a "N bytes transferred in ..." summary line
}

// lftp's output format drifts between versions, so each field is matched on its
// own rather than trying to match a whole line. Between them these cover the
// 4.8-era `at N (P%)` form and the 4.9-era `got N of M` form.
var (
	reGotOf     = regexp.MustCompile(`got\s+(\d+)\s+of\s+(\d+)`)
	reAt        = regexp.MustCompile(`\sat\s+(\d+)\b`)
	rePercent   = regexp.MustCompile(`\((\d+)%\)`)
	reSpeed     = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*([kKmMgGtT]i?)?[bB]?/s`)
	reSummary   = regexp.MustCompile(`(\d+)\s+bytes\s+transferred`)
	reETA       = regexp.MustCompile(`eta:\s*([0-9dhms:]+)`)
	reETAUnits  = regexp.MustCompile(`(\d+)([dhms])`)
	scaleFactor = map[string]int64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}
)

// ParseProgress extracts what it can from a single output line. The second
// return is false when the line carries nothing useful — most lines do not.
func ParseProgress(line string) (Progress, bool) {
	p := Progress{Percent: -1}
	found := false

	if m := reSummary.FindStringSubmatch(line); m != nil {
		p.Bytes = atoi64(m[1])
		p.Final = true
		found = true
	}
	if m := reGotOf.FindStringSubmatch(line); m != nil {
		p.Bytes, p.Total = atoi64(m[1]), atoi64(m[2])
		found = true
	} else if m := reAt.FindStringSubmatch(line); m != nil {
		p.Bytes = atoi64(m[1])
		found = true
	}
	if m := rePercent.FindStringSubmatch(line); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil && v >= 0 && v <= 100 {
			p.Percent = v
			found = true
		}
	}
	if m := reSpeed.FindStringSubmatch(line); m != nil {
		p.SpeedBPS = parseSpeed(m[1], m[2])
		found = true
	}
	if m := reETA.FindStringSubmatch(line); m != nil {
		p.ETASeconds = parseETA(m[1])
		found = true
	}
	return p, found
}

func atoi64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseSpeed turns "1.5", "M" into bytes per second. lftp's units are binary
// and it writes a bare "b/s" for bytes per second.
func parseSpeed(num, unit string) int64 {
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	scale := scaleFactor[strings.ToLower(strings.TrimSuffix(unit, "i"))]
	if scale == 0 {
		scale = 1
	}
	return int64(v * float64(scale))
}

// parseETA understands both "1h20m" and "01:20" shapes.
func parseETA(s string) int64 {
	if strings.Contains(s, ":") {
		var total int64
		for _, part := range strings.Split(s, ":") {
			total = total*60 + atoi64(part)
		}
		return total
	}
	var total int64
	for _, m := range reETAUnits.FindAllStringSubmatch(s, -1) {
		n := atoi64(m[1])
		switch m[2] {
		case "d":
			total += n * 86400
		case "h":
			total += n * 3600
		case "m":
			total += n * 60
		default:
			total += n
		}
	}
	return total
}
