package main

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Frogeye's raw lists shrank below the historical floors in November 2026.
// The hosts variants contain the same domains plus generation statistics:
// validate those instead of trusting a smaller unannotated response. The
// operator-approved floors remain an independent check against feed loss.
// Upstream format: git.frogeye.fr/geoffrey/eulaurarien, export_lists.sh.
func parseFrogeye(data []byte, variant string, now time.Time) parsed {
	var p parsed
	fail := func(reason string) parsed {
		p.err = fmt.Errorf("Frogeye integrity: %s", reason)
		return p
	}
	metadata := map[string]string{}
	required := []string{"Variant", "List generation date", "Oldest record", "Number of source subdomains", "Number of source DNS records", "Output rules"}
	seen := map[string]bool{}
	title := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "# First-party trackers host list" {
			if title {
				return fail("duplicate title")
			}
			title = true
		}
		if strings.HasPrefix(line, "#") {
			key, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#")), ":")
			if ok {
				for _, requiredKey := range required {
					if key == requiredKey {
						if _, exists := metadata[key]; exists {
							return fail("duplicate metadata: " + key)
						}
						metadata[key] = strings.TrimSpace(value)
					}
				}
			}
			continue
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "0.0.0.0" {
			return fail("expected one null-routed domain per line")
		}
		host, ok := normalizeHost(fields[1])
		if !ok || host != fields[1] || seen[host] {
			return fail("invalid, noncanonical or duplicate domain")
		}
		seen[host] = true
		p.hosts = append(p.hosts, host)
	}
	if err := sc.Err(); err != nil {
		return fail("reading response: " + err.Error())
	}
	if !title || metadata["Variant"] != variant {
		return fail("missing title or unexpected variant")
	}
	for _, key := range required {
		if metadata[key] == "" {
			return fail("missing metadata: " + key)
		}
	}
	timestamps := map[string]time.Time{}
	for _, key := range []string{"List generation date", "Oldest record"} {
		timestamp, err := time.Parse(time.RFC3339, metadata[key])
		if err != nil || timestamp.Before(now.Add(-14*24*time.Hour)) || timestamp.After(now.Add(24*time.Hour)) {
			return fail("invalid, stale or future timestamp: " + key)
		}
		timestamps[key] = timestamp
	}
	if timestamps["Oldest record"].After(timestamps["List generation date"]) {
		return fail("oldest record is newer than generation")
	}
	for _, key := range []string{"Number of source subdomains", "Number of source DNS records"} {
		count, err := strconv.Atoi(metadata[key])
		if err != nil || count < 1_000_000 {
			return fail("insufficient source volume: " + key)
		}
	}
	countText, ok := strings.CutPrefix(metadata["Output rules"], "hostnames: ")
	count, err := strconv.Atoi(countText)
	if !ok || err != nil || count != len(p.hosts) || count == 0 {
		return fail("declared output count does not match unique domains")
	}
	return p
}
