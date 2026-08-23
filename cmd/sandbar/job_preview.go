package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type cliJobSnapshot struct {
	JobID        string `json:"job_id"`
	State        string `json:"state"`
	StdoutTail   string `json:"stdout_tail"`
	StderrTail   string `json:"stderr_tail"`
	Error        string `json:"error"`
	WaitTimedOut bool   `json:"wait_timed_out"`
}

func supervisedJobResultPreview(toolName, raw string) (string, bool) {
	if toolName != "shell_exec" && toolName != "job" || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return "", false
	}
	if toolName == "job" {
		var listed struct {
			Jobs []cliJobSnapshot `json:"jobs"`
		}
		if err := json.Unmarshal([]byte(raw), &listed); err == nil && listed.Jobs != nil {
			running := 0
			for _, job := range listed.Jobs {
				if job.State == "running" {
					running++
				}
			}
			if len(listed.Jobs) == 0 {
				return "no background jobs", true
			}
			return fmt.Sprintf("%d background jobs · %d running", len(listed.Jobs), running), true
		}
	}

	var job cliJobSnapshot
	if err := json.Unmarshal([]byte(raw), &job); err != nil || job.JobID == "" {
		return "", false
	}
	state := job.State
	if state == "" {
		state = "unknown"
	}
	preview := fmt.Sprintf("job %s · %s", compactJobID(job.JobID), state)
	switch {
	case job.WaitTimedOut:
		preview += " · wait timed out"
	case strings.TrimSpace(job.StdoutTail) != "":
		preview += " · " + clip(oneline(job.StdoutTail), 96)
	case strings.TrimSpace(job.StderrTail) != "":
		preview += " · stderr: " + clip(oneline(job.StderrTail), 88)
	case job.Error != "":
		preview += " · " + clip(oneline(job.Error), 96)
	}
	return preview, true
}

func compactJobID(id string) string {
	const width = 12
	if len(id) <= width {
		return id
	}
	return id[:width]
}
