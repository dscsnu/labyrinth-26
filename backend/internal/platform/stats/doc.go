// Package stats collects the process and runtime figures the server reports
// on GET /health: Go heap memory, goroutines, CPU time and load average. It's
// plumbing with no business rules; the values are read on demand, not sampled
// in the background.
package stats
