package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
)

// TraceSummary is the curated view of a Playwright trace.zip we ship to the agent
// AND persist as ordered steps. Playwright's full trace can be megabytes — this is
// an opinionated, agent-friendly slice.
type TraceSummary struct {
	Steps    []TraceStep    `json:"steps"`
	Console  []TraceConsole `json:"console"`
	Network  []TraceNetwork `json:"network"`
	PageURLs []string       `json:"page_urls,omitempty"`
}

// TraceStep is one user-visible action from the trace (page.goto, locator.click, expect, etc.)
// Times are in seconds, relative to the first action in the trace.
type TraceStep struct {
	Idx        int     `json:"idx"`
	StartS     float64 `json:"start_s"`
	DurationMS int     `json:"duration_ms"`
	APIName    string  `json:"api_name"` // e.g. "page.goto", "locator.click"
	Selector   string  `json:"selector,omitempty"`
	URL        string  `json:"url,omitempty"`
	Status     string  `json:"status"` // "passed" | "failed"
	Error      string  `json:"error,omitempty"`
}

type TraceConsole struct {
	Time  float64 `json:"time_s"`
	Level string  `json:"level"`
	Text  string  `json:"text"`
}

type TraceNetwork struct {
	Time   float64 `json:"time_s"`
	Method string  `json:"method"`
	URL    string  `json:"url"`
	Status int     `json:"status,omitempty"`
	Failed bool    `json:"failed,omitempty"`
}

// parseTraceZip walks a Playwright trace.zip and extracts a structured summary.
// Trace events of interest in the v7 format:
//
//	{type:"before", callId, startTime, apiName, class, method, params:{selector,url}}
//	{type:"after",  callId, endTime, error:{message}}
//	{type:"log",    time,  message}
//	{type:"event",  class:"Frame", method:"navigated", params:{url}}
//
// All times are floats in milliseconds, relative to context start.
func parseTraceZip(path string) (*TraceSummary, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open trace: %w", err)
	}
	defer zr.Close()

	type pending struct {
		startTime float64
		apiName   string
		selector  string
		url       string
	}
	stepByCallID := map[string]*TraceStep{}
	pendingByCallID := map[string]*pending{}
	var steps []*TraceStep
	var consoles []TraceConsole
	pageURLSet := map[string]bool{}
	var minStartMS float64
	var minStartSet bool

	// Trace events: walk every *.trace entry (test.trace + per-context 0-trace.trace, etc).
	// If Playwright changes its trace format we may stop recognising events; we count
	// parse failures so a bump in skip count signals "go look at the wire format."
	var skippedLines int
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		if !strings.HasSuffix(name, ".trace") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				skippedLines++
				continue
			}
			switch ev["type"] {
			case "before":
				callID, _ := ev["callId"].(string)
				st, _ := ev["startTime"].(float64)
				apiName, _ := ev["apiName"].(string)
				// Skip uninteresting framework-internal calls.
				if isInternalCall(apiName) {
					continue
				}
				params, _ := ev["params"].(map[string]any)
				p := &pending{startTime: st, apiName: apiName}
				if params != nil {
					if s, ok := params["selector"].(string); ok {
						p.selector = trimSelector(s)
					}
					if u, ok := params["url"].(string); ok {
						p.url = u
						pageURLSet[u] = true
					}
				}
				pendingByCallID[callID] = p
				if !minStartSet || st < minStartMS {
					minStartMS = st
					minStartSet = true
				}
			case "after":
				callID, _ := ev["callId"].(string)
				p, ok := pendingByCallID[callID]
				if !ok {
					continue
				}
				delete(pendingByCallID, callID)
				et, _ := ev["endTime"].(float64)
				durMS := int(et - p.startTime)
				if durMS < 0 {
					durMS = 0
				}
				step := &TraceStep{
					StartS:     p.startTime / 1000.0,
					DurationMS: durMS,
					APIName:    p.apiName,
					Selector:   p.selector,
					URL:        p.url,
					Status:     "passed",
				}
				if errObj, ok := ev["error"].(map[string]any); ok && errObj != nil {
					if msg, _ := errObj["message"].(string); msg != "" {
						step.Status = "failed"
						step.Error = truncateLine(stripANSI(msg), 600)
					}
				}
				stepByCallID[callID] = step
				steps = append(steps, step)
			case "log":
				// Console events have type=log with method-ish fields. We capture only the
				// browser-side console messages, not Playwright's internal action logs.
				// The internal log lines carry no `messageType`; the browser console events do.
				lvl, _ := ev["messageType"].(string)
				if lvl == "" {
					continue
				}
				t, _ := ev["time"].(float64)
				text, _ := ev["message"].(string)
				consoles = append(consoles, TraceConsole{
					Time: t / 1000.0, Level: lvl, Text: truncateLine(text, 400),
				})
			case "event":
				if class, _ := ev["class"].(string); class == "Frame" {
					if params, ok := ev["params"].(map[string]any); ok {
						if u, ok := params["url"].(string); ok {
							pageURLSet[u] = true
						}
					}
				}
			}
		}
	}

	if skippedLines > 0 {
		slog.Warn("trace parser: lines failed to JSON-decode (Playwright trace format may have changed)", "skipped", skippedLines, "path", path)
	}

	// Normalize start times to be relative to the first action.
	if minStartSet {
		for _, s := range steps {
			s.StartS = s.StartS - (minStartMS / 1000.0)
			if s.StartS < 0 {
				s.StartS = 0
			}
		}
	}

	// Stable order by time, then assign idx.
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].StartS < steps[j].StartS })
	for i, s := range steps {
		s.Idx = i + 1
	}

	// Network entries
	var network []TraceNetwork
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		if !strings.HasSuffix(name, ".network") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		byID := map[string]*TraceNetwork{}
		var order []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			id, _ := ev["requestId"].(string)
			if id == "" {
				continue
			}
			rec := byID[id]
			if rec == nil {
				rec = &TraceNetwork{}
				byID[id] = rec
				order = append(order, id)
			}
			switch ev["type"] {
			case "resource":
				if m, ok := ev["method"].(string); ok {
					rec.Method = m
				}
				if u, ok := ev["url"].(string); ok {
					rec.URL = u
				}
				if s, ok := ev["status"].(float64); ok {
					rec.Status = int(s)
				}
				if t, ok := ev["startTime"].(float64); ok && minStartSet {
					rec.Time = (t - minStartMS) / 1000.0
				}
			case "request":
				if m, ok := ev["method"].(string); ok {
					rec.Method = m
				}
				if u, ok := ev["url"].(string); ok {
					rec.URL = u
				}
				if t, ok := ev["startTime"].(float64); ok && minStartSet {
					rec.Time = (t - minStartMS) / 1000.0
				}
			case "response":
				if s, ok := ev["status"].(float64); ok {
					rec.Status = int(s)
				}
			case "requestfailed", "requestFailed":
				rec.Failed = true
			}
		}
		for _, id := range order {
			rec := byID[id]
			if rec.URL == "" {
				continue
			}
			if rec.Failed || rec.Status >= 400 || len(network) < 30 {
				network = append(network, *rec)
			}
		}
	}
	sort.SliceStable(network, func(i, j int) bool { return network[i].Time < network[j].Time })
	sort.SliceStable(consoles, func(i, j int) bool { return consoles[i].Time < consoles[j].Time })

	sum := &TraceSummary{Console: consoles, Network: network}
	for _, s := range steps {
		sum.Steps = append(sum.Steps, *s)
	}
	for u := range pageURLSet {
		sum.PageURLs = append(sum.PageURLs, u)
	}
	sort.Strings(sum.PageURLs)
	return sum, nil
}

// isInternalCall returns true for Playwright framework calls that aren't useful as user-visible steps.
func isInternalCall(api string) bool {
	switch api {
	case "":
		return true
	case "browserContext.newPage",
		"browser.newContext",
		"browserContext.close",
		"page.close":
		return true
	}
	return false
}

func trimSelector(s string) string {
	// Strip Playwright internal selector prefix to keep things readable.
	s = strings.TrimPrefix(s, "internal:testid=")
	if i := strings.Index(s, "[data-testid=\""); i >= 0 {
		end := strings.Index(s[i:], "\"s]")
		if end > 0 {
			return s[i+len("[data-testid=\"") : i+end]
		}
	}
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// stripANSI removes ANSI colour escape codes from a string (Playwright errors are ANSI-coloured).
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			if j < len(s) {
				i = j
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func truncateLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
