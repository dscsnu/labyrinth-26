package tests

import (
	"encoding/json"
	"net/http"
	"runtime"
	"testing"
)

// TestHealthStats checks that GET /health answers without a token and returns
// the structured process stats, alongside the open stream count.
func TestHealthStats(t *testing.T) {
	e := setup(t)
	res := e.get("/health")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", res.StatusCode)
	}
	var body struct {
		Success bool `json:"success"`
		Payload struct {
			UptimeMS   int64  `json:"uptime_ms"`
			Streams    int    `json:"streams"`
			GoVersion  string `json:"go_version"`
			Goroutines int    `json:"goroutines"`
			NumCPU     int    `json:"num_cpu"`
			CPU        struct {
				ProcessSeconds float64 `json:"process_seconds"`
				LoadAverage    struct {
					One     float64 `json:"1"`
					Five    float64 `json:"5"`
					Fifteen float64 `json:"15"`
				} `json:"load_average"`
			} `json:"cpu"`
			Memory struct {
				Alloc uint64 `json:"alloc"`
				RSS   uint64 `json:"rss"`
			} `json:"memory"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Success {
		t.Error("success = false")
	}
	if body.Payload.UptimeMS < 0 {
		t.Errorf("uptime_ms = %d, want >= 0", body.Payload.UptimeMS)
	}
	if body.Payload.Streams != 0 {
		t.Errorf("streams = %d, want 0 with no stream open", body.Payload.Streams)
	}
	if body.Payload.GoVersion == "" {
		t.Error("go_version is empty")
	}
	if body.Payload.Goroutines < 1 || body.Payload.NumCPU < 1 {
		t.Errorf("goroutines = %d, num_cpu = %d", body.Payload.Goroutines, body.Payload.NumCPU)
	}
	if body.Payload.Memory.Alloc == 0 {
		t.Error("memory.alloc = 0")
	}
	if runtime.GOOS == "linux" {
		if body.Payload.Memory.RSS == 0 {
			t.Error("memory.rss = 0 on Linux")
		}
		if body.Payload.CPU.ProcessSeconds < 0 {
			t.Errorf("cpu.process_seconds = %v, want >= 0", body.Payload.CPU.ProcessSeconds)
		}
		if body.Payload.CPU.LoadAverage.One < 0 {
			t.Errorf("cpu.load_average.1 = %v, want >= 0", body.Payload.CPU.LoadAverage.One)
		}
	}
}
