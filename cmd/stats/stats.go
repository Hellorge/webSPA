package main

import (
	"encoding/json"
	"fmt"
	"gogogo/modules/metrics"
	"gogogo/modules/profiler"
	"log"
	"net/http"
	"time"

	"github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
)

type MetricsUI struct {
	cpuChart      *widgets.Plot
	memChart      *widgets.Plot
	reqChart      *widgets.Plot
	gauges        []*widgets.Gauge
	requestList   *widgets.List
	summaryText   *widgets.Paragraph
	profilerStats *widgets.Paragraph
}

func NewMetricsUI() *MetricsUI {
	return &MetricsUI{
		cpuChart:      widgets.NewPlot(),
		memChart:      widgets.NewPlot(),
		reqChart:      widgets.NewPlot(),
		gauges:        make([]*widgets.Gauge, 3),
		requestList:   widgets.NewList(),
		summaryText:   widgets.NewParagraph(),
		profilerStats: widgets.NewParagraph(),
	}
}

func (m *MetricsUI) setupUI() {
	termui.Clear()

	m.cpuChart.Title = "LATENCY VELOCITY (ms)"
	m.cpuChart.LineColors = []termui.Color{termui.ColorYellow}
	m.cpuChart.TitleStyle.Fg = termui.ColorYellow
	m.cpuChart.BorderStyle.Fg = termui.ColorYellow
	m.cpuChart.AxesColor = termui.ColorWhite
	m.cpuChart.Data = [][]float64{{0}}
	
	m.memChart.Title = "CORE THROUGHPUT (MiB/s)"
	m.memChart.LineColors = []termui.Color{termui.ColorYellow}
	m.memChart.TitleStyle.Fg = termui.ColorYellow
	m.memChart.BorderStyle.Fg = termui.ColorYellow
	m.memChart.AxesColor = termui.ColorWhite
	m.memChart.Data = [][]float64{{0}}

	m.reqChart.Title = "ARCHITECTURAL PRESSURE (Total)"
	m.reqChart.LineColors = []termui.Color{termui.ColorYellow}
	m.reqChart.TitleStyle.Fg = termui.ColorYellow
	m.reqChart.BorderStyle.Fg = termui.ColorYellow
	m.reqChart.AxesColor = termui.ColorWhite
	m.reqChart.Data = [][]float64{{0}}

	for i := range m.gauges {
		m.gauges[i] = widgets.NewGauge()
		m.gauges[i].BarColor = termui.ColorYellow
		m.gauges[i].TitleStyle.Fg = termui.ColorYellow
		m.gauges[i].BorderStyle.Fg = termui.ColorYellow
	}
	m.gauges[0].Title = "VELOCITY RATIO"
	m.gauges[1].Title = "CORE SATURATION"
	m.gauges[2].Title = "SCHEMATIC LOAD"

	m.requestList.Title = "TRANSACTION LOG"
	m.requestList.TitleStyle.Fg = termui.ColorYellow
	m.requestList.BorderStyle.Fg = termui.ColorYellow
	m.requestList.TextStyle = termui.NewStyle(termui.ColorWhite)
	m.requestList.WrapText = false

	m.summaryText.Title = "ENGINE SPECIFICATIONS"
	m.summaryText.TitleStyle.Fg = termui.ColorYellow
	m.summaryText.BorderStyle.Fg = termui.ColorYellow
	m.summaryText.TextStyle = termui.NewStyle(termui.ColorWhite)

	m.profilerStats.Title = "MACHINE SYSTEM STATE"
	m.profilerStats.TitleStyle.Fg = termui.ColorYellow
	m.profilerStats.BorderStyle.Fg = termui.ColorYellow
	m.profilerStats.TextStyle = termui.NewStyle(termui.ColorWhite)

	m.layoutUI()
}

func (m *MetricsUI) layoutUI() {
	termWidth, termHeight := termui.TerminalDimensions()

	chartHeight := termHeight / 3
	gaugeHeight := 3
	summaryHeight := 5

	// Charts
	m.cpuChart.SetRect(0, 0, termWidth/3, chartHeight)
	m.memChart.SetRect(termWidth/3, 0, 2*termWidth/3, chartHeight)
	m.reqChart.SetRect(2*termWidth/3, 0, termWidth, chartHeight)

	// Gauges
	gaugeWidth := termWidth / 3
	for i, gauge := range m.gauges {
		gauge.SetRect(i*gaugeWidth, chartHeight, (i+1)*gaugeWidth, chartHeight+gaugeHeight)
	}

	// Summary
	m.summaryText.SetRect(0, chartHeight+gaugeHeight, termWidth, chartHeight+gaugeHeight+summaryHeight)

	// Request List
	m.requestList.SetRect(0, chartHeight+gaugeHeight+summaryHeight, termWidth, termHeight)

	profilerStatsHeight := 5
	m.profilerStats.SetRect(0, termHeight-profilerStatsHeight, termWidth, termHeight)
}

func (m *MetricsUI) fetchSnapshot() (*metrics.Snapshot, error) {
	resp, err := http.Get("http://localhost:8080/api/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var snapshot metrics.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (m *MetricsUI) updateCharts(snapshot *metrics.Snapshot) {
	cpuUsage := float64(snapshot.AvgLatency) / 1000000.0 // ms
	memUsage := float64(snapshot.TotalBytes) / (1024 * 1024)
	reqRate := snapshot.Throughput // MiB/s

	if len(m.cpuChart.Data[0]) >= 100 {
		m.cpuChart.Data[0] = m.cpuChart.Data[0][1:]
	}
	m.cpuChart.Data[0] = append(m.cpuChart.Data[0], cpuUsage)

	if len(m.memChart.Data[0]) >= 100 {
		m.memChart.Data[0] = m.memChart.Data[0][1:]
	}
	m.memChart.Data[0] = append(m.memChart.Data[0], memUsage)

	if len(m.reqChart.Data[0]) >= 100 {
		m.reqChart.Data[0] = m.reqChart.Data[0][1:]
	}
	m.reqChart.Data[0] = append(m.reqChart.Data[0], reqRate)
}

func (m *MetricsUI) updateGauges(snapshot *metrics.Snapshot) {
	// Percentages based on arbitrary limits for visualization
	m.gauges[0].Percent = int(snapshot.Throughput)               // Throughput as % of 100 MiB/s
	if m.gauges[0].Percent > 100 { m.gauges[0].Percent = 100 }
	
	m.gauges[1].Percent = int(snapshot.ActiveRequests)           // Active requests
	if m.gauges[1].Percent > 100 { m.gauges[1].Percent = 100 }

	m.gauges[2].Percent = int(snapshot.AvgLatency / 1000000000) // 1s = 100%
	if m.gauges[2].Percent > 100 { m.gauges[2].Percent = 100 }
}

func (m *MetricsUI) updateRequestList(snapshot *metrics.Snapshot) {
	m.requestList.Rows = []string{
		fmt.Sprintf("Total Requests: %d", snapshot.TotalRequests),
		fmt.Sprintf("Active Requests: %d", snapshot.ActiveRequests),
		fmt.Sprintf("Total Bytes:    %d", snapshot.TotalBytes),
		fmt.Sprintf("Avg Latency:    %v ms", float64(snapshot.AvgLatency)/1000000.0),
		fmt.Sprintf("Throughput:     %.2f MiB/s", snapshot.Throughput),
	}
}

func (m *MetricsUI) updateSummary(snapshot *metrics.Snapshot) {
	m.summaryText.Text = fmt.Sprintf(
		"ADDR: localhost:8080 | UPTIME: %v\nREQUESTS: %d | ACTIVE_THREADS: %d | FLOW: %.2f MiB/s",
		time.Duration(snapshot.Uptime).Round(time.Second),
		snapshot.TotalRequests,
		snapshot.ActiveRequests,
		snapshot.Throughput,
	)
}

func (m *MetricsUI) updateProfilerStats(stats profiler.ProfileStats) {
	m.profilerStats.Text = fmt.Sprintf(
		"Uptime: %v\nAllocated Memory: %v MB\nTotal Allocations: %v MB\nSystem Memory: %v MB\nGC Runs: %d",
		stats.Uptime.Round(time.Second),
		stats.AllocatedMem/1024/1024,
		stats.TotalAlloc/1024/1024,
		stats.Sys/1024/1024,
		stats.NumGC,
	)
}

func (m *MetricsUI) Run(profiler *profiler.Profiler) error {
	if err := termui.Init(); err != nil {
		return fmt.Errorf("failed to initialize termui: %v", err)
	}
	defer termui.Close()

	m.setupUI()

	updateUI := func() {
		snapshot, err := m.fetchSnapshot()
		if err == nil {
			m.updateCharts(snapshot)
			m.updateGauges(snapshot)
			m.updateRequestList(snapshot)
			m.updateSummary(snapshot)
		} else {
			m.summaryText.Text = fmt.Sprintf("Error fetching metrics: %v\nIs the server running?", err)
		}
		m.updateProfilerStats(profiler.GetStats())
	}

	updateUI()

	uiEvents := termui.PollEvents()
	ticker := time.NewTicker(time.Second).C

	for {
		select {
		case e := <-uiEvents:
			switch e.ID {
			case "q", "<C-c>":
				return nil
			case "<Resize>":
				m.layoutUI()
				termui.Clear()
				updateUI()
				termui.Render(m.cpuChart, m.memChart, m.reqChart, m.gauges[0], m.gauges[1], m.gauges[2], m.summaryText, m.requestList, m.profilerStats)
			}
		case <-ticker:
			updateUI()
			termui.Render(m.cpuChart, m.memChart, m.reqChart, m.gauges[0], m.gauges[1], m.gauges[2], m.summaryText, m.requestList, m.profilerStats)
		}
	}
}

func main() {
	prof := profiler.New()
	err := prof.Start("/tmp/stats_cpu.prof", "/tmp/stats_mem.prof")
	if err != nil {
		log.Printf("Failed to start profiler: %v", err)
	} else {
		defer prof.Stop()
	}

	ui := NewMetricsUI()
	if err := ui.Run(prof); err != nil {
		log.Fatalf("Error running ui: %v", err)
	}
}
