// Command coveragecheck fails when Go statement coverage is below a minimum.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

type coverage struct {
	Covered int
	Total   int
}

func (c coverage) Percent() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Covered) * 100 / float64(c.Total)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("coveragecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "coverage.out", "Go coverprofile to check")
	min := fs.Float64("min", 70, "minimum total statement coverage percentage")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if math.IsNaN(*min) || *min < 0 || *min > 100 {
		fmt.Fprintf(stderr, "coveragecheck: -min must be between 0 and 100\n")
		return 2
	}

	cov, err := check(*profile, *min)
	if err != nil {
		fmt.Fprintf(stderr, "coveragecheck: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "coveragecheck: %.2f%% statement coverage (%d/%d), minimum %.2f%%\n",
		cov.Percent(), cov.Covered, cov.Total, *min)
	return 0
}

func check(path string, min float64) (coverage, error) {
	f, err := os.Open(path)
	if err != nil {
		return coverage{}, fmt.Errorf("open profile: %w", err)
	}
	defer f.Close()

	cov, err := parseProfile(f)
	if err != nil {
		return coverage{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cov.Percent() < min {
		return cov, fmt.Errorf("statement coverage %.2f%% (%d/%d) is below minimum %.2f%%",
			cov.Percent(), cov.Covered, cov.Total, min)
	}
	return cov, nil
}

func parseProfile(r io.Reader) (coverage, error) {
	scanner := bufio.NewScanner(r)
	lineNo := 0
	modeSeen := false
	var cov coverage

	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "mode:") {
			if modeSeen {
				return coverage{}, fmt.Errorf("line %d: duplicate mode line", lineNo)
			}
			switch strings.TrimSpace(strings.TrimPrefix(line, "mode:")) {
			case "set", "count", "atomic":
			default:
				return coverage{}, fmt.Errorf("line %d: invalid coverage mode", lineNo)
			}
			modeSeen = true
			continue
		}
		if !modeSeen {
			return coverage{}, fmt.Errorf("line %d: coverage block before mode line", lineNo)
		}

		fields := strings.Fields(line)
		if len(fields) != 3 || !strings.Contains(fields[0], ":") {
			return coverage{}, fmt.Errorf("line %d: malformed coverage block", lineNo)
		}
		stmts, err := strconv.Atoi(fields[1])
		if err != nil || stmts < 0 {
			return coverage{}, fmt.Errorf("line %d: invalid statement count", lineNo)
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil || count < 0 {
			return coverage{}, fmt.Errorf("line %d: invalid execution count", lineNo)
		}
		cov.Total += stmts
		if count > 0 {
			cov.Covered += stmts
		}
	}
	if err := scanner.Err(); err != nil {
		return coverage{}, err
	}
	if !modeSeen {
		return coverage{}, fmt.Errorf("missing mode line")
	}
	if cov.Total == 0 {
		return coverage{}, fmt.Errorf("profile contains no statements")
	}
	return cov, nil
}
